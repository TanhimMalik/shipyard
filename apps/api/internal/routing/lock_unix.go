//go:build darwin || linux

package routing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func checkPlatform() error { return nil }

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

// The lock file remains in place: replacing or removing it would let separate
// managers lock different inodes while changing the same route directory.
func lockDirectory(ctx context.Context, root *os.Root) (*os.File, error) {
	return lockNamedDirectory(ctx, root, ".shipyard.lock")
}

func lockNamedDirectory(ctx context.Context, root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect route lock: %w", err)
	}
	if err == nil && !privateOwned(info, false) {
		return nil, ErrNotOwned
	}
	file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("open route lock: %w", err)
	}
	opened, err := file.Stat()
	if err != nil || !privateOwned(opened, false) || info != nil && !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, ErrNotOwned
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, err
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = file.Close()
			return nil, fmt.Errorf("acquire route lock: %w", err)
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}
