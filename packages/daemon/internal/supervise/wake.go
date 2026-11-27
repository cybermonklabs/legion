package supervise

// The wake unit: the pacing that holds back the wakes this package causes, so that a restart or a
// plugin wave starts a project's claims at the rate its operator set rather than all at once
// (dispatch://INFRA-976). One wake takes a place in one of the pacing's two queues at the gate
// (reserveWake, WakeQueue), waits on the machine until the pacing lets it out (waitOnMachine)
// and runs then (wakeNow); a request a person is waiting on is let through unpaced (exemptWake).
// A wake for a role with an Envoy message waiting, and the relaunch of a role whose own process
// died, wait in the first queue, ahead of a restart's backlog (dispatch://INFRA-5682 R1, R2). A
// request that reaches a claim a wake parked over no process takes that wake off the machine first,
// its place kept, and gives it back if it launched nothing (dropWakeFor, reparkWake), an operator's
// retry no row of the state the gate found decides runs that wake's launch there and then
// (runDroppedLaunchForRetry), and an operator's task over a launch parked above a process the
// claim still records runs that launch there and then, unpaced (runParkedLaunchNow). Every wake
// writes one line as it runs, in a shape the daemon's own catch-up lane shares (wakeRan,
// WakeRanMessage, WakeRanFields). A launch the issue pod's start gate cannot grant at once parks
// on the machine as a wake too, whose turn is that gate's grant rather than a let-out of the
// pacing's (askPod, wake.park, podStartDue).

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// WakeKind is which of the daemon's paced lanes a wake is in, and the whole of what the line that
// wake writes says it was (wakeRan, WakeRanFields). The three below are every lane the daemon has
// named — `launch`, `plugin-refresh` and `catch-up` — and each of them draws from the one limiter
// the daemon builds (internal/wakepace, openSupervision in internal/daemon), because what an
// Anthropic organization's per-minute uncached-input cap counts is the context a woken session
// reads, not whether the daemon started a process for it.
//
// Two of the three start a role process and are held in the claim's own machine: the launch every
// other decision of this package raises, and the relaunch the stale-plugin refresh lane raises,
// which is a launch in every step it takes and is told apart only so that a reader of the
// daemon's state can tell the refresh of a fleet from the work that refresh is holding up
// (dispatch://INFRA-2712 R4). That is why no decision reads a kind of its own accord: one that
// means "this wake starts a process" asks StartsProcess, which both answer, and a kind itself is
// read only to be written — into the view a reader of every claim takes and into the lines the
// machine writes of a wake (WakeWaiting, wakeRan). The third is the outbox's own — a catch-up
// waits on its durable row rather than in a machine, and no machine parks one.
//
// They are one named type rather than loose strings so that a misspelt kind does not compile and
// a fourth lane is found by the compiler in every place a kind travels — the machine's own wake,
// the view a reader of every claim takes (WakeWaiting, WaitingWake) and the daemon's state
// (api.WakeWaiting) — rather than by grep. The wire stays a plain string, and the operator API's
// schema keeps it unconstrained (contracts' legionWakeWaiting), so a plugin reads a kind a later
// daemon adds.
type WakeKind string

const (
	// WakeLaunch is a role process this package starts: the launch a spawn, a resume, a retry or
	// a relaunch raises, which lets the claim's previous process go at its turn (launchNow,
	// wakeNow). Every launch carries it but one, the relaunch the stale-plugin refresh lane
	// raises (WakePluginRefresh). The relaunch a stale registration raises keeps this kind: that
	// is one claim's own launch, answering the registration in front of it, rather than the lane
	// the gate bounds over a whole fleet (staleRegistration, relaunchStale, RefreshGate).
	WakeLaunch WakeKind = "launch"
	// WakePluginRefresh is the relaunch the stale-plugin refresh lane raises: a process found
	// speaking a plugin contract this daemon does not, replaced onto this daemon's plugin at one
	// paced wake (refreshPlugin, relaunchStale). It starts a process exactly as a launch does —
	// it is a launch, reserved at the same gate, and every decision that asks whether a wake
	// starts a process answers the same of both (StartsProcess) — and it is a kind of its own
	// for one reader: an operator reading `GET /legion/v1/state` through a plugin wave tells the
	// lane's wakes from the launches that start a tree's work by it, which no reading of the
	// wave of 2026-11-10 could do while the two were one kind (dispatch://INFRA-2712 R4).
	WakePluginRefresh WakeKind = "plugin-refresh"
	// WakeCatchUp is the notice a start row writes to an architect already running in place of a
	// launch, which waits on its own durable row rather than in a machine and is run by the
	// daemon's outbox (internal/daemon's wakeRan, the workflow's claimReady). No machine reserves
	// one, and it is the one kind here that starts no process; it is declared beside the other
	// two so that the daemon's lanes are named in one place.
	WakeCatchUp WakeKind = "catch-up"
)

// StartsProcess is whether a wake of this kind brings a role process up: the launch and the
// stale-plugin refresh's relaunch, and not the outbox's catch-up, which wakes a session that is
// already running. It is the question every decision about a waiting wake really asks, and each
// of them asks it here rather than naming a kind — the delivery held back from a process a wake
// is about to let go (claimHold), the observations and the probe that wake stands in for
// (judge, probe), the compaction refused for it (compactStart), the held suspension it ends
// (wakeNow), the lifecycle recheck and the turn take-back its run makes (runWake), the parked
// launch an operator's own delivery runs at once (runParkedLaunchNow), the second relaunch a
// stale registration does not raise over it (staleRegistration), the launch in flight the
// operator API reports of a claim (api's relaunchWaits) and the launch a delivery of the daemon's
// outbox waits for (internal/daemon's wakeForDelivery). Most of them reach it through
// launchWaits, which is this question asked of the wake a machine holds. A lane added later
// decides every one of them by answering this, in one place, rather than by being added to a
// list of kinds in each.
//
// It is exported for those last two readers, which are in internal/api and internal/daemon;
// nothing outside this package writes a kind.
func StartsProcess(kind WakeKind) bool {
	return kind == WakeLaunch || kind == WakePluginRefresh
}

