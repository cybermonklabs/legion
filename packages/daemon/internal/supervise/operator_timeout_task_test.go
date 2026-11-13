package supervise

import (
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A forced operator drain ends a long task turn without StreamTurnEnd. Its taken phase task is
// interrupted, not retired as finished: it is taken back under its number, so the operator's
// resume has the same owner take it again.
func TestTakeBackByAnOperatorStopTimeoutKeepsTheNumber(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "Continue testing.", ID: "outbox:7", Phase: phase.Testing, Generation: 1})
	number := h.pending().Number
	h.must(StreamTurnStart{Claim: testToken})
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("operator suspend = %v, want ErrSuspendHeld", err)
	}
	h.advance(testStop)
	h.wantState(StateSuspended)
	pending := h.pending()
	if !pending.Interrupted || pending.taken() || pending.ID != "outbox:7" || pending.Number != number {
		t.Fatalf("after the forced stop pending %+v, want the task interrupted and taken back under number %d", pending, number)
	}
	h.must(RequestResume{Claim: testToken})
	h.reach(StateReady)
	if pending = h.pending(); !pending.Interrupted || pending.Task != "Continue testing." || pending.Number != number {
		t.Fatalf("after the operator resume pending %+v, want the interrupted phase task", pending)
	}
	if again := h.wantPrompts(2)[1]; again.Number != number {
		t.Fatalf("the take after the resume was of task %d, want %d", again.Number, number)
	}
}
