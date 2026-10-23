package runtime

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	dockerimage "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestManagerCreateUsesBoundedOwnedConfigurationAndAdoptsRetries(t *testing.T) {
	m, f, spec := runtimeTestManager(t)
	spec.InstanceID = strings.ToUpper(spec.InstanceID)
	got, err := m.Create(t.Context(), spec)
	if err != nil || got.ID != f.container.ID || got.Name != "shipyard-"+strings.ToLower(spec.InstanceID) || got.Running {
		t.Fatalf("create = %+v, %v", got, err)
	}
	options := f.created
	config, host := options.Config, options.HostConfig
	if options.Name != got.Name || (options.Image != spec.ImageID && config.Image != spec.ImageID) || f.imageInspected != spec.ImageID {
		t.Errorf("creation did not use deterministic name and immutable image: %+v", options)
	}
	if (options.Image == "") == (config.Image == "") {
		t.Error("SDK requires exactly one of Image and Config.Image")
	}
	for key, want := range runtimeTestLabels(spec) {
		if config.Labels[key] != want {
			t.Errorf("ownership label %s = %q, want %q", key, config.Labels[key], want)
		}
	}
	if config.Healthcheck == nil || !slices.Equal(config.Healthcheck.Test, []string{"NONE"}) || config.StopSignal != "SIGTERM" || config.StopTimeout == nil || *config.StopTimeout != 5 {
		t.Errorf("image health/shutdown defaults not overridden: %+v", config)
	}
	if config.Tty || config.OpenStdin || config.AttachStdin || len(config.Env) != 0 || len(config.Volumes) != 0 {
		t.Error("creation enables stdin/TTY or supplies environment/volumes")
	}
	if f.container.Config.User != "10001" || config.Entrypoint != nil || config.Cmd != nil {
		t.Error("creation overrides the image user or application command")
	}
	if host.NanoCPUs != 1e9 || host.Memory != 256<<20 || host.MemorySwap != host.Memory || host.PidsLimit == nil || *host.PidsLimit != 64 {
		t.Errorf("resource limits: %+v", host.Resources)
	}
	if host.Privileged || host.ReadonlyRootfs || host.AutoRemove || !host.RestartPolicy.IsNone() || len(host.Binds)+len(host.Mounts)+len(host.VolumesFrom)+len(host.Devices)+len(host.DeviceRequests)+len(host.CapAdd) != 0 {
		t.Error("unsafe or incompatible host options")
	}
	if !slices.Contains(host.CapDrop, "ALL") || !(slices.Contains(host.SecurityOpt, "no-new-privileges=true") || slices.Contains(host.SecurityOpt, "no-new-privileges:true") || slices.Contains(host.SecurityOpt, "no-new-privileges")) || host.NetworkMode != "bridge" || host.PublishAllPorts {
		t.Errorf("capability/network policy: %+v", host)
	}
	bindings := host.PortBindings[runtimeTestPort(spec)]
	if len(host.PortBindings) != 1 || len(bindings) != 1 || bindings[0].HostIP != netip.MustParseAddr("127.0.0.1") || bindings[0].HostPort != "" && bindings[0].HostPort != "0" {
		t.Errorf("publication must be one loopback ephemeral TCP port: %+v", host.PortBindings)
	}
	if host.LogConfig.Type != "local" || host.LogConfig.Config["max-size"] != "10m" || host.LogConfig.Config["max-file"] != "3" {
		t.Errorf("unbounded logging: %+v", host.LogConfig)
	}
	repeated, err := m.Create(t.Context(), spec)
	if err != nil || repeated.ID != got.ID || f.createdCount != 1 {
		t.Errorf("retry created another instance: %+v, %v, count %d", repeated, err, f.createdCount)
	}
}

