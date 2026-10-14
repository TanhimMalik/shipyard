//go:build integration

package deployments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"shipyard/apps/api/internal/projects"
	"shipyard/apps/api/migrations"
)

func TestDeploymentSnapshotPersistsAndRestrictsProjectDeletion(t *testing.T) {
	pool, config := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	projectStore := projects.NewStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	project := createDeploymentTestProject(t, ctx, pool, "snapshot-project")
	var before time.Time
	if err := pool.QueryRow(ctx, "SELECT now()").Scan(&before); err != nil {
		t.Fatalf("read database time before queueing: %v", err)
	}
	queued, err := store.Create(ctx, project.ID)
	if err != nil {
		t.Fatalf("queue deployment: %v", err)
	}
	if queued.ProjectID != project.ID || queued.GitURL != project.GitURL ||
		queued.Branch != project.Branch || queued.AppPort != project.AppPort ||
		queued.HealthPath != project.HealthPath || queued.Status != StatusQueued {
		t.Errorf("queued deployment = %+v, want QUEUED with project snapshot %+v", queued, project)
	}
	var id pgtype.UUID
	if err := id.Scan(queued.ID); err != nil || !id.Valid || id.Bytes == [16]byte{} {
		t.Errorf("deployment ID = %q, want a generated UUID; parse error = %v", queued.ID, err)
	}
	var after time.Time
	if err := pool.QueryRow(ctx, "SELECT now()").Scan(&after); err != nil {
		t.Fatalf("read database time after queueing: %v", err)
	}
	for name, timestamp := range map[string]time.Time{"created_at": queued.CreatedAt, "updated_at": queued.UpdatedAt} {
		if timestamp.IsZero() || timestamp.Before(before) || timestamp.After(after) {
			t.Errorf("%s = %v, want database time between %v and %v", name, timestamp, before, after)
		}
	}

	gitURL, branch, port, healthPath := "https://example.com/changed/repository.git", "changed/branch", 9876, "/changed-ready"
	if _, err := projectStore.Update(ctx, project.ID, projects.PatchInput{
		GitURL: &gitURL, Branch: &branch, AppPort: &port, HealthPath: &healthPath,
	}); err != nil {
		t.Fatalf("edit project after queueing: %v", err)
	}
	// A fresh pool verifies that the record and snapshot are persisted independently
	// of the store instance and its original database connections.
	reopenedPool, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatalf("reopen isolated database pool: %v", err)
	}
	t.Cleanup(reopenedPool.Close)
	reopened := NewStore(reopenedPool)
	found, err := reopened.Get(ctx, queued.ID)
	if err != nil {
		t.Fatalf("read persisted deployment: %v", err)
	}
	assertDeploymentEqual(t, found, queued)
	listed, err := reopened.List(ctx, project.ID, 10, 0)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list persisted deployment: count = %d, error = %v; want 1", len(listed), err)
	}
	assertDeploymentEqual(t, listed[0], queued)

	if err := projectStore.Delete(ctx, project.ID); !errors.Is(err, projects.ErrHasDeployments) {
		t.Fatalf("delete project with history error = %v, want ErrHasDeployments", err)
	}
	if _, err := projectStore.Get(ctx, project.ID); err != nil {
		t.Fatalf("project was not preserved after restricted delete: %v", err)
	}
	found, err = reopened.Get(ctx, queued.ID)
	if err != nil {
		t.Fatalf("deployment was not preserved after restricted delete: %v", err)
	}
	assertDeploymentEqual(t, found, queued)

	emptyProject := createDeploymentTestProject(t, ctx, pool, "deletable-project")
	if err := projectStore.Delete(ctx, emptyProject.ID); err != nil {
		t.Fatalf("delete project without history: %v", err)
	}
	if _, err := projectStore.Get(ctx, emptyProject.ID); !errors.Is(err, projects.ErrNotFound) {
		t.Errorf("get deleted empty project error = %v, want projects.ErrNotFound", err)
	}
}

