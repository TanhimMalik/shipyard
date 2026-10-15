package builds

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/client"
)

var ErrInvalidInput = errors.New("invalid image build input")

type Config struct {
	DockerHost string
	Timeout    time.Duration
}

type Input struct {
	CheckoutPath string
	DeploymentID string
	CommitSHA    string
}

type Image struct {
	Reference string
	ID        string
}

type engine interface {
	ImageBuild(context.Context, io.Reader, client.ImageBuildOptions) (client.ImageBuildResult, error)
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	BuildCancel(context.Context, string, client.BuildCancelOptions) (client.BuildCancelResult, error)
	ImageRemove(context.Context, string, client.ImageRemoveOptions) (client.ImageRemoveResult, error)
}

type Builder struct {
	docker  engine
	timeout time.Duration
	close   func() error
}

// NewBuilder uses an explicit local Docker socket, without reading Docker's
// environment, credential files, or current CLI context.
func NewBuilder(config Config) (*Builder, error) {
	u, err := url.Parse(config.DockerHost)
	if err != nil || u.Scheme != "unix" || u.Host != "" || u.User != nil ||
		!filepath.IsAbs(u.Path) || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("Docker host must be an explicit absolute unix socket URL")
	}
	if config.Timeout == 0 {
		config.Timeout = 5 * time.Minute
	}
	if config.Timeout < 0 || config.Timeout > 30*time.Minute {
		return nil, errors.New("build timeout must be positive and at most thirty minutes")
	}
	docker, err := client.New(client.WithHost(config.DockerHost), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, errors.New("could not configure Docker client")
	}
	return &Builder{docker: docker, timeout: config.Timeout, close: docker.Close}, nil
}

func (b *Builder) Close() error {
	if b.close != nil {
		return b.close()
	}
	return nil
}

var (
	deploymentIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	commitPattern       = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	imageIDPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Build consumes a controlled source checkout. The caller owns that checkout
// until Build returns and owns the resulting image after a successful return.
func (b *Builder) Build(ctx context.Context, input Input) (result Image, resultErr error) {
	if !deploymentIDPattern.MatchString(input.DeploymentID) || !commitPattern.MatchString(input.CommitSHA) || !filepath.IsAbs(input.CheckoutPath) {
		return Image{}, ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Image{}, errors.New("could not generate build identity")
	}
	buildID := "shipyard-" + hex.EncodeToString(nonce[:])
	deploymentID := strings.ToLower(input.DeploymentID)
	reference := "shipyard/deployments:" + deploymentID + "-" + hex.EncodeToString(nonce[:])
	started := false
	defer func() {
		if resultErr == nil {
			return
		}
		result = Image{}
		if ctx.Err() != nil {
			resultErr = ctx.Err()
		}
		cancel()
		if started {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_, cancelErr := b.docker.BuildCancel(cleanup, buildID, client.BuildCancelOptions{})
			_, removeErr := b.docker.ImageRemove(cleanup, reference, client.ImageRemoveOptions{PruneChildren: false})
			for _, err := range []error{cancelErr, removeErr} {
				if err != nil && !errdefs.IsNotFound(err) {
					resultErr = errors.Join(resultErr, fmt.Errorf("build cleanup: %w", err))
				}
			}
		}
		resultErr = &BuildError{cause: resultErr}
	}()
	archive, err := prepareContext(ctx, input.CheckoutPath)
	if err != nil {
		return Image{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, archive.Close()) }()
	labels := map[string]string{
		"io.shipyard.managed":       "true",
		"io.shipyard.deployment_id": deploymentID,
		"io.shipyard.commit_sha":    input.CommitSHA,
		"io.shipyard.build_id":      buildID,
	}
	started = true
	response, err := b.docker.ImageBuild(ctx, archive, client.ImageBuildOptions{
		Version: build.BuilderBuildKit, Dockerfile: "Dockerfile", Tags: []string{reference},
		Labels: labels, BuildID: buildID, Remove: true, ForceRemove: true,
	})
	if err != nil {
		return Image{}, err
	}
	defer response.Body.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = response.Body.Close() })
	defer stopClose()
	imageID, err := readBuildResult(response.Body)
	if err != nil {
		return Image{}, err
	}
	inspected, err := b.docker.ImageInspect(ctx, reference)
	if err != nil {
		return Image{}, err
	}
	if inspected.ID != imageID || inspected.Os != "linux" || inspected.Config == nil {
		return Image{}, errors.New("built image could not be verified in Docker")
	}
	for key, value := range labels {
		if inspected.Config.Labels[key] != value {
			return Image{}, errors.New("built image ownership metadata did not match")
		}
	}
	if err := ctx.Err(); err != nil {
		return Image{}, err
	}
	return Image{Reference: reference, ID: imageID}, nil
}

// BuildError keeps daemon/build diagnostics private and bounded. Its public
// error text is safe to include in a higher-level failure response.
type BuildError struct{ cause error }

func (e *BuildError) Error() string { return "image build failed" }
func (e *BuildError) Unwrap() error { return e.cause }
func (e *BuildError) LogValue() slog.Value {
	detail := e.cause.Error()
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	return slog.GroupValue(slog.String("message", e.Error()), slog.String("detail", detail))
}
