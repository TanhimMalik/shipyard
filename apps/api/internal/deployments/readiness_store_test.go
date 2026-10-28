package deployments

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReadinessRejectsInvalidMetadataBeforeStorage(t *testing.T) {
	store := NewStore(nil)
	valid := ReadinessObservation{ContainerID: strings.Repeat("a", 64), HostPort: 32123,
		Passed: true, Attempts: 1, StatusCode: 200, CheckedAt: time.Now()}
	for _, change := range []func(*ReadinessObservation){
		func(o *ReadinessObservation) { o.ContainerID = "short-id" },
		func(o *ReadinessObservation) { o.HostPort = 0 },
		func(o *ReadinessObservation) { o.HostPort = 65536 },
		func(o *ReadinessObservation) { o.Attempts = 0 },
		func(o *ReadinessObservation) { o.Attempts = -1 },
		func(o *ReadinessObservation) { o.StatusCode = 0 },
		func(o *ReadinessObservation) { o.StatusCode = 503 },
		func(o *ReadinessObservation) { o.Passed, o.StatusCode = false, 99 },
		func(o *ReadinessObservation) { o.Passed, o.StatusCode = false, 1000 },
		func(o *ReadinessObservation) { o.CheckedAt = time.Time{} },
		func(o *ReadinessObservation) { o.CheckedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
	} {
		observed := valid
		change(&observed)
		if _, err := store.RecordReadiness(t.Context(), Claim{}, "instance", observed); !errors.Is(err, ErrInvalidExecution) {
			t.Errorf("invalid readiness reached storage: %+v, %v", observed, err)
		}
	}
}
