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
// (runtime.Observation.Displaced), or it was another runtime's process — a Sandbox role process a
// daemon running the project under kubernetes recorded — that this daemon takes over
// (`runtime.tmux.take_over`) and cannot see. The observation's detail names which. An end that
// something outside the pod made (runtime.PodEndNotOurs) is no fault of the process either:
// endedFromOutside recovers it through this one's two steps (recordDisplaced, replaceProcess),
// with the take-back and its decision between them. The process did nothing wrong, so it is
// recovered as repoint recovers a stale one: nothing is charged, neither a launch failure nor a
// death with work outstanding, and the claim's session is relaunched at once (replaceProcess). A
// turn the process was in is lost, and its task goes back to waiting, sent again once the session
// resumes; a held suspension ends in a suspension. The process is gone, so a compaction running
// over it is dropped before this runs and its end gives the claim back nothing (judge,
// compactionLostProcess). It and endedFromOutside are the recoveries that carry an out-of-memory
// end's note (runtime.Observation.OutOfMemory), since a pod the daemon replaced can also have run
// out of memory before the replacement landed, and the role whose turn that end cut off is told
// so wherever the end is recovered.
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
// The record is typed as displaced's (EndedWork.Displaced), and it is left whatever the end ended
// but over an end already decided (markDisplaced), so a later report of the same launch finds
// this judgement standing.
//
// The cause it records and lets the process go with (launch, letGo) is endDisplaced, an act of the
// daemon's own: judge sends an end from outside the pod to endedFromOutside, which hands
// recordDisplaced its own cause, endFromOutside, so the daemon never tells the next process that
// it made that end.
//
// The first judgement of a process stands, and a displacement is one: judge sends here, beside
// every report the runtime reads as displaced, every report read neither way of a process whose
// end the claim records as displaced's (recordedDisplaced) over no death died judged first
// (judgedDeath). After a daemon restart the runtime's record of its own act is gone (sandbox's
// meanToEndPod), so its report of the same pod comes read neither way, or as from outside the
// pod, which endedFromOutside decides. Sent to died, a
// report read neither way would decide a death of the daemon's own replacement: a launch
// failure, a death where the claim held work, and the relaunch in the first queue. Here it is
// that displacement again: its line, no record over displaced's, the take-back of a task whose
// turn it cut off where displaced's own take-back was refused, and the relaunch, charged
// nothing and waiting in the second queue, whose next process is told the replacement
// displaced recorded.
func (m *Machine) displaced(ctx context.Context, observation runtime.Observation) error {
	m.recordDisplaced(ctx, observation, endDisplaced, false)
	m.markDisplaced()
	_, err := m.replaceProcess(ctx, displacedMidTurn, observation.OutOfMemory, endDisplaced)
	return err
}

// recordDisplaced is displaced's first step: the line that says the process was displaced, and
// the record of what that ended (recordGoneEnd), made before the replacement's take-back changes
// the task the record reads. Its second step is the replacement (replaceProcess).
// endedFromOutside takes the task back and writes its decision after this step and before that
// one, so the claim it writes carries the record. cause is who ended the process, in the
// daemon's words, and fromOutside whether that was something outside the pod, which the record
// carries from its making (EndedWork.FromOutside), and otherwise that displaced judged it
// (EndedWork.Displaced): endDisplaced and false for displaced, endFromOutside and true for
// endedFromOutside.
func (m *Machine) recordDisplaced(ctx context.Context, observation runtime.Observation, cause string, fromOutside bool) {
	// Guarded as repoint's own read is, and for the reason its comment gives
	// (backstopStalledTurn, noProcessRecorded).
	incarnation, process := noProcessRecorded, noProcessRecorded
	if loc := m.claim.Locator; loc != nil {
		incarnation, process = loc.Incarnation, loc.Label()
	}
	m.log.Warn("supervise: the process was displaced through no fault of its own, its pod replaced by the daemon, its pod ended by something outside it, or it another runtime's process this daemon takes over; relaunching it uncharged",
		"incarnation", incarnation, "process", process, "observed", string(observation.Kind),
		"podEndReading", string(observation.EndReading), "detail", observation.Detail, "cause", cause)
	m.recordGoneEnd(ctx, cause, "", fromOutside, !fromOutside, false)
}

