package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

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
	// sendReleased: the send is no failure of the row, so it counts no attempt; the row's claim is
	// released and it is due again after sendVerdict.delay. The hold's deadline refused the send
	// before it left this process (bus.ErrWindowDeadline): due at once, and the next pass to claim
	// it decides it. NATS could not be reached (bus.ErrUnavailable): due after retry.BaseDelay, as
	// an event's batch waits for the connection.
	sendReleased
	// sendFailed: any other failure; the row counts one more failed attempt, logged at the
	// dead-letter threshold (failedAttempt), and is retried after retry.Delay of that count.
	sendFailed
)

// sendVerdict is judgeSend's answer: what the pass does with the row, and for a released row how
// long it waits before it is due again.
type sendVerdict struct {
	outcome sendOutcome
	delay   time.Duration
}

// judgeSend is the one reading of a pass's send answer, err: what the pass does with the row
// (sendVerdict), the count of a send that did not reach NATS, by the batch's own rule (sendChains,
// forwardUnanswered), and the log line of a send that was refused or failed, which names the row
// by what and attrs. forward is the row's role-route send - a forward to the role's holder or a
// keep on its kept lane, both a "forward" as the batch counts one (a destination with a role
// forward) - and nil for a plain publish.
//
// A send the server refused under this connection's own publish permissions is a denial; one NATS
// refuses for good is refused; a role-route send whose holder's resolution held it pending
// (roleForward.err) sent nothing and is counted under that holder; a role-route send the stream
// never acknowledged is unanswered; every other failure is failed, save an answer a stopping
// process stopped waiting for, which says nothing of NATS and counts nothing. A send the hold's
// deadline refused counts nothing and logs nothing.
func judgeSend(ctx context.Context, deps Deps, err error, forward *roleForward, what string, attrs ...any) sendVerdict {
	switch {
	case err == nil:
		return sendVerdict{outcome: sendReached}
	case errors.Is(err, bus.ErrWindowDeadline):
		return sendVerdict{outcome: sendReleased}
	case errors.Is(err, bus.ErrPublishDenied):
		deps.countPublishFailure("denied")
	case errors.Is(err, bus.ErrRefused):
		deps.countPublishFailure("refused")
		slog.Error("dispatch outbox: "+what+" refused", append(attrs, "error", err)...)
		return sendVerdict{outcome: sendRefused}
	case forward != nil && forward.err != nil:
		deps.countPublishFailure("holder")
	case forwardUnanswered(forward != nil, err):
		deps.countPublishFailure("unanswered")
	case !lostToContextEnd(ctx, err):
		deps.countPublishFailure("failed")
	}
	slog.Error("dispatch outbox: publish "+what, append(attrs, "error", err)...)
	if errors.Is(err, bus.ErrUnavailable) {
		return sendVerdict{outcome: sendReleased, delay: retry.BaseDelay}
	}
	return sendVerdict{outcome: sendFailed}
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
