//go:build integration

package source

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const sourceFixtureHost = "git.shipyard.test"

func TestFetchChecksOutExactBranchAndDetachedCommit(t *testing.T) {
	fixture := newSourceGitFixture(t, nil)
	fetcher, root := fixture.fetcher(t, 10*time.Second)
	checkout, err := fetcher.Fetch(t.Context(), fixture.URL, "release")
	if err != nil {
		t.Fatalf("fetch exact release branch: %v", err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	if checkout.CommitSHA != fixture.ReleaseSHA || checkout.CommitSHA == fixture.MainSHA {
		t.Errorf("commit = %q, want branch commit %q rather than same-named tag %q", checkout.CommitSHA, fixture.ReleaseSHA, fixture.MainSHA)
	}
	assertSourceFile(t, filepath.Join(checkout.Path, "marker.txt"), "release branch\n")
	if got := fixture.git(t, checkout.Path, "rev-parse", "--verify", "HEAD"); got != checkout.CommitSHA {
		t.Errorf("HEAD = %q, returned commit = %q", got, checkout.CommitSHA)
	}
	command := exec.CommandContext(t.Context(), fixture.Git, "-C", checkout.Path, "symbolic-ref", "-q", "HEAD")
	command.Env = fixture.Env
	if err := command.Run(); err == nil {
		t.Error("checkout HEAD must be detached")
	} else {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
			t.Errorf("inspect detached HEAD: %v", err)
		}
	}
	relative, err := filepath.Rel(root, checkout.Path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("checkout path %q is not a child of workspace root %q", checkout.Path, root)
	}
	if err := checkout.Close(); err != nil {
		t.Fatalf("close successful checkout: %v", err)
	}
	if _, err := os.Lstat(checkout.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkout remains after Close: %v", err)
	}
	assertSourceFile(t, filepath.Join(root, "unrelated.txt"), "keep me\n")
}

func TestFetchDoesNotFallBackFromInjectedAddressPin(t *testing.T) {
	var fixtureHits atomic.Int64
	fixture := newSourceGitFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fixtureHits.Add(1)
			next.ServeHTTP(w, r)
		})
	})
	fetcher, root := fixture.fetcher(t, 5*time.Second)
	fetcher.resolve = func(ctx context.Context, u *url.URL) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if u.String() != fixture.URL {
			return "", errors.New("test resolver received an unexpected repository URL")
		}
		// The server listens only on 127.0.0.1. A different loopback address
		// fails locally, with no external DNS lookup or nonlocal connection.
		return net.JoinHostPort(u.Hostname(), u.Port()) + ":[::1]", nil
	}
	checkout, err := fetcher.Fetch(t.Context(), fixture.URL, "release")
	if checkout != nil || err == nil {
		if checkout != nil {
			_ = checkout.Close()
		}
		t.Fatalf("fetch with wrong address pin = %v, %v; want no checkout and an error", checkout, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wrong loopback pin timed out instead of failing locally: %v", err)
	}
	if hits := fixtureHits.Load(); hits != 0 {
		t.Errorf("correct-address fixture received %d requests after a wrong pin, want zero", hits)
	}
	assertSourceWorkspaceEmpty(t, root)
}