// displacedMidTurn is the sentence the replacement of a displaced process logs the take-back of
// the task whose turn the displacement cut off under (replaceProcess, interrupted).
const displacedMidTurn = "the process was displaced mid-turn, its pod replaced by the daemon, its pod ended by something outside it, or it another runtime's process this daemon takes over"

// endedFromOutside is the claim's process gone with a pod the runtime read as ended by something
// outside it (runtime.PodEndNotOurs): an eviction the cluster marked a disruption target, a
// preemption, a node going down, or a delete no act of the daemon's accounts for. Such an end is
// continued in the claim's own session as a displacement is (recordDisplaced, replaceProcess),
// recorded as an end from outside the pod (endFromOutside, EndedWork.FromOutside) and never as
// the daemon's replacement of it, and charged nothing — neither a death nor a launch failure —
// however often it comes, whatever the claim's agent was doing and whether it was ready or still
// starting. Nothing the role in the pod can do fixes a cause outside it, and stopping the role
// fixes a cause that stays no better: a charge at some count would only turn a loop of restarts
// into a stall. An end the runtime read neither way, of a process whose end the claim holds no
// record of as one from outside the pod, is charged as a death (died), as it always was, but
// where the claim records that end as displaced's over no death died judged first: judge sends
// such a report to displaced (recordedDisplaced), as the displacement again. Three of the
// kubelet's evictions are such ends: the eviction of a pod over a limit of its own, which
// carries no DisruptionTarget condition, and the two of a node under pressure whose message
// names a container over its memory or ephemeral-storage request, which carry it
// (runtime.DeathMemoryRequest, runtime.DeathDiskRequest; sandbox's clusterEndedReasons and
// evictedOverRequest say why).
//
// judge sends it every report the runtime read so over no death of this launch that died judged
// (judgedDeath), and every report of a process whose end the claim records as one from outside
// the pod, however that report was read (recordedFromOutside): such a report is this end again,
// never a death. That record is made here first, before the take-back and the decision, so it
// stands for every report after this one, whether or not the decision was made. After a daemon
// restart the runtime's report usually reaches the claim before the boot's replay
// (RecordedDeath), read neither way, since the reading went with the old daemon's memory, and
// died would charge it a launch failure and record it as a death. Where a write of the claim
// carried the record into the store before the restart, the report finds that record standing
// and comes here; where the decision was stored with it, it finds the task already taken back,
// so it raises the uncharged relaunch, which the replay then finds raised (launchMet).
//
// It takes back the task whose turn the end cut off first, after the record of what the end
// ended (recordDisplaced), which reads the task before the take-back changes it, as died's
// recordGoneEnd does; and a take-back the store refused returns here before the decision, as it
// does in died. The record of this launch stays in memory and goes into the store with the
// claim's next write, such as the one an Uncertain report makes (judge), so the next report of
// the process, however it is read, comes back here and decides the end then, after it takes the
// task back. The stored claim holds no decision over a task still confirmed, so no replay after
// a restart relaunches it past the take-back (the boot replays only a decided end), and settle
// retires nothing. Decided before the take-back, the stored claim would hold the decision over a
// task still confirmed: the replay would relaunch it with no take-back, and settle would retire
// a task no turn finished.
//
// It decides the end once. A report over an end already decided (decidedFromOutside, read on
// entry) — a restarted daemon's first report over a stored decision, or a sweep report over a
// relaunch the lifecycle refused — writes this end's line, leaves the record of this launch as
// it stands (recordDisplaced), finds nothing confirmed to take back and replaces the process,
// but records none of the three things below again and writes nothing for them. The claim
// reads working while its relaunch waits, so the end of the turn, which operators are shown
// (Claim.LastTurnEnd), would otherwise move to the moment of each such report.
//
// The first judgement of the process stands. A report over a death of this launch that died
// judged first — decided (chargedDeath), or recorded over a take-back the store refused — never
// comes here, however the runtime read it: judge sends it to died (judgedDeath), which handles
// it as a later report of that death, so the claim stays that death, charged once, its relaunch
// waits in the first queue, and the boot raises it as the death's. An end is continued uncharged
// only where the daemon read it as one from outside the pod before died judged it, so an end
// decided on entry (Claim.DeathCharged) is one this decided (decidedFromOutside).
//
// With the task back, over an end not decided yet (Claim.DeathCharged false on entry), it
// records three things, before the relaunch, because a parked relaunch writes nothing until its
// turn; a write the store would not take is owed and the relaunch goes on under it (died states
// why):
//
//   - the end of the turn the end cut off (Claim.LastTurnEnd), as a charged death records it
//     (chargeDeath), for the reason chargeDeath gives: the agent that turn belonged to is gone
//     and never ends it, so without that record a later death of the relaunched agent, idle with
//     nothing pending, would find the dead turn open and be charged against it;
//   - that the ended process is decided (Claim.DeathCharged), the record died keeps for one dead
//     process, so a later report of that same process — the orphan sweep's, over a claim whose
//     relaunch the lifecycle refused, or a restarted daemon's first — comes back here, finds the
//     end decided and decides nothing again (decidedFromOutside). A relaunch the wake pacing
//     parks brings no such report back: judge drops each while it waits (launchWaits);
//   - that the end was from outside the pod, in the record of what that end ended
//     (EndedWork.FromOutside, markFromOutside). A daemon restart while the relaunch waits
//     restores the claim standing over its decided process, and the boot raises the relaunch the
//     restart lost (RecordedDeath). This typed value is what tells that replay, and a report that
//     comes before it, the end from a charged death, so the relaunch is charged nothing, resumes
//     the claim's own session and sends its task again, and the next process is told that its pod
//     was ended from outside it where the end cut off its work (relaunchRecordedDeath, judge).
//
// The write comes after the record and the take-back and before the replacement, so the stored
// claim carries that record over a task taken back, and the replacement's own take-back finds
// nothing confirmed. Written before the record, the store would hold the decided process with
// no record of this end, which the boot reads as a charged death: one launch failure, its
// backoff, and a next process told that the process died.
//
// Each end writes one line for the claim saying what it got (continuedFromOutsideLine) before
// recordDisplaced writes displaced's. The runtime's line for the pod's end is written before any
// claim is decided and names every role the end ended, so this line, one per claim, is what says
// what each role got.
func (m *Machine) endedFromOutside(ctx context.Context, observation runtime.Observation) error {
	decided := m.claim.decidedFromOutside()
	m.log.Info(continuedFromOutsideLine, "starting", slices.Contains(unready, m.claim.State), "detail", observation.Detail)
	m.recordDisplaced(ctx, observation, endFromOutside, true)
	if err := m.interrupted(ctx, displacedMidTurn, observation.OutOfMemory); err != nil {
		return err
	}
	var written error
	if !decided {
		m.claim.DeathCharged = true
		if m.turnOpen() {
			m.claim.LastTurnEnd = m.deps.Clock.Now()
		}
		m.markFromOutside()
		written = m.persist(ctx)
	}
	_, err := m.replaceProcess(ctx, displacedMidTurn, observation.OutOfMemory, endFromOutside)
	return errors.Join(written, err)
}

