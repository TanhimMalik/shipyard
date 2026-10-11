//go:build integration

package projects

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresStoreGetUpdateAndDelete(t *testing.T) {
	pool := newProjectStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	original, err := store.Create(ctx, projectStoreTestInput("editable-project"))
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	found, err := store.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("get created project: %v", err)
	}
	assertStoredProjectEqual(t, found, original)

	// An older timestamp makes advancement observable without a sleep.
	original.UpdatedAt = original.CreatedAt.Add(-time.Hour)
	if _, err := pool.Exec(ctx, "UPDATE projects SET updated_at = $2 WHERE id = $1", original.ID, original.UpdatedAt); err != nil {
		t.Fatalf("set old update timestamp: %v", err)
	}
	var before time.Time
	if err := pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&before); err != nil {
		t.Fatalf("read time before update: %v", err)
	}
	name, port := "Updated name", 9876
	updated, err := store.Update(ctx, original.ID, PatchInput{Name: &name, AppPort: &port})
	if err != nil {
		t.Fatalf("partially update project: %v", err)
	}
	var after time.Time
	if err := pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&after); err != nil {
		t.Fatalf("read time after update: %v", err)
	}
	want := original
	want.Name, want.AppPort, want.UpdatedAt = name, port, updated.UpdatedAt
	assertStoredProjectEqual(t, updated, want)
	if !updated.UpdatedAt.After(original.UpdatedAt) || updated.UpdatedAt.Before(before) || updated.UpdatedAt.After(after) {
		t.Errorf("updated_at = %v, want an advanced database time between %v and %v", updated.UpdatedAt, before, after)
	}
	found, err = store.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("get partially updated project: %v", err)
	}
	assertStoredProjectEqual(t, found, updated)

	name, slug := "All fields changed", "all-fields-changed"
	gitURL, branch, healthPath := "https://example.com/updated/repository.git", "updated/branch", "/updated-ready"
	port = 5432
	fullyUpdated, err := store.Update(ctx, original.ID, PatchInput{
		Name: &name, Slug: &slug, GitURL: &gitURL, Branch: &branch, AppPort: &port, HealthPath: &healthPath,
	})
	if err != nil {
		t.Fatalf("update all project fields: %v", err)
	}
	want = Project{
		ID: original.ID, Name: name, Slug: slug, GitURL: gitURL, Branch: branch,
		AppPort: port, HealthPath: healthPath, CreatedAt: original.CreatedAt, UpdatedAt: fullyUpdated.UpdatedAt,
	}
	assertStoredProjectEqual(t, fullyUpdated, want)
	if !fullyUpdated.UpdatedAt.After(updated.UpdatedAt) {
		t.Errorf("second updated_at = %v, want later than %v", fullyUpdated.UpdatedAt, updated.UpdatedAt)
	}

	other, err := store.Create(ctx, projectStoreTestInput("taken-slug"))
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	conflictingName := "Must not be saved"
	if _, err := store.Update(ctx, original.ID, PatchInput{Name: &conflictingName, Slug: &other.Slug}); !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("conflicting update error = %v, want ErrSlugTaken", err)
	}
	found, err = store.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("get project after conflict: %v", err)
	}
	assertStoredProjectEqual(t, found, fullyUpdated)
	found, err = store.Get(ctx, other.ID)
	if err != nil {
		t.Fatalf("get other project after conflict: %v", err)
	}
	assertStoredProjectEqual(t, found, other)

	if err := store.Delete(ctx, original.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	listed, err := store.List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d projects after delete, want 1", len(listed))
	}
	assertStoredProjectEqual(t, listed[0], other)
	if _, err := store.Get(ctx, original.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get deleted project error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, original.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("repeated delete error = %v, want ErrNotFound", err)
	}
}

