//go:build integration

package builds

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
)

func TestBuilderCleanupRemovesOnlyItsStableBuild(t *testing.T) {
	builder := integrationBuilder(t)
	root := t.TempDir()
	writeBuildFixture(t, root, "Dockerfile", "FROM scratch\nCOPY payload /payload\n")
	writeBuildFixture(t, root, "payload", "shipyard-cleanup-fixture")
	inputs := make([]Input, 2)
	images := make([]Image, 2)
	for index := range inputs {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			t.Fatal(err)
		}
		value := hex.EncodeToString(nonce[:])
		inputs[index] = Input{CheckoutPath: root, DeploymentID: "00000000-0000-4000-8000-000000000051", CommitSHA: strings.Repeat("d", 40),
			BuildID: value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:]}
		input := inputs[index]
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := builder.Cleanup(cleanup, input, ""); err != nil {
				t.Errorf("remove owned cleanup fixture: %v", err)
			}
		})
		var err error
		images[index], err = builder.Build(t.Context(), input)
		if err != nil {
			t.Fatalf("build cleanup fixture: %v", err)
		}
	}
	input := inputs[0]
	input.CheckoutPath = ""
	if err := builder.Cleanup(t.Context(), input, images[0].ID); err != nil {
		t.Fatalf("cleanup without checkout path: %v", err)
	}
	if _, err := builder.docker.ImageInspect(t.Context(), images[0].Reference); !errdefs.IsNotFound(err) {
		t.Fatalf("cleanup retained its tagged image: %v", err)
	}
	if inspected, err := builder.docker.ImageInspect(t.Context(), images[1].Reference); err != nil || inspected.ID != images[1].ID {
		t.Fatalf("cleanup affected another build: image ID=%s, error=%v", inspected.ID, err)
	}
	if err := builder.Cleanup(t.Context(), input, images[0].ID); err != nil {
		t.Fatalf("repeated cleanup failed: %v", err)
	}
}
