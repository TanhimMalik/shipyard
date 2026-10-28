//go:build linux || darwin

package routing

import (
	"errors"
	"strings"
	"testing"
)

func TestHostnameIsStableAndRejectsNonCanonicalProjectIDs(t *testing.T) {
	binding := routingTestBinding()
	got, err := Hostname(binding.ProjectID)
	if err != nil || got != binding.ProjectID+".shipyard.localhost" {
		t.Fatalf("hostname: %q, %v", got, err)
	}
	for _, projectID := range []string{"", "demo", "../outside", strings.ToUpper(binding.ProjectID), binding.ProjectID + ".example.com"} {
		if _, err := Hostname(projectID); !errors.Is(err, ErrInvalidBinding) {
			t.Errorf("accepted project ID %q: %v", projectID, err)
		}
	}
}

func TestManagerRejectsInvalidBindingsBeforePublication(t *testing.T) {
	for _, change := range []func(*Binding){
		func(b *Binding) { b.ProjectID = "../../outside" },
		func(b *Binding) { b.ProjectID = strings.ToUpper(b.ProjectID) },
		func(b *Binding) { b.DeploymentID = "deployment" },
		func(b *Binding) { b.InstanceID = "instance" },
		func(b *Binding) { b.ContainerID = "short-id" },
		func(b *Binding) { b.ContainerID = strings.Repeat("A", 64) },
		func(b *Binding) { b.HostPort = 0 },
		func(b *Binding) { b.HostPort = -1 },
		func(b *Binding) { b.HostPort = 65536 },
	} {
		m, _ := routingTestManager(t)
		binding := routingTestBinding()
		change(&binding)
		if err := m.Put(t.Context(), binding, nil); !errors.Is(err, ErrInvalidBinding) {
			t.Errorf("accepted invalid binding %+v: %v", binding, err)
		}
		if err := m.Remove(t.Context(), binding); !errors.Is(err, ErrInvalidBinding) {
			t.Errorf("accepted invalid removal %+v: %v", binding, err)
		}
	}
	m, _ := routingTestManager(t)
	for _, port := range []int{1, 65535} {
		binding := routingTestBinding()
		binding.HostPort = port
		if err := m.Put(t.Context(), binding, nil); err != nil {
			t.Fatalf("rejected valid boundary port %d: %v", port, err)
		}
		if err := m.Remove(t.Context(), binding); err != nil {
			t.Fatal(err)
		}
	}
}
