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
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var projectIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func NewHandler(store Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/projects", func(w http.ResponseWriter, r *http.Request) {
		createProject(w, r, store)
	})
	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		listProjects(w, r, store)
	})
	mux.HandleFunc("GET /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		getProject(w, r, store)
	})
	mux.HandleFunc("PATCH /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		patchProject(w, r, store)
	})
	mux.HandleFunc("DELETE /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		deleteProject(w, r, store)
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
	input, ok := decodeInput[CreateInput](w, r)
	if !ok {
		return
	}
	normalized, fields := Validate(*input)
	if len(fields) != 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	project, err := store.Create(ctx, normalized)
	if err != nil {
		writeStoreError(w, "create", err)
		return
	}
	writeJSON(w, http.StatusCreated, project)
}

func getProject(w http.ResponseWriter, r *http.Request, store Store) {
	id, ok := requestProjectID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	project, err := store.Get(ctx, id)
	if err != nil {
		writeStoreError(w, "get", err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func patchProject(w http.ResponseWriter, r *http.Request, store Store) {
	id, ok := requestProjectID(w, r)
	if !ok {
		return
	}
	input, ok := decodeInput[PatchInput](w, r)
	if !ok {
		return
	}
	if input.empty() {
		writeError(w, http.StatusBadRequest, "Provide at least one project setting to update.")
		return
	}
	normalized, fields := ValidatePatch(*input)
	if len(fields) != 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	project, err := store.Update(ctx, id, normalized)
	if err != nil {
		writeStoreError(w, "update", err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func deleteProject(w http.ResponseWriter, r *http.Request, store Store) {
	id, ok := requestProjectID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := store.Delete(ctx, id); err != nil {
		writeStoreError(w, "delete", err)
		return
	}
	setResponseHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

func requestProjectID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !projectIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "Use a UUID project ID.")
		return "", false
	}
	return id, true
}

func decodeInput[T any](w http.ResponseWriter, r *http.Request) (*T, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Use application/json.")
		return nil, false
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
		return nil, false
	}
	var input *T
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if !utf8.Valid(body) || decoder.Decode(&input) != nil || input == nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "Provide one JSON object containing project settings.")
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		writeError(w, http.StatusBadRequest, "Provide one JSON object containing project settings.")
		return nil, false
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			writeError(w, http.StatusBadRequest, "Project settings cannot be null.")
			return nil, false
		}
	}
	return input, true
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
		writeStoreError(w, "list", err)
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

func writeValidationError(w http.ResponseWriter, fields map[string]string) {
	writeJSON(w, http.StatusUnprocessableEntity, struct {
		Error  string            `json:"error"`
		Fields map[string]string `json:"fields"`
	}{"Check the project settings.", fields})
}

func writeStoreError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "Project not found.")
	case errors.Is(err, ErrSlugTaken):
		writeError(w, http.StatusConflict, "A project with this slug already exists.")
	default:
		slog.Error("Project storage unavailable", "operation", operation)
		writeError(w, http.StatusServiceUnavailable, "Project storage is unavailable. Try again later.")
	}
}

func setResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	setResponseHeaders(w)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Debug("Could not write project response")
	}
}
