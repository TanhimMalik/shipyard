//go:build !darwin && !linux

package routing

import (
	"context"
	"os"
)

func checkPlatform() error                { return ErrUnsupportedPlatform }
func ownedByCurrentUser(os.FileInfo) bool { return false }
func lockDirectory(context.Context, *os.Root) (*os.File, error) {
	return nil, ErrUnsupportedPlatform
}
func lockNamedDirectory(context.Context, *os.Root, string) (*os.File, error) {
	return nil, ErrUnsupportedPlatform
}
