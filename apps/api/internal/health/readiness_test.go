package health

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testChecker(t *testing.T, timeout time.Duration) *Checker {
	t.Helper()
	c, err := NewChecker(Config{Timeout: timeout, RequestTimeout: 200 * time.Millisecond, Interval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testTarget(t *testing.T, server *httptest.Server, path string) Target {
	t.Helper()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return Target{HostPort: n, Path: path}
}

func TestCheckerRejectsInvalidConfiguration(t *testing.T) {
	for _, config := range []Config{
		{Timeout: -time.Second}, {RequestTimeout: -time.Second}, {Interval: -time.Second},
		{Timeout: 2*time.Minute + time.Nanosecond}, {RequestTimeout: 10*time.Second + time.Nanosecond},
		{Interval: 10*time.Second + time.Nanosecond},
	} {
		if _, err := NewChecker(config); err == nil {
			t.Errorf("accepted invalid config: %+v", config)
		}
	}
	if _, err := NewChecker(Config{}); err != nil {
		t.Fatalf("default config: %v", err)
	}
}

func TestCheckerRetriesUntilReadyAndPreservesTheSavedPath(t *testing.T) {
	var requests atomic.Int32
	paths := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.RequestURI
		if r.Method != http.MethodGet {
			t.Errorf("probe used %s", r.Method)
		}
		if requests.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	result, err := testChecker(t, time.Second).Wait(t.Context(), testTarget(t, server, "/he%61lth%20check"))
	if err != nil || result.Attempts != 3 || result.StatusCode != http.StatusNoContent || result.CheckedAt.IsZero() || result.CheckedAt.Location() != time.UTC {
		t.Fatalf("readiness result: %+v, %v", result, err)
	}
	for range 3 {
		if got := <-paths; got != "/he%61lth%20check" {
			t.Errorf("saved path changed: %q", got)
		}
	}
}

func TestCheckerOnlyAcceptsSuccessfulHTTPStatus(t *testing.T) {
	for _, status := range []int{200, 204, 299, 300, 404, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			result, err := testChecker(t, 50*time.Millisecond).Wait(t.Context(), testTarget(t, server, "/ready"))
			if result.Attempts < 1 || result.StatusCode != status && (status < 300 || result.StatusCode != 0) {
				t.Fatalf("missing observed HTTP response: %+v", result)
			}
			if status < 300 {
				if err != nil || result.Attempts != 1 {
					t.Fatalf("successful response rejected: %+v, %v", result, err)
				}
			} else if !errors.Is(err, ErrNotReady) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("non-2xx response reported ready: %+v, %v", result, err)
			}
		})
	}
}

func TestCheckerDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer server.Close()
	result, err := testChecker(t, 50*time.Millisecond).Wait(t.Context(), testTarget(t, server, "/ready"))
	if !errors.Is(err, ErrNotReady) || result.StatusCode != http.StatusFound && result.StatusCode != 0 || redirected.Load() != 0 {
		t.Fatalf("redirect escaped the candidate: result=%+v error=%v followed=%d", result, err, redirected.Load())
	}
}