// markFromOutside leaves on the claim, for the launch it records, a record typed as an end from
// outside the pod (EndedWork.FromOutside), whatever that end ended, so the decision's write
// carries it and every later read of the end reads it as one from outside the pod: a report of
// the ended process (judge, recordedFromOutside) and the boot's replay of the decision
// (decidedFromOutside). A record made here carries endFromOutside as its cause; where
// recordGoneEnd recorded the end here (recordDisplaced), that record already carries both; and a
// record that already stands for this launch is typed, keeping its cause where displaced made it
// and taking this end's where no judgement types it (unjudgedRecord, below).
//
// Where the end ended nothing worth telling — no turn running, no task taken back and no
// background work reported (recordEnd) — recordGoneEnd records nothing, and the record left here
// is told already, by no delivery (leaveRecord): no prompt carries it, so the next process is
// told nothing of such an end, as it is told nothing where no restart comes. A record of an
// earlier launch that still tells is not marked told: it becomes this launch's record, typed as
// this end's and keeping its own cause, so what the earlier process ended is still told in its
// own words, this end having ended nothing to tell.
//
// A record of this launch that already stands keeps its told state and the rest, and is typed as
// one from outside the pod. A death died judged never comes here (judge, judgedDeath), so that
// record is this end's own, the one displaced made of the same end first, or one no judgement
// types (unjudgedRecord).
//
// Displaced's keeps its cause and its own type (EndedWork.Displaced): the daemon replaced the
// pod, and a later read of it found it ended from outside it, as a restart between the
// replacement and the pod's end makes it, since the runtime holds its record of the daemon's own
// act in memory alone (sandbox's meanToEndPod). Typed, the claim reads the end decided from
// outside the pod: charged nothing, its relaunch waiting in the second queue, relaunched charged
// nothing by the boot's replay, and decided nothing again by a later report. Left untyped, it
// would read a charged death (chargedDeath) that nothing judged: its relaunch in the first
// queue, the replay's relaunch charged a launch failure, and a later report sent to died. The
// next process is still told the cause displaced recorded first.
//
// One no judgement types takes this end's cause and drops the kind of a death's
// (EndedWork.Death), and so tells the next process that its pod was ended from outside it: the
// end that let the process go is this one. That is right of died's record of a death that a
// verdict finding the process running ended: the process ran on after that judgement. It also
// takes the cause of a displacement record an earlier daemon stored without its type, which
// only its sentence tells from that death's, and the record is read by its types alone; such a
// role, relaunched once across the upgrade, is told that its pod was ended from outside it
// rather than replaced, and is charged nothing either way.
func (m *Machine) markFromOutside() {
	held := &m.claim.EndedWork
	if held.Cause == "" || held.Generation != m.claim.Generation {
		m.leaveRecord(EndedWork{Cause: endFromOutside, FromOutside: true})
		return
	}
	if m.unjudgedRecord() != nil {
		held.Cause, held.Death, held.Outlived = endFromOutside, "", false
		m.carried.ended = false
	}
	held.FromOutside = true
}

