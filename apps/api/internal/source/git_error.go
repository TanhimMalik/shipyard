package source

import (
	"fmt"
	"log/slog"
	"os/exec"
)

// GitCommandError keeps bounded diagnostics for internal logging. Error omits
// remote-controlled stderr; callers can use errors.As to inspect the details.
type GitCommandError struct {
	cause      error
	diagnostic string
}

func (e *GitCommandError) Error() string {
	if exit, ok := e.cause.(*exec.ExitError); ok {
		return fmt.Sprintf("Git command failed (exit code %d)", exit.ExitCode())
	}
	return "Git command failed"
}

func (e *GitCommandError) Unwrap() error { return e.cause }

// LogValue is intended for private server logs, never an HTTP error response.
func (e *GitCommandError) LogValue() slog.Value {
	return slog.GroupValue(slog.String("message", e.Error()), slog.String("detail", e.diagnostic))
}
