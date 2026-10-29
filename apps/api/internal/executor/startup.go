package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/runtime"
)

func (w *Worker) startup(ctx context.Context, claim deployments.Claim, deployment deployments.Deployment) (bool, error) {
	spec, err := startupSpec(deployment)
	if err != nil {
		return false, err
	}
	if deployment.ID != claim.ID || deployment.ProjectID != claim.ProjectID || deployment.Status != deployments.StatusStarting {
		return false, errors.New("startup metadata does not belong to this claim")
	}
	created, err := w.runtime.Create(ctx, spec)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return false, fmt.Errorf("container creation outcome is uncertain: %w", err)
		}
		// A lost create response can conceal a container not visible yet. Only
		// definite pre-create rejections can terminalize an unknown identity.
		if !errors.Is(err, runtime.ErrInvalidInput) && !errors.Is(err, runtime.ErrUnsupportedImage) &&
			!errors.Is(err, runtime.ErrUnsupportedEngine) && !errdefs.IsInvalidArgument(err) {
			return false, fmt.Errorf("container creation outcome is uncertain: %w", err)
		}
		return false, w.failStartup(ctx, claim, deployment, "Container creation failed.", err)
	}
	if err := w.recordInstance(ctx, claim, *deployment.InstanceID, created); err != nil {
		return false, err
	}
	spec.ContainerID = created.ID
	started, err := w.runtime.Start(ctx, spec)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, w.failStartup(ctx, claim, deployment, "Container startup failed.", err)
	}
	if err := w.recordInstance(ctx, claim, *deployment.InstanceID, started); err != nil {
		return false, err
	}
	if !started.Running || started.Status != "running" {
		return false, w.failStartup(ctx, claim, deployment, "Application did not stay running during startup.", errors.New("candidate was not running after startup"))
	}
	return true, nil
}

func startupSpec(deployment deployments.Deployment) (runtime.Spec, error) {
	if (deployment.Status != deployments.StatusStarting && deployment.Status != deployments.StatusHealthChecking && deployment.Status != deployments.StatusActivating) || deployment.InstanceID == nil || deployment.ImageID == nil ||
		deployment.ImageReference == nil || deployment.CommitSHA == nil {
		return runtime.Spec{}, errors.New("startup has incomplete persisted instance metadata")
	}
	id := strings.ToLower(deployment.ID)
	if *deployment.ImageReference != "shipyard/deployments:"+id+"-"+strings.ReplaceAll(id, "-", "") {
		return runtime.Spec{}, errors.New("startup does not use this executor's build identity")
	}
	return runtime.Spec{ProjectID: deployment.ProjectID, DeploymentID: deployment.ID, InstanceID: *deployment.InstanceID,
		ImageID: *deployment.ImageID, CommitSHA: *deployment.CommitSHA, AppPort: deployment.AppPort}, nil
}

func (w *Worker) recordInstance(ctx context.Context, claim deployments.Claim, instanceID string, observed runtime.Instance) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := w.store.RecordInstance(ctx, claim, instanceID, deployments.Observation{ContainerID: observed.ID, Status: observed.Status,
		Running: observed.Running, ExitCode: observed.ExitCode, OOMKilled: observed.OOMKilled, HostPort: observed.HostPort})
	if err != nil {
		return fmt.Errorf("record container observation: %w", err)
	}
	return nil
}

func (w *Worker) failStartup(ctx context.Context, claim deployments.Claim, deployment deployments.Deployment, reason string, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	slog.Error("Container startup failed", "deployment_id", claim.ID, "error", cause)
	if err := w.cleanupStartup(ctx, claim, deployment); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := w.store.FailExecution(ctx, claim, deployments.StatusStarting, reason)
	if err != nil {
		return fmt.Errorf("record startup failure: %w", err)
	}
	return nil
}

// cleanupStartup reads authoritative identity before mutating Docker. Any
// uncertainty leaves the claim and image available to the next owner.
func (w *Worker) cleanupStartup(ctx context.Context, claim deployments.Claim, deployment deployments.Deployment) error {
	spec, err := startupSpec(deployment)
	if err != nil {
		return err
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	instance, err := w.store.GetInstance(readCtx, *deployment.InstanceID)
	cancel()
	if err != nil {
		return fmt.Errorf("read startup instance: %w", err)
	}
	if instance.ID != *deployment.InstanceID || instance.DeploymentID != deployment.ID {
		return errors.New("startup instance does not belong to this deployment")
	}
	if instance.ContainerID != nil {
		spec.ContainerID = *instance.ContainerID
	}
	readCtx, cancel = context.WithTimeout(ctx, 3*time.Second)
	found, err := w.runtime.Inspect(readCtx, spec)
	cancel()
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return fmt.Errorf("inspect startup candidate: %w", err)
	}
	if err == nil {
		spec.ContainerID = found.ID
		stopCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		stopped, err := w.runtime.Stop(stopCtx, spec)
		stop()
		if err != nil && !errors.Is(err, runtime.ErrNotFound) {
			return fmt.Errorf("stop startup candidate: %w", err)
		}
		if err == nil {
			if stopped.Running {
				return errors.New("startup candidate remained running after stop")
			}
			if err := w.recordInstance(ctx, claim, instance.ID, stopped); err != nil {
				return err
			}
		}
		removeCtx, remove := context.WithTimeout(ctx, 10*time.Second)
		err = w.runtime.Remove(removeCtx, spec)
		remove()
		if err != nil {
			return fmt.Errorf("remove startup candidate: %w", err)
		}
	}
	input := builds.Input{DeploymentID: deployment.ID, CommitSHA: *deployment.CommitSHA, BuildID: deployment.ID}
	if err := w.builder.Cleanup(ctx, input, *deployment.ImageID); err != nil {
		return fmt.Errorf("clean startup image: %w", err)
	}
	return nil
}
