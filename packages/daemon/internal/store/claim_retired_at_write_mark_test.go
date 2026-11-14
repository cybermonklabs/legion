package store

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// retiredAtWriteMark reads the mark putClaim stamps a claim with when it records a retirement:
// the number outbox_write_mark_seq handed that write.
func retiredAtWriteMark(t *testing.T, store *Store, token claim.Token) int64 {
	t.Helper()
	var mark int64
	if err := store.Pool().QueryRow(context.Background(),
		`select retired_at_write_mark from claims where token = $1`, string(token)).Scan(&mark); err != nil {
		t.Fatalf("read the retirement mark of %s: %v", token, err)
	}
	return mark
}

// enqueueOutboxRow writes one outbox row and answers the write mark it took, which is what the
// claims of this test are ordered against: a row takes its number from outbox_write_mark_seq
// through the column's own default, a retirement takes one from the same sequence, and the
// sequence hands its numbers out in the order the statements ask for them.
func enqueueOutboxRow(t *testing.T, store *Store, issue string) int64 {
	t.Helper()
	var mark int64
	if err := store.Pool().QueryRow(context.Background(),
		`insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('supervise', $1, '{"op":"start","tree":"LEGION-208","role":"implementer","generation":4}', 0, now(), '')
		returning write_mark`, issue).Scan(&mark); err != nil {
		t.Fatalf("enqueue an outbox row of %s: %v", issue, err)
	}
	return mark
}

// The mark a stop is ordered against is taken by the write that records the retirement, and by no
// other write of that claim. It is what tells an owed start row written before the stop from one
// the workflow wrote after it (admit's stopOutlivedStart), so a mark taken early would end a row
// the workflow still means to run and one taken late would start a stopped worker.
//
// Every read here is of the order one sequence puts those writes in and never of a number itself:
// what the mark says is which write came first (store's putClaim, for what that order covers).
//
// The five reads are the whole of that rule: a claim written in a live state carries no mark at
// all, the write that records it retired takes a number above every outbox write made before it,
// a row enqueued after that write takes one above the mark — which is what makes it a row written
// after the stop — a second write of the already-retired claim leaves the mark where the stop put
// it, so a later state write of a stopped worker never moves the stop's own moment, and a claim
// whose first write is already retired is marked by that insert, the other half of the one upsert
// that records a state.
func TestTheRetirementMarkIsTakenWhenTheRetirementIsWritten(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	c.State = supervise.StateWorking
	before := enqueueOutboxRow(t, store, c.Issue)

	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the working claim: %v", err)
	}
	if mark := retiredAtWriteMark(t, store, c.Token); mark != 0 {
		t.Fatalf("the working claim is marked at write %d, want 0: a claim that has not retired has no stop to order anything against", mark)
	}

	atTheStop := enqueueOutboxRow(t, store, c.Issue)
	if atTheStop <= before {
		t.Fatalf("the row written last is marked at write %d, want one above %d: the sequence hands its numbers out in the order the statements ask for them",
			atTheStop, before)
	}
	c.State = supervise.StateRetired
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the retired claim: %v", err)
	}
	mark := retiredAtWriteMark(t, store, c.Token)
	if mark <= atTheStop {
		t.Fatalf("the stop is marked at write %d, want one above %d, the newest write before it: every outbox write made before the retirement took its number before the retirement's own (row %d among them)",
			mark, atTheStop, before)
	}

	after := enqueueOutboxRow(t, store, c.Issue)
	if after <= mark {
		t.Fatalf("a row enqueued after the stop is marked at write %d, want one above the mark %d: both come from one sequence, which is what makes the mark a split of the outbox at the stop",
			after, mark)
	}

	// The writes that follow a stop — the machine persisting its retired claim again — leave the
	// stop's own moment alone, or every one of them would move the split past the rows the
	// workflow wrote after the stop.
	c.UncertainStreak++
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the retired claim a second time: %v", err)
	}
	if again := retiredAtWriteMark(t, store, c.Token); again != mark {
		t.Fatalf("the second write of the retired claim moved the mark from write %d to write %d, want it unmoved: the stop happened once",
			mark, again)
	}

	// A claim whose first write is already retired — a fixture, a migration's historical row —
	// is marked by that insert, from the same sequence: the mark goes with the state, whichever
	// half of the upsert writes it.
	born := tmuxClaim("legion-LEGION-209-tester")
	born.Role = claim.RoleTester
	born.State = supervise.StateRetired
	if err := store.PutClaim(ctx, born); err != nil {
		t.Fatalf("put a claim that is retired when it is first written: %v", err)
	}
	if got := retiredAtWriteMark(t, store, born.Token); got <= after {
		t.Fatalf("a claim inserted retired is marked at write %d, want one above %d, the newest write before it", got, after)
	}
}
