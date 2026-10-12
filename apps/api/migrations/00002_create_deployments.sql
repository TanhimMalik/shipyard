-- +goose Up
CREATE TABLE deployments (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL CONSTRAINT deployments_project_id_fkey REFERENCES projects(id) ON DELETE RESTRICT,
    git_url text NOT NULL CHECK (git_url = btrim(git_url) AND char_length(git_url) BETWEEN 1 AND 2048),
    branch text NOT NULL CHECK (branch = btrim(branch) AND char_length(branch) BETWEEN 1 AND 255),
    app_port integer NOT NULL CHECK (app_port BETWEEN 1 AND 65535),
    health_path text NOT NULL CHECK (health_path LIKE '/%' AND char_length(health_path) <= 2048),
    status text NOT NULL DEFAULT 'QUEUED' CONSTRAINT deployments_status_check CHECK (status = 'QUEUED'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX deployments_one_queued_per_project ON deployments (project_id) WHERE status = 'QUEUED';
CREATE INDEX deployments_project_history ON deployments (project_id, created_at DESC, id DESC);

-- +goose Down
DROP TABLE deployments;
