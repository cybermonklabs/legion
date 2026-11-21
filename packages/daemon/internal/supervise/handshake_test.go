package supervise

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
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
// entry, its run, when its task was first queued and when a take of it was first accepted. The
// task itself is finished by the report of its take's turn, and the handshake outlasts it: through
// that finish, through a daemon restart that reads the claim back from the store, and through a
// task of no entry, which is no part of it. A later entry's task replaces it at that task's queued
// time, a retirement of that task untaken leaves it, and the sweep's second task of the same
// entry keeps the moment the first was queued (dispatch://INFRA-2614, k5 of the Verification
// document at version 10).
func TestAClaimsEntryHandshakeOutlastsItsTaskAndMovesOnlyWithALaterEntry(t *testing.T) {
	h := newHarness(t)
	h.manualTakes()
	h.reach(StateReady)

	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
		Phase: phase.Implementing, Generation: 7, Entry: 1})
	firstQueued := h.claim().Pending.QueuedAt
	h.wantHandshake("once entry 1's task is queued,", EntryHandshake{Entry: 1, Generation: 7, QueuedAt: firstQueued})

	h.advance(time.Second)
	h.mustTake()
	confirmed := h.claim().Pending.ConfirmedAt
	if !confirmed.After(firstQueued) {
		t.Fatalf("the take of entry 1's task was accepted at %s, want a moment after its queueing at %s: this test proves nothing", confirmed, firstQueued)
	}
	taken := EntryHandshake{Entry: 1, Generation: 7, QueuedAt: firstQueued, ConfirmedAt: confirmed}
	h.wantHandshake("once a take of entry 1's task was accepted,", taken)

	h.must(StreamTurnStart{Claim: testToken})
	h.wantHandshake("once that take's turn began,", taken)
	h.must(StreamTurnEnd{Claim: testToken})
	if p := h.claim().Pending; p != nil {
		t.Fatalf("the claim holds %+v after the turn its task ran, want the task finished", p)
	}
	h.wantHandshake("after the turn ended and its task was finished,", taken)

	h.restart()
	h.wantHandshake("after a restart read the claim back,", taken)

	// A task of no entry — a wake's, a resume's — is queued and taken, and leaves the handshake
	// of entry 1 as it was.
	h.connect()
	h.advance(time.Second)
	h.must(RequestDeliver{Claim: testToken, Task: "a person answered your ask", ID: "outbox:41", Row: 41, Generation: 7})
	h.mustTake()
	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantHandshake("after a take of a task of no entry was accepted,", taken)

	// Entry 2's task replaces it, at its own queued moment and with no take accepted.
	h.advance(time.Second)
	h.must(RequestDeliver{Claim: testToken, Task: "test it", ID: "outbox:42", Row: 42,
		Phase: phase.Testing, Generation: 7, Entry: 2})
	secondQueued := h.claim().Pending.QueuedAt
	later := EntryHandshake{Entry: 2, Generation: 7, QueuedAt: secondQueued}
	h.wantHandshake("once entry 2's task is queued,", later)

	// The daemon suspends the claim before a take of it, which retires the task untaken and puts
	// the given entry back, so the entry is owed again. The handshake stays.
	h.must(RequestSuspend{Claim: testToken, Reason: "the tree of LEGION-208 lingers", By: SuspendedByDaemon})
	if c := h.claim(); c.Pending != nil || c.GivenEntry != 1 {
		t.Fatalf("after the suspension the claim holds %+v and was given entry %d, want no task and entry 1: the sweep sends entry 2 again only where it is owed",
			c.Pending, c.GivenEntry)
	}
	h.wantHandshake("after entry 2's task was retired untaken,", later)

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
// the task the claim holds: its queued moment and the moment a take of it was accepted.
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

// takenEntryOne brings the claim to an accepted take of its task of entry 1, run 7, a second after
// that task was queued, with the take's turn still to come, and answers when the task was queued
// and when the take was accepted. The worker takes nothing more on its own (manualTakes).
func takenEntryOne(h *harness) (queued, confirmed time.Time) {
	h.t.Helper()
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:40", Row: 40,
		Phase: phase.Implementing, Generation: 7, Entry: 1})
	queued = h.claim().Pending.QueuedAt
	h.advance(time.Second)
	h.mustTake()
	confirmed = h.claim().Pending.ConfirmedAt
	h.wantHandshake("once a take of entry 1's task was accepted,",
		EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued, ConfirmedAt: confirmed})
	return queued, confirmed
}