// WakePace is the pace the daemon lets the wakes it causes out at — in this package, every role
// process a launch starts (reserveWake). A restart or a plugin wave wakes every claim at once,
// and the sessions those wakes start read their context in the minute that follows, which is what
// pushes an Anthropic organization over its per-minute uncached-input cap. The parts travel
// together because they are read together: the pacer decides when a launch runs, the interval
// how long after its let-out a place its caller took may reach the gate and still run at once,
// and the rate and the burst are what the line that launch writes says it was held to (wakeRan).
type WakePace struct {
	// Pacer is the daemon's two queues in front of its one limiter (wakepace.Pacer): a paced
	// wake takes a place in the queue its claim's class names (WakeQueue) and runs when the
	// pacer lets that place out. A waiting place has drawn no token, so a wake that leaves its
	// place before its turn gives that turn to the wake behind it. nil is the pacing off — every
	// launch runs as it is raised — which is this package's convention for a collaborator a
	// deployment does not have, as Secrets and PhaseHolds are: the daemon builds one pacer over
	// the one limiter all its lanes draw from and hands it here, and this package reads no
	// configuration (openSupervision in internal/daemon).
	Pacer *wakepace.Pacer
	// Interval is what one wake of the pacer's limiter costs (wakepace.Limiter.Interval), taken
	// from that same limiter: a launch whose caller took its place and that reaches the gate less
	// than this after the pacer let that place out still runs at once (RequestSpawn.Place). Zero
	// with a Pacer has no such grace: every carried place let out already is joined afresh, even
	// one reached at its let-out.
	Interval time.Duration
	// Rate and Burst are the pace the operator set (worker_wakes_per_minute, worker_wake_burst),
	// carried for the line a wake writes as it runs and read for nothing else.
	Rate  int
	Burst int
}

// WakeWaiting is a machine's waiting wake as its view names it: what kind of wake it is (WakeLaunch
// or WakePluginRefresh, the two kinds a machine parks) and the pacing's estimate of the moment it
// lets the wake's place out, as its queues stand (wakepace.Place.Estimate, read from the pacer when
// the view is read, Machine.View). A waiting wake holds a place and no moment. A change to the
// queues or to the pace can move that moment earlier or later, so two reads of one wake can name
// two moments: for example, a wake ahead of it in its own queue that leaves moves it earlier, and a
// wake that joins and is let out ahead of it moves it later. It is the memory-only `wake` the
// machine holds, cut down to what a reader outside this package is owed: never the launch itself,
// and never the state the gate found, which only an operator's request that does not wait reads
// (dropWakeFor).
type WakeWaiting struct {
	Kind WakeKind
	At   time.Time
}

// WaitingWake is one claim's waiting wake as the reader of every claim takes it
// (Machine.WaitingWake): the claim the wake will wake, that claim's issue and role, and the kind
// and the moment the view names (WakeWaiting). It is every field the daemon's state names a wake
// by (api.WakesWaitingOf), so that read copies these five and nothing else of the claim.
type WaitingWake struct {
	Claim claim.Token
	Issue string
	Role  claim.Role
	Kind  WakeKind
	At    time.Time
}

