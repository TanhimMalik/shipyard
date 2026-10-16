package deployments

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestExecutionRejectsInvalidMetadataBeforeStorage(t *testing.T) {
	store := NewStore(nil)
	for _, commit := range []string{"", "main", strings.Repeat("A", 40), strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("a", 39) + "g"} {
		if _, err := store.RecordSourceCommit(context.Background(), Claim{}, commit); !errors.Is(err, ErrInvalidExecution) {
			t.Errorf("commit %q: error = %v, want ErrInvalidExecution", commit, err)
		}
	}
	for _, reason := range []string{"", " ", " reason", "reason ", "reason\nmore", "reason\x00", string([]byte{0xff}), strings.Repeat("界", 1025)} {
		if _, err := store.FailSource(context.Background(), Claim{}, reason); !errors.Is(err, ErrInvalidExecution) {
			t.Errorf("reason length %d: error = %v, want ErrInvalidExecution", len(reason), err)
		}
	}
}

func TestClaimTokenIsNotJSONMetadata(t *testing.T) {
	data, err := json.Marshal(Claim{Deployment: Deployment{ID: "deployment"}, Token: "private-claim-token"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "token") || strings.Contains(string(data), "private-claim-token") {
		t.Fatalf("claim token leaked in JSON: %s", data)
	}
}

func TestBuildRuntimeMetadataIsValidatedBeforeStorage(t *testing.T) {
	store := NewStore(nil)
	claim := Claim{Deployment: Deployment{ID: "11111111-1111-4111-8111-111111111111"}}
	reference, imageID := "shipyard/deployments:"+claim.ID+"-"+strings.Repeat("a", 32), "sha256:"+strings.Repeat("b", 64)
	for _, invalid := range [][2]string{
		{"image:latest", imageID}, {reference, "image:latest"},
		{strings.Replace(reference, claim.ID, "22222222-2222-4222-8222-222222222222", 1), imageID},
		{reference + "a", imageID}, {reference, strings.ToUpper(imageID)},
	} {
		if _, err := store.BeginStart(t.Context(), claim, invalid[0], invalid[1]); !errors.Is(err, ErrInvalidExecution) {
			t.Errorf("invalid build metadata reached storage: %v", err)
		}
	}
	valid := Observation{ContainerID: strings.Repeat("c", 64), Status: "running", Running: true, HostPort: 32123}
	for _, change := range []func(*Observation){
		func(o *Observation) { o.ContainerID = "short-id" },
		func(o *Observation) { o.Status = "healthy" },
		func(o *Observation) { o.HostPort = 0 },
		func(o *Observation) { o.HostPort = 65536 },
		func(o *Observation) { o.Running = false },
	} {
		observed := valid
		change(&observed)
		if _, err := store.RecordInstance(t.Context(), claim, "instance", observed); !errors.Is(err, ErrInvalidExecution) {
			t.Errorf("invalid observation reached storage: %v", err)
		}
	}
	for _, stage := range []string{StatusQueued, StatusFailed, "RUNNING", "unknown"} {
		if _, err := store.FailExecution(t.Context(), claim, stage, "Public failure."); !errors.Is(err, ErrInvalidExecution) {
			t.Errorf("invalid failure stage reached storage: %v", err)
		}
	}
}
