package supervise

// A turn-outcome report whose writes the store refused (decision 8 of dispatch://INFRA-2811). The
// report is applied to the claim's window before its finish or its take-back is written, so the
// worker's report sent again is a repeat, and what the report does to the task is done by the
// decision that applied it, or owed from it (Machine.finishOwed, Machine.ladderOwed), in the same
// daemon and across a restart.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A report's finish the store refused is made again by the end-frame bound, armed wherever the
// report arrived, and armed again after every write the store refuses, until one lands
// (decision 8 of dispatch://INFRA-2811): across a store outage of several bounds the
// task is finished once the store answers, once, by the report. The report can come after its
// turn's end frame, ahead of it — while the claim is still working, where the bound is armed at
// the turn's end — during a compaction with no turn under it, begun after that end, where the
// bound is armed at the compaction's end (compactEnded), or, for a plugin that names no number,
// after the bound already fired and its own write was refused.
//
// The negative controls are the bound armed once and not again after a refused write (finishTask
// with no re-arm): the finish is then lost with the first refusal, and the task stands unfinished
// after the store answers; and the compaction's end arming no bound (compactEnded before this
// change): the finish refused under the compaction is owed with no bound to make it again.
func TestTurnOutcomeFinishTheStoreRefusedIsMadeAgainAcrossAnOutage(t *testing.T) {
	outage := func(t *testing.T, h *harness, number uint64) {
		t.Helper()
		for bound := 1; bound <= 3; bound++ {
			h.advance(testRPC)
			if p := h.claim().Pending; p == nil || p.Number != number {
				t.Fatalf("bound %d of the outage left %+v, want task %d still pending", bound, p, number)
			}
		}
		h.store.fail("RetireDelivery", nil)
		h.advance(testRPC)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v a bound after the store answered, want task %d finished", p, number)
		}
		h.wantFinishedOnce(finishedByReport)
	}
	t.Run("a report after its turn's end", func(t *testing.T) {
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		h.reach(StateReady)
		h.takesTurnEndsWithItsReportToCome()
		number := h.pending().Number
		h.store.fail("RetireDelivery", errBoom)
		if err := h.reportTurn(number, nil); err == nil {
			t.Fatal("the report whose finish the store refused answered no failure")
		}
		outage(t, h, number)
	})
	t.Run("a report ahead of its turn's end frame", func(t *testing.T) {
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		h.reach(StateReady)
		h.taskTheAgentTook()
		number := h.pending().Number
		h.must(StreamTurnStart{Claim: h.token})
		h.store.fail("RetireDelivery", errBoom)
		if err := h.reportTurn(number, nil); err == nil {
			t.Fatal("the report whose finish the store refused answered no failure")
		}
		h.advance(3 * testRPC)
		// The claim is still working, so no bound is armed until the turn's end frame, and the
		// three bounds that pass make nothing again.
		if _, armed := h.m.timers[TimerFinish]; armed {
			t.Fatal("the end-frame bound is armed while the claim is still working, want it armed at the turn's end")
		}
		if p := h.claim().Pending; p == nil || p.Number != number {
			t.Fatalf("three bounds into the turn the claim holds %+v, want task %d still pending", p, number)
		}
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		outage(t, h, number)
	})
	t.Run("a report during a compaction with no turn under it", func(t *testing.T) {
		h, number, _ := reportToComeOfACurrentPlugin(t)
		release, compacted := h.compactHeld()
		h.store.fail("RetireDelivery", errBoom)
		if err := h.reportTurn(number, nil); err == nil {
			t.Fatal("the report whose finish the store refused answered no failure")
		}
		// The claim is working for the compaction, so no bound is armed at the refusal: the
		// compaction's end arms it, as a turn's end does.
		if h.armed(TimerFinish) {
			t.Fatal("the end-frame bound is armed while the compaction runs, want it armed at the compaction's end")
		}
		release()
		if err := <-compacted; err != nil {
			t.Fatalf("the compaction answered %v, want the claim given back", err)
		}
		h.m.Wait()
		outage(t, h, number)
	})
	t.Run("a report after the bound fired", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.takesTurnEndsWithItsReportToCome()
		number := h.pending().Number
		h.store.fail("RetireDelivery", errBoom)
		h.advance(testRPC)
		if err := h.reportTurn(number, nil); err == nil {
			t.Fatal("the report whose finish the store refused answered no failure")
		}
		outage(t, h, number)
	})
}

