package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var idPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

func LocalOnly(handler http.Handler) http.Handler {
	protection := http.NewCrossOriginProtection()
	protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, http.StatusForbidden, "Cross-origin API requests are not allowed.")
	}))
	protected := protection.Handler(handler)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Loopback binding alone does not protect against DNS rebinding.
		if !localHost(r.Host) {
			WriteError(w, http.StatusForbidden, "API requests must use a loopback host.")
			return
		}
		protected.ServeHTTP(w, r)
	})
}

func localHost(authority string) bool {
	host := authority
	if parsed, _, err := net.SplitHostPort(authority); err == nil {
		host = parsed
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address, err := netip.ParseAddr(strings.TrimPrefix(strings.TrimSuffix(host, "]"), "["))
	return err == nil && address.Unmap().IsLoopback()
}

func Pagination(rawQuery string) (int, int, error) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return 0, 0, err
	}
	limit, offset := 50, 0
	for key, values := range query {
		if (key != "limit" && key != "offset") || len(values) != 1 {
			return 0, 0, errors.New("unknown or repeated pagination parameter")
		}
		value, err := strconv.Atoi(values[0])
		if err != nil {
			return 0, 0, err
		}
		if key == "limit" {
			limit = value
		} else {
			offset = value
		}
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > math.MaxInt-limit {
		return 0, 0, errors.New("pagination out of range")
	}
	return limit, offset, nil
}

func SetResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, struct {
		Error string `json:"error"`
	}{message})
}

func WriteJSON(w http.ResponseWriter, status int, value any) {
	SetResponseHeaders(w)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Debug("Could not write API response")
	}
}
