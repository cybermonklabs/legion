package supervise

// A task whose adoption the shim refused is sent again once the refusal's cause is gone, with no
// wake (dispatch://INFRA-3916 R3). A refusal arms nothing: the sweep's observation of a live
// process in a ready claim is the retry of a delivery that waits (observe, table.go), so the next
// sweep after the cause is gone sends the task. On OSSV-537 the task waited for other reasons —
// the stale-plugin hold, then the architect's turn — and this holds that retry. A refusal is the
// shim's answer, so a third one is counted on the pending task like the first two and charges
// the claim nothing (R7).
//
// An observe that skipped sendPending for a pending task with SendFailures above 0 would send
// nothing at the sweep that should meet the second refusal, and both cases would fail.

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

func TestARefusedAdoptionIsSentAgainAtTheNextSweep(t *testing.T) {
	const task = "implement the plan"
	// refusedTwice is the implementer's task refused at its first send and at the sweep after it,
	// as OSSV-537's was while a git lock another role's cut-off command left stood in the worktree.
	refusedTwice := func(t *testing.T) *harness {
		t.Helper()
		h := adoptingHarness(t)
		h.rt.FailAdoptWorkingCopy(adoptRefusal(staleLockRefusal))

		h.must(RequestDeliver{Claim: testToken, Task: task, Phase: phase.Implementing, Generation: 7})
		h.observe(runtime.Alive)

		h.wantCalls("AdoptWorkingCopy", 2)
		h.wantPrompts(0)
		if p := h.pending(); p.Task != task || p.SendFailures != 2 || !p.ConfirmedAt.IsZero() {
			t.Fatalf("pending %+v, want the task waiting with its two refused sends counted", p)
		}
		return h
	}

	t.Run("the task is sent at the next sweep once the cause is gone", func(t *testing.T) {
		h := refusedTwice(t)

		h.rt.FailAdoptWorkingCopy(nil) // the lock is removed
		h.observe(runtime.Alive)

		h.wantCalls("AdoptWorkingCopy", 3)
		if sent, p := h.wantPrompts(1)[0], h.pending(); sent.DeliveryID != p.ID || sent.Message != task {
			t.Fatalf("sent %+v while %+v is pending, want the pending task itself", sent, p)
		}
		// No wake carried it: the claim's process is the one it had, and nothing waits to launch.
		h.wantCalls("Spawn", 1)
		h.wantCalls("Resume", 0)
		h.wantCalls("Suspend", 0)
		if waiting := h.m.View().WakeWaiting; waiting != nil {
			t.Fatalf("the claim holds the wake %+v, want none: the sweep sends the task to the process it has", waiting)
		}
		h.wantBudgets(Budgets{})
	})

	t.Run("a third refusal is counted on the task and charges nothing", func(t *testing.T) {
		h := refusedTwice(t)

		h.observe(runtime.Alive) // the lock still stands

		h.wantCalls("AdoptWorkingCopy", 3)
		h.wantPrompts(0)
		h.wantState(StateReady)
		h.wantBudgets(Budgets{})
		h.wantCalls("Suspend", 0)
		if p := h.pending(); p.SendFailures != 3 || !strings.Contains(p.LastSendError, staleLockRefusal) {
			t.Fatalf("pending %+v, want three refused sends counted, the last naming the shim's words", p)
		}
		if stored, ok := h.store.delivery(testToken); !ok || stored.SendFailures != 3 {
			t.Fatalf("stored delivery %+v (held %t), want the three refused sends written with it", stored, ok)
		}
	})
}
