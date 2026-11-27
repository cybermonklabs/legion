package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A start's task names a decision a person made while nobody in the root's tree could hear it, and
// the root's architect takes it: its turn on that task starts, so the architect has been told. The
// write recording that confirmation is the one that tells the decision (store.tellKeptDecisions),
// and it can fail while the turn goes on. The daemon then cannot record that it told, so the
// decision must be named again rather than lost: the tree's rule is that a person's decision is
// never lost and that telling it twice is the lesser fault (the architect's decision on #2163,
// review round cap, 31 October 2026). What this test holds is that the decision is told again,
// never zero times, so a failed write cannot lose it.
//
// Here the server refuses the write (a trigger's SQLSTATE P0001), which the claim's machine sorts
// as refused (supervise.SortClaimWriteFailure): it reads the claim's row and builds itself again
// over it, so the task stands unconfirmed and still holds the decision, and the claim's writes then
// wait one probe interval (R2 of dispatch://INFRA-4099's spec, D5 of its plan). The prompt's result
// in that window retires the task in memory and owes the retirement, and the claim's first decision
// after the window makes it (supervise's repair). A retirement tells nothing and leaves what the
// task held KEPT (store.RetireDelivery), so the root's next start names the decision again. Until
// that decision the task that holds the decision still stands, unconfirmed, and a restart sends it
// again, so the decision is never lost in between either. The machines' clock stands still, so the
// whole turn falls inside the window, and each row moves it past the window's end before that
// decision.
//
// The rows differ in when the root's next generation comes. After the window, its first start names
// the decision. Inside the window, the unconfirmed task still holds the decision, so a start
// composed then names nothing (record's keptCounts); the generation's first start composed once the
// repair retired that task, the first start nothing claims the decision for any more, names it. The
// start composed inside the window is not run before the window ends: the claim's writes wait until
// then, so its run would answer that wait rather than act.
func TestADecisionToldInATurnWhoseConfirmationFailedIsToldAgain(t *testing.T) {
	t.Parallel()
	t.Run("readmitted after the window", func(t *testing.T) {
		t.Parallel()
		turn := toldInATurnWhoseConfirmationIsRefused(t)
		r := turn.rig
		turn.repairAfterTheWindow(t)

		// The root's next generation: its start task names what the root still keeps.
		r.readmit()
		var task string
		err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			task, _, err = r.engine.ParkedDecisionTask(r.ctx, tx, r.root)
			return err
		})
		if err != nil {
			t.Fatalf("compose the next start's task: %v", err)
		}
		if !strings.Contains(task, turn.ref) {
			t.Fatalf("the root's next start task %q does not name %s; the turn's confirmation write was refused, so nothing recorded "+
				"that the architect was told, and the decision must be told again rather than lost (kept now: %s)",
				task, turn.ref, keptRefs(keptDecisions(t, r.pool, r.records, heldTaskRoot)))
		}
	})

	t.Run("readmitted inside the window", func(t *testing.T) {
		t.Parallel()
		turn := toldInATurnWhoseConfirmationIsRefused(t)
		r := turn.rig

		// The root's next generation comes at once, inside the window. The task the refused
		// confirmation left unconfirmed still holds the decision, so the generation's first start
		// names nothing.
		r.readmit()
		first := r.enqueueStart()
		kept := keptDecisions(t, r.pool, r.records, heldTaskRoot)
		if len(kept) != 1 || kept[0].Ref != turn.ref || kept[0].HeldBy.Delivery != turn.sent {
			t.Fatalf("the root keeps %+v inside the window, want %s held by the unconfirmed task %s", kept, turn.ref, turn.sent)
		}
		if starts := startRequestsOf(t, r.pool, heldTaskRoot); len(starts) != 1 || strings.Contains(starts[0].Task, turn.ref) {
			t.Fatalf("the starts queued inside the window = %+v, want one that does not name %s, which the unconfirmed task holds", starts, turn.ref)
		}

		// The repair retires the unconfirmed task, so nothing claims the decision any more, and the
		// generation's next start names it.
		turn.repairAfterTheWindow(t)
		second := r.enqueueStart()
		starts := startRequestsOf(t, r.pool, heldTaskRoot)
		if len(starts) != 2 || !strings.Contains(starts[1].Task, turn.ref) {
			t.Fatalf("the starts of generation %d = %+v, want the second, composed once the repair retired the unconfirmed task, naming %s "+
				"(kept now: %s)", r.root.Generation, starts, turn.ref, keptRefs(keptDecisions(t, r.pool, r.records, heldTaskRoot)))
		}

		// Both starts run in the order they were queued. The first names nothing and prompts
		// nothing, and the second tells the architect the decision again.
		r.runRow(first)
		r.runRow(second)
		prompts := r.conn.Prompts()
		if len(prompts) != 2 || !strings.Contains(prompts[1].Message, turn.ref) {
			t.Fatalf("the architect was prompted %#v, want a second prompt, the generation's second start, naming %s", prompts, turn.ref)
		}
	})
}

