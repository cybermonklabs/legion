package supervise

// Every order in which an end from outside the pod is met again (dispatch://INFRA-1117 R3, R10),
// on the daemon that read it (D1) and after a daemon restart (D2). D1 got the runtime's first
// report of the claim's process gone, read as an end from outside the pod (endedFromOutside).
// What comes of the next report of that process is decided by what D1's store holds when it
// comes — the record of that end typed as one from outside the pod, the take-back of the task
// whose turn the end cut off, and the decision (Claim.DeathCharged) — by whether a restart comes
// first, by which of the boot's replay (RecordedDeath) and the runtime's report reaches the claim
// first at D2, and by whether that report is read as an end from outside the pod again or neither
// way, its reading gone with D1's memory or expired. One order beside them has D1's first report
// read neither way, so died decides the end as a death, and the later reports read as outside.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// outsideStood is how the claim stood when its pod ended from outside it.
type outsideStood string

const (
	stoodInTaskTurn outsideStood = "in a turn of its confirmed task"
	// stoodInEntryTurn is a turn of a confirmed task of a workflow entry whose prompt still waits
	// on its result: the confirmation is in doubt (confirmationInDoubt), so the take-back gives
	// the handshake's confirmation back and writes the claim with the delivery (interrupted), and
	// the record of the end goes into the store with that write.
	stoodInEntryTurn    outsideStood = "in a turn of its confirmed task of a workflow entry"
	stoodOwingTask      outsideStood = "ready holding its task sent and not confirmed"
	stoodInMessageTurn  outsideStood = "in a message's turn"
	stoodHoldingNothing outsideStood = "holding nothing"
)

// takeBackWrite is what D1's store did of the take-back of the task whose turn the end cut off
// (interrupted), in the table's words.
type takeBackWrite string

const (
	takeBackNone          takeBackWrite = "none"
	takeBackTaken         takeBackWrite = "taken"
	takeBackRefused       takeBackWrite = "refused"
	takeBackUnknownLanded takeBackWrite = "unknown, landed"
	takeBackUnknownLost   takeBackWrite = "unknown, not landed"
)

// failed is whether the take-back answered an error, refused or of unknown outcome, which
// endedFromOutside returns on before it decides anything.
func (w takeBackWrite) failed() bool {
	return w == takeBackRefused || w == takeBackUnknownLanded || w == takeBackUnknownLost
}

// nextEvent is one event that reaches the claim after the end: at D1 where no restart comes, and
// at D2 after one.
type nextEvent string

const (
	atReplay nextEvent = "the boot's replay"
	// atReplayAtTurn is the boot's replay at the turn the boot's pass reserved for it, come
	// already, as it is for the first relaunch of the restart's wave: the wake gate runs a
	// relaunch it raises at once, with no wake parked whose turn would take the task back
	// (reserveWake, runWake).
	atReplayAtTurn  nextEvent = "the boot's replay at its reserved turn"
	atReportOutside nextEvent = "the report read again as outside"
	atReportNeither nextEvent = "the report read neither way"
	// atReportRefused is a report read neither way whose relaunch D2's lifecycle recheck
	// refuses, as it refused D1's; the store answers the recheck again after it.
	atReportRefused nextEvent = "the report read neither way, its relaunch refused"
	// atReportOutsideRefused is a minute passing and then a report read as outside whose
	// relaunch the lifecycle recheck refuses, as it refused the one before; the store answers
	// the recheck again after it.
	atReportOutsideRefused nextEvent = "a minute on, the report read as outside, its relaunch refused"
	// atReportOutsideQueued is a report read as outside behind another claim's wake, taken just
	// before it, so the relaunch the report raises parks as queueAhead parks D1's: the minutes
	// the events before it passed have let the pacing's wait run out.
	atReportOutsideQueued nextEvent = "the report read as outside, behind another claim's wake"
)

// outsideCell is one cell of the product of the table's six dimensions, each read when the event
// after the end comes: whether D1's store holds the record of the end typed as one from outside
// the pod, what it did of the take-back, whether it holds the decision, whether that event is a
// restart, which of the runtime's report and the boot's replay reaches the claim first at D2, and
// whether the report is read as outside or neither way.
type outsideCell struct {
	recordStored   bool
	takeBack       takeBackWrite
	decisionStored bool
	restart        bool
	replayFirst    bool
	outside        bool
}

// cannotHappen is the rule of the table that says the cell cannot happen, the first of the five
// that takes it, and "" for a cell that can.
func (c outsideCell) cannotHappen() string {
	switch {
	case c.decisionStored && !c.recordStored:
		// I1: one claim row carries both, and endedFromOutside puts the record in memory before
		// it sets DeathCharged, so no write stores the decision without the record.
		return "I1"
	case c.decisionStored && c.takeBack.failed():
		// I2: endedFromOutside returns on the take-back's error before it sets DeathCharged.
		return "I2"
	case c.replayFirst && !c.restart:
		// I3: only the boot of a restarted daemon posts the replay.
		return "I3"
	case c.replayFirst && !c.decisionStored:
		// I4: the boot replays only a claim stored with DeathCharged (recordedDeaths).
		return "I4"
	case c.recordStored && c.takeBack == takeBackNone && !c.decisionStored:
		// I5: with nothing confirmed the take-back writes nothing, so the decision is in memory
		// beside the record before any write, and every write that lands stores both.
		return "I5"
	}
	return ""
}

