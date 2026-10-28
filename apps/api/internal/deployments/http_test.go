package deployments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const httpTestProjectID = "abcdef12-3456-4789-abcd-123456789abc"
const httpTestDeploymentID = "fedcba98-7654-4321-abcd-987654321abc"
const httpTestCollectionURL = "http://127.0.0.1:8080/api/projects/" + httpTestProjectID + "/deployments"
const httpTestDetailURL = "http://127.0.0.1:8080/api/deployments/" + httpTestDeploymentID

func TestHTTPCreateDeployment(t *testing.T) {
	want := httpTestDeployment(httpTestDeploymentID)
	for _, tt := range []struct {
		name, body, contentType, origin, projectID string
	}{
		{"no body", "", "", "", httpTestProjectID},
		{"empty JSON body", "", "application/json", "", httpTestProjectID},
		{"empty object and same origin", "{}", "application/json; charset=utf-8", "http://127.0.0.1:8080", httpTestProjectID},
		{"uppercase UUID", " {} \n", "application/json", "", strings.ToUpper(httpTestProjectID)},
		{"exact body limit", "{}" + strings.Repeat(" ", 64*1024-2), "application/json", "", httpTestProjectID},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &httpTestStore{create: func(ctx context.Context, projectID string) (Deployment, error) {
				httpTestDeadline(t, ctx)
				if !strings.EqualFold(projectID, httpTestProjectID) {
					t.Errorf("project ID = %q, want %q", projectID, httpTestProjectID)
				}
				return want, nil
			}}
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/projects/"+tt.projectID+"/deployments", strings.NewReader(tt.body))
			req.ContentLength = -1
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, req)
			httpTestDeploymentResponse(t, response, http.StatusCreated, want)
			if got := response.Header().Get("Location"); got != "/api/deployments/"+want.ID {
				t.Errorf("Location = %q, want deployment detail path", got)
			}
			if store.createCalls != 1 || store.totalCalls() != 1 {
				t.Errorf("store calls: create = %d, total = %d", store.createCalls, store.totalCalls())
			}
		})
	}
}

func TestHTTPRejectsCreateBeforeStore(t *testing.T) {
	for _, tt := range []struct {
		name        string
		body        []byte
		contentType string
		query       string
		status      int
	}{
		{"missing media type for object", []byte("{}"), "", "", http.StatusUnsupportedMediaType},
		{"wrong media type", []byte("{}"), "text/plain", "", http.StatusUnsupportedMediaType},
		{"malformed media type", []byte("{}"), "application/json; charset", "", http.StatusUnsupportedMediaType},
		{"malformed JSON", []byte("{"), "application/json", "", http.StatusBadRequest},
		{"whitespace without object", []byte(" \n "), "application/json", "", http.StatusBadRequest},
		{"null JSON", []byte("null"), "application/json", "", http.StatusBadRequest},
		{"array JSON", []byte("[]"), "application/json", "", http.StatusBadRequest},
		{"scalar JSON", []byte("true"), "application/json", "", http.StatusBadRequest},
		{"unknown field", []byte(`{"unexpected":true}`), "application/json", "", http.StatusBadRequest},
		{"null field", []byte(`{"branch":null}`), "application/json", "", http.StatusBadRequest},
		{"repository override", []byte(`{"git_url":"https://other.test/repo"}`), "application/json", "", http.StatusBadRequest},
		{"branch override", []byte(`{"branch":"release/next"}`), "application/json", "", http.StatusBadRequest},
		{"port override", []byte(`{"app_port":9090}`), "application/json", "", http.StatusBadRequest},
		{"health path override", []byte(`{"health_path":"/other"}`), "application/json", "", http.StatusBadRequest},
		{"status override", []byte(`{"status":"RUNNING"}`), "application/json", "", http.StatusBadRequest},
		{"project override", []byte(`{"project_id":"` + httpTestDeploymentID + `"}`), "application/json", "", http.StatusBadRequest},
		{"trailing object", []byte("{} {}"), "application/json", "", http.StatusBadRequest},
		{"trailing junk", []byte("{} x"), "application/json", "", http.StatusBadRequest},
		{"invalid UTF-8", append([]byte(`{"unexpected":"`), 0xff, '"', '}'), "application/json", "", http.StatusBadRequest},
		{"oversized stream", []byte("{}" + strings.Repeat(" ", 64*1024-1)), "application/json", "", http.StatusRequestEntityTooLarge},
		{"pagination query on POST", nil, "", "?limit=1", http.StatusBadRequest},
		{"unknown query on POST", nil, "", "?unexpected=true", http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &httpTestStore{}
			req := httptest.NewRequest(http.MethodPost, httpTestCollectionURL+tt.query, bytes.NewReader(tt.body))
			req.ContentLength = -1
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, req)
			httpTestErrorResponse(t, response, tt.status)
			if store.totalCalls() != 0 {
				t.Errorf("invalid request reached store %d times", store.totalCalls())
			}
		})
	}
}

