package builds

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	dockerimage "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestBuilderUsesSuppliedStableBuildIdentity(t *testing.T) {
	input := builderTestInput(t)
	input.DeploymentID = strings.ToUpper("00000000-0000-4000-8000-0000000000ab")
	input.BuildID = strings.ToUpper("00000000-0000-4000-8000-0000000000cd")
	fake := &stableBuildTestEngine{builderTestEngine: &builderTestEngine{}}
	builder := &Builder{docker: fake, timeout: time.Second}
	image, err := builder.Build(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if fake.options.BuildID != "shipyard-000000000000400080000000000000cd" ||
		image.Reference != "shipyard/deployments:00000000-0000-4000-8000-0000000000ab-000000000000400080000000000000cd" ||
		fake.options.Labels["io.shipyard.deployment_id"] != strings.ToLower(input.DeploymentID) {
		t.Fatalf("stable identity was not canonicalized: image=%+v, options=%+v", image, fake.options)
	}
	second, err := builder.Build(t.Context(), input)
	if err == nil || second != (Image{}) || fake.buildCalls != 1 || len(fake.canceled) != 0 || len(fake.removed) != 0 {
		t.Fatalf("existing stable build was overwritten or removed: second=%+v, error=%v, calls=%d", second, err, fake.buildCalls)
	}
}

func TestBuilderStableBuildPreflightFailsClosed(t *testing.T) {
	input := builderTestInput(t)
	input.BuildID = input.DeploymentID
	for _, inspectErr := range []error{nil, errors.New("private-inspection-sentinel")} {
		fake := &stableBuildTestEngine{builderTestEngine: &builderTestEngine{}, preexisting: true, preflightErr: inspectErr}
		_, err := (&Builder{docker: fake, timeout: time.Second}).Build(t.Context(), input)
		var private *BuildError
		if !errors.As(err, &private) || fake.buildCalls != 0 || len(fake.canceled) != 0 || len(fake.removed) != 0 || strings.Contains(err.Error(), "sentinel") {
			t.Fatalf("preflight mutated Docker or exposed its cause: error=%v, fake=%+v", err, fake)
		}
	}
}

func TestBuilderStableFailureVerifiesCleanupOwnership(t *testing.T) {
	input := builderTestInput(t)
	input.BuildID = input.DeploymentID
	for _, wrongLabels := range []bool{false, true} {
		fake := &stableBuildTestEngine{builderTestEngine: &builderTestEngine{wrongLabels: wrongLabels, buildError: errors.New("build failed after upload")}}
		_, err := (&Builder{docker: fake, timeout: time.Second}).Build(t.Context(), input)
		if err == nil || len(fake.canceled) != 1 || (len(fake.removed) == 1) == wrongLabels || fake.unsafeRemoval || fake.badCleanupContext {
			t.Fatalf("stable failure cleaned an unverified image: wrong labels=%t, error=%v, fake=%+v", wrongLabels, err, fake)
		}
	}
}

func TestBuilderCleanupUsesExactVerifiedTag(t *testing.T) {
	input := cleanupTestInput()
	identity, err := identifyBuild(input, false)
	if err != nil {
		t.Fatal(err)
	}
	fake := &cleanupTestEngine{labels: identity.labels()}
	builder := &Builder{docker: fake}
	if err := builder.Cleanup(t.Context(), input, builderTestImageID); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fake.calls, ",") != "cancel,inspect,remove" || fake.canceled != identity.id ||
		fake.inspected != identity.reference || fake.removed != identity.reference ||
		fake.options.Force || fake.options.PruneChildren || fake.badDeadline {
		t.Fatalf("unsafe cleanup request: %+v", fake)
	}
}

