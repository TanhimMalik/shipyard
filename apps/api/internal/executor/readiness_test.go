package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/health"
	"shipyard/apps/api/internal/runtime"
)

func readinessTestWorker(t *testing.T) (*Worker, deployments.Claim, *deployments.Deployment, *deployments.RuntimeInstance) {
	t.Helper()
	claim := workerTestClaim()
	deployment := workerTestStarting(claim, strings.Repeat("a", 40))
	w, instance := startupTestWorker(t, claim, deployment)
	id, status, running, port := strings.Repeat("c", 64), "running", true, 32123
	instance.ContainerID, instance.ObservedStatus, instance.Running, instance.HostPort = &id, &status, &running, &port
	store := w.store.(testStore)
	store.beginHealth = func(ctx context.Context, got deployments.Claim) (deployments.Deployment, error) {
		workerTestWrite(t, ctx, got, claim)
		deployment.Status = deployments.StatusHealthChecking
		return deployment, nil
	}
	store.readiness = func(ctx context.Context, got deployments.Claim, instanceID string, observed deployments.ReadinessObservation) (deployments.Deployment, error) {
		workerTestWrite(t, ctx, got, claim)
		if instanceID != instance.ID || observed.ContainerID != id || observed.HostPort != port {
			t.Fatalf("readiness result lost candidate identity: %+v", observed)
		}
		deployment.ReadinessPassed, deployment.ReadinessAttempts = &observed.Passed, &observed.Attempts
		deployment.ReadinessStatusCode, deployment.ReadinessCheckedAt = &observed.StatusCode, &observed.CheckedAt
		return deployment, nil
	}
	store.fail = func(ctx context.Context, got deployments.Claim, stage, reason string) (deployments.Deployment, error) {
		workerTestWrite(t, ctx, got, claim)
		if stage != deployments.StatusHealthChecking || reason == "" || strings.Contains(reason, "private") {
			t.Fatalf("readiness failed at wrong stage or exposed diagnostics: %s/%s", stage, reason)
		}
		deployment.Status = deployments.StatusFailed
		return deployment, nil
	}
	w.store = store
	w.runtime = testRuntime{inspect: func(context.Context, runtime.Spec) (runtime.Instance, error) {
		return runtime.Instance{ID: id, Status: status, Running: true, HostPort: port}, nil
	}}
	w.readiness = probeFunc(workerTestReady)
	return w, claim, &deployment, instance
}

func TestReadinessUsesFreshPinnedObservationsAndLeavesRoutingPending(t *testing.T) {
	w, claim, deployment, _ := readinessTestWorker(t)
	var calls []string
	w.runtime = testRuntime{inspect: func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
		if spec.ContainerID != strings.Repeat("c", 64) || spec.InstanceID != *deployment.InstanceID || spec.ImageID != *deployment.ImageID {
			t.Fatalf("readiness inspected an unpinned candidate: %+v", spec)
		}
		calls = append(calls, "inspect")
		return workerTestContainer(spec, true), nil
	}}
	w.readiness = probeFunc(func(ctx context.Context, target health.Target) (health.Result, error) {
		if target.HostPort != 32123 || target.Path != deployment.HealthPath || deployment.Status != deployments.StatusHealthChecking {
			t.Fatalf("probed wrong endpoint or before the transition: %+v", target)
		}
		calls = append(calls, "probe")
		return health.Result{Attempts: 3, StatusCode: 204, CheckedAt: time.Now().UTC()}, nil
	})
	store := w.store.(testStore)
	readiness := store.readiness
	store.readiness = func(ctx context.Context, claim deployments.Claim, id string, observed deployments.ReadinessObservation) (deployments.Deployment, error) {
		calls = append(calls, "save")
		return readiness(ctx, claim, id, observed)
	}
	store.fail = func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error) {
		t.Fatal("successful readiness failed the deployment")
		return deployments.Deployment{}, nil
	}
	w.store = store
	if ready, err := w.checkReadiness(t.Context(), claim, *deployment); err != nil || ready == nil || ready.Deployment.ID != deployment.ID || ready.Instance.ID != strings.Repeat("c", 64) {
		t.Fatalf("missing successful readiness handoff: %+v, %v", ready, err)
	}
	if strings.Join(calls, ",") != "inspect,probe,inspect,save" || deployment.Status != deployments.StatusHealthChecking || deployment.FinishedAt != nil || deployment.ReadinessPassed == nil || !*deployment.ReadinessPassed || *deployment.ReadinessAttempts != 3 || *deployment.ReadinessStatusCode != 204 || deployment.ReadinessCheckedAt.IsZero() {
		t.Fatalf("readiness advanced routing or lost the actual result: calls=%v deployment=%+v", calls, deployment)
	}
}

