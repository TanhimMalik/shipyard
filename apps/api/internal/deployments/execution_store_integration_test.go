//go:build integration

package deployments

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestExecutionClaimsSkipLocksAndHaveOneOwner(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if _, err := store.ClaimNext(ctx); !errors.Is(err, ErrNoQueuedDeployments) {
		t.Fatalf("empty queue: %v", err)
	}
	firstProject := createDeploymentTestProject(t, ctx, pool, "first-claim")
	secondProject := createDeploymentTestProject(t, ctx, pool, "second-claim")
	first, err := store.Create(ctx, firstProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx, secondProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Make ordering explicit, independently of transaction timestamp precision.
	if _, err := pool.Exec(ctx, "UPDATE deployments SET created_at = created_at - interval '1 hour' WHERE id = $1", first.ID); err != nil {
		t.Fatal(err)
	}
	first, err = store.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT id FROM deployments WHERE id = $1 FOR UPDATE", first.ID); err != nil {
		t.Fatal(err)
	}
	claimCtx, cancelClaim := context.WithTimeout(ctx, 3*time.Second)
	secondClaim, err := store.ClaimNext(claimCtx)
	cancelClaim()
	if err != nil || secondClaim.ID != second.ID {
		t.Fatalf("claim past locked oldest row = %+v, %v; want %s", secondClaim, err, second.ID)
	}
	assertExecutionClaim(t, secondClaim, second)
	if _, err := store.Create(ctx, secondProject.ID); !errors.Is(err, ErrAlreadyQueued) {
		t.Errorf("queue while fetching: %v, want active-attempt conflict", err)
	}
	if _, err := store.ClaimNext(ctx); !errors.Is(err, ErrNoQueuedDeployments) {
		t.Errorf("queue containing only locked/claimed rows: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	type result struct {
		claim Claim
		err   error
	}
	start, results := make(chan struct{}), make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			claim, err := store.ClaimNext(ctx)
			results <- result{claim, err}
		}()
	}
	close(start)
	winners, empty := 0, 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			winners++
			if result.claim.ID != first.ID || result.claim.Token == secondClaim.Token {
				t.Errorf("wrong/shared claim ownership: %+v", result.claim)
			}
			assertExecutionClaim(t, result.claim, first)
		case errors.Is(result.err, ErrNoQueuedDeployments):
			empty++
		default:
			t.Errorf("concurrent claim: %v", result.err)
		}
	}
	if winners != 1 || empty != 1 {
		t.Fatalf("claim results: %d owners, %d empty; want one each", winners, empty)
	}
}

