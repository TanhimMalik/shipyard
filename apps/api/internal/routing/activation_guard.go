package routing

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// WithActivation serializes an entire activation or its recovery across local
// engine processes. The callback may call Get, Put, and Remove; it must not
// reenter WithActivation or call Close. Close waits for this operation to finish.
func (m *Manager) WithActivation(ctx context.Context, operation func(context.Context) error) (resultErr error) {
	if operation == nil {
		return errors.New("activation operation is required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.activationGate:
	}
	defer func() { m.activationGate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.gate:
	}
	if m.closed {
		m.gate <- struct{}{}
		return os.ErrClosed
	}
	guard, err := lockNamedDirectory(ctx, m.root, ".shipyard-activation.lock")
	m.gate <- struct{}{}
	if err != nil {
		return err
	}
	defer func() {
		if err := guard.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("release activation ownership: %w", err))
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	return operation(ctx)
}
