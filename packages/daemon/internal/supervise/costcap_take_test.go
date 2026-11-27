package supervise

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// These are the words Oh My Pi gives the plugin and the cause main gives the worker.
const (
	costCapRefusal = "Cost cap reached: this session spent $5.01 of its $5.00 cap (--max-cost / session.maxCost), so the next model request was not sent."
	costCapCause   = "the session's cost cap (Oh My Pi's session.maxCost, cost_cap)"
)

func (h *harness) reportCostCap(number uint64) (TurnOutcomeAnswer, error) {
	h.worker.sequence++
	answer, err := h.m.HandleTurnOutcome(h.ctx, RequestTurnOutcome{
		Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence, Task: number,
		Refused: true, Message: costCapRefusal, ProviderError: &ProviderError{Message: costCapRefusal}, Code: "cost_cap",
	})
	h.m.Wait()
	h.workerAsks()
	return answer, err
}

func (h *harness) mustReportCostCap(number uint64) {
	h.t.Helper()
	if answer, err := h.reportCostCap(number); err != nil || answer.Failed {
		h.t.Fatalf("cost_cap report answered %+v, %v; want no error and failed=false", answer, err)
	}
}

// retakeAfterCostCap checks that one ladder step gives the same task again after its wait.
func (h *harness) retakeAfterCostCap(number uint64, step int) string {
	h.t.Helper()
	p := h.pending()
	if p.Number != number || p.taken() || p.ProviderErrorRetries != step || !h.m.waitsForProviderError(p.ID) {
		h.t.Fatalf("pending is %+v (waiting %v), want task %d taken back onto ladder step %d",
			p, h.m.waitsForProviderError(p.ID), number, step)
	}
	if !strings.Contains(p.ProviderErrorNote, costCapCause) && step == 1 {
		h.t.Errorf("pending note is %q, want it to name %q", p.ProviderErrorNote, costCapCause)
	}
	h.wantCharges()
	h.wantBudgets(Budgets{})
	h.wantNoFailure()
	if state := h.state(); FailedOrRetired(state) {
		h.t.Fatalf("claim is %s at ladder step %d, want it live", state, step)
	}

	h.advance(promptRetryDelay(step) - time.Second)
	if written := h.values.written(); len(written) != step {
		h.t.Fatalf("before ladder step %d the bucket saw %d writes, want %d", step, len(written), step)
	}
	h.advance(time.Second)
	if written := h.values.written(); len(written) != step+1 || written[step].Number != number {
		h.t.Fatalf("at ladder step %d the bucket saw %+v, want one new write of task %d", step, written, number)
	}
	text := h.mustTake()
	h.wantPrompts(step + 1)
	if p := h.pending(); !p.awaitsStart() || p.Number != number {
		h.t.Fatalf("after the wait pending is %+v, want task %d accepted and awaiting its turn", p, number)
	}
	return text
}

func (h *harness) finishAfterCostCap(number uint64, takes int) {
	h.t.Helper()
	h.must(StreamTurnStart{Claim: h.token})
	if p := h.pending(); !p.startRecorded() || p.Number != number {
		h.t.Errorf("after the cap cleared and the turn started, pending is %+v, want task %d's turn recorded started", p, number)
	}
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.mustReport(nil)
	if p := h.claim().Pending; p != nil {
		h.t.Fatalf("after the clean report, pending is %+v, want task %d finished", p, number)
	}
	h.wantFinishedOnce(finishedByReport)
	h.advance(2 * turnStartBound)
	h.wantPrompts(takes)
	if _, ok := h.values.value(h.token); ok {
		h.t.Error("the bucket still holds a value after the task finished")
	}
	h.wantCharges()
	h.wantBudgets(Budgets{})
	h.wantNoFailure()
}

