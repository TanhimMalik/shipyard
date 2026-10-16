package deployments

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var (
	ErrNoQueuedDeployments = errors.New("no queued deployments")
	ErrInvalidTransition   = errors.New("deployment is not owned by this claim in the required state")
	ErrInvalidExecution    = errors.New("invalid deployment execution metadata")
	commitPattern          = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// ClaimNext takes the oldest unlocked queued attempt. Claiming also keeps the
// project's unfinished-attempt slot occupied until this attempt is terminal.
func (s *PostgresStore) ClaimNext(ctx context.Context) (Claim, error) {
	var claim Claim
	deployment, err := scanDeployment(s.pool.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id FROM deployments WHERE status = 'QUEUED'
			ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		UPDATE deployments
		SET status = 'FETCHING_SOURCE', claim_token = gen_random_uuid(),
		    started_at = now(), updated_at = now()
		WHERE id = (SELECT id FROM candidate) AND status = 'QUEUED'
		RETURNING `+deploymentColumns+`, claim_token::text`), &claim.Token)
	if errors.Is(err, pgx.ErrNoRows) {
		return Claim{}, ErrNoQueuedDeployments
	}
	if err != nil {
		return Claim{}, fmt.Errorf("claim deployment: %w", err)
	}
	claim.Deployment = deployment
	return claim, nil
}

// RecordSourceCommit records the fetched commit once. Repeating the same write
// is harmless; a different commit or a stale claim cannot replace it.
func (s *PostgresStore) RecordSourceCommit(ctx context.Context, claim Claim, commit string) (Deployment, error) {
	if !commitPattern.MatchString(commit) {
		return Deployment{}, ErrInvalidExecution
	}
	deployment, err := scanDeployment(s.pool.QueryRow(ctx, `
		UPDATE deployments
		SET commit_sha = $3,
		    updated_at = CASE WHEN commit_sha IS NULL THEN now() ELSE updated_at END
		WHERE id = $1 AND claim_token = $2 AND status = 'FETCHING_SOURCE'
		  AND (commit_sha IS NULL OR commit_sha = $3)
		RETURNING `+deploymentColumns, claim.ID, claim.Token, commit))
	return executionResult(deployment, err, "record source commit")
}

// FailSource persists a caller-supplied public reason, never a raw Git or storage
// error. FAILED is terminal; retrying creates a new attempt with a new claim.
func (s *PostgresStore) FailSource(ctx context.Context, claim Claim, reason string) (Deployment, error) {
	return s.FailExecution(ctx, claim, StatusFetchingSource, reason)
}

// FailExecution only accepts a public reason and the exact current stage.
// Cleanup is separate: a failed attempt may still own resources to inspect.
func (s *PostgresStore) FailExecution(ctx context.Context, claim Claim, stage, reason string) (Deployment, error) {
	if !validFailureReason(reason) || !executionStage(stage) {
		return Deployment{}, ErrInvalidExecution
	}
	deployment, err := scanDeployment(s.pool.QueryRow(ctx, `
		UPDATE deployments
		SET status = 'FAILED', failure_stage = $3, failure_reason = $4,
		    finished_at = now(), updated_at = now(), claim_token = NULL
		WHERE id = $1 AND claim_token = $2 AND status = $3
		RETURNING `+deploymentColumns, claim.ID, claim.Token, stage, reason))
	return executionResult(deployment, err, "fail deployment execution")
}

func executionStage(stage string) bool {
	return stage == StatusFetchingSource || stage == StatusBuilding || stage == StatusStarting || stage == StatusHealthChecking
}

func executionResult(deployment Deployment, err error, operation string) (Deployment, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrInvalidTransition
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("%s: %w", operation, err)
	}
	return deployment, nil
}

func validFailureReason(value string) bool {
	return utf8.ValidString(value) && value != "" && value == strings.TrimSpace(value) &&
		utf8.RuneCountInString(value) <= 1024 && strings.IndexFunc(value, unicode.IsControl) < 0
}
