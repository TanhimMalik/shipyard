package source

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"shipyard/apps/api/internal/gitinput"
)

var (
	ErrInvalidSource       = errors.New("invalid repository URL or branch")
	ErrUnsupportedPlatform = errors.New("source checkout requires macOS or Linux")
	ErrNotAllowed          = errors.New("repository is not approved for source checkout")
)

type Config struct {
	WorkspaceRoot       string
	AllowedRepositories []string
	Timeout             time.Duration
}

type Fetcher struct {
	root      string
	allowed   map[string]struct{}
	timeout   time.Duration
	gitPath   string
	resolve   func(context.Context, *url.URL) (string, error)
	gitConfig []string
}

// Checkout owns one temporary workspace. The caller must Close it after use.
type Checkout struct {
	Path      string
	CommitSHA string
	root      string
	mu        sync.Mutex
}

func (c *Checkout) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.root == "" {
		return nil
	}
	if err := os.RemoveAll(c.root); err != nil {
		return fmt.Errorf("remove source workspace: %w", err)
	}
	c.root = ""
	return nil
}

func NewFetcher(config Config) (*Fetcher, error) {
	if err := checkPlatform(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(config.WorkspaceRoot) {
		return nil, errors.New("source workspace root must be an absolute path")
	}
	if config.Timeout == 0 {
		config.Timeout = 2 * time.Minute
	}
	if config.Timeout < 0 || config.Timeout > 10*time.Minute {
		return nil, errors.New("source timeout must be positive and at most ten minutes")
	}
	if len(config.AllowedRepositories) == 0 {
		return nil, errors.New("at least one source repository must be explicitly approved")
	}
	allowed := make(map[string]struct{}, len(config.AllowedRepositories))
	for _, repository := range config.AllowedRepositories {
		if !gitinput.ValidURL(repository) {
			return nil, ErrInvalidSource
		}
		allowed[repository] = struct{}{}
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("find Git: %w", err)
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		return nil, fmt.Errorf("resolve Git executable: %w", err)
	}
	if err := checkGitVersion(gitPath); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(config.WorkspaceRoot, 0700); err != nil {
		return nil, fmt.Errorf("create source workspace root: %w", err)
	}
	root, err := filepath.EvalSymlinks(config.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve source workspace root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect source workspace root: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("source workspace root must be a private directory (0700)")
	}
	if err := checkWorkspaceOwner(info); err != nil {
		return nil, err
	}
	return &Fetcher{root: root, allowed: allowed, timeout: config.Timeout, gitPath: gitPath, resolve: pinHost}, nil
}

var commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Fetch only accepts exact URLs approved by the operator. It does not use
// credentials, follow redirects, fetch submodules, or execute repository hooks.
func (f *Fetcher) Fetch(ctx context.Context, repository, branch string) (_ *Checkout, resultErr error) {
	if !gitinput.ValidURL(repository) || !gitinput.ValidBranch(branch) {
		return nil, ErrInvalidSource
	}
	if _, ok := f.allowed[repository]; !ok {
		return nil, ErrNotAllowed
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u, _ := url.Parse(repository)
	resolution, err := f.resolve(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("resolve source host: %w", err)
	}
	root, err := os.MkdirTemp(f.root, "source-")
	if err != nil {
		return nil, fmt.Errorf("create source workspace: %w", err)
	}
	checkout := &Checkout{root: root, Path: filepath.Join(root, "repository")}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, checkout.Close())
		}
	}()
	empty := filepath.Join(root, "empty")
	home := filepath.Join(root, "home")
	for _, path := range []string{empty, home} {
		if err := os.Mkdir(path, 0700); err != nil {
			return nil, fmt.Errorf("prepare source workspace: %w", err)
		}
	}
	run := func(stage string, args ...string) (string, error) {
		out, err := f.runGit(ctx, root, home, empty, resolution, args...)
		if err != nil {
			return "", fmt.Errorf("%s: %w", stage, err)
		}
		return out, nil
	}
	if _, err := run("initialize source", "init", "--quiet", "--template="+empty, checkout.Path); err != nil {
		return nil, err
	}
	ref := "refs/shipyard/source"
	if _, err := run("fetch source branch", "-C", checkout.Path, "fetch", "--quiet", "--depth=1", "--no-tags", "--no-recurse-submodules", "--", u.String(), "+refs/heads/"+branch+":"+ref); err != nil {
		return nil, err
	}
	commit, err := run("resolve source commit", "-C", checkout.Path, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return nil, err
	}
	commit = strings.TrimSpace(commit)
	if !commitPattern.MatchString(commit) {
		return nil, errors.New("Git returned an invalid source commit")
	}
	if _, err := run("checkout source commit", "-C", checkout.Path, "checkout", "--quiet", "--detach", "--force", commit, "--"); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	checkout.CommitSHA = commit
	return checkout, nil
}
