// Package errkind names the kind of a failure, for a log line that prints it as `error_kind` where
// the failure's own text may not print: the people store's errors quote the address they were asked
// about ("read membership of %q"), and the broker client's the person it asked after
// ("agent-secrets: ask what <login> has that outlives the browser"), so a line that logs one of
// them logs a person's address.
package errkind

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/cipher"
)

// Of is err's kind, the first of these that holds, and "" for no error:
//
//   - "canceled" or "deadline exceeded", for a context that ended the call;
//   - "database connection", for a connection to the database that could not be made;
//   - "database SQLSTATE <code>", for an error the database answered, with its five-character code;
//   - "broker HTTP <status>", for an answer of the secrets broker that was no success;
//   - "broker account token", for Dispatch's own broker token that could not be read;
//   - "network timeout" or "network", for any other network failure;
//   - otherwise the Go type of the innermost error err wraps, such as "*errors.errorString",
//     following the last of the errors an error wraps where it wraps several (`"%w: %w"`,
//     errors.Join), which is the cause where the first names the failure.
//
// Each is fixed words, a code the database or the broker chose, or a type the code names, and never
// text a caller formatted into err.
func Of(err error) string {
	if err == nil {
		return ""
	}
	var (
		connect *pgconn.ConnectError
		server  *pgconn.PgError
		broker  *cipher.Error
		token   *cipher.AccountTokenError
		network net.Error
	)
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.As(err, &connect):
		return "database connection"
	case errors.As(err, &server):
		return "database SQLSTATE " + server.Code
	case errors.As(err, &broker):
		return "broker HTTP " + strconv.Itoa(broker.Status)
	case errors.As(err, &token):
		return "broker account token"
	case errors.As(err, &network):
		if network.Timeout() {
			return "network timeout"
		}
		return "network"
	}
	return reflect.TypeOf(innermost(err)).String()
}

// innermost is the error err wraps that wraps nothing, taking the last of several.
func innermost(err error) error {
	for {
		switch wrapper := err.(type) {
		case interface{ Unwrap() []error }:
			wrapped := wrapper.Unwrap()
			if len(wrapped) == 0 || wrapped[len(wrapped)-1] == nil {
				return err
			}
			err = wrapped[len(wrapped)-1]
		case interface{ Unwrap() error }:
			next := wrapper.Unwrap()
			if next == nil {
				return err
			}
			err = next
		default:
			return err
		}
	}
}
