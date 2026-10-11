-- +goose Up
CREATE TABLE projects (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL CHECK (name = btrim(name) AND char_length(name) BETWEEN 1 AND 100),
    slug text NOT NULL UNIQUE CHECK (
        char_length(slug) BETWEEN 1 AND 63
        AND slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'
    ),
    git_url text NOT NULL CHECK (git_url = btrim(git_url) AND char_length(git_url) BETWEEN 1 AND 2048),
    branch text NOT NULL CHECK (branch = btrim(branch) AND char_length(branch) BETWEEN 1 AND 255),
    app_port integer NOT NULL CHECK (app_port BETWEEN 1 AND 65535),
    health_path text NOT NULL CHECK (health_path LIKE '/%' AND char_length(health_path) <= 2048),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE projects;