func TestReadinessFailureSavesResultBeforeCleaningAndFailing(t *testing.T) {
	for _, failure := range []string{"timeout", "exited after HTTP success", "port changed after HTTP success"} {
		t.Run(failure, func(t *testing.T) {
			w, claim, deployment, instance := readinessTestWorker(t)
			inspections, removed, cleaned := 0, false, false
			w.runtime = testRuntime{
				inspect: func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
					inspections++
					got := workerTestContainer(spec, true)
					if failure == "exited after HTTP success" && inspections >= 2 {
						got.Status, got.Running, got.HostPort, got.ExitCode = "exited", false, 0, 7
					} else if failure == "port changed after HTTP success" && inspections >= 2 {
						got.HostPort++
					}
					return got, nil
				},
				remove: func(ctx context.Context, spec runtime.Spec) error {
					if deployment.ReadinessPassed == nil || *deployment.ReadinessPassed || instance.Running == nil || *instance.Running || spec.ContainerID != strings.Repeat("c", 64) {
						t.Fatal("removed before saving failed readiness and stopped process identity")
					}
					removed = true
					return nil
				},
			}
			status := 200
			w.readiness = probeFunc(func(context.Context, health.Target) (health.Result, error) {
				result := health.Result{Attempts: 2, StatusCode: 200, CheckedAt: time.Now().UTC()}
				if failure == "timeout" {
					result.StatusCode, status = 503, 503
					return result, errors.Join(health.ErrNotReady, context.DeadlineExceeded)
				}
				return result, nil
			})
			w.builder = testBuilder{cleanup: func(ctx context.Context, input builds.Input, expectedID string) error {
				if !removed || expectedID != *deployment.ImageID {
					t.Fatal("image cleanup preceded owned container removal")
				}
				cleaned = true
				return nil
			}}
			store := w.store.(testStore)
			fail := store.fail
			store.fail = func(ctx context.Context, c deployments.Claim, stage, reason string) (deployments.Deployment, error) {
				if !cleaned {
					t.Fatal("failed the deployment before verified cleanup")
				}
				return fail(ctx, c, stage, reason)
			}
			w.store = store
			if ready, err := w.checkReadiness(t.Context(), claim, *deployment); err != nil || ready != nil || deployment.Status != deployments.StatusFailed || deployment.ReadinessPassed == nil || *deployment.ReadinessPassed || deployment.ReadinessStatusCode == nil || *deployment.ReadinessStatusCode != status {
				t.Fatalf("readiness failure lost its truthful outcome: %+v, %v", deployment, err)
			}
		})
	}
}

