package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

var (
	ErrInvalidInput      = errors.New("invalid container specification")
	ErrNotFound          = errors.New("container not found")
	ErrNotOwned          = errors.New("container ownership does not match")
	ErrUnsupportedImage  = errors.New("image must be a managed Linux image without declared volumes")
	ErrUnsupportedEngine = errors.New("container startup requires Linux Docker Engine 28 or newer")
	ErrUnsafeContainer   = errors.New("container configuration does not match the runtime policy")
)

type Config struct {
	DockerHost string
	Timeout    time.Duration
}

type Spec struct {
	ProjectID    string
	DeploymentID string
	InstanceID   string
	ContainerID  string
	ImageID      string
	CommitSHA    string
	AppPort      int
}

type Instance struct {
	ID        string
	Name      string
	ImageID   string
	Status    string
	Running   bool
	ExitCode  int
	OOMKilled bool
	HostPort  int
}

type engine interface {
	ServerVersion(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error)
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

type Manager struct {
	docker  engine
	timeout time.Duration
	close   func() error
}

func NewManager(config Config) (*Manager, error) {
	u, err := url.Parse(config.DockerHost)
	if err != nil || u.Scheme != "unix" || u.Host != "" || u.User != nil || !filepath.IsAbs(u.Path) ||
		u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("Docker host must be an explicit absolute unix socket URL")
	}
	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}
	if config.Timeout < 0 || config.Timeout > 2*time.Minute {
		return nil, errors.New("container operation timeout must be positive and at most two minutes")
	}
	docker, err := client.New(client.WithHost(config.DockerHost), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, errors.New("could not configure Docker client")
	}
	return &Manager{docker: docker, timeout: config.Timeout, close: docker.Close}, nil
}

func (m *Manager) Close() error {
	if m.close != nil {
		return m.close()
	}
	return nil
}

