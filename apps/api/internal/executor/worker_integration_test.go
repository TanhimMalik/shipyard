//go:build integration

package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/moby/moby/client"
	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/runtime"
	"shipyard/apps/api/internal/source"
	"shipyard/apps/api/migrations"
)

func TestAsyncSourceOwnershipAPIAndRestartRecovery(t *testing.T) {
	pool := executorTestDatabase(t)
	store := deployments.NewStore(pool)
	projectID := executorTestProject(t, pool, "https://example.com/approved.git", "release/demo")
	api := httptest.NewServer(deployments.NewHandler(store))
	t.Cleanup(api.Close)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	worker := executorTestWorker(pool, fetchFunc(func(ctx context.Context, repository, branch string) (*source.Checkout, error) {
		if repository != "https://example.com/approved.git" || branch != "release/demo" {
			t.Error("queued snapshot changed")
		}
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return &source.Checkout{CommitSHA: strings.Repeat("a", 40)}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	stop := executorTestRun(t, worker)
	queued := executorTestQueue(t, api.URL, projectID)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("source fetch did not start")
	}
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(api.URL + "/api/deployments/" + queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	var fetching deployments.Deployment
	if err := json.NewDecoder(response.Body).Decode(&fetching); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || fetching.Status != deployments.StatusFetchingSource || fetching.CommitSHA != nil {
		t.Fatalf("API blocked on source or invented progress: %+v", fetching)
	}
	other := executorTestWorker(pool, fetchFunc(func(context.Context, string, string) (*source.Checkout, error) {
		t.Error("second executor fetched source")
		return nil, errors.New("unexpected fetch")
	}))
	if err := other.Run(t.Context()); !errors.Is(err, ErrExecutorBusy) {
		t.Fatalf("second executor was not excluded: %v", err)
	}
	current, err := store.Get(t.Context(), queued.ID)
	if err != nil || current.Status != deployments.StatusFetchingSource {
		t.Fatalf("second executor recovered live work: %+v, %v", current, err)
	}
	if err := stop(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// The interrupted claim remains durable until the next exclusive owner starts.
	replacement := executorTestWorker(pool, fetchFunc(func(context.Context, string, string) (*source.Checkout, error) {
		return &source.Checkout{CommitSHA: strings.Repeat("b", 40)}, nil
	}))
	stopReplacement := executorTestRun(t, replacement)
	failed := executorTestWait(t, store, queued.ID, func(d deployments.Deployment) bool { return d.Status == deployments.StatusFailed })
	if failed.FailureStage == nil || *failed.FailureStage != deployments.StatusFetchingSource || failed.FinishedAt == nil {
		t.Fatalf("restart did not preserve interrupted failure: %+v", failed)
	}
	next := executorTestQueue(t, api.URL, projectID)
	checkpoint := executorTestWait(t, store, next.ID, func(d deployments.Deployment) bool { return d.Status == deployments.StatusStarting })
	if checkpoint.CommitSHA == nil || *checkpoint.CommitSHA != strings.Repeat("b", 40) || checkpoint.ImageID == nil || checkpoint.InstanceID == nil {
		t.Fatalf("build checkpoint was not saved: %+v", checkpoint)
	}
	instance, err := store.GetInstance(t.Context(), *checkpoint.InstanceID)
	if err != nil || instance.ContainerID != nil || instance.ObservedAt != nil || instance.Running != nil {
		t.Fatalf("planned instance invented Docker observations: %+v, %v", instance, err)
	}
	if err := stopReplacement(); err != nil {
		t.Fatal(err)
	}
	stopRecovery := executorTestRun(t, executorTestWorker(pool, nil))
	recovered := executorTestWait(t, store, next.ID, func(d deployments.Deployment) bool { return d.Status == deployments.StatusFailed })
	if recovered.CommitSHA == nil || *recovered.CommitSHA != strings.Repeat("b", 40) || recovered.ImageID == nil || *recovered.ImageID != *checkpoint.ImageID || recovered.FailureStage == nil || *recovered.FailureStage != deployments.StatusStarting {
		t.Fatalf("restart discarded build history or failed the wrong stage: %+v", recovered)
	}
	if err := stopRecovery(); err != nil {
		t.Fatal(err)
	}
}

func TestExecutorFetchesApprovedPublicGitThroughQueuedAPI(t *testing.T) {
	repository := os.Getenv("SHIPYARD_TEST_EXECUTOR_GIT_URL")
	if repository == "" {
		t.Skip("set SHIPYARD_TEST_EXECUTOR_GIT_URL to an approved public HTTPS repository for the network checkout test")
	}
	branch := os.Getenv("SHIPYARD_TEST_EXECUTOR_GIT_BRANCH")
	if branch == "" {
		branch = "main"
	}
	pool := executorTestDatabase(t)
	root := filepath.Join(t.TempDir(), "workspace")
	worker, err := New(Config{Enabled: true, WorkspaceRoot: root, AllowedRepositories: []string{repository}, DockerHost: "unix:///tmp/shipyard-source-test.sock"}, pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Close() })
	building := make(chan builds.Input, 1)
	worker.builder = testBuilder{build: func(ctx context.Context, input builds.Input) (builds.Image, error) {
		building <- input
		<-ctx.Done()
		return builds.Image{}, ctx.Err()
	}}
	stop := executorTestRun(t, worker)
	store := deployments.NewStore(pool)
	api := httptest.NewServer(deployments.NewHandler(store))
	t.Cleanup(api.Close)
	queued := executorTestQueue(t, api.URL, executorTestProject(t, pool, repository, branch))
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		saved, err := store.Get(ctx, queued.ID)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if saved.Status == deployments.StatusFailed {
			t.Fatalf("real Git checkout failed: %+v", saved)
		}
		if saved.Status == deployments.StatusBuilding {
			if saved.CommitSHA == nil || len(*saved.CommitSHA) != 40 && len(*saved.CommitSHA) != 64 || saved.ImageID != nil || saved.InstanceID != nil {
				t.Fatalf("invalid real source checkpoint: %+v", saved)
			}
			select {
			case input := <-building:
				if input.CommitSHA != *saved.CommitSHA || input.DeploymentID != saved.ID {
					t.Fatalf("build did not receive fetched source identity: %+v", input)
				}
				if _, err := os.Stat(input.CheckoutPath); err != nil {
					t.Fatalf("source workspace did not survive through build: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("builder did not receive the real checkout")
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("checkout workspace was not cleaned: %d entries, %v", len(entries), err)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("real source checkpoint was not recorded within 90 seconds")
}

func TestExecutorBuildsThroughQueuedAPIAndRecoversBeforeStartup(t *testing.T) {
	host := os.Getenv("SHIPYARD_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("set SHIPYARD_TEST_DOCKER_HOST to an explicit disposable Linux Docker socket")
	}
	for _, broken := range []bool{false, true} {
		name := "saved image and safe restart cleanup"
		if broken {
			name = "failed build keeps source history"
		}
		t.Run(name, func(t *testing.T) {
			pool := executorTestDatabase(t)
			store := deployments.NewStore(pool)
			builder, err := builds.NewBuilder(builds.Config{DockerHost: host, Timeout: 30 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = builder.Close() })
			inspector, err := runtime.NewManager(runtime.Config{DockerHost: host})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = inspector.Close() })
			docker, err := client.New(client.WithHost(host), client.WithAPIVersionNegotiation())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = docker.Close() })
			root, sha := t.TempDir(), strings.Repeat("d", 40)
			for filename, content := range map[string]string{"Dockerfile": "FROM scratch\nCOPY payload /payload\n", "payload": "executor-build-fixture"} {
				if broken && filename == "payload" {
					continue
				}
				if err := os.WriteFile(filepath.Join(root, filename), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			worker := executorTestWorker(pool, fetchFunc(func(context.Context, string, string) (*source.Checkout, error) {
				return &source.Checkout{Path: root, CommitSHA: sha}, nil
			}))
			worker.builder, worker.inspector = builder, inspector
			stop := executorTestRun(t, worker)
			api := httptest.NewServer(deployments.NewHandler(store))
			t.Cleanup(api.Close)
			queued := executorTestQueue(t, api.URL, executorTestProject(t, pool, "https://example.com/build-fixture.git", "main"))
			input := builds.Input{DeploymentID: queued.ID, CommitSHA: sha, BuildID: queued.ID}
			reference := workerTestImage(queued.ID).Reference
			t.Cleanup(func() {
				if err := stop(); err != nil {
					t.Error(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := builder.Cleanup(ctx, input, ""); err != nil {
					t.Errorf("clean exact executor fixture build: %v", err)
				}
			})
			saved := executorTestWait(t, store, queued.ID, func(d deployments.Deployment) bool {
				return d.Status == deployments.StatusStarting || d.Status == deployments.StatusFailed
			}, 45*time.Second)
			if saved.CommitSHA == nil || *saved.CommitSHA != sha {
				t.Fatalf("build lost exact source history: %+v", saved)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if broken {
				if saved.Status != deployments.StatusFailed || saved.FailureStage == nil || *saved.FailureStage != deployments.StatusBuilding || saved.FailureReason == nil || *saved.FailureReason != "Image build failed." || saved.ImageID != nil || saved.InstanceID != nil {
					t.Fatalf("real build failure was not saved safely: %+v", saved)
				}
				if _, err := docker.ImageInspect(ctx, reference); !errdefs.IsNotFound(err) {
					t.Fatalf("failed build left an image: %v", err)
				}
				return
			}
			if saved.Status != deployments.StatusStarting || saved.ImageReference == nil || *saved.ImageReference != reference || saved.ImageID == nil || saved.InstanceID == nil {
				t.Fatalf("build result or instance intent missing: %+v", saved)
			}
			image, err := docker.ImageInspect(ctx, reference)
			if err != nil || image.ID != *saved.ImageID || image.Config == nil || image.Config.Labels["io.shipyard.deployment_id"] != queued.ID || image.Config.Labels["io.shipyard.commit_sha"] != sha {
				t.Fatalf("persisted image not present with correct ownership: %v", err)
			}
			instance, err := store.GetInstance(t.Context(), *saved.InstanceID)
			if err != nil || instance.ContainerID != nil || instance.ObservedAt != nil || instance.Running != nil {
				t.Fatalf("build worker invented Docker observations: %+v, %v", instance, err)
			}
			spec := runtime.Spec{ProjectID: saved.ProjectID, DeploymentID: saved.ID, InstanceID: *saved.InstanceID, ImageID: *saved.ImageID, CommitSHA: sha, AppPort: saved.AppPort}
			if _, err := inspector.Inspect(ctx, spec); !errors.Is(err, runtime.ErrNotFound) {
				t.Fatalf("build worker unexpectedly created a container: %v", err)
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			replacement := executorTestWorker(pool, nil)
			replacement.builder, replacement.inspector = builder, inspector
			stopRecovery := executorTestRun(t, replacement)
			recovered := executorTestWait(t, store, queued.ID, func(d deployments.Deployment) bool { return d.Status == deployments.StatusFailed })
			if recovered.FailureStage == nil || *recovered.FailureStage != deployments.StatusStarting || recovered.ImageID == nil || *recovered.ImageID != *saved.ImageID || recovered.InstanceID == nil || *recovered.InstanceID != *saved.InstanceID {
				t.Fatalf("restart lost failed startup history: %+v", recovered)
			}
			if err := stopRecovery(); err != nil {
				t.Fatal(err)
			}
			if _, err := docker.ImageInspect(ctx, reference); !errdefs.IsNotFound(err) {
				t.Fatalf("restart did not remove its exact image tag: %v", err)
			}
		})
	}
}

func executorTestWorker(pool *pgxpool.Pool, fetcher fetcher) *Worker {
	return &Worker{store: deployments.NewStore(pool), fetcher: fetcher,
		builder: testBuilder{
			build: func(ctx context.Context, input builds.Input) (builds.Image, error) {
				return workerTestImage(input.DeploymentID), nil
			},
			cleanup: func(context.Context, builds.Input, string) error { return nil },
		}, inspector: inspectFunc(func(context.Context, runtime.Spec) (runtime.Instance, error) {
			return runtime.Instance{}, runtime.ErrNotFound
		}),
		acquire: func(ctx context.Context) (lease, error) { return acquireLease(ctx, pool) }, pollInterval: 5 * time.Millisecond, checkInterval: 20 * time.Millisecond}
}

func executorTestRun(t *testing.T, w *Worker) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var result error
	go func() { defer close(done); result = w.Run(ctx) }()
	stop := func() error {
		cancel()
		select {
		case <-done:
			return result
		case <-time.After(5 * time.Second):
			return errors.New("executor did not stop within five seconds")
		}
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	return stop
}

func executorTestQueue(t *testing.T, origin, projectID string) deployments.Deployment {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Post(origin+"/api/projects/"+projectID+"/deployments", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var deployment deployments.Deployment
	if err := json.NewDecoder(response.Body).Decode(&deployment); err != nil || response.StatusCode != http.StatusCreated || deployment.Status != deployments.StatusQueued {
		t.Fatalf("queue response: status=%d deployment=%+v error=%v", response.StatusCode, deployment, err)
	}
	return deployment
}

func executorTestWait(t *testing.T, store *deployments.PostgresStore, id string, ready func(deployments.Deployment) bool, timeout ...time.Duration) deployments.Deployment {
	t.Helper()
	wait := 5 * time.Second
	if len(timeout) > 0 {
		wait = timeout[0]
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		deployment, err := store.Get(ctx, id)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if ready(deployment) {
			return deployment
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("deployment did not reach expected state")
	return deployments.Deployment{}
}

func executorTestProject(t *testing.T, pool *pgxpool.Pool, repository, branch string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(t.Context(), `INSERT INTO projects (name,slug,git_url,branch,app_port,health_path)
		VALUES ('Worker project','worker-project',$1,$2,8080,'/ready') RETURNING id::text`, repository, branch).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func executorTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("SHIPYARD_TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("integration tests require SHIPYARD_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"shipyard_executor_test_" + hex.EncodeToString(random[:])}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.MaxConns = 4
	db := stdlib.OpenDB(*config.ConnConfig.Copy())
	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Up(ctx)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