// A suspension held over a compaction under which the store refused a report's finish, whose stop
// the runtime refuses at the compaction's end, leaves the end-frame bound armed over that finish,
// as the end of a turn leaves it after a refused stop (runHeldAtEnd): the claim is given back idle
// with the finish owed, and the bound makes the finish once the store answers, by the report, ahead
// of the held stop's retry at the probe interval; the held stop then runs (settleThenAfterTurn).
//
// The negative control is a compactEnded that arms no bound after a refused stop: the finish
// refused under the compaction stays owed with no bound to make it again, and the task stands
// pending a bound after the compaction's end.
func TestARefusedHeldStopAtACompactionsEndLeavesTheOwedFinishBounded(t *testing.T) {
	h, number, _ := reportToComeOfACurrentPlugin(t)
	release, compacted := h.compactHeld()
	h.store.fail("RetireDelivery", errBoom)
	if err := h.reportTurn(number, nil); err == nil {
		t.Fatal("the report whose finish the store refused answered no failure")
	}
	if err := h.handle(daemonSuspension(h)); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the daemon's suspension during the compaction answered %v, want it held for the compaction's end", err)
	}
	h.rt.FailSuspend(errBoom)
	release()
	if err := <-compacted; !errors.Is(err, errBoom) {
		t.Fatalf("the compaction whose held stop the runtime refused answered %v, want that refusal", err)
	}
	h.m.Wait()
	h.wantState(StateIdle)
	if !h.armed(TimerFinish) {
		t.Fatal("no end-frame bound is armed after the refused stop, want one over the finish the store refused")
	}
	h.store.fail("RetireDelivery", nil)
	h.rt.FailSuspend(nil)

	h.advance(testRPC)

	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v a bound after the compaction's end, want task %d finished by its report", p, number)
	}
	h.wantFinishedOnce(finishedByReport)
	h.wantState(StateSuspended)
}

