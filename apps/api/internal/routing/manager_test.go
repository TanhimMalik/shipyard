//go:build linux || darwin

package routing

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestManagerRejectsUnsafeDirectories(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(root, "public")
	if err := os.Mkdir(public, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(public, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"", "relative", filepath.Join(root, "missing"), file, public, link} {
		if m, err := NewManager(Config{Directory: directory}); err == nil {
			_ = m.Close()
			t.Errorf("accepted unsafe directory %q", directory)
		}
	}
}

func TestManagerPublishesOwnedRoutesAndReusesExactRetries(t *testing.T) {
	m, directory := routingTestManager(t)
	binding := routingTestBinding()
	if _, err := m.Get(t.Context(), binding.ProjectID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing route: %v", err)
	}
	if err := m.Put(t.Context(), binding, nil); err != nil {
		t.Fatal(err)
	}
	got, err := m.Get(t.Context(), binding.ProjectID)
	if err != nil || got != binding {
		t.Fatalf("saved route: %+v, %v", got, err)
	}
	path := routingTestPath(directory, binding.ProjectID)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe route file: %v, %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("# shipyard-route-v2 ")) || !bytes.Contains(data, []byte("http://127.0.0.1:32123")) ||
		!bytes.Contains(data, []byte(binding.ProjectID+".shipyard.localhost")) {
		t.Fatalf("route lacks ownership, fixed loopback target, or stable host: %s", data)
	}
	if err := m.Put(t.Context(), binding, nil); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	again, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, again) || !again.ModTime().Equal(info.ModTime()) {
		t.Fatal("identical retry rewrote the route")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(path) && entry.Name() != ".shipyard.lock" {
			t.Errorf("publication left a temporary artifact: %s", entry.Name())
		}
	}
}

