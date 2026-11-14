package supervise

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// PodUpdateContract is the daemon contract that introduced ready_for_update (api/version.go's
// 46), the op a worker answers the update round over its pod with. A claim whose registration
// declared a contract below it has no op to answer that round's ask with: the op list is a
// closed enum compiled into the plugin, and a Claude Code role's plugin is its image's own,
// never installed. Such a claim is asked all the same, on its role's own Envoy topic
// (internal/daemon's openPodUpdate), and the round over it ends at its bound whether or not it
// answers, so its pod is replaced like any other. The end of a turn is no answer, at any
// contract: an ask an older daemon queued as a task is answered with the op or not at all.
//
// The stale-plugin lane reads it no longer. That lane asks its worker nothing: a stale claim is
// relaunched onto this daemon's plugin at one paced wake (refreshPlugin, relaunchStale), which
// keeps the pod and the disk, so what op a claim's plugin holds decides nothing there. What a
// claim can answer is the pod round's question alone, and the one reader left makes a decision
// of its own: internal/api serves a caller in a pod whose claims record a contract below this
// one the state without the two update-round keys that contract added (shapeState,
// withoutUpdateRounds).
//
// It lives here rather than beside DaemonAPIVersion because internal/api imports this package.
// internal/api's own test pins it to the contract that brought the op, so an edit of this
// constant alone cannot move it; a merge that renumbers this change moves that test's literal
// and api/version.go's entry with it, which names this constant. Behind is the dangerous
// direction: at 45, with the op at 46, a pod of the released plugin is served the two keys its
// strict schema refuses and reads no state at all.
const PodUpdateContract = 46

