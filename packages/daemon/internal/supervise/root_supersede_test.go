package supervise

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// startTask is the task a start row of the given run and entry gives token's claim, as the
// daemon's outbox hands it over (RequestDeliver.Row, ID). An entry of 0 is a start of no entry.
func startTask(token claim.Token, row int64, generation uint64, entry int64) RequestDeliver {
	return RequestDeliver{Claim: token, ID: fmt.Sprintf("outbox:%d", row), Row: row,
		Task: fmt.Sprintf("the work of generation %d, entry %d", generation, entry), Generation: generation, Entry: entry}
}

// holding is one way a claim comes to hold a task it has not confirmed, with no send of it in
// flight and no prompt of it waiting on a result.
type holding struct {
	name string
	hold func(h *harness, task RequestDeliver)
}

var holdings = []holding{
	// The start's task reached a claim whose launch has not come up, so nothing has sent it.
	{"queued while the launch runs", func(h *harness, task RequestDeliver) {
		h.t.Helper()
		h.launch()
		h.must(task)
	}},
	// The defect's own shape (dispatch://INFRA-2614): the task was sent and acknowledged, no turn
	// took it, and the daemon's suspension of the lingering tree let the process go, and with it
	// the wait for that prompt's result (letGo).
	{"sent and acknowledged, then suspended before any turn", func(h *harness, task RequestDeliver) {
		h.t.Helper()
		h.reach(StateReady)
		h.must(task)
		if p := h.pending(); !p.Prompted || p.DeliveredAt.IsZero() || len(h.outstanding()) != 1 {
			h.t.Fatalf("the task after its send = %+v with prompts %v awaiting a result, want it sent and acknowledged",
				p, h.outstanding())
		}
		h.must(RequestSuspend{Claim: h.token, Reason: "the tree of LEGION-208 lingers", By: SuspendedByDaemon})
		h.wantState(StateSuspended)
	}},
}

// bringUp has the claim's agent come up, which is where a pending task is sent: a suspended claim
// is resumed by rule first, as the start of the tree's next run resumes it.
func bringUp(h *harness) {
	h.t.Helper()
	if h.state() == StateSuspended {
		h.must(RequestRuleResume{Claim: h.token, Reason: "the tree runs again"})
	}
	h.relaunched()
}

// heldUnconfirmed fails the test unless the claim holds task, unconfirmed, with nothing of it in
// the agent's hands: the shape the rule is about.
func heldUnconfirmed(h *harness, task RequestDeliver) {
	h.t.Helper()
	if p := h.pending(); p.ID != task.ID || !p.ConfirmedAt.IsZero() || len(h.outstanding()) != 0 {
		h.t.Fatalf("the claim holds %+v with prompts %v awaiting a result, want %s unconfirmed with none",
			p, h.outstanding(), task.ID)
	}
}

