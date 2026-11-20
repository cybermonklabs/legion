package dutytest_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/duty/dutytest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// within bounds each wait of these rows for a write the test itself makes.
const within = 30 * time.Second

// write writes the hold name's row naming holder, as a take or a renewal does.
func write(t *testing.T, db dutytest.Database, holder uuid.UUID) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `
		insert into duty_holds (name, holder, token, expires_at) values ('test-hold', $1, '', now() + interval '5 s')
		on conflict (name) do update set holder = excluded.holder, expires_at = excluded.expires_at`, holder); err != nil {
		t.Fatalf("write the hold's row: %v", err)
	}
}

// A wait for a renewal answers only a write committed after the wait began: a write committed
// before it, whose notification the recorder may read later, is not one. The row is written once
// before the wait and once after it begins, and the wait answers the second.
func TestAWaitForARenewalIsNotAnsweredByAWriteCommittedBeforeIt(t *testing.T) {
	database := storetest.Open(t)
	writes := dutytest.Record(t, database.Pool, database.Pool.Listener, "test-hold")
	holder := uuid.New()
	write(t, database.Pool, holder)
	answered := make(chan dutytest.Write, 1)
	go func() { answered <- writes.WaitForRenewal(t, holder, within) }()
	time.Sleep(200 * time.Millisecond)
	write(t, database.Pool, holder)
	got := <-answered
	all := writes.All()
	if len(all) != 2 || got.Seq != all[1].Seq {
		t.Fatalf("the wait answered write %d of %+v, want the second, the one committed after the wait began", got.Seq, all)
	}
	if got.Seen.Before(got.Changed) {
		t.Errorf("the write was seen committed at %s, before its row changed at %s", got.Seen, got.Changed)
	}
}

// waitForWrites returns once the recorder has read n writes, within within. A wait for a renewal
// (WaitForRenewal) cannot stand in for it: a write committed before such a wait never answers it.
func waitForWrites(t *testing.T, writes *dutytest.Writes, n int) {
	t.Helper()
	for deadline := time.Now().Add(within); len(writes.All()) < n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the recorder read %d of the %d writes made, within %v", len(writes.All()), n, within)
		}
	}
}

// A take over is read once, however often its new holder renews after it: of the writes since
// the earlier holder's last renewal, only those that change the row's holder are takes. A second
// real take, by a third holder, is a second change. Each count is read once the recorder has read
// every write made, so a renewal it read late is counted all the same.
func TestATakeOverCountsTheWritesThatChangeTheHolderAlone(t *testing.T) {
	database := storetest.Open(t)
	writes := dutytest.Record(t, database.Pool, database.Pool.Listener, "test-hold")
	earlier, taker, third := uuid.New(), uuid.New(), uuid.New()
	write(t, database.Pool, earlier)
	signaledBy := writes.Now(t)
	write(t, database.Pool, taker)
	for range 3 {
		write(t, database.Pool, taker)
	}
	waitForWrites(t, writes, 5)
	take := writes.TakeOver(t, earlier, signaledBy, within)
	if take.Took.Holder != taker || take.Changes != 1 || take.LateRenewals != 0 {
		t.Fatalf("the take over read %+v, want one change, to %s, and no late renewal", take, taker)
	}
	write(t, database.Pool, third)
	waitForWrites(t, writes, 6)
	if again := writes.TakeOver(t, earlier, signaledBy, within); again.Changes != 2 {
		t.Fatalf("after a third holder's take the take over read %d changes, want 2", again.Changes)
	}
}
