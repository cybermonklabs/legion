package supervise

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// The tree lifecycle (internal/treelifecycle) is the store's barrier between a tree's runnable
// work and its cleanup, under every runtime. A claim is bound to its tree's open epoch when it is
// admitted (Store.AdmitClaim) and every launch rechecks that binding (Store.CheckLaunch) before it
// persists StateLaunching or calls the runtime. A reserved cleanup answers either with
// treelifecycle.ErrCleanupReserved, a wait that charges no budget and changes nothing but a claim
// whose process a retirement already stopped (relaunchRefused); the start that met it retries once
// the cleanup confirmed and a fresh admission opened the next epoch.

// revive binds an explicitly spawned, resumed or retried claim to the open tree epoch before
// launching. In-lifetime recovery only rechecks the bound epoch (checkLaunch), so an old launch
// cannot cross a cleanup confirmation into a later admission. exempt is whether the request that
// asked for it is one the wake pacing never makes wait (exemptWake); every caller reads it from
// its own event, so a request that carries no such field is paced. place is the place the
// request's caller already took for this launch in the pacing (RequestSpawn.Place), nil for every
// request that took none (launchAfter).
//
// admitted, where a caller gives one, runs once the admission has bound the claim and before
// anything else of the start: it is where a caller puts what it may change only for a claim
// that is being run again, since an admission the lifecycle refuses leaves the claim exactly as
// it was (retry, whose failure record and budgets go there).
//
// The claims it starts — spawned, resumed, retried, a suspended claim given a task — record no
// process, so its launch lets none go and records nothing of one (noEnd). Each was raised by a
// request, which the pod's start gate puts first among the starts waiting in the claim's pod
// (launchAfter's requested, podQueue.next).
//
// The admission writes the claim as memory holds it, normalized as every claim write is
// (stored), so one that lands is the repair of any claim write the machine owed
// (settleOwedWrite), unless a pause's stop is owed, which only its own transaction records
// (putStops). Its failure is put to the rule (writeFailed), but for the tree lifecycle's own
// answers, which keep their handling: a reserved cleanup, a lifecycle that is absent, its row
// missing among them, or one whose cleanup is confirmed is returned as it is, the claim left
// exactly as it was. A failure the rule owes owes nothing: nothing of this decision has reached
// memory before the admission lands (admitted runs after it), so memory is what the row holds.
//
// A revive in the window after a refused claim write's read (D5 of dispatch://INFRA-4099's plan)
// makes no write and is made again at the window's end (reviveAfterWindow).
func (m *Machine) revive(ctx context.Context, exempt bool, place *wakepace.Place, admitted func()) error {
	if until, waits := m.writesWait(); waits {
		m.reviveAfterWindow(until, exempt, admitted)
		return nil
	}
	// The admission writes the claim row alone, so a take-back the claim must carry goes first
	// (carryPending, writeCarried).
	if err := m.writeCarried(ctx); err != nil {
		return err
	}
	c := m.stored()
	// A refusal this decision met stands for the admission too, which reaches no store (D2 of
	// dispatch://INFRA-4099's plan).
	bound, err := Claim{}, m.withheld()
	if err == nil {
		bound, err = m.deps.Store.AdmitClaim(ctx, c)
	}
	switch {
	case errors.Is(err, treelifecycle.ErrCleanupReserved), errors.Is(err, treelifecycle.ErrLifecycleAbsent),
		errors.Is(err, treelifecycle.ErrLifecycleClosed):
		return err
	case err != nil:
		m.recorded(ctx, err)
		return m.writeFailed("AdmitClaim", err)
	}
	m.rowState = bound.State
	if len(m.owedStops()) == 0 {
		m.settleOwedWrite(ctx, "AdmitClaim")
	}
	m.recorded(ctx, nil)
	m.claim.TreeEpoch = bound.TreeEpoch
	if admitted != nil {
		admitted()
	}
	return m.launchAfter(ctx, false, exempt, true, place, noEnd)
}

