package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/source"
)

type store interface {
	ClaimNext(context.Context) (deployments.Claim, error)
	NextInterrupted(context.Context) (deployments.Claim, error)
	RecordSourceCommit(context.Context, deployments.Claim, string) (deployments.Deployment, error)
	FailExecution(context.Context, deployments.Claim, string, string) (deployments.Deployment, error)
}

type fetcher interface {
	Fetch(context.Context, string, string) (*source.Checkout, error)
}

type Worker struct {
	store         store
	fetcher       fetcher
	acquire       func(context.Context) (lease, error)
	pollInterval  time.Duration
	checkInterval time.Duration
}

func New(config Config, pool *pgxpool.Pool) (*Worker, error) {
	if pool == nil || pool.Config().MaxConns < 2 {
		return nil, errors.New("executor requires a PostgreSQL pool with at least two connections")
	}
	fetcher, err := source.NewFetcher(source.Config{WorkspaceRoot: config.WorkspaceRoot, AllowedRepositories: config.AllowedRepositories})
	if err != nil {
		return nil, err
	}
	return &Worker{store: deployments.NewStore(pool), fetcher: fetcher,
		acquire:      func(ctx context.Context) (lease, error) { return acquireLease(ctx, pool) },
		pollInterval: time.Second, checkInterval: time.Second}, nil
}

func (w *Worker) Run(parent context.Context) (resultErr error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	acquireCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	ownership, err := w.acquire(acquireCtx)
	stop()
	if err != nil {
		if parent.Err() != nil {
			return nil
		}
		return err
	}
	lost, watched := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(watched)
		ticker := time.NewTicker(w.checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkCtx, stop := context.WithTimeout(ctx, 3*time.Second)
				err := ownership.Check(checkCtx)
				stop()
				if err != nil && ctx.Err() == nil {
					lost <- err
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-watched
		closeErr := ownership.Close()
		select {
		case err := <-lost:
			resultErr = errors.Join(resultErr, fmt.Errorf("executor ownership lost: %w", err))
		default:
		}
		if parent.Err() != nil {
			resultErr = nil
		}
		if closeErr != nil && parent.Err() == nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("release executor ownership: %w", closeErr))
		}
	}()
	if err := w.recover(ctx); err != nil {
		return err
	}
	for ctx.Err() == nil {
		claimCtx, stop := context.WithTimeout(ctx, 3*time.Second)
		claim, err := w.store.ClaimNext(claimCtx)
		stop()
		if errors.Is(err, deployments.ErrNoQueuedDeployments) {
			select {
			case <-ctx.Done():
			case <-time.After(w.pollInterval):
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("claim queued deployment: %w", err)
		}
		if err := w.execute(ctx, claim); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (w *Worker) recover(ctx context.Context) error {
	ctx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	for {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		claim, err := w.store.NextInterrupted(readCtx)
		cancel()
		if errors.Is(err, deployments.ErrNoInterruptedDeployments) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read interrupted attempt: %w", err)
		}
		if claim.Status != deployments.StatusFetchingSource {
			return errors.New("source executor cannot recover build or container-stage attempts")
		}
		writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err = w.store.FailExecution(writeCtx, claim, claim.Status, "Deployment was interrupted during source preparation.")
		cancel()
		if errors.Is(err, deployments.ErrInvalidTransition) {
			continue
		}
		if err != nil {
			return fmt.Errorf("record interrupted attempt: %w", err)
		}
	}
}

// execute records the source checkpoint. The attempt stays in FETCHING_SOURCE
// until the executor can actually begin building its image.
func (w *Worker) execute(ctx context.Context, claim deployments.Claim) (resultErr error) {
	checkout, err := w.fetcher.Fetch(ctx, claim.GitURL, claim.Branch)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		reason := "Source checkout failed."
		if errors.Is(err, source.ErrNotAllowed) {
			reason = "Repository is not approved for source checkout."
		}
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "Source checkout timed out."
		}
		slog.Error("Source checkout failed", "deployment_id", claim.ID, "error", err)
		writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, err = w.store.FailExecution(writeCtx, claim, deployments.StatusFetchingSource, reason)
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, checkout.Close()) }()
	writeCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	_, err = w.store.RecordSourceCommit(writeCtx, claim, checkout.CommitSHA)
	stop()
	if err != nil {
		return fmt.Errorf("record fetched source: %w", err)
	}
	return nil
}
