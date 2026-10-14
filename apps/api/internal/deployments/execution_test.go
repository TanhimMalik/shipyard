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
