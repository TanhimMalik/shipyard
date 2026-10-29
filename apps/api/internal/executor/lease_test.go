package executor

import (
	"context"
	"testing"
	"time"

	"shipyard/apps/api/internal/deployments"
)

func TestShutdownFinishesHeartbeatBeforeReleasingOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	entered := make(chan context.Context, 1)
	release, closed := make(chan struct{}), make(chan struct{})
	worker := &Worker{routes: &testRoutes{}, checkInterval: time.Millisecond,
		store: testStore{interrupted: func(ctx context.Context) (deployments.Claim, error) {
			<-ctx.Done()
			return deployments.Claim{}, ctx.Err()
		}},
		acquire: func(context.Context) (lease, error) {
			return testLease{check: func(ctx context.Context) error {
				entered <- ctx
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}, close: func() error { close(closed); return nil }}, nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var checkCtx context.Context
	select {
	case checkCtx = <-entered:
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not start")
	}
	cancel()
	deadline, bounded := checkCtx.Deadline()
	if checkCtx.Err() != nil || !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 3*time.Second {
		t.Fatalf("shutdown canceled the heartbeat or removed its deadline: %v", checkCtx.Err())
	}
	select {
	case <-closed:
		t.Fatal("released ownership before the in-flight heartbeat finished")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not finish after heartbeat completion")
	}
	select {
	case <-closed:
	default:
		t.Fatal("worker finished without releasing ownership")
	}
}
