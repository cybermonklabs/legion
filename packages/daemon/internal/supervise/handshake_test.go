package supervise

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// wantHandshake holds the handshake the machine keeps and the one the store holds to want, so a
// handshake memory holds and the claims row does not reads as the failure it is: a restart would
// lose it.
func (h *harness) wantHandshake(when string, want EntryHandshake) {
	h.t.Helper()
	if got := h.claim().Handshake; !sameHandshake(got, want) {
		h.t.Fatalf("%s the claim's handshake is %+v, want %+v", when, got, want)
	}
	if got := h.store.load(testToken).Handshake; !sameHandshake(got, want) {
		h.t.Fatalf("%s the stored claim's handshake is %+v, want %+v: the handshake is the claims row's", when, got, want)
	}
}

// sameHandshake compares two handshakes by their moments rather than by the time values' own
// fields, which a monotonic reading or a location can tell apart for one instant.
func sameHandshake(a, b EntryHandshake) bool {
	return a.Entry == b.Entry && a.Generation == b.Generation &&
		a.QueuedAt.Equal(b.QueuedAt) && a.ConfirmedAt.Equal(b.ConfirmedAt)
}

// The claim keeps a handshake of the last entry it was queued a task of (Claim.Handshake): the
// entry, its run, when its task was first queued and when a turn first confirmed it. The task
// itself is retired when the turn that confirmed it ends, and the handshake outlasts it: through
// that end, through a daemon restart that reads the claim back from the store, and through a task
// of no entry, which is no part of it. A later entry's task replaces it at that task's queued
// time, a retirement of that task unconfirmed leaves it, and the sweep's second task of the same
// entry keeps the moment the first was queued (dispatch://INFRA-2614, k5 of the Verification
// document at version 10).
func TestAClaimsEntryHandshakeOutlastsItsTaskAndMovesOnlyWithALaterEntry(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)

	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
		Phase: phase.Implementing, Generation: 7, Entry: 1})
	firstQueued := h.claim().Pending.QueuedAt
	h.wantHandshake("once entry 1's task is queued,", EntryHandshake{Entry: 1, Generation: 7, QueuedAt: firstQueued})

	h.advance(time.Second)
	h.must(StreamTurnStart{Claim: testToken})
	confirmed := h.claim().Pending.ConfirmedAt
	if !confirmed.After(firstQueued) {
		t.Fatalf("the turn confirmed entry 1's task at %s, want a moment after its queueing at %s: this test proves nothing", confirmed, firstQueued)
	}
	taken := EntryHandshake{Entry: 1, Generation: 7, QueuedAt: firstQueued, ConfirmedAt: confirmed}
	h.wantHandshake("once a turn confirmed entry 1's task,", taken)

	h.must(StreamTurnEnd{Claim: testToken})
	if p := h.claim().Pending; p != nil {
		t.Fatalf("the claim holds %+v after the turn its task ran, want the task retired", p)
	}
	h.wantHandshake("after the turn ended and its task was retired,", taken)

	h.restart()
	h.wantHandshake("after a restart read the claim back,", taken)

	// A task of no entry — a wake's, a resume's — is queued and confirmed, and leaves the
	// handshake of entry 1 as it was.
	h.advance(time.Second)
	h.must(RequestDeliver{Claim: testToken, Task: "a person answered your ask", ID: "outbox:41", Row: 41, Generation: 7})
	h.must(StreamTurnStart{Claim: testToken, DeliveryID: "outbox:41"})
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantHandshake("after a task of no entry was confirmed,", taken)

	// Entry 2's task replaces it, at its own queued moment and with no confirmation.
	h.advance(time.Second)
	h.must(RequestDeliver{Claim: testToken, Task: "test it", ID: "outbox:42", Row: 42,
		Phase: phase.Testing, Generation: 7, Entry: 2})
	secondQueued := h.claim().Pending.QueuedAt
	later := EntryHandshake{Entry: 2, Generation: 7, QueuedAt: secondQueued}
	h.wantHandshake("once entry 2's task is queued,", later)

	// The daemon suspends the claim before a turn takes it, which retires the task unconfirmed and
	// puts the given entry back, so the entry is owed again. The handshake stays.
	h.must(RequestSuspend{Claim: testToken, Reason: "the tree of LEGION-208 lingers", By: SuspendedByDaemon})
	if c := h.claim(); c.Pending != nil || c.GivenEntry != 1 {
		t.Fatalf("after the suspension the claim holds %+v and was given entry %d, want no task and entry 1: the sweep sends entry 2 again only where it is owed",
			c.Pending, c.GivenEntry)
	}
	h.wantHandshake("after entry 2's task was retired unconfirmed,", later)

	// The sweep's second task of entry 2 keeps the moment the first was queued.
	h.advance(time.Second)
	h.must(RequestDeliver{Claim: testToken, Task: "test it", ID: "outbox:43", Row: 43,
		Phase: phase.Testing, Generation: 7, Entry: 2})
	if p := h.claim().Pending; p == nil || p.Entry != 2 || !p.QueuedAt.After(secondQueued) {
		t.Fatalf("the second start of entry 2 left %+v pending, want its task queued after %s: this test proves nothing", p, secondQueued)
	}
	h.wantHandshake("after the sweep sent entry 2's task again,", later)
}