// String is the cell's six values, as the table writes them.
func (c outsideCell) String() string {
	stored := map[bool]string{true: "stored", false: "not stored"}
	restart, first, reading := "no restart", "first -", "read outside"
	if c.restart {
		restart, first = "restart", "report first"
		if c.replayFirst {
			first = "replay first"
		}
	}
	if !c.outside {
		reading = "read neither way"
	}
	return fmt.Sprintf("record %s, take-back %s, decision %s, %s, %s, %s",
		stored[c.recordStored], c.takeBack, stored[c.decisionStored], restart, first, reading)
}

// order is the cell as the walk makes it, under the number n the table gives it: the table's
// "How the test makes it", its events and what it must hold.
func (c outsideCell) order(n int) outsideOrder {
	o := outsideOrder{name: fmt.Sprintf("row %d: %s", n, c), stood: stoodInTaskTurn,
		recordStored: c.recordStored, takeBack: c.takeBack, decisionStored: c.decisionStored,
		restart: c.restart, told: endFromOutside}
	switch {
	case c.takeBack == takeBackNone:
		o.stood, o.told = stoodOwingTask, ""
	case c.recordStored && c.takeBack == takeBackTaken && !c.decisionStored:
		o.stood = stoodInEntryTurn
	}
	report := atReportOutside
	if !c.outside {
		report = atReportNeither
	}
	switch {
	case c.replayFirst:
		o.at = []nextEvent{atReplay, report}
	case c.restart && c.decisionStored:
		o.at = []nextEvent{report, atReplay}
	default:
		o.at = []nextEvent{report}
	}
	if c.restart && !c.recordStored && !c.outside {
		// A death of a claim holding a task it never confirmed cut off no work, and a report
		// read neither way names no cause the runtime read, so it records nothing for the next
		// process to be told (recordEnd).
		o.death = true
		if o.stood != stoodOwingTask {
			o.told = "the process died"
		}
	}
	return o
}

// possibleOrders is the table's 48 rows, in its order and under its numbers: the product of the
// six dimensions, 160 cells, less the 112 that its five rules say cannot happen. It fails the
// test unless each rule takes the number of cells the table counts for it.
func possibleOrders(t *testing.T) []outsideOrder {
	t.Helper()
	var orders []outsideOrder
	ruled := map[string]int{}
	takeBacks := []takeBackWrite{takeBackNone, takeBackTaken, takeBackRefused, takeBackUnknownLanded, takeBackUnknownLost}
	for _, recordStored := range []bool{true, false} {
		for _, takeBack := range takeBacks {
			for _, decisionStored := range []bool{true, false} {
				for _, restart := range []bool{false, true} {
					for _, replayFirst := range []bool{false, true} {
						for _, outside := range []bool{true, false} {
							c := outsideCell{recordStored, takeBack, decisionStored, restart, replayFirst, outside}
							if rule := c.cannotHappen(); rule != "" {
								ruled[rule]++
								continue
							}
							orders = append(orders, c.order(len(orders)+1))
						}
					}
				}
			}
		}
	}
	want := map[string]int{"I1": 40, "I2": 24, "I3": 24, "I4": 20, "I5": 4}
	if !maps.Equal(ruled, want) || len(orders) != 48 {
		t.Fatalf("the product leaves %d cells, its rules taking %v; want the table's 48, the rules taking %v",
			len(orders), ruled, want)
	}
	return orders
}

// besideTheProduct is the table's rows beside the product: the variations its six dimensions do
// not name.
func besideTheProduct() []outsideOrder {
	decided := func(o outsideOrder) outsideOrder {
		o.recordStored, o.decisionStored, o.restart = true, true, true
		return o
	}
	return []outsideOrder{
		decided(outsideOrder{
			name:  "beside the product: holding nothing, the decision stored, restart, the report read neither way, then the replay",
			stood: stoodHoldingNothing, takeBack: takeBackNone, at: []nextEvent{atReportNeither, atReplay},
		}),
		decided(outsideOrder{
			name:  "beside the product: in a message's turn, the decision stored, restart, the report read neither way, then the replay",
			stood: stoodInMessageTurn, takeBack: takeBackNone, at: []nextEvent{atReportNeither, atReplay}, told: endFromOutside,
		}),
		decided(outsideOrder{
			name:  "beside the product: the relaunch refused at D1 and at D2, then the report read neither way",
			stood: stoodInTaskTurn, takeBack: takeBackTaken, refused: true,
			at: []nextEvent{atReportRefused, atReportNeither}, told: endFromOutside,
		}),
		decided(outsideOrder{
			name:  "beside the product: the earlier daemon's cause, restart, the replay alone",
			stood: stoodInTaskTurn, takeBack: takeBackTaken, cause: earlierOutsideWords,
			at: []nextEvent{atReplay}, told: earlierOutsideWords,
		}),
		decided(outsideOrder{
			name:  "beside the product: the earlier daemon's cause, restart, the report read neither way, then the replay",
			stood: stoodInTaskTurn, takeBack: takeBackTaken, cause: earlierOutsideWords,
			at: []nextEvent{atReportNeither, atReplay}, told: earlierOutsideWords,
		}),
		{
			name:  "beside the product: the take-back refused, restart, the replay at its reserved turn over no decision, then the report read outside",
			stood: stoodInTaskTurn, takeBack: takeBackRefused, restart: true,
			at: []nextEvent{atReplayAtTurn, atReportOutside}, told: endFromOutside,
		},
		{
			name:  "beside the product: the take-back of unknown outcome not landed, restart, the replay at its reserved turn over no decision, then the report read outside",
			stood: stoodInTaskTurn, takeBack: takeBackUnknownLost, restart: true,
			at: []nextEvent{atReplayAtTurn, atReportOutside}, told: endFromOutside,
		},
		{
			name:  "beside the product: the first report read neither way, its relaunch refused, then the report read as outside three times a minute apart, its relaunch refused",
			stood: stoodInTaskTurn, firstNeither: true, takeBack: takeBackTaken, decisionStored: true, refused: true,
			at:    []nextEvent{atReportOutsideRefused, atReportOutsideRefused, atReportOutsideRefused, atReportOutsideQueued},
			death: true, told: "the process died",
		},
	}
}

