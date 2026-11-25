package supervise

// Every order in which an end from outside the pod is met again after a daemon restart
// (dispatch://INFRA-1117 R3, R10). The daemon before the restart (D1) got the runtime's first
// report of the claim's process gone, read as an end from outside the pod, and decided it
// (endedFromOutside); the relaunch that decision raised was parked by the wake pacing, or refused
// by the lifecycle recheck, so the claim still records the ended process when the next daemon
// (D2) restores it. What D2 does is decided by four things: what D1's store did of the take-back
// of the task whose turn the end cut off, what it did of the decision's write, which of the
// boot's replay (RecordedDeath) and the runtime's report of the ended process reaches the claim
// first, and whether D2's runtime reads that report as an end from outside the pod again or
// neither way, its reading gone with D1's memory.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// outsideStood is how the claim stood when its pod ended from outside it.
type outsideStood string

const (
	stoodInTaskTurn     outsideStood = "in a turn of its confirmed task"
	stoodOwingTask      outsideStood = "ready holding its task unconfirmed"
	stoodInMessageTurn  outsideStood = "in a message's turn"
	stoodHoldingNothing outsideStood = "holding nothing"
)

// takeBackWrite is what D1's store did of the take-back of the task whose turn the end cut off
// (interrupted).
type takeBackWrite string

const (
	takeBackNone          takeBackWrite = "none to take"
	takeBackTaken         takeBackWrite = "taken"
	takeBackRefused       takeBackWrite = "refused"
	takeBackUnknownLanded takeBackWrite = "outcome unknown, landed"
	takeBackUnknownLost   takeBackWrite = "outcome unknown, not landed"
)

// decisionWrite is what D1's store did of the decision's write (endedFromOutside's persist):
// the claim's DeathCharged and the record of the end, typed as one from outside the pod.
type decisionWrite string

const (
	decisionTaken      decisionWrite = "taken"
	decisionRefused    decisionWrite = "refused"
	decisionNotReached decisionWrite = "not reached, the take-back answered an error"
)

// atRestart is one event that reaches the claim at D2.
type atRestart string

const (
	atReplay atRestart = "the boot's replay"
	// atReplayAtTurn is the boot's replay at the turn the boot's pass reserved for it, come
	// already, as it is for the first relaunch of the restart's wave: the wake gate runs a
	// relaunch it raises at once, with no wake parked whose turn would take the task back
	// (reserveWake, runWake).
	atReplayAtTurn  atRestart = "the boot's replay at its reserved turn"
	atReportOutside atRestart = "the report read again as outside"
	atReportNeither atRestart = "the report read neither way"
	// atReportRefused is a report read neither way whose relaunch D2's lifecycle recheck
	// refuses, as it refused D1's; the store answers the recheck again after it.
	atReportRefused atRestart = "the report read neither way, its relaunch refused"
)

// outsideRestartOrder is one order of the table (the issue's table of orders, by its row). The
// fields state how the claim stood, what D1's store did and what reached the claim at D2, in
// order; death and told state what must hold where the rows differ. What holds of every row is
// read in walk: the role is continued in its own recorded session after the ended process, the
// task the claim held is still held at every step and sent again once at the relaunch's ready,
// and nothing is charged but where death says so.
type outsideRestartOrder struct {
	row      string
	stood    outsideStood
	takeBack takeBackWrite
	decision decisionWrite
	// refused is whether D1's lifecycle recheck refused the relaunch the decision raised; the
	// wake pacing parks it otherwise.
	refused bool
	// cause is the sentence an earlier daemon wrote as the stored record's cause, over the
	// record D1 typed as an end from outside the pod, and "" for today's endFromOutside.
	cause string
	at    []atRestart
	// death is whether the end is counted as a stop of the role's process: one death and the
	// launch failure every death is charged, which the relaunch's ready clears. Every other row
	// is charged nothing.
	death bool
	// told is the cause the relaunched role is told its process ended because of, and "" where
	// the end cut off no work, so the role is told nothing of it.
	told string
}

// earlierOutsideWords is the cause an earlier daemon wrote for an end from outside the pod, which
// is not today's endFromOutside: the record is read by its typed value (EndedWork.FromOutside),
// so a reword of the sentence leaves the records written before it read as they were.
const earlierOutsideWords = "something outside its pod ended that pod"

// errStoreRefused is a write the store refused (memStore.fail).
var errStoreRefused = errors.New("the store refused the write")

