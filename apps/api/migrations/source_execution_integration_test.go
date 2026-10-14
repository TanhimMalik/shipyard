//go:build integration

package migrations_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"shipyard/apps/api/migrations"
)

func TestSourceExecutionMigrationPreservesQueuedDataAndSupportsSafeDown(t *testing.T) {
	db, _ := newMigrationTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 2); err != nil {
		t.Fatalf("apply queued deployment schema: %v", err)
	}
	project, queued := createSourceMigrationDeployment(t, ctx, db)
	readQueued := func() migrationDeploymentRecord {
		return scanMigrationDeployment(t, db.QueryRowContext(ctx, `
			SELECT id::text, project_id::text, git_url, branch, app_port, health_path,
			       status, created_at, updated_at FROM deployments WHERE id = $1`, queued.ID))
	}
	readProject := func() projectRecord {
		return scanProject(t, db.QueryRowContext(ctx, `
			SELECT id::text, name, slug, git_url, branch, app_port, health_path,
			       created_at, updated_at FROM projects WHERE id = $1`, project.ID))
	}
	results, err := provider.UpTo(ctx, 3)
	if err != nil || len(results) != 1 {
		t.Fatalf("upgrade queued schema: applied %d, error = %v; want 1", len(results), err)
	}
	if got := readQueued(); got != queued {
		t.Errorf("upgrade changed queued snapshot: got %+v, want %+v", got, queued)
	}
	if got := readProject(); got != project {
		t.Errorf("upgrade changed project: got %+v, want %+v", got, project)
	}
	var untouched bool
	if err := db.QueryRowContext(ctx, `
		SELECT commit_sha IS NULL AND failure_stage IS NULL AND failure_reason IS NULL
		       AND started_at IS NULL AND finished_at IS NULL AND claim_token IS NULL
		FROM deployments WHERE id = $1`, queued.ID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("upgraded queued lifecycle is empty = %t, error = %v", untouched, err)
	}
	beforeRerun := sourceMigrationRowJSON(t, ctx, db, queued.ID)
	rerun, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	results, err = rerun.Up(ctx)
	if err != nil || len(results) != 0 {
		t.Fatalf("rerun source migration: applied %d, error = %v; want 0", len(results), err)
	}
	if got := sourceMigrationRowJSON(t, ctx, db, queued.ID); got != beforeRerun {
		t.Error("migration rerun changed queued data")
	}
	version, err := rerun.GetDBVersion(ctx)
	if err != nil || version != 3 {
		t.Fatalf("upgraded version = %d, error = %v; want 3", version, err)
	}

	results, err = rerun.DownTo(ctx, 2)
	if err != nil || len(results) != 1 {
		t.Fatalf("revert queued-only source migration: applied %d, error = %v; want 1", len(results), err)
	}
	version, err = rerun.GetDBVersion(ctx)
	if err != nil || version != 2 {
		t.Fatalf("safe downgrade version = %d, error = %v; want 2", version, err)
	}
	if got := readQueued(); got != queued {
		t.Errorf("safe downgrade changed queued snapshot: got %+v, want %+v", got, queued)
	}
	if got := readProject(); got != project {
		t.Errorf("safe downgrade changed project: got %+v, want %+v", got, project)
	}
	var removedColumns int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'deployments'
		  AND column_name IN ('commit_sha', 'failure_stage', 'failure_reason', 'started_at', 'finished_at', 'claim_token')`).Scan(&removedColumns); err != nil || removedColumns != 0 {
		t.Errorf("source columns after safe downgrade = %d, error = %v; want 0", removedColumns, err)
	}
	_, err = db.ExecContext(ctx, "UPDATE deployments SET status = 'FETCHING_SOURCE' WHERE id = $1", queued.ID)
	assertSourceMigrationConstraint(t, err, "23514", "deployments_status_check")
	_, err = db.ExecContext(ctx, `
		INSERT INTO deployments (project_id, git_url, branch, app_port, health_path)
		SELECT id, git_url, branch, app_port, health_path FROM projects WHERE id = $1`, project.ID)
	assertSourceMigrationConstraint(t, err, "23505", "deployments_one_queued_per_project")
}

func TestSourceExecutionSchemaEnforcesLifecycleAndUnfinishedUniqueness(t *testing.T) {
	db, _ := newMigrationTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 3); err != nil {
		t.Fatal(err)
	}
	project, queued := createSourceMigrationDeployment(t, ctx, db)
	const token = "00000000-0000-4000-8000-000000000001"
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	finish := start.Add(time.Second)
	values := func(status string) []any {
		v := []any{status, nil, nil, nil, nil, nil, nil}
		switch status {
		case "FETCHING_SOURCE":
			v[4], v[6] = start, token
		case "FAILED":
			v[2], v[3], v[4], v[5] = "FETCHING_SOURCE", "Source fetch did not complete.", start, finish
		}
		return v
	}
	update := func(v []any) error {
		_, err := db.ExecContext(ctx, `
			UPDATE deployments SET status = $2, commit_sha = $3, failure_stage = $4,
			       failure_reason = $5, started_at = $6, finished_at = $7, claim_token = $8
			WHERE id = $1`, append([]any{queued.ID}, v...)...)
		return err
	}
	beforeInvalid := sourceMigrationRowJSON(t, ctx, db, queued.ID)
	tests := []struct {
		name       string
		status     string
		field      int
		value      any
		constraint string
	}{
		{"queued commit", "QUEUED", 1, strings.Repeat("a", 40), "deployments_source_state_check"},
		{"queued claim", "QUEUED", 6, token, "deployments_source_state_check"},
		{"fetching without start", "FETCHING_SOURCE", 4, nil, "deployments_source_state_check"},
		{"fetching without claim", "FETCHING_SOURCE", 6, nil, "deployments_source_state_check"},
		{"fetching already finished", "FETCHING_SOURCE", 5, finish, "deployments_source_state_check"},
		{"fetching with failure", "FETCHING_SOURCE", 3, "Failure.", "deployments_source_state_check"},
		{"failed without finish", "FAILED", 5, nil, "deployments_source_state_check"},
		{"failed without stage", "FAILED", 2, nil, "deployments_source_state_check"},
		{"failed wrong stage", "FAILED", 2, "BUILDING", "deployments_source_state_check"},
		{"failed without reason", "FAILED", 3, nil, "deployments_source_state_check"},
		{"failed retains claim", "FAILED", 6, token, "deployments_source_state_check"},
		{"unsupported status", "BUILDING", 0, "BUILDING", ""},
		{"short commit", "FETCHING_SOURCE", 1, strings.Repeat("a", 39), "deployments_commit_sha_check"},
		{"uppercase commit", "FETCHING_SOURCE", 1, strings.Repeat("A", 40), "deployments_commit_sha_check"},
		{"untrimmed reason", "FAILED", 3, " Failure.", "deployments_failure_reason_check"},
		{"empty reason", "FAILED", 3, "", "deployments_failure_reason_check"},
		{"long reason", "FAILED", 3, strings.Repeat("r", 1025), "deployments_failure_reason_check"},
		{"control in reason", "FAILED", 3, "Failure.\nPrivate output.", "deployments_failure_reason_check"},
		{"finish before start", "FAILED", 5, start.Add(-time.Second), "deployments_source_timestamps_check"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			v := values(test.status)
			v[test.field] = test.value
			assertSourceMigrationConstraint(t, update(v), "23514", test.constraint)
			if got := sourceMigrationRowJSON(t, ctx, db, queued.ID); got != beforeInvalid {
				t.Error("rejected lifecycle update changed queued data")
			}
		})
	}
	insertQueued := func() error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO deployments (project_id, git_url, branch, app_port, health_path)
			SELECT id, git_url, branch, app_port, health_path FROM projects WHERE id = $1`, project.ID)
		return err
	}
	assertSourceMigrationConstraint(t, insertQueued(), "23505", "deployments_one_unfinished_per_project")
	for _, commit := range []any{nil, strings.Repeat("a", 40), strings.Repeat("b", 64)} {
		v := values("FETCHING_SOURCE")
		v[1] = commit
		if err := update(v); err != nil {
			t.Fatalf("accept fetching state with commit %v: %v", commit, err)
		}
		assertSourceMigrationConstraint(t, insertQueued(), "23505", "deployments_one_unfinished_per_project")
	}
	v := values("FAILED")
	v[1], v[3] = strings.Repeat("b", 64), strings.Repeat("é", 1024)
	if err := update(v); err != nil {
		t.Fatalf("accept failed state with preserved commit and maximum character-count reason: %v", err)
	}
	failed := sourceMigrationRowJSON(t, ctx, db, queued.ID)
	if err := insertQueued(); err != nil {
		t.Fatalf("queue again after terminal failure: %v", err)
	}
	if got := sourceMigrationRowJSON(t, ctx, db, queued.ID); got != failed {
		t.Error("new queued attempt changed failed history")
	}
}

