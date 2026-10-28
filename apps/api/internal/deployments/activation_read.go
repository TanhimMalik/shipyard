package deployments

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const activationQuery = `SELECT a.project_id::text, a.deployment_id::text, a.instance_id::text,
	a.container_id, a.host_port, a.created_at, a.confirmed_at,
	p.project_id::text, p.deployment_id::text, p.instance_id::text, p.container_id, p.host_port
	FROM activation_intents a LEFT JOIN activation_intents p ON p.deployment_id = a.previous_deployment_id
	WHERE a.deployment_id = $1`

const activeRouteQuery = `SELECT a.project_id::text, a.deployment_id::text, a.instance_id::text, a.container_id, a.host_port
	FROM active_routes r JOIN activation_intents a
	  ON a.deployment_id = r.deployment_id AND a.project_id = r.project_id AND a.confirmed_at = r.confirmed_at
	WHERE r.project_id = $1`

func (s *PostgresStore) GetActivation(ctx context.Context, deploymentID string) (Activation, error) {
	return scanActivation(s.pool.QueryRow(ctx, activationQuery, deploymentID))
}

func (s *PostgresStore) GetActiveRoute(ctx context.Context, projectID string) (RouteBinding, error) {
	return scanActiveRoute(s.pool.QueryRow(ctx, activeRouteQuery, projectID))
}

func scanActivation(row pgx.Row) (Activation, error) {
	var activation Activation
	var projectID, deploymentID, instanceID, containerID *string
	var port *int
	err := row.Scan(&activation.Target.ProjectID, &activation.Target.DeploymentID, &activation.Target.InstanceID,
		&activation.Target.ContainerID, &activation.Target.HostPort, &activation.CreatedAt, &activation.ConfirmedAt,
		&projectID, &deploymentID, &instanceID, &containerID, &port)
	if errors.Is(err, pgx.ErrNoRows) {
		return Activation{}, ErrActivationNotFound
	}
	if err != nil {
		return Activation{}, fmt.Errorf("read activation intent: %w", err)
	}
	if deploymentID != nil {
		if projectID == nil || instanceID == nil || containerID == nil || port == nil {
			return Activation{}, errors.New("previous activation binding is incomplete")
		}
		activation.Previous = &RouteBinding{ProjectID: *projectID, DeploymentID: *deploymentID, InstanceID: *instanceID, ContainerID: *containerID, HostPort: *port}
	}
	activation.CreatedAt = activation.CreatedAt.UTC()
	if activation.ConfirmedAt != nil {
		*activation.ConfirmedAt = activation.ConfirmedAt.UTC()
	}
	return activation, nil
}

func scanActiveRoute(row pgx.Row) (RouteBinding, error) {
	var binding RouteBinding
	err := row.Scan(&binding.ProjectID, &binding.DeploymentID, &binding.InstanceID, &binding.ContainerID, &binding.HostPort)
	if errors.Is(err, pgx.ErrNoRows) {
		return RouteBinding{}, ErrActiveRouteNotFound
	}
	if err != nil {
		return RouteBinding{}, fmt.Errorf("read active route: %w", err)
	}
	return binding, nil
}

func previousRoute(ctx context.Context, tx pgx.Tx, projectID string) (*RouteBinding, error) {
	binding, err := scanActiveRoute(tx.QueryRow(ctx, activeRouteQuery, projectID))
	if errors.Is(err, ErrActiveRouteNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &binding, nil
}

// Activation mutations lock the project before any deployment, including the
// previous active attempt, so concurrent confirmation follows one lock order.
func lockActivationProject(ctx context.Context, tx pgx.Tx, claim Claim, stages ...string) (Deployment, error) {
	var projectID string
	err := tx.QueryRow(ctx, `SELECT id::text FROM projects WHERE id = $1 FOR UPDATE`, claim.ProjectID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrInvalidTransition
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("lock activation project: %w", err)
	}
	deployment, err := lockExecution(ctx, tx, claim, stages...)
	if err != nil {
		return Deployment{}, err
	}
	if !strings.EqualFold(deployment.ProjectID, projectID) {
		return Deployment{}, ErrInvalidTransition
	}
	return deployment, nil
}

func runningActivationTarget(ctx context.Context, tx pgx.Tx, target RouteBinding) error {
	var matches bool
	err := tx.QueryRow(ctx, `SELECT container_id = $3 AND observed_status = 'running' AND running IS TRUE AND host_port = $4
		FROM runtime_instances WHERE id = $1 AND deployment_id = $2`, target.InstanceID, target.DeploymentID, target.ContainerID, target.HostPort).Scan(&matches)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidTransition
	}
	if err != nil {
		return fmt.Errorf("read activation candidate: %w", err)
	}
	if !matches {
		return ErrInvalidTransition
	}
	return nil
}