func TestPostgresStoreMissingProject(t *testing.T) {
	store := NewStore(newProjectStoreTestDatabase(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const missingID = "00000000-0000-4000-8000-000000000099"
	name := "Missing project"
	if _, err := store.Get(ctx, missingID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get missing project error = %v, want ErrNotFound", err)
	}
	if _, err := store.Update(ctx, missingID, PatchInput{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing project error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, missingID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing project error = %v, want ErrNotFound", err)
	}
}

func TestPostgresStoreCRUDHonorsCanceledContext(t *testing.T) {
	store := NewStore(newProjectStoreTestDatabase(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	original, err := store.Create(ctx, projectStoreTestInput("cancellation-project"))
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	canceled, cancelOperation := context.WithCancel(context.Background())
	cancelOperation()
	name := "Must not be saved"
	if _, err := store.Get(canceled, original.ID); !errors.Is(err, context.Canceled) {
		t.Errorf("get with canceled context error = %v, want context.Canceled", err)
	}
	if _, err := store.Update(canceled, original.ID, PatchInput{Name: &name}); !errors.Is(err, context.Canceled) {
		t.Errorf("update with canceled context error = %v, want context.Canceled", err)
	}
	if err := store.Delete(canceled, original.ID); !errors.Is(err, context.Canceled) {
		t.Errorf("delete with canceled context error = %v, want context.Canceled", err)
	}
	found, err := store.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("get after canceled operations: %v", err)
	}
	assertStoredProjectEqual(t, found, original)
}

func TestPostgresStoreUpdateKeepsOtherUniqueViolationsDistinct(t *testing.T) {
	pool := newProjectStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, "ALTER TABLE projects ADD CONSTRAINT projects_test_branch_key UNIQUE (branch)"); err != nil {
		t.Fatalf("add unrelated unique constraint: %v", err)
	}
	firstInput, secondInput := projectStoreTestInput("first-project"), projectStoreTestInput("second-project")
	firstInput.Branch, secondInput.Branch = "first/branch", "second/branch"
	if _, err := store.Create(ctx, firstInput); err != nil {
		t.Fatalf("create first project: %v", err)
	}
	second, err := store.Create(ctx, secondInput)
	if err != nil {
		t.Fatalf("create second project: %v", err)
	}
	_, err = store.Update(ctx, second.ID, PatchInput{Branch: &firstInput.Branch})
	if err == nil || errors.Is(err, ErrSlugTaken) {
		t.Fatalf("unrelated unique violation = %v, want an error distinct from ErrSlugTaken", err)
	}
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "23505" || postgresError.ConstraintName != "projects_test_branch_key" {
		t.Errorf("unrelated unique violation lost PostgreSQL error: %v", err)
	}
	found, err := store.Get(ctx, second.ID)
	if err != nil {
		t.Fatalf("get project after unrelated conflict: %v", err)
	}
	assertStoredProjectEqual(t, found, second)
}

func TestPostgresStoreConcurrentDisjointUpdates(t *testing.T) {
	store := NewStore(newProjectStoreTestDatabase(t))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	original, err := store.Create(ctx, projectStoreTestInput("concurrent-project"))
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	for attempt := range 8 {
		name := fmt.Sprintf("Concurrent name %d", attempt)
		branch := fmt.Sprintf("concurrent/branch-%d", attempt)
		start := make(chan struct{})
		completed := make(chan error, 2)
		for _, patch := range []PatchInput{{Name: &name}, {Branch: &branch}} {
			go func() {
				<-start
				_, err := store.Update(ctx, original.ID, patch)
				completed <- err
			}()
		}
		close(start)
		for range 2 {
			if err := <-completed; err != nil {
				t.Fatalf("concurrent update: %v", err)
			}
		}
		found, err := store.Get(ctx, original.ID)
		if err != nil {
			t.Fatalf("get concurrent updates: %v", err)
		}
		want := original
		want.Name, want.Branch, want.UpdatedAt = name, branch, found.UpdatedAt
		assertStoredProjectEqual(t, found, want)
	}
}
