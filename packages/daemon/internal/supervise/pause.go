package supervise

// The project's pause (dispatch://INFRA-3759): what a machine does while the daemon's project is
// paused, as the plan's decisions say it. Four things, each written once, here:
//
//   - The gate (decision 7): a launch of a claim that is not the controller's runs nothing while
//     the project is paused (reserveWake, wakeNow, pauseGate). It is held on the machine with no
//     timer and no token, and the machine stops the claim for the pause in its next decision
//     (TimerPause, pauseStopDue), with a launch owed.
//   - The task gate (decision 8): no task is sent to such a claim while the project is paused
//     (sendPending, pauseHolds).
//   - The stop transaction (decision 6): a suspension a pause asked for (RequestSuspend.Pause)
//     ends in a claim write through Deps.PauseStopped, which writes the pause's records with the
//     claim (persistStop). One that fails is owed on the owed mark (owedClaimWrite.stops), and
//     every write of the claim goes through it until one lands (putStops).
//   - The holds (decision 5): a pause's held suspension survives a start while its pause is open
//     (StartedBy), a later pause's request takes it over (holdSuspension), and once its pause has
//     ended it is dropped at the start of the claim's next decision, before anything of that
//     decision reads it (dropEndedPauseHold); the resume's event (ResumePause) is such a decision.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// PauseRoleStoppedMessage is the line the machine writes once a project pause's stop transaction
// has landed (decision 23): the claim, which the machine's logger carries, the pause (`pause`) and
// whether the role is owed a launch at the resume (`launch_owed`). It is written once per stop,
// by whichever claim write landed it (persistStop, putStops), and pinned because the daemon's
// tests and the production check read it whole.
const PauseRoleStoppedMessage = "pause: a role stopped"

// PauseStop is what a project pause's stop transaction is handed beside the claim
// (Deps.PauseStopped).
type PauseStop struct {
	// Pause is the id of the pause (store.Pause.ID) the suspension stopped the claim for, which
	// may have ended by the time the transaction runs.
	Pause int64
	// LaunchOwed is whether the role is owed a launch at the resume (decision 6): it holds a task
	// no turn has taken (holdsTask), or the stop met a launch — a wake parked in the pacing, a
	// start the stop cancelled, a launch held until its agent is ready, a relaunch waiting its
	// retry, or a launch the pause's gate held (suspendedFor, launchMet). The stop transaction
	// owes one besides to every stopped role whose stop commits after its pause has ended with no
	// later pause open: the role was live when the resume ended the pause, so the resume's promise
	// is that it runs again (projectPause.stopTx in internal/daemon).
	LaunchOwed bool
}

// pauseHolds is the open pause that holds this claim's launches and tasks, and whether one does:
// the daemon's project is paused (Deps.Paused) and the claim is not the controller's
// (claim.IsController), which is the one role a pause never holds. A claim of another project's
// issue, which the operator's spawn makes under the daemon's own project token, is a session this
// daemon runs, so the pause holds it as it holds every other claim.
func (m *Machine) pauseHolds() (int64, bool) {
	if m.deps.Paused == nil || claim.IsController(m.claim.Role, m.claim.Issue, m.claim.Tree) {
		return 0, false
	}
	pause, paused := m.deps.Paused()
	return pause, paused && pause > 0
}

// pauseOpen is whether pause is the project's open pause now (Deps.Paused). A daemon whose project
// is never paused has no open pause.
func (m *Machine) pauseOpen(pause int64) bool {
	if m.deps.Paused == nil {
		return false
	}
	open, paused := m.deps.Paused()
	return paused && open == pause
}

// pauseStopEnded is whether request is a project pause's stop (RequestSuspend.Pause) whose pause is
// not the open one now (pauseOpen): the resume ended it after the stop was asked — between the
// `pause_stop` executor's read of the pause and this decision, say — or a later pause followed
// it, which asks a stop of its own. Such a stop stops nothing (suspendRequest, ErrPauseEnded), and
// Handle cuts no start short for it. The gate's own stop names the pause it read open a moment
// before, in the same decision, and is posted again where a resume came between (pauseStopDue).
func (m *Machine) pauseStopEnded(request RequestSuspend) bool {
	return request.Pause != 0 && !m.pauseOpen(request.Pause)
}

// startCutShort is whether the suspension being decided cut its claim's start short (Handle,
// stopStart) and left it launching with nothing running, read without taking it, since that
// suspension's own row takes it (holdSuspendUntilReady, stoppedAStart): a pause's stop that did
// so while its pause still stood is decided as asked even if the pause ended meanwhile, since
// nothing else would end that launch (suspendRequest).
func (m *Machine) startCutShort() bool {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	return m.startStopped && m.launchRecordsNoProcess()
}