// outsideOrder is one order of the table of orders: how the claim stood, what D1's store held
// or did of the end's writes when the next event came, and the events after the end, at D1
// where no restart comes and at D2 after one, in order; death and told state what must hold
// where the rows differ. What holds of every row is read in walk: the role is continued in its
// own recorded session after the ended process, the task the claim held is still held at every
// step and sent again once at the relaunch's ready, a repeated report moves nothing, and nothing
// is charged but where death says so.
type outsideOrder struct {
	// name is the row's number and its six values, or the variation beside the product it is,
	// so a failing row reads as the order it is.
	name  string
	stood outsideStood
	// recordStored and decisionStored are whether D1's store holds the record of the end typed
	// as one from outside the pod, and the decision (Claim.DeathCharged), when the next event
	// comes; takeBack is what it did of the take-back.
	recordStored   bool
	takeBack       takeBackWrite
	decisionStored bool
	// refused is whether D1's lifecycle recheck refused the relaunch the decision raised; the
	// wake pacing parks it otherwise.
	refused bool
	// firstNeither is whether D1's first report is read neither way, so died decides the end as
	// a death: one death charged, the end of the turn recorded (chargeDeath), and the record of
	// the ended launch a death's. Every other row's first report is read as outside.
	firstNeither bool
	// cause is the sentence an earlier daemon wrote as the stored record's cause, over the
	// record D1 typed as an end from outside the pod, and "" for today's endFromOutside.
	cause string
	// restart is whether the next event is a daemon restart; at is the events after the end.
	restart bool
	at      []nextEvent
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

// claimRefusedStore refuses the claim's own write (PutClaim), as a store that refuses the
// decision's write does, and takes the claim written with the delivery in one transaction
// (PutClaimAndDelivery), which memStore makes through its own PutClaim. The take-back of a
// confirmation in doubt writes the claim so (interrupted), so the record of the end lands with
// it while the decision does not.
type claimRefusedStore struct{ *memStore }

func (claimRefusedStore) PutClaim(context.Context, Claim) error { return errStoreRefused }

// Every possible order of an end from outside the pod met again, one subtest for each row of the
// table of orders (dispatch://INFRA-1117/artifact/infra-1117-every-order-of-an-outside-end-md):
// the 48 cells of the product of its six dimensions that can happen, named by the table's row
// number and the six values, and the rows beside the product it lists. Each row stands the claim
// and makes D1's store answer the end's writes as the row says, with a real write wherever the
// record is stored and the decision is not: an Uncertain report's (judge) after a take-back that
// failed, or the take-back's own claim write where the confirmation it takes back is in doubt
// (interrupted). It holds D1's store to the row before the next event, and then runs the row's
// events, at D1 or across a restart.
//
// An end the daemon read as from outside the pod is continued charged nothing however each later
// report of the process is read, on one daemon and after a restart wherever the record of that
// end reached the store, since judge sends every report of a process whose end the claim records
// so to endedFromOutside (recordedFromOutside). A report over an end already decided decides
// nothing again (decidedFromOutside): no charge, and the end of the turn (Claim.LastTurnEnd)
// stays where the decision put it, in the claim and in the store, through every event after it,
// a repeated report a second later included. The role is resumed in its own recorded session,
// holds its task at every step, in the claim and in the store, and is sent it once at the
// relaunch's ready, and an end from outside the pod is never told as a death. Of the product's
// rows, only the five where nothing stored and nothing read says the end came from outside the
// pod (32, 36, 40, 44 and 48) are counted as a stop of the role's process: one death, and the
// task still sent again. Row 38 is the take-back refused on one daemon and the next report read
// neither way; row 20 is the same with the record stored by an Uncertain report's write and a
// restart before the report. Both are charged nothing. Beside the product, a first report read
// neither way is a death that died charges and decides at D1, its relaunch refused; the reports
// read as outside after it decide nothing again: they charge nothing more and leave the end of
// the turn where the death put it.
//
// The negative controls: judge routing on decidedFromOutside in place of the record, so a report
// read neither way over a record with no decision goes to died (rows 16, 18, 20, 22, 24, 26, 28,
// 38, 42 and 46 charge a death); and endedFromOutside deciding again at every report, so D2's
// report over a decision D1 stored for a claim in a turn moves the end of the turn (rows 9 and
// 10, and the rows beside the product of a message's turn, of the relaunch refused at both
// daemons, of the earlier daemon's cause with the report first and of the first report read
// neither way).
func TestAnEndFromOutsideMetAgainIsContinuedUnchargedInEveryPossibleOrder(t *testing.T) {
	for _, o := range append(possibleOrders(t), besideTheProduct()...) {
		t.Run(o.name, o.walk)
	}
}

// walk is the order: the end at D1 as the row has it, what D1's store holds then, the restart
// where the row has one, the events after the end in order, a repeated report, the relaunch's
// turn and its ready, and at each step what the row says must hold.
func (o outsideOrder) walk(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	task := o.stand(h)
	queueAhead(h, 1)
	ended, read := h.locator(), h.generation()

	o.breakD1(h)
	end := runtime.Observation{Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs}
	if o.firstNeither {
		end.Kind, end.EndReading = runtime.NotRecordedProcess, ""
	}
	err := h.handle(RuntimeObservation{Observation: end})
	if want := o.d1Fails(); (err != nil) != want {
		t.Fatalf("the end at D1 answered %v, want an error %v", err, want)
	}
	if !o.takeBack.failed() && !o.refused {
		h.wantWaitingWake(WakeLaunch)
	}
	answerEveryWrite(h)
	if o.recordStored && o.takeBack.failed() {
		// The store answers again, and an Uncertain report's write (judge) carries the record
		// the end left in memory into it, with no decision.
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.Uncertain, At: h.clock.Now()}})
	}
	if o.firstNeither {
		o.wantCharged(h, "the end at D1")
	} else {
		h.wantNothingCharged("the end at D1")
	}
	o.wantStoredAtD1(h, task, read)
	if o.cause != "" {
		c := h.store.load(h.token)
		c.EndedWork.Cause = o.cause
		if err := h.store.PutClaim(h.ctx, c); err != nil {
			t.Fatalf("write the earlier daemon's record: %v", err)
		}
	}

	var turnEnd turnEndWatch
	// want fails the row at when on the first of wrong that is not "".
	want := func(when string, wrong ...string) {
		t.Helper()
		if got := cmp.Or(wrong...); got != "" {
			t.Fatalf("%s: %s", when, got)
		}
	}
	want("the end at D1", turnEnd.see(h))
	h.advance(time.Second)
	sent := len(h.prompts())
	if o.restart {
		h.restart()
		turnEnd.restarted(h)
		want("the restart", turnEnd.see(h))
	}
	for _, ev := range o.at {
		o.reach(h, ev, ended, read)
		want(string(ev), notHeld(h, task))
		o.wantCharged(h, string(ev))
		want(string(ev), turnEnd.see(h))
	}
	o.wantRecord(h, read, "the events after the end")

	charged := h.budgets()
	h.advance(time.Second)
	o.reach(h, atReportNeither, ended, read)
	if got := h.budgets(); got != charged {
		t.Fatalf("a repeated report left budgets %+v, want %+v: it charges nothing new", got, charged)
	}
	if !turnEnd.claim.decided {
		t.Fatalf("a repeated report: the claim reads deathCharged=%v, want the end decided", h.claim().DeathCharged)
	}
	want("a repeated report", turnEnd.see(h), notHeld(h, task))

	waiting := h.wantWaitingWake(WakeLaunch)
	h.advance(waiting.At.Sub(h.clock.Now()))
	resume := h.wantCalls("Resume", 1)[0]
	if resume.Previous == nil || *resume.Previous != ended || resume.Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("resumed %+v from %+v, want the claim's own recorded session after the ended incarnation",
			resume.Spec, resume.Previous)
	}
	o.wantCharged(h, "the relaunch")
	want("the relaunch", notHeld(h, task), turnEnd.see(h))
	o.wantRecord(h, read, "the relaunch")

	h.relaunched()
	if o.death {
		h.wantBudgets(Budgets{Deaths: 1})
	} else {
		h.wantNothingCharged("the relaunch's ready")
	}
	want("the relaunch's ready", notHeld(h, task), turnEnd.see(h))
	o.wantTold(h, sent, task)
}

