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
	"shipyard/apps/api/internal/source"
)

func startupTestWorker(t *testing.T, claim deployments.Claim, deployment deployments.Deployment) (*Worker, *deployments.RuntimeInstance) {
	t.Helper()
	instance := &deployments.RuntimeInstance{ID: *deployment.InstanceID, DeploymentID: claim.ID}
	w := &Worker{runtime: testRuntime{}, builder: testBuilder{cleanup: func(context.Context, builds.Input, string) error { return nil }}}
	w.store = testStore{
		instance: func(context.Context, string) (deployments.RuntimeInstance, error) { return *instance, nil },
		observe: func(ctx context.Context, got deployments.Claim, id string, observed deployments.Observation) (deployments.RuntimeInstance, error) {
			workerTestWrite(t, ctx, got, claim)
			if id != instance.ID || instance.ContainerID != nil && *instance.ContainerID != observed.ContainerID {
				t.Fatal("changed persisted runtime identity")
			}
			instance.ContainerID, instance.ObservedStatus, instance.Running, instance.ExitCode, instance.OOMKilled = &observed.ContainerID, &observed.Status, &observed.Running, &observed.ExitCode, &observed.OOMKilled
			instance.HostPort = nil
			if observed.Running {
				instance.HostPort = &observed.HostPort
			}
			return *instance, nil
		},
		fail: func(ctx context.Context, got deployments.Claim, stage, reason string) (deployments.Deployment, error) {
			workerTestWrite(t, ctx, got, claim)
			return deployment, nil
		},
	}
	return w, instance
}

func TestStartupPersistsCreatedAndRunningContainerWithPinnedIdentity(t *testing.T) {
	claim := workerTestClaim()
	deployment := workerTestStarting(claim, strings.Repeat("a", 40))
	w, saved := startupTestWorker(t, claim, deployment)
	var calls []string
	store := w.store.(testStore)
	observe := store.observe
	store.observe = func(ctx context.Context, c deployments.Claim, id string, o deployments.Observation) (deployments.RuntimeInstance, error) {
		calls = append(calls, "record "+o.Status)
		return observe(ctx, c, id, o)
	}
	w.store = store
	w.runtime = testRuntime{
		create: func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
			calls = append(calls, "create")
			if spec.ContainerID != "" || spec.InstanceID != *deployment.InstanceID || spec.ImageID != *deployment.ImageID || spec.CommitSHA != *deployment.CommitSHA {
				t.Fatalf("creation identity: %+v", spec)
			}
			return workerTestContainer(spec, false), nil
		},
		start: func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
			calls = append(calls, "start")
			if spec.ContainerID != strings.Repeat("c", 64) {
				t.Fatal("start did not pin the recorded Docker ID")
			}
			return workerTestContainer(spec, true), nil
		},
	}
	if started, err := w.startup(t.Context(), claim, deployment); err != nil || !started {
		t.Fatalf("started=%t error=%v", started, err)
	}
	want := "create,record created,start,record running"
	if strings.Join(calls, ",") != want || saved.ContainerID == nil || *saved.ContainerID != strings.Repeat("c", 64) || saved.Running == nil || !*saved.Running || saved.HostPort == nil || *saved.HostPort != 32123 {
		t.Fatalf("startup calls=%v state=%+v", calls, saved)
	}
}

func TestStartupStorageUncertaintyPreservesTheContainer(t *testing.T) {
	for _, stage := range []string{"created", "running"} {
		t.Run(stage, func(t *testing.T) {
			claim := workerTestClaim()
			deployment := workerTestStarting(claim, strings.Repeat("a", 40))
			w, _ := startupTestWorker(t, claim, deployment)
			uncertain := errors.New("observation commit response lost")
			store := w.store.(testStore)
			observe := store.observe
			store.observe = func(ctx context.Context, c deployments.Claim, id string, o deployments.Observation) (deployments.RuntimeInstance, error) {
				if o.Status == stage {
					return deployments.RuntimeInstance{}, uncertain
				}
				return observe(ctx, c, id, o)
			}
			store.fail = func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error) {
				t.Fatal("invented terminal state after uncertain observation")
				return deployment, nil
			}
			w.store = store
			started := false
			w.runtime = testRuntime{start: func(ctx context.Context, s runtime.Spec) (runtime.Instance, error) {
				started = true
				return workerTestContainer(s, true), nil
			}, inspect: func(context.Context, runtime.Spec) (runtime.Instance, error) {
				t.Fatal("cleaned uncertain persisted container")
				return runtime.Instance{}, nil
			}, remove: func(context.Context, runtime.Spec) error {
				t.Fatal("removed uncertain persisted container")
				return nil
			}}
			w.builder = testBuilder{cleanup: func(context.Context, builds.Input, string) error {
				t.Fatal("removed image after uncertain observation")
				return nil
			}}
			if ready, err := w.startup(t.Context(), claim, deployment); ready || !errors.Is(err, uncertain) || started != (stage == "running") {
				t.Fatalf("stage=%s started=%t error=%v", stage, started, err)
			}
		})
	}
}

