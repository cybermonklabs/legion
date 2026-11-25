package supervise

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// repoint is the claim's process found alive holding an address a process launched now is not
// handed (runtime.StaleAddress): one of the addresses the runtime hands every new process from the
// daemon's configuration (the worker stream it dials, the daemon's API, NATS, Envoy, Dispatch, the
// secrets broker) moved since this one launched, as each does when the daemon restarts with its key
// changed; the worker stream's keys are `advertise_host`, `bind` when no `advertise_host` is set,
// and `worker_stream_port`. The process keeps what it holds for as long as it runs, and what it
// holds may never reach the daemon or the service again, so there is nothing to wait out but the
// report of a turn it has already run (below). A task whose turn the process was running goes back
// to waiting first (interrupted), the same recovery a death in a turn gets: that turn used the
// addresses the process holds, and the relaunch ends it. A held suspension that waits for no report
// ends here as it does for a death (endHeld): the claim is suspended, not relaunched. Otherwise the
// claim is relaunched at once through the launch path a death uses
// (launch): a Resume of its recorded session, or a Spawn over the claim's existing Sandbox when it
// has not registered yet, onto a process handed the current addresses. The stale observation is
// never charged, unlike a death (chargeDeath, relaunchAfterFailure): the process did nothing wrong.
// A relaunch the runtime refuses is charged as any launch failure is. The process runs on, so a
// compaction of its session runs on with it: the end of one gives the claim back as it would have
// without the repoint, and the task that end owes waits for the relaunch's turn rather than going
// to a process that turn replaces (ownsCompaction, claimHold). A compaction asked for after the
// relaunch is parked is refused before anything is sent (compactStart). The launch lets the
// process go as one whose address changed (endRepointed), and records what it ended there:
// in this decision, or at the turn of the wake the pacing parks it on, since the process runs
// until then (launchNow, letGo).
//
// A repoint that reaches the claim after its take's turn ended and before that turn's report
// reached the daemon is held for the report (holdReplacement; rule two of T3 of
// dispatch://INFRA-2811's plan): the process ran the turn whole and is alive, and its report
// travels over HTTP apart from the worker stream, so it can still land. Run at once, the
// relaunch would take the task back as a cut turn (interrupted), the report would then finish
// nothing, and the relaunched process would run the task again. So the relaunch runs at the
// task's finish, or at the wait's bound once the task is taken back with the note of a cut turn:
// the report wait's own bound over a whole view (TimerReport, as when the stale address is
// another service's), the reconnect deadline over a view not whole (as when it is the worker
// stream's, which the shim cannot dial: replacementAtDeadline), where the relaunch is still this
// one, uncharged. The hold is the report wait's in every phase work joins it (reportHolds), a
// held suspension that waits for the report among them: the repoint waits with that suspension
// rather than take the task back first. It begins only once the take's turn is over: a repoint
// during a running turn cuts that turn off as before, so a hung turn is still cut. And it is
// never made where the observation says the daemon's own API moved
// (runtime.Observation.DaemonAPIMoved): no report of that process can reach the daemon, so there
// is nothing to wait for, there or at the turn of a wake the pacing parks this relaunch on.
func (m *Machine) repoint(ctx context.Context, observation runtime.Observation) error {
	// The process is read through the guard backstopStalledTurn's comment gives the reason for:
	// judge drops an observation while the claim records none, so no live path arrives here
	// without one, and a panic in the supervisor would take every tree of the deployment with it
	// (noProcessRecorded).
	incarnation := noProcessRecorded
	if loc := m.claim.Locator; loc != nil {
		incarnation = loc.Incarnation
	}
	held := !observation.DaemonAPIMoved
	if held && m.boundedReportHolds() {
		// A replacement held already keeps what it holds (holdReplacement).
		m.holdReplacement("repoint", m.replacing(repointInterruption, ordinaryEnd, endRepointed),
			"incarnation", incarnation, "detail", observation.Detail)
		return nil
	}
	m.log.Warn("supervise: the process is alive at a stale address; replacing it with one at the current address",
		"incarnation", incarnation, "detail", observation.Detail, "daemonApiMoved", observation.DaemonAPIMoved)
	_, err := m.replaceProcess(ctx, repointInterruption, ordinaryEnd, endRepointed, held)
	return err
}

