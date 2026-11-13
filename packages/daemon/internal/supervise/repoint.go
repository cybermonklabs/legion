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
// relaunch is parked is refused before anything is sent (compactStart).
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
	_, err := m.replaceProcess(ctx, "the process at a stale address is replaced mid-turn", ordinaryEnd)
	return err
}

// displaced is the claim's process found gone, or found to be some other process, through no
// fault of its own: the daemon itself replaced its pod while it ran, to launch another role there
// (runtime.Observation.Displaced); something outside the pod ended the pod it ran in — the
// cluster evicted, preempted or shut that pod down, or something deleted it that no act of the
// daemon's accounts for (runtime.PodEndNotOurs, which endedFromOutside sends here until such ends
// run out their count); or it was another runtime's process — a Sandbox role process a daemon
// running the project under kubernetes recorded — that this daemon takes over
// (`runtime.tmux.take_over`) and cannot see. The
// observation's detail names which. The process did
// nothing wrong, so it is recovered as repoint recovers a stale one: nothing is charged, neither a
// launch failure nor a death with work outstanding, and the claim's session is relaunched at once
// (replaceProcess). A turn the process was in is lost, and its task goes back to waiting, sent
// again once the session resumes; a held suspension ends in a suspension. The process is gone, so
// a compaction running over it is dropped before this runs and its end gives the claim back
// nothing (judge, compactionLostProcess). It is the one recovery that carries an out-of-memory
// end's note (runtime.Observation.OutOfMemory), since a pod the daemon replaced can also have run
// out of memory before the replacement landed, and the role whose turn that end cut off is told
// so wherever the end is recovered.
//
// Every claim the runtime observes is recovered this way, the project controller's included: what
// decides the recovery is the cause of the end and never the kind of pod or the role in it.
func (m *Machine) displaced(ctx context.Context, observation runtime.Observation) error {
	// Guarded as repoint's own read is, and for the reason its comment gives
	// (backstopStalledTurn, noProcessRecorded).
	incarnation, process := noProcessRecorded, noProcessRecorded
	if loc := m.claim.Locator; loc != nil {
		incarnation, process = loc.Incarnation, loc.Label()
	}
	m.log.Warn("supervise: the process was displaced through no fault of its own, its pod replaced by the daemon, its pod ended by something outside it, or it another runtime's process this daemon takes over; relaunching it uncharged",
		"incarnation", incarnation, "process", process, "observed", string(observation.Kind),
		"podEnd", string(observation.PodEnd), "detail", observation.Detail)
	_, err := m.replaceProcess(ctx, "the process was displaced mid-turn, its pod replaced by the daemon, its pod ended by something outside it, or it another runtime's process this daemon takes over", observation.OutOfMemory)
	return err
}

