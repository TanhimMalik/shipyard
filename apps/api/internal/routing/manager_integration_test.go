//go:build integration

package routing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagerRoutesThroughTraefik(t *testing.T) {
	if goruntime.GOOS != "darwin" && goruntime.GOOS != "linux" {
		t.Skip("native Traefik routing requires macOS or Linux")
	}
	binary := os.Getenv("SHIPYARD_TEST_TRAEFIK_BINARY")
	if binary == "" {
		t.Skip("set SHIPYARD_TEST_TRAEFIK_BINARY to an explicit native Traefik executable")
	}
	info, err := os.Stat(binary)
	if err != nil || !filepath.IsAbs(binary) || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("Traefik binary must be an absolute executable regular file: %v", err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	backend := func(marker string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(MarkerHeader, "app-supplied-marker")
			if r.URL.Path == "/missing" {
				w.WriteHeader(http.StatusNotFound)
			}
			_, _ = io.WriteString(w, marker+" "+r.URL.RequestURI())
		}))
		t.Cleanup(server.Close)
		return server
	}
	oldBackend, newBackend, otherBackend := backend("old"), backend("new"), backend("other")
	old := Binding{
		ProjectID: "00000000-0000-4000-8000-000000000071", DeploymentID: "00000000-0000-4000-8000-000000000072",
		InstanceID: "00000000-0000-4000-8000-000000000073", ContainerID: strings.Repeat("a", 64), HostPort: routingIntegrationPort(t, oldBackend.Listener.Addr().String()),
	}
	next := old
	next.DeploymentID, next.InstanceID, next.ContainerID, next.HostPort = "00000000-0000-4000-8000-000000000074", "00000000-0000-4000-8000-000000000075", strings.Repeat("b", 64), routingIntegrationPort(t, newBackend.Listener.Addr().String())
	other := Binding{
		ProjectID: "00000000-0000-4000-8000-000000000081", DeploymentID: "00000000-0000-4000-8000-000000000082",
		InstanceID: "00000000-0000-4000-8000-000000000083", ContainerID: strings.Repeat("c", 64), HostPort: routingIntegrationPort(t, otherBackend.Listener.Addr().String()),
	}
	host, err := Hostname(old.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	otherHost, err := Hostname(other.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	proxy := routingIntegrationStart(t, binary, directory, address)
	proxy.waitResponse(t, address, host, http.StatusNotFound, "")
	verifier, err := NewVerifier(VerifierConfig{URL: "http://" + address, Timeout: 3 * time.Second, RequestTimeout: 200 * time.Millisecond, Interval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Restored(t.Context(), old.ProjectID, nil); err != nil {
		t.Fatalf("empty Traefik route was not observed: %v", err)
	}
	if err := manager.Put(t.Context(), old, nil); err != nil {
		t.Fatal(err)
	}
	proxy.waitResponse(t, address, host, http.StatusOK, "old /app?from=route")
	if err := verifier.Ready(t.Context(), old, "/ready"); err != nil {
		t.Fatalf("proxy did not identify the full old binding: %v", err)
	}
	marker, _ := Marker(old)
	routingIntegrationAssertMarker(t, address, host, "/missing", http.StatusNotFound, marker)
	oldBackend.Close()
	proxy.waitResponse(t, address, host, http.StatusBadGateway, "")
	routingIntegrationAssertMarker(t, address, host, "/", http.StatusBadGateway, marker)
	if err := verifier.Restored(t.Context(), old.ProjectID, &old); err != nil {
		t.Fatalf("old route selection was confused with backend health: %v", err)
	}
	shortVerifier, err := NewVerifier(VerifierConfig{URL: "http://" + address, Timeout: 100 * time.Millisecond, RequestTimeout: 50 * time.Millisecond, Interval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := shortVerifier.Ready(t.Context(), old, "/ready"); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("unhealthy old backend was reported ready: %v", err)
	}
	if err := shortVerifier.Restored(t.Context(), old.ProjectID, nil); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("a matched broken route was reported absent: %v", err)
	}
	proxy.waitResponse(t, address, host+".other", http.StatusNotFound, "")
	if err := manager.Put(t.Context(), other, nil); err != nil {
		t.Fatal(err)
	}
	proxy.waitResponse(t, address, otherHost, http.StatusOK, "other /app?from=route")
	if err := manager.Put(t.Context(), next, &old); err != nil {
		t.Fatal(err)
	}
	proxy.waitResponse(t, address, host, http.StatusOK, "new /app?from=route")
	if err := verifier.Ready(t.Context(), next, "/ready"); err != nil {
		t.Fatalf("proxy did not identify the replacement binding: %v", err)
	}
	if err := manager.Remove(t.Context(), old); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale cleanup accepted: %v", err)
	}
	proxy.waitResponse(t, address, host, http.StatusOK, "new /app?from=route")
	proxy.waitResponse(t, address, otherHost, http.StatusOK, "other /app?from=route")
	proxy.stop(t)
	proxy = routingIntegrationStart(t, binary, directory, address)
	proxy.waitResponse(t, address, host, http.StatusOK, "new /app?from=route")
	proxy.waitResponse(t, address, otherHost, http.StatusOK, "other /app?from=route")
	if err := manager.Remove(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	proxy.waitResponse(t, address, host, http.StatusNotFound, "")
	if err := verifier.Restored(t.Context(), next.ProjectID, nil); err != nil {
		t.Fatalf("removed route was not observed: %v", err)
	}
	proxy.waitResponse(t, address, otherHost, http.StatusOK, "other /app?from=route")
}

func routingIntegrationAssertMarker(t *testing.T, address, host, path string, status int, marker string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = host
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	values := response.Header.Values(MarkerHeader)
	if response.StatusCode != status || len(values) != 1 || values[0] != marker {
		t.Fatalf("Traefik did not override app marker on status%d: status=%d marker=%v", status, response.StatusCode, values)
	}
}

func routingIntegrationPort(t *testing.T, address string) int {
	t.Helper()
	_, raw, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

type routingIntegrationProcess struct {
	command *exec.Cmd
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
	logs    routingIntegrationLog
}

func routingIntegrationStart(t *testing.T, binary, directory, address string) *routingIntegrationProcess {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	process := &routingIntegrationProcess{cancel: cancel, done: make(chan struct{})}
	root := t.TempDir()
	config := fmt.Sprintf(`[global]
  checkNewVersion = false
  sendAnonymousUsage = false
[entryPoints.web]
  address = %q
[providers]
  providersThrottleDuration = "100ms"
[providers.file]
  directory = %q
  watch = true
[log]
  level = "ERROR"
`, address, directory)
	configPath := filepath.Join(root, "traefik.toml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		cancel()
		t.Fatal(err)
	}
	process.command = exec.CommandContext(ctx, binary, "--configFile="+configPath)
	process.command.Dir = root
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "TRAEFIK_") || upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" || upper == "NO_PROXY" {
			continue
		}
		process.command.Env = append(process.command.Env, entry)
	}
	process.command.Stdout, process.command.Stderr = &process.logs, &process.logs
	process.command.Cancel = func() error { return process.command.Process.Signal(os.Interrupt) }
	process.command.WaitDelay = 3 * time.Second
	if err := process.command.Start(); err != nil {
		cancel()
		t.Fatalf("start native Traefik: %v", err)
	}
	go func() {
		process.err = process.command.Wait()
		close(process.done)
	}()
	t.Cleanup(func() { process.stop(t) })
	return process
}

func (process *routingIntegrationProcess) stop(t *testing.T) {
	t.Helper()
	process.cancel()
	select {
	case <-process.done:
	case <-time.After(5 * time.Second):
		_ = process.command.Process.Kill()
		select {
		case <-process.done:
		case <-time.After(2 * time.Second):
			t.Error("owned Traefik process did not exit after kill")
		}
	}
}

func (process *routingIntegrationProcess) waitResponse(t *testing.T, address, host string, status int, body string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var last string
	for ctx.Err() == nil {
		select {
		case <-process.done:
			t.Fatalf("Traefik exited before routing settled: %v\n%s", process.err, process.logs.String())
		default:
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/app?from=route", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = host
		response, err := client.Do(request)
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
			_ = response.Body.Close()
			if readErr == nil && len(data) <= 4096 && response.StatusCode == status && (body == "" || string(data) == body) {
				return
			}
			last = fmt.Sprintf("status=%d body=%q error=%v", response.StatusCode, data, readErr)
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
		case <-time.After(25 * time.Millisecond):
		}
	}
	t.Fatalf("Traefik did not serve host %q with status=%d body=%q: %s\n%s", host, status, body, last, process.logs.String())
}

type routingIntegrationLog struct {
	mu   sync.Mutex
	data []byte
}

func (log *routingIntegrationLog) Write(data []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	if remaining := (64 << 10) - len(log.data); remaining > 0 {
		log.data = append(log.data, data[:min(len(data), remaining)]...)
	}
	return len(data), nil
}

func (log *routingIntegrationLog) String() string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return string(log.data)
}
