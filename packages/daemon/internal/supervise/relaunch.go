package supervise

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// RelaunchNote is the relaunch note of issue (texts.RelaunchNoteRecord): what the last let-go of
// the claim's process ended, as the claim's record of it holds it (EndedWork.Facts), in front of a
// task (task true: Delivery.message) or alone, as the text of the daemon's own delivery of it to a
// claim that holds no task (queueRelaunch). The steps an agent takes on reading it are its role
// prompt's.
func RelaunchNote(issue string, ended EndedWork, task bool) texts.RelaunchNoteRecord {
	return texts.RelaunchNoteRecord{
		TaskHead:      texts.TaskHead{Kind: texts.RelaunchNote, Issue: issue},
		RelaunchFacts: ended.Facts(task),
	}
}

// Facts is the record as the relaunch note states it (texts.RelaunchFacts), in front of a task
// where task is true:
//
//   - cause: why the process ended, in the record's own words (EndedWork.Cause), which quote
//     whatever of them is not the daemon's (endDied, endFailed);
//   - in_turn: a turn was cut off. A record of the task's turn with no turn running is one whose
//     let-go's own decision took the task back while the claim was in no turn, as from a turn
//     that ended before its prompt's result came (recordEnd): the task's turn is the one cut off,
//     and it is told as one;
//   - task_turn: in front of a task, the turn cut off was that task's (EndedWork.TaskTurn), which
//     Delivery.message hands only in front of the task that let-go took back (inFrontOf); alone,
//     with no task, the turn cut off is only the turn the agent was in;
//   - in_flight_tool_calls and turn_background_work: what every turn cut off lost, said wherever
//     in_turn is (texts.ToolCallsNeverAnswered, texts.TurnBackgroundWorkGone): its tool calls in
//     flight were never answered, and the subagents and background jobs of that turn are gone,
//     whatever report the record holds, for the reason reported gives below;
//   - process_background_work: every subagent and background job started in the process is gone
//     (texts.ProcessBackgroundWorkGone), said where main's note said it: where no report speaks of
//     the process's last turn (reported, below), for a process in no turn, and for a turn cut off
//     whose record names no work that stopped (no stopped, no background). A report, or a turn cut
//     off whose record names what stopped, says no more of the process's work than the record
//     names;
//   - stopped: each kind of background work the record names, all of which ended with the
//     process, in the store's own words for them (`subagent`, `async-job`,
//     `claude-background-task`: claim.BackgroundWork.KnownKinds); a kind the store does not know
//     is not read into the prompt;
//   - background: background work of no named kind ended: a report said work ran and named none
//     of the known kinds (EndedWork.Background with no Kinds);
//   - reported: the record holds a report of the process's last turn (EndedWork.Reported). No
//     report is unknown, which is said rather than read as nothing: the process left none, its
//     only report is older than its last turn and names the work that ran then or nothing, or the
//     record is joined from such a one (joined). Even a report read after a turn began can miss
//     what it started in its last seconds, since Oh My Pi reads its background work every five
//     seconds (pi-legion's SESSION_ACTIVITY_PERIOD_MS), so the subagents and background jobs of
//     every turn cut off are gone whatever the report says;
//   - task: the facts go in front of a task.
//
// Every string is made valid UTF-8 (validText), so the note always renders.
func (e EndedWork) Facts(task bool) texts.RelaunchFacts {
	stopped := claim.BackgroundWork{Kinds: e.Kinds}.KnownKinds()
	facts := texts.RelaunchFacts{
		Cause:      validText(e.Cause),
		InTurn:     e.Turn || e.TaskTurn,
		TaskTurn:   task && e.TaskTurn,
		Stopped:    stopped,
		Background: e.Background && len(stopped) == 0,
		Reported:   e.Reported,
		Task:       task,
	}
	if facts.InTurn {
		facts.InFlightToolCalls, facts.TurnBackgroundWork = texts.ToolCallsNeverAnswered, texts.TurnBackgroundWorkGone
	}
	if !facts.Reported && (!facts.InTurn || (len(facts.Stopped) == 0 && !facts.Background)) {
		facts.ProcessBackgroundWork = texts.ProcessBackgroundWorkGone
	}
	return facts
}

// inFrontOf is the record as the note in front of task d tells it (Delivery.message). The turn
// of the claim's task the record says was cut off (EndedWork.TaskTurn) is d's own only where d is
// the task the let-go took back from that turn (Delivery.Interrupted, interrupted): a let-go that
// takes nothing back leaves the task a turn confirmed to settle, which retires it, and a
// confirmed task is never sent again. So any other task was never in that turn: the operator's
// retry of a worker a stop cut off in its task's turn is the case, since the stop retires that
// task and the retry gives the claim a new task of its phase (api's retryStart). In front of such
// a task the turn cut off is told as the note alone tells it: as the turn the agent was in, never
// as this task's.
func (e EndedWork) inFrontOf(d Delivery) EndedWork {
	if !d.Interrupted {
		e.Turn, e.TaskTurn = e.Turn || e.TaskTurn, false
	}
	return e
}

// partText is a part a start puts before a task, or the daemon's own delivery of one, as its
// kind's record renders it (texts.Render). The records of these parts hold strings, booleans, an
// integer and a list of strings, and each builder makes every string of them valid UTF-8
// (validText), so Render refuses none of them; a refusal would be a defect of a record type,
// which this package's tests render, and it panics naming the type.
func partText(record any) string {
	text, err := texts.Render(record)
	if err != nil {
		panic(fmt.Sprintf("supervise: the part %T does not render: %v", record, err))
	}
	return text
}

