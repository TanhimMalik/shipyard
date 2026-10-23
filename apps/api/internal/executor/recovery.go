package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/runtime"
)

func (w *Worker) cleanInterrupted(ctx context.Context, claim deployments.Claim) error {
	if claim.Status == deployments.StatusFetchingSource {
		return nil
	}
	if claim.Status != deployments.StatusBuilding && claim.Status != deployments.StatusStarting {
		return errors.New("build executor cannot recover observed container stages")
	}
	if claim.CommitSHA == nil {
		return errors.New("interrupted build has no recorded source commit")
	}
	expectedImageID := ""
	if claim.Status == deployments.StatusStarting {
		if claim.InstanceID == nil || claim.ImageID == nil || claim.ImageReference == nil {
			return errors.New("interrupted startup has incomplete instance metadata")
		}
		expectedReference := "shipyard/deployments:" + strings.ToLower(claim.ID) + "-" + strings.ReplaceAll(strings.ToLower(claim.ID), "-", "")
		if *claim.ImageReference != expectedReference {
			return errors.New("interrupted startup does not use this executor's build identity")
		}
		expectedImageID = *claim.ImageID
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		instance, err := w.store.GetInstance(readCtx, *claim.InstanceID)
		cancel()
		if err != nil {
			return fmt.Errorf("read interrupted instance: %w", err)
		}
		if instance.ID != *claim.InstanceID || instance.DeploymentID != claim.ID || instance.ContainerID != nil {
			return errors.New("interrupted instance is not an unobserved intent owned by this attempt")
		}
		spec := runtime.Spec{ProjectID: claim.ProjectID, DeploymentID: claim.ID, InstanceID: instance.ID, ImageID: *claim.ImageID, CommitSHA: *claim.CommitSHA, AppPort: claim.AppPort}
		readCtx, cancel = context.WithTimeout(ctx, 3*time.Second)
		_, err = w.inspector.Inspect(readCtx, spec)
		cancel()
		if !errors.Is(err, runtime.ErrNotFound) {
			if err == nil {
				return errors.New("interrupted instance has a Docker container; runtime recovery is required")
			}
			return fmt.Errorf("verify interrupted container absence: %w", err)
		}
	}
	input := builds.Input{DeploymentID: claim.ID, CommitSHA: *claim.CommitSHA, BuildID: claim.ID}
	if err := w.builder.Cleanup(ctx, input, expectedImageID); err != nil {
		return fmt.Errorf("clean interrupted build: %w", err)
	}
	return nil
}