// markDisplaced leaves on the claim, for the launch it records, displaced's record of its end
// (EndedWork.Displaced), whatever that end ended, as markFromOutside does for an end from outside
// the pod. Where recordGoneEnd recorded the end (recordDisplaced), that record carries the type
// already; where the end ended nothing worth telling, the record left here (leaveRecord) carries
// endDisplaced as its cause; and a record of this launch no judgement types (unjudgedRecord)
// takes endDisplaced as its cause and the type. So a later report of the same launch read
// neither way, on this daemon or after a restart wherever a write carried the record into the
// store, finds the displacement standing and is that displacement again, charged nothing
// (recordedDisplaced), and the next process is told the replacement. A record of this launch
// another judgement typed first stands as it is, and so does an end already decided
// (Claim.DeathCharged): the decision's record, or its finding that the end ended nothing worth
// recording, stands (recordEnd).
func (m *Machine) markDisplaced() {
	if m.claim.DeathCharged {
		return
	}
	if held := m.unjudgedRecord(); held != nil {
		held.Cause, held.Death, held.Displaced, held.Outlived = endDisplaced, "", true, false
		m.carried.ended = false
		return
	}
	if held := m.claim.EndedWork; held.Cause != "" && held.Generation == m.claim.Generation {
		return
	}
	m.leaveRecord(EndedWork{Cause: endDisplaced, Displaced: true})
}