// refreshPlugin is the claim's process found to speak a plugin contract this daemon does not: at
// boot, re-adopting a claim whose recorded contract is an earlier daemon's, or none at all (the
// first boot on this release); or, uncharged, a registration that still differs from the daemon's
// but was not itself the registration a relaunch was already made for (staleRegistration, which
// charges that case instead). It is never reached for a claim whose plugin is current — a claim
// still launching is left to its own registration to decide — but it re-checks the comparison
// before doing anything, because the boot's own decision runs later than the post that queued it:
// a registration may have landed, recording the daemon's contract, in between, and such a claim is
// left alone rather than relaunched for a fact already fixed.
//
// The lane has one act, and it asks the claim's worker nothing: the process is replaced onto this
// daemon's plugin (relaunchStale). The lane asked first until this change — a claim whose plugin
// had the ready_for_update op was given an update round of its own and kept running until it
// answered — and that ask cost a second paced wake and a model turn for every live session,
// which is what took INFRA's wake queue 6 h 23 min to drain at the boot of 2026-11-10
// (dispatch://INFRA-2712). What the ask bought is the turn in flight alone: the relaunch keeps
// the pod and the disk under every runtime, so a worker loses what that turn has not pushed and
// no workspace with it.
//
// That turn is not spent either: a claim in one is left running and the relaunch waits for it
// (holdStaleRelaunch, which turnEnded runs back through here), because a worker the lane asks
// nothing is exactly the worker whose unpushed turn this relaunch would throw
// away — the harm the wait exists to end, measured live on 2026-10-30, when an issue's claims
// came up on the previous image. The wait costs nothing: such a claim holds no round open,
// nothing else of its pod waits on it, and the machine already waits a turn out this way for a
// held suspension. It is not unbounded either: the turn ends, or the process dies or is
// suspended, and letGo drops the mark with it. A suspension already held for that same turn
// (holdSuspension) changes none of this: the relaunch waits as it does for any other turn, and
// the turn's end runs the suspension rather than the relaunch (turnEnded), which stops the
// process this lane would have replaced and launches nothing in its place. Nothing is lost by
// waiting — the claim's next resume comes up on this daemon's plugin by itself — and what the
// wait keeps is the turn the relaunch would otherwise have cut, which is the whole point of the
// lane. A held suspension outside a turn, one a launch retained, is relaunched where it stands:
// replaceProcess ends that hold in a suspension rather than a launch (endHeld), and the
// suspension, not this lane, decides what follows. The other caller of relaunchStale is the
// stale registration (staleRegistration), whose claim is registering rather than in a turn.
//
// A claim whose process holds no capability is decided nothing (HoldsCapability): a suspended, a
// failed, a launching or a queued claim has no process to replace, and the one its next launch
// starts speaks this daemon's contract by itself, so a relaunch raised here would start a
// process nobody asked for — an operator's suspension ended by a lane that only meant to change
// a plugin. The controller's claim is no exception: what it is spared is the wait for its turn,
// not the need for a process to replace. This is the one state guard this function has, and it
// is new with the deletion of the round: the branch that opened one tested HoldsCapability
// itself, so until now every claim with no running process took that branch and never the
// relaunch. The daemon's own pass reads the same rule before it posts (refreshStalePlugins in
// internal/daemon), but that pass is not the only way in — turnEnded runs a held relaunch back
// through here (afterTurn), with the claim in whatever state the turn's end left it — so the
// guard belongs here rather than in the pass alone.
//
// The project controller's own claim keeps the immediate relaunch, whatever contract it records,
// and it keeps it mid-turn: its pod holds no issue workspace, so it has nothing to push, which is
// the whole reason a worker's turn is waited out and the controller's is not. Its wake is exempt
// from the pacing too (reserveWake), so the one session an operator reads a running wave through
// comes back ahead of the wave it is reading.
//
// What it does once it decides the claim is still stale is exactly repoint: the turn in flight is
// interrupted and its task kept waiting for the relaunch to send again, a held suspension ends in a
// suspension rather than a relaunch (endHeld), and otherwise the recorded session is resumed on a
// new process (launch). Nothing here is charged — the process did nothing wrong; it simply
// predates this daemon's plugin — and the loop this could otherwise become is closed by the budget
// ready alone resets: a claim whose new process never reaches ready at the generation this starts
// keeps whatever LaunchFailures a true launch failure already charged it, and a registration that
// arrives at that generation still speaking the old contract is not this path's to decide again —
// it is staleRegistration's, which charges it.
//
// relaunchStale can refuse the relaunch it starts — a tree lifecycle cleanup already reserved, a
// moved epoch, a transient store error — so when no suspension is already held (m.held nil, the
// case replaceProcess would launch rather than end in a suspension) that same check runs there
// first, before the Warn or interrupted's turn recovery: a refused attempt returns having
// changed nothing, exactly as stale as it found it, and the orphan sweep's re-post of
// StalePlugin for it (refreshStalePlugins in internal/daemon) is a true no-op until the
// refusal clears. A held suspension is checked nothing there, since replaceProcess ends it in a
// suspension (endHeld) rather than a launch, and endHeld makes no launch-path call to refuse.
// launch rechecks the same thing inside replaceProcess's own call — that second check is the price
// of keeping launch whole, the same price relaunchAfterFailure already pays (budgets.go).
// Interrupting an already-interrupted turn (interrupted) is itself a no-op, the pending delivery
// already marked, so a retried attempt never re-delivers or charges a task twice.
//
// A claim that already holds a wake of any kind is decided nothing at all, whichever branch it
// would reach: it returns here having taken no token and changed nothing. That is what makes the
// sweep's re-posts free — the sweep posts StalePlugin to every stale live claim at every tick and
// skips a claim that already holds a round, one whose process is gone and one no install can
// move (refreshStalePlugins in internal/daemon), so a claim whose relaunch is merely queued is
// posted again every minute of a wave, and a second reserve would charge it the rate again each
// time. The wake that waits is a launch of this same claim, which brings its process up on this
// daemon's plugin by itself, so a relaunch raised over it would replace that wake, take a second
// token, and do what the wake was already going to do. Nothing is lost by returning: the sweep's
// next post decides the claim again as it stands then, once its wake has run.
//
// Every relaunch this lane raises waits under a wake kind of its own (WakePluginRefresh), the
// controller's immediate one included: it is the lane's wake wherever it runs, and a reader of
// the daemon's state tells the refresh of a fleet from the launches that start a tree's work by
// that kind alone (dispatch://INFRA-2712 R4). The other caller of relaunchStale passes the
// launch kind instead, because it is one claim's own launch (staleRegistration).
//
// admitted is whether a credit of the refresh gate was taken for this claim before this decision
// was asked for (RefreshGate, dispatch://INFRA-2712 R1): the daemon's pass takes one for each
// claim it posts and hands it over on the event (StalePlugin.Admitted). The machine holds it
// from here until the decision that raises the relaunch ends with no refresh wake of this
// claim's waiting, which is where every credit of this lane goes back (refreshCredit,
// endRefreshLane). Every return below that raises no relaunch is such an end, because the lane
// gave up nothing by it and the next sweep will decide this claim again: the contract already
// current, a wake already held, no process to replace, and the turn this relaunch waits out,
// whose own end runs the relaunch back through here.
//
// A post that arrives carrying a credit over a machine that already holds one gives the new
// credit straight back to the gate, keeping the flag for the wake the held one was taken for.
// The pass reads the views the machines last published, so a wake parked a moment before it
// counted is one its reading misses (internal/daemon's parkedWakes), and the post that reading
// raises reaches a machine whose refresh is already waiting. The flag is one bit and says only
// that a credit is held, so taking the second onto it would lose it: the decision would return
// at the guard below over the wake already waiting, the end of it would keep one flag for the
// two credits, and that wake's turn would give back one credit for two admissions.
//
// A decision reached with no credit asks the gate itself, at the guard below, which is the
// machine's one ask of it (admitRefresh). Three roads reach that guard holding none, and the
// last of them is no post at all. The controller's claim is posted uncredited because its wake
// is outside the pacing (reserveWake), and the guard spares it rather than asking for it. A
// claim the pass read in a turn is posted uncredited because its relaunch takes no wake now
// (holdStaleRelaunch, R2); the credit such a post does carry is the window where the pass's
// snapshot and this machine disagree — a claim read idle that has started a turn since — and the
// end of this decision gives it back (endRefreshLane). And the relaunch that hold left waiting
// comes back through here at the turn's end, with no credit of its own and the claim in whatever
// state that end left it (afterTurn).
func (m *Machine) refreshPlugin(ctx context.Context, trigger string, admitted bool) error {
	if admitted {
		if m.refreshCredit {
			// This machine's wake already holds a credit, and one flag holds one credit: the
			// post's own goes back to the gate here, where losing it would cost the lane a
			// credit nothing ever waits for (the doc above, RefreshGate.Release).
			m.deps.RefreshGate.Release()
		} else {
			m.refreshCredit = true
		}
	}
	if m.claim.PluginContract == m.deps.PluginContract {
		return nil
	}
	// A claim already queued for a wake takes no second one.
	if m.wake != nil {
		return nil
	}
	if !HoldsCapability(m.claim.State) {
		return nil
	}
	controller := claim.IsController(m.claim.Role, m.claim.Issue, m.claim.Tree)
	if m.claim.State == StateWorking && !controller {
		m.holdStaleRelaunch(trigger)
		return nil
	}
	// Every relaunch of this lane waits under a credit of the gate, so one reached with none asks
	// for its own here, which is the lane's one ask inside a machine; the roads that reach it
	// holding none are named above. Relaunching on no credit would park a wake the gate never
	// counted — eight of them at once where a wave's turns end together, which is twice the rate
	// in the pacing and the one thing R1 promises cannot happen. Refused, the claim is left to
	// the next sweep, which reads it idle and admits it like any other. The ask is made here
	// rather than on those roads because it is made after the three guards above: a claim that
	// is no longer stale, that already holds a wake or that holds no capability burns no credit
	// it would give back at once.
	if !controller && !m.refreshCredit {
		if !m.admitRefresh() {
			m.log.Info("supervise: this claim's stale-plugin relaunch reached the refresh gate with no credit of its own and the gate is full; its relaunch waits for the next sweep",
				"state", m.claim.State, "pluginContract", m.claim.PluginContract, "daemonApiVersion", m.deps.PluginContract, "trigger", trigger)
			return nil
		}
		m.refreshCredit = true
	}
	return m.relaunchStale(ctx, WakePluginRefresh, trigger)
}

