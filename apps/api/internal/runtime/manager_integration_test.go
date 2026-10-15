//go:build integration

package runtime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"shipyard/apps/api/internal/builds"
)

func TestManagerRuntimeInDocker(t *testing.T) {
	host := os.Getenv("SHIPYARD_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("set SHIPYARD_TEST_DOCKER_HOST to an explicit disposable Linux Docker engine socket")
	}
	docker, err := client.New(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })
	versionCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	version, err := docker.ServerVersion(versionCtx, client.ServerVersionOptions{})
	cancel()
	if err != nil || version.Os != "linux" || (version.Arch != "arm64" && version.Arch != "amd64") {
		t.Fatalf("fixture needs a Linux arm64/amd64 engine: OS=%s arch=%s error=%v", version.Os, version.Arch, err)
	}
	manager, err := NewManager(Config{DockerHost: host, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	builder, err := builds.NewBuilder(builds.Config{DockerHost: host, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = builder.Close() })
	app := runtimeIntegrationCompileApp(t, version.Arch)
	base := "FROM scratch\nCOPY app /app\nUSER 65532:65532\nEXPOSE 8080 9090\nENTRYPOINT [\"/app\"]\nHEALTHCHECK --interval=1s --timeout=1s CMD [\"/app\", \"exit\"]\nSTOPSIGNAL SIGKILL\n"

	t.Run("bounded lifecycle and actual process limits", func(t *testing.T) {
		spec := runtimeIntegrationImage(t, builder, docker, app, base)
		runtimeIntegrationCleanup(t, manager, spec)
		created, err := manager.Create(t.Context(), spec)
		if err != nil || created.ID == "" || created.Name != "shipyard-"+spec.InstanceID || created.ImageID != spec.ImageID || created.Running {
			t.Fatalf("create: instance=%+v error=%v", created, err)
		}
		again, err := manager.Create(t.Context(), spec)
		if err != nil || again.ID != created.ID {
			t.Fatalf("repeated create did not reuse owned container: %+v, %v", again, err)
		}
		started, err := manager.Start(t.Context(), spec)
		if err != nil || started.ID != created.ID || !started.Running || started.HostPort < 1 || started.HostPort > 65535 {
			t.Fatalf("start: instance=%+v error=%v", started, err)
		}
		again, err = manager.Start(t.Context(), spec)
		if err != nil || again.ID != created.ID || !again.Running || again.HostPort != started.HostPort {
			t.Fatalf("repeated start changed owned container: %+v, %v", again, err)
		}
		inspected, err := docker.ContainerInspect(t.Context(), created.ID, client.ContainerInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		runtimeIntegrationAssertPolicy(t, inspected.Container, spec, started.HostPort)
		if body := runtimeIntegrationHTTP(t, started.HostPort, "/ready"); string(body) != "ok" {
			t.Fatalf("unexpected fixture response: %q", body)
		}
		var limits map[string]string
		if err := json.Unmarshal(runtimeIntegrationHTTP(t, started.HostPort, "/limits"), &limits); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{"memory_max": "268435456", "memory_swap_max": "0", "pids_max": "64", "NoNewPrivs": "1"} {
			if limits[key] != want {
				t.Errorf("actual %s=%q, want %q", key, limits[key], want)
			}
		}
		cpu := strings.Fields(limits["cpu_max"])
		if len(cpu) != 2 || cpu[0] == "0" || cpu[0] != cpu[1] {
			t.Errorf("actual cpu.max=%q, want one CPU", limits["cpu_max"])
		}
		uid := strings.Fields(limits["Uid"])
		if len(uid) != 4 || slices.ContainsFunc(uid, func(value string) bool { return value != "65532" }) {
			t.Errorf("fixture image USER was not respected: Uid=%q", limits["Uid"])
		}
		if caps, err := strconv.ParseUint(limits["CapEff"], 16, 64); err != nil || caps != 0 {
			t.Errorf("effective capabilities were not empty: %q (%v)", limits["CapEff"], err)
		}
		stopped, err := manager.Stop(t.Context(), spec)
		if err != nil || stopped.ID != created.ID || stopped.Running || stopped.ExitCode != 0 || stopped.OOMKilled {
			t.Fatalf("graceful stop: instance=%+v error=%v", stopped, err)
		}
		if err := manager.Remove(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Inspect(t.Context(), spec); !errors.Is(err, ErrNotFound) {
			t.Fatalf("inspect removed container: %v", err)
		}
		if err := manager.Remove(t.Context(), spec); err != nil {
			t.Fatalf("repeated remove: %v", err)
		}
	})

	t.Run("real entrypoint failure removes the created container", func(t *testing.T) {
		spec := runtimeIntegrationImage(t, builder, docker, app, strings.Replace(base, `ENTRYPOINT ["/app"]`, `ENTRYPOINT ["/missing"]`, 1))
		runtimeIntegrationCleanup(t, manager, spec)
		if _, err := manager.Create(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
		_, err := manager.Start(t.Context(), spec)
		var runtimeError *RuntimeError
		if !errors.As(err, &runtimeError) {
			t.Fatalf("missing entrypoint did not produce RuntimeError: %v", err)
		}
		if _, err := docker.ContainerInspect(t.Context(), "shipyard-"+spec.InstanceID, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
			t.Fatalf("failed start left an owned container: %v", err)
		}
	})

	t.Run("image volumes are rejected before container creation", func(t *testing.T) {
		spec := runtimeIntegrationImage(t, builder, docker, app, base+"VOLUME [\"/data\"]\n")
		runtimeIntegrationCleanup(t, manager, spec)
		if _, err := manager.Create(t.Context(), spec); !errors.Is(err, ErrUnsupportedImage) {
			t.Fatalf("VOLUME image was not rejected: %v", err)
		}
		if _, err := docker.ContainerInspect(t.Context(), "shipyard-"+spec.InstanceID, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
			t.Fatalf("rejected image left a container: %v", err)
		}
	})
}

func runtimeIntegrationCompileApp(t *testing.T, arch string) []byte {
	t.Helper()
	source, err := filepath.Abs("testdata/app/main.go")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	output := filepath.Join(root, "app")
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	goBinary := filepath.Join(goruntime.GOROOT(), "bin", "go")
	if goruntime.GOOS == "windows" {
		goBinary += ".exe"
	}
	cmd := exec.CommandContext(ctx, goBinary, "build", "-trimpath", "-o", output, source)
	cmd.Dir = root
	controlled := map[string]string{"GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0", "GOTOOLCHAIN": "local", "GOWORK": "off", "GOFLAGS": "", "GOPROXY": "off", "GOSUMDB": "off", "GO111MODULE": "off", "GOENV": "off", "GOAMD64": "v1", "GOARM64": "v8.0"}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := controlled[key]; !replaced {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	for key, value := range controlled {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile pull-free Linux fixture: %v\n%s", err, output)
	}
	app, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func runtimeIntegrationImage(t *testing.T, builder *builds.Builder, docker *client.Client, app []byte, dockerfile string) Spec {
	t.Helper()
	spec := Spec{ProjectID: runtimeIntegrationUUID(t), DeploymentID: runtimeIntegrationUUID(t), InstanceID: runtimeIntegrationUUID(t), CommitSHA: strings.Repeat("a", 40), AppPort: 8080}
	root := t.TempDir()
	for name, data := range map[string][]byte{"Dockerfile": []byte(dockerfile), "app": app} {
		mode := os.FileMode(0600)
		if name == "app" {
			mode = 0755
		}
		if err := os.WriteFile(filepath.Join(root, name), data, mode); err != nil {
			t.Fatal(err)
		}
	}
	image, err := builder.Build(t.Context(), builds.Input{CheckoutPath: root, DeploymentID: spec.DeploymentID, CommitSHA: spec.CommitSHA})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := docker.ImageRemove(ctx, image.Reference, client.ImageRemoveOptions{PruneChildren: false}); err != nil && !errdefs.IsNotFound(err) {
			t.Errorf("remove owned fixture image %s: %v", image.Reference, err)
		}
	})
	spec.ImageID = image.ID
	return spec
}

func runtimeIntegrationCleanup(t *testing.T, manager *Manager, spec Spec) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := manager.Stop(ctx, spec); err != nil && !errors.Is(err, ErrNotFound) {
			t.Errorf("stop owned fixture instance %s: %v", spec.InstanceID, err)
		}
		if err := manager.Remove(ctx, spec); err != nil {
			t.Errorf("remove owned fixture instance %s: %v", spec.InstanceID, err)
		}
	})
}

func runtimeIntegrationAssertPolicy(t *testing.T, got container.InspectResponse, spec Spec, hostPort int) {
	t.Helper()
	host := got.HostConfig
	if host == nil || got.Config == nil || got.NetworkSettings == nil {
		t.Fatal("container inspect omitted required policy fields")
	}
	if got.Image != spec.ImageID || got.Config.User != "65532:65532" || len(got.Mounts) != 0 || len(host.Binds)+len(host.Mounts)+len(host.VolumesFrom)+len(host.Tmpfs) != 0 {
		t.Error("container did not preserve the exact image/USER or unexpectedly mounted storage")
	}
	if host.NanoCPUs != 1_000_000_000 || host.Memory != 256<<20 || host.MemorySwap != 256<<20 || host.PidsLimit == nil || *host.PidsLimit != 64 {
		t.Errorf("unexpected resource limits: %+v", host.Resources)
	}
	nnp := slices.Equal(host.SecurityOpt, []string{"no-new-privileges=true"})
	if len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" || len(host.CapAdd) != 0 || !nnp || host.Privileged || host.NetworkMode != "bridge" || host.PublishAllPorts || host.AutoRemove || host.RestartPolicy.Name != container.RestartPolicyDisabled {
		t.Error("container security/network/lifecycle policy differed from the fixed policy")
	}
	if host.LogConfig.Type != "local" || host.LogConfig.Config["max-size"] != "10m" || host.LogConfig.Config["max-file"] != "3" || got.Config.Healthcheck == nil || !slices.Equal(got.Config.Healthcheck.Test, []string{"NONE"}) || got.Config.StopSignal != "SIGTERM" || got.Config.StopTimeout == nil || *got.Config.StopTimeout != 5 {
		t.Error("container logging/healthcheck/stop policy differed from the fixed policy")
	}
	port := network.MustParsePort("8080/tcp")
	bindings := 0
	for exposed, values := range got.NetworkSettings.Ports {
		bindings += len(values)
		for _, value := range values {
			if exposed != port || value.HostIP != netip.MustParseAddr("127.0.0.1") || value.HostPort != strconv.Itoa(hostPort) {
				t.Errorf("unexpected published binding: %v %+v", exposed, value)
			}
		}
	}
	if bindings != 1 || len(got.NetworkSettings.Ports[network.MustParsePort("9090/tcp")]) != 0 {
		t.Errorf("expected only the declared app port published: %+v", got.NetworkSettings.Ports)
	}
}

