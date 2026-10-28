-- +goose Up
ALTER TABLE deployments
    ADD CONSTRAINT deployments_id_project_key UNIQUE (id, project_id),
    DROP CONSTRAINT deployments_status_check,
    ADD CONSTRAINT deployments_status_check CHECK (
        status IN ('QUEUED', 'FETCHING_SOURCE', 'BUILDING', 'STARTING', 'HEALTH_CHECKING',
                   'ACTIVATING', 'RUNNING', 'SUPERSEDED', 'FAILED')
    ),
    DROP CONSTRAINT deployments_execution_state_check,
    ADD CONSTRAINT deployments_execution_state_check CHECK (
        (status = 'QUEUED' AND commit_sha IS NULL AND image_id IS NULL AND instance_id IS NULL
         AND failure_stage IS NULL AND failure_reason IS NULL
         AND started_at IS NULL AND finished_at IS NULL AND claim_token IS NULL)
        OR
        (status IN ('FETCHING_SOURCE', 'BUILDING', 'STARTING', 'HEALTH_CHECKING', 'ACTIVATING')
         AND started_at IS NOT NULL AND finished_at IS NULL AND claim_token IS NOT NULL
         AND failure_stage IS NULL AND failure_reason IS NULL
         AND (
             (status = 'FETCHING_SOURCE' AND image_id IS NULL AND instance_id IS NULL)
             OR (status = 'BUILDING' AND commit_sha IS NOT NULL AND image_id IS NULL AND instance_id IS NULL)
             OR (status IN ('STARTING', 'HEALTH_CHECKING') AND commit_sha IS NOT NULL
                 AND image_id IS NOT NULL AND instance_id IS NOT NULL)
             OR (status = 'ACTIVATING' AND commit_sha IS NOT NULL AND image_id IS NOT NULL
                 AND instance_id IS NOT NULL AND readiness_passed IS TRUE)
         ))
        OR
        (status IN ('RUNNING', 'SUPERSEDED') AND commit_sha IS NOT NULL
         AND image_id IS NOT NULL AND instance_id IS NOT NULL AND readiness_passed IS TRUE
         AND started_at IS NOT NULL AND finished_at IS NOT NULL AND claim_token IS NULL
         AND failure_stage IS NULL AND failure_reason IS NULL)
        OR
        (status = 'FAILED' AND started_at IS NOT NULL AND finished_at IS NOT NULL
         AND claim_token IS NULL AND failure_stage IS NOT NULL AND failure_reason IS NOT NULL
         AND (
             (failure_stage = 'FETCHING_SOURCE' AND image_id IS NULL AND instance_id IS NULL)
             OR (failure_stage = 'BUILDING' AND commit_sha IS NOT NULL AND image_id IS NULL AND instance_id IS NULL)
             OR (failure_stage IN ('STARTING', 'HEALTH_CHECKING') AND commit_sha IS NOT NULL
                 AND image_id IS NOT NULL AND instance_id IS NOT NULL)
             OR (failure_stage = 'ACTIVATING' AND commit_sha IS NOT NULL AND image_id IS NOT NULL
                 AND instance_id IS NOT NULL AND readiness_passed IS TRUE)
         ))
    ),
    DROP CONSTRAINT deployments_readiness_check,
    ADD CONSTRAINT deployments_readiness_check CHECK (
        (readiness_passed IS NULL AND readiness_attempts IS NULL
         AND readiness_status_code IS NULL AND readiness_checked_at IS NULL)
        OR
        (readiness_passed IS NOT NULL AND readiness_attempts IS NOT NULL
         AND readiness_status_code IS NOT NULL AND readiness_checked_at IS NOT NULL
         AND instance_id IS NOT NULL
         AND (status IN ('HEALTH_CHECKING', 'ACTIVATING', 'RUNNING', 'SUPERSEDED')
              OR (status = 'FAILED' AND failure_stage IN ('HEALTH_CHECKING', 'ACTIVATING')))
         AND readiness_attempts > 0
         AND (readiness_status_code = 0 OR readiness_status_code BETWEEN 100 AND 999)
         AND (NOT readiness_passed OR readiness_status_code BETWEEN 200 AND 299)
         AND isfinite(readiness_checked_at)
         AND readiness_checked_at >= timestamptz '0001-01-01 00:00:00+00'
         AND readiness_checked_at <= timestamptz '9999-12-31 23:59:59.999999+00')
    );