// holdStaleRelaunch leaves the relaunch waiting for the agent's turn to end (turnEnded), which
// re-decides it through refreshPlugin as it stands then: by that point the claim is idle, so the
// relaunch runs, and a registration that recorded this daemon's contract meanwhile leaves it
// alone — as does a suspension held for that same turn, which the turn's end runs first and which
// ends the process this relaunch was for. trigger is the pass that found the claim, carried so
// the line the relaunch logs names the pass that asked for it. A sweep finds the same claim every
// tick while its turn runs, so only the first of them logs; the rest replace the trigger and say
// nothing.
//
// A claim in a turn takes no admission of the refresh gate, and the relaunch its turn's end
// runs asks for one where every uncredited relaunch of this lane asks, at refreshPlugin's own
// guard (afterTurn, dispatch://INFRA-2712 R2). A wait of a turn is minutes, and a credit held
// through it is one of the K the lane has spent on a claim that raises nothing with it: at a
// fleet whose stale claims are mostly working, the lane would admit its whole minute of the
// pacing and relaunch nobody.
//
// The daemon's own pass posts such a claim with no credit at all, reading the state its
// snapshot of the claims holds (refreshStalePlugins in internal/daemon). That snapshot and this
// machine can disagree — the claim it read idle can have started a turn by the time the post is
// decided here, and one it read working can have ended its turn — so a post that reaches this
// call carrying a credit is exactly that window, and the end of this decision gives the credit
// straight back, since it leaves no refresh wake waiting (endRefreshLane). The window costs the
// claim one sweep, which is what an imperfect skip costs the pass everywhere else it makes one.
func (m *Machine) holdStaleRelaunch(trigger string) {
	if m.staleRelaunch == nil {
		m.log.Info("supervise: the process speaks another plugin contract; its relaunch waits for the agent's turn to end",
			"incarnation", m.claim.Locator.Incarnation, "pluginContract", m.claim.PluginContract,
			"daemonApiVersion", m.deps.PluginContract, "trigger", trigger)
	}
	m.staleRelaunch = &trigger
}

