package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// keptRoleMessageHoldBound is how long after its row was made a message the daemon publishes with
// keep to a role and no source session, a hook prompt (hookPrompt) or a phase-ended message
// (phaseEnded), waits for a session to hold its role (keptRoleMessageEnded): the hour the Envoy
// listener kept such a message for (its keep window, roleKeepWindow in
// packages/envoy/cmd/listener/role_keep.go) before it refused to keep a publish that names no
// source session. So a session of the role that comes within the hour gets the message, as it got
// the kept copy, and one that comes later does not, as it did not.
const keptRoleMessageHoldBound = time.Hour

// keptRoleMessage is one row that publishes with keep to the role of its issue and names no source
// session, as its executor words it: a hook prompt (hookPrompt) or a phase-ended message
// (phaseEnded). publishKeptToRole runs the one flow both take; the executors differ only in these
// words and in what they do with the row's end (keptRoleMessageUndelivered).
type keptRoleMessage struct {
	// what names the kind in the end's error: "the hook prompt".
	what string
	// subject names this message in the row's waits and errors and in the wake it asks for: "the
	// review hook's prompt for LEGION-208", "the end of LEGION-208's testing phase".
	subject string
	// who names the role in the end's clauses: "the implementer", "the role".
	who string
	// after names the moment the message's hour runs from: "the hook fired", "the phase ended".
	after   string
	role    claim.Role
	message string
	// kept is the Info line logged, with fields, when a listener keeps the message for the role's
	// next holder.
	kept   string
	fields []any
}

// keptRoleMessageUndelivered is a kept role message's row ending undelivered
// (keptRoleMessageEnded): why is the clause that says why no session of its role will be handed
// it, which each executor words its end with. For a hook prompt it is a failure no retry repairs
// (permanentRefusal), and the row is finished with the message on the issue that carries why
// (finishRefused, record.HookPrompt.Undelivered); a phase-ended message logs it and ends its row.
type keptRoleMessageUndelivered struct {
	what  string
	why   string
	cause error
}

func (e *keptRoleMessageUndelivered) Error() string {
	return e.what + " is not delivered, because " + e.why + ": " + e.cause.Error()
}

func (e *keptRoleMessageUndelivered) Unwrap() error { return e.cause }

// errKeptRoleMessagePastItsHour and errKeptRoleMessageRoleNotComing are the causes
// keptRoleMessageEnded gives a message it ends before any attempt to publish it.
var (
	errKeptRoleMessagePastItsHour   = errors.New("the message's hour has passed")
	errKeptRoleMessageRoleNotComing = errors.New("the role's claim failed or was retired")
)

// keptRoleMessageEnded is the end of a kept role message's row that no attempt to publish it can
// change, and nil while the message may still be delivered, read from wake, the claim's one read
// of this attempt (claimWakeOf). publishKeptToRole runs it at the top of every attempt, before
// anything wakes the role or publishes. Two ends:
//   - A role whose claim has failed or was retired gets no session that holds the role again.
//   - A message whose hour has passed: keptRoleMessageHoldBound from the later of the row's making
//     and the role's last ready (supervise.Claim.LastReady). So the hour a woken session has is its
//     own, as the kept copy's was counted from its publish. While a wake of the role is coming
//     (wakeComing), the row waits for the woken session, as the architect's ruling has it: the row
//     waits while the role is being woken, and the hour does not end it then.
//
// So a session that held the role all along is not handed a message past its hour, and a failed
// or retired role is never published to.
func (r *outbox) keptRoleMessageEnded(ctx context.Context, row record.OutboxRow, wake claimWake, m keptRoleMessage) (*keptRoleMessageUndelivered, error) {
	start := row.CreatedAt
	if wake.machine != nil {
		if supervise.FailedOrRetired(wake.view.State) {
			return &keptRoleMessageUndelivered{what: m.what, cause: errKeptRoleMessageRoleNotComing,
				why: fmt.Sprintf("%s's claim is %s, so no session of it will hold the role again", m.who, wake.view.State)}, nil
		}
		coming, err := r.wakeComing(ctx, row.Issue, wake)
		if err != nil || coming {
			return nil, err
		}
		if wake.view.LastReady.After(start) {
			start = wake.view.LastReady
		}
	}
	if r.now().Sub(start) >= keptRoleMessageHoldBound {
		return &keptRoleMessageUndelivered{what: m.what, cause: errKeptRoleMessagePastItsHour,
			why: fmt.Sprintf("the hour after %s passed and no session of %s took it", m.after, m.who)}, nil
	}
	return nil, nil
}

