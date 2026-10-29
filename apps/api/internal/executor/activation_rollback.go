package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/routing"
)

func (w *Worker) rollbackActivation(ctx context.Context, claim deployments.Claim, deployment deployments.Deployment, target routing.Binding, previous *routing.Binding, reason string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	current, err := w.readRoute(ctx, deployment.ProjectID)
	if err != nil {
		return err
	}
	if !sameBinding(current, &target) && !sameBinding(current, previous) {
		return errors.New("cannot restore an unexpected published route")
	}
	operationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if previous == nil {
		err = w.routes.Remove(operationCtx, target)
	} else {
		err = w.routes.Put(operationCtx, *previous, current)
	}
	cancel()
	if err != nil {
		return fmt.Errorf("restore previous route: %w", err)
	}
	if err := w.verifier.Restored(ctx, deployment.ProjectID, previous); err != nil {
		return fmt.Errorf("previous route restoration is uncertain: %w", err)
	}
	current, err = w.readRoute(ctx, deployment.ProjectID)
	if err != nil {
		return err
	}
	if !sameBinding(current, previous) {
		return errors.New("previous route changed after restoration verification")
	}
	if err := w.cleanupStartup(ctx, claim, deployment); err != nil {
		return err
	}
	var restored *deployments.RouteBinding
	if previous != nil {
		binding := deployments.RouteBinding(*previous)
		restored = &binding
	}
	writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err = w.store.AbortActivation(writeCtx, claim, restored, reason)
	if err != nil {
		return fmt.Errorf("record activation abort: %w", err)
	}
	return nil
}
