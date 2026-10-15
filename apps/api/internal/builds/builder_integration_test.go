//go:build integration

package builds

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

func TestBuildKitImageIsAvailableInDocker(t *testing.T) {
	builder := integrationBuilder(t)
	root := t.TempDir()
	writeBuildFixture(t, root, "Dockerfile", "FROM scratch\nCOPY payload /payload\n")
	writeBuildFixture(t, root, "payload", "shipyard-build-fixture")
	writeBuildFixture(t, root, ".dockerignore", "payload\n")
	writeBuildFixture(t, root, "Dockerfile.dockerignore", "")
	input := Input{CheckoutPath: root, DeploymentID: "00000000-0000-4000-8000-000000000041", CommitSHA: strings.Repeat("a", 40)}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	image, err := builder.Build(ctx, input)
	if err != nil {
		t.Fatalf("build controlled scratch image: %v", err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := builder.docker.ImageRemove(cleanup, image.Reference, client.ImageRemoveOptions{PruneChildren: false}); err != nil {
			t.Errorf("remove owned fixture image: %v", err)
		}
	})
	inspected, err := builder.docker.ImageInspect(ctx, image.Reference)
	if err != nil || inspected.ID != image.ID || len(inspected.RootFS.Layers) == 0 {
		t.Fatalf("built image was not available with COPY layer: ID=%s, error=%v", inspected.ID, err)
	}
	if !strings.HasPrefix(image.Reference, "shipyard/deployments:"+input.DeploymentID+"-") || !imageIDPattern.MatchString(image.ID) {
		t.Errorf("unexpected built image identity: %+v", image)
	}
	if inspected.Config.Labels["io.shipyard.commit_sha"] != input.CommitSHA || inspected.Config.Labels["io.shipyard.deployment_id"] != input.DeploymentID {
		t.Error("built image lost source/attempt identity")
	}
	if _, err := os.Stat(filepath.Join(root, "payload")); err != nil {
		t.Error("builder changed the caller-owned checkout")
	}
}

func TestBuildKitFailureLeavesNoTaggedImage(t *testing.T) {
	for _, excluded := range []bool{false, true} {
		name := "missing COPY input"
		if excluded {
			name = "dockerignore excludes COPY input"
		}
		t.Run(name, func(t *testing.T) {
			builder := integrationBuilder(t)
			observed := &observedBuildEngine{engine: builder.docker}
			builder.docker = observed
			root := t.TempDir()
			writeBuildFixture(t, root, "Dockerfile", "FROM scratch\nCOPY missing /missing\n")
			if excluded {
				writeBuildFixture(t, root, "missing", "must-not-be-in-context")
				writeBuildFixture(t, root, ".dockerignore", "missing\n")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			image, err := builder.Build(ctx, Input{CheckoutPath: root, DeploymentID: "00000000-0000-4000-8000-000000000042", CommitSHA: strings.Repeat("b", 40)})
			var buildError *BuildError
			if !errors.As(err, &buildError) || image != (Image{}) || observed.reference == "" {
				t.Fatalf("failed build returned image=%+v, error=%v", image, err)
			}
			if _, err := builder.docker.ImageInspect(ctx, observed.reference); !errdefs.IsNotFound(err) {
				t.Errorf("failed build left an image tag: %v", err)
			}
		})
	}
}

func integrationBuilder(t *testing.T) *Builder {
	t.Helper()
	host := os.Getenv("SHIPYARD_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("set SHIPYARD_TEST_DOCKER_HOST to an explicit disposable Linux Docker engine socket")
	}
	builder, err := NewBuilder(Config{DockerHost: host, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = builder.Close() })
	return builder
}

func writeBuildFixture(t *testing.T, root, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

type observedBuildEngine struct {
	engine
	reference string
}

func (e *observedBuildEngine) ImageBuild(ctx context.Context, archive io.Reader, options client.ImageBuildOptions) (client.ImageBuildResult, error) {
	e.reference = options.Tags[0]
	return e.engine.ImageBuild(ctx, archive, options)
}
