//go:build integration

package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/health"
	"shipyard/apps/api/internal/routing"
	"shipyard/apps/api/internal/runtime"
	"shipyard/apps/api/internal/source"
)

func TestExecutorBuildsAndStartsThroughQueuedAPI(t *testing.T) {
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
	app := executorTestCompileApp(t, version.Arch)
	for _, scenario := range []string{"running candidate survives restart", "missing COPY source", "bad ENTRYPOINT", "readiness timeout"} {
		t.Run(scenario, func(t *testing.T) {
			pool := executorTestDatabase(t)
			store := deployments.NewStore(pool)
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
			root, sha := t.TempDir(), strings.Repeat("d", 40)
			entrypoint := "/app"
			if scenario == "bad ENTRYPOINT" {
				entrypoint = "/missing"
			}
			dockerfile := "FROM scratch\nCOPY app /app\nUSER 65532:65532\nEXPOSE 8080\nENTRYPOINT [\"" + entrypoint + "\"]\n"
			if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
				t.Fatal(err)
			}
			if scenario != "missing COPY source" {
				if err := os.WriteFile(filepath.Join(root, "app"), app, 0755); err != nil {
					t.Fatal(err)
				}
			}
			worker := executorTestWorker(pool, fetchFunc(func(context.Context, string, string) (*source.Checkout, error) {
				return &source.Checkout{Path: root, CommitSHA: sha}, nil
			}))
			worker.builder, worker.runtime = builder, manager
			worker.readiness = executorTestReadiness(t)
			var realRoutes *routing.Manager
			var realVerifier *routing.Verifier
			if scenario == "running candidate survives restart" && os.Getenv("SHIPYARD_TEST_TRAEFIK_BINARY") != "" {
				realRoutes, realVerifier, _ = executorTestTraefik(t)
				worker.routes, worker.verifier = realRoutes, realVerifier
				worker.runtime = executorTestRelayRuntime(t, manager)
			}
			stop := executorTestRun(t, worker)
			api := httptest.NewServer(deployments.NewHandler(store))
			t.Cleanup(api.Close)
			projectID := executorTestProject(t, pool, "https://example.com/startup-fixture.git", "main")
			if scenario == "readiness timeout" {
				if _, err := pool.Exec(t.Context(), `UPDATE projects SET health_path = '/missing' WHERE id = $1`, projectID); err != nil {
					t.Fatal(err)
				}
			}
			queued := executorTestQueue(t, api.URL, projectID)
			input := builds.Input{DeploymentID: queued.ID, CommitSHA: sha, BuildID: queued.ID}
			reference := workerTestImage(queued.ID).Reference
			t.Cleanup(func() {
				if err := stop(); err != nil {
					t.Error(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				saved, err := store.Get(ctx, queued.ID)
				if err == nil && realRoutes != nil {
					if binding, err := realRoutes.Get(ctx, saved.ProjectID); err == nil {
						if err := realRoutes.Remove(ctx, binding); err != nil {
							t.Errorf("remove fixture route: %v", err)
						}
					} else if !errors.Is(err, routing.ErrNotFound) {
						t.Errorf("read fixture route: %v", err)
					}
				}
				if err == nil && saved.InstanceID != nil && saved.ImageID != nil {
					spec := runtime.Spec{ProjectID: saved.ProjectID, DeploymentID: saved.ID, InstanceID: *saved.InstanceID, ImageID: *saved.ImageID, CommitSHA: sha, AppPort: saved.AppPort}
					if instance, err := store.GetInstance(ctx, *saved.InstanceID); err == nil && instance.ContainerID != nil {
						spec.ContainerID = *instance.ContainerID
					}
					if _, err := manager.Stop(ctx, spec); err != nil && !errors.Is(err, runtime.ErrNotFound) {
						t.Errorf("stop fixture: %v", err)
					}
					if err := manager.Remove(ctx, spec); err != nil {
						t.Errorf("remove fixture: %v", err)
					}
				}
				if err := builder.Cleanup(ctx, input, ""); err != nil {
					t.Errorf("remove fixture image: %v", err)
				}
			})
			saved := executorTestWait(t, store, queued.ID, func(d deployments.Deployment) bool {
				return d.Status == deployments.StatusRunning || d.Status == deployments.StatusFailed
			}, 45*time.Second)
			if saved.CommitSHA == nil || *saved.CommitSHA != sha {
				t.Fatalf("build lost source history: %+v", saved)
			}
			if scenario != "running candidate survives restart" {
				saved = executorTestWait(t, store, queued.ID, func(d deployments.Deployment) bool { return d.Status == deployments.StatusFailed }, 15*time.Second)
				stage, reason := deployments.StatusStarting, "Container startup failed."
				if scenario == "missing COPY source" {
					stage, reason = deployments.StatusBuilding, "Image build failed."
				} else if scenario == "readiness timeout" {
					stage, reason = deployments.StatusHealthChecking, "Application did not become ready before the readiness deadline."
				}
				if saved.FailureStage == nil || *saved.FailureStage != stage || saved.FailureReason == nil || *saved.FailureReason != reason {
					t.Fatalf("failed wrong stage or leaked diagnostics: %+v", saved)
				}
				if scenario == "missing COPY source" {
					if saved.ImageID != nil || saved.InstanceID != nil {
						t.Fatal("failed build invented runtime intent")
					}
				} else {
					if saved.ImageID == nil || saved.InstanceID == nil {
						t.Fatal("failed startup lost build/runtime history")
					}
					instance := executorTestWaitInstance(t, store, *saved.InstanceID, func(i deployments.RuntimeInstance) bool { return i.ContainerID != nil })
					if len(*instance.ContainerID) != 64 || instance.Running == nil || *instance.Running || instance.ObservedAt == nil || instance.HostPort != nil {
						t.Fatalf("failed startup invented running state: %+v", instance)
					}
					if _, err := docker.ContainerInspect(t.Context(), *instance.ContainerID, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
						t.Fatalf("failed startup left container: %v", err)
					}
				}
				if scenario == "readiness timeout" {
					if saved.ReadinessPassed == nil || *saved.ReadinessPassed || saved.ReadinessAttempts == nil || *saved.ReadinessAttempts < 1 || saved.ReadinessStatusCode == nil || *saved.ReadinessStatusCode != 404 && *saved.ReadinessStatusCode != 0 || saved.ReadinessCheckedAt == nil {
						t.Fatalf("missing durable failed readiness result: %+v", saved)
					}
				} else if saved.ReadinessPassed != nil {
					t.Fatalf("failure before readiness invented a probe result: %+v", saved)
				}
				if _, err := docker.ImageInspect(t.Context(), reference); !errdefs.IsNotFound(err) {
					t.Fatalf("failed attempt left image: %v", err)
				}
				return
			}
			if saved.ImageReference == nil || *saved.ImageReference != reference || saved.ImageID == nil || saved.InstanceID == nil {
				t.Fatalf("missing build/runtime identity: %+v", saved)
			}
			instance := executorTestWaitInstance(t, store, *saved.InstanceID, func(i deployments.RuntimeInstance) bool { return i.Running != nil && *i.Running })
			if instance.ContainerID == nil || len(*instance.ContainerID) != 64 || instance.ObservedStatus == nil || *instance.ObservedStatus != "running" || instance.ObservedAt == nil || instance.HostPort == nil || *instance.HostPort < 1 {
				t.Fatalf("missing real startup observations: %+v", instance)
			}
			image, err := docker.ImageInspect(t.Context(), reference)
			if err != nil || image.ID != *saved.ImageID || image.Config == nil || image.Config.Labels["io.shipyard.deployment_id"] != queued.ID || image.Config.Labels["io.shipyard.commit_sha"] != sha {
				t.Fatalf("persisted image identity changed: %v", err)
			}
			spec := runtime.Spec{ProjectID: saved.ProjectID, DeploymentID: saved.ID, InstanceID: *saved.InstanceID, ImageID: *saved.ImageID, ContainerID: *instance.ContainerID, CommitSHA: sha, AppPort: saved.AppPort}
			actual, err := manager.Inspect(t.Context(), spec)
			if err != nil || !actual.Running || actual.ID != *instance.ContainerID || actual.HostPort != *instance.HostPort {
				t.Fatalf("persisted runtime differs from Docker: %+v, %v", actual, err)
			}
			if body := executorTestHTTP(t, *instance.HostPort, "/ready"); string(body) != "ok" {
				t.Fatalf("unexpected candidate HTTP: %q", body)
			}
			current, err := store.Get(t.Context(), saved.ID)
			if err != nil || current.Status != deployments.StatusRunning || current.FinishedAt == nil || current.ReadinessPassed == nil || !*current.ReadinessPassed || current.ReadinessStatusCode == nil || *current.ReadinessStatusCode != 200 || current.ReadinessAttempts == nil || *current.ReadinessAttempts < 1 || current.ReadinessCheckedAt == nil {
				t.Fatalf("activation did not complete the verified deployment: %+v, %v", current, err)
			}
			active, err := store.GetActiveRoute(t.Context(), current.ProjectID)
			if err != nil || active.DeploymentID != current.ID || active.ContainerID != *instance.ContainerID || active.HostPort != *instance.HostPort {
				t.Fatalf("actual active binding was not saved: %+v, %v", active, err)
			}
			if realVerifier != nil {
				if err := realVerifier.Ready(t.Context(), routing.Binding(active), current.HealthPath); err != nil {
					t.Fatalf("actual Docker app was not served through Traefik: %v", err)
				}
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			replacement := executorTestWorker(pool, nil)
			replacement.builder, replacement.runtime = builder, manager
			if realRoutes != nil {
				replacement.routes, replacement.verifier = realRoutes, realVerifier
			}
			stopRecovery := executorTestRun(t, replacement)
			recovered, err := store.Get(t.Context(), saved.ID)
			if err != nil || recovered.Status != deployments.StatusRunning || recovered.FailureStage != nil || recovered.ImageID == nil || *recovered.ImageID != *saved.ImageID || recovered.InstanceID == nil || *recovered.InstanceID != *saved.InstanceID {
				t.Fatalf("restart changed a confirmed running app: %+v, %v", recovered, err)
			}
			if err := stopRecovery(); err != nil {
				t.Fatal(err)
			}
			stillRunning, err := manager.Inspect(t.Context(), spec)
			if err != nil || !stillRunning.Running || stillRunning.ID != *instance.ContainerID || stillRunning.HostPort != *instance.HostPort {
				t.Fatalf("restart stopped or replaced the active container: %+v, %v", stillRunning, err)
			}
			if realVerifier != nil {
				if err := realVerifier.Ready(t.Context(), routing.Binding(active), current.HealthPath); err != nil {
					t.Fatalf("restart lost actual routed traffic: %v", err)
				}
			}
		})
	}
}

func executorTestWaitInstance(t *testing.T, store *deployments.PostgresStore, id string, ready func(deployments.RuntimeInstance) bool) deployments.RuntimeInstance {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var instance deployments.RuntimeInstance
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		var err error
		instance, err = store.GetInstance(ctx, id)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if ready(instance) {
			return instance
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("runtime instance did not reach expected observation: %+v", instance)
	return deployments.RuntimeInstance{}
}

func executorTestCompileApp(t *testing.T, arch string) []byte {
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
		t.Fatalf("compile Linux HTTP fixture: %v\n%s", err, output)
	}
	app, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func executorTestHTTP(t *testing.T, port int, path string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil}
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
			err = fmt.Errorf("status=%d read error=%v", response.StatusCode, readErr)
		}
		last = err
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("candidate HTTP on Docker host loopback failed: %v", last)
	return nil
}

func executorTestReadiness(t *testing.T) readinessChecker {
	t.Helper()
	checker, err := health.NewChecker(health.Config{Timeout: 2 * time.Second, RequestTimeout: 500 * time.Millisecond, Interval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	proxy := os.Getenv("SHIPYARD_TEST_DOCKER_HTTP_PROXY")
	if proxy == "" {
		return checker
	}
	address, err := url.Parse(proxy)
	if err != nil || address.Scheme != "socks5" || address.Hostname() != "127.0.0.1" || address.Port() == "" || address.User != nil || address.Path != "" || address.RawQuery != "" || address.Fragment != "" {
		t.Fatal("SHIPYARD_TEST_DOCKER_HTTP_PROXY must be a loopback socks5://127.0.0.1:port URL")
	}
	// The production checker stays proxy-free. This test relay bridges only the
	// known VM loopback endpoint to a temporary local port.
	return probeFunc(func(ctx context.Context, target health.Target) (health.Result, error) {
		backend := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(target.HostPort))}
		transport := &http.Transport{Proxy: http.ProxyURL(address), DisableKeepAlives: true, ResponseHeaderTimeout: 500 * time.Millisecond}
		defer transport.CloseIdleConnections()
		relay := httputil.NewSingleHostReverseProxy(backend)
		relay.Transport = transport
		server := httptest.NewServer(relay)
		defer server.Close()
		_, port, err := net.SplitHostPort(server.Listener.Addr().String())
		if err != nil {
			return health.Result{}, err
		}
		localPort, err := strconv.Atoi(port)
		if err != nil {
			return health.Result{}, err
		}
		return checker.Wait(ctx, health.Target{HostPort: localPort, Path: target.Path})
	})
}