func TestHTTPListDeployments(t *testing.T) {
	first, second, third := httpTestDeployment("first"), httpTestDeployment("second"), httpTestDeployment("third")
	for _, tt := range []struct {
		name, query string
		rows        []Deployment
		limit       int
		offset      int
		wantIDs     []string
		nextOffset  *int
	}{
		{"existing project with no deployments", "", nil, 51, 0, []string{}, nil},
		{"minimum page with lookahead", "?limit=1&offset=3", []Deployment{first, second}, 2, 3, []string{"first"}, httpTestIntPointer(4)},
		{"exact page has no next", "?limit=2&offset=5", []Deployment{first, second}, 3, 5, []string{"first", "second"}, nil},
		{"extra row is withheld", "?limit=2&offset=5", []Deployment{third, first, second}, 3, 5, []string{"third", "first"}, httpTestIntPointer(7)},
		{"maximum page size", "?limit=100&offset=0", []Deployment{first}, 101, 0, []string{"first"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &httpTestStore{list: func(ctx context.Context, projectID string, limit, offset int) ([]Deployment, error) {
				httpTestDeadline(t, ctx)
				if projectID != httpTestProjectID || limit != tt.limit || offset != tt.offset {
					t.Errorf("list arguments = (%q, %d, %d), want (%q, %d, %d)", projectID, limit, offset, httpTestProjectID, tt.limit, tt.offset)
				}
				return tt.rows, nil
			}}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpTestCollectionURL+tt.query, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
			}
			httpTestJSONHeaders(t, response)
			var body struct {
				Deployments []Deployment `json:"deployments"`
				NextOffset  *int         `json:"next_offset"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Deployments == nil {
				t.Error("deployments must encode as an array, including empty results")
			}
			ids := make([]string, 0, len(body.Deployments))
			for _, deployment := range body.Deployments {
				ids = append(ids, deployment.ID)
			}
			if !slices.Equal(ids, tt.wantIDs) {
				t.Errorf("deployment IDs = %v, want %v", ids, tt.wantIDs)
			}
			if (body.NextOffset == nil) != (tt.nextOffset == nil) || (body.NextOffset != nil && *body.NextOffset != *tt.nextOffset) {
				t.Errorf("next_offset = %v, want %v", body.NextOffset, tt.nextOffset)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 2 || fields["deployments"] == nil || fields["next_offset"] == nil {
				t.Errorf("unexpected list fields: %s", response.Body.String())
			}
			if store.listCalls != 1 || store.totalCalls() != 1 {
				t.Errorf("store calls: list = %d, total = %d", store.listCalls, store.totalCalls())
			}
		})
	}
}

func TestHTTPRejectsInvalidPaginationBeforeStore(t *testing.T) {
	for _, query := range []string{
		"limit=0", "limit=101", "limit=1.5", "limit=", "limit=999999999999999999999999999",
		"offset=-1", "offset=invalid", "offset=", "offset=999999999999999999999999999",
		fmt.Sprintf("limit=2&offset=%d", int(^uint(0)>>1)-1),
		"limit=2&limit=3", "offset=0&offset=1", "unexpected=true", "limit=%", "limit=2;offset=0",
	} {
		t.Run(query, func(t *testing.T) {
			store := &httpTestStore{}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpTestCollectionURL+"?"+query, nil))
			httpTestErrorResponse(t, response, http.StatusBadRequest)
			if store.totalCalls() != 0 {
				t.Errorf("invalid pagination reached store %d times", store.totalCalls())
			}
		})
	}
}

func TestHTTPGetDeployment(t *testing.T) {
	want := httpTestDeployment(httpTestDeploymentID)
	for _, id := range []string{httpTestDeploymentID, strings.ToUpper(httpTestDeploymentID)} {
		t.Run(id, func(t *testing.T) {
			store := &httpTestStore{get: func(ctx context.Context, gotID string) (Deployment, error) {
				httpTestDeadline(t, ctx)
				if !strings.EqualFold(gotID, httpTestDeploymentID) {
					t.Errorf("deployment ID = %q, want %q", gotID, httpTestDeploymentID)
				}
				return want, nil
			}}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/deployments/"+id, nil))
			httpTestDeploymentResponse(t, response, http.StatusOK, want)
			if store.getCalls != 1 || store.totalCalls() != 1 {
				t.Errorf("store calls: get = %d, total = %d", store.getCalls, store.totalCalls())
			}
		})
	}
}

func TestHTTPGetDeploymentExecutionMetadata(t *testing.T) {
	want := httpTestDeployment(httpTestDeploymentID)
	commit, stage, reason := strings.Repeat("a", 40), StatusFetchingSource, "The repository branch could not be fetched."
	started, finished := want.CreatedAt.Add(time.Second), want.CreatedAt.Add(2*time.Second)
	want.Status, want.CommitSHA = StatusFailed, &commit
	want.FailureStage, want.FailureReason = &stage, &reason
	want.StartedAt, want.FinishedAt, want.UpdatedAt = &started, &finished, finished
	store := &httpTestStore{get: func(context.Context, string) (Deployment, error) { return want, nil }}
	response := httptest.NewRecorder()
	NewHandler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpTestDetailURL, nil))
	httpTestDeploymentResponse(t, response, http.StatusOK, want)
}

func TestHTTPGetDeploymentReadinessObservation(t *testing.T) {
	want := httpTestDeployment(httpTestDeploymentID)
	passed, attempts, status := false, 4, 503
	checked := want.CreatedAt.Add(time.Second)
	want.Status = StatusHealthChecking
	want.ReadinessPassed, want.ReadinessAttempts = &passed, &attempts
	want.ReadinessStatusCode, want.ReadinessCheckedAt = &status, &checked
	store := &httpTestStore{get: func(context.Context, string) (Deployment, error) { return want, nil }}
	response := httptest.NewRecorder()
	NewHandler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpTestDetailURL, nil))
	httpTestDeploymentResponse(t, response, http.StatusOK, want)
}

func TestHTTPRejectsInvalidIDsBeforeStore(t *testing.T) {
	for _, id := range []string{"not-a-uuid", strings.ReplaceAll(httpTestProjectID, "-", ""), "abcdef12-3456-4789-abcd-123456789abg"} {
		for _, route := range []struct{ method, path string }{
			{http.MethodPost, "/api/projects/" + id + "/deployments"},
			{http.MethodGet, "/api/projects/" + id + "/deployments"},
			{http.MethodGet, "/api/deployments/" + id},
		} {
			t.Run(route.method+route.path, func(t *testing.T) {
				store := &httpTestStore{}
				response := httptest.NewRecorder()
				NewHandler(store).ServeHTTP(response, httptest.NewRequest(route.method, "http://127.0.0.1:8080"+route.path, nil))
				httpTestErrorResponse(t, response, http.StatusBadRequest)
				if store.totalCalls() != 0 {
					t.Errorf("invalid ID reached store %d times", store.totalCalls())
				}
			})
		}
	}
}

func TestHTTPStoreErrorsAreSanitized(t *testing.T) {
	privateError := errors.New("private-sentinel: postgres://admin:secret@private-host/database SQL internals")
	for _, tt := range []struct {
		name, method, url string
		err               error
		status            int
	}{
		{"create unavailable", http.MethodPost, httpTestCollectionURL, privateError, http.StatusServiceUnavailable},
		{"list unavailable", http.MethodGet, httpTestCollectionURL, privateError, http.StatusServiceUnavailable},
		{"get unavailable", http.MethodGet, httpTestDetailURL, privateError, http.StatusServiceUnavailable},
		{"create missing project", http.MethodPost, httpTestCollectionURL, fmt.Errorf("private-sentinel: %w", ErrProjectNotFound), http.StatusNotFound},
		{"list missing project", http.MethodGet, httpTestCollectionURL, fmt.Errorf("private-sentinel: %w", ErrProjectNotFound), http.StatusNotFound},
		{"missing deployment", http.MethodGet, httpTestDetailURL, fmt.Errorf("private-sentinel: %w", ErrNotFound), http.StatusNotFound},
		{"already queued", http.MethodPost, httpTestCollectionURL, fmt.Errorf("private-sentinel: %w", ErrAlreadyQueued), http.StatusConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &httpTestStore{
				create: func(context.Context, string) (Deployment, error) { return Deployment{}, tt.err },
				list:   func(context.Context, string, int, int) ([]Deployment, error) { return nil, tt.err },
				get:    func(context.Context, string) (Deployment, error) { return Deployment{}, tt.err },
			}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, httptest.NewRequest(tt.method, tt.url, nil))
			httpTestErrorResponse(t, response, tt.status)
			for _, private := range []string{"private-sentinel", "private-host", "postgres://", "SQL internals", "secret"} {
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

func TestHTTPDatabaseContextFollowsRequest(t *testing.T) {
	for _, route := range []struct {
		name, method, url string
		status            int
	}{
		{"create", http.MethodPost, httpTestCollectionURL, http.StatusCreated},
		{"list", http.MethodGet, httpTestCollectionURL, http.StatusOK},
		{"get", http.MethodGet, httpTestDetailURL, http.StatusOK},
	} {
		for _, mode := range []string{"success", "parent cancellation", "earlier parent deadline"} {
			t.Run(route.name+"/"+mode, func(t *testing.T) {
				type markerKey struct{}
				parent := context.WithValue(context.Background(), markerKey{}, "request marker")
				var cancel context.CancelFunc
				if mode == "earlier parent deadline" {
					parent, cancel = context.WithTimeout(parent, time.Second)
				} else {
					parent, cancel = context.WithCancel(parent)
				}
				defer cancel()
				var databaseContext context.Context
				check := func(ctx context.Context) error {
					databaseContext = ctx
					httpTestDeadline(t, ctx)
					if ctx.Value(markerKey{}) != "request marker" {
						t.Error("database context lost request values")
					}
					if deadline, ok := parent.Deadline(); ok {
						got, exists := ctx.Deadline()
						if !exists || got.After(deadline) {
							t.Error("database context extended the request deadline")
						}
					}
					if mode == "parent cancellation" {
						cancel()
						if !errors.Is(ctx.Err(), context.Canceled) {
							t.Errorf("database context ignored parent cancellation: %v", ctx.Err())
						}
					}
					return ctx.Err()
				}
				store := &httpTestStore{
					create: func(ctx context.Context, _ string) (Deployment, error) {
						return httpTestDeployment(httpTestDeploymentID), check(ctx)
					},
					list: func(ctx context.Context, _ string, _, _ int) ([]Deployment, error) { return nil, check(ctx) },
					get: func(ctx context.Context, _ string) (Deployment, error) {
						return httpTestDeployment(httpTestDeploymentID), check(ctx)
					},
				}
				response := httptest.NewRecorder()
				NewHandler(store).ServeHTTP(response, httptest.NewRequest(route.method, route.url, nil).WithContext(parent))
				status := route.status
				if mode == "parent cancellation" {
					status = http.StatusServiceUnavailable
				}
				if response.Code != status {
					t.Errorf("status = %d, want %d; body = %s", response.Code, status, response.Body.String())
				}
				if databaseContext == nil || databaseContext.Err() != context.Canceled {
					t.Errorf("database context was not canceled after handler completion: %v", databaseContext)
				}
				if store.totalCalls() != 1 {
					t.Errorf("store call count = %d, want 1", store.totalCalls())
				}
			})
		}
	}
}

func TestHTTPProtectsRequestsBeforeStore(t *testing.T) {
	for _, tt := range []struct{ name, method, url, host, origin, fetchSite string }{
		{"foreign create Host", http.MethodPost, httpTestCollectionURL, "rebound.example", "", ""},
		{"foreign list Host", http.MethodGet, httpTestCollectionURL, "rebound.example", "", ""},
		{"foreign detail Host", http.MethodGet, httpTestDetailURL, "rebound.example", "", ""},
		{"cross-origin create", http.MethodPost, httpTestCollectionURL, "", "http://other.test", ""},
		{"cross-site create", http.MethodPost, httpTestCollectionURL, "", "", "cross-site"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &httpTestStore{}
			req := httptest.NewRequest(tt.method, tt.url, nil)
			if tt.host != "" {
				req.Host = tt.host
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, req)
			httpTestErrorResponse(t, response, http.StatusForbidden)
			if store.totalCalls() != 0 {
				t.Errorf("unsafe request reached store %d times", store.totalCalls())
			}
		})
	}
}

func TestHTTPHeadAndUnsupportedMethods(t *testing.T) {
	for _, tt := range []struct {
		method, url string
		status      int
		calls       int
	}{
		{http.MethodHead, httpTestCollectionURL, http.StatusOK, 1},
		{http.MethodHead, httpTestDetailURL, http.StatusOK, 1},
		{http.MethodPut, httpTestCollectionURL, http.StatusMethodNotAllowed, 0},
		{http.MethodPatch, httpTestCollectionURL, http.StatusMethodNotAllowed, 0},
		{http.MethodDelete, httpTestCollectionURL, http.StatusMethodNotAllowed, 0},
		{http.MethodOptions, httpTestCollectionURL, http.StatusMethodNotAllowed, 0},
		{http.MethodPost, httpTestDetailURL, http.StatusMethodNotAllowed, 0},
		{http.MethodDelete, httpTestDetailURL, http.StatusMethodNotAllowed, 0},
	} {
		t.Run(tt.method+tt.url, func(t *testing.T) {
			store := &httpTestStore{
				list: func(context.Context, string, int, int) ([]Deployment, error) { return nil, nil },
				get: func(context.Context, string) (Deployment, error) {
					return httpTestDeployment(httpTestDeploymentID), nil
				},
			}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, httptest.NewRequest(tt.method, tt.url, nil))
			if response.Code != tt.status {
				t.Errorf("status = %d, want %d; body = %s", response.Code, tt.status, response.Body.String())
			}
			if tt.method == http.MethodHead {
				httpTestJSONHeaders(t, response)
			} else if allow := response.Header().Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "HEAD") {
				t.Errorf("Allow = %q, want GET and HEAD", allow)
			}
			if store.totalCalls() != tt.calls {
				t.Errorf("store call count = %d, want %d", store.totalCalls(), tt.calls)
			}
		})
	}
}

type httpTestStore struct {
	create                           func(context.Context, string) (Deployment, error)
	list                             func(context.Context, string, int, int) ([]Deployment, error)
	get                              func(context.Context, string) (Deployment, error)
	createCalls, listCalls, getCalls int
}

func (s *httpTestStore) Create(ctx context.Context, projectID string) (Deployment, error) {
	s.createCalls++
	if s.create == nil {
		return Deployment{}, errors.New("unexpected create call")
	}
	return s.create(ctx, projectID)
}

func (s *httpTestStore) List(ctx context.Context, projectID string, limit, offset int) ([]Deployment, error) {
	s.listCalls++
	if s.list == nil {
		return nil, errors.New("unexpected list call")
	}
	return s.list(ctx, projectID, limit, offset)
}

func (s *httpTestStore) Get(ctx context.Context, id string) (Deployment, error) {
	s.getCalls++
	if s.get == nil {
		return Deployment{}, errors.New("unexpected get call")
	}
	return s.get(ctx, id)
}

func (s *httpTestStore) totalCalls() int { return s.createCalls + s.listCalls + s.getCalls }

func httpTestDeployment(id string) Deployment {
	timestamp := time.Date(2026, time.October, 11, 12, 0, 0, 0, time.UTC)
	return Deployment{ID: id, ProjectID: httpTestProjectID, GitURL: "https://example.com/demo.git", Branch: "main", AppPort: 8080, HealthPath: "/ready", Status: StatusQueued, CreatedAt: timestamp, UpdatedAt: timestamp}
}

func httpTestDeploymentResponse(t *testing.T, response *httptest.ResponseRecorder, status int, want Deployment) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, status, response.Body.String())
	}
	httpTestJSONHeaders(t, response)
	var got Deployment
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deployment = %+v, want %+v", got, want)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	gotFields := make([]string, 0, len(fields))
	for field := range fields {
		gotFields = append(gotFields, field)
	}
	slices.Sort(gotFields)
	wantFields := []string{"app_port", "branch", "commit_sha", "created_at", "failure_reason", "failure_stage", "finished_at", "git_url", "health_path", "id", "image_id", "image_reference", "instance_id", "project_id", "readiness_attempts", "readiness_checked_at", "readiness_passed", "readiness_status_code", "started_at", "status", "updated_at"}
	if !slices.Equal(gotFields, wantFields) {
		t.Errorf("deployment JSON fields = %v, want %v", gotFields, wantFields)
	}
}

func httpTestErrorResponse(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, status, response.Body.String())
	}
	httpTestJSONHeaders(t, response)
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Errorf("expected nonempty JSON error, got %s; decode error = %v", response.Body.String(), err)
	}
}

func httpTestJSONHeaders(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	mediaType, _, err := mime.ParseMediaType(response.Header().Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", response.Header().Get("Content-Type"))
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	for name := range response.Header() {
		if strings.HasPrefix(strings.ToLower(name), "access-control-") {
			t.Errorf("unexpected CORS header %s", name)
		}
	}
}

func httpTestDeadline(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining <= 0 || remaining > 3*time.Second {
		t.Errorf("database deadline remaining = %v, present = %t; want positive and at most 3s", remaining, ok)
	}
}

func httpTestIntPointer(value int) *int { return &value }
