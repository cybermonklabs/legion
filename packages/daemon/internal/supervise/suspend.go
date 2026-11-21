package supervise

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/wait"
)

// ErrSuspendHeld answers a suspension that arrived while the claim's agent is in a turn: the
// machine holds it and suspends the claim itself (holdSuspension). It is not a refusal. A caller
// that keeps the request durably asks again, and the claim answers nil once it is suspended.
var ErrSuspendHeld = wait.New("the suspension is held for the agent's turn to end")

// ErrPauseEnded answers a project pause's stop (RequestSuspend.Pause) that reaches its claim once
// that pause is no longer the open one (Deps.Paused): the pause the stop names has ended, so
// nothing was stopped. The resume ended it between the stop's ask and the machine's decision —
// the `pause_stop` executor read the pause open, and the resume committed before its Handle was
// decided — or a later pause followed it, which asks its own stop. It is not a refusal: the
// stop's asker is answered that its pause no longer asks anything (suspendRequest).
var ErrPauseEnded = errors.New("the pause this stop names has ended, so nothing was stopped")

// suspend stops the process and keeps the session. The daemon's own suspension ends the claim's
// phase (the workflow suspends a worker only when its issue closes or its run is over), so a task
// queued for a phase and still pending unconfirmed (acknowledged and then refused, or lost to the
// transport) is retired with it (settle): the next resume is started with its new phase's task,
// never handed the finished one's. A task of no phase — an operator's own, an architect's — is
// not the workflow's to end, and neither is the phase task of a claim whose idle pod was released:
// that suspension ends no phase, and the wake that brings the pod back resumes the claim with the
// task it kept (SuspendSource.RetiresPhaseTask).
//
// A stop that names the moment its message reached the claim (RequestSuspend.TurnAfter, the work
// hold's) does not stop a ready or idle claim at once: the message starts a turn, and the stop is
// held for that turn as a stop that arrives in a turn is (holdSuspension), to its deadline. A claim
// whose last turn ended after the message reached it has had that turn — the stop's row ran late,
// or was posted again after a restart — and is stopped now, and so is one whose deadline has come.
func suspend(m *Machine, ctx context.Context, ev Event) error {
	request, err := suspendRequest(m, ev)
	if err != nil {
		return err
	}
	if m.awaitsTurn(request) {
		m.log.Info("supervise: the suspension waits for the turn its message starts", "reason", request.Reason, "by", request.By,
			"messageAt", request.TurnAfter, "deadline", request.Deadline)
		return holdSuspension(m, ctx, ev)
	}
	return m.suspendNow(ctx, request, false)
}

// awaitsTurn is whether request, as suspendRequest handed it, waits for the turn its message
// starts rather than stopping the claim now (suspend): it names when its message reached the
// claim, the claim is ready or idle, no turn has ended since the message, and the stop's deadline
// is still ahead.
func (m *Machine) awaitsTurn(request RequestSuspend) bool {
	if request.TurnAfter.IsZero() || (m.claim.State != StateReady && m.claim.State != StateIdle) {
		return false
	}
	return !m.claim.LastTurnEnd.After(request.TurnAfter) && m.deps.Clock.Now().Before(request.Deadline)
}

