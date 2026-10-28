-- +goose Up
ALTER TABLE deployments
    ADD COLUMN readiness_passed boolean,
    ADD COLUMN readiness_attempts integer,
    ADD COLUMN readiness_status_code integer,
    ADD COLUMN readiness_checked_at timestamptz,
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

-- +goose Down
LOCK TABLE deployments IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM deployments WHERE readiness_checked_at IS NOT NULL) THEN
        RAISE EXCEPTION USING ERRCODE = '55000',
            MESSAGE = 'Cannot remove readiness fields while observation data exists.';
    END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE deployments
    DROP CONSTRAINT deployments_readiness_check,
    DROP COLUMN readiness_checked_at,
    DROP COLUMN readiness_status_code,
    DROP COLUMN readiness_attempts,
    DROP COLUMN readiness_passed;
