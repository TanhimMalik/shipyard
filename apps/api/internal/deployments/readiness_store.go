package deployments

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type ReadinessObservation struct {
	ContainerID string
	HostPort    int
	Passed      bool
	Attempts    int
	StatusCode  int
	CheckedAt   time.Time
}

// RecordReadiness saves the final point-in-time check without implying route
// activation. Only identical saved result metadata can be repeated for this instance.
func (s *PostgresStore) RecordReadiness(ctx context.Context, claim Claim, instanceID string, observed ReadinessObservation) (Deployment, error) {
	observed.CheckedAt = observed.CheckedAt.UTC().Truncate(time.Microsecond)
	if !validReadinessObservation(observed) {
		return Deployment{}, ErrInvalidExecution
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Deployment{}, fmt.Errorf("begin readiness observation: %w", err)
	}
	defer tx.Rollback(ctx)
	deployment, err := lockExecution(ctx, tx, claim, StatusHealthChecking)
	if err != nil {
		return Deployment{}, err
	}
	if deployment.InstanceID == nil || !strings.EqualFold(*deployment.InstanceID, instanceID) {
		return Deployment{}, ErrInvalidTransition
	}
	instance, err := scanInstance(tx.QueryRow(ctx, `SELECT `+instanceColumns+`
		FROM runtime_instances WHERE id = $1 AND deployment_id = $2`, instanceID, claim.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrInvalidTransition
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("read readiness instance: %w", err)
	}
	if instance.ContainerID == nil || *instance.ContainerID != observed.ContainerID {
		return Deployment{}, ErrInvalidTransition
	}
	if deployment.ReadinessPassed != nil {
		if *deployment.ReadinessPassed != observed.Passed || deployment.ReadinessAttempts == nil ||
			*deployment.ReadinessAttempts != observed.Attempts || deployment.ReadinessStatusCode == nil ||
			*deployment.ReadinessStatusCode != observed.StatusCode || deployment.ReadinessCheckedAt == nil ||
			!deployment.ReadinessCheckedAt.Equal(observed.CheckedAt) {
			return Deployment{}, ErrInvalidTransition
		}
		return deployment, nil
	}
	if observed.Passed && (instance.ObservedStatus == nil || *instance.ObservedStatus != "running" ||
		instance.Running == nil || !*instance.Running || instance.HostPort == nil || *instance.HostPort != observed.HostPort) {
		return Deployment{}, ErrInvalidTransition
	}
	deployment, err = scanDeployment(tx.QueryRow(ctx, `
		UPDATE deployments SET readiness_passed = $3, readiness_attempts = $4,
		    readiness_status_code = $5, readiness_checked_at = $6, updated_at = now()
		WHERE id = $1 AND claim_token = $2 AND status = 'HEALTH_CHECKING'
		RETURNING `+deploymentColumns, claim.ID, claim.Token, observed.Passed, observed.Attempts, observed.StatusCode, observed.CheckedAt))
	if err != nil {
		return executionResult(deployment, err, "record readiness observation")
	}
	if err := tx.Commit(ctx); err != nil {
		return Deployment{}, fmt.Errorf("commit readiness observation: %w", err)
	}
	return deployment, nil
}

func validReadinessObservation(observed ReadinessObservation) bool {
	return containerIDPattern.MatchString(observed.ContainerID) && observed.HostPort >= 1 && observed.HostPort <= 65535 &&
		observed.Attempts > 0 && observed.Attempts <= math.MaxInt32 &&
		(observed.StatusCode == 0 || observed.StatusCode >= 100 && observed.StatusCode <= 999) &&
		(!observed.Passed || observed.StatusCode >= 200 && observed.StatusCode <= 299) &&
		!observed.CheckedAt.IsZero() && observed.CheckedAt.Year() >= 1 && observed.CheckedAt.Year() <= 9999
}
