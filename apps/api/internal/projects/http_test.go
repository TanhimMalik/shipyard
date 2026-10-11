package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const httpTestCreateJSON = `{"name":" Demo ","slug":" demo-project ","git_url":" https://example.com/demo.git ","branch":" main ","app_port":8080,"health_path":" /ready "}`

func TestHTTPCreateProject(t *testing.T) {
	wantInput := CreateInput{
		Name: "Demo", Slug: "demo-project", GitURL: "https://example.com/demo.git",
		Branch: "main", AppPort: 8080, HealthPath: "/ready",
	}
	wantProject := httpTestProject("11111111-1111-4111-8111-111111111111")
	for _, tt := range []struct {
		name   string
		body   []byte
		url    string
		origin string
	}{
		{"same origin and JSON charset", []byte(httpTestCreateJSON), "http://localhost:8080/api/projects", "http://localhost:8080"},
		{"CLI and exact 64 KiB body", append([]byte(httpTestCreateJSON), bytes.Repeat([]byte(" "), 64*1024-len(httpTestCreateJSON))...), "http://127.0.0.1:8080/api/projects", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			type contextKey struct{}
			parent := context.WithValue(context.Background(), contextKey{}, "request marker")
			var databaseContext context.Context
			store := &httpTestStore{
				create: func(ctx context.Context, input CreateInput) (Project, error) {
					databaseContext = ctx
					if input != wantInput {
						t.Errorf("normalized input = %+v, want %+v", input, wantInput)
					}
					if ctx.Value(contextKey{}) != "request marker" {
						t.Error("database context lost request values")
					}
					httpTestDatabaseDeadline(t, ctx)
					return wantProject, nil
				},
			}
			req := httptest.NewRequest(http.MethodPost, tt.url, bytes.NewReader(tt.body)).WithContext(parent)
			req.ContentLength = -1
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, req)
			if response.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body = %s", response.Code, response.Body.String())
			}
			httpTestJSONHeaders(t, response)
			if store.createCalls != 1 || store.listCalls != 0 {
				t.Fatalf("store calls: create = %d, list = %d", store.createCalls, store.listCalls)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
				t.Fatalf("decode project JSON: %v", err)
			}
			wantFields := []string{"app_port", "branch", "created_at", "git_url", "health_path", "id", "name", "slug", "updated_at"}
			gotFields := make([]string, 0, len(fields))
			for field := range fields {
				gotFields = append(gotFields, field)
			}
			slices.Sort(gotFields)
			if !slices.Equal(gotFields, wantFields) {
				t.Errorf("project JSON fields = %v, want %v", gotFields, wantFields)
			}
			var got Project
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode project: %v", err)
			}
			if got != wantProject {
				t.Errorf("project = %+v, want %+v", got, wantProject)
			}
			if databaseContext.Err() != context.Canceled {
				t.Errorf("database context was not canceled after completion: %v", databaseContext.Err())
			}
		})
	}
}

