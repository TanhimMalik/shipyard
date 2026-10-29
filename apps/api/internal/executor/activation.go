package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/routing"
	"shipyard/apps/api/internal/runtime"
)

func (w *Worker) activate(ctx context.Context, claim deployments.Claim, ready readyDeployment) error {
	deployment := ready.Deployment
	spec, err := startupSpec(deployment)
	if err != nil {
		return err
	}
	if deployment.ID != claim.ID || deployment.ProjectID != claim.ProjectID || deployment.Status != deployments.StatusHealthChecking ||
		deployment.ReadinessPassed == nil || !*deployment.ReadinessPassed {
		return errors.New("activation does not have this claim's passed readiness result")
	}
	spec.ContainerID = ready.Instance.ID
	target := routing.Binding{ProjectID: deployment.ProjectID, DeploymentID: deployment.ID, InstanceID: *deployment.InstanceID,
		ContainerID: spec.ContainerID, HostPort: ready.Instance.HostPort}
	if _, err := routing.Marker(target); err != nil || !readyCandidate(ready.Instance, spec, target.HostPort) {
		return errors.New("activation has no verified running candidate")
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	previous, err := w.store.GetActiveRoute(readCtx, deployment.ProjectID)
	cancel()
	if err != nil && !errors.Is(err, deployments.ErrActiveRouteNotFound) {
		return fmt.Errorf("read active route before activation: %w", err)
	}
	var expected *routing.Binding
	if err == nil {
		binding := routing.Binding(previous)
		expected = &binding
	}
	current, err := w.readRoute(ctx, deployment.ProjectID)
	if err != nil {
		return err
	}
	if !sameBinding(current, expected) {
		return errors.New("published route differs from the saved active route")
	}
	before, err := w.inspectReadiness(ctx, spec)
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return err
	}
	if err == nil {
		if err := w.recordInstance(ctx, claim, *deployment.InstanceID, before); err != nil {
			return err
		}
	}
	if err != nil || !readyCandidate(before, spec, target.HostPort) {
		if err := w.cleanupStartup(ctx, claim, deployment); err != nil {
			return err
		}
		writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, err := w.store.FailExecution(writeCtx, claim, deployments.StatusHealthChecking, "Application changed before route activation.")
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	intent, err := w.store.BeginActivation(writeCtx, claim, target.ContainerID, target.HostPort)
	cancel()
	if err != nil {
		return fmt.Errorf("record activation intent: %w", err)
	}
	var previousBinding *routing.Binding
	if intent.Previous != nil {
		binding := routing.Binding(*intent.Previous)
		previousBinding = &binding
	}
	if routing.Binding(intent.Target) != target || !sameBinding(previousBinding, expected) || intent.ConfirmedAt != nil {
		return errors.New("activation intent differs from the verified route identities")
	}
	deployment.Status = deployments.StatusActivating
	operationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = w.routes.Put(operationCtx, target, expected)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return w.rollbackActivation(ctx, claim, deployment, target, expected, "Route publication failed.")
	}
	if err := w.verifier.Ready(ctx, target, deployment.HealthPath); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !errors.Is(err, routing.ErrNotObserved) {
			return fmt.Errorf("route verification outcome is uncertain: %w", err)
		}
		return w.rollbackActivation(ctx, claim, deployment, target, expected, "Application route did not become ready.")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	after, err := w.inspectReadiness(ctx, spec)
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return err
	}
	if err == nil {
		if err := w.recordInstance(ctx, claim, *deployment.InstanceID, after); err != nil {
			return err
		}
	}
	if err != nil || !readyCandidate(after, spec, target.HostPort) {
		return w.rollbackActivation(ctx, claim, deployment, target, expected, "Application changed during route activation.")
	}
	current, err = w.readRoute(ctx, deployment.ProjectID)
	if err != nil {
		return err
	}
	if current == nil || *current != target {
		return errors.New("published route changed after verification")
	}
	writeCtx, cancel = context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err = w.store.ConfirmActivation(writeCtx, claim, deployments.RouteBinding(target))
	if err != nil {
		// The transaction may have committed. Never undo a possibly confirmed route.
		return fmt.Errorf("confirm activation outcome is uncertain: %w", err)
	}
	return nil
}

func (w *Worker) readRoute(ctx context.Context, projectID string) (*routing.Binding, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	binding, err := w.routes.Get(ctx, projectID)
	if errors.Is(err, routing.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read published route: %w", err)
	}
	return &binding, nil
}

func sameBinding(left, right *routing.Binding) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