// A report's finish the store refused is owed still after a daemon restart (decision 8 of
// dispatch://INFRA-2811). The report is applied to the claim's window before its finish is
// written, so the worker's report sent again is a repeat that finishes nothing, and the daemon
// that saw the refusal owed the finish in memory alone. The restarted daemon reads it from the
// window — the newest applied report of the claim's generation that names the pending task's
// number, over a take that stands accepted (Claim.owedReport) — and the end-frame bound it arms
// at its start makes it, with no act of the worker's awaited: the task is finished once, by the
// report. A report that did nothing to the task leaves nothing owed, live or after a restart:
// one of an earlier generation, which the process the claim relaunched from sent after its
// death took the take of that number back and after the next process took it again, finishes
// nothing in the daemon it reaches (Claim.reportOfTake); and one that carried a provider error,
// whose take-back and ladder step the store took, is not of the take that stands now, the next
// one. The report of the take's own turn is what finishes the task then.
//
// The negative controls: the restored machine without the derivation (NewMachine with no
// owedReport) leaves the task whose finish the store refused pending, its turn over, with nothing
// to finish it; a report of any generation reaching the take (reportOfTake with no generation)
// finishes the next process's take by the earlier one's report; and a provider error's report
// owed whatever its ladder step (owedReport with no step) takes back the take after the ladder.
func TestTurnOutcomeFinishTheStoreRefusedIsOwedAgainAfterADaemonRestart(t *testing.T) {
	// owesNothing restarts the daemon over a claim idle after the turn of task number's take,
	// with that turn's report to come, and holds that no bound finishes the task before the
	// report does.
	owesNothing := func(t *testing.T, h *harness, number uint64) {
		t.Helper()
		if p := h.pending(); p.Number != number || !p.startRecorded() || h.state() != StateIdle {
			t.Fatalf("the claim is %s holding %+v, want it idle over task %d's turn, its report to come", h.state(), p, number)
		}
		h.restart()
		h.connect()
		h.advance(3 * testRPC)
		if p := h.claim().Pending; p == nil || p.Number != number || !p.startRecorded() {
			t.Fatalf("three bounds after the restart the claim holds %+v, want task %d's turn over and its report to come", p, number)
		}
		if finished := h.records.of(finishedLine); len(finished) != 0 {
			t.Fatalf("%q was logged %d times before the turn's report, want none", finishedLine, len(finished))
		}
		h.mustReport(nil)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the turn's report, want task %d finished", p, number)
		}
	}
	t.Run("a report naming its number, its finish refused", func(t *testing.T) {
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		h.reach(StateReady)
		h.takesTurnEndsWithItsReportToCome()
		number := h.pending().Number
		h.store.fail("RetireDelivery", errBoom)
		if err := h.reportTurn(number, nil); !errors.Is(err, errBoom) {
			t.Fatalf("the report whose finish the store refused = %v, want %v", err, errBoom)
		}
		kept := h.store.load(h.token).TurnOutcomes
		if len(kept) != 1 || kept[0].Unapplied || kept[0].Task != number || kept[0].ProviderError {
			t.Fatalf("the store keeps %+v, want the report applied, naming task %d and no provider error", kept, number)
		}
		h.store.fail("RetireDelivery", nil)

		h.restart()
		h.advance(testRPC - time.Second)
		if p := h.claim().Pending; p == nil || p.Number != number || !p.startRecorded() {
			t.Fatalf("less than a bound after the restart the claim holds %+v, want task %d's turn over and its finish to come", p, number)
		}
		h.advance(time.Second)

		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v a bound after the restart, want task %d finished", p, number)
		}
		h.advance(3 * testRPC)
		h.wantFinishedOnce(finishedByReport)
	})
	t.Run("a report of an earlier generation", func(t *testing.T) {
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		h.reach(StateReady)
		h.takesTurnEndsWithItsReportToCome()
		number, left := h.pending().Number, h.generation()
		h.observe(runtime.Gone)
		h.relaunched()
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		// The report the dead process sent as it ended its turn reaches the daemon after its death
		// took the take back, and after the relaunched process took the task again and ran it.
		h.worker.sequence++
		h.must(RequestTurnOutcome{Claim: h.token, Generation: left, Sequence: h.worker.sequence, Task: number})
		if p := h.claim().Pending; p == nil || p.Number != number || !p.startRecorded() {
			t.Fatalf("after generation %d's report the claim holds %+v, want generation %d's take of task %d standing, its turn over",
				left, p, h.generation(), number)
		}
		kept := h.store.load(h.token).TurnOutcomes
		if h.generation() == left || len(kept) != 1 || kept[0].Generation != left || kept[0].Unapplied || kept[0].Task != number {
			t.Fatalf("the claim is at generation %d keeping %+v, want a later one keeping generation %d's report of task %d applied",
				h.generation(), kept, left, number)
		}
		owesNothing(t, h, number)
	})
	t.Run("a report that carried a provider error", func(t *testing.T) {
		h := workingClaim(t, currentPlugin)
		number := h.pending().Number
		h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})
		h.climbs(1, promptRetryDelay(1))
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		kept := h.store.load(h.token).TurnOutcomes
		if len(kept) != 1 || kept[0].Unapplied || kept[0].Task != number || !kept[0].ProviderError ||
			kept[0].ProviderErrorStatus != 529 || kept[0].ProviderErrorRetries != 1 {
			t.Fatalf("the store keeps %+v, want the report applied, naming task %d, its provider error and the ladder's first step", kept, number)
		}
		owesNothing(t, h, number)
	})
}

