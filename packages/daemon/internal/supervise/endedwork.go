package supervise

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// EndedWork is the claim's record of what the last let-go of its process ended of the work its
// agent had running (dispatch://INFRA-3276 R3): when, why in the daemon's own words, whether a
// turn was cut off and whether that turn was the pending task's, what background work the
// process's own activity report named, whether the daemon held a report of that process that
// speaks of its last turn at all, and whether the record has been told. The next process is told
// it in the text of an accepted take: the sentence RelaunchSentence composes from it, in front of
// the task the claim holds or as a delivery of the daemon's own when it holds none
// (Delivery.message, queueRelaunch), and marked told by the accepted take whose text carried it
// (accept). This is the record that sentence is built from, written where the machine lets the
// process go (letGo, recordEnd) and persisted on the claim's row (store's ended_work), since that
// take can be a relaunch, a wake and a daemon restart away.
//
// The zero value is a claim whose let-go ended nothing worth telling, or that has been let go of
// none: Cause is empty.
type EndedWork struct {
	// At is when the process was let go, and Cause why: a sentence the daemon writes, beginning
	// with what the decision that let the process go found (the cause constants and functions
	// below), so the sentence it is told in can name it after "because".
	At    time.Time `json:"at"`
	Cause string    `json:"cause"`
	// Generation is the launch whose process this records the end of, which is what keeps one
	// process's end to one record: a decision that records the end of a process it already knows
	// is gone (died, displaced) and the let-go that follows it in the same decision or at a wake's
	// turn are two calls about one launch, and the second finds this launch's record standing
	// (recordEnd).
	Generation uint64 `json:"generation"`
	// Turn is whether a turn was running when the process was let go: the claim was working for
	// one, which is a turn the stream showed begin or one an earlier daemon's process was in. A
	// claim working for nothing that runs is in no turn (endState): one whose compaction has no
	// turn under it (compactStart puts the claim in working in no turn), and one whose held
	// suspension runs at the end of its turn or its compaction (suspendHeldAtEnd). TaskTurn is
	// whether the pending task's turn was cut off: the decision that let the process go took the
	// task back from its turn (interrupted, Machine.cutTurnOf), whatever the claim's state, or a
	// take-back that kept the process sent it back as a cut turn before this let-go (takeBack,
	// Machine.cutTurnOf), or a turn was running and it was the turn of the task's accepted take
	// (Delivery.startRecorded). So TaskTurn can stand without Turn, where the let-go reads the
	// claim after the turn that decision took the task back from has ended: a relaunch the pacing
	// parked lets the process go at a wake's turn (launchNow). A take that awaits its turn is in no
	// turn cut off where the claim's connection showed every frame of its turns: the worker ran
	// nothing of it, and it goes back with no note of an interruption. Where it did not, that turn
	// may have run unseen, so TaskTurn says it was cut off and the take goes back as a cut turn
	// (turnMayHaveRunUnseen, Machine.turnUnseen); and one whose report came with its finish refused
	// by the store goes back at no let-go, which finishes it by that report (takePhase,
	// waitFinish). A take whose turn ended with its report still to come, a compaction running
	// after it or not, is settled by its claim's contract (interrupted). For a plugin below
	// TaskTakeContract it is in no turn cut off: a let-go finishes it by its end frame rather than
	// take it back (finishTask), as settle finishes the task of the held stop at a turn's end and
	// the task an idle claim's let-go leaves it (suspendNow). For a plugin that names its number
	// only the report finishes it, and every let-go that can wait for the report waits instead
	// (reportwait.go); one that does not — a held suspension's deadline, the bound of that wait —
	// takes it back first, as a cut turn (reportMissed), and its mark says so (Machine.cutTurnOf),
	// while a death or a displacement records its end before it takes the task back, and names no
	// task's turn.
	Turn     bool `json:"turn,omitempty"`
	TaskTurn bool `json:"taskTurn,omitempty"`
	// Background is whether the process's own last activity report of its launch said background
	// work of its session was running, and Kinds the kinds that report named (the store keeps the
	// known ones alone: claim.BackgroundWork.KnownKinds). A report that said work ran and named no
	// known kind is Background with no Kinds. They are taken from that report however old it is: a
	// report read before the start of the process's last turn still says what background work ran
	// in that process, though it says nothing of that turn (Reported, recordEnd).
	Background bool     `json:"background,omitempty"`
	Kinds      []string `json:"kinds,omitempty"`
	// Reported is whether the store held an activity report of that launch (Deps.ClaimActivity)
	// that speaks of what the process last did: for a process that ran a turn, a report read no
	// earlier than the start of its last turn (Claim.LastTurnStart), since a report older than
	// that turn has said nothing about it — the idle rule's own rule (claim.ReadSessionWork).
	// Such a report still gives the record its Background and Kinds, so a record that is not
	// Reported can name background work. A record that is not Reported is one whose process ran a
	// turn and left no report of it — a Claude Code worker reports only at its Stop hook, so a turn
	// cut off before its end has none and the newest report is the turn before's, and a stale
	// plugin's report is refused — so what background work that turn started is unknown, as is all
	// of the process's where it left no report at all, which is said rather than read as nothing
	// (dispatch://INFRA-3276 R3, "unknown is not silence").
	Reported bool `json:"reported"`
	// Told is whether the record has been told to a later process, and ToldBy the delivery whose
	// accepted take told it, as accept states, or "" for an old session's record the abandonment
	// of that session ended (forgetSession). A record not yet told is replaced by a newer one only
	// as the union of the two (joined), so what the earlier let-go ended is not lost before anyone
	// reads it.
	Told   bool   `json:"told,omitempty"`
	ToldBy string `json:"toldBy,omitempty"`
}