// The outbox records an entry as given where the claim already holds that entry's task and no
// delivery of it recorded the number (EntryGiven). The handshake is written with the number, from
// the task the claim holds: its queued moment and the moment a turn confirmed it.
func TestEntryGivenWritesTheHandshakeOfTheTaskTheClaimHolds(t *testing.T) {
	restored := fixture(StateWorking)
	restored.GivenEntry = 3
	restored.Handshake = EntryHandshake{Entry: 3, Generation: 3, QueuedAt: epoch.Add(-time.Hour), ConfirmedAt: epoch.Add(-time.Hour)}
	restored.Pending.Phase, restored.Pending.Entry, restored.Pending.Generation = phase.Implementing, 4, 3
	restored.Pending.QueuedAt = epoch.Add(-time.Minute)
	h := newHarnessOf(t, restored)

	if err := h.m.EntryGiven(h.ctx, 4); err != nil {
		t.Fatalf("record entry 4 as given: %v", err)
	}
	h.wantHandshake("once entry 4 was recorded as given,",
		EntryHandshake{Entry: 4, Generation: 3, QueuedAt: epoch.Add(-time.Minute), ConfirmedAt: epoch})
}

// confirmedEntryOne brings the claim to a turn that confirmed its task of entry 1, run 7, a second
// after that task was queued, and answers when the task was queued and when it was confirmed.
func confirmedEntryOne(h *harness) (queued, confirmed time.Time) {
	h.t.Helper()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
		Phase: phase.Implementing, Generation: 7, Entry: 1})
	queued = h.claim().Pending.QueuedAt
	h.advance(time.Second)
	h.must(StreamTurnStart{Claim: testToken})
	confirmed = h.claim().Pending.ConfirmedAt
	h.wantHandshake("once a turn confirmed entry 1's task,",
		EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued, ConfirmedAt: confirmed})
	return queued, confirmed
}

