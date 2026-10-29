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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/health"
	"shipyard/apps/api/internal/routing"
	"shipyard/apps/api/internal/runtime"
	"shipyard/apps/api/internal/source"
)

func TestWorkerPublishesAndRestoresRealTraefikRoutes(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "first failed route", true: "failed replacement preserves old route"}[replacement], func(t *testing.T) {
			manager, verifier, _ := executorTestTraefik(t)
			pool := executorTestDatabase(t)
			store := deployments.NewStore(pool)
			oldApp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "old") }))
			defer oldApp.Close()
			newApp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/ready" && strings.HasPrefix(r.Host, "127.0.0.1:") {
					_, _ = io.WriteString(w, "new")
					return
				}
				http.Error(w, "not ready under the project hostname", http.StatusServiceUnavailable)
			}))
			defer newApp.Close()
			oldPort, newPort := executorTestPort(t, oldApp.Listener.Addr().String()), executorTestPort(t, newApp.Listener.Addr().String())
			worker := executorTestWorker(pool, fetchFunc(func(context.Context, string, string) (*source.Checkout, error) {
				return &source.Checkout{CommitSHA: strings.Repeat("a", 40)}, nil
			}))
			worker.routes, worker.verifier = manager, verifier
			checks, err := health.NewChecker(health.Config{Timeout: time.Second, RequestTimeout: 200 * time.Millisecond, Interval: 20 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			worker.readiness = checks
			states := make(map[string]runtime.Instance)
			var mu sync.Mutex
			ordinal := 0
			worker.runtime = testRuntime{
				create: func(_ context.Context, spec runtime.Spec) (runtime.Instance, error) {
					mu.Lock()
					defer mu.Unlock()
					ordinal++
					id := strings.Repeat("b", 64)
					if replacement && ordinal == 1 {
						id = strings.Repeat("a", 64)
					}
					got := runtime.Instance{ID: id, Name: "shipyard-" + spec.InstanceID, ImageID: spec.ImageID, Status: "created"}
					states[spec.InstanceID] = got
					return got, nil
				},
				start: func(_ context.Context, spec runtime.Spec) (runtime.Instance, error) {
					mu.Lock()
					defer mu.Unlock()
					got := states[spec.InstanceID]
					got.Status, got.Running, got.HostPort = "running", true, newPort
					if got.ID == strings.Repeat("a", 64) {
						got.HostPort = oldPort
					}
					states[spec.InstanceID] = got
					return got, nil
				},
				inspect: func(_ context.Context, spec runtime.Spec) (runtime.Instance, error) {
					mu.Lock()
					defer mu.Unlock()
					got, found := states[spec.InstanceID]
					if !found {
						return runtime.Instance{}, runtime.ErrNotFound
					}
					return got, nil
				},
				stop: func(_ context.Context, spec runtime.Spec) (runtime.Instance, error) {
					mu.Lock()
					defer mu.Unlock()
					got := states[spec.InstanceID]
					got.Status, got.Running, got.HostPort = "exited", false, 0
					states[spec.InstanceID] = got
					return got, nil
				},
				remove: func(_ context.Context, spec runtime.Spec) error {
					mu.Lock()
					defer mu.Unlock()
					delete(states, spec.InstanceID)
					return nil
				},
			}
			stop := executorTestRun(t, worker)
			api := httptest.NewServer(deployments.NewHandler(store))
			defer api.Close()
			projectID := executorTestProject(t, pool, "https://example.com/route-fixture.git", "main")
			var old deployments.Deployment
			if replacement {
				queued := executorTestQueue(t, api.URL, projectID)
				old = executorTestWait(t, store, queued.ID, func(d deployments.Deployment) bool { return d.Status == deployments.StatusRunning })
			}
			queued := executorTestQueue(t, api.URL, projectID)
			failed := executorTestWait(t, store, queued.ID, func(d deployments.Deployment) bool { return d.Status == deployments.StatusFailed }, 10*time.Second)
			if failed.FailureStage == nil || *failed.FailureStage != deployments.StatusActivating || failed.ReadinessPassed == nil || !*failed.ReadinessPassed {
				t.Fatalf("routed failure did not retain passed direct readiness: %+v", failed)
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			if replacement {
				binding, err := store.GetActiveRoute(t.Context(), projectID)
				if err != nil || binding.DeploymentID != old.ID {
					t.Fatalf("lost old active binding: %+v, %v", binding, err)
				}
				if err := verifier.Ready(t.Context(), routing.Binding(binding), "/ready"); err != nil {
					t.Fatalf("old traffic was not restored: %v", err)
				}
				current, err := store.Get(t.Context(), old.ID)
				if err != nil || current.Status != deployments.StatusRunning {
					t.Fatalf("failed replacement superseded old deployment: %+v, %v", current, err)
				}
			} else {
				if _, err := store.GetActiveRoute(t.Context(), projectID); !errors.Is(err, deployments.ErrActiveRouteNotFound) {
					t.Fatalf("failed first route became active: %v", err)
				}
				if err := verifier.Restored(t.Context(), projectID, nil); err != nil {
					t.Fatalf("failed first route remained selected: %v", err)
				}
			}
			instance, err := store.GetInstance(t.Context(), *failed.InstanceID)
			if err != nil || instance.Running == nil || *instance.Running {
				t.Fatalf("failed candidate was not cleaned: %+v, %v", instance, err)
			}
		})
	}
}

