//go:build integration

package migrations_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"shipyard/apps/api/migrations"
)

func TestProjectsMigrationPreservesDataOnRerun(t *testing.T) {
	db, _ := newMigrationTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		t.Fatalf("apply initial migration: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("initial migration applied %d migrations, want 2", len(results))
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil || version != 2 {
		t.Fatalf("migration version = %d, error = %v; want 2", version, err)
	}

	var before time.Time
	if err := db.QueryRowContext(ctx, "SELECT now()").Scan(&before); err != nil {
		t.Fatalf("read database time: %v", err)
	}
	original := scanProject(t, db.QueryRowContext(ctx, `
		INSERT INTO projects (name, slug, git_url, branch, app_port, health_path)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text, name, slug, git_url, branch, app_port, health_path,
		          created_at, updated_at`,
		"Example project", "example-project", "https://example.com/example.git",
		"release/demo", 8080, "/ready"))
	if original.ID == "" || original.ID == "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("project UUID default = %q, want a generated UUID", original.ID)
	}
	var after time.Time
	if err := db.QueryRowContext(ctx, "SELECT now()").Scan(&after); err != nil {
		t.Fatalf("read database time: %v", err)
	}
	for name, timestamp := range map[string]time.Time{
		"created_at": original.CreatedAt,
		"updated_at": original.UpdatedAt,
	} {
		if timestamp.IsZero() || timestamp.Before(before) || timestamp.After(after) {
			t.Errorf("%s default = %v, want database time between %v and %v", name, timestamp, before, after)
		}
	}

	// A new provider represents a later invocation of the migration command.
	rerun, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatalf("create rerun provider: %v", err)
	}
	results, err = rerun.Up(ctx)
	if err != nil {
		t.Fatalf("rerun migration: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("rerun applied %d migrations, want 0", len(results))
	}
	version, err = rerun.GetDBVersion(ctx)
	if err != nil || version != 2 {
		t.Fatalf("rerun version = %d, error = %v; want 2", version, err)
	}
	preserved := scanProject(t, db.QueryRowContext(ctx, `
		SELECT id::text, name, slug, git_url, branch, app_port, health_path,
		       created_at, updated_at
		FROM projects WHERE id = $1`, original.ID))
	if preserved != original {
		t.Errorf("rerunning migrations changed project: got %+v, want %+v", preserved, original)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM projects").Scan(&count); err != nil {
		t.Fatalf("count projects after rerun: %v", err)
	}
	if count != 1 {
		t.Errorf("project count after rerun = %d, want 1", count)
	}
}