func TestFetchFallsBackAcrossOnlyInjectedAddressPins(t *testing.T) {
	fixture := newSourceGitFixture(t, nil)
	fetcher, root := fixture.fetcher(t, 10*time.Second)
	fetcher.resolve = func(ctx context.Context, u *url.URL) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if u.String() != fixture.URL {
			return "", errors.New("test resolver received an unexpected repository URL")
		}
		// The first address has no fixture listener. libcurl must try the
		// second supplied address while keeping the reserved TLS hostname.
		return net.JoinHostPort(u.Hostname(), u.Port()) + ":[::1],127.0.0.1", nil
	}
	checkout, err := fetcher.Fetch(t.Context(), fixture.URL, "release")
	if err != nil {
		t.Fatalf("fetch with multiple pinned addresses: %v", err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	if checkout.CommitSHA != fixture.ReleaseSHA {
		t.Errorf("multi-address pinned commit = %q, want %q", checkout.CommitSHA, fixture.ReleaseSHA)
	}
	assertSourceFile(t, filepath.Join(checkout.Path, "marker.txt"), "release branch\n")
	if err := checkout.Close(); err != nil {
		t.Fatal(err)
	}
	assertSourceWorkspaceEmpty(t, root)
}

func TestFetchAcceptsApprovedMixedCaseHTTPS(t *testing.T) {
	fixture := newSourceGitFixture(t, nil)
	fetcher, root := fixture.fetcher(t, 10*time.Second)
	repository := strings.Replace(fixture.URL, "https://", "HTTPS://", 1)
	fetcher.allowed = map[string]struct{}{repository: {}}
	checkout, err := fetcher.Fetch(t.Context(), repository, "release")
	if err != nil {
		t.Fatalf("fetch approved mixed-case HTTPS scheme: %v", err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	if checkout.CommitSHA != fixture.ReleaseSHA {
		t.Errorf("mixed-case HTTPS commit = %q, want %q", checkout.CommitSHA, fixture.ReleaseSHA)
	}
	assertSourceFile(t, filepath.Join(checkout.Path, "marker.txt"), "release branch\n")
	if err := checkout.Close(); err != nil {
		t.Fatal(err)
	}
	assertSourceWorkspaceEmpty(t, root)
}

func TestFetchDoesNotResolveTagsOrRevisionExpressions(t *testing.T) {
	fixture := newSourceGitFixture(t, nil)
	for _, branch := range []string{"tag-only", fixture.MainSHA, "main~1", "refs/heads/main", "missing-branch"} {
		t.Run(branch, func(t *testing.T) {
			fetcher, root := fixture.fetcher(t, 10*time.Second)
			checkout, err := fetcher.Fetch(t.Context(), fixture.URL, branch)
			if checkout != nil || err == nil {
				if checkout != nil {
					_ = checkout.Close()
				}
				t.Fatalf("fetch %q = %v, %v; want no checkout and an error", branch, checkout, err)
			}
			assertSourceWorkspaceEmpty(t, root)
		})
	}
}

func TestFetchFailureRemovesOnlyItsWorkspace(t *testing.T) {
	fixture := newSourceGitFixture(t, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "private-upstream-sentinel", http.StatusInternalServerError)
		})
	})
	fetcher, root := fixture.fetcher(t, 10*time.Second)
	checkout, err := fetcher.Fetch(t.Context(), fixture.URL, "main")
	if checkout != nil || err == nil {
		if checkout != nil {
			_ = checkout.Close()
		}
		t.Fatalf("failed fetch = %v, %v; want no checkout and an error", checkout, err)
	}
	if strings.Contains(err.Error(), "private-upstream-sentinel") {
		t.Error("fetch error exposed upstream response content")
	}
	assertSourceWorkspaceEmpty(t, root)
}

func TestFetchDoesNotFollowHTTPRedirect(t *testing.T) {
	var forbiddenHits atomic.Int64
	fixture := newSourceGitFixture(t, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/forbidden-target" {
				forbiddenHits.Add(1)
				http.Error(w, "redirect target must not be requested", http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, "/forbidden-target", http.StatusFound)
		})
	})
	fetcher, root := fixture.fetcher(t, 10*time.Second)
	checkout, err := fetcher.Fetch(t.Context(), fixture.URL, "main")
	if checkout != nil || err == nil {
		if checkout != nil {
			_ = checkout.Close()
		}
		t.Fatalf("redirected fetch = %v, %v; want no checkout and an error", checkout, err)
	}
	if hits := forbiddenHits.Load(); hits != 0 {
		t.Errorf("redirect target requested %d times, want zero", hits)
	}
	assertSourceWorkspaceEmpty(t, root)
}

