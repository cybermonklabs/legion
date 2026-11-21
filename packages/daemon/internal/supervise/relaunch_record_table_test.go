package supervise

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// olderCause is the cause of the record every row of the relaunch record's table starts its claim
// with: a let-go before the row's own, already told, in words no let-go writes (the cause
// constants and functions of endedwork.go). A prompt that names it names an older let-go's cause.
const olderCause = "an older let-go's cause"

// olderToldClaim is the queued claim each row starts from, as the spawn route leaves one, holding
// the told record of an older let-go of a turn and its task: newHarnessOf seeds a claim's record
// as relaunch_test.go's cases of a forgotten session do. Told, it stands in front of no prompt
// (EndedWork.Tells), so a prompt that names its cause, or that says no cause was recorded, is a
// task taken again under a record that is not its own let-go's (Delivery.message,
// interruptedTask).
func olderToldClaim() Claim {
	c := queuedClaim()
	c.EndedWork = EndedWork{At: epoch.Add(-time.Hour), Cause: olderCause, Turn: true, TaskTurn: true,
		Reported: true, Told: true, ToldBy: "an older delivery"}
	return c
}

// earlierCause is the cause of the record the take of the table's claim awaiting its take's turn
// told (earlierUntold), in words no let-go writes either.
const earlierCause = "an earlier let-go's cause"

// earlierUntold is a record of an earlier launch's let-go that ended a job of its own, untold, as a
// restart reads one an earlier daemon left: the record the take of claimAwaitingItsTakesTurn tells.
func earlierUntold() EndedWork {
	return EndedWork{At: epoch.Add(-time.Minute), Cause: earlierCause, Background: true,
		Kinds: []string{"async-job"}, Reported: true}
}

// letGoCell is the claim of one row of the table, in the state the row's let-go lands in: its
// harness, the record the claim holds there (seeded), and, where that state is a compaction, the
// compaction its connection holds (compactHeld) with the answer Machine.Compact gives once it is
// released.
type letGoCell struct {
	h         *harness
	seeded    EndedWork
	release   func()
	compacted chan error
}

// cellOf is the cell of h as its setup left it, holding the record it holds now.
func cellOf(h *harness) *letGoCell { return &letGoCell{h: h, seeded: h.claim().EndedWork} }

// endCompaction lets the held compaction answer, and holds that answer to want: nil for a
// compaction whose claim is given back to the process it ran on (compactEnded),
// ErrCompactProcessGone for one whose process a let-go took first (ownsCompaction), and
// ErrCompactSuspended for one whose end a held suspension took. A cell with no compaction, or one
// whose compaction has already answered, ends none.
func (c *letGoCell) endCompaction(want error) {
	c.h.t.Helper()
	if c.release == nil {
		return
	}
	c.release()
	c.release = nil
	if err := <-c.compacted; !errors.Is(err, want) {
		c.h.t.Fatalf("the compaction answered %v, want %v", err, want)
	}
	c.h.m.Wait()
}

// claimState is one state a claim can be in when a let-go lands: its name, the background work
// its process's own report names, read after the state's last turn began, the background work of
// the earlier record the state's accepted take told (told), and the setup that puts a claim there
// reporting that work. The record keeps what the report names, and what a take whose turn never
// started told goes back to untold, so the let-go's record joins it (EndedWork.Background, Kinds,
// joined); the sentence names both (relaunchSentence).
type claimState struct {
	name  string
	kinds []string
	told  []string
	setup func(t *testing.T, kinds ...string) *letGoCell
}

// claimInNoTurn is an idle claim holding no task: the report of its task's take's turn finished
// that task (reach), and its process reported after that turn.
func claimInNoTurn(t *testing.T, kinds ...string) *letGoCell {
	t.Helper()
	h := newHarnessOf(t, olderToldClaim())
	h.reach(StateIdle)
	h.advance(time.Second)
	h.reportActivity(kinds...)
	return cellOf(h)
}

// claimInATurn is a claim working in the turn of its task's accepted take (reach, turnStarted),
// its process reporting after the turn began.
func claimInATurn(t *testing.T, kinds ...string) *letGoCell {
	t.Helper()
	return workingInATurn(t, kinds)
}

// claimInATurnOfACurrentPlugin is claimInATurn on a plugin that names its task's number in its
// reports (currentPlugin), whose task only the report of its take's turn finishes
// (endFrameFinishes).
func claimInATurnOfACurrentPlugin(t *testing.T, kinds ...string) *letGoCell {
	t.Helper()
	return workingInATurn(t, kinds, currentPlugin)
}