// wake is a launch the pacing holds until the pacer lets its place out: its place in the pacing's
// queues (place), the moment it took that place, which the line it writes reports as how long it
// waited, what kind of wake it is for that line, the state the gate found the claim in — which a
// request that never waits restores before it acts, over a claim parked with no process
// (dropWakeFor) — the launch itself, and, for a launch, the same launch raised anew. It keeps no
// moment of its own: a line the claim writes of a waiting wake names the pacer's estimate for its
// place as the line is written (due), since a change to the queues or to the pace can move that
// estimate earlier or later after the park.
type wake struct {
	// place is the wake's place in the pacing's queues (wakepace.Place), nil for a launch the
	// pause's gate held before the pacing saw it and for a start that waits at its issue pod's
	// gate (pause, park). While it waits, TimerWake is bound to it (bindWake), and every path that
	// spends or abandons the wake gives it up (placeTimer, reparkWake, runDroppedLaunchForRetry).
	place  *wakepace.Place
	joined time.Time
	kind   WakeKind
	state  ClaimState
	run    func(context.Context) error
	// again is the work this wake holds raised again from the top (launchAfter), for the turn
	// whose own lifecycle recheck the store could not answer: that retry waits the probe interval
	// (relaunchLater, relaunchAgain) and must recheck the lifecycle when it comes, as every launch
	// of this package does, so it re-enters the gate rather than the launch — and takes a place
	// in the pacing again, which reserveWake states. Every wake a machine parks today starts a
	// process and carries it; the field is nil where the launch path raised none to retry with.
	again func(context.Context) error
	// pluginMark is the intent of the stale-plugin relaunch this wake holds: the contract that
	// relaunch is made for, which the launch writes to PluginRelaunchFor when it starts the
	// process at this wake's turn, and the kind this wake was reserved under, which came from
	// the same intent (pluginIntent, markPluginRelaunch, launchNow, commitPluginRelaunch). It is
	// nil for every wake no such relaunch raised. It travels on the wake because the turn comes
	// tens of minutes after the frame that decided the relaunch, and because the mark belongs to
	// the launch and to no other: a wake that is dropped or replaced takes the intent with it,
	// and nothing of the relaunch it held is written, which is right — that relaunch never
	// replaced anything.
	pluginMark *pluginIntent
	// takeBack is the take-back of the task the process this wake replaces holds, which the
	// wake's turn makes where it lets that process go (runWake): the replacement's own, with the
	// sentence it was raised with and whether its end was the pod running out of memory
	// (launchReplacing, replaceNow), so a displacement the pacing parked keeps its out-of-memory
	// note. It is nil for a wake no replacement parked, whose turn makes the take-back of its own.
	takeBack func(context.Context) error
	// replaceAgain is the replacement this wake's launch is, raised anew from the top, for a
	// replacement of a live process whose report can reach the daemon (replaceNow, heldAtItsTurn):
	// a repoint whose observation does not say the daemon's API moved, the stall backstop's, the
	// relaunch of a hello no answer followed. A turn of such a wake that comes after the take's
	// turn ended with its report still to come holds it for that report rather than let the
	// process go (runWake, holdReplacement), as the held repoint is held. It is nil for every other
	// wake, whose turn lets the process go as it stands.
	replaceAgain func(context.Context) (ProcessEnd, error)
	// pause is the project pause whose gate holds this launch (pauseGate), 0 for a wake the
	// pacing holds. A held launch has no moment to run: it waits on the machine with no timer and
	// is published as no waiting wake (publish). It keeps the turn the pacing let it out on before
	// the gate held it, if any (paid), and holds no place in the pacing's queues while it is held.
	// It leaves the machine by one of these:
	//
	//   - The claim's stop for the pause (pauseStopDue), which drops it with the claim's process
	//     (suspended, letGo) and owes the role a launch at the resume (PauseStop.LaunchOwed),
	//     which meets the gates as a launch of its own.
	//   - Its let-out once that pause has ended (letOutHeldLaunch), by that stop finding no pause
	//     open or by the resume's event (resumePause): a launch that paid runs now and takes no
	//     place, one on a daemon whose pacing is off runs now unpaced, and any other takes a place
	//     in the queue its claim's class names and runs when the pacer lets that place out, at
	//     once or after a wait (WakeQueue, bindWake, waitOnMachine).
	//   - An operator's retry once that pause has ended, before either of those has let it out, over
	//     a claim the gate found in a state no retry row acts on (runDroppedLaunchForRetry): it runs
	//     now, unpaced, and takes no place.
	//   - Any other request the pacing never makes wait over a claim that records no process
	//     (dropWakeFor), a retry while a pause holds the claim or over a state a retry row acts on
	//     among them. A decision that launches makes its own launch through the wake gate, which
	//     holds it at the pause's gate again while a pause holds the claim (reserveWake); one that
	//     launches nothing gives this launch back to the pause's gate, its stop posted again
	//     (reparkWake, pauseGate).
	//   - Whatever else lets the claim's process go — another source's suspension, a retirement,
	//     a failure — which drops it with that process (letGo).
	//
	// A restart loses it with the machine, as it loses any wake (pauseGate).
	pause int64
	// paid is whether this launch's let-out, once a pause's gate holds it, runs it with no place
	// of its own (letOutHeldLaunch): the launch has had its turn from the pacing already. The
	// pacing let its place out, which a pause then met at its turn (reserveWake, wakeNow), an
	// earlier let-out joined it a place that was let out (letOutHeldLaunch), or the wake gate let
	// it past (ran) — on a turn where the pacing held it to one, and on none for an exempt launch,
	// the controller's, or one a daemon whose pacing is off ran.
	paid bool
	// ran is whether the wake gate has let this launch past already, and so it has paid and written
	// its wake line (reserveWake, wakeRan): a try the pause met after its write (pauseHeldTry), and
	// a start parked at its issue pod's gate (askPod), whose grant the pause can meet
	// (podStartDue). Its let-out writes no second line (runWake).
	ran bool
	// park is the hold of the start this launch waits for at its issue pod's gate (askPod), nil for
	// a wake the pacing or the pause's gate holds. Such a launch has let its process go, stopped it
	// and written its claim launching already (launchNow, launchTries, askPod); the park itself
	// takes no place in the pacing and has no moment to run: it waits on the machine with no
	// timer of the clock's, is published as no waiting wake (publish), is no wake of the pacing's
	// for a request that does not wait to drop (dropWakeFor), and leaves by its grant (TimerPodStart,
	// podStartDue) or by whatever drops the wake, whose decision takes its start out of the gate as
	// it ends (Machine.podPark, endPodPark). A grant during a pause clears it, and the pause's gate
	// holds the wake in its place (podStartDue).
	park *podHold
}

