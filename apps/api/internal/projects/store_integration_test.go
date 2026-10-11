//go:build integration

package projects

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"shipyard/apps/api/migrations"
)

func TestPostgresStoreCreateAndListRoundTrip(t *testing.T) {
	pool := newProjectStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var before time.Time
	if err := pool.QueryRow(ctx, "SELECT now()").Scan(&before); err != nil {
		t.Fatalf("read database time before create: %v", err)
	}
	input := projectStoreTestInput("integration-project")
	created, err := store.Create(ctx, input)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if created.Name != input.Name || created.Slug != input.Slug ||
		created.GitURL != input.GitURL || created.Branch != input.Branch ||
		created.AppPort != input.AppPort || created.HealthPath != input.HealthPath {
		t.Errorf("created project fields = %+v, want input %+v", created, input)
	}
	var id pgtype.UUID
	if err := id.Scan(created.ID); err != nil || !id.Valid || id.Bytes == [16]byte{} {
		t.Errorf("created ID = %q, want a generated UUID; parse error = %v", created.ID, err)
	}
	var after time.Time
	if err := pool.QueryRow(ctx, "SELECT now()").Scan(&after); err != nil {
		t.Fatalf("read database time after create: %v", err)
	}
	for name, timestamp := range map[string]time.Time{
		"created_at": created.CreatedAt,
		"updated_at": created.UpdatedAt,
	} {
		if timestamp.IsZero() || timestamp.Before(before) || timestamp.After(after) {
			t.Errorf("%s = %v, want database time between %v and %v", name, timestamp, before, after)
		}
	}

	listed, err := store.List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("list created project: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d projects, want 1", len(listed))
	}
	assertStoredProjectEqual(t, listed[0], created)

	replacement := input
	replacement.Name = "Replacement attempt"
	replacement.GitURL = "https://example.com/other/repository.git"
	replacement.Branch = "other"
	replacement.AppPort = 9999
	replacement.HealthPath = "/different"
	if _, err := store.Create(ctx, replacement); !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("duplicate create error = %v, want ErrSlugTaken", err)
	}
	listed, err = store.List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("list after duplicate create: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d projects after duplicate, want 1", len(listed))
	}
	assertStoredProjectEqual(t, listed[0], created)
}