// holdSuspension is a suspension that arrives while the agent is in a turn, and this is the one
// statement of what a held suspension (Machine.held) does. The work hold's stop of a ready or
// idle claim is held here too, for the turn its close message starts (suspend, awaitsTurn).
//
// A worker's issue can close, or an operator suspend it, while it is in a turn: the production
// check's sign-off, say, arriving while the implementer's report of the check is still a tool call
// inside that turn, or an architect's own sign-off moving its tree into linger. Stopping the
// process at once would cut that call off before Oh My Pi writes its result, and a session resumed
// from that transcript holds a report with no answer. So the suspension is held:
//
//   - It runs when the turn ends (turnEnded), or at its deadline if the turn has not ended by then
//     (TimerSuspend, suspendAtTimeout). The operator's suspension has one at the stop timeout from
//     its arrival: an operator stopping the process — to drain the daemon, say — needs the stop
//     bounded. The daemon's own suspension has none unless it names one
//     (RequestSuspend.Deadline), and waits for the turn however long it takes: the tree's linger
//     close or the child's close as done ends the process otherwise. Asked again meanwhile, it is
//     the same hold, under one rule: a request can only move the deadline earlier, and the
//     operator's also takes the hold over, from then on the operator's. The work hold's stop
//     names its own deadline, 2 minutes after its close message reached the claim. It takes over
//     a held release's — the idle release's or a pause's, whose pause it drops, as the operator's
//     does — and the daemon's takes the hold's over. Until the deadline the agent keeps its
//     process and its capability.
//   - A stop the runtime refuses keeps it held and tries again every probe interval (suspendHeld),
//     so the deadline bounds the wait only while the runtime can stop the process. A claim
//     whose turn ended stays idle meanwhile and is handed nothing (sendPending); a turn it starts
//     anyway is cut off at the next try.
//   - A process that dies first, or is found at a stale address (repoint), leaves the claim
//     suspended, charged nothing (endHeld). A prompt retirement meanwhile is the held stop itself
//     (suspendHeld), charged nothing.
//   - A start run against the claim drops a daemon's hold (StartedBy); one the operator's
//     suspension or the work hold's stop holds waits instead (the work hold's end drops the
//     hold's stop, Machine.EndHold), and a project pause's hold whose pause is still the open
//     one stays (decision 5 of dispatch://INFRA-3759's plan). Every other end of the process
//     drops it with the process (letGo).
//   - A project pause's hold (RequestSuspend.Pause) whose pause has ended is dropped at the start
//     of the claim's next decision instead of run (dropEndedPauseHold), the resume's event
//     (ResumePause) among them. A pause's request that reaches a claim whose suspension is already
//     held writes its own pause's id on a released hold — the idle release's, or an earlier
//     pause's — so that hold fires for the open pause; a held daemon's, operator's or work hold's
//     suspension keeps its own source and reason.
//   - It lives in memory only. A restart forgets it, so whoever holds the request asks again: the
//     outbox retries its suspend row until the claim answers nil. A restart starts the operator's
//     stop timeout over; a deadline the daemon names is a time, so it holds as named.
//   - It is the one thing that lets a row end in suspended without naming that state (Handle).
//
// One claim stored working is not held for here: the one no process stands behind, whose last
// report of the process it records found it gone and whose relaunch never ran (processGone,
// Claim.ProcessGone, which states the two windows that leave one). A hold protects a turn and a
// process, and that claim has neither — the turn the dead process was running ended with it, and
// there is nothing left to cut off — so its suspension is decided where it arrives, exactly as
// the suspension of a claim whose paced wake waits over no process is (holdSuspendUntilReady).
// Held instead, it would wait for a turn that will never end: an operator's stop timeout long,
// and for ever for a daemon's suspension that names no deadline, over a claim nothing will
// relaunch. That is the one path out of this row that ends in suspended by naming it, which is
// why the row names StateSuspended among its destinations (Handle, table.go).
//
// It answers ErrSuspendHeld, except to a suspension that may not be held
// (RequestSuspend.NeverHold), which it refuses before it records anything.
func holdSuspension(m *Machine, ctx context.Context, ev Event) error {
	request, err := suspendRequest(m, ev)
	if err != nil {
		return err
	}
	// Read before the refusal below and before anything is recorded, because this claim is the
	// one the recovery that posts a suspension it will not have held reaches (api's
	// suspendForFreshSession): refused here, the claim it is asked over would stand working over
	// its dead process with no door left. A claim recording no process at all is left to that
	// refusal: nothing was read of a process it does not have, and no process means no stop to
	// make. A launch waiting its paced turn owns the process it waits over (launchWaits), and a
	// compaction in flight is a claim whose session Oh My Pi is writing a summary of, which the
	// compaction's own end gives back (compactEnded): neither is this row's to end.
	if m.claim.Locator != nil && m.processGone && !m.launchWaits() && m.compacting == nil {
		m.log.Info("supervise: a suspension reached a claim stored working over a process the daemon found gone; deciding it here",
			"reason", request.Reason, "by", request.By, "incarnation", m.claim.Locator.Incarnation)
		return m.suspendOverADeadProcess(ctx, request)
	}
	// The flag and the deadline are the asker's own: suspendRequest hands a daemon's request under
	// the operator's hold the operator's source, which must not give it the operator's stop timeout
	// too, nor hold a request whose asker reports a refusal as nothing having happened.
	if ev.(RequestSuspend).NeverHold {
		return &RefusedError{State: m.claim.State, Request: "suspend",
			Reason: "the claim's agent is in a turn, and this suspension is not one that is held for the turn's end"}
	}
	deadline := ev.(RequestSuspend).Deadline
	if ev.(RequestSuspend).By == SuspendedByOperator {
		deadline = m.deps.Clock.Now().Add(m.deps.Timeouts.Stop)
	}
	switch {
	case m.held == nil:
		m.held, m.heldLaunch = &RequestSuspend{Claim: request.Claim, Reason: request.Reason, By: request.By, Pause: request.Pause}, false
		m.log.Info("supervise: the suspension is held for the agent's turn to end", "reason", request.Reason, "by", request.By)
	case request.By == SuspendedByOperator && m.held.By != SuspendedByOperator:
		m.log.Info("supervise: the operator's suspension takes the held suspension over", "reason", request.Reason, "held", m.held.Reason)
		m.held.By, m.held.Reason, m.held.Pause = request.By, request.Reason, 0
	case request.Pause != 0 && m.held.By == SuspendedByReleased:
		m.log.Info("supervise: the project's pause takes the held suspension over", "pause", request.Pause,
			"heldPause", m.held.Pause, "held", m.held.Reason)
		m.held.Pause, m.held.Reason = request.Pause, request.Reason
	case request.By == SuspendedByHold && m.held.By == SuspendedByReleased,
		request.By == SuspendedByDaemon && m.held.By == SuspendedByHold:
		m.log.Info("supervise: a stronger suspension takes the held suspension over", "reason", request.Reason, "by", request.By,
			"held", m.held.Reason, "heldBy", m.held.By, "heldPause", m.held.Pause)
		m.held.By, m.held.Reason, m.held.Pause = request.By, request.Reason, 0
	}
	// The held deadline only moves earlier, and the timer is re-armed only when that deadline comes
	// before the one already armed: a stop the runtime refused is retried at the probe interval
	// (suspendHeld), and a deadline later than that retry must not push it back.
	if !deadline.IsZero() && (m.held.Deadline.IsZero() || deadline.Before(m.held.Deadline)) {
		m.held.Deadline = deadline
		if a, ok := m.timers[TimerSuspend]; !ok || deadline.Before(a.due) {
			m.arm(TimerSuspend, max(0, deadline.Sub(m.deps.Clock.Now())), "")
			m.log.Info("supervise: the held suspension stops the agent at its deadline if its turn has not ended", "deadline", deadline, "by", m.held.By)
		}
	}
	return ErrSuspendHeld
}

