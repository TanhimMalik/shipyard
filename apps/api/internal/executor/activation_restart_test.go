package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/routing"
	"shipyard/apps/api/internal/runtime"
)

func activationRecoveryFixture(t *testing.T, replacement, published bool) *activationTestFixture {
	t.Helper()
	f := activationTestWorker(t, replacement)
	f.deployment.Status = deployments.StatusActivating
	f.claim.Deployment = *f.deployment
	f.begun = true
	store := f.worker.store.(testStore)
	store.activation = func(context.Context, string) (deployments.Activation, error) {
		return deployments.Activation{Target: deployments.RouteBinding(f.target), Previous: f.previous, CreatedAt: time.Now().UTC()}, nil
	}
	f.worker.store = store
	if published {
		var previous *routing.Binding
		if f.previous != nil {
			binding := routing.Binding(*f.previous)
			previous = &binding
		}
		if err := f.routes.putDefault(f.target, previous); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestActivationRecoveryConfirmsHealthyPublishedTarget(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		f := activationRecoveryFixture(t, replacement, true)
		if err := f.worker.recoverActivation(t.Context(), f.claim); err != nil || !f.confirmed || f.aborted || f.cleaned {
			t.Fatalf("published target was not recovered: confirmed=%t aborted=%t cleaned=%t error=%v", f.confirmed, f.aborted, f.cleaned, err)
		}
		if binding, err := f.routes.Get(t.Context(), f.claim.ProjectID); err != nil || binding != f.target {
			t.Fatalf("recovered target changed: %+v, %v", binding, err)
		}
	}
}

func TestActivationRecoveryFinishesAnUnpublishedOrRestoredAttempt(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		f := activationRecoveryFixture(t, replacement, false)
		f.routes.put = func(_ context.Context, next routing.Binding, previous *routing.Binding) error {
			if next == f.target {
				t.Fatal("recovery republished a candidate after observing the previous route")
			}
			return f.routes.putDefault(next, previous)
		}
		if err := f.worker.recoverActivation(t.Context(), f.claim); err != nil || !f.restored || !f.cleaned || !f.aborted || f.confirmed {
			t.Fatalf("unpublished attempt was not aborted safely: restored=%t cleaned=%t aborted=%t confirmed=%t error=%v", f.restored, f.cleaned, f.aborted, f.confirmed, err)
		}
	}
}

func TestActivationRecoveryRollsBackUnusablePublishedCandidates(t *testing.T) {
	for _, problem := range []string{"missing", "exited", "changed port", "routed unhealthy"} {
		t.Run(problem, func(t *testing.T) {
			f := activationRecoveryFixture(t, true, true)
			if problem == "routed unhealthy" {
				v := f.worker.verifier.(testVerifier)
				v.ready = func(context.Context, routing.Binding, string) error {
					return errors.Join(routing.ErrNotObserved, context.DeadlineExceeded)
				}
				f.worker.verifier = v
			} else {
				r := f.worker.runtime.(testRuntime)
				r.inspect = func(_ context.Context, spec runtime.Spec) (runtime.Instance, error) {
					if problem == "missing" {
						return runtime.Instance{}, runtime.ErrNotFound
					}
					got := workerTestContainer(spec, problem != "exited")
					if problem == "exited" {
						got.Status = "exited"
					} else {
						got.HostPort++
					}
					return got, nil
				}
				f.worker.runtime = r
			}
			if problem == "missing" {
				f.worker.builder = testBuilder{cleanup: func(context.Context, builds.Input, string) error { f.cleaned = true; return nil }}
			}
			if err := f.worker.recoverActivation(t.Context(), f.claim); err != nil || !f.restored || !f.cleaned || !f.aborted || f.confirmed {
				t.Fatalf("unusable target was not rolled back: restored=%t cleaned=%t aborted=%t confirmed=%t error=%v", f.restored, f.cleaned, f.aborted, f.confirmed, err)
			}
			if binding, err := f.routes.Get(t.Context(), f.claim.ProjectID); err != nil || binding != routing.Binding(*f.previous) {
				t.Fatalf("previous binding was not kept: %+v, %v", binding, err)
			}
		})
	}
}

func TestActivationRecoveryPreservesUncertainOutcomes(t *testing.T) {
	for _, at := range []string{"intent", "active route", "instance", "inspect", "observation", "confirm", "restore verification"} {
		t.Run(at, func(t *testing.T) {
			f := activationRecoveryFixture(t, false, true)
			uncertain := errors.New("private uncertain recovery operation")
			store := f.worker.store.(testStore)
			switch at {
			case "intent":
				store.activation = func(context.Context, string) (deployments.Activation, error) {
					return deployments.Activation{}, uncertain
				}
			case "active route":
				store.active = func(context.Context, string) (deployments.RouteBinding, error) {
					return deployments.RouteBinding{}, uncertain
				}
			case "instance":
				store.instance = func(context.Context, string) (deployments.RuntimeInstance, error) {
					return deployments.RuntimeInstance{}, uncertain
				}
			case "observation":
				store.observe = func(context.Context, deployments.Claim, string, deployments.Observation) (deployments.RuntimeInstance, error) {
					return deployments.RuntimeInstance{}, uncertain
				}
			case "confirm":
				store.confirm = func(context.Context, deployments.Claim, deployments.RouteBinding) (deployments.Deployment, error) {
					return deployments.Deployment{}, uncertain
				}
			}
			f.worker.store = store
			if at == "inspect" {
				r := f.worker.runtime.(testRuntime)
				r.inspect = func(context.Context, runtime.Spec) (runtime.Instance, error) { return runtime.Instance{}, uncertain }
				f.worker.runtime = r
			} else if at == "restore verification" {
				v := f.worker.verifier.(testVerifier)
				v.ready = func(context.Context, routing.Binding, string) error {
					return errors.Join(routing.ErrNotObserved, context.DeadlineExceeded)
				}
				v.restored = func(context.Context, string, *routing.Binding) error { return uncertain }
				f.worker.verifier = v
			}
			if err := f.worker.recoverActivation(t.Context(), f.claim); !errors.Is(err, uncertain) || f.cleaned || f.aborted || f.confirmed {
				t.Fatalf("uncertain recovery mutated resources: cleaned=%t aborted=%t confirmed=%t error=%v", f.cleaned, f.aborted, f.confirmed, err)
			}
		})
	}
}

