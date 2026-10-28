//go:build integration

package migrations_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"shipyard/apps/api/migrations"
)

func TestActivationMigrationPreservesReadinessAndSupportsSafeDown(t *testing.T) {
	db, _ := newMigrationTestDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 5); err != nil {
		t.Fatal(err)
	}
	candidate := createActivationMigrationCandidate(t, ctx, db, "preserved")
	before := sourceMigrationRowJSON(t, ctx, db, candidate.deploymentID)
	var runtimeBefore string
	if err := db.QueryRowContext(ctx, `SELECT to_jsonb(i)::text FROM runtime_instances i WHERE id = $1`, candidate.instanceID).Scan(&runtimeBefore); err != nil {
		t.Fatal(err)
	}
	if results, err := provider.Up(ctx); err != nil || len(results) != 1 {
		t.Fatalf("activation upgrade: %d migrations, %v", len(results), err)
	}
	if sourceMigrationRowJSON(t, ctx, db, candidate.deploymentID) != before {
		t.Fatal("activation upgrade changed deployment history")
	}
	if results, err := provider.Up(ctx); err != nil || len(results) != 0 {
		t.Fatalf("activation rerun: %d migrations, %v", len(results), err)
	}
	if results, err := provider.DownTo(ctx, 5); err != nil || len(results) != 1 {
		t.Fatalf("safe activation downgrade: %d migrations, %v", len(results), err)
	}
	if sourceMigrationRowJSON(t, ctx, db, candidate.deploymentID) != before {
		t.Fatal("activation migration cycle changed readiness history")
	}
	var runtimeAfter string
	if err := db.QueryRowContext(ctx, `SELECT to_jsonb(i)::text FROM runtime_instances i WHERE id = $1`, candidate.instanceID).Scan(&runtimeAfter); err != nil || runtimeAfter != runtimeBefore {
		t.Fatalf("activation migration cycle changed runtime history: %v", err)
	}
	var removed bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('activation_intents') IS NULL AND to_regclass('active_routes') IS NULL`).Scan(&removed); err != nil || !removed {
		t.Fatalf("safe downgrade retained activation tables: %v", err)
	}
}

func TestActivationSchemaRequiresReadinessAndKeepsOnlyUnfinishedSlot(t *testing.T) {
	db, _ := newMigrationTestDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	candidate := createActivationMigrationCandidate(t, ctx, db, "lifecycle")
	for _, fields := range []string{
		`status = 'ACTIVATING', readiness_passed = NULL, readiness_attempts = NULL, readiness_status_code = NULL, readiness_checked_at = NULL`,
		`status = 'ACTIVATING', readiness_passed = false`,
		`status = 'ACTIVATING', claim_token = NULL`,
		`status = 'RUNNING', finished_at = now()`,
		`status = 'RUNNING', claim_token = NULL`,
		`status = 'SUPERSEDED', finished_at = now()`,
	} {
		_, err := db.ExecContext(ctx, "UPDATE deployments SET "+fields+" WHERE id = $1", candidate.deploymentID)
		assertSourceMigrationConstraint(t, err, "23514", "deployments_execution_state_check")
	}
	if _, err := db.ExecContext(ctx, `UPDATE deployments SET status = 'ACTIVATING' WHERE id = $1`, candidate.deploymentID); err != nil {
		t.Fatal(err)
	}
	queue := func() error {
		_, err := db.ExecContext(ctx, `INSERT INTO deployments (project_id, git_url, branch, app_port, health_path)
			SELECT id, git_url, branch, app_port, health_path FROM projects WHERE id = $1`, candidate.projectID)
		return err
	}
	assertSourceMigrationConstraint(t, queue(), "23505", "deployments_one_unfinished_per_project")
	if _, err := db.ExecContext(ctx, `UPDATE deployments SET status = 'RUNNING', finished_at = now(), claim_token = NULL WHERE id = $1`, candidate.deploymentID); err != nil {
		t.Fatal(err)
	}
	if err := queue(); err != nil {
		t.Fatalf("completed activation kept unfinished slot: %v", err)
	}
	var finishedAt time.Time
	if err := db.QueryRowContext(ctx, `SELECT finished_at FROM deployments WHERE id = $1`, candidate.deploymentID).Scan(&finishedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE deployments SET status = 'SUPERSEDED' WHERE id = $1`, candidate.deploymentID); err != nil {
		t.Fatal(err)
	}
	var stillRunning bool
	var preserved time.Time
	if err := db.QueryRowContext(ctx, `SELECT d.finished_at, i.running FROM deployments d JOIN runtime_instances i ON i.id = d.instance_id WHERE d.id = $1`, candidate.deploymentID).Scan(&preserved, &stillRunning); err != nil || !preserved.Equal(finishedAt) || !stillRunning {
		t.Fatalf("supersession changed process or completion history: %v", err)
	}
	if _, err := provider.DownTo(ctx, 5); err == nil {
		t.Fatal("downgrade discarded a completed activation state")
	} else {
		assertSourceMigrationConstraint(t, err, "55000", "")
	}
}