// repointInterruption is the sentence a repoint's take-back of the task whose turn its process
// was running is logged with (interrupted).
const repointInterruption = "the process at a stale address is replaced mid-turn"

// holdReplacement leaves run, a replacement of the claim's live process raised anew from the top,
// waiting for the report of the take's turn (Machine.heldReplacement, reportwait.go): the
// stale-address relaunch (repoint), or a replacement the wake pacing parked whose turn came
// (runWake). It runs where that wait ends, reading the claim as it stands then rather than as it
// stood here: at the report's finish, or at the wait's bound once the task is taken back with the
// note of a cut turn (afterTurn), or at the reconnect deadline over a view not whole
// (replacementAtDeadline). It counts as held work (workHeld), so the report wait's bound arms over a
// take a daemon start restored, too (reportBounded), which the bound leaves out with nothing held.
// A replacement held already keeps what it holds and what it waits for, as a held stale-plugin
// relaunch does: the sweep observes a stale address at every pass. The line is written where the
// replacement begins to wait, by the take's phase (waitsLine), naming by, what raised it, and attrs.
func (m *Machine) holdReplacement(by string, run func(context.Context) (ProcessEnd, error), attrs ...any) {
	if m.heldReplacement != nil {
		return
	}
	m.log.Info(m.takePhase().waitsLine(), append([]any{"task", pendingNumber(m.claim.Pending), "held", "replacement", "by", by}, attrs...)...)
	m.heldReplacement = run
}

// boundedReportHolds is whether work that reaches the claim now — a replacement of its live
// process (repoint, runWake), or the operator's stop of it (stop, holdStop) — waits for the report
// of the take's turn rather than let the process go: work that reaches the claim joins that wait
// (reportHolds) — the take's turn is over and its report has not come, or work already waits on
// the take — and a bound of the wait stands. Over a whole view that is the report wait's own
// (TimerReport, armed at the end of the decision: reportBounded), and over a view not whole the
// reconnect deadline (TimerRegistration), which a daemon start, a close and a reconnect hello arm
// over a pending take (connview.go), and which a launch the pacing parked takes from the process
// it waits over (waitOnMachine): work no bound would reach lets the process go as before. While a
// turn runs it does not hold, so a turn is cut off as before.
func (m *Machine) boundedReportHolds() bool {
	if !m.reportHolds() {
		return false
	}
	if m.connSeen == viewWhole {
		return true
	}
	_, deadline := m.timers[TimerRegistration]
	return deadline
}

// replacing is the replacement replaceProcess makes, raised anew from the top: what a held
// replacement runs where the wait for the report ends (holdReplacement), and what a parked one
// carries to its wake's turn for that wait (wake.replaceAgain). It goes through the lifecycle
// recheck and the wake gate again, so a held replacement released by a report is paced as every
// launch is, and through replaceNow, so a suspension held meanwhile ends the process instead. It
// answers replaceProcess's end beside its error, which the reconnect deadline reads
// (replacementAtDeadline).
func (m *Machine) replacing(interruption string, outOfMemory bool, ending string) func(context.Context) (ProcessEnd, error) {
	return func(ctx context.Context) (ProcessEnd, error) {
		return m.replaceProcess(ctx, interruption, outOfMemory, ending, heldAtItsTurn)
	}
}

// heldAtItsTurn and letGoAtItsTurn are replaceProcess's held at its callers: whether its launch,
// where the wake pacing parks it, is held at its wake's turn for the report of the take's turn
// still to come, rather than let the process go (runWake, wake.replaceAgain). The replacement of
// a live process whose report can reach the daemon is — a repoint whose observation does not say
// the daemon's API moved, the stall backstop's, the relaunch of a hello no answer followed — and a
// displacement, whose process is gone, is not, nor a stale-plugin relaunch, whose lane holds its
// own wake for that report (runWake, relaunchWaitsForReport).
const (
	heldAtItsTurn  = true
	letGoAtItsTurn = false
)