// workingInATurn is claimInATurn over a harness that set tunes.
func workingInATurn(t *testing.T, kinds []string, set ...func(*Deps)) *letGoCell {
	t.Helper()
	h := newHarnessOf(t, olderToldClaim(), set...)
	h.reach(StateWorking)
	h.advance(time.Second)
	h.reportActivity(kinds...)
	return cellOf(h)
}

// claimIdleWithItsReportToCome is idleWithItsReportToCome over the seeded claim: idle holding the
// task whose take's turn ran and ended with its report still to come, the end-frame bound waiting
// on it (takesTurnEndsWithItsReportToCome), its process reporting after that turn began.
func claimIdleWithItsReportToCome(t *testing.T, kinds ...string) *letGoCell {
	t.Helper()
	h := newHarnessOf(t, olderToldClaim())
	h.reach(StateIdle)
	h.takesTurnEndsWithItsReportToCome()
	h.advance(time.Second)
	h.reportActivity(kinds...)
	return cellOf(h)
}

// claimAwaitingItsTakesTurn is an idle claim holding a task whose take is accepted and whose turn
// has not started (taskTheAgentTook), its process reporting after its last turn. The claim holds
// an earlier let-go's record untold when that take is accepted (earlierUntold), as a daemon
// restarted over it reads it, so the take tells that record by its delivery.
func claimAwaitingItsTakesTurn(t *testing.T, kinds ...string) *letGoCell {
	t.Helper()
	h := newHarnessOf(t, olderToldClaim())
	h.reach(StateIdle)
	h.advance(time.Second)
	h.reportActivity(kinds...)
	c := h.claim()
	c.EndedWork = earlierUntold()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.connect()
	h.taskTheAgentTook()
	if p, e := h.pending(), h.claim().EndedWork; e.Cause != earlierCause || !e.Told || e.ToldBy != p.ID {
		t.Fatalf("the take of %s left the record %+v, want the earlier record told by it", p.ID, e)
	}
	return cellOf(h)
}

// claimInACompaction is compactingIdle over the seeded claim: an idle claim holding no task, its
// process reporting after its last turn, taken for a compaction its connection holds, which
// stores the claim working in no turn (compactHeld, compactStart).
func claimInACompaction(t *testing.T, kinds ...string) *letGoCell {
	t.Helper()
	h := newHarnessOf(t, olderToldClaim())
	h.reach(StateIdle)
	h.advance(time.Second)
	h.reportActivity(kinds...)
	release, compacted := h.compactHeld()
	c := cellOf(h)
	c.release, c.compacted = release, compacted
	return c
}

// claimInATurnUnderACompaction is claimInACompaction with a turn Oh My Pi started under the
// compaction (turnStartedWhileBusy, compacting.inTurn), as
// TestARelaunchRecordsTheTurnStartedUnderACompaction drives it. The claim holds no task, so the
// turn is no task's; its process reports again after the turn began.
func claimInATurnUnderACompaction(t *testing.T, kinds ...string) *letGoCell {
	t.Helper()
	c := claimInACompaction(t, kinds...)
	c.h.must(StreamTurnStart{Claim: c.h.token})
	c.h.advance(time.Second)
	c.h.reportActivity(kinds...)
	return c
}

// letGoKind is one kind of let-go the table lands: the name its rows go by, the drive that lands
// it on a cell, and the request that brings the claim's next process up after it — the relaunch
// coming up to ready, the operator's resume of a suspension, or the operator's retry of a stopped
// claim.
type letGoKind struct {
	name  string
	drive func(c *letGoCell)
	next  func(h *harness)
}

// retiresAfterPromptFailures is the prompt-failure retirement as
// TestARelaunchRecordsAPromptRetirement drives it, through R5's first bound: the claim is handed a
// task where it holds none, its worker reports the task's value seen and asks no take of it, and
// three first bounds run out, each charging it (takeBoundOver, chargePrompt), the third retiring
// the process. The worker takes on its own again once the drive is over.
func retiresAfterPromptFailures(c *letGoCell) {
	h := c.h
	h.t.Helper()
	h.m.deps.Timeouts.Take = takeBound
	h.manualTakes()
	defer func() { h.worker.manual = false }()
	if h.claim().Pending == nil {
		h.must(RequestDeliver{Claim: h.token, Task: "the task its value names"})
	}
	h.seen()
	for range testLimits().PromptFailures {
		h.advance(takeBound)
	}
}

