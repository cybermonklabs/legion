package supervise

import (
	"context"
	"errors"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// repoint is the claim's process found alive holding an address a process launched now is not
// handed (runtime.StaleAddress): one of the addresses the runtime hands every new process from the
// daemon's configuration (the worker stream it dials, the daemon's API, NATS, Envoy, Dispatch, the
// secrets broker) moved since this one launched, as each does when the daemon restarts with its key
// changed; the worker stream's keys are `advertise_host`, `bind` when no `advertise_host` is set,
// and `worker_stream_port`. The process keeps what it holds for as long as it runs, and what it
// holds may never reach the daemon or the service again, so there is nothing to wait out. A task
// whose turn the process was running goes back to waiting first (interrupted), the same recovery a
// death in a turn gets: that turn used the addresses the process holds, and the relaunch ends it. A
// held suspension ends here as it does for a death (endHeld): the claim is suspended, not
// relaunched. Otherwise the claim is relaunched at once through the launch path a death uses
// (launch): a Resume of its recorded session, or a Spawn over the claim's existing Sandbox when it
// has not registered yet, onto a process handed the current addresses. The stale observation is
// never charged, unlike a death (chargeDeath, relaunchAfterFailure): the process did nothing wrong.
// A relaunch the runtime refuses is charged as any launch failure is. The process runs on, so a
// compaction of its session runs on with it: the end of one gives the claim back as it would have
// without the repoint, and the task that end owes waits for the relaunch's turn rather than going
// to a process that turn replaces (ownsCompaction, sendPending). A compaction asked for after the
// relaunch is parked is refused before anything is sent (compactStart). The launch lets the
// process go as one whose address changed (endRepointed), and records what it ended there:
// in this decision, or at the turn of the wake the pacing parks it on, since the process runs
// until then (launchNow, letGo).
func (m *Machine) repoint(ctx context.Context, observation runtime.Observation) error {
	// The process is read through the guard backstopStalledTurn's comment gives the reason for:
	// judge drops an observation while the claim records none, so no live path arrives here
	// without one, and a panic in the supervisor would take every tree of the deployment with it
	// (noProcessRecorded).
	incarnation := noProcessRecorded
	if loc := m.claim.Locator; loc != nil {
		incarnation = loc.Incarnation
	}
	m.log.Warn("supervise: the process is alive at a stale address; replacing it with one at the current address",
		"incarnation", incarnation, "detail", observation.Detail)
	_, err := m.replaceProcess(ctx, "the process at a stale address is replaced mid-turn", ordinaryEnd, endRepointed)
	return err
}

// displaced is the claim's process found gone, or found to be some other process, through no
// fault of its own: the daemon itself replaced its pod while it ran, to launch another role there
// (runtime.Observation.Displaced); something outside the pod ended the pod it ran in — the
// cluster evicted, preempted or shut that pod down, or something deleted it that no act of the
// daemon's accounts for (runtime.PodEndNotOurs, which endedFromOutside sends here every time); or
// it was another runtime's process — a Sandbox role process a daemon running the project under
// kubernetes recorded — that this daemon takes over (`runtime.tmux.take_over`) and cannot see. The
// observation's detail names which. The process did nothing wrong, so it is recovered as repoint
// recovers a stale one: nothing is charged, neither a launch failure nor a death with work
// outstanding, and the claim's session is relaunched at once (replaceProcess). A turn the process
// was in is lost, and its task goes back to waiting, sent again once the session resumes; a held
// suspension ends in a suspension. The process is gone, so a compaction running over it is dropped
// before this runs and its end gives the claim back nothing (judge, compactionLostProcess). It is
// the one recovery that carries an out-of-memory end's note (runtime.Observation.OutOfMemory),
// since a pod the daemon replaced can also have run out of memory before the replacement landed,
// and the role whose turn that end cut off is told so wherever the end is recovered.
//
// Every claim the runtime observes is recovered this way, the project controller's included: what
// decides the recovery is the cause of the end and never the kind of pod or the role in it.
//
// What the displacement ended is recorded first, as a death's is (died): the process is gone
// whatever the replacement goes on to do, and the take-back it begins with changes the task the
// record reads (recordGoneEnd). That take-back comes whatever the claim's state, so a confirmed
// task is marked as taken by this decision before the record is read (Machine.cutTurnOf), and
// the record says its turn was cut off. A held suspension's stop then finds that record standing
// and records no suspension in its place (endHeld), so the next process is told what ended it.
//
// cause is who ended the process, in the daemon's words, which the record carries and the launch
// lets the process go with (launch, letGo): endDisplaced for an act of the daemon's own
// (runtime.Observation.Displaced, judge), endFromOutside for an end something outside the pod
// made (endedFromOutside), which the daemon must not tell the next process it made.
func (m *Machine) displaced(ctx context.Context, observation runtime.Observation, cause string) error {
	// Guarded as repoint's own read is, and for the reason its comment gives
	// (backstopStalledTurn, noProcessRecorded).
	incarnation, process := noProcessRecorded, noProcessRecorded
	if loc := m.claim.Locator; loc != nil {
		incarnation, process = loc.Incarnation, loc.Label()
	}
	m.log.Warn("supervise: the process was displaced through no fault of its own, its pod replaced by the daemon, its pod ended by something outside it, or it another runtime's process this daemon takes over; relaunching it uncharged",
		"incarnation", incarnation, "process", process, "observed", string(observation.Kind),
		"podEndReading", string(observation.EndReading), "detail", observation.Detail, "cause", cause)
	m.recordGoneEnd(ctx, cause, "")
	_, err := m.replaceProcess(ctx, "the process was displaced mid-turn, its pod replaced by the daemon, its pod ended by something outside it, or it another runtime's process this daemon takes over", observation.OutOfMemory, cause)
	return err
}

// endedFromOutside is the claim's process gone with a pod the runtime read as ended by something
// outside it (runtime.PodEndNotOurs): an eviction the cluster marked a disruption target, a
// preemption, a node going down, or a delete no act of the daemon's accounts for. Such an end is
// continued in the claim's own session (displaced), recorded as an end from outside the pod
// (endFromOutside) and never as the daemon's replacement of it, and charged nothing — neither a
// death nor a launch failure — however often it comes, whatever the claim's agent was doing and
// whether it was ready or still starting. Nothing the role in the pod can do fixes a cause
// outside it, and stopping the role fixes a cause that stays no better: a charge at some count
// would only turn a loop of restarts into a stall. An end the runtime read neither way is charged
// as a death (died), as it always was. Three of the kubelet's evictions are such ends: the eviction
// of a pod over a limit of its own, which carries no DisruptionTarget condition, and the two of a
// node under pressure whose message names a container over its memory or ephemeral-storage
// request, which carry it (runtime.DeathMemoryRequest, runtime.DeathDiskRequest; sandbox's
// clusterEndedReasons and evictedOverRequest say why).
//
// It records two things all the same, before the relaunch, because a parked relaunch writes
// nothing until its turn; a write the store would not take is owed and the relaunch goes on
// under it (died states why):
//
//   - the end of the turn the end cut off (Claim.LastTurnEnd), as a charged death records it
//     (chargeDeath), for the reason chargeDeath gives: the agent that turn belonged to is gone
//     and never ends it, so without that record a later death of the relaunched agent, idle with
//     nothing pending, would find the dead turn open and be charged against it;
//   - that the ended process is decided (Claim.DeathCharged), the record died keeps for one dead
//     process, so a later report of that same process — the orphan sweep's, over a claim whose
//     relaunch the lifecycle refused or the wake pacing parked — is charged no death.
//
// Each end writes one line for the claim saying what it got (continuedFromOutsideLine) before
// displaced writes its own. The runtime's line for the pod's end is written before any claim is
// decided and names every role the end ended, so this line, one per claim, is what says what each
// role got.
func (m *Machine) endedFromOutside(ctx context.Context, observation runtime.Observation) error {
	m.log.Info(continuedFromOutsideLine, "starting", slices.Contains(unready, m.claim.State), "detail", observation.Detail)
	m.claim.DeathCharged = true
	if m.turnOpen() {
		m.claim.LastTurnEnd = m.deps.Clock.Now()
	}
	return errors.Join(m.persist(ctx), m.displaced(ctx, observation, endFromOutside))
}

// continuedFromOutsideLine is the line, at Info, of an end from outside the claim's pod: the
// claim's own session is resumed and nothing is charged (endedFromOutside). It carries
// `starting`, whether the claim's agent was still starting when its pod ended, and the `detail`
// of the reading. It is pinned as a constant because an operator and a production check read it
// whole beside the runtime's line for the pod's end: that line says what ended, and each claim's
// line says what that claim got.
const continuedFromOutsideLine = "supervise: the claim's pod ended from outside it; continued in its own session and charged nothing"

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
	// it, with no launch queued: the take-back that replacement begins with was refused
	// (interrupted), or the take-back landed and the launch's own lifecycle recheck refused
	// (relaunchRefused). No process was let go and none is coming, so the next sweep posts over
	// the claim again, which is why it is the one end told to nobody (TellsOwner).
	EndNotMade ProcessEnd = "not made"
	// EndRelaunched is the process let go and stopped and the claim's session brought back on a
	// new one, which the claim records (launchTries).
	EndRelaunched ProcessEnd = "relaunched"
	// EndRelaunchParked is the turn's task taken back (interrupted) and the launch that replaces
	// the process parked by the wake pacing until its turn (reserveWake, parkWake). The claim
	// still records the process and still runs it until that turn, which is where the launch
	// stops it and brings the session back (wakeNow, launchNow) — unless a suspension held
	// meanwhile ends the claim there instead (endHeld), the lifecycle recheck that turn makes
	// first refuses the launch and leaves the process as it is (runWake, relaunchRefused), or the
	// tree's close has dropped the wake with the process (letGo). It is an end of its own because
	// something has happened to this worker — the task it was serving is owed to it
	// again — while nothing has been relaunched yet, and the two sentences are not the same news.
	EndRelaunchParked ProcessEnd = "relaunch parked"
	// EndRelaunchWaitsForPod is the process let go and stopped and the launch that replaces it
	// waiting at its issue pod's gate behind the roles of the pod that start or load their
	// sessions before it, one load at a time (askPod, wake.park). It asks as no request's
	// (launch), so, where no request has led the pod's run of starts, a start asked after it can
	// go ahead of it (podQueue.next): one a request raised, or, where no such start waits, the
	// start of the role working the issue's current phase and, unless this launch is that role's,
	// one of a role before its own in claim.Roles. It may wait out more than one load. The claim
	// is written launching and records no process, and the gate's grant of its start starts the
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
// (repoint's stale address, refreshPlugin's stale plugin contract, displaced's pod replaced or
// ended from outside it) shares once logged: a task whose turn the process was running goes back
// to waiting first (interrupted), the same recovery a death in a turn gets; a held suspension ends
// here as it does for a death (endHeld), suspended rather than relaunched; otherwise the claim is
// relaunched at once through the launch path a death uses (launch), a Resume of its recorded
// session or a Spawn over its existing Sandbox when it has not registered yet. interruption is the
// sentence the turn's recovery is logged with, and outOfMemory says whether the end that cut that
// turn off was the pod running out of memory: only a displacement can carry one, since the other
// callers replace a process that is still running. ending is why the process is let go, in the
// daemon's words, which the launch records with what that process ended (launch, letGo); a held
// suspension's stop records its own (endHeld).
//
// It answers the end it reached beside its error, for the caller that has somebody to tell
// (ProcessEnd): every one of these recoveries can end the process and still fail — the launch's
// write is refused, its tries run the launch budget out, the held suspension's write is refused
// — and the end is what happened to the worker, which is decided here, once, rather than at each
// call site.
func (m *Machine) replaceProcess(ctx context.Context, interruption string, outOfMemory bool, ending string) (ProcessEnd, error) {
	found := ""
	if loc := m.claim.Locator; loc != nil {
		found = loc.Incarnation
	}
	parked := m.wake
	// Whether the runtime left a process running is this replacement's own fact, so it starts
	// unset here: a refusal an earlier replacement of this claim met is not what the end reached
	// now says of the process it let go (stopUnconfirmed, backstopStalledTurn).
	m.stopUnconfirmed = false
	err := m.replaceNow(ctx, interruption, outOfMemory, ending)
	return m.endReached(found, parked), err
}

// replaceNow is the replacement itself, whose end replaceProcess reads off the claim it leaves.
//
// Its first step can end the replacement where it stands: a take-back the store refuses returns
// here with the claim's process kept, nothing relaunched and no wake raised (interrupted). A
// stale-plugin relaunch that ends there is raised again by the next pass, which reads the claim
// as stale as it left it (refreshStalePlugins in internal/daemon). A take-back that lands marks
// the task as taken from that process's turn, which the let-go that follows records
// (Machine.cutTurnOf), here or at the parked launch's turn.
func (m *Machine) replaceNow(ctx context.Context, interruption string, outOfMemory bool, ending string) error {
	if err := m.interrupted(ctx, interruption, outOfMemory); err != nil {
		return err
	}
	if m.held != nil {
		return m.endHeld(ctx, false, stopOwed)
	}
	return m.launch(ctx, ending)
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
