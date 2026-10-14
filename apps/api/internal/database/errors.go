package database

import (
	"context"
	"errors"
	"log/slog"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrorLogValue preserves useful failure metadata without logging connection
// strings, query text, or PostgreSQL messages that may contain stored values.
func ErrorLogValue(err error) slog.Value {
	kind := "unknown"
	var pgError *pgconn.PgError
	var connectError *pgconn.ConnectError
	var networkError net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		kind = "timeout"
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	case errors.As(err, &pgError):
		kind = "postgresql"
	case errors.As(err, &connectError):
		kind = "connection"
	case errors.As(err, &networkError):
		kind = "network"
		if networkError.Timeout() {
			kind = "timeout"
		}
	}
	attributes := []slog.Attr{slog.String("kind", kind)}
	if pgError != nil && validSQLState(pgError.Code) {
		attributes = append(attributes, slog.String("sql_state", pgError.Code))
	}
	return slog.GroupValue(attributes...)
}

func validSQLState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for _, char := range code {
		if !(char >= 'A' && char <= 'Z' || char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}