// unjudgedRecord is the claim's record of the end of the launch it records where no judgement of
// that end is typed on it (EndedWork.Died, FromOutside, Displaced), and nil where there is none.
// Over no decision such a record is died's record of a death that a verdict finding the process
// running ended (reportedProcess), or one an earlier daemon stored without the types. The
// judgement that next judges the end types it and gives it that judgement's own cause and kind
// (died, markFromOutside, markDisplaced), since the end that let the process go is that one.
func (m *Machine) unjudgedRecord() *EndedWork {
	if e := &m.claim.EndedWork; e.Cause != "" && e.Generation == m.claim.Generation && !e.Died && !e.FromOutside && !e.Displaced {
		return e
	}
	return nil
}

// leaveRecord leaves typed, under its cause and its type, as the record of the end of the launch
// the claim records, where no record of that launch stands because the end ended nothing worth
// telling (markFromOutside, markDisplaced). A record of an earlier launch that still tells becomes
// this launch's, with typed's type and this moment and its own cause and kind: this end ended
// nothing to tell, so what the earlier process ended is told in the words its end was recorded
// in, a death's plain sentence of what killed it among them (dispatch://INFRA-1117 R9), and the
// types route the reports of this launch. Otherwise typed is left told already, by no delivery
// (EndedWork.ToldBy empty), so no prompt carries it and the next process is told nothing of an end
// that ended nothing.
func (m *Machine) leaveRecord(typed EndedWork) {
	typed.At, typed.Generation = m.deps.Clock.Now(), m.claim.Generation
	if held := &m.claim.EndedWork; held.Tells() {
		held.At, held.Died, held.FromOutside, held.Displaced, held.Outlived, held.Generation =
			typed.At, false, typed.FromOutside, typed.Displaced, false, typed.Generation
		return
	}
	typed.Told = true
	m.claim.EndedWork = typed
}

// recordedFromOutside is whether the claim holds the record of the end of the launch it records
// now typed as an end from outside the pod (EndedWork.FromOutside). It reads that typed value and
// never the record's sentence. Only endedFromOutside writes such a record (recordDisplaced,
// markFromOutside), so it says the daemon read this launch's end as one from outside the pod,
// whether or not that end is decided yet: the record is made before the take-back, which the
// store can refuse, and before the decision, and it goes into the store with any write of the
// claim. judge sends every report of the process to endedFromOutside where it holds, however the
// runtime read that report, so the one end is continued charged nothing on one daemon, and after
// a restart wherever the record reached the store. A running verdict (reportedProcess) leaves the
// record standing, and cannot follow such an end for the launch the claim records: the end is a
// pod deleted or terminal, the locator names that pod by uid, and the runtime never answers a pod
// running after it answered it gone. The next launch is another generation, whose record this
// does not read.
func (c Claim) recordedFromOutside() bool {
	return c.EndedWork.FromOutside && c.EndedWork.Generation == c.Generation
}

// recordedDisplaced is whether the claim holds the record of the end of the launch it records
// typed as displaced's (EndedWork.Displaced): the daemon judged that end its own replacement of
// the pod, or its take-over of another runtime's process, before anything else judged it. It
// reads that typed value and never the record's sentence. Only displaced writes such a record
// (recordDisplaced, markDisplaced), and it writes one whatever the end ended, but over an end
// already decided, so this holds on the daemon that judged the end, and after a restart wherever
// a write of the claim carried the
// record into the store. judge sends a report read neither way to displaced where this holds and
// no death died judged first (judgedDeath), so the daemon's own replacement is never charged as
// a death; a report read as from outside the pod still goes to endedFromOutside, which types the
// record as one from outside the pod and leaves this as it stands (markFromOutside).
func (c Claim) recordedDisplaced() bool {
	return c.EndedWork.Displaced && c.EndedWork.Generation == c.Generation
}

