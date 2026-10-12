package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/projects"
)

func New(config Config, projectStore projects.Store, deploymentStore deployments.Store) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "{\"status\":\"ok\"}\n")
	})
	if projectStore != nil {
		projectHandler := projects.NewHandler(projectStore)
		mux.Handle("/api/projects", projectHandler)
		mux.Handle("/api/projects/", projectHandler)
	}

	if deploymentStore != nil {
		deploymentHandler := deployments.NewHandler(deploymentStore)
		mux.Handle("/api/projects/{projectID}/deployments", deploymentHandler)
		mux.Handle("/api/deployments", deploymentHandler)
		mux.Handle("/api/deployments/", deploymentHandler)
	}

	return &http.Server{
		Addr:              config.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func Serve(ctx context.Context, httpServer *http.Server, listener net.Listener) error {
	defer httpServer.Close()
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- httpServer.Serve(listener)
	}()

	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		closeErr := httpServer.Close()
		<-serveResult
		return errors.Join(fmt.Errorf("shutdown HTTP: %w", err), closeErr)
	}

	if err := <-serveResult; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP: %w", err)
	}
	return nil
}
