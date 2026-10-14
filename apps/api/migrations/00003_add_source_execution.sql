-- +goose Up
ALTER TABLE deployments
    ADD COLUMN commit_sha text,
    ADD COLUMN failure_stage text,
    ADD COLUMN failure_reason text,
    ADD COLUMN started_at timestamptz,
    ADD COLUMN finished_at timestamptz,
    ADD COLUMN claim_token uuid,
    DROP CONSTRAINT deployments_status_check,
    ADD CONSTRAINT deployments_status_check CHECK (status IN ('QUEUED', 'FETCHING_SOURCE', 'FAILED')),
    ADD CONSTRAINT deployments_commit_sha_check CHECK (
        commit_sha IS NULL OR commit_sha ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    ADD CONSTRAINT deployments_failure_reason_check CHECK (
        failure_reason IS NULL OR (
            failure_reason = btrim(failure_reason)
            AND char_length(failure_reason) BETWEEN 1 AND 1024
            AND failure_reason !~ '[[:cntrl:]]'
        )
    ),
    ADD CONSTRAINT deployments_source_timestamps_check CHECK (
        finished_at IS NULL OR finished_at >= started_at
    ),
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

DROP INDEX deployments_one_queued_per_project;
CREATE UNIQUE INDEX deployments_one_unfinished_per_project ON deployments (project_id)
    WHERE status IN ('QUEUED', 'FETCHING_SOURCE');
CREATE INDEX deployments_pending_queue ON deployments (created_at, id) WHERE status = 'QUEUED';

-- +goose Down
-- Keep the inspection and schema change in the same locked transaction.
LOCK TABLE deployments IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM deployments
        WHERE status <> 'QUEUED'
           OR commit_sha IS NOT NULL
           OR failure_stage IS NOT NULL
           OR failure_reason IS NOT NULL
           OR started_at IS NOT NULL
           OR finished_at IS NOT NULL
           OR claim_token IS NOT NULL
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'Cannot remove source execution fields while deployment lifecycle or source data exists.';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP INDEX deployments_one_unfinished_per_project;
DROP INDEX deployments_pending_queue;
CREATE UNIQUE INDEX deployments_one_queued_per_project ON deployments (project_id) WHERE status = 'QUEUED';

ALTER TABLE deployments
    DROP CONSTRAINT deployments_source_state_check,
    DROP CONSTRAINT deployments_source_timestamps_check,
    DROP CONSTRAINT deployments_failure_reason_check,
    DROP CONSTRAINT deployments_commit_sha_check,
    DROP CONSTRAINT deployments_status_check,
    ADD CONSTRAINT deployments_status_check CHECK (status = 'QUEUED'),
    DROP COLUMN claim_token,
    DROP COLUMN finished_at,
    DROP COLUMN started_at,
    DROP COLUMN failure_reason,
    DROP COLUMN failure_stage,
    DROP COLUMN commit_sha;