// reviveAfterWindow is a revive that met the window after a refused claim write's read (D5 of
// dispatch://INFRA-4099's plan). Its admission reaches no store before until, the window's end, and
// a launch with no admission starts nothing, so the revive is made again then, as the request
// asked it (exempt, admitted), over the claim as the request found it: the state is put back
// first, so the admission writes what the request's own would have written, and one the tree
// lifecycle refuses leaves the claim exactly as it was (revive). Until then the claim waits
// launching with no process (relaunchLater), as a launch whose launching write the window held
// waits (writeTry): a second request finds a launch under way, as it would with no window, a stop
// ends the wait (letGo), a suspension is held and ends it at its turn (holdSuspendUntilReady,
// relaunchAgain), and a restart, which loses the wait with the rest of memory, takes the claim up
// from its row. The place in the pacing a request carried is given up as its decision ends
// (leaveUntaken), and the launch takes a place of its own at its turn.
//
// The request is answered with no error, since its launch is the machine's to make now. A wait
// would leave the launch to the requester, and a request the daemon posts to the machine has none
// who asks again: the supervisor logs a decision's error and drops the event (internal/daemon's
// decisionFailed), which is what the boot's spawn of a released launch met. A launching write the
// window holds back needs nothing of this: its launch is tried again at the probe interval from
// that try (writeTry, relaunchAtProbe), which is past the window's end already.
func (m *Machine) reviveAfterWindow(until time.Time, exempt bool, admitted func()) {
	found := m.claim.State
	m.log.Info("supervise: the launch waits out the window after a refused claim write; it is made at the window's end",
		"state", found, "writesWaitUntil", until)
	m.relaunchLater(until.Sub(m.deps.Clock.Now()), WakeLaunch, func(ctx context.Context) error {
		m.claim.State = found
		return m.revive(ctx, exempt, nil, admitted)
	})
}

// checkLaunch is the lifecycle recheck every launch makes first: a claim whose tree cleanup is
// reserved, or whose bound epoch is no longer the open one, starts nothing and changes nothing, so
// a relaunch refused here charges no budget.
func (m *Machine) checkLaunch(ctx context.Context) error {
	return m.deps.Store.CheckLaunch(ctx, m.claim)
}