// A take the daemon gives back before its turn started was never run: the worker took the task and
// no turn ran its text. The second wait that finds no turn of the take, and a compaction that
// aborts the turn the take was to start, each take the task back (takeBack), and the handshake
// gives that confirmation back with it (giveBackTake), in memory and in the store, so the state's
// `entryConfirmedAt` does not say the worker took an entry it never ran. The task's next accepted
// take is then the one recorded. A provider error is different: its turn started on the take's
// text, so the handshake keeps the confirmation across that take-back. A death is
// TestADeathTakesTheHandshakesConfirmationBackOnlyWhileTheTakeAwaitsItsTurn's.
func TestATakenBackConfirmationIsTakenFromTheHandshake(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  []func(*Deps)
		// takeBack takes the accepted take back before its turn started, and has the task's value
		// written again for the worker to take.
		takeBack func(h *harness)
	}{
		{name: "the second wait finds no turn of the take", set: []func(*Deps){withWaits},
			takeBack: func(h *harness) {
				h.advance(turnStartBound)
				h.wantCharges(WaitTurnStart)
			}},
		{name: "a compaction aborts the turn the take was to start",
			takeBack: func(h *harness) {
				release, compacted := h.compactHeld()
				if p := h.claim().Pending; p == nil || p.taken() {
					h.t.Fatalf("the claim holds %+v while the compaction runs, want the task taken back", p)
				}
				release()
				if err := <-compacted; err != nil {
					h.t.Fatalf("the compaction answered %v, want it over", err)
				}
				h.m.Wait()
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessOf(t, queuedClaim(), tc.set...)
			queued, first := takenEntryOne(h)

			tc.takeBack(h)

			if p := h.claim().Pending; p == nil || p.taken() {
				t.Fatalf("after the take-back the claim holds %+v, want the task back and untaken: this test proves nothing", p)
			}
			unconfirmed := EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued}
			h.wantHandshake("after the take-back gave the confirmation back,", unconfirmed)
			h.restart()
			h.wantHandshake("after a restart read the claim back,", unconfirmed)

			h.connect()
			h.advance(time.Second)
			h.mustTake()
			confirmed := h.claim().Pending.ConfirmedAt
			if !confirmed.After(first) {
				t.Fatalf("the take after the take-back was accepted at %s, want after the one given back at %s: this test proves nothing", confirmed, first)
			}
			h.wantHandshake("after the next take of the task was accepted,",
				EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued, ConfirmedAt: confirmed})
		})
	}

	t.Run("a provider error after the take's turn started", func(t *testing.T) {
		h := newHarness(t)
		queued, confirmed := takenEntryOne(h)
		h.must(StreamTurnStart{Claim: testToken})

		h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})

		if p := h.claim().Pending; p == nil || p.taken() {
			t.Fatalf("after the provider error the claim holds %+v, want the task taken back: this test proves nothing", p)
		}
		kept := EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued, ConfirmedAt: confirmed}
		h.wantHandshake("after the provider error took the task back,", kept)
		h.restart()
		h.wantHandshake("after a restart read the claim back,", kept)
	})
}

// A death takes back the task whose take it finds (interrupted), for the relaunched worker to take
// again, and the handshake keeps that take's confirmation only where the take's turn started on
// its text. A take that awaits its turn was never run: the worker took the entry's task and the
// process died before any turn of it, so the handshake gives that confirmation back, in memory and
// in the store, and the state's `entryConfirmedAt` does not say the worker ran an entry it never
// ran. A take whose turn started is one a turn of the task did run, and the handshake keeps it
// across the relaunch, while the task goes back marked interrupted.
func TestADeathTakesTheHandshakesConfirmationBackOnlyWhileTheTakeAwaitsItsTurn(t *testing.T) {
	t.Run("with the take awaiting its turn", func(t *testing.T) {
		h := newHarness(t)
		queued, _ := takenEntryOne(h)

		h.observe(runtime.Gone)

		if p := h.claim().Pending; p == nil || p.taken() || p.Interrupted {
			t.Fatalf("after the death the claim holds %+v, want the task taken back with no note: no turn of it ran", p)
		}
		unconfirmed := EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued}
		h.wantHandshake("after the death took back the take awaiting its turn,", unconfirmed)
		h.restart()
		h.wantHandshake("after a restart read the claim back,", unconfirmed)
	})

	t.Run("once the take's turn started", func(t *testing.T) {
		h := newHarness(t)
		queued, confirmed := takenEntryOne(h)
		h.must(StreamTurnStart{Claim: testToken})

		h.observe(runtime.Gone)

		if p := h.claim().Pending; p == nil || p.taken() || !p.Interrupted {
			t.Fatalf("after the death the claim holds %+v, want the task taken back from its turn: this test proves nothing", p)
		}
		kept := EntryHandshake{Entry: 1, Generation: 7, QueuedAt: queued, ConfirmedAt: confirmed}
		h.wantHandshake("after the death took the task back from its take's turn,", kept)
		h.restart()
		h.wantHandshake("after a restart read the claim back,", kept)
	})
}
