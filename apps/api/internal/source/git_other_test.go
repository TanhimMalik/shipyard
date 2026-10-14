//go:build !darwin && !linux

package source

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNewFetcherRejectsUnsupportedPlatformWithoutCreatingWorkspace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces")
	_, err := NewFetcher(Config{WorkspaceRoot: root, AllowedRepositories: []string{"https://example.com/app.git"}})
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("NewFetcher = %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace touched on unsupported platform: %v", err)
	}
}