func TestSourceExecutionMigrationRefusesLossyDown(t *testing.T) {
	for _, status := range []string{"FETCHING_SOURCE", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			db, _ := newMigrationTestDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
				_, err = db.ExecContext(ctx, `
					UPDATE deployments SET status = 'FETCHING_SOURCE', started_at = clock_timestamp(),
					       claim_token = gen_random_uuid() WHERE id = $1`, queued.ID)
			} else {
				_, err = db.ExecContext(ctx, `
					UPDATE deployments SET status = 'FAILED', started_at = now(), finished_at = now(),
					       commit_sha = $2, failure_stage = 'FETCHING_SOURCE', failure_reason = 'Fetch interrupted.'
					WHERE id = $1`, queued.ID, strings.Repeat("c", 40))
			}
			if err != nil {
				t.Fatalf("prepare durable lifecycle data: %v", err)
			}
			before := sourceMigrationRowJSON(t, ctx, db, queued.ID)
			if _, err := provider.DownTo(ctx, 2); err == nil {
				t.Fatal("source downgrade discarded lifecycle data")
			} else {
				var postgresError *pgconn.PgError
				if !errors.As(err, &postgresError) || postgresError.Code != "55000" {
					t.Fatalf("unsafe downgrade error = %v, want prerequisite-state refusal", err)
				}
			}
			version, err := provider.GetDBVersion(ctx)
			if err != nil || version != 3 {
				t.Errorf("version after refused downgrade = %d, error = %v; want 3", version, err)
			}
			if got := sourceMigrationRowJSON(t, ctx, db, queued.ID); got != before {
				t.Error("refused downgrade changed lifecycle or source data")
			}
			var indexPreserved bool
			if err := db.QueryRowContext(ctx, `
				SELECT to_regclass('deployments_one_unfinished_per_project') IS NOT NULL
				       AND to_regclass('deployments_one_queued_per_project') IS NULL`).Scan(&indexPreserved); err != nil || !indexPreserved {
				t.Errorf("unfinished-attempt index preserved = %t, error = %v", indexPreserved, err)
			}
		})
	}
}