func TestActivationSchemaProtectsBindingIdentityAndConfirmedRoutes(t *testing.T) {
	db, _ := newMigrationTestDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	first := createActivationMigrationCandidate(t, ctx, db, "first")
	other := createActivationMigrationCandidate(t, ctx, db, "other")
	insert := func(deployment, project, instance, container string, port int, previous any) error {
		_, err := db.ExecContext(ctx, `INSERT INTO activation_intents
			(deployment_id, project_id, instance_id, container_id, host_port, previous_deployment_id)
			VALUES ($1, $2, $3, $4, $5, $6)`, deployment, project, instance, container, port, previous)
		return err
	}
	assertSourceMigrationConstraint(t, insert(first.deploymentID, other.projectID, first.instanceID, first.containerID, 32123, nil), "23503", "activation_intents_deployment_fkey")
	assertSourceMigrationConstraint(t, insert(first.deploymentID, first.projectID, other.instanceID, first.containerID, 32123, nil), "23503", "activation_intents_instance_fkey")
	for _, invalid := range []struct {
		container string
		port      int
	}{
		{"short", 32123}, {strings.Repeat("A", 64), 32123}, {first.containerID, 0}, {first.containerID, 65536},
	} {
		assertSourceMigrationConstraint(t, insert(first.deploymentID, first.projectID, first.instanceID, invalid.container, invalid.port, nil), "23514", "")
	}
	assertSourceMigrationConstraint(t, insert(first.deploymentID, first.projectID, first.instanceID, first.containerID, 32123, first.deploymentID), "23514", "")
	if err := insert(other.deploymentID, other.projectID, other.instanceID, other.containerID, 32123, nil); err != nil {
		t.Fatal(err)
	}
	assertSourceMigrationConstraint(t, insert(first.deploymentID, first.projectID, first.instanceID, first.containerID, 32123, other.deploymentID), "23503", "activation_intents_previous_fkey")
	if err := insert(first.deploymentID, first.projectID, first.instanceID, first.containerID, 32123, nil); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO active_routes (project_id, deployment_id, confirmed_at) VALUES ($1, $2, now())`, first.projectID, first.deploymentID)
	assertSourceMigrationConstraint(t, err, "23503", "active_routes_intent_fkey")
	if _, err := db.ExecContext(ctx, `UPDATE activation_intents SET confirmed_at = now() WHERE deployment_id = $1`, first.deploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO active_routes (project_id, deployment_id, confirmed_at)
		SELECT project_id, deployment_id, confirmed_at FROM activation_intents WHERE deployment_id = $1`, first.deploymentID); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO active_routes (project_id, deployment_id, confirmed_at)
		SELECT $2, deployment_id, confirmed_at FROM activation_intents WHERE deployment_id = $1`, first.deploymentID, other.projectID)
	assertSourceMigrationConstraint(t, err, "23503", "active_routes_intent_fkey")
	before := sourceMigrationRowJSON(t, ctx, db, first.deploymentID)
	if _, err := provider.DownTo(ctx, 5); err == nil {
		t.Fatal("downgrade discarded activation bindings")
	} else {
		assertSourceMigrationConstraint(t, err, "55000", "")
	}
	if sourceMigrationRowJSON(t, ctx, db, first.deploymentID) != before {
		t.Fatal("refused activation downgrade changed deployment history")
	}
	if version, err := provider.GetDBVersion(ctx); err != nil || version != 6 {
		t.Fatalf("refused downgrade version=%d: %v", version, err)
	}
}

type activationMigrationCandidate struct {
	deploymentID string
	projectID    string
	instanceID   string
	containerID  string
}

func createActivationMigrationCandidate(t *testing.T, ctx context.Context, db *sql.DB, slug string) activationMigrationCandidate {
	t.Helper()
	var candidate activationMigrationCandidate
	if err := db.QueryRowContext(ctx, `INSERT INTO projects (name, slug, git_url, branch, app_port, health_path)
		VALUES ('Activation project', $1, 'https://example.com/activation.git', 'main', 8080, '/ready') RETURNING id::text`, slug).Scan(&candidate.projectID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO deployments (project_id, git_url, branch, app_port, health_path)
		SELECT id, git_url, branch, app_port, health_path FROM projects WHERE id = $1 RETURNING id::text`, candidate.projectID).Scan(&candidate.deploymentID); err != nil {
		t.Fatal(err)
	}
	candidate.containerID = strings.Repeat(strings.ReplaceAll(candidate.deploymentID, "-", ""), 2)
	if err := db.QueryRowContext(ctx, `INSERT INTO runtime_instances (deployment_id, container_id, observed_status,
		running, exit_code, oom_killed, host_port, observed_at)
		VALUES ($1, $2, 'running', true, 0, false, 32123, now()) RETURNING id::text`, candidate.deploymentID, candidate.containerID).Scan(&candidate.instanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE deployments SET status = 'HEALTH_CHECKING', started_at = now(),
		claim_token = gen_random_uuid(), commit_sha = repeat('a',40), image_id = 'sha256:' || repeat('a',64),
		image_reference = 'shipyard/deployments:' || id::text || '-' || repeat('a',32), instance_id = $2,
		readiness_passed = true, readiness_attempts = 1, readiness_status_code = 200, readiness_checked_at = now()
		WHERE id = $1`, candidate.deploymentID, candidate.instanceID); err != nil {
		t.Fatal(err)
	}
	return candidate
}