// relaunchRefused is a relaunch checkLaunch refused, relaunch the call that made it
// (relaunchAfterFailure, launchAfter, runWake) and so the one to try again, and kind the wake kind
// the refused launch waits under, which the retry records with it (relaunchLater): the lane's own
// kind for a launch of the stale-plugin lane, WakeLaunch for every other. A claim that records
// its process, or runs none — a spawn, a resume, a retry — is left exactly as it was: the error
// goes back to whoever asked for it, and this writes nothing, arms nothing and starts nothing.
//
// A claim that records its process is left stored working over it where a death or a
// displacement raised the relaunch, and nothing of the claim is written to say so: this
// refusal is one of the windows Claim.ProcessGone is read for, and what that field is and
// what reads it is written there. It is no record this refusal makes, and none it clears —
// the report that raised the relaunch made it, and the next report of the same process, the
// one that raises the relaunch again, is what changes it.
//
// The credit of the refresh gate a machine may hold goes back at the end of the decision this
// refusal belongs to, which is where every credit of this lane goes back (endRefreshLane,
// dispatch://INFRA-2712 R1), unless a launch of the lane is still owed there (laneLaunchOwed).
// This is the refusal of every launch there is, wherever it came from — the launch path's own
// pre-check, the recheck the wake's turn makes and the relaunch a failure raises (launchAfter,
// runWake, relaunchAfterFailure) — and a refused launch raises no wake. So a refused launch of
// the stale-plugin lane keeps its credit only through the retry this arms, which records the
// lane's kind and owes the launch that credit was admitted on; a refusal that arms no retry, or
// that suspends the claim, owes the lane nothing, and the credit goes back as the decision ends.
//
// No refresh wake can be waiting on the machine when this runs, and the refusal parks none, so
// the retry is the one thing the credit of a refused launch of this lane can be held for: a
// credit is taken over a claim that holds no wake at all (refreshPlugin's own guard), the turn of
// a parked wake takes it off the machine before anything is rechecked (wakeNow,
// runParkedLaunchNow), and no decision of this package raises a launch over one still waiting
// (endRefreshLane lists them).
//
// Why the retry such a refusal may arm carries the intent, and what holding the credit through
// it costs the gate, is where retryHolding and the gate state it (retryHolding, RefreshGate): the
// launch that retry makes then waits under the lane that raised it rather than under the launch
// kind every other launch takes (reserveWake, dispatch://INFRA-2712 R4). A retry armed here with
// the bare relaunch would park under `launch`, where the reader this kind exists for counts no
// refresh of a claim the lane is still working through, and where the machine holds no credit for
// it, so a relaunch of the lane would wait in the pacing outside the gate's bound
// (api.State.ClaimsToRefresh, laneLaunchOwed).
//
// Two roads reach this line with an intent on the machine, and each is a retry of this lane
// refused a second time: the retry a failed write of StateLaunching or an unanswered hold of the
// session armed (launchTries, relaunchAtProbe), and the retry the refused turn of this lane's own
// parked wake armed (runWake). Both put the intent back as they run and raise the same launch
// anew from the top, whose own lifecycle check is refused here (launchAfter, retryHolding).
//
// The other two callers hold no intent where they refuse, so the wrapping is inert for them.
// The wake's turn took the intent off the wake it was parked with and puts it back only past
// both steps, which is past this refusal, so that caller wraps the intent its own wake carried
// (runWake, holdPluginRelaunch). A relaunch after a launch failure is no relaunch of this lane
// at all: the charged road a stale registration takes raises none, and nothing else of that
// path does either (relaunchAfterFailure, staleRegistration).
//
// What one refused relaunch leaves behind is put right by the caller that left it, never here.
// The one such thing is the watch a park took off the process the relaunch was to replace, and
// it belongs to the launch the wake pacing held: that turn is where it is armed again (runWake,
// watchAgain). This is the refusal of every launch there is — launchAfter's own pre-check, a
// relaunch after a failure, a refused retry or resume over a claim that runs no process — and
// those launches park nothing, so arming a timer for one would arm what the fence has just
// fired.
//
// No plugin mark is touched here, by this refusal or by any other. A stale-plugin relaunch
// writes PluginRelaunchFor where its launch starts the process that replaces the stale one
// (launchNow, commitPluginRelaunch), so a refused relaunch has written none, and the mark the
// claim carries names the relaunch that did run — which is what the charged second stale
// registration in a row rests on (staleRegistration).
//
// A live claim that records none has already let its process go: a retirement's relaunch whose
// stop already ended the process (the registration deadline and a prompt retirement check before
// they stop, so the check failed while the stop waited out its grace), or a launch whose write
// failed, waiting (relaunchLater). A claim left there would have nothing watching or relaunching
// it. A reserved cleanup suspends it for the daemon, its session kept, for the tree's close to
// retire. Any other failure is not the tree's — a store that did not answer — and ends nothing:
// the relaunch waits (relaunchLater), as a stop that fails does.
func (m *Machine) relaunchRefused(ctx context.Context, kind WakeKind, relaunch func(context.Context) error, err error) error {
	if m.claim.Locator != nil || !slices.Contains(live, m.claim.State) {
		return err
	}
	if errors.Is(err, treelifecycle.ErrCleanupReserved) {
		m.log.Warn("supervise: the tree's cleanup is reserved and the retired process is stopped; suspending the claim for the tree's close", "error", err)
		return errors.Join(err, m.suspended(ctx, SuspendedByDaemon, nil, stopOwed))
	}
	m.log.Warn("supervise: the lifecycle check of the retired process's relaunch failed; trying it again at the probe interval", "error", err)
	m.relaunchLater(m.deps.Timeouts.Probe, kind, m.retryHolding(m.pluginRelaunch, relaunch))
	return errors.Join(err, m.persist(ctx))
}

