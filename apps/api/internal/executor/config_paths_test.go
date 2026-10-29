package executor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRouteDirectoryCannotContainSourceThroughAnAlias(t *testing.T) {
	root := t.TempDir()
	source, routes := filepath.Join(root, "source"), filepath.Join(root, "routes")
	for _, path := range []string{source, routes, filepath.Join(source, "nested")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if overlap, err := existingDirectoriesOverlap(source, routes); err != nil || overlap {
		t.Fatalf("separate directories: overlap=%t error=%v", overlap, err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(source, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	for _, pair := range [][2]string{{source, alias}, {source, filepath.Join(alias, "nested")}, {filepath.Join(alias, "nested"), source}} {
		if overlap, err := existingDirectoriesOverlap(pair[0], pair[1]); err != nil || !overlap {
			t.Fatalf("source alias exposed to route watcher: %v, overlap=%t error=%v", pair, overlap, err)
		}
	}
}
