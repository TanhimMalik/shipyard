//go:build integration

package executor

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/routing"
	"shipyard/apps/api/internal/runtime"
)

func TestWorkerRecoversInterruptedRealTraefikActivation(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		replacement bool
		published   bool
		problem     string
	}{
		{name: "first switch not published"},
		{name: "first switch published", published: true},
		{name: "replacement published", replacement: true, published: true},
		{name: "previous route already restored", replacement: true},
		{name: "unhealthy replacement", replacement: true, published: true, problem: "unhealthy"},
		{name: "unexpected file", replacement: true, published: true, problem: "foreign"},
		{name: "proxy unavailable then recovered", replacement: true, published: true, problem: "proxy"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			manager, verifier, _ := executorTestTraefik(t)
			pool := executorTestDatabase(t)
			store := deployments.NewStore(pool)
			projectID := executorTestProject(t, pool, "https://example.com/recovery.git", "main")
			oldApp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "old") }))
			defer oldApp.Close()
			newApp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if scenario.problem == "unhealthy" {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				_, _ = io.WriteString(w, "new")
			}))
			defer newApp.Close()
			var previous *routing.Binding
			if scenario.replacement {
				claim, binding := executorTestPendingActivation(t, store, projectID, "a", executorTestPort(t, oldApp.Listener.Addr().String()))
				if err := manager.Put(t.Context(), binding, nil); err != nil {
					t.Fatal(err)
				}
				if err := verifier.Ready(t.Context(), binding, "/ready"); err != nil {
					t.Fatal(err)
				}
				if _, err := store.ConfirmActivation(t.Context(), claim, deployments.RouteBinding(binding)); err != nil {
					t.Fatal(err)
				}
				previous = &binding
			}
			stale, target := executorTestPendingActivation(t, store, projectID, "b", executorTestPort(t, newApp.Listener.Addr().String()))
			if scenario.published {
				if err := manager.Put(t.Context(), target, previous); err != nil {
					t.Fatal(err)
				}
			}
			foreign := target
			if scenario.problem == "foreign" {
				foreign.InstanceID = "00000000-0000-4000-8000-000000000099"
				if err := manager.Put(t.Context(), foreign, &target); err != nil {
					t.Fatal(err)
				}
			}
			cleaned := false
			newWorker := func() *Worker {
				worker := executorTestWorker(pool, nil)
				worker.routes, worker.verifier = manager, verifier
				worker.runtime = testRuntime{
					inspect: func(_ context.Context, spec runtime.Spec) (runtime.Instance, error) {
						if spec.DeploymentID != target.DeploymentID || spec.ContainerID != target.ContainerID {
							t.Fatal("recovery touched a different container")
						}
						if cleaned {
							return runtime.Instance{}, runtime.ErrNotFound
						}
						got := workerTestContainer(spec, true)
						got.HostPort = target.HostPort
						return got, nil
					},
					stop: func(_ context.Context, spec runtime.Spec) (runtime.Instance, error) {
						if spec.DeploymentID != target.DeploymentID || spec.ContainerID != target.ContainerID {
							t.Fatal("recovery stopped the previous container")
						}
						got := workerTestContainer(spec, false)
						got.Status = "exited"
						return got, nil
					},
					remove: func(context.Context, runtime.Spec) error { cleaned = true; return nil },
				}
				worker.builder = testBuilder{cleanup: func(_ context.Context, input builds.Input, _ string) error {
					if input.DeploymentID != target.DeploymentID || !cleaned {
						t.Fatal("image cleanup preceded candidate removal")
					}
					return nil
				}}
				return worker
			}
			worker := newWorker()
			if scenario.problem == "proxy" {
				listener, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				origin := "http://" + listener.Addr().String()
				_ = listener.Close()
				unavailable, err := routing.NewVerifier(routing.VerifierConfig{URL: origin, Timeout: 100 * time.Millisecond, RequestTimeout: 30 * time.Millisecond, Interval: 10 * time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				worker.verifier = unavailable
			}
			// A new worker reads only the saved pre-crash checkpoint and actual route.
			err := worker.recover(t.Context())
			if scenario.problem == "foreign" || scenario.problem == "proxy" {
				pending, readErr := store.NextInterrupted(t.Context())
				if err == nil || readErr != nil || pending.Status != deployments.StatusActivating || pending.Token == stale.Token || cleaned {
					t.Fatalf("uncertain recovery lost ownership or resources: cleaned=%t recovery=%v read=%v", cleaned, err, readErr)
				}
				if scenario.problem == "foreign" {
					if got, err := manager.Get(t.Context(), projectID); err != nil || got != foreign {
						t.Fatalf("unexpected file changed: %+v, %v", got, err)
					}
					return
				}
				if got, err := manager.Get(t.Context(), projectID); err != nil || got != *previous {
					t.Fatalf("previous file not restored before uncertain proxy check: %+v, %v", got, err)
				}
				err = newWorker().recover(t.Context())
			}
			if err != nil {
				t.Fatal(err)
			}
			confirmed := scenario.published && scenario.problem == ""
			want := deployments.StatusFailed
			if confirmed {
				want = deployments.StatusRunning
			}
			got, err := store.Get(t.Context(), target.DeploymentID)
			if err != nil || got.Status != want || cleaned == confirmed || got.ReadinessPassed == nil || !*got.ReadinessPassed {
				t.Fatalf("wrong recovery result: deployment=%+v cleaned=%t error=%v", got, cleaned, err)
			}
			if _, err := store.ConfirmActivation(t.Context(), stale, deployments.RouteBinding(target)); !errors.Is(err, deployments.ErrInvalidTransition) {
				t.Fatalf("pre-restart claim retained ownership: %v", err)
			}
			if _, err := store.NextInterrupted(t.Context()); !errors.Is(err, deployments.ErrNoInterruptedDeployments) {
				t.Fatalf("recovery did not release unfinished slot: %v", err)
			}
			expected := previous
			if confirmed {
				expected = &target
			}
			if err := verifier.Restored(t.Context(), projectID, expected); err != nil {
				t.Fatalf("wrong actual proxy route after recovery: %v", err)
			}
			active, activeErr := store.GetActiveRoute(t.Context(), projectID)
			if expected == nil {
				if !errors.Is(activeErr, deployments.ErrActiveRouteNotFound) {
					t.Fatalf("aborted first route became active: %v", activeErr)
				}
			} else if activeErr != nil || routing.Binding(active) != *expected {
				t.Fatalf("active pointer differs from observed route: %+v, %v", active, activeErr)
			}
			if previous != nil {
				old, err := store.Get(t.Context(), previous.DeploymentID)
				wantOld := deployments.StatusRunning
				if confirmed {
					wantOld = deployments.StatusSuperseded
				}
				instance, instanceErr := store.GetInstance(t.Context(), previous.InstanceID)
				if err != nil || old.Status != wantOld || instanceErr != nil || instance.Running == nil || !*instance.Running {
					t.Fatalf("previous runtime changed: deployment=%+v instance=%+v errors=%v,%v", old, instance, err, instanceErr)
				}
			}
		})
	}
}

func executorTestPendingActivation(t *testing.T, store *deployments.PostgresStore, projectID, marker string, port int) (deployments.Claim, routing.Binding) {
	t.Helper()
	ctx := t.Context()
	if _, err := store.Create(ctx, projectID); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordSourceCommit(ctx, claim, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginBuild(ctx, claim); err != nil {
		t.Fatal(err)
	}
	image := workerTestImage(claim.ID)
	starting, err := store.BeginStart(ctx, claim, image.Reference, image.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := routing.Binding{ProjectID: projectID, DeploymentID: claim.ID, InstanceID: *starting.InstanceID, ContainerID: strings.Repeat(marker, 64), HostPort: port}
	if _, err := store.RecordInstance(ctx, claim, target.InstanceID, deployments.Observation{ContainerID: target.ContainerID, Status: "running", Running: true, HostPort: port}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginHealthCheck(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordReadiness(ctx, claim, target.InstanceID, deployments.ReadinessObservation{ContainerID: target.ContainerID, HostPort: port, Passed: true, Attempts: 1, StatusCode: 200, CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginActivation(ctx, claim, target.ContainerID, port); err != nil {
		t.Fatal(err)
	}
	return claim, target
}
