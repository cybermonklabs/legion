package supervise

import (
	"errors"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The entry a start was queued for is written on the claim in the transaction that writes the
// delivery (Claim.GivenEntry, queue), and the entry its worker took at the retirement of the
// task that turn confirmed (Claim.ServedEntry, retirePending). A write the store refuses leaves
// the claim neither the delivery nor the number, so the row's retry gives that entry's task.
func TestTheGivenEntryIsWrittenWithTheDeliveryAndTheServedEntryAtItsRetirement(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)

	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
		Phase: phase.Implementing, Generation: 7, Entry: 1})

	if c := h.claim(); c.GivenEntry != 1 || c.ServedEntry != 0 {
		t.Fatalf("the claim was given entry %d and served entry %d, want entry 1 given and none served: the task is queued, not taken",
			c.GivenEntry, c.ServedEntry)
	}
	stored := h.store.load(testToken)
	if stored.GivenEntry != 1 || stored.Pending == nil || stored.Pending.Entry != 1 {
		t.Fatalf("the stored claim was given entry %d beside delivery %+v, want entry 1 beside the delivery carrying it",
			stored.GivenEntry, stored.Pending)
	}

	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})

	if c := h.claim(); c.GivenEntry != 1 || c.ServedEntry != 1 {
		t.Fatalf("after the turn the claim was given entry %d and served entry %d, want 1 and 1",
			c.GivenEntry, c.ServedEntry)
	}
	if stored := h.store.load(testToken); stored.GivenEntry != 1 || stored.ServedEntry != 1 {
		t.Fatalf("after the turn the stored claim was given entry %d and served entry %d, want 1 and 1",
			stored.GivenEntry, stored.ServedEntry)
	}

	h.store.fail("PutDelivery", errBoom)
	err := h.handle(RequestDeliver{Claim: testToken, Task: "test it", ID: "outbox:41", Row: 41,
		Phase: phase.Testing, Generation: 7, Entry: 2})

	if !errors.Is(err, errBoom) {
		t.Fatalf("the deliver the store refused = %v, want the store's refusal", err)
	}
	if c := h.claim(); c.GivenEntry != 1 {
		t.Fatalf("the claim was given entry %d after the write was refused, want the 1 the store holds: the entry is written with the delivery",
			c.GivenEntry)
	}
	if stored := h.store.load(testToken); stored.GivenEntry != 1 {
		t.Fatalf("the stored claim was given entry %d after the write was refused, want 1", stored.GivenEntry)
	}
}

// Both numbers are the claims row's, so a daemon that restarts reads back what the role was
// given and what its worker took, and judges the next start on the record rather than on what
// the machine it lost held.
func TestARestartReadsTheGivenEntryAndTheServedEntryBack(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
		Phase: phase.Implementing, Generation: 7, Entry: 1})
	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})

	h.restart()

	if c := h.claim(); c.GivenEntry != 1 || c.ServedEntry != 1 {
		t.Fatalf("the restarted claim was given entry %d and served entry %d, want the 1 and 1 the store holds",
			c.GivenEntry, c.ServedEntry)
	}
}

// A task of an entry no turn confirmed never reached the worker, so its retirement puts
// GivenEntry back to ServedEntry: the role is owed that entry's work again, which is what the
// admission pass reads off the difference between the entry its phases row names
// (record.PhaseRow.Entry) and the entry its claim was given. The start that carries that entry's
// work then queues a task where the duplicate rule declined one before.
func TestAnUnconfirmedRetirementMakesTheGivenEntryOwedAgain(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
		Phase: phase.Implementing, Generation: 7, Entry: 1})
	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})

	// Entry 2's task is queued and the daemon suspends the claim before a turn takes it. That
	// suspension ends the claim's phase, so settle retires the task unconfirmed.
	h.must(RequestDeliver{Claim: testToken, Task: "test it", ID: "outbox:41", Row: 41,
		Phase: phase.Testing, Generation: 7, Entry: 2})
	if c := h.claim(); c.GivenEntry != 2 {
		t.Fatalf("the claim was given entry %d, want the queued 2", c.GivenEntry)
	}
	h.must(RequestSuspend{Claim: testToken, Reason: "the tree of LEGION-208 lingers", By: SuspendedByDaemon})

	if p := h.claim().Pending; p != nil {
		t.Fatalf("the suspension left %+v pending, want entry 2's task retired with the phase it was queued for", p)
	}
	if c := h.claim(); c.GivenEntry != 1 || c.ServedEntry != 1 {
		t.Fatalf("after the retirement the claim was given entry %d and served entry %d, want both 1: a task no worker confirmed is owed again",
			c.GivenEntry, c.ServedEntry)
	}
	if stored := h.store.load(testToken); stored.GivenEntry != 1 {
		t.Fatalf("the stored claim was given entry %d, want the 1 the retirement put it back to", stored.GivenEntry)
	}

	h.must(RequestDeliver{Claim: testToken, Task: "test it", ID: "outbox:42", Row: 42,
		Phase: phase.Testing, Generation: 7, Entry: 2})

	p := h.claim().Pending
	if p == nil || p.Entry != 2 || p.ID != "outbox:42" {
		t.Fatalf("the second start of entry 2 left %+v pending, want its task queued: the entry is owed again", p)
	}
	if c := h.claim(); c.GivenEntry != 2 {
		t.Fatalf("the claim was given entry %d after the second start, want 2", c.GivenEntry)
	}
}