// publishKeptToRole is the one flow of a kept role message's row (keptRoleMessage), the flow
// hookPrompt and phaseEnded share. Each attempt reads the role's claim once (claimWakeOf), then:
//
//   - The row's end, read before anything wakes or publishes (keptRoleMessageEnded): a role whose
//     claim failed or was retired, or an hour that passed with no wake coming. It is answered as
//     the *keptRoleMessageUndelivered, with no error, and the executor words the end.
//   - A role whose pod was released is not "no live session holds the role": its session is intact
//     and its pod is one wake away, so the row wakes it and waits on its backoff, and the row's
//     first attempt after that resume has launched publishes (deliveryWake). A launch the wake
//     pacing holds is waited for until it runs, the claim read again at its turn or
//     parkedWakeBound on, whichever comes first (parkedWait). The daemon's own wake brings the pod
//     back whatever the listener does with the publish; the listener's own report of a publish to
//     a role with no live holder meets the role's open resume, and the pod starts once.
//   - The publish, with keep, to the Envoy topic of the role's claim (notify.RoleTopicPrefix and
//     the claim's token), under the row's own key, through the gate every publish of the outbox
//     goes through (publishKept). A retry of the row while its copy is still on the role's kept
//     lane stores nothing new; notify.HTTPPublisher.PublishKept gives the listener's answers and
//     what a repeat after the copy's hand-over does.
//
// What each answer of the publish does to the row:
//
//   - a live holder took the message: the row ends, with nothing logged;
//   - the listener kept the message on the role's kept lane for one hour: the row ends with kept
//     logged at Info with fields, the listener being the message's keeper from there; a listener
//     that refuses to keep a publish naming no source session, as the daemon's hook prompts and
//     phase-ended messages name none, never answers so;
//   - no live session holds the role and the listener kept nothing (notify.ErrNoHolder), the
//     role's claim records a session it does not hold and nothing was published (the gate's
//     notify.ErrNoHolder), or a holder the listener calls unresponsive
//     (notify.ErrHolderUnresponsive): the row is the message's keeper, so it waits
//     (wait.ErrWaiting), tried again on its backoff, until a session of the role holds it or an
//     attempt's keptRoleMessageEnded ends the row. A released role's woken session claims the role
//     a few attempts after the resume launched it, and a role with no claim yet may be started
//     inside the hour. A refused publish stores nothing, so the one publish a session takes is the
//     one it is handed, and a retry of a publish whose answer was lost is stored once, under the
//     row's own key (record.OutboxKey), which every attempt sends;
//   - every other failure: the error is returned, wrapped with the message's subject, and the row
//     keeps its retry.
//
// The row is not held for a holder as an architect's notice is held, and it can reach a later
// session than the one it was written for: for a role nobody claims, the listener hands a kept
// copy to the first session that claims the role inside the hour, and a waiting row reaches the
// first session that holds the role inside the hour after the row was made
// (keptRoleMessageHoldBound), which can be a later round's. That is intended — the role's topic
// names the issue's role, not one session of it — and the hour bounds it: the listener drops a
// copy no live holder took inside its hour (roleKeepWindow, packages/envoy/cmd/listener/
// role_keep.go), and a row ends at its own, so no session is handed a message older than that,
// and each message names its move back or its phases, so its reader judges which round it was for.
//
// Five other executors publish to a role's topic, each with its own rules for what the publisher
// answers: updateAsk, through publishToRole, ends the row for a topic with no holder and retries
// every other failure; deliverNotice, an architect's notice, holds the row for a topic with no
// holder while the owning architect can hold its role again (noticeAnswered); roleMessage ends the
// row for a topic with no holder, waits for a holder the listener calls unresponsive
// (notify.ErrHolderUnresponsive), and ends the row for a holder that refuses the delivery mode
// (notify.ErrRefusedByHolder) or a forward the listener's own NATS grant denies
// (notify.ErrPublishDenied); mergeQueueRemoval, the merger's message of a removal from the merge
// queue, holds the row for a topic with no holder and for an unresponsive holder while the
// merger's claim has neither failed nor retired, and retries every other failure
// (merge_queue_removal.go); and mergeForwardMessage, a message of a merge forward, holds its row
// the same way while the role's claim has neither failed nor retired and the merge forward still
// stands at the row's head (merge_forward.go). Each of those five publishes with keep: false:
// deliverNotice, mergeQueueRemoval and mergeForwardMessage hold their own row, and a kept copy
// beside it would be a second keeper of one message; roleMessage's message is stale by the time a
// role nobody holds is claimed; and updateAsk's round ends at its bound whether or not the role
// answers. So whoever changes the rule for a topic with no holder must read all seven executors,
// not this function alone.
func (r *outbox) publishKeptToRole(ctx context.Context, row record.OutboxRow, token claim.Token, m keptRoleMessage) (*keptRoleMessageUndelivered, error) {
	wake, err := r.claimWakeOf(ctx, token)
	if err != nil {
		return nil, err
	}
	if ended, err := r.keptRoleMessageEnded(ctx, row, wake, m); err != nil || ended != nil {
		return ended, err
	}
	if waits, turn, err := r.deliveryWake(ctx, token, wake, m.subject, row.HeldByPause); err != nil {
		return nil, err
	} else if !turn.IsZero() {
		return nil, r.parkedWait(turn, fmt.Sprintf("%s waits for its %s's launch, which the wake pacing holds until %s",
			m.subject, m.role, turn.UTC().Format(time.RFC3339)))
	} else if waits {
		return nil, wait.Errorf("%s waits for its %s's released pod to come back", m.subject, m.role)
	}
	keeps, err := r.publishKept(ctx, notify.RoleTopicPrefix+string(token), m.message, m.message, record.OutboxKey(row.ID))
	switch {
	case err == nil:
		if keeps {
			r.log.Info(m.kept, m.fields...)
		}
		return nil, nil
	case errors.Is(err, notify.ErrNoHolder), errors.Is(err, notify.ErrHolderUnresponsive):
		return nil, wait.Errorf("%s waits for a session to hold its %s's role: %w", m.subject, m.role, err)
	}
	return nil, fmt.Errorf("publish %s to its %s: %w", m.subject, m.role, err)
}