func TestManagerLifecycleUsesFullIDsAndActualObservedState(t *testing.T) {
	m, f, spec := runtimeTestExisting(t)
	f.exitImmediately = true
	got, err := m.Start(t.Context(), spec)
	if err != nil || got.Status != "exited" || got.Running || got.ExitCode != 7 || !got.OOMKilled {
		t.Fatalf("immediate exit was presented as success/running: %+v, %v", got, err)
	}
	got, err = m.Inspect(t.Context(), spec)
	if err != nil || got.Status != "exited" || got.ExitCode != 7 {
		t.Fatalf("inspect actual exit: %+v, %v", got, err)
	}
	f.exitImmediately = false
	got, err = m.Start(t.Context(), spec)
	if err != nil || !got.Running || got.HostPort != 32123 {
		t.Fatalf("running instance/publication = %+v, %v", got, err)
	}
	got, err = m.Stop(t.Context(), spec)
	if err != nil || got.Running || got.HostPort != 0 || f.stop.Timeout == nil || *f.stop.Timeout != 5 || f.stop.Signal != "SIGTERM" {
		t.Fatalf("bounded stop = %+v, %v, options %+v", got, err, f.stop)
	}
	if err := m.Remove(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(t.Context(), spec); err != nil || len(f.removed) != 1 {
		t.Errorf("missing removal is not idempotent: %v", err)
	}
	if _, err := m.Inspect(t.Context(), spec); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing inspect: %v", err)
	}
}

func TestManagerRejectsForeignContainersAndAllowsSafeDriftCleanup(t *testing.T) {
	for _, operation := range []string{"create", "start", "inspect", "stop", "remove"} {
		t.Run(operation, func(t *testing.T) {
			m, f, spec := runtimeTestExisting(t)
			f.container.Config.Labels["io.shipyard.instance_id"] = "another-instance"
			var err error
			switch operation {
			case "create":
				_, err = m.Create(t.Context(), spec)
			case "start":
				_, err = m.Start(t.Context(), spec)
			case "inspect":
				_, err = m.Inspect(t.Context(), spec)
			case "stop":
				_, err = m.Stop(t.Context(), spec)
			case "remove":
				err = m.Remove(t.Context(), spec)
			}
			if !errors.Is(err, ErrNotOwned) || f.started != 0 || f.stopped != 0 || len(f.removed) != 0 {
				t.Errorf("foreign container was accepted/mutated: %v", err)
			}
		})
	}
	for name, change := range map[string]func(*runtimeTestEngine){
		"privileged": func(f *runtimeTestEngine) { f.container.HostConfig.Privileged = true },
		"extra security option": func(f *runtimeTestEngine) {
			f.container.HostConfig.SecurityOpt = append(f.container.HostConfig.SecurityOpt, "seccomp=unconfined")
		},
		"fixed host port": func(f *runtimeTestEngine) {
			f.container.HostConfig.PortBindings[runtimeTestPort(f.spec)][0].HostPort = "43210"
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, f, spec := runtimeTestExisting(t)
			change(f)
			if _, err := m.Create(t.Context(), spec); !errors.Is(err, ErrUnsafeContainer) {
				t.Errorf("adopt unsafe container: %v", err)
			}
			if _, err := m.Start(t.Context(), spec); !errors.Is(err, ErrUnsafeContainer) || f.started != 0 {
				t.Errorf("start unsafe container: %v", err)
			}
			f.container.State.Running, f.container.State.Status = true, "running"
			f.version = client.ServerVersionResult{Version: "27.5.1", Os: "linux"}
			if _, err := m.Stop(t.Context(), spec); err != nil {
				t.Fatalf("cannot stop owned drifted container: %v", err)
			}
			if err := m.Remove(t.Context(), spec); err != nil {
				t.Fatalf("cannot remove owned drifted container: %v", err)
			}
		})
	}
}