// validText is s with each byte that is not UTF-8 replaced by U+FFFD, the one string a record's
// renderer refuses (texts.Render): a cause or a reason can carry the runtime's or the provider's
// own bytes.
func validText(s string) string { return strings.ToValidUTF8(s, "\uFFFD") }

// relaunchTaskPrefix starts the delivery id of the delivery of the daemon's own that tells a
// claim holding no task what its relaunch ended (queueRelaunch); a fresh word of its own completes
// it (relaunchTaskID). The prefix is the delivery's whole identity: it is what says the delivery
// carries the record's note as its text rather than in front of a task (Delivery.message),
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
// work of the claim's — no phase, no run, no entry — only the note of what the last let-go of its
// process ended. A reader that asks whether a claim holds work it was given asks HoldsTask.
func (d Delivery) Relaunch() bool { return relaunchTask(d.ID) }

// HoldsTask is whether pending, the delivery a claim holds (Claim.Pending, or the pending task a
// role run reads, record.RoleClaim.Pending), is a task of the claim's: a delivery, and not the
// daemon's own delivery of a relaunch's record (Delivery.Relaunch), which is no work of the
// claim's. It is the one statement of that rule, and every reader that asks whether a claim holds
// a task to send or to take asks it: a death's charge (chargeDeath), the machine's refusal of a
// task of news alone behind another task (queue), the daemon's reconcile, which queues no news
// behind a task not yet taken, and api's heldTaskNotCurrent, which then tells a nil from a
// relaunch's delivery only to choose its reason text. A reader that asks whether the claim holds
// the work of its phase asks HoldsWork. It takes the pointer the claim holds, nil for none,
// because those readers hold two types of claim and the delivery is the one field they share.
func HoldsTask(pending *Delivery) bool { return pending != nil && !pending.Relaunch() }

// HoldsWork is whether pending is a task that hands the claim work: a task (HoldsTask) other than
// one of news alone (Delivery.NewsOnly), which hands the worker only the news of the approved
// design. Every reader that asks whether a claim still owes the work it was given asks it: the
// workflow's owed and readyWorkerTask, and so the admission sweep's reading of a root architect's
// start on its way (workflow.ArchitectStartPending), and api's freshSession. A claim that holds a
// task of news alone is read as owing no work, so the phase's task is sent to it as well, and is
// lost to neither: a task of an entry takes the news task's place while no turn has taken it,
// carrying that news itself (supervise's queue, Machine.supersedes), and any other task waits
// behind the news task until a turn has taken it.
func HoldsWork(pending *Delivery) bool { return HoldsTask(pending) && !pending.NewsOnly }

// queueRelaunch gives a claim that holds no delivery at its ready a delivery of the daemon's own
// that carries the note of its untold record alone (RelaunchNote, with no task), so the next
// prompted turn of the new process reads what the last one ended at once, whatever the role: a
// root architect's goal-red-team reading that died while it held nothing is the measured case
// (dispatch://INFRA-3276 R3). A claim that holds a delivery needs none: its task carries the note
// in front of it (Delivery.message).
//
// The delivery has no phase, no run, no entry and no look, so nothing holds it back
// (phaseHolds, asksRoot), no working copy is adopted for it (adopts), and its confirmation and
// retirement leave what the claim is serving alone and end no phase (serving, settle). Its id is
// relaunchTaskID's. The record is told when a turn confirms a prompt of it (confirmCarrying), as a
// task's would be, and its text is the record as it stands when the prompt goes (Delivery.message).
//
// It is queued by finishReady after the ready's durable write and report, which the workflow reads
// synchronously (Machine.OnDurableState, the daemon's applyTerminal): the workflow's ready reads
// the claim holding nothing, so a phase worker is still queued its resume task
// (workflow's readyWorkerTask), and that task takes this delivery's place while no prompt of it is
// waiting on a result, or waits for its turn to end (queue, replaceable). It is queued again where
// sendPending drops the task a claim held and leaves it holding nothing (relaunchAfterDrop), since
// the task that would have carried the note is gone.
//
// One claim is given none: the daemon-launched controller. Its keeper sends its start message at
// every ready and only logs a refusal of it (the daemon's controllerKeeper.ready), so a delivery
// queued here would make the start procedure wait behind a note; the start message is a delivery,
// which carries the note in front of it instead. A claim whose fresh session has not yet told the
// session it abandoned (Claim.Abandoned) is given one as any other is: a record that tells here is
// one of the fresh session's own processes (forgetSession), and the delivery carries the
// abandoned session's note and then the record's (Delivery.message).
func (m *Machine) queueRelaunch(ctx context.Context) error {
	ended := m.claim.EndedWork
	if !ended.Tells() || m.claim.Pending != nil || m.claim.Role == claim.RoleController {
		return nil
	}
	m.log.Info("supervise: the claim holds no task; a delivery of the daemon's own tells its agent what the relaunch ended",
		"cause", ended.Cause, "ended", ended.Generation)
	return m.queue(ctx, RequestDeliver{Claim: m.claim.Token, ID: relaunchTaskID(),
		Task: partText(RelaunchNote(m.claim.Issue, ended, false))})
}