// unknownOutcomeStore is a store whose writes of a delivery answer an error whose outcome the
// daemon cannot know, as a commit whose answer the claim-write deadline or a dropped connection
// cut off does (lostAnswerStore): landed commits the write and then answers the error, and a
// write that did not land answers it with nothing written.
type unknownOutcomeStore struct {
	*memStore
	landed bool
}

var errOutcomeUnknown = errors.New("the commit's answer was lost")

func (s *unknownOutcomeStore) PutDelivery(ctx context.Context, token claim.Token, d Delivery) error {
	if s.landed {
		if err := s.memStore.PutDelivery(ctx, token, d); err != nil {
			return err
		}
	}
	return errOutcomeUnknown
}

func (s *unknownOutcomeStore) PutClaimAndDelivery(ctx context.Context, c Claim, d Delivery) error {
	if s.landed {
		if err := s.memStore.PutClaimAndDelivery(ctx, c, d); err != nil {
			return err
		}
	}
	return errOutcomeUnknown
}

// Every order of the table of orders (rows 1 to 16, and 17 and 18, which this test adds): an end
// from outside the pod that D1 decided is continued uncharged at D2 whichever of the boot's
// replay and the runtime's report reaches the claim first, and however D2's runtime reads that
// report, since every read of a decided end asks the record's typed value (decidedFromOutside):
// the report goes to endedFromOutside, never to died, and the replay relaunches it through
// relaunchDeath's uncharged branch. A take-back the store refused, or whose outcome is unknown,
// leaves nothing decided (endedFromOutside returns on it), so the replay decides nothing and the
// report read again takes the task back and continues the role. The replay of rows 11 and 14
// comes at the turn the boot's pass reserved for it, which runs a relaunch it raises at once,
// with no parked wake whose turn takes the task back first (reserveWake, runWake): the window
// the task was lost in. Where the store took no decision and D2 cannot read the end (rows 12 and
// 16), nothing tells the end from a death: it is one death, and the task is still sent again,
// never retired.
//
// Rows 1 to 3 are the two tests of the round before, TestAnEndFromOutsideWhoseRelaunchARestartLost-
// IsRelaunchedUncharged and TestAnEndFromOutsideInAMessageTurnWhoseRelaunchARestartLostIsToldAsAn-
// EndFromOutside, folded in with every assertion they made.
//
// The negative controls: judge sending a report over a decided end from outside the pod to died
// (rows 6, 8, 9, 10's report-first row, 17 and 18 charge a launch failure); endedFromOutside
// taking the task back after the decision's write, as it did at 68e811cc (rows 11 and 14: the
// replay relaunches with no take-back and settle retires the task; rows 12 and 13 find the
// decision stored); and decidedFromOutside deciding by the sentence (Cause == endFromOutside)
// rather than the typed field (both of row 10's rows charge a launch failure, as a death's
// relaunch does).
func TestAnEndFromOutsideMetAgainAfterARestartIsContinuedUnchargedInEveryOrder(t *testing.T) {
	for _, o := range []outsideRestartOrder{
		{row: "1", stood: stoodOwingTask, takeBack: takeBackNone, decision: decisionTaken,
			at: []atRestart{atReplay}},
		{row: "2", stood: stoodHoldingNothing, takeBack: takeBackNone, decision: decisionTaken,
			at: []atRestart{atReplay}},
		{row: "3", stood: stoodInMessageTurn, takeBack: takeBackNone, decision: decisionTaken,
			at: []atRestart{atReplay}, told: endFromOutside},
		{row: "4", stood: stoodInTaskTurn, takeBack: takeBackTaken, decision: decisionTaken,
			at: []atRestart{atReplay, atReportNeither}, told: endFromOutside},
		{row: "5", stood: stoodInTaskTurn, takeBack: takeBackTaken, decision: decisionTaken,
			at: []atRestart{atReplay, atReportOutside}, told: endFromOutside},
		{row: "6", stood: stoodInTaskTurn, takeBack: takeBackTaken, decision: decisionTaken,
			at: []atRestart{atReportNeither, atReplay}, told: endFromOutside},
		{row: "7", stood: stoodInTaskTurn, takeBack: takeBackTaken, decision: decisionTaken,
			at: []atRestart{atReportOutside, atReplay}, told: endFromOutside},
		{row: "8", stood: stoodInTaskTurn, takeBack: takeBackTaken, decision: decisionTaken, refused: true,
			at: []atRestart{atReportRefused, atReportNeither}, told: endFromOutside},
		{row: "9", stood: stoodOwingTask, takeBack: takeBackNone, decision: decisionTaken,
			at: []atRestart{atReportNeither, atReplay}},
		{row: "10, the replay alone", stood: stoodInTaskTurn, takeBack: takeBackTaken, decision: decisionTaken,
			cause: earlierOutsideWords, at: []atRestart{atReplay}, told: earlierOutsideWords},
		{row: "10, the report first", stood: stoodInTaskTurn, takeBack: takeBackTaken, decision: decisionTaken,
			cause: earlierOutsideWords, at: []atRestart{atReportNeither, atReplay}, told: earlierOutsideWords},
		{row: "11", stood: stoodInTaskTurn, takeBack: takeBackRefused, decision: decisionNotReached,
			at: []atRestart{atReplayAtTurn, atReportOutside}, told: endFromOutside},
		{row: "12", stood: stoodInTaskTurn, takeBack: takeBackRefused, decision: decisionNotReached,
			at: []atRestart{atReportNeither}, death: true, told: "the process died"},
		{row: "13", stood: stoodInTaskTurn, takeBack: takeBackUnknownLanded, decision: decisionNotReached,
			at: []atRestart{atReportOutside}, told: endFromOutside},
		{row: "14", stood: stoodInTaskTurn, takeBack: takeBackUnknownLost, decision: decisionNotReached,
			at: []atRestart{atReplayAtTurn, atReportOutside}, told: endFromOutside},
		{row: "15", stood: stoodInTaskTurn, takeBack: takeBackTaken, decision: decisionRefused,
			at: []atRestart{atReportOutside}, told: endFromOutside},
		{row: "16", stood: stoodInTaskTurn, takeBack: takeBackTaken, decision: decisionRefused,
			at: []atRestart{atReportNeither}, death: true, told: "the process died"},
		{row: "17", stood: stoodHoldingNothing, takeBack: takeBackNone, decision: decisionTaken,
			at: []atRestart{atReportNeither, atReplay}},
		{row: "18", stood: stoodInMessageTurn, takeBack: takeBackNone, decision: decisionTaken,
			at: []atRestart{atReportNeither, atReplay}, told: endFromOutside},
	} {
		t.Run(o.name(), o.walk)
	}
}

