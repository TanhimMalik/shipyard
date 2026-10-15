package builds

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/build"
	dockerimage "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const builderTestDeployment = "00000000-0000-4000-8000-000000000001"

var builderTestImageID = "sha256:" + strings.Repeat("a", 64)

func TestBuilderBuildVerifiesLocalImageAndOwnsResponse(t *testing.T) {
	fake := &builderTestEngine{}
	closed := false
	builder := &Builder{docker: fake, timeout: time.Second, close: func() error { closed = true; return nil }}
	input := builderTestInput(t)
	first, err := builder.Build(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	options := fake.options
	if options.Version != build.BuilderBuildKit || options.Dockerfile != "Dockerfile" || len(options.Tags) != 1 || options.Tags[0] != first.Reference {
		t.Fatalf("unexpected build request: %+v", options)
	}
	if options.AuthConfigs != nil || options.BuildArgs != nil || options.SessionID != "" || options.RemoteContext != "" || options.Outputs != nil {
		t.Error("minimal build unexpectedly supplied auth, arguments, session, remote context, or exporters")
	}
	if options.BuildID == "" || options.Labels["io.shipyard.managed"] != "true" ||
		options.Labels["io.shipyard.deployment_id"] != input.DeploymentID ||
		options.Labels["io.shipyard.commit_sha"] != input.CommitSHA ||
		options.Labels["io.shipyard.build_id"] != options.BuildID {
		t.Errorf("build ownership labels = %v, build ID = %q", options.Labels, options.BuildID)
	}
	if first.ID != builderTestImageID || fake.inspected != first.Reference || fake.uploadBytes == 0 {
		t.Errorf("result = %+v, inspected = %q, upload = %d bytes", first, fake.inspected, fake.uploadBytes)
	}
	if !fake.body.closed.Load() || fake.body.reader.Len() != 0 {
		t.Error("successful response was not consumed through EOF and closed")
	}
	if len(fake.canceled) != 0 || len(fake.removed) != 0 {
		t.Error("successful build cleaned up its returned image")
	}
	second, err := builder.Build(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if second.Reference == first.Reference || fake.options.BuildID == options.BuildID {
		t.Error("repeated builds reused their image reference or cancellation identity")
	}
	if err := builder.Close(); err != nil || !closed {
		t.Errorf("Close did not release the client: closed = %t, error = %v", closed, err)
	}
}

func TestBuilderRejectsFailedOrUnverifiableBuildResults(t *testing.T) {
	for _, test := range []struct {
		name, stream, inspectID string
		wrongLabels             bool
	}{
		{name: "HTTP success with build error", stream: `{"errorDetail":{"message":"private-build-sentinel"}}`},
		{name: "malformed JSON", stream: `{"stream":oops}`},
		{name: "truncated JSON", stream: `{"stream":"unfinished`},
		{name: "missing image aux", stream: `{"stream":"completed without an identity"}`},
		{name: "invalid image aux", stream: `{"id":"moby.image.id","aux":{"ID":"not-a-digest"}}`},
		{name: "error after image aux", stream: `{"id":"moby.image.id","aux":{"ID":"` + builderTestImageID + `"}}` + "\n" + `{"error":"late build failure"}`},
		{name: "different inspected digest", inspectID: "sha256:" + strings.Repeat("b", 64)},
		{name: "different ownership labels", wrongLabels: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &builderTestEngine{stream: test.stream, inspectID: test.inspectID, wrongLabels: test.wrongLabels}
			builder := &Builder{docker: fake, timeout: time.Second}
			result, err := builder.Build(t.Context(), builderTestInput(t))
			var buildError *BuildError
			if result != (Image{}) || !errors.As(err, &buildError) || err.Error() != "image build failed" {
				t.Fatalf("failed build = %+v, %v; want no image and sanitized BuildError", result, err)
			}
			if strings.Contains(err.Error(), "private-build-sentinel") || buildError.Unwrap() == nil {
				t.Error("public error exposed build output or discarded its cause")
			}
			if fake.body == nil || !fake.body.closed.Load() {
				t.Error("failed build response was not closed")
			}
			fake.assertCleanup(t)
		})
	}
}

func TestBuildResponseHasAByteLimit(t *testing.T) {
	reader := strings.NewReader(strings.Repeat(" ", 17*1024*1024))
	_, err := readBuildResult(reader)
	if err == nil || !strings.Contains(err.Error(), "exceeded") || reader.Len() < 1024*1024-1 {
		t.Fatalf("oversized response was not bounded: error=%v, unread=%d", err, reader.Len())
	}
}

func TestBuilderKeepsPrivateDaemonErrorInspectableAndBounded(t *testing.T) {
	cause := errors.New("private-daemon-sentinel " + strings.Repeat("x", 20*1024))
	fake := &builderTestEngine{buildError: cause}
	builder := &Builder{docker: fake, timeout: time.Second}
	_, err := builder.Build(t.Context(), builderTestInput(t))
	var buildError *BuildError
	if !errors.As(err, &buildError) || !errors.Is(err, cause) || err.Error() != "image build failed" {
		t.Fatalf("daemon error = %v, want private cause retained under sanitized BuildError", err)
	}
	value := buildError.LogValue()
	if value.Kind() != slog.KindGroup || len(value.String()) > 4096+128 {
		t.Fatalf("structured diagnostic was not bounded: kind = %s, bytes = %d", value.Kind(), len(value.String()))
	}
	for _, attribute := range value.Group() {
		if attribute.Value.Kind() == slog.KindString && len(attribute.Value.String()) > 4096 {
			t.Error("a private diagnostic string exceeded four KiB")
		}
	}
	fake.assertCleanup(t)
}

func TestBuilderCancelsSlowResponseAndCleansUpWithFreshContext(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller cancellation", true: "configured deadline"}[timeout], func(t *testing.T) {
			reader, writer := io.Pipe()
			t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
			body := &builderTestBlockedBody{ReadCloser: reader, started: make(chan struct{}), closed: make(chan struct{})}
			fake := &builderTestEngine{response: body}
			duration := 10 * time.Second
			want := context.Canceled
			if timeout {
				duration, want = time.Second, context.DeadlineExceeded
			}
			builder := &Builder{docker: fake, timeout: duration}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			input := builderTestInput(t)
			completed := make(chan error, 1)
			go func() { _, err := builder.Build(ctx, input); completed <- err }()
			select {
			case <-body.started:
			case err := <-completed:
				t.Fatalf("build ended before reading response: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("build did not start reading response")
			}
			if !timeout {
				cancel()
			}
			select {
			case err := <-completed:
				if !errors.Is(err, want) {
					t.Errorf("slow response error = %v, want %v", err, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("canceled build remained blocked reading response")
			}
			select {
			case <-body.closed:
			default:
				t.Error("interrupted response body was not closed")
			}
			fake.assertCleanup(t)
		})
	}
}

func TestBuilderRejectsInvalidInputsBeforeDocker(t *testing.T) {
	base := builderTestInput(t)
	for _, test := range []struct {
		name   string
		change func(*Input)
	}{
		{"relative checkout", func(i *Input) { i.CheckoutPath = "checkout" }},
		{"missing deployment ID", func(i *Input) { i.DeploymentID = "" }},
		{"invalid deployment ID", func(i *Input) { i.DeploymentID = "../../outside" }},
		{"short commit", func(i *Input) { i.CommitSHA = "abc123" }},
		{"uppercase commit", func(i *Input) { i.CommitSHA = strings.Repeat("A", 40) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := base
			test.change(&input)
			fake := &builderTestEngine{}
			_, err := (&Builder{docker: fake, timeout: time.Second}).Build(t.Context(), input)
			if !errors.Is(err, ErrInvalidInput) || fake.buildCalls != 0 {
				t.Errorf("invalid input error = %v, Docker calls = %d", err, fake.buildCalls)
			}
		})
	}
}

func TestNewBuilderRequiresExplicitLocalSocketAndBoundedTimeout(t *testing.T) {
	for _, config := range []Config{
		{}, {DockerHost: "tcp://127.0.0.1:2375"}, {DockerHost: "unix://relative"},
		{DockerHost: "unix:///tmp/docker.sock?extra=1"},
		{DockerHost: "unix:///tmp/docker.sock", Timeout: -time.Second},
		{DockerHost: "unix:///tmp/docker.sock", Timeout: 31 * time.Minute},
	} {
		if builder, err := NewBuilder(config); err == nil || builder != nil {
			t.Errorf("invalid config %+v accepted: builder = %v, error = %v", config, builder, err)
		}
	}
	t.Setenv("DOCKER_HOST", "tcp://untrusted.invalid:2375")
	builder, err := NewBuilder(Config{DockerHost: "unix:///tmp/shipyard-unit-test.sock"})
	if err != nil {
		t.Fatal(err)
	}
	defer builder.Close()
	actual, ok := builder.docker.(*client.Client)
	if !ok || actual.DaemonHost() != "unix:///tmp/shipyard-unit-test.sock" {
		t.Fatal("constructor did not retain the explicit socket")
	}
}

type builderTestEngine struct {
	options                          client.ImageBuildOptions
	stream, inspectID, inspected     string
	wrongLabels                      bool
	buildError                       error
	response                         io.ReadCloser
	body                             *builderTestBody
	buildCalls, uploadBytes          int
	canceled, removed                []string
	badCleanupContext, unsafeRemoval bool
}

func (f *builderTestEngine) ImageBuild(_ context.Context, archive io.Reader, options client.ImageBuildOptions) (client.ImageBuildResult, error) {
	f.buildCalls++
	f.options = options
	f.options.Labels = maps.Clone(options.Labels)
	count, err := io.Copy(io.Discard, archive)
	f.uploadBytes = int(count)
	if err != nil || f.buildError != nil {
		return client.ImageBuildResult{}, errors.Join(err, f.buildError)
	}
	if f.response != nil {
		return client.ImageBuildResult{Body: f.response}, nil
	}
	stream := f.stream
	if stream == "" {
		stream = "{\"id\":\"moby.buildkit.trace\",\"aux\":\"dHJhY2U=\"}\n{\"id\":\"moby.image.id\",\"aux\":{\"ID\":\"" + builderTestImageID + "\"}}\n{\"stream\":\"finished\\n\"}\n"
	}
	f.body = &builderTestBody{reader: strings.NewReader(stream)}
	return client.ImageBuildResult{Body: f.body}, nil
}

func (f *builderTestEngine) ImageInspect(_ context.Context, reference string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	f.inspected = reference
	id := f.inspectID
	if id == "" {
		id = builderTestImageID
	}
	labels := maps.Clone(f.options.Labels)
	if f.wrongLabels {
		labels["io.shipyard.build_id"] = "another-build"
	}
	return client.ImageInspectResult{InspectResponse: dockerimage.InspectResponse{
		ID: id, Os: "linux", Config: &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{Labels: labels}},
	}}, nil
}

