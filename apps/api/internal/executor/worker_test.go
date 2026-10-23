package executor

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"shipyard/apps/api/internal/builds"
	"shipyard/apps/api/internal/deployments"
	"shipyard/apps/api/internal/runtime"
	"shipyard/apps/api/internal/source"
)

type fetchFunc func(context.Context, string, string) (*source.Checkout, error)

func (f fetchFunc) Fetch(ctx context.Context, repository, branch string) (*source.Checkout, error) {
	return f(ctx, repository, branch)
}

type testBuilder struct {
	build   func(context.Context, builds.Input) (builds.Image, error)
	cleanup func(context.Context, builds.Input, string) error
}

func (b testBuilder) Build(ctx context.Context, input builds.Input) (builds.Image, error) {
	return b.build(ctx, input)
}
func (b testBuilder) Cleanup(ctx context.Context, input builds.Input, expectedID string) error {
	return b.cleanup(ctx, input, expectedID)
}

type inspectFunc func(context.Context, runtime.Spec) (runtime.Instance, error)

func (f inspectFunc) Inspect(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
	return f(ctx, spec)
}

type testStore struct {
	claim       func(context.Context) (deployments.Claim, error)
	interrupted func(context.Context) (deployments.Claim, error)
	record      func(context.Context, deployments.Claim, string) (deployments.Deployment, error)
	beginBuild  func(context.Context, deployments.Claim) (deployments.Deployment, error)
	beginStart  func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error)
	instance    func(context.Context, string) (deployments.RuntimeInstance, error)
	fail        func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error)
}

func (s testStore) ClaimNext(ctx context.Context) (deployments.Claim, error) {
	if s.claim != nil {
		return s.claim(ctx)
	}
	return deployments.Claim{}, deployments.ErrNoQueuedDeployments
}
func (s testStore) NextInterrupted(ctx context.Context) (deployments.Claim, error) {
	if s.interrupted != nil {
		return s.interrupted(ctx)
	}
	return deployments.Claim{}, deployments.ErrNoInterruptedDeployments
}
func (s testStore) RecordSourceCommit(ctx context.Context, claim deployments.Claim, sha string) (deployments.Deployment, error) {
	return s.record(ctx, claim, sha)
}
func (s testStore) BeginBuild(ctx context.Context, claim deployments.Claim) (deployments.Deployment, error) {
	return s.beginBuild(ctx, claim)
}
func (s testStore) BeginStart(ctx context.Context, claim deployments.Claim, reference, id string) (deployments.Deployment, error) {
	return s.beginStart(ctx, claim, reference, id)
}
func (s testStore) GetInstance(ctx context.Context, id string) (deployments.RuntimeInstance, error) {
	return s.instance(ctx, id)
}
func (s testStore) FailExecution(ctx context.Context, claim deployments.Claim, stage, reason string) (deployments.Deployment, error) {
	return s.fail(ctx, claim, stage, reason)
}

type testLease struct {
	check func(context.Context) error
	close func() error
}

func (l testLease) Check(ctx context.Context) error {
	if l.check != nil {
		return l.check(ctx)
	}
	return nil
}
func (l testLease) Close() error {
	if l.close != nil {
		return l.close()
	}
	return nil
}

func workerTestClaim() deployments.Claim {
	return deployments.Claim{Deployment: deployments.Deployment{ID: "00000000-0000-4000-8000-000000000061", ProjectID: "00000000-0000-4000-8000-000000000062", GitURL: "https://example.com/old.git", Branch: "release/old", AppPort: 8080, Status: deployments.StatusFetchingSource}, Token: "private-owner"}
}

func workerTestImage(id string) builds.Image {
	return builds.Image{Reference: "shipyard/deployments:" + id + "-" + strings.ReplaceAll(id, "-", ""), ID: "sha256:" + strings.Repeat("b", 64)}
}

func workerTestWrite(t *testing.T, ctx context.Context, got, want deployments.Claim) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 3*time.Second || got.ID != want.ID || got.Token != want.Token {
		t.Fatal("unbounded write or changed claim ownership")
	}
}