// dies is the claim's process found gone (judge, died).
func dies(c *letGoCell) {
	c.h.t.Helper()
	c.h.observe(runtime.Gone)
}

// repoints is the claim's process found alive at a stale address (judge, repoint).
func repoints(c *letGoCell) {
	c.h.t.Helper()
	c.h.observe(runtime.StaleAddress)
}

// relaunchesOntoTheCurrentPlugin is the stale-plugin lane's post over a claim whose process speaks
// an older contract than the daemon's (refreshPlugin), as
// TestARelaunchRecordsNoTaskTurnForADeathBetweenATakesTurnEndAndItsReport drives it: the harness's
// register() walk records h.deps's contract, so the relaunched process comes up on the contract
// this daemon now speaks.
func relaunchesOntoTheCurrentPlugin(c *letGoCell) {
	c.h.t.Helper()
	c.h.deps.PluginContract, c.h.m.deps.PluginContract = 25, 25
	c.h.must(StalePlugin{Claim: c.h.token, Trigger: "sweep"})
}

// stopsForTheOperator is the operator's stop (stop, end, retire), which records what it ended
// under its own cause (endStopped) and puts the claim where the operator's retry reaches it:
// retired (RequestOperatorRetry's row, operatorRetry).
func stopsForTheOperator(c *letGoCell) {
	c.h.t.Helper()
	c.h.must(RequestStop{Claim: c.h.token})
}

// backstopsTheStall is the daemon's stall backstop once the claim's last step is past its role's
// bound, posted with the record the store holds, as the sweep reads it (backstop,
// backstopStalledTurn).
func backstopsTheStall(c *letGoCell) {
	h := c.h
	h.t.Helper()
	last := h.store.step(h.token).LastStep()
	h.advance(backstopBound + time.Minute)
	h.must(h.backstop(last))
}

// operatorSuspension is the operator's suspension every suspension row asks for.
func operatorSuspension(h *harness) RequestSuspend {
	return RequestSuspend{Claim: h.token, By: SuspendedByOperator, Reason: "the operator is draining the daemon"}
}

// suspendsAtOnce is the operator's suspension of a claim nothing runs on, decided where it
// arrives (suspend, suspendNow).
func suspendsAtOnce(c *letGoCell) {
	c.h.t.Helper()
	c.h.must(operatorSuspension(c.h))
}

// holdsTheSuspension is the operator's suspension of a claim working for a turn or a compaction,
// held for its end and bounded by the stop timeout (holdSuspension).
func holdsTheSuspension(h *harness) {
	h.t.Helper()
	if err := h.handle(operatorSuspension(h)); !errors.Is(err, ErrSuspendHeld) {
		h.t.Fatalf("the suspension answered %v, want it held for what the claim is working for", err)
	}
}

// suspendsAtTheTurnsEnd lands the held suspension at the end of the turn it was held for
// (turnEnded, suspendHeldAtEnd), or, where the turn was a task's whose plugin names its number, at
// the report right behind that end, which finishes the task first (reportedTask, afterTurn).
func suspendsAtTheTurnsEnd(c *letGoCell) {
	c.h.t.Helper()
	holdsTheSuspension(c.h)
	c.h.must(StreamTurnEnd{Claim: c.h.token})
}

// suspendsAtTheCompactionsEnd lands the held suspension at the end of the compaction it was held
// for (compactEnded, suspendHeldAtEnd).
func suspendsAtTheCompactionsEnd(c *letGoCell) {
	c.h.t.Helper()
	holdsTheSuspension(c.h)
	c.endCompaction(ErrCompactSuspended)
}

// suspendsAtTheHoldsDeadline lands the held suspension at its deadline, while what it was held
// for still runs (TimerSuspend, suspendAtTimeout).
func suspendsAtTheHoldsDeadline(c *letGoCell) {
	c.h.t.Helper()
	holdsTheSuspension(c.h)
	c.h.advance(testStop)
}

// suspendsAtTheTurnsEndAfterTheCompactions ends the compaction first, which leaves the claim
// working for the turn under it with the suspension still held (compactEnded's inTurn branch);
// that turn's end then lands it (turnEnded, suspendHeldAtEnd).
func suspendsAtTheTurnsEndAfterTheCompactions(c *letGoCell) {
	c.h.t.Helper()
	holdsTheSuspension(c.h)
	c.endCompaction(ErrCompactSuspended)
	c.h.wantState(StateWorking)
	c.h.must(StreamTurnEnd{Claim: c.h.token})
}

