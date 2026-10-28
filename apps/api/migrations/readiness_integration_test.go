//go:build integration

package migrations_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"shipyard/apps/api/migrations"
)

func TestReadinessMigrationPreservesRuntimeHistoryAndSupportsSafeDown(t *testing.T) {
	db, _ := newMigrationTestDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 4); err != nil {
		t.Fatal(err)
	}
	id := createReadinessMigrationCandidate(t, ctx, db)
	before := sourceMigrationRowJSON(t, ctx, db, id)
	if results, err := provider.Up(ctx); err != nil || len(results) != 1 {
		t.Fatalf("readiness upgrade: %d migrations, %v", len(results), err)
	}
	var preserved string
	if err := db.QueryRowContext(ctx, `SELECT (to_jsonb(d) - 'readiness_passed' - 'readiness_attempts'
		- 'readiness_status_code' - 'readiness_checked_at')::text FROM deployments d WHERE id = $1`, id).Scan(&preserved); err != nil || preserved != before {
		t.Fatalf("readiness upgrade changed runtime history: %v", err)
	}
	upgraded := sourceMigrationRowJSON(t, ctx, db, id)
	if results, err := provider.Up(ctx); err != nil || len(results) != 0 {
		t.Fatalf("readiness rerun: %d migrations, %v", len(results), err)
	}
	if sourceMigrationRowJSON(t, ctx, db, id) != upgraded {
		t.Fatal("readiness rerun changed data")
	}
	if results, err := provider.DownTo(ctx, 4); err != nil || len(results) != 1 {
		t.Fatalf("safe readiness downgrade: %d migrations, %v", len(results), err)
	}
	if sourceMigrationRowJSON(t, ctx, db, id) != before {
		t.Fatal("safe readiness downgrade changed runtime history")
	}
}

func TestReadinessSchemaRejectsPartialResultsAndLossyDown(t *testing.T) {
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
	id := createReadinessMigrationCandidate(t, ctx, db)
	for _, fields := range []string{
		`readiness_passed = true`,
		`readiness_passed = true, readiness_attempts = 0, readiness_status_code = 200, readiness_checked_at = now()`,
		`readiness_passed = true, readiness_attempts = 1, readiness_status_code = 503, readiness_checked_at = now()`,
		`readiness_passed = false, readiness_attempts = 1, readiness_status_code = 99, readiness_checked_at = now()`,
		`readiness_passed = false, readiness_attempts = 1, readiness_status_code = 1000, readiness_checked_at = now()`,
		`readiness_passed = true, readiness_attempts = 1, readiness_status_code = 200, readiness_checked_at = 'infinity'`,
		`readiness_passed = true, readiness_attempts = 1, readiness_status_code = 200, readiness_checked_at = '10000-01-01 00:00:00+00'`,
	} {
		_, err := db.ExecContext(ctx, "UPDATE deployments SET "+fields+" WHERE id = $1", id)
		assertSourceMigrationConstraint(t, err, "23514", "deployments_readiness_check")
	}
	if _, err := db.ExecContext(ctx, `UPDATE deployments SET readiness_passed = false,
		readiness_attempts = 3, readiness_status_code = 0, readiness_checked_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `UPDATE deployments SET status = 'STARTING' WHERE id = $1`, id)
	assertSourceMigrationConstraint(t, err, "23514", "deployments_readiness_check")
	before := sourceMigrationRowJSON(t, ctx, db, id)
	if _, err := provider.DownTo(ctx, 4); err == nil {
		t.Fatal("downgrade discarded readiness history")
	} else {
		assertSourceMigrationConstraint(t, err, "55000", "")
	}
	if sourceMigrationRowJSON(t, ctx, db, id) != before {
		t.Fatal("refused downgrade changed readiness history")
	}
	if version, err := provider.GetDBVersion(ctx); err != nil || version != 5 {
		t.Fatalf("refused downgrade version=%d: %v", version, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE deployments SET status = 'FAILED', failure_stage = 'HEALTH_CHECKING',
		failure_reason = 'Readiness failed.', finished_at = now(), claim_token = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `UPDATE deployments SET failure_stage = 'STARTING' WHERE id = $1`, id)
	assertSourceMigrationConstraint(t, err, "23514", "deployments_readiness_check")
}

func createReadinessMigrationCandidate(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	_, queued := createSourceMigrationDeployment(t, ctx, db)
	var instanceID string
	if err := db.QueryRowContext(ctx, `INSERT INTO runtime_instances (deployment_id, container_id, observed_status,
		running, exit_code, oom_killed, host_port, observed_at)
		VALUES ($1, repeat('a',64), 'running', true, 0, false, 32123, now()) RETURNING id::text`, queued.ID).Scan(&instanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE deployments SET status = 'HEALTH_CHECKING', started_at = now(),
		claim_token = gen_random_uuid(), commit_sha = repeat('a',40), image_id = 'sha256:' || repeat('a',64),
		image_reference = 'shipyard/deployments:' || id::text || '-' || repeat('a',32), instance_id = $2
		WHERE id = $1`, queued.ID, instanceID); err != nil {
		t.Fatal(err)
	}
	return queued.ID
}