// replacementAtDeadline is the reconnect deadline of a claim whose replacement waits for the report
// of its take's turn (reconnectDeadline): over a view not whole, as when the stale address is the
// worker stream's, that deadline is the wait's bound, since the report wait's own bound judges only
// a whole view (reportBounded). With no report by it, the task is taken back with the note of a
// cut turn (reportMissedAtDeadline), and the replacement then runs: the repoint's own relaunch,
// uncharged, rather than the no-hello path's retirement, which charges CauseNeverReconnected, since
// a stale observation is never charged. The take-back comes first, as at the report wait's own
// bound, so a relaunch the pacing parks finds no report to wait for at its turn. A take-back the
// store refuses, and a replacement that let nothing go — its lifecycle recheck refused
// (EndNotMade) — keep the replacement held and the process, and the deadline is tried again at the
// probe interval, as the deadline's other refusals are. The operator's stop held for the report
// takes the same deadline the same way (stopAtDeadline). With no replacement held it does nothing.
func (m *Machine) replacementAtDeadline(ctx context.Context) error {
	held := m.heldReplacement
	if held == nil {
		return nil
	}
	if err := m.reportMissedAtDeadline(ctx); err != nil {
		return err
	}
	m.heldReplacement = nil
	end, err := held(ctx)
	if end == EndNotMade {
		m.heldReplacement = held
		m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
	}
	return err
}

// displaced is the claim's process found gone, or found to be some other process, through no
// fault of its own (runtime.Observation.Displaced): the daemon itself replaced its pod while it ran,
// to launch another role there, or it was another runtime's process — a Sandbox role process a
// daemon running the project under kubernetes recorded — that this daemon takes over
// (`runtime.tmux.take_over`) and cannot see. The observation's detail names which. The process did
// nothing wrong, so it is recovered as repoint recovers a stale one: nothing is charged, neither a
// launch failure nor a death with work outstanding, and the claim's session is relaunched at once
// (replaceProcess). A turn the process was in is lost, and its task is taken back under its
// number, which the resumed session takes again; a held suspension ends in a suspension. The
// process is gone, so a compaction running over it is dropped before this runs and its end gives
// the claim back nothing (judge, compactionLostProcess). It is the one recovery that carries an
// out-of-memory end's note (runtime.Observation.OutOfMemory), since a pod the daemon replaced can
// also have run out of memory before the replacement landed, and the role whose turn that end cut
// off is told so wherever the end is recovered.
//
// What the displacement ended is recorded first, as a death's is (died): the process is gone
// whatever the replacement goes on to do, and its take-back, made at once or at the turn of a
// launch the wake pacing parks (replaceNow), changes the task the record reads (recordEnd,
// endDisplaced). A task whose take's turn was running is still taken
// there, so the record says its turn was cut off.
func (m *Machine) displaced(ctx context.Context, observation runtime.Observation) error {
	// Guarded as repoint's own read is, and for the reason its comment gives
	// (backstopStalledTurn, noProcessRecorded).
	incarnation, process := noProcessRecorded, noProcessRecorded
	if loc := m.claim.Locator; loc != nil {
		incarnation, process = loc.Incarnation, loc.Label()
	}
	m.log.Warn("supervise: the process was displaced through no fault of its own, its pod replaced by the daemon or it another runtime's process this daemon takes over; relaunching it uncharged",
		"incarnation", incarnation, "process", process, "observed", string(observation.Kind), "detail", observation.Detail)
	m.recordEnd(ctx, endDisplaced)
	_, err := m.replaceProcess(ctx, "the process was displaced mid-turn, its pod replaced by the daemon or it another runtime's process this daemon takes over", observation.OutOfMemory, endDisplaced, letGoAtItsTurn)
	return err
}

