//go:build linux || darwin

package routing

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

const legacyRouteFixture = `# shipyard-route-v1 {"project_id":"3b8e17ad-4eb9-4c27-86a6-8cc5f932ad41","deployment_id":"695f0db2-74a2-4d19-8d3f-362a5397018c","instance_id":"c5d34a49-8586-419e-8ea1-fc0c8a346d72","container_id":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","host_port":32123}
[http.routers.shipyard-3b8e17ad-4eb9-4c27-86a6-8cc5f932ad41]
  entryPoints = ["web"]
  rule = "Host(` + "`3b8e17ad-4eb9-4c27-86a6-8cc5f932ad41.shipyard.localhost`" + `)"
  service = "shipyard-3b8e17ad-4eb9-4c27-86a6-8cc5f932ad41"

[http.services.shipyard-3b8e17ad-4eb9-4c27-86a6-8cc5f932ad41.loadBalancer]
  passHostHeader = true
  [[http.services.shipyard-3b8e17ad-4eb9-4c27-86a6-8cc5f932ad41.loadBalancer.servers]]
    url = "http://127.0.0.1:32123"
`

func TestManagerReadsUpgradesAndRemovesExactLegacyRoutes(t *testing.T) {
	for _, operation := range []string{"upgrade", "remove", "tampered"} {
		t.Run(operation, func(t *testing.T) {
			manager, directory := routingTestManager(t)
			binding := routingTestBinding()
			data := []byte(legacyRouteFixture)
			if operation == "tampered" {
				data = append(data, []byte("# edited\n")...)
			}
			path := routingTestPath(directory, binding.ProjectID)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if operation == "tampered" {
				if _, err := manager.Get(t.Context(), binding.ProjectID); !errors.Is(err, ErrNotOwned) {
					t.Fatalf("accepted edited legacy route: %v", err)
				}
				if err := manager.Put(t.Context(), binding, nil); !errors.Is(err, ErrNotOwned) {
					t.Fatalf("rewrote edited legacy route: %v", err)
				}
				if err := manager.Remove(t.Context(), binding); !errors.Is(err, ErrNotOwned) {
					t.Fatalf("removed edited legacy route: %v", err)
				}
				return
			}
			if got, err := manager.Get(t.Context(), binding.ProjectID); err != nil || got != binding {
				t.Fatalf("legacy binding: %+v, %v", got, err)
			}
			if operation == "remove" {
				if err := manager.Remove(t.Context(), binding); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("legacy file remains: %v", err)
				}
				return
			}
			if err := manager.Put(t.Context(), binding, nil); err != nil {
				t.Fatal(err)
			}
			upgraded, err := os.ReadFile(path)
			marker, _ := Marker(binding)
			if err != nil || bytes.Equal(upgraded, data) || !bytes.HasPrefix(upgraded, []byte(headerPrefix)) || !bytes.Contains(upgraded, []byte(marker)) {
				t.Fatalf("legacy route was not upgraded: %s, %v", upgraded, err)
			}
		})
	}
}