// reserveWake is the wake gate: it takes this wake a place in the pacing's queue its claim's class
// names (placeFor, WakeQueue) and, when the pacer does not let that place out at once, parks the
// wake on the machine until it does (waitOnMachine, wakeNow) and returns with the caller's work
// left to that let-out. A wake whose place the pacer lets out at once runs that work here, having
// written the line it writes as it goes (wakeRan). A launch whose caller took its place already
// (carried) is decided by the rule RequestSpawn.Place states (placeFor).
//
// A claim that records no process when the gate holds it — a spawn from queued, a rule resume
// from suspended, a retry from failed or retired, a process already retired by the decision that
// launches — is written launching and persisted first, exactly as relaunchLater writes one. That
// is not cosmetic: the outbox's start row delivers its task after the launch call, and a task
// delivered to a claim left failed is a named ignore, while one delivered to a claim left
// suspended launches it a second time (deliverResuming). At launching the task is queued
// (deliver), and the next process's worker takes it once it is ready. A claim that still records
// its process is left exactly as it is, so a restart never finds it launching with a live
// process; the launch lets that process go when its turn comes (launchNow).
//
// Exempt is the request saying so (exemptWake) or the claim being the project controller's: a
// death, a stale address or a displaced pod reaches a launch through displaced and repoint with
// no request at all, and the controller is the one session the operator's own read of a running
// wave relies on. An exempt wake takes no place and writes its line as it goes.
//
// A second launch raised over a claim whose wake still waits replaces that wake and takes a place
// of its own at the back of its queue; the place the first held is given up as the second binds
// TimerWake (bindWake, placeTimer), and the turn it would have taken goes to the wake behind it.
// The replaced wake's work is the same launch the new one makes, and the intent of a stale-plugin
// relaunch it carried goes with it unwritten, so nothing of the first relaunch is left to put
// right (wake.pluginMark). Neither stale-plugin relaunch raises such a second launch at all:
// refreshPlugin returns over a claim that already holds a wake of any kind, and staleRegistration
// over one whose launch waits. A paced request reaches here twice over a parked claim for no
// other reason, since the table answers a second start of a claim at launching without launching
// anything.
//
// again is the launch raised anew, for the turn to retry with when its own lifecycle recheck is
// unanswered; nil for a kind whose turn makes no such check (wake.again).
//
// The lane this wake belongs to is read here rather than passed in. Every launch of this
// package reaches the gate through one path (relaunchMarked, replaceProcess, launchAfter), and
// that path is every launch's: it knows nothing of the lane that raised the launch it is
// making, so a caller could only ever name the launch kind, and would tell a reader the wake is
// a launch when it is the stale-plugin lane's. The lane says which kind its launch waits under
// on the intent that launch already carries (pluginIntent, markPluginRelaunch, relaunchStale),
// and the read is made before the two returns below as well as on the wake parked, so a refresh
// that runs where it is raised — the controller's own claim, a daemon whose pacing is off —
// writes its lane in the line it writes (wakeRan) exactly as one that waits does.
//
// Those same two returns leave the credit of the refresh gate this machine may hold with no
// wake waiting for it, so the end of the decision gives it back (endRefreshLane,
// dispatch://INFRA-2712 R1): the work the credit was taken for runs here rather than waiting in
// the pacing, which is the whole of what the gate counts. A wake of this lane that parks keeps
// the credit on the machine for as long as it waits, and the turn that takes that wake off the
// machine is what gives it back (waitOnMachine, wakeNow, runWake).
//
// The project's pause is checked first, before the exempt return (decision 7 of
// dispatch://INFRA-3759's plan): while the project is paused a launch of any claim but the
// controller's runs nothing, takes no place and arms no retry, and is held by the pause's gate
// instead (pauseHolds, pauseGate), whoever raised it; a place its caller took for it is given up
// as the decision ends (Machine.handle). The daemon is told of a paced launch's let-out when the
// pacer lets its place out (wakeLetOut, decision 14) — here, for a place let out at once, and at
// the wake's turn for one that waits (wakeNow) — and of the wait as the decision that began it
// ends (tellWakeQueue), never of a moment at the join.
func (m *Machine) reserveWake(ctx context.Context, exempt bool, carried *wakepace.Place, run, again func(context.Context) error) error {
	kind := WakeLaunch
	if intent := m.pluginRelaunch; intent != nil {
		kind = intent.kind
	}
	if pause, holds := m.pauseHolds(); holds {
		return m.pauseGate(ctx, &wake{joined: m.deps.Clock.Now(), kind: kind, state: m.claim.State, run: run, again: again,
			pluginMark: m.takePluginRelaunch()}, pause)
	}
	controller := claim.IsController(m.claim.Role, m.claim.Issue, m.claim.Tree)
	if exempt || !gatePaces(&m.deps, controller) {
		// The operator counts the paced wakes of a minute against the rate and the burst this line
		// names (R6), so a wake no pacing could have held — the pacing off, or this wake let
		// through — says so rather than swelling that count.
		m.wakeRan(kind, false, 0, wakepace.Let{})
		return run(ctx)
	}
	// Past the return above there is a pacer and this wake is not one it lets through, so the
	// pacing could have held it: the daemon hands a Pacer only while the operator's rate is above
	// 0 (the daemon's run).
	now := m.deps.Clock.Now()
	place := m.placeFor(carried, now)
	if let, out := m.bindWake(place); out {
		m.wakeLetOut(let.At)
		m.wakeRan(kind, true, 0, let)
		return run(ctx)
	}
	// The intent of a stale-plugin relaunch travels with the launch it names, so the wake that
	// holds that launch holds the intent while it waits and gives it back at its turn
	// (markPluginRelaunch, runWake, wake.pluginMark). A launch that runs above leaves it on the
	// machine, where the launch itself commits it (launchNow, commitPluginRelaunch).
	return m.waitOnMachine(ctx, &wake{place: place, joined: now, kind: kind, state: m.claim.State, paid: true, run: run, again: again,
		pluginMark: m.takePluginRelaunch()})
}

// gatePaces is the gate's rule for a launch no request exempts and no project pause holds
// (reserveWake): the pacing paces it where the daemon has a pacer (WakePace.Pacer) and the claim
// is not the project controller's, the one claim it never holds.
func gatePaces(deps *Deps, controller bool) bool {
	return deps.WakePace.Pacer != nil && !controller
}

// PacedLaunch is whether the wake gate would hold a launch of c that no request exempts, under
// deps: no project pause holds c's launches (pauseHolding) and the gate paces it (gatePaces). It is
// the gate's own rule, read by the one caller that takes places itself, the boot that relaunches
// the launches the previous daemon left waiting (RequestSpawn.Place, internal/daemon's
// launchUnfinished), so that caller takes a place for exactly the launches the gate would have
// taken one for.
func PacedLaunch(deps *Deps, c *Claim) bool {
	controller := claim.IsController(c.Role, c.Issue, c.Tree)
	if _, held := pauseHolding(deps.Paused, controller); held {
		return false
	}
	return gatePaces(deps, controller)
}

