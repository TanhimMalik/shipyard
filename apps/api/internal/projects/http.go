package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"
	"unicode/utf8"

	"shipyard/apps/api/internal/database"
	"shipyard/apps/api/internal/httpapi"
)

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
	return httpapi.LocalOnly(mux)
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
	httpapi.WriteJSON(w, http.StatusCreated, project)
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
	httpapi.WriteJSON(w, http.StatusOK, project)
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
		httpapi.WriteError(w, http.StatusBadRequest, "Provide at least one project setting to update.")
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
	httpapi.WriteJSON(w, http.StatusOK, project)
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
	httpapi.SetResponseHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

func requestProjectID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !httpapi.ValidID(id) {
		httpapi.WriteError(w, http.StatusBadRequest, "Use a UUID project ID.")
		return "", false
	}
	return id, true
}

func decodeInput[T any](w http.ResponseWriter, r *http.Request) (*T, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		httpapi.WriteError(w, http.StatusUnsupportedMediaType, "Use application/json.")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpapi.WriteError(w, http.StatusRequestEntityTooLarge, "Request body must be at most 64 KiB.")
		} else {
			httpapi.WriteError(w, http.StatusBadRequest, "Could not read request body.")
		}
		return nil, false
	}
	var input *T
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if !utf8.Valid(body) || decoder.Decode(&input) != nil || input == nil || decoder.Decode(new(any)) != io.EOF {
		httpapi.WriteError(w, http.StatusBadRequest, "Provide one JSON object containing project settings.")
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "Provide one JSON object containing project settings.")
		return nil, false
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			httpapi.WriteError(w, http.StatusBadRequest, "Project settings cannot be null.")
			return nil, false
		}
	}
	return input, true
}

func listProjects(w http.ResponseWriter, r *http.Request, store Store) {
	limit, offset, err := httpapi.Pagination(r.URL.RawQuery)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "Use limit from 1 to 100 and a nonnegative offset, each supplied once.")
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
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Projects   []Project `json:"projects"`
		NextOffset *int      `json:"next_offset"`
	}{projects, nextOffset})
}

func writeValidationError(w http.ResponseWriter, fields map[string]string) {
	httpapi.WriteJSON(w, http.StatusUnprocessableEntity, struct {
		Error  string            `json:"error"`
		Fields map[string]string `json:"fields"`
	}{"Check the project settings.", fields})
}

func writeStoreError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "Project not found.")
	case errors.Is(err, ErrHasDeployments):
		httpapi.WriteError(w, http.StatusConflict, "Projects with deployment history cannot be deleted.")
	case errors.Is(err, ErrSlugTaken):
		httpapi.WriteError(w, http.StatusConflict, "A project with this slug already exists.")
	default:
		slog.Error("Project storage unavailable", "operation", operation, "error", database.ErrorLogValue(err))
		httpapi.WriteError(w, http.StatusServiceUnavailable, "Project storage is unavailable. Try again later.")
	}
}
