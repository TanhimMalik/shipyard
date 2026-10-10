package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

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

	listener, err := net.Listen("tcp", config.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", config.ListenAddr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("API listening", "address", listener.Addr().String())
	if err := server.Serve(ctx, server.New(config), listener); err != nil {
		return err
	}
	slog.Info("API stopped")
	return nil
}