// dropEndedPauseHold drops a project pause's held suspension whose pause is no longer the open one
// (decision 5), and is the one place such a hold goes. It runs at the start of every decision,
// once the lock is taken and before settle and the fence (Handle), and at the start of a start
// run against the claim (StartedBy), the one decision that does not go through Handle. So every
// row, timer, event and send of a decision reads a held suspension only while it still stands: a
// compaction is not refused for an ended pause's hold, a task is not held back behind one, and
// where a held suspension fires — the end of a turn or of a compaction, the agent's ready, the
// hold's own timer, a prompt retirement, a death, a stale address, a stall, a wake's turn and a
// waiting relaunch — a role in a turn runs on and a launch held until ready goes ahead. The
// resume's event (ResumePause) is one such decision, and a resume whose event never reaches this
// machine — one another daemon made — is met by the claim's next decision. Its pause has ended
// when it is not the open pause, a later pause's included: a later pause's own request writes its
// id on the hold (holdSuspension), so a hold that still names an earlier one was taken over by no
// pause.
func (m *Machine) dropEndedPauseHold() {
	if m.held == nil || m.held.Pause == 0 || m.pauseOpen(m.held.Pause) {
		return
	}
	m.log.Info("supervise: the held suspension's pause has ended; it is dropped rather than run",
		"pause", m.held.Pause, "reason", m.held.Reason)
	m.dropHeld()
}

// launchMet is whether a launch of the claim waits on the machine now: a wake parked in the pacing,
// held by the pause's gate or waiting at its issue pod's gate (Machine.wake), or a relaunch waiting
// its retry (Machine.relaunch). A pause's stop that meets one owes the role that launch
// (PauseStop.LaunchOwed), and it is read before the stop lets the process go, which drops both
// (letGo).
func (m *Machine) launchMet() bool { return m.wake != nil || m.relaunch != nil }

// holdsTask is whether the claim holds a task no turn has taken: its pending delivery, while it
// is unconfirmed. A confirmed one is the task of a turn that ran, which settle retires once the
// claim is no longer working, so it owes the role no launch.
func (m *Machine) holdsTask() bool {
	p := m.claim.Pending
	return p != nil && p.ConfirmedAt.IsZero()
}

// pauseGate is the gate's hold of a launch while the project is paused (decision 7), reached from
// the wake gate before its exempt return (reserveWake), from a parked wake's turn (wakeNow), from a
// dropped wake taken up again (reparkWake), and from the grant of a start parked at its issue pod's
// gate (podStartDue). The launch runs nothing, takes no token from
// the pacing and arms no retry: it waits on the machine as a parked wake does (waitOnMachine),
// with no timer and no moment (wake.pause). A claim that records no process is written launching
// and persisted, so a restart finds a launch to relaunch, which meets the gate again
// (relaunchUnfinished in internal/daemon); one whose process runs keeps its state, since the
// launch held is to replace that process.
//
// The decision ends in a state its own row names (Handle). A suspension made inside it would not:
// no row that launches names suspended, and the hold that would excuse it is read before the row
// acts. So the claim's stop is posted for its next decision (TimerPause, armed at once), as the
// machine posts itself every event it owes itself, and is made there (pauseStopDue).
func (m *Machine) pauseGate(ctx context.Context, waiting *wake, pause int64) error {
	waiting.pause = pause
	m.disarm(TimerWake)
	m.arm(TimerPause, 0, "")
	m.log.Info("supervise: the project is paused; its gate holds this claim's launch, and the claim is stopped for the pause",
		"pause", pause, "kind", waiting.kind, "state", m.claim.State)
	return m.waitOnMachine(ctx, waiting)
}