func TestStartupKnownFailuresCleanOnlyTheRecordedContainer(t *testing.T) {
	for _, failure := range []string{"start error", "exit", "paused", "unsupported image"} {
		t.Run(failure, func(t *testing.T) {
			claim := workerTestClaim()
			deployment := workerTestStarting(claim, strings.Repeat("a", 40))
			w, saved := startupTestWorker(t, claim, deployment)
			failed, imageCleaned, removed := false, false, false
			store := w.store.(testStore)
			store.fail = func(ctx context.Context, c deployments.Claim, stage, reason string) (deployments.Deployment, error) {
				workerTestWrite(t, ctx, c, claim)
				if stage != deployments.StatusStarting || reason == "" || strings.Contains(reason, "private") || !imageCleaned {
					t.Fatalf("unsafe or premature failure: %s/%s", stage, reason)
				}
				failed = true
				return deployment, nil
			}
			w.store = store
			check := func(s runtime.Spec) {
				if s.ContainerID != strings.Repeat("c", 64) {
					t.Fatalf("cleanup did not pin full Docker ID: %+v", s)
				}
			}
			w.runtime = testRuntime{
				create: func(ctx context.Context, s runtime.Spec) (runtime.Instance, error) {
					if failure == "unsupported image" {
						return runtime.Instance{}, runtime.ErrUnsupportedImage
					}
					return workerTestContainer(s, false), nil
				},
				start: func(ctx context.Context, s runtime.Spec) (runtime.Instance, error) {
					check(s)
					if failure == "start error" {
						return runtime.Instance{}, errors.New("private OCI diagnostic")
					}
					got := workerTestContainer(s, true)
					if failure == "exit" {
						got.Running, got.Status, got.HostPort, got.ExitCode = false, "exited", 0, 7
					} else {
						got.Status = "paused"
					}
					return got, nil
				},
				inspect: func(ctx context.Context, s runtime.Spec) (runtime.Instance, error) {
					if failure == "unsupported image" {
						return runtime.Instance{}, runtime.ErrNotFound
					}
					check(s)
					return workerTestContainer(s, true), nil
				},
				stop: func(ctx context.Context, s runtime.Spec) (runtime.Instance, error) {
					check(s)
					got := workerTestContainer(s, false)
					got.Status = "exited"
					if failure == "exit" {
						got.ExitCode = 7
					}
					return got, nil
				},
				remove: func(ctx context.Context, s runtime.Spec) error {
					check(s)
					if saved.Running == nil || *saved.Running {
						t.Fatal("removed before storing stopped observation")
					}
					removed = true
					return nil
				},
			}
			w.builder = testBuilder{cleanup: func(ctx context.Context, input builds.Input, expectedID string) error {
				if input.DeploymentID != claim.ID || input.BuildID != claim.ID || expectedID != *deployment.ImageID || failure != "unsupported image" && !removed {
					t.Fatal("wrong image or image cleanup before container removal")
				}
				imageCleaned = true
				return nil
			}}
			if started, err := w.startup(t.Context(), claim, deployment); started || err != nil || !failed {
				t.Fatalf("failed=%t error=%v", failed, err)
			}
			if failure == "exit" && (saved.ExitCode == nil || *saved.ExitCode != 7) {
				t.Fatalf("startup lost the observed exit code: %+v", saved)
			}
		})
	}
}

func TestAmbiguousCreationAndCancellationRemainRecoverable(t *testing.T) {
	for _, problem := range []error{errors.New("create response lost"), context.DeadlineExceeded, context.Canceled} {
		claim := workerTestClaim()
		deployment := workerTestStarting(claim, strings.Repeat("a", 40))
		w, _ := startupTestWorker(t, claim, deployment)
		w.runtime = testRuntime{create: func(context.Context, runtime.Spec) (runtime.Instance, error) { return runtime.Instance{}, problem }, inspect: func(context.Context, runtime.Spec) (runtime.Instance, error) {
			t.Fatal("guessed creation outcome")
			return runtime.Instance{}, nil
		}}
		store := w.store.(testStore)
		store.fail = func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error) {
			t.Fatal("terminalized ambiguous creation")
			return deployment, nil
		}
		w.store = store
		if started, err := w.startup(t.Context(), claim, deployment); started || !errors.Is(err, problem) {
			t.Fatalf("lost create cause: %v", err)
		}
	}
}

