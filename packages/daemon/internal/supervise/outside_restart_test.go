package supervise

// Every order in which an end from outside the pod is met again (dispatch://INFRA-1117 R3, R10),
// on the daemon that read it (D1) and after a daemon restart (D2). D1 got the runtime's first
// report of the claim's process gone, read as an end from outside the pod (endedFromOutside).
// What comes of the next report of that process is decided by what D1's store holds when it
// comes — the record of that end typed as one from outside the pod, the take-back of the task
// whose turn the end cut off, and the decision (Claim.DeathCharged) — by whether a restart comes
// first, by which of the boot's replay (RecordedDeath) and the runtime's report reaches the claim
// first at D2, and by whether that report is read as an end from outside the pod again or neither
// way, its reading gone with D1's memory or expired. Three orders beside them have D1's first
// report read neither way, so died judges the end a death, and every later report, read as
// outside or not, is a later report of that death (judgedDeath). Five have D1's first report the
// daemon's own replacement of the pod (displaced), and the same pod's end read as outside or
// neither way only after a restart, which lost the runtime's record of that replacement.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
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
	// atReplayAtPlace is the boot's replay carrying a place the boot's pass took in the pacing's
	// first queue (RecordedDeath.Place), as the pass posts one for each charged death of the
	// restart's wave (internal/daemon's launchUnfinished). The pass takes it at the restart,
	// before any event after the restart reaches the claim (walk).
	atReplayAtPlace nextEvent = "the boot's replay at the place its pass took"
	atReportOutside nextEvent = "the report read again as outside"
	atReportNeither nextEvent = "the report read neither way"
	// atReportRefused is a report read neither way whose relaunch D2's lifecycle recheck
	// refuses, as it refused D1's; the store answers the recheck again after it.
	atReportRefused nextEvent = "the report read neither way, its relaunch refused"
	// atReportTakeBackRefused is a report read neither way whose take-back of the task the store
	// refuses; the store answers the take-back again after it.
	atReportTakeBackRefused nextEvent = "the report read neither way, its take-back refused"
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
			name:  "beside the product: the take-back refused, restart, the replay at its place over no decision, then the report read outside",
			stood: stoodInTaskTurn, takeBack: takeBackRefused, restart: true,
			at: []nextEvent{atReplayAtPlace, atReportOutside}, told: endFromOutside,
		},
		{
			name:  "beside the product: the take-back of unknown outcome not landed, restart, the replay at its place over no decision, then the report read outside",
			stood: stoodInTaskTurn, takeBack: takeBackUnknownLost, restart: true,
			at: []nextEvent{atReplayAtPlace, atReportOutside}, told: endFromOutside,
		},
		{
			name:  "beside the product: the first report read neither way, its take-back refused, then the report read as outside, its relaunch refused, then the report read as outside behind another claim's wake",
			stood: stoodInTaskTurn, firstNeither: true, takeBack: takeBackRefused,
			at:    []nextEvent{atReportOutsideRefused, atReportOutsideQueued},
			death: true, told: "the process died",
		},
		{
			name:  "beside the product: the first report read neither way, its relaunch refused, then the report read as outside three times a minute apart, its relaunch refused",
			stood: stoodInTaskTurn, firstNeither: true, takeBack: takeBackTaken, decisionStored: true, refused: true,
			at:    []nextEvent{atReportOutsideRefused, atReportOutsideRefused, atReportOutsideRefused, atReportOutsideQueued},
			death: true, told: "the process died",
		},
		{
			name:  "beside the product: holding nothing, the first report read neither way, restart, the report read as outside, then the replay at the place the boot's pass took",
			stood: stoodHoldingNothing, firstNeither: true, takeBack: takeBackNone, decisionStored: true, restart: true,
			at: []nextEvent{atReportOutside, atReplayAtPlace}, death: true,
		},
		{
			name:  "beside the product: displaced first, its relaunch refused, restart, the report read as outside, its relaunch refused, then the report read as outside behind another claim's wake",
			stood: stoodInTaskTurn, displacedFirst: true, takeBack: takeBackTaken, refused: true, restart: true,
			at: []nextEvent{atReportOutsideRefused, atReportOutsideQueued}, told: endDisplaced,
		},
		{
			name:  "beside the product: displaced first, its take-back refused, restart, the report read neither way, its take-back refused, then the report read neither way",
			stood: stoodInTaskTurn, displacedFirst: true, takeBack: takeBackRefused, restart: true,
			at: []nextEvent{atReportTakeBackRefused, atReportNeither}, told: endDisplaced,
		},
		{
			name:  "beside the product: displaced first, its take-back refused, restart, then the report read neither way",
			stood: stoodInTaskTurn, displacedFirst: true, takeBack: takeBackRefused, restart: true,
			at: []nextEvent{atReportNeither}, told: endDisplaced,
		},
		{
			name:  "beside the product: displaced first, its relaunch refused, restart, then the report read neither way",
			stood: stoodInTaskTurn, displacedFirst: true, takeBack: takeBackTaken, refused: true, restart: true,
			at: []nextEvent{atReportNeither}, told: endDisplaced,
		},
		{
			name:  "beside the product: holding nothing, displaced first, its relaunch refused, restart, then the report read neither way",
			stood: stoodHoldingNothing, displacedFirst: true, takeBack: takeBackNone, refused: true, restart: true,
			at: []nextEvent{atReportNeither},
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
	// firstNeither is whether D1's first report is read neither way, so died judges the end a
	// death: where its take-back lands it decides it there, one death charged where the claim
	// held a task or a turn and the end of the turn recorded (chargeDeath), and where the store
	// refuses the take-back it decides nothing until the next report. Either way the record of
	// the ended launch is a death's where the death cut off work, none where it cut off none
	// (recordEnd). Every other row's first report is read as outside, but displacedFirst's.
	firstNeither bool
	// displacedFirst is whether D1's first report is the daemon's own replacement of the pod
	// (runtime.Observation.Displaced), so displaced judges the end first: it records the end as
	// displaced's (EndedWork.Displaced), whatever it ended, takes the task back and relaunches
	// the claim charged nothing, and decides nothing (Claim.DeathCharged). A later report of the
	// same pod read neither way is that displacement again (recordedDisplaced); one read as
	// outside is continued uncharged by endedFromOutside, which types displaced's record of it as
	// an end from outside the pod (markFromOutside), so the claim reads it decided from outside.
	displacedFirst bool
	// cause is the sentence an earlier daemon wrote as the stored record's cause, over the
	// record D1 typed as an end from outside the pod, and "" for today's endFromOutside.
	cause string
	// restart is whether the next event is a daemon restart; at is the events after the end.
	restart bool
	at      []nextEvent
	// death is whether the end is counted as a stop of the role's process: the launch failure
	// every death is charged, which the relaunch's ready clears, and one death where the claim
	// held a task or a turn (deaths). Every other row is charged nothing.
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
// relaunch's ready, and an end from outside the pod is never told as a death. While the claim
// holds that end decided, its relaunch waits in the pacing's second queue, never in the first
// that a charged death's waits in (outsideEndQueued). Of the product's rows, only the five where
// nothing stored and nothing read says the end came from outside the pod (32, 36, 40, 44 and 48)
// are counted as a stop of the role's process: one death, and the task still sent again. Row 38
// is the take-back refused on one daemon and the next report read neither way; row 20 is the
// same with the record stored by an Uncertain report's write and a restart before the report.
// Both are charged nothing. Beside the product, three rows have a first report read neither way,
// so died judges the end a death at D1: it decides it there, its relaunch refused or parked, or,
// where the store refuses the take-back, records what the death ended and decides nothing. The
// first judgement stands: every report after it, however it is read, goes to died as a later
// report of that death (judge, judgedDeath), which decides a death it left undecided and raises
// again the relaunch of one it decided. So at every step the claim holds that death, no record
// of the launch typed as an end from outside the pod, and a paced relaunch waiting in the first
// queue, at the place the boot's pass took once the replay moves it there, and it logs no line
// of an end continued uncharged (deathStands); the end of the turn stays where the decision put
// it. The role is told that its process died, and the death is charged once: one death where the
// claim held a task or a turn, and one launch failure, charged by the first raise of its
// relaunch that passes the lifecycle recheck (wantCharged). That raise is D1's where the recheck
// passed it; the report behind another claim's wake where the reports before it were refused;
// and, for the claim holding nothing, the report a restart lets reach it before the boot's
// replay, since died stores its decision with no launch failure charged (Claim.PendingCause
// empty) and the relaunch it parks writes nothing before its turn, so the restarted claim holds
// the decision with no charge, and the first raise there charges it.
//
// Five more rows beside the product have D1's first report the daemon's own replacement of the
// pod, its take-back or its relaunch refused, so the claim stands over the ended process holding
// displaced's record of the end, which an Uncertain report's write stores, and no decision.
// After a restart, which lost the runtime's record of that replacement, the same pod's end is
// read as from outside in one: endedFromOutside decides it and types displaced's record as an end
// from outside the pod, keeping its cause and its own type. In the other four it is read neither
// way, one with its take-back refused first, one over the claim holding nothing: judge sends each
// such report to displaced (recordedDisplaced), which relaunches the claim again and decides
// nothing. In all five nothing is charged, no report reaches died, the relaunch waits in the
// second queue, and the role is told that its pod was replaced where the end cut off its work
// (displacementRejudged). At every step of every row, a claim reads a charged death only where
// died judged its process (deathUnjudged).
//
// The negative controls: judge routing on decidedFromOutside in place of the record, so a report
// read neither way over a record with no decision goes to died (rows 16, 18, 20, 22, 24, 26, 28,
// 38, 42 and 46 charge a death); endedFromOutside deciding again at every report, so D2's
// report over a decision D1 stored for a claim in a turn moves the end of the turn (rows 9 and
// 10, and the rows beside the product of a message's turn, of the relaunch refused at both
// daemons and of the earlier daemon's cause with the report first); WakeQueue reading
// Claim.DeathCharged in place of Claim.chargedDeath, so the relaunch of a decided end from
// outside the pod waits in the first queue; judge sending every report read as outside to
// endedFromOutside whatever the claim holds, as it did before judgedDeath, so each of the three
// rows beside the product whose first report is read neither way continues a later report of
// its death as an end from outside the pod and logs that end's line; markFromOutside leaving a
// standing record of the launch untyped, so the first displaced row's claim reads a charged
// death that died never judged once the report read as outside decides it; judge sending a
// report read neither way over displaced's record to died, so each of the four displaced rows
// read neither way logs died's line at that report; and displaced leaving no record of an end
// that ended nothing worth telling (markDisplaced), so D1's store holds no record in the row
// holding nothing.
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
	switch {
	case o.firstNeither:
		end.Kind, end.EndReading = runtime.NotRecordedProcess, ""
	case o.displacedFirst:
		end.EndReading, end.Displaced = "", true
	}
	err := h.handle(RuntimeObservation{Observation: end})
	if want := o.d1Fails(); (err != nil) != want {
		t.Fatalf("the end at D1 answered %v, want an error %v", err, want)
	}
	if !o.takeBack.failed() && !o.refused {
		h.wantWaitingWake(WakeLaunch)
	}
	// raised is whether a raise of the relaunch has passed the lifecycle recheck since the end at
	// D1, or since D2's boot restored the claim (wantCharged).
	raised := !o.takeBack.failed() && !o.refused
	answerEveryWrite(h)
	if o.displacedFirst || o.recordStored && o.takeBack.failed() {
		// The store answers again, and an Uncertain report's write (judge) carries the record
		// the end left in memory into it, with no decision: a take-back that failed wrote none
		// of the claim, and displaced's replacement writes the delivery alone before the recheck
		// refuses its relaunch.
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: ended, Kind: runtime.Uncertain, At: h.clock.Now()}})
	}
	// An end read as outside is charged nothing, and died decides a death at D1 only where its
	// take-back lands: it returns on the take-back's error before the decision.
	if o.firstNeither && !o.takeBack.failed() {
		o.wantCharged(h, "the end at D1", raised)
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
	want("the end at D1", turnEnd.see(h), outsideEndQueued(h), o.deathStands(h, read), deathUnjudged(h, ended))
	h.advance(time.Second)
	sent := len(h.prompts())
	var boot *wakepace.Place
	if o.restart {
		h.restart()
		// The restart's wave places the relaunches of the charged deaths it found in the pacing's
		// first queue, ahead of the backlog in the second, where an end from outside the pod's
		// relaunch waits (internal/daemon's launchUnfinished): another role's place in the first
		// queue stands for that wave, so a relaunch this restart raises, in either queue, waits
		// its turn behind it. A row whose replay carries a place has the pass take it behind
		// that wave's, before any event after the restart reaches the claim.
		firstAhead(h.deps.WakePace.Pacer, 1)
		if slices.Contains(o.at, atReplayAtPlace) {
			boot = h.deps.WakePace.Pacer.Join(wakepace.First)
		}
		turnEnd.restarted(h)
		raised = false
		want("the restart", turnEnd.see(h), o.deathStands(h, read), deathUnjudged(h, ended))
	}
	for _, ev := range o.at {
		o.reach(h, ev, ended, read, boot)
		raised = raised || !slices.Contains([]nextEvent{atReportRefused, atReportOutsideRefused, atReportTakeBackRefused}, ev)
		want(string(ev), notHeld(h, task), outsideEndQueued(h), o.deathStands(h, read), deathUnjudged(h, ended))
		o.wantCharged(h, string(ev), raised)
		want(string(ev), turnEnd.see(h))
	}
	o.wantRecord(h, read, "the events after the end")

	charged := h.budgets()
	h.advance(time.Second)
	o.reach(h, atReportNeither, ended, read, nil)
	if got := h.budgets(); got != charged {
		t.Fatalf("a repeated report left budgets %+v, want %+v: it charges nothing new", got, charged)
	}
	// displaced decides nothing, so a row it judged first holds a decision only where a report
	// read as outside decided the end.
	if decides := !o.displacedFirst || o.readsOutside(); turnEnd.claim.decided != decides {
		t.Fatalf("a repeated report: the claim reads deathCharged=%v, want the end decided %v", h.claim().DeathCharged, decides)
	}
	want("a repeated report", turnEnd.see(h), notHeld(h, task), outsideEndQueued(h), o.deathStands(h, read), deathUnjudged(h, ended))

	// A relaunch charged no death waits in the second queue, where at D1 the wake queueAhead
	// parked waits ahead of it, and that wake's turn's jitter puts the relaunch's turn past the
	// moment its view names: the clock moves on to that turn as the pacer gives it (advanceToTurn).
	h.wantWaitingWake(WakeLaunch)
	h.advanceToTurn(h.m)
	resume := h.wantCalls("Resume", 1)[0]
	if resume.Previous == nil || *resume.Previous != ended || resume.Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("resumed %+v from %+v, want the claim's own recorded session after the ended incarnation",
			resume.Spec, resume.Previous)
	}
	o.wantCharged(h, "the relaunch", raised)
	want("the relaunch", notHeld(h, task), turnEnd.see(h))
	o.wantRecord(h, read, "the relaunch")

	h.relaunched()
	if o.death {
		h.wantBudgets(Budgets{Deaths: o.deaths()})
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
// decision (displaced makes none), and the relaunch refused by the lifecycle recheck.
func (o outsideOrder) breakD1(h *harness) {
	switch o.takeBack {
	case takeBackRefused:
		h.store.fail("PutDelivery", errStoreRefused)
	case takeBackUnknownLanded, takeBackUnknownLost:
		h.m.deps.Store = &unknownOutcomeStore{memStore: h.store, landed: o.takeBack == takeBackUnknownLanded}
	}
	switch {
	case o.decisionStored || o.takeBack.failed() || o.displacedFirst:
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
// record of the ended launch typed as an end from outside the pod, or displaced's record of it
// typed as displaced's where displaced judged the end first, the decision, and the task still
// confirmed where the take-back did not land, taken back where it did.
func (o outsideOrder) wantStoredAtD1(h *harness, task string, read uint64) {
	h.t.Helper()
	stored := h.store.load(h.token)
	recorded := stored.EndedWork.FromOutside && stored.EndedWork.Generation == read
	if recorded != o.recordStored || stored.DeathCharged != o.decisionStored {
		h.t.Fatalf("D1's store holds the record %+v and deathCharged=%v, want the record of the end stored %v and the decision stored %v",
			stored.EndedWork, stored.DeathCharged, o.recordStored, o.decisionStored)
	}
	if got := stored.EndedWork; o.displacedFirst && (got.Cause != endDisplaced || !got.Displaced || got.Generation != read) {
		h.t.Fatalf("D1's store holds the record %+v, want displaced's record of the ended launch %d", got, read)
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
// place boot, the place the boot's pass took at the restart, too, as that pass posts it
// (internal/daemon's launchUnfinished).
func (o outsideOrder) reach(h *harness, ev nextEvent, ended runtime.Locator, read uint64, boot *wakepace.Place) {
	h.t.Helper()
	if ev == atReportOutsideRefused {
		h.advance(time.Minute)
	}
	outside := runtime.Observation{Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs}
	neither := runtime.Observation{Locator: ended, Kind: runtime.NotRecordedProcess, At: h.clock.Now()}
	switch ev {
	case atReplay:
		h.must(RecordedDeath{Claim: h.token, Generation: read})
	case atReplayAtPlace:
		h.must(RecordedDeath{Claim: h.token, Generation: read, Place: boot})
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
	case atReportTakeBackRefused:
		h.store.fail("PutDelivery", errStoreRefused)
		if err := h.handle(RuntimeObservation{Observation: neither}); err == nil {
			h.t.Fatal("the take-back the report made was written, want the store's refusal of it")
		}
		h.store.fail("PutDelivery", nil)
	}
}

// readsOutside is whether a report after the end is read as from outside the pod.
func (o outsideOrder) readsOutside() bool {
	return slices.ContainsFunc(o.at, func(ev nextEvent) bool {
		return ev == atReportOutside || ev == atReportOutsideRefused || ev == atReportOutsideQueued
	})
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

// outsideEndQueued answers how the claim's paced wake waits at a place in the pacing's first
// queue while the end the claim holds decided is one from outside the pod, and "" where it does
// not, both read off the view the machine last published (publish): such an end charges no
// death, so its relaunch waits in the second queue, as a displaced relaunch does, and is never
// placed as a charged death's (WakeQueue, dispatch://INFRA-5682 R2). Both walks check it.
func outsideEndQueued(h *harness) string {
	v := h.m.view.Load()
	if v.place == nil || !v.Claim.decidedFromOutside() || v.place.Queue() == wakepace.Second {
		return ""
	}
	return "the claim holds its end decided from outside the pod and its paced wake waits in the pacing's first queue, want the second: such an end charges no death"
}

// deathRetyped answers how the claim no longer holds the death died judged first over the launch
// generation names as that death, and "" where it does or where the claim records another
// launch. The first judgement of a process stands: a later report read as outside goes to died
// as a later report of that death (judge, judgedDeath), which changes neither the death nor its
// type nor its wake's queue. So the claim holds that death (judgedDeath), decided or recorded
// with its take-back refused, no record of that launch typed as an end from outside the pod, and
// its paced wake, where it holds one, waits in the pacing's first queue (WakeQueue), all read off
// the view the machine last published (publish). Both walks check it.
func deathRetyped(h *harness, generation uint64) string {
	v := h.m.view.Load()
	switch c := v.Claim; {
	case c.Generation != generation || c.Locator == nil:
	case !c.judgedDeath() || c.recordedFromOutside():
		return fmt.Sprintf("the claim reads deathCharged=%v over the record %+v, want the death died judged first to stay that death, its end never typed as one from outside the pod",
			c.DeathCharged, c.EndedWork)
	case v.place != nil && v.place.Queue() != wakepace.First:
		return "the claim holds the death died judged first and its paced wake waits in the pacing's second queue, want the first, where a charged death's waits"
	}
	return ""
}

// deathStands is deathRetyped for a row whose first report is read neither way (firstNeither),
// whose end died judges first, displacementRejudged for a row displaced judged first, and "" for
// every other row. No report after died's judgement is continued as an end from outside the pod,
// so the claim logs no line of one at any step.
func (o outsideOrder) deathStands(h *harness, read uint64) string {
	switch {
	case o.displacedFirst:
		return displacementRejudged(h, read, 0)
	case !o.firstNeither:
		return ""
	}
	if n := continuedLines(h); n != 0 {
		return fmt.Sprintf("the claim logged %d lines of an end from outside its pod continued charged nothing, want none over the death died judged first", n)
	}
	return deathRetyped(h, read)
}

// deathUnjudged answers how the claim reads a charged death (chargedDeath) of the ended process
// that died never judged, its line for the claim unlogged, and "" where it does not. Every other
// decision of the end is endedFromOutside's, which the claim reads as decided from outside the
// pod (decidedFromOutside): its relaunch waits in the second queue, and the boot's replay
// relaunches it charged nothing. So does an end displaced judged first and a later report read as
// outside: endedFromOutside types displaced's record of it as one from outside the pod
// (markFromOutside). Both walks check it.
func deathUnjudged(h *harness, ended runtime.Locator) string {
	c := h.claim()
	if !c.chargedDeath() || c.Locator == nil || *c.Locator != ended {
		return ""
	}
	if len(h.logs.lines(diedLine, "claim="+string(h.token))) != 0 {
		return ""
	}
	return fmt.Sprintf("the claim reads a charged death over the record %+v and died never judged its process, want an end decided from outside the pod read as one",
		c.EndedWork)
}

// diedLine is the line died writes for every report it judges (died).
const diedLine = "supervise: process died"

// continuedLines is how many lines the claim has logged of an end from outside its pod continued
// charged nothing (continuedFromOutsideLine). Both walks read it.
func continuedLines(h *harness) int {
	return len(h.logs.lines(continuedFromOutsideLine, "claim="+string(h.token)))
}

// answerEveryWrite makes the machine's store answer every write again: no write refused, and the
// harness's own store in place of one that wraps it, a restarted machine's included.
func answerEveryWrite(h *harness) {
	h.store.fail("PutDelivery", nil)
	h.store.fail("PutClaim", nil)
	h.m.deps.Store = h.store
}

// wantCharged fails unless the claim is charged what the row says: nothing, or the row's deaths
// with the launch failure every death is charged until the relaunch's ready. raised is whether a
// raise of the death's relaunch has passed the lifecycle recheck since the end at D1, or since
// D2's boot restored the claim. The first such raise charges the launch failure
// (relaunchAfterFailure), and a raise after it charges none (deathLaunchFailureCharged); a raise
// the recheck refuses charges none, so a death under refused raises alone is charged none yet.
// died stores its decision with no launch failure charged (Claim.PendingCause empty), and the
// relaunch it parks writes nothing before its turn, so a restart before that turn restores the
// decision with no charge, and the first raise at D2 charges it: a report of the death, however
// it is read, or the boot's replay.
func (o outsideOrder) wantCharged(h *harness, when string, raised bool) {
	h.t.Helper()
	if !o.death {
		h.wantNothingCharged(when)
		return
	}
	want := Budgets{Deaths: o.deaths()}
	if raised {
		want.LaunchFailures = 1
	}
	if got := h.budgets(); got != want {
		h.t.Fatalf("%s: budgets %+v, want %+v: the row's deaths, and the launch failure where the death's relaunch charged it",
			when, got, want)
	}
}

// deaths is how many deaths the row's end is charged: one for a death of a claim that held a task
// or a turn, and none for every other end, the death of a claim that held neither included, which
// is a parked agent the environment killed (chargeDeath).
func (o outsideOrder) deaths() int {
	if !o.death || o.stood == stoodHoldingNothing {
		return 0
	}
	return 1
}

// wantRecord fails unless the claim's record of the end is of the ended launch and says what the
// row says: typed as an end from outside the pod, under the cause the row's record carries, or
// displaced's, typed so, where displaced judged the end first, and from outside the pod too only
// where a report read so came; telling only where the end cut off work; or, for a death, a death
// died judged (EndedWork.Died), and nothing that tells where the death cut off no work.
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
		if got.FromOutside || !got.Died || !strings.HasPrefix(got.Cause, o.told) {
			h.t.Fatalf("%s: the claim records %+v, want a death's record", when, got)
		}
		return
	}
	cause, outside := endFromOutside, !o.displacedFirst || o.readsOutside()
	switch {
	case o.cause != "":
		cause = o.cause
	case o.displacedFirst:
		cause = endDisplaced
	}
	if got.FromOutside != outside || got.Displaced != o.displacedFirst || got.Cause != cause || got.Tells() != (o.told != "") {
		h.t.Fatalf("%s: the claim records %+v, want a record under %q, from outside the pod %v and displaced's %v, that tells %v",
			when, got, cause, outside, o.displacedFirst, o.told != "")
	}
}

// wantTold fails unless the relaunched role was sent, at its ready, what the row says and
// nothing else: the task it held, once, behind the sentence of the end where the end cut off
// work; the daemon's own relaunch record where it held no task and the end cut off its turn; and
// nothing where it held nothing and the end cut off nothing. An end from outside the pod is never
// told as a death, nor a death as an end from outside the pod.
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
	case o.death && strings.Contains(told, endFromOutside):
		h.t.Fatalf("the relaunched role was sent %q, want a death never told as an end from outside the pod", told)
	}
}
