package docs

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// faultSentinels are the errors LoggedFault names a fault by when err is one of them, each by its
// own message, which this module or the package that declares the error wrote and no caller or
// document did. They are the ones a server fault commonly wraps, the more particular of two that
// one error can be both of first: a room that failed or reloads before the service it belongs to.
var faultSentinels = [...]error{
	ErrRoomBehindHead,
	errRoomReloading,
	ErrServiceUnavailable,
	ErrDocumentUnloadable,
	ErrDocumentTooLarge,
	pmdoc.ErrTooManyElements,
	ErrIssueClosed,
	ErrQueueSealed,
	ErrPreconditionBusy,
	pmdoc.ErrPanic,
	context.Canceled,
	context.DeadlineExceeded,
	pgx.ErrNoRows,
	pgx.ErrTooManyRows,
	pgx.ErrTxClosed,
	pgx.ErrTxCommitRollback,
}

// LoggedFault is err as the log line of a server fault carries it: a description built only of
// words this program wrote, never of err's message nor of a field that holds message text.
//
// LoggedError logs a refusal of a document by its class and any other error whole, which keeps the
// document's text out of the line only while every wrapper between the refusal and the line keeps
// the chain (%w). A refusal wrapped with %v, or quoted in the message of an error a handler formatted
// from it, is no refusal errors.Is can see, so LoggedError logs it whole, with the text it quotes,
// and a document's text can hold a person's address. LoggedFault reads no message at all. The
// group it answers holds, each only where err has it:
//
//   - refusal: the class of the document refusal err is (RefusalClass);
//   - sentinel: the messages of the faultSentinels err is, joined by "; ", leaving out one whose
//     words an earlier one already holds (a room's failure holds its service's);
//   - sqlstate and constraint: a Postgres error's SQLSTATE and the name of the constraint it
//     names, which are the schema's words and never a row's values;
//   - types: the Go type of each error in err's tree (errorTypes).
func LoggedFault(err error) slog.Value {
	attrs := make([]slog.Attr, 0, 5)
	if class := RefusalClass(err); class != "" {
		attrs = append(attrs, slog.String("refusal", class))
	}
	var sentinels strings.Builder
	for _, sentinel := range faultSentinels {
		words := sentinel.Error()
		if !errors.Is(err, sentinel) || strings.Contains(sentinels.String(), words) {
			continue
		}
		if sentinels.Len() > 0 {
			sentinels.WriteString("; ")
		}
		sentinels.WriteString(words)
	}
	if sentinels.Len() > 0 {
		attrs = append(attrs, slog.String("sentinel", sentinels.String()))
	}
	var database *pgconn.PgError
	if errors.As(err, &database) {
		attrs = append(attrs, slog.String("sqlstate", database.Code))
		if database.ConstraintName != "" {
			attrs = append(attrs, slog.String("constraint", database.ConstraintName))
		}
	}
	attrs = append(attrs, slog.Any("types", errorTypes(err)))
	return slog.GroupValue(attrs...)
}

// errorTypes is the Go type of each error in err's tree, err first and every error after the one
// that wraps it, depth first through a wrapper of several (Unwrap() []error). Each is reflect's
// name of the type, a word of the program's own that holds nothing of the error's value.
func errorTypes(err error) []string {
	var types []string
	var walk func(link error)
	walk = func(link error) {
		for link != nil {
			types = append(types, reflect.TypeOf(link).String())
			if joined, ok := link.(interface{ Unwrap() []error }); ok {
				for _, wrapped := range joined.Unwrap() {
					walk(wrapped)
				}
				return
			}
			link = errors.Unwrap(link)
		}
	}
	walk(err)
	return types
}
