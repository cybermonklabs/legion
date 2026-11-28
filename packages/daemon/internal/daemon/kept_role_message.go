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
	after string
	// took names the message in the hour's clause: "it", "the message".
	took    string
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
// keptRoleMessageEnded gives a message it ends before any attempt to publish it. A message past
// its hour carries pastItsHour, worded with the executor's moment, which is
// errKeptRoleMessagePastItsHour (errors.Is).
var (
	errKeptRoleMessagePastItsHour   = errors.New("the message's hour has passed")
	errKeptRoleMessageRoleNotComing = errors.New("the role's claim failed or was retired")
)

// pastItsHour is the cause of a kept role message whose hour has passed, after naming the moment
// the hour ran from: "the hour after the hook fired has passed".
type pastItsHour struct{ after string }

func (e pastItsHour) Error() string { return "the hour after " + e.after + " has passed" }

func (pastItsHour) Is(target error) bool { return target == errKeptRoleMessagePastItsHour }

// keptRoleMessageEnded is the end of a kept role message's row that no attempt to publish it can
// change, and nil while the message may still be delivered, read from wake, the attempt's first
// read of the claim (claimWakeOf). publishKeptToRole runs it at the top of every attempt, before
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
			return m.roleNotComing(wake.view.State), nil
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
		return &keptRoleMessageUndelivered{what: m.what, cause: pastItsHour{after: m.after},
			why: fmt.Sprintf("the hour after %s passed and no session of %s took %s", m.after, m.who, m.took)}, nil
	}
	return nil, nil
}

// roleNotComing is the end of m's row whose role's claim is in state, failed or retired, so that no
// session of the role holds it again (errKeptRoleMessageRoleNotComing).
func (m keptRoleMessage) roleNotComing(state supervise.ClaimState) *keptRoleMessageUndelivered {
	return &keptRoleMessageUndelivered{what: m.what, cause: errKeptRoleMessageRoleNotComing,
		why: fmt.Sprintf("%s's claim is %s, so no session of it will hold the role again", m.who, state)}
}

