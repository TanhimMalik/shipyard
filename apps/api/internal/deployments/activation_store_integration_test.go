//go:build integration

package deployments

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestActivationIntentRequiresReadyOwnedTargetAndPersists(t *testing.T) {
	pool, config := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	claim, candidate, target := activationStoreTestCandidate(t, ctx, pool, "", 'a', 32101, false)
	if _, err := store.GetActivation(ctx, claim.ID); !errors.Is(err, ErrActivationNotFound) {
		t.Fatalf("unstarted activation invented intent: %v", err)
	}
	if _, err := store.GetActiveRoute(ctx, claim.ProjectID); !errors.Is(err, ErrActiveRouteNotFound) {
		t.Fatalf("unstarted activation invented active route: %v", err)
	}
	if _, err := store.BeginActivation(ctx, claim, target.ContainerID, target.HostPort); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("activation skipped readiness: %v", err)
	}
	activationStoreTestReadiness(t, ctx, store, claim, candidate, target)
	stale := activationStoreTestStale(claim)
	if _, err := store.BeginActivation(ctx, stale, target.ContainerID, target.HostPort); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stale claim began activation: %v", err)
	}
	for _, changed := range []RouteBinding{
		{ContainerID: strings.Repeat("f", 64), HostPort: target.HostPort},
		{ContainerID: target.ContainerID, HostPort: target.HostPort + 1},
	} {
		if _, err := store.BeginActivation(ctx, claim, changed.ContainerID, changed.HostPort); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("different candidate began activation: %v", err)
		}
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := store.BeginActivation(canceled, claim, target.ContainerID, target.HostPort); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled activation: %v", err)
	}
	intent, err := store.BeginActivation(ctx, claim, target.ContainerID, target.HostPort)
	if err != nil || intent.Target != target || intent.Previous != nil || intent.CreatedAt.IsZero() || intent.ConfirmedAt != nil {
		t.Fatalf("saved activation intent: %+v, %v", intent, err)
	}
	activating, err := store.Get(ctx, claim.ID)
	if err != nil || activating.Status != StatusActivating || activating.FinishedAt != nil {
		t.Fatalf("intent prematurely completed deployment: %+v, %v", activating, err)
	}
	if repeated, err := store.BeginActivation(ctx, claim, target.ContainerID, target.HostPort); err != nil || !reflect.DeepEqual(repeated, intent) {
		t.Fatalf("intent retry changed metadata: %+v, %v", repeated, err)
	}
	if repeated, err := store.Get(ctx, claim.ID); err != nil {
		t.Fatal(err)
	} else {
		assertDeploymentEqual(t, repeated, activating)
	}
	if _, err := store.BeginActivation(ctx, claim, target.ContainerID, target.HostPort+1); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("replaced persisted target: %v", err)
	}
	if _, err := store.Create(ctx, claim.ProjectID); !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("activation released unfinished slot: %v", err)
	}
	if interrupted, err := store.NextInterrupted(ctx); err != nil || interrupted.ID != claim.ID || interrupted.Status != StatusActivating || interrupted.Token != claim.Token {
		t.Fatalf("activation missing from interrupted work: %+v, %v", interrupted, err)
	}
	if _, err := store.FailExecution(ctx, claim, StatusActivating, "Activation failed."); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("generic failure bypassed rollback verification: %v", err)
	}
	reopened, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if found, err := NewStore(reopened).GetActivation(ctx, claim.ID); err != nil || !reflect.DeepEqual(found, intent) {
		t.Fatalf("intent was not durable: %+v, %v", found, err)
	}
}

