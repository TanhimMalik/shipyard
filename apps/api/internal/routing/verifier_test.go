package routing

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func verifierTestBinding() Binding {
	return Binding{ProjectID: "3b8e17ad-4eb9-4c27-86a6-8cc5f932ad41", DeploymentID: "695f0db2-74a2-4d19-8d3f-362a5397018c",
		InstanceID: "c5d34a49-8586-419e-8ea1-fc0c8a346d72", ContainerID: strings.Repeat("c", 64), HostPort: 32123}
}

func verifierTestClient(t *testing.T, address string, timeout time.Duration) *Verifier {
	t.Helper()
	verifier, err := NewVerifier(VerifierConfig{URL: address, Timeout: timeout, RequestTimeout: 40 * time.Millisecond, Interval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func TestVerifierRequiresAnExplicitLoopbackProxy(t *testing.T) {
	for _, address := range []string{"http://127.0.0.1:8180", "http://[::1]:8180"} {
		if err := ValidateProxyURL(address); err != nil {
			t.Errorf("rejected proxy %s: %v", address, err)
		}
	}
	for _, address := range []string{"", "/relative", "https://127.0.0.1:8180", "http://localhost:8180", "http://127.0.0.2:8180",
		"http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://127.0.0.1:08180",
		"http://user@127.0.0.1:8180", "http://127.0.0.1:8180/", "http://127.0.0.1:8180?", "http://127.0.0.1:8180#"} {
		if err := ValidateProxyURL(address); !errors.Is(err, ErrInvalidBinding) {
			t.Errorf("accepted proxy %q: %v", address, err)
		}
	}
	for _, config := range []VerifierConfig{{Timeout: -time.Second}, {RequestTimeout: -time.Second}, {Interval: -time.Second},
		{Timeout: 2*time.Minute + time.Nanosecond}, {RequestTimeout: 10*time.Second + time.Nanosecond}, {Interval: 10*time.Second + time.Nanosecond}} {
		config.URL = "http://127.0.0.1:8180"
		if _, err := NewVerifier(config); err == nil {
			t.Errorf("accepted unbounded verification config: %+v", config)
		}
	}
}

func TestVerifierWaitsForExactTargetAndPreservesSavedPath(t *testing.T) {
	binding := verifierTestBinding()
	marker, _ := Marker(binding)
	host, _ := Hostname(binding.ProjectID)
	var expectedHost string
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != expectedHost || r.Method != http.MethodGet || r.RequestURI != "/he%61lth%20check" {
			t.Errorf("unexpected routed probe: %s %s %s", r.Method, r.Host, r.RequestURI)
		}
		w.Header().Set(MarkerHeader, "stale")
		if calls.Add(1) >= 3 {
			w.Header().Set(MarkerHeader, marker)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	verifier := verifierTestClient(t, server.URL, time.Second)
	expectedHost = net.JoinHostPort(host, verifier.origin.Port())
	if err := verifier.Ready(t.Context(), binding, "/he%61lth%20check"); err != nil || calls.Load() != 3 {
		t.Fatalf("exact target was not observed: calls=%d error=%v", calls.Load(), err)
	}
}

func TestVerifierDistinguishesHealthSelectionAndAbsence(t *testing.T) {
	binding := verifierTestBinding()
	marker, _ := Marker(binding)
	for _, scenario := range []struct {
		name    string
		status  int
		markers []string
		mode    string
		want    bool
	}{
		{"ready", 200, []string{marker}, "ready", true},
		{"no marker", 200, nil, "ready", false},
		{"wrong marker", 200, []string{"old"}, "ready", false},
		{"duplicate marker", 200, []string{marker, "old"}, "ready", false},
		{"app missing", 404, []string{marker}, "ready", false},
		{"app unhealthy", 503, []string{marker}, "ready", false},
		{"old app unhealthy", 503, []string{marker}, "restored", true},
		{"old backend missing", 502, []string{marker}, "restored", true},
		{"no route", 404, nil, "absent", true},
		{"app404 is not absence", 404, []string{marker}, "absent", false},
		{"empty marker is not absence", 404, []string{""}, "absent", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for _, value := range scenario.markers {
					w.Header().Add(MarkerHeader, value)
				}
				w.WriteHeader(scenario.status)
			}))
			defer server.Close()
			verifier := verifierTestClient(t, server.URL, 60*time.Millisecond)
			var err error
			switch scenario.mode {
			case "ready":
				err = verifier.Ready(t.Context(), binding, "/ready")
			case "restored":
				err = verifier.Restored(t.Context(), binding.ProjectID, &binding)
			case "absent":
				err = verifier.Restored(t.Context(), binding.ProjectID, nil)
			}
			if scenario.want && err != nil || !scenario.want && (!errors.Is(err, ErrNotObserved) || !errors.Is(err, context.DeadlineExceeded)) {
				t.Fatalf("route observation=%v, want success=%t", err, scenario.want)
			}
		})
	}
}