func TestManagerRejectsDifferentContainerBehindPinnedIdentity(t *testing.T) {
	for _, operation := range []string{"create", "start", "inspect", "stop", "remove"} {
		t.Run(operation, func(t *testing.T) {
			m, f, spec := runtimeTestExisting(t)
			spec.ContainerID = strings.Repeat("b", 64)
			var err error
			switch operation {
			case "create":
				_, err = m.Create(t.Context(), spec)
			case "start":
				_, err = m.Start(t.Context(), spec)
			case "inspect":
				_, err = m.Inspect(t.Context(), spec)
			case "stop":
				_, err = m.Stop(t.Context(), spec)
			case "remove":
				err = m.Remove(t.Context(), spec)
			}
			if !errors.Is(err, ErrNotOwned) || f.createdCount != 1 || f.started != 0 || f.stopped != 0 || len(f.removed) != 0 {
				t.Fatalf("replacement container accepted or mutated: %v", err)
			}
		})
	}
}

func TestManagerPinnedCreateOnlyAdoptsTheRecordedContainer(t *testing.T) {
	m, f, spec := runtimeTestExisting(t)
	spec.ContainerID = f.container.ID
	if got, err := m.Create(t.Context(), spec); err != nil || got.ID != spec.ContainerID || f.createdCount != 1 {
		t.Fatalf("adopt pinned container: %+v, %v", got, err)
	}
	if got, err := m.Start(t.Context(), spec); err != nil || got.ID != spec.ContainerID || !got.Running {
		t.Fatalf("start pinned container: %+v, %v", got, err)
	}
	if got, err := m.Stop(t.Context(), spec); err != nil || got.ID != spec.ContainerID || got.Running {
		t.Fatalf("stop pinned container: %+v, %v", got, err)
	}
	if err := m.Remove(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(t.Context(), spec); !errors.Is(err, ErrNotFound) || f.createdCount != 1 {
		t.Fatalf("missing pinned container was recreated: %v", err)
	}
}

func TestManagerRejectsMalformedPinnedContainerIDsBeforeDocker(t *testing.T) {
	for _, id := range []string{"short-id", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), "sha256:" + strings.Repeat("a", 64)} {
		m, f, spec := runtimeTestManager(t)
		spec.ContainerID = id
		if _, err := m.Start(t.Context(), spec); !errors.Is(err, ErrInvalidInput) || f.calls != 0 {
			t.Errorf("invalid container pin reached Docker: %v", err)
		}
	}
}

func TestManagerFailedStartCleanupDoesNotKillAmbiguousRunningInstance(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(strconv.FormatBool(running), func(t *testing.T) {
			m, f, spec := runtimeTestExisting(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.startHook = func() error {
				f.container.State.Running = running
				if running {
					f.container.State.Status = "running"
				}
				cancel()
				return context.Canceled
			}
			if _, err := m.Start(ctx, spec); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled start lost cause: %v", err)
			}
			want := 1
			if running {
				want = 0
			}
			if len(f.removed) != want {
				t.Errorf("running=%t: %d removals, want %d", running, len(f.removed), want)
			}
			if len(f.inspected) == 0 || f.inspected[len(f.inspected)-1] != strings.Repeat("a", 64) {
				t.Error("failed-start cleanup re-inspected a mutable name")
			}
		})
	}
}

func TestManagerRejectsUnsupportedImagesAndCleansFailedCreation(t *testing.T) {
	for _, change := range []func(*runtimeTestEngine){
		func(f *runtimeTestEngine) { f.image.Config.Volumes = map[string]struct{}{"/data": {}} },
		func(f *runtimeTestEngine) { f.image.Config.Labels["io.shipyard.commit_sha"] = "different" },
		func(f *runtimeTestEngine) { f.image.Os = "windows" },
	} {
		m, f, spec := runtimeTestManager(t)
		change(f)
		if _, err := m.Create(t.Context(), spec); !errors.Is(err, ErrUnsupportedImage) || f.createdCount != 0 {
			t.Errorf("unsupported/foreign image created a container: %v", err)
		}
	}
	for _, warning := range []bool{true, false} {
		m, f, spec := runtimeTestManager(t)
		if warning {
			f.warnings = []string{"memory limit ignored"}
		} else {
			f.afterCreate = func() { f.container.HostConfig.Privileged = true }
		}
		if _, err := m.Create(t.Context(), spec); err == nil || len(f.removed) != 1 {
			t.Errorf("failed creation was accepted or leaked: %v", err)
		}
	}
	for _, version := range []client.ServerVersionResult{{Version: "27.5.1", Os: "linux"}, {Version: "29.5.2", Os: "windows"}} {
		m, f, spec := runtimeTestManager(t)
		f.version = version
		if _, err := m.Create(t.Context(), spec); !errors.Is(err, ErrUnsupportedEngine) || f.createdCount != 0 {
			t.Errorf("unsupported engine: %v", err)
		}
		m, f, spec = runtimeTestExisting(t)
		f.version = version
		if _, err := m.Start(t.Context(), spec); !errors.Is(err, ErrUnsupportedEngine) || f.started != 0 {
			t.Errorf("start on unsupported engine: %v", err)
		}
	}
}

