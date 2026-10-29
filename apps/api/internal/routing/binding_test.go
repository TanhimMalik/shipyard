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

func TestMarkerIdentifiesEveryPartOfTheBinding(t *testing.T) {
	binding := routingTestBinding()
	marker, err := Marker(binding)
	if err != nil || marker != "1256d61c9699c82b3e0900e854a0e9c574b79cc9fa4f8514f13c8ace1228f678" {
		t.Fatalf("stable binding marker: %q, %v", marker, err)
	}
	for _, change := range []func(*Binding){
		func(b *Binding) { b.ProjectID = b.DeploymentID },
		func(b *Binding) { b.DeploymentID = b.InstanceID },
		func(b *Binding) { b.InstanceID = b.ProjectID },
		func(b *Binding) { b.ContainerID = strings.Repeat("d", 64) },
		func(b *Binding) { b.HostPort++ },
	} {
		next := binding
		change(&next)
		if got, err := Marker(next); err != nil || got == marker {
			t.Fatalf("marker did not identify changed binding: %+v, %q, %v", next, got, err)
		}
	}
	if _, err := Marker(Binding{}); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("invalid binding has a marker: %v", err)
	}
}
