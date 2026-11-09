package admission

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The codes a request is answered with when the server could not serve it: a refusal by a Limiter
// is a 429, and the two failures after admission are 503s. Each carries Retry-After and
// Cache-Control: no-store (writeRetryable).
const (
	// CodeOverloaded is a request a Limiter refused, answered 429 Too Many Requests: nothing of it
	// ran, so any method may be retried.
	CodeOverloaded = "OVERLOADED"
	// CodeDatabaseBusy is a request a bound of the server's own stopped before the database
	// answered it (store.ErrPoolBusy, store.DatabaseStalled): its handler waited its pool's
	// acquire bound for a connection, or the database stopped answering on the connection it
	// held. Answered 503. A write may have committed part of what it does before the bound that
	// failed it.
	CodeDatabaseBusy = "DATABASE_BUSY"
	// CodeRequestTimeout is a read that ran past its deadline (WithDeadline,
	// WithDeadlineUntilAnswer), answered 503. Only reads have a deadline: a write runs to
	// completion once admitted.
	CodeRequestTimeout = "REQUEST_TIMEOUT"
)

// RetryAfter is the Retry-After, in seconds, of every refusal and 503 this package writes, and of
// the document writer gate's 429: one second, after which the burst that refused the request may
// have passed.
const RetryAfter = "1"

// ErrRequestDeadline is the cause of a read's context whose deadline passed (WithDeadline,
// WithDeadlineUntilAnswer). It tells the read's own deadline from every other deadline its
// handler meets - a pool's acquire bound, a client's timeout of its own - so only it is answered
// REQUEST_TIMEOUT.
var ErrRequestDeadline = errors.New("the read ran past its deadline")

// WithDeadline bounds a read's context at timeout, with ErrRequestDeadline as the cause.
func WithDeadline(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, timeout, ErrRequestDeadline)
}

// WithDeadlineUntilAnswer bounds a read's context at timeout as WithDeadline does, and hands back
// a stop that takes that bound off: a read whose answer has begun is no longer bounded by it, and
// nothing but the answer's own transfer bound (the api package's boundedAnswer for the answer's
// writes, and storeBody for a download's reads of the file store) decides how long the rest of
// it may take. Until stop is called the context ends at timeout with
// ErrRequestDeadline as its cause, so DeadlinePassed tells this deadline from every other one
// exactly as it does WithDeadline's; after it the context ends only with its parent, or with
// cancel.
//
// A file version download is the read that needs it (apiRoute.Download): the deadline bounds the
// work the server does for itself - the row's read and the store's open - and the bytes that
// follow move at the reader's pace, which the transfer bound already measures. Its context
// carries no deadline of its own (ctx.Deadline), since nothing says when it ends: a statement run
// under it is bounded by its pool's acquire bound and by the commit bound, as every other
// statement is.
func WithDeadlineUntilAnswer(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(timeout, func() { cancel(ErrRequestDeadline) })
	stop := func() { timer.Stop() }
	return ctx, func() {
		timer.Stop()
		cancel(nil)
	}, stop
}

// DeadlinePassed reports whether ctx is a read's context whose own deadline has passed.
func DeadlinePassed(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrRequestDeadline)
}

// WriteRefused answers a request a Limiter refused (Admit's *Refusal, EnterLane's) 429
// OVERLOADED. A refusal by a heavy read's lane names the lane, so the client knows which route is
// busy rather than reading that every read is. A request that carries a body has it drained
// first, by its caller: this writes the answer only.
func WriteRefused(w http.ResponseWriter, refusal *Refusal) {
	what := "another " + refusal.Class.String() + " request"
	if refusal.Lane != NoLane {
		what = "another request of the " + refusal.Lane.String() + " lane"
	}
	writeRetryable(w, http.StatusTooManyRequests, CodeOverloaded, "the server has no room for "+
		what+" ("+string(refusal.Reason)+"); nothing ran, retry after Retry-After")
}

