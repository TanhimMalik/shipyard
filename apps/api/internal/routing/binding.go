package routing

import (
	"crypto/sha256"
	"encoding/hex"
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

const (
	MarkerHeader       = "X-Shipyard-Route"
	headerPrefix       = "# shipyard-route-v2 "
	legacyHeaderPrefix = "# shipyard-route-v1 "
)

// Marker identifies the whole binding, including the exact container and port.
func Marker(binding Binding) (string, error) {
	if !validBinding(binding) {
		return "", ErrInvalidBinding
	}
	data, _ := json.Marshal(binding)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func render(binding Binding) []byte {
	metadata, _ := json.Marshal(binding)
	hostname, _ := Hostname(binding.ProjectID)
	marker, _ := Marker(binding)
	name := "shipyard-" + binding.ProjectID
	return fmt.Appendf(nil, `%s%s
[http.routers.%s]
  entryPoints = ["web"]
  rule = "Host(%s)"
  service = "%s"
  middlewares = ["%s-identity"]

[http.services.%s.loadBalancer]
  passHostHeader = true
  [[http.services.%s.loadBalancer.servers]]
    url = "http://127.0.0.1:%d"

[http.middlewares.%s-identity.headers.customResponseHeaders]
  %s = "%s"
`, headerPrefix, metadata, name, "`"+hostname+"`", name, name, name, name, binding.HostPort, name, MarkerHeader, marker)
}

// Keep exact v1 documents readable so a retry can upgrade them safely.
func renderLegacy(binding Binding) []byte {
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
`, legacyHeaderPrefix, metadata, name, "`"+hostname+"`", name, name, name, binding.HostPort)
}

func routeName(projectID string) string { return "shipyard-" + projectID + ".toml" }