func executorTestPort(t *testing.T, address string) int {
	t.Helper()
	_, value, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func executorTestTraefik(t *testing.T) (*routing.Manager, *routing.Verifier, string) {
	t.Helper()
	binary := os.Getenv("SHIPYARD_TEST_TRAEFIK_BINARY")
	if binary == "" {
		t.Skip("set SHIPYARD_TEST_TRAEFIK_BINARY for actual worker routing checks")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("Traefik test binary must be absolute")
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	manager, err := routing.NewManager(routing.Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	config := filepath.Join(t.TempDir(), "traefik.toml")
	data := fmt.Sprintf("[entryPoints.web]\naddress = %q\n[providers]\nprovidersThrottleDuration = \"50ms\"\n[providers.file]\ndirectory = %q\nwatch = true\n[global]\ncheckNewVersion = false\nsendAnonymousUsage = false\n[log]\nlevel = \"ERROR\"\n", address, directory)
	if err := os.WriteFile(config, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, binary, "--configFile="+config)
	command.Dir = filepath.Dir(config)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "TRAEFIK_") || strings.HasSuffix(strings.ToUpper(key), "_PROXY") {
			continue
		}
		command.Env = append(command.Env, entry)
	}
	logs := &executorProxyLog{}
	command.Stdout, command.Stderr = logs, logs
	command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
	command.WaitDelay = 3 * time.Second
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
	})
	verifier, err := routing.NewVerifier(routing.VerifierConfig{URL: "http://" + address, Timeout: time.Second, RequestTimeout: 200 * time.Millisecond, Interval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Restored(t.Context(), "00000000-0000-4000-8000-000000000001", nil); err != nil {
		t.Fatalf("Traefik did not start: %v\n%s", err, logs.String())
	}
	return manager, verifier, "http://" + address
}

type executorProxyLog struct {
	mu   sync.Mutex
	data []byte
}

func (l *executorProxyLog) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := (64 << 10) - len(l.data); n > 0 {
		l.data = append(l.data, data[:min(n, len(data))]...)
	}
	return len(data), nil
}
func (l *executorProxyLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return string(l.data) }

type executorRelayRuntime struct {
	containerRuntime
	start func(context.Context, runtime.Spec) (runtime.Instance, error)
}

func (r executorRelayRuntime) Start(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
	return r.start(ctx, spec)
}

func executorTestRelayRuntime(t *testing.T, manager containerRuntime) containerRuntime {
	t.Helper()
	proxy := os.Getenv("SHIPYARD_TEST_DOCKER_HTTP_PROXY")
	if proxy == "" {
		return manager
	}
	address, err := url.Parse(proxy)
	if err != nil || address.Scheme != "socks5" || address.Hostname() != "127.0.0.1" || address.Port() == "" || address.User != nil || address.Path != "" || address.RawQuery != "" || address.Fragment != "" {
		t.Fatal("invalid explicit Docker loopback test proxy")
	}
	return executorRelayRuntime{containerRuntime: manager, start: func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
		started, err := manager.Start(ctx, spec)
		if err != nil {
			return started, err
		}
		endpoint := net.JoinHostPort("127.0.0.1", strconv.Itoa(started.HostPort))
		listener, err := net.Listen("tcp4", endpoint)
		if err != nil {
			return started, fmt.Errorf("bind test-only relay at Docker's observed port %s: %w", endpoint, err)
		}
		transport := &http.Transport{Proxy: http.ProxyURL(address), DisableKeepAlives: true, ResponseHeaderTimeout: time.Second}
		relay := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: endpoint})
		relay.Transport = transport
		server := httptest.NewUnstartedServer(relay)
		_ = server.Listener.Close()
		server.Listener = listener
		server.Start()
		t.Cleanup(func() { server.Close(); transport.CloseIdleConnections() })
		return started, nil
	}}
}
