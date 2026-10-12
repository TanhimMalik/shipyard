//go:build integration

package migrations_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"shipyard/apps/api/migrations"
)

func TestDeploymentMigrationUpgradesAndPreservesExistingData(t *testing.T) {
	db, _ := newMigrationTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}
	results, err := provider.UpTo(ctx, 1)
	if err != nil || len(results) != 1 {
		t.Fatalf("apply version 1: applied %d migrations, error = %v; want 1", len(results), err)
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil || version != 1 {
		t.Fatalf("initial version = %d, error = %v; want 1", version, err)
	}
	var deploymentTableAbsent bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('deployments') IS NULL").Scan(&deploymentTableAbsent); err != nil || !deploymentTableAbsent {
		t.Fatalf("deployments absent at version 1 = %t, error = %v; want true", deploymentTableAbsent, err)
	}
	original := scanProject(t, db.QueryRowContext(ctx, `
		INSERT INTO projects (name, slug, git_url, branch, app_port, health_path)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text, name, slug, git_url, branch, app_port, health_path,
		          created_at, updated_at`,
		"Existing project", "existing-project", "https://example.com/existing.git",
		"release/existing", 3210, "/ready/existing"))
	readProject := func() projectRecord {
		return scanProject(t, db.QueryRowContext(ctx, `
			SELECT id::text, name, slug, git_url, branch, app_port, health_path,
			       created_at, updated_at FROM projects WHERE id = $1`, original.ID))
	}

	results, err = provider.Up(ctx)
	if err != nil || len(results) != 1 {
		t.Fatalf("upgrade from version 1: applied %d migrations, error = %v; want 1", len(results), err)
	}
	version, err = provider.GetDBVersion(ctx)
	if err != nil || version != 2 {
		t.Fatalf("upgraded version = %d, error = %v; want 2", version, err)
	}
	if got := readProject(); got != original {
		t.Errorf("upgrade changed existing project: got %+v, want %+v", got, original)
	}
	queued := scanMigrationDeployment(t, db.QueryRowContext(ctx, `
		INSERT INTO deployments (project_id, git_url, branch, app_port, health_path)
		SELECT id, git_url, branch, app_port, health_path FROM projects WHERE id = $1
		RETURNING id::text, project_id::text, git_url, branch, app_port, health_path,
		          status, created_at, updated_at`, original.ID))
	if queued.ID == "" || queued.ID == "00000000-0000-0000-0000-000000000000" {
		t.Errorf("deployment UUID default = %q, want a generated UUID", queued.ID)
	}
	if queued.ProjectID != original.ID || queued.GitURL != original.GitURL ||
		queued.Branch != original.Branch || queued.AppPort != original.AppPort ||
		queued.HealthPath != original.HealthPath || queued.Status != "QUEUED" {
		t.Errorf("queued deployment does not preserve project settings: got %+v, project %+v", queued, original)
	}
	if queued.CreatedAt.IsZero() || queued.UpdatedAt != queued.CreatedAt {
		t.Errorf("deployment timestamp defaults = %v, %v; want equal nonzero timestamps", queued.CreatedAt, queued.UpdatedAt)
	}
	readDeployment := func() migrationDeploymentRecord {
		return scanMigrationDeployment(t, db.QueryRowContext(ctx, `
			SELECT id::text, project_id::text, git_url, branch, app_port, health_path,
			       status, created_at, updated_at FROM deployments WHERE id = $1`, queued.ID))
	}

	rerun, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatalf("create rerun provider: %v", err)
	}
	results, err = rerun.Up(ctx)
	if err != nil || len(results) != 0 {
		t.Fatalf("rerun version 2: applied %d migrations, error = %v; want 0", len(results), err)
	}
	version, err = rerun.GetDBVersion(ctx)
	if err != nil || version != 2 {
		t.Fatalf("rerun version = %d, error = %v; want 2", version, err)
	}
	if got := readDeployment(); got != queued {
		t.Errorf("rerun changed deployment: got %+v, want %+v", got, queued)
	}
	if got := readProject(); got != original {
		t.Errorf("rerun changed project: got %+v, want %+v", got, original)
	}
	_, err = db.ExecContext(ctx, "UPDATE deployments SET status = 'RUNNING' WHERE id = $1", queued.ID)
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "23514" || postgresError.ConstraintName != "deployments_status_check" {
		t.Fatalf("fabricated RUNNING status error = %v; want deployments_status_check violation", err)
	}
	if got := readDeployment(); got != queued {
		t.Errorf("rejected status update changed deployment: got %+v, want %+v", got, queued)
	}

	// This destructive check is confined to the helper's unique test schema.
	results, err = rerun.DownTo(ctx, 1)
	if err != nil || len(results) != 1 {
		t.Fatalf("revert version 2: applied %d migrations, error = %v; want 1", len(results), err)
	}
	version, err = rerun.GetDBVersion(ctx)
	if err != nil || version != 1 {
		t.Fatalf("reverted version = %d, error = %v; want 1", version, err)
	}
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('deployments') IS NULL").Scan(&deploymentTableAbsent); err != nil || !deploymentTableAbsent {
		t.Errorf("deployments absent after down = %t, error = %v; want true", deploymentTableAbsent, err)
	}
	if got := readProject(); got != original {
		t.Errorf("down migration changed project: got %+v, want %+v", got, original)
	}
}

type migrationDeploymentRecord struct {
	ID         string
	ProjectID  string
	GitURL     string
	Branch     string
	AppPort    int
	HealthPath string
	Status     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func scanMigrationDeployment(t *testing.T, row *sql.Row) migrationDeploymentRecord {
	t.Helper()
	var deployment migrationDeploymentRecord
	if err := row.Scan(&deployment.ID, &deployment.ProjectID, &deployment.GitURL, &deployment.Branch,
		&deployment.AppPort, &deployment.HealthPath, &deployment.Status, &deployment.CreatedAt, &deployment.UpdatedAt); err != nil {
		t.Fatalf("read migration deployment: %v", err)
	}
	deployment.CreatedAt = deployment.CreatedAt.UTC()
	deployment.UpdatedAt = deployment.UpdatedAt.UTC()
	return deployment
}