func TestDeploymentMissingRecordsAndEmptyHistory(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const missingID = "00000000-0000-4000-8000-000000000099"
	if _, err := store.Create(ctx, missingID); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("queue missing project error = %v, want ErrProjectNotFound", err)
	}
	if _, err := store.List(ctx, missingID, 10, 0); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("list missing project error = %v, want ErrProjectNotFound", err)
	}
	if _, err := store.Get(ctx, missingID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get missing deployment error = %v, want ErrNotFound", err)
	}
	project := createDeploymentTestProject(t, ctx, pool, "empty-history")
	listed, err := store.List(ctx, project.ID, 10, 0)
	if err != nil || len(listed) != 0 {
		t.Errorf("existing empty project history: count = %d, error = %v; want empty", len(listed), err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM deployments").Scan(&count); err != nil || count != 0 {
		t.Errorf("deployment count after missing-project queue = %d, error = %v; want 0", count, err)
	}
}

func TestDeploymentListScopesProjectsAndAppliesOffset(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first := createDeploymentTestProject(t, ctx, pool, "first-history")
	second := createDeploymentTestProject(t, ctx, pool, "second-history")
	queuedByProject := make(map[string]Deployment, 2)
	for _, project := range []projects.Project{first, second} {
		queued, err := store.Create(ctx, project.ID)
		if err != nil {
			t.Fatalf("queue project %s: %v", project.Slug, err)
		}
		queuedByProject[project.ID] = queued
	}
	for _, project := range []projects.Project{first, second} {
		listed, err := store.List(ctx, project.ID, 1, 0)
		if err != nil || len(listed) != 1 {
			t.Fatalf("list project %s: count = %d, error = %v; want 1", project.Slug, len(listed), err)
		}
		assertDeploymentEqual(t, listed[0], queuedByProject[project.ID])
	}
	// Both projects have one unfinished attempt in this test.
	listed, err := store.List(ctx, first.ID, 1, 1)
	if err != nil || len(listed) != 0 {
		t.Errorf("offset past project history: count = %d, error = %v; want empty", len(listed), err)
	}
}

func TestConcurrentDeploymentQueueAllowsExactlyOneWinner(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	project := createDeploymentTestProject(t, ctx, pool, "concurrent-queue")
	type result struct {
		deployment Deployment
		err        error
	}
	start := make(chan struct{})
	completed := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			deployment, err := store.Create(ctx, project.ID)
			completed <- result{deployment, err}
		}()
	}
	close(start)
	var winner Deployment
	successes, conflicts := 0, 0
	for range 2 {
		result := <-completed
		switch {
		case result.err == nil:
			successes++
			winner = result.deployment
		case errors.Is(result.err, ErrAlreadyQueued):
			conflicts++
		default:
			t.Errorf("concurrent queue returned unexpected error: %v", result.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent queue: successes = %d, conflicts = %d; want 1 each", successes, conflicts)
	}
	if _, err := store.Create(ctx, project.ID); !errors.Is(err, ErrAlreadyQueued) {
		t.Errorf("subsequent queue error = %v, want ErrAlreadyQueued", err)
	}
	listed, err := store.List(ctx, project.ID, 10, 0)
	if err != nil || len(listed) != 1 {
		t.Fatalf("history after competing queues: count = %d, error = %v; want 1", len(listed), err)
	}
	assertDeploymentEqual(t, listed[0], winner)
}

func TestDeploymentQueueKeepsOtherUniqueViolationsDistinct(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, "ALTER TABLE deployments ADD CONSTRAINT deployments_test_git_url_key UNIQUE (git_url)"); err != nil {
		t.Fatalf("add unrelated unique constraint: %v", err)
	}
	first := createDeploymentTestProject(t, ctx, pool, "first-unique")
	second := createDeploymentTestProject(t, ctx, pool, "second-unique")
	if _, err := store.Create(ctx, first.ID); err != nil {
		t.Fatalf("queue first project: %v", err)
	}
	_, err := store.Create(ctx, second.ID)
	if err == nil || errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("unrelated unique violation = %v, want an error distinct from ErrAlreadyQueued", err)
	}
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "23505" || postgresError.ConstraintName != "deployments_test_git_url_key" {
		t.Errorf("unrelated unique violation lost PostgreSQL error: %v", err)
	}
	listed, err := store.List(ctx, second.ID, 10, 0)
	if err != nil || len(listed) != 0 {
		t.Errorf("unrelated conflict persisted history: count = %d, error = %v; want empty", len(listed), err)
	}
}