// A tree's root architect holding a task of an earlier run, or of an earlier entry of the same
// run, that no turn confirmed and that nothing of is in the agent's hands has it superseded by a
// task of a newer entry: the newer task is pending in its place, written with its entry and that
// entry's handshake as any delivery is, the older one is retired, and the first prompt the agent
// is sent once it comes up is the newer task. Refusing it instead, as the claim did before
// dispatch://INFRA-2614, sent a re-admitted architect the generation-1 task that told it the tree
// was open at generation 1 and that no message was coming, while the tree's children ran.
func TestANewerEntrysTaskSupersedesARootArchitectsUnconfirmedTask(t *testing.T) {
	for _, held := range holdings {
		for _, c := range []struct {
			name       string
			old, newer RequestDeliver
		}{
			{"a task of the run before", startTask(rootToken, 3, 1, 1), startTask(rootToken, 15, 2, 2)},
			{"a task of a lower entry of the same run", startTask(rootToken, 3, 2, 1), startTask(rootToken, 15, 2, 2)},
		} {
			t.Run(held.name+"/"+c.name, func(t *testing.T) {
				h := newHarnessOf(t, rootClaim())
				held.hold(h, c.old)
				heldUnconfirmed(h, c.old)
				sent := len(h.prompts())

				if err := h.handle(c.newer); err != nil {
					t.Fatalf("the newer entry's task was answered %v, want it queued in place of %s", err, c.old.ID)
				}

				p := h.pending()
				if p.ID != c.newer.ID || p.Task != c.newer.Task || p.Generation != c.newer.Generation || p.Entry != c.newer.Entry {
					t.Fatalf("pending %+v, want the newer entry's task %s", p, c.newer.ID)
				}
				got := h.claim()
				if got.GivenEntry != c.newer.Entry || got.Handshake.Entry != c.newer.Entry || got.Handshake.Generation != c.newer.Generation {
					t.Fatalf("the claim records entry %d given with handshake %+v, want entry %d of generation %d",
						got.GivenEntry, got.Handshake, c.newer.Entry, c.newer.Generation)
				}
				if stored := h.store.load(rootToken); stored.Pending == nil || stored.Pending.ID != c.newer.ID ||
					stored.GivenEntry != c.newer.Entry || !slices.Contains(stored.DeliveredRows, c.newer.Row) {
					t.Fatalf("the store holds %+v, want the newer task with its entry and its row", stored)
				}
				log := h.store.deliveryLog()
				want := []string{"retire " + c.old.ID, "put " + c.newer.ID}
				if len(log) < 2 || !slices.Equal(log[len(log)-2:], want) {
					t.Fatalf("the delivery writes = %v, want them to end %v: the older task retired, then the newer one written", log, want)
				}
				if lines := h.logs.lines("is superseded by the newer entry's task", "superseded="+c.old.ID, "delivery="+c.newer.ID); len(lines) != 1 {
					t.Fatalf("logged %v, want one line naming both deliveries", lines)
				}

				bringUp(h)

				prompts := h.prompts()[sent:]
				if len(prompts) != 1 || prompts[0].DeliveryID != c.newer.ID || prompts[0].Message != c.newer.Task {
					t.Fatalf("prompts once the agent came up = %+v, want the newer entry's task alone", prompts)
				}
			})
		}
	}
}

// A prompt of the pending task that Oh My Pi acknowledged and has not answered is a task in the
// agent's hands: that prompt's turn is the session's next, and a newer task put in its place would
// be confirmed by it and retired unsent at its end. So the newer entry's task is refused, as any
// task behind an untaken one is, and the row that carries it is retried.
func TestARootArchitectsTaskAPromptOfWhichAwaitsItsResultIsNotSuperseded(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.reach(StateReady)
	old, newer := startTask(rootToken, 3, 1, 1), startTask(rootToken, 15, 2, 2)
	h.must(old)
	if waits := h.outstanding(); !slices.Equal(waits, []string{old.ID}) {
		t.Fatalf("prompts awaiting a result = %v, want the one prompt of %s", waits, old.ID)
	}
	sent := len(h.prompts())

	err := h.handle(newer)

	if !errors.Is(err, ErrDeliveryPending) {
		t.Fatalf("the newer entry's task was answered %v, want the pending-delivery refusal", err)
	}
	if p := h.pending(); p.ID != old.ID {
		t.Fatalf("pending %+v, want %s kept", p, old.ID)
	}
	if log := h.store.deliveryLog(); slices.Contains(log, "retire "+old.ID) {
		t.Fatalf("the delivery writes = %v, want %s never retired", log, old.ID)
	}
	h.wantPrompts(sent)
}