// suspendsAtTheCompactionsEndAfterTheTurns ends the turn first, which leaves the claim working for
// the compaction with the suspension still held (turnEnded's compaction branch); the compaction's
// end then lands it (compactEnded, suspendHeldAtEnd).
func suspendsAtTheCompactionsEndAfterTheTurns(c *letGoCell) {
	c.h.t.Helper()
	holdsTheSuspension(c.h)
	c.h.must(StreamTurnEnd{Claim: c.h.token})
	c.h.wantState(StateWorking)
	c.endCompaction(ErrCompactSuspended)
}

// relaunchedToReady brings the process a relaunch started up to ready (relaunched).
func relaunchedToReady(h *harness) {
	h.t.Helper()
	h.relaunched()
}

// resumedByTheOperator is the operator's resume of the suspended claim, brought up to ready
// (resume, revive).
func resumedByTheOperator(h *harness) {
	h.t.Helper()
	h.must(RequestResume{Claim: h.token})
	h.relaunched()
}

// retriedByTheOperator is the operator's retry of the stopped claim, brought up to ready
// (operatorRetry, retry, revive).
func retriedByTheOperator(h *harness) {
	h.t.Helper()
	h.must(RequestOperatorRetry{Claim: h.token})
	h.relaunched()
}

// recorded is what a row's let-go records (recordEnd): its cause, whether a turn was running, and
// whether the turn of the task the claim holds was cut off. A let-go that records nothing has no
// cause.
type recorded struct {
	cause          string
	turn, taskTurn bool
}

// firstPrompt is what the claim's next process takes first.
type firstPrompt int

const (
	// sentNothing is no take at all: the claim holds no task and no record that shows running work
	// (queueRelaunch).
	sentNothing firstPrompt = iota
	// sentRecordAlone is the daemon's own delivery of the record to a claim holding no task
	// (queueRelaunch), whose text is the record's sentence (RelaunchSentence).
	sentRecordAlone
	// sentTask is the task the claim holds, never taken back from a turn, behind the record's
	// sentence (Delivery.message).
	sentTask
	// sentTaskTakenBack is the task the let-go took back from the turn it cut off (interrupted),
	// marked Interrupted, behind the record's sentence (Delivery.message).
	sentTaskTakenBack
)

// relaunchRecordRow is one cell of the table: a let-go kind landing on a claim in one state. A
// row whose kind does not let the process go in that state names the state the claim keeps
// instead. Every other row names what its let-go records, what the next process takes first, and
// the statement that take's text makes of the turn (saysNoTurn, saysTurnCut, saysTaskTurn).
type relaunchRecordRow struct {
	kind   letGoKind
	state  claimState
	keeps  ClaimState
	record recorded
	sent   firstPrompt
	says   string
}

// run lands the row's let-go on a claim in the row's state and holds what follows to the row. The
// rule runs before the checks that stop the row (wantFirstPrompt, wantEndedLine), so a row that
// breaks it says so whatever else it breaks.
func (row relaunchRecordRow) run(t *testing.T) {
	c := row.state.setup(t, row.state.kinds...)
	h := c.h
	letGo, process, from := h.generation(), h.locator(), len(h.prompts())
	suspends := len(h.calls("Suspend"))

	row.kind.drive(c)
	at := h.clock.Now()

	if row.keeps != "" {
		h.wantState(row.keeps)
		if got := h.claim().Locator; got == nil || *got != process {
			t.Errorf("the claim records the process %+v, want the one it ran, %+v, kept", got, process)
		}
		if got := len(h.calls("Suspend")); got != suspends {
			t.Errorf("Suspend calls = %d, want %d: nothing stopped the process", got, suspends)
		}
		h.wantOnlyTheRecordItHeld(c.seeded)
		c.endCompaction(nil)
		h.wantNoTaskSentInterruptedWithoutItsRecord("", letGo, h.prompts()[from:])
		return
	}
	if row.record.cause == "" {
		h.wantOnlyTheRecordItHeld(c.seeded)
	} else {
		kinds := slices.Concat(row.state.told, row.state.kinds)
		h.wantEnded(EndedWork{At: at, Cause: row.record.cause, Generation: letGo, Turn: row.record.turn,
			TaskTurn: row.record.taskTurn, Background: len(kinds) > 0, Kinds: kinds, Reported: true})
	}
	c.endCompaction(ErrCompactProcessGone)
	next := len(h.prompts())
	row.kind.next(h)
	h.m.Wait()
	h.wantNoTaskSentInterruptedWithoutItsRecord(row.record.cause, letGo, h.prompts()[from:])
	row.wantFirstPrompt(h, h.prompts()[next:])
	if row.record.cause != "" {
		h.wantEndedLine(row.record.cause)
	}
}