func TestReadinessUncertaintyPreservesTheUnfinishedCandidate(t *testing.T) {
	for _, at := range []string{"read instance", "inspect before", "record before", "begin health", "probe", "inspect after", "record after", "record readiness"} {
		t.Run(at, func(t *testing.T) {
			w, claim, deployment, _ := readinessTestWorker(t)
			uncertain := errors.New("private uncertain operation")
			store := w.store.(testStore)
			read, begin, observe, readiness := store.instance, store.beginHealth, store.observe, store.readiness
			store.instance = func(ctx context.Context, id string) (deployments.RuntimeInstance, error) {
				if at == "read instance" {
					return deployments.RuntimeInstance{}, uncertain
				}
				return read(ctx, id)
			}
			store.beginHealth = func(ctx context.Context, c deployments.Claim) (deployments.Deployment, error) {
				if at == "begin health" {
					return deployments.Deployment{}, uncertain
				}
				return begin(ctx, c)
			}
			observations := 0
			store.observe = func(ctx context.Context, c deployments.Claim, id string, o deployments.Observation) (deployments.RuntimeInstance, error) {
				observations++
				if at == "record before" && observations == 1 || at == "record after" && observations == 2 {
					return deployments.RuntimeInstance{}, uncertain
				}
				return observe(ctx, c, id, o)
			}
			store.readiness = func(ctx context.Context, c deployments.Claim, id string, o deployments.ReadinessObservation) (deployments.Deployment, error) {
				if at == "record readiness" {
					return deployments.Deployment{}, uncertain
				}
				return readiness(ctx, c, id, o)
			}
			store.fail = func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error) {
				t.Fatal("failed uncertain readiness")
				return deployments.Deployment{}, nil
			}
			w.store = store
			inspections := 0
			w.runtime = testRuntime{inspect: func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
				inspections++
				if at == "inspect before" && inspections == 1 || at == "inspect after" && inspections == 2 {
					return runtime.Instance{}, uncertain
				}
				return workerTestContainer(spec, true), nil
			}, stop: func(context.Context, runtime.Spec) (runtime.Instance, error) {
				t.Fatal("stopped uncertain readiness candidate")
				return runtime.Instance{}, nil
			}, remove: func(context.Context, runtime.Spec) error {
				t.Fatal("removed uncertain readiness candidate")
				return nil
			}}
			w.builder = testBuilder{cleanup: func(context.Context, builds.Input, string) error {
				t.Fatal("removed uncertain readiness image")
				return nil
			}}
			w.readiness = probeFunc(func(ctx context.Context, target health.Target) (health.Result, error) {
				if at == "probe" {
					return health.Result{}, uncertain
				}
				return workerTestReady(ctx, target)
			})
			if ready, err := w.checkReadiness(t.Context(), claim, *deployment); !errors.Is(err, uncertain) || ready != nil {
				t.Fatalf("lost readiness uncertainty: %v", err)
			}
		})
	}
}

func TestCanceledReadinessLeavesNoInventedObservation(t *testing.T) {
	w, claim, deployment, _ := readinessTestWorker(t)
	ctx, cancel := context.WithCancel(t.Context())
	w.readiness = probeFunc(func(context.Context, health.Target) (health.Result, error) {
		cancel()
		return health.Result{}, context.Canceled
	})
	store := w.store.(testStore)
	store.readiness = func(context.Context, deployments.Claim, string, deployments.ReadinessObservation) (deployments.Deployment, error) {
		t.Fatal("saved readiness without a completed probe")
		return deployments.Deployment{}, nil
	}
	store.fail = func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error) {
		t.Fatal("failed canceled readiness")
		return deployments.Deployment{}, nil
	}
	w.store = store
	if ready, err := w.checkReadiness(ctx, claim, *deployment); !errors.Is(err, context.Canceled) || ready != nil || deployment.ReadinessPassed != nil {
		t.Fatalf("canceled readiness invented an outcome: %+v, %v", deployment, err)
	}
}

func TestStoppedCandidateBeforeReadinessDoesNotInventAProbe(t *testing.T) {
	for _, missing := range []bool{false, true} {
		w, claim, deployment, _ := readinessTestWorker(t)
		w.runtime = testRuntime{inspect: func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
			if missing {
				return runtime.Instance{}, runtime.ErrNotFound
			}
			got := workerTestContainer(spec, false)
			got.Status = "exited"
			return got, nil
		}}
		w.readiness = probeFunc(func(context.Context, health.Target) (health.Result, error) {
			t.Fatal("probed a missing or stopped candidate")
			return health.Result{}, nil
		})
		store := w.store.(testStore)
		store.beginHealth = func(context.Context, deployments.Claim) (deployments.Deployment, error) {
			t.Fatal("started readiness without a running candidate")
			return deployments.Deployment{}, nil
		}
		store.readiness = func(context.Context, deployments.Claim, string, deployments.ReadinessObservation) (deployments.Deployment, error) {
			t.Fatal("invented an HTTP observation")
			return deployments.Deployment{}, nil
		}
		store.fail = func(ctx context.Context, c deployments.Claim, stage, reason string) (deployments.Deployment, error) {
			if stage != deployments.StatusStarting || reason != "Application stopped before readiness completed." {
				t.Fatalf("wrong pre-probe failure: %s/%s", stage, reason)
			}
			deployment.Status = deployments.StatusFailed
			return *deployment, nil
		}
		w.store = store
		if ready, err := w.checkReadiness(t.Context(), claim, *deployment); err != nil || ready != nil || deployment.Status != deployments.StatusFailed || deployment.ReadinessPassed != nil {
			t.Fatalf("pre-probe failure invented readiness: %+v, %v", deployment, err)
		}
	}
}