// name is the row's number and its fields, so a failing row reads as the order it is.
func (o outsideRestartOrder) name() string {
	events := make([]string, len(o.at))
	for i, ev := range o.at {
		events[i] = string(ev)
	}
	name := "row " + o.row + ": " + string(o.stood) + "; take-back " + string(o.takeBack) +
		"; decision " + string(o.decision)
	if o.refused {
		name += "; its relaunch refused"
	}
	return name + "; then " + strings.Join(events, ", ")
}

// walk is the order: the end at D1 as the row has it, the restart, the events at D2 in order,
// the relaunch's turn and its ready, and at each step what the row says must hold.
func (o outsideRestartOrder) walk(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	task := o.stand(h)
	queueAhead(h, 1)
	ended := h.locator()

	o.breakD1(h)
	err := h.handle(RuntimeObservation{Observation: runtime.Observation{
		Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs,
	}})
	if want := o.d1Fails(); (err != nil) != want {
		t.Fatalf("the end at D1 answered %v, want an error %v", err, want)
	}
	if o.raisesAtD1() {
		h.wantWaitingWake(WakeLaunch)
	}
	h.store.fail("PutDelivery", nil)
	h.store.fail("PutClaim", nil)
	h.wantNothingCharged("the end at D1")
	o.wantStoredAtD1(h, task)
	if o.cause != "" {
		c := h.store.load(h.token)
		c.EndedWork.Cause = o.cause
		if err := h.store.PutClaim(h.ctx, c); err != nil {
			t.Fatalf("write the earlier daemon's record: %v", err)
		}
	}

	h.restart()
	read, sent := h.claim().Generation, len(h.prompts())
	for _, ev := range o.at {
		o.reach(h, ev, ended, read)
		o.wantHeld(h, task, string(ev))
		o.wantCharged(h, string(ev))
	}
	o.wantRecord(h, read, "the events at D2")

	waiting := h.wantWaitingWake(WakeLaunch)
	h.advance(waiting.At.Sub(h.clock.Now()))
	resume := h.wantCalls("Resume", 1)[0]
	if resume.Previous == nil || *resume.Previous != ended || resume.Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("resumed %+v from %+v, want the claim's own recorded session after the ended incarnation",
			resume.Spec, resume.Previous)
	}
	o.wantCharged(h, "the relaunch")
	o.wantHeld(h, task, "the relaunch")
	o.wantRecord(h, read, "the relaunch")

	h.relaunched()
	if o.death {
		h.wantBudgets(Budgets{Deaths: 1})
	} else {
		h.wantNothingCharged("the relaunch's ready")
	}
	o.wantHeld(h, task, "the relaunch's ready")
	o.wantTold(h, sent, task)
}