// decidedFromOutside is whether the claim's decided end is one from outside the pod: the process
// the claim records is decided (Claim.DeathCharged), and the claim holds the record of its end as
// one from outside the pod (recordedFromOutside). The boot's replay of the decision asks it
// (relaunchRecordedDeath), so a decided end from outside the pod is relaunched charged nothing
// whichever of the replay and a report reaches the claim first; and endedFromOutside asks it on
// entry, so a report over an end already decided decides nothing again. It is the one statement
// of that rule, which chargedDeath reads too.
func (c Claim) decidedFromOutside() bool {
	return c.DeathCharged && c.recordedFromOutside()
}

// chargedDeath is whether the end the claim holds decided is a death of the claim's own process:
// died's decision, the death charged or found to have nothing to charge (Claim.DeathCharged), and
// not an end from outside the pod, which writes the same mark and charges nothing
// (decidedFromOutside). Its readers are the queue a paced wake of the claim waits in
// (WakeQueue), since a relaunch after such an end waits in the second, as a displaced relaunch
// does (dispatch://INFRA-5682 R2), and judgedDeath.
func (c Claim) chargedDeath() bool {
	return c.DeathCharged && !c.decidedFromOutside()
}

// judgedDeath is whether the claim holds a death of the launch it records that died judged: one
// died decided (chargedDeath), or one died judged and recorded without deciding. died records
// what the death ended before it takes the task back (recordGoneEnd), typed as a death it judged
// (EndedWork.Died), and a take-back the store refuses returns before the decision; the confirmed
// task that take-back was for makes the record whatever else the death ended (recordEnd). So
// such a claim holds a record of the launch it records (EndedWork.Generation) that says died
// judged it, over no decision. It reads that typed value and never the record's sentence. The
// record and the decision go into the store with any write of the claim, so after a restart
// this holds wherever a write carried either. A verdict that finds the process running clears
// both, and writes the clear (reportedProcess): the process lives, so the death judged of it is
// over, and a later end of that process is judged afresh, as one from outside the pod where it
// is read so.
//
// It is the rule that the first judgement of a process stands, in every order. judge sends a
// report read as from outside the pod (runtime.PodEndNotOurs) to died wherever this holds, and
// died handles it as the later report of that death it is: it decides a death its take-back left
// undecided, and raises again the relaunch of one it decided (relaunchDeath). So
// endedFromOutside never sees a death. The daemon continues a role uncharged where it read the
// end as one from outside the pod (endedFromOutside); where died judged the end first, the daemon
// read it as a death. The one death is charged once, its launch failure by the first raise of its
// relaunch that passes the lifecycle check (deathLaunchFailureCharged), it is told as that death,
// and its relaunch waits in the first queue (WakeQueue). A claim that holds the record of its end
// as one from outside the pod (recordedFromOutside) never holds this.
func (c Claim) judgedDeath() bool {
	return c.chargedDeath() || c.EndedWork.Died && c.EndedWork.Generation == c.Generation
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
	// the process parked by the wake pacing until its turn (reserveWake, waitOnMachine). The claim
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
// (repoint's stale address, refreshPlugin's stale plugin contract, displaced's pod replaced, and
// endedFromOutside's pod ended from outside it, whose take-back comes before this) shares once
// logged: a task whose turn the process was running goes back to waiting first (interrupted),
// the same recovery a death in a turn gets; a held suspension ends here as it does for a death
// (endHeld), suspended rather than relaunched; otherwise the claim is relaunched at once through
// the launch path a death uses (launch), a Resume of its recorded session or a Spawn over its
// existing Sandbox when it has not registered yet. interruption is the sentence the turn's
// recovery is logged with, and outOfMemory says whether the end that cut that turn off was the
// pod running out of memory: only an end of a pod can carry one, since the other callers replace
// a process that is still running. ending is why the process is let go, in the daemon's words,
// which the launch records with what that process ended (launch, letGo); a held suspension's
// stop records its own (endHeld).
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
// state and all (waitOnMachine), and the wake the machine holds after is this replacement's only
// where it is not the one it began with. A launch waiting for its issue pod's start left the claim
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