// A report naming the accepted take's number that comes ahead of the take's turn start, while the
// store refuses both the start's write and the finish's, leaves the finish owed (decision 8 of
// dispatch://INFRA-2811): the report is applied, so the worker's report sent again is a repeat,
// and nothing else would finish the task. In the daemon that received it the start stands in
// memory, the decision goes on to the finish, and the finish's refusal arms the end-frame bound;
// in a daemon restarted over what the store holds — the report applied, the take still awaiting
// its turn — the take's turn is started from the report and the finish is owed again. Either way
// the bound finishes the task once, by the report, with the serving credit a finished take's
// turn earns (retireTask): its phase entry served, not handed back as owed.
//
// The negative controls: the decision stopped at the start's refused write (reportedTask
// returning it) leaves no finish owed in the same daemon; a finish owed after a restart only over
// a take whose turn started (owedReport requiring it) leaves none; and the restored take's turn
// left unstarted (NewMachine without the start) finishes the task with its entry handed back.
func TestTurnOutcomeAheadOfItsTurnStartIsFinishedWhenTheStoreRefusedItsStartAndFinish(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "in the same daemon", true: "across a daemon restart"}[restart], func(t *testing.T) {
			h := newHarnessOf(t, queuedClaim(), currentPlugin)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Phase: phase.Implementing, Generation: 7, Entry: 1})
			number := h.pending().Number
			if p := h.pending(); !p.awaitsStart() {
				t.Fatalf("the task is %+v, want its take accepted and its turn to come", p)
			}
			h.store.fail("PutDelivery", errBoom)
			h.store.fail("RetireDelivery", errBoom)
			if err := h.reportTurn(number, nil); !errors.Is(err, errBoom) {
				t.Fatalf("the report whose start and finish the store refused = %v, want %v", err, errBoom)
			}
			h.store.fail("PutDelivery", nil)
			h.store.fail("RetireDelivery", nil)
			if stored, ok := h.store.delivery(h.token); !ok || !stored.awaitsStart() {
				t.Fatalf("the store holds %+v (held %v), want the take still awaiting its turn", stored, ok)
			}
			if restart {
				h.restart()
			}

			h.advance(testRPC - time.Second)
			if p := h.claim().Pending; p == nil || p.Number != number {
				t.Fatalf("less than a bound on the claim holds %+v, want task %d's finish to come", p, number)
			}
			h.advance(time.Second)

			c := h.claim()
			if c.Pending != nil || c.TaskFinished != number {
				t.Fatalf("a bound on the claim holds %+v and finished task %d, want task %d finished", c.Pending, c.TaskFinished, number)
			}
			if c.ServedEntry != 1 || c.GivenEntry != 1 {
				t.Fatalf("the finished claim served entry %d and was given entry %d, want entry 1 served", c.ServedEntry, c.GivenEntry)
			}
			h.wantFinishedOnce(finishedByReport)
		})
	}
}

// cutOffStore is the store going away part way through a decision: it takes the next takes
// writes of the claim and its delivery, whatever their kind, and refuses each one after them with
// errBoom. A negative takes is a store that takes every write.
type cutOffStore struct {
	Store
	mu    sync.Mutex
	takes int
}

func (s *cutOffStore) set(takes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.takes = takes
}

func (s *cutOffStore) admit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.takes < 0:
		return nil
	case s.takes == 0:
		return errBoom
	}
	s.takes--
	return nil
}

func (s *cutOffStore) PutClaim(ctx context.Context, c Claim) error {
	if err := s.admit(); err != nil {
		return err
	}
	return s.Store.PutClaim(ctx, c)
}

func (s *cutOffStore) PutDelivery(ctx context.Context, token claim.Token, d Delivery) error {
	if err := s.admit(); err != nil {
		return err
	}
	return s.Store.PutDelivery(ctx, token, d)
}

func (s *cutOffStore) PutClaimAndDelivery(ctx context.Context, c Claim, d Delivery) error {
	if err := s.admit(); err != nil {
		return err
	}
	return s.Store.PutClaimAndDelivery(ctx, c, d)
}

func (s *cutOffStore) RetireDelivery(ctx context.Context, c Claim, deliveryID string) error {
	if err := s.admit(); err != nil {
		return err
	}
	return s.Store.RetireDelivery(ctx, c, deliveryID)
}