func TestVerifierRejectsRedirectsAndAmbientTransport(t *testing.T) {
	binding := verifierTestBinding()
	marker, _ := Marker(binding)
	var followed atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		w.Header().Set(MarkerHeader, marker)
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer server.Close()
	original := http.DefaultTransport
	http.DefaultTransport = verifierRoundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("ambient transport used") })
	t.Cleanup(func() { http.DefaultTransport = original })
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	if err := verifierTestClient(t, server.URL, 60*time.Millisecond).Ready(t.Context(), binding, "/ready"); !errors.Is(err, ErrNotObserved) || followed.Load() != 0 {
		t.Fatalf("redirect followed or verification succeeded: %v, followed=%d", err, followed.Load())
	}
	if err := verifierTestClient(t, destination.URL, time.Second).Ready(t.Context(), binding, "/ready"); err != nil {
		t.Fatalf("private transport did not reach configured proxy: %v", err)
	}
}

func TestVerifierHonorsRequestAndCallerDeadlines(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	binding := verifierTestBinding()
	verifier := verifierTestClient(t, server.URL, 150*time.Millisecond)
	if err := verifier.Ready(t.Context(), binding, "/ready"); !errors.Is(err, ErrNotObserved) || calls.Load() < 2 {
		t.Fatalf("stalled proxy requests were not bounded/retried: %v, calls=%d", err, calls.Load())
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := verifier.Ready(ctx, binding, "/ready"); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNotObserved) {
		t.Fatalf("caller deadline changed: %v", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	for len(entered) > 0 {
		<-entered
	}
	done := make(chan error, 1)
	go func() { done <- verifier.Ready(ctx, binding, "/ready") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotObserved) {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("cancellation did not interrupt verification")
	}
}

func TestVerifierRejectsOversizedHeadersAndUnreachableAbsence(t *testing.T) {
	binding := verifierTestBinding()
	marker, _ := Marker(binding)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Large", strings.Repeat("x", 32<<10))
		w.Header().Set(MarkerHeader, marker)
	}))
	defer server.Close()
	if err := verifierTestClient(t, server.URL, 60*time.Millisecond).Ready(t.Context(), binding, "/ready"); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("oversized headers were accepted: %v", err)
	}
	verifier := verifierTestClient(t, "http://127.0.0.1:8180", 40*time.Millisecond)
	verifier.client.Transport = verifierRoundTrip(func(*http.Request) (*http.Response, error) { return nil, io.EOF })
	if err := verifier.Restored(t.Context(), binding.ProjectID, nil); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("unreachable proxy proved route absence: %v", err)
	}
}

type verifierRoundTrip func(*http.Request) (*http.Response, error)

func (f verifierRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type verifierBody struct{ closed bool }

func (*verifierBody) Read([]byte) (int, error) { panic("route verification must not read bodies") }
func (b *verifierBody) Close() error           { b.closed = true; return nil }

func TestVerifierValidatesBeforeRequestsAndClosesBodies(t *testing.T) {
	verifier := verifierTestClient(t, "http://127.0.0.1:8180", time.Second)
	verifier.client.Transport = verifierRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid target reached network")
		return nil, io.EOF
	})
	binding := verifierTestBinding()
	if err := verifier.Ready(t.Context(), binding, "//other/ready"); !errors.Is(err, ErrInvalidBinding) {
		t.Fatal(err)
	}
	if err := verifier.Ready(t.Context(), Binding{}, "/ready"); !errors.Is(err, ErrInvalidBinding) {
		t.Fatal(err)
	}
	if err := verifier.Restored(t.Context(), "bad", nil); !errors.Is(err, ErrInvalidBinding) {
		t.Fatal(err)
	}
	other := binding
	other.ProjectID = other.DeploymentID
	if err := verifier.Restored(t.Context(), binding.ProjectID, &other); !errors.Is(err, ErrInvalidBinding) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := verifier.Ready(ctx, binding, "/ready"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		body := &verifierBody{}
		marker, _ := Marker(binding)
		verifier.client.Transport = verifierRoundTrip(func(r *http.Request) (*http.Response, error) {
			if canceled {
				cancel()
			}
			return &http.Response{StatusCode: 200, Header: http.Header{MarkerHeader: []string{marker}}, Body: body, Request: r}, nil
		})
		err := verifier.Ready(ctx, binding, "/ready")
		cancel()
		if !body.closed || canceled && !errors.Is(err, context.Canceled) || !canceled && err != nil {
			t.Fatalf("body closed=%t canceled=%t error=%v", body.closed, canceled, err)
		}
	}
}
