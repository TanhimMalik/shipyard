package migrations

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

type unlockProbe struct {
	t *testing.T
}

func (p unlockProbe) SessionLock(context.Context, *sql.Conn) error { return nil }

func (p unlockProbe) SessionUnlock(ctx context.Context, _ *sql.Conn) error {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
		p.t.Fatal("unlock must have its own bounded deadline")
	}
	if err := ctx.Err(); err != nil {
		p.t.Fatalf("cleanup context is already cancelled: %v", err)
	}
	return nil
}

func TestUnlockBoundsDetachedCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	locker := boundedSessionLocker{unlockProbe{t}}
	if err := locker.SessionUnlock(context.WithoutCancel(ctx), nil); err != nil {
		t.Fatal(err)
	}
}