func TestManagerPostStartFailuresCleanOnlyVerifiedStoppedContainer(t *testing.T) {
	for _, kind := range []string{"inspection", "policy", "published binding"} {
		for _, keepRunning := range []bool{false, true} {
			t.Run(kind+"/running="+strconv.FormatBool(keepRunning), func(t *testing.T) {
				m, f, spec := runtimeTestExisting(t)
				inspectionFailure := errors.New("temporary post-start inspect failure")
				f.afterStart = func() {
					f.container.State.Running = keepRunning
					if !keepRunning {
						f.container.State.Status = "exited"
					}
					if kind == "policy" {
						f.container.HostConfig.SecurityOpt = append(f.container.HostConfig.SecurityOpt, "seccomp=unconfined")
					}
					if kind == "published binding" {
						f.container.State.Running, f.container.State.Status = true, "running"
						f.container.NetworkSettings.Ports[runtimeTestPort(spec)][0].HostIP = netip.MustParseAddr("0.0.0.0")
					}
				}
				f.postStartInspectHook = func(read int) error {
					if read == 1 && kind == "inspection" {
						return inspectionFailure
					}
					if read > 1 && kind == "published binding" {
						// The process may exit between unsafe-port observation and cleanup.
						f.container.State.Running = keepRunning
						if !keepRunning {
							f.container.State.Status = "exited"
						}
					}
					return nil
				}
				got, err := m.Start(t.Context(), spec)
				want := error(ErrUnsafeContainer)
				if kind == "inspection" {
					want = inspectionFailure
				}
				if !errors.Is(err, want) || got.ID != "" {
					t.Fatalf("post-start failure = %+v, %v", got, err)
				}
				removals := 1
				if keepRunning {
					removals = 0
				}
				if len(f.removed) != removals || f.postStartReads < 2 || f.inspected[len(f.inspected)-1] != strings.Repeat("a", 64) {
					t.Errorf("post-start cleanup: removals=%d, reads=%d, ids=%v", len(f.removed), f.postStartReads, f.inspected)
				}
			})
		}
	}
}

