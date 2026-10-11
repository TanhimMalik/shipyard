package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed *.sql
var files embed.FS

func NewProvider(db *sql.DB) (*goose.Provider, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("configure migration lock: %w", err)
	}
	return goose.NewProvider(goose.DialectPostgres, db, files,
		goose.WithSessionLocker(boundedSessionLocker{locker}),
		goose.WithDisableGlobalRegistry(true),
	)
}

type boundedSessionLocker struct {
	lock.SessionLocker
}

func (l boundedSessionLocker) SessionUnlock(ctx context.Context, conn *sql.Conn) error {
	// Goose detaches cancellation during cleanup, so unlock needs its own deadline.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return l.SessionLocker.SessionUnlock(ctx, conn)
}
