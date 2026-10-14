//go:build !darwin && !linux

package source

import (
	"context"
	"os"
)

func checkPlatform() error                  { return ErrUnsupportedPlatform }
func checkGitVersion(string) error          { return ErrUnsupportedPlatform }
func checkWorkspaceOwner(os.FileInfo) error { return ErrUnsupportedPlatform }

func (f *Fetcher) runGit(context.Context, string, string, string, string, ...string) (string, error) {
	return "", ErrUnsupportedPlatform
}