// relaunchStale replaces the claim's process now, onto this daemon's plugin: the one act of the
// stale-plugin lane, for a claim no turn is running on. Its two callers are refreshPlugin — for a
// stale claim that is idle, for one whose turn has just ended, and for the controller's, whose
// turn is never waited out — and staleRegistration, whose own registration is refused in the same
// decision. trigger names the pass that found the claim ("boot", "sweep", "registration"), for
// the line it logs. refreshPlugin states what the relaunch does and what refuses it.
//
// kind is which of those two callers raised this relaunch, and it is the wake kind the launch
// waits under: WakePluginRefresh for the lane's own pass over a fleet, WakeLaunch for the
// registration, which is one claim's own launch and outside the lane the gate bounds
// (dispatch://INFRA-2712 R4, RefreshGate). It is carried on the relaunch's intent rather than
// passed down the launch path, because the launch path is every launch's and knows nothing of
// the lane that raised this one (relaunchMarked, markPluginRelaunch, reserveWake).
//
// The relaunch marks the claim where its launch starts the process that replaces the stale one
// (relaunchMarked, launchNow), which is what keeps the registration that follows a relaunch that
// ran from being relaunched again for free (staleRegistration). A registration of the old
// process that arrives while that launch still waits its turn meets no mark of this relaunch,
// because none is written until the launch starts; staleRegistration's own guard is what answers
// it, and stops a second relaunch being raised over the wake the first one parked.
func (m *Machine) relaunchStale(ctx context.Context, kind WakeKind, trigger string) error {
	if m.held == nil {
		if err := m.checkLaunch(ctx); err != nil {
			// The relaunch reserves nothing, so the credit it was admitted on goes back at the
			// end of this decision, which raised no wake (endRefreshLane): this return is as far
			// as it gets, and the next sweep raises it again once the refusal clears
			// (refreshStalePlugins in internal/daemon).
			return err
		}
	}
	m.log.Warn("supervise: the process speaks another plugin contract; relaunching it onto the current plugin",
		"incarnation", m.claim.Locator.Incarnation, "pluginContract", m.claim.PluginContract,
		"daemonApiVersion", m.deps.PluginContract, "trigger", trigger)
	return m.relaunchMarked(ctx, kind, "the process on another plugin contract is replaced mid-turn")
}