func TestFetchDumbHTTPShallowFailureDoesNotFollowAlternates(t *testing.T) {
	var forbiddenHits atomic.Int64
	var fixture *sourceGitFixture
	fixture = newSourceGitFixture(t, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			switch r.URL.Path {
			case "/repo.git/info/refs":
				_, _ = fmt.Fprintf(w, "%s\trefs/heads/main\n", fixture.MainSHA)
			case "/repo.git/HEAD":
				_, _ = io.WriteString(w, "ref: refs/heads/main\n")
			case "/repo.git/objects/info/http-alternates":
				_, _ = io.WriteString(w, strings.TrimSuffix(fixture.URL, "/repo.git")+"/forbidden-target/objects\n")
			case "/repo.git/objects/info/packs":
				w.WriteHeader(http.StatusOK)
			default:
				if strings.HasPrefix(r.URL.Path, "/forbidden-target") {
					forbiddenHits.Add(1)
				}
				http.NotFound(w, r)
			}
		})
	})
	fetcher, root := fixture.fetcher(t, 10*time.Second)
	checkout, err := fetcher.Fetch(t.Context(), fixture.URL, "main")
	if checkout != nil || err == nil {
		if checkout != nil {
			_ = checkout.Close()
		}
		t.Fatalf("dumb HTTP shallow fetch = %v, %v; want no checkout and an error", checkout, err)
	}
	if hits := forbiddenHits.Load(); hits != 0 {
		t.Errorf("dumb HTTP alternate requested %d times, want zero", hits)
	}
	assertSourceWorkspaceEmpty(t, root)
}

func TestFetchTimeoutAndCallerCancellationCleanUp(t *testing.T) {
	for _, mode := range []string{"configured timeout", "caller cancellation"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var startOnce, releaseOnce sync.Once
			fixture := newSourceGitFixture(t, func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					startOnce.Do(func() { close(started) })
					// Stall a real HTTPS response after the client has connected. No
					// arbitrary sleep or external network dependency is needed.
					w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
					case <-release:
					}
				})
			})
			// Release blocked handlers before httptest.Server.Close, including
			// when a regression makes the fetch fail to observe cancellation.
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			timeout := 10 * time.Second
			wantError := context.Canceled
			if mode == "configured timeout" {
				timeout = time.Second
				wantError = context.DeadlineExceeded
			}
			fetcher, root := fixture.fetcher(t, timeout)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			type fetchResult struct {
				checkout *Checkout
				err      error
			}
			completed := make(chan fetchResult, 1)
			go func() {
				checkout, err := fetcher.Fetch(ctx, fixture.URL, "main")
				completed <- fetchResult{checkout, err}
			}()
			select {
			case <-started:
			case result := <-completed:
				if result.checkout != nil {
					_ = result.checkout.Close()
				}
				t.Fatalf("fetch returned before reaching HTTPS fixture: %v", result.err)
			case <-time.After(5 * time.Second):
				t.Fatal("fetch did not reach HTTPS fixture")
			}
			if mode == "caller cancellation" {
				cancel()
			}
			select {
			case result := <-completed:
				if result.checkout != nil {
					_ = result.checkout.Close()
					t.Error("interrupted fetch returned a checkout")
				}
				if !errors.Is(result.err, wantError) {
					t.Errorf("fetch error = %v, want %v", result.err, wantError)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("interrupted fetch did not return promptly")
			}
			assertSourceWorkspaceEmpty(t, root)
		})
	}
}