// stand brings the claim to how it stood when its pod ended, and answers the task it held, ""
// for none.
func (o outsideRestartOrder) stand(h *harness) string {
	h.t.Helper()
	switch o.stood {
	case stoodInTaskTurn:
		h.reach(StateWorking)
		if p := h.pending(); p.Task != owedTask || p.ConfirmedAt.IsZero() {
			h.t.Fatalf("pending %+v, want the task %q confirmed by the turn the end cuts off", p, owedTask)
		}
		return owedTask
	case stoodOwingTask:
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: owedTask})
		h.wantOwed(owedTask)
		return owedTask
	case stoodInMessageTurn:
		h.reach(StateIdle)
		h.advance(time.Minute)
		h.must(StreamTurnStart{Claim: h.token})
		h.advance(time.Minute)
		return ""
	}
	h.reach(StateReady)
	return ""
}

// breakD1 sets D1's store to answer the end's writes as the row says.
func (o outsideRestartOrder) breakD1(h *harness) {
	switch o.takeBack {
	case takeBackRefused:
		h.store.fail("PutDelivery", errStoreRefused)
	case takeBackUnknownLanded, takeBackUnknownLost:
		h.m.deps.Store = &unknownOutcomeStore{memStore: h.store, landed: o.takeBack == takeBackUnknownLanded}
	}
	if o.decision == decisionRefused {
		h.store.fail("PutClaim", errStoreRefused)
	}
	if o.refused {
		h.m.deps.Store = &unansweredCheckLaunchStore{memStore: h.store, failing: true}
	}
}

// d1Fails is whether the end at D1 answers an error: a write the store refused or whose outcome
// is unknown, or a relaunch the lifecycle recheck refused.
func (o outsideRestartOrder) d1Fails() bool {
	return o.takeBack == takeBackRefused || o.takeBack == takeBackUnknownLanded ||
		o.takeBack == takeBackUnknownLost || o.decision == decisionRefused || o.refused
}

// raisesAtD1 is whether the end at D1 raised a relaunch the wake pacing parked: every row whose
// take-back landed or had nothing to take, but the one whose relaunch the lifecycle refused.
func (o outsideRestartOrder) raisesAtD1() bool {
	return o.decision != decisionNotReached && !o.refused
}

// wantStoredAtD1 fails unless D1's store holds what the row says it did: the decision, typed as
// an end from outside the pod, where its write was taken and nothing of it elsewhere; and the
// task still confirmed where the take-back did not land, taken back where it did. A store that
// holds something else fails the row and the walk goes on, so the row also says what D2 then
// makes of what that store holds.
func (o outsideRestartOrder) wantStoredAtD1(h *harness, task string) {
	h.t.Helper()
	stored := h.store.load(h.token)
	decided := o.decision == decisionTaken
	if stored.DeathCharged != decided || stored.EndedWork.FromOutside != decided {
		h.t.Errorf("D1's store holds deathCharged=%v and the record %+v, want the decision stored %v",
			stored.DeathCharged, stored.EndedWork, decided)
	}
	if task == "" {
		return
	}
	if stored.Pending == nil || stored.Pending.Task != task {
		h.t.Fatalf("D1's store holds the task %+v, want %q", stored.Pending, task)
	}
	confirmed := !stored.Pending.ConfirmedAt.IsZero()
	if want := o.takeBack == takeBackRefused || o.takeBack == takeBackUnknownLost; confirmed != want {
		h.t.Errorf("D1's store holds the task confirmed %v, want %v: the take-back %s", confirmed, want, o.takeBack)
	}
}