func TestActivationConfirmationReplacesRouteWithoutStoppingOldRuntime(t *testing.T) {
	pool, config := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	claim, candidate, target := activationStoreTestCandidate(t, ctx, pool, "", 'a', 32101, true)
	if _, err := store.BeginActivation(ctx, claim, target.ContainerID, target.HostPort); err != nil {
		t.Fatal(err)
	}
	changed := target
	changed.ContainerID = strings.Repeat("f", 64)
	if _, err := store.ConfirmActivation(ctx, claim, changed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("confirmed a different target: %v", err)
	}
	if _, err := store.ConfirmActivation(ctx, activationStoreTestStale(claim), target); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stale claim confirmed activation: %v", err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := store.ConfirmActivation(canceled, claim, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled confirmation: %v", err)
	}
	first, err := store.ConfirmActivation(ctx, claim, target)
	if err != nil || first.Status != StatusRunning || first.FinishedAt == nil {
		t.Fatalf("first confirmation: %+v, %v", first, err)
	}
	activationStoreTestActive(t, ctx, store, claim.ProjectID, target)
	confirmed, err := store.GetActivation(ctx, claim.ID)
	if err != nil || confirmed.ConfirmedAt == nil || confirmed.Previous != nil || confirmed.Target != target {
		t.Fatalf("confirmation was not saved: %+v, %v", confirmed, err)
	}
	if _, err := store.ConfirmActivation(ctx, claim, target); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("released claim confirmed again: %v", err)
	}
	oldRuntime, err := store.GetInstance(ctx, *candidate.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	nextClaim, nextCandidate, nextTarget := activationStoreTestCandidate(t, ctx, pool, claim.ProjectID, 'b', 32102, true)
	nextIntent, err := store.BeginActivation(ctx, nextClaim, nextTarget.ContainerID, nextTarget.HostPort)
	if err != nil || nextIntent.Previous == nil || *nextIntent.Previous != target {
		t.Fatalf("replacement lost previous binding: %+v, %v", nextIntent, err)
	}
	activationStoreTestActive(t, ctx, store, claim.ProjectID, target)
	if before, err := store.Get(ctx, claim.ID); err != nil {
		t.Fatal(err)
	} else {
		assertDeploymentEqual(t, before, first)
	}
	if running, err := store.ConfirmActivation(ctx, nextClaim, nextTarget); err != nil || running.Status != StatusRunning || running.FinishedAt == nil {
		t.Fatalf("replacement confirmation: %+v, %v", running, err)
	}
	activationStoreTestActive(t, ctx, store, claim.ProjectID, nextTarget)
	previous, err := store.Get(ctx, claim.ID)
	if err != nil || previous.Status != StatusSuperseded {
		t.Fatalf("old attempt not superseded: %+v, %v", previous, err)
	}
	wantPrevious := first
	wantPrevious.Status, wantPrevious.UpdatedAt = StatusSuperseded, previous.UpdatedAt
	assertDeploymentEqual(t, previous, wantPrevious)
	if actual, err := store.GetInstance(ctx, *candidate.InstanceID); err != nil || !reflect.DeepEqual(actual, oldRuntime) || actual.Running == nil || !*actual.Running {
		t.Fatalf("route replacement invented a stopped old runtime: %+v, %v", actual, err)
	}
	if actual, err := store.GetInstance(ctx, *nextCandidate.InstanceID); err != nil || actual.Running == nil || !*actual.Running {
		t.Fatalf("new runtime observation changed: %+v, %v", actual, err)
	}
	reopened, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	activationStoreTestActive(t, ctx, NewStore(reopened), claim.ProjectID, nextTarget)
	if found, err := NewStore(reopened).GetActivation(ctx, nextClaim.ID); err != nil || found.Previous == nil || *found.Previous != target || found.ConfirmedAt == nil {
		t.Fatalf("confirmed replacement lost durable intent: %+v, %v", found, err)
	}
}