func TestCheckerBoundsStalledRequestsAndOverallWait(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()
	start := time.Now()
	c, err := NewChecker(Config{Timeout: 100 * time.Millisecond, RequestTimeout: 20 * time.Millisecond, Interval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Wait(t.Context(), testTarget(t, server, "/ready"))
	if !errors.Is(err, ErrNotReady) || !errors.Is(err, context.DeadlineExceeded) || result.StatusCode != 0 || result.Attempts < 2 || attempts.Load() < 2 || time.Since(start) > time.Second {
		t.Fatalf("stalled readiness was not bounded/retried: %+v, %v", result, err)
	}
}

func TestCheckerRejectsOversizedResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Oversized", strings.Repeat("a", 32<<10))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	result, err := testChecker(t, 50*time.Millisecond).Wait(t.Context(), testTarget(t, server, "/ready"))
	if !errors.Is(err, ErrNotReady) || result.StatusCode != 0 || result.Attempts < 1 {
		t.Fatalf("oversized HTTP headers accepted: %+v, %v", result, err)
	}
}

func TestCheckerPreservesCallerCancellation(t *testing.T) {
	for _, waiting := range []string{"request", "retry"} {
		t.Run(waiting, func(t *testing.T) {
			entered := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered <- struct{}{}
				if waiting == "request" {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			c, err := NewChecker(Config{Timeout: time.Second, RequestTimeout: time.Second, Interval: 500 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			target := testTarget(t, server, "/ready")
			go func() {
				_, err := c.Wait(ctx, target)
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("probe never reached candidate")
			}
			if waiting == "retry" {
				time.Sleep(20 * time.Millisecond)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotReady) {
					t.Fatalf("caller cancellation changed: %v", err)
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatal("cancellation did not interrupt readiness promptly")
			}
		})
	}
}

func TestCheckerRejectsInvalidTargetsBeforeAnyRequest(t *testing.T) {
	c := testChecker(t, time.Second)
	c.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid target reached the network")
		return nil, errors.New("unexpected request")
	})
	for _, target := range []Target{
		{HostPort: 0, Path: "/ready"}, {HostPort: -1, Path: "/ready"}, {HostPort: 65536, Path: "/ready"},
		{HostPort: 8080, Path: ""}, {HostPort: 8080, Path: "//other.example/ready"},
		{HostPort: 8080, Path: "/ready?next=1"}, {HostPort: 8080, Path: "/%2fother"},
		{HostPort: 8080, Path: "/../ready"}, {HostPort: 8080, Path: "/" + strings.Repeat("a", 2048)},
		{HostPort: 8080, Path: "/\xff"},
	} {
		result, err := c.Wait(t.Context(), target)
		if !errors.Is(err, ErrInvalidTarget) || result != (Result{}) {
			t.Errorf("invalid target was not rejected: %+v => %+v, %v", target, result, err)
		}
	}
}

func TestCheckerPreservesAnEarlierCallerDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err := testChecker(t, time.Second).Wait(ctx, testTarget(t, server, "/ready"))
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNotReady) {
		t.Fatalf("caller deadline became readiness failure: %v", err)
	}
}

func TestCheckerDoesNotRequestWithAnAlreadyCanceledContext(t *testing.T) {
	c := testChecker(t, time.Second)
	c.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("request made after cancellation")
		return nil, errors.New("unexpected request")
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := c.Wait(ctx, Target{HostPort: 8080, Path: "/ready"})
	if !errors.Is(err, context.Canceled) || result != (Result{}) {
		t.Fatalf("pre-canceled readiness: %+v, %v", result, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type probeBody struct{ closed bool }

func (*probeBody) Read([]byte) (int, error) { panic("status-only readiness must not read the body") }
func (b *probeBody) Close() error           { b.closed = true; return nil }

func TestCheckerClosesBodiesWithoutReadingAndCancellationWinsOverSuccess(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		c := testChecker(t, time.Second)
		ctx, cancel := context.WithCancel(t.Context())
		var bodies []*probeBody
		c.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := &probeBody{}
			bodies = append(bodies, body)
			if canceled {
				cancel()
			}
			status := http.StatusOK
			if !canceled && len(bodies) == 1 {
				status = http.StatusServiceUnavailable
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: body, Request: r}, nil
		})
		_, err := c.Wait(ctx, Target{HostPort: 8080, Path: "/ready"})
		cancel()
		for _, body := range bodies {
			if !body.closed {
				t.Fatal("probe did not close a response body")
			}
		}
		if canceled && !errors.Is(err, context.Canceled) || !canceled && err != nil {
			t.Fatalf("canceled=%t error=%v", canceled, err)
		}
	}
}

func TestCheckerUsesItsOwnTransport(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("ambient transport must not be used")
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("readiness sent ambient credentials")
		}
		_, _ = io.WriteString(w, "ready")
	}))
	defer server.Close()
	if _, err := testChecker(t, time.Second).Wait(t.Context(), testTarget(t, server, "/ready")); err != nil {
		t.Fatal(err)
	}
}
