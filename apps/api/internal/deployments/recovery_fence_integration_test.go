//go:build integration

package deployments

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTakeInterruptedFencesEveryRecoverableStage(t *testing.T) {
	for _, stage := range []string{StatusFetchingSource, StatusBuilding, StatusStarting, StatusHealthChecking, StatusActivating} {
		t.Run(stage, func(t *testing.T) {
			pool, _ := newDeploymentStoreTestDatabase(t)
			store := NewStore(pool)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			captured := recoveryFenceTestClaim(t, ctx, pool, stage)
			wrongStage := captured
			wrongStage.Status = StatusQueued
			if _, err := store.TakeInterrupted(ctx, wrongStage); !errors.Is(err, ErrInvalidExecution) {
				t.Fatalf("took unsupported stage: %v", err)
			}
			wrongStage.Status = StatusStarting
			if stage == StatusStarting {
				wrongStage.Status = StatusHealthChecking
			}
			if _, err := store.TakeInterrupted(ctx, wrongStage); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("took a changed stage: %v", err)
			}
			canceled, stop := context.WithCancel(ctx)
			stop()
			if _, err := store.TakeInterrupted(canceled, captured); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled takeover: %v", err)
			}
			taken, err := store.TakeInterrupted(ctx, captured)
			if err != nil || taken.Token == "" || taken.Token == captured.Token || taken.Status != stage {
				t.Fatalf("ownership was not rotated in place: %+v, %v", taken, err)
			}
			want := captured.Deployment
			want.UpdatedAt = taken.UpdatedAt
			assertDeploymentEqual(t, taken.Deployment, want)
			if _, err := store.TakeInterrupted(ctx, captured); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("stale snapshot took ownership twice: %v", err)
			}
			if stage != StatusActivating {
				if _, err := store.FailExecution(ctx, captured, stage, "Stale worker failed."); !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("old owner failed the taken deployment: %v", err)
				}
			}
			if captured.InstanceID != nil {
				_, err := store.RecordInstance(ctx, captured, *captured.InstanceID, Observation{ContainerID: strings.Repeat("a", 64), Status: "created"})
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("old owner recorded Docker state: %v", err)
				}
			}
			if stage == StatusHealthChecking {
				if _, err := store.BeginActivation(ctx, captured, strings.Repeat("a", 64), 32101); !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("old owner entered activation after takeover: %v", err)
				}
			}
			if stage == StatusActivating {
				if _, err := store.AbortActivation(ctx, captured, nil, "Stale worker rolled back."); !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("old owner aborted the taken activation: %v", err)
				}
			}
			current, err := store.NextInterrupted(ctx)
			if err != nil || current.Token != taken.Token || current.Status != stage {
				t.Fatalf("takeover was not durable: %+v, %v", current, err)
			}
		})
	}
}

func TestReadinessTakeoverAndActivationCannotBothWin(t *testing.T) {
	for _, order := range []string{"concurrent", "takeover first", "activation first"} {
		t.Run(order, func(t *testing.T) {
			pool, _ := newDeploymentStoreTestDatabase(t)
			store := NewStore(pool)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			captured := recoveryFenceTestClaim(t, ctx, pool, StatusHealthChecking)
			var taken Claim
			var takeErr, activationErr error
			take := func() { taken, takeErr = store.TakeInterrupted(ctx, captured) }
			activate := func() { _, activationErr = store.BeginActivation(ctx, captured, strings.Repeat("a", 64), 32101) }
			switch order {
			case "takeover first":
				take()
				activate()
			case "activation first":
				activate()
				take()
			default:
				start, done := make(chan struct{}), make(chan struct{}, 2)
				go func() { <-start; take(); done <- struct{}{} }()
				go func() { <-start; activate(); done <- struct{}{} }()
				close(start)
				<-done
				<-done
			}
			current, err := store.NextInterrupted(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if takeErr == nil {
				if !errors.Is(activationErr, ErrInvalidTransition) || current.Token != taken.Token || current.Status != StatusHealthChecking {
					t.Fatalf("old owner crossed the takeover fence: take=%v activation=%v current=%+v", takeErr, activationErr, current)
				}
				if _, err := store.BeginActivation(ctx, taken, strings.Repeat("a", 64), 32101); err != nil {
					t.Fatalf("new owner could not continue: %v", err)
				}
			} else if activationErr == nil {
				if !errors.Is(takeErr, ErrInvalidTransition) || current.Token != captured.Token || current.Status != StatusActivating {
					t.Fatalf("stale readiness cleanup could continue: take=%v activation=%v current=%+v", takeErr, activationErr, current)
				}
			} else {
				t.Fatalf("race had no owner: take=%v activation=%v", takeErr, activationErr)
			}
		})
	}
}

func recoveryFenceTestClaim(t *testing.T, ctx context.Context, pool *pgxpool.Pool, stage string) Claim {
	t.Helper()
	store := NewStore(pool)
	if stage == StatusHealthChecking || stage == StatusActivating {
		claim, _, target := activationStoreTestCandidate(t, ctx, pool, "", 'a', 32101, true)
		if stage == StatusActivating {
			if _, err := store.BeginActivation(ctx, claim, target.ContainerID, target.HostPort); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		project := createDeploymentTestProject(t, ctx, pool, "recovery-fence")
		if _, err := store.Create(ctx, project.ID); err != nil {
			t.Fatal(err)
		}
		claim, err := store.ClaimNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if stage != StatusFetchingSource {
			if _, err := store.RecordSourceCommit(ctx, claim, strings.Repeat("a", 40)); err != nil {
				t.Fatal(err)
			}
			if _, err := store.BeginBuild(ctx, claim); err != nil {
				t.Fatal(err)
			}
		}
		if stage == StatusStarting {
			reference, imageID := buildRuntimeTestImage(claim)
			if _, err := store.BeginStart(ctx, claim, reference, imageID); err != nil {
				t.Fatal(err)
			}
		}
	}
	claim, err := store.NextInterrupted(ctx)
	if err != nil || claim.Status != stage {
		t.Fatalf("prepare interrupted stage: %+v, %v", claim, err)
	}
	return claim
}