func TestActivationAbortRequiresPreviousBindingAndPreservesActiveRoute(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	claim, _, target := activationStoreTestCandidate(t, ctx, pool, "", 'a', 32101, true)
	if _, err := store.BeginActivation(ctx, claim, target.ContainerID, target.HostPort); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AbortActivation(ctx, claim, &target, "Activation failed."); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("first activation accepted an invented previous route: %v", err)
	}
	failed, err := store.AbortActivation(ctx, claim, nil, "Activation failed before a route became active.")
	if err != nil || failed.Status != StatusFailed || failed.FailureStage == nil || *failed.FailureStage != StatusActivating || failed.FinishedAt == nil {
		t.Fatalf("first activation abort: %+v, %v", failed, err)
	}
	if found, err := store.GetActivation(ctx, claim.ID); err != nil || found.Target != target || found.ConfirmedAt != nil {
		t.Fatalf("abort lost target history: %+v, %v", found, err)
	}
	if _, err := store.GetActiveRoute(ctx, claim.ProjectID); !errors.Is(err, ErrActiveRouteNotFound) {
		t.Fatalf("abort invented an active route: %v", err)
	}
	activeClaim, activeCandidate, activeTarget := activationStoreTestCandidate(t, ctx, pool, claim.ProjectID, 'b', 32102, true)
	if _, err := store.BeginActivation(ctx, activeClaim, activeTarget.ContainerID, activeTarget.HostPort); err != nil {
		t.Fatal(err)
	}
	active, err := store.ConfirmActivation(ctx, activeClaim, activeTarget)
	if err != nil {
		t.Fatal(err)
	}
	runtimeBefore, err := store.GetInstance(ctx, *activeCandidate.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	brokenClaim, _, brokenTarget := activationStoreTestCandidate(t, ctx, pool, claim.ProjectID, 'c', 32103, true)
	if _, err := store.BeginActivation(ctx, brokenClaim, brokenTarget.ContainerID, brokenTarget.HostPort); err != nil {
		t.Fatal(err)
	}
	wrongPrevious := activeTarget
	wrongPrevious.HostPort++
	for _, previous := range []*RouteBinding{nil, &wrongPrevious, &brokenTarget} {
		if _, err := store.AbortActivation(ctx, brokenClaim, previous, "Activation failed."); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("abort accepted a different restored route: %v", err)
		}
	}
	if _, err := store.AbortActivation(ctx, activationStoreTestStale(brokenClaim), &activeTarget, "Activation failed."); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stale claim aborted activation: %v", err)
	}
	if aborted, err := store.AbortActivation(ctx, brokenClaim, &activeTarget, "Previous route was restored."); err != nil || aborted.Status != StatusFailed || aborted.ReadinessPassed == nil || !*aborted.ReadinessPassed {
		t.Fatalf("replacement abort lost readiness history: %+v, %v", aborted, err)
	}
	activationStoreTestActive(t, ctx, store, claim.ProjectID, activeTarget)
	if unchanged, err := store.Get(ctx, activeClaim.ID); err != nil {
		t.Fatal(err)
	} else {
		assertDeploymentEqual(t, unchanged, active)
	}
	if unchanged, err := store.GetInstance(ctx, *activeCandidate.InstanceID); err != nil || !reflect.DeepEqual(unchanged, runtimeBefore) {
		t.Fatalf("abort altered old runtime history: %+v, %v", unchanged, err)
	}
}

func TestActivationConfirmationRejectsChangedRuntime(t *testing.T) {
	for _, change := range []string{"exit", "port"} {
		t.Run(change, func(t *testing.T) {
			pool, _ := newDeploymentStoreTestDatabase(t)
			store := NewStore(pool)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			claim, candidate, target := activationStoreTestCandidate(t, ctx, pool, "", 'a', 32101, true)
			if _, err := store.BeginActivation(ctx, claim, target.ContainerID, target.HostPort); err != nil {
				t.Fatal(err)
			}
			observed := Observation{ContainerID: target.ContainerID, Status: "running", Running: true, HostPort: target.HostPort + 1}
			if change == "exit" {
				observed.Status, observed.Running, observed.HostPort, observed.ExitCode = "exited", false, 0, 7
			}
			if _, err := store.RecordInstance(ctx, claim, *candidate.InstanceID, observed); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ConfirmActivation(ctx, claim, target); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("confirmed a changed runtime: %v", err)
			}
			if found, err := store.GetActivation(ctx, claim.ID); err != nil || found.ConfirmedAt != nil {
				t.Fatalf("failed confirmation modified intent: %+v, %v", found, err)
			}
			if _, err := store.GetActiveRoute(ctx, claim.ProjectID); !errors.Is(err, ErrActiveRouteNotFound) {
				t.Fatalf("failed confirmation installed a route: %v", err)
			}
		})
	}
}