// ProcessEnd is what a replacement did to the claim's process (replaceProcess): the end the
// machine reached, which is not always the one it set out to make. It is read from the claim the
// replacement left rather than from its error, because the two answer different questions — the
// error says whether everything that end needed was written, and this says what became of the
// worker — and a caller that tells somebody about it needs the second (backstopStalledTurn,
// Deps.StallBackstopped). A process ended whose record the store then refused is ended all the
// same, and a replacement that stopped before it touched the process ended nothing at all.
//
// It is the daemon's whole classification of what became of a worker, so a state a replacement
// can leave a worker in is a value here and never a hole between two values: the owner's notice
// is written from this type alone, and three things are total over it rather than over the paths
// anyone remembered — the telling (TellsOwner), the sentence the notice carries (the daemon's
// stallBackstopEnds) and the case that drives each end (the walk in stallbackstop_test.go).
// Every value is listed in ProcessEnds, which those walks read.
type ProcessEnd string

const (
	// EndNotMade is the claim still holding the process the replacement found, and its turn with
	// it, with no launch queued: the launch's own lifecycle recheck, which a replacing launch makes
	// before any take-back, refused it (launchReplacing, relaunchRefused), or the take-back made
	// right before the process is let go — a launch that runs now, a held suspension's end — was
	// refused (interrupted). No process was let go and none is coming, so the next sweep posts over
	// the claim again, which is why it is the one end told to nobody (TellsOwner).
	EndNotMade ProcessEnd = "not made"
	// EndRelaunched is the process let go and stopped and the claim's session brought back on a
	// new one, which the claim records (launchTries).
	EndRelaunched ProcessEnd = "relaunched"
	// EndRelaunchParked is the launch that replaces the process parked by the wake pacing until
	// its turn (reserveWake, parkWake), with no take-back made at the park: the claim still records
	// the process and still runs it, its turn and the task that turn serves with it, until that
	// wake's turn, which is where the take-back is made and the launch stops the process and
	// brings the session back (launchReplacing, runWake, wakeNow, launchNow) — unless a suspension
	// held meanwhile ends the claim there instead (endHeld), the lifecycle recheck that turn makes
	// first refuses the launch and leaves the process as it is (runWake, relaunchRefused), the
	// take's turn ended in the wait with its report still to come, which holds the replacement for
	// that report (runWake, holdReplacement), or the tree's close has dropped the wake with the
	// process (letGo). It is an end of its own because
	// something has happened to this worker — a launch that replaces it is queued — while nothing
	// has been relaunched yet, and the two sentences are not the same news.
	EndRelaunchParked ProcessEnd = "relaunch parked"
	// EndRelaunchWaitsForPod is the process let go and stopped and the launch that replaces it
	// waiting at its issue pod's gate behind the roles of the pod that start or load their
	// sessions before it, one load at a time (askPod, wake.park). It asks as no request's
	// (launchReplacing), so, where no request has led the pod's run of starts, a start asked after
	// it can go ahead of it (podQueue.next): one a request raised, or, where no such start waits,
	// the start of the role working the issue's current phase and, unless this launch is that
	// role's, one of a role before its own in claim.Roles. It may wait out more than one load. The
	// claim is written launching and records no process, and the gate's grant of its start starts the
	// replacement (podStartDue) — unless a suspension or a retirement of the claim takes the
	// launch out of the gate first (endPodPark), the start the grant makes is refused until the
	// claim's launch budget runs out (launchTries, fail), a project pause holds it at the grant
	// (pauseGate), or the daemon's stop leaves the launch
	// to its next boot. It is an end of its own because the claim alone reads as the relaunch
	// still to make (EndRelaunchPending), where nothing refused or cut this one short: it is
	// queued at the gate.
	EndRelaunchWaitsForPod ProcessEnd = "relaunch waits for its pod"
	// EndRelaunchPending is the process let go and stopped with nothing in its place: the
	// launch's own write was refused and the same launch waits on the probe interval
	// (relaunchLater), or the daemon's stop cut it short and the next boot makes it. The claim
	// records no process either way.
	EndRelaunchPending ProcessEnd = "relaunch pending"
	// EndSuspended is a suspension held for the turn ending the process rather than replacing it
	// (endHeld): nothing runs for the claim until it is resumed.
	EndSuspended ProcessEnd = "suspended"
	// EndFailed is every try at the replacement failing until the claim's launch budget is gone
	// (launchTries, fail): nothing runs for the claim and nothing relaunches it.
	EndFailed ProcessEnd = "failed"
)

