package deployments

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
	mux.HandleFunc("POST /api/projects/{projectID}/deployments", func(w http.ResponseWriter, r *http.Request) {
		createDeployment(w, r, store)
	})
	mux.HandleFunc("GET /api/projects/{projectID}/deployments", func(w http.ResponseWriter, r *http.Request) {
		listDeployments(w, r, store)
	})
	mux.HandleFunc("GET /api/deployments/{id}", func(w http.ResponseWriter, r *http.Request) {
		getDeployment(w, r, store)
	})
	return httpapi.LocalOnly(mux)
}

func createDeployment(w http.ResponseWriter, r *http.Request, store Store) {
	projectID := r.PathValue("projectID")
	if !httpapi.ValidID(projectID) {
		httpapi.WriteError(w, http.StatusBadRequest, "Use a UUID project ID.")
		return
	}
	if r.URL.RawQuery != "" {
		httpapi.WriteError(w, http.StatusBadRequest, "Deployment creation does not accept query parameters.")
		return
	}
	if !validCreateRequest(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	deployment, err := store.Create(ctx, projectID)
	if err != nil {
		writeStoreError(w, "create", err)
		return
	}
	w.Header().Set("Location", "/api/deployments/"+deployment.ID)
	httpapi.WriteJSON(w, http.StatusCreated, deployment)
}

func validCreateRequest(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpapi.WriteError(w, http.StatusRequestEntityTooLarge, "Request body must be at most 64 KiB.")
		} else {
			httpapi.WriteError(w, http.StatusBadRequest, "Could not read deployment request.")
		}
		return false
	}
	if len(body) == 0 {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		httpapi.WriteError(w, http.StatusUnsupportedMediaType, "Use application/json for a request body.")
		return false
	}
	var input *struct{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if !utf8.Valid(body) || decoder.Decode(&input) != nil || input == nil || decoder.Decode(new(any)) != io.EOF {
		httpapi.WriteError(w, http.StatusBadRequest, "Send an empty body or a JSON object with no settings; deployment settings come from the project.")
		return false
	}
	return true
}

func getDeployment(w http.ResponseWriter, r *http.Request, store Store) {
	id := r.PathValue("id")
	if !httpapi.ValidID(id) {
		httpapi.WriteError(w, http.StatusBadRequest, "Use a UUID deployment ID.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	deployment, err := store.Get(ctx, id)
	if err != nil {
		writeStoreError(w, "get", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, deployment)
}

func listDeployments(w http.ResponseWriter, r *http.Request, store Store) {
	projectID := r.PathValue("projectID")
	if !httpapi.ValidID(projectID) {
		httpapi.WriteError(w, http.StatusBadRequest, "Use a UUID project ID.")
		return
	}
	limit, offset, err := httpapi.Pagination(r.URL.RawQuery)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "Use limit from 1 to 100 and a nonnegative offset, each supplied once.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	deployments, err := store.List(ctx, projectID, limit+1, offset)
	if err != nil {
		writeStoreError(w, "list", err)
		return
	}
	var nextOffset *int
	if len(deployments) > limit {
		deployments = deployments[:limit]
		next := offset + limit
		nextOffset = &next
	}
	if deployments == nil {
		deployments = []Deployment{}
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Deployments []Deployment `json:"deployments"`
		NextOffset  *int         `json:"next_offset"`
	}{deployments, nextOffset})
}

func writeStoreError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, ErrProjectNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "Project not found.")
	case errors.Is(err, ErrNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "Deployment not found.")
	case errors.Is(err, ErrAlreadyQueued):
		httpapi.WriteError(w, http.StatusConflict, "This project already has an unfinished deployment.")
	default:
		slog.Error("Deployment storage unavailable", "operation", operation, "error", database.ErrorLogValue(err))
		httpapi.WriteError(w, http.StatusServiceUnavailable, "Deployment storage is unavailable. Try again later.")
	}
}