func TestBuilderCleanupRejectsForeignOrUnverifiableImages(t *testing.T) {
	input := cleanupTestInput()
	identity, _ := identifyBuild(input, false)
	for _, field := range []string{"io.shipyard.managed", "io.shipyard.deployment_id", "io.shipyard.commit_sha", "io.shipyard.build_id", "image ID", "missing config"} {
		t.Run(field, func(t *testing.T) {
			fake := &cleanupTestEngine{labels: maps.Clone(identity.labels())}
			switch field {
			case "image ID":
				fake.imageID = "not-an-image-id"
			case "missing config":
				fake.missingConfig = true
			default:
				fake.labels[field] = "another-owner"
			}
			err := (&Builder{docker: fake}).Cleanup(t.Context(), input, "")
			var buildError *BuildError
			if !errors.As(err, &buildError) || err.Error() != "image cleanup failed" || fake.removed != "" {
				t.Fatalf("unverified image was removed: error=%v, removed=%q", err, fake.removed)
			}
		})
	}
}

func TestBuilderCleanupHandlesMissingAndFailedOperations(t *testing.T) {
	input := cleanupTestInput()
	identity, _ := identifyBuild(input, false)
	cause := errors.New("private-daemon-sentinel " + strings.Repeat("x", 8192))
	missing := errdefs.ErrNotFound
	for _, test := range []struct {
		name                             string
		cancelErr, inspectErr, removeErr error
		wantCalls                        string
		wantError                        bool
	}{
		{"missing build is harmless", missing, nil, nil, "cancel,inspect,remove", false},
		{"missing image is harmless", nil, missing, nil, "cancel,inspect", false},
		{"concurrent image removal is harmless", nil, nil, missing, "cancel,inspect,remove", false},
		{"failed cancellation prevents removal", cause, nil, nil, "cancel", true},
		{"failed inspection prevents removal", nil, cause, nil, "cancel,inspect", true},
		{"failed removal is retained", nil, nil, cause, "cancel,inspect,remove", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &cleanupTestEngine{labels: identity.labels(), cancelErr: test.cancelErr, inspectErr: test.inspectErr, removeErr: test.removeErr}
			err := (&Builder{docker: fake}).Cleanup(t.Context(), input, "")
			if (err != nil) != test.wantError || strings.Join(fake.calls, ",") != test.wantCalls || fake.badDeadline {
				t.Fatalf("cleanup error=%v, calls=%v, invalid deadline=%t", err, fake.calls, fake.badDeadline)
			}
			if test.wantError {
				var private *BuildError
				if !errors.Is(err, cause) || !errors.As(err, &private) || strings.Contains(err.Error(), "sentinel") {
					t.Fatalf("cleanup lost or exposed private diagnostics: %v", err)
				}
				for _, attr := range private.LogValue().Group() {
					if len(attr.Value.String()) > 4096 {
						t.Fatal("cleanup diagnostic exceeded four KiB")
					}
				}
			}
		})
	}
}

func TestBuilderCleanupRequiresReconstructibleIdentityBeforeDocker(t *testing.T) {
	for _, change := range []func(*Input){
		func(i *Input) { i.BuildID = "" },
		func(i *Input) { i.BuildID = "../../outside" },
		func(i *Input) { i.DeploymentID = "invalid" },
		func(i *Input) { i.CommitSHA = "short" },
	} {
		input := cleanupTestInput()
		change(&input)
		fake := &cleanupTestEngine{}
		if err := (&Builder{docker: fake}).Cleanup(t.Context(), input, ""); !errors.Is(err, ErrInvalidInput) || len(fake.calls) != 0 {
			t.Fatalf("invalid identity reached Docker: error=%v, calls=%v", err, fake.calls)
		}
	}
	input := builderTestInput(t)
	input.BuildID = "invalid"
	fake := &builderTestEngine{}
	if _, err := (&Builder{docker: fake}).Build(t.Context(), input); !errors.Is(err, ErrInvalidInput) || fake.buildCalls != 0 {
		t.Fatalf("invalid build ID reached Docker: error=%v, calls=%d", err, fake.buildCalls)
	}
}

func TestBuilderCleanupHonorsCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fake := &cleanupTestEngine{}
	err := (&Builder{docker: fake}).Cleanup(ctx, cleanupTestInput(), "")
	if !errors.Is(err, context.Canceled) || strings.Join(fake.calls, ",") != "cancel" || fake.removed != "" {
		t.Fatalf("canceled cleanup continued: error=%v, calls=%v", err, fake.calls)
	}
}

func cleanupTestInput() Input {
	return Input{DeploymentID: builderTestDeployment, CommitSHA: strings.Repeat("c", 40), BuildID: "00000000-0000-4000-8000-000000000002"}
}

func TestBuilderCleanupPinsRecordedImageIdentity(t *testing.T) {
	input := cleanupTestInput()
	identity, _ := identifyBuild(input, false)
	fake := &cleanupTestEngine{labels: identity.labels()}
	err := (&Builder{docker: fake}).Cleanup(t.Context(), input, "sha256:"+strings.Repeat("b", 64))
	if err == nil || fake.removed != "" {
		t.Fatalf("cleanup removed a replaced recorded image: error=%v, removed=%q", err, fake.removed)
	}
	fake = &cleanupTestEngine{}
	err = (&Builder{docker: fake}).Cleanup(t.Context(), input, "not-an-image-id")
	if !errors.Is(err, ErrInvalidInput) || len(fake.calls) != 0 {
		t.Fatalf("invalid recorded image ID reached Docker: error=%v, calls=%v", err, fake.calls)
	}
}

type cleanupTestEngine struct {
	engine
	labels                           map[string]string
	imageID                          string
	missingConfig                    bool
	cancelErr, inspectErr, removeErr error
	calls                            []string
	canceled, inspected, removed     string
	options                          client.ImageRemoveOptions
	badDeadline                      bool
}

func (f *cleanupTestEngine) bounded(ctx context.Context) {
	deadline, ok := ctx.Deadline()
	f.badDeadline = f.badDeadline || !ok || time.Until(deadline) > 5*time.Second
}

func (f *cleanupTestEngine) BuildCancel(ctx context.Context, id string, _ client.BuildCancelOptions) (client.BuildCancelResult, error) {
	f.calls = append(f.calls, "cancel")
	f.canceled = id
	f.bounded(ctx)
	return client.BuildCancelResult{}, errors.Join(ctx.Err(), f.cancelErr)
}

func (f *cleanupTestEngine) ImageInspect(ctx context.Context, reference string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	f.calls = append(f.calls, "inspect")
	f.inspected = reference
	f.bounded(ctx)
	id := f.imageID
	if id == "" {
		id = builderTestImageID
	}
	config := &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{Labels: f.labels}}
	if f.missingConfig {
		config = nil
	}
	return client.ImageInspectResult{InspectResponse: dockerimage.InspectResponse{ID: id, Config: config}}, errors.Join(ctx.Err(), f.inspectErr)
}

func (f *cleanupTestEngine) ImageRemove(ctx context.Context, reference string, options client.ImageRemoveOptions) (client.ImageRemoveResult, error) {
	f.calls = append(f.calls, "remove")
	f.removed, f.options = reference, options
	f.bounded(ctx)
	return client.ImageRemoveResult{}, errors.Join(ctx.Err(), f.removeErr)
}

type stableBuildTestEngine struct {
	*builderTestEngine
	preexisting  bool
	preflightErr error
}

func (f *stableBuildTestEngine) ImageInspect(ctx context.Context, reference string, options ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	if f.buildCalls == 0 {
		if f.preflightErr != nil {
			return client.ImageInspectResult{}, f.preflightErr
		}
		if !f.preexisting {
			return client.ImageInspectResult{}, errdefs.ErrNotFound
		}
	}
	return f.builderTestEngine.ImageInspect(ctx, reference, options...)
}
