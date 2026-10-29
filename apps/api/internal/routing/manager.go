package routing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct{ Directory string }

type Manager struct {
	root           *os.Root
	gate           chan struct{}
	activationGate chan struct{}
	closed         bool
}

func NewManager(config Config) (*Manager, error) {
	if err := checkPlatform(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(config.Directory) {
		return nil, errors.New("route directory must be an absolute path")
	}
	info, err := os.Lstat(config.Directory)
	if err != nil {
		return nil, fmt.Errorf("inspect route directory: %w", err)
	}
	if !privateOwned(info, true) {
		return nil, errors.New("route directory must be owned by the service user with permissions 0700")
	}
	root, err := os.OpenRoot(config.Directory)
	if err != nil {
		return nil, fmt.Errorf("open route directory: %w", err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, errors.New("route directory changed while opening")
	}
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	activationGate := make(chan struct{}, 1)
	activationGate <- struct{}{}
	return &Manager{root: root, gate: gate, activationGate: activationGate}, nil
}

func (m *Manager) Close() error {
	<-m.activationGate
	defer func() { m.activationGate <- struct{}{} }()
	<-m.gate
	defer func() { m.gate <- struct{}{} }()
	if m.closed {
		return nil
	}
	m.closed = true
	return m.root.Close()
}

// Put conditionally publishes a route. A nil expected binding requires absence;
// retrying an already published identical binding is harmless. Errors after the
// rename can conceal publication; inspect Get before deciding what to do next.
func (m *Manager) Put(ctx context.Context, next Binding, expected *Binding) error {
	if !validBinding(next) || expected != nil && (!validBinding(*expected) || expected.ProjectID != next.ProjectID) {
		return ErrInvalidBinding
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	current, legacy, err := m.readOwned(next.ProjectID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err == nil && current == next {
		if err := ctx.Err(); err != nil {
			return err
		}
		if legacy {
			return m.write(ctx, next)
		}
		return m.syncDirectory()
	}
	if errors.Is(err, ErrNotFound) {
		if expected != nil {
			return ErrConflict
		}
	} else if expected == nil || current != *expected {
		return ErrConflict
	}
	return m.write(ctx, next)
}

func (m *Manager) Get(ctx context.Context, projectID string) (Binding, error) {
	if !uuidPattern.MatchString(projectID) {
		return Binding{}, ErrInvalidBinding
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return Binding{}, err
	}
	defer unlock()
	return m.read(projectID)
}

// Remove only retires the exact expected binding. Stale cleanup cannot delete
// a newer route, including when another manager has replaced the project file.
func (m *Manager) Remove(ctx context.Context, expected Binding) error {
	if !validBinding(expected) {
		return ErrInvalidBinding
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := m.read(expected.ProjectID)
	if errors.Is(err, ErrNotFound) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return m.syncDirectory()
	}
	if err != nil {
		return err
	}
	if current != expected {
		return ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.root.Remove(routeName(expected.ProjectID)); err != nil {
		return fmt.Errorf("remove route: %w", err)
	}
	return m.syncDirectory()
}

func (m *Manager) lock(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.gate:
	}
	if err := ctx.Err(); err != nil {
		m.gate <- struct{}{}
		return nil, err
	}
	if m.closed {
		m.gate <- struct{}{}
		return nil, os.ErrClosed
	}
	file, err := lockDirectory(ctx, m.root)
	if err != nil {
		m.gate <- struct{}{}
		return nil, err
	}
	return func() { _ = file.Close(); m.gate <- struct{}{} }, nil
}

func privateOwned(info os.FileInfo, directory bool) bool {
	if !ownedByCurrentUser(info) {
		return false
	}
	if directory {
		return info.Mode() == os.ModeDir|0700
	}
	return info.Mode() == 0600
}
