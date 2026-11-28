package supervise

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// fixIssue is the issue record the fence of a delivery reads, as far as that fence reads it: the
// phase it stands in, its run, and the fix of its task that runs beside the step of that phase (0
// for none). holds answers as the daemon's answer does (record.PhaseHolds, which this package
// cannot import): a task of a run holds on that run alone; a fix's task holds while that fix runs,
// whatever phase the issue stands in; any other task holds while the issue is in its phase.
type fixIssue struct {
	phase      phase.Phase
	generation uint64
	runningFix int
}

func (i *fixIssue) holds(_ context.Context, _ string, queuedFor phase.Phase, generation uint64, fix int) (bool, error) {
	if generation != 0 && generation != i.generation {
		return false, nil
	}
	if fix > 0 {
		return fix == i.runningFix, nil
	}
	return i.phase == queuedFor, nil
}

// heldShut is a ready claim of an issue in reviewing on run 5 whose fix 1 runs, with its connection
// held shut, so a task it is given waits unsent and no fence has been asked about it yet.
func heldShut(t *testing.T) (*harness, *fixIssue) {
	t.Helper()
	h := newBareHarness(t)
	issue := &fixIssue{phase: phase.Reviewing, generation: 5, runningFix: 1}
	h.deps.PhaseHolds = issue.holds
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	h.reach(StateReady)
	h.conns.Remove(h.token)
	return h, issue
}

// A fix's task is queued for implementing while the issue stands in reviewing, the step it runs
// beside, and the issue moves on to held — for the fix, or for a step beside it that failed —
// before the task is sent. The fence of the send asks whether the task is still the issue's work
// with the task's fix (Deps.PhaseHolds): the fix still runs, so the task is not retired, and it is
// sent once the connection opens. The controls are the same move under a task of no fix queued for
// reviewing, which is retired as any task whose phase the issue left is, and the fix's task once
// its fix ended, which is retired too, so the hold is the running fix's.
func TestAPendingFixTaskOutlivesTheIssuesMove(t *testing.T) {
	t.Run("the fix's task, while its fix runs", func(t *testing.T) {
		h, issue := heldShut(t)
		h.must(fixTask())
		issue.phase = phase.Held

		h.conns.Register(h.token, h.conn)
		h.observe(runtime.Alive)

		if sent := h.wantPrompts(1)[0]; sent.Message != fixTask().Task {
			t.Fatalf("sent %+v, want the fix's task", sent)
		}
		if p := h.pending(); p.Fix != 1 || p.Phase != phase.Implementing {
			t.Fatalf("the pending delivery = %+v, want the fix's task, fix 1, queued for %s", p, phase.Implementing)
		}
	})

	t.Run("control: a task of no fix whose phase the issue left", func(t *testing.T) {
		h, issue := heldShut(t)
		h.must(RequestDeliver{Claim: testToken, Task: "review the head", Phase: phase.Reviewing, Generation: 5})
		issue.phase = phase.Held

		h.conns.Register(h.token, h.conn)
		h.observe(runtime.Alive)

		h.wantPrompts(0)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the claim still holds %+v, want the task retired: the issue left reviewing", p)
		}
		if _, ok := h.store.delivery(testToken); ok {
			t.Fatal("the store still holds the task, want it retired durably")
		}
	})

	t.Run("control: the fix's task once its fix ended", func(t *testing.T) {
		h, issue := heldShut(t)
		h.must(fixTask())
		issue.phase, issue.runningFix = phase.Held, 0

		h.conns.Register(h.token, h.conn)
		h.observe(runtime.Alive)

		h.wantPrompts(0)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the claim still holds %+v, want the fix's task retired: its fix no longer runs", p)
		}
	})
}