// The retirement puts the number back only for the entry the claim stands on. A task of an older
// entry retired unconfirmed — one a daemon that restarted read back beneath a newer start's
// number — lowers nothing: the entry the claim was last given has been given, and owing it again
// would send its work a second time.
func TestAnUnconfirmedRetirementBelowTheGivenEntryLowersNothing(t *testing.T) {
	restored := fixture(StateReady)
	restored.GivenEntry, restored.ServedEntry = 3, 1
	restored.Pending.Phase, restored.Pending.Entry = phase.Implementing, 2
	restored.Pending.Generation = restored.Generation
	h := newHarnessOf(t, restored)
	if err := h.store.PutDelivery(h.ctx, testToken, *restored.Pending); err != nil {
		t.Fatalf("seed the restored delivery: %v", err)
	}

	h.must(RequestSuspend{Claim: testToken, Reason: "the tree of LEGION-208 lingers", By: SuspendedByDaemon})

	if p := h.claim().Pending; p != nil {
		t.Fatalf("the suspension left %+v pending, want the phase's task retired", p)
	}
	if c := h.claim(); c.GivenEntry != 3 || c.ServedEntry != 1 {
		t.Fatalf("after the retirement of entry 2's task the claim was given entry %d and served entry %d, want 3 and 1: the retirement of an older task lowers nothing",
			c.GivenEntry, c.ServedEntry)
	}
	if stored := h.store.load(testToken); stored.GivenEntry != 3 {
		t.Fatalf("the stored claim was given entry %d, want the 3 it stood on", stored.GivenEntry)
	}
}

// A delivery that carries no entry — here the resume of a cancelled idle release, which hands
// the role no new work — leaves both numbers where they are, queued and retired alike.
func TestADeliveryOfNoEntryLeavesTheGivenEntryAndTheServedEntryAlone(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
		Phase: phase.Implementing, Generation: 7, Entry: 1})
	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})

	h.must(RequestDeliver{Claim: testToken, Task: "Continue. Reason: the pod came back.", ID: "outbox:41", Row: 41,
		Phase: phase.Implementing, Generation: 8})

	if c := h.claim(); c.GivenEntry != 1 || c.ServedEntry != 1 {
		t.Fatalf("the queued resume left the claim given entry %d and served entry %d, want both 1: it carries no entry",
			c.GivenEntry, c.ServedEntry)
	}

	h.must(RequestSuspend{Claim: testToken, Reason: "the tree of LEGION-208 lingers", By: SuspendedByDaemon})

	if p := h.claim().Pending; p != nil {
		t.Fatalf("the suspension left %+v pending, want the resume's task retired with the phase it was queued for", p)
	}
	if c := h.claim(); c.GivenEntry != 1 || c.ServedEntry != 1 {
		t.Fatalf("the retired resume left the claim given entry %d and served entry %d, want both 1: a delivery of no entry moves neither",
			c.GivenEntry, c.ServedEntry)
	}
	if stored := h.store.load(testToken); stored.GivenEntry != 1 || stored.ServedEntry != 1 {
		t.Fatalf("the stored claim was given entry %d and served entry %d, want both 1", stored.GivenEntry, stored.ServedEntry)
	}
}

// The row of an entry the claim has taken and finished is run again — a failure after its
// delivery, or a crash before the row finished — and queues nothing: that entry's work was given
// once.
func TestARowRunAgainAfterTheGivenEntryWasRetiredQueuesNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	start := RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
		Phase: phase.Implementing, Generation: 7, Entry: 1}
	h.must(start)
	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	prompts := len(h.prompts())

	h.must(start)

	if p := h.claim().Pending; p != nil {
		t.Fatalf("the row run again queued %+v, want nothing: the claim was given entry 1 already", p)
	}
	if _, ok := h.store.delivery(testToken); ok {
		t.Error("the row run again wrote a delivery over the retired one")
	}
	h.wantPrompts(prompts)
	if c := h.claim(); c.GivenEntry != 1 || c.ServedEntry != 1 {
		t.Fatalf("after the row ran again the claim was given entry %d and served entry %d, want both 1",
			c.GivenEntry, c.ServedEntry)
	}
}

