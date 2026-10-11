package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"shipyard/apps/api/migrations"
)

const usage = "usage: migrate <up|status>"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.LookupEnv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, lookupEnv func(string) (string, bool), out io.Writer) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		_, err := fmt.Fprintln(out, usage)
		return err
	}
	if len(args) != 1 || (args[0] != "up" && args[0] != "status") {
		return errors.New(usage)
	}

	url, _ := lookupEnv("SHIPYARD_DATABASE_URL")
	if strings.TrimSpace(url) == "" {
		return errors.New("SHIPYARD_DATABASE_URL is required")
	}
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return errors.New("SHIPYARD_DATABASE_URL is not a valid PostgreSQL connection string")
	}
	config.ConnectTimeout = 5 * time.Second
	db := stdlib.OpenDB(*config)
	defer db.Close()

	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = db.PingContext(connectCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	provider, err := migrations.NewProvider(db)
	if err != nil {
		return fmt.Errorf("configure migrations: %w", err)
	}

	if args[0] == "up" {
		results, err := provider.Up(ctx)
		if err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		_, err = fmt.Fprintf(out, "Applied %d migration(s).\n", len(results))
		return err
	}
	statuses, err := provider.Status(ctx)
	if err != nil {
		return fmt.Errorf("read migration status: %w", err)
	}
	for _, status := range statuses {
		if _, err := fmt.Fprintf(out, "%s %s\n", status.State, filepath.Base(status.Source.Path)); err != nil {
			return err
		}
	}
	return nil
}
