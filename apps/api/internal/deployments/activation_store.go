package deployments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// BeginActivation saves the immutable candidate and previous route before any
// proxy mutation. The caller must freshly verify the candidate before publishing.
func (s *PostgresStore) BeginActivation(ctx context.Context, claim Claim, containerID string, hostPort int) (Activation, error) {
	if !containerIDPattern.MatchString(containerID) || hostPort < 1 || hostPort > 65535 {
		return Activation{}, ErrInvalidExecution
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Activation{}, fmt.Errorf("begin activation intent: %w", err)
	}
	defer tx.Rollback(ctx)
	deployment, err := lockActivationProject(ctx, tx, claim, StatusHealthChecking, StatusActivating)
	if err != nil {
		return Activation{}, err
	}
	if deployment.InstanceID == nil || deployment.ReadinessPassed == nil || !*deployment.ReadinessPassed {
		return Activation{}, ErrInvalidTransition
	}
	target := RouteBinding{ProjectID: deployment.ProjectID, DeploymentID: deployment.ID,
		InstanceID: *deployment.InstanceID, ContainerID: containerID, HostPort: hostPort}
	previous, err := previousRoute(ctx, tx, deployment.ProjectID)
	if err != nil {
		return Activation{}, err
	}
	if deployment.Status == StatusActivating {
		activation, err := scanActivation(tx.QueryRow(ctx, activationQuery, deployment.ID))
		if err != nil {
			return Activation{}, err
		}
		if activation.Target != target || !sameRoute(activation.Previous, previous) || activation.ConfirmedAt != nil {
			return Activation{}, ErrInvalidTransition
		}
		return activation, nil
	}
	if err := runningActivationTarget(ctx, tx, target); err != nil {
		return Activation{}, err
	}
	var previousID *string
	if previous != nil {
		previousID = &previous.DeploymentID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO activation_intents
		(deployment_id, project_id, instance_id, container_id, host_port, previous_deployment_id)
		VALUES ($1, $2, $3, $4, $5, $6)`, target.DeploymentID, target.ProjectID, target.InstanceID, target.ContainerID, target.HostPort, previousID); err != nil {
		return Activation{}, fmt.Errorf("save activation intent: %w", err)
	}
	result, err := tx.Exec(ctx, `UPDATE deployments SET status = 'ACTIVATING', updated_at = now()
		WHERE id = $1 AND claim_token = $2 AND status = 'HEALTH_CHECKING'`, claim.ID, claim.Token)
	if err != nil {
		return Activation{}, fmt.Errorf("begin activation stage: %w", err)
	}
	if result.RowsAffected() != 1 {
		return Activation{}, ErrInvalidTransition
	}
	activation, err := scanActivation(tx.QueryRow(ctx, activationQuery, deployment.ID))
	if err != nil {
		return Activation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Activation{}, fmt.Errorf("commit activation intent: %w", err)
	}
	return activation, nil
}

// ConfirmActivation is called only after verifying that Traefik serves this
// exact target. It records that observation; it does not contact the proxy.
// A lost commit response is resolved with GetActivation and GetActiveRoute,
// since successful confirmation releases the claim and cannot be replayed.
func (s *PostgresStore) ConfirmActivation(ctx context.Context, claim Claim, target RouteBinding) (Deployment, error) {
	if !validRouteBinding(target) {
		return Deployment{}, ErrInvalidExecution
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Deployment{}, fmt.Errorf("begin activation confirmation: %w", err)
	}
	defer tx.Rollback(ctx)
	deployment, activation, err := lockedActivation(ctx, tx, claim)
	if err != nil {
		return Deployment{}, err
	}
	if activation.Target != target {
		return Deployment{}, ErrInvalidTransition
	}
	if err := runningActivationTarget(ctx, tx, target); err != nil {
		return Deployment{}, err
	}
	var confirmedAt time.Time
	if err := tx.QueryRow(ctx, `UPDATE activation_intents SET confirmed_at = now()
		WHERE deployment_id = $1 AND confirmed_at IS NULL RETURNING confirmed_at`, deployment.ID).Scan(&confirmedAt); err != nil {
		return Deployment{}, fmt.Errorf("confirm activation intent: %w", err)
	}
	if previous := activation.Previous; previous != nil {
		result, err := tx.Exec(ctx, `UPDATE deployments SET status = 'SUPERSEDED', updated_at = now()
			WHERE id = $1 AND project_id = $2 AND status = 'RUNNING'`, previous.DeploymentID, target.ProjectID)
		if err != nil {
			return Deployment{}, fmt.Errorf("supersede previous deployment: %w", err)
		}
		if result.RowsAffected() != 1 {
			return Deployment{}, ErrInvalidTransition
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO active_routes (project_id, deployment_id, confirmed_at) VALUES ($1, $2, $3)
		ON CONFLICT (project_id) DO UPDATE SET deployment_id = EXCLUDED.deployment_id, confirmed_at = EXCLUDED.confirmed_at`,
		target.ProjectID, target.DeploymentID, confirmedAt); err != nil {
		return Deployment{}, fmt.Errorf("save active route: %w", err)
	}
	deployment, err = scanDeployment(tx.QueryRow(ctx, `UPDATE deployments
		SET status = 'RUNNING', finished_at = now(), updated_at = now(), claim_token = NULL
		WHERE id = $1 AND claim_token = $2 AND status = 'ACTIVATING' RETURNING `+deploymentColumns, claim.ID, claim.Token))
	if err != nil {
		return executionResult(deployment, err, "complete activation")
	}
	if err := tx.Commit(ctx); err != nil {
		return Deployment{}, fmt.Errorf("commit activation confirmation: %w", err)
	}
	return deployment, nil
}

