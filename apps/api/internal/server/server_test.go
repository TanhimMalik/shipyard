package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	response := httptest.NewRecorder()
	New(Config{}, nil, nil).Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["status"] != "ok" {
		t.Errorf("unexpected health response: %v", body)
	}
}

func TestHealthRouting(t *testing.T) {
	tests := []struct {
		method string
		path   string
		status int
	}{
		{http.MethodHead, "/api/health", http.StatusOK},
		{http.MethodPost, "/api/health", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/health/", http.StatusNotFound},
		{http.MethodGet, "/api/unknown", http.StatusNotFound},
	}
	handler := New(Config{}, nil, nil).Handler
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tt.method, tt.path, nil))
			if response.Code != tt.status {
				t.Errorf("status = %d, want %d", response.Code, tt.status)
			}
		})
	}
}

func TestServeDrainsActiveRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	observed := &observedListener{Listener: listener, closed: make(chan struct{})}
	t.Cleanup(func() { _ = observed.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	finishRequest := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(finishRequest)
	httpServer := New(Config{}, nil, nil)
	httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "completed")
	})
	serveResult := make(chan error, 1)
	go func() { serveResult <- Serve(ctx, httpServer, observed) }()

	requestResult := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		defer client.CloseIdleConnections()
		response, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			requestResult <- err
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err == nil && (response.StatusCode != http.StatusOK || string(body) != "completed") {
			err = errors.New("active request did not complete successfully")
		}
		requestResult <- err
	}()

	waitForSignal(t, started)
	cancel()
	waitForSignal(t, observed.closed)
	select {
	case err := <-serveResult:
		t.Fatalf("server returned before active request finished: %v", err)
	default:
	}
	finishRequest()
	waitForResult(t, requestResult)
	waitForResult(t, serveResult)
}

func TestServeReturnsListenerError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Serve(t.Context(), New(Config{}, nil, nil), listener); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected closed-listener error, got %v", err)
	}
}

type observedListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *observedListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func waitForSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for lifecycle signal")
	}
}

func waitForResult(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for lifecycle result")
	}
}
