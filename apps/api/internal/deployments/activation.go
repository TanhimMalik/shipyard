package deployments

import (
	"errors"
	"regexp"
	"time"
)

var (
	ErrActivationNotFound  = errors.New("activation intent not found")
	ErrActiveRouteNotFound = errors.New("active route not found")
	activationUUIDPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type RouteBinding struct {
	ProjectID    string `json:"project_id"`
	DeploymentID string `json:"deployment_id"`
	InstanceID   string `json:"instance_id"`
	ContainerID  string `json:"container_id"`
	HostPort     int    `json:"host_port"`
}

// Activation retains both identities across a process restart. ConfirmedAt is
// null until a caller reports a verified route switch; it is not Docker state.
type Activation struct {
	Target      RouteBinding  `json:"target"`
	Previous    *RouteBinding `json:"previous"`
	CreatedAt   time.Time     `json:"created_at"`
	ConfirmedAt *time.Time    `json:"confirmed_at"`
}

func validRouteBinding(binding RouteBinding) bool {
	return activationUUIDPattern.MatchString(binding.ProjectID) && activationUUIDPattern.MatchString(binding.DeploymentID) &&
		activationUUIDPattern.MatchString(binding.InstanceID) && containerIDPattern.MatchString(binding.ContainerID) &&
		binding.HostPort >= 1 && binding.HostPort <= 65535
}

func sameRoute(left, right *RouteBinding) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