// The rule is a tree's root architect's, and only for a request that carries an entry it raised:
// in every other case an unconfirmed pending task refuses a newer one, as it did before. A phase
// worker's claim holding a task of the run before is refused (the daemon's
// TestARetryOfAClaimThatKeptOtherWorkStillDeliversItsTask pins the same refusal through the
// outbox), and so is a root architect's request of no entry, which hands the architect no newer
// entry's work. In each the older task is the one the agent is sent once it comes up.
func TestAnUnconfirmedTaskTheRuleDoesNotReachStillRefusesANewerOne(t *testing.T) {
	for _, held := range holdings {
		for _, c := range []struct {
			name       string
			claim      Claim
			old, newer RequestDeliver
		}{
			{"a phase worker's claim", queuedClaim(), startTask(testToken, 3, 1, 1), startTask(testToken, 15, 2, 2)},
			{"a request of no entry", rootClaim(), startTask(rootToken, 3, 1, 1), startTask(rootToken, 15, 2, 0)},
		} {
			t.Run(held.name+"/"+c.name, func(t *testing.T) {
				h := newHarnessOf(t, c.claim)
				held.hold(h, c.old)
				heldUnconfirmed(h, c.old)
				sent := len(h.prompts())

				err := h.handle(c.newer)

				if !errors.Is(err, ErrDeliveryPending) {
					t.Fatalf("the newer task was answered %v, want the pending-delivery refusal", err)
				}
				if p := h.pending(); p.ID != c.old.ID {
					t.Fatalf("pending %+v, want %s kept", p, c.old.ID)
				}
				if log := h.store.deliveryLog(); slices.Contains(log, "retire "+c.old.ID) {
					t.Fatalf("the delivery writes = %v, want %s never retired", log, c.old.ID)
				}

				bringUp(h)

				prompts := h.prompts()[sent:]
				if len(prompts) == 0 || prompts[0].DeliveryID != c.old.ID {
					t.Fatalf("prompts once the agent came up = %+v, want the older task first", prompts)
				}
			})
		}
	}
}

// A start of the root's next run releases a task held while the root had no architect to hear it
// (StartedBy), except one the start's own task supersedes: sent there, the held task would be the
// architect's first prompt and the start's own task would wait behind its turn. The start's
// delivery then retires the held task unsent and queues its own, which is the first prompt. A
// start that carries no task, and one whose task is of no entry, supersede nothing, so the held
// task is sent at the start as before.
func TestAStartReleasesTheHeldTaskUnlessItsOwnTaskSupersedesIt(t *testing.T) {
	held, newer, noEntry := startTask(rootToken, 3, 1, 1), startTask(rootToken, 15, 2, 2), startTask(rootToken, 15, 2, 0)
	for _, c := range []struct {
		name string
		task *RequestDeliver
		// first is the delivery the start's own work leaves the agent prompted with.
		first string
	}{
		{"a start of a newer entry", &newer, newer.ID},
		{"a start that carries no task", nil, held.ID},
		{"a start of no entry", &noEntry, held.ID},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newBareHarness(t)
			h.token = rootToken
			hears := false
			h.deps.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return hears, nil }
			if err := h.store.PutClaim(h.ctx, rootClaim()); err != nil {
				t.Fatal(err)
			}
			h.start(rootClaim())
			h.reach(StateReady)
			h.must(held)
			h.wantPrompts(0)
			heldUnconfirmed(h, held)

			hears = true
			if err := h.m.StartedBy(h.ctx, 15, c.task); err != nil {
				t.Fatalf("record the start: %v", err)
			}
			h.m.Wait()
			if c.task != nil {
				// The start's own task follows, as the outbox delivers it once the claim is
				// brought back; behind a held task it is refused, and its row retried.
				if err := h.handle(*c.task); err != nil && !errors.Is(err, ErrDeliveryPending) {
					t.Fatalf("deliver the start's own task: %v", err)
				}
			}

			prompts := h.prompts()
			if len(prompts) != 1 || prompts[0].DeliveryID != c.first {
				t.Fatalf("prompts after the start = %+v, want %s alone", prompts, c.first)
			}
			if c.first == newer.ID {
				if log := h.store.deliveryLog(); !slices.Contains(log, "retire "+held.ID) {
					t.Fatalf("the delivery writes = %v, want the held task %s retired", log, held.ID)
				}
				if lines := h.logs.lines("the start's own task supersedes the held task", "held="+held.ID); len(lines) != 1 {
					t.Fatalf("logged %v, want one line naming the held task the start does not send", lines)
				}
			}
		})
	}
}