// watchAgain arms again what watches the process the claim records, where that is not armed: the
// timers the claim's state calls for, which are the ones a restart reading this claim back from
// the store would arm for it (NewMachine), and the bound an outstanding prompt waits for its turn
// on. Its one caller is the refusal a parked launch's turn meets (runWake), over a claim that
// keeps its process with the launch that was to replace it gone: the park disarmed the boot
// interval, the registration deadline, the probe interval and the turn bound of that process
// (waitOnMachine), the claim keeps it and the wake is spent, so a booting process whose agent
// never registers would meet no deadline and an outstanding prompt would wait for its turn on
// nothing. The next paragraph gives the rule for each of an outstanding prompt's two states.
//
// A claim still coming up is watched by its boot interval and its registration deadline, a
// registered one by the deadline alone, and a claim whose agent is ready by neither: a live
// session is watched by the runtime's sweep (observe) and by its own connection, and the probe
// interval is an uncertain answer's retry rather than a watch of the claim's — the sweep asks
// again at its own pass. The turn bound goes with them for a prompt of the task outstanding with
// no turn started on it (Machine.outstanding), in either of its states: one Oh My Pi acknowledged
// (acked), or one held, whose frame the daemon wrote and whose reply did not come
// (keepHeldPrompt). It is armed for the same RPC wait that prompt was given, so one the agent never
// turned on still ends rather than waiting for ever: an acknowledged one is charged, and a held one
// is sent again under its id, charged nothing (noTurn).
//
// A timer already armed keeps the moment it already holds: the park takes only the four timers
// above, so whatever the claim still holds is a watch nothing of this launch disturbed, and
// arming again over it would hand the process a fresh boot interval and a fresh deadline at
// every refusal.
func (m *Machine) watchAgain() {
	if m.claim.Locator == nil {
		return
	}
	armed := func(kind TimerKind) bool {
		_, ok := m.timers[kind]
		return ok
	}
	switch m.claim.State {
	case StateLaunching, StateShimConnected:
		if !armed(TimerBoot) {
			m.arm(TimerBoot, m.deps.Timeouts.Boot, "")
		}
		if !armed(TimerRegistration) {
			m.armRegistration()
		}
	case StateRegistered:
		if !armed(TimerRegistration) {
			m.armRegistration()
		}
	}
	if p := m.claim.Pending; p != nil && m.outstandingPrompt(p.ID) && p.ConfirmedAt.IsZero() && !armed(TimerTurn) {
		m.arm(TimerTurn, m.deps.Timeouts.RPC, p.ID)
	}
}

// waitingRelaunch is the relaunch relaunchLater left waiting (Machine.relaunch): retry is the
// call TimerRelaunch makes again, and kind the wake kind relaunchLater's caller recorded for the
// launch it retries, which is the stale-plugin lane's own for a retry of that lane's launch and
// WakeLaunch for every other. The two are one value so that one assignment sets both and one
// clears both, and neither is left standing without the other. That is all the one value holds:
// kind is the caller's argument, not read off the intent retry restores, so the two agree only
// because each caller of relaunchLater passes that intent's kind (relaunchLater).
type waitingRelaunch struct {
	retry func(context.Context) error
	kind  WakeKind
}