// relaunchMarked is the stale-plugin relaunch itself, which its one caller has logged what it
// found before it runs (relaunchStale): the claim's process is replaced (replaceProcess), and the
// launch that replaces it is recorded as the stale-plugin relaunch it is, so that the process it
// starts carries the mark of this daemon's contract and its wake waits under the lane's own kind
// (markPluginRelaunch, launchNow, reserveWake). kind is the lane that raised it, which
// relaunchStale states; interruption is the sentence the turn this cuts off is recovered with
// (interrupted).
//
// The invariant the mark carries is that it names a relaunch whose launch has run, which is what
// lets staleRegistration read a registration that still differs after it as the second stale one
// in a row and charge the claim toward failed. That is why nothing of the claim is written here.
// This call can end with no launch at all — the take-back the replacement makes first can fail
// (interrupted), the lifecycle recheck every launch makes can refuse it (launchAfter's
// checkLaunch), and a held suspension ends it in a suspension instead (endHeld) — and the launch
// it does raise can wait tens of minutes for its turn in the wake pacing and be abandoned at
// that turn (reserveWake, runWake). Each of those leaves the claim's process where it was, or
// suspended, with nothing relaunched and the mark exactly as it found it, and the next
// stale-plugin pass raises the relaunch again (refreshPlugin, the daemon's refreshStalePlugins).
// The mark costs no write of its own either: the launch that commits it writes the claim as it
// starts (launchNow).
//
// The intent names one launch and no other: a later launch of this machine, raised by something
// else, marks nothing for this relaunch. The launch that took it holds it by then — the gate
// takes it onto the wake it parks, and launchNow consumes it where no gate held it
// (takePluginRelaunch) — and an intent no launch took is dropped where every decision ends, so
// this function drops none of its own (endRefreshLane).
//
// One relaunch ends with no mark: the one a held suspension ends in a suspension (endHeld). The
// resume of that claim raises a launch of its own from the top, where the operator or the rule
// asks for it (revive, launchAfter), rather than carrying this one on, and it can come hours
// later or after a restart, which no intent of this machine outlives. So that claim comes back
// unmarked, and a resumed process that still speaks another contract is relaunched once more
// uncharged. That is one relaunch, not a loop: the relaunch that follows marks the claim where
// it starts its process, and the registration after it is the second in a row.
func (m *Machine) relaunchMarked(ctx context.Context, kind WakeKind, interruption string) error {
	m.markPluginRelaunch(kind)
	_, err := m.replaceProcess(ctx, interruption, ordinaryEnd)
	return err
}

// pluginIntent is one stale-plugin relaunch as the machine carries it between the decision that
// raised it and the launch that runs it: the contract the relaunch is made for, which that
// launch writes to PluginRelaunchFor as it starts the process replacing the stale one
// (commitPluginRelaunch), and the wake kind that launch is reserved under, which is the lane
// that raised it (relaunchStale, reserveWake).
//
// The two are one value rather than two fields of the machine because they are one fact — this
// launch is a stale-plugin relaunch of this lane, for this contract — and because they travel
// the same road together: both are taken onto the wake the gate parks, handed back at its turn
// and dropped with a relaunch that is abandoned (wake.pluginMark, holdPluginRelaunch). Two
// fields could be left half-set by a path that remembered one of them, and a launch half-marked
// is either a refresh the daemon's state reports as ordinary work or a mark naming no contract.
type pluginIntent struct {
	contract int
	kind     WakeKind
}

// markPluginRelaunch records that the launch this decision is raising is a stale-plugin relaunch
// for this daemon's contract, waiting under kind. It writes nothing of the claim:
// PluginRelaunchFor is written where that launch starts the process the stale one is replaced
// with (launchNow, commitPluginRelaunch), so the mark names a relaunch that has run rather than
// one that was decided. That is what leaves nothing to put back when a relaunch is abandoned —
// by a refusal at its turn, by a take-back the store refused, by a suspension that ended it, or
// by a second relaunch raised over the wake it parked.
func (m *Machine) markPluginRelaunch(kind WakeKind) {
	m.pluginRelaunch = &pluginIntent{contract: m.deps.PluginContract, kind: kind}
}

// takePluginRelaunch hands the intent on and leaves the machine none, so that exactly one launch
// can commit one relaunch's mark: the wake gate takes it onto the wake it parks (reserveWake),
// and the launch commits it as it starts (commitPluginRelaunch). It answers nil for every launch
// no stale-plugin relaunch raised. The gate reads the kind off the intent before this, without
// taking it, since a launch that runs where it is raised leaves the intent standing for launchNow
// to consume.
func (m *Machine) takePluginRelaunch() *pluginIntent {
	intent := m.pluginRelaunch
	m.pluginRelaunch = nil
	return intent
}

