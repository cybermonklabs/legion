package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// slotCalls is every call of the fake runtime that wrote or read token's slot, as "<method>
// <role>", in order; a write, which names no role since the runtime judges it, is its method alone.
func slotCalls(rt *fake.Runtime, token claim.Token) []string {
	var calls []string
	for _, call := range rt.Calls() {
		if (call.Method == "WriteSlotCredential" || call.Method == "SlotHoldsCredential") && call.Claim == token {
			calls = append(calls, strings.TrimSpace(call.Method+" "+string(call.Role)))
		}
	}
	return calls
}

// The owner is told its step once its slot holds the step's App. LEGION-1 stands at step 1 of
// `implementer:owner reviewer`, which its owner, LEGION-1's own architect, does:
//
//  1. While the runtime answers that the slot's gh files do not hold the implementer's App yet,
//     the notice owner-step of step 1 is not published, and its row waits as a notice held for its
//     architect; the runtime was asked to write the slot for the implementer, then asked whether
//     it holds it.
//  2. A pod whose launcher cannot say (runtime.ErrAwaitsPodUpdate) holds the row the same way.
//  3. Once the runtime answers that the slot holds it, the notice is published once, to the
//     owner's role topic.
//  4. A supervise slot_credential of the owner's claim asks the runtime to write the slot back to
//     the owner's own App when the row runs: no write while step 1 runs, whose owner-step writes
//     that step's App, and one once the task stands at step 2, the reviewer's step, which the
//     runtime makes for the App it judges for the slot; it waits for nothing.
//  5. A notice owner-step of step 1 that runs after the task moved to step 2, whose run ended with
//     the move, finishes unpublished, with one log line, and asks the runtime nothing.
func TestAnOwnerStepIsToldOnceItsSlotHoldsTheStepsApp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stepOne := record.NoticeStep{Task: "a", Kind: "step", Place: 1, Starts: 1}
	r := newOwnerHoldRig(t, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, "implementer:owner", "reviewer")
	r.started(t, "step", 1, false)
	told := leasedOutboxRow(t, r.pool, r.records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: record.OwnerStepNotice, Role: claim.RoleImplementer,
		Phase: phase.Implementing, Reason: "Step 1 of 2: implementer:owner. Build the widget.", OwnerStep: &stepOne}, r.now))

	t.Run("1 a slot that does not hold the step's App yet", func(t *testing.T) {
		r.rt.ScriptSlotHolds(runtime.ErrSlotCredentialPending)
		err := r.runner.execute(ctx, told)
		if !errors.Is(err, errNoticeWaits) || !errors.Is(err, wait.ErrWaiting) || !errors.Is(err, runtime.ErrSlotCredentialPending) {
			t.Fatalf("the notice while the slot does not hold the implementer's App = %v, want a notice held for its architect", err)
		}
		if n := len(r.published()); n != 0 {
			t.Fatalf("the notice was published %d times while the slot does not hold the step's App, want none", n)
		}
		if got, want := slotCalls(r.rt, r.owner), []string{"WriteSlotCredential", "SlotHoldsCredential implementer"}; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("the runtime was asked %q of the owner's slot, want %q", got, want)
		}
	})

	t.Run("2 a pod whose launcher cannot say", func(t *testing.T) {
		r.rt.ScriptSlotHolds(runtime.ErrAwaitsPodUpdate)
		told.LeaseToken = leaseOutboxRow(t, r.pool, told.ID)
		if err := r.runner.execute(ctx, told); !errors.Is(err, errNoticeWaits) || !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
			t.Fatalf("the notice of a pod whose launcher cannot say = %v, want a notice held for its architect", err)
		}
		if n := len(r.published()); n != 0 {
			t.Fatalf("the notice was published %d times while the pod's launcher cannot say, want none", n)
		}
	})

	t.Run("3 a slot that holds the step's App", func(t *testing.T) {
		r.rt.ScriptSlotHolds(nil)
		told.LeaseToken = leaseOutboxRow(t, r.pool, told.ID)
		// The runner deletes a row its executor finished, so no later notice waits behind it.
		r.ran(t, told, "the notice once the slot holds the implementer's App")
		if got := r.published(); len(got) != 1 || got[0] != architectTopic(t, "LEGION-1") {
			t.Fatalf("the notice publishes %v, want one to the owner's role topic", got)
		}
	})

	slotCredential := func(t *testing.T, writes bool) {
		t.Helper()
		before := len(slotCalls(r.rt, r.owner))
		r.writtenBack(t)
		got := slotCalls(r.rt, r.owner)[before:]
		if !writes && len(got) != 0 || writes && (len(got) != 1 || got[0] != "WriteSlotCredential") {
			t.Fatalf("the slot_credential row asked the runtime %q of the owner's slot, want one write: %t", got, writes)
		}
	}
	t.Run("4 a slot_credential row writes the owner's own App back", func(t *testing.T) {
		slotCredential(t, false)
		r.stands(t, phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 2})
		r.ended(t, "step", 1)
		slotCredential(t, true)
	})

	t.Run("5 a notice of a step that no longer runs", func(t *testing.T) {
		before := len(slotCalls(r.rt, r.owner))
		stale := leasedOutboxRow(t, r.pool, r.records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: record.OwnerStepNotice, Role: claim.RoleImplementer,
			Phase: phase.Implementing, Reason: "Step 1 of 2: implementer:owner. Build the widget.", OwnerStep: &stepOne}, r.now))
		if err := r.runner.execute(ctx, stale); err != nil {
			t.Fatalf("the notice of step 1 at step 2 = %v, want it finished", err)
		}
		if n := len(r.published()); n != 1 {
			t.Fatalf("the notices published = %d after the stale one, want the one of case 3 alone", n)
		}
		if got := slotCalls(r.rt, r.owner)[before:]; len(got) != 0 {
			t.Fatalf("the stale notice asked the runtime %q of the owner's slot, want nothing", got)
		}
		staleUnread(t, r.logged.String(), "the stale notice")
	})
}