// ProcessEnds is every value of ProcessEnd, in the order they are declared. A value added to the
// type is added here: TestProcessEndsListsEveryEndDeclared reads this package's own source and
// fails until it is, and the walks that keep the owner's notice total over the type read this
// list (TestEveryProcessEndTellsItsOwnerExactlyOnce, the daemon's
// TestTheNoticeHasWordsForEveryEndThatTellsItsOwner). A fresh slice is returned so that no
// caller can edit the one the next reads.
func ProcessEnds() []ProcessEnd {
	return []ProcessEnd{EndNotMade, EndRelaunched, EndRelaunchParked, EndRelaunchWaitsForPod, EndRelaunchPending, EndSuspended, EndFailed}
}

// TellsOwner is whether this end is news for the claim's owner, which is the one thing the
// machine decides about the telling: the call that tells is a single one (backstopStalledTurn),
// so an end that tells is told once per decision that reaches it. A stall can be decided twice:
// a wake lives only in memory, so a daemon restart while a relaunch is parked forgets it, and
// the next sweep decides the claim again and tells again.
//
// Every end but EndNotMade is something that happened to the worker — a relaunch committed, its
// process stopped, its session brought back, its claim failed — and EndNotMade is the claim
// still holding its process with nothing queued to replace it, which telling of would send an
// architect looking for a recovery nobody made.
//
// It is written as "everything but" rather than as a list of the ends that tell, because the
// failure this rule exists against is silence: a value added to the type tells its owner before
// anyone remembers to enumerate it, and the worst it can then say is that the daemon has no
// sentence for what it did (the daemon's stallBackstopEnd), which is a notice somebody reads
// rather than a worker that stopped with nobody told.
func (e ProcessEnd) TellsOwner() bool { return e != EndNotMade }

// replaceProcess is the recovery a process ended or found stale through no fault of its own
// (repoint's stale address, refreshPlugin's stale plugin contract, displaced's replaced pod)
// shares once logged: a task whose turn the process was running goes back to waiting where the
// process is let go (interrupted, replaceNow) — at once for a launch that runs now or a held
// suspension, and at the wake's turn for a launch the wake pacing parks — the same recovery a
// death in a turn gets; a held suspension ends here as it does for a death (endHeld), suspended
// rather than relaunched; otherwise the claim is relaunched at once through the launch path a
// death uses (launch), a Resume of its recorded session or a Spawn
// over its existing Sandbox when it has not registered yet. interruption is the sentence the
// turn's recovery is logged with, and outOfMemory says whether the end that cut that turn off was
// the pod running out of memory: only a displacement can carry one, since the other callers
// replace a process that is still running. ending is why the process is let go, in the daemon's
// words, which the launch records with what that process ended (launch, letGo); a held
// suspension's stop records its own (endHeld). held is whether the launch, where the wake pacing
// parks it, is held at its wake's turn for the report of the take's turn still to come
// (heldAtItsTurn, runWake).
//
// It answers the end it reached beside its error, for the caller that has somebody to tell
// (ProcessEnd): every one of these recoveries can end the process and still fail — the launch's
// write is refused, its tries run the launch budget out, the held suspension's write is refused
// — and the end is what happened to the worker, which is decided here, once, rather than at each
// call site.
func (m *Machine) replaceProcess(ctx context.Context, interruption string, outOfMemory bool, ending string, held bool) (ProcessEnd, error) {
	found := ""
	if loc := m.claim.Locator; loc != nil {
		found = loc.Incarnation
	}
	parked := m.wake
	// Whether the runtime left a process running is this replacement's own fact, so it starts
	// unset here: a refusal an earlier replacement of this claim met is not what the end reached
	// now says of the process it let go (stopUnconfirmed, backstopStalledTurn).
	m.stopUnconfirmed = false
	err := m.replaceNow(ctx, interruption, outOfMemory, ending, held)
	return m.endReached(found, parked), err
}

