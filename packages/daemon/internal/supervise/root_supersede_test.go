package supervise

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// startTask is the task a start row of the given run and entry gives token's claim, as the
// daemon's outbox hands it over (RequestDeliver.Row, ID). An entry of 0 is a start of no entry.
func startTask(token claim.Token, row int64, generation uint64, entry int64) RequestDeliver {
	return RequestDeliver{Claim: token, ID: fmt.Sprintf("outbox:%d", row), Row: row,
		Task: fmt.Sprintf("the work of generation %d, entry %d", generation, entry), Generation: generation, Entry: entry}
}

// wakeTask is the task a wake's start row gives the root architect's claim (the workflow's
// wakeRow): of the root's run at generation, naming no phase and no entry, carrying the sentence
// that says why the pod came back.
func wakeTask(row int64, generation uint64) RequestDeliver {
	return RequestDeliver{Claim: rootToken, ID: fmt.Sprintf("outbox:%d", row), Row: row,
		Task: "Your pod was released and has come back: a message was published to your role.", Generation: generation}
}

// takeTurn is the agent working the task its take was accepted for, delivery: that take's turn
// and the turn's end, behind which the worker reports the turn of that task (handle), after which
// the claim holds no delivery.
func takeTurn(h *harness, delivery string) {
	h.t.Helper()
	if p := h.pending(); p.ID != delivery || !p.awaitsTurn() {
		h.t.Fatalf("the claim holds %+v, want the take of %s accepted and its turn to come", p, delivery)
	}
	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})
	if p := h.claim().Pending; p != nil {
		h.t.Fatalf("the claim holds %+v after the turn of %s, want that task finished", p, delivery)
	}
}

// holding is one way a claim comes to hold a task no take of which is accepted, and nothing of
// which is in the agent's hands.
type holding struct {
	name string
	hold func(h *harness, task RequestDeliver)
}

var holdings = []holding{
	// The start's task reached a claim whose launch has not come up, so nobody has taken it.
	{"queued while the launch runs", func(h *harness, task RequestDeliver) {
		h.t.Helper()
		h.launch()
		h.must(task)
	}},
	// The defect's own shape (dispatch://INFRA-2614), on the take: the task's value was written
	// and the worker reported it seen, no take of it was made, and the daemon's suspension of the
	// lingering tree let the process go.
	{"its value written and seen, no take of it made, then suspended", func(h *harness, task RequestDeliver) {
		h.t.Helper()
		h.manualTakes()
		h.reach(StateReady)
		h.must(task)
		h.seen()
		h.must(RequestSuspend{Claim: h.token, Reason: "the tree of LEGION-208 lingers", By: SuspendedByDaemon})
		h.wantState(StateSuspended)
	}},
	// The worker asked to take it while the agent was in a turn of its own, and the take was
	// refused (claimHold's in-turn hold); the daemon's suspension of the lingering tree was held
	// for that turn and stopped the process at its end.
	{"its take refused in a turn of the agent's own, then suspended at that turn's end", func(h *harness, task RequestDeliver) {
		h.t.Helper()
		h.manualTakes()
		h.reach(StateReady)
		h.must(StreamTurnStart{Claim: h.token})
		// The session reports that nothing of its own runs, so the stop at the turn's end ends no
		// work and records nothing the task would carry (recordEnd).
		h.reportActivity()
		h.must(task)
		h.wantRefused(HoldInTurn)
		if err := h.handle(RequestSuspend{Claim: h.token, Reason: "the tree of LEGION-208 lingers", By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
			h.t.Fatalf("the suspension in the agent's own turn answered %v, want it held for the turn's end", err)
		}
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.wantState(StateSuspended)
	}},
}

// bringUp has the claim's agent come up, which is where its worker takes the pending task: a
// suspended claim is resumed by rule first, as the start of the tree's next run resumes it. The
// worker takes on its own from here (workerAsks).
func bringUp(h *harness) {
	h.t.Helper()
	h.worker.manual = false
	if h.state() == StateSuspended {
		h.must(RequestRuleResume{Claim: h.token, Reason: "the tree runs again"})
	}
	h.relaunched()
}

// heldUntaken fails the test unless the claim holds task and no take of it is accepted, so
// nothing of it is in the agent's hands: the shape the rule is about.
func heldUntaken(h *harness, task RequestDeliver) {
	h.t.Helper()
	if p := h.pending(); p.ID != task.ID || p.taken() {
		h.t.Fatalf("the claim holds %+v, want %s with no take of it accepted", p, task.ID)
	}
}

// A tree's root architect holding a task of an earlier run, or of an earlier entry of the same
// run, no take of which is accepted, has it superseded by a task of a newer entry: the newer task
// is pending in its place, written with its entry and that entry's handshake as any delivery is,
// the older one is retired, and the first take the agent's worker makes once it comes up is
// answered with the newer task. Refusing it instead, as the claim did before
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
				heldUntaken(h, c.old)
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
					t.Fatalf("takes once the agent came up = %+v, want the newer entry's task alone", prompts)
				}
			})
		}
	}
}

