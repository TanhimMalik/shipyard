package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrExecutorBusy = errors.New("another executor is already running for this database")

const executorLock int64 = 0x53485059415244

type lease interface {
	Check(context.Context) error
	Close() error
}

type postgresLease struct{ connection *pgxpool.Conn }

func acquireLease(ctx context.Context, pool *pgxpool.Pool) (lease, error) {
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire executor connection: %w", err)
	}
	var locked bool
	if err := connection.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", executorLock).Scan(&locked); err != nil {
		// A lost response can conceal successful session-lock acquisition.
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = connection.Conn().Close(closeCtx)
		cancel()
		connection.Release()
		return nil, fmt.Errorf("acquire executor ownership: %w", err)
	}
	if !locked {
		connection.Release()
		return nil, ErrExecutorBusy
	}
	return &postgresLease{connection: connection}, nil
}

func (l *postgresLease) Check(ctx context.Context) error {
	return l.connection.Ping(ctx)
}

func (l *postgresLease) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var unlocked bool
	err := l.connection.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", executorLock).Scan(&unlocked)
	if err != nil || !unlocked {
		_ = l.connection.Conn().Close(ctx)
		if err == nil {
			err = errors.New("executor ownership was already lost")
		}
	}
	l.connection.Release()
	return err
}
