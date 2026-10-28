package deployments

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrInstanceNotFound = errors.New("runtime instance not found")
	containerIDPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Observation struct {
	ContainerID string
	Status      string
	Running     bool
	ExitCode    int
	OOMKilled   bool
	HostPort    int
}

// Nil observation fields mean Docker has not been observed yet, not that the
// planned instance is stopped. Desired state and route activation are separate.
type RuntimeInstance struct {
	ID             string     `json:"id"`
	DeploymentID   string     `json:"deployment_id"`
	ContainerID    *string    `json:"container_id"`
	ObservedStatus *string    `json:"observed_status"`
	Running        *bool      `json:"running"`
	ExitCode       *int       `json:"exit_code"`
	OOMKilled      *bool      `json:"oom_killed"`
	HostPort       *int       `json:"host_port"`
	ObservedAt     *time.Time `json:"observed_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

const instanceColumns = `id::text, deployment_id::text, container_id, observed_status,
	running, exit_code, oom_killed, host_port, observed_at, created_at, updated_at`

// RecordInstance pins the first full Docker ID and records subsequent observed
// state only for this claim's persisted instance intent.
func (s *PostgresStore) RecordInstance(ctx context.Context, claim Claim, instanceID string, observed Observation) (RuntimeInstance, error) {
	if !validObservation(observed) {
		return RuntimeInstance{}, ErrInvalidExecution
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RuntimeInstance{}, fmt.Errorf("begin instance observation: %w", err)
	}
	defer tx.Rollback(ctx)
	deployment, err := lockExecution(ctx, tx, claim, StatusStarting, StatusHealthChecking, StatusActivating)
	if err != nil {
		return RuntimeInstance{}, err
	}
	if deployment.InstanceID == nil || !strings.EqualFold(*deployment.InstanceID, instanceID) {
		return RuntimeInstance{}, ErrInvalidTransition
	}
	var port *int
	if observed.Running {
		port = &observed.HostPort
	}
	instance, err := scanInstance(tx.QueryRow(ctx, `
		UPDATE runtime_instances
		SET container_id = $3, observed_status = $4, running = $5, exit_code = $6,
		    oom_killed = $7, host_port = $8, observed_at = now(), updated_at = now()
		WHERE id = $1 AND deployment_id = $2 AND (container_id IS NULL OR container_id = $3)
		RETURNING `+instanceColumns, instanceID, claim.ID, observed.ContainerID, observed.Status,
		observed.Running, observed.ExitCode, observed.OOMKilled, port))
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeInstance{}, ErrInvalidTransition
	}
	if err != nil {
		return RuntimeInstance{}, fmt.Errorf("record instance observation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RuntimeInstance{}, fmt.Errorf("commit instance observation: %w", err)
	}
	return instance, nil
}

func (s *PostgresStore) GetInstance(ctx context.Context, id string) (RuntimeInstance, error) {
	instance, err := scanInstance(s.pool.QueryRow(ctx, `SELECT `+instanceColumns+` FROM runtime_instances WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeInstance{}, ErrInstanceNotFound
	}
	if err != nil {
		return RuntimeInstance{}, fmt.Errorf("get runtime instance: %w", err)
	}
	return instance, nil
}

func validObservation(observed Observation) bool {
	if !containerIDPattern.MatchString(observed.ContainerID) || observed.ExitCode < math.MinInt32 || observed.ExitCode > math.MaxInt32 ||
		(observed.Running && (observed.HostPort < 1 || observed.HostPort > 65535)) || (!observed.Running && observed.HostPort != 0) {
		return false
	}
	switch observed.Status {
	case "created", "running", "paused", "restarting", "removing", "exited", "dead":
		return true
	default:
		return false
	}
}

func scanInstance(row pgx.Row) (RuntimeInstance, error) {
	var instance RuntimeInstance
	err := row.Scan(&instance.ID, &instance.DeploymentID, &instance.ContainerID, &instance.ObservedStatus,
		&instance.Running, &instance.ExitCode, &instance.OOMKilled, &instance.HostPort, &instance.ObservedAt,
		&instance.CreatedAt, &instance.UpdatedAt)
	instance.CreatedAt, instance.UpdatedAt = instance.CreatedAt.UTC(), instance.UpdatedAt.UTC()
	if instance.ObservedAt != nil {
		*instance.ObservedAt = instance.ObservedAt.UTC()
	}
	return instance, err
}
