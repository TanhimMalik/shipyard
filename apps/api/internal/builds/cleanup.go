package builds

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

type buildIdentity struct {
	id, reference, deploymentID, commitSHA string
}

func identifyBuild(input Input, allowRandom bool) (buildIdentity, error) {
	if !deploymentIDPattern.MatchString(input.DeploymentID) || !commitPattern.MatchString(input.CommitSHA) {
		return buildIdentity{}, ErrInvalidInput
	}
	var nonce string
	if input.BuildID == "" {
		if !allowRandom {
			return buildIdentity{}, ErrInvalidInput
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return buildIdentity{}, errors.New("could not generate build identity")
		}
		nonce = hex.EncodeToString(random[:])
	} else {
		if !deploymentIDPattern.MatchString(input.BuildID) {
			return buildIdentity{}, ErrInvalidInput
		}
		nonce = strings.ReplaceAll(strings.ToLower(input.BuildID), "-", "")
	}
	deploymentID := strings.ToLower(input.DeploymentID)
	return buildIdentity{
		id: "shipyard-" + nonce, reference: "shipyard/deployments:" + deploymentID + "-" + nonce,
		deploymentID: deploymentID, commitSHA: input.CommitSHA,
	}, nil
}

func (i buildIdentity) labels() map[string]string {
	return map[string]string{
		"io.shipyard.managed": "true", "io.shipyard.deployment_id": i.deploymentID,
		"io.shipyard.commit_sha": i.commitSHA, "io.shipyard.build_id": i.id,
	}
}

// Cleanup needs the stable BuildID supplied before starting a build. It requests
// cancellation and removes only the matching tagged image currently in Docker;
// cancellation does not establish that a remote build has finished stopping.
func (b *Builder) Cleanup(ctx context.Context, input Input, expectedImageID string) error {
	if expectedImageID != "" && !imageIDPattern.MatchString(expectedImageID) {
		return ErrInvalidInput
	}
	identity, err := identifyBuild(input, false)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := b.docker.BuildCancel(ctx, identity.id, client.BuildCancelOptions{}); err != nil && !errdefs.IsNotFound(err) {
		return cleanupError(err)
	}
	inspected, err := b.docker.ImageInspect(ctx, identity.reference)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return cleanupError(err)
	}
	if !imageIDPattern.MatchString(inspected.ID) || inspected.Config == nil {
		return cleanupError(errors.New("image ownership could not be verified"))
	}
	if expectedImageID != "" && inspected.ID != expectedImageID {
		return cleanupError(errors.New("image identity did not match the recorded build"))
	}
	for key, value := range identity.labels() {
		if inspected.Config.Labels[key] != value {
			return cleanupError(errors.New("image ownership metadata did not match"))
		}
	}
	_, err = b.docker.ImageRemove(ctx, identity.reference, client.ImageRemoveOptions{Force: false, PruneChildren: false})
	if err != nil && !errdefs.IsNotFound(err) {
		return cleanupError(err)
	}
	return nil
}

func cleanupError(cause error) error {
	return &BuildError{cause: cause, message: "image cleanup failed"}
}