// holdPluginRelaunch puts the intent a parked wake carries back on the machine, for the launch
// that turn is about to run to commit (runWake, commitPluginRelaunch). intent is nil for every
// wake no stale-plugin relaunch raised, and the machine is then left holding none, which is what
// it held before the turn.
func (m *Machine) holdPluginRelaunch(intent *pluginIntent) {
	m.pluginRelaunch = intent
}

// commitPluginRelaunch writes the mark of the stale-plugin relaunch whose launch is starting now:
// PluginRelaunchFor names the contract that relaunch was made for, which is what lets
// staleRegistration read the next registration that still differs as the second stale one in a
// row and charge it toward failed. Its one caller is launchNow, before the write that records the
// launch, so the mark reaches the store with that write, which comes before the runtime starts
// the process: a restart between the two, or a start the runtime refuses, can leave a mark whose
// launch was recorded but whose process never ran. It writes nothing for a launch no stale-plugin
// relaunch raised. The intent's other half, the kind, was spent at the gate this launch passed
// and is written nowhere of the claim (reserveWake).
func (m *Machine) commitPluginRelaunch() {
	if intent := m.takePluginRelaunch(); intent != nil {
		m.claim.PluginRelaunchFor = intent.contract
	}
}

// refreshStalePlugin is the StalePlugin event's row, for the four states a claim's relaunched
// process can run in: the event's own Trigger (boot or sweep) names the pass that posted it, and
// its Admitted says whether that pass took a credit of the refresh gate for this claim, which
// the machine then holds for the relaunch it is about to raise (refreshPlugin, refreshCredit).
func refreshStalePlugin(m *Machine, ctx context.Context, ev Event) error {
	stale := ev.(StalePlugin)
	return m.refreshPlugin(ctx, stale.Trigger, stale.Admitted)
}

// refreshOverturned is the fact the lane's relaunch was raised for, read again at the wake's own
// turn: a refresh wake can wait tens of minutes in a wave, and what made the claim stale can be
// gone by then. Two things end it, and each leaves the claim better off than the relaunch would.
// Its recorded plugin contract is this daemon's: a registration landed inside the wait — the
// process reconnected and registered current, or a relaunch of another path got there first — so
// there is nothing left to replace. Or the claim has started a turn: the worker is working, and
// a relaunch now is exactly the cut turn the whole lane is built to avoid (holdStaleRelaunch,
// dispatch://INFRA-2712 R2), where the next sweep finds the claim idle and relaunches it then.
//
// Its one caller is runWake, ahead of the launch's own lifecycle recheck and the turn take-back
// (StartsProcess), which is the only order in which the claim still holds what those two would
// spend. A launch wake makes no such re-read, and until this gate none waited long enough to
// need one: the check is what the deleted plugin round made at its own wake.
func (m *Machine) refreshOverturned() bool {
	return m.claim.PluginContract == m.deps.PluginContract || m.claim.State == StateWorking
}

