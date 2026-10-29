package executor

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"shipyard/apps/api/internal/gitinput"
	"shipyard/apps/api/internal/routing"
)

type Config struct {
	Enabled             bool
	WorkspaceRoot       string
	AllowedRepositories []string
	DockerHost          string
	RoutesDirectory     string
	TraefikURL          string
}

func LoadConfig(lookup func(string) (string, bool)) (Config, error) {
	value, exists := lookup("SHIPYARD_EXECUTOR_ENABLED")
	if !exists || value == "false" {
		return Config{}, nil
	}
	if value != "true" {
		return Config{}, errors.New("SHIPYARD_EXECUTOR_ENABLED must be true or false")
	}
	root, _ := lookup("SHIPYARD_WORKSPACE_ROOT")
	if !filepath.IsAbs(root) {
		return Config{}, errors.New("SHIPYARD_WORKSPACE_ROOT must be an absolute private directory")
	}
	var repositories []string
	approved, _ := lookup("SHIPYARD_ALLOWED_REPOSITORIES")
	if err := json.Unmarshal([]byte(approved), &repositories); err != nil || len(repositories) == 0 {
		return Config{}, errors.New("SHIPYARD_ALLOWED_REPOSITORIES must be a nonempty JSON array of approved repository URLs")
	}
	for _, repository := range repositories {
		if !gitinput.ValidURL(repository) {
			return Config{}, errors.New("SHIPYARD_ALLOWED_REPOSITORIES contains an invalid repository URL")
		}
	}
	host, _ := lookup("SHIPYARD_DOCKER_HOST")
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "unix" || u.Host != "" || u.User != nil || !filepath.IsAbs(u.Path) || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return Config{}, errors.New("SHIPYARD_DOCKER_HOST must be an explicit absolute Unix socket URL")
	}
	routes, _ := lookup("SHIPYARD_ROUTES_DIR")
	if !filepath.IsAbs(routes) || overlappingDirectories(root, routes) {
		return Config{}, errors.New("SHIPYARD_ROUTES_DIR must be an absolute private directory separate from the source workspace")
	}
	proxy, _ := lookup("SHIPYARD_TRAEFIK_URL")
	if err := routing.ValidateProxyURL(proxy); err != nil {
		return Config{}, errors.New("SHIPYARD_TRAEFIK_URL must be an explicit loopback HTTP origin with a port")
	}
	return Config{Enabled: true, WorkspaceRoot: root, AllowedRepositories: repositories, DockerHost: host, RoutesDirectory: routes, TraefikURL: proxy}, nil
}

func overlappingDirectories(left, right string) bool {
	contains := func(parent, child string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	return contains(left, right) || contains(right, left)
}

// Compare actual directory identities as well as spelling, including symlink
// ancestors and case aliases on filesystems that treat names case-insensitively.
func existingDirectoriesOverlap(left, right string) (bool, error) {
	contains := func(parent, child string) (bool, error) {
		info, err := os.Stat(parent)
		if err != nil {
			return false, err
		}
		for {
			current, err := os.Stat(child)
			if err != nil {
				return false, err
			}
			if os.SameFile(info, current) {
				return true, nil
			}
			next := filepath.Dir(child)
			if next == child {
				return false, nil
			}
			child = next
		}
	}
	if overlap, err := contains(left, right); overlap || err != nil {
		return overlap, err
	}
	return contains(right, left)
}
