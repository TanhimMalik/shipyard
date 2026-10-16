package executor

import (
	"path/filepath"
	"testing"
)

func TestExecutorIsDisabledUnlessExplicitlyConfigured(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	if config, err := LoadConfig(lookup); err != nil || config.Enabled {
		t.Fatalf("default config: %+v, %v", config, err)
	}
	valid := map[string]string{"SHIPYARD_EXECUTOR_ENABLED": "true", "SHIPYARD_WORKSPACE_ROOT": filepath.Join(t.TempDir(), "source"), "SHIPYARD_ALLOWED_REPOSITORIES": `["https://example.com/repo.git"]`}
	read := func(key string) (string, bool) { value, exists := valid[key]; return value, exists }
	if config, err := LoadConfig(read); err != nil || !config.Enabled || len(config.AllowedRepositories) != 1 {
		t.Fatalf("valid config: %+v, %v", config, err)
	}
	for key, values := range map[string][]string{"SHIPYARD_EXECUTOR_ENABLED": {"", "yes", "TRUE"}, "SHIPYARD_WORKSPACE_ROOT": {"", "relative"}, "SHIPYARD_ALLOWED_REPOSITORIES": {"", "null", "[]", `["ssh://example.com/repo.git"]`, `["https://example.com/repo.git",null]`}} {
		original := valid[key]
		for _, value := range values {
			valid[key] = value
			if _, err := LoadConfig(read); err == nil {
				t.Errorf("accepted invalid %s", key)
			}
		}
		valid[key] = original
	}
}