// staleRegistration is what register and reregister run once they have recorded the plugin
// contract the request declared, whatever it is: a registration whose contract still differs from
// the daemon's is relaunched onto the current one, and the registration itself is refused with the
// sentence that tells the plugin why (claim.StalePluginContract) — the agent that just registered
// is not the one the relaunch starts, and pi-legion exits on the 4xx, so its own log then says why.
// The relaunch runs first and is charged only when it is the second relaunch in a row still to find
// the daemon's contract: the claim remembers, in PluginRelaunchFor, the contract its last relaunch
// was made for, so a registration that still differs while PluginRelaunchFor already names this
// daemon's contract means the plugin or the image the relaunch put the process on is itself behind
// this daemon, and relaunching it again for free would loop; relaunchAfterFailure charges it
// instead and ends it in a worker-died once the launch budget runs out. Every other stale
// registration — PluginRelaunchFor unset, or made for a contract this daemon has since moved past —
// is relaunchStale's uncharged relaunch. This path asks the worker nothing, as the lane's own
// pass asks nothing (refreshPlugin): the agent that just registered is refused and its process
// exits on that refusal, so a question put to it here would wait for an answer no running turn
// could give.
//
// A registration that already speaks this daemon's contract ends a plugin round left over the
// claim (endCurrentPluginRound): no daemon on this release opens one, and the claim of a round an
// earlier daemon opened needs no replacement, since the process that registered speaks this
// daemon's plugin. Leaving the round open would hold the claim's next task behind it until a
// sweep ended it.
//
// A daemon with nothing to relaunch onto (PluginRelaunchable false, LEGION-164's degraded
// Kubernetes mode) relaunches nothing and refuses nothing: the registration stands, accepted, and
// one Error line records the gap for the operator to close with runtime.kubernetes.plugins.
//
// A registration no relaunch can take past the daemon's contract is accepted and charged
// nothing, exactly as that one is: a claim whose launch installs no plugin (Deps.NoPluginInstall
// — a Claude Code role's plugin is its pod image's own claude-envoy), whatever contract it
// records. Relaunching it would keep the pod, bring the same plugin back, and land here again
// with PluginRelaunchFor already naming this daemon, which is the charged path: the claim would
// walk its launch budget down to failed for a plugin no launch of this deployment can change.
// The contract it records decides nothing here — a claim at this release's own contract is as
// unreachable by an install as one below it, and the next bump would walk exactly that claim to
// failed — so the rule is the install alone.
//
// What replaces such a claim's plugin is a new image, which the update round over its pod is
// opened for: it is asked like every other worker, answers with the op or not at all, and the
// round's replacement brings the image, at the latest at the round's bound (internal/daemon's
// refreshStalePlugins, which posts no relaunch for it either).
//
// A registration that arrives while a launch the pacing holds waits over the process it comes
// from relaunches nothing and is charged nothing. PluginRelaunchFor is written where the
// relaunch starts the process that replaces the stale one (launchNow, commitPluginRelaunch),
// never where that relaunch is decided, so a relaunch the pacing holds over the live old
// process for tens of minutes (reserveWake, parkWake) has marked nothing yet, and the charged
// branch below reads a mark that names a relaunch that did run. What the guard adds is the rest
// of the answer: without it a process that reconnects and re-registers on the old contract
// inside that wait would be relaunched here, which raises a second relaunch over the one still
// waiting, replaces its wake and takes a second token for the launch that wake was already
// going to make (reserveWake). The same guard keeps a claim already marked by a relaunch that ran
// from being charged twice: when that relaunch's process registers stale, the charged relaunch
// parks a wake with the mark still set, and a re-registration of that process inside the wait
// would otherwise meet the mark and be charged a second launch failure for the one registration
// the claim was already charged for. The registration is still refused, as every stale one is,
// so the agent exits rather than take work on a contract this daemon does not speak, and the
// launch's turn replaces its process (wakeNow). It is the guard judge makes of every observation
// of that same process, for the one report that reaches a decision of its own instead. The
// guard reads whether a wake that starts a process waits, whichever lane parked it
// (launchWaits, StartsProcess): the refresh lane's relaunch of this same claim is exactly the
// launch this registration must not raise a second one over.
//
// The relaunch this path raises keeps the launch kind, where the refresh lane's carries one of
// its own (WakePluginRefresh, refreshPlugin). It is this claim's own launch, answering the
// registration in front of it rather than working through a fleet, and a reader of the daemon's
// state counting what the refresh has left to do must not count it (dispatch://INFRA-2712 R4).
// The charged path beside it raises no stale-plugin intent at all, so its launch takes the kind
// every launch takes (relaunchAfterFailure, launchAfter).
func (m *Machine) staleRegistration(ctx context.Context, got int) error {
	if got == m.deps.PluginContract {
		return m.endCurrentPluginRound(ctx)
	}
	if m.deps.NoPluginInstall != nil && m.deps.NoPluginInstall(m.claim) {
		m.log.Warn("supervise: this claim's launch installs no plugin, so no relaunch can move its contract; the registration stands and the update round over its pod brings the new image",
			"pluginContract", got, "daemonApiVersion", m.deps.PluginContract)
		return nil
	}
	if !m.deps.PluginRelaunchable {
		m.log.Error(NoPluginSourceMessage,
			"incarnation", m.claim.Locator.Incarnation, "pluginContract", got, "daemonApiVersion", m.deps.PluginContract)
		return nil
	}
	if m.launchWaits() {
		m.log.Info("supervise: a registration on another plugin contract reached a claim whose relaunch waits its turn; that turn replaces this process and nothing is relaunched or charged here",
			"pluginContract", got, "daemonApiVersion", m.deps.PluginContract, "due", m.wake.at)
		return claim.StalePluginContract(got, m.deps.PluginContract)
	}
	var err error
	if m.claim.PluginRelaunchFor == m.deps.PluginContract {
		err = m.relaunchAfterFailure(ctx, CauseStalePlugin)
	} else {
		err = m.relaunchStale(ctx, WakeLaunch, "registration")
	}
	if err != nil {
		return err
	}
	return claim.StalePluginContract(got, m.deps.PluginContract)
}

