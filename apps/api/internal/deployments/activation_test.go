package deployments

import (
	"errors"
	"strings"
	"testing"
)

func TestActivationRejectsInvalidMetadataBeforeStorage(t *testing.T) {
	store := NewStore(nil)
	for _, input := range []struct {
		id   string
		port int
	}{
		{"short", 32101}, {strings.Repeat("A", 64), 32101},
		{strings.Repeat("a", 64), 0}, {strings.Repeat("a", 64), 65536},
	} {
		if _, err := store.BeginActivation(t.Context(), Claim{}, input.id, input.port); !errors.Is(err, ErrInvalidExecution) {
			t.Errorf("invalid activation reached storage: %v", err)
		}
	}
	if _, err := store.ConfirmActivation(t.Context(), Claim{}, RouteBinding{}); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("invalid confirmation reached storage: %v", err)
	}
	for _, reason := range []string{"", " reason", "reason\nmore", strings.Repeat("a", 1025)} {
		if _, err := store.AbortActivation(t.Context(), Claim{}, nil, reason); !errors.Is(err, ErrInvalidExecution) {
			t.Errorf("invalid activation failure reached storage: %v", err)
		}
	}
}