// waitOnMachine is the park the wake gate, the re-park, the pause's let-out and the pause's gate
// share (reserveWake, reparkWake, letOutHeldLaunch, pauseGate), each having armed its own timer
// first, the first three by binding TimerWake to the wake's place (bindWake): waiting waits on the
// machine (Machine.wake), and a claim that records no process is written launching and persisted.
// A claim that still records its process is left exactly as it stands.
//
// What that claim does not keep is the three timers that watch the process this wake replaces:
// its boot interval and its registration deadline (armBoot, armRegistration), and the probe
// interval an uncertain answer armed (judge).
// The process is kept until the turn, so every one of those timers would otherwise fire into the
// wait — the boot interval of a process that died while it booted charging a launch failure of
// its own at each one, which is a claim failing before launch_failure_limit real failures
// (judge's own guard answers what they report, and this keeps them from reporting, as probe's
// own guard keeps the probe interval from being armed again inside the wait). The turn replaces
// the process and arms the new one's timers (launchTries, armBoot), a turn whose own lifecycle
// recheck refuses keeps the process and arms these again for it (runWake, watchAgain),
// and the runtime's sweep observes the process throughout. TimerSuspend is kept: a suspension
// held over this claim is still bounded by the deadline its arrival set (holdSuspension), which
// the wake's turn does not run.
//
// A launch this wake replaces that waits at its issue pod's gate leaves the gate at the end of the
// decision, as a failed launch: its start never ran (endPodPark).
func (m *Machine) waitOnMachine(ctx context.Context, waiting *wake) error {
	m.wake = waiting
	if m.claim.Locator != nil {
		for _, kind := range []TimerKind{TimerBoot, TimerRegistration, TimerProbe} {
			m.disarm(kind)
		}
		return nil
	}
	m.claim.State = StateLaunching
	return m.persist(ctx)
}

// wakeNow is the wake's turn (TimerWake): the pacer has let the wake's place out, which is the one
// turn of the pacing the launch takes, and the launch the gate held runs now. The daemon is told of
// that let-out first (wakeLetOut, decision 14), whatever the turn goes on to do. A suspension held
// meanwhile ends here instead of in a launch, exactly as relaunchAgain ends one, and the process
// the claim records is stopped first, as the relaunch this wake holds would have stopped it
// (replaceProcess, endHeld). A stale-plugin relaunch that ends this way marks nothing, as one a
// held suspension ends at its decision marks nothing: the intent the wake carries is dropped with
// the wake, no process was replaced, and the resume raises a launch of its own (wake.pluginMark,
// relaunchMarked, revive).
//
// Every hold a launch's turn finds here is over a process that still runs: the suspend of a claim
// in a turn (holdSuspension) and the suspend of a launch that did start a process
// (holdSuspendUntilReady), which are the only two the machine keeps, and letGo drops a hold with
// the process it is held for. A suspend of a parked claim that records no process is held no
// more — it is decided where it arrives, since there is no process to protect and the wake's turn
// is tens of minutes away over a wave (holdSuspendUntilReady) — and that suspension takes the wake
// with it, so no turn of such a claim is reached at all.
//
// Both ways out of a held suspension leave nothing of this refresh waiting in the pacing, so the
// end of this decision gives the credit of the refresh gate this machine may hold back
// (endRefreshLane, dispatch://INFRA-2712 R1): the wake that starts no process is spent where it
// is read, and the suspension that ends the claim's process lets that process go with the wake
// it was parked over (endHeld, suspendedFor, letGo). The wake is off the machine either way,
// which is what that end reads.
//
// Two things a launch does before it launches, which together close the window over a process
// the gate left running (reserveWake), and which runWake below takes for this turn and for an
// operator's delivery alike, though that delivery makes the first of them itself, before the wake
// leaves the machine, answers its refusal itself, and takes the second from launchWake
// (runParkedLaunchNow):
//
//   - The tree lifecycle is rechecked (checkLaunch), because the turn can be tens of minutes after
//     the check launchAfter made: an operator's close reserves its tree's cleanup before it stops
//     the tree's claims, so a wake that fires between the two would launch into a tree whose
//     cleanup is reserved. The refusal goes where launchAfter's own goes (relaunchRefused), and
//     the turn gives back what its own park took before it does: the watch of the process this
//     turn keeps (watchAgain). The credit of the refresh gate a refresh of the stale-plugin
//     lane waited on goes back at the end of the decision that met the refusal, which is where
//     every refused launch of this lane gives one back (relaunchRefused, endRefreshLane), and
//     the retry that refusal may leave waiting re-enters the gate as the lane that raised it,
//     carrying the intent this wake held (retryHolding, wake.again). Nothing of the claim is put
//     right for a stale-plugin relaunch there, because nothing was written for it: only a launch
//     that starts a process writes the mark (wake.pluginMark, launchNow).
//   - A turn the process it replaces may have started is taken back first (interrupted), exactly
//     as replaceProcess takes one back before it relaunches. The gate let this wake park over a
//     live process, and that process can have taken a task since: letting it go without the
//     take-back would leave the task taken in a launching claim, which settle settles in its
//     place: for a plugin below TaskTakeContract it finishes a task whose turn started by its end
//     frame though the turn never finished (finishTask), and for a plugin that names its number
//     it takes the task back as a cut turn. The other half of that race is a take accepted inside
//     the wait, which claimHold refuses for as long as a launch waits over the process it would
//     reach (launchWaits, take.go).
//
// A turn that comes while the project is paused launches nothing (decision 7 of
// dispatch://INFRA-3759's plan): the wake is held by the pause's gate instead, and the claim is
// stopped for the pause (pauseGate). The gate is asked after the held suspension, because a held
// suspension ends the claim here without a launch, for its own source — the pause's own hold
// among them, whose stop then owes the role this launch (endHeld) — and a pause's hold whose pause
// has ended is gone before this row runs, as it is at the start of every decision
// (dropEndedPauseHold).
func wakeNow(m *Machine, ctx context.Context, _ Event) error {
	waiting := m.wake
	m.wake = nil
	if let, out := waiting.letOut(); out {
		m.wakeLetOut(let.At)
	}
	if m.held != nil {
		// Every kind a machine parks today starts a process (StartsProcess), and this hold is
		// ended for one (endHeld): the process that launch would replace is stopped by the
		// suspension instead. The branch is what keeps a kind added later that starts no process
		// out of a launch's own steps — such a wake replaces nothing, so ending the hold at its
		// turn would stop the process in the middle of the turn the hold exists to keep whole
		// (holdSuspension).
		if !StartsProcess(waiting.kind) {
			m.log.Info("supervise: this claim's wake came due while a suspension waits for its turn to end; the hold keeps that turn and the wake does nothing",
				"kind", waiting.kind, "reason", m.held.Reason, "by", m.held.By)
			// The wake is spent and nothing is left of it; the credit goes back with the
			// decision (endRefreshLane).
			return nil
		}
		if m.reportHolds() {
			// The hold waits for the report of its take's turn (reportwait.go): ended here, its
			// stop would come before that report, and settle would take back as a cut turn a task
			// whose turn ran whole; or, over a take a report put back, it would cut off the take's
			// own turn, whose start may be on its way. So the wake is spent into the hold, whose
			// stop, at that report's finish or at the wait's bound or the hold's deadline, owes a
			// pause's role the launch this wake was, as endHeld would here (heldLaunch,
			// suspendedFor). A hold dropped unrun meanwhile leaves the launch to whoever raised
			// it: the stale-plugin pass, or the runtime's next report of the process. A hold over
			// a turn that runs is no such hold: the wake ends it (endHeld), as it ends every hold
			// over a running turn. The line says what the hold waits for, by the take's phase
			// (waitsLine).
			m.log.Info(m.takePhase().waitsLine(), "task", pendingNumber(m.claim.Pending), "held", "wake", "kind", waiting.kind, "by", m.held.By)
			m.heldLaunch = true
			return nil
		}
		return m.endHeld(ctx, true, stopOwed)
	}
	if pause, holds := m.pauseHolds(); holds {
		return m.pauseGate(ctx, waiting, pause)
	}
	return m.runWake(ctx, waiting, true)
}