// holdSuspendUntilReady retains a suspend that reaches a rule-resuming launch after the rule
// changed the claim to launching. The old table ignored it, returning a conflict to an operator and
// dropping a daemon's linger or leave row. The hold survives launch and is run as soon as the agent
// says ready (ready), before the claim is published ready or takes a task. It answers
// ErrSuspendHeld so the operator sees 202 and a daemon row retries across a restart, except to a
// suspension that may not be held (RequestSuspend.NeverHold), which it refuses before it records
// anything: a claim read idle can be launching again by the time the request reaches the lock.
//
// A claim whose launch this very suspension stopped is suspended here and now instead: the
// suspension cancelled that start before it took the lock (Handle), so nothing of this claim is
// running and no ready will ever arrive to run a hold. The caller is answered that the claim is
// suspended, which is what it asked for, rather than a hold nothing would ever end. A launch
// waiting to be tried again (relaunchLater), which this suspension did not stop, keeps the hold:
// its retry runs it.
//
// One claim here does not wait: the one whose paced wake is still waiting over no process at all,
// which the gate wrote launching and parked before it had launched anything (reserveWake,
// parkWake). A hold protects a turn and a process, and this claim has neither — the launch its
// wake holds has not been made — so the suspension has nothing to wait for, and the wait is dear.
// A wave's queue is hundreds of claims let out at a few wakes a minute, so a claim far down it
// waits tens of minutes, and for that long both halves of R2 of dispatch://INFRA-976/spec fail: the
// claim does not "still decide" the suspend, since an operator's drain, a park or a tree close
// lands only at the wake's turn (wakeNow), and it loses it as well, since the hold lives in memory
// alone (holdSuspension) and an operator's suspend writes no outbox row — a restart inside those
// minutes forgets the drain while the claim stands persisted launching, and the boot relaunches
// the very claim the operator told it to drain (the daemon's launchUnfinished).
//
// So that suspension is decided where it arrives: the claim is suspended with the request's own
// source and reason and written (suspendedFor), which is why this row names StateSuspended among
// its destinations (Handle, which holds every row to its own destinations because nothing is held
// here). The wake goes with the process the claim never had (letGo clears it and disarms
// TimerWake), and no token is handed back, since the limiter charges on the reserve: every wake
// behind it keeps the turn it already holds, exactly as a wake dropped for a request that does not
// wait leaves them (dropWakeFor, reparkWake).
//
// A claim whose process still runs is untouched by any of this. The gate left such a claim exactly
// as it stood (parkWake), so its suspend is decided by the row of the state it still holds — and a
// launch that did start a process reaches the hold here as it always did, since that process is
// what the hold is for.
//
// A project pause's stop of a claim whose relaunch waits parked over the process it still records
// — one that died and whose relaunch the pacing parked before letting it go (the gate sits before
// letGo, launchAfter) — is decided where it arrives too: the parked relaunch is what the pause
// holds, and the process it records is the one that relaunch was to replace, so the process is
// stopped (letProcessGoFor) and the claim suspended now, with the parked launch owed, as the
// pause's gate stops a process it held a launch over (pauseStopDue). Held until a ready instead,
// the stop would land only at the wake's turn, which over a wave is tens of minutes out, and a
// restart inside them would forget it and leave the claim to the boot (decision 4 of
// dispatch://INFRA-3759's plan: a parked wake with no process is stopped at once).
//
// Each of them meets a launch, which a project pause's stop owes the role at the resume
// (PauseStop.LaunchOwed, decision 6 of dispatch://INFRA-3759's plan): the start this suspension
// cancelled, the wake parked over no process or over the process it would replace, and the launch
// the hold waits on until ready (Machine.heldLaunch).
//
// A hold already standing is replaced by the newest request, as it always was, with two
// exceptions that keep a stronger hold's own words: a request of a weaker source leaves a held
// suspension of a stronger one its source and reason (suspensionRank) — a released request, the
// idle release's or a pause's, leaves a held daemon's or work hold's suspension as it is, and the
// work hold's leaves a held daemon's (an operator's is kept by suspendRequest before this reads
// it) — and the idle release's leaves a pause's hold its pause, so that hold still fires for the
// pause and is dropped once that pause ends (dropEndedPauseHold). The work hold's replaces a held
// release's, the pause's among them, and the daemon's and the operator's replace the hold's.
func holdSuspendUntilReady(m *Machine, ctx context.Context, ev Event) error {
	request, err := suspendRequest(m, ev)
	if err != nil {
		return err
	}
	if m.stoppedAStart() && m.launchRecordsNoProcess() {
		m.log.Info("supervise: the suspension stopped the claim's start, so it suspends the claim now", "reason", request.Reason, "by", request.By)
		return m.suspendedFor(ctx, request, true)
	}
	if ev.(RequestSuspend).NeverHold {
		return &RefusedError{State: m.claim.State, Request: "suspend",
			Reason: "the claim's launch has not registered, and this suspension is not one that is held for it"}
	}
	if m.wake != nil && m.claim.Locator == nil {
		m.log.Info("supervise: a suspension reached a claim whose paced wake waits over no process; deciding it here",
			"reason", request.Reason, "by", request.By, "kind", m.wake.kind, "due", m.wake.at)
		return m.suspendedFor(ctx, request, true)
	}
	if m.wake != nil && request.Pause != 0 {
		m.log.Info("supervise: a project pause's stop reached a claim whose paced relaunch waits over the process it would replace; stopping that process now",
			"reason", request.Reason, "pause", request.Pause, "kind", m.wake.kind, "due", m.wake.at, "incarnation", m.claim.Locator.Incarnation)
		return m.letProcessGoFor(ctx, request, true, stopOwed)
	}
	switch held := m.held; {
	case held != nil && suspensionRank(request.By) < suspensionRank(held.By):
		m.log.Info("supervise: the held suspension keeps its own source over a weaker one", "reason", request.Reason,
			"pause", request.Pause, "requestBy", request.By, "held", held.Reason, "by", held.By)
	case held != nil && request.By == SuspendedByReleased && request.Pause == 0 && held.Pause != 0:
		m.log.Info("supervise: the held suspension of the project's pause keeps its pause over the idle release's", "reason", request.Reason,
			"pause", held.Pause)
	default:
		m.held, m.heldLaunch = &request, true
		m.log.Info("supervise: the suspension waits for the rule-resuming agent to become ready", "reason", request.Reason, "by", request.By)
	}
	return ErrSuspendHeld
}