func (f *builderTestEngine) BuildCancel(ctx context.Context, id string, _ client.BuildCancelOptions) (client.BuildCancelResult, error) {
	f.canceled = append(f.canceled, id)
	_, bounded := ctx.Deadline()
	f.badCleanupContext = f.badCleanupContext || ctx.Err() != nil || !bounded
	return client.BuildCancelResult{}, nil
}

func (f *builderTestEngine) ImageRemove(ctx context.Context, reference string, options client.ImageRemoveOptions) (client.ImageRemoveResult, error) {
	f.removed = append(f.removed, reference)
	_, bounded := ctx.Deadline()
	f.badCleanupContext = f.badCleanupContext || ctx.Err() != nil || !bounded
	f.unsafeRemoval = options.Force || options.PruneChildren
	return client.ImageRemoveResult{}, nil
}

func (f *builderTestEngine) assertCleanup(t *testing.T) {
	t.Helper()
	if len(f.canceled) != 1 || f.canceled[0] != f.options.BuildID || len(f.removed) != 1 || f.removed[0] != f.options.Tags[0] || f.badCleanupContext || f.unsafeRemoval {
		t.Errorf("cleanup = cancel %v, remove %v, bad context %t, unsafe removal %t", f.canceled, f.removed, f.badCleanupContext, f.unsafeRemoval)
	}
}

type builderTestBody struct {
	reader *strings.Reader
	closed atomic.Bool
}

func (b *builderTestBody) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (b *builderTestBody) Close() error               { b.closed.Store(true); return nil }

type builderTestBlockedBody struct {
	io.ReadCloser
	started, closed      chan struct{}
	startOnce, closeOnce sync.Once
}

func (b *builderTestBlockedBody) Read(p []byte) (int, error) {
	b.startOnce.Do(func() { close(b.started) })
	return b.ReadCloser.Read(p)
}
func (b *builderTestBlockedBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return b.ReadCloser.Close()
}

func builderTestInput(t *testing.T) Input {
	t.Helper()
	root := t.TempDir()
	for name, content := range map[string]string{"Dockerfile": "FROM scratch\nCOPY marker.txt /marker.txt\n", "marker.txt": "test source\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Input{CheckoutPath: root, DeploymentID: builderTestDeployment, CommitSHA: strings.Repeat("c", 40)}
}
