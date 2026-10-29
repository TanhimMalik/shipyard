package routing

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"shipyard/apps/api/internal/health"
)

var ErrNotObserved = errors.New("expected route was not observed before the verification deadline")

type VerifierConfig struct {
	URL            string
	Timeout        time.Duration
	RequestTimeout time.Duration
	Interval       time.Duration
}

type Verifier struct {
	origin         *url.URL
	client         *http.Client
	timeout        time.Duration
	requestTimeout time.Duration
	interval       time.Duration
}

func ValidateProxyURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" || u.Path != "" ||
		u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") {
		return ErrInvalidBinding
	}
	if u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return ErrInvalidBinding
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || u.Host != net.JoinHostPort(u.Hostname(), strconv.Itoa(port)) {
		return ErrInvalidBinding
	}
	return nil
}

func NewVerifier(config VerifierConfig) (*Verifier, error) {
	if err := ValidateProxyURL(config.URL); err != nil {
		return nil, err
	}
	if config.Timeout == 0 {
		config.Timeout = 15 * time.Second
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 2 * time.Second
	}
	if config.Interval == 0 {
		config.Interval = 200 * time.Millisecond
	}
	if config.Timeout < 0 || config.Timeout > 2*time.Minute || config.RequestTimeout < 0 ||
		config.RequestTimeout > 10*time.Second || config.Interval < 0 || config.Interval > 10*time.Second {
		return nil, errors.New("route verification durations must be positive: deadline at most two minutes, request and interval at most ten seconds")
	}
	origin, _ := url.Parse(config.URL)
	transport := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: config.RequestTimeout}).DialContext,
		DisableKeepAlives: true, DisableCompression: true, MaxConnsPerHost: 1,
		MaxResponseHeaderBytes: 16 << 10, ResponseHeaderTimeout: config.RequestTimeout,
	}
	return &Verifier{origin: origin, timeout: config.Timeout, requestTimeout: config.RequestTimeout, interval: config.Interval,
		client: &http.Client{Transport: transport, Timeout: config.RequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Ready observes the exact published binding and a successful app response.
func (v *Verifier) Ready(ctx context.Context, binding Binding, path string) error {
	marker, err := Marker(binding)
	if err != nil || !health.ValidPath(path) {
		return ErrInvalidBinding
	}
	return v.wait(ctx, binding.ProjectID, path, marker, true, false)
}

// Restored observes route selection, independently of the previous app's health.
// A first deployment needs a reachable proxy 404 with no managed route marker.
func (v *Verifier) Restored(ctx context.Context, projectID string, previous *Binding) error {
	if !uuidPattern.MatchString(projectID) {
		return ErrInvalidBinding
	}
	if previous == nil {
		return v.wait(ctx, projectID, "/", "", false, true)
	}
	marker, err := Marker(*previous)
	if err != nil || previous.ProjectID != projectID {
		return ErrInvalidBinding
	}
	return v.wait(ctx, projectID, "/", marker, false, false)
}

func (v *Verifier) wait(parent context.Context, projectID, path, marker string, healthy, absent bool) error {
	host, _ := Hostname(projectID)
	address, _ := url.ParseRequestURI(path)
	address.Scheme, address.Host = v.origin.Scheme, v.origin.Host
	ctx, cancel := context.WithTimeout(parent, v.timeout)
	defer cancel()
	for {
		if ctx.Err() != nil {
			return verificationError(parent, ctx)
		}
		requestCtx, stop := context.WithTimeout(ctx, v.requestTimeout)
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, address.String(), nil)
		if err != nil {
			stop()
			return ErrInvalidBinding
		}
		request.Host = net.JoinHostPort(host, v.origin.Port())
		response, err := v.client.Do(request)
		observed := false
		if response != nil {
			values := response.Header.Values(MarkerHeader)
			if absent {
				observed = response.StatusCode == http.StatusNotFound && len(values) == 0
			} else {
				observed = len(values) == 1 && values[0] == marker && (!healthy || response.StatusCode >= 200 && response.StatusCode < 300)
			}
			_ = response.Body.Close()
		}
		stop()
		if ctx.Err() != nil {
			return verificationError(parent, ctx)
		}
		if err == nil && observed {
			return nil
		}
		timer := time.NewTimer(v.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return verificationError(parent, ctx)
		case <-timer.C:
		}
	}
}

func verificationError(parent, ctx context.Context) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	return errors.Join(ErrNotObserved, ctx.Err())
}