func TestManagerInputConfigurationCancellationAndPrivateErrors(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://unexpected.invalid:2375")
	for _, config := range []Config{{}, {DockerHost: "tcp://localhost:2375"}, {DockerHost: "unix:///tmp/docker.sock?x=1"}, {DockerHost: "unix:///tmp/docker.sock", Timeout: -time.Second}, {DockerHost: "unix:///tmp/docker.sock", Timeout: 2*time.Minute + time.Nanosecond}} {
		if m, err := NewManager(config); err == nil {
			_ = m.Close()
			t.Errorf("invalid config accepted: %+v", config)
		}
	}
	if m, err := NewManager(Config{DockerHost: "unix:///tmp/docker.sock"}); err != nil {
		t.Fatal(err)
	} else {
		_ = m.Close()
	}
	for _, change := range []func(*Spec){func(s *Spec) { s.InstanceID = "bad" }, func(s *Spec) { s.ImageID = "image:latest" }, func(s *Spec) { s.AppPort = 0 }} {
		m, f, spec := runtimeTestManager(t)
		change(&spec)
		if _, err := m.Create(t.Context(), spec); !errors.Is(err, ErrInvalidInput) || f.calls != 0 {
			t.Errorf("invalid input reached Docker: %v", err)
		}
	}
	m, f, spec := runtimeTestManager(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.Create(ctx, spec); !errors.Is(err, context.Canceled) || f.createdCount != 0 {
		t.Errorf("canceled create: %v", err)
	}
	m, f, spec = runtimeTestExisting(t)
	f.startHook = func() error { return errors.New(strings.Repeat("private-detail", 500)) }
	_, err := m.Start(t.Context(), spec)
	var private *RuntimeError
	if !errors.As(err, &private) || private.Error() != "container operation failed" || strings.Contains(err.Error(), "private-detail") {
		t.Fatalf("unsafe/missing runtime error: %v", err)
	}
	detail := ""
	for _, attr := range private.LogValue().Group() {
		if attr.Key == "detail" {
			detail = attr.Value.String()
		}
	}
	if !strings.Contains(detail, "private-detail") || len(detail) > 4096 {
		t.Error("private log detail was lost or unbounded")
	}
}

func runtimeTestManager(t *testing.T) (*Manager, *runtimeTestEngine, Spec) {
	t.Helper()
	spec := Spec{ProjectID: "11111111-1111-4111-8111-111111111111", DeploymentID: "22222222-2222-4222-8222-222222222222", InstanceID: "abcdefab-abcd-4abc-8abc-abcdefabcdef", ImageID: "sha256:" + strings.Repeat("d", 64), CommitSHA: strings.Repeat("e", 40), AppPort: 8080}
	f := &runtimeTestEngine{t: t, spec: spec, version: client.ServerVersionResult{Version: "29.5.2", Os: "linux", Arch: "arm64"}}
	f.image = client.ImageInspectResult{InspectResponse: dockerimage.InspectResponse{ID: spec.ImageID, Os: "linux", Config: &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{User: "10001", Labels: map[string]string{"io.shipyard.managed": "true", "io.shipyard.deployment_id": spec.DeploymentID, "io.shipyard.commit_sha": spec.CommitSHA, "io.shipyard.build_id": "shipyard-test-build"}}}}}
	return &Manager{docker: f, timeout: time.Second}, f, spec
}

func runtimeTestExisting(t *testing.T) (*Manager, *runtimeTestEngine, Spec) {
	t.Helper()
	m, f, spec := runtimeTestManager(t)
	if _, err := m.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	return m, f, spec
}

func runtimeTestLabels(s Spec) map[string]string {
	return map[string]string{"io.shipyard.managed": "true", "io.shipyard.project_id": strings.ToLower(s.ProjectID), "io.shipyard.deployment_id": strings.ToLower(s.DeploymentID), "io.shipyard.instance_id": strings.ToLower(s.InstanceID), "io.shipyard.commit_sha": s.CommitSHA, "io.shipyard.app_port": strconv.Itoa(s.AppPort)}
}
func runtimeTestPort(s Spec) network.Port {
	return network.MustParsePort(strconv.Itoa(s.AppPort) + "/tcp")
}

type runtimeTestEngine struct {
	t                                     *testing.T
	spec                                  Spec
	image                                 client.ImageInspectResult
	version                               client.ServerVersionResult
	container                             *container.InspectResponse
	created                               client.ContainerCreateOptions
	stop                                  client.ContainerStopOptions
	imageInspected                        string
	inspected                             []string
	calls, createdCount, started, stopped int
	removed                               []string
	warnings                              []string
	exitImmediately                       bool
	startHook                             func() error
	afterCreate                           func()
	afterStart                            func()
	postStartInspectHook                  func(int) error
	postStartReads                        int
}

