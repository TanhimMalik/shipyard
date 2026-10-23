package runtime

import (
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

var (
	uuidPattern        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	imageIDPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitPattern      = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const memoryLimit = 256 << 20

func normalizeSpec(spec Spec) (Spec, error) {
	if !uuidPattern.MatchString(spec.ProjectID) || !uuidPattern.MatchString(spec.DeploymentID) || !uuidPattern.MatchString(spec.InstanceID) ||
		spec.ContainerID != "" && !containerIDPattern.MatchString(spec.ContainerID) ||
		!imageIDPattern.MatchString(spec.ImageID) || !commitPattern.MatchString(spec.CommitSHA) || spec.AppPort < 1 || spec.AppPort > 65535 {
		return Spec{}, ErrInvalidInput
	}
	spec.ProjectID, spec.DeploymentID, spec.InstanceID = strings.ToLower(spec.ProjectID), strings.ToLower(spec.DeploymentID), strings.ToLower(spec.InstanceID)
	return spec, nil
}

func containerName(spec Spec) string { return "shipyard-" + spec.InstanceID }

func ownershipLabels(spec Spec) map[string]string {
	return map[string]string{
		"io.shipyard.managed": "true", "io.shipyard.project_id": spec.ProjectID,
		"io.shipyard.deployment_id": spec.DeploymentID, "io.shipyard.instance_id": spec.InstanceID,
		"io.shipyard.commit_sha": spec.CommitSHA, "io.shipyard.app_port": strconv.Itoa(spec.AppPort),
	}
}

func appPort(spec Spec) network.Port {
	port, _ := network.ParsePort(strconv.Itoa(spec.AppPort) + "/tcp")
	return port
}

func createOptions(spec Spec) client.ContainerCreateOptions {
	port := appPort(spec)
	pids, seconds := int64(64), 5
	return client.ContainerCreateOptions{
		Name: containerName(spec),
		Config: &container.Config{
			Image: spec.ImageID, Labels: ownershipLabels(spec), ExposedPorts: network.PortSet{port: {}},
			Healthcheck: &container.HealthConfig{Test: []string{"NONE"}}, StopSignal: "SIGTERM", StopTimeout: &seconds,
		},
		HostConfig: &container.HostConfig{
			NetworkMode: "bridge", IpcMode: "private", CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges=true"},
			PortBindings:  network.PortMap{port: {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: ""}}},
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
			LogConfig:     container.LogConfig{Type: "local", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
			Resources:     container.Resources{NanoCPUs: 1_000_000_000, Memory: memoryLimit, MemorySwap: memoryLimit, PidsLimit: &pids},
		},
	}
}

func checkPolicy(spec Spec, found container.InspectResponse) error {
	host := found.HostConfig
	if host == nil || host.Privileged || host.AutoRemove || !host.RestartPolicy.IsNone() || host.PublishAllPorts ||
		host.NetworkMode != "bridge" || host.IpcMode != "private" || host.PidMode != "" || host.UTSMode != "" || host.UsernsMode == "host" ||
		len(host.Binds)+len(host.Mounts)+len(host.VolumesFrom)+len(host.Tmpfs)+len(host.Devices)+len(host.DeviceRequests)+len(found.Mounts) != 0 ||
		host.NanoCPUs != 1_000_000_000 || host.Memory != memoryLimit || host.MemorySwap != memoryLimit || host.PidsLimit == nil || *host.PidsLimit != 64 ||
		len(host.CapAdd) != 0 || len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" ||
		!slices.Equal(host.SecurityOpt, []string{"no-new-privileges=true"}) ||
		host.LogConfig.Type != "local" || host.LogConfig.Config["max-size"] != "10m" || host.LogConfig.Config["max-file"] != "3" ||
		found.Config.Healthcheck == nil || !slices.Equal(found.Config.Healthcheck.Test, []string{"NONE"}) ||
		found.Config.StopSignal != "SIGTERM" || found.Config.StopTimeout == nil || *found.Config.StopTimeout != 5 || len(found.Config.Volumes) != 0 {
		return ErrUnsafeContainer
	}
	bindings := host.PortBindings[appPort(spec)]
	if len(host.PortBindings) != 1 || len(bindings) != 1 || bindings[0].HostIP != netip.MustParseAddr("127.0.0.1") || bindings[0].HostPort != "" {
		return ErrUnsafeContainer
	}
	return nil
}

func observedInstance(spec Spec, found container.InspectResponse) (Instance, error) {
	instance := Instance{ID: found.ID, Name: strings.TrimPrefix(found.Name, "/"), ImageID: found.Image,
		Status: string(found.State.Status), Running: found.State.Running, ExitCode: found.State.ExitCode, OOMKilled: found.State.OOMKilled}
	if !instance.Running {
		return instance, nil
	}
	if found.NetworkSettings == nil {
		return Instance{}, ErrUnsafeContainer
	}
	for port, bindings := range found.NetworkSettings.Ports {
		if len(bindings) == 0 {
			continue
		}
		if port != appPort(spec) || len(bindings) != 1 || bindings[0].HostIP != netip.MustParseAddr("127.0.0.1") {
			return Instance{}, ErrUnsafeContainer
		}
		number, err := strconv.Atoi(bindings[0].HostPort)
		if err != nil || number < 1 || number > 65535 {
			return Instance{}, ErrUnsafeContainer
		}
		instance.HostPort = number
	}
	if instance.HostPort == 0 {
		return Instance{}, ErrUnsafeContainer
	}
	return instance, nil
}