// wantFirstPrompt is the first of the takes the next process made (sent), as the row says: none
// at all, the daemon's own delivery of the record, or the task the claim holds, taken back from
// the turn or not; and the sentence its text reads: because the row's own cause, the row's
// statement of the turn and no other, and the subagent the state's report named and the job of
// the record its take told, or no background work where neither named any.
func (row relaunchRecordRow) wantFirstPrompt(h *harness, sent []taken) {
	t := h.t
	t.Helper()
	if row.sent == sentNothing {
		if len(sent) != 0 {
			t.Errorf("the next process took %+v, want nothing: the claim holds no task and no record of running work", sent)
		}
		return
	}
	if len(sent) == 0 {
		t.Fatal("the next process took nothing, want the sentence of the let-go's record")
	}
	told, p := sent[0], h.pending()
	if told.DeliveryID != p.ID {
		t.Fatalf("the next process took %+v first, want the delivery the claim holds, %+v", told, p)
	}
	switch row.sent {
	case sentRecordAlone:
		if !p.Relaunch() {
			t.Errorf("the next process took %+v, want the daemon's own delivery of the record", p)
		}
	case sentTask, sentTaskTakenBack:
		interrupted := row.sent == sentTaskTakenBack
		if p.Relaunch() || p.Interrupted != interrupted || !strings.HasSuffix(told.Message, "\n\n"+p.Task) {
			t.Errorf("the next process took %+v as %+v, want the task the claim holds, interrupted = %v, behind the record's sentence",
				told, p, interrupted)
		}
	}
	wantSays(t, told.Message, "ended because "+row.record.cause+".", row.says)
	for _, other := range []string{saysNoTurn, saysTurnCut, saysTaskTurn} {
		if other != row.says {
			wantSaysNot(t, told.Message, other)
		}
	}
	switch {
	case len(row.state.kinds) > 0:
		wantSays(t, told.Message, "subagent that you started stopped with that process.")
	case len(row.state.told) == 0:
		wantSaysNot(t, told.Message, "stopped with that process")
	}
	if len(row.state.told) > 0 {
		wantSays(t, told.Message, "background job")
	}
}

// wantOnlyTheRecordItHeld is a claim the row's let-go recorded nothing on: it holds the record
// its setup left it with (seeded), in the machine and in the store, and no line says a let-go
// ended work. It is wantNoEnded for a claim seeded with a record.
func (h *harness) wantOnlyTheRecordItHeld(seeded EndedWork) {
	h.t.Helper()
	h.wantEnded(seeded)
	if lines := h.records.of(endedWorkLine); len(lines) != 0 {
		h.t.Errorf("logged %d lines that the relaunch ended work, want none", len(lines))
	}
}

// wantNoTaskSentInterruptedWithoutItsRecord is the rule the table holds, on every row's takes
// from its let-go on (sent). A task taken as interrupted goes behind the record of the let-go
// that took it back: the claim's record is that let-go's own — its cause, and its launch (letGo)
// — and the text says that the task's turn was interrupted and why. A row whose kind let no
// process go, or whose let-go recorded nothing, has no cause, and a task it hands over as
// interrupted is that fault. And no take's text names the older let-go's cause (olderCause), nor
// says that the daemon recorded none, which is what a task taken back with no record of its own
// let-go is told (interruptedTask).
func (h *harness) wantNoTaskSentInterruptedWithoutItsRecord(cause string, letGo uint64, sent []taken) {
	h.t.Helper()
	held := h.claim()
	for _, prompt := range sent {
		wantSaysNot(h.t, prompt.Message, olderCause, "the daemon recorded no cause")
		if p := held.Pending; p == nil || !p.Interrupted || prompt.DeliveryID != p.ID {
			continue
		}
		if ended := held.EndedWork; cause == "" || ended.Cause != cause || ended.Generation != letGo {
			h.t.Errorf("the task %s was taken as interrupted under the record %+v, want the record of this row's own let-go of launch %d, cause %q",
				prompt.DeliveryID, ended, letGo, cause)
		}
		wantSays(h.t, prompt.Message, saysTaskTurn, "ended because "+cause+".")
	}
}

