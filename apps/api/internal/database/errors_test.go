package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestErrorLogValueKeepsSafeFailureMetadata(t *testing.T) {
	for _, test := range []struct {
		name        string
		err         error
		kind, state string
	}{
		{"deadline", fmt.Errorf("private: %w", context.DeadlineExceeded), "timeout", ""},
		{"canceled", context.Canceled, "canceled", ""},
		{"schema mismatch", fmt.Errorf("private: %w", &pgconn.PgError{Code: "42P01", Message: "private SQL", Detail: "private connection string"}), "postgresql", "42P01"},
		{"untrusted code", &pgconn.PgError{Code: "private", Message: "private SQL"}, "postgresql", ""},
		{"connection", &pgconn.ConnectError{Config: &pgconn.Config{Password: "private"}}, "connection", ""},
		{"network", &net.DNSError{Err: "private", Name: "private.example"}, "network", ""},
		{"network timeout", &net.DNSError{Err: "private", IsTimeout: true}, "timeout", ""},
		{"unknown", errors.New("postgres://user:private@private-host/database"), "unknown", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := []slog.Attr{slog.String("kind", test.kind)}
			if test.state != "" {
				want = append(want, slog.String("sql_state", test.state))
			}
			got := ErrorLogValue(test.err).Group()
			if len(got) != len(want) {
				t.Fatalf("attributes = %v, want %v", got, want)
			}
			for i := range want {
				if !got[i].Equal(want[i]) {
					t.Fatalf("attributes = %v, want %v", got, want)
				}
			}
		})
	}
}
