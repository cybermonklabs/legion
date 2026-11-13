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
//   - each kind of background work the process's own last report named, all of which ended with
//     it, or — for a process that left no report (EndedWork.Reported) — that the daemon has no
//     report of it, so the subagents and background jobs of the turn, and every other one the
//     process started, are gone: unknown is said, never read as nothing;
//   - that no result of that work will come, and that the transcript does not say so, since the
//     resumed session shows neither a lost tool call nor a lost subagent as missing
//     (dispatch://INFRA-714 R6);
//   - what to do: check what the work did before doing it again, and for a task, then continue it.
//
// The two statements the sentence of an interrupted task made before this record existed — the
// tool calls in flight never answered, and the subagents and background jobs of that turn gone —
// keep their words, which agents and the daemon's own tests read.
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
	if turn {
		said = append(said, "Your tool calls in flight were never answered.")
	}
	switch kinds := backgroundKinds(ended.Kinds); {
	case !ended.Reported && turn:
		said = append(said, "The daemon has no report of your background work in that process.",
			"So the subagents and background jobs of that turn are gone.",
			"Every other subagent and background job that you started in that process is gone too.")
	case !ended.Reported:
		said = append(said, "The daemon has no report of your background work in that process.",
			"So every subagent and background job that you started in that process is gone.")
	case kinds != "":
		said = append(said, "Each "+kinds+" that you started stopped with that process.")
	case ended.Background:
		said = append(said, "The background work that you started stopped with that process.")
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
// work of the claim's — no phase, no run, no assignment — only the sentence of what the last
// let-go of its process ended. A reader that asks whether a claim still owes work it was given
// reads such a delivery as none (workflow's owed), as a death's charge does (chargeDeath).
func (d Delivery) Relaunch() bool { return relaunchTask(d.ID) }

// queueRelaunch gives a claim that holds no delivery at its ready a delivery of the daemon's own
// that carries the sentence of its untold record alone (RelaunchSentence), so the next prompted
// turn of the new process reads what the last one ended at once, whatever the role: a root
// architect's goal-red-team reading that died while it held nothing is the measured case
// (dispatch://INFRA-3276 R3). A claim that holds a delivery needs none: its task carries the
// sentence in front of it (Delivery.message).
//
// The delivery has no phase, no run, no assignment and no look, so nothing holds it back
// (phaseHolds, asksRoot), no working copy is adopted for it (adopts), and its confirmation and
// retirement leave what the claim is serving alone and end no phase (serving, settle). Its id is
// relaunchTaskID's. The record is told when a turn confirms a prompt of it (confirm), as a task's
// would be, and its text is the record as it stands when the prompt goes (Delivery.message).
//
// It is queued by finishReady after the ready's durable write and report, which the workflow reads
// synchronously (Machine.OnDurableState, the daemon's applyTerminal): the workflow's ready reads
// the claim holding nothing, so a phase worker is still queued its resume task
// (workflow's readyWorkerTask), and that task takes this delivery's place while no prompt of it is
// waiting on a result, or waits for its turn to end (queue, replaceable).
//
// Two claims are given none. The daemon-launched controller's keeper sends its start message at
// every ready and only logs a refusal of it (the daemon's controllerKeeper.ready), so a delivery
// queued here would make the start procedure wait behind a sentence; the start message is a
// delivery, which carries the sentence in front of it instead. And a claim whose fresh session has
// not yet told the session it abandoned (Claim.Abandoned): that sentence takes precedence, says
// that everything of the old session is gone, and waits for the first task, as it always has.
func (m *Machine) queueRelaunch(ctx context.Context) error {
	ended := m.claim.EndedWork
	if !ended.Tells() || m.claim.Pending != nil || m.claim.Role == claim.RoleController || m.claim.Abandoned.Tells() {
		return nil
	}
	m.log.Info("supervise: the claim holds no task at its ready; a delivery of the daemon's own tells its agent what the relaunch ended",
		"cause", ended.Cause, "ended", ended.Generation)
	return m.queue(ctx, RequestDeliver{Claim: m.claim.Token, ID: relaunchTaskID(), Task: RelaunchSentence(ended)})
}
