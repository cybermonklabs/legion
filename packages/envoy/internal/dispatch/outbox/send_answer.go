package outbox

import (
	"context"
	"errors"
	"log/slog"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
)

// awaitAnswer is the answer a pass's send gave: one already in is read before ctx is, so a stopping
// process records what NATS accepted, and a send still unanswered when ctx ends answers ctx's
// error. The fallback pass (publishFallbackCopies) and the expiry pass (keepExpiredCopiesAgain)
// both read their sends through it.
func awaitAnswer(ctx context.Context, answer <-chan error) error {
	select {
	case err := <-answer:
		return err
	default:
	}
	select {
	case err := <-answer:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendOutcome is what a pass does with a row whose send answered (judgeSend).
type sendOutcome int

const (
	// sendReached: the send reached NATS; the row settles as its pass settles a sent row.
	sendReached sendOutcome = iota
	// sendRefused: NATS refuses the send however often it is sent (bus.ErrRefused); the row
	// settles, since nothing a later send does would be taken, and its pass says what that means
	// for the row.
	sendRefused
	// sendNotSent: the hold's deadline refused the send before it left this process
	// (bus.ErrWindowDeadline), so it is no failure of the row and counts nothing; the row is
	// released due at once, and the next pass to claim it decides it.
	sendNotSent
	// sendUnavailable: NATS could not be reached (bus.ErrUnavailable); the row is released after
	// retry.BaseDelay, as an event's batch waits for the connection, counting no attempt.
	sendUnavailable
	// sendFailed: any other failure; the row counts one more failed attempt and is retried after
	// retry.Delay of that count (failedAttempt).
	sendFailed
)

// judgeSend is the one reading of a pass's send answer, err: what the pass does with the row
// (sendOutcome), and the count of a send that did not reach NATS, by the batch's own rule
// (sendChains, forwardUnanswered). A send the server refused under this connection's own publish
// permissions is a denial; one NATS refuses for good is refused; a role forward whose holder's
// resolution held it pending (heldPending) sent nothing and is counted under that holder; a
// forward (isForward) the stream never acknowledged is unanswered; every other failure is failed,
// save an answer a stopping process stopped waiting for, which says nothing of NATS and counts
// nothing. A send the hold's deadline refused counts nothing either.
func judgeSend(ctx context.Context, deps Deps, err error, isForward, heldPending bool) sendOutcome {
	switch {
	case err == nil:
		return sendReached
	case errors.Is(err, bus.ErrWindowDeadline):
		return sendNotSent
	case errors.Is(err, bus.ErrPublishDenied):
		deps.countPublishFailure("denied")
	case errors.Is(err, bus.ErrRefused):
		deps.countPublishFailure("refused")
		return sendRefused
	case heldPending:
		deps.countPublishFailure("holder")
	case forwardUnanswered(isForward, err):
		deps.countPublishFailure("unanswered")
	case !lostToContextEnd(ctx, err):
		deps.countPublishFailure("failed")
	}
	if errors.Is(err, bus.ErrUnavailable) {
		return sendUnavailable
	}
	return sendFailed
}

// failedAttempt reports a row's failed attempt, its attempts'th: the attempt that brings a row's
// count to retry.DeadLetterAttempts, the threshold the event scan logs at, is logged at Error with
// attrs naming the row, and the row is retried on the same schedule after it (retry.Delay of its
// count). Nothing about a send failing is permanent, so no row is given up here.
func failedAttempt(attempts int, what string, attrs ...any) {
	if attempts == retry.DeadLetterAttempts {
		slog.Error("dispatch outbox: "+what+" reached dead-letter threshold", append(attrs, "attempts", attempts)...)
	}
}