// stand brings the claim to how it stood when its pod ended, and answers the task it held, ""
// for none.
func (o outsideOrder) stand(h *harness) string {
	h.t.Helper()
	switch o.stood {
	case stoodInTaskTurn:
		h.reach(StateWorking)
		if p := h.pending(); p.Task != owedTask || p.ConfirmedAt.IsZero() {
			h.t.Fatalf("pending %+v, want the task %q confirmed by the turn the end cuts off", p, owedTask)
		}
		return owedTask
	case stoodInEntryTurn:
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: owedTask, ID: "outbox:40", Row: 40,
			Phase: phase.Implementing, Generation: 7, Entry: 1})
		h.must(StreamTurnStart{Claim: h.token})
		p, handshake := h.pending(), h.claim().Handshake
		if p.Task != owedTask || p.ConfirmedAt.IsZero() || !slices.Contains(h.outstanding(), p.ID) ||
			!handshake.ConfirmedAt.Equal(p.ConfirmedAt) {
			h.t.Fatalf("pending %+v with prompts %v outstanding and the handshake %+v, want the entry's task confirmed by the turn the end cuts off and its prompt waiting on its result",
				p, h.outstanding(), handshake)
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

// breakD1 sets D1's store to answer the end's writes as the row says: the take-back refused or of
// unknown outcome, the decision's write refused where the take-back lands and the row stores no
// decision, and the relaunch refused by the lifecycle recheck.
func (o outsideOrder) breakD1(h *harness) {
	switch o.takeBack {
	case takeBackRefused:
		h.store.fail("PutDelivery", errStoreRefused)
	case takeBackUnknownLanded, takeBackUnknownLost:
		h.m.deps.Store = &unknownOutcomeStore{memStore: h.store, landed: o.takeBack == takeBackUnknownLanded}
	}
	switch {
	case o.decisionStored || o.takeBack.failed():
	case o.recordStored:
		h.m.deps.Store = claimRefusedStore{h.store}
	default:
		h.store.fail("PutClaim", errStoreRefused)
	}
	if o.refused {
		h.m.deps.Store = &unansweredCheckLaunchStore{memStore: h.store, failing: true}
	}
}

// d1Fails is whether the end at D1 answers an error: a take-back the store refused or whose
// outcome is unknown, a decision's write the store refused, or a relaunch the lifecycle recheck
// refused.
func (o outsideOrder) d1Fails() bool {
	return o.takeBack.failed() || !o.decisionStored || o.refused
}

// wantStoredAtD1 fails unless D1's store holds what the row says before the next event: the
// record of the ended launch typed as an end from outside the pod, the decision, and the task
// still confirmed where the take-back did not land, taken back where it did.
func (o outsideOrder) wantStoredAtD1(h *harness, task string, read uint64) {
	h.t.Helper()
	stored := h.store.load(h.token)
	recorded := stored.EndedWork.FromOutside && stored.EndedWork.Generation == read
	if recorded != o.recordStored || stored.DeathCharged != o.decisionStored {
		h.t.Fatalf("D1's store holds the record %+v and deathCharged=%v, want the record of the end stored %v and the decision stored %v",
			stored.EndedWork, stored.DeathCharged, o.recordStored, o.decisionStored)
	}
	if task == "" {
		return
	}
	if stored.Pending == nil || stored.Pending.Task != task {
		h.t.Fatalf("D1's store holds the task %+v, want %q", stored.Pending, task)
	}
	confirmed := !stored.Pending.ConfirmedAt.IsZero()
	if want := o.takeBack == takeBackRefused || o.takeBack == takeBackUnknownLost; confirmed != want {
		h.t.Fatalf("D1's store holds the task confirmed %v, want %v: the take-back %s", confirmed, want, o.takeBack)
	}
}

// reach is one event after the end. The replay carries the generation the boot read, and at its
// reserved turn that turn too, as the boot's pass posts it (internal/daemon's launchUnfinished).
func (o outsideOrder) reach(h *harness, ev nextEvent, ended runtime.Locator, read uint64) {
	h.t.Helper()
	if ev == atReportOutsideRefused {
		h.advance(time.Minute)
	}
	outside := runtime.Observation{Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs}
	neither := runtime.Observation{Locator: ended, Kind: runtime.NotRecordedProcess, At: h.clock.Now()}
	switch ev {
	case atReplay:
		h.must(RecordedDeath{Claim: h.token, Generation: read})
	case atReplayAtTurn:
		h.must(RecordedDeath{Claim: h.token, Generation: read, Turn: h.clock.Now()})
	case atReportOutside:
		h.must(RuntimeObservation{Observation: outside})
	case atReportOutsideQueued:
		next := waveToken(1)
		h.mustPostTo(h.machineFor(wakeClaim(next, StateQueued)), RequestSpawn{Claim: next})
		h.must(RuntimeObservation{Observation: outside})
	case atReportNeither:
		h.must(RuntimeObservation{Observation: neither})
	case atReportRefused, atReportOutsideRefused:
		report := neither
		if ev == atReportOutsideRefused {
			report = outside
		}
		store := &unansweredCheckLaunchStore{memStore: h.store, failing: true}
		h.m.deps.Store = store
		if err := h.handle(RuntimeObservation{Observation: report}); err == nil {
			h.t.Fatal("the relaunch the report raised was taken, want the lifecycle recheck's refusal of it")
		}
		store.failing = false
	}
}

// turnEndWatch holds the end of the turn (Claim.LastTurnEnd) where the decision of the end put
// it, in the claim and in the store, for both walks. The end is decided once (Claim.DeathCharged,
// by died or by endedFromOutside, which decidedFromOutside keeps from deciding again), so no step
// after the decision moves it. A decision D1 made in memory and its store did not take is gone
// with D1's memory, and D2's is the one decision there is, so a restart that restores the claim
// undecided starts the claim's side again (restarted); the store's side never starts again.
type turnEndWatch struct{ claim, stored turnEndSide }

// turnEndSide is one side of a turnEndWatch: whether it reads the end decided yet, and the end of
// the turn the decision put there.
type turnEndSide struct {
	decided bool
	at      time.Time
}

// see answers how the last step moved the end of the turn after the decision, in the claim or in
// the store, and "" where it did not; a side that reads the end decided first takes it there.
func (w *turnEndWatch) see(h *harness) string {
	return cmp.Or(w.claim.see("the claim's", h.claim()), w.stored.see("the stored claim's", h.store.load(h.token)))
}

// see is turnEndWatch.see for one side, whose naming it in the answer and c its claim.
func (s *turnEndSide) see(whose string, c Claim) string {
	switch {
	case s.decided && !c.LastTurnEnd.Equal(s.at):
		return fmt.Sprintf("%s lastTurnEnd moved to %s, want %s, where the decision of the end put it",
			whose, c.LastTurnEnd, s.at)
	case !s.decided && c.DeathCharged:
		s.decided, s.at = true, c.LastTurnEnd
	}
	return ""
}

// restarted starts the claim's side again where the claim D2's boot restored reads undecided: a
// decision D1's store took is restored as it stood, and one it did not take is gone.
func (w *turnEndWatch) restarted(h *harness) {
	if !h.claim().DeathCharged {
		w.claim = turnEndSide{}
	}
}

// notHeld answers how the claim or the store no longer holds task, the task the claim held when
// its pod ended, and "" where both still hold it: it is never lost, and never retired, at any
// step. A claim that held no task has none to hold.
func notHeld(h *harness, task string) string {
	if task == "" {
		return ""
	}
	if p := h.claim().Pending; p == nil || p.Task != task {
		return fmt.Sprintf("the claim holds %+v, want its task %q still held", p, task)
	}
	if stored, held := h.store.delivery(h.token); !held || stored.Task != task {
		return fmt.Sprintf("the store holds %+v (held %v), want the task %q still held", stored, held, task)
	}
	return ""
}

// answerEveryWrite makes the machine's store answer every write again: no write refused, and the
// harness's own store in place of one that wraps it, a restarted machine's included.
func answerEveryWrite(h *harness) {
	h.store.fail("PutDelivery", nil)
	h.store.fail("PutClaim", nil)
	h.m.deps.Store = h.store
}

// wantCharged fails unless the claim is charged what the row says: nothing, or the one death
// with the launch failure every death is charged until the relaunch's ready. A death D1's first
// report decided (firstNeither) under a relaunch the lifecycle recheck refused is charged no
// launch failure: the first raise of the death's relaunch that passes the recheck charges it
// (relaunchAfterFailure), and the reports after it are read as outside, whose relaunch charges
// none.
func (o outsideOrder) wantCharged(h *harness, when string) {
	h.t.Helper()
	if !o.death {
		h.wantNothingCharged(when)
		return
	}
	want := Budgets{LaunchFailures: 1, Deaths: 1}
	if o.firstNeither && o.refused {
		want.LaunchFailures = 0
	}
	if got := h.budgets(); got != want {
		h.t.Fatalf("%s: budgets %+v, want %+v: the one death, and its launch failure where its relaunch charged it",
			when, got, want)
	}
}

// wantRecord fails unless the claim's record of the end is of the ended launch and says what the
// row says: typed as an end from outside the pod, under the cause the row's record carries, and
// telling only where the end cut off work; or, for a death, a death's, and nothing that tells
// where the death cut off no work.
func (o outsideOrder) wantRecord(h *harness, read uint64, when string) {
	h.t.Helper()
	got := h.claim().EndedWork
	if o.death && o.told == "" {
		if got.Tells() {
			h.t.Fatalf("%s: the claim records %+v, want nothing told of a death that cut off no work", when, got)
		}
		return
	}
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
func (o outsideOrder) wantTold(h *harness, sent int, task string) {
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

// outsideSeeds and outsideSteps bound TestAnEndFromOutsideHoldsItsInvariantsInRandomOrders: the
// seeds it walks, one subtest each, and the most random steps one walk takes after the end.
const (
	outsideSeeds = 1000
	outsideSteps = 10
)

// The faults a random walk sets for the next writes (outsideWalk.setFault).
const (
	faultTakeBackRefused = "the take-back's PutDelivery refused"
	faultClaimRefused    = "the claim's PutClaim refused"
	faultUnknownLanded   = "writes of unknown outcome, landed"
	faultUnknownLost     = "writes of unknown outcome, not landed"
	// faultRelaunchRefused is the lifecycle recheck's refusal of every relaunch raised while it
	// is set, which leaves the claim over its ended process with no relaunch waiting, so the
	// reports after it reach judge rather than being dropped while one waits (launchWaits).
	faultRelaunchRefused = "the relaunch refused by the lifecycle recheck"
)

// An end from outside the pod holds its invariants in random orders of the events that can meet
// it, against the real machine and this file's harness, from a fixed list of seeds, so every run
// walks the same orders and a failing seed is replayed by its subtest's name. Each seed stands
// the claim in a turn of its confirmed task or ready holding its task sent and not confirmed,
// parks the relaunch behind another claim's wake, maybe sets a fault for the next writes, and
// sends the end: in about two seeds of three a Gone report read as an end from outside the pod,
// and in the rest a report read neither way, which died decides as a death (diedFirst), and a
// second after it. Then come up to outsideSteps random steps, each one of a report read as
// outside, a report read neither way, an Uncertain report (a claim write), a fault set for the
// next writes (a write refused or of unknown outcome, or the relaunch refused by the lifecycle
// recheck), the faults cleared, a second passing, a daemon restart, and the boot's replay a
// restart owes where its store held the decision (internal/daemon's recordedDeaths). An event
// the machine answers with an error is allowed. The walk closes with the faults cleared, the
// replay still owed, one report read neither way, the relaunch's turn and its ready.
//
// After every step, with the seed and the steps walked in each failure:
//
//  1. Where the end's first report was read as outside and no restart came while the store held
//     no record of the ended launch typed as an end from outside the pod, the claim is charged
//     nothing, in memory or in the store; otherwise, at most one death and its launch failure.
//  2. The claim and the store hold the task at every step: it is never retired.
//  3. Once the claim reads the end decided (Claim.DeathCharged), the end of the turn
//     (Claim.LastTurnEnd) does not move in it, and once the stored claim reads it decided, not in
//     the store either (turnEndWatch). A decision D1 made in memory and its store refused is gone
//     at a restart, and D2 makes it again, so the claim's watch starts again where the restored
//     claim is undecided; the store's never does.
//  4. At the relaunch's ready the task is sent once, never twice.
//
// The negative controls are the table test's: judge routing on decidedFromOutside fails
// invariant 1, a report read neither way over a record with no decision charged a death, on one
// daemon or after a restart that kept the record; and endedFromOutside deciding again at every
// report fails invariant 3, D2's report before the replay over a stored decision moving the end
// of the turn after the decision.
func TestAnEndFromOutsideHoldsItsInvariantsInRandomOrders(t *testing.T) {
	for seed := uint64(1); seed <= outsideSeeds; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			newOutsideWalk(t, seed).run()
		})
	}
}

