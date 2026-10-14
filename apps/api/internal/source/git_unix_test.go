//go:build darwin || linux

package source

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunGitStopsHelpersAfterParentExits(t *testing.T) {
	for _, test := range []struct {
		name      string
		redirect  string
		waitDelay bool
	}{
		{"helper retains output pipes", "", true},
		{"helper closes output pipes", ">/dev/null 2>&1", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			home, empty := filepath.Join(root, "home"), filepath.Join(root, "empty")
			for _, path := range []string{home, empty} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			fakeGit := filepath.Join(root, "git")
			// The helper writes only to a file: closing its inherited stdout must
			// not accidentally stop it through SIGPIPE and make this test pass.
			script := `#!/bin/sh
printf '%s\n' "$$" > process-group
(
  printf '.' > heartbeat
  while :; do
    sleep 0.02
    printf '.' >> heartbeat
  done
) ` + test.redirect + ` &
while [ ! -s heartbeat ]; do sleep 0.01; done
exit 0
`
			if err := os.WriteFile(fakeGit, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			// Also clean up when testing a broken runner. PID is used only to
			// terminate our process group, never as evidence that a helper lives.
			t.Cleanup(func() {
				data, err := os.ReadFile(filepath.Join(root, "process-group"))
				if err != nil {
					return
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
				if err == nil && pid > 0 {
					_ = syscall.Kill(-pid, syscall.SIGKILL)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			fetcher := &Fetcher{gitPath: fakeGit}
			_, err := fetcher.runGit(ctx, root, home, empty, "", "test")
			if test.waitDelay {
				if !errors.Is(err, exec.ErrWaitDelay) {
					t.Fatalf("runGit error = %v, want ErrWaitDelay", err)
				}
			} else if err != nil {
				t.Fatalf("runGit error = %v, want successful parent exit", err)
			}
			// Allow delivery of the final SIGKILL before measuring activity.
			time.Sleep(50 * time.Millisecond)
			before, err := os.ReadFile(filepath.Join(root, "heartbeat"))
			if err != nil || len(before) == 0 {
				t.Fatalf("helper did not start: heartbeat = %q, error = %v", before, err)
			}
			time.Sleep(150 * time.Millisecond)
			after, err := os.ReadFile(filepath.Join(root, "heartbeat"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Errorf("helper kept writing after runGit returned: heartbeat grew from %d to %d bytes", len(before), len(after))
			}
		})
	}
}

func TestRunGitRetainsBoundedDiagnosticsWithoutExposingThemInErrorText(t *testing.T) {
	root := t.TempDir()
	home, empty := filepath.Join(root, "home"), filepath.Join(root, "empty")
	for _, path := range []string{home, empty} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	git := filepath.Join(root, "git")
	script := `#!/bin/sh
printf 'private-git-detail:' >&2
i=0
while [ "$i" -lt 5000 ]; do printf x >&2; i=$((i+1)); done
exit 128
`
	if err := os.WriteFile(git, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	fetcher := &Fetcher{gitPath: git}
	_, err := fetcher.runGit(ctx, root, home, empty, "", "test")
	var commandError *GitCommandError
	var exitError *exec.ExitError
	if !errors.As(err, &commandError) || !errors.As(err, &exitError) || exitError.ExitCode() != 128 {
		t.Fatalf("error lost its command failure cause: %v", err)
	}
	if strings.Contains(err.Error(), "private-git-detail") {
		t.Fatal("public error text exposed Git stderr")
	}
	if len(commandError.diagnostic) != 4096 || !strings.HasPrefix(commandError.diagnostic, "private-git-detail:") {
		t.Fatalf("diagnostic length/prefix = %d / %q", len(commandError.diagnostic), commandError.diagnostic[:min(len(commandError.diagnostic), 19)])
	}
	if got := commandError.LogValue().Group()[1].Value.String(); got != commandError.diagnostic {
		t.Fatal("private logging lost the bounded diagnostic")
	}
}
