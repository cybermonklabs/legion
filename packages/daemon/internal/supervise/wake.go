package supervise

// The wake unit: the pacing that holds back the wakes this package causes, so that a restart or a
// plugin wave starts a project's claims at the rate its operator set rather than all at once
// (dispatch://INFRA-976). One wake takes a place in one of the pacing's two queues at the gate
// (reserveWake, WakeQueue), waits on the machine until the pacing lets it out (waitOnMachine)
// and runs then (wakeNow); a request a person is waiting on is let through unpaced (exemptWake).
// A wake for a role with an Envoy message waiting, and the relaunch of a role whose own process
// died, wait in the first queue, ahead of a restart's backlog (dispatch://INFRA-5682 R1, R2). A
// request that reaches a claim with a wake delayed over no process takes that wake off the machine
// first, its place kept, and gives it back if it launched nothing (dropWakeFor, delayWakeAgain), an
// operator's retry no row of the state the gate found decides runs that wake's launch there and
// then (runDroppedLaunchForRetry), and an operator's task over a launch delayed above a process the
// claim still records runs that launch there and then, unpaced (runDelayedLaunchNow). Every wake
// writes one line as it runs, in a shape the daemon's own catch-up lane shares (wakeRan,
// WakeRanMessage, WakeRanFields). A launch the issue pod's start gate cannot grant at once waits
// on the machine as a wake too, whose turn is that gate's grant rather than a let-out of the
// pacing's (askPod, wake.podStartWait, podStartDue).

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
// waits on its durable row rather than in a machine, and no machine delays one.
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
// is about to let go (sendPending), the observations and the probe that wake stands in for
// (judge, probe), the compaction refused for it (compactStart), the held suspension it ends
// (wakeNow), the lifecycle recheck and the turn take-back its run makes (runWake), the delayed
// launch an operator's own delivery runs at once (runDelayedLaunchNow), the second relaunch a
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
// or WakePluginRefresh, the two kinds a machine delays) and the pacing's estimate of the moment it
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
// request that never waits restores before it acts, over a claim delayed with no process
// (dropWakeFor) — the launch itself, and, for a launch, the same launch raised anew. It keeps no
// moment of its own: a line the claim writes of a waiting wake names the pacer's estimate for its
// place as the line is written (due), since a change to the queues or to the pace can move that
// estimate earlier or later after the delay.
type wake struct {
	// place is the wake's place in the pacing's queues (wakepace.Place), nil for a launch the
	// pause's gate held before the pacing saw it and for a start that waits at its issue pod's
	// gate (pause, podStartWait). While it waits, TimerWake is bound to it (bindWake), and every
	// path that spends or abandons the wake gives it up (placeTimer, delayWakeAgain,
	// runDroppedLaunchForRetry).
	place  *wakepace.Place
	joined time.Time
	kind   WakeKind
	state  ClaimState
	run    func(context.Context) error
	// again is the work this wake holds raised again from the top (launchAfter), for the turn
	// whose own lifecycle recheck the store could not answer: that retry waits the probe interval
	// (relaunchLater, relaunchAgain) and must recheck the lifecycle when it comes, as every launch
	// of this package does, so it re-enters the gate rather than the launch — and takes a place
	// in the pacing again, which reserveWake states. Every wake a machine delays today starts a
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
	// pause is the project pause whose gate holds this launch (pauseGate), 0 for a wake the
	// pacing holds. A held launch has no moment to run: it waits on the machine with no timer, is
	// published as no waiting wake (publish), and leaves only by the claim's stop for the pause
	// (pauseStopDue) or by its let-out once that pause has ended (letOutHeldLaunch). Its let-out
	// takes a place in the pacing for it unless it paid.
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
	// a start waiting at its issue pod's gate (askPod), whose grant the pause can meet
	// (podStartDue). Its let-out writes no second line (runWake).
	ran bool
	// podStartWait is the hold of the start this launch waits for at its issue pod's gate
	// (askPod), nil for a wake the pacing or the pause's gate holds. Such a launch has let its
	// process go, stopped it and written its claim launching already (launchNow, launchTries,
	// askPod); the wait itself takes no place in the pacing and has no moment to run: it waits on
	// the machine with no timer of the clock's, is published as no waiting wake (publish), is no
	// wake of the pacing's for a request that does not wait to drop (dropWakeFor), and leaves by
	// its grant (TimerPodStart, podStartDue) or by whatever drops the wake, whose decision takes
	// its start out of the gate as it ends (Machine.podStartWait, endPodStartWait). A grant during
	// a pause clears it, and the pause's gate holds the wake in its place (podStartDue).
	podStartWait *podHold
}

