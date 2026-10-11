package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"down"}, {"up", "extra"}} {
		err := run(context.Background(), args, func(string) (string, bool) {
			t.Fatal("invalid arguments must not read database configuration")
			return "", false
		}, &bytes.Buffer{})
		if err == nil || err.Error() != usage {
			t.Fatalf("run(%v) = %v, want usage error", args, err)
		}
	}
}

func TestRunHelpNeedsNoDatabase(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"--help"}, func(string) (string, bool) {
		t.Fatal("help must not read database configuration")
		return "", false
	}, &out)
	if err != nil || strings.TrimSpace(out.String()) != usage {
		t.Fatalf("help = %q, %v", out.String(), err)
	}
}

func TestRunRejectsMissingOrMalformedDatabaseURL(t *testing.T) {
	for _, url := range []string{"", "  ", "postgres://shipyard:private-sentinel@%invalid-host/shipyard"} {
		t.Run(url, func(t *testing.T) {
			var out bytes.Buffer
			err := run(context.Background(), []string{"up"}, func(key string) (string, bool) {
				if key != "SHIPYARD_DATABASE_URL" {
					t.Fatalf("unexpected environment lookup: %s", key)
				}
				return url, url != ""
			}, &out)
			if err == nil || !strings.Contains(err.Error(), "SHIPYARD_DATABASE_URL") {
				t.Fatalf("expected database configuration error, got %v", err)
			}
			if strings.Contains(err.Error(), "private-sentinel") || out.Len() != 0 {
				t.Fatalf("configuration error exposed connection details")
			}
		})
	}
}
