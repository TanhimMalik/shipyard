package deployments

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

var (
	imageIDPattern        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	imageReferencePattern = regexp.MustCompile(`^shipyard/deployments:([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})-[0-9a-f]{32}$`)
)

// BeginBuild requires an already recorded exact commit. Same-stage retries do
// not change timestamps; a stale owner cannot advance a different attempt.
func (s *PostgresStore) BeginBuild(ctx context.Context, claim Claim) (Deployment, error) {
	deployment, err := scanDeployment(s.pool.QueryRow(ctx, `
		UPDATE deployments
		SET status = 'BUILDING',
		    updated_at = CASE WHEN status = 'FETCHING_SOURCE' THEN now() ELSE updated_at END
		WHERE id = $1 AND claim_token = $2
		  AND status IN ('FETCHING_SOURCE', 'BUILDING') AND commit_sha IS NOT NULL
		RETURNING `+deploymentColumns, claim.ID, claim.Token))
	return executionResult(deployment, err, "begin image build")
}

// BeginStart records the immutable build result and a stable instance identity
// in one transaction, before the executor creates anything in Docker.
func (s *PostgresStore) BeginStart(ctx context.Context, claim Claim, reference, imageID string) (Deployment, error) {
	parts := imageReferencePattern.FindStringSubmatch(reference)
	if len(parts) != 2 || parts[1] != strings.ToLower(claim.ID) || !imageIDPattern.MatchString(imageID) {
		return Deployment{}, ErrInvalidExecution
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Deployment{}, fmt.Errorf("begin instance intent: %w", err)
	}
	defer tx.Rollback(ctx)
	deployment, err := lockExecution(ctx, tx, claim, StatusBuilding, StatusStarting)
	if err != nil {
		return Deployment{}, err
	}
	if deployment.Status == StatusStarting {
		if deployment.ImageReference == nil || *deployment.ImageReference != reference || deployment.ImageID == nil || *deployment.ImageID != imageID {
			return Deployment{}, ErrInvalidTransition
		}
		return deployment, nil
	}
	var instanceID string
	if err := tx.QueryRow(ctx, `INSERT INTO runtime_instances (deployment_id) VALUES ($1) RETURNING id::text`, claim.ID).Scan(&instanceID); err != nil {
		return Deployment{}, fmt.Errorf("record instance intent: %w", err)
	}
	deployment, err = scanDeployment(tx.QueryRow(ctx, `
		UPDATE deployments SET status = 'STARTING', image_reference = $3, image_id = $4,
		       instance_id = $5, updated_at = now()
		WHERE id = $1 AND claim_token = $2 AND status = 'BUILDING'
		RETURNING `+deploymentColumns, claim.ID, claim.Token, reference, imageID, instanceID))
	if err != nil {
		return executionResult(deployment, err, "record build result")
	}
	if err := tx.Commit(ctx); err != nil {
		return Deployment{}, fmt.Errorf("commit instance intent: %w", err)
	}
	return deployment, nil
}

func (s *PostgresStore) BeginHealthCheck(ctx context.Context, claim Claim) (Deployment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Deployment{}, fmt.Errorf("begin readiness transition: %w", err)
	}
	defer tx.Rollback(ctx)
	deployment, err := lockExecution(ctx, tx, claim, StatusStarting, StatusHealthChecking)
	if err != nil {
		return Deployment{}, err
	}
	var running bool
	if err := tx.QueryRow(ctx, `
		SELECT container_id IS NOT NULL AND observed_status = 'running' AND running IS TRUE AND host_port IS NOT NULL
		FROM runtime_instances WHERE id = $1 AND deployment_id = $2`, deployment.InstanceID, deployment.ID).Scan(&running); err != nil {
		return Deployment{}, fmt.Errorf("read candidate instance: %w", err)
	}
	if !running {
		return Deployment{}, ErrInvalidTransition
	}
	if deployment.Status == StatusHealthChecking {
		return deployment, nil
	}
	deployment, err = scanDeployment(tx.QueryRow(ctx, `
		UPDATE deployments SET status = 'HEALTH_CHECKING', updated_at = now()
		WHERE id = $1 AND claim_token = $2 AND status = 'STARTING'
		RETURNING `+deploymentColumns, claim.ID, claim.Token))
	if err != nil {
		return executionResult(deployment, err, "begin readiness checks")
	}
	if err := tx.Commit(ctx); err != nil {
		return Deployment{}, fmt.Errorf("commit readiness transition: %w", err)
	}
	return deployment, nil
}

func lockExecution(ctx context.Context, tx pgx.Tx, claim Claim, stages ...string) (Deployment, error) {
	deployment, err := scanDeployment(tx.QueryRow(ctx, `
		SELECT `+deploymentColumns+` FROM deployments
		WHERE id = $1 AND claim_token = $2 AND status = ANY($3::text[]) FOR UPDATE`, claim.ID, claim.Token, stages))
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrInvalidTransition
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("lock deployment execution: %w", err)
	}
	return deployment, nil
}
