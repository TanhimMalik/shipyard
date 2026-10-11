package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("SHIPYARD_DATABASE_URL must be a valid PostgreSQL connection string")
	}
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("could not initialize PostgreSQL pool")
	}
	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		if connectCtx.Err() != nil {
			return nil, fmt.Errorf("connect to PostgreSQL: %w", connectCtx.Err())
		}
		return nil, errors.New("cannot connect to PostgreSQL; check SHIPYARD_DATABASE_URL and database availability")
	}
	return pool, nil
}
