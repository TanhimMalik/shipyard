//go:build integration

package health

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/runtime"
)

func TestCheckerProbesManagedContainer(t *testing.T) {
	host := os.Getenv("SHIPYARD_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("set SHIPYARD_TEST_DOCKER_HOST to an explicit disposable Linux Docker socket")
	}
	docker, err := client.New(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	version, err := docker.ServerVersion(ctx, client.ServerVersionOptions{})
	cancel()
	if err != nil || version.Os != "linux" || version.Arch != "amd64" && version.Arch != "arm64" {
		t.Fatalf("fixture needs a Linux arm64/amd64 engine: OS=%s arch=%s error=%v", version.Os, version.Arch, err)
	}
	builder, err := builds.NewBuilder(builds.Config{DockerHost: host, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = builder.Close() })
	manager, err := runtime.NewManager(runtime.Config{DockerHost: host})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\nCOPY app /app\nUSER 65532:65532\nEXPOSE 8080\nENTRYPOINT [\"/app\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app"), readinessIntegrationApp(t, version.Arch), 0755); err != nil {
		t.Fatal(err)
	}
	spec := runtime.Spec{ProjectID: readinessIntegrationUUID(t), DeploymentID: readinessIntegrationUUID(t), InstanceID: readinessIntegrationUUID(t), CommitSHA: strings.Repeat("a", 40), AppPort: 8080}
	input := builds.Input{CheckoutPath: root, DeploymentID: spec.DeploymentID, BuildID: spec.DeploymentID, CommitSHA: spec.CommitSHA}
	image, err := builder.Build(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	spec.ImageID = image.ID
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if _, err := manager.Stop(ctx, spec); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			t.Errorf("stop owned readiness fixture: %v", err)
		}
		if err := manager.Remove(ctx, spec); err != nil {
			t.Errorf("remove owned readiness fixture: %v", err)
			return
		}
		if err := builder.Cleanup(ctx, input, image.ID); err != nil {
			t.Errorf("remove owned readiness image: %v", err)
		}
	})
	created, err := manager.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.ContainerID = created.ID
	started, err := manager.Start(t.Context(), spec)
	if err != nil || !started.Running || started.HostPort < 1 {
		t.Fatalf("start readiness fixture: %+v, %v", started, err)
	}
	checker := readinessIntegrationChecker(t, 5*time.Second)
	result, err := checker.Wait(t.Context(), Target{HostPort: started.HostPort, Path: "/ready"})
	if err != nil || result.StatusCode != http.StatusOK || result.Attempts < 1 || result.CheckedAt.IsZero() {
		t.Fatalf("actual container readiness: %+v, %v", result, err)
	}
	checker = readinessIntegrationChecker(t, 500*time.Millisecond)
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/missing", started.HostPort), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	response, err := checker.client.Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	cancel()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("fixture missing endpoint returned %d", response.StatusCode)
	}
	result, err = checker.Wait(t.Context(), Target{HostPort: started.HostPort, Path: "/missing"})
	if !errors.Is(err, ErrNotReady) || !errors.Is(err, context.DeadlineExceeded) || result.StatusCode != 0 && result.StatusCode != http.StatusNotFound || result.Attempts < 1 {
		t.Fatalf("missing container endpoint became ready: %+v, %v", result, err)
	}
	stopped, err := manager.Stop(t.Context(), spec)
	if err != nil || stopped.Running {
		t.Fatalf("stop readiness fixture: %+v, %v", stopped, err)
	}
	result, err = checker.Wait(t.Context(), Target{HostPort: started.HostPort, Path: "/ready"})
	if !errors.Is(err, ErrNotReady) || result.StatusCode != 0 || result.Attempts < 1 {
		t.Fatalf("stopped container became ready: %+v, %v", result, err)
	}
}

func readinessIntegrationChecker(t *testing.T, timeout time.Duration) *Checker {
	t.Helper()
	checker, err := NewChecker(Config{Timeout: timeout, RequestTimeout: 200 * time.Millisecond, Interval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// Production probes bypass proxies. A VM test needs explicit access to its loopback.
	if proxy := os.Getenv("SHIPYARD_TEST_DOCKER_HTTP_PROXY"); proxy != "" {
		address, err := url.Parse(proxy)
		if err != nil || address.Scheme != "socks5" || address.Hostname() != "127.0.0.1" || address.Port() == "" || address.User != nil || address.Path != "" || address.RawQuery != "" || address.Fragment != "" {
			t.Fatal("SHIPYARD_TEST_DOCKER_HTTP_PROXY must be a loopback socks5://127.0.0.1:port URL")
		}
		checker.client.Transport.(*http.Transport).Proxy = http.ProxyURL(address)
	}
	return checker
}

func readinessIntegrationApp(t *testing.T, arch string) []byte {
	t.Helper()
	source, err := filepath.Abs("../runtime/testdata/app/main.go")
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
		t.Fatalf("compile Linux readiness fixture: %v\n%s", err, output)
	}
	app, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func readinessIntegrationUUID(t *testing.T) string {
	t.Helper()
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatal(err)
	}
	value[6], value[8] = value[6]&0x0f|0x40, value[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[:4], value[4:6], value[6:8], value[8:10], value[10:])
}
