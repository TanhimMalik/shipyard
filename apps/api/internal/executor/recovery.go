package executor

import (
	"context"
	"errors"
	"fmt"

	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/deployments"
)

func (w *Worker) cleanInterrupted(ctx context.Context, claim deployments.Claim) error {
	if claim.Status == deployments.StatusFetchingSource {
		return nil
	}
	if claim.Status == deployments.StatusStarting {
		return w.cleanupStartup(ctx, claim, claim.Deployment)
	}
	if claim.Status != deployments.StatusBuilding {
		return errors.New("executor cannot recover readiness or active deployment stages")
	}
	if claim.CommitSHA == nil {
		return errors.New("interrupted build has no recorded source commit")
	}
	input := builds.Input{DeploymentID: claim.ID, CommitSHA: *claim.CommitSHA, BuildID: claim.ID}
	if err := w.builder.Cleanup(ctx, input, ""); err != nil {
		return fmt.Errorf("clean interrupted build: %w", err)
	}
	return nil
}