// AbortActivation requires the caller to have verified restoration of Previous
// (or route absence on a first deploy). Generic FailExecution cannot skip this
// boundary. This method changes metadata only; it does not clean Docker resources.
func (s *PostgresStore) AbortActivation(ctx context.Context, claim Claim, restored *RouteBinding, reason string) (Deployment, error) {
	if !validFailureReason(reason) || restored != nil && !validRouteBinding(*restored) {
		return Deployment{}, ErrInvalidExecution
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Deployment{}, fmt.Errorf("begin activation abort: %w", err)
	}
	defer tx.Rollback(ctx)
	_, activation, err := lockedActivation(ctx, tx, claim)
	if err != nil {
		return Deployment{}, err
	}
	if !sameRoute(activation.Previous, restored) {
		return Deployment{}, ErrInvalidTransition
	}
	deployment, err := scanDeployment(tx.QueryRow(ctx, `UPDATE deployments
		SET status = 'FAILED', failure_stage = 'ACTIVATING', failure_reason = $3,
		    finished_at = now(), updated_at = now(), claim_token = NULL
		WHERE id = $1 AND claim_token = $2 AND status = 'ACTIVATING' RETURNING `+deploymentColumns, claim.ID, claim.Token, reason))
	if err != nil {
		return executionResult(deployment, err, "abort activation")
	}
	if err := tx.Commit(ctx); err != nil {
		return Deployment{}, fmt.Errorf("commit activation abort: %w", err)
	}
	return deployment, nil
}

func lockedActivation(ctx context.Context, tx pgx.Tx, claim Claim) (Deployment, Activation, error) {
	deployment, err := lockActivationProject(ctx, tx, claim, StatusActivating)
	if err != nil {
		return Deployment{}, Activation{}, err
	}
	activation, err := scanActivation(tx.QueryRow(ctx, activationQuery, deployment.ID))
	if err != nil {
		return Deployment{}, Activation{}, err
	}
	previous, err := previousRoute(ctx, tx, deployment.ProjectID)
	if err != nil {
		return Deployment{}, Activation{}, err
	}
	if activation.ConfirmedAt != nil || !sameRoute(activation.Previous, previous) {
		return Deployment{}, Activation{}, ErrInvalidTransition
	}
	if deployment.InstanceID == nil || activation.Target.InstanceID != *deployment.InstanceID {
		return Deployment{}, Activation{}, errors.New("activation candidate no longer matches the deployment")
	}
	return deployment, activation, nil
}