// The relaunch record's table: every kind of let-go of a claim's process, against every state the
// claim can be in when it lands (dispatch://INFRA-3276, the Verification document's c-r3-tests).
// Each row states what the let-go records — its own cause, whether a turn and the task's turn were
// cut off, and the background work the newest report named — and the sentence the next process
// reads first: that cause, that statement of the turn, and that work. Every claim starts with an
// older let-go's record already told (olderToldClaim), and the rule holds on every row
// (wantNoTaskSentInterruptedWithoutItsRecord): no task is taken as interrupted without the record
// of the let-go that took it back, and no take's text names the older cause or says none was
// recorded. The idle claim whose take's report is still to come is driven twice, its report
// naming a subagent and saying that nothing runs, since only the first leaves the let-go anything
// to record.
//
// The claim whose take's report is still to come holds a task whose turn ran whole: a let-go
// finishes it by its end frame rather than take it back (interrupted, finishTask), and the
// stopped or suspended claim's settle does the same, so the record is never of the task's turn and
// the next process holds no task. Where the report names a subagent, it is given the daemon's own
// delivery of the record, which says it was in no turn; where the report says nothing runs, the
// let-go ended nothing that ran and records nothing, and no delivery follows (queueRelaunch,
// dispatch://INFRA-2811 departure D2).
//
// The claim whose take awaits its turn is idle with that take accepted, and the take told an
// earlier record (claimAwaitingItsTakesTurn). Every let-go of its process gives that telling back,
// since no turn ran the take's text (untellUnstarted, giveBackTake), so the let-go's record joins
// the earlier one and names its job beside the row's subagent; none records the task's turn.
// Every let-go also takes the take back under its number — a death, a repoint and a stale-plugin
// relaunch (interrupted), and the stop that retires the claim and the operator's suspension
// (takeBackUnstarted) — so the next process takes the task afresh behind the joined record, which
// that take tells once.
//
// The claim in a turn is driven a second time on a plugin that names its task's number
// (claimInATurnOfACurrentPlugin), whose task only the report of its take's turn finishes. A
// suspension held for that turn waits at its end for the report right behind it, which finishes
// the task, and stops the process then, so the record and the next process's first take are
// those of the older plugin's row: no turn cut off, no task held. At the hold's deadline in the
// turn, the task is taken back from the turn it cuts off, as on the older plugin.
//
// The cells where the kind does not let the process go, each of which asserts that the claim
// keeps its process and state and that nothing is recorded:
//
//   - A retirement after prompt failures, in a turn, idle with its take's report to come, idle
//     with its take awaiting its turn, and in a compaction with or without a turn: no first bound
//     runs out. R5's first wait waits only on a value no take holds, seen by a ready or idle
//     worker no hold stands over (firstWaitNow): a working claim is held in its turn, and a take
//     that is accepted is no value left to take. The task whose take's report is to come is
//     finished by its end frame inside the first bound.
//   - A stale-plugin relaunch in a turn, and in a compaction with or without a turn: the claim is
//     working, so the relaunch waits for that end (refreshPlugin's holdStaleRelaunch, run by
//     afterTurn).
//   - A stall backstop in no turn, idle with its take's report to come and idle with its take
//     awaiting its turn: the table ignores the backstop outside StateWorking (fillTable's
//     onStallBackstop rows).
//
// Two let-gos record nothing, both over the idle claim whose take's report is still to come and
// whose report says nothing runs: the operator's stop and the operator's suspension, each of which
// takes nothing back (stop, suspendNow) and so cuts off no turn, no task's turn and no reported
// work (recordEnd's early return), while settle finishes the task. So do a death, a repoint and a
// stale-plugin relaunch of that claim: each finishes the task by its end frame rather than take it
// back. In every other state the stop records under its own cause (endStopped) and settle
// finishes any task whose take's turn ran, and the retry that follows lets nothing go (revive's
// noEnd), so the retried process, holding no task, takes the stop's record alone: of a turn cut
// off, the turn it was in and never the task's.
func TestARelaunchRecordsEveryLetGoKindInEveryClaimState(t *testing.T) {
	subagent := []string{"subagent"}
	var (
		noTurn            = claimState{name: "no turn", kinds: subagent, setup: claimInNoTurn}
		inATurn           = claimState{name: "in a turn", kinds: subagent, setup: claimInATurn}
		reportToCome      = claimState{name: "idle with its take's report to come", kinds: subagent, setup: claimIdleWithItsReportToCome}
		reportToComeQuiet = claimState{name: "idle with its take's report to come, its report saying nothing runs",
			setup: claimIdleWithItsReportToCome}
		awaitingTurn = claimState{name: "idle with an accepted take awaiting its turn", kinds: subagent,
			told: earlierUntold().Kinds, setup: claimAwaitingItsTakesTurn}
		compaction              = claimState{name: "in a compaction with no turn", kinds: subagent, setup: claimInACompaction}
		turnUnderCompaction     = claimState{name: "in a compaction with a turn", kinds: subagent, setup: claimInATurnUnderACompaction}
		inATurnOfACurrentPlugin = claimState{name: "in a turn, on a plugin that names its task's number", kinds: subagent,
			setup: claimInATurnOfACurrentPlugin}
	)
	var (
		retirement        = letGoKind{name: "a retirement after prompt failures", drive: retiresAfterPromptFailures, next: relaunchedToReady}
		deathBetweenTurns = letGoKind{name: "a death between turns", drive: dies, next: relaunchedToReady}
		deathInATurn      = letGoKind{name: "a death in a turn", drive: dies, next: relaunchedToReady}
		repoint           = letGoKind{name: "a repoint", drive: repoints, next: relaunchedToReady}
		stalePlugin       = letGoKind{name: "a stale-plugin relaunch", drive: relaunchesOntoTheCurrentPlugin, next: relaunchedToReady}
		operatorRetry     = letGoKind{name: "an operator's retry", drive: stopsForTheOperator, next: retriedByTheOperator}
		stallBackstop     = letGoKind{name: "a stall backstop", drive: backstopsTheStall, next: relaunchedToReady}
		suspensionAtOnce  = letGoKind{name: "a suspension landing at once", drive: suspendsAtOnce, next: resumedByTheOperator}
	)
	// The held suspension's landings: at the end of what it was held for, or at its deadline.
	var (
		suspensionAtTurnEnd = letGoKind{
			name: "a suspension landing at the turn's end", drive: suspendsAtTheTurnsEnd, next: resumedByTheOperator,
		}
		suspensionAtCompactionEnd = letGoKind{
			name: "a suspension landing at the compaction's end", drive: suspendsAtTheCompactionsEnd, next: resumedByTheOperator,
		}
		suspensionAtDeadline = letGoKind{
			name: "a suspension landing at the hold's deadline", drive: suspendsAtTheHoldsDeadline, next: resumedByTheOperator,
		}
		suspensionAtTurnEndAfterCompaction = letGoKind{
			name:  "a suspension landing at the turn's end, after the compaction's",
			drive: suspendsAtTheTurnsEndAfterTheCompactions, next: resumedByTheOperator,
		}
		suspensionAtCompactionEndAfterTurn = letGoKind{
			name:  "a suspension landing at the compaction's end, after the turn's",
			drive: suspendsAtTheCompactionsEndAfterTheTurns, next: resumedByTheOperator,
		}
	)
	var (
		died         = endDied(deathCause(runtime.Observation{Kind: runtime.Gone}))
		stalledTurn  = endStalled(backstopBound, true)
		stalledClaim = endStalled(backstopBound, false)
		suspended    = endSuspended(SuspendedByOperator)
	)
	for _, row := range []relaunchRecordRow{
		{kind: retirement, state: noTurn, record: recorded{cause: endRetired}, sent: sentTask, says: saysNoTurn},
		{kind: retirement, state: inATurn, keeps: StateWorking},
		{kind: retirement, state: reportToCome, keeps: StateIdle},
		{kind: retirement, state: reportToComeQuiet, keeps: StateIdle},
		{kind: retirement, state: awaitingTurn, keeps: StateIdle},
		{kind: retirement, state: compaction, keeps: StateWorking},
		{kind: retirement, state: turnUnderCompaction, keeps: StateWorking},

		{kind: deathBetweenTurns, state: noTurn, record: recorded{cause: died}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: deathInATurn, state: inATurn, record: recorded{cause: died, turn: true, taskTurn: true}, sent: sentTaskTakenBack, says: saysTaskTurn},
		{kind: deathBetweenTurns, state: reportToCome, record: recorded{cause: died}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: deathBetweenTurns, state: reportToComeQuiet, sent: sentNothing},
		{kind: deathBetweenTurns, state: awaitingTurn, record: recorded{cause: died}, sent: sentTask, says: saysNoTurn},
		{kind: deathBetweenTurns, state: compaction, record: recorded{cause: died}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: deathInATurn, state: turnUnderCompaction, record: recorded{cause: died, turn: true}, sent: sentRecordAlone, says: saysTurnCut},

		{kind: repoint, state: noTurn, record: recorded{cause: endRepointed}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: repoint, state: inATurn, record: recorded{cause: endRepointed, turn: true, taskTurn: true}, sent: sentTaskTakenBack, says: saysTaskTurn},
		{kind: repoint, state: reportToCome, record: recorded{cause: endRepointed}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: repoint, state: reportToComeQuiet, sent: sentNothing},
		{kind: repoint, state: awaitingTurn, record: recorded{cause: endRepointed}, sent: sentTask, says: saysNoTurn},
		{kind: repoint, state: compaction, record: recorded{cause: endRepointed}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: repoint, state: turnUnderCompaction, record: recorded{cause: endRepointed, turn: true}, sent: sentRecordAlone, says: saysTurnCut},

		{kind: stalePlugin, state: noTurn, record: recorded{cause: endStalePlugin}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: stalePlugin, state: inATurn, keeps: StateWorking},
		{kind: stalePlugin, state: reportToCome, record: recorded{cause: endStalePlugin}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: stalePlugin, state: reportToComeQuiet, sent: sentNothing},
		{kind: stalePlugin, state: awaitingTurn, record: recorded{cause: endStalePlugin}, sent: sentTask, says: saysNoTurn},
		{kind: stalePlugin, state: compaction, keeps: StateWorking},
		{kind: stalePlugin, state: turnUnderCompaction, keeps: StateWorking},

		{kind: operatorRetry, state: noTurn, record: recorded{cause: endStopped}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: operatorRetry, state: inATurn, record: recorded{cause: endStopped, turn: true, taskTurn: true}, sent: sentRecordAlone, says: saysTurnCut},
		{kind: operatorRetry, state: reportToCome, record: recorded{cause: endStopped}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: operatorRetry, state: reportToComeQuiet, sent: sentNothing},
		{kind: operatorRetry, state: awaitingTurn, record: recorded{cause: endStopped}, sent: sentTask, says: saysNoTurn},
		{kind: operatorRetry, state: compaction, record: recorded{cause: endStopped}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: operatorRetry, state: turnUnderCompaction, record: recorded{cause: endStopped, turn: true}, sent: sentRecordAlone, says: saysTurnCut},

		{kind: stallBackstop, state: noTurn, keeps: StateIdle},
		{kind: stallBackstop, state: inATurn, record: recorded{cause: stalledTurn, turn: true, taskTurn: true}, sent: sentTaskTakenBack, says: saysTaskTurn},
		{kind: stallBackstop, state: reportToCome, keeps: StateIdle},
		{kind: stallBackstop, state: reportToComeQuiet, keeps: StateIdle},
		{kind: stallBackstop, state: awaitingTurn, keeps: StateIdle},
		{kind: stallBackstop, state: compaction, record: recorded{cause: stalledClaim}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: stallBackstop, state: turnUnderCompaction, record: recorded{cause: stalledTurn, turn: true}, sent: sentRecordAlone, says: saysTurnCut},

		{kind: suspensionAtOnce, state: noTurn, record: recorded{cause: suspended}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: suspensionAtTurnEnd, state: inATurn, record: recorded{cause: suspended}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: suspensionAtDeadline, state: inATurn, record: recorded{cause: suspended, turn: true, taskTurn: true}, sent: sentTaskTakenBack, says: saysTaskTurn},
		{kind: suspensionAtTurnEnd, state: inATurnOfACurrentPlugin, record: recorded{cause: suspended}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: suspensionAtDeadline, state: inATurnOfACurrentPlugin, record: recorded{cause: suspended, turn: true, taskTurn: true}, sent: sentTaskTakenBack, says: saysTaskTurn},
		{kind: suspensionAtOnce, state: reportToCome, record: recorded{cause: suspended}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: suspensionAtOnce, state: reportToComeQuiet, sent: sentNothing},
		{kind: suspensionAtOnce, state: awaitingTurn, record: recorded{cause: suspended}, sent: sentTask, says: saysNoTurn},
		{kind: suspensionAtCompactionEnd, state: compaction, record: recorded{cause: suspended}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: suspensionAtDeadline, state: compaction, record: recorded{cause: suspended}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: suspensionAtTurnEndAfterCompaction, state: turnUnderCompaction, record: recorded{cause: suspended}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: suspensionAtCompactionEndAfterTurn, state: turnUnderCompaction, record: recorded{cause: suspended}, sent: sentRecordAlone, says: saysNoTurn},
		{kind: suspensionAtDeadline, state: turnUnderCompaction, record: recorded{cause: suspended, turn: true}, sent: sentRecordAlone, says: saysTurnCut},
	} {
		t.Run(row.kind.name+" / "+row.state.name, row.run)
	}
}