// A confirmation the daemon takes back was never the worker's: Oh My Pi's busy answer says the turn
// that confirmed the task was one the agent was already in, so the task goes back unread and that
// turn's end sends it again. The handshake gives that confirmation back too, in memory and in the
// store, so the state's `entryConfirmedAt` does not say the worker took a task it never read. The
// task's real confirmation, by the turn its re-send starts, is then the one recorded. A death in
// the turn is TestADeathTakesTheHandshakesConfirmationBackOnlyWhileItIsInDoubt's.
func TestATakenBackConfirmationIsTakenFromTheHandshake(t *testing.T) {
	t.Run("a busy answer after the turn started", func(t *testing.T) {
		h := newHarness(t)
		queued, _ := confirmedEntryOne(h)

		h.must(StreamPromptResult{Claim: testToken, DeliveryID: h.claim().Pending.ID, Error: "Agent is already processing a prompt"})

		if p := h.claim().Pending; p == nil || !p.ConfirmedAt.IsZero() {
			t.Fatalf("after the busy answer the claim holds %+v, want the task back and unconfirmed: this test proves nothing", p)
		}
		unconfirmed := EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued}
		h.wantHandshake("after the busy answer took the confirmation back,", unconfirmed)
		h.restart()
		h.wantHandshake("after a restart read the claim back,", unconfirmed)
	})

	t.Run("the re-sent task's own turn confirms it", func(t *testing.T) {
		h := newHarness(t)
		queued, first := confirmedEntryOne(h)
		h.must(StreamPromptResult{Claim: testToken, DeliveryID: h.claim().Pending.ID, Error: "Agent is already processing a prompt"})
		// The agent's own turn ends with no result of this prompt behind it, and its end sends
		// the task again.
		h.mustQuiet(StreamTurnEnd{Claim: testToken})
		h.wantPrompts(2)

		h.advance(time.Second)
		h.must(StreamTurnStart{Claim: testToken})

		confirmed := h.claim().Pending.ConfirmedAt
		if !confirmed.After(first) {
			t.Fatalf("the re-sent task was confirmed at %s, want after the confirmation taken back at %s: this test proves nothing", confirmed, first)
		}
		h.wantHandshake("after the re-sent task's turn confirmed it,",
			EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued, ConfirmedAt: confirmed})
	})
}