// suspendedAgain is a suspension of a claim already suspended, and the one place a suspension's
// source changes under it (Claim.SuspendedBy). A source takes the claim over only when it
// outranks the one that holds it (suspensionRank): an operator's suspend of a claim the daemon
// suspended, so the rule resumes it no more until the operator does; a daemon's or an operator's
// suspend of a claim whose idle pod was released, or that the work hold stopped, so the linger,
// park, close or hand-over that asked for it owns the claim and neither a wake nor the hold's end
// acts on it; and the work hold's stop of a released claim, a pause's stop among them, so no wake
// resumes it while the hold stands and it keeps its bundle and its pending task. Anything else
// changes nothing, and an operator's suspension is never taken over.
func suspendedAgain(m *Machine, ctx context.Context, ev Event) error {
	request, err := suspendRequest(m, ev)
	if err != nil {
		return err
	}
	if suspensionRank(request.By) <= suspensionRank(m.claim.SuspendedBy) {
		return nil
	}
	m.log.Info("supervise: a suspension takes the suspended claim over", "reason", request.Reason,
		"by", request.By, "suspendedBy", m.claim.SuspendedBy)
	m.claim.SuspendedBy = request.By
	return m.persist(ctx)
}

// suspensionRank orders the sources a suspended claim can be held by, weakest first: a released
// claim's suspension is the weakest, because the release ended no phase and every wake may
// resume it; the work hold's is stronger, since it holds a claim no wake may resume while its
// tree's standing design gate is closed; the daemon's is stronger again, since it is a linger, a
// park or a close, which ends the claim's phase whatever the gate says; the operator's is
// strongest and nothing takes it over. An exit's is the root's own and is left where it is, and an
// unknown one is the daemon's own that 0038 found unrecorded (SuspendedByUnknown), so both rank
// with the daemon's.
func suspensionRank(source SuspendSource) int {
	switch source {
	case SuspendedByReleased:
		return 1
	case SuspendedByHold:
		return 2
	case SuspendedByOperator:
		return 4
	default:
		return 3
	}
}