// StaleRefusesCalls is the stale-plugin refusal's own rule, held in one place because two
// packages decide by it: whether a daemon speaking daemonContract would refuse this claim's own
// `legion` calls for the contract its process speaks. pluginRelaunchable is whether that daemon
// can relaunch onto a plugin at all (Deps.PluginRelaunchable, api.Options.PluginRelaunchable)
// and noPluginInstall whether this claim's own launch installs one (Deps.NoPluginInstall,
// api.Options.NoPluginInstall); nil is every launch installing a plugin, which is what a tmux
// daemon and a harness that builds neither hold.
//
// The terms are the three a stale claim is refused by: the recorded contract is not this
// daemon's, the claim is in no update round — a round's whole point is that the daemon serves
// the claim on its contract of before the round until the round ends — and a relaunch onto this
// daemon's plugin can reach it at all. Two claims no relaunch can ever bring forward are
// therefore served rather than refused. A claim whose launch installs no plugin — a Claude Code
// role's in a pod, whose plugin is the image's own claude-envoy — is served on whatever
// contract it records, at this release's own or below it, because nothing but a new image moves
// that plugin; and on a daemon that can relaunch onto no plugin (LEGION-164's degraded
// Kubernetes mode) every stale claim is served, since no relaunch is coming for any of them.
// Refused there, such a worker would wait on a replacement process that is never started.
//
// Each caller adds the term that is its own, and neither may copy the other's. internal/api's
// staleClaim adds that the claim is in no turn, because a worker in a turn is served so that it
// can push what that turn holds before the relaunch its end releases (holdStaleRelaunch);
// staleRefusesClaim below adds nothing, because the one place it is asked has answered that
// term already (sendPending). Holding the three shared terms here is what keeps the two from
// drifting apart: drift one way and a task is held back for a claim the API serves, which waits
// for a relaunch nothing is making; drift the other and a task reaches a claim whose every
// `legion` call is refused, which buys a model turn the worker can do nothing in and puts the
// claim in a turn, postponing the very relaunch it is waiting for.
func StaleRefusesCalls(c Claim, daemonContract int, pluginRelaunchable bool, noPluginInstall func(Claim) bool) bool {
	if noPluginInstall != nil && noPluginInstall(c) {
		return false
	}
	return pluginRelaunchable && c.PluginContract != daemonContract && c.UpdateRound == ""
}

// staleRefusesClaim is whether this daemon would refuse the claim's own `legion` calls because
// its plugin is stale, and it is what holds a pending delivery back (sendPending): a task handed
// to a claim whose every `legion` call is refused buys a model turn in which the worker can read
// the task and do nothing with it — no grant, no handoff, no phase op — and the turn puts the
// claim in a turn, which postpones the one thing the claim is waiting for, the relaunch this
// lane holds until that turn ends (refreshPlugin, holdStaleRelaunch).
//
// It is the shared rule over the Deps this machine already holds (StaleRefusesCalls, which
// states the rule and what each caller adds to it), rather than a second copy of it or a
// question put to internal/api, which imports this package and not the other way round.
// staleClaim's fourth term, that the claim is in no turn, is answered before this is asked:
// sendPending hands nothing to a claim that is not ready or idle, so every claim that reaches
// this is out of a turn, which is the claim staleClaim refuses.
func (m *Machine) staleRefusesClaim() bool {
	return StaleRefusesCalls(m.claim, m.deps.PluginContract, m.deps.PluginRelaunchable, m.deps.NoPluginInstall)
}

// NoPluginSourceMessage is logged whenever a daemon that can relaunch nothing onto a plugin
// (supervise.Deps.PluginRelaunchable false, LEGION-164's degraded Kubernetes mode) finds a stale
// process instead of relaunching it: staleRegistration here, refreshStalePlugins
// (internal/daemon/daemon.go) at boot.
const NoPluginSourceMessage = "supervise: the process speaks another plugin contract, and this daemon knows no plugin versions to relaunch it onto; set runtime.kubernetes.plugins"
