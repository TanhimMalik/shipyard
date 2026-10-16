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

func TestBuildRuntimeLifecyclePersistsIntentAndActualObservation(t *testing.T) {
	pool, config := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	project := createDeploymentTestProject(t, ctx, pool, "build-runtime")
	if _, err := store.Create(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginBuild(ctx, claim); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("build without exact source: %v", err)
	}
	if _, err := store.RecordSourceCommit(ctx, claim, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	building, err := store.BeginBuild(ctx, claim)
	if err != nil || building.Status != StatusBuilding {
		t.Fatalf("begin build: %+v, %v", building, err)
	}
	if again, err := store.BeginBuild(ctx, claim); err != nil {
		t.Fatal(err)
	} else {
		assertDeploymentEqual(t, again, building)
	}
	if _, err := store.RecordSourceCommit(ctx, claim, strings.Repeat("b", 40)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("source changed after build began: %v", err)
	}
	if _, err := store.BeginHealthCheck(ctx, claim); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("skipped startup: %v", err)
	}
	reference, imageID := buildRuntimeTestImage(claim)
	wrong := claim
	wrong.Token = "00000000-0000-4000-8000-000000000099"
	if _, err := store.BeginStart(ctx, wrong, reference, imageID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stale build result: %v", err)
	}
	starting, err := store.BeginStart(ctx, claim, reference, imageID)
	if err != nil || starting.Status != StatusStarting || starting.InstanceID == nil || starting.ImageReference == nil || *starting.ImageReference != reference || starting.ImageID == nil || *starting.ImageID != imageID {
		t.Fatalf("begin startup: %+v, %v", starting, err)
	}
	instanceID := *starting.InstanceID
	intent, err := store.GetInstance(ctx, instanceID)
	if err != nil || intent.DeploymentID != claim.ID || intent.ContainerID != nil || intent.ObservedStatus != nil || intent.Running != nil || intent.HostPort != nil || intent.ObservedAt != nil {
		t.Fatalf("unobserved intent invented Docker state: %+v, %v", intent, err)
	}
	if again, err := store.BeginStart(ctx, claim, reference, imageID); err != nil {
		t.Fatal(err)
	} else {
		assertDeploymentEqual(t, again, starting)
	}
	if _, err := store.BeginStart(ctx, claim, reference, "sha256:"+strings.Repeat("c", 64)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("replaced immutable image: %v", err)
	}
	otherReference := "shipyard/deployments:" + claim.ID + "-" + strings.Repeat("c", 32)
	if _, err := store.BeginStart(ctx, claim, otherReference, imageID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("replaced image tag: %v", err)
	}
	if _, err := store.BeginHealthCheck(ctx, claim); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unobserved instance considered running: %v", err)
	}
	observed := Observation{ContainerID: strings.Repeat("d", 64), Status: "created"}
	if _, err := store.RecordInstance(ctx, wrong, instanceID, observed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stale instance observation: %v", err)
	}
	if _, err := store.RecordInstance(ctx, claim, "00000000-0000-4000-8000-000000000098", observed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("different intent accepted: %v", err)
	}
	created, err := store.RecordInstance(ctx, claim, instanceID, observed)
	if err != nil || created.ContainerID == nil || *created.ContainerID != observed.ContainerID || created.Running == nil || *created.Running || created.HostPort != nil {
		t.Fatalf("created observation: %+v, %v", created, err)
	}
	canceled, cancelObservation := context.WithCancel(ctx)
	cancelObservation()
	if _, err := store.RecordInstance(canceled, claim, instanceID, observed); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled observation: %v", err)
	}
	if saved, err := store.GetInstance(ctx, instanceID); err != nil || !reflect.DeepEqual(saved, created) {
		t.Fatalf("canceled observation changed state: %+v, %v", saved, err)
	}
	if _, err := store.BeginHealthCheck(ctx, claim); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("created instance considered running: %v", err)
	}
	otherContainer := observed
	otherContainer.ContainerID = strings.Repeat("e", 64)
	if _, err := store.RecordInstance(ctx, claim, instanceID, otherContainer); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("changed pinned Docker ID: %v", err)
	}
	observed.Status, observed.Running, observed.HostPort = "running", true, 32123
	if _, err := store.RecordInstance(ctx, claim, instanceID, observed); err != nil {
		t.Fatal(err)
	}
	health, err := store.BeginHealthCheck(ctx, claim)
	if err != nil || health.Status != StatusHealthChecking || health.FinishedAt != nil {
		t.Fatalf("begin health checks: %+v, %v", health, err)
	}
	if again, err := store.BeginHealthCheck(ctx, claim); err != nil {
		t.Fatal(err)
	} else {
		assertDeploymentEqual(t, again, health)
	}
	if _, err := store.Create(ctx, project.ID); !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("unfinished health attempt lost slot: %v", err)
	}
	if _, err := store.FailExecution(ctx, claim, StatusStarting, "Startup failed."); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stale stage failed health checks: %v", err)
	}
	observed.Status, observed.Running, observed.HostPort, observed.ExitCode, observed.OOMKilled = "exited", false, 0, 137, true
	exited, err := store.RecordInstance(ctx, claim, instanceID, observed)
	if err != nil || exited.ExitCode == nil || *exited.ExitCode != 137 || exited.OOMKilled == nil || !*exited.OOMKilled || exited.HostPort != nil {
		t.Fatalf("actual exit observation: %+v, %v", exited, err)
	}
	if _, err := store.BeginHealthCheck(ctx, claim); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("exited instance considered running: %v", err)
	}
	failed, err := store.FailExecution(ctx, claim, StatusHealthChecking, "Application exited before readiness.")
	if err != nil || failed.Status != StatusFailed || failed.FailureStage == nil || *failed.FailureStage != StatusHealthChecking || failed.FinishedAt == nil || failed.InstanceID == nil || *failed.InstanceID != instanceID || failed.ImageID == nil || *failed.ImageID != imageID {
		t.Fatalf("failed candidate lost execution history: %+v, %v", failed, err)
	}
	if _, err := store.RecordInstance(ctx, claim, instanceID, observed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal owner wrote observation: %v", err)
	}
	if _, err := store.FailExecution(ctx, claim, StatusHealthChecking, "Overwrite failure."); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal failure overwritten: %v", err)
	}
	freshPool, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(freshPool.Close)
	fresh := NewStore(freshPool)
	if saved, err := fresh.Get(ctx, claim.ID); err != nil {
		t.Fatal(err)
	} else {
		assertDeploymentEqual(t, saved, failed)
	}
	if saved, err := fresh.GetInstance(ctx, instanceID); err != nil || !reflect.DeepEqual(saved, exited) {
		t.Fatalf("fresh store lost actual observation: %+v, %v", saved, err)
	}
	if next, err := fresh.Create(ctx, project.ID); err != nil || next.ID == claim.ID {
		t.Fatalf("queue after candidate failure: %+v, %v", next, err)
	}
}