// pauseStopDue is the gate's own stop (TimerPause): the machine posting itself the suspension of
// the claim whose launch the pause's gate held (decision 7) — By released, the pause's id, and a
// deadline of now. It meets the claim as the pause's own stop meets a parked wake: a claim that
// records no process is stopped at once (holdSuspendUntilReady), and so is one whose process is
// registered, ready or idle (suspend) or still booting, since the launch the gate holds was to
// replace that process and a hold until ready would protect a process nothing wants; a claim in a
// turn — the stall backstop's case — is held to the deadline of now and stopped mid-turn, its
// task taken back interrupted (holdSuspension, suspendAtTimeout). Each ends in the pause's stop
// transaction with a launch owed, since the held launch waits on the machine until the stop
// lets it go (suspendedFor, launchMet). A stop that fails with the launch still held is posted
// again at the probe interval; one that stopped the process and whose write failed owes the
// write instead (persistStop).
//
// The pause it stops for is the open one when it runs. A launch held for a pause that has ended
// by then, with no pause open, goes to the wake pacing rather than stopping the claim
// (letOutHeldLaunch); one held for a pause that a later pause has followed is stopped for the
// later one. The flag is read again where the stop is decided (suspendRequest), so a resume that
// clears it between the two reads leaves a stop that stops nothing (ErrPauseEnded): the stop is
// posted again at once, and that decision lets the launch out, unless the resume's event has
// already (resumePause).
func pauseStopDue(m *Machine, ctx context.Context, _ Event) error {
	waiting := m.wake
	if waiting == nil || waiting.pause == 0 {
		return nil
	}
	pause, holds := m.pauseHolds()
	if !holds {
		m.log.Info("supervise: the pause the gate held this claim's launch for has ended; the launch goes to the wake pacing",
			"pause", waiting.pause, "kind", waiting.kind)
		return m.letOutHeldLaunch(ctx)
	}
	waiting.pause = pause
	request := RequestSuspend{Claim: m.claim.Token, By: SuspendedByReleased, Pause: pause, Deadline: m.deps.Clock.Now(),
		Reason: fmt.Sprintf("the project is paused (pause %d) and its gate held this claim's launch", pause)}
	var err error
	switch {
	case m.claim.State == StateWorking:
		err = holdSuspension(m, ctx, request)
	case m.claim.Locator == nil:
		err = holdSuspendUntilReady(m, ctx, request)
	default:
		err = suspend(m, ctx, request)
	}
	if err == nil || errors.Is(err, ErrSuspendHeld) {
		return nil
	}
	if errors.Is(err, ErrPauseEnded) {
		m.arm(TimerPause, 0, "")
		return nil
	}
	if m.wake == waiting {
		m.arm(TimerPause, m.deps.Timeouts.Probe, "")
	}
	return err
}

// letOutHeldLaunch takes the launch the pause's gate holds into the wake pacing once the pause it
// was held for has ended (decision 7): a reservation from the limiter, told to the daemon as the
// moment the pacing lets it out (wakeLetOut, decision 14), then its timer, exactly as the wake gate
// parks a launch (reserveWake, parkWake). A launch the reservation lets out now runs now, and a
// daemon whose pacing is off runs it now, unpaced. So does a launch that passed the wake gate
// before the pause's gate held it (wake.ran): it took its token there. Its callers are the gate's
// own stop finding no pause open (pauseStopDue) and the resume's event (resumePause).
func (m *Machine) letOutHeldLaunch(ctx context.Context) error {
	waiting := m.wake
	waiting.pause = 0
	m.disarm(TimerPause)
	now := m.deps.Clock.Now()
	reserve := m.deps.WakePace.Reserve
	if reserve == nil || waiting.ran {
		m.wake = nil
		waiting.at, waiting.reserved = now, now
		return m.runWake(ctx, waiting, false)
	}
	at := reserve(now)
	m.wakeLetOut(at)
	waiting.at, waiting.reserved = at, now
	if !at.After(now) {
		m.wake = nil
		return m.runWake(ctx, waiting, true)
	}
	return m.parkWake(ctx, waiting, at.Sub(now))
}

// resumePause is the resume's event (ResumePause): what this claim holds of the pause that ended
// goes, and nothing of any other pause. A suspension held for that pause has gone already, at the
// start of this decision (dropEndedPauseHold), so a role in a turn runs on and a launch held until
// ready goes ahead; a launch the gate held for it goes to the wake pacing, or is stopped for a
// later pause that holds the project by now (pauseStopDue); and the task the claim holds is sent
// where it may now go, which is the daemon's step of handing each role its held task at the
// resume (decision 22).
func resumePause(m *Machine, ctx context.Context, ev Event) error {
	pause := ev.(ResumePause).Pause
	if pause <= 0 {
		return fmt.Errorf("supervise: the resume reaching %s names pause %d, which is no pause's id", m.claim.Token, pause)
	}
	if w := m.wake; w != nil && w.pause == pause {
		if _, holds := m.pauseHolds(); holds {
			m.arm(TimerPause, 0, "")
		} else if err := m.letOutHeldLaunch(ctx); err != nil {
			return err
		}
	}
	return m.sendPending(ctx)
}

// persistStop is persist for the claim write a project pause's suspension ends in (suspended,
// decision 6): the claim goes through the pause's stop transaction (Deps.PauseStopped) with the
// pause's records, rather than through the plain write. A failure the rule owes (writeOwed)
// leaves the stop on the owed mark (owedClaimWrite.stops), for the repair and every write that
// settles the mark to make (putStops). Where a write is already owed, the stop joins it and the
// write is made as an owed one, the reports and any earlier stop with it. A daemon that hands no
// stop transaction writes the claim through the plain write.
func (m *Machine) persistStop(ctx context.Context, stop PauseStop) error {
	if m.deps.PauseStopped == nil {
		return m.persist(ctx)
	}
	if m.owedWrite != nil {
		m.owedWrite.stops = oweStop(m.owedWrite.stops, stop)
		return m.persist(ctx)
	}
	// Published before the write, so View shows the stopped claim while the stop's transaction
	// runs.
	m.publish()
	err := m.owes("PauseStopped", m.putStop(ctx, m.stored(), stop))
	if m.owedWrite != nil {
		m.owedWrite.stops = []PauseStop{stop}
	}
	return err
}