// The host drops the take's text before a turn starts. The cap does not charge the worker,
// even after more retries than the launch failure limit. The next take and the controller's
// notice explain why the task is owed. Once the cap clears, the same task runs to completion.
func TestChargeNoneForACappedSessionsTakeAndRunOnceAfterCapClears(t *testing.T) {
	var reasons []string
	h := waitsHarness(t, currentPlugin, func(d *Deps) {
		d.ProviderErrorRunReached = func(_ Claim, reason string) { reasons = append(reasons, reason) }
	})
	h.manualTakes()
	h.reach(StateReady)
	number := h.takeOf(providerErrorTaskText)
	h.advance(turnStartBound - time.Second)

	steps := max(providerErrorRunTold+1, h.deps.Limits.LaunchFailures+2)
	for step := 1; step <= steps; step++ {
		h.mustReportCostCap(number)
		if back := h.records.of(takenBackLiteral); len(back) != step {
			t.Fatalf("step %d: logged %d take-backs, want one per report", step, len(back))
		}
		wantNotices := 0
		if step >= providerErrorRunTold {
			wantNotices = 1
		}
		if len(reasons) != wantNotices {
			t.Fatalf("step %d: controller notices = %q, want %d", step, reasons, wantNotices)
		}
		text := h.retakeAfterCostCap(number, step)
		if !strings.HasSuffix(text, providerErrorTaskText) {
			t.Fatalf("step %d: retake text is %q, want the same task", step, text)
		}
		if step == steps && !strings.Contains(text, costCapCause) {
			t.Errorf("retake text is %q, want it to name %q", text, costCapCause)
		}
	}
	if !strings.Contains(reasons[0], costCapCause) {
		t.Errorf("controller notice = %q, want it to name %q", reasons[0], costCapCause)
	}

	h.finishAfterCostCap(number, steps+1)
	if run := h.claim().ProviderErrorRun; run != 0 {
		t.Errorf("after the clean turn the provider-error run is %d, want 0", run)
	}
}

// A drop has no stream frames. Its report must not consume the start of the first real turn.
func TestTaskNumberOfARetakeAfterOneCostCapDropHasItsOwnTurn(t *testing.T) {
	h := waitsHarness(t, currentPlugin)
	h.manualTakes()
	h.reach(StateReady)
	number := h.takeOf(providerErrorTaskText)

	h.mustReportCostCap(number)
	h.retakeAfterCostCap(number, 1)
	h.finishAfterCostCap(number, 2)
}

// The store applies the report, but refuses the take-back. A restarted daemon must read the
// code in the stored outcome and take the task back onto the same ladder, with the same note.
func TestTakeBackOfCostCapOwedAcrossADaemonRestartNamesTheCap(t *testing.T) {
	cut := &cutOffStore{takes: -1}
	h := waitsHarness(t, currentPlugin, func(d *Deps) { cut.Store, d.Store = d.Store, cut })
	h.manualTakes()
	h.reach(StateReady)
	number := h.takeOf(providerErrorTaskText)

	cut.set(1)
	_, err := h.reportCostCap(number)
	cut.set(-1)
	if !errors.Is(err, errBoom) {
		t.Fatalf("cost_cap report whose take-back the store refused = %v, want %v", err, errBoom)
	}
	stored := h.store.load(h.token)
	if p := stored.Pending; p == nil || p.Number != number || !p.taken() || p.ProviderErrorRetries != 0 {
		t.Fatalf("the store holds %+v, want task %d still taken with no ladder step", p, number)
	}
	if kept := stored.TurnOutcomes; len(kept) != 1 || kept[0].Unapplied || kept[0].Task != number || kept[0].ProviderErrorCode != "cost_cap" {
		t.Fatalf("the store keeps %+v, want the applied cost_cap report for task %d", kept, number)
	}

	h.restart()
	h.connect()
	h.wantLadderOwedLine(true)
	if text := h.retakeAfterCostCap(number, 1); !strings.Contains(text, costCapCause) {
		t.Errorf("retake text after restart is %q, want it to name %q", text, costCapCause)
	}
	h.finishAfterCostCap(number, 2)
}

// The host stops a running turn before its next model call. Its cost-cap report takes the task
// back once instead of finishing it. The next clean turn finishes it.
func TestTakeBackOfATurnCutByCostCapClimbsTheLadderAndRunsOnceAfterCapClears(t *testing.T) {
	h := waitsHarness(t, currentPlugin)
	h.manualTakes()
	h.reach(StateReady)
	number := h.takeOf(providerErrorTaskText)

	h.must(StreamTurnStart{Claim: h.token})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.mustReportCostCap(number)
	if finished := h.records.of(finishedLine); len(finished) != 0 {
		t.Fatalf("logged %d finishes after a cost-cap cut turn, want none", len(finished))
	}
	if back := h.records.of(takenBackLiteral); len(back) != 1 {
		t.Fatalf("logged %d take-backs for the cut turn, want one", len(back))
	}
	if text := h.retakeAfterCostCap(number, 1); !strings.Contains(text, costCapCause) {
		t.Errorf("retake text after the cut turn is %q, want it to name %q", text, costCapCause)
	}
	h.finishAfterCostCap(number, 2)
}