// reserveWake is the wake gate: it takes this wake a place in the pacing's queue its claim's class
// names (placeFor, WakeQueue) and, when the pacer does not let that place out at once, delays the
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
// (deliverLater) and goes at the next ready. A claim that still records its process is left
// exactly as it is, so a restart never finds it launching with a live process; the launch lets
// that process go when its turn comes (launchNow).
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
// over one whose launch waits. A paced request reaches here twice over a delayed claim for no
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
// and the read is made before the two returns below as well as on the wake delayed, so a refresh
// that runs where it is raised — the controller's own claim, a daemon whose pacing is off —
// writes its lane in the line it writes (wakeRan) exactly as one that waits does.
//
// Those same two returns leave the credit of the refresh gate this machine may hold with no
// wake waiting for it, so the end of the decision gives it back (endRefreshLane,
// dispatch://INFRA-2712 R1): the work the credit was taken for runs here rather than waiting in
// the pacing, which is the whole of what the gate counts. A wake of this lane that is delayed keeps
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

// waitOnMachine is the wait on the machine that the wake gate, the wake taken up again, the pause's
// let-out and the pause's gate share (reserveWake, delayWakeAgain, letOutHeldLaunch, pauseGate),
// each having armed its own timer first, the first three by binding TimerWake to the wake's place
// (bindWake): waiting waits on the machine (Machine.wake), and a claim that records no process is
// written launching and persisted. A claim that still records its process is left exactly as it
// stands.
//
// What that claim does not keep is the four timers that watch the process this wake replaces: its
// boot interval and its registration deadline (armBoot, armRegistration), the probe interval an
// uncertain answer armed (judge), and the wait for the turn of an outstanding prompt (acked,
// keepHeldPrompt). The process is kept until the turn, so every one of those timers would
// otherwise fire into the wait — the boot interval of a process that died while it booted
// charging a launch failure of its own at each one, which is a claim failing before
// launch_failure_limit real failures (judge's own guard answers what they report, and this keeps
// them from reporting, as probe's own guard keeps the probe interval from being armed again inside
// the wait). The turn replaces the process and arms the new one's timers (launchTries, armBoot), a
// turn whose own lifecycle recheck refuses keeps the process and arms these again for it (runWake,
// watchAgain), and the runtime's sweep observes the process throughout. TimerSuspend is kept: a
// suspension held over this claim is still bounded by the deadline its arrival set
// (holdSuspension), which the wake's turn does not run.
//
// A launch this wake replaces that waits at its issue pod's gate leaves the gate at the end of the
// decision, as a failed launch: its start never ran (endPodStartWait).
func (m *Machine) waitOnMachine(ctx context.Context, waiting *wake) error {
	m.wake = waiting
	if m.claim.Locator != nil {
		for _, kind := range []TimerKind{TimerBoot, TimerRegistration, TimerProbe, TimerTurn} {
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
// the process it is held for. A suspend of a delayed claim that records no process is held no
// more — it is decided where it arrives, since there is no process to protect and the wake's turn
// is tens of minutes away over a wave (holdSuspendUntilReady) — and that suspension takes the wake
// with it, so no turn of such a claim is reached at all.
//
// Both ways out of a held suspension leave nothing of this refresh waiting in the pacing, so the
// end of this decision gives the credit of the refresh gate this machine may hold back
// (endRefreshLane, dispatch://INFRA-2712 R1): the wake that starts no process is spent where it
// is read, and the suspension that ends the claim's process lets that process go with the wake
// it was delayed over (endHeld, suspendedFor, letGo). The wake is off the machine either way,
// which is what that end reads.
//
// Two things a launch does before it launches, which together close the window over a process
// the gate left running (reserveWake), and which runWake below takes for this turn and for an
// operator's delivery alike:
//
//   - The tree lifecycle is rechecked (checkLaunch), because the turn can be tens of minutes after
//     the check launchAfter made: an operator's close reserves its tree's cleanup before it stops
//     the tree's claims, so a wake that fires between the two would launch into a tree whose
//     cleanup is reserved. The refusal goes where launchAfter's own goes (relaunchRefused), and
//     the turn gives back what its own delay took before it does: the watch of the process this
//     turn keeps (watchAgain). The credit of the refresh gate a refresh of the stale-plugin
//     lane waited on goes back at the end of the decision that met the refusal, which is where
//     every refused launch of this lane gives one back (relaunchRefused, endRefreshLane), and
//     the retry that refusal may leave waiting re-enters the gate as the lane that raised it,
//     carrying the intent this wake held (retryHolding, wake.again). Nothing of the claim is put
//     right for a stale-plugin relaunch there, because nothing was written for it: only a launch
//     that starts a process writes the mark (wake.pluginMark, launchNow).
//   - A turn the process it replaces may have started is taken back first (interrupted), exactly
//     as replaceProcess takes one back before it relaunches. The gate delayed this wake over a
//     live process, and that process can have confirmed a task since: letting it go without the
//     take-back would leave a confirmed task in a launching claim, which settle retires as served
//     though no turn of it ever finished (retirePending). The other half of that race is a
//     delivery sent inside the wait, which sendPending holds back for as long as a launch waits
//     over the process it would reach (launchWaits, delivery.go).
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
		// Every kind a machine delays today starts a process (StartsProcess), and this hold is
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
		return m.endHeld(ctx, true, stopOwed)
	}
	if pause, holds := m.pauseHolds(); holds {
		return m.pauseGate(ctx, waiting, pause)
	}
	return m.runWake(ctx, waiting, true)
}

// runWake is everything a wake does once it is off the machine: the lifecycle recheck and the
// take-back a launch makes first, the one line the wake writes (none for a launch that wrote it at
// the wake gate already, wake.ran), and the work itself. Its three callers are the turn the pacing
// gave (wakeNow), an operator's delivery, which runs a delayed launch where it arrives rather than
// let a person wait for that turn (runDelayedLaunchNow), and the let-out of a launch the pause's
// gate held once its pause is over (letOutHeldLaunch); all go through here so that the launch an
// operator's task or a resume starts is the same launch in every step it takes, timers, charges
// and take-backs included.
//
// paced is whether the pacing decided when this wake ran, which is what the line says of it
// (wakeRan): true of the turn and of a let-out the pacing held, false of the launch a request
// pulled forward. The line of a paced wake names how the pacer let its place out — the queue and
// the turns it waited (wake.letOut) — and that of a launch a request pulled forward names none,
// its place given up rather than let out. The wait reported is the whole of the hold either way
// — for the turn, the hold the pacing gave it; for a launch a request pulled forward, how long it
// had been held when that request arrived; for a wake the pacing delayed and the pause met at its
// turn, the hold from its join to its let-out.
//
// Both steps can end the wake's work with the process the claim records kept and the wake
// spent, and each gives back exactly what it left standing. The watch is the first step's
// alone (watchAgain), and so is the retry: a refusal that leaves one hands it the intent this
// wake carried, so that the launch it raises waits under the lane that raised it
// (retryHolding). A refusal leaves a retry only over a live claim that records no process
// (relaunchRefused's first branch), and this lane does delay a wake over such a claim: the
// launch whose StateLaunching write failed arms a retry that holds the intent, and the wake
// that retry delays waits over the claim that write left recording none (launchTries,
// relaunchLater, waitOnMachine). So the retry handed on here is a case the machine drives, and the
// lane's kind is what it must not lose on the way.
//
// Neither step gives the credit of the refresh gate back; the end of the decision does: the wake
// is off the machine before either runs (wakeNow, runDelayedLaunchNow), so a decision that ends at
// the lifecycle refusal or at a take-back the store would not take holds a credit no wake of this
// lane waits for, and gives it back there (endRefreshLane, relaunchRefused,
// dispatch://INFRA-2712 R1). Neither step gives a plugin mark back, because a wake that is
// abandoned writes none: the intent is written by the launch that starts the process replacing
// the stale one, and by nothing else (wake.pluginMark, holdPluginRelaunch, launchNow). A
// relaunch abandoned at either step started nothing, so the claim keeps the mark of whatever
// relaunch last did run, and the next registration of this same process is read against that
// one (staleRegistration).
//
// What giving that credit back means for the retry the refusal may arm is where the gate states
// it: that retry asks this gate nothing, so the wake it delays waits in the pacing with no
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
// it.
//
// The difference between the two steps is what the store is reached for: only a delivery a turn
// has confirmed is taken back (interrupted), a turn is confirmed in a ready or idle claim
// alone, and the confirmation itself leaves that claim working — the table ignores a turn start
// in every state a process is still coming up in (turnStarted, unready), and no live claim goes
// back to one of them (helloed). So the claim at the second step's return is working or idle,
// which calls for neither the boot interval nor the registration deadline a delay takes from a
// claim still coming up; its confirmed task waits on no turn bound, because the confirmation
// disarms that bound where it sets ConfirmedAt (confirmCarrying); and the probe interval an
// uncertain answer may have armed is one the sweep's next observation arms again (observe, judge).
// What such a claim is left without is the relaunch this wake held, which is what the lifecycle
// refusal leaves owing too: the next report of the process, or the sweep's own stale-plugin
// pass, raises one again.
//
// This turn is where a launch the gate delayed over a running process lets that process go, so it
// is where what the process ended is recorded: the launch carries the cause the decision that
// raised it named (launchAfter), and the record reads the process as it stands now — the turn
// it may have begun in the wait, the task the take-back above took from it, its own last report
// — rather than as it stood when the gate delayed it (launchNow, letGo, recordEnd).
func (m *Machine) runWake(ctx context.Context, waiting *wake, paced bool) error {
	if waiting.kind == WakePluginRefresh && m.refreshOverturned() {
		m.log.Info("supervise: the fact this claim's plugin refresh was raised for is gone; its relaunch is left to the next stale-plugin pass",
			"state", m.claim.State, "pluginContract", m.claim.PluginContract, "daemonApiVersion", m.deps.PluginContract)
		m.watchAgain()
		return nil
	}
	// Every kind a machine delays today starts a process (StartsProcess), so both steps are taken
	// for every wake that reaches here. The branch is what keeps a kind added later that starts
	// no process out of a launch's own steps: it replaces nothing, so it has no tree to recheck
	// the lifecycle of and no process whose turn is owed a take-back.
	if StartsProcess(waiting.kind) {
		if err := m.checkLaunch(ctx); err != nil {
			m.watchAgain()
			return m.relaunchRefused(ctx, m.retryHolding(waiting.pluginMark, waiting.again), err)
		}
		if m.claim.Locator != nil {
			if err := m.interrupted(ctx, "the process this launch replaces is let go now that its turn has come", ordinaryEnd); err != nil {
				return err
			}
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
// re-entered with the machine holding none would delay a stale-plugin relaunch under the launch
// kind: an operator reading the state through a wave would count the lane short by exactly the
// claims whose refresh is in trouble, while the count of what the lane has left still names
// them, and the two numbers would disagree in the one case the operator is reading them for
// (dispatch://INFRA-2712 R4, api.State.ClaimsToRefresh).
//
// It answers the three retries a stale-plugin relaunch can leave waiting: the one a refused turn
// arms, holding the intent the turn took off the wake it was delayed with (runWake,
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
// decision ended and the retry would be delayed under the launch kind after all (endRefreshLane).
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
// daemon whose rate is 0, which is the pacing off, and for a delayed launch an operator's task
// ran before its turn (runDelayedLaunchNow) — and waitedFor is how long it was held, left out of
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

// dropWakeFor is an operator's wake never waiting over a claim the gate delayed with no process
// of its own: that delay wrote the claim launching (waitOnMachine), so the row the request reaches
// would decide a claim that is only waiting its turn. An operator's resume is answered "the
// claim is not suspended" and an operator's forget-session "the claim has a live process or
// launch", neither of which is true of such a claim, and an operator's own task is queued behind
// a turn that is tens of minutes away in a wave, where the claim the gate found would have
// launched for it at once (dropsWaitingWake, deliverResuming). So the wake is taken off the
// machine and the state the gate found is put back in memory, and the decision that follows
// writes the claim — a resume, a retry, a spawn and an operator's delivery launch, and a
// forget-session persists.
//
// A claim delayed over a process it still records is dropped for nothing, whatever the request.
// That delay changed neither the claim nor the process, so every row already decides it as it
// stands, and the wake still holds the only thing that retires that process: a launch whose turn
// lets it go and arms the next process's timers (wakeNow, waitOnMachine). The one request that may
// not wait for the turn — an operator's task, which the send holds back while a launch waits
// (sendPending's launchWaits) — runs that launch where it arrives rather than dropping it
// (runDelayedLaunchNow).
//
// The dropped wake keeps its place in the pacing's queue: its TimerWake is taken off the place
// rather than disarmed (detachWake), so the place waits on with no release to call, and the wakes
// around it keep their order. A let-out that comes while the wake is off the machine is read when
// the wake is taken up again (delayWakeAgain), and a wake the decision does not take up gives the
// place up there.
//
// The wake is returned so that a decision which launched nothing can take it up again
// (delayWakeAgain): a wake that waits must lose nothing, and a refusal, a named ignore or a
// forget-session leaves the claim exactly where the gate found it, with the launch this wake
// holds still owed to it.
//
// A paced request drops nothing: a second wake request takes no second place, and the claim's
// own wake is the launch it is asking for.
//
// Nor does a launch that waits at its issue pod's gate (wake.podStartWait) drop for any request.
// It is no turn of the pacing's: the launch has started, letting its process go and writing its
// claim launching, and waits only for another role's session load, which an operator's start
// waits for too (dispatch://INFRA-5297). So every request decides that claim as the launch under
// way it is, and the launch keeps its place in the pod's order.
func (m *Machine) dropWakeFor(ev Event) *wake {
	if m.wake == nil || m.wake.podStartWait != nil || m.claim.Locator != nil || !dropsWaitingWake(ev) {
		return nil
	}
	dropped := m.wake
	request, _ := requestName(ev)
	m.log.Info("supervise: a request that does not wait reached a claim the wake pacing holds over no process; dropping the wake",
		"request", request, "kind", dropped.kind, "due", dropped.due(), "state", dropped.state)
	m.claim.State = dropped.state
	m.detachWake()
	m.wake = nil
	return dropped
}

// runDelayedLaunchNow is an operator's task meeting a launch the pacing delayed over the process
// the claim still records: that launch runs here, unpaced, before the row that queues the task
// (Handle). Both halves of what the operator is owed hold only this way. A person is waiting on
// the task they have just given the claim, so it may not wait out a paced turn that is tens of
// minutes away in a wave; and the task may not reach the process that launch is about to let go,
// since the launch would kill the turn it had just begun and settle would retire the task as
// served (sendPending's launchWaits, retirePending). Running the launch here leaves the claim
// exactly where an unpaced daemon's would have left it — launching, over a process let go, with
// the next one's timers armed — so the row the task then reaches queues it for the process this
// launch starts (deliverLater), and it goes at that process's ready.
//
// Dropping the wake instead would leave the claim recording a process nothing retires: the delay
// disarmed that process's boot interval, registration deadline, probe interval and turn wait
// (waitOnMachine), the launch that was to replace it would be gone, and every report of it decides
// nothing while no launch waits only because one does (judge). The next report of that process
// would charge another launch failure, the death it already charged excepted (died).
//
// Three things it does not touch. A request the table refuses or ignores decides nothing, so it
// starts no launch either: the row is read here, before anything runs, exactly as Handle reads
// it after. A task the row acts on but queue refuses decides nothing either, and for an
// operator's task that refusal is exactly a claim holding a delivery no provider error took
// back: the deliver route sends the task with no id of its own and no outbox row
// (operatorDelivery, RequestDeliver.ID, Claim.DeliveredRows), so neither of the two repeats
// queue accepts over a pending delivery — the repeat of that very delivery under its id, and a
// row whose task the claim has already taken — can be what arrived, and what is left is "a
// delivery is already pending" (queue, ErrDeliveryPending). Running the launch for such a task
// would replace a live process for a request the claim answers with nothing. And a claim
// holding a suspension is left to its hold: the turn of such a claim ends the suspension rather
// than launching (wakeNow, endHeld), which is not what a task asks for, and the task is queued
// and kept until the hold lifts whichever process the claim is running (sendPending).
//
// The one pending delivery that is not that refusal is one a newer task replaces (replaceable):
// the task a provider error took back while the ladder's wait stands for it and nothing works it,
// or the daemon's own delivery of a relaunch's record while no prompt of it waits on a result. A
// newer task takes its place rather than waiting out a wait the next error only makes longer
// (queue, dispatch://INFRA-956 R2), or behind a sentence it carries itself (Delivery.message).
// The claim takes an operator's task over such a one, so a person is waiting on a task this claim
// will work, and the launch runs for it. The replacement is made here, before that launch, and
// not left to the row (replaceOwedTask, the one path both places a task replaces an owed one
// take). For the task a provider error took back it must be: the launch takes the ladder's wait
// with the claim's timers (letGo), and that wait is the whole of what makes that task
// replaceable, so queue — reading the claim the launch leaves — would refuse the very task the
// launch had just replaced a live process for. The relaunch's delivery would stay replaceable,
// since the let-go also ends the wait for its prompts' results (Machine.outstanding), which is
// all that holds it; it is replaced here with the other, on the one path. Nothing is lost if the
// launch's own lifecycle recheck refuses (runWake): the row that follows queues the operator's
// task into a claim holding none, which is where R2 leaves it either way.
//
// The launch takes no turn of the pacing: disarming its timer gives its place up (placeTimer), and
// the turn that place would have taken goes to the wake behind it. The daemon is told the launch is
// let out now (wakeLetOut), and the line it writes says the pacing did not decide when it ran
// (wakeRan, runWake).
//
// A launch the project pause's gate holds (wake.pause) is not run for the operator's task either:
// nothing but the controller launches while the project is paused (pauseGate), so the task is
// queued and kept, and goes where every task held for a launch goes once the claim runs again.
func (m *Machine) runDelayedLaunchNow(ctx context.Context, ev Event) error {
	if !operatorDelivery(ev) || !m.launchWaits() || m.wake.pause != 0 || m.claim.Locator == nil || m.held != nil {
		return nil
	}
	if r, ok := table[key{m.claim.State, kindOf(ev)}]; !ok || r.act == nil {
		return nil
	}
	if pending := m.claim.Pending; pending != nil {
		owed, attempt := pending.ID, pending.ProviderErrorRetries
		replaced, err := m.replaceOwedTask(ctx)
		if err != nil || !replaced {
			return err
		}
		if relaunchTask(owed) {
			m.log.Info("supervise: the operator's task replaces the daemon's delivery of the relaunch's record, and the launch delayed over the claim's process runs for it",
				"replaced", owed)
		} else {
			m.log.Info("supervise: the operator's task replaces the one a provider error took back, and the launch delayed over the claim's process runs for it",
				"owed", owed, "attempt", attempt)
		}
	}
	waiting := m.wake
	// The estimate is read before the disarm gives the place up (placeTimer): a place that left
	// estimates the zero time.
	due := waiting.due()
	m.wake = nil
	m.disarm(TimerWake)
	m.log.Info("supervise: an operator's task reached a claim whose paced launch waits over the process it replaces; that launch runs now and the task goes to the process it starts",
		"kind", waiting.kind, "due", due, "state", m.claim.State)
	m.wakeLetOut(m.deps.Clock.Now())
	return m.runWake(ctx, waiting, false)
}

// runDroppedLaunchForRetry is the operator's retry (RequestAskedRetry naming the operator, the line
// exemptWake draws: the controller's retries, which it asks of as many agents at once as it decides
// to, keep the turn the pacing gave them and launch at it) of a claim whose launch the pacing or
// the pause's gate delayed over no process, where the state the gate found is one no retry row acts
// on: queued, which every spawn and the boot's relaunch of an unfinished launch delay from;
// suspended, which a rule's resume delays from; and launching, which a try the pause met after its
// write (pauseHeldTry) and a start whose pod grant the pause met (podStartDue) are held in, their
// wake already past the pacing (wake.ran), whose run is a fresh try. The claim reads launching for
// the whole wait, so a person who sees it stuck reaches for `legion claims retry`; refusing that
// retry as "a queued claim is spawned, not retried" left the claim behind a restart's whole queue,
// an hour of it on 2026-11-23 (dispatch://INFRA-5482). The person is asking for the claim to run,
// and the launch the dropped wake holds is the one that runs it, so that launch runs here, unpaced,
// exactly as its turn would have run it: the claim is put back launching, where the delay wrote it
// (waitOnMachine), and the wake goes through runWake, lifecycle recheck and all. ran is false, and
// nothing is done, for every other request, for a claim whose gate-found state a retry row decides
// (failed, retired), and for any launch while a project pause holds the claim (pauseHolds): the API
// refuses the operator's retry while the project is paused (refusedWhilePaused), and a pause that
// opened after that refusal was read holds this launch as it holds every other, so the retry is
// refused and the wake delayed again, for its turn, or the pause's gate, to take it (wakeNow,
// pauseGate). A launch the pause's gate held whose pause has ended, before the gate's own stop or
// the resume's event let it out (pauseStopDue, resumePause), is let out here as those let it out
// (letOutHeldLaunch), unpaced.
//
// The launch takes no turn of the pacing, as an operator's task over a delayed launch takes none
// (runDelayedLaunchNow): its place is given up, the turn it would have taken going to the wake
// behind it, and the daemon is told the launch is let out now (wakeLetOut).
func (m *Machine) runDroppedLaunchForRetry(ctx context.Context, ev Event, dropped *wake) (ran bool, err error) {
	if asked, retry := ev.(RequestAskedRetry); !retry || asked.By != SuspendedByOperator || dropped == nil || m.held != nil {
		return false, nil
	}
	if _, paused := m.pauseHolds(); paused {
		return false, nil
	}
	if dropped.pause != 0 {
		dropped.pause = 0
		m.disarm(TimerPause)
	}
	m.log.Info("supervise: an operator's retry reached a claim whose paced launch waits over no process; that launch runs now",
		"kind", dropped.kind, "due", dropped.due(), "state", dropped.state)
	dropped.leave()
	m.claim.State = StateLaunching
	m.wakeLetOut(m.deps.Clock.Now())
	return true, m.runWake(ctx, dropped, false)
}

// delayWakeAgain takes a dropped wake up again when the decision it was dropped for launched nothing
// (Handle, after both the named ignore's refusal and the row that acted): the claim is in the
// state the gate found, with no process and no wake of its own, which is where a refusal, an
// ignore and a forget-session each leave it — and the launch the wake holds is still owed. Its
// TimerWake is bound to the same place again (bindWake) and the wake is delayed again as the gate
// itself delays a wake (waitOnMachine): the place kept its spot in its queue while the wake was
// off the machine (dropWakeFor), so this costs nothing and moves no other wake. A place the pacer
// let out meanwhile arms TimerWake at its let-out moment, which fires at once, so the wake runs
// through its own timer as its let-out would have run it.
//
// A claim that records a process takes no wake back, which is what the locator in the guard
// below says: every wake dropped here was delayed over no process, so a claim recording one is a
// claim the decision launched (dropWakeFor). A dropped wake that is not taken back — that launch,
// a claim the decision moved on, or a wake the decision raised in its place — gives its place up,
// the turn it would have taken going to the wake behind it.
//
// A write that fails is logged and changes no verdict: the request is answered as the table
// answered it, and the claim stands as that answer left it, which is what a refusal leaves today.
// The wake still waits in memory, and a restart loses it as it loses any other.
//
// A launch the project pause's gate held goes back to that gate, as it was held, rather than to a
// place in the pacing it never had (pauseGate): the gate's own stop is posted again, and it reads
// then whether that pause still holds the claim (pauseStopDue).
func (m *Machine) delayWakeAgain(ctx context.Context, dropped *wake) {
	if dropped == nil {
		return
	}
	if m.wake != nil || m.claim.Locator != nil || m.claim.State != dropped.state {
		dropped.leave()
		return
	}
	m.log.Info("supervise: the request launched nothing; the wake the pacing holds keeps its place",
		"kind", dropped.kind, "due", dropped.due(), "state", dropped.state, "pause", dropped.pause)
	var err error
	if dropped.pause != 0 {
		err = m.pauseGate(ctx, dropped, dropped.pause)
	} else {
		m.rebindWake(dropped.place)
		err = m.waitOnMachine(ctx, dropped)
	}
	if err != nil {
		m.log.Warn("supervise: the wake that keeps its place could not be written; it waits in memory alone",
			"kind", dropped.kind, "due", dropped.due(), "error", err)
	}
}

// dropsWaitingWake is whether ev is a request the wake pacing never makes wait over a claim it
// delayed with no process: every request exemptWake lets through, every forget-session whoever
// asks it (the operator's own, and the one a fresh session of the operator or the controller
// sends: RequestForgetSession.By) — which launches nothing, but which a claim left launching under
// a delayed wake would refuse "the claim has a live process or launch", and it has neither — and an
// operator's own delivery (operatorDelivery), which a suspended claim delayed by the rule's own
// resume would queue behind that paced turn where the state the gate found launches for it at once
// (deliverResuming, revive). An operator's delivery over a launch delayed above a process the
// claim still records is none of this: that launch runs where the task arrives instead
// (runDelayedLaunchNow).
//
// The daemon's resume by rule is none of them: it meets a delayed claim with "the claim runs:
// nothing to resume" (nothingToDo), which is the truth — the wake the claim waits for is the
// launch that rule would make. Nor is the controller's resume_agent or retry, which the pacing makes
// wait (exemptWake): dropping the wake for it would delay a wake of its own behind every turn the
// queue holds, for the launch this claim already waits its turn for, so it reaches the agent while
// its claim is left launching under a delayed wake, is refused because that claim is neither
// suspended nor failed, and the claim keeps the turn it holds.
func dropsWaitingWake(ev Event) bool {
	if _, ok := ev.(RequestForgetSession); ok {
		return true
	}
	return operatorDelivery(ev) || exemptWake(ev)
}

// operatorDelivery is a task a person gave a claim through the daemon's API: that route sends it
// with no id of an outbox row (api.operator's deliver, RequestDeliver.ID), where every task the
// workflow queues names the row it came from. The pacing makes no such task wait (dropsWaitingWake).
func operatorDelivery(ev Event) bool {
	deliver, ok := ev.(RequestDeliver)
	return ok && deliver.ID == ""
}

// exemptWake is whether the launch a request raises is one the wake pacing lets through: the
// operator's own resume and retry (RequestResume.By, RequestAskedRetry.By naming the operator),
// which a person is waiting on, and a spawn whose caller marked it exempt, which is the
// operator's spawn route and nothing else — the project controller is exempt by identity, in the
// gate itself, which covers the launches no request raises as well (reserveWake,
// claim.IsController). The same two events from the agent door are paced: the controller's
// resume, retry and fresh session (api's resumeAgent, retryAgent, claimFreshSession) are launches
// an agent raises, as many at once as it asks for — a controller lifting its own holds after a
// deploy asks one for every agent it held — and let through, they would start them all at once,
// past `worker_wakes_per_minute`, the bound that keeps a wave under the provider's per-minute cap.
// Every other path into a launch is paced, so every other event is too: an operator's delivery
// carries no launch of its own, and revive reads the exemption from the delivery itself
// (deliverResuming).
func exemptWake(ev Event) bool {
	switch ev := ev.(type) {
	case RequestSpawn:
		return ev.Exempt
	case RequestResume:
		return ev.By == SuspendedByOperator
	case RequestAskedRetry:
		return ev.By == SuspendedByOperator
	}
	return false
}