func runtimeIntegrationHTTP(t *testing.T, port int, path string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil}
	// A Docker VM may need a temporary SSH SOCKS5 tunnel to its own loopback.
	if proxy := os.Getenv("SHIPYARD_TEST_DOCKER_HTTP_PROXY"); proxy != "" {
		address, err := url.Parse(proxy)
		if err != nil || address.Scheme != "socks5" || address.Hostname() != "127.0.0.1" || address.Port() == "" || address.User != nil || address.Path != "" || address.RawQuery != "" || address.Fragment != "" {
			t.Fatal("SHIPYARD_TEST_DOCKER_HTTP_PROXY must be a loopback socks5://127.0.0.1:port URL")
		}
		transport.Proxy = http.ProxyURL(address)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var last error
	for ctx.Err() == nil {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK && len(body) <= 4096 {
				return body
			}
			err = fmt.Errorf("status=%d body=%q read error=%v", response.StatusCode, body, readErr)
		}
		last = err
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("fixture HTTP on Docker host loopback failed after five seconds: %v", last)
	return nil
}

func runtimeIntegrationUUID(t *testing.T) string {
	t.Helper()
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatal(err)
	}
	value[6], value[8] = value[6]&0x0f|0x40, value[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[:4], value[4:6], value[6:8], value[8:10], value[10:])
}