func TestBuildPipelineUsesSnapshotAndStopsOnStorageUncertainty(t *testing.T) {
	for _, failedWrite := range []string{"", "record", "begin build", "begin start"} {
		t.Run(failedWrite, func(t *testing.T) {
			claim, sha, path := workerTestClaim(), strings.Repeat("a", 40), t.TempDir()
			image, storageError := workerTestImage(claim.ID), errors.New("storage write lost")
			var calls []string
			write := func(ctx context.Context, got deployments.Claim, name string) (deployments.Deployment, error) {
				workerTestWrite(t, ctx, got, claim)
				calls = append(calls, name)
				if name == failedWrite {
					return deployments.Deployment{}, storageError
				}
				return claim.Deployment, nil
			}
			w := &Worker{fetcher: fetchFunc(func(ctx context.Context, repository, branch string) (*source.Checkout, error) {
				calls = append(calls, "fetch")
				if repository != claim.GitURL || branch != claim.Branch {
					t.Fatal("did not use queued source snapshot")
				}
				return &source.Checkout{Path: path, CommitSHA: sha}, nil
			}), store: testStore{
				record: func(ctx context.Context, got deployments.Claim, commit string) (deployments.Deployment, error) {
					if commit != sha {
						t.Fatal("did not save exact fetched commit")
					}
					return write(ctx, got, "record")
				},
				beginBuild: func(ctx context.Context, got deployments.Claim) (deployments.Deployment, error) {
					return write(ctx, got, "begin build")
				},
				beginStart: func(ctx context.Context, got deployments.Claim, reference, id string) (deployments.Deployment, error) {
					if reference != image.Reference || id != image.ID {
						t.Fatal("did not save immutable build result")
					}
					return write(ctx, got, "begin start")
				},
			}, builder: testBuilder{build: func(ctx context.Context, input builds.Input) (builds.Image, error) {
				calls = append(calls, "build")
				if input.CheckoutPath != path || input.DeploymentID != claim.ID || input.CommitSHA != sha || input.BuildID != claim.ID {
					t.Fatalf("incorrect build input: %+v", input)
				}
				if _, err := os.Stat(input.CheckoutPath); err != nil {
					t.Fatal("closed source before build finished")
				}
				return image, nil
			}}}
			err := w.execute(t.Context(), claim)
			want := []string{"fetch", "record", "begin build", "build", "begin start"}
			if failedWrite != "" {
				if !errors.Is(err, storageError) {
					t.Fatalf("lost storage error: %v", err)
				}
				for index, call := range want {
					if call == failedWrite {
						want = want[:index+1]
						break
					}
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls=%v, want %v", calls, want)
			}
		})
	}
}

func TestSourceFailuresPersistOnlyPublicReasons(t *testing.T) {
	for _, sourceError := range []error{errors.New("private Git diagnostic"), source.ErrNotAllowed, context.DeadlineExceeded} {
		w := &Worker{fetcher: fetchFunc(func(context.Context, string, string) (*source.Checkout, error) { return nil, sourceError }), store: testStore{fail: func(ctx context.Context, claim deployments.Claim, stage, reason string) (deployments.Deployment, error) {
			if stage != deployments.StatusFetchingSource || reason == "" || strings.Contains(reason, "private") {
				t.Fatalf("unsafe failure: %s/%s", stage, reason)
			}
			return deployments.Deployment{}, nil
		}}}
		if err := w.execute(t.Context(), workerTestClaim()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildFailuresCleanExactAttemptBeforeSavingPublicReason(t *testing.T) {
	for _, failedStep := range []string{"", "cleanup", "failure write"} {
		for _, buildError := range []error{errors.New("private Docker diagnostic"), context.DeadlineExceeded} {
			t.Run(failedStep+"/"+buildError.Error(), func(t *testing.T) {
				claim, sha := workerTestClaim(), strings.Repeat("a", 40)
				fatalError := errors.New("cleanup or storage uncertain")
				var calls []string
				w := &Worker{fetcher: fetchFunc(func(context.Context, string, string) (*source.Checkout, error) {
					return &source.Checkout{Path: t.TempDir(), CommitSHA: sha}, nil
				}), store: testStore{
					record: func(context.Context, deployments.Claim, string) (deployments.Deployment, error) {
						return claim.Deployment, nil
					},
					beginBuild: func(context.Context, deployments.Claim) (deployments.Deployment, error) { return claim.Deployment, nil },
					fail: func(ctx context.Context, got deployments.Claim, stage, reason string) (deployments.Deployment, error) {
						workerTestWrite(t, ctx, got, claim)
						calls = append(calls, "fail")
						if stage != deployments.StatusBuilding || reason == "" || strings.Contains(reason, "private") {
							t.Fatalf("unsafe build failure: %s/%s", stage, reason)
						}
						if failedStep == "failure write" {
							return deployments.Deployment{}, fatalError
						}
						return deployments.Deployment{}, nil
					},
				}, builder: testBuilder{
					build: func(context.Context, builds.Input) (builds.Image, error) {
						calls = append(calls, "build")
						return builds.Image{}, buildError
					},
					cleanup: func(ctx context.Context, input builds.Input, expectedID string) error {
						calls = append(calls, "cleanup")
						if input.DeploymentID != claim.ID || input.CommitSHA != sha || input.BuildID != claim.ID || expectedID != "" || ctx.Err() != nil {
							t.Fatalf("incorrect cleanup identity/context: %+v, %v", input, ctx.Err())
						}
						if failedStep == "cleanup" {
							return fatalError
						}
						return nil
					},
				}}
				err := w.execute(t.Context(), claim)
				want := []string{"build", "cleanup", "fail"}
				if failedStep == "cleanup" {
					want = want[:2]
				}
				if !reflect.DeepEqual(calls, want) || failedStep == "" && err != nil || failedStep != "" && !errors.Is(err, fatalError) {
					t.Fatalf("calls=%v, error=%v", calls, err)
				}
			})
		}
	}
}

func TestLeaseLossCancelsWorkBeforeReleasingOwnership(t *testing.T) {
	for _, phase := range []string{"fetch", "build"} {
		t.Run(phase, func(t *testing.T) {
			entered, exited := make(chan struct{}), make(chan struct{})
			leaseError := errors.New("lease connection lost")
			closed, claimed := false, false
			claim := workerTestClaim()
			wait := func(ctx context.Context) error { close(entered); <-ctx.Done(); close(exited); return ctx.Err() }
			w := &Worker{pollInterval: time.Millisecond, checkInterval: time.Millisecond, store: testStore{
				claim: func(context.Context) (deployments.Claim, error) {
					if claimed {
						t.Fatal("claimed after ownership loss")
					}
					claimed = true
					return claim, nil
				},
				record: func(context.Context, deployments.Claim, string) (deployments.Deployment, error) {
					return claim.Deployment, nil
				},
				beginBuild: func(context.Context, deployments.Claim) (deployments.Deployment, error) { return claim.Deployment, nil },
			}, fetcher: fetchFunc(func(ctx context.Context, _ string, _ string) (*source.Checkout, error) {
				if phase == "fetch" {
					return nil, wait(ctx)
				}
				return &source.Checkout{Path: t.TempDir(), CommitSHA: strings.Repeat("a", 40)}, nil
			}),
				builder: testBuilder{build: func(ctx context.Context, input builds.Input) (builds.Image, error) { return builds.Image{}, wait(ctx) }},
				acquire: func(context.Context) (lease, error) {
					return testLease{check: func(ctx context.Context) error {
						select {
						case <-entered:
							return leaseError
						case <-ctx.Done():
							return ctx.Err()
						}
					}, close: func() error {
						select {
						case <-exited:
						default:
							t.Error("released ownership before work exited")
						}
						closed = true
						return nil
					}}, nil
				},
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := w.Run(ctx); !errors.Is(err, leaseError) || !closed {
				t.Fatalf("closed=%t error=%v", closed, err)
			}
		})
	}
}

func TestRecoveryCleansOnlySafeInterruptedStages(t *testing.T) {
	for _, stage := range []string{deployments.StatusFetchingSource, deployments.StatusBuilding, deployments.StatusStarting} {
		t.Run(stage, func(t *testing.T) {
			claim, sha, instanceID := workerTestClaim(), strings.Repeat("a", 40), "00000000-0000-4000-8000-000000000063"
			image := workerTestImage(claim.ID)
			claim.Status, claim.CommitSHA = stage, &sha
			claim.ImageReference, claim.ImageID, claim.InstanceID = &image.Reference, &image.ID, &instanceID
			failed, cleaned, inspected := false, false, false
			w := &Worker{store: testStore{
				interrupted: func(context.Context) (deployments.Claim, error) {
					if failed {
						return deployments.Claim{}, deployments.ErrNoInterruptedDeployments
					}
					return claim, nil
				},
				instance: func(ctx context.Context, id string) (deployments.RuntimeInstance, error) {
					if id != instanceID {
						t.Fatal("read different instance")
					}
					return deployments.RuntimeInstance{ID: instanceID, DeploymentID: claim.ID}, nil
				},
				fail: func(ctx context.Context, got deployments.Claim, gotStage, reason string) (deployments.Deployment, error) {
					workerTestWrite(t, ctx, got, claim)
					if gotStage != stage || reason == "" || cleaned != (stage != deployments.StatusFetchingSource) || inspected != (stage == deployments.StatusStarting) {
						t.Fatal("failed wrong stage or before safe cleanup")
					}
					failed = true
					return deployments.Deployment{}, nil
				},
			}, builder: testBuilder{cleanup: func(ctx context.Context, input builds.Input, expectedID string) error {
				if input.DeploymentID != claim.ID || input.CommitSHA != sha || input.BuildID != claim.ID || stage == deployments.StatusStarting && expectedID != image.ID || stage != deployments.StatusStarting && expectedID != "" {
					t.Fatalf("cleanup identity: %+v / %s", input, expectedID)
				}
				cleaned = true
				return nil
			}},
				inspector: inspectFunc(func(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
					if spec.DeploymentID != claim.ID || spec.ProjectID != claim.ProjectID || spec.InstanceID != instanceID || spec.ImageID != image.ID || spec.CommitSHA != sha || spec.AppPort != claim.AppPort {
						t.Fatalf("inspection identity: %+v", spec)
					}
					inspected = true
					return runtime.Instance{}, runtime.ErrNotFound
				}),
			}
			if err := w.recover(t.Context()); err != nil || !failed {
				t.Fatalf("failed=%t error=%v", failed, err)
			}
		})
	}
}

func TestRecoveryUncertaintyDoesNotMutateOrClaimWork(t *testing.T) {
	for _, problem := range []string{"later stage", "missing SHA", "missing image", "missing instance", "instance read", "different deployment", "observed container", "existing container", "foreign container", "inspect error", "cleanup"} {
		t.Run(problem, func(t *testing.T) {
			claim, sha, instanceID := workerTestClaim(), strings.Repeat("a", 40), "00000000-0000-4000-8000-000000000063"
			image := workerTestImage(claim.ID)
			claim.Status, claim.CommitSHA = deployments.StatusStarting, &sha
			claim.ImageReference, claim.ImageID, claim.InstanceID = &image.Reference, &image.ID, &instanceID
			instance := deployments.RuntimeInstance{ID: instanceID, DeploymentID: claim.ID}
			switch problem {
			case "later stage":
				claim.Status = deployments.StatusHealthChecking
			case "missing SHA":
				claim.Status, claim.CommitSHA = deployments.StatusBuilding, nil
			case "missing image":
				claim.ImageID = nil
			case "missing instance":
				claim.InstanceID = nil
			case "different deployment":
				instance.DeploymentID = "another-attempt"
			case "observed container":
				containerID := strings.Repeat("c", 64)
				instance.ContainerID = &containerID
			}
			w := &Worker{pollInterval: time.Millisecond, checkInterval: time.Millisecond, store: testStore{
				interrupted: func(context.Context) (deployments.Claim, error) { return claim, nil },
				instance: func(context.Context, string) (deployments.RuntimeInstance, error) {
					if problem == "instance read" {
						return deployments.RuntimeInstance{}, errors.New("storage unavailable")
					}
					return instance, nil
				},
				claim: func(context.Context) (deployments.Claim, error) {
					t.Fatal("claimed after unsafe recovery")
					return deployments.Claim{}, nil
				},
				fail: func(context.Context, deployments.Claim, string, string) (deployments.Deployment, error) {
					t.Fatal("mutated uncertain recovery")
					return deployments.Deployment{}, nil
				},
			}, builder: testBuilder{cleanup: func(context.Context, builds.Input, string) error {
				if problem != "cleanup" {
					t.Fatal("touched image before proving recovery safe")
				}
				return errors.New("cleanup uncertain")
			}},
				inspector: inspectFunc(func(context.Context, runtime.Spec) (runtime.Instance, error) {
					switch problem {
					case "existing container":
						return runtime.Instance{ID: strings.Repeat("c", 64), Status: "exited"}, nil
					case "foreign container":
						return runtime.Instance{}, runtime.ErrNotOwned
					case "inspect error":
						return runtime.Instance{}, errors.New("Docker unavailable")
					}
					return runtime.Instance{}, runtime.ErrNotFound
				}),
				acquire: func(context.Context) (lease, error) { return testLease{}, nil },
			}
			if err := w.Run(t.Context()); err == nil {
				t.Fatal("accepted unsafe recovery")
			}
		})
	}
}

func TestWorkerBusyCancellationAndClose(t *testing.T) {
	w := &Worker{acquire: func(context.Context) (lease, error) { return nil, ErrExecutorBusy }}
	if err := w.Run(t.Context()); !errors.Is(err, ErrExecutorBusy) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	w.fetcher = fetchFunc(func(context.Context, string, string) (*source.Checkout, error) {
		cancel()
		return nil, context.Canceled
	})
	if err := w.execute(ctx, workerTestClaim()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closeError := errors.New("close Docker clients")
	w.close = func() error { return closeError }
	if err := w.Close(); !errors.Is(err, closeError) {
		t.Fatalf("lost client close error: %v", err)
	}
}