// reach is one event at D2. The replay carries the generation the boot read, and at its reserved
// turn that turn too, as the boot's pass posts it (internal/daemon's launchUnfinished).
func (o outsideRestartOrder) reach(h *harness, ev atRestart, ended runtime.Locator, read uint64) {
	h.t.Helper()
	outside := runtime.Observation{Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs}
	neither := runtime.Observation{Locator: ended, Kind: runtime.NotRecordedProcess, At: h.clock.Now()}
	switch ev {
	case atReplay:
		h.must(RecordedDeath{Claim: h.token, Generation: read})
	case atReplayAtTurn:
		h.must(RecordedDeath{Claim: h.token, Generation: read, Turn: h.clock.Now()})
	case atReportOutside:
		h.must(RuntimeObservation{Observation: outside})
	case atReportNeither:
		h.must(RuntimeObservation{Observation: neither})
	case atReportRefused:
		store := &unansweredCheckLaunchStore{memStore: h.store, failing: true}
		h.m.deps.Store = store
		if err := h.handle(RuntimeObservation{Observation: neither}); err == nil {
			h.t.Fatal("the relaunch the report raised was taken, want the lifecycle recheck's refusal of it")
		}
		store.failing = false
	}
}

// wantHeld fails unless the claim still holds the task it held when its pod ended: never lost,
// and never retired, at any step.
func (o outsideRestartOrder) wantHeld(h *harness, task, when string) {
	h.t.Helper()
	if task == "" {
		return
	}
	if p := h.claim().Pending; p == nil || p.Task != task {
		h.t.Fatalf("%s: the claim holds %+v, want its task %q still held", when, p, task)
	}
	if stored, held := h.store.delivery(h.token); !held || stored.Task != task {
		h.t.Fatalf("%s: the store holds %+v (held %v), want the task %q still held", when, stored, held, task)
	}
}

// wantCharged fails unless the claim is charged what the row says: nothing, or the one death
// with the launch failure every death is charged until the relaunch's ready.
func (o outsideRestartOrder) wantCharged(h *harness, when string) {
	h.t.Helper()
	if !o.death {
		h.wantNothingCharged(when)
		return
	}
	if got := h.budgets(); got != (Budgets{LaunchFailures: 1, Deaths: 1}) {
		h.t.Fatalf("%s: budgets %+v, want the one death and its launch failure", when, got)
	}
}

// wantRecord fails unless the claim's record of the end is of the ended launch and says what the
// row says: typed as an end from outside the pod, under the cause the row's record carries, and
// telling only where the end cut off work; or, for a death, a death's.
func (o outsideRestartOrder) wantRecord(h *harness, read uint64, when string) {
	h.t.Helper()
	got := h.claim().EndedWork
	if got.Generation != read {
		h.t.Fatalf("%s: the claim records %+v, want the record of the ended launch %d", when, got, read)
	}
	if o.death {
		if got.FromOutside || !strings.HasPrefix(got.Cause, o.told) {
			h.t.Fatalf("%s: the claim records %+v, want a death's record", when, got)
		}
		return
	}
	cause := endFromOutside
	if o.cause != "" {
		cause = o.cause
	}
	if !got.FromOutside || got.Cause != cause || got.Tells() != (o.told != "") {
		h.t.Fatalf("%s: the claim records %+v, want a record of an end from outside the pod under %q that tells %v",
			when, got, cause, o.told != "")
	}
}

// wantTold fails unless the relaunched role was sent, at its ready, what the row says and
// nothing else: the task it held, once, behind the sentence of the end where the end cut off
// work; the daemon's own relaunch record where it held no task and the end cut off its turn; and
// nothing where it held nothing and the end cut off nothing. An end from outside the pod is never
// told as a death.
func (o outsideRestartOrder) wantTold(h *harness, sent int, task string) {
	h.t.Helper()
	if task == "" && o.told == "" {
		h.wantPrompts(sent)
		return
	}
	told := h.wantPrompts(sent + 1)[sent].Message
	if task != "" && !strings.HasSuffix(told, task) {
		h.t.Fatalf("the relaunched role was sent %q, want the task %q the claim held", told, task)
	}
	says := "ended because " + o.told
	if !o.death {
		says += "."
	}
	switch {
	case o.told == "" && strings.Contains(told, "ended because"):
		h.t.Fatalf("the relaunched role was sent %q, want nothing told of an end that cut off no work", told)
	case o.told != "" && !strings.Contains(told, says):
		h.t.Fatalf("the relaunched role was sent %q, want it to say %q", told, says)
	case !o.death && strings.Contains(told, "the process died"):
		h.t.Fatalf("the relaunched role was sent %q, want an end from outside the pod never told as a death", told)
	}
}
