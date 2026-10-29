package executor

import (
	"context"
	"errors"
	"testing"

	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/runtime"
)

func TestRecoveryLeavesIncompleteActivationResourcesUntouched(t *testing.T) {
	claim := workerTestClaim()
	claim.Status = deployments.StatusActivating
	worker := &Worker{routes: &testRoutes{}, store: testStore{
		interrupted: func(context.Context) (deployments.Claim, error) { return claim, nil },
		fail: func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error) {
			t.Fatal("failed an activation without verifying route rollback")
			return deployments.Deployment{}, nil
		},
	}, runtime: testRuntime{inspect: func(context.Context, runtime.Spec) (runtime.Instance, error) {
		t.Fatal("touched a possibly routed candidate during ordinary recovery")
		return runtime.Instance{}, nil
	}}, builder: testBuilder{cleanup: func(context.Context, builds.Input, string) error {
		t.Fatal("removed an image before resolving activation")
		return nil
	}}}
	if err := worker.recover(t.Context()); err == nil {
		t.Fatal("unresolved activation did not stop recovery")
	}
}

func TestRecoveryUsesTakenClaimAndRereadsOnStageConflict(t *testing.T) {
	claim := workerTestClaim()
	taken := claim
	taken.Token = "new-recovery-owner"
	failed := false
	worker := &Worker{routes: &testRoutes{}, store: testStore{
		interrupted: func(context.Context) (deployments.Claim, error) {
			if failed {
				return deployments.Claim{}, deployments.ErrNoInterruptedDeployments
			}
			return claim, nil
		},
		take: func(ctx context.Context, got deployments.Claim) (deployments.Claim, error) {
			workerTestWrite(t, ctx, got, claim)
			return taken, nil
		},
		fail: func(ctx context.Context, got deployments.Claim, stage, reason string) (deployments.Deployment, error) {
			workerTestWrite(t, ctx, got, taken)
			failed = true
			return deployments.Deployment{}, nil
		},
	}}
	if err := worker.recover(t.Context()); err != nil || !failed {
		t.Fatalf("recovery did not use the fenced claim: failed=%t error=%v", failed, err)
	}
	reads := 0
	worker.store = testStore{
		interrupted: func(context.Context) (deployments.Claim, error) {
			reads++
			got := claim
			if reads > 1 {
				got.Status = deployments.StatusActivating
			}
			return got, nil
		},
		take: func(_ context.Context, got deployments.Claim) (deployments.Claim, error) {
			if got.Status != deployments.StatusActivating {
				return deployments.Claim{}, deployments.ErrInvalidTransition
			}
			got.Token = taken.Token
			return got, nil
		},
		fail: func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error) {
			t.Fatal("failed a route activation using stale recovery metadata")
			return deployments.Deployment{}, nil
		},
	}
	if err := worker.recover(t.Context()); err == nil || errors.Is(err, deployments.ErrInvalidTransition) || reads != 2 {
		t.Fatalf("recovery did not reread the changed stage: reads=%d error=%v", reads, err)
	}
}
