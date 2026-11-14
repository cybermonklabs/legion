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

// The codes a request is answered with when the server could not serve it, or could not say what
// it did: a refusal by a Limiter is a 429, the two failures after admission are 503s, each
// carrying Retry-After and Cache-Control: no-store (writeRetryable), and a call the database
// never answered that may have committed is a 500 with neither header
// (writeOutcomeUnknown).
const (
	// CodeOverloaded is a request a Limiter refused, answered 429 Too Many Requests: nothing of it
	// ran, so any method may be retried.
	CodeOverloaded = "OVERLOADED"
	// CodeDatabaseBusy is a request a bound of the server's own stopped before the database
	// answered it (store.ErrPoolBusy, store.DatabaseStalled): its handler waited its pool's
	// acquire bound for a connection, or the database stopped answering on the connection it
	// held. Answered 503. Nothing of the call that failed committed - that is what tells it from
	// CodeCommitOutcomeUnknown - though a write may have committed an earlier statement of its
	// own before the bound that failed it, its idempotency key's claim among them. A read whose
	// call the database never answered is this too, its transaction's COMMIT included: a read
	// commits nothing, whether or not its transaction was opened read-only, so nothing of it
	// can be in doubt.
	CodeDatabaseBusy = "DATABASE_BUSY"
	// CodeRequestTimeout is a read that ran past its deadline (WithDeadline,
	// WithDeadlineUntilAnswer), answered 503. Only reads have a deadline: a write runs to
	// completion once admitted.
	CodeRequestTimeout = "REQUEST_TIMEOUT"
	// CodeCommitOutcomeUnknown is a call of a request that wrote which the server sent and never
	// got an answer to, so it may have committed: the commit of its transaction
	// (store.ErrCommitTimeout), or a statement it ran outside any transaction, which commits
	// itself (store.ErrStatementOutcomeUnknown). Answered 500 with no Retry-After: a 503 with
	// one would tell the client to send a write the server may already have applied, so the
	// client reads what the write would have written before sending it again, or sends it again
	// under its Idempotency-Key.
	CodeCommitOutcomeUnknown = "COMMIT_OUTCOME_UNKNOWN"
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
// serve it, or could not say what one of its calls did, and reports whether it did; on false the
// caller maps err as it always has. wrote says whether a statement of this request that reached
// the database could have committed anything: every caller reads it off the request's method
// (Reads), but the sign-in routes, whose GET writes (routes.writeUnservedOr500).
//
// Its order is the order the causes are told apart:
//
//   - a call the server sent and never got an answer to, which may have committed, where wrote
//     is true: a commit (store.ErrCommitTimeout), or a statement a pool ran outside any
//     transaction, which commits itself (store.ErrStatementOutcomeUnknown): 500
//     COMMIT_OUTCOME_UNKNOWN with no Retry-After (writeOutcomeUnknown). It is told first
//     because its outcome, unlike every other failure's, is unknown, so the operator's log
//     carries the ERROR line whether or not a client is still there to be told: a write runs on
//     its client's context unless it is keyed, and only the commit itself is detached
//     (store.CommitContext), so a client that leaves while the commit runs out its bound is the
//     likeliest way this is met. The request is then answered as a client that went away is,
//     below, and the line records the 499 that it is;
//   - a request cancelled by a refusal of the units it asked back (Ticket.Resume, whose refusal
//     the caller made ctx's cause): 429 OVERLOADED, as a request refused before it ran is
//     answered. It is told before the cancellation below because that cancellation reads as a
//     client that went away, which it is not: the client is still there, and what it is owed is
//     the refusal;
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
//     rolls back with the transaction it was in;
//   - a call the database never answered, met by a request that wrote nothing: its
//     transaction's COMMIT, or a statement a pool ran outside any transaction. 503
//     DATABASE_BUSY, since what a read runs commits nothing however the database answered it,
//     and a client that sends the read again only reads. It is the method that says the
//     request wrote nothing (Reads), not the access mode of its transaction: GET
//     /api/v1/search opens a plain read-write one (api/search.go), and the store refuses a
//     read-only transaction's unanswered COMMIT on its own account besides
//     (store.commitTimedOut).
//
// A failure of the server's own is none of these and is left to the caller. None of the
// refusals is a fault of the server's to alarm an operator over, and under load every request
// meets one, so each is logged at debug; only the unknown outcome is logged at error.
func WriteUnserved(w http.ResponseWriter, ctx context.Context, err error, wrote bool) bool {
	unanswered := unknownOutcome(err)
	if unanswered != "" && wrote {
		slog.Error("dispatch: the database did not answer "+unanswered+"; its outcome is unknown and it may have committed", "error", err)
		if unserved(w, ctx, err) {
			return true
		}
		writeOutcomeUnknown(w, unanswered)
		return true
	}
	if unserved(w, ctx, err) {
		return true
	}
	if unanswered != "" {
		writeDatabaseBusy(w, err)
		return true
	}
	return false
}

// unknownOutcome describes the call err is of where the database never answered it and it may
// have committed - "this write's commit" (store.ErrCommitTimeout) or "a statement of this
// write" (store.ErrStatementOutcomeUnknown, a statement a pool ran outside any transaction,
// which commits itself) - and is empty for every other error. Its answer is the description the
// log line and the 500 both carry, so the pair is read once and nothing downstream asks which
// of the two it was.
//
// Whether the outcome is in doubt at all is the caller's word, not this one's
// (WriteUnserved's wrote): a request that wrote nothing has none, since what it ran commits
// nothing however the database answered it. Such a request is refused as busy instead, with no
// ERROR line, since there is no unknown outcome to record.
func unknownOutcome(err error) string {
	switch {
	case store.IsCommitTimeout(err):
		return "this write's commit"
	case store.IsStatementOutcomeUnknown(err):
		return "a statement of this write"
	}
	return ""
}

// unserved is the refusals WriteUnserved writes for a request nothing of which can have
// committed, in the order its documentation gives.
func unserved(w http.ResponseWriter, ctx context.Context, err error) bool {
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
		writeDatabaseBusy(w, err)
	default:
		return false
	}
	return true
}

// writeDatabaseBusy answers a request 503 DATABASE_BUSY with Retry-After: a pool that could not
// serve a call, and a stall after which nothing of the request can have committed.
func writeDatabaseBusy(w http.ResponseWriter, err error) {
	slog.Debug("dispatch: request found the database busy", "error", err)
	writeRetryable(w, http.StatusServiceUnavailable, CodeDatabaseBusy, "the database did not answer this request in time; retry after Retry-After")
}

// writeOutcomeUnknown answers a request 500 COMMIT_OUTCOME_UNKNOWN, naming the call the
// database never answered as unknownOutcome described it - this write's commit, or a statement
// of it. It carries no Retry-After and no Cache-Control: the call may have committed, so the
// client reads what the write would have written before sending it again rather than being
// told to wait and repeat it.
func writeOutcomeUnknown(w http.ResponseWriter, unanswered string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "the database did not answer " + unanswered + " in time; its outcome is unknown, so read what it wrote before sending it again",
		"code":  CodeCommitOutcomeUnknown,
	})
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