// runWake is everything a wake does once it is off the machine: the lifecycle recheck and the
// take-back a launch makes first, the one line the wake writes (none for a launch that wrote it at
// the wake gate already, wake.ran), and the work itself. Its four callers are the turn the pacing
// gave (wakeNow), an operator's delivery, which runs a parked launch where it arrives rather than
// let a person wait for that turn (runParkedLaunchNow), an operator's retry of a claim whose
// launch waits over no process in a state no retry row acts on (runDroppedLaunchForRetry), and the
// let-out of a launch the pause's gate held once its pause is over (letOutHeldLaunch); all go
// through here, or through launchWake past the recheck, so that the launch an operator's task, a
// retry or a resume starts is the same launch in every step it takes, timers, charges and
// take-backs included.
//
// paced is whether the pacing decided when this wake ran, which is what the line says of it
// (wakeRan): true of the turn and of a let-out the pacing held, false of the launch a request
// pulled forward. The line of a paced wake names how the pacer let its place out — the queue and
// the turns it waited (wake.letOut) — and that of a launch a request pulled forward names none,
// its place given up rather than let out. The wait reported is the whole of the hold either way
// — for the turn, the hold the pacing gave it; for a launch a request pulled forward, how long it
// had been held when that request arrived; for a wake the pacing parked and the pause met at its
// turn, the hold from its join to its let-out.
//
// It is split at the first step, the lifecycle recheck: what follows it is launchWake, which an
// operator's task calls itself, since it makes that recheck in its own decision before it changes
// anything a refusal must leave as it was — it drops, retires or replaces the claim's pending task
// only once that recheck has passed, and answers a refusal with the pending task and the wake as
// they were (runParkedLaunchNow). Every other caller leaves the recheck to this function, and so
// does an operator's task over a plugin-refresh wake whose fact is gone: that wake launches
// nothing, so there is no launch to recheck, and the re-read below spends it before either step.
//
// Both steps can end the wake's work with the process the claim records kept and the wake
// spent, and each gives back exactly what it left standing. The watch is the first step's
// alone (watchAgain), and so is the retry: a refusal that leaves one hands it the intent this
// wake carried, so that the launch it raises waits under the lane that raised it
// (retryHolding). A refusal leaves a retry only over a live claim that records no process
// (relaunchRefused's first branch), and this lane does park a wake over such a claim: the
// launch whose StateLaunching write failed arms a retry that holds the intent, and the wake
// that retry parks waits over the claim that write left recording none (launchTries,
// relaunchLater, waitOnMachine). So the retry handed on here is a case the machine drives, and the
// lane's kind is what it must not lose on the way.
//
// Neither step gives the credit of the refresh gate back; the end of the decision does: the wake
// is off the machine before either runs here (wakeNow, runParkedLaunchNow), so a decision that
// ends at the lifecycle refusal or at a take-back the store would not take holds a credit no wake
// of this lane waits for, and gives it back there (endRefreshLane, relaunchRefused,
// dispatch://INFRA-2712 R1). An operator's task whose own recheck is refused leaves the wake
// waiting, and the credit with it (runParkedLaunchNow). Neither step gives a plugin mark back,
// because a wake that is abandoned writes none: the intent is written by the launch that starts
// the process replacing the stale one, and by nothing else (wake.pluginMark, holdPluginRelaunch,
// launchNow). A relaunch abandoned at either step started nothing, so the claim keeps the mark of
// whatever relaunch last did run, and the next registration of this same process is read against
// that one (staleRegistration).
//
// What giving that credit back means for the retry the refusal may arm is where the gate states
// it: that retry asks this gate nothing, so the wake it parks waits in the pacing with no
// admission behind it until the next sweep's correction counts it (RefreshGate, Correct).
// Holding the credit here instead would not close that window — the correction reclaims it at
// that same sweep anyway — and would open a worse one, since the machine would go on reading as
// held a credit whose wake is gone, and skip the gate at its next uncredited refresh
// (refreshCredit, refreshPlugin).
//
// The refresh lane's own wake reads its claim once more before either step, which is the only
// order in which the claim still holds what those two steps would spend: a claim that is no
// longer stale, or that has started a turn since, is relaunched by this wake not at all
// (refreshOverturned, dispatch://INFRA-2712 R2). Such a claim keeps its turn and the task that
// turn is working, gives back at the end of this decision the credit the gate admitted it on,
// and is left watched exactly as the lifecycle refusal below leaves one (watchAgain), its probe
// left to the sweep's next observation. The wake is spent and the relaunch is owed again, which
// the sweep's own stale-plugin pass raises for a claim that is still stale when it next reads
// it. An operator's task that meets such a wake spends it here as well, with no lifecycle
// recheck made, and is queued over the claim as it stands (runParkedLaunchNow).
//
// The difference between the two steps is what the store is reached for: only a task a take
// holds is taken back (interrupted, Delivery.taken), and a take is accepted in a ready or idle
// claim alone, whose turn's start then moves it to working — the table refuses a take and ignores
// a turn start in every state a process is still coming up in (takeNotReady, turnStarted,
// unready), and no live claim goes back to one of them (helloed). So the claim at the second
// step's return is ready, idle or working, which calls for neither the boot interval nor the
// registration deadline a park takes from a claim still coming up. What a take of its task waits
// on is not the park's either: the second wait is armed again for the claim this decision leaves
// (armWaits, TimerTurnStart), and the end-frame bound is one the park never disarms
// (TimerFinish). The probe interval an uncertain answer may have armed is one the sweep's next
// observation arms again (observe, judge). What such a claim is left without is the relaunch
// this wake held, which is what the lifecycle refusal leaves owing too: the next report of the
// process, or the sweep's own stale-plugin pass, raises one again.
//
// This turn is where a launch the gate parked over a running process lets that process go, so it
// is where what the process ended is recorded: the launch carries the cause the decision that
// raised it named (launchAfter), and the record reads the process as it stands now — the turn
// it may have begun in the wait, the task the take-back above took from it, its own last report
// — rather than as it stood at the park (launchNow, letGo, recordEnd).
func (m *Machine) runWake(ctx context.Context, waiting *wake, paced bool) error {
	if waiting.kind == WakePluginRefresh && m.refreshOverturned() {
		m.log.Info("supervise: the fact this claim's plugin refresh was raised for is gone; its relaunch is left to the next stale-plugin pass",
			"state", m.claim.State, "pluginContract", m.claim.PluginContract, "daemonApiVersion", m.deps.PluginContract)
		m.watchAgain()
		return nil
	}
	// A take's turn that ran in the wait and ended with its report still to come is the turn the
	// refresh's own relaunch waits for the report of, as a relaunch decided now would
	// (refreshPlugin, reportHolds, relaunchWaitsForReport): let go now, the process would take that
	// report with it, and the take-back below would mark the task a cut turn. The wake is spent and
	// the relaunch waits on the machine behind the wait for the report (reportwait.go), which runs
	// it through refreshPlugin.
	if waiting.kind == WakePluginRefresh && m.reportHolds() {
		m.watchAgain()
		m.relaunchWaitsForReport("wake")
		return nil
	}
	// The same holds for every other replacement of a live process the pacing parked whose report
	// can reach the daemon (wake.replaceAgain): a repoint, a stall backstop, the relaunch of a hello
	// no answer followed. Its wake is spent and the replacement waits for the report as the held
	// repoint does (holdReplacement, boundedReportHolds): run at the task's finish, or at the wait's
	// bound once the task is taken back with the note of a cut turn, raised anew from the top, so it
	// is paced again there. A turn that comes while a turn runs holds nothing, and cuts that turn
	// off below, as rule one of T3 says.
	if waiting.replaceAgain != nil && m.boundedReportHolds() {
		m.watchAgain()
		m.holdReplacement("wake", waiting.replaceAgain, "kind", waiting.kind)
		return nil
	}
	// Every kind a machine parks today starts a process (StartsProcess), so both steps are taken
	// for every wake that reaches here. The branch is what keeps a kind added later that starts
	// no process out of a launch's own steps: it replaces nothing, so it has no tree to recheck
	// the lifecycle of and no process whose turn is owed a take-back (launchWake).
	if StartsProcess(waiting.kind) {
		if err := m.checkLaunch(ctx); err != nil {
			m.watchAgain()
			return m.relaunchRefused(ctx, m.retryHolding(waiting.pluginMark, waiting.again), err)
		}
	}
	return m.launchWake(ctx, waiting, paced)
}