func TestFetchIgnoresInheritedGitConfigurationAndHooks(t *testing.T) {
	var leakedHeader atomic.Bool
	fixture := newSourceGitFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Private-Test") != "" {
				leakedHeader.Store(true)
			}
			next.ServeHTTP(w, r)
		})
	})
	poison := t.TempDir()
	marker := filepath.Join(poison, "hook-ran")
	hooks := filepath.Join(poison, "hooks")
	templates := filepath.Join(poison, "templates")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(templates, "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\nprintf 'hook-fired\\n' > " + sourceShellQuote(marker) + "\nexit 73\n"
	for _, path := range []string{filepath.Join(hooks, "post-checkout"), filepath.Join(templates, "hooks", "post-checkout")} {
		if err := os.WriteFile(path, []byte(hook), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(poison, "gitconfig")
	contents := fmt.Sprintf("[core]\n\thooksPath = %s\n[init]\n\ttemplateDir = %s\n[http]\n\textraHeader = X-Private-Test: private-sentinel\n[filter \"shipyard-test\"]\n\tsmudge = %s\n", sourceConfigQuote(hooks), sourceConfigQuote(templates), sourceConfigQuote(filepath.Join(hooks, "post-checkout")))
	if err := os.WriteFile(config, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poison, ".gitconfig"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	// Each configuration channel is intentional: ignoring HOME alone does not
	// stop system overrides, environment-injected config, or init templates.
	t.Setenv("HOME", poison)
	t.Setenv("XDG_CONFIG_HOME", poison)
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_SYSTEM", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
	t.Setenv("GIT_TEMPLATE_DIR", templates)
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", hooks)
	t.Setenv("GIT_CONFIG_KEY_1", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_1", "X-Private-Test: private-sentinel")
	t.Setenv("GIT_DIR", filepath.Join(fixture.Writer, ".git"))
	t.Setenv("GIT_WORK_TREE", fixture.Writer)
	fetcher, root := fixture.fetcher(t, 10*time.Second)
	checkout, err := fetcher.Fetch(t.Context(), fixture.URL, "release")
	if err != nil {
		t.Fatalf("fetch with hostile inherited config: %v", err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	assertSourceFile(t, filepath.Join(checkout.Path, "marker.txt"), "release branch\n")
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("inherited hook/filter ran: %v", err)
	}
	if leakedHeader.Load() {
		t.Error("inherited Git HTTP header reached the remote")
	}
	assertSourceFile(t, filepath.Join(fixture.Writer, "marker.txt"), "release branch\n")
	if err := checkout.Close(); err != nil {
		t.Fatal(err)
	}
	assertSourceWorkspaceEmpty(t, root)
}

func TestFetchDoesNotInheritDisabledTLSVerification(t *testing.T) {
	fixture := newSourceGitFixture(t, nil)
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	fetcher, root := fixture.fetcher(t, 10*time.Second)
	fetcher.gitConfig = nil // Deliberately do not trust this self-signed fixture CA.
	checkout, err := fetcher.Fetch(t.Context(), fixture.URL, "main")
	if checkout != nil || err == nil {
		if checkout != nil {
			_ = checkout.Close()
		}
		t.Fatalf("fetch without a trusted CA = %v, %v; want TLS failure", checkout, err)
	}
	assertSourceWorkspaceEmpty(t, root)
}

func TestFetchPreservesTrackedSymlinkWithoutFollowingItsTarget(t *testing.T) {
	fixture := newSourceGitFixture(t, nil)
	fetcher, root := fixture.fetcher(t, 10*time.Second)
	checkout, err := fetcher.Fetch(t.Context(), fixture.URL, "release")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	link := filepath.Join(checkout.Path, "external-link")
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("tracked link was not preserved as a symlink: %v, %v", info, err)
	}
	if target, err := os.Readlink(link); err != nil || target != fixture.OutsideFile {
		t.Errorf("tracked symlink target = %q, %v; want %q", target, err, fixture.OutsideFile)
	}
	assertSourceFile(t, fixture.OutsideFile, "outside checkout\n")
	if err := checkout.Close(); err != nil {
		t.Fatal(err)
	}
	assertSourceFile(t, fixture.OutsideFile, "outside checkout\n")
	assertSourceWorkspaceEmpty(t, root)
}

func TestFetchOperatorApprovedPublicRepository(t *testing.T) {
	repository := os.Getenv("SHIPYARD_TEST_GIT_URL")
	if repository == "" {
		t.Skip("set SHIPYARD_TEST_GIT_URL to opt in to fetching an approved public HTTPS repository")
	}
	branch := os.Getenv("SHIPYARD_TEST_GIT_BRANCH")
	if branch == "" {
		branch = "main"
	}
	fetcher, err := NewFetcher(Config{
		WorkspaceRoot:       filepath.Join(t.TempDir(), "workspaces"),
		AllowedRepositories: []string{repository}, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("configure approved public repository: %v", err)
	}
	// This smoke test deliberately uses the production resolver, public-address
	// checks, certificate trust, and Git settings without fixture overrides.
	checkout, err := fetcher.Fetch(t.Context(), repository, branch)
	if err != nil {
		t.Fatalf("fetch approved public repository: %v", err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	if want := os.Getenv("SHIPYARD_TEST_GIT_COMMIT"); want != "" && checkout.CommitSHA != want {
		t.Errorf("fetched commit = %q, want configured commit %q", checkout.CommitSHA, want)
	}
	command := exec.CommandContext(t.Context(), fetcher.gitPath, "-C", checkout.Path, "rev-parse", "--verify", "HEAD")
	command.Env = []string{"LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null"}
	output, err := command.Output()
	if err != nil || strings.TrimSpace(string(output)) != checkout.CommitSHA {
		t.Errorf("checkout HEAD = %q, %v; returned commit = %q", output, err, checkout.CommitSHA)
	}
	if err := checkout.Close(); err != nil {
		t.Fatalf("close public checkout: %v", err)
	}
	if _, err := os.Lstat(checkout.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("public checkout remains after Close: %v", err)
	}
}

type sourceGitFixture struct {
	URL         string
	CAFile      string
	Git         string
	Env         []string
	Writer      string
	MainSHA     string
	ReleaseSHA  string
	OutsideFile string
}

func newSourceGitFixture(t *testing.T, wrap func(http.Handler) http.Handler) *sourceGitFixture {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("source integration tests require Git, including git-http-backend")
	}
	root := t.TempDir()
	home := filepath.Join(root, "git-home")
	templates := filepath.Join(root, "empty-templates")
	for _, directory := range []string{home, templates} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	emptyConfig := filepath.Join(root, "empty-gitconfig")
	if err := os.WriteFile(emptyConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := &sourceGitFixture{
		Git: git, Writer: filepath.Join(root, "writer"), OutsideFile: filepath.Join(root, "outside.txt"),
		Env: []string{
			"PATH=" + filepath.Dir(git) + string(os.PathListSeparator) + "/usr/bin:/bin",
			"HOME=" + home, "XDG_CONFIG_HOME=" + home,
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=" + emptyConfig, "GIT_CONFIG_GLOBAL=" + emptyConfig,
			"GIT_CONFIG_COUNT=0", "GIT_ATTR_NOSYSTEM=1", "GIT_TEMPLATE_DIR=" + templates,
			"GIT_TERMINAL_PROMPT=0", "LC_ALL=C",
		},
	}
	if err := os.WriteFile(fixture.OutsideFile, []byte("outside checkout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.git(t, root, "init", "--initial-branch=main", "--object-format=sha1", fixture.Writer)
	if err := os.WriteFile(filepath.Join(fixture.Writer, "marker.txt"), []byte("main branch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.Writer, ".gitattributes"), []byte("*.txt filter=shipyard-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fixture.OutsideFile, filepath.Join(fixture.Writer, "external-link")); err != nil {
		t.Fatal(err)
	}
	fixture.git(t, fixture.Writer, "add", "--", ".")
	fixture.git(t, fixture.Writer, "commit", "--no-gpg-sign", "-m", "main fixture")
	fixture.MainSHA = fixture.git(t, fixture.Writer, "rev-parse", "HEAD")
	fixture.git(t, fixture.Writer, "tag", "release")
	fixture.git(t, fixture.Writer, "tag", "tag-only")
	fixture.git(t, fixture.Writer, "checkout", "-b", "release")
	if err := os.WriteFile(filepath.Join(fixture.Writer, "marker.txt"), []byte("release branch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.git(t, fixture.Writer, "add", "--", "marker.txt")
	fixture.git(t, fixture.Writer, "commit", "--no-gpg-sign", "-m", "release fixture")
	fixture.ReleaseSHA = fixture.git(t, fixture.Writer, "rev-parse", "HEAD")
	gitRoot := filepath.Join(root, "repositories")
	if err := os.Mkdir(gitRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.git(t, root, "-c", "protocol.file.allow=always", "clone", "--bare", "--no-hardlinks", "--", fixture.Writer, filepath.Join(gitRoot, "repo.git"))
	fixture.git(t, filepath.Join(gitRoot, "repo.git"), "symbolic-ref", "HEAD", "refs/heads/main")
	backend := filepath.Join(fixture.git(t, root, "--exec-path"), "git-http-backend")
	if info, err := os.Stat(backend); err != nil || info.IsDir() {
		t.Fatalf("source integration tests require git-http-backend: %v", err)
	}
	handler := http.Handler(&cgi.Handler{
		Path: backend, Root: "/", Dir: gitRoot,
		Env:    append(append([]string(nil), fixture.Env...), "GIT_PROJECT_ROOT="+gitRoot, "GIT_HTTP_EXPORT_ALL=1"),
		Stderr: io.Discard,
	})
	if wrap != nil {
		handler = wrap(handler)
	}
	certificate, caFile := newSourceFixtureCertificate(t, root)
	fixture.CAFile = caFile
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Hostname() != "127.0.0.1" {
		t.Fatalf("Git fixture must listen on IPv4 loopback, got %q", u.Hostname())
	}
	u.Host = net.JoinHostPort(sourceFixtureHost, u.Port())
	u.Path = "/repo.git"
	fixture.URL = u.String()
	return fixture
}

func (f *sourceGitFixture) fetcher(t *testing.T, timeout time.Duration) (*Fetcher, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspaces")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unrelated.txt"), []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fetcher, err := NewFetcher(Config{WorkspaceRoot: root, AllowedRepositories: []string{f.URL}, Timeout: timeout})
	if err != nil {
		t.Fatalf("create fixture source fetcher: %v", err)
	}
	// Only tests replace the public-address resolver and add this fixture CA.
	// The reserved fixture hostname requires this exact curl address pin; TLS
	// still verifies that hostname, because the certificate has no IP SAN.
	// The production constructor keeps HTTPS validation and certificate checks.
	fetcher.resolve = func(ctx context.Context, u *url.URL) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if u.String() != f.URL {
			return "", errors.New("test resolver received an unexpected repository URL")
		}
		return net.JoinHostPort(u.Hostname(), u.Port()) + ":127.0.0.1", nil
	}
	fetcher.gitConfig = []string{"http.sslCAInfo=" + f.CAFile}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return fetcher, canonicalRoot
}

func (f *sourceGitFixture) git(t *testing.T, directory string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, f.Git, append([]string{"-c", "core.hooksPath=/dev/null", "-c", "user.name=Shipyard source test", "-c", "user.email=shipyard-test@example.invalid", "-C", directory}, args...)...)
	command.Env = f.Env
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("fixture git %v: %v\n%s\n%s", args, err, output, stderr.String())
	}
	return strings.TrimSpace(string(output))
}

func newSourceFixtureCertificate(t *testing.T, root string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Shipyard local Git fixture"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		DNSNames: []string{sourceFixtureHost},
		IsCA:     true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	certificate, err := tls.X509KeyPair(certificatePEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(root, "fixture-ca.pem")
	if err := os.WriteFile(caFile, certificatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certificate, caFile
}

func assertSourceWorkspaceEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "unrelated.txt" {
		t.Errorf("workspace entries = %v, want only caller-owned unrelated.txt", entries)
	}
	assertSourceFile(t, filepath.Join(root, "unrelated.txt"), "keep me\n")
}

func assertSourceFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Errorf("file %s = %q, %v; want %q", path, got, err, want)
	}
}

func sourceShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func sourceConfigQuote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value) + `"`
}
