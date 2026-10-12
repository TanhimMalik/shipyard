package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const httpDetailID = "abcdef12-3456-4789-abcd-123456789abc"
const httpDetailURL = "http://127.0.0.1:8080/api/projects/" + httpDetailID

func TestHTTPGetProject(t *testing.T) {
	want := httpTestProject(httpDetailID)
	for _, id := range []string{httpDetailID, strings.ToUpper(httpDetailID)} {
		t.Run(id, func(t *testing.T) {
			var databaseContext context.Context
			store := &httpDetailStore{get: func(ctx context.Context, gotID string) (Project, error) {
				databaseContext = ctx
				httpTestDatabaseDeadline(t, ctx)
				if !strings.EqualFold(gotID, httpDetailID) {
					t.Errorf("get ID = %q, want %q", gotID, httpDetailID)
				}
				return want, nil
			}}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/projects/"+id, nil))
			httpDetailProjectResponse(t, response, want)
			if store.getCalls != 1 || store.totalCalls() != 1 {
				t.Fatalf("store calls: get = %d, total = %d", store.getCalls, store.totalCalls())
			}
			if databaseContext.Err() != context.Canceled {
				t.Errorf("get database context was not canceled after completion: %v", databaseContext.Err())
			}
		})
	}
}

func TestHTTPPatchUpdatesOnlySuppliedFields(t *testing.T) {
	for _, tt := range []struct {
		name  string
		body  string
		patch PatchInput
	}{
		{"trim supplied strings", `{"name":" Updated ","branch":" release/next "}`, PatchInput{Name: httpDetailPointer("Updated"), Branch: httpDetailPointer("release/next")}},
		{"port alone", `{"app_port":65535}`, PatchInput{AppPort: httpDetailPointer(65535)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := httpTestProject(httpDetailID)
			if tt.patch.Name != nil {
				want.Name = *tt.patch.Name
			}
			if tt.patch.Branch != nil {
				want.Branch = *tt.patch.Branch
			}
			if tt.patch.AppPort != nil {
				want.AppPort = *tt.patch.AppPort
			}
			var databaseContext context.Context
			store := &httpDetailStore{update: func(ctx context.Context, id string, patch PatchInput) (Project, error) {
				databaseContext = ctx
				httpTestDatabaseDeadline(t, ctx)
				if !strings.EqualFold(id, httpDetailID) || !reflect.DeepEqual(patch, tt.patch) {
					t.Errorf("update = (%q, %+v), want (%q, %+v)", id, patch, httpDetailID, tt.patch)
				}
				return want, nil
			}}
			response := httptest.NewRecorder()
			url := httpDetailURL
			if tt.patch.Name != nil {
				url = "http://127.0.0.1:8080/api/projects/" + strings.ToUpper(httpDetailID)
			}
			NewHandler(store).ServeHTTP(response, httpDetailRequest(http.MethodPatch, url, tt.body))
			httpDetailProjectResponse(t, response, want)
			if store.updateCalls != 1 || store.totalCalls() != 1 {
				t.Fatalf("store calls: update = %d, total = %d", store.updateCalls, store.totalCalls())
			}
			if databaseContext.Err() != context.Canceled {
				t.Errorf("update database context was not canceled after completion: %v", databaseContext.Err())
			}
		})
	}
}

func TestHTTPDeleteProject(t *testing.T) {
	var databaseContext context.Context
	store := &httpDetailStore{delete: func(ctx context.Context, id string) error {
		databaseContext = ctx
		httpTestDatabaseDeadline(t, ctx)
		if !strings.EqualFold(id, httpDetailID) {
			t.Errorf("delete ID = %q, want %q", id, httpDetailID)
		}
		return nil
	}}
	req := httpDetailRequest(http.MethodDelete, "http://127.0.0.1:8080/api/projects/"+strings.ToUpper(httpDetailID), "")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	response := httptest.NewRecorder()
	NewHandler(store).ServeHTTP(response, req)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Errorf("delete response = %d %q, want 204 with no body", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("delete headers = %v, want no-store and nosniff", response.Header())
	}
	if store.deleteCalls != 1 || store.totalCalls() != 1 {
		t.Fatalf("store calls: delete = %d, total = %d", store.deleteCalls, store.totalCalls())
	}
	if databaseContext.Err() != context.Canceled {
		t.Errorf("delete database context was not canceled after completion: %v", databaseContext.Err())
	}
}

