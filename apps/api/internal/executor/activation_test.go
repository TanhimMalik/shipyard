package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/routing"
	"shipyard/apps/api/internal/runtime"
)

type activationTestFixture struct {
	worker                                      *Worker
	claim                                       deployments.Claim
	ready                                       readyDeployment
	deployment                                  *deployments.Deployment
	instance                                    *deployments.RuntimeInstance
	routes                                      *testRoutes
	target                                      routing.Binding
	previous                                    *deployments.RouteBinding
	begun, verified, restored, removed, cleaned bool
	confirmed, aborted                          bool
}

func activationTestWorker(t *testing.T, replacement bool) *activationTestFixture {
	t.Helper()
	w, claim, deployment, instance := readinessTestWorker(t)
	deployment.Status = deployments.StatusHealthChecking
	passed := true
	deployment.ReadinessPassed = &passed
	spec, err := startupSpec(*deployment)
	if err != nil {
		t.Fatal(err)
	}
	spec.ContainerID = *instance.ContainerID
	f := &activationTestFixture{worker: w, claim: claim, deployment: deployment, instance: instance, routes: &testRoutes{}}
	f.ready = readyDeployment{Deployment: *deployment, Instance: workerTestContainer(spec, true)}
	f.target = routing.Binding{ProjectID: claim.ProjectID, DeploymentID: claim.ID, InstanceID: instance.ID, ContainerID: spec.ContainerID, HostPort: *instance.HostPort}
	if replacement {
		previous := deployments.RouteBinding{ProjectID: claim.ProjectID, DeploymentID: "00000000-0000-4000-8000-000000000091", InstanceID: "00000000-0000-4000-8000-000000000092", ContainerID: strings.Repeat("d", 64), HostPort: 32124}
		f.previous = &previous
		f.routes.bindings = map[string]routing.Binding{claim.ProjectID: routing.Binding(previous)}
	}
	store := w.store.(testStore)
	store.active = func(context.Context, string) (deployments.RouteBinding, error) {
		if f.previous == nil {
			return deployments.RouteBinding{}, deployments.ErrActiveRouteNotFound
		}
		return *f.previous, nil
	}
	store.activate = func(ctx context.Context, got deployments.Claim, id string, port int) (deployments.Activation, error) {
		workerTestWrite(t, ctx, got, claim)
		if id != f.target.ContainerID || port != f.target.HostPort {
			t.Fatalf("activation changed the checked target: %s/%d", id, port)
		}
		f.begun = true
		deployment.Status = deployments.StatusActivating
		return deployments.Activation{Target: deployments.RouteBinding(f.target), Previous: f.previous}, nil
	}
	store.confirm = func(ctx context.Context, got deployments.Claim, target deployments.RouteBinding) (deployments.Deployment, error) {
		workerTestWrite(t, ctx, got, claim)
		if !f.begun || !f.verified || target != deployments.RouteBinding(f.target) || f.cleaned {
			t.Fatal("confirmed before checking the exact routed target")
		}
		f.confirmed = true
		deployment.Status = deployments.StatusRunning
		return *deployment, nil
	}
	store.abort = func(ctx context.Context, got deployments.Claim, previous *deployments.RouteBinding, reason string) (deployments.Deployment, error) {
		workerTestWrite(t, ctx, got, claim)
		if !f.restored || !f.cleaned || !sameTestBinding(previous, f.previous) || reason == "" || strings.Contains(reason, "private") {
			t.Fatal("aborted before verified restoration and candidate cleanup, or leaked diagnostics")
		}
		f.aborted = true
		deployment.Status = deployments.StatusFailed
		return *deployment, nil
	}
	w.store = store
	w.routes = f.routes
	w.verifier = testVerifier{ready: func(ctx context.Context, target routing.Binding, path string) error {
		current, err := f.routes.Get(ctx, claim.ProjectID)
		if !f.begun || err != nil || current != f.target || target != f.target || path != deployment.HealthPath {
			t.Fatal("checked traffic before publishing the exact saved target")
		}
		f.verified = true
		return nil
	}, restored: func(ctx context.Context, projectID string, previous *routing.Binding) error {
		current, err := f.routes.Get(ctx, projectID)
		if previous == nil {
			if !errors.Is(err, routing.ErrNotFound) || f.previous != nil {
				t.Fatal("route absence was not restored")
			}
		} else if err != nil || current != *previous || f.previous == nil || current != routing.Binding(*f.previous) {
			t.Fatal("different route was restored")
		}
		f.restored = true
		return nil
	}}
	w.runtime = testRuntime{inspect: func(_ context.Context, got runtime.Spec) (runtime.Instance, error) {
		if got.ContainerID != f.target.ContainerID {
			t.Fatal("activation inspected an unpinned container")
		}
		return workerTestContainer(got, true), nil
	}, remove: func(_ context.Context, got runtime.Spec) error {
		if f.begun && !f.restored || got.ContainerID != f.target.ContainerID || instance.Running == nil || *instance.Running {
			t.Fatal("removed a candidate before restoring the route and saving stopped state")
		}
		f.removed = true
		return nil
	}}
	w.builder = testBuilder{cleanup: func(_ context.Context, input builds.Input, id string) error {
		if !f.removed || id != *deployment.ImageID || input.DeploymentID != claim.ID {
			t.Fatal("removed the wrong image or cleaned before candidate removal")
		}
		f.cleaned = true
		return nil
	}}
	return f
}

