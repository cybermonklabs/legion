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
)

// appendOutOfOrder appends two events through broker so they commit out of id order: tx A
// appends on issue first (the lower id, low) and stays open, then tx B appends on issue second
// (high) and commits. commitLow commits A; until it runs, low is an id below a committed one that
// is not yet visible. A that the test never commits rolls back when the test ends.
func appendOutOfOrder(t *testing.T, database *store.Store, broker *events.Broker, first, second string) (low, high int64, commitLow func()) {
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
	return lowEvent.ID, highEvent.ID, func() {
		t.Helper()
		if err := open.Commit(ctx); err != nil {
			t.Fatalf("commit the lower id: %v", err)
		}
	}
}

// holdClusterTransactionID opens a transaction in the test cluster's postgres database and takes
// a transaction id, as a job in another database or another package's test does. It holds no
// event id of this database, so it holds no horizon back (events.Horizon): a row that needs the
// horizon held holds it with holdEventWrite. It commits when release runs or after hold, whichever
// comes first, and by the time the test ends; release returns once it has.
func holdClusterTransactionID(t *testing.T, hold time.Duration) (release func()) {
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
	released := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(hold)
		defer timer.Stop()
		select {
		case <-released:
		case <-timer.C:
		}
		_ = tx.Commit(context.Background())
		_ = conn.Close(context.Background())
	}()
	var once sync.Once
	release = func() {
		once.Do(func() { close(released) })
		<-done
	}
	t.Cleanup(release)
	return release
}

// holdEventWrite opens an event write of Dispatch's own database and holds it: a transaction that
// takes its transaction id and inserts an ownerless event, as events.Broker.Append does, so the
// event's id is open and no horizon passes it (events.Horizon) until the write ends. It rolls back
// when release runs or after hold, whichever comes first, and by the time the test ends, so the
// id it drew stays a gap, and its end sends no notification; release returns once it has.
func holdEventWrite(t *testing.T, database *store.Store, hold time.Duration) (release func()) {
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
	released := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(hold)
		defer timer.Stop()
		select {
		case <-released:
		case <-timer.C:
		}
		_ = tx.Rollback(context.Background())
	}()
	var once sync.Once
	release = func() {
		once.Do(func() { close(released) })
		<-done
	}
	t.Cleanup(release)
	return release
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