func TestHTTPDetailRejectsInvalidIDsBeforeStore(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
		for _, id := range []string{"not-a-uuid", strings.ReplaceAll(httpDetailID, "-", "")} {
			t.Run(method+"/"+id, func(t *testing.T) {
				store := &httpDetailStore{}
				response := httptest.NewRecorder()
				NewHandler(store).ServeHTTP(response, httpDetailRequest(method, "http://127.0.0.1:8080/api/projects/"+id, `{"name":"Updated"}`))
				if response.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body.String())
				}
				httpTestErrorJSON(t, response)
				if store.totalCalls() != 0 {
					t.Errorf("invalid ID reached store %d times", store.totalCalls())
				}
			})
		}
	}
}

func TestHTTPRejectsInvalidPatchBeforeStore(t *testing.T) {
	for _, tt := range []struct {
		name        string
		body        string
		contentType string
		status      int
	}{
		{"empty patch", `{}`, "application/json", http.StatusBadRequest},
		{"null supplied field", `{"name":"Updated","app_port":null}`, "application/json", http.StatusBadRequest},
		{"unknown field", `{"name":"Updated","unexpected":true}`, "application/json", http.StatusBadRequest},
		{"trailing object", `{"name":"Updated"} {}`, "application/json", http.StatusBadRequest},
		{"wrong media type", `{"name":"Updated"}`, "text/plain", http.StatusUnsupportedMediaType},
		{"oversized stream", `{"name":"Updated"}` + strings.Repeat(" ", 64*1024), "application/json", http.StatusRequestEntityTooLarge},
		{"invalid supplied field", `{"name":"   "}`, "application/json", http.StatusUnprocessableEntity},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &httpDetailStore{}
			req := httpDetailRequest(http.MethodPatch, httpDetailURL, tt.body)
			req.Header.Set("Content-Type", tt.contentType)
			req.ContentLength = -1
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, req)
			if response.Code != tt.status {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, tt.status, response.Body.String())
			}
			httpTestErrorJSON(t, response)
			if store.totalCalls() != 0 {
				t.Errorf("invalid patch reached store %d times", store.totalCalls())
			}
			if tt.status == http.StatusUnprocessableEntity {
				var body struct {
					Fields map[string]json.RawMessage `json:"fields"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Fields["name"] == nil {
					t.Errorf("expected name validation error: %s; decode error = %v", response.Body.String(), err)
				}
			}
		})
	}
}

func TestHTTPDetailStoreErrors(t *testing.T) {
	privateError := errors.New("private-sentinel: postgres://admin:secret@private-host/database")
	for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
		for _, tt := range []struct {
			name   string
			err    error
			status int
		}{
			{"not found", fmt.Errorf("private-sentinel: %w", ErrNotFound), http.StatusNotFound},
			{"unavailable", privateError, http.StatusServiceUnavailable},
			{"slug conflict", fmt.Errorf("private-sentinel: %w", ErrSlugTaken), http.StatusConflict},
		} {
			if tt.status == http.StatusConflict && method != http.MethodPatch {
				continue
			}
			t.Run(method+"/"+tt.name, func(t *testing.T) {
				store := &httpDetailStore{
					get:    func(context.Context, string) (Project, error) { return Project{}, tt.err },
					update: func(context.Context, string, PatchInput) (Project, error) { return Project{}, tt.err },
					delete: func(context.Context, string) error { return tt.err },
				}
				response := httptest.NewRecorder()
				NewHandler(store).ServeHTTP(response, httpDetailRequest(method, httpDetailURL, `{"name":"Updated"}`))
				if response.Code != tt.status {
					t.Fatalf("status = %d, want %d; body = %s", response.Code, tt.status, response.Body.String())
				}
				httpTestErrorJSON(t, response)
				for _, private := range []string{"private-sentinel", "private-host", "postgres://"} {
					if strings.Contains(response.Body.String(), private) {
						t.Errorf("error response exposes %q", private)
					}
				}
				if store.totalCalls() != 1 {
					t.Errorf("store call count = %d, want 1", store.totalCalls())
				}
			})
		}
	}
}

func TestHTTPDetailDatabaseContextFollowsRequestCancellation(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			type markerKey struct{}
			parent, cancel := context.WithCancel(context.WithValue(context.Background(), markerKey{}, "request marker"))
			defer cancel()
			checkContext := func(ctx context.Context) error {
				httpTestDatabaseDeadline(t, ctx)
				if ctx.Value(markerKey{}) != "request marker" {
					t.Error("database context lost request values")
				}
				cancel()
				if !errors.Is(ctx.Err(), context.Canceled) {
					t.Errorf("database context ignored parent cancellation: %v", ctx.Err())
				}
				return ctx.Err()
			}
			store := &httpDetailStore{
				get: func(ctx context.Context, _ string) (Project, error) { return Project{}, checkContext(ctx) },
				update: func(ctx context.Context, _ string, _ PatchInput) (Project, error) {
					return Project{}, checkContext(ctx)
				},
				delete: func(ctx context.Context, _ string) error { return checkContext(ctx) },
			}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, httpDetailRequest(method, httpDetailURL, `{"name":"Updated"}`).WithContext(parent))
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body = %s", response.Code, response.Body.String())
			}
			httpTestErrorJSON(t, response)
			if store.totalCalls() != 1 {
				t.Errorf("store call count = %d, want 1", store.totalCalls())
			}
		})
	}
}

func TestHTTPProtectsPatchAndDeleteBeforeStore(t *testing.T) {
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		for _, protection := range []string{"external Host", "cross origin", "cross-site fetch"} {
			t.Run(method+"/"+protection, func(t *testing.T) {
				store := &httpDetailStore{}
				req := httpDetailRequest(method, httpDetailURL, `{"name":"Updated"}`)
				switch protection {
				case "external Host":
					req.Host = "rebound.example"
				case "cross origin":
					req.Header.Set("Origin", "http://other.test")
				case "cross-site fetch":
					req.Header.Set("Sec-Fetch-Site", "cross-site")
				}
				response := httptest.NewRecorder()
				NewHandler(store).ServeHTTP(response, req)
				if response.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403; body = %s", response.Code, response.Body.String())
				}
				httpTestErrorJSON(t, response)
				if store.totalCalls() != 0 {
					t.Errorf("unsafe request reached store %d times", store.totalCalls())
				}
			})
		}
	}
}

type httpDetailStore struct {
	httpTestStore
	get         func(context.Context, string) (Project, error)
	update      func(context.Context, string, PatchInput) (Project, error)
	delete      func(context.Context, string) error
	getCalls    int
	updateCalls int
	deleteCalls int
}

func (s *httpDetailStore) Get(ctx context.Context, id string) (Project, error) {
	s.getCalls++
	if s.get == nil {
		return Project{}, errors.New("unexpected get call")
	}
	return s.get(ctx, id)
}

func (s *httpDetailStore) Update(ctx context.Context, id string, patch PatchInput) (Project, error) {
	s.updateCalls++
	if s.update == nil {
		return Project{}, errors.New("unexpected update call")
	}
	return s.update(ctx, id, patch)
}

func (s *httpDetailStore) Delete(ctx context.Context, id string) error {
	s.deleteCalls++
	if s.delete == nil {
		return errors.New("unexpected delete call")
	}
	return s.delete(ctx, id)
}

func (s *httpDetailStore) totalCalls() int {
	return s.createCalls + s.listCalls + s.getCalls + s.updateCalls + s.deleteCalls
}

func httpDetailRequest(method, url, body string) *http.Request {
	req := httptest.NewRequest(method, url, bytes.NewBufferString(body))
	if method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func httpDetailProjectResponse(t *testing.T, response *httptest.ResponseRecorder, want Project) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	httpTestJSONHeaders(t, response)
	var got Project
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode direct project JSON: %v", err)
	}
	if got != want {
		t.Errorf("project = %+v, want %+v", got, want)
	}
}

func httpDetailPointer[T any](value T) *T { return &value }

func TestHTTPDeletePreservesDeploymentHistory(t *testing.T) {
	store := &httpDetailStore{delete: func(context.Context, string) error {
		return fmt.Errorf("private-sentinel: %w", ErrHasDeployments)
	}}
	response := httptest.NewRecorder()
	NewHandler(store).ServeHTTP(response, httpDetailRequest(http.MethodDelete, httpDetailURL, ""))
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", response.Code, response.Body.String())
	}
	httpTestErrorJSON(t, response)
	if !strings.Contains(response.Body.String(), "deployment history") || strings.Contains(response.Body.String(), "private-sentinel") {
		t.Errorf("unexpected conflict message: %s", response.Body.String())
	}
}