func TestHTTPRejectsCreateBeforeStore(t *testing.T) {
	valid := []byte(httpTestCreateJSON)
	oversized := append([]byte(httpTestCreateJSON), bytes.Repeat([]byte(" "), 64*1024-len(httpTestCreateJSON)+1)...)
	for _, tt := range []struct {
		name        string
		body        []byte
		contentType string
		origin      string
		status      int
	}{
		{"missing content type", valid, "", "", http.StatusUnsupportedMediaType},
		{"wrong content type", valid, "text/plain", "", http.StatusUnsupportedMediaType},
		{"malformed JSON", []byte("{"), "application/json", "", http.StatusBadRequest},
		{"null JSON", []byte("null"), "application/json", "", http.StatusBadRequest},
		{"null setting", []byte(`{"name":null}`), "application/json", "", http.StatusBadRequest},
		{"array JSON", []byte("[]"), "application/json", "", http.StatusBadRequest},
		{"unknown field", []byte(strings.TrimSuffix(httpTestCreateJSON, "}") + `,"unexpected":true}`), "application/json", "", http.StatusBadRequest},
		{"trailing JSON", append([]byte(httpTestCreateJSON), []byte(" {}")...), "application/json", "", http.StatusBadRequest},
		{"invalid UTF-8", bytes.Replace(valid, []byte("Demo"), []byte{0xff}, 1), "application/json", "", http.StatusBadRequest},
		{"oversized stream", oversized, "application/json", "", http.StatusRequestEntityTooLarge},
		{"invalid port", bytes.Replace(valid, []byte(`"app_port":8080`), []byte(`"app_port":0`), 1), "application/json", "", http.StatusUnprocessableEntity},
		{"cross origin", valid, "application/json", "http://other.test", http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &httpTestStore{}
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/projects", bytes.NewReader(tt.body))
			req.ContentLength = -1
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, req)
			if response.Code != tt.status {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, tt.status, response.Body.String())
			}
			httpTestErrorJSON(t, response)
			if store.createCalls != 0 || store.listCalls != 0 {
				t.Errorf("invalid request reached store: create = %d, list = %d", store.createCalls, store.listCalls)
			}
			if tt.status == http.StatusUnprocessableEntity {
				var body struct {
					Fields map[string]json.RawMessage `json:"fields"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode validation error: %v", err)
				}
				if _, ok := body.Fields["app_port"]; !ok {
					t.Errorf("validation fields = %v, want app_port", body.Fields)
				}
			}
		})
	}
}

func TestHTTPListProjects(t *testing.T) {
	first, second, third := httpTestProject("first"), httpTestProject("second"), httpTestProject("third")
	for _, tt := range []struct {
		name       string
		query      string
		rows       []Project
		wantLimit  int
		wantOffset int
		wantIDs    []string
		wantNext   *int
	}{
		{"empty defaults", "", nil, 51, 0, []string{}, nil},
		{"minimum page with more", "?limit=1&offset=3", []Project{first, second}, 2, 3, []string{"first"}, httpTestIntPointer(4)},
		{"exact page has no next", "?limit=2&offset=5", []Project{first, second}, 3, 5, []string{"first", "second"}, nil},
		{"extra row is withheld", "?limit=2&offset=5", []Project{third, first, second}, 3, 5, []string{"third", "first"}, httpTestIntPointer(7)},
		{"maximum page size", "?limit=100", []Project{first}, 101, 0, []string{"first"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &httpTestStore{
				list: func(ctx context.Context, limit, offset int) ([]Project, error) {
					httpTestDatabaseDeadline(t, ctx)
					if limit != tt.wantLimit || offset != tt.wantOffset {
						t.Errorf("store pagination = (%d, %d), want (%d, %d)", limit, offset, tt.wantLimit, tt.wantOffset)
					}
					return tt.rows, nil
				},
			}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/projects"+tt.query, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
			}
			httpTestJSONHeaders(t, response)
			var body struct {
				Projects   []Project `json:"projects"`
				NextOffset *int      `json:"next_offset"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode project list: %v", err)
			}
			if body.Projects == nil {
				t.Error("projects must encode as an array, including empty results")
			}
			gotIDs := make([]string, 0, len(body.Projects))
			for _, project := range body.Projects {
				gotIDs = append(gotIDs, project.ID)
			}
			if !slices.Equal(gotIDs, tt.wantIDs) {
				t.Errorf("listed project IDs = %v, want %v", gotIDs, tt.wantIDs)
			}
			if (body.NextOffset == nil) != (tt.wantNext == nil) || (body.NextOffset != nil && *body.NextOffset != *tt.wantNext) {
				t.Errorf("next_offset = %v, want %v", body.NextOffset, tt.wantNext)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 2 || fields["projects"] == nil || fields["next_offset"] == nil {
				t.Errorf("list must include only projects and next_offset: %s", response.Body.String())
			}
			if store.createCalls != 0 || store.listCalls != 1 {
				t.Errorf("store calls: create = %d, list = %d", store.createCalls, store.listCalls)
			}
		})
	}
}

func TestHTTPRejectsUnsafeBrowserRequestsBeforeStore(t *testing.T) {
	for _, tt := range []struct {
		name         string
		method       string
		url          string
		secFetchSite string
	}{
		{"external POST Host", http.MethodPost, "http://rebound.example/api/projects", ""},
		{"external GET Host", http.MethodGet, "http://rebound.example/api/projects", ""},
		{"cross-site fetch", http.MethodPost, "http://127.0.0.1:8080/api/projects", "cross-site"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &httpTestStore{}
			req := httptest.NewRequest(tt.method, tt.url, strings.NewReader(httpTestCreateJSON))
			req.Header.Set("Content-Type", "application/json")
			if tt.secFetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tt.secFetchSite)
			}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, req)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", response.Code, response.Body.String())
			}
			httpTestErrorJSON(t, response)
			if store.createCalls != 0 || store.listCalls != 0 {
				t.Errorf("unsafe request reached store: create = %d, list = %d", store.createCalls, store.listCalls)
			}
		})
	}
}

func TestHTTPRejectsInvalidPaginationBeforeStore(t *testing.T) {
	for _, query := range []string{
		"limit=0", "limit=101", "limit=1.5", "limit=", "limit=999999999999999999999999999",
		"offset=-1", "offset=invalid", "offset=999999999999999999999999999",
		fmt.Sprintf("limit=2&offset=%d", int(^uint(0)>>1)-1),
		"limit=2&limit=3", "offset=0&offset=1", "unexpected=true",
	} {
		t.Run(query, func(t *testing.T) {
			store := &httpTestStore{}
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/projects?"+query, nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body.String())
			}
			httpTestErrorJSON(t, response)
			if store.createCalls != 0 || store.listCalls != 0 {
				t.Errorf("invalid pagination reached store: create = %d, list = %d", store.createCalls, store.listCalls)
			}
		})
	}
}

