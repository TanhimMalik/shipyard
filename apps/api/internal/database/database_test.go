package database

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOpenRejectsMalformedURLWithoutExposingCredentials(t *testing.T) {
	pool, err := Open(t.Context(), "postgres://user:private-sentinel@127.0.0.1/%zz")
	if pool != nil || err == nil || !strings.Contains(err.Error(), "SHIPYARD_DATABASE_URL") {
		t.Fatalf("expected configuration error and no pool, got %v, %v", pool, err)
	}
	if strings.Contains(err.Error(), "private-sentinel") {
		t.Fatal("configuration error exposed the password")
	}
}

func TestOpenHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	pool, err := Open(ctx, "postgres://shipyard@127.0.0.1:5432/shipyard?sslmode=disable")
	if pool != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation and no pool, got %v, %v", pool, err)
	}
}