// outsideWalk is one random order of the events that can meet an end from outside the pod, and
// what its invariants read of the steps it has walked.
type outsideWalk struct {
	t    *testing.T
	seed uint64
	rng  *rand.Rand
	// more is a second stream from the seed, of the choices drawn beside rng's: whether the end's
	// first report is read neither way, and whether a fault set is the relaunch's refusal. Drawn
	// apart, they leave every draw of rng as it is, so a seed that takes neither walks the order
	// rng alone gives it.
	more *rand.Rand
	h    *harness
	// ended and generation are the process the end ended and its launch, task the task the claim
	// held, and sent how many prompts went out before the end.
	ended      runtime.Locator
	generation uint64
	task       string
	sent       int
	// walked is every step so far, in order, which a failure prints with the seed.
	walked []string
	// fault is the fault set for the next writes, "" for none.
	fault string
	// replayOwed is whether the last restart's boot owes the claim its replay, and replayRead the
	// generation that boot read.
	replayOwed bool
	replayRead uint64
	// lost is whether a restart came while the store held no record of the end as one from
	// outside the pod.
	lost bool
	// diedFirst is whether the end's first report was read neither way, so died decided the end
	// as a death.
	diedFirst bool
	// turnEnd holds the end of the turn where the decision of the end put it (invariant 3).
	turnEnd turnEndWatch
}

