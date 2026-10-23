package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

var (
	ErrInvalidTarget = errors.New("invalid readiness target")
	ErrNotReady      = errors.New("application did not become ready before the readiness deadline")
)

type Config struct {
	Timeout        time.Duration
	RequestTimeout time.Duration
	Interval       time.Duration
}

// HostPort must come from a freshly inspected, owned Docker container. The
// checker probes the Docker host's loopback; it does not inspect container state.
type Target struct {
	HostPort int
	Path     string
}

// StatusCode describes the last attempt, or zero if no response was received.
// A successful check is a point-in-time HTTP observation, not route activation.
type Result struct {
	Attempts   int
	StatusCode int
	CheckedAt  time.Time
}

type Checker struct {
	client         *http.Client
	timeout        time.Duration
	requestTimeout time.Duration
	interval       time.Duration
}

func NewChecker(config Config) (*Checker, error) {
	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 2 * time.Second
	}
	if config.Interval == 0 {
		config.Interval = 500 * time.Millisecond
	}
	if config.Timeout < 0 || config.Timeout > 2*time.Minute ||
		config.RequestTimeout < 0 || config.RequestTimeout > 10*time.Second ||
		config.Interval < 0 || config.Interval > 10*time.Second {
		return nil, errors.New("readiness durations must be positive: deadline at most two minutes, request and interval at most ten seconds")
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: config.RequestTimeout}).DialContext,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxConnsPerHost:        1,
		MaxResponseHeaderBytes: 16 << 10,
		ResponseHeaderTimeout:  config.RequestTimeout,
	}
	return &Checker{
		client: &http.Client{
			Transport: transport,
			Timeout:   config.RequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		timeout: config.Timeout, requestTimeout: config.RequestTimeout, interval: config.Interval,
	}, nil
}

// Wait retries GET until it receives 2xx headers or its deadline expires. Bodies
// are closed without reading; redirects, cookies, and environment proxies are unused.
func (c *Checker) Wait(parent context.Context, target Target) (Result, error) {
	if target.HostPort < 1 || target.HostPort > 65535 || !ValidPath(target.Path) {
		return Result{}, ErrInvalidTarget
	}
	address, _ := url.ParseRequestURI(target.Path)
	address.Scheme = "http"
	address.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(target.HostPort))
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	var result Result
	for {
		if ctx.Err() != nil {
			return result, waitError(parent, ctx)
		}
		requestCtx, stop := context.WithTimeout(ctx, c.requestTimeout)
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, address.String(), nil)
		if err != nil {
			stop()
			return result, ErrInvalidTarget
		}
		result.Attempts++
		result.StatusCode = 0
		response, err := c.client.Do(request)
		if response != nil {
			result.StatusCode = response.StatusCode
			_ = response.Body.Close()
		}
		stop()
		result.CheckedAt = time.Now().UTC()
		if ctx.Err() != nil {
			return result, waitError(parent, ctx)
		}
		if err == nil && result.StatusCode >= 200 && result.StatusCode < 300 {
			return result, nil
		}
		timer := time.NewTimer(c.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, waitError(parent, ctx)
		case <-timer.C:
		}
	}
}

func waitError(parent, ctx context.Context) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	return errors.Join(ErrNotReady, ctx.Err())
}