func (f *runtimeTestEngine) check(ctx context.Context) error {
	f.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		f.t.Error("Docker operation has no deadline")
	}
	return nil
}
func (f *runtimeTestEngine) fullID(id string) {
	if id != strings.Repeat("a", 64) {
		f.t.Errorf("mutation used a name/short ID: %q", id)
	}
}
func (f *runtimeTestEngine) ServerVersion(ctx context.Context, _ client.ServerVersionOptions) (client.ServerVersionResult, error) {
	if err := f.check(ctx); err != nil {
		return client.ServerVersionResult{}, err
	}
	return f.version, nil
}
func (f *runtimeTestEngine) ImageInspect(ctx context.Context, id string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	if err := f.check(ctx); err != nil {
		return client.ImageInspectResult{}, err
	}
	f.imageInspected = id
	return f.image, nil
}
func (f *runtimeTestEngine) ContainerCreate(ctx context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if err := f.check(ctx); err != nil {
		return client.ContainerCreateResult{}, err
	}
	if f.container != nil {
		return client.ContainerCreateResult{}, errdefs.ErrConflict
	}
	f.created = o
	f.createdCount++
	config := *o.Config
	if config.User == "" {
		config.User = f.image.Config.User
	}
	f.container = &container.InspectResponse{ID: strings.Repeat("a", 64), Name: "/" + o.Name, Image: f.spec.ImageID, Config: &config, HostConfig: o.HostConfig, State: &container.State{Status: "created"}, NetworkSettings: &container.NetworkSettings{}}
	if f.afterCreate != nil {
		f.afterCreate()
	}
	return client.ContainerCreateResult{ID: f.container.ID, Warnings: f.warnings}, nil
}
func (f *runtimeTestEngine) ContainerInspect(ctx context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if err := f.check(ctx); err != nil {
		return client.ContainerInspectResult{}, err
	}
	f.inspected = append(f.inspected, id)
	if id != "shipyard-"+f.spec.InstanceID && id != strings.Repeat("a", 64) {
		f.t.Errorf("inspection used unexpected identity: %q", id)
		return client.ContainerInspectResult{}, errdefs.ErrNotFound
	}
	if f.container == nil {
		return client.ContainerInspectResult{}, errdefs.ErrNotFound
	}
	if f.started > 0 {
		f.postStartReads++
		if f.postStartInspectHook != nil {
			if err := f.postStartInspectHook(f.postStartReads); err != nil {
				return client.ContainerInspectResult{}, err
			}
		}
	}
	return client.ContainerInspectResult{Container: *f.container}, nil
}
func (f *runtimeTestEngine) ContainerStart(ctx context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	if err := f.check(ctx); err != nil {
		return client.ContainerStartResult{}, err
	}
	f.fullID(id)
	f.started++
	if f.startHook != nil {
		return client.ContainerStartResult{}, f.startHook()
	}
	f.container.State = &container.State{Status: "running", Running: true}
	f.container.NetworkSettings.Ports = network.PortMap{runtimeTestPort(f.spec): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "32123"}}}
	if f.exitImmediately {
		f.container.State = &container.State{Status: "exited", ExitCode: 7, OOMKilled: true}
		f.container.NetworkSettings.Ports = nil
	}
	if f.afterStart != nil {
		f.afterStart()
	}
	return client.ContainerStartResult{}, nil
}
func (f *runtimeTestEngine) ContainerStop(ctx context.Context, id string, o client.ContainerStopOptions) (client.ContainerStopResult, error) {
	if err := f.check(ctx); err != nil {
		return client.ContainerStopResult{}, err
	}
	f.fullID(id)
	f.stopped++
	f.stop = o
	f.container.State.Running = false
	f.container.State.Status = "exited"
	f.container.NetworkSettings.Ports = nil
	return client.ContainerStopResult{}, nil
}
func (f *runtimeTestEngine) ContainerRemove(ctx context.Context, id string, o client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if err := f.check(ctx); err != nil {
		return client.ContainerRemoveResult{}, err
	}
	f.fullID(id)
	if o.Force || o.RemoveVolumes || o.RemoveLinks {
		f.t.Errorf("unsafe removal options: %+v", o)
	}
	if f.container.State.Running {
		return client.ContainerRemoveResult{}, errdefs.ErrConflict
	}
	f.removed = append(f.removed, id)
	f.container = nil
	return client.ContainerRemoveResult{}, nil
}