// launchWake is runWake past its lifecycle recheck, for a wake that recheck passed: the take-back
// a launch makes first, the one line the wake writes, and the work itself (runWake states each
// step). An operator's task that made the recheck itself comes here directly
// (runParkedLaunchNow).
func (m *Machine) launchWake(ctx context.Context, waiting *wake, paced bool) error {
	if StartsProcess(waiting.kind) && m.claim.Locator != nil {
		takeBack := waiting.takeBack
		if takeBack == nil {
			takeBack = func(ctx context.Context) error {
				return m.interrupted(ctx, "the process this launch replaces is let go now that its turn has come", ordinaryEnd)
			}
		}
		if err := takeBack(ctx); err != nil {
			return err
		}
	}
	// Past both steps the launch runs, so the intent this wake carried goes back to the machine
	// for that launch to commit as it starts (holdPluginRelaunch, launchNow). The credit the gate
	// admitted this refresh on goes back at the end of this decision, as it does wherever the
	// wake is spent: what it was counting is a refresh waiting in the pacing, and this one is
	// waiting no longer (endRefreshLane).
	m.holdPluginRelaunch(waiting.pluginMark)
	if !waiting.ran {
		var let wakepace.Let
		if paced {
			let, _ = waiting.letOut()
		}
		m.wakeRan(waiting.kind, paced, m.deps.Clock.Now().Sub(waiting.joined), let)
	}
	return waiting.run(ctx)
}