func TestConcurrentStartupIntentHasOneIdentityAndFailureKeepsIt(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	project := createDeploymentTestProject(t, ctx, pool, "concurrent-start")
	claim := buildRuntimeTestClaim(t, ctx, store, project.ID)
	reference, imageID := buildRuntimeTestImage(claim)
	type result struct {
		deployment Deployment
		err        error
	}
	start, results := make(chan struct{}), make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			deployment, err := store.BeginStart(ctx, claim, reference, imageID)
			results <- result{deployment, err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.deployment.InstanceID == nil || second.deployment.InstanceID == nil || *first.deployment.InstanceID != *second.deployment.InstanceID {
		t.Fatalf("duplicate intents: %+v / %+v", first, second)
	}
	assertDeploymentEqual(t, first.deployment, second.deployment)
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM runtime_instances WHERE deployment_id = $1", claim.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("instance intents=%d, %v", count, err)
	}
	if _, err := store.Create(ctx, project.ID); !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("startup attempt lost unfinished slot: %v", err)
	}
	if _, err := store.FailExecution(ctx, claim, StatusStarting, "Container could not start."); err != nil {
		t.Fatal(err)
	}
	if intent, err := store.GetInstance(ctx, *first.deployment.InstanceID); err != nil || intent.ContainerID != nil {
		t.Fatalf("failed intent was deleted or invented state: %+v, %v", intent, err)
	}
	if _, err := store.BeginStart(ctx, claim, reference, imageID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal intent reused: %v", err)
	}
}