// refusedConfirmationTurn is the root architect's turn on a start task naming ref, whose
// confirmation the database refused: the rig, its held clock, the architect's machine, and the
// task's delivery id.
type refusedConfirmationTurn struct {
	rig     *heldTaskRig
	clock   *heldClock
	machine *supervise.Machine
	ref     string
	sent    string
}

// toldInATurnWhoseConfirmationIsRefused sends a start task naming one kept decision to the root's
// architect, and runs its turn while the database refuses the telling of that decision. The clock
// stands still, so it ends inside the window the refusal opened.
func toldInATurnWhoseConfirmationIsRefused(t *testing.T) refusedConfirmationTurn {
	t.Helper()
	clock := &heldClock{at: time.Now()}
	r := newHeldTaskRig(t, func(r *heldTaskRig) { r.sup.deps.Clock = clock })
	ref := "dispatch://" + heldTaskRoot + "/ask/ask-told"
	keepDecision(t, r.pool, r.records, heldTaskRoot, ref, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	r.runRow(r.enqueueStart())
	prompts := r.conn.Prompts()
	if len(prompts) != 1 || !strings.Contains(prompts[0].Message, ref) {
		t.Fatalf("the architect was prompted %#v, want the start's task naming %s", prompts, ref)
	}

	// The confirmation's write is refused for as long as the turn lasts: the database refuses the
	// telling of the row the delivery holds.
	for _, statement := range []string{
		`create function refuse_the_tell() returns trigger language plpgsql as $$
			begin raise exception 'the confirmation''s write did not commit'; end $$`,
		`create trigger refuse_the_tell before delete on parked_decisions for each row execute function refuse_the_tell()`,
	} {
		if _, err := r.pool.Exec(r.ctx, statement); err != nil {
			t.Fatalf("make the confirmation's write fail: %v", err)
		}
	}
	machine, _ := r.sup.Machine(r.token)
	// The turn the prompt ran starts and ends, and Oh My Pi reports the prompt's result, which is
	// what retires the delivery (supervise's promptResulted).
	sent := machine.Claim().Pending.ID
	for _, ev := range []supervise.Event{
		supervise.StreamTurnStart{Claim: r.token},
		supervise.StreamTurnEnd{Claim: r.token},
		supervise.StreamPromptResult{Claim: r.token, DeliveryID: sent, AgentInvoked: true},
	} {
		_ = machine.Handle(r.ctx, ev)
		machine.Wait()
	}
	for _, statement := range []string{
		"drop trigger refuse_the_tell on parked_decisions",
		"drop function refuse_the_tell()",
	} {
		if _, err := r.pool.Exec(r.ctx, statement); err != nil {
			t.Fatalf("let the store take writes again: %v", err)
		}
	}
	return refusedConfirmationTurn{rig: r, clock: clock, machine: machine, ref: ref, sent: sent}
}

// repairAfterTheWindow moves the clock past the window and makes the claim's first decision after
// it, which makes the retirement the prompt's result owed: the sweep's observation of the
// architect's process.
func (turn refusedConfirmationTurn) repairAfterTheWindow(t *testing.T) {
	t.Helper()
	r := turn.rig
	turn.clock.advance(r.sup.deps.Timeouts.Probe)
	if err := turn.machine.Handle(r.ctx, aliveObservation(turn.machine)); err != nil {
		t.Fatalf("observe the architect's process after the window: %v", err)
	}
	turn.machine.Wait()
}
