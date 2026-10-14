package source

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFetchRejectsUnapprovedOrInvalidInputsBeforeResolving(t *testing.T) {
	stubGit(t)
	const repository = "https://example.com/approved.git"
	fetcher, err := NewFetcher(Config{WorkspaceRoot: filepath.Join(t.TempDir(), "workspaces"), AllowedRepositories: []string{repository}})
	if err != nil {
		t.Fatal(err)
	}
	fetcher.resolve = func(context.Context, *url.URL) (string, error) {
		return "", errors.New("unexpected network resolution")
	}
	for _, test := range []struct {
		name, repository, branch string
		want                     error
	}{
		{"unlisted", "https://example.com/other.git", "main", ErrNotAllowed},
		{"different spelling", "https://EXAMPLE.com/approved.git", "main", ErrNotAllowed},
		{"file", "file:///tmp/repo.git", "main", ErrInvalidSource},
		{"http", "http://example.com/approved.git", "main", ErrInvalidSource},
		{"ssh", "ssh://example.com/approved.git", "main", ErrInvalidSource},
		{"credentials", "https://token@example.com/approved.git", "main", ErrInvalidSource},
		{"query", repository + "?secret=value", "main", ErrInvalidSource},
		{"revision", repository, "main~1", ErrInvalidSource},
		{"ref", repository, "refs/heads/main", ErrInvalidSource},
		{"option", repository, "--upload-pack=evil", ErrInvalidSource},
		{"empty", repository, "", ErrInvalidSource},
		{"cancelled", repository, "main", context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.want == context.Canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			checkout, err := fetcher.Fetch(ctx, test.repository, test.branch)
			if checkout != nil || !errors.Is(err, test.want) {
				t.Fatalf("Fetch = %v, %v; want %v", checkout, err, test.want)
			}
		})
	}
	assertEmptyWorkspace(t, fetcher.root)
}

func TestFetchDeadlineIncludesResolution(t *testing.T) {
	stubGit(t)
	const repository = "https://example.com/approved.git"
	fetcher, err := NewFetcher(Config{WorkspaceRoot: filepath.Join(t.TempDir(), "workspaces"), AllowedRepositories: []string{repository}, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	fetcher.resolve = func(ctx context.Context, _ *url.URL) (string, error) { <-ctx.Done(); return "", ctx.Err() }
	checkout, err := fetcher.Fetch(context.Background(), repository, "main")
	if checkout != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Fetch = %v, %v", checkout, err)
	}
	assertEmptyWorkspace(t, fetcher.root)
}

func TestCheckoutCloseOwnsItsOriginalWorkspace(t *testing.T) {
	if err := checkPlatform(); err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	attempt, err := os.MkdirTemp(root, "source-")
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(attempt, "link")); err != nil {
		t.Fatal(err)
	}
	checkout := &Checkout{root: attempt, Path: outside}
	if err := checkout.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkout.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("external target changed: %v", err)
	}
	assertEmptyWorkspace(t, root)
}

func TestSupportedGitVersion(t *testing.T) {
	for value, want := range map[string]bool{
		"git version 2.39.5 (Apple Git-154)\n": true,
		"git version 2.56.0\n":                 true,
		"git version 3.0.0\n":                  true,
		"git version 2.38.5\n":                 false,
		"git version 1.99.0\n":                 false,
		"unknown":                              false,
	} {
		if got := supportedGitVersion(value); got != want {
			t.Errorf("supportedGitVersion(%q) = %v", value, got)
		}
	}
}

func assertEmptyWorkspace(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("workspace not empty: %v", entries)
	}
}

func stubGit(t *testing.T) {
	t.Helper()
	if err := checkPlatform(); err != nil {
		t.Skip(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf 'git version 2.39.5\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}