func TestExecutionWritesPersistAndPreserveFailureHistory(t *testing.T) {
	pool, config := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	project := createDeploymentTestProject(t, ctx, pool, "execution-history")
	queued, err := store.Create(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertExecutionClaim(t, claim, queued)
	wrong := claim
	wrong.Token = "00000000-0000-4000-8000-000000000099"
	sha := strings.Repeat("a", 40)
	if _, err := store.RecordSourceCommit(ctx, wrong, sha); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("record with wrong token: %v", err)
	}
	if _, err := store.FailSource(ctx, wrong, "Source checkout failed."); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("fail with wrong token: %v", err)
	}
	wrongID := claim
	wrongID.ID = "00000000-0000-4000-8000-000000000098"
	if _, err := store.RecordSourceCommit(ctx, wrongID, sha); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("record with wrong attempt ID: %v", err)
	}
	if _, err := store.FailSource(ctx, wrongID, "Source checkout failed."); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("fail with wrong attempt ID: %v", err)
	}
	unchanged, err := store.Get(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertDeploymentEqual(t, unchanged, claim.Deployment)
	recorded, err := store.RecordSourceCommit(ctx, claim, sha)
	if err != nil || recorded.CommitSHA == nil || *recorded.CommitSHA != sha {
		t.Fatalf("record commit = %+v, %v", recorded, err)
	}
	if recorded.Status != StatusFetchingSource || recorded.StartedAt == nil || !recorded.StartedAt.Equal(*claim.StartedAt) || recorded.FinishedAt != nil {
		t.Errorf("commit recording changed execution state: %+v", recorded)
	}
	wantRecorded := claim.Deployment
	wantRecorded.CommitSHA, wantRecorded.UpdatedAt = &sha, recorded.UpdatedAt
	assertDeploymentEqual(t, recorded, wantRecorded)
	repeated, err := store.RecordSourceCommit(ctx, claim, sha)
	if err != nil {
		t.Fatal(err)
	}
	assertDeploymentEqual(t, repeated, recorded)
	if _, err := store.RecordSourceCommit(ctx, claim, strings.Repeat("b", 40)); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("replace recorded commit: %v", err)
	}
	freshPool, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(freshPool.Close)
	fresh := NewStore(freshPool)
	found, err := fresh.Get(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertDeploymentEqual(t, found, recorded)
	const reason = "Source checkout failed."
	failed, err := fresh.FailSource(ctx, claim, reason)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != StatusFailed || failed.FailureStage == nil || *failed.FailureStage != StatusFetchingSource ||
		failed.FailureReason == nil || *failed.FailureReason != reason || failed.StartedAt == nil ||
		failed.FinishedAt == nil || failed.FinishedAt.Before(*failed.StartedAt) || failed.CommitSHA == nil || *failed.CommitSHA != sha {
		t.Fatalf("failed execution metadata: %+v", failed)
	}
	wantFailed := recorded
	wantFailed.Status, wantFailed.UpdatedAt = StatusFailed, failed.UpdatedAt
	wantFailed.FailureStage, wantFailed.FailureReason, wantFailed.FinishedAt = failed.FailureStage, failed.FailureReason, failed.FinishedAt
	assertDeploymentEqual(t, failed, wantFailed)
	var tokenCleared bool
	if err := pool.QueryRow(ctx, "SELECT claim_token IS NULL FROM deployments WHERE id = $1", claim.ID).Scan(&tokenCleared); err != nil || !tokenCleared {
		t.Fatalf("terminal claim token cleared = %t, %v", tokenCleared, err)
	}
	if _, err := store.RecordSourceCommit(ctx, claim, sha); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("record after terminal failure: %v", err)
	}
	if _, err := store.FailSource(ctx, claim, "Overwrite failure."); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("repeat terminal failure: %v", err)
	}
	found, err = fresh.Get(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertDeploymentEqual(t, found, failed)
	next, err := store.Create(ctx, project.ID)
	if err != nil || next.ID == failed.ID || next.Status != StatusQueued {
		t.Fatalf("new attempt after failure = %+v, %v", next, err)
	}
	for offset, want := range []Deployment{next, failed} {
		page, err := fresh.List(ctx, project.ID, 1, offset)
		if err != nil || len(page) != 1 {
			t.Fatalf("history page %d: %d rows, %v", offset, len(page), err)
		}
		assertDeploymentEqual(t, page[0], want)
	}
	if page, err := fresh.List(ctx, project.ID, 1, 2); err != nil || len(page) != 0 {
		t.Errorf("history after final page: %d rows, %v", len(page), err)
	}
	nextClaim, err := fresh.ClaimNext(ctx)
	if err != nil || nextClaim.ID != next.ID || nextClaim.Token == claim.Token {
		t.Fatalf("new attempt ownership = %+v, %v", nextClaim, err)
	}
	stale := nextClaim
	stale.Token = claim.Token
	if _, err := store.FailSource(ctx, stale, reason); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("old owner modifying new attempt: %v", err)
	}
	sha64 := strings.Repeat("c", 64)
	if recorded, err := fresh.RecordSourceCommit(ctx, nextClaim, sha64); err != nil || recorded.CommitSHA == nil || *recorded.CommitSHA != sha64 {
		t.Fatalf("record SHA-256 commit = %+v, %v", recorded, err)
	}
	boundaryReason := strings.Repeat("界", 1024)
	if failed, err := fresh.FailSource(ctx, nextClaim, boundaryReason); err != nil || failed.FailureReason == nil || *failed.FailureReason != boundaryReason {
		t.Fatalf("record maximum-length public failure reason: %v", err)
	}
}

func TestExecutionStoreCanceledOperationsLeaveStateUnchanged(t *testing.T) {
	pool, _ := newDeploymentStoreTestDatabase(t)
	store := NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	project := createDeploymentTestProject(t, ctx, pool, "canceled-execution")
	queued, err := store.Create(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancelOperation := context.WithCancel(ctx)
	cancelOperation()
	if _, err := store.ClaimNext(canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled claim: %v", err)
	}
	found, err := store.Get(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertDeploymentEqual(t, found, queued)
	claim, err := store.ClaimNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordSourceCommit(canceled, claim, strings.Repeat("a", 40)); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled commit recording: %v", err)
	}
	if _, err := store.FailSource(canceled, claim, "Source checkout failed."); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled failure recording: %v", err)
	}
	found, err = store.Get(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertDeploymentEqual(t, found, claim.Deployment)
}

func assertExecutionClaim(t *testing.T, claim Claim, queued Deployment) {
	t.Helper()
	if claim.Token == "" || claim.Status != StatusFetchingSource || claim.StartedAt == nil || claim.FinishedAt != nil ||
		claim.CommitSHA != nil || claim.FailureStage != nil || claim.FailureReason != nil {
		t.Fatalf("invalid claimed state: %+v", claim)
	}
	queued.Status, queued.UpdatedAt, queued.StartedAt = StatusFetchingSource, claim.UpdatedAt, claim.StartedAt
	assertDeploymentEqual(t, claim.Deployment, queued)
}
