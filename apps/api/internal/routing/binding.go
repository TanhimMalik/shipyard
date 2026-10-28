package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

var (
	ErrInvalidBinding      = errors.New("invalid route binding")
	ErrConflict            = errors.New("route does not match the expected binding")
	ErrNotOwned            = errors.New("route file is not an unchanged managed configuration")
	ErrNotFound            = errors.New("route not found")
	ErrUnsupportedPlatform = errors.New("route publication requires macOS or Linux")
	uuidPattern            = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	containerIDPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Binding identifies an already verified candidate. Publishing does not check
// Docker or HTTP readiness, or prove that Traefik has loaded the configuration.
type Binding struct {
	ProjectID    string `json:"project_id"`
	DeploymentID string `json:"deployment_id"`
	InstanceID   string `json:"instance_id"`
	ContainerID  string `json:"container_id"`
	HostPort     int    `json:"host_port"`
}

func validBinding(binding Binding) bool {
	return uuidPattern.MatchString(binding.ProjectID) && uuidPattern.MatchString(binding.DeploymentID) &&
		uuidPattern.MatchString(binding.InstanceID) && containerIDPattern.MatchString(binding.ContainerID) &&
		binding.HostPort >= 1 && binding.HostPort <= 65535
}

func Hostname(projectID string) (string, error) {
	if !uuidPattern.MatchString(projectID) {
		return "", ErrInvalidBinding
	}
	return projectID + ".shipyard.localhost", nil
}

const headerPrefix = "# shipyard-route-v1 "

func render(binding Binding) []byte {
	metadata, _ := json.Marshal(binding)
	hostname, _ := Hostname(binding.ProjectID)
	name := "shipyard-" + binding.ProjectID
	return fmt.Appendf(nil, `%s%s
[http.routers.%s]
  entryPoints = ["web"]
  rule = "Host(%s)"
  service = "%s"

[http.services.%s.loadBalancer]
  passHostHeader = true
  [[http.services.%s.loadBalancer.servers]]
    url = "http://127.0.0.1:%d"
`, headerPrefix, metadata, name, "`"+hostname+"`", name, name, name, binding.HostPort)
}

func routeName(projectID string) string { return "shipyard-" + projectID + ".toml" }