func newOutsideWalk(t *testing.T, seed uint64) *outsideWalk {
	w := &outsideWalk{t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, 0)), more: rand.New(rand.NewPCG(seed, 1)),
		h: pacedHarness(t, wakeRate)}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("seed %d walked: %s", seed, strings.Join(w.walked, "; "))
		}
	})
	return w
}

// fail ends the walk with the seed and the steps walked so far in front of what failed.
func (w *outsideWalk) fail(format string, args ...any) {
	w.t.Helper()
	w.t.Fatalf("seed %d, after %s: %s", w.seed, strings.Join(w.walked, "; "), fmt.Sprintf(format, args...))
}

// run walks the seed's order: the claim stood, the end, the random steps and the close.
func (w *outsideWalk) run() {
	h := w.h
	w.diedFirst = w.more.IntN(3) == 0
	if w.rng.IntN(2) == 0 {
		h.reach(StateWorking)
		w.walked = append(w.walked, "stood in a turn of its confirmed task")
	} else {
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: owedTask})
		w.walked = append(w.walked, "stood ready holding its task sent and not confirmed")
	}
	queueAhead(h, 1)
	w.ended, w.generation, w.task, w.sent = h.locator(), h.generation(), owedTask, len(h.prompts())
	second := func() { w.step("a second passed", func() error { h.advance(time.Second); return nil }) }
	// A fault before the end, so the end's own take-back and decision can fail as a later
	// report's can.
	if w.rng.IntN(2) == 0 {
		w.setFault()
	}
	if w.diedFirst {
		w.report("the end, read neither way", runtime.NotRecordedProcess, "")
		// The runtime reads the end as one from outside the pod only at a later event of the pod,
		// the one that carries DisruptionTarget, so the reports after this one come later.
		second()
	} else {
		w.report("the end, read as outside", runtime.Gone, runtime.PodEndNotOurs)
	}
	for range w.rng.IntN(outsideSteps + 1) {
		steps := []func(){
			func() { w.report("a report read as outside", runtime.Gone, runtime.PodEndNotOurs) },
			func() { w.report("a report read neither way", runtime.NotRecordedProcess, "") },
			func() { w.report("an Uncertain report", runtime.Uncertain, "") },
			w.setFault,
			w.clearFaults,
			second,
			w.restart,
		}
		if w.replayOwed {
			steps = append(steps, w.replay)
		}
		steps[w.rng.IntN(len(steps))]()
	}
	w.clearFaults()
	if w.replayOwed {
		w.replay()
	}
	w.report("a report read neither way", runtime.NotRecordedProcess, "")
	w.relaunch()
}