// providerErrorAsTheStoreGoes is a claim whose take's turn ended in a provider error of status,
// reported as the store went away: from the report on, the store takes takes writes — the
// report's own the first of them — and refuses each one after them, and it answers again once
// the report is decided. ahead is a report that comes ahead of its turn's start frame, so the
// report starts that turn itself (startedUnseen); otherwise the report follows the turn's end.
// It answers the task's number and the report's error.
func providerErrorAsTheStoreGoes(t *testing.T, status, takes int, ahead bool) (*harness, uint64, error) {
	t.Helper()
	cut := &cutOffStore{takes: -1}
	through := func(d *Deps) { cut.Store, d.Store = d.Store, cut }
	var h *harness
	if ahead {
		h = newHarnessOf(t, queuedClaim(), currentPlugin, through)
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: providerErrorTaskText, Phase: phase.Implementing, Generation: 7, Entry: 1})
	} else {
		h = workingClaim(t, currentPlugin, through)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
	}
	number := h.pending().Number
	cut.set(takes)
	err := h.reportTurn(number, &ProviderError{Message: "the provider answered an error", Status: status})
	cut.set(-1)
	return h, number, err
}

// refusedTakeBack is providerErrorAsTheStoreGoes with a store that takes the report's own write
// and refuses every write after it, so the report is applied and the take-back, the ladder's step
// and any failure of the claim are written nowhere. Across a restart, the daemon restarts over
// what the store holds and the worker's shim says hello: the claim's first decision.
func refusedTakeBack(t *testing.T, status int, restart, ahead bool) *harness {
	t.Helper()
	h, number, err := providerErrorAsTheStoreGoes(t, status, 1, ahead)
	if !errors.Is(err, errBoom) {
		t.Fatalf("the report whose take-back the store refused = %v, want %v", err, errBoom)
	}
	stored := h.store.load(h.token)
	if p := stored.Pending; p == nil || !p.taken() || p.startRecorded() == ahead || p.ProviderErrorRetries != 0 {
		t.Fatalf("the store holds %+v, want its take standing, its turn started %v, with no ladder step", p, !ahead)
	}
	if kept := stored.TurnOutcomes; len(kept) != 1 || kept[0].Unapplied || kept[0].Task != number {
		t.Fatalf("the store keeps %+v, want the report of task %d applied", kept, number)
	}
	if restart {
		h.restart()
		h.connect()
	}
	return h
}

// wantLadderOwedLine holds that a daemon restarted over a provider error's report whose take-back
// the store refused logged that take-back's making once (ladderOwedLine), and one that did not
// restart logged none.
func (h *harness) wantLadderOwedLine(restart bool) {
	h.t.Helper()
	want := map[bool]int{false: 0, true: 1}[restart]
	if lines := h.logs.lines(ladderOwedLine); len(lines) != want {
		h.t.Fatalf("logged %q, want the owed take-back's line %d times", lines, want)
	}
}