// Create uses a stable instance name so an uncertain response can be retried
// without creating a second container. It never pulls an image or starts it.
// A supplied ContainerID only permits adoption of that existing container.
func (m *Manager) Create(ctx context.Context, spec Spec) (instance Instance, resultErr error) {
	spec, err := normalizeSpec(spec)
	if err != nil {
		return Instance{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	defer finishOperation(&instance, &resultErr)
	if err := m.checkEngine(ctx); err != nil {
		return Instance{}, err
	}
	image, err := m.docker.ImageInspect(ctx, spec.ImageID)
	if err != nil {
		return Instance{}, err
	}
	if image.ID != spec.ImageID || image.Os != "linux" || image.Config == nil || len(image.Config.Volumes) != 0 ||
		image.Config.Labels["io.shipyard.managed"] != "true" || image.Config.Labels["io.shipyard.deployment_id"] != spec.DeploymentID ||
		image.Config.Labels["io.shipyard.commit_sha"] != spec.CommitSHA || image.Config.Labels["io.shipyard.build_id"] == "" {
		return Instance{}, ErrUnsupportedImage
	}
	if spec.ContainerID != "" {
		found, err := m.inspectOwned(ctx, spec, containerName(spec))
		if err != nil {
			return Instance{}, err
		}
		if err := checkPolicy(spec, found); err != nil {
			return Instance{}, err
		}
		return observedInstance(spec, found)
	}
	created, err := m.docker.ContainerCreate(ctx, createOptions(spec))
	if errdefs.IsConflict(err) {
		found, err := m.inspectOwned(ctx, spec, containerName(spec))
		if err != nil {
			return Instance{}, err
		}
		if err := checkPolicy(spec, found); err != nil {
			return Instance{}, err
		}
		return observedInstance(spec, found)
	}
	if err != nil {
		return Instance{}, err
	}
	if !containerIDPattern.MatchString(created.ID) {
		return Instance{}, errors.New("Docker returned an invalid container identity")
	}
	found, err := m.inspectOwned(ctx, spec, created.ID)
	if err == nil {
		err = checkPolicy(spec, found)
	}
	if len(created.Warnings) != 0 {
		err = errors.Join(err, fmt.Errorf("Docker create warnings: %s", strings.Join(created.Warnings, "; ")))
	}
	if err != nil {
		return Instance{}, errors.Join(err, m.removeStopped(spec, created.ID))
	}
	return observedInstance(spec, found)
}

// Start returns observed process state, not HTTP readiness or route activation.
func (m *Manager) Start(ctx context.Context, spec Spec) (instance Instance, resultErr error) {
	spec, err := normalizeSpec(spec)
	if err != nil {
		return Instance{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	defer finishOperation(&instance, &resultErr)
	if err := m.checkEngine(ctx); err != nil {
		return Instance{}, err
	}
	found, err := m.inspectOwned(ctx, spec, containerName(spec))
	if err != nil {
		return Instance{}, err
	}
	if err := checkPolicy(spec, found); err != nil {
		return Instance{}, err
	}
	if !found.State.Running {
		if _, err := m.docker.ContainerStart(ctx, found.ID, client.ContainerStartOptions{}); err != nil {
			return Instance{}, errors.Join(err, m.removeStopped(spec, found.ID))
		}
	}
	id := found.ID
	found, err = m.inspectOwned(ctx, spec, id)
	if err == nil {
		err = checkPolicy(spec, found)
	}
	if err == nil {
		instance, err = observedInstance(spec, found)
	}
	if err != nil {
		return Instance{}, errors.Join(err, m.removeStopped(spec, id))
	}
	return instance, nil
}

func (m *Manager) Inspect(ctx context.Context, spec Spec) (instance Instance, resultErr error) {
	spec, err := normalizeSpec(spec)
	if err != nil {
		return Instance{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	defer finishOperation(&instance, &resultErr)
	found, err := m.inspectOwned(ctx, spec, containerName(spec))
	if err != nil {
		return Instance{}, err
	}
	return observedInstance(spec, found)
}

func (m *Manager) Stop(ctx context.Context, spec Spec) (instance Instance, resultErr error) {
	spec, err := normalizeSpec(spec)
	if err != nil {
		return Instance{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	defer finishOperation(&instance, &resultErr)
	found, err := m.inspectOwned(ctx, spec, containerName(spec))
	if err != nil {
		return Instance{}, err
	}
	if found.State.Running {
		seconds := 5
		if _, err := m.docker.ContainerStop(ctx, found.ID, client.ContainerStopOptions{Signal: "SIGTERM", Timeout: &seconds}); err != nil {
			return Instance{}, err
		}
	}
	found, err = m.inspectOwned(ctx, spec, found.ID)
	if err != nil {
		return Instance{}, err
	}
	if found.State.Running {
		return Instance{}, errors.New("container was still running after stop")
	}
	return observedInstance(spec, found)
}

// Remove does not force a running container to stop or remove its image.
func (m *Manager) Remove(ctx context.Context, spec Spec) (resultErr error) {
	spec, err := normalizeSpec(spec)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	defer func() { resultErr = runtimeError(resultErr) }()
	found, err := m.inspectOwned(ctx, spec, containerName(spec))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = m.docker.ContainerRemove(ctx, found.ID, client.ContainerRemoveOptions{})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (m *Manager) checkEngine(ctx context.Context) error {
	version, err := m.docker.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return err
	}
	major, _, _ := strings.Cut(version.Version, ".")
	number, err := strconv.Atoi(major)
	if err != nil || number < 28 || version.Os != "linux" {
		return ErrUnsupportedEngine
	}
	return nil
}

func (m *Manager) inspectOwned(ctx context.Context, spec Spec, id string) (container.InspectResponse, error) {
	response, err := m.docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		return container.InspectResponse{}, ErrNotFound
	}
	if err != nil {
		return container.InspectResponse{}, err
	}
	found := response.Container
	if !containerIDPattern.MatchString(found.ID) || containerIDPattern.MatchString(id) && found.ID != id ||
		spec.ContainerID != "" && found.ID != spec.ContainerID ||
		found.Name != "/"+containerName(spec) || found.Image != spec.ImageID || found.Config == nil || found.State == nil {
		return container.InspectResponse{}, ErrNotOwned
	}
	for key, value := range ownershipLabels(spec) {
		if found.Config.Labels[key] != value {
			return container.InspectResponse{}, ErrNotOwned
		}
	}
	return found, nil
}

// A lost start response may conceal a running container. Recheck the full ID
// and let Docker atomically refuse removal if another caller has started it.
func (m *Manager) removeStopped(spec Spec, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	found, err := m.inspectOwned(ctx, spec, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil || found.State.Running {
		return err
	}
	_, err = m.docker.ContainerRemove(ctx, found.ID, client.ContainerRemoveOptions{})
	if errdefs.IsNotFound(err) || errdefs.IsConflict(err) {
		return nil
	}
	return err
}

func finishOperation(instance *Instance, err *error) {
	if *err != nil {
		*instance = Instance{}
		*err = runtimeError(*err)
	}
}

type RuntimeError struct{ cause error }

func runtimeError(err error) error {
	if err == nil {
		return nil
	}
	return &RuntimeError{cause: err}
}

func (e *RuntimeError) Error() string { return "container operation failed" }
func (e *RuntimeError) Unwrap() error { return e.cause }
func (e *RuntimeError) LogValue() slog.Value {
	detail := e.cause.Error()
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	return slog.GroupValue(slog.String("message", e.Error()), slog.String("detail", detail))
}
