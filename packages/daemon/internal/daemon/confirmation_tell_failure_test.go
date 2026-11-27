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
// whole turn falls inside the window, and the test moves it past the window's end before that
// decision.
func TestADecisionToldInATurnWhoseConfirmationFailedIsToldAgain(t *testing.T) {
	t.Parallel()
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

	// The claim's first decision after the window, which makes the retirement the prompt's result
	// owed: the sweep's observation of the architect's process.
	clock.advance(r.sup.deps.Timeouts.Probe)
	if err := machine.Handle(r.ctx, aliveObservation(machine)); err != nil {
		t.Fatalf("observe the architect's process after the window: %v", err)
	}
	machine.Wait()

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
	if !strings.Contains(task, ref) {
		t.Fatalf("the root's next start task %q does not name %s; the turn's confirmation write was refused, so nothing recorded "+
			"that the architect was told, and the decision must be told again rather than lost (kept now: %s)",
			task, ref, keptRefs(keptDecisions(t, r.pool, r.records, heldTaskRoot)))
	}
}