// suspendAtTimeout is the held suspension's timer: the turn did not end by the hold's deadline, so
// the process is stopped mid-turn; or an earlier stop failed, and it is tried again. A project
// pause's hold whose pause has ended by then is dropped at the start of the decision, and this
// timer with it (dropEndedPauseHold, dropHeld), so the fence drops the timer and the turn runs on.
//
// A stop forced mid-turn is not the turn's end: the confirmed task did not finish, so it goes back
// interrupted, as a death in a turn takes it back. The task is taken back before the stop, as died
// does: a take-back that fails stops nothing and is tried again at the probe interval, and a crash
// between the two leaves a running claim holding an interrupted task, never a suspended claim holding
// a confirmed one, which settle would retire as a finished turn. What happens to it then is the
// suspension's: the operator's resume re-sends it to the same owner, and a daemon's suspension retires
// a phase's task with the phase (settle), so the next start hands the session its phase's resume task;
// the workspace, the issue's branch and the session are kept either way. The stop records the turn
// it cuts off, and the task it took back from that turn, as ended by the suspension (suspendNow,
// Machine.cutTurnOf, recordEnd).
func suspendAtTimeout(m *Machine, ctx context.Context, _ Event) error {
	if m.held == nil {
		return nil
	}
	if m.claim.State == StateWorking {
		m.log.Warn("supervise: the agent's turn has not ended; suspending it mid-turn", "by", m.held.By, "deadline", m.held.Deadline)
		if err := m.interrupted(ctx, "the held suspension's deadline ended the task's turn", ordinaryEnd); err != nil {
			m.arm(TimerSuspend, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("take back the task of %s before its forced stop: %w", m.claim.Token, err)
		}
	} else {
		m.log.Info("supervise: trying the held suspension's stop again")
	}
	return m.suspendHeld(ctx)
}

// suspendRequest is the event a suspension's row is handed. The table routes only RequestSuspend
// there (kindOf's onSuspend); another event is a wiring error, and so is a suspension that names
// none of the operator, the daemon, the idle pod's release and the work hold (a root's exit
// suspends it through RequestExit, never this), since the source is what decides whether the
// daemon may resume the claim and what becomes of the phase's task. So is a project pause's id on
// any source but the release's, or an id that is no pause's (RequestSuspend.Pause), and a stop that
// waits for its message's turn with no deadline to end the wait (RequestSuspend.TurnAfter). Each
// is refused before anything is stopped, rather than a panic, a journal line with no reason, a
// claim suspended by nobody or a stop that is never made.
//
// A project pause's stop whose pause is no longer the open one is answered ErrPauseEnded before
// anything reads the held suspension, in every row, so it stops nothing and takes no hold over: the
// resume ended the pause after the stop was asked (pauseStopEnded). The one such stop decided
// here is the one that already cut its claim's start short while its pause still stood (Handle,
// stopStart): that start leaves the claim launching with nothing running, which only this stop's
// own row completes (holdSuspendUntilReady), and the stop transaction then owes the role its
// launch, since its pause has ended (projectPause.stopTx in internal/daemon).
func suspendRequest(m *Machine, ev Event) (RequestSuspend, error) {
	request, ok := ev.(RequestSuspend)
	if !ok {
		return RequestSuspend{}, fmt.Errorf("suspend %s: the suspend action was handed %T, not a RequestSuspend", m.claim.Token, ev)
	}
	switch request.By {
	case SuspendedByOperator, SuspendedByDaemon, SuspendedByReleased, SuspendedByHold:
	default:
		return RequestSuspend{}, fmt.Errorf("suspend %s: the suspension names source %q, not operator, daemon, released or hold", m.claim.Token, request.By)
	}
	if !request.TurnAfter.IsZero() && request.Deadline.IsZero() {
		return RequestSuspend{}, fmt.Errorf("suspend %s: the suspension waits for the turn of a message that reached the claim at %s and names no deadline",
			m.claim.Token, request.TurnAfter.UTC().Format(time.RFC3339))
	}
	if request.Pause < 0 {
		return RequestSuspend{}, fmt.Errorf("suspend %s: the suspension names pause %d, which is no pause's id", m.claim.Token, request.Pause)
	}
	if request.Pause != 0 && request.By != SuspendedByReleased {
		return RequestSuspend{}, fmt.Errorf("suspend %s: the suspension names pause %d with source %q; a pause's stop names its pause's id with source %q",
			m.claim.Token, request.Pause, request.By, SuspendedByReleased)
	}
	if m.pauseStopEnded(request) && !m.startCutShort() {
		return RequestSuspend{}, fmt.Errorf("suspend %s for pause %d: %w", m.claim.Token, request.Pause, ErrPauseEnded)
	}
	// An operator's held suspension wins everywhere — launching, registered, ready while its
	// stop retries, and working — until it completes. A later daemon linger/leave request must
	// use that same source rather than changing a deliberate operator hold into a rule-resumable
	// daemon suspension.
	if m.held != nil && m.held.By == SuspendedByOperator && request.By != SuspendedByOperator {
		return *m.held, nil
	}
	return request, nil
}

// suspendNow stops the claim's process for request and keeps its session, recording what the
// stop ended as a suspension by request's source (endSuspended, letGo). It is the stop of the
// suspend row and of suspendHeld, not of every suspension of a live process: a held suspension
// that a replacement or a wake's turn ends stops through endHeld and letProcessGoFor, and a
// root's exit stops through exit. It sends the clean stop of the process's Envoy session once the
// runtime's stop has returned, and none for a stop the runtime refused (stopForSuspension).
// launch is whether the stop meets a launch its caller knows of (suspendedFor); a launch still
// waiting on the machine is read here, before the stop lets it go (launchMet).
func (m *Machine) suspendNow(ctx context.Context, request RequestSuspend, launch bool) error {
	launch = launch || m.launchMet()
	if err := m.stopForSuspension(ctx, request.By, stopOwed); err != nil {
		return fmt.Errorf("suspend %s: %w", m.claim.Token, err)
	}
	return m.suspendedFor(ctx, request, launch)
}

// stopForSuspension asks the runtime to stop the claim's process for a suspension by by, and
// once that stop has returned sends the clean stop of the process's Envoy session (sessionGone),
// unless cleanStopSent says this decision sent it already. A stop the runtime refused sends
// nothing, since the process may still run, and its error is returned for the caller to decide
// on: suspendNow refuses the suspension, while letProcessGoFor and exit suspend the claim all the
// same. It is the one place a suspension's stop and its clean stop are written together.
func (m *Machine) stopForSuspension(ctx context.Context, by SuspendSource, cleanStopSent bool) error {
	if err := m.endProcess(ctx, endSuspended(by)); err != nil {
		return err
	}
	if !cleanStopSent {
		m.sessionGone(ctx)
	}
	return nil
}

// suspendedFor moves the claim to suspended once request's stop has ended its process, and says
// why. A suspension a project pause asked for (RequestSuspend.Pause) ends in the pause's stop
// transaction rather than the plain write (suspended, persistStop), carrying whether the role is
// owed a launch at the resume: launch is whether the stop met one its caller knows of — a start
// it cancelled, a launch held until ready, a wake's turn, a waiting relaunch — and a launch still
// waiting on the machine is read here too (launchMet); the task the claim holds is read where the
// stop is written (putStop).
func (m *Machine) suspendedFor(ctx context.Context, request RequestSuspend, launch bool) error {
	var stop *PauseStop
	if request.Pause != 0 {
		stop = &PauseStop{Pause: request.Pause, LaunchOwed: launch || m.launchMet()}
	}
	if err := m.suspended(ctx, request.By, stop); err != nil {
		return err
	}
	m.log.Info("supervise: suspended", "reason", request.Reason, "by", request.By)
	return nil
}

// suspended moves the claim to suspended by by: its session kept, and the process it stopped let
// go for the resume to wait out. It only persists — revoking the stopped agent's capability, in
// memory even when the write fails. The finished phase's unconfirmed task is retired after it by
// settle, which Handle runs after every row; a retirement that fails is reported, and the claim's
// next decision retires it before anything else. A project pause's stop is written through its
// stop transaction (persistStop); nil is a suspension of no pause, written as it always was.
//
// A process still recorded here is one whose stop the runtime refused and the suspension let go
// all the same (endHeld, exit): what it ended is recorded as a suspension by by, as the stop that
// did land records it (endSuspended, letGo), and the record goes to the store with this write.
func (m *Machine) suspended(ctx context.Context, by SuspendSource, stop *PauseStop) error {
	m.letGo(ctx, endSuspended(by))
	m.claim.State, m.claim.SuspendedBy = StateSuspended, by
	if stop != nil {
		return m.persistStop(ctx, *stop)
	}
	return m.persist(ctx)
}

// suspendHeld runs the held suspension's stop. One the runtime refuses stays held and is tried
// again at the probe interval, as the registration deadline retries its own: the operator's
// suspend, answered 202, has nothing else to retry it. A stop that landed has let the process go
// and the hold with it (letGo), whether or not its write then failed; nothing is retried then, and
// m.held is nil — which is how a caller tells the two apart.
func (m *Machine) suspendHeld(ctx context.Context) error {
	request := m.held
	if request == nil {
		return fmt.Errorf("supervise: no suspension of %s is held", m.claim.Token)
	}
	if err := m.suspendNow(ctx, *request, m.heldLaunch); err != nil {
		if m.held != nil {
			m.arm(TimerSuspend, m.deps.Timeouts.Probe, "")
		}
		return err
	}
	return nil
}

// suspendHeldAtEnd is suspendHeld at the end of the turn, or of the compaction, the claim is
// still stored working for (turnEnded, compactEnded): nothing runs any more, so the stop cuts
// off no turn, and the let-go it makes reads the claim in back, the state that end gives the
// claim back in, for its record (Machine.heldStopBack, endState, recordEnd). Read as the working it
// is still stored in, the record would tell the next process that the turn it was in was
// interrupted, of a turn that had ended or a compaction that ran none. The claim's own state is
// left alone, as is everything else the stop decides: the end sets it once the hold's outcome is
// known, and the mark goes when the stop returns, landed or refused.
func (m *Machine) suspendHeldAtEnd(ctx context.Context, back ClaimState) error {
	m.heldStopBack = back
	defer func() { m.heldStopBack = "" }()
	return m.suspendHeld(ctx)
}

// dropHeld ends the held suspension without running it.
func (m *Machine) dropHeld() {
	m.held, m.heldLaunch = nil, false
	m.disarm(TimerSuspend)
}

// endHeld is the claim's process found dead, or alive holding an address a process launched now is
// not handed (repoint), while a suspension is held. Nothing is left to hold the suspension for, so
// the claim is suspended rather than relaunched, and nothing is charged: died asks before it charges
// anything. The stop and the suspension are the two steps every claim with nothing left to run
// takes, written once (letProcessGoFor). cleanStopSent is whether the decision that reached here
// has already sent the clean stop of the process's Envoy session (sessionGone): a death a probe
// judged has (died, stopSent), and a replacement (replaceNow) and a paced launch's turn (wakeNow)
// have not (stopOwed). replaceNow is reached by displaced too — a process found gone, or another
// runtime's process — and stopOwed is right there as well: a displaced process may still run
// under another runtime, so its clean stop waits for the runtime's stop, and a stop the runtime
// refused sends nothing. launch is whether the caller ends the hold in a launch's place — a wake's
// turn (wakeNow) — which a project pause's stop owes the role (suspendedFor).
//
// What the stop ends is recorded as a suspension by the held request's source (endSuspended):
// the suspension is what ends the process, rather than the relaunch it replaces. A death or a
// displacement that reached here recorded its own end first (died, displaced), and that record
// stands.
func (m *Machine) endHeld(ctx context.Context, launch, cleanStopSent bool) error {
	request, launched := *m.held, m.heldLaunch
	m.dropHeld()
	return m.letProcessGoFor(ctx, request, launch || launched, cleanStopSent)
}

// stopSent and stopOwed are the cleanStopSent of endHeld, letProcessGoFor and stopForSuspension:
// the decision letting the process go has already sent the clean stop of its Envoy session, or it
// still owes that stop once the runtime's stop has returned (sessionGone).
const (
	stopSent = true
	stopOwed = false
)

// suspendOverADeadProcess runs the suspension of a claim stored working that no process stands
// behind: the daemon's last report of the process it records found it gone, and the relaunch that
// would have replaced it never ran (holdSuspension, which states when this path is taken). It is
// what a death with a suspension already held does (died, endHeld), for a suspension that
// arrives after the death rather than before it.
//
// The task of the dead turn goes back to waiting first, unconfirmed and under a new id, exactly
// as a death in a turn takes it back (interrupted): the turn ran, the agent read the task, and
// no process finished it. It carries the note of a pod that ran out of memory where the report
// that found the process gone said so (goneOutOfMemory), and the claim keeps that report's
// account of the death for the operator's listing and a failed claim's notices (goneCause,
// Claim.LastDeathCause), as died keeps the death it judges. A take-back the store will not write
// stops everything — nothing is stopped, nothing is sent and the claim is left as it stands, its
// last death's cause put back, as died leaves it — so the caller's next request takes it back
// then, rather than this leaving a suspended claim holding a task settle would retire as a
// finished turn.
//
// The clean stop of the dead process's Envoy session is sent once that write has landed, and
// before the process is let go (sessionGone), so it goes whether or not the runtime's stop of a
// process already dead lands; letProcessGoFor is told it went (stopSent). Sent before the write, a
// write the store refused would leave the stop sent for a claim this suspension then left standing
// as it was. The report that found the process gone may be no death died judged: where the
// recovery's own read is the first to find it (ReadProcess) — after a restart, or before any sweep
// has reported it — this is the one stop that death gets, and without it the listener would keep
// the dead session's role and issue claims while the fresh session forgets that session's id.
// Where died did judge the death, the stop goes again for a registration already ended, which
// tells the listener nothing new.
//
// What the dead process ended is recorded as its death once the take-back has landed (recordEnd,
// endDied, under the same goneCause): the take-back's own mark is what tells the record that the
// task was taken from the dead turn (Machine.cutTurnOf), and the stop's let-go that follows finds
// this launch's record standing and records no suspension in its place. So the task is sent again
// saying the process died, which is what the sentence in front of it reads from that record
// (interruptedTask, relaunchSentence), rather than saying a suspension ended it. Where died judged
// the death, its record stands and this adds none. A take-back that fails records nothing, as it
// changes nothing else.
//
// Nothing is charged, as endHeld charges nothing. Where died judged the death it was charged
// there, or found to have nothing to charge (Claim.DeathCharged). Where the read is the first to
// find it no judgement ran, and that death is charged nothing at all: the suspension is an
// operator's or a controller's explicit act of recovery, and charging it would walk the claim
// toward the deaths limit for a death the daemon is being asked to recover from. A suspension is
// no launch failure of its own either.
func (m *Machine) suspendOverADeadProcess(ctx context.Context, request RequestSuspend) error {
	cause := m.claim.LastDeathCause
	m.claim.LastDeathCause = m.goneCause
	if err := m.interrupted(ctx, "the process the task's turn ran on is gone", m.goneOutOfMemory); err != nil {
		m.claim.LastDeathCause = cause
		return err
	}
	m.recordEnd(ctx, endDied(m.goneCause))
	m.sessionGone(ctx)
	return m.letProcessGoFor(ctx, request, false, stopSent)
}

// letProcessGoFor asks the runtime to let the claim's process go and suspends the claim for
// request, whether or not that stop landed. It is written once because two paths take exactly
// these two steps: a process found dead, at a stale address or otherwise replaced while a
// suspension is held (endHeld), and a suspension that arrives over a process the daemon has
// already found gone (suspendOverADeadProcess). A runtime that cannot stop the process is logged
// and recorded (stopUnconfirmed) and the claim is suspended all the same, as a failed claim is
// (fail): the claim runs nothing either way, and a process the runtime would not stop goes on
// holding what it runs on, which is the owner of a stalled worker's to hear (the daemon's
// stallBackstopRecord).
//
// Between the two it sends the clean stop of the process's Envoy session once the runtime's stop
// has returned, unless cleanStopSent says this decision sent it already (stopForSuspension).
// launch is suspendNow's: a launch the caller knows the stop meets, beside the one waiting on the
// machine, read before the stop lets it go (launchMet).
func (m *Machine) letProcessGoFor(ctx context.Context, request RequestSuspend, launch, cleanStopSent bool) error {
	launch = launch || m.launchMet()
	incarnation := m.claim.Locator.Incarnation
	if err := m.stopForSuspension(ctx, request.By, cleanStopSent); err != nil {
		m.stopUnconfirmed = true
		m.log.Error("supervise: could not stop the process with the runtime; suspending its claim anyway",
			"incarnation", incarnation, "error", err)
	}
	return m.suspendedFor(ctx, request, launch)
}