func TestActivationRecoveryRejectsUnexpectedPublishedRoute(t *testing.T) {
	f := activationRecoveryFixture(t, false, true)
	foreign := f.target
	foreign.InstanceID = "00000000-0000-4000-8000-000000000099"
	f.routes.bindings[f.claim.ProjectID] = foreign
	if err := f.worker.recoverActivation(t.Context(), f.claim); err == nil || f.cleaned || f.aborted || f.confirmed {
		t.Fatalf("unexpected route was touched: cleaned=%t aborted=%t confirmed=%t error=%v", f.cleaned, f.aborted, f.confirmed, err)
	}
	if binding, err := f.routes.Get(t.Context(), f.claim.ProjectID); err != nil || binding != foreign {
		t.Fatalf("unexpected route lost: %+v, %v", binding, err)
	}
}

func TestActivationRecoveryRejectsInconsistentSavedIdentities(t *testing.T) {
	for _, mismatch := range []string{"target instance", "target container", "instance container", "active pointer"} {
		t.Run(mismatch, func(t *testing.T) {
			f := activationRecoveryFixture(t, true, true)
			store := f.worker.store.(testStore)
			if mismatch == "target instance" || mismatch == "target container" {
				read := store.activation
				store.activation = func(ctx context.Context, id string) (deployments.Activation, error) {
					intent, err := read(ctx, id)
					if mismatch == "target instance" {
						intent.Target.InstanceID = "00000000-0000-4000-8000-000000000099"
					} else {
						intent.Target.ContainerID = strings.Repeat("f", 64)
					}
					return intent, err
				}
			} else if mismatch == "instance container" {
				read := store.instance
				store.instance = func(ctx context.Context, id string) (deployments.RuntimeInstance, error) {
					instance, err := read(ctx, id)
					containerID := strings.Repeat("f", 64)
					instance.ContainerID = &containerID
					return instance, err
				}
			} else {
				store.active = func(context.Context, string) (deployments.RouteBinding, error) {
					return deployments.RouteBinding(f.target), nil
				}
			}
			f.worker.store = store
			if err := f.worker.recoverActivation(t.Context(), f.claim); err == nil || f.cleaned || f.confirmed || f.aborted {
				t.Fatalf("inconsistent identity was acted on: cleaned=%t confirmed=%t aborted=%t error=%v", f.cleaned, f.confirmed, f.aborted, err)
			}
			if current, err := f.routes.Get(t.Context(), f.claim.ProjectID); err != nil || current != f.target {
				t.Fatalf("inconsistent identity changed the route: %+v, %v", current, err)
			}
		})
	}
}

func TestRecoveryAcquiresCoordinatorGuardBeforeClaimTakeover(t *testing.T) {
	claim := workerTestClaim()
	guarded, finished := false, false
	taken := claim
	taken.Token = "new-recovery-token"
	worker := &Worker{routes: &testRoutes{withActivation: func(ctx context.Context, operation func(context.Context) error) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < time.Minute || time.Until(deadline) > 90*time.Second {
			t.Fatal("recovery budget does not cover route verification and rollback")
		}
		guarded = true
		defer func() { guarded = false }()
		return operation(ctx)
	}}, store: testStore{
		interrupted: func(context.Context) (deployments.Claim, error) {
			if finished {
				return deployments.Claim{}, deployments.ErrNoInterruptedDeployments
			}
			return claim, nil
		},
		take: func(ctx context.Context, got deployments.Claim) (deployments.Claim, error) {
			if !guarded {
				t.Fatal("claim takeover happened before waiting for the old route writer")
			}
			workerTestWrite(t, ctx, got, claim)
			return taken, nil
		},
		fail: func(ctx context.Context, got deployments.Claim, stage, reason string) (deployments.Deployment, error) {
			if !guarded {
				t.Fatal("recovery released its guard before recording failure")
			}
			workerTestWrite(t, ctx, got, taken)
			finished = true
			return deployments.Deployment{}, nil
		},
	}}
	if err := worker.recover(t.Context()); err != nil || !finished {
		t.Fatalf("guarded recovery did not finish: finished=%t error=%v", finished, err)
	}
}

func TestRecoveryGuardCancellationDoesNotTakeOrCleanClaim(t *testing.T) {
	claim := workerTestClaim()
	ctx, cancel := context.WithCancel(t.Context())
	worker := &Worker{routes: &testRoutes{withActivation: func(context.Context, func(context.Context) error) error {
		cancel()
		return context.Canceled
	}}, store: testStore{
		interrupted: func(context.Context) (deployments.Claim, error) { return claim, nil },
		take: func(context.Context, deployments.Claim) (deployments.Claim, error) {
			t.Fatal("took claim after guard cancellation")
			return deployments.Claim{}, nil
		},
	}}
	defer cancel()
	if err := worker.recover(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("guard cancellation was lost: %v", err)
	}
}
