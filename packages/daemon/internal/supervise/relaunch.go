package supervise

import (
	"context"
	"crypto/rand"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// RelaunchSentence is what a claim's next process is told of what the last let-go of its process
// ended (EndedWork, dispatch://INFRA-3276 R3), where the sentence stands alone: the delivery of the
// daemon's own that a claim holding no task is given at its ready (queueRelaunch), and a root
// architect's catch-up, which the workflow is handed through the ready's report
// (intake.ClaimReady.Relaunch, workflow's QueueCatchUp). In front of a task the same record is told
// by the same composer, ending in the task's own instruction instead (relaunchSentence,
// Delivery.message).
//
// It reads ended as the let-go recorded it, so the caller asks whether there is anything to tell
// (EndedWork.Tells) before it asks for the words.
func RelaunchSentence(ended EndedWork) string { return relaunchSentence(ended, false) }

// relaunchSentence is the one composer of the sentence a record of a let-go is told in, for the
// sentence alone (RelaunchSentence) and in front of a task (task true: Delivery.message, and
// interruptedTask's record of a turn cut off). Another agent reads it, so it follows ASD-STE100,
// as providerErrorTask does: short sentences, one statement each. In order, it says:
//
//   - why the process ended, in the record's own words (EndedWork.Cause), which read after
//     "because" and quote whatever of them is not the daemon's (endDied, endFailed); for a task
//     whose turn the let-go cut off it says first that this turn was interrupted;
//   - whether a turn was cut off, and that its tool calls in flight were never answered, or that
//     the agent was in no turn;
//   - for a turn cut off, what of the background work ended: first each kind of it the record
//     names (EndedWork.Kinds), all of which ended with the process, or background work of no
//     named kind — a report older than the turn still names what ran in the process (recordEnd);
//     then, for a record that holds no report of that turn (EndedWork.Reported: the process left
//     none, or its newest report is older than the turn's start), that the daemon has none; then
//     that the subagents and background jobs of that turn are gone, which is said of every turn
//     cut off, since no report shows all that a turn started: Oh My Pi reads its background work
//     every five seconds (pi-legion's SESSION_ACTIVITY_PERIOD_MS), so even a report read after
//     the turn began can miss what it started in its last seconds; and last, for a record that
//     holds no report of the turn and names no background work at all, that every other subagent
//     and background job the process started is gone too;
//   - with no turn cut off, each kind of background work the record names; then, for a record
//     that holds no report of the process's last turn (EndedWork.Reported), that the daemon has
//     no report of the background work of that turn, so every subagent and background job the
//     agent started in that process is gone. That is true of a process that left no report, of
//     one whose only report is older than its last turn, whether that report named background
//     work or nothing (recordEnd), and of a record joined from such a one (joined): what that
//     turn started is unknown, and unknown is said, never read as nothing;
//   - that no result of that work will come, and that the transcript does not say so, since the
//     resumed session shows neither a lost tool call nor a lost subagent as missing
//     (dispatch://INFRA-714 R6);
//   - what to do: check what the work did before doing it again, and for a task, then continue it.
//
// The two statements the sentence of an interrupted task made before this record existed — the
// tool calls in flight never answered, and the subagents and background jobs of that turn gone —
// keep their words, which agents and the daemon's own tests read, and are said of every turn cut
// off, as they were then.
//
// The words of the background work are the three kinds the store keeps (backgroundKind); a kind
// it does not know is not read into the prompt, and a report that named none of them is said as
// background work of no named kind.
func relaunchSentence(ended EndedWork, task bool) string {
	cutTask := task && ended.TaskTurn
	process := "Your previous process"
	if cutTask {
		process = "Your process"
	}
	because := process + " ended, and the daemon recorded no cause for it."
	if ended.Cause != "" {
		because = process + " ended because " + ended.Cause + "."
	}
	turn := ended.Turn || ended.TaskTurn
	var said []string
	switch {
	case cutTask:
		said = append(said, "Your previous turn on this task was interrupted.", because)
	case turn:
		said = append(said, because, "The turn you were in was interrupted.")
	default:
		said = append(said, because, "You were not in a turn then.")
	}
	// stopped is what the record names of the background work that ended with the process, or ""
	// for a record that names none.
	var stopped string
	switch kinds := backgroundKinds(ended.Kinds); {
	case kinds != "":
		stopped = "Each " + kinds + " that you started stopped with that process."
	case ended.Background:
		stopped = "The background work that you started stopped with that process."
	}
	switch {
	case turn:
		said = append(said, "Your tool calls in flight were never answered.")
		if stopped != "" {
			said = append(said, stopped)
		}
		if ended.Reported {
			said = append(said, "The subagents and background jobs of that turn are gone.")
			break
		}
		said = append(said, "The daemon has no report of the background work of that turn.",
			"So the subagents and background jobs of that turn are gone.")
		if stopped == "" {
			said = append(said, "Every other subagent and background job that you started in that process is gone too.")
		}
	case !ended.Reported:
		// No report speaks of the process's last turn: it left none, its only report is older than
		// that turn and names the work that ran then or nothing (recordEnd), or the record is
		// joined from such a one. What that turn started is unknown.
		if stopped != "" {
			said = append(said, stopped)
		}
		said = append(said, "The daemon has no report of the background work of your last turn.",
			"So every subagent and background job that you started in that process is gone.")
	case stopped != "":
		said = append(said, stopped)
	}
	said = append(said, "No result of that work will come to you.", "Your transcript does not say so.")
	if task {
		said = append(said, "Before you do any of that work again, check what it already did.", "Then continue the task.")
	} else {
		said = append(said, "Before you start any of that work again, check what it already did.")
	}
	return strings.Join(said, " ")
}

// backgroundKinds names each kind of background work a report named, in the report's order, for
// the sentence "Each … that you started stopped with that process": "subagent and each background
// job", and "" for a report that named none of the three kinds the store keeps
// (claim.BackgroundWork.KnownKinds).
func backgroundKinds(kinds []string) string {
	var named []string
	for _, kind := range kinds {
		if words := backgroundKind(kind); words != "" {
			named = append(named, words)
		}
	}
	return strings.Join(named, " and each ")
}

// backgroundKind is the words an agent reads for one kind of background work the session reports
// (pi-shared's session-activity): a `task` subagent, an `async` job of the session's own, and a
// background task of a Claude Code worker.
func backgroundKind(kind string) string {
	switch kind {
	case "subagent":
		return "subagent"
	case "async-job":
		return "background job"
	case "claude-background-task":
		return "Claude Code background task"
	}
	return ""
}

// relaunchTaskPrefix starts the delivery id of the delivery of the daemon's own that tells a
// claim holding no task what its relaunch ended (queueRelaunch); a fresh word of its own completes
// it (relaunchTaskID). The prefix is the delivery's whole identity: it is what says the delivery
// carries the record's sentence as its text rather than in front of a task (Delivery.message),
// and that a newer task may take its place while no prompt of it is waiting on a result
// (replaceable). It is kept across every take-back, as an update round's ask keeps its round
// (rotateTaskID).
const relaunchTaskPrefix = "relaunch:"

// relaunchTaskID is the id of one delivery of the daemon's own that tells a relaunch's record.
// Its word is random, as an operator's own delivery id is (queue), for the reason updateTaskID
// gives: the shim answers a prompt whose id it holds from its own record without prompting the
// agent, so each delivery and each take-back needs an id the shim has not seen.
func relaunchTaskID() string { return relaunchTaskPrefix + rand.Text() }

// relaunchTask is whether delivery id is the daemon's own delivery of a relaunch's record.
func relaunchTask(id string) bool { return strings.HasPrefix(id, relaunchTaskPrefix) }

// Relaunch is whether d is the daemon's own delivery of a relaunch's record (queueRelaunch): no
// work of the claim's — no phase, no run, no entry — only the sentence of what the last
// let-go of its process ended. A reader that asks whether a claim holds work it was given asks
// HoldsTask.
func (d Delivery) Relaunch() bool { return relaunchTask(d.ID) }

// HoldsTask is whether pending, the delivery a claim holds (Claim.Pending, or the pending task a
// role run reads, record.RoleClaim.Pending), is a task of the claim's: a delivery, and not the
// daemon's own delivery of a relaunch's record (Delivery.Relaunch), which is no work of the
// claim's. It is the one statement of that rule, and every reader that asks whether a claim still
// owes work it was given asks it: a death's charge (chargeDeath), the workflow's owed and
// readyWorkerTask, and api's freshSession. It takes the pointer the claim holds, nil for none,
// because those readers hold two types of claim and the delivery is the one field they share.
func HoldsTask(pending *Delivery) bool { return pending != nil && !pending.Relaunch() }

// queueRelaunch gives a claim that holds no delivery at its ready a delivery of the daemon's own
// that carries the sentence of its untold record alone (RelaunchSentence), so the next prompted
// turn of the new process reads what the last one ended at once, whatever the role: a root
// architect's goal-red-team reading that died while it held nothing is the measured case
// (dispatch://INFRA-3276 R3). A claim that holds a delivery needs none: its task carries the
// sentence in front of it (Delivery.message).
//
// The delivery has no phase, no run, no entry and no look, so nothing holds it back
// (phaseHolds, asksRoot), no working copy is adopted for it (adopts), and its confirmation and
// retirement leave what the claim is serving alone and end no phase (serving, settle). Its id is
// relaunchTaskID's. The record is told when a turn confirms a prompt of it (confirm), as a task's
// would be, and its text is the record as it stands when the prompt goes (Delivery.message).
//
// It is queued by finishReady after the ready's durable write and report, which the workflow reads
// synchronously (Machine.OnDurableState, the daemon's applyTerminal): the workflow's ready reads
// the claim holding nothing, so a phase worker is still queued its resume task
// (workflow's readyWorkerTask), and that task takes this delivery's place while no prompt of it is
// waiting on a result, or waits for its turn to end (queue, replaceable). It is queued again where
// sendPending drops the task a claim held and leaves it holding nothing (relaunchAfterDrop), since
// the task that would have carried the sentence is gone.
//
// One claim is given none: the daemon-launched controller. Its keeper sends its start message at
// every ready and only logs a refusal of it (the daemon's controllerKeeper.ready), so a delivery
// queued here would make the start procedure wait behind a sentence; the start message is a
// delivery, which carries the sentence in front of it instead. A claim whose fresh session has not
// yet told the session it abandoned (Claim.Abandoned) is given one as any other is: a record that
// tells here is one of the fresh session's own processes (forgetSession), and the delivery carries
// the abandoned session's sentence and then the record's (Delivery.message).
func (m *Machine) queueRelaunch(ctx context.Context) error {
	ended := m.claim.EndedWork
	if !ended.Tells() || m.claim.Pending != nil || m.claim.Role == claim.RoleController {
		return nil
	}
	m.log.Info("supervise: the claim holds no task; a delivery of the daemon's own tells its agent what the relaunch ended",
		"cause", ended.Cause, "ended", ended.Generation)
	return m.queue(ctx, RequestDeliver{Claim: m.claim.Token, ID: relaunchTaskID(), Task: RelaunchSentence(ended)})
}