// oweStop adds stop to the stops an owed write carries, one per pause: a stop of a pause already
// owed keeps its place, and owes a launch where either did.
func oweStop(stops []PauseStop, stop PauseStop) []PauseStop {
	for i := range stops {
		if stops[i].Pause == stop.Pause {
			stops[i].LaunchOwed = stops[i].LaunchOwed || stop.LaunchOwed
			return stops
		}
	}
	return append(stops, stop)
}

// owedStops is the pause stops the machine's owed write carries, nil for none.
func (m *Machine) owedStops() []PauseStop {
	if m.owedWrite == nil {
		return nil
	}
	return m.owedWrite.stops
}

// putStop writes c through stop's transaction (Deps.PauseStopped) under the claim-write deadline,
// and says so once it has landed (PauseRoleStoppedMessage). The launch it owes is decided here,
// at the write, beside what the stop met: a claim that holds a task no turn has taken is owed the
// launch that delivers it (holdsTask), which a later write of an owed stop reads afresh.
func (m *Machine) putStop(ctx context.Context, c Claim, stop PauseStop) error {
	stop.LaunchOwed = stop.LaunchOwed || m.holdsTask()
	if err := m.write(ctx, func(ctx context.Context) error { return m.deps.PauseStopped(ctx, c, stop) }); err != nil {
		return err
	}
	m.log.Info(PauseRoleStoppedMessage, "pause", stop.Pause, "launch_owed", stop.LaunchOwed)
	return nil
}

// putStops is the claim write the machine makes while it owes pause stops: c through the
// transaction of each, oldest first, each off the mark as it lands, and the mark settled once the
// last has, since a claim write landed and it is the repair (settleOwedWrite). A failure leaves
// the stops not yet written owed and marks nothing more: its caller owes it or not, as for any
// claim write (persist, repair).
func (m *Machine) putStops(ctx context.Context, c Claim) error {
	for len(m.owedWrite.stops) > 0 {
		if err := m.putStop(ctx, c, m.owedWrite.stops[0]); err != nil {
			return err
		}
		m.owedWrite.stops = m.owedWrite.stops[1:]
	}
	m.settleOwedWrite("PauseStopped")
	return nil
}

// stopWaitsForResult is whether the end of the claim's turn leaves a project pause's held
// suspension waiting for the result of the turn's prompt (turnEnded): the hold is a pause's, and
// Oh My Pi has not said yet how that prompt's work ended (resultPending). The result is what
// decides the task — a turn that finished it retires it, and a provider's refusal of it takes it
// back for another prompt (providerErrored) — so a stop made at the turn's end would retire a
// refused task and leave the role owed nothing at the resume (check k1 of dispatch://INFRA-3759: a
// role whose turn a provider refuses keeps its task). Oh My Pi writes the result right behind the
// turn's end, and a process that dies before it does ends the hold through its death (died).
func (m *Machine) stopWaitsForResult() bool {
	return m.held != nil && m.held.Pause != 0 && m.resultPending()
}

// pauseHoldAtResult is whether a project pause's held suspension waited for the result that has
// just come in (stopWaitsForResult): the turn it was held for is over, and the hold still stands,
// so its pause is still the open one (dropEndedPauseHold dropped it at the start of this decision
// had that pause ended).
func (m *Machine) pauseHoldAtResult() bool {
	return m.claim.State == StateIdle && m.held != nil && m.held.Pause != 0
}

// stopAtResult runs the project pause's held suspension that waited for this result
// (pauseHoldAtResult), once the result has decided the task, and does nothing otherwise.
func (m *Machine) stopAtResult(ctx context.Context) error {
	if !m.pauseHoldAtResult() {
		return nil
	}
	return m.suspendHeld(ctx)
}

// wakeLetOut tells the daemon the moment the wake pacing lets a launch of this claim out
// (Deps.WakeLetOut, decision 14), at each reservation the machine takes for one: the wake gate's
// (reserveWake) and the let-out of a launch the pause's gate held (letOutHeldLaunch).
func (m *Machine) wakeLetOut(at time.Time) {
	if m.deps.WakeLetOut != nil {
		m.deps.WakeLetOut(m.claim.Token, m.claim.Issue, m.claim.Role, at)
	}
}
