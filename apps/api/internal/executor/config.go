package executor

import (
	"encoding/json"
	"errors"
	"path/filepath"

	"shipyard/apps/api/internal/gitinput"
)

type Config struct {
	Enabled             bool
	WorkspaceRoot       string
	AllowedRepositories []string
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
	return Config{Enabled: true, WorkspaceRoot: root, AllowedRepositories: repositories}, nil
}