// A task whose take is accepted is in the agent's hands: that take's turn is the session's next,
// and the text the worker runs in it is that task's. So the newer entry's task is refused, as any
// task behind a take is, and the row that carries it is retried.
func TestARootArchitectsTaskATakeHoldsIsNotSuperseded(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.reach(StateReady)
	old, newer := startTask(rootToken, 3, 1, 1), startTask(rootToken, 15, 2, 2)
	h.must(old)
	if p := h.pending(); p.ID != old.ID || !p.awaitsTurn() {
		t.Fatalf("the claim holds %+v, want the take of %s accepted and its turn to come", p, old.ID)
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

// The rule is a tree's root architect's, and only for a request that carries an entry it raised
// and a held task that carries an entry of its own: in every other case an untaken pending task
// refuses a newer one, as it did before. A phase worker's claim holding a task of the run before
// is refused, which this test is the one to pin: the daemon's
// TestARetryOfAClaimThatKeptOtherWorkStillDeliversItsTask starts with no entry, so it never
// reaches the role test. So is a root architect's request of no entry, which hands the architect
// no newer entry's work, and a root architect's request of a newer entry behind a wake's task,
// which carries no entry (the workflow's wakeRow): it is the sentence that says why the pod came
// back, which no entry's task says, so no entry's task takes its place (Delivery.SupersededBy).
// In each the older task is the one the agent's worker takes first once it comes up.
func TestAnUnconfirmedTaskTheRuleDoesNotReachStillRefusesANewerOne(t *testing.T) {
	for _, held := range holdings {
		for _, c := range []struct {
			name       string
			claim      Claim
			old, newer RequestDeliver
		}{
			{"a phase worker's claim", queuedClaim(), startTask(testToken, 3, 1, 1), startTask(testToken, 15, 2, 2)},
			{"a request of no entry", rootClaim(), startTask(rootToken, 3, 1, 1), startTask(rootToken, 15, 2, 0)},
			{"a root architect's wake task, of no entry", rootClaim(), wakeTask(3, 2), startTask(rootToken, 15, 2, 2)},
		} {
			t.Run(held.name+"/"+c.name, func(t *testing.T) {
				h := newHarnessOf(t, c.claim)
				held.hold(h, c.old)
				heldUntaken(h, c.old)
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
					t.Fatalf("takes once the agent came up = %+v, want the older task first", prompts)
				}
			})
		}
	}
}

// unheardRootTask is a tree root architect's claim that is ready and holds task untaken, because
// its root had no architect to hear it (Deps.RootHears) and its worker's take was refused for
// that, and whose root hears from here on: the claim a start of the root's run reaches
// (StartedBy).
func unheardRootTask(t *testing.T, task RequestDeliver) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.token = rootToken
	hears := false
	h.deps.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return hears, nil }
	if err := h.store.PutClaim(h.ctx, rootClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(rootClaim())
	h.reach(StateReady)
	h.must(task)
	h.wantPrompts(0)
	heldUntaken(h, task)
	hears = true
	return h
}