// step runs one step, notes it, and checks the invariants after it. Its error is allowed: a
// write the store refused or whose outcome is unknown fails the event that made it.
func (w *outsideWalk) step(name string, run func() error) {
	w.t.Helper()
	if err := run(); err != nil {
		name += " (answered: " + err.Error() + ")"
	}
	w.walked = append(w.walked, name)
	w.check()
}

// report is a step of the runtime's report of the ended process, of kind read as reading.
func (w *outsideWalk) report(name string, kind runtime.ObservationKind, reading runtime.PodEndReading) {
	w.step(name, func() error {
		return w.h.handle(RuntimeObservation{Observation: runtime.Observation{
			Locator: w.ended, Kind: kind, At: w.h.clock.Now(), EndReading: reading,
		}})
	})
}

// setFault is a step that sets one fault, at random, for the next writes: one of the store's
// four, or, one time in five, the lifecycle recheck's refusal of the relaunch, drawn from more.
func (w *outsideWalk) setFault() {
	faults := []string{faultTakeBackRefused, faultClaimRefused, faultUnknownLanded, faultUnknownLost}
	fault := faults[w.rng.IntN(len(faults))]
	if w.more.IntN(len(faults)+1) == 0 {
		fault = faultRelaunchRefused
	}
	w.step("a fault set: "+fault, func() error {
		w.fault = fault
		w.applyFault()
		return nil
	})
}

