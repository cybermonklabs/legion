package api

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// appendOutOfOrder appends two events through broker so they commit out of id order: tx A
// appends on issue first (the lower id, low) and stays open, then tx B appends on issue second
// (high) and commits. commitLow commits A and rollbackLow rolls it back, which notifies nothing
// and leaves low a gap for good; until one of them runs, low is an id below a committed one that
// is not yet visible. A that the test ends neither way is rolled back when the test ends.
func appendOutOfOrder(
	t *testing.T, database *store.Store, broker *events.Broker, first, second string,
) (low, high int64, commitLow, rollbackLow func()) {
	t.Helper()
	ctx := t.Context()
	open, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lower id's transaction: %v", err)
	}
	t.Cleanup(func() { _ = open.Rollback(context.Background()) })
	lowEvent, err := broker.Append(ctx, open, model.Event{
		IssueKey: new(first),
		Type:     "test.concurrent",
		Actor:    model.Actor{Kind: "session", ID: "seed"},
		Payload:  map[string]any{},
	})
	if err != nil {
		t.Fatalf("append the lower id: %v", err)
	}
	committed, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the higher id's transaction: %v", err)
	}
	defer func() { _ = committed.Rollback(context.Background()) }()
	highEvent, err := broker.Append(ctx, committed, model.Event{
		IssueKey: new(second),
		Type:     "test.concurrent",
		Actor:    model.Actor{Kind: "session", ID: "seed"},
		Payload:  map[string]any{},
	})
	if err != nil {
		t.Fatalf("append the higher id while the lower one is open: %v", err)
	}
	if err := committed.Commit(ctx); err != nil {
		t.Fatalf("commit the higher id: %v", err)
	}
	if highEvent.ID <= lowEvent.ID {
		t.Fatalf("event ids %d (open) and %d (committed) are not out of commit order, so this test proves nothing", lowEvent.ID, highEvent.ID)
	}
	commitLow = func() {
		t.Helper()
		if err := open.Commit(ctx); err != nil {
			t.Fatalf("commit the lower id: %v", err)
		}
	}
	rollbackLow = func() {
		t.Helper()
		if err := open.Rollback(ctx); err != nil {
			t.Fatalf("roll back the lower id: %v", err)
		}
	}
	return lowEvent.ID, highEvent.ID, commitLow, rollbackLow
}

// holdClusterTransactionID opens a transaction in the test cluster's postgres database and takes
// a transaction id, as a job in another database or another package's test does. It holds no
// event id of this database, so it holds no horizon back (events.Horizon): a row that needs the
// horizon held holds it with holdEventWrite. It commits when release runs, at the test's first
// failure, or when the test ends, whichever comes first, and at no other moment (endHold).
func holdClusterTransactionID(t *testing.T) (release func()) {
	t.Helper()
	config, err := pgx.ParseConfig(os.Getenv(storetest.DatabaseURLEnv))
	if err != nil {
		t.Fatalf("parse %s: %v", storetest.DatabaseURLEnv, err)
	}
	config.Database = "postgres"
	conn, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("connect to the cluster's postgres database: %v", err)
	}
	tx, err := conn.Begin(t.Context())
	if err != nil {
		_ = conn.Close(context.Background())
		t.Fatalf("begin the transaction in the cluster's postgres database: %v", err)
	}
	var xid string
	if err := tx.QueryRow(t.Context(), `select pg_current_xact_id()::text`).Scan(&xid); err != nil {
		_ = conn.Close(context.Background())
		t.Fatalf("take a transaction id: %v", err)
	}
	return endHold(t, func() {
		_ = tx.Commit(context.Background())
		_ = conn.Close(context.Background())
	})
}

// endHold answers the release of a hold that end ends, and runs end when release runs, at t's
// first failure, or when t ends, whichever comes first, and at no other moment: a hold that also
// ended on a clock of its own let a loaded machine's test see the horizon proven before the check
// the hold was for. The first failure is read off t.Failed every testwait.Poll, and the test's end
// off t.Context, which ends before any cleanup runs, so a wait that gives up lets the hold go
// before anything else of its test is undone. A row whose wait hangs without failing holds it until
// that wait gives up, at the test binary's deadline less testwait's margin: no failure is known
// before then, and no release fits that ends the hold sooner without a clock of its own. release
// returns once end has run.
func endHold(t *testing.T, end func()) (release func()) {
	var once sync.Once
	released := make(chan struct{})
	release = func() {
		once.Do(func() {
			close(released)
			end()
		})
	}
	go func() {
		poll := time.NewTicker(testwait.Poll)
		defer poll.Stop()
		for {
			select {
			case <-released:
				return
			case <-t.Context().Done():
				release()
				return
			case <-poll.C:
				if t.Failed() {
					release()
					return
				}
			}
		}
	}()
	t.Cleanup(release)
	return release
}

// holdEventWrite opens an event write of Dispatch's own database and holds it: a transaction that
// takes its transaction id and inserts an ownerless event, as events.Broker.Append does, so the
// event's id is open and no horizon passes it (events.Horizon) until the write ends. It rolls back
// when release runs, at the test's first failure, or when the test ends, whichever comes first,
// and at no other moment (endHold), so the id it drew stays a gap, and its end sends no
// notification; release returns once it has.
func holdEventWrite(t *testing.T, database *store.Store) (release func()) {
	t.Helper()
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the event write that holds the horizon: %v", err)
	}
	for _, statement := range []string{
		`select pg_current_xact_id()`,
		`insert into events (seq, type, actor, payload, notify) values (0, 'test.held', '{"kind":"system","id":"test"}', '{}', false)`,
	} {
		if _, err := tx.Exec(t.Context(), statement); err != nil {
			_ = tx.Rollback(context.Background())
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return endHold(t, func() { _ = tx.Rollback(context.Background()) })
}

// holdTransactionIDWithoutAnEvent opens a transaction in Dispatch's own database that takes a
// transaction id and writes a table other than events, as a write that appends no event does, and
// holds it open until the test ends.
func holdTransactionIDWithoutAnEvent(t *testing.T, database *store.Store) {
	t.Helper()
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the transaction that writes no event: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	for _, statement := range []string{
		`select pg_current_xact_id()`,
		`create temporary table no_event (id int) on commit drop`,
		`insert into no_event values (1)`,
	} {
		if _, err := tx.Exec(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}