func TestManagerReplacementAndRemovalRequireTheExpectedBinding(t *testing.T) {
	m, directory := routingTestManager(t)
	old := routingTestBinding()
	next := routingTestReplacement(old)
	if err := m.Put(t.Context(), next, &old); !errors.Is(err, ErrConflict) {
		t.Fatalf("replaced an absent expected route: %v", err)
	}
	if err := m.Put(t.Context(), old, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(t.Context(), next, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrote an existing route without an expectation: %v", err)
	}
	if err := m.Put(t.Context(), next, &old); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(t.Context(), next, &old); err != nil {
		t.Fatalf("lost-response replacement retry: %v", err)
	}
	if err := m.Put(t.Context(), old, &old); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale writer replaced the current route: %v", err)
	}
	if err := m.Remove(t.Context(), old); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale cleanup removed a newer route: %v", err)
	}
	if got, err := m.Get(t.Context(), old.ProjectID); err != nil || got != next {
		t.Fatalf("new route changed after stale operation: %+v, %v", got, err)
	}
	other := old
	other.ProjectID = "a7b310d2-9c6e-4ef8-a419-0c1d2e3f4a56"
	if err := m.Put(t.Context(), other, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(t.Context(), next); err != nil {
		t.Fatalf("repeated removal: %v", err)
	}
	if _, err := os.Lstat(routingTestPath(directory, next.ProjectID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned file still exists: %v", err)
	}
	if got, err := m.Get(t.Context(), other.ProjectID); err != nil || got != other {
		t.Fatalf("cleanup changed another project: %+v, %v", got, err)
	}
}

func TestManagerRefusesForeignTamperedAndLinkedRouteFiles(t *testing.T) {
	for _, kind := range []string{"foreign", "tampered", "symlink", "directory", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			m, directory := routingTestManager(t)
			binding := routingTestBinding()
			path := routingTestPath(directory, binding.ProjectID)
			var before []byte
			outside := filepath.Join(t.TempDir(), "keep")
			if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "tampered":
				if err := m.Put(t.Context(), binding, nil); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before = append(data, []byte("\n# manual edit\n")...)
			case "foreign":
				before = []byte("# unrelated operator configuration\n")
			case "oversized":
				before = []byte(strings.Repeat("x", 16*1024+1))
			case "symlink":
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if before != nil {
				if err := os.WriteFile(path, before, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.Get(t.Context(), binding.ProjectID); !errors.Is(err, ErrNotOwned) {
				t.Fatalf("read accepted %s file: %v", kind, err)
			}
			next := routingTestReplacement(binding)
			if err := m.Put(t.Context(), next, &binding); !errors.Is(err, ErrNotOwned) {
				t.Fatalf("publication accepted %s file: %v", kind, err)
			}
			if err := m.Remove(t.Context(), binding); !errors.Is(err, ErrNotOwned) {
				t.Fatalf("removal accepted %s file: %v", kind, err)
			}
			if before != nil {
				if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
					t.Fatalf("foreign content changed: %v", err)
				}
			} else if info, err := os.Lstat(path); err != nil || kind == "symlink" && info.Mode()&os.ModeSymlink == 0 || kind == "directory" && !info.IsDir() {
				t.Fatalf("foreign file was replaced or removed: %v", err)
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "outside" {
				t.Fatalf("external file changed: %q, %v", data, err)
			}
		})
	}
}

func TestConcurrentManagersCannotBothReplaceTheSameRoute(t *testing.T) {
	first, directory := routingTestManager(t)
	second, err := NewManager(Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	old := routingTestBinding()
	if err := first.Put(t.Context(), old, nil); err != nil {
		t.Fatal(err)
	}
	a, b := routingTestReplacement(old), routingTestReplacement(old)
	b.ContainerID, b.HostPort = strings.Repeat("e", 64), 32125
	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, candidate := range []struct {
		manager *Manager
		binding Binding
	}{{first, a}, {second, b}} {
		go func() {
			ready.Done()
			<-start
			errorsCh <- candidate.manager.Put(t.Context(), candidate.binding, &old)
		}()
	}
	ready.Wait()
	close(start)
	winners, conflicts := 0, 0
	for range 2 {
		err := <-errorsCh
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("replacement results: success=%d conflict=%d", winners, conflicts)
	}
	if got, err := first.Get(t.Context(), old.ProjectID); err != nil || got != a && got != b {
		t.Fatalf("concurrent publication produced another route: %+v, %v", got, err)
	}
}

func TestManagerCancellationDoesNotPublishOrRemove(t *testing.T) {
	m, directory := routingTestManager(t)
	binding := routingTestBinding()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.Put(ctx, binding, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled publication: %v", err)
	}
	if _, err := os.Lstat(routingTestPath(directory, binding.ProjectID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled publication changed disk: %v", err)
	}
	if err := m.Put(t.Context(), binding, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, binding); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled removal: %v", err)
	}
	if _, err := m.Get(ctx, binding.ProjectID); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled read: %v", err)
	}
	if got, err := m.Get(t.Context(), binding.ProjectID); err != nil || got != binding {
		t.Fatalf("cancellation changed route: %+v, %v", got, err)
	}
}

func TestManagerLockWaitHonorsTheCallerDeadline(t *testing.T) {
	m, directory := routingTestManager(t)
	lock, err := os.OpenFile(filepath.Join(directory, ".shipyard.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := m.Put(ctx, routingTestBinding(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked writer ignored deadline: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("lock wait did not cancel promptly")
	}
	if _, err := os.Lstat(routingTestPath(directory, routingTestBinding().ProjectID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked writer changed route: %v", err)
	}
}

func routingTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, directory
}

func routingTestBinding() Binding {
	return Binding{ProjectID: "3b8e17ad-4eb9-4c27-86a6-8cc5f932ad41", DeploymentID: "695f0db2-74a2-4d19-8d3f-362a5397018c",
		InstanceID: "c5d34a49-8586-419e-8ea1-fc0c8a346d72", ContainerID: strings.Repeat("c", 64), HostPort: 32123}
}

func routingTestReplacement(old Binding) Binding {
	old.DeploymentID = "41c7926b-4268-4c82-b591-2a4fab916c01"
	old.InstanceID = "f746260f-b160-40e5-b4ad-5e7f2d1a03f9"
	old.ContainerID, old.HostPort = strings.Repeat("d", 64), 32124
	return old
}

func routingTestPath(directory, projectID string) string {
	return filepath.Join(directory, "shipyard-"+projectID+".toml")
}
