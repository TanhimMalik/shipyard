package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/health"
	"shipyard/apps/api/internal/runtime"
)

func (w *Worker) checkReadiness(ctx context.Context, claim deployments.Claim, deployment deployments.Deployment) error {
	spec, err := startupSpec(deployment)
	if err != nil {
		return err
	}
	if deployment.ID != claim.ID || deployment.ProjectID != claim.ProjectID || deployment.Status != deployments.StatusStarting {
		return errors.New("readiness metadata does not belong to this startup claim")
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	instance, err := w.store.GetInstance(readCtx, *deployment.InstanceID)
	cancel()
	if err != nil {
		return fmt.Errorf("read readiness instance: %w", err)
	}
	if instance.ID != *deployment.InstanceID || instance.DeploymentID != deployment.ID || instance.ContainerID == nil || instance.HostPort == nil {
		return errors.New("readiness instance has no pinned container and port")
	}
	spec.ContainerID = *instance.ContainerID
	port := *instance.HostPort
	before, err := w.inspectReadiness(ctx, spec)
	if errors.Is(err, runtime.ErrNotFound) {
		return w.failStartup(ctx, claim, deployment, "Application stopped before readiness completed.", err)
	}
	if err != nil {
		return err
	}
	if err := w.recordInstance(ctx, claim, instance.ID, before); err != nil {
		return err
	}
	if !readyCandidate(before, spec, port) {
		return w.failStartup(ctx, claim, deployment, "Application stopped before readiness completed.", errors.New("candidate changed before probing"))
	}
	writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	deployment, err = w.store.BeginHealthCheck(writeCtx, claim)
	cancel()
	if err != nil {
		return fmt.Errorf("begin readiness checks: %w", err)
	}
	result, probeErr := w.readiness.Wait(ctx, health.Target{HostPort: port, Path: deployment.HealthPath})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if probeErr != nil && !errors.Is(probeErr, health.ErrNotReady) {
		return fmt.Errorf("readiness check outcome is uncertain: %w", probeErr)
	}
	after, err := w.inspectReadiness(ctx, spec)
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return err
	}
	if err == nil {
		if err := w.recordInstance(ctx, claim, instance.ID, after); err != nil {
			return err
		}
	}
	unchanged := err == nil && readyCandidate(after, spec, port)
	passed := probeErr == nil && unchanged
	if result.Attempts > 0 {
		writeCtx, cancel = context.WithTimeout(ctx, 3*time.Second)
		_, err = w.store.RecordReadiness(writeCtx, claim, instance.ID, deployments.ReadinessObservation{
			ContainerID: spec.ContainerID, HostPort: port, Passed: passed, Attempts: result.Attempts,
			StatusCode: result.StatusCode, CheckedAt: result.CheckedAt,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("record readiness outcome: %w", err)
		}
	} else if probeErr == nil {
		return errors.New("readiness success has no HTTP observation")
	}
	if passed {
		return nil
	}
	reason := "Application stopped before readiness completed."
	if unchanged {
		reason = "Application did not become ready before the readiness deadline."
	}
	slog.Error("Application readiness failed", "deployment_id", claim.ID, "reason", reason)
	if err := w.cleanupStartup(ctx, claim, deployment); err != nil {
		return err
	}
	writeCtx, cancel = context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err = w.store.FailExecution(writeCtx, claim, deployments.StatusHealthChecking, reason)
	if err != nil {
		return fmt.Errorf("record readiness failure: %w", err)
	}
	return nil
}

func (w *Worker) inspectReadiness(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	observed, err := w.runtime.Inspect(ctx, spec)
	if err != nil {
		return runtime.Instance{}, fmt.Errorf("inspect readiness candidate: %w", err)
	}
	return observed, nil
}

func readyCandidate(observed runtime.Instance, spec runtime.Spec, port int) bool {
	return observed.ID == spec.ContainerID && observed.Running && observed.Status == "running" && observed.HostPort == port
}