func TestPostgresStoreListOrdersAndPaginates(t *testing.T) {
	pool := newProjectStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Deliberately insert out of order and tie timestamps, so both sort keys matter.
	fixtures := []struct {
		id      string
		slug    string
		created time.Time
	}{
		{"00000000-0000-4000-8000-000000000002", "tied-low", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
		{"00000000-0000-4000-8000-000000000004", "oldest", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"00000000-0000-4000-8000-000000000001", "newest", time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)},
		{"00000000-0000-4000-8000-000000000003", "tied-high", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
	}
	for _, fixture := range fixtures {
		input := projectStoreTestInput(fixture.slug)
		_, err := pool.Exec(ctx, `
			INSERT INTO projects (id, name, slug, git_url, branch, app_port, health_path, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			fixture.id, input.Name, input.Slug, input.GitURL, input.Branch,
			input.AppPort, input.HealthPath, fixture.created)
		if err != nil {
			t.Fatalf("insert ordering fixture %s: %v", fixture.slug, err)
		}
	}
	for _, test := range []struct {
		name   string
		limit  int
		offset int
		ids    []string
	}{
		{"all", 10, 0, []string{fixtures[2].id, fixtures[3].id, fixtures[0].id, fixtures[1].id}},
		{"first page", 2, 0, []string{fixtures[2].id, fixtures[3].id}},
		{"offset across tie", 2, 1, []string{fixtures[3].id, fixtures[0].id}},
		{"second page", 2, 2, []string{fixtures[0].id, fixtures[1].id}},
		{"past end", 2, 4, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			listed, err := store.List(ctx, test.limit, test.offset)
			if err != nil {
				t.Fatalf("list projects: %v", err)
			}
			if len(listed) != len(test.ids) {
				t.Fatalf("listed %d projects, want %d", len(listed), len(test.ids))
			}
			for i, id := range test.ids {
				if listed[i].ID != id {
					t.Errorf("project %d ID = %q, want %q", i, listed[i].ID, id)
				}
			}
		})
	}
}

func TestPostgresStoreKeepsOtherUniqueViolationsDistinct(t *testing.T) {
	pool := newProjectStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// This constraint exists only in the isolated test schema. It exercises the
	// distinction between a duplicate slug and another PostgreSQL 23505 error.
	if _, err := pool.Exec(ctx, "ALTER TABLE projects ADD CONSTRAINT projects_test_git_url_key UNIQUE (git_url)"); err != nil {
		t.Fatalf("add unrelated unique constraint: %v", err)
	}
	if _, err := store.Create(ctx, projectStoreTestInput("first-project")); err != nil {
		t.Fatalf("create first project: %v", err)
	}
	_, err := store.Create(ctx, projectStoreTestInput("different-slug"))
	if err == nil || errors.Is(err, ErrSlugTaken) {
		t.Fatalf("unrelated unique violation = %v, want an error distinct from ErrSlugTaken", err)
	}
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "23505" ||
		postgresError.ConstraintName != "projects_test_git_url_key" {
		t.Errorf("unrelated unique violation lost PostgreSQL error: %v", err)
	}
}

func TestPostgresStoreHonorsCanceledContext(t *testing.T) {
	pool := newProjectStoreTestDatabase(t)
	store := NewStore(pool)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := store.Create(canceled, projectStoreTestInput("canceled-project")); !errors.Is(err, context.Canceled) {
		t.Errorf("create with canceled context error = %v, want context.Canceled", err)
	}
	if _, err := store.List(canceled, 10, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("list with canceled context error = %v, want context.Canceled", err)
	}
	ctx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()
	listed, err := store.List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("list after canceled create: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("canceled create persisted %d projects, want 0", len(listed))
	}
}

func projectStoreTestInput(slug string) CreateInput {
	return CreateInput{
		Name:       "Integration project",
		Slug:       slug,
		GitURL:     "https://example.com/team/integration.git",
		Branch:     "release/integration",
		AppPort:    4317,
		HealthPath: "/ready",
	}
}

func assertStoredProjectEqual(t *testing.T, got, want Project) {
	t.Helper()
	if got.ID != want.ID || got.Name != want.Name || got.Slug != want.Slug ||
		got.GitURL != want.GitURL || got.Branch != want.Branch ||
		got.AppPort != want.AppPort || got.HealthPath != want.HealthPath ||
		!got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("stored project = %+v, want %+v", got, want)
	}
}

func newProjectStoreTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("SHIPYARD_TEST_DATABASE_URL")
	if strings.TrimSpace(databaseURL) == "" {
		t.Fatal("integration tests require SHIPYARD_TEST_DATABASE_URL for a database where the test user can create schemas")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("SHIPYARD_TEST_DATABASE_URL is not a valid PostgreSQL connection string")
	}
	admin := stdlib.OpenDB(*config.ConnConfig.Copy())
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close schema administration database: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("connect to integration database: %v", err)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatalf("generate isolated schema name: %v", err)
	}
	schema := "shipyard_projects_test_" + hex.EncodeToString(random[:])
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
	if scoped.ConnConfig.RuntimeParams == nil {
		scoped.ConnConfig.RuntimeParams = make(map[string]string)
	}
	scoped.ConnConfig.RuntimeParams["search_path"] = quotedSchema
	migrationDB := stdlib.OpenDB(*scoped.ConnConfig.Copy())
	t.Cleanup(func() {
		if err := migrationDB.Close(); err != nil {
			t.Errorf("close isolated migration database: %v", err)
		}
	})
	provider, err := migrations.NewProvider(migrationDB)
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply project migrations: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, scoped)
	if err != nil {
		t.Fatalf("create isolated project pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("connect isolated project pool: %v", err)
	}
	return pool
}
