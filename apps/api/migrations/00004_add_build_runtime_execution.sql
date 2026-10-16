-- +goose Up
CREATE TABLE runtime_instances (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id uuid NOT NULL REFERENCES deployments(id) ON DELETE RESTRICT,
    container_id text UNIQUE CHECK (container_id ~ '^[0-9a-f]{64}$'),
    observed_status text CHECK (observed_status IN ('created', 'running', 'paused', 'restarting', 'removing', 'exited', 'dead')),
    running boolean,
    exit_code integer,
    oom_killed boolean,
    host_port integer CHECK (host_port BETWEEN 1 AND 65535),
    observed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, deployment_id),
    CONSTRAINT runtime_instances_observation_check CHECK (
        (container_id IS NULL AND observed_status IS NULL AND running IS NULL
         AND exit_code IS NULL AND oom_killed IS NULL AND host_port IS NULL AND observed_at IS NULL)
        OR
        (container_id IS NOT NULL AND observed_status IS NOT NULL AND running IS NOT NULL
         AND exit_code IS NOT NULL AND oom_killed IS NOT NULL AND observed_at IS NOT NULL
         AND ((running AND host_port IS NOT NULL) OR (NOT running AND host_port IS NULL)))
    )
);
CREATE INDEX runtime_instances_deployment ON runtime_instances (deployment_id, created_at, id);

ALTER TABLE deployments
    ADD COLUMN image_reference text,
    ADD COLUMN image_id text,
    ADD COLUMN instance_id uuid,
    ADD CONSTRAINT deployments_instance_fkey FOREIGN KEY (instance_id, id)
        REFERENCES runtime_instances(id, deployment_id) ON DELETE RESTRICT,
    ADD CONSTRAINT deployments_image_check CHECK (
        (image_reference IS NULL AND image_id IS NULL)
        OR
        (image_reference IS NOT NULL AND image_id IS NOT NULL
         AND image_reference ~ ('^shipyard/deployments:' || id::text || '-[0-9a-f]{32}$')
         AND image_id ~ '^sha256:[0-9a-f]{64}$')
    ),
    DROP CONSTRAINT deployments_status_check,
    ADD CONSTRAINT deployments_status_check CHECK (
        status IN ('QUEUED', 'FETCHING_SOURCE', 'BUILDING', 'STARTING', 'HEALTH_CHECKING', 'FAILED')
    ),
    DROP CONSTRAINT deployments_source_state_check,
    ADD CONSTRAINT deployments_execution_state_check CHECK (
        (status = 'QUEUED' AND commit_sha IS NULL AND image_id IS NULL AND instance_id IS NULL
         AND failure_stage IS NULL AND failure_reason IS NULL
         AND started_at IS NULL AND finished_at IS NULL AND claim_token IS NULL)
        OR
        (status IN ('FETCHING_SOURCE', 'BUILDING', 'STARTING', 'HEALTH_CHECKING')
         AND started_at IS NOT NULL AND finished_at IS NULL AND claim_token IS NOT NULL
         AND failure_stage IS NULL AND failure_reason IS NULL
         AND (
             (status = 'FETCHING_SOURCE' AND image_id IS NULL AND instance_id IS NULL)
             OR (status = 'BUILDING' AND commit_sha IS NOT NULL AND image_id IS NULL AND instance_id IS NULL)
             OR (status IN ('STARTING', 'HEALTH_CHECKING') AND commit_sha IS NOT NULL
                 AND image_id IS NOT NULL AND instance_id IS NOT NULL)
         ))
        OR
        (status = 'FAILED' AND started_at IS NOT NULL AND finished_at IS NOT NULL
         AND claim_token IS NULL AND failure_stage IS NOT NULL AND failure_reason IS NOT NULL
         AND (
             (failure_stage = 'FETCHING_SOURCE' AND image_id IS NULL AND instance_id IS NULL)
             OR (failure_stage = 'BUILDING' AND commit_sha IS NOT NULL AND image_id IS NULL AND instance_id IS NULL)
             OR (failure_stage IN ('STARTING', 'HEALTH_CHECKING') AND commit_sha IS NOT NULL
                 AND image_id IS NOT NULL AND instance_id IS NOT NULL)
         ))
    );

DROP INDEX deployments_one_unfinished_per_project;
CREATE UNIQUE INDEX deployments_one_unfinished_per_project ON deployments (project_id)
    WHERE status IN ('QUEUED', 'FETCHING_SOURCE', 'BUILDING', 'STARTING', 'HEALTH_CHECKING');

-- +goose Down
LOCK TABLE deployments, runtime_instances IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM runtime_instances)
       OR EXISTS (
           SELECT 1 FROM deployments
           WHERE image_reference IS NOT NULL OR image_id IS NOT NULL OR instance_id IS NOT NULL
              OR status NOT IN ('QUEUED', 'FETCHING_SOURCE', 'FAILED')
              OR (failure_stage IS NOT NULL AND failure_stage <> 'FETCHING_SOURCE')
       ) THEN
        RAISE EXCEPTION USING ERRCODE = '55000',
            MESSAGE = 'Cannot remove build and runtime fields while execution data exists.';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP INDEX deployments_one_unfinished_per_project;
CREATE UNIQUE INDEX deployments_one_unfinished_per_project ON deployments (project_id)
    WHERE status IN ('QUEUED', 'FETCHING_SOURCE');

ALTER TABLE deployments
    DROP CONSTRAINT deployments_instance_fkey,
    DROP CONSTRAINT deployments_execution_state_check,
    DROP CONSTRAINT deployments_image_check,
    DROP CONSTRAINT deployments_status_check,
    ADD CONSTRAINT deployments_status_check CHECK (status IN ('QUEUED', 'FETCHING_SOURCE', 'FAILED')),
    DROP COLUMN instance_id,
    DROP COLUMN image_id,
    DROP COLUMN image_reference,
    ADD CONSTRAINT deployments_source_state_check CHECK (
        (
            status = 'QUEUED'
            AND commit_sha IS NULL
            AND failure_stage IS NULL
            AND failure_reason IS NULL
            AND started_at IS NULL
            AND finished_at IS NULL
            AND claim_token IS NULL
        ) OR (
            status = 'FETCHING_SOURCE'
            AND started_at IS NOT NULL
            AND finished_at IS NULL
            AND failure_stage IS NULL
            AND failure_reason IS NULL
            AND claim_token IS NOT NULL
        ) OR (
            status = 'FAILED'
            AND started_at IS NOT NULL
            AND finished_at IS NOT NULL
            AND failure_stage IS NOT NULL
            AND failure_stage = 'FETCHING_SOURCE'
            AND failure_reason IS NOT NULL
            AND claim_token IS NULL
        )
    );

DROP TABLE runtime_instances;
