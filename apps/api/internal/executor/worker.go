package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/health"
	"shipyard/apps/api/internal/routing"
	"shipyard/apps/api/internal/runtime"
	"shipyard/apps/api/internal/source"
)

type store interface {
	ClaimNext(context.Context) (deployments.Claim, error)
	NextInterrupted(context.Context) (deployments.Claim, error)
	TakeInterrupted(context.Context, deployments.Claim) (deployments.Claim, error)
	RecordSourceCommit(context.Context, deployments.Claim, string) (deployments.Deployment, error)
	BeginBuild(context.Context, deployments.Claim) (deployments.Deployment, error)
	BeginStart(context.Context, deployments.Claim, string, string) (deployments.Deployment, error)
	BeginHealthCheck(context.Context, deployments.Claim) (deployments.Deployment, error)
	RecordReadiness(context.Context, deployments.Claim, string, deployments.ReadinessObservation) (deployments.Deployment, error)
	GetActiveRoute(context.Context, string) (deployments.RouteBinding, error)
	BeginActivation(context.Context, deployments.Claim, string, int) (deployments.Activation, error)
	ConfirmActivation(context.Context, deployments.Claim, deployments.RouteBinding) (deployments.Deployment, error)
	AbortActivation(context.Context, deployments.Claim, *deployments.RouteBinding, string) (deployments.Deployment, error)
	GetInstance(context.Context, string) (deployments.RuntimeInstance, error)
	RecordInstance(context.Context, deployments.Claim, string, deployments.Observation) (deployments.RuntimeInstance, error)
	FailExecution(context.Context, deployments.Claim, string, string) (deployments.Deployment, error)
}

type builder interface {
	Build(context.Context, builds.Input) (builds.Image, error)
	Cleanup(context.Context, builds.Input, string) error
}

type containerRuntime interface {
	Create(context.Context, runtime.Spec) (runtime.Instance, error)
	Start(context.Context, runtime.Spec) (runtime.Instance, error)
	Inspect(context.Context, runtime.Spec) (runtime.Instance, error)
	Stop(context.Context, runtime.Spec) (runtime.Instance, error)
	Remove(context.Context, runtime.Spec) error
}

type fetcher interface {
	Fetch(context.Context, string, string) (*source.Checkout, error)
}

type readinessChecker interface {
	Wait(context.Context, health.Target) (health.Result, error)
}

type routeManager interface {
	Get(context.Context, string) (routing.Binding, error)
	Put(context.Context, routing.Binding, *routing.Binding) error
	Remove(context.Context, routing.Binding) error
}

type routeVerifier interface {
	Ready(context.Context, routing.Binding, string) error
	Restored(context.Context, string, *routing.Binding) error
}

type Worker struct {
	store         store
	fetcher       fetcher
	builder       builder
	runtime       containerRuntime
	readiness     readinessChecker
	routes        routeManager
	verifier      routeVerifier
	close         func() error
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
	imageBuilder, err := builds.NewBuilder(builds.Config{DockerHost: config.DockerHost})
	if err != nil {
		return nil, err
	}
	containers, err := runtime.NewManager(runtime.Config{DockerHost: config.DockerHost})
	if err != nil {
		_ = imageBuilder.Close()
		return nil, err
	}
	checks, err := health.NewChecker(health.Config{})
	if err != nil {
		_ = imageBuilder.Close()
		_ = containers.Close()
		return nil, err
	}
	routes, err := routing.NewManager(routing.Config{Directory: config.RoutesDirectory})
	if err != nil {
		_ = imageBuilder.Close()
		_ = containers.Close()
		return nil, err
	}
	proxy, err := routing.NewVerifier(routing.VerifierConfig{URL: config.TraefikURL})
	overlap, pathErr := existingDirectoriesOverlap(config.WorkspaceRoot, config.RoutesDirectory)
	if err == nil && (pathErr != nil || overlap) {
		err = errors.New("source workspace and route directory must be separate")
	}
	if err != nil {
		_ = imageBuilder.Close()
		_ = containers.Close()
		_ = routes.Close()
		return nil, err
	}
	return &Worker{store: deployments.NewStore(pool), fetcher: fetcher, builder: imageBuilder, runtime: containers, readiness: checks, routes: routes, verifier: proxy,
		close:        func() error { return errors.Join(imageBuilder.Close(), containers.Close(), routes.Close()) },
		acquire:      func(ctx context.Context) (lease, error) { return acquireLease(ctx, pool) },
		pollInterval: time.Second, checkInterval: time.Second}, nil
}

func (w *Worker) Close() error {
	if w.close != nil {
		return w.close()
	}
	return nil
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
		takeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		claim, err = w.store.TakeInterrupted(takeCtx, claim)
		cancel()
		if errors.Is(err, deployments.ErrInvalidTransition) {
			continue
		}
		if err != nil {
			return fmt.Errorf("take interrupted attempt: %w", err)
		}
		if err := w.cleanInterrupted(ctx, claim); err != nil {
			return err
		}
		writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err = w.store.FailExecution(writeCtx, claim, claim.Status, "Deployment was interrupted by an executor restart.")
		cancel()
		if errors.Is(err, deployments.ErrInvalidTransition) {
			continue
		}
		if err != nil {
			return fmt.Errorf("record interrupted attempt: %w", err)
		}
	}
}

// execute builds, starts, and checks a candidate. Passing readiness is separate
// from route activation and does not complete the deployment.
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
	writeCtx, stop = context.WithTimeout(ctx, 3*time.Second)
	_, err = w.store.BeginBuild(writeCtx, claim)
	stop()
	if err != nil {
		return fmt.Errorf("begin image build: %w", err)
	}
	input := builds.Input{CheckoutPath: checkout.Path, DeploymentID: claim.ID, CommitSHA: checkout.CommitSHA, BuildID: claim.ID}
	image, err := w.builder.Build(ctx, input)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Error("Image build failed", "deployment_id", claim.ID, "error", err)
		// Keep the claim unfinished if cancellation or cleanup is uncertain.
		if err := w.builder.Cleanup(ctx, input, ""); err != nil {
			return fmt.Errorf("clean failed build: %w", err)
		}
		reason := "Image build failed."
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "Image build timed out."
		}
		writeCtx, stop = context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		_, err = w.store.FailExecution(writeCtx, claim, deployments.StatusBuilding, reason)
		return err
	}
	writeCtx, stop = context.WithTimeout(ctx, 3*time.Second)
	deployment, err := w.store.BeginStart(writeCtx, claim, image.Reference, image.ID)
	stop()
	if err != nil {
		return fmt.Errorf("record built image: %w", err)
	}
	started, err := w.startup(ctx, claim, deployment)
	if err != nil || !started {
		return err
	}
	ready, err := w.checkReadiness(ctx, claim, deployment)
	if err != nil || ready == nil {
		return err
	}
	return w.activate(ctx, claim, *ready)
}