// endedFromOutside is the claim's process gone with a pod the runtime read as ended by something
// outside it (runtime.PodEndNotOurs): an eviction, a preemption, a node going down, or a delete no
// act of the daemon's accounts for. Such an end is continued uncharged (displaced) and counted
// (Budgets.PodEndsNotOurs). At launch_failure_limit such ends in a row, with no turn of the claim
// completed between them (turnEnded resets the count, and so do the end of the task it was
// counted against, retirePending, and an agent that becomes ready with no work outstanding,
// ready), the end is charged as the claim's own death (died), and so is every such end after it
// until the run is reset: a pod that uses more memory than it requested can drive its own node
// into pressure, and that eviction carries the same DisruptionTarget condition a neighbour's
// does. An outcome the daemon cannot read is charged, and a claim that keeps ending its own pod
// that way is bounded as a crash loop is, rather than relaunched for ever.
//
// One ended process is counted once, however often the runtime reports it gone, by the record
// died keeps for the same purpose (Claim.DeathCharged): the report that counts the end writes
// it, and a later report of that same process — the orphan sweep's, over a claim whose relaunch
// the lifecycle refused or the wake pacing parked — counts nothing. The count and the record are
// written before the relaunch, as died writes its charge, because a parked relaunch writes
// nothing until its turn; a write the store would not take is owed and the relaunch goes on under
// it (died states why). At the limit the charge is died's whole, so a second report of the end it
// charged is died's second report and charged nothing more.
//
// An end that finds the claim's agent not yet ready (unready) is counted too, whether or not the
// claim holds a task: the start the claim is making is the work such an end interrupts, and it is
// the end died charges a launch failure for when the runtime reads it neither way
// (relaunchAfterFailure). A pod's own start — the workspace's restore and six agents starting at
// once in a Burstable pod — can drive its node into memory pressure, and that eviction carries
// DisruptionTarget, so a start that keeps ending its own pod would otherwise be relaunched for
// ever, holding a task it never reaches. At the limit such an end goes to died as any other does,
// and died charges an agent not yet ready a launch failure rather than a death (chargeDeath reads
// workOutstanding): the launch failures then bound the loop, and only a ready agent ends them.
// The run is counted against that start, so an agent that becomes ready with no work outstanding
// ends it there (ready): its start is over and nothing is left for the run to count against. One
// that becomes ready holding its task keeps its run, which that task's completed turn or its end
// resets.
//
// An end that finds the agent ready with no work outstanding — no task held and no turn open
// (workOutstanding) — is neither counted nor charged: it is displaced as it always was, and the
// count stays where it stood. The rule is about work that keeps ending its own pod, and a death
// is charged only while there is work for one to be charged against (chargeDeath reads the same
// condition). A claim idle between assignments completes no turn, so a count it could still
// raise would only grow, and a pod the cluster repacks once a week would cost that claim a launch
// failure and a backoff from its third repacking on, for work it was not doing.
//
// An end counted on a ready agent ends the turn it interrupted (Claim.LastTurnEnd), as a charged
// death does (chargeDeath), for the reason chargeDeath gives: the agent that turn belonged to is
// gone and never ends it, so without that record every later end of the relaunched agent, idle
// with nothing pending, would find the dead turn open and be counted against it. An agent not yet
// ready was in no turn, so an end before its ready records none. At the limit the turn is left
// for died's charge to end, since that charge reads the same open turn.
func (m *Machine) endedFromOutside(ctx context.Context, observation runtime.Observation) error {
	starting := slices.Contains(unready, m.claim.State)
	if !starting && !m.workOutstanding() {
		return m.displaced(ctx, observation)
	}
	if !m.claim.DeathCharged {
		m.claim.Budgets.PodEndsNotOurs++
	}
	if m.claim.Budgets.PodEndsNotOurs >= m.deps.Limits.LaunchFailures {
		m.log.Warn("supervise: the claim's pod ended from outside it again with no turn completed since; charging this end as the claim's own death",
			"podEndsNotOurs", m.claim.Budgets.PodEndsNotOurs, "limit", m.deps.Limits.LaunchFailures,
			"starting", starting, "detail", observation.Detail)
		return m.died(ctx, observation)
	}
	m.claim.DeathCharged = true
	if !starting {
		m.claim.LastTurnEnd = m.deps.Clock.Now()
	}
	return errors.Join(m.persist(ctx), m.displaced(ctx, observation))
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
	// it, with no launch queued: the take-back that replacement begins with was refused
	// (interrupted), or the take-back landed and the launch's own lifecycle recheck refused
	// (relaunchRefused). No process was let go and none is coming, so the next sweep posts over
	// the claim again, which is why it is the one end told to nobody (TellsOwner).
	EndNotMade ProcessEnd = "not made"
	// EndRelaunched is the process let go and stopped and the claim's session brought back on a
	// new one, which the claim records (launchNow).
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
	// EndRelaunchPending is the process let go and stopped with nothing in its place: the
	// launch's own write was refused and the same launch waits on the probe interval
	// (relaunchLater), or the daemon's stop cut it short and the next boot makes it. The claim
	// records no process either way.
	EndRelaunchPending ProcessEnd = "relaunch pending"
	// EndSuspended is a suspension held for the turn ending the process rather than replacing it
	// (endHeld): nothing runs for the claim until it is resumed.
	EndSuspended ProcessEnd = "suspended"
	// EndFailed is every try at the replacement failing until the claim's launch budget is gone
	// (launchNow, fail): nothing runs for the claim and nothing relaunches it.
	EndFailed ProcessEnd = "failed"
)