func TestProjectsSchemaConstraints(t *testing.T) {
	db, _ := newMigrationTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	const insertProject = `
		INSERT INTO projects (name, slug, git_url, branch, app_port, health_path)
		VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := db.ExecContext(ctx, insertProject,
		"Existing project", "existing-project", "https://example.com/repo.git", "main", 8080, "/"); err != nil {
		t.Fatalf("insert existing slug: %v", err)
	}

	tests := []struct {
		name  string
		field int
		value any
		state string
	}{
		{"missing name", 0, nil, "23502"},
		{"empty name", 0, "", "23514"},
		{"blank name", 0, "   ", "23514"},
		{"untrimmed name", 0, " Project ", "23514"},
		{"long name", 0, strings.Repeat("a", 101), "23514"},
		{"missing slug", 1, nil, "23502"},
		{"duplicate slug", 1, "existing-project", "23505"},
		{"empty slug", 1, "", "23514"},
		{"uppercase slug", 1, "Project", "23514"},
		{"leading hyphen", 1, "-project", "23514"},
		{"trailing hyphen", 1, "project-", "23514"},
		{"repeated hyphens", 1, "my--project", "23514"},
		{"slug punctuation", 1, "my_project", "23514"},
		{"long slug", 1, strings.Repeat("a", 64), "23514"},
		{"missing Git URL", 2, nil, "23502"},
		{"empty Git URL", 2, "", "23514"},
		{"blank Git URL", 2, "   ", "23514"},
		{"untrimmed Git URL", 2, " https://example.com/repo.git ", "23514"},
		{"long Git URL", 2, strings.Repeat("a", 2049), "23514"},
		{"missing branch", 3, nil, "23502"},
		{"empty branch", 3, "", "23514"},
		{"blank branch", 3, "   ", "23514"},
		{"untrimmed branch", 3, " main ", "23514"},
		{"long branch", 3, strings.Repeat("a", 256), "23514"},
		{"missing port", 4, nil, "23502"},
		{"zero port", 4, 0, "23514"},
		{"port above maximum", 4, 65536, "23514"},
		{"missing health path", 5, nil, "23502"},
		{"empty health path", 5, "", "23514"},
		{"relative health path", 5, "ready", "23514"},
		{"absolute health URL", 5, "https://example.com/ready", "23514"},
		{"long health path", 5, "/" + strings.Repeat("a", 2048), "23514"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := []any{"Project", fmt.Sprintf("project-%d", i), "https://example.com/repo.git", "main", 8080, "/ready"}
			values[tt.field] = tt.value
			_, err := db.ExecContext(ctx, insertProject, values...)
			var postgresError *pgconn.PgError
			if !errors.As(err, &postgresError) || postgresError.Code != tt.state {
				t.Fatalf("insert error = %v, want PostgreSQL SQLSTATE %s", err, tt.state)
			}
		})
	}
	for _, port := range []int{1, 65535} {
		t.Run(fmt.Sprintf("accept port %d", port), func(t *testing.T) {
			_, err := db.ExecContext(ctx, insertProject,
				"Project", fmt.Sprintf("port-%d", port), "https://example.com/repo.git", "main", port, "/")
			if err != nil {
				t.Fatalf("insert valid boundary port: %v", err)
			}
		})
	}
	t.Run("accept maximum text lengths", func(t *testing.T) {
		const gitPrefix = "https://example.com/"
		_, err := db.ExecContext(ctx, insertProject,
			strings.Repeat("n", 100), strings.Repeat("s", 63),
			gitPrefix+strings.Repeat("r", 2048-len(gitPrefix)),
			strings.Repeat("b", 255), 8080, "/"+strings.Repeat("h", 2047))
		if err != nil {
			t.Fatalf("insert valid boundary text lengths: %v", err)
		}
	})
}

func TestConcurrentInitialMigrations(t *testing.T) {
	firstDB, config := newMigrationTestDatabase(t)
	secondDB := stdlib.OpenDB(*config.Copy())
	t.Cleanup(func() {
		if err := secondDB.Close(); err != nil {
			t.Errorf("close second migration database: %v", err)
		}
	})
	first, err := migrations.NewProvider(firstDB)
	if err != nil {
		t.Fatalf("create first migration provider: %v", err)
	}
	second, err := migrations.NewProvider(secondDB)
	if err != nil {
		t.Fatalf("create second migration provider: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type result struct {
		applied int
		err     error
	}
	start := make(chan struct{})
	completed := make(chan result, 2)
	for _, provider := range []*goose.Provider{first, second} {
		go func() {
			<-start
			results, err := provider.Up(ctx)
			completed <- result{len(results), err}
		}()
	}
	close(start)
	applied := 0
	for range 2 {
		result := <-completed
		if result.err != nil {
			t.Errorf("concurrent initial migration: %v", result.err)
		}
		applied += result.applied
	}
	if t.Failed() {
		return
	}
	if applied != 2 {
		t.Errorf("concurrent providers applied %d migrations in total, want 2", applied)
	}
	version, err := first.GetDBVersion(ctx)
	if err != nil || version != 2 {
		t.Errorf("migration version = %d, error = %v; want 2", version, err)
	}
}

type projectRecord struct {
	ID         string
	Name       string
	Slug       string
	GitURL     string
	Branch     string
	AppPort    int
	HealthPath string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func scanProject(t *testing.T, row *sql.Row) projectRecord {
	t.Helper()
	var project projectRecord
	if err := row.Scan(&project.ID, &project.Name, &project.Slug, &project.GitURL,
		&project.Branch, &project.AppPort, &project.HealthPath, &project.CreatedAt, &project.UpdatedAt); err != nil {
		t.Fatalf("read project: %v", err)
	}
	project.CreatedAt = project.CreatedAt.UTC()
	project.UpdatedAt = project.UpdatedAt.UTC()
	return project
}

func newMigrationTestDatabase(t *testing.T) (*sql.DB, *pgx.ConnConfig) {
	t.Helper()
	databaseURL := os.Getenv("SHIPYARD_TEST_DATABASE_URL")
	if strings.TrimSpace(databaseURL) == "" {
		t.Fatal("integration tests require SHIPYARD_TEST_DATABASE_URL for a database where the test user can create schemas")
	}
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("SHIPYARD_TEST_DATABASE_URL is not a valid PostgreSQL connection string")
	}
	admin := stdlib.OpenDB(*config)
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close schema administration database: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("connect to integration database: %v", err)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatalf("generate isolated schema name: %v", err)
	}
	schema := "shipyard_test_" + hex.EncodeToString(random[:])
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("create isolated test schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.ExecContext(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("remove isolated schema %s: %v", schema, err)
		}
	})
	scoped := config.Copy()
	if scoped.RuntimeParams == nil {
		scoped.RuntimeParams = make(map[string]string)
	}
	scoped.RuntimeParams["search_path"] = quotedSchema
	db := stdlib.OpenDB(*scoped)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close isolated test database: %v", err)
		}
	})
	return db, scoped
}
