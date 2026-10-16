//go:build integration

package migrations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"shipyard/apps/api/migrations"
)

func TestBuildRuntimeMigrationPreservesSourceDataAndSupportsSafeDown(t *testing.T) {
	for _, status := range []string{"QUEUED", "FETCHING_SOURCE", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			db, _ := newMigrationTestDatabase(t)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			provider, err := migrations.NewProvider(db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.UpTo(ctx, 3); err != nil {
				t.Fatal(err)
			}
			_, queued := createSourceMigrationDeployment(t, ctx, db)
			if status == "FETCHING_SOURCE" {
				_, err = db.ExecContext(ctx, `UPDATE deployments SET status = 'FETCHING_SOURCE',
					started_at = now(), claim_token = gen_random_uuid(), commit_sha = $2 WHERE id = $1`, queued.ID, strings.Repeat("a", 40))
			} else if status == "FAILED" {
				_, err = db.ExecContext(ctx, `UPDATE deployments SET status = 'FAILED',
					started_at = now(), finished_at = now(), failure_stage = 'FETCHING_SOURCE',
					failure_reason = 'Source fetch failed.', commit_sha = $2 WHERE id = $1`, queued.ID, strings.Repeat("a", 40))
			}
			if err != nil {
				t.Fatal(err)
			}
			before := sourceMigrationRowJSON(t, ctx, db, queued.ID)
			results, err := provider.UpTo(ctx, 4)
			if err != nil || len(results) != 1 {
				t.Fatalf("upgrade: %d migrations, %v", len(results), err)
			}
			var preserved string
			if err := db.QueryRowContext(ctx, `SELECT (to_jsonb(d) - 'image_reference' - 'image_id' - 'instance_id')::text
				FROM deployments d WHERE id = $1`, queued.ID).Scan(&preserved); err != nil || preserved != before {
				t.Fatalf("upgrade changed source data: %v", err)
			}
			upgraded := sourceMigrationRowJSON(t, ctx, db, queued.ID)
			if results, err := provider.Up(ctx); err != nil || len(results) != 0 {
				t.Fatalf("rerun: %d migrations, %v", len(results), err)
			}
			if sourceMigrationRowJSON(t, ctx, db, queued.ID) != upgraded {
				t.Fatal("rerun changed execution data")
			}
			results, err = provider.DownTo(ctx, 3)
			if err != nil || len(results) != 1 {
				t.Fatalf("safe down: %d migrations, %v", len(results), err)
			}
			if sourceMigrationRowJSON(t, ctx, db, queued.ID) != before {
				t.Fatal("safe down changed source history")
			}
			var removed bool
			if err := db.QueryRowContext(ctx, `SELECT to_regclass('runtime_instances') IS NULL`).Scan(&removed); err != nil || !removed {
				t.Fatalf("runtime table remained: %v", err)
			}
		})
	}
}

func TestBuildRuntimeSchemaRejectsPartialMetadataAndLossyDown(t *testing.T) {
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
	project, queued := createSourceMigrationDeployment(t, ctx, db)
	for _, query := range []string{
		`UPDATE deployments SET status = 'BUILDING', started_at = now(), claim_token = gen_random_uuid() WHERE id = $1`,
		`UPDATE deployments SET status = 'STARTING', started_at = now(), claim_token = gen_random_uuid(), commit_sha = repeat('a',40) WHERE id = $1`,
		`UPDATE deployments SET image_id = 'sha256:' || repeat('a',64) WHERE id = $1`,
	} {
		_, err := db.ExecContext(ctx, query, queued.ID)
		assertSourceMigrationConstraint(t, err, "23514", "")
	}
	if _, err := db.ExecContext(ctx, `UPDATE deployments SET status = 'BUILDING', started_at = now(),
		claim_token = gen_random_uuid(), commit_sha = repeat('a',40) WHERE id = $1`, queued.ID); err != nil {
		t.Fatal(err)
	}
	before := sourceMigrationRowJSON(t, ctx, db, queued.ID)
	if _, err := provider.DownTo(ctx, 3); err == nil {
		t.Fatal("downgrade discarded building state")
	} else {
		assertSourceMigrationConstraint(t, err, "55000", "")
	}
	if sourceMigrationRowJSON(t, ctx, db, queued.ID) != before {
		t.Fatal("refused downgrade changed state")
	}
	if version, err := provider.GetDBVersion(ctx); err != nil || version != 4 {
		t.Fatalf("refused downgrade version=%d: %v", version, err)
	}
	var instanceID string
	if err := db.QueryRowContext(ctx, `INSERT INTO runtime_instances (deployment_id) VALUES ($1) RETURNING id::text`, queued.ID).Scan(&instanceID); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE runtime_instances SET container_id = repeat('a',64) WHERE id = $1`,
		`UPDATE runtime_instances SET container_id = 'short', observed_status = 'running', running = true,
		 exit_code = 0, oom_killed = false, host_port = 32123, observed_at = now() WHERE id = $1`,
		`UPDATE runtime_instances SET container_id = repeat('a',64), observed_status = 'running', running = true,
		 exit_code = 0, oom_killed = false, observed_at = now() WHERE id = $1`,
	} {
		_, err := db.ExecContext(ctx, query, instanceID)
		assertSourceMigrationConstraint(t, err, "23514", "")
	}
	if _, err := db.ExecContext(ctx, `UPDATE deployments SET status = 'STARTING',
		image_reference = 'shipyard/deployments:' || id::text || '-' || repeat('a',32),
		image_id = 'sha256:' || repeat('a',64), instance_id = $2 WHERE id = $1`, queued.ID, instanceID); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `UPDATE deployments SET image_reference = 'shipyard/deployments:00000000-0000-4000-8000-000000000000-' || repeat('a',32) WHERE id = $1`, queued.ID)
	assertSourceMigrationConstraint(t, err, "23514", "deployments_image_check")
	// A valid-looking instance UUID must still belong to this exact deployment.
	var otherDeployment, otherInstance string
	if err := db.QueryRowContext(ctx, `INSERT INTO deployments (project_id,git_url,branch,app_port,health_path,status,started_at,finished_at,failure_stage,failure_reason)
		SELECT id,git_url,branch,app_port,health_path,'FAILED',now(),now(),'FETCHING_SOURCE','Source failed.' FROM projects WHERE id=$1 RETURNING id::text`, project.ID).Scan(&otherDeployment); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO runtime_instances (deployment_id) VALUES ($1) RETURNING id::text`, otherDeployment).Scan(&otherInstance); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `UPDATE deployments SET instance_id = $2 WHERE id = $1`, queued.ID, otherInstance)
	assertSourceMigrationConstraint(t, err, "23503", "deployments_instance_fkey")
	before = sourceMigrationRowJSON(t, ctx, db, queued.ID)
	if _, err := provider.DownTo(ctx, 3); err == nil {
		t.Fatal("downgrade discarded an instance intent")
	} else {
		assertSourceMigrationConstraint(t, err, "55000", "")
	}
	if sourceMigrationRowJSON(t, ctx, db, queued.ID) != before {
		t.Fatal("refused downgrade changed image or instance identity")
	}
}