func createSourceMigrationDeployment(t *testing.T, ctx context.Context, db *sql.DB) (projectRecord, migrationDeploymentRecord) {
	t.Helper()
	project := scanProject(t, db.QueryRowContext(ctx, `
		INSERT INTO projects (name, slug, git_url, branch, app_port, health_path)
		VALUES ('Source project', 'source-project', 'https://example.com/source.git', 'release/source', 4321, '/source-ready')
		RETURNING id::text, name, slug, git_url, branch, app_port, health_path, created_at, updated_at`))
	deployment := scanMigrationDeployment(t, db.QueryRowContext(ctx, `
		INSERT INTO deployments (project_id, git_url, branch, app_port, health_path)
		SELECT id, git_url, branch, app_port, health_path FROM projects WHERE id = $1
		RETURNING id::text, project_id::text, git_url, branch, app_port, health_path,
		          status, created_at, updated_at`, project.ID))
	return project, deployment
}

func sourceMigrationRowJSON(t *testing.T, ctx context.Context, db *sql.DB, id string) string {
	t.Helper()
	var snapshot string
	if err := db.QueryRowContext(ctx, "SELECT to_jsonb(d)::text FROM deployments AS d WHERE id = $1", id).Scan(&snapshot); err != nil {
		t.Fatalf("read complete deployment snapshot: %v", err)
	}
	return snapshot
}

func assertSourceMigrationConstraint(t *testing.T, err error, state, constraint string) {
	t.Helper()
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != state || constraint != "" && postgresError.ConstraintName != constraint {
		t.Fatalf("database error = %v, want SQLSTATE %s and constraint %s", err, state, constraint)
	}
}