func TestHTTPStoreErrorsAreSanitized(t *testing.T) {
	privateError := errors.New("private-sentinel: postgres://admin:secret@private-host/database SQL internals")
	for _, tt := range []struct {
		name   string
		method string
		err    error
		status int
	}{
		{"create unavailable", http.MethodPost, privateError, http.StatusServiceUnavailable},
		{"list unavailable", http.MethodGet, privateError, http.StatusServiceUnavailable},
		{"wrapped duplicate slug", http.MethodPost, fmt.Errorf("private-sentinel: %w", ErrSlugTaken), http.StatusConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &httpTestStore{
				create: func(context.Context, CreateInput) (Project, error) { return Project{}, tt.err },
				list:   func(context.Context, int, int) ([]Project, error) { return nil, tt.err },
			}
			req := httptest.NewRequest(tt.method, "http://127.0.0.1:8080/api/projects", strings.NewReader(httpTestCreateJSON))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, req)
			if response.Code != tt.status {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, tt.status, response.Body.String())
			}
			httpTestErrorJSON(t, response)
			for _, private := range []string{"private-sentinel", "private-host", "postgres://", "SQL internals"} {
				if strings.Contains(response.Body.String(), private) {
					t.Errorf("error response exposes %q", private)
				}
			}
		})
	}
}

func TestHTTPDatabaseContextFollowsRequestCancellation(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			checkContext := func(ctx context.Context) error {
				httpTestDatabaseDeadline(t, ctx)
				cancel()
				if !errors.Is(ctx.Err(), context.Canceled) {
					t.Errorf("database context ignored parent cancellation: %v", ctx.Err())
				}
				return ctx.Err()
			}
			store := &httpTestStore{
				create: func(ctx context.Context, _ CreateInput) (Project, error) { return Project{}, checkContext(ctx) },
				list:   func(ctx context.Context, _, _ int) ([]Project, error) { return nil, checkContext(ctx) },
			}
			req := httptest.NewRequest(method, "http://127.0.0.1:8080/api/projects", strings.NewReader(httpTestCreateJSON)).WithContext(parent)
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			NewHandler(store).ServeHTTP(response, req)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body = %s", response.Code, response.Body.String())
			}
			httpTestErrorJSON(t, response)
			if store.createCalls+store.listCalls != 1 {
				t.Errorf("store call count = %d, want 1", store.createCalls+store.listCalls)
			}
		})
	}
}

type httpTestStore struct {
	create      func(context.Context, CreateInput) (Project, error)
	list        func(context.Context, int, int) ([]Project, error)
	createCalls int
	listCalls   int
}

func (s *httpTestStore) Get(context.Context, string) (Project, error) {
	return Project{}, errors.New("unexpected get call")
}

func (s *httpTestStore) Update(context.Context, string, PatchInput) (Project, error) {
	return Project{}, errors.New("unexpected update call")
}

func (s *httpTestStore) Delete(context.Context, string) error {
	return errors.New("unexpected delete call")
}

func (s *httpTestStore) Create(ctx context.Context, input CreateInput) (Project, error) {
	s.createCalls++
	if s.create == nil {
		return Project{}, errors.New("unexpected create call")
	}
	return s.create(ctx, input)
}

func (s *httpTestStore) List(ctx context.Context, limit, offset int) ([]Project, error) {
	s.listCalls++
	if s.list == nil {
		return nil, errors.New("unexpected list call")
	}
	return s.list(ctx, limit, offset)
}

func httpTestProject(id string) Project {
	timestamp := time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)
	return Project{
		ID: id, Name: "Demo", Slug: "demo-project", GitURL: "https://example.com/demo.git",
		Branch: "main", AppPort: 8080, HealthPath: "/ready", CreatedAt: timestamp, UpdatedAt: timestamp,
	}
}

func httpTestJSONHeaders(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	mediaType, _, err := mime.ParseMediaType(response.Header().Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", response.Header().Get("Content-Type"))
	}
	if value := response.Header().Get("Cache-Control"); value != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", value)
	}
	if value := response.Header().Get("X-Content-Type-Options"); value != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", value)
	}
	for name := range response.Header() {
		if strings.HasPrefix(strings.ToLower(name), "access-control-") {
			t.Errorf("unexpected CORS header %s", name)
		}
	}
}

func httpTestErrorJSON(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	httpTestJSONHeaders(t, response)
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Errorf("expected nonempty JSON error, got %s; decode error = %v", response.Body.String(), err)
	}
}

func httpTestDatabaseDeadline(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining <= 0 || remaining > 3*time.Second {
		t.Errorf("database deadline remaining = %v, present = %t; want positive and at most 3s", remaining, ok)
	}
}

func httpTestIntPointer(value int) *int { return &value }
