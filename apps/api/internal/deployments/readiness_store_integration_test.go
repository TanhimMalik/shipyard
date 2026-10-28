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

func TestReadinessResultPersistsWithoutActivatingAndIsWriteOnce(t *testing.T) {
	pool, config := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	claim, deployment, observed := readinessStoreTestCandidate(t, ctx, pool)
	if deployment.ReadinessPassed != nil || deployment.ReadinessAttempts != nil || deployment.ReadinessStatusCode != nil || deployment.ReadinessCheckedAt != nil {
		t.Fatal("health transition invented a readiness observation")
	}
	wrong := claim
	wrong.Token = "00000000-0000-4000-8000-000000000099"
	if _, err := store.RecordReadiness(ctx, wrong, *deployment.InstanceID, observed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stale readiness write: %v", err)
	}
	if _, err := store.RecordReadiness(ctx, claim, "00000000-0000-4000-8000-000000000098", observed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("different instance accepted: %v", err)
	}
	for _, change := range []func(*ReadinessObservation){
		func(o *ReadinessObservation) { o.ContainerID = strings.Repeat("e", 64) },
		func(o *ReadinessObservation) { o.HostPort++ },
	} {
		mismatched := observed
		change(&mismatched)
		if _, err := store.RecordReadiness(ctx, claim, *deployment.InstanceID, mismatched); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("different Docker target accepted: %v", err)
		}
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := store.RecordReadiness(canceled, claim, *deployment.InstanceID, observed); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled readiness write: %v", err)
	}
	before, err := store.Get(ctx, claim.ID)
	if err != nil || before.ReadinessPassed != nil {
		t.Fatalf("canceled result changed history: %+v, %v", before, err)
	}
	saved, err := store.RecordReadiness(ctx, claim, *deployment.InstanceID, observed)
	if err != nil || saved.Status != StatusHealthChecking || saved.FinishedAt != nil || saved.ReadinessPassed == nil || !*saved.ReadinessPassed ||
		saved.ReadinessStatusCode == nil || *saved.ReadinessStatusCode != observed.StatusCode || saved.ReadinessAttempts == nil || *saved.ReadinessAttempts != observed.Attempts ||
		saved.ReadinessCheckedAt == nil || !saved.ReadinessCheckedAt.Equal(observed.CheckedAt.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("readiness result was lost or activated the candidate: %+v, %v", saved, err)
	}
	if again, err := store.RecordReadiness(ctx, claim, *deployment.InstanceID, observed); err != nil {
		t.Fatal(err)
	} else {
		assertDeploymentEqual(t, again, saved)
	}
	changed := observed
	changed.Attempts++
	if _, err := store.RecordReadiness(ctx, claim, *deployment.InstanceID, changed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("replaced a final observation: %v", err)
	}
	if _, err := store.Create(ctx, claim.ProjectID); !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("unrouted readiness result released unfinished slot: %v", err)
	}
	freshPool, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer freshPool.Close()
	if fresh, err := NewStore(freshPool).Get(ctx, saved.ID); err != nil {
		t.Fatal(err)
	} else {
		assertDeploymentEqual(t, fresh, saved)
	}
	failed, err := store.FailExecution(ctx, claim, StatusHealthChecking, "Deployment was interrupted before routing.")
	if err != nil || failed.ReadinessPassed == nil || !*failed.ReadinessPassed || failed.ReadinessCheckedAt == nil || !failed.ReadinessCheckedAt.Equal(*saved.ReadinessCheckedAt) {
		t.Fatalf("terminalization erased the readiness history: %+v, %v", failed, err)
	}
	if _, err := store.RecordReadiness(ctx, claim, *deployment.InstanceID, observed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal attempt accepted observation: %v", err)
	}
}

func TestReadinessFailureCanFollowAnObservedRuntimeChange(t *testing.T) {
	for _, change := range []string{"exit", "port changed"} {
		t.Run(change, func(t *testing.T) {
			pool, _ := newDeploymentStoreTestDatabase(t)
			store := NewStore(pool)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			claim, deployment, observed := readinessStoreTestCandidate(t, ctx, pool)
			actual := Observation{ContainerID: observed.ContainerID, Status: "exited", ExitCode: 7}
			if change == "port changed" {
				actual = Observation{ContainerID: observed.ContainerID, Status: "running", Running: true, HostPort: observed.HostPort + 1}
			}
			if _, err := store.RecordInstance(ctx, claim, *deployment.InstanceID, actual); err != nil {
				t.Fatal(err)
			}
			if _, err := store.RecordReadiness(ctx, claim, *deployment.InstanceID, observed); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("HTTP success promoted a changed candidate: %v", err)
			}
			observed.Passed = false
			saved, err := store.RecordReadiness(ctx, claim, *deployment.InstanceID, observed)
			if err != nil || saved.ReadinessPassed == nil || *saved.ReadinessPassed || saved.ReadinessStatusCode == nil || *saved.ReadinessStatusCode != 200 {
				t.Fatalf("lost the response from before the runtime changed: %+v, %v", saved, err)
			}
			if _, err := store.FailExecution(ctx, claim, StatusHealthChecking, "Application changed before readiness completed."); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func readinessStoreTestCandidate(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (Claim, Deployment, ReadinessObservation) {
	t.Helper()
	store := NewStore(pool)
	project := createDeploymentTestProject(t, ctx, pool, "readiness")
	if _, err := store.Create(ctx, project.ID); err != nil {
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
	observed := ReadinessObservation{ContainerID: strings.Repeat("d", 64), HostPort: 32123, Passed: true, Attempts: 3,
		StatusCode: 200, CheckedAt: time.Now().In(time.FixedZone("test", 2*60*60))}
	if _, err := store.RecordReadiness(ctx, claim, *starting.InstanceID, observed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("readiness skipped health stage: %v", err)
	}
	if _, err := store.RecordInstance(ctx, claim, *starting.InstanceID, Observation{ContainerID: observed.ContainerID, Status: "running", Running: true, HostPort: observed.HostPort}); err != nil {
		t.Fatal(err)
	}
	deployment, err := store.BeginHealthCheck(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	return claim, deployment, observed
}