DROP INDEX deployments_one_unfinished_per_project;
CREATE UNIQUE INDEX deployments_one_unfinished_per_project ON deployments (project_id)
    WHERE status IN ('QUEUED', 'FETCHING_SOURCE', 'BUILDING', 'STARTING', 'HEALTH_CHECKING', 'ACTIVATING');

CREATE TABLE activation_intents (
    deployment_id uuid PRIMARY KEY,
    project_id uuid NOT NULL,
    instance_id uuid NOT NULL,
    container_id text NOT NULL CHECK (container_id ~ '^[0-9a-f]{64}$'),
    host_port integer NOT NULL CHECK (host_port BETWEEN 1 AND 65535),
    previous_deployment_id uuid CHECK (previous_deployment_id <> deployment_id),
    created_at timestamptz NOT NULL DEFAULT now() CHECK (isfinite(created_at)),
    confirmed_at timestamptz CHECK (confirmed_at IS NULL OR isfinite(confirmed_at)),
    CONSTRAINT activation_intents_deployment_project_key UNIQUE (deployment_id, project_id),
    CONSTRAINT activation_intents_confirmed_key UNIQUE (deployment_id, project_id, confirmed_at),
    CONSTRAINT activation_intents_deployment_fkey FOREIGN KEY (deployment_id, project_id)
        REFERENCES deployments(id, project_id) ON DELETE RESTRICT,
    CONSTRAINT activation_intents_instance_fkey FOREIGN KEY (instance_id, deployment_id)
        REFERENCES runtime_instances(id, deployment_id) ON DELETE RESTRICT,
    CONSTRAINT activation_intents_previous_fkey FOREIGN KEY (previous_deployment_id, project_id)
        REFERENCES activation_intents(deployment_id, project_id) ON DELETE RESTRICT
);

CREATE TABLE active_routes (
    project_id uuid PRIMARY KEY REFERENCES projects(id) ON DELETE RESTRICT,
    deployment_id uuid NOT NULL,
    confirmed_at timestamptz NOT NULL,
    CONSTRAINT active_routes_intent_fkey FOREIGN KEY (deployment_id, project_id, confirmed_at)
        REFERENCES activation_intents(deployment_id, project_id, confirmed_at) ON DELETE RESTRICT
);

-- +goose Down
LOCK TABLE deployments, activation_intents, active_routes IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM activation_intents)
       OR EXISTS (SELECT 1 FROM active_routes)
       OR EXISTS (
           SELECT 1 FROM deployments
           WHERE status IN ('ACTIVATING', 'RUNNING', 'SUPERSEDED') OR failure_stage = 'ACTIVATING'
       ) THEN
        RAISE EXCEPTION USING ERRCODE = '55000',
            MESSAGE = 'Cannot remove activation state while activation data exists.';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TABLE active_routes;
DROP TABLE activation_intents;

DROP INDEX deployments_one_unfinished_per_project;
CREATE UNIQUE INDEX deployments_one_unfinished_per_project ON deployments (project_id)
    WHERE status IN ('QUEUED', 'FETCHING_SOURCE', 'BUILDING', 'STARTING', 'HEALTH_CHECKING');

ALTER TABLE deployments
    DROP CONSTRAINT deployments_id_project_key,
    DROP CONSTRAINT deployments_status_check,
    ADD CONSTRAINT deployments_status_check CHECK (
        status IN ('QUEUED', 'FETCHING_SOURCE', 'BUILDING', 'STARTING', 'HEALTH_CHECKING', 'FAILED')
    ),
    DROP CONSTRAINT deployments_execution_state_check,
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
    ),
    DROP CONSTRAINT deployments_readiness_check,
    ADD CONSTRAINT deployments_readiness_check CHECK (
        (readiness_passed IS NULL AND readiness_attempts IS NULL
         AND readiness_status_code IS NULL AND readiness_checked_at IS NULL)
        OR
        (readiness_passed IS NOT NULL AND readiness_attempts IS NOT NULL
         AND readiness_status_code IS NOT NULL AND readiness_checked_at IS NOT NULL
         AND instance_id IS NOT NULL
         AND (status = 'HEALTH_CHECKING' OR (status = 'FAILED' AND failure_stage = 'HEALTH_CHECKING'))
         AND readiness_attempts > 0
         AND (readiness_status_code = 0 OR readiness_status_code BETWEEN 100 AND 999)
         AND (NOT readiness_passed OR readiness_status_code BETWEEN 200 AND 299)
         AND isfinite(readiness_checked_at)
         AND readiness_checked_at >= timestamptz '0001-01-01 00:00:00+00'
         AND readiness_checked_at <= timestamptz '9999-12-31 23:59:59.999999+00')
    );
