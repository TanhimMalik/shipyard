package projects

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrSlugTaken = errors.New("project slug already exists")
	ErrNotFound  = errors.New("project not found")
)

type Store interface {
	Create(context.Context, CreateInput) (Project, error)
	List(ctx context.Context, limit, offset int) ([]Project, error)
	Get(ctx context.Context, id string) (Project, error)
	Update(ctx context.Context, id string, input PatchInput) (Project, error)
	Delete(ctx context.Context, id string) error
}

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) Create(ctx context.Context, input CreateInput) (Project, error) {
	project, err := scanProject(s.pool.QueryRow(ctx, `
		INSERT INTO projects (name, slug, git_url, branch, app_port, health_path)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text, name, slug, git_url, branch, app_port, health_path, created_at, updated_at`,
		input.Name, input.Slug, input.GitURL, input.Branch, input.AppPort, input.HealthPath))
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" && postgresError.ConstraintName == "projects_slug_key" {
			return Project{}, ErrSlugTaken
		}
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return project, nil
}

func (s *PostgresStore) List(ctx context.Context, limit, offset int) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, slug, git_url, branch, app_port, health_path, created_at, updated_at
		FROM projects ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	projects := make([]Project, 0)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("read project: %w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return projects, nil
}

func (s *PostgresStore) Get(ctx context.Context, id string) (Project, error) {
	project, err := scanProject(s.pool.QueryRow(ctx, `
		SELECT id::text, name, slug, git_url, branch, app_port, health_path, created_at, updated_at
		FROM projects WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("get project: %w", err)
	}
	return project, nil
}

func (s *PostgresStore) Update(ctx context.Context, id string, input PatchInput) (Project, error) {
	project, err := scanProject(s.pool.QueryRow(ctx, `
		UPDATE projects
		SET name = COALESCE($2, name),
		    slug = COALESCE($3, slug),
		    git_url = COALESCE($4, git_url),
		    branch = COALESCE($5, branch),
		    app_port = COALESCE($6, app_port),
		    health_path = COALESCE($7, health_path),
		    updated_at = clock_timestamp()
		WHERE id = $1
		RETURNING id::text, name, slug, git_url, branch, app_port, health_path, created_at, updated_at`,
		id, input.Name, input.Slug, input.GitURL, input.Branch, input.AppPort, input.HealthPath))
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" && postgresError.ConstraintName == "projects_slug_key" {
			return Project{}, ErrSlugTaken
		}
		return Project{}, fmt.Errorf("update project: %w", err)
	}
	return project, nil
}

func (s *PostgresStore) Delete(ctx context.Context, id string) error {
	result, err := s.pool.Exec(ctx, "DELETE FROM projects WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanProject(row pgx.Row) (Project, error) {
	var project Project
	err := row.Scan(&project.ID, &project.Name, &project.Slug, &project.GitURL,
		&project.Branch, &project.AppPort, &project.HealthPath, &project.CreatedAt, &project.UpdatedAt)
	project.CreatedAt = project.CreatedAt.UTC()
	project.UpdatedAt = project.UpdatedAt.UTC()
	return project, err
}