func TestBuildFailureAndCanceledStorageKeepConsistentState(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	project := createDeploymentTestProject(t, ctx, pool, "build-failure")
	claim := buildRuntimeTestClaim(t, ctx, store, project.ID)
	before, err := store.Get(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, project.ID); !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("build lost unfinished slot: %v", err)
	}
	canceled, cancelOperation := context.WithCancel(ctx)
	cancelOperation()
	reference, imageID := buildRuntimeTestImage(claim)
	if _, err := store.BeginBuild(canceled, claim); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled build transition: %v", err)
	}
	if _, err := store.BeginStart(canceled, claim, reference, imageID); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled startup intent: %v", err)
	}
	if _, err := store.BeginHealthCheck(canceled, claim); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled readiness transition: %v", err)
	}
	if _, err := store.FailExecution(canceled, claim, StatusBuilding, "Build failed."); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled failure write: %v", err)
	}
	if saved, err := store.Get(ctx, claim.ID); err != nil {
		t.Fatal(err)
	} else {
		assertDeploymentEqual(t, saved, before)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM runtime_instances").Scan(&count); err != nil || count != 0 {
		t.Fatalf("canceled intent leaked a row: %d, %v", count, err)
	}
	failed, err := store.FailExecution(ctx, claim, StatusBuilding, "Dockerfile build failed.")
	if err != nil || failed.CommitSHA == nil || failed.ImageID != nil || failed.InstanceID != nil || failed.FailureStage == nil || *failed.FailureStage != StatusBuilding {
		t.Fatalf("build failure: %+v, %v", failed, err)
	}
	if _, err := store.Create(ctx, project.ID); err != nil {
		t.Fatalf("build failure did not release slot: %v", err)
	}
}

func buildRuntimeTestClaim(t *testing.T, ctx context.Context, store *PostgresStore, projectID string) Claim {
	t.Helper()
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
	return claim
}

func buildRuntimeTestImage(claim Claim) (string, string) {
	return "shipyard/deployments:" + claim.ID + "-" + strings.Repeat("b", 32), "sha256:" + strings.Repeat("b", 64)
}

func TestStartupIntentRollsBackIfDeploymentWriteFails(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	project := createDeploymentTestProject(t, ctx, pool, "intent-rollback")
	claim := buildRuntimeTestClaim(t, ctx, store, project.ID)
	before, err := store.Get(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION reject_test_start() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'test startup write failure'; END $$;
		CREATE TRIGGER reject_test_start BEFORE UPDATE ON deployments
		FOR EACH ROW WHEN (NEW.status = 'STARTING') EXECUTE FUNCTION reject_test_start()`); err != nil {
		t.Fatal(err)
	}
	reference, imageID := buildRuntimeTestImage(claim)
	if _, err := store.BeginStart(ctx, claim, reference, imageID); err == nil {
		t.Fatal("injected deployment write unexpectedly succeeded")
	}
	if saved, err := store.Get(ctx, claim.ID); err != nil {
		t.Fatal(err)
	} else {
		assertDeploymentEqual(t, saved, before)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM runtime_instances").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed transaction left %d intents: %v", count, err)
	}
}