func TestLeaseLossDuringStartupOrReadinessCancelsBeforeOwnershipRelease(t *testing.T) {
	for _, phase := range []string{"startup", "readiness"} {
		t.Run(phase, func(t *testing.T) {
			claim := workerTestClaim()
			sha := strings.Repeat("a", 40)
			deployment := workerTestStarting(claim, sha)
			w, _ := startupTestWorker(t, claim, deployment)
			entered, exited := make(chan struct{}), make(chan struct{})
			lost := errors.New("lease lost while starting")
			claimed := false
			store := w.store.(testStore)
			store.claim = func(context.Context) (deployments.Claim, error) {
				if claimed {
					t.Fatal("claimed after lease loss")
				}
				claimed = true
				return claim, nil
			}
			store.record = func(context.Context, deployments.Claim, string) (deployments.Deployment, error) {
				return claim.Deployment, nil
			}
			store.beginBuild = func(context.Context, deployments.Claim) (deployments.Deployment, error) { return claim.Deployment, nil }
			store.beginStart = func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error) {
				return deployment, nil
			}
			store.beginHealth = func(context.Context, deployments.Claim) (deployments.Deployment, error) {
				got := deployment
				got.Status = deployments.StatusHealthChecking
				return got, nil
			}
			store.readiness = func(context.Context, deployments.Claim, string, deployments.ReadinessObservation) (deployments.Deployment, error) {
				t.Fatal("persisted canceled readiness")
				return deployment, nil
			}
			store.fail = func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error) {
				t.Fatal("terminalized canceled startup")
				return deployment, nil
			}
			w.store = store
			w.fetcher = fetchFunc(func(context.Context, string, string) (*source.Checkout, error) {
				return &source.Checkout{Path: t.TempDir(), CommitSHA: sha}, nil
			})
			w.builder = testBuilder{build: func(context.Context, builds.Input) (builds.Image, error) { return workerTestImage(claim.ID), nil }}
			w.runtime = testRuntime{start: func(ctx context.Context, s runtime.Spec) (runtime.Instance, error) {
				close(entered)
				<-ctx.Done()
				close(exited)
				return runtime.Instance{}, ctx.Err()
			}}
			if phase == "readiness" {
				w.runtime = testRuntime{inspect: func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
					return workerTestContainer(spec, true), nil
				}}
				w.readiness = probeFunc(func(ctx context.Context, target health.Target) (health.Result, error) {
					close(entered)
					<-ctx.Done()
					close(exited)
					return health.Result{}, ctx.Err()
				})
			}
			w.acquire = func(context.Context) (lease, error) {
				return testLease{check: func(ctx context.Context) error {
					select {
					case <-entered:
						return lost
					case <-ctx.Done():
						return ctx.Err()
					}
				}, close: func() error {
					select {
					case <-exited:
					default:
						t.Error("released ownership before Docker call returned")
					}
					return nil
				}}, nil
			}
			w.pollInterval, w.checkInterval = time.Millisecond, time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := w.Run(ctx); !errors.Is(err, lost) {
				t.Fatal(err)
			}
		})
	}
}

func TestStartupRecoveryPinsCandidatesAndRecordsStopBeforeRemoval(t *testing.T) {
	for _, outcome := range []string{"unrecorded candidate", "recorded candidate", "missing candidate", "missing at stop"} {
		t.Run(outcome, func(t *testing.T) {
			claim := workerTestClaim()
			claim.Deployment = workerTestStarting(claim, strings.Repeat("a", 40))
			w, saved := startupTestWorker(t, claim, claim.Deployment)
			id := strings.Repeat("c", 64)
			if outcome != "unrecorded candidate" {
				saved.ContainerID = &id
			}
			var calls []string
			store := w.store.(testStore)
			observe := store.observe
			store.interrupted = func(context.Context) (deployments.Claim, error) {
				if len(calls) > 0 && calls[len(calls)-1] == "fail" {
					return deployments.Claim{}, deployments.ErrNoInterruptedDeployments
				}
				return claim, nil
			}
			store.observe = func(ctx context.Context, c deployments.Claim, instanceID string, o deployments.Observation) (deployments.RuntimeInstance, error) {
				if o.Running || o.Status != "exited" || o.ContainerID != id {
					t.Fatalf("wrong stopped observation: %+v", o)
				}
				calls = append(calls, "record stopped")
				return observe(ctx, c, instanceID, o)
			}
			store.fail = func(ctx context.Context, c deployments.Claim, stage, reason string) (deployments.Deployment, error) {
				workerTestWrite(t, ctx, c, claim)
				if stage != deployments.StatusStarting {
					t.Fatal("restart failed wrong stage")
				}
				calls = append(calls, "fail")
				return claim.Deployment, nil
			}
			w.store = store
			pinned := func(spec runtime.Spec) {
				if spec.ContainerID != id {
					t.Fatalf("cleanup must pin discovered full ID: %+v", spec)
				}
			}
			w.runtime = testRuntime{
				inspect: func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
					calls = append(calls, "inspect")
					if (outcome == "unrecorded candidate") != (spec.ContainerID == "") {
						t.Fatalf("lost persisted identity: %+v", spec)
					}
					if outcome == "missing candidate" {
						return runtime.Instance{}, runtime.ErrNotFound
					}
					return workerTestContainer(spec, true), nil
				},
				stop: func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
					pinned(spec)
					calls = append(calls, "stop")
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) < 5*time.Second || time.Until(deadline) > 10*time.Second {
						t.Fatal("stop budget does not cover the five second grace period")
					}
					if outcome == "missing at stop" {
						return runtime.Instance{}, runtime.ErrNotFound
					}
					got := workerTestContainer(spec, false)
					got.Status = "exited"
					return got, nil
				},
				remove: func(ctx context.Context, spec runtime.Spec) error {
					pinned(spec)
					calls = append(calls, "remove")
					return nil
				},
			}
			w.builder = testBuilder{cleanup: func(ctx context.Context, input builds.Input, expectedID string) error {
				if input.DeploymentID != claim.ID || input.BuildID != claim.ID || input.CommitSHA != *claim.CommitSHA || expectedID != *claim.ImageID {
					t.Fatal("cleanup lost build identity")
				}
				calls = append(calls, "image")
				return nil
			}}
			if err := w.recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			want := "inspect,stop,record stopped,remove,image,fail"
			if outcome == "missing candidate" {
				want = "inspect,image,fail"
			}
			if outcome == "missing at stop" {
				want = "inspect,stop,remove,image,fail"
			}
			if strings.Join(calls, ",") != want {
				t.Fatalf("cleanup calls=%v want=%s", calls, want)
			}
		})
	}
}