// retryHolding wraps a retry so that it re-enters the wake gate as the lane that raised it. The
// retry is the same launch raised anew from the top (wake.again, launchAfter), and the gate
// reads the lane off the intent the machine holds (reserveWake, pluginIntent), so a retry that
// re-entered with the machine holding none would park a stale-plugin relaunch under the launch
// kind: an operator reading the state through a wave would count the lane short by exactly the
// claims whose refresh is in trouble, while the count of what the lane has left still names
// them, and the two numbers would disagree in the one case the operator is reading them for
// (dispatch://INFRA-2712 R4, api.State.ClaimsToRefresh).
//
// It answers the three retries a stale-plugin relaunch can leave waiting: the one a refused turn
// arms, holding the intent the turn took off the wake it was parked with (runWake,
// relaunchRefused), the one a failed StateLaunching write arms after the launch had already
// committed the intent (launchTries, relaunchLater), and the one armed where a refusal meets a
// retry that has already put its intent back — that retry raises the same launch anew from the
// top, the lifecycle check of the launch it raises refuses it, and the refusal arms a second
// retry holding that same intent (launchAfter, relaunchRefused). again is the launch raised anew
// from the top and intent the relaunch it still owes.
//
// The intent goes back on the machine where that retry runs and nowhere earlier, so it reaches
// the launch that retry makes and no other (holdPluginRelaunch, reserveWake), and nowhere
// earlier is the only place it can go: nothing of this lane outlives the decision that raised
// it, so an intent put back at the refusal or at the failed write would be dropped as that
// decision ended and the retry would park under the launch kind after all (endRefreshLane).
// The closure is what carries it across the probe interval the retry waits out. A retry
// relaunchLater armed and the machine then drops — by a suspension it ends that claim with at
// that timer instead (relaunchAgain), or with the process it is let go with (letGo) — takes the
// intent with it, so nothing is left to mark the next launch of this machine, whatever raised
// it (markPluginRelaunch, commitPluginRelaunch). Restored where the retry runs, the intent
// reaches exactly the launch that retry makes, and that launch writes the mark where it starts
// the process replacing the stale one and nowhere else (launchNow), which is the rule every
// other path of this relaunch keeps.
//
// A retry no stale-plugin relaunch raised, and a kind that leaves none to raise, are handed
// back as they are: nil wraps nothing.
func (m *Machine) retryHolding(intent *pluginIntent, again func(context.Context) error) func(context.Context) error {
	if again == nil || intent == nil {
		return again
	}
	return func(ctx context.Context) error {
		m.holdPluginRelaunch(intent)
		return again(ctx)
	}
}

// messageLate is how long after its message mark a message wake may be let out before the line it
// writes says it came late for that message (wakeRan): Envoy keeps a role's message an hour and
// then drops it, so a wake let out later than this reaches its role with a quarter of that hour
// or less left, and the line goes to Warn for the operator to find the wait that cost it.
const messageLate = 45 * time.Minute

// wakeRan is the one line a wake writes, where it runs: at Info, so the production handler keeps
// it, and in one shape whether the wake waited or not, because the operator counts the wakes of a
// minute from it. kind is which wake it was, paced says whether the pacing decided when this
// wake ran — false for the controller's own claim, for a request the pacing lets through, for a
// daemon whose rate is 0, which is the pacing off, and for a parked launch an operator's task
// ran before its turn (runParkedLaunchNow) — and waitedFor is how long it was held, left out of
// a wake that was not. let is how the pacer let the wake's place out (wakepace.Let), the zero Let
// for a wake no let-out of the pacer's ran, and the line names the queue the place came out of
// and the turns it waited there. A message wake — one of a claim whose message mark stands
// (Claim.MessageMarked) — that the pacer let out more than messageLate after that mark says it
// came late for the message, at Warn and under the same message text, so that one grep still
// finds every wake. rate and burst are the pace it was held to, so one line says both what
// happened and what the operator set. The claim is the logger's own (NewMachine).
func (m *Machine) wakeRan(kind WakeKind, paced bool, waitedFor time.Duration, let wakepace.Let) {
	late := !let.At.IsZero() && m.claim.MessageMarked() && let.At.Sub(m.claim.MessageMarkAt) > messageLate
	level := slog.LevelInfo
	if late {
		level = slog.LevelWarn
	}
	m.log.Log(context.Background(), level, WakeRanMessage,
		WakeRanFields(kind, paced, waitedFor, let, late, m.deps.WakePace.Rate, m.deps.WakePace.Burst)...)
}

// WakeRanMessage and WakeRanFields are the shape of that line, for the one wake lane outside this
// package: the catch-up the daemon's outbox runs, whose place in the pacing's queue belongs to its
// outbox row rather than to a machine (the outbox's wakeRan). The shape is the contract — one grep
// over the daemon's log finds every wake of a restart or a plugin wave, whichever lane caused it,
// and a reader counts the wakes of a minute against the rate and the burst each line names — so
// the two lanes compose it here rather than each write their own. A caller with a claim of its
// own, where this package's logger already carries one, prepends it to the fields.
const WakeRanMessage = "supervise: wake ran"

// WakeRanFields is the fields of a wake's line, in order, as wakeRan states them. A wake that
// waited nothing leaves waitedFor out, one no let-out of the pacer's ran — the zero Let, which is
// a wake the pacing did not hold — leaves queue and turnsWaited out, and one that did not come
// late for its message (lateForMessage) leaves that out.
func WakeRanFields(kind WakeKind, paced bool, waitedFor time.Duration, let wakepace.Let, lateForMessage bool, rate, burst int) []any {
	fields := []any{"kind", kind, "waited", waitedFor > 0}
	if waitedFor > 0 {
		fields = append(fields, "waitedFor", waitedFor)
	}
	fields = append(fields, "paced", paced)
	if !let.At.IsZero() {
		fields = append(fields, "queue", let.Queue.String(), "turnsWaited", let.Turns)
	}
	if lateForMessage {
		fields = append(fields, "lateForMessage", true)
	}
	return append(fields, "rate", rate, "burst", burst)
}