// publishKeptToRole is the one flow of a kept role message's row (keptRoleMessage), the flow
// hookPrompt and phaseEnded share. Each attempt reads the role's claim (claimWakeOf), then:
//
//   - The row's end, read before anything wakes or publishes (keptRoleMessageEnded): a role whose
//     claim failed or was retired, or an hour that passed with no wake coming. It is answered as
//     the *keptRoleMessageUndelivered, with no error, and the executor words the end.
//   - A role whose pod was released is not "no live session holds the role": its session is intact
//     and its pod is one wake away, so the row wakes it and waits for that launch (deliveryWake). A
//     launch the wake pacing holds is waited for until it runs, the claim read again at its turn or
//     parkedWakeBound on, whichever comes first (parkedWait). Either wait is one for any change of
//     the role's claim (launchWait), so the launch, its registration and its ready each make the
//     row due at once, and it publishes as the gate lets it (roleWaitGate). The daemon's own wake
//     brings the pod back whatever the listener does with the publish; the listener's own report of
//     a publish to a role with no live holder meets the role's open resume, and the pod starts once.
//   - The gate every publish of the outbox goes through (roleWaitGate), which reads the claim
//     again just before the publish: a claim that failed or was retired since the attempt's first
//     read ends the row as keptRoleMessageEnded ends it (roleNotComing), so a failed or retired
//     role is never published to, and a wait the gate decides is the row's wait.
//   - The publish, with keep, to the Envoy topic of the role's claim (notify.RoleTopicPrefix and
//     the claim's token), under the row's own key (publishKept). A retry of the row while its copy
//     is still on the role's kept lane stores nothing new; notify.HTTPPublisher.PublishKept gives
//     the listener's answers and what a repeat after the copy's hand-over does.
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
//     (notify.ErrHolderUnresponsive): the row is the message's keeper, so it waits for its role
//     (roleWait), tried again on the capped backoff of a role wait
//     (dispatch://INFRA-4131/spec#b-r-capped), until a session of the role holds it or an
//     attempt's keptRoleMessageEnded ends the row. A released role's woken session claims the role
//     a few attempts after the resume launched it, and a role with no claim yet may be started
//     inside the hour. A refused publish stores nothing, so the one publish a session takes is the
//     one it is handed, and a retry of a publish whose answer was lost is stored once, under the
//     row's own key (record.OutboxKey), which every attempt sends;
//   - every other failed publish (publishFailed) but the next one: a holder that did not confirm in
//     time, one that does not advertise the delivery mode, a forward the listener's own NATS grant
//     denies, any other refusal or 5xx of the listener's, and a publish that did not reach the
//     listener or could not read its answer: the row waits for its role the same way, since a
//     later attempt can go through, the failure being the listener's last answer its wait's lines
//     name;
//   - a message the listener refuses for itself, a 400 or a 413 with its own error
//     (notify.ErrMessageRefused): the role wait it is ends the row undelivered at that run, with
//     one line at Error and no wait, since no retry of the same message changes that answer
//     (messageRefused, permanentRefusal, finishRefused).
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
// Every executor that publishes to a role's topic, the controller's role topic among them
// (record.OutboxKind.WaitsForRole: notice, controller_notice, merge_queue_removal,
// merge_holds_changed, merge_forward, hook_prompt, phase_ended, role_message and update_ask),
// passes through the one gate roleWaitGate (daemon/role_wait.go,
// dispatch://INFRA-4131/spec#b-r-wait-ready) before publishing, and turns every failed publish
// that keeps its row (publishFailed) into a roleWait on the capped backoff
// (dispatch://INFRA-4131/spec#b-r-capped):
// whoever changes the rule for waiting on a role changes roleWaitGate and role_wait.go rather than
// chasing the nine executors apart. The one role wait that ends its row, a message the listener
// refuses for itself (messageRefused), is settle's end for every kind rather than an executor's.
// The executors still own their own non-claim ends and waits, and what each listener refusal ends
// rather than keeps (notice's fence and architectEnded, roleMessage's no-holder/capability/denied
// ends, updateAsk's round and no-holder end, keptRoleMessageEnded's hour, deliverHeld's
// design-gate and session checks, and controllerNotice's read that the registered controller
// session holds the controller's role live, controllerHolds, whose wait is a roleWait too). The
// seven of them other than this function's two kinds publish with keep: false: deliverNotice, the
// three through deliverHeld and controllerNotice hold their own row, and a kept copy beside it
// would be a second keeper of one message; roleMessage's message is stale by the time a role nobody
// holds is claimed; and updateAsk's round ends at its bound whether or not the role answers.
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
		return nil, launchWait(token, r.parkedWait(turn, fmt.Sprintf("%s waits for its %s's launch, which the wake pacing holds until %s",
			m.subject, m.role, turn.UTC().Format(time.RFC3339))))
	} else if waits {
		return nil, launchWait(token, wait.Errorf("%s waits for its %s's released pod to come back", m.subject, m.role))
	}
	topic := notify.RoleTopicPrefix + string(token)
	gate := r.roleWaitGate(row, topic)
	if gate.ended != "" {
		return m.roleNotComing(gate.view.State), nil
	}
	if gate.wait != nil {
		return nil, gate.wait
	}
	keeps, err := r.publishKept(ctx, topic, m.message, m.message, record.OutboxKey(row.ID))
	switch {
	case err == nil:
		if keeps {
			r.log.Info(m.kept, m.fields...)
		}
		return nil, nil
	case errors.Is(err, notify.ErrNoHolder), errors.Is(err, notify.ErrHolderUnresponsive):
		return nil, r.roleWait(topic, gate.view, err, wait.Errorf("%s waits for a session to hold its %s's role: %w", m.subject, m.role, err))
	}
	return nil, r.roleWait(topic, gate.view, err, wait.Errorf("%s waits for its %s to take it: %w", m.subject, m.role, err))
}