func TestStartupRecoveryUncertaintyStopsBeforeFurtherMutation(t *testing.T) {
	for _, problem := range []string{"foreign", "unsafe", "inspect", "stop", "still running", "record stopped", "remove"} {
		t.Run(problem, func(t *testing.T) {
			claim := workerTestClaim()
			claim.Deployment = workerTestStarting(claim, strings.Repeat("a", 40))
			w, saved := startupTestWorker(t, claim, claim.Deployment)
			id := strings.Repeat("c", 64)
			saved.ContainerID = &id
			uncertain := errors.New("cleanup outcome uncertain")
			store := w.store.(testStore)
			observe := store.observe
			store.interrupted = func(context.Context) (deployments.Claim, error) { return claim, nil }
			store.claim = func(context.Context) (deployments.Claim, error) {
				t.Fatal("claimed new work after uncertain recovery")
				return deployments.Claim{}, nil
			}
			store.fail = func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error) {
				t.Fatal("terminalized uncertain cleanup")
				return claim.Deployment, nil
			}
			store.observe = func(ctx context.Context, c deployments.Claim, instanceID string, o deployments.Observation) (deployments.RuntimeInstance, error) {
				if problem == "record stopped" {
					return deployments.RuntimeInstance{}, uncertain
				}
				return observe(ctx, c, instanceID, o)
			}
			w.store = store
			pinned := func(spec runtime.Spec) {
				if spec.ContainerID != id {
					t.Fatal("replaced persisted container identity")
				}
			}
			w.runtime = testRuntime{
				inspect: func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
					pinned(spec)
					switch problem {
					case "foreign":
						return runtime.Instance{}, runtime.ErrNotOwned
					case "unsafe":
						return runtime.Instance{}, runtime.ErrUnsafeContainer
					case "inspect":
						return runtime.Instance{}, uncertain
					}
					return workerTestContainer(spec, true), nil
				},
				stop: func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
					pinned(spec)
					if problem == "foreign" || problem == "unsafe" || problem == "inspect" {
						t.Fatal("stopped uncertain or foreign container")
					}
					if problem == "stop" {
						return runtime.Instance{}, uncertain
					}
					got := workerTestContainer(spec, problem == "still running")
					if !got.Running {
						got.Status = "exited"
					}
					return got, nil
				},
				remove: func(ctx context.Context, spec runtime.Spec) error {
					pinned(spec)
					if problem != "remove" {
						t.Fatal("removed container before safe stopped observation")
					}
					return uncertain
				},
			}
			w.builder = testBuilder{cleanup: func(context.Context, builds.Input, string) error {
				t.Fatal("removed image before successful container cleanup")
				return nil
			}}
			w.acquire = func(context.Context) (lease, error) { return testLease{}, nil }
			w.pollInterval, w.checkInterval = time.Millisecond, time.Millisecond
			if err := w.Run(t.Context()); err == nil {
				t.Fatal("accepted uncertain startup cleanup")
			}
		})
	}
}