// replaceNow is the replacement itself, whose end replaceProcess reads off the claim it leaves.
//
// The take-back of a task the process holds is made where the process is let go, and not
// before. A launch that runs now takes the task back first and then lets the process go, so a
// take-back the store refuses ends the replacement where it stands: the claim's process kept,
// nothing relaunched and no wake raised (interrupted). A launch the wake pacing parks makes no
// take-back here: the process it replaces runs on until the wake's turn, and a turn it starts in
// the wait is the take's turn, which the take-back the wake's turn makes cuts off or finds over
// (runWake) — this replacement's own take-back, carried on the wake with interruption and
// outOfMemory (launchReplacing), so a parked displacement still tells of an out-of-memory end.
// Taking the task back at the park would leave that process running a turn that is no
// take's, and the process the launch starts taking the number again: two runs of one task. A
// stale-plugin relaunch that ends at a refused take-back is raised again by the next pass, which
// reads the claim as stale as it left it (refreshStalePlugins in internal/daemon). A take-back
// that lands marks the task as taken from that process's turn, which the let-go that follows
// records (Machine.cutTurnOf). A held suspension ends the process here, with no launch to park,
// so its take-back is made first. A replacement held at its turn carries itself, raised anew, to
// that turn (replacing, wake.replaceAgain), which holds it for the report of a take's turn that
// ended in the wait rather than take that task back as a cut turn (runWake).
func (m *Machine) replaceNow(ctx context.Context, interruption string, outOfMemory bool, ending string, held bool) error {
	takeBack := func(ctx context.Context) error { return m.interrupted(ctx, interruption, outOfMemory) }
	if m.held != nil {
		if err := takeBack(ctx); err != nil {
			return err
		}
		return m.endHeld(ctx, false, stopOwed)
	}
	var again func(context.Context) (ProcessEnd, error)
	if held {
		again = m.replacing(interruption, outOfMemory, ending)
	}
	return m.launchReplacing(ctx, ending, takeBack, again)
}

// endReached is the end a replacement made, read from the claim it left beside found, the
// incarnation that claim recorded when the replacement began, and parked, the wake that claim
// held before it. The claim says it whole: a claim whose launch the pacing has just parked waits
// for that wake's turn, one whose launch waits at its issue pod's gate waits for that gate's
// grant, one still recording that incarnation was never let go of, one recording another is
// relaunched, and a claim recording none was let go and is where the let-go left it — suspended,
// failed, or launching with the launch still to make, since every caller of letGo writes the
// claim's state in the same step.
//
// The wake is read first and by identity, because it is the one end the claim alone cannot show:
// a launch the pacing parked leaves the claim exactly as the replacement found it, process and
// state and all (parkWake), and the wake the machine holds after is this replacement's only where
// it is not the one it began with. A launch waiting for its issue pod's start left the claim
// launching with no process (askPod, wake.park), which alone reads as the relaunch still to make,
// and its wake is what tells the two apart. The caller holds mu.
func (m *Machine) endReached(found string, parked *wake) ProcessEnd {
	if m.wake != nil && m.wake != parked {
		if m.wake.park != nil {
			return EndRelaunchWaitsForPod
		}
		return EndRelaunchParked
	}
	if loc := m.claim.Locator; loc != nil {
		if loc.Incarnation == found {
			return EndNotMade
		}
		return EndRelaunched
	}
	switch m.claim.State {
	case StateSuspended:
		return EndSuspended
	case StateFailed:
		return EndFailed
	}
	return EndRelaunchPending
}
