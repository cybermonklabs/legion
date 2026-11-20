package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// A take-over that replaced a lapsed row of the publisher hold ends the database sessions of the
// process the row named: once before this process publishes (tryHold), and, when that ending did
// not finish by its deadline, once more beside the first scan (publish). This file holds the
// endings and the lines they log; tryHold, publish and Run own when they run.

// endedSessionLine is the line endReplacedSessions logs for each session of the previous holder it
// ended. cmd/dispatch's two-process rows wait on its words.
const endedSessionLine = "dispatch outbox: ended a database session of the publisher hold's previous holder"

// replacedSessionsLine is the line endReplacedSessions logs once it has ended the previous
// holder's sessions.
const replacedSessionsLine = "dispatch outbox: replaced the publisher hold's lapsed row and ended its holder's sessions"

// lateTakeOverLine is the line logged at WARN, with one of the causes below, when a take-over
// that replaced a lapsed row may start its first batch past takeOverWithin: tryHold's, when it
// did not end the previous holder's sessions by the ending's deadline, and scan's, when the first
// batch's reads did not answer by the take-over's budget.
const lateTakeOverLine = "dispatch outbox: late take-over of the publisher hold"

// The causes tryHold names on lateTakeOverLine, and scan the last of them, when the take-over's
// first batch's reads did not answer by the take-over's budget.
const (
	lateCauseTakeAfterDeadline = "the take came after the ending's deadline"
	lateCauseEndingTimedOut    = "the ending of its sessions did not answer by its deadline"
	lateCauseEndingFailed      = "the ending of its sessions failed"
	lateCauseFirstReads        = "the first batch's reads did not answer within their bound"
)

// untakenLine is the line tryHold logs when the process stopped while it ended the previous
// holder's sessions, before it published: the take-over stopped, and is no late take-over.
const untakenLine = "dispatch outbox: stopped during a take-over of the publisher hold, before publishing, and released it"

// The lines endLateSessions logs as its ending of a late take-over's sessions ends: done, or not
// done with its cause (lateEndingCause).
const (
	lateEndingDoneLine   = "dispatch outbox: ended the previous holder's sessions after a late take-over"
	lateEndingFailedLine = "dispatch outbox: did not end the previous holder's sessions after a late take-over"
)

// errNotHolding is why an ending of the previous holder's sessions was not made, or was cut: the
// publisher hold this process took over is no longer its own - the hold was lost, or found
// another's - and so the previous holder may hold it again, with sessions of its own that are no
// stopped process's to end.
var errNotHolding = errors.New("the publisher hold is no longer this process's")

// errGaveUp is why a late take-over's ending was cut when this process gave the hold up for NATS
// lost: the hold is released, and the previous holder may take it back.
var errGaveUp = errors.New("this process gave the publisher hold up")

// endReplacedSessions ends every database session of previous, the process whose lapsed row of
// the publisher hold this process's take replaced as hold (store.Pool.EndSessionsOf), by
// deadline, logs each one it ended, and returns the ending's error, which it logs too. It makes no
// ending once hold is lost (errNotHolding), and checks that before each ending: a standby that
// lost the hold must not end the sessions of a holder that may have taken it since.
//
// A holder's row lapses without a release only when the holder could not send one: it was killed,
// frozen (SIGSTOP, a VM paused), or cut off from the database. A frozen process still holds every
// connection it had, and Postgres never ends a session whose client is merely silent, so a
// transaction it had open when it froze keeps its locks - an issue's owner lock among them - and
// every write of that issue, through any process, waits on it for as long as the freeze lasts. The
// standby that takes the hold over ends those sessions, which rolls the transactions back and
// frees the locks within the take-over's six seconds. A process that was only slow, and runs on,
// meets the ended sessions as SQLSTATE 57P01 (store.SessionEnded) on the requests they served,
// which the API answers 503 DATABASE_SESSION_ENDED; the first of them has its pools drop every
// connection they hold (store.Pool.onSessionEnded), and it stands by for the hold like any other
// process.
//
// A failure is logged and the take stands: the process holds the hold, and a session it could not
// end is the frozen process's alone until the ending is made again (publish). When that second
// ending fails too, nothing ends those sessions again: they stand until the frozen process runs
// and ends them itself, or until Postgres finds its connection gone.
func (o *Outbox) endReplacedSessions(ctx context.Context, hold *duty.Hold, previous uuid.UUID, deadline time.Time) error {
	select {
	case <-hold.Lost():
		return errNotHolding
	default:
	}
	ending, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ended, err := o.deps.Store.Pool.EndSessionsOf(ending, previous)
	if err != nil {
		slog.Error("dispatch outbox: end the sessions of the publisher hold's previous holder",
			"instance", o.instance, "previous", previous, "deadline", deadline.Format(time.RFC3339Nano), "error", err)
		return err
	}
	for _, session := range ended {
		slog.Info(endedSessionLine, "instance", o.instance, "previous", previous,
			"pid", session.PID, "application_name", session.ApplicationName, "signalled", session.Signalled)
	}
	slog.Info(replacedSessionsLine, "instance", o.instance, "previous", previous, "sessions_ended", len(ended))
	return nil
}

