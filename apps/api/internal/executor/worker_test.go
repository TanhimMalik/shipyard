package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/source"
)

type fetchFunc func(context.Context, string, string) (*source.Checkout, error)

func (f fetchFunc) Fetch(ctx context.Context, repository, branch string) (*source.Checkout, error) {
	return f(ctx, repository, branch)
}

type testStore struct {
	claim       func(context.Context) (deployments.Claim, error)
	interrupted func(context.Context) (deployments.Claim, error)
	record      func(context.Context, deployments.Claim, string) (deployments.Deployment, error)
	fail        func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error)
}

func (s testStore) ClaimNext(ctx context.Context) (deployments.Claim, error) {
	if s.claim != nil {
		return s.claim(ctx)
	}
	return deployments.Claim{}, deployments.ErrNoQueuedDeployments
}
func (s testStore) NextInterrupted(ctx context.Context) (deployments.Claim, error) {
	if s.interrupted != nil {
		return s.interrupted(ctx)
	}
	return deployments.Claim{}, deployments.ErrNoInterruptedDeployments
}
func (s testStore) RecordSourceCommit(ctx context.Context, c deployments.Claim, sha string) (deployments.Deployment, error) {
	return s.record(ctx, c, sha)
}
func (s testStore) FailExecution(ctx context.Context, c deployments.Claim, stage, reason string) (deployments.Deployment, error) {
	return s.fail(ctx, c, stage, reason)
}

type testLease struct {
	check func(context.Context) error
	close func() error
}

func (l testLease) Check(ctx context.Context) error {
	if l.check != nil {
		return l.check(ctx)
	}
	return nil
}
func (l testLease) Close() error {
	if l.close != nil {
		return l.close()
	}
	return nil
}

func TestSourceCheckpointUsesSnapshotAndStopsOnStorageFailure(t *testing.T) {
	claim := deployments.Claim{Deployment: deployments.Deployment{ID: "attempt", GitURL: "https://example.com/old.git", Branch: "release/old"}, Token: "private-owner"}
	sha := strings.Repeat("a", 40)
	for _, storageError := range []error{nil, errors.New("storage write lost")} {
		calls := 0
		w := &Worker{fetcher: fetchFunc(func(ctx context.Context, repository, branch string) (*source.Checkout, error) {
			if repository != claim.GitURL || branch != claim.Branch {
				t.Fatal("did not use queued source snapshot")
			}
			return &source.Checkout{CommitSHA: sha}, nil
		}), store: testStore{record: func(ctx context.Context, got deployments.Claim, commit string) (deployments.Deployment, error) {
			calls++
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 3*time.Second || got.Token != claim.Token || commit != sha {
				t.Fatal("unbounded or incorrect source write")
			}
			return deployments.Deployment{}, storageError
		}}}
		if err := w.execute(t.Context(), claim); !errors.Is(err, storageError) || calls != 1 {
			t.Fatalf("source checkpoint: calls=%d, error=%v", calls, err)
		}
	}
}

func TestSourceFailuresPersistOnlyPublicReasons(t *testing.T) {
	for _, sourceError := range []error{errors.New("private Git diagnostic"), source.ErrNotAllowed, context.DeadlineExceeded} {
		w := &Worker{fetcher: fetchFunc(func(context.Context, string, string) (*source.Checkout, error) { return nil, sourceError }), store: testStore{fail: func(ctx context.Context, claim deployments.Claim, stage, reason string) (deployments.Deployment, error) {
			if stage != deployments.StatusFetchingSource || reason == "" || strings.Contains(reason, "private") {
				t.Fatalf("unsafe failure: %s/%s", stage, reason)
			}
			return deployments.Deployment{}, nil
		}}}
		if err := w.execute(t.Context(), deployments.Claim{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLeaseLossCancelsFetchBeforeReleasingOwnership(t *testing.T) {
	entered, exited := make(chan struct{}), make(chan struct{})
	leaseError := errors.New("lease connection lost")
	closed := false
	claimed := false
	w := &Worker{pollInterval: time.Millisecond, checkInterval: time.Millisecond,
		store: testStore{claim: func(context.Context) (deployments.Claim, error) {
			if claimed {
				t.Fatal("claimed again after ownership loss")
			}
			claimed = true
			return deployments.Claim{}, nil
		}},
		fetcher: fetchFunc(func(ctx context.Context, _ string, _ string) (*source.Checkout, error) {
			close(entered)
			<-ctx.Done()
			close(exited)
			return nil, ctx.Err()
		}),
		acquire: func(context.Context) (lease, error) {
			return testLease{check: func(ctx context.Context) error {
				select {
				case <-entered:
					return leaseError
				case <-ctx.Done():
					return ctx.Err()
				}
			}, close: func() error {
				select {
				case <-exited:
				default:
					t.Error("released ownership before fetch exited")
				}
				closed = true
				return nil
			}}, nil
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := w.Run(ctx); !errors.Is(err, leaseError) || !closed {
		t.Fatalf("lost ownership: closed=%t error=%v", closed, err)
	}
}

func TestWorkerBusyAndRecoveryFailureDoNotClaimNewWork(t *testing.T) {
	w := &Worker{acquire: func(context.Context) (lease, error) { return nil, ErrExecutorBusy }}
	if err := w.Run(t.Context()); !errors.Is(err, ErrExecutorBusy) {
		t.Fatal(err)
	}
	for _, status := range []string{deployments.StatusBuilding, deployments.StatusStarting, deployments.StatusHealthChecking} {
		w.store = testStore{interrupted: func(context.Context) (deployments.Claim, error) {
			return deployments.Claim{Deployment: deployments.Deployment{Status: status}}, nil
		}}
		if err := w.recover(t.Context()); err == nil {
			t.Fatalf("source-only worker mutated later stage %s", status)
		}
	}
}

func TestWorkerCancellationLeavesAttemptForRestartRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	w := &Worker{fetcher: fetchFunc(func(context.Context, string, string) (*source.Checkout, error) {
		cancel()
		return nil, context.Canceled
	})}
	if err := w.execute(ctx, deployments.Claim{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