func sameTestBinding(left, right *deployments.RouteBinding) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func TestActivationConfirmsOnlyTheVerifiedCandidate(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		f := activationTestWorker(t, replacement)
		if err := f.worker.activate(t.Context(), f.claim, f.ready); err != nil || !f.confirmed || f.aborted || f.cleaned || f.deployment.Status != deployments.StatusRunning {
			t.Fatalf("activation did not confirm cleanly: confirmed=%t aborted=%t cleaned=%t error=%v", f.confirmed, f.aborted, f.cleaned, err)
		}
		if current, err := f.routes.Get(t.Context(), f.claim.ProjectID); err != nil || current != f.target {
			t.Fatalf("confirmed route differs: %+v, %v", current, err)
		}
	}
}

func TestActivationFailureRestoresBeforeCleanupAndAbort(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		for _, problem := range []string{"publish response lost", "proxy timeout", "exit after verification"} {
			t.Run(problem+map[bool]string{false: "/first", true: "/replacement"}[replacement], func(t *testing.T) {
				f := activationTestWorker(t, replacement)
				if problem == "publish response lost" {
					failed := false
					f.routes.put = func(_ context.Context, next routing.Binding, previous *routing.Binding) error {
						if err := f.routes.putDefault(next, previous); err != nil {
							return err
						}
						if !failed && next == f.target {
							failed = true
							return errors.New("private publication response lost")
						}
						return nil
					}
				} else if problem == "proxy timeout" {
					verifier := f.worker.verifier.(testVerifier)
					verifier.ready = func(context.Context, routing.Binding, string) error {
						return errors.Join(routing.ErrNotObserved, context.DeadlineExceeded)
					}
					f.worker.verifier = verifier
				} else {
					engine := f.worker.runtime.(testRuntime)
					inspect := engine.inspect
					inspections := 0
					engine.inspect = func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
						inspections++
						if inspections >= 2 {
							got := workerTestContainer(spec, false)
							got.Status = "exited"
							return got, nil
						}
						return inspect(ctx, spec)
					}
					f.worker.runtime = engine
				}
				if err := f.worker.activate(t.Context(), f.claim, f.ready); err != nil || !f.aborted || !f.restored || !f.cleaned || f.confirmed {
					t.Fatalf("failure cleanup was not ordered: aborted=%t restored=%t cleaned=%t confirmed=%t error=%v", f.aborted, f.restored, f.cleaned, f.confirmed, err)
				}
			})
		}
	}
}

