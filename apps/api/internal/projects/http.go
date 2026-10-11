package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func NewHandler(store Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/projects", func(w http.ResponseWriter, r *http.Request) {
		createProject(w, r, store)
	})
	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		listProjects(w, r, store)
	})
	protection := http.NewCrossOriginProtection()
	protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "Cross-origin project requests are not allowed.")
	}))
	protected := protection.Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Loopback binding alone does not protect against DNS rebinding.
		if !localHost(r.Host) {
			writeError(w, http.StatusForbidden, "Project requests must use a loopback host.")
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

func createProject(w http.ResponseWriter, r *http.Request, store Store) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Use application/json.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "Request body must be at most 64 KiB.")
		} else {
			writeError(w, http.StatusBadRequest, "Could not read request body.")
		}
		return
	}
	var input *CreateInput
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if !utf8.Valid(body) || decoder.Decode(&input) != nil || input == nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "Provide one JSON object containing project settings.")
		return
	}
	normalized, fields := Validate(*input)
	if len(fields) != 0 {
		writeJSON(w, http.StatusUnprocessableEntity, struct {
			Error  string            `json:"error"`
			Fields map[string]string `json:"fields"`
		}{"Check the project settings.", fields})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	project, err := store.Create(ctx, normalized)
	if errors.Is(err, ErrSlugTaken) {
		writeError(w, http.StatusConflict, "A project with this slug already exists.")
		return
	}
	if err != nil {
		slog.Error("Project storage unavailable", "operation", "create")
		writeError(w, http.StatusServiceUnavailable, "Project storage is unavailable. Try again later.")
		return
	}
	writeJSON(w, http.StatusCreated, project)
}

func listProjects(w http.ResponseWriter, r *http.Request, store Store) {
	limit, offset, err := pagination(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Use limit from 1 to 100 and a nonnegative offset, each supplied once.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	projects, err := store.List(ctx, limit+1, offset)
	if err != nil {
		slog.Error("Project storage unavailable", "operation", "list")
		writeError(w, http.StatusServiceUnavailable, "Project storage is unavailable. Try again later.")
		return
	}
	var nextOffset *int
	if len(projects) > limit {
		projects = projects[:limit]
		next := offset + limit
		nextOffset = &next
	}
	if projects == nil {
		projects = []Project{}
	}
	writeJSON(w, http.StatusOK, struct {
		Projects   []Project `json:"projects"`
		NextOffset *int      `json:"next_offset"`
	}{projects, nextOffset})
}

func pagination(rawQuery string) (int, int, error) {
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

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Debug("Could not write project response")
	}
}