// clearFaults is a step after which the store answers every write again.
func (w *outsideWalk) clearFaults() {
	w.step("the faults cleared", func() error {
		w.fault = ""
		w.applyFault()
		return nil
	})
}

// applyFault makes the machine's store answer as the fault set says, the store of a machine a
// restart built included.
func (w *outsideWalk) applyFault() {
	h := w.h
	answerEveryWrite(h)
	switch w.fault {
	case faultTakeBackRefused:
		h.store.fail("PutDelivery", errStoreRefused)
	case faultClaimRefused:
		h.store.fail("PutClaim", errStoreRefused)
	case faultUnknownLanded, faultUnknownLost:
		h.m.deps.Store = &unknownOutcomeStore{memStore: h.store, landed: w.fault == faultUnknownLanded}
	case faultRelaunchRefused:
		h.m.deps.Store = &unansweredCheckLaunchStore{memStore: h.store, failing: true}
	}
}

// restart is a step of a daemon restart: the machine is built from what the store holds, the
// fault set stands, and the boot owes the claim its replay where the store holds the decision of
// a live claim recording a process, as internal/daemon's recordedDeaths selects.
func (w *outsideWalk) restart() {
	w.step("a restart", func() error {
		h := w.h
		h.restart()
		w.applyFault()
		stored := h.store.load(h.token)
		if !stored.EndedWork.FromOutside || stored.EndedWork.Generation != w.generation {
			w.lost = true
		}
		w.turnEnd.restarted(h)
		w.replayOwed = stored.DeathCharged && stored.Locator != nil && slices.Contains(LiveStates(), stored.State)
		w.replayRead = stored.Generation
		return nil
	})
}

// replay is a step of the boot's replay the last restart owes, with the generation it read.
func (w *outsideWalk) replay() {
	w.replayOwed = false
	w.step("the boot's replay", func() error {
		return w.h.handle(RecordedDeath{Claim: w.h.token, Generation: w.replayRead})
	})
}

// relaunch is the close's relaunch: its turn, where it still waits for one, and its ready, at
// which the task is sent once (invariant 4).
func (w *outsideWalk) relaunch() {
	h := w.h
	if c := h.claim(); c.Generation == w.generation {
		waiting := h.m.View().WakeWaiting
		if waiting == nil || waiting.Kind != WakeLaunch {
			w.fail("the claim waits for %+v, want its relaunch waiting its turn", waiting)
		}
		w.step("the relaunch's turn", func() error { h.advance(waiting.At.Sub(h.clock.Now())); return nil })
	}
	w.step("the relaunch's ready", func() error {
		h.relaunched()
		return nil
	})
	prompts := h.prompts()[w.sent:]
	if len(prompts) != 1 || !strings.HasSuffix(prompts[0].Message, w.task) {
		w.fail("the relaunched role was sent %+v, want its task %q once", prompts, w.task)
	}
}

// check holds the invariants after a step (TestAnEndFromOutsideHoldsItsInvariantsInRandomOrders).
func (w *outsideWalk) check() {
	w.t.Helper()
	h := w.h
	c, stored := h.claim(), h.store.load(h.token)
	switch {
	case !w.lost && !w.diedFirst && (c.Budgets != Budgets{} || stored.Budgets != Budgets{}):
		w.fail("the claim carries budgets %+v (%+v stored), want nothing charged for an end from outside the pod that no restart lost the record of",
			c.Budgets, stored.Budgets)
	case !atMostOneDeath(c.Budgets) || !atMostOneDeath(stored.Budgets):
		w.fail("the claim carries budgets %+v (%+v stored), want at most the one death and its launch failure",
			c.Budgets, stored.Budgets)
	}
	if wrong := cmp.Or(notHeld(h, w.task), w.turnEnd.see(h)); wrong != "" {
		w.fail("%s", wrong)
	}
}

// atMostOneDeath is whether b charges nothing but one death and its launch failure at most.
func atMostOneDeath(b Budgets) bool {
	return b == Budgets{LaunchFailures: b.LaunchFailures, Deaths: b.Deaths} && b.LaunchFailures <= 1 && b.Deaths <= 1
}
