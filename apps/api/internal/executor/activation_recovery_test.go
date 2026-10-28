package executor

import (
	"context"
	"testing"

	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/runtime"
)

func TestRecoveryLeavesUnresolvedActivationResourcesUntouched(t *testing.T) {
	claim := workerTestClaim()
	claim.Status = deployments.StatusActivating
	worker := &Worker{store: testStore{
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