// A start of the root's next run releases a task held while the root had no architect to hear it
// (StartedBy), except one the start's own task supersedes: released there, the held task would be
// the architect's first take and the start's own task would wait behind its turn. The start's
// delivery then retires the held task untaken and queues its own, which is the first take. A
// start that carries no task, and one whose task is of no entry, supersede nothing, so the held
// task's value is written again at the start and the worker takes it, as before.
func TestAStartReleasesTheHeldTaskUnlessItsOwnTaskSupersedesIt(t *testing.T) {
	held, newer, noEntry := startTask(rootToken, 3, 1, 1), startTask(rootToken, 15, 2, 2), startTask(rootToken, 15, 2, 0)
	for _, c := range []struct {
		name string
		task *RequestDeliver
		// first is the delivery of the worker's one accepted take once the start's own work is done.
		first string
	}{
		{"a start of a newer entry", &newer, newer.ID},
		{"a start that carries no task", nil, held.ID},
		{"a start of no entry", &noEntry, held.ID},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := unheardRootTask(t, held)
			if err := h.m.StartedBy(h.ctx, 15, c.task); err != nil {
				t.Fatalf("record the start: %v", err)
			}
			h.m.Wait()
			// The worker reads the value the start wrote again, as its plugin reads each value
			// the relay hands it.
			h.workerAsks()
			if c.task != nil {
				// The start's own task follows, as the outbox delivers it once the claim is
				// brought back; behind a held task it is refused, and its row retried.
				if err := h.handle(*c.task); err != nil && !errors.Is(err, ErrDeliveryPending) {
					t.Fatalf("deliver the start's own task: %v", err)
				}
			}

			prompts := h.prompts()
			if len(prompts) != 1 || prompts[0].DeliveryID != c.first {
				t.Fatalf("takes after the start = %+v, want %s alone", prompts, c.first)
			}
			if c.first == newer.ID {
				if log := h.store.deliveryLog(); !slices.Contains(log, "retire "+held.ID) {
					t.Fatalf("the delivery writes = %v, want the held task %s retired", log, held.ID)
				}
				if lines := h.logs.lines("the start's own task supersedes the held task", "held="+held.ID); len(lines) != 1 {
					t.Fatalf("logged %v, want one line naming the held task the start does not offer", lines)
				}
			}
		})
	}
}

// An entry's task refused behind a root architect's wake task (the table above) is not lost: its
// row is retried, and once the turn of the wake task's take has ended, the entry's task is
// delivered and is the next take after the wake's. Retired in the wake's place instead, the
// wake's sentence was never taken.
func TestAnEntrysTaskRefusedBehindAWakesTaskIsSentOnceTheWakesTurnEnds(t *testing.T) {
	for _, held := range holdings {
		t.Run(held.name, func(t *testing.T) {
			h := newHarnessOf(t, rootClaim())
			wake, entry := wakeTask(3, 2), startTask(rootToken, 15, 2, 2)
			held.hold(h, wake)
			sent := len(h.prompts())
			if err := h.handle(entry); err != nil && !errors.Is(err, ErrDeliveryPending) {
				t.Fatalf("deliver the entry's task: %v", err)
			}
			bringUp(h)
			if prompts := h.prompts()[sent:]; len(prompts) != 1 || prompts[0].DeliveryID != wake.ID ||
				!strings.Contains(prompts[0].Message, wake.Task) {
				t.Fatalf("takes once the agent came up = %+v, want the wake's task alone", prompts)
			}

			takeTurn(h, wake.ID)
			h.must(entry)

			prompts := h.prompts()[sent:]
			if len(prompts) != 2 || prompts[1].DeliveryID != entry.ID || prompts[1].Message != entry.Task {
				t.Fatalf("takes once the wake's turn ended and the entry's row was retried = %+v, want the entry's task after the wake's", prompts)
			}
		})
	}
}

// A start run against a tree root architect's claim writes again the value of the task the claim
// held while its root had no architect to hear it (StartedBy), and a held wake's task is one no
// start's own task supersedes, whatever entry and run that task carries: the wake's task is the
// first take, the start's own task is refused behind it and its row retried, and it is delivered
// once the wake's turn is over.
func TestAStartSendsAHeldWakeTaskBeforeItsOwnTask(t *testing.T) {
	wake := wakeTask(3, 2)
	for _, c := range []struct {
		name string
		task RequestDeliver
	}{
		{"a start of an entry of the wake's run", startTask(rootToken, 15, 2, 2)},
		{"a start of the run after the wake's", startTask(rootToken, 15, 3, 1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := unheardRootTask(t, wake)
			if err := h.m.StartedBy(h.ctx, c.task.Row, &c.task); err != nil {
				t.Fatalf("record the start: %v", err)
			}
			h.m.Wait()
			h.workerAsks()
			if prompts := h.prompts(); len(prompts) != 1 || prompts[0].DeliveryID != wake.ID {
				t.Fatalf("takes after the start = %+v, want the held wake's task alone", prompts)
			}
			if lines := h.logs.lines("the start's own task supersedes the held task"); len(lines) != 0 {
				t.Fatalf("logged %v, want no held task passed over for a wake's", lines)
			}
			if err := h.handle(c.task); !errors.Is(err, ErrDeliveryPending) {
				t.Fatalf("the start's own task was answered %v, want the pending-delivery refusal behind the wake's task", err)
			}

			takeTurn(h, wake.ID)
			h.must(c.task)
			prompts := h.prompts()
			if len(prompts) != 2 || prompts[1].DeliveryID != c.task.ID {
				t.Fatalf("takes once the wake's turn ended and the start's row was retried = %+v, want the start's task after the wake's", prompts)
			}
		})
	}
}