// Tells is whether the next accepted take of this claim's agent must still carry the record: a
// let-go recorded something and no accepted take that carried it has kept it told since.
func (e EndedWork) Tells() bool { return e.Cause != "" && !e.Told }

// ranWork is whether the let-go ended work that was running: a turn, the task's turn, or
// background work a report named (Turn, TaskTurn, Background).
func (e EndedWork) ranWork() bool { return e.Turn || e.TaskTurn || e.Background }

// joined is newer recorded over e, an earlier record nobody has been told yet: the newer moment,
// cause and launch, and the union of what each ended — a turn either cut off, the task's turn
// either cut off, every background kind either named, and no report where either process left
// none. What the earlier process ended is still true and still untold, so the newer record keeps
// it rather than dropping it.
func (e EndedWork) joined(newer EndedWork) EndedWork {
	newer.Turn = newer.Turn || e.Turn
	newer.TaskTurn = newer.TaskTurn || e.TaskTurn
	newer.Background = newer.Background || e.Background
	kinds := slices.Clone(e.Kinds)
	for _, kind := range newer.Kinds {
		if !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	newer.Kinds = kinds
	newer.Reported = newer.Reported && e.Reported
	return newer
}

// ActivityReport is one launch's last activity report as the store holds it
// (PutClaimActivity's columns, the phase reminder aside): when the session read what it was
// doing, whether background work of its own was running, and the kinds it named.
type ActivityReport struct {
	At         time.Time
	Background bool
	Kinds      []string
}

// The causes a let-go records, in the daemon's words: each is what the decision that let the
// process go found, written so that it reads after "because" (EndedWork.Cause). noEnd is the
// cause of a let-go that records nothing: a launch of a claim that records no process to let go
// — a spawn, a resume, a retry, and every relaunch whose process an earlier step of the same
// recovery already let go — and a retirement that ends the claim's work on purpose, the tree's
// or the issue's close and the agent's own exit (retire). endStopped is a stop's (RequestStop:
// `legion claims stop`, the operator's close of a tree stopping its other agents, and the daemon
// stopping a controller it launched once the deployment leaves the controller to its operator),
// since every retry that keeps the session relaunches a stopped agent on the session its claim
// records — the operator's (`legion claims retry`: RequestAskedRetry for an architect or a worker
// whose issue works no phase of its role, and the workflow's start of that phase, below, for a
// worker in its phase), the controller keeper's retry of a retired controller (daemon's
// controllerKeeper.keep, RequestRetry) and the workflow's start of a retired worker's phase
// (daemon's outbox.supervise, RequestRetry) — and that session's next turn reads what the stop
// ended (dispatch://INFRA-3276 R3). endUnanswered is the relaunch at the reconnect deadline of a
// shim that came back while its agent never said whether it was in a turn (reconnectDeadline,
// connview.go).
//
// The cause is read into an agent's task text as it stands (RelaunchSentence), so every word of it
// that is not the daemon's own — the runtime's account of a death, the failure sentence that can
// carry a provider's or a launcher's text — is quoted as a Go string (strconv.Quote), as
// abandonedTask quotes the provider's reason: the quotes bound it, and a newline or a quote inside
// it is escaped rather than read as a sentence of the daemon's own.
const (
	noEnd          = ""
	endRetired     = "the daemon retired it because it did not act on its task in time"
	endStalePlugin = "it ran an older plugin and was started again on the current one"
	endDisplaced   = "its pod was replaced"
	endRepointed   = "its address changed"
	endStopped     = "its claim was stopped"
	endUnanswered  = "its agent did not say whether it was in a turn after its connection came back"
)

// endDied is a death's cause, with the runtime's own account of it (Claim.LastDeathCause), quoted.
func endDied(cause string) string { return "the process died: " + strconv.Quote(cause) }

// endStalled is the stall backstop's cause, with the bound the claim's last step passed. It names
// a turn only where the stop finds one (inTurn: the claim reads working, endState). The daemon's
// sweep posts over any claim stored working past its bound (the daemon's backstopStalledTurns),
// and that includes a claim working for a compaction with no turn started under it, which endState
// reads as no turn: there it is the claim that showed no progress, so the sentence the next process
// reads never says a turn stalled and then that it was in none (relaunchSentence).
func endStalled(bound time.Duration, inTurn bool) string {
	if inTurn {
		return fmt.Sprintf("its turn showed no progress for %s", bound)
	}
	return fmt.Sprintf("its claim showed no progress for %s", bound)
}

// endSuspended is a suspension's cause, naming who suspended the claim.
func endSuspended(by SuspendSource) string { return fmt.Sprintf("it was suspended (%s)", by) }

// endFailed is a failure's cause, with the sentence the failure is recorded under (fail), quoted:
// it can carry the provider's refusal or the launcher's error.
func endFailed(why string) string { return "its claim failed: " + strconv.Quote(why) }

// endNeverReady is the cause of the registration deadline and of the reconnect deadline, with
// what its agent never did (registrationDeadline, reconnectDeadline), and then that the daemon
// retired the process for it: the cause reads after "because" and carries no "because" of its own.
func endNeverReady(never string) string { return "its agent " + never + ", so the daemon retired it" }

// recordEnd records what letting the claim's process go ends, for cause, before anything of the
// process is cleared: letGo calls it first, and so do the two decisions that find the process
// already gone (died, displaced), before they take its task back, since what they find is the
// whole of what that process's end did and the let-go they lead to can be a wake's turn away.
//
// It records nothing for a claim that records no process, for noEnd, and for a launch whose end
// is recorded already (EndedWork.Generation). Otherwise it reads what it decides on: whether a
// turn was running (the claim is working for one: endState), whether the pending task's turn was
// cut off (this let-go's own decision took the task back from its turn, whatever the claim's
// state: cutTurnOf; or the turn that was running was the turn of the task's accepted take), and
// the launch's own activity report from the store (launchActivity), read here because the
// machine's claim holds only what it read at its own boot and because the launch that follows
// clears it (store's putClaim). Of a process that ran a turn, a report read before that turn
// began (Claim.LastTurnStart) is no report of that turn, as the idle rule holds
// (claim.ReadSessionWork): the record is not Reported, rather than reading the turn before's
// "nothing runs" as this one's. It still takes that report's background work, which the report
// saw running in this process, so the sentence of a turn cut off names that work and then says
// that the daemon has no report of what the turn started (relaunchSentence). Nothing is written
// when no turn was running, no task was taken back from its turn and a report of the last turn
// says nothing runs, or when no turn was running, no task was taken back from its turn and none
// had run in this process, which can have started no background work; a process that ran a turn
// and left no report of it is recorded as unreported rather than as having ended nothing. A
// take-back from a turn always leaves a record of its let-go, since the task it took is taken
// again behind that record's sentence (Delivery.message). A record still untold is replaced as
// the union of the two (joined).
//
// A claim stored working is in a turn, except where it is working for nothing that runs
// (endState): while a compaction runs with no turn started under it, since compactStart puts the
// claim in working in no turn; once a gone verdict has dropped such a compaction with its process
// (compactionLostProcess), until the let-go, which a parked relaunch makes a wake's turn after the
// death or the displacement that calls this first; and while a held suspension runs at the end of
// the turn or the compaction the claim was working for (suspendHeldAtEnd). The let-go there cuts
// off no turn: the record is not Turn, its task is in no turn cut off unless the let-go's own
// decision takes it back (above), and whether the process ran a turn is read from the state that
// compaction or that end gives the claim back in, ready or idle. A turn Oh My Pi started under
// the compaction (compacting.inTurn) is a turn as any other.
//
// A record written here is new words, so only a take accepted from here on tells it (accept): a
// take accepted before it carried none of this record's words.
//
// Before anything else it reads whether a take awaiting its turn may have had that turn run with
// no frame of it reaching the machine (turnMayHaveRunUnseen: the claim's connection not showing
// every frame of its turns, or the take marked so), and keeps that as the take's mark
// (Machine.turnUnseen, setTurnUnseen): a writer of the mark beside markTurnUnseen, which also
// clears a mark where the take no longer awaits its turn. So the take-back of the let-go — made
// after this, by a death or a displacement (died, displaced), and after letGo has made the view
// whole for the next process, by a retirement or a suspension (takeBackUnstarted) — takes it back
// as a turn cut off, as the record here says it was. Then it gives back what an accepted take
// that awaits its turn told (untellUnstarted): no turn the daemon saw ran that take's text, and
// the let-go takes it back, so the record that take had marked told is untold again and the record
// written here joins it.
//
// It writes nothing itself: the record is a field of the claim, and the let-go's own write — the
// launch's, the suspension's, the failure's, the retirement's, or the death's charge — carries
// it, and the untelling with it. So it costs the relaunch one read and waits for nothing else.
func (m *Machine) recordEnd(ctx context.Context, cause string) {
	m.setTurnUnseen(m.turnMayHaveRunUnseen())
	m.untellUnstarted()
	if cause == noEnd || m.claim.Locator == nil {
		return
	}
	if held := m.claim.EndedWork; held.Cause != "" && held.Generation == m.claim.Generation {
		return
	}
	state := m.endState()
	ended := EndedWork{Cause: cause, Generation: m.claim.Generation, Turn: state == StateWorking}
	// The pending task's turn was cut off where this let-go's own decision takes the task back
	// from its turn (interrupted, cutTurnOf), whatever the claim's state, where a take-back that
	// kept the process sent it back as a cut turn before this let-go (takeBack, cutTurnOf), and
	// where a turn was running over a take whose turn ran, as the one rule of where the take
	// stands reads it (takesTurnRan). A take that awaits its turn is in no turn cut off: the
	// worker ran nothing of it, unless that turn may have run with no frame of it reaching the
	// machine (turnMayHaveRunUnseen), which the take-back that follows reads as a turn cut off. Nor
	// is a take of a plugin below TaskTakeContract whose turn ended with its report still to come,
	// which a let-go finishes rather than take back (interrupted), the take of the turn whose end
	// runs a held suspension (suspendHeldAtEnd) and that of an idle claim's let-go that takes
	// nothing back (suspendNow) among them, whose task settle finishes. Such a take of a plugin
	// that names its number is in no turn cut off either where the let-go records before its
	// take-back (died, displaced); where the let-go's own decision takes it back first, as a held
	// suspension's deadline and a stale-plugin relaunch's bound do, the mark says it was
	// (cutTurnOf).
	if p := m.claim.Pending; p != nil {
		ended.TaskTurn = (m.cutTurnOf != "" && p.ID == m.cutTurnOf) || (ended.Turn && m.takesTurnRan()) || m.turnUnseen
	}
	// A claim idle or working has run a turn in this process: idle is reached only by a turn's
	// end, and a relaunched process comes up through ready. A claim working for a compaction in
	// no turn has run one where the compaction took it from idle (endState).
	ranTurn := state == StateWorking || state == StateIdle
	report, reported := m.launchActivity(ctx)
	if reported && report.Background {
		ended.Background, ended.Kinds = true, slices.Clone(report.Kinds)
	}
	// A report read before the claim's last turn began has said nothing about that turn
	// (claim.ReadSessionWork), so it is no report of it, though the background work it saw is
	// kept above. LastTurnStart is the daemon's first word of the turn: its agent_start as read
	// (turnStarted). A daemon restarted under a working claim reads the one the daemon before it
	// stored.
	ended.Reported = reported && !(ranTurn && report.At.Before(m.claim.LastTurnStart))
	if !ended.ranWork() && (ended.Reported || !ranTurn) {
		return
	}
	ended.At = m.deps.Clock.Now()
	m.log.Warn("supervise: the relaunch ended work the agent had running",
		"cause", cause, "generation", ended.Generation, "turn", ended.Turn, "taskTurn", ended.TaskTurn,
		"background", ended.Background, "kinds", ended.Kinds, "reported", ended.Reported,
		"delivery", pendingID(m.claim.Pending))
	if held := m.claim.EndedWork; held.Tells() {
		ended = held.joined(ended)
	}
	m.claim.EndedWork = ended
}

// endState is the claim's state as a let-go reads it for the turn it cuts off (recordEnd): the
// claim's own, except for a claim stored working for nothing that runs, which reads as the state
// it is given back in. That is the state a held suspension running at the end of the turn or the
// compaction the claim was working for finds it in (Machine.heldStopBack); and, for a claim working
// for a compaction with no turn started under it, the compaction in flight or the one a gone
// verdict dropped with its process (Machine.compacting, Machine.lostCompaction), the state that
// compaction took the claim from, or idle once a turn ended under it (compacting.back). A turn
// Oh My Pi started under the compaction (compacting.inTurn) is a turn, and reads working. A
// claim no longer working — the turn of a dead connection that ended after its compaction was
// dropped (compactionLostProcess) — reads as it stands.
func (m *Machine) endState() ClaimState {
	switch {
	case !m.busy():
		return m.claim.State
	case m.heldStopBack != "":
		return m.heldStopBack
	case m.compacting != nil && !m.compacting.inTurn:
		return m.compacting.back
	case m.compacting == nil && m.lostCompaction != nil && !m.lostCompaction.inTurn:
		return m.lostCompaction.back
	}
	return StateWorking
}

// cutsTurn is whether a let-go now cuts off a turn that runs: the claim in a turn (inTurn) as a
// let-go reads it (endState), which knows two moments a claim is stored working for nothing that
// runs where inTurn reads a turn — the held suspension's stop at the end of the turn or the
// compaction it was held for (Machine.heldStopBack), and a compaction a gone verdict dropped with
// its process (Machine.lostCompaction). During a compaction with no turn started under it, the two
// agree: no turn runs. The take-back of a let-go reads it (interrupted), and so does the stall
// backstop's record; recordEnd reads the state it is read from.
func (m *Machine) cutsTurn() bool { return m.endState() == StateWorking }

// launchActivity is the activity report the store holds for the launch the claim records now
// (Deps.ClaimActivity), and whether it holds one. Only a report of this launch counts: the store
// writes a report onto the launch it was authenticated for alone, and clears the report on the
// write that moves the generation, so a report of an earlier launch is never this one's. Whether
// the report it reads speaks of the turn the process last ran is recordEnd's to decide: one read
// before that turn began is no report of that turn (claim.ReadSessionWork), though the background
// work it says ran is kept.
//
// The read is bounded as the writes of a decision are (bounded), so a store that has stopped
// answering costs the record and never holds the let-go: a read that fails, and a daemon that
// wires no read, are a launch with no report, which the record says rather than reading it as
// nothing running.
func (m *Machine) launchActivity(ctx context.Context) (ActivityReport, bool) {
	if m.deps.ClaimActivity == nil {
		return ActivityReport{}, false
	}
	reading, cancel := m.bounded(ctx)
	defer cancel()
	report, held, err := m.deps.ClaimActivity(reading, m.claim.Token, m.claim.Generation)
	if err != nil {
		m.log.Warn("supervise: the activity report of the process being let go could not be read; its end records no report",
			"generation", m.claim.Generation, "error", err)
		return ActivityReport{}, false
	}
	return report, held
}