// WriteUnserved answers a request whose handler failed with err because the server could not
// serve it, and reports whether it did; on false the caller maps err as it always has. Its
// order is the order the causes are told apart:
//
//   - a request cancelled by a refusal of the units it asked back (Ticket.Resume, whose refusal
//     the caller made ctx's cause): 429 OVERLOADED, as a request refused before it ran is
//     answered. It is told first because that cancellation reads as a client that went away,
//     which it is not: the client is still there, and what it is owed is the refusal;
//   - the read's deadline passed (DeadlinePassed): 503 REQUEST_TIMEOUT, whatever err says,
//     since every failure after the deadline is the deadline's;
//   - the client went away (ctx cancelled): nothing is written, since nobody reads it, and the
//     access log records the request as 499 rather than as the 200 a handler that wrote nothing
//     would otherwise be logged as (accesslog.ClientGone);
//   - a bound of the server's own ended the call before the database answered it, and nothing
//     of it can have committed: no connection of a pool came free within its acquire bound
//     (store.IsPoolBusy), or the database stopped answering on the connection the handler held
//     while the request's own context was still live (store.DatabaseStalled, a pgx socket
//     timeout pgx raises only from a context it was watching). 503 DATABASE_BUSY. The second is
//     read off the error here as well as inside the store, because a statement inside a
//     transaction runs on pgx directly, never passes through a pool's own error mapping, and
//     rolls back with the transaction it was in.
//
// A failure of the server's own is none of these and is left to the caller, and so are the two
// calls a lost answer leaves with an unknown outcome: a commit (store.ErrCommitTimeout), which
// was once answered DATABASE_BUSY by a guess from its deadline, and a statement a pool ran
// outside a transaction and the database may still have run
// (store.ErrStatementOutcomeUnknown). Neither may be sent again unanswered, so neither is
// written here; api.writeHandlerError answers both, and a read's handler, whose statements
// commit nothing, refuses the second as busy with WriteDatabaseBusy.
//
// None of these is a fault of the server's to alarm an operator over, and under load every
// request meets one, so each is logged at debug and never at error.
func WriteUnserved(w http.ResponseWriter, ctx context.Context, err error) bool {
	var refusal *Refusal
	switch {
	case errors.As(context.Cause(ctx), &refusal):
		slog.Debug("dispatch: request refused the units it asked back", "error", refusal)
		WriteRefused(w, refusal)
	case DeadlinePassed(ctx):
		slog.Debug("dispatch: read ran past its deadline", "error", err)
		writeRetryable(w, http.StatusServiceUnavailable, CodeRequestTimeout, "the read ran past its deadline; retry after Retry-After")
	case errors.Is(ctx.Err(), context.Canceled):
		slog.Debug("dispatch: request's client went away", "error", err)
		accesslog.ClientGone(ctx)
	case store.IsPoolBusy(err), store.DatabaseStalled(ctx, err):
		WriteDatabaseBusy(w, err)
	default:
		return false
	}
	return true
}

// WriteDatabaseBusy answers a request 503 DATABASE_BUSY with Retry-After, the answer
// WriteUnserved gives a pool that could not serve a call. It is exported for the one caller
// that decides for itself that a stalled call of its own is retryable: a read's handler, whose
// statements run outside a transaction but commit nothing, so the unknown outcome
// store.ErrStatementOutcomeUnknown carries is no unknown outcome there
// (api.writeHandlerError).
func WriteDatabaseBusy(w http.ResponseWriter, err error) {
	slog.Debug("dispatch: request found the database busy", "error", err)
	writeRetryable(w, http.StatusServiceUnavailable, CodeDatabaseBusy, "the database did not answer this request in time; retry after Retry-After")
}

// writeRetryable writes status with code and message in Dispatch's {error, code} body, telling
// the client to retry after RetryAfter and every cache not to keep it.
func writeRetryable(w http.ResponseWriter, status int, code, message string) {
	header := w.Header()
	header.Set("Retry-After", RetryAfter)
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
}