// A second row of the same entry — one written while the first was still leased — queues nothing
// over the task the first row gave, and records no row of its own: the entry decides, so the row
// the claim never queued a task from is declined all the same.
func TestASecondRowOfTheGivenEntryQueuesNothingWhileTheFirstIsLeased(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
		Phase: phase.Implementing, Generation: 7, Entry: 1})
	first := h.pending()

	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:41", Row: 41,
		Phase: phase.Implementing, Generation: 7, Entry: 1})

	if p := h.pending(); p.ID != first.ID {
		t.Fatalf("the claim holds delivery %s, want the first row's %s: one entry reaches one claim once", p.ID, first.ID)
	}
	h.wantPrompts(1)
	if rows := h.claim().DeliveredRows; !slices.Equal(rows, []int64{40}) {
		t.Fatalf("the claim records rows %v, want only the 40 it queued a task from", rows)
	}
	if c := h.claim(); c.GivenEntry != 1 {
		t.Fatalf("the claim was given entry %d, want 1", c.GivenEntry)
	}
}

// A start that hands the role the work of the entry its phases row already names says so
// (RequestDeliver.ResendEntry, the held phase's retry), and the claim takes its task though it
// was given that entry: the retry carries a task of its own that names the retry's reason, and
// a claim whose worker confirmed the entry's last task and whose turn then ended holds that
// entry with no delivery pending, so the entry test alone would relaunch the worker with no
// prompt. The same request without the mark is the control, and queues nothing.
func TestAResendOfTheGivenEntryQueuesItsTaskWhereTheEntryAloneDeclinesIt(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
		Phase: phase.Implementing, Generation: 7, Entry: 1})
	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	if c := h.claim(); c.GivenEntry != 1 || c.ServedEntry != 1 || c.Pending != nil {
		t.Fatalf("the claim was given entry %d, served entry %d and holds %+v, want 1, 1 and no task: the fixture's own premise",
			c.GivenEntry, c.ServedEntry, c.Pending)
	}

	h.must(RequestDeliver{Claim: testToken, Task: "the architect's retry", ID: "outbox:41", Row: 41,
		Phase: phase.Implementing, Generation: 7, Entry: 1})

	if p := h.claim().Pending; p != nil {
		t.Fatalf("a request of entry 1 carrying no resend queued %+v, want nothing: that entry's work was given once", p)
	}
	h.wantPrompts(1)

	retry := RequestDeliver{Claim: testToken, Task: "the architect's retry", ID: "outbox:42", Row: 42,
		Phase: phase.Implementing, Generation: 7, Entry: 1, ResendEntry: true}
	h.must(retry)

	p := h.pending()
	if p.ID != "outbox:42" || p.Task != "the architect's retry" || p.Entry != 1 {
		t.Fatalf("the resend queued %+v, want the retry's own task of entry 1: the claim takes it though it was given that entry", p)
	}
	h.wantPrompts(2)
	if rows := h.claim().DeliveredRows; !slices.Equal(rows, []int64{40, 42}) {
		t.Fatalf("the claim records rows %v, want 40 and 42: the resend is recorded by its row, which is what declines its retry", rows)
	}

	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	prompts := len(h.prompts())

	h.must(retry)

	if p := h.claim().Pending; p != nil {
		t.Fatalf("the resend's own row run again queued %+v, want nothing: the row-id guard still holds", p)
	}
	h.wantPrompts(prompts)
}

// EntryGiven is what records an entry whose task the outbox held back because the claim already
// holds that entry's work. It raises the number and persists it, and a call naming an entry the
// claim is already at or above writes nothing.
func TestEntryGivenRaisesTheGivenEntryAndNeverLowersIt(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	writes := len(h.store.history())

	if err := h.m.EntryGiven(h.ctx, 4); err != nil {
		t.Fatalf("record entry 4 as given: %v", err)
	}
	if c := h.claim(); c.GivenEntry != 4 {
		t.Fatalf("the claim was given entry %d, want the 4 recorded", c.GivenEntry)
	}
	if stored := h.store.load(testToken); stored.GivenEntry != 4 {
		t.Fatalf("the stored claim was given entry %d, want 4: the record is the claims row's", stored.GivenEntry)
	}
	written := len(h.store.history())
	if written != writes+1 {
		t.Fatalf("the claim was written %d times, want one write for the entry", written-writes)
	}

	for _, entry := range []int64{4, 3} {
		if err := h.m.EntryGiven(h.ctx, entry); err != nil {
			t.Fatalf("record entry %d as given: %v", entry, err)
		}
		if c := h.claim(); c.GivenEntry != 4 {
			t.Fatalf("after entry %d the claim was given entry %d, want the 4 it stood on: the number only rises",
				entry, c.GivenEntry)
		}
	}
	if got := len(h.store.history()); got != written {
		t.Fatalf("the claim was written %d more times for entries it already holds, want none", got-written)
	}
}
