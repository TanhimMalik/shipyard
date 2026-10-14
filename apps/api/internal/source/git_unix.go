//go:build darwin || linux

package source

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func checkGitVersion(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = []string{"LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null"}
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return errors.New("could not determine Git version")
	}
	if !supportedGitVersion(string(output.data)) {
		return errors.New("source checkout requires Git 2.39 or newer")
	}
	return nil
}

func (f *Fetcher) runGit(ctx context.Context, root, home, empty, resolution string, args ...string) (string, error) {
	config := []string{
		"core.hooksPath=" + empty,
		"core.attributesFile=/dev/null",
		"core.autocrlf=false",
		"core.askPass=",
		"credential.helper=",
		"protocol.allow=never",
		"protocol.https.allow=always",
		"http.proxy=",
		"http.emptyAuth=false",
		"http.sslVerify=true",
		"http.followRedirects=false",
		"http.curloptResolve=" + resolution,
		"http.lowSpeedLimit=1",
		"http.lowSpeedTime=30",
		"submodule.recurse=false",
		"fetch.recurseSubmodules=false",
		"fetch.fsckObjects=true",
		"transfer.fsckObjects=true",
		"gc.auto=0",
		"maintenance.auto=false",
	}
	var commandArgs []string
	for _, value := range append(config, f.gitConfig...) {
		commandArgs = append(commandArgs, "-c", value)
	}
	commandArgs = append(commandArgs, args...)
	cmd := exec.CommandContext(ctx, f.gitPath, commandArgs...)
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + filepath.Dir(f.gitPath) + ":/usr/bin:/bin",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"TMPDIR=" + root,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=https",
	}
	// Git starts transport helpers. Cancel the whole process group so a stalled
	// helper cannot keep downloading or writing after workspace cleanup.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	var output, diagnostic boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = &diagnostic
	err := cmd.Run()
	// A helper can outlive an exited parent without keeping its output pipes
	// open. Stop the process group on every exit, including WaitDelay failures.
	if cmd.Process != nil {
		if cleanupErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cleanupErr != nil && !errors.Is(cleanupErr, syscall.ESRCH) {
			err = errors.Join(err, cleanupErr)
		}
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return string(output.data), &GitCommandError{cause: err, diagnostic: string(diagnostic.data)}
	}
	return string(output.data), err
}

type boundedOutput struct{ data []byte }

func (b *boundedOutput) Write(data []byte) (int, error) {
	const limit = 4096
	if remaining := limit - len(b.data); remaining > 0 {
		b.data = append(b.data, data[:min(remaining, len(data))]...)
	}
	return len(data), nil
}

func checkPlatform() error { return nil }

func checkWorkspaceOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("source workspace root must be owned by the current service user")
	}
	return nil
}