// A provider error's report whose take-back the store refused still takes the task to the ladder
// (dispatch://INFRA-956 R2, decision 8 of dispatch://INFRA-2811). The report is applied, so the
// worker's report sent again is a repeat, and what it does to the task is done by the decision
// that applied it or by nothing. In the daemon that received it the take-back stands in memory,
// and the ladder's step and its wait follow it, or, for a status that holds the issue, the
// claim's failure; the next decision writes them (settle, repair). In a daemon restarted over what
// the store holds — the report applied, the take still accepted, its ladder step not recorded —
// the claim's first decision makes the take-back and the step from the report
// (Machine.ladderOwed). Either way the task is counted one provider error and one step, and is
// given again once its wait is over; a status that holds the issue fails the claim. A report that
// came ahead of its turn's start frame started that turn first (startedUnseen), so the take-back
// keeps the handshake's confirmation of the take's entry, as any provider error's does: the turn
// started on the take's text (giveBackTake).
//
// The take-back, the ladder's step and the claim's run are one write (takeToLadder), so they land
// together or not at all: a store that goes away right after that write leaves a restarted
// daemon a task taken back with its step, owing nothing, whose next take carries the provider
// error's note; one that refuses it leaves the take standing with no step, which the restart owes
// the ladder (above).
//
// The negative controls: the ladder stopped at the take-back's refused write (takeToLadder
// returning it) leaves the task in the same daemon with no step and no wait, and the claim a 400
// ended running; the restored machine with no ladder owed (NewMachine without ladderOwed) leaves
// the take standing, its turn over, with nothing to take it back; a restored take awaiting its
// turn taken back with that turn unstarted gives back the entry's confirmation; and the take-back
// written apart from the step (takeBack's own write, then providerErrorClears'), with the second
// write refused, leaves the store a take-back with no step, which a restart gives again with no
// note.
func TestTurnOutcomeOfAProviderErrorWhoseTakeBackTheStoreRefusedTakesTheTaskToTheLadder(t *testing.T) {
	clears := func(t *testing.T, h *harness, restart bool) {
		t.Helper()
		p := h.pending()
		if p.taken() || p.ProviderErrorRetries != 1 || p.ProviderErrorNote == "" || !h.m.waitsForProviderError(p.ID) {
			t.Fatalf("the task is %+v (waiting %v), want it taken back, one ladder step counted and its wait armed",
				p, h.m.waitsForProviderError(p.ID))
		}
		if run := h.claim().ProviderErrorRun; run != 1 {
			t.Fatalf("the run of provider errors is %d, want 1", run)
		}
		h.advance(promptRetryDelay(1) - time.Second)
		h.wantPrompts(1)
		h.advance(time.Second)
		h.wantPrompts(2)
		if stored, ok := h.store.delivery(h.token); !ok || stored.ProviderErrorRetries != 1 || stored.ProviderErrorNote == "" {
			t.Fatalf("the store holds %+v (held %v), want the ladder's step written", stored, ok)
		}
		h.wantLadderOwedLine(restart)
	}
	for _, restart := range []bool{false, true} {
		name := map[bool]string{false: "in the same daemon", true: "across a daemon restart"}[restart]
		t.Run(name+", a status that clears", func(t *testing.T) {
			clears(t, refusedTakeBack(t, 529, restart, false), restart)
		})
		t.Run(name+", a report ahead of its turn's start", func(t *testing.T) {
			h := refusedTakeBack(t, 529, restart, true)
			if confirmed := h.claim().Handshake.ConfirmedAt; confirmed.IsZero() {
				t.Fatalf("the claim's handshake is %+v, want its confirmation of the take's entry kept", h.claim().Handshake)
			}
			clears(t, h, restart)
		})
		t.Run(name+", a status that holds the issue", func(t *testing.T) {
			h := refusedTakeBack(t, 400, restart, false)
			h.wantState(StateFailed)
			if failure := h.claim().Failure; !strings.Contains(failure, "HTTP 400") {
				t.Fatalf("failure %q, want the provider error's status named", failure)
			}
			h.wantLadderOwedLine(restart)
		})
	}
	t.Run("across a daemon restart, the take-back and the ladder's step written together", func(t *testing.T) {
		h, _, _ := providerErrorAsTheStoreGoes(t, 529, 2, false)
		stored := h.store.load(h.token)
		if p := stored.Pending; p == nil || p.taken() || p.ProviderErrorRetries != 1 || p.ProviderErrorNote == "" || stored.ProviderErrorRun != 1 {
			t.Fatalf("the store holds a run of %d and %+v, want the take-back, the ladder's step and the run written together",
				stored.ProviderErrorRun, p)
		}
		h.restart()
		h.connect()
		if taken := h.wantPrompts(2)[1]; !strings.Contains(taken.Message, classifyProviderError(529, "").cause) {
			t.Fatalf("the take after the restart was answered %q, want the provider error's note in front of the task", taken.Message)
		}
		if p := h.pending(); p.ProviderErrorRetries != 1 || h.claim().ProviderErrorRun != 1 {
			t.Fatalf("after the restart the task is %+v with a run of %d, want one step and a run of one", p, h.claim().ProviderErrorRun)
		}
		h.wantLadderOwedLine(false)
	})
}
