package deployments

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrProjectNotFound = errors.New("project not found")
	ErrNotFound        = errors.New("deployment not found")
	ErrAlreadyQueued   = errors.New("project already has an unfinished deployment")
)

type Store interface {
	Create(ctx context.Context, projectID string) (Deployment, error)
	List(ctx context.Context, projectID string, limit, offset int) ([]Deployment, error)
	Get(ctx context.Context, id string) (Deployment, error)
}

type PostgresStore struct {
	pool *pgxpool.Pool
}

const deploymentColumns = `id::text, project_id::text, git_url, branch, app_port, health_path,
	status, commit_sha, image_reference, image_id, instance_id::text,
	readiness_passed, readiness_attempts, readiness_status_code, readiness_checked_at,
	failure_stage, failure_reason, started_at, finished_at, created_at, updated_at`

func NewStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) Create(ctx context.Context, projectID string) (Deployment, error) {
	// Copy all settings from one database snapshot, including during project edits.
	deployment, err := scanDeployment(s.pool.QueryRow(ctx, `
		INSERT INTO deployments (project_id, git_url, branch, app_port, health_path)
		SELECT id, git_url, branch, app_port, health_path FROM projects WHERE id = $1
		RETURNING `+deploymentColumns, projectID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrProjectNotFound
	}
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) {
			switch {
			case postgresError.Code == "23505" && postgresError.ConstraintName == "deployments_one_unfinished_per_project":
				return Deployment{}, ErrAlreadyQueued
			case postgresError.Code == "23503" && postgresError.ConstraintName == "deployments_project_id_fkey":
				return Deployment{}, ErrProjectNotFound
			}
		}
		return Deployment{}, fmt.Errorf("create deployment: %w", err)
	}
	return deployment, nil
}

func (s *PostgresStore) Get(ctx context.Context, id string) (Deployment, error) {
	deployment, err := scanDeployment(s.pool.QueryRow(ctx, `
		SELECT `+deploymentColumns+`
		FROM deployments WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("get deployment: %w", err)
	}
	return deployment, nil
}

func (s *PostgresStore) List(ctx context.Context, projectID string, limit, offset int) ([]Deployment, error) {
	// Keep the existence check and history read consistent if an empty project is deleted.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin deployment history read: %w", err)
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1)", projectID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check deployment project: %w", err)
	}
	if !exists {
		return nil, ErrProjectNotFound
	}
	rows, err := tx.Query(ctx, `
		SELECT `+deploymentColumns+`
		FROM deployments WHERE project_id = $1
		ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, projectID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()
	deployments := make([]Deployment, 0)
	for rows.Next() {
		deployment, err := scanDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("read deployment: %w", err)
		}
		deployments = append(deployments, deployment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("finish deployment history read: %w", err)
	}
	return deployments, nil
}

func scanDeployment(row pgx.Row, extra ...any) (Deployment, error) {
	var deployment Deployment
	fields := []any{&deployment.ID, &deployment.ProjectID, &deployment.GitURL, &deployment.Branch,
		&deployment.AppPort, &deployment.HealthPath, &deployment.Status, &deployment.CommitSHA,
		&deployment.ImageReference, &deployment.ImageID, &deployment.InstanceID,
		&deployment.ReadinessPassed, &deployment.ReadinessAttempts, &deployment.ReadinessStatusCode, &deployment.ReadinessCheckedAt,
		&deployment.FailureStage, &deployment.FailureReason, &deployment.StartedAt, &deployment.FinishedAt,
		&deployment.CreatedAt, &deployment.UpdatedAt}
	err := row.Scan(append(fields, extra...)...)
	deployment.CreatedAt = deployment.CreatedAt.UTC()
	deployment.UpdatedAt = deployment.UpdatedAt.UTC()
	if deployment.StartedAt != nil {
		*deployment.StartedAt = deployment.StartedAt.UTC()
	}
	if deployment.FinishedAt != nil {
		*deployment.FinishedAt = deployment.FinishedAt.UTC()
	}
	if deployment.ReadinessCheckedAt != nil {
		*deployment.ReadinessCheckedAt = deployment.ReadinessCheckedAt.UTC()
	}
	return deployment, err
}