func TestActivationUncertaintyPreservesTheCandidate(t *testing.T) {
	for _, at := range []string{"active read", "route preflight", "begin commit", "inspect after", "observation after", "confirm commit", "rollback verification", "cleanup"} {
		t.Run(at, func(t *testing.T) {
			f := activationTestWorker(t, false)
			uncertain := errors.New("private uncertain operation")
			store := f.worker.store.(testStore)
			if at == "active read" {
				store.active = func(context.Context, string) (deployments.RouteBinding, error) {
					return deployments.RouteBinding{}, uncertain
				}
			} else if at == "begin commit" {
				store.activate = func(context.Context, deployments.Claim, string, int) (deployments.Activation, error) {
					return deployments.Activation{}, uncertain
				}
			} else if at == "confirm commit" {
				store.confirm = func(context.Context, deployments.Claim, deployments.RouteBinding) (deployments.Deployment, error) {
					return deployments.Deployment{}, uncertain
				}
			} else if at == "observation after" {
				observe, count := store.observe, 0
				store.observe = func(ctx context.Context, claim deployments.Claim, id string, observation deployments.Observation) (deployments.RuntimeInstance, error) {
					count++
					if count == 2 {
						return deployments.RuntimeInstance{}, uncertain
					}
					return observe(ctx, claim, id, observation)
				}
			}
			f.worker.store = store
			if at == "route preflight" {
				f.routes.get = func(context.Context, string) (routing.Binding, error) { return routing.Binding{}, uncertain }
			} else if at == "inspect after" {
				r := f.worker.runtime.(testRuntime)
				inspect, count := r.inspect, 0
				r.inspect = func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
					count++
					if count == 2 {
						return runtime.Instance{}, uncertain
					}
					return inspect(ctx, spec)
				}
				f.worker.runtime = r
			} else if at == "rollback verification" || at == "cleanup" {
				v := f.worker.verifier.(testVerifier)
				v.ready = func(context.Context, routing.Binding, string) error {
					return errors.Join(routing.ErrNotObserved, context.DeadlineExceeded)
				}
				if at == "rollback verification" {
					v.restored = func(context.Context, string, *routing.Binding) error { return uncertain }
				} else {
					f.worker.builder = testBuilder{cleanup: func(context.Context, builds.Input, string) error { return uncertain }}
				}
				f.worker.verifier = v
			}
			if err := f.worker.activate(t.Context(), f.claim, f.ready); !errors.Is(err, uncertain) || f.aborted || f.confirmed || at != "cleanup" && f.cleaned {
				t.Fatalf("uncertainty was terminalized: aborted=%t confirmed=%t cleaned=%t error=%v", f.aborted, f.confirmed, f.cleaned, err)
			}
		})
	}
}

func TestActivationCancellationLeavesDurableIntentForInspection(t *testing.T) {
	f := activationTestWorker(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.worker.verifier = testVerifier{ready: func(context.Context, routing.Binding, string) error {
		cancel()
		return context.Canceled
	}, restored: func(context.Context, string, *routing.Binding) error {
		t.Fatal("rolled back after cancellation")
		return nil
	}}
	if err := f.worker.activate(ctx, f.claim, f.ready); !errors.Is(err, context.Canceled) || !f.begun || f.cleaned || f.aborted || f.confirmed {
		t.Fatalf("canceled activation was changed: begun=%t cleaned=%t aborted=%t confirmed=%t error=%v", f.begun, f.cleaned, f.aborted, f.confirmed, err)
	}
}

func TestActivationDoesNotOverwriteAnUnexpectedRoute(t *testing.T) {
	for _, afterPublish := range []bool{false, true} {
		f := activationTestWorker(t, false)
		foreign := f.target
		foreign.DeploymentID = "00000000-0000-4000-8000-000000000099"
		if afterPublish {
			f.worker.verifier = testVerifier{ready: func(context.Context, routing.Binding, string) error {
				f.routes.bindings[f.claim.ProjectID] = foreign
				return errors.Join(routing.ErrNotObserved, context.DeadlineExceeded)
			}}
		} else {
			f.routes.bindings = map[string]routing.Binding{f.claim.ProjectID: foreign}
		}
		if err := f.worker.activate(t.Context(), f.claim, f.ready); err == nil || f.cleaned || f.aborted || f.confirmed {
			t.Fatalf("unexpected route was changed: cleaned=%t aborted=%t confirmed=%t error=%v", f.cleaned, f.aborted, f.confirmed, err)
		}
		if current, err := f.routes.Get(t.Context(), f.claim.ProjectID); err != nil || current != foreign {
			t.Fatalf("foreign route lost: %+v, %v", current, err)
		}
	}
}
