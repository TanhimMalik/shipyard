package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"shipyard/apps/api/internal/database"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/executor"
	"shipyard/apps/api/internal/projects"
	"shipyard/apps/api/internal/server"
)

func main() {
	if err := run(); err != nil {
		slog.Error("API stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	config, err := server.LoadConfig(os.LookupEnv)
	if err != nil {
		return err
	}
	executorConfig, err := executor.LoadConfig(os.LookupEnv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := database.Open(ctx, config.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	var worker *executor.Worker
	if executorConfig.Enabled {
		worker, err = executor.New(executorConfig, pool)
		if err != nil {
			return fmt.Errorf("configure executor: %w", err)
		}
		defer worker.Close()
	}

	listener, err := net.Listen("tcp", config.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", config.ListenAddr, err)
	}
	defer listener.Close()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workerDone chan error
	if worker != nil {
		workerDone = make(chan error, 1)
		go func() { workerDone <- worker.Run(runCtx); cancel() }()
	}

	slog.Info("API listening", "address", listener.Addr().String())
	serveErr := server.Serve(runCtx, server.New(config, projects.NewStore(pool), deployments.NewStore(pool)), listener)
	cancel()
	var workerErr error
	if workerDone != nil {
		workerErr = <-workerDone
	}
	if err := errors.Join(serveErr, workerErr); err != nil {
		return err
	}
	slog.Info("API stopped")
	return nil
}