// ProcessEnds is every value of ProcessEnd, in the order they are declared. A value added to the
// type is added here: TestProcessEndsListsEveryEndDeclared reads this package's own source and
// fails until it is, and the walks that keep the owner's notice total over the type read this
// list (TestEveryProcessEndTellsItsOwnerExactlyOnce, the daemon's
// TestTheNoticeHasWordsForEveryEndThatTellsItsOwner). A fresh slice is returned so that no
// caller can edit the one the next reads.
func ProcessEnds() []ProcessEnd {
	return []ProcessEnd{EndNotMade, EndRelaunched, EndRelaunchParked, EndRelaunchPending, EndSuspended, EndFailed}
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
// shares once logged: a task whose turn the process was running goes back to waiting first
// (interrupted), the same recovery a death in a turn gets; a held suspension ends here as it does
// for a death (endHeld), suspended rather than relaunched; otherwise the claim is relaunched at
// once through the launch path a death uses (launch), a Resume of its recorded session or a Spawn
// over its existing Sandbox when it has not registered yet. interruption is the sentence the
// turn's recovery is logged with, and outOfMemory says whether the end that cut that turn off was
// the pod running out of memory: only a displacement can carry one, since the other callers
// replace a process that is still running.
//
// It answers the end it reached beside its error, for the caller that has somebody to tell
// (ProcessEnd): every one of these recoveries can end the process and still fail — the launch's
// write is refused, its tries run the launch budget out, the held suspension's write is refused
// — and the end is what happened to the worker, which is decided here, once, rather than at each
// call site.
func (m *Machine) replaceProcess(ctx context.Context, interruption string, outOfMemory bool) (ProcessEnd, error) {
	found := ""
	if loc := m.claim.Locator; loc != nil {
		found = loc.Incarnation
	}
	parked := m.wake
	// Whether the runtime left a process running is this replacement's own fact, so it starts
	// unset here: a refusal an earlier replacement of this claim met is not what the end reached
	// now says of the process it let go (stopUnconfirmed, backstopStalledTurn).
	m.stopUnconfirmed = false
	err := m.replaceNow(ctx, interruption, outOfMemory)
	return m.endReached(found, parked), err
}

// replaceNow is the replacement itself, whose end replaceProcess reads off the claim it leaves.
//
// Its first step can end the replacement where it stands: a take-back the store refuses returns
// here with the claim's process kept, nothing relaunched and no wake raised (interrupted). A
// stale-plugin relaunch that ends there is raised again by the next pass, which reads the claim
// as stale as it left it (refreshStalePlugins in internal/daemon).
func (m *Machine) replaceNow(ctx context.Context, interruption string, outOfMemory bool) error {
	if err := m.interrupted(ctx, interruption, outOfMemory); err != nil {
		return err
	}
	if m.held != nil {
		return m.endHeld(ctx)
	}
	return m.launch(ctx)
}

// endReached is the end a replacement made, read from the claim it left beside found, the
// incarnation that claim recorded when the replacement began, and parked, the wake that claim
// held before it. The claim says it whole: a claim whose launch the pacing has just parked waits
// for that wake's turn, one still recording that incarnation was never let go of, one recording
// another is relaunched, and a claim recording none was let go and is where the let-go left it —
// suspended, failed, or launching with the launch still to make, since every caller of letGo
// writes the claim's state in the same step.
//
// The park is read first and by identity, because it is the one end the claim alone cannot show:
// a parked launch leaves the claim exactly as the replacement found it, process and state and
// all (parkWake), and the wake the machine holds after is this replacement's only where it is
// not the one it began with. The caller holds mu.
func (m *Machine) endReached(found string, parked *wake) ProcessEnd {
	if m.wake != nil && m.wake != parked {
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
