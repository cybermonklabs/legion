package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A process found alive at a stale address (repoint) after its take's turn ended and before the
// report of that turn reached the daemon: the turn ran whole, and the process that ran it is
// alive, so its report, which travels over HTTP apart from the worker stream and is sent again
// until the daemon answers, lands after the stale observation. The task must run once: the report
// of the turn that ran it finishes it, and the relaunched process is not handed it again. The plan
// accepts a second run only for a process that dies before its report (decision 8, "this
// take-back runs a task twice only in the accepted case"), and holds every other let-go at a
// turn's end for the take's report (rule two, review round 2's R2-1); this let-go is of a live
// process. The architect's decision (dispatch://INFRA-2811/comment/5e8de1a8-c2ec-473e-8831-8bb851828198):
// repoint holds the relaunch for the owed report, bounded by R5's second wait, as rule two holds
// the stale-plugin relaunch; a repoint during a running turn stays as it is.
//
// Test round 12 (the tester's class search for R2-1 at ba2fbe5f, packages/daemon/internal/supervise/
// repoint.go:42 through replaceProcess and interrupted, delivery.go:1462-1467): at ba2fbe5f the
// repoint takes the task back at once as a cut turn, the report of the earlier generation then
// finishes nothing, and the relaunched process takes the task again, a second run; the tester's
// real-stack drive shows the same (step a10q: the old process's report refused, the relaunched
// process handed the task at its registration with the note of an interrupted turn).
func TestARelaunchForAStaleAddressAfterAnEndFrameAndBeforeItsReportRunsItsTaskOnce(t *testing.T) {
	h := boundsHarness(t)
	number := h.takeOf("implement the plan")
	ran := h.generation()
	h.must(StreamTurnStart{Claim: h.token})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	h.observe(runtime.StaleAddress)

	if err := h.m.Handle(h.ctx, RequestTurnOutcome{Claim: h.token, Generation: ran, Sequence: 1, Task: number}); err != nil {
		t.Fatalf("the report of task %d's turn, from the process that ran it: %v", number, err)
	}
	h.m.Wait()
	if h.state() == StateLaunching {
		h.relaunched()
	}
	if p := h.claim().Pending; p != nil && p.Number == number && !p.taken() {
		h.take()
	}
	takes := 0
	for _, took := range h.prompts() {
		if took.Number == number {
			takes++
		}
	}
	if takes != 1 {
		t.Errorf("task %d was taken %d times (%+v), want once: its turn ran whole and its report came", number, takes, h.prompts())
	}
	if finished := h.recordsNaming(finishedLine, number); len(finished) != 1 {
		t.Errorf("task %d was finished %d times, want once, by the report of the turn that ran it", number, len(finished))
	}
	h.wantBudgets(Budgets{})
}