// A death takes back the task whose turn it cut off, for the relaunch to send again
// (interrupted), and the handshake keeps that turn's confirmation only where nothing leaves it in
// doubt (confirmationInDoubt). A turn that starts while the send of the task's prompt is in flight,
// or while an acknowledged prompt of the task still waits on its result, is taken for the task's,
// but it may be another input's, an Envoy notice's, and the answer that would say so dies with
// the process. So the handshake gives that confirmation back, in memory and in the store, and the
// state's `entryConfirmedAt` does not say the worker took an entry it may never have taken. A
// confirmation whose prompt's result has come is one a turn of the task did take, and the
// handshake keeps it across the relaunch.
func TestADeathTakesTheHandshakesConfirmationBackOnlyWhileItIsInDoubt(t *testing.T) {
	// tookBack holds the claim to the death's take-back: the task waits for the relaunch,
	// interrupted and unconfirmed, whatever the handshake keeps.
	tookBack := func(t *testing.T, h *harness) {
		t.Helper()
		if p := h.claim().Pending; p == nil || !p.Interrupted || !p.ConfirmedAt.IsZero() {
			t.Fatalf("after the death the claim holds %+v, want the task taken back, interrupted and unconfirmed: this test proves nothing", p)
		}
	}

	t.Run("with the send in flight", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		gated := newGatedConn()
		h.conns.Register(testToken, gated)
		// Handle, not must: must waits for the send, which the gate is holding.
		if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
			Phase: phase.Implementing, Generation: 7, Entry: 1}); err != nil {
			t.Fatal(err)
		}
		inFlight := waitFor(t, "the send", gated.entered)
		if err := h.m.Handle(h.ctx, StreamTurnStart{Claim: testToken}); err != nil {
			t.Fatal(err)
		}
		p := h.claim().Pending
		if p == nil || p.ID != inFlight || p.ConfirmedAt.IsZero() {
			t.Fatalf("after the turn began the claim holds %+v, want the delivery %s in flight confirmed by it: this test proves nothing", p, inFlight)
		}
		queued := p.QueuedAt
		h.wantHandshake("once a turn confirmed the task whose send is in flight,",
			EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued, ConfirmedAt: p.ConfirmedAt})

		gone := runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}
		if err := h.m.Handle(h.ctx, RuntimeObservation{Observation: gone}); err != nil {
			t.Fatal(err)
		}

		tookBack(t, h)
		unconfirmed := EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued}
		h.wantHandshake("after the death took the task back with its send in flight,", unconfirmed)
		gated.release <- struct{}{}
		h.m.Wait()
		h.wantHandshake("after the send of the dead process answered,", unconfirmed)
		h.restart()
		h.wantHandshake("after a restart read the claim back,", unconfirmed)
	})

	t.Run("with the acknowledged prompt still waiting on its result", func(t *testing.T) {
		h := newHarness(t)
		queued, _ := confirmedEntryOne(h)
		if id := h.claim().Pending.ID; !slices.Contains(h.outstanding(), id) {
			t.Fatalf("the acknowledged prompt %s waits on no result (outstanding %v), want it to: this test proves nothing", id, h.outstanding())
		}

		h.observe(runtime.Gone)

		tookBack(t, h)
		h.wantHandshake("after the death took the task back before its prompt's result,",
			EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued})
	})

	t.Run("once the prompt's result came", func(t *testing.T) {
		h := newHarness(t)
		queued, confirmed := confirmedEntryOne(h)
		h.must(StreamPromptResult{Claim: testToken, DeliveryID: h.claim().Pending.ID, AgentInvoked: true})
		if p := h.claim().Pending; p == nil || p.ConfirmedAt.IsZero() || len(h.outstanding()) != 0 {
			t.Fatalf("after the prompt's result in its turn the claim holds %+v with prompts %v outstanding, want the task confirmed and none: this test proves nothing",
				p, h.outstanding())
		}

		h.observe(runtime.Gone)

		tookBack(t, h)
		h.wantHandshake("after the death took the task back once its prompt's result had come,",
			EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued, ConfirmedAt: confirmed})
		h.restart()
		h.wantHandshake("after a restart read the claim back,",
			EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued, ConfirmedAt: confirmed})
	})

	// The result can be read before the outcome of the prompt's send, which reaches the machine
	// on the send's own goroutine. The send is still in flight then, and its answer has come all
	// the same (sendUnanswered): the result says a turn ran the prompt, so the death takes nothing
	// of the confirmation back.
	t.Run("once the prompt's result came, its send still in flight", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		gated := newGatedConn()
		h.conns.Register(testToken, gated)
		// Handle, not must: must waits for the send, which the gate is holding.
		if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
			Phase: phase.Implementing, Generation: 7, Entry: 1}); err != nil {
			t.Fatal(err)
		}
		inFlight := testwait.Receive(t, "the send", gated.entered)
		for _, ev := range []Event{
			StreamTurnStart{Claim: testToken},
			StreamPromptResult{Claim: testToken, DeliveryID: inFlight, AgentInvoked: true},
		} {
			if err := h.m.Handle(h.ctx, ev); err != nil {
				t.Fatalf("handle %T: %v", ev, err)
			}
		}
		p := h.claim().Pending
		if p == nil || p.ID != inFlight || p.ConfirmedAt.IsZero() || h.state() != StateWorking {
			t.Fatalf("after the prompt's result in its turn the claim is %s holding %+v, want it working with the delivery %s confirmed: this test proves nothing",
				h.state(), p, inFlight)
		}
		kept := EntryHandshake{Entry: 1, Generation: 7, QueuedAt: p.QueuedAt, ConfirmedAt: p.ConfirmedAt}

		gone := runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}
		if err := h.m.Handle(h.ctx, RuntimeObservation{Observation: gone}); err != nil {
			t.Fatal(err)
		}

		tookBack(t, h)
		h.wantHandshake("after the death took the task back once its prompt's result had come, its send still in flight,", kept)
		gated.release <- struct{}{}
		h.m.Wait()
		h.wantHandshake("after the send of the dead process answered,", kept)
		h.restart()
		h.wantHandshake("after a restart read the claim back,", kept)
	})
}
