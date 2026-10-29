//go:build linux || darwin

package routing

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestActivationGuardAllowsNestedRouteOperations(t *testing.T) {
	m, _ := routingTestManager(t)
	binding := routingTestBinding()
	want := errors.New("operation failed")
	err := m.WithActivation(t.Context(), func(ctx context.Context) error {
		if err := m.Put(ctx, binding, nil); err != nil {
			t.Fatal(err)
		}
		if got, err := m.Get(ctx, binding.ProjectID); err != nil || got != binding {
			t.Fatalf("nested route read: %+v, %v", got, err)
		}
		if err := m.Remove(ctx, binding); err != nil {
			t.Fatal(err)
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("lost callback error: %v", err)
	}
	if err := m.WithActivation(t.Context(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("error left ownership held: %v", err)
	}
	func() {
		defer func() {
			if got := recover(); got != "test panic" {
				t.Fatalf("unexpected callback panic: %v", got)
			}
		}()
		_ = m.WithActivation(t.Context(), func(context.Context) error { panic("test panic") })
	}()
	if err := m.WithActivation(t.Context(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("panic left ownership held: %v", err)
	}
}

func TestActivationGuardSerializesManagersAndCancelsWaiters(t *testing.T) {
	first, directory := routingTestManager(t)
	second, err := NewManager(Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	binding := routingTestBinding()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go func() {
		done <- first.WithActivation(t.Context(), func(ctx context.Context) error {
			if err := first.Put(ctx, binding, nil); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first manager did not acquire activation ownership")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	err = second.WithActivation(ctx, func(context.Context) error {
		t.Error("second manager entered an owned activation")
		return nil
	})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wait did not honor deadline: %v", err)
	}
	if got, err := second.Get(t.Context(), binding.ProjectID); err != nil || got != binding {
		t.Errorf("canceled waiter changed the route: %+v, %v", got, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := second.WithActivation(t.Context(), func(ctx context.Context) error {
		next := routingTestReplacement(binding)
		return second.Put(ctx, next, &binding)
	}); err != nil {
		t.Fatalf("next owner could not proceed: %v", err)
	}
}

func TestActivationGuardCloseWaitsWithoutBlockingNestedWrites(t *testing.T) {
	m, _ := routingTestManager(t)
	entered, release, operationDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go func() {
		operationDone <- m.WithActivation(t.Context(), func(ctx context.Context) error {
			close(entered)
			<-release
			return m.Put(ctx, routingTestBinding(), nil)
		})
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("closed during activation: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-operationDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close prevented the callback's nested write")
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := m.WithActivation(t.Context(), func(context.Context) error {
		t.Error("entered a closed manager")
		return nil
	}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed guard: %v", err)
	}
}

func TestActivationGuardRejectsCanceledAndUnsafeOwnership(t *testing.T) {
	for _, kind := range []string{"canceled", "public lock", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			m, directory := routingTestManager(t)
			name := filepath.Join(directory, ".shipyard-activation.lock")
			ctx := t.Context()
			if kind == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else if kind == "public lock" {
				if err := os.WriteFile(name, []byte("keep"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(name, 0644); err != nil {
					t.Fatal(err)
				}
			} else if kind == "directory" {
				if err := os.Mkdir(name, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				outside := filepath.Join(t.TempDir(), "sentinel")
				if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, name); err != nil {
					t.Fatal(err)
				}
			}
			err := m.WithActivation(ctx, func(context.Context) error {
				t.Error("callback entered without ownership")
				return nil
			})
			if kind == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled guard: %v", err)
				}
				if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("pre-cancel created a lock: %v", err)
				}
			} else if !errors.Is(err, ErrNotOwned) {
				t.Fatalf("unsafe guard accepted: %v", err)
			}
		})
	}
}

func TestActivationGuardIsReleasedAfterProcessCrash(t *testing.T) {
	m, directory := routingTestManager(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "-test.run=^TestActivationGuardChildProcessHelper$")
	command.Env = append(os.Environ(), "SHIPYARD_GUARD_TEST_HELPER=1", "SHIPYARD_GUARD_TEST_DIRECTORY="+directory)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	t.Cleanup(func() { _ = command.Process.Kill(); <-done })
	entered := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); entered <- line }()
	select {
	case line := <-entered:
		if line != "locked\n" {
			t.Fatalf("child did not acquire ownership: %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child did not announce activation ownership")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	err = m.WithActivation(ctx, func(context.Context) error {
		t.Error("entered while another process held activation ownership")
		return nil
	})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cross-process ownership was not held: %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("killed child did not exit")
	}
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := m.WithActivation(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("crash did not release activation ownership: %v", err)
	}
}

func TestActivationGuardChildProcessHelper(t *testing.T) {
	if os.Getenv("SHIPYARD_GUARD_TEST_HELPER") != "1" {
		return
	}
	m, err := NewManager(Config{Directory: os.Getenv("SHIPYARD_GUARD_TEST_DIRECTORY")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.WithActivation(t.Context(), func(ctx context.Context) error {
		fmt.Println("locked")
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("guard helper was not terminated by its parent")
		}
	}); err != nil {
		t.Fatal(err)
	}
}