func TestDeploymentQueueAndProjectDeleteRacePreservesReferences(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	projectStore := projects.NewStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type queueResult struct {
		deployment Deployment
		err        error
	}
	for attempt := range 8 {
		project := createDeploymentTestProject(t, ctx, pool, fmt.Sprintf("delete-race-%d", attempt))
		start := make(chan struct{})
		queued := make(chan queueResult, 1)
		deleted := make(chan error, 1)
		go func() {
			<-start
			deployment, err := store.Create(ctx, project.ID)
			queued <- queueResult{deployment, err}
		}()
		go func() {
			<-start
			deleted <- projectStore.Delete(ctx, project.ID)
		}()
		close(start)
		queue, deleteErr := <-queued, <-deleted
		switch {
		case queue.err == nil && errors.Is(deleteErr, projects.ErrHasDeployments):
			if _, err := projectStore.Get(ctx, project.ID); err != nil {
				t.Fatalf("winning queue lost its project: %v", err)
			}
			found, err := store.Get(ctx, queue.deployment.ID)
			if err != nil {
				t.Fatalf("winning queue lost its deployment: %v", err)
			}
			assertDeploymentEqual(t, found, queue.deployment)
		case deleteErr == nil && errors.Is(queue.err, ErrProjectNotFound):
			if _, err := projectStore.Get(ctx, project.ID); !errors.Is(err, projects.ErrNotFound) {
				t.Fatalf("winning delete left project behind: %v", err)
			}
			var count int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM deployments WHERE project_id = $1", project.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("winning delete left %d deployments, error = %v; want 0", count, err)
			}
		default:
			t.Fatalf("invalid race outcome: queue error = %v, delete error = %v", queue.err, deleteErr)
		}
	}
	var orphans int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM deployments AS d
		LEFT JOIN projects AS p ON p.id = d.project_id WHERE p.id IS NULL`).Scan(&orphans); err != nil || orphans != 0 {
		t.Errorf("orphan count = %d, error = %v; want 0", orphans, err)
	}
}

func TestDeploymentStoreHonorsCanceledContext(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	project := createDeploymentTestProject(t, ctx, pool, "canceled-queue")
	canceled, cancelOperation := context.WithCancel(context.Background())
	cancelOperation()
	if _, err := store.Create(canceled, project.ID); !errors.Is(err, context.Canceled) {
		t.Errorf("queue with canceled context error = %v, want context.Canceled", err)
	}
	listed, err := store.List(ctx, project.ID, 10, 0)
	if err != nil || len(listed) != 0 {
		t.Fatalf("canceled queue persisted history: count = %d, error = %v; want empty", len(listed), err)
	}
	queued, err := store.Create(ctx, project.ID)
	if err != nil {
		t.Fatalf("queue after canceled attempt: %v", err)
	}
	if _, err := store.List(canceled, project.ID, 10, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("list with canceled context error = %v, want context.Canceled", err)
	}
	if _, err := store.Get(canceled, queued.ID); !errors.Is(err, context.Canceled) {
		t.Errorf("get with canceled context error = %v, want context.Canceled", err)
	}
	found, err := store.Get(ctx, queued.ID)
	if err != nil {
		t.Fatalf("read deployment after canceled operations: %v", err)
	}
	assertDeploymentEqual(t, found, queued)
}

func createDeploymentTestProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug string) projects.Project {
	t.Helper()
	project, err := projects.NewStore(pool).Create(ctx, projects.CreateInput{
		Name: "Deployment test project", Slug: slug, GitURL: "https://example.com/team/deployment.git",
		Branch: "release/queue-test", AppPort: 4317, HealthPath: "/ready",
	})
	if err != nil {
		t.Fatalf("create deployment test project: %v", err)
	}
	return project
}

func assertDeploymentEqual(t *testing.T, got, want Deployment) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deployment = %+v, want %+v", got, want)
	}
}

func newDeploymentStoreTestDatabase(t *testing.T) (*pgxpool.Pool, *pgxpool.Config) {
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
	schema := "shipyard_deployments_test_" + hex.EncodeToString(random[:])
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
		t.Fatalf("apply deployment migrations: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, scoped)
	if err != nil {
		t.Fatalf("create isolated deployment pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("connect isolated deployment pool: %v", err)
	}
	return pool, scoped
}