// relaunchLater leaves a claim whose process is already let go waiting launching with no process,
// its task kept, and tries relaunch again after wait (TimerRelaunch, relaunchAgain): a relaunch
// whose lifecycle check the store did not answer (relaunchRefused), or a launch whose write failed
// or whose hold of the session the store did not answer (relaunchAtProbe), each at the probe
// interval, a start the session store could not answer or that waits for its pod's update
// round, each at its own minute (startWaitsForStore, startWaitsForPodRound), a row the rebuild
// after a refused claim write found naming a launch no process carries, at the probe interval
// (rebuild), and a request's revive the window after a refused claim write held back, at the
// window's end (reviveAfterWindow). Nothing else would relaunch such a claim before a restart,
// which takes it up from what the store last holds.
//
// kind is the wake kind of the launch relaunch retries, recorded with it. A retry of a launch of
// the stale-plugin lane is that lane's launch still owed, so the machine keeps the credit of the
// refresh gate that launch was admitted on until the retry is dropped, or runs and leaves nothing
// of that launch owed (laneLaunchOwed, endRefreshLane). Nothing here reads kind off relaunch, and
// the intent relaunch restores is what decides the kind the launch it retries parks under
// (retryHolding, reserveWake), so each caller must pass the kind of that intent: the kind it
// waits under (pluginIntent.waitsUnder), which is WakeLaunch for a relaunch that restores none. A
// caller that passed another would park a wake of the lane with its credit given back, or hold a
// credit for a launch that is not the lane's. These are the callers, and what each passes:
//
//   - relaunchRefused records the kind its own callers hand it: launchAfter and
//     relaunchAfterFailure read it off the machine's intent, which the retry relaunchRefused arms
//     restores, and runWake passes its wake's kind, read where that wake was made off the intent
//     the wake's retry restores (reserveWake, heldTryWake, retryHolding).
//   - relaunchAtProbe reads it off the intent of the try's launch, the one its retry restores
//     (launchFromTop).
//   - startWaitsForStore and startWaitsForPodRound pass WakeLaunch whatever launch they retry,
//     and their retries restore no intent: each waits on a fault outside the daemon or on a pod's
//     round, which can take hours, and a credit held that long would stop the lane.
//   - rebuild and reviveAfterWindow pass WakeLaunch: the rebuild has given back the plugin intent
//     with everything else the refused decision held (giveBackHeld), and a request's revive
//     restores none.
func (m *Machine) relaunchLater(wait time.Duration, kind WakeKind, relaunch func(context.Context) error) {
	m.claim.State = StateLaunching
	m.relaunch = &waitingRelaunch{retry: relaunch, kind: kind}
	m.arm(TimerRelaunch, wait, "")
}

// relaunchAgain is the relaunch relaunchLater left waiting (TimerRelaunch). A suspension held
// meanwhile ends here, with no process left to stop, rather than in a relaunch — a stop that meets
// the relaunch it ends, which a project pause's stop owes the role (suspendedFor); otherwise the
// relaunch is tried again as it was first made, charged as that call charges it. Either way the
// retry is taken off the machine first, so the end of this decision holds the credit of a launch
// of the stale-plugin lane only for what this retry leaves owed: the wake its launch leaves on the
// machine, in the pacing or held by a gate, or a retry of its own (laneLaunchOwed).
func relaunchAgain(m *Machine, ctx context.Context, _ Event) error {
	relaunch := m.relaunch
	m.relaunch = nil
	if m.held != nil {
		return m.suspendedFor(ctx, *m.held, true, stopOwed)
	}
	return relaunch.retry(ctx)
}

// RunsNothing is whether the claim is in a state with no process of its own (RunsNothingStates):
// only such a claim may move to another tree.
func (c Claim) RunsNothing() bool { return slices.Contains(runsNothing, c.State) }

// Gone is whether the claim is suspended, failed or retired (GoneStates): its process has been
// let go, so a project's pause has nothing of it to stop.
func (c Claim) Gone() bool { return IsGone(c.State) }

// Retree re-points a claim to tree, the tree its issue belongs to now: a child of a closed tree
// re-admitted as a root of its own keeps its roles' claims, which still name the tree it left. The
// claim drops its old tree's epoch, so its next start binds the new tree's lifecycle and launches
// in the new tree's resources, and keeps its session: the session is the issue's, on the host under
// tmux, and under a sandbox in the runtime's session database (runtime.kubernetes.session_store
// postgres), which every tree's pods read, so it resumes in the new tree while the old tree
// lingers, whatever the old tree's cleanup takes. Only a claim that runs nothing is re-pointed; one
// whose process still runs in the old tree is a wait (wait.ErrWaiting): the new tree's start
// suspends it first, keeping its session, and waits for that suspension to land (the daemon's
// outbox, stopElsewhere).
func (m *Machine) Retree(ctx context.Context, tree string) error {
	m.mu.Lock()
	defer m.unlock()
	if err := m.rereadWait(); err != nil {
		return err
	}
	if m.claim.Tree == tree {
		return nil
	}
	if !m.claim.RunsNothing() {
		return wait.Errorf("re-point claim %s from tree %s to %s: it is %s there", m.claim.Token, m.claim.Tree, tree, m.claim.State)
	}
	m.log.Info("supervise: the claim's issue moved to another tree", "from", m.claim.Tree, "to", tree)
	m.claim.Tree, m.claim.TreeEpoch = tree, 0
	return m.persist(ctx)
}