// releaseUntaken releases hold, which tryHold took over from previous and the process's stop
// kept it from publishing under, and logs that by name (untakenLine): a stop that ended the
// ending of previous's sessions is a stop, not a late take-over, and the process stands by no
// more. The release is the one publish makes, so a standby is woken at once.
func (o *Outbox) releaseUntaken(ctx context.Context, hold *duty.Hold, previous uuid.UUID) {
	slog.Info(untakenLine, "instance", o.instance, "previous", previous)
	release, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := hold.Release(release); err != nil {
		slog.Error("dispatch outbox: release the publisher hold", "instance", o.instance, "error", err)
	}
}

// endLateSessions ends the sessions of previous, which tryHold gave up on at the ending's deadline
// under the take-over of hold, under recordTimeout, and logs how that ending ended, by name:
// lateEndingDoneLine, or lateEndingFailedLine with its cause. publish runs it on a goroutine of its
// own beside the first scan, under a context of the turn as holder that publish ends when serve
// returns, and waits for it before it releases the hold. The ending is cut, too, the moment hold
// is lost (errNotHolding): from then on the previous holder may take the hold back, and its new
// sessions are no stopped process's.
func (o *Outbox) endLateSessions(ctx context.Context, hold *duty.Hold, previous uuid.UUID) {
	ending, cut := context.WithCancelCause(ctx)
	defer cut(nil)
	go func() {
		select {
		case <-hold.Lost():
			cut(errNotHolding)
		case <-ending.Done():
		}
	}()
	if err := o.endReplacedSessions(ending, hold, previous, time.Now().Add(recordTimeout)); err != nil {
		slog.Error(lateEndingFailedLine, "instance", o.instance, "previous", previous, "cause", lateEndingCause(ending, err))
		return
	}
	slog.Info(lateEndingDoneLine, "instance", o.instance, "previous", previous)
}

// lateEndingCause names why endLateSessions's ending, under ctx, ended with err.
func lateEndingCause(ctx context.Context, err error) string {
	switch cause := context.Cause(ctx); {
	case errors.Is(err, errNotHolding) || errors.Is(cause, errNotHolding):
		return errNotHolding.Error()
	case errors.Is(cause, errGaveUp):
		return errGaveUp.Error()
	case ctx.Err() != nil:
		return "the outbox stopped"
	case errors.Is(err, context.DeadlineExceeded):
		return "the ending did not answer within recordTimeout"
	default:
		return "the ending failed"
	}
}

// turnEndCause is the cause publish cuts its turn's late ending with, once serve returned end: a
// lost hold is errNotHolding, a hold given up for NATS lost errGaveUp, and a stop the process's
// own, which its context already carries.
func turnEndCause(end holdEnd) error {
	switch end {
	case holdLost:
		return errNotHolding
	case natsLost:
		return errGaveUp
	default:
		return context.Canceled
	}
}
