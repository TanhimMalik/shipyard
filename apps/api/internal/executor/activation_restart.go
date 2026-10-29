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

// recoverActivation runs only while holding the coordinator lock and the newly
// taken claim. It resolves observed state, not an unrecorded pre-crash decision.
func (w *Worker) recoverActivation(ctx context.Context, claim deployments.Claim) error {
	if claim.Status != deployments.StatusActivating {
		return errors.New("activation recovery requires an activating claim")
	}
	spec, err := startupSpec(claim.Deployment)
	if err != nil {
		return err
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	intent, err := w.store.GetActivation(readCtx, claim.ID)
	cancel()
	if err != nil {
		return fmt.Errorf("read interrupted activation: %w", err)
	}
	target := routing.Binding(intent.Target)
	if _, err := routing.Marker(target); err != nil || intent.ConfirmedAt != nil || target.ProjectID != claim.ProjectID ||
		target.DeploymentID != claim.ID || target.InstanceID != *claim.InstanceID {
		return errors.New("interrupted activation has inconsistent target metadata")
	}
	spec.ContainerID = target.ContainerID
	var previous *routing.Binding
	if intent.Previous != nil {
		binding := routing.Binding(*intent.Previous)
		if _, err := routing.Marker(binding); err != nil || binding.ProjectID != claim.ProjectID || binding.DeploymentID == claim.ID {
			return errors.New("interrupted activation has inconsistent previous metadata")
		}
		previous = &binding
	}
	readCtx, cancel = context.WithTimeout(ctx, 3*time.Second)
	active, err := w.store.GetActiveRoute(readCtx, claim.ProjectID)
	cancel()
	if err != nil && !errors.Is(err, deployments.ErrActiveRouteNotFound) {
		return fmt.Errorf("read active route during recovery: %w", err)
	}
	var saved *routing.Binding
	if err == nil {
		binding := routing.Binding(active)
		saved = &binding
	}
	if !sameBinding(saved, previous) {
		return errors.New("active route differs from the interrupted activation's previous binding")
	}
	readCtx, cancel = context.WithTimeout(ctx, 3*time.Second)
	instance, err := w.store.GetInstance(readCtx, target.InstanceID)
	cancel()
	if err != nil {
		return fmt.Errorf("read interrupted activation instance: %w", err)
	}
	if instance.ID != target.InstanceID || instance.DeploymentID != claim.ID || instance.ContainerID == nil || *instance.ContainerID != target.ContainerID {
		return errors.New("interrupted activation lost its pinned Docker identity")
	}
	current, err := w.readRoute(ctx, claim.ProjectID)
	if err != nil {
		return err
	}
	if sameBinding(current, previous) {
		return w.rollbackActivation(ctx, claim, claim.Deployment, target, previous, "Deployment was interrupted before route activation completed.")
	}
	if !sameBinding(current, &target) {
		return errors.New("cannot recover an unexpected published route")
	}
	observed, err := w.inspectReadiness(ctx, spec)
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return err
	}
	if err == nil {
		if err := w.recordInstance(ctx, claim, target.InstanceID, observed); err != nil {
			return err
		}
	}
	if err != nil || !readyCandidate(observed, spec, target.HostPort) {
		return w.rollbackActivation(ctx, claim, claim.Deployment, target, previous, "Application changed while activation was interrupted.")
	}
	// The same binding may be an older generated format, or a rename whose
	// directory sync lost its acknowledgement. Republish before proxy verification.
	operationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = w.routes.Put(operationCtx, target, &target)
	cancel()
	if err != nil {
		return fmt.Errorf("republish interrupted activation: %w", err)
	}
	return w.finishActivation(ctx, claim, claim.Deployment, target, previous)
}