func TestActivationRejectsAnUnexpectedActiveRouteChange(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	oldClaim, _, oldTarget := activationStoreTestCandidate(t, ctx, pool, "", 'a', 32101, true)
	if _, err := store.BeginActivation(ctx, oldClaim, oldTarget.ContainerID, oldTarget.HostPort); err != nil {
		t.Fatal(err)
	}
	old, err := store.ConfirmActivation(ctx, oldClaim, oldTarget)
	if err != nil {
		t.Fatal(err)
	}
	claim, _, target := activationStoreTestCandidate(t, ctx, pool, oldClaim.ProjectID, 'b', 32102, true)
	if _, err := store.BeginActivation(ctx, claim, target.ContainerID, target.HostPort); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM active_routes WHERE project_id = $1`, claim.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmActivation(ctx, claim, target); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("confirmation overwrote an unexpected active-route change: %v", err)
	}
	if _, err := store.AbortActivation(ctx, claim, &oldTarget, "Previous route was restored."); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("abort ignored an unexpected active-route change: %v", err)
	}
	if found, err := store.GetActivation(ctx, claim.ID); err != nil || found.ConfirmedAt != nil {
		t.Fatalf("refused route change modified intent: %+v, %v", found, err)
	}
	if unchanged, err := store.Get(ctx, old.ID); err != nil {
		t.Fatal(err)
	} else {
		assertDeploymentEqual(t, unchanged, old)
	}
}

func TestConcurrentActivationConfirmationHasOneWinner(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	claim, _, target := activationStoreTestCandidate(t, ctx, pool, "", 'a', 32101, true)
	if _, err := store.BeginActivation(ctx, claim, target.ContainerID, target.HostPort); err != nil {
		t.Fatal(err)
	}
	start, completed := make(chan struct{}), make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := store.ConfirmActivation(ctx, claim, target)
			completed <- err
		}()
	}
	close(start)
	winners, stale := 0, 0
	for range 2 {
		switch err := <-completed; {
		case err == nil:
			winners++
		case errors.Is(err, ErrInvalidTransition):
			stale++
		default:
			t.Fatalf("unexpected confirmation result: %v", err)
		}
	}
	if winners != 1 || stale != 1 {
		t.Fatalf("confirmation winners=%d stale=%d", winners, stale)
	}
	activationStoreTestActive(t, ctx, store, claim.ProjectID, target)
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM active_routes WHERE project_id = $1`, claim.ProjectID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("active route count=%d: %v", count, err)
	}
}

func activationStoreTestCandidate(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string, marker byte, port int, ready bool) (Claim, Deployment, RouteBinding) {
	t.Helper()
	store := NewStore(pool)
	if projectID == "" {
		projectID = createDeploymentTestProject(t, ctx, pool, "activation").ID
	}
	if _, err := store.Create(ctx, projectID); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordSourceCommit(ctx, claim, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginBuild(ctx, claim); err != nil {
		t.Fatal(err)
	}
	reference, imageID := buildRuntimeTestImage(claim)
	starting, err := store.BeginStart(ctx, claim, reference, imageID)
	if err != nil {
		t.Fatal(err)
	}
	target := RouteBinding{ProjectID: projectID, DeploymentID: claim.ID, InstanceID: *starting.InstanceID,
		ContainerID: strings.Repeat(string(marker), 64), HostPort: port}
	if _, err := store.RecordInstance(ctx, claim, target.InstanceID, Observation{ContainerID: target.ContainerID, Status: "running", Running: true, HostPort: port}); err != nil {
		t.Fatal(err)
	}
	candidate, err := store.BeginHealthCheck(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		candidate = activationStoreTestReadiness(t, ctx, store, claim, candidate, target)
	}
	return claim, candidate, target
}

func activationStoreTestReadiness(t *testing.T, ctx context.Context, store *PostgresStore, claim Claim, candidate Deployment, target RouteBinding) Deployment {
	t.Helper()
	saved, err := store.RecordReadiness(ctx, claim, *candidate.InstanceID, ReadinessObservation{ContainerID: target.ContainerID,
		HostPort: target.HostPort, Passed: true, Attempts: 1, StatusCode: 200, CheckedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func activationStoreTestStale(claim Claim) Claim {
	claim.Token = "00000000-0000-4000-8000-000000000099"
	return claim
}

func activationStoreTestActive(t *testing.T, ctx context.Context, store *PostgresStore, projectID string, expected RouteBinding) {
	t.Helper()
	if found, err := store.GetActiveRoute(ctx, projectID); err != nil || found != expected {
		t.Fatalf("active route=%+v, want %+v: %v", found, expected, err)
	}
}
