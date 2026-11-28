package workflowstore

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A write of a plan signals its issue as a write of a run does (runs_signal_test.go), on a channel
// of its own: PutPlan sends the issue's key on the channel below inside the write's own
// transaction, so Postgres delivers it at the commit, and a write that appends nothing sends
// nothing. The Legion facts consumer computes the issue's stored row again on it.

// plansSignalChannel is the channel a plan's write signals its issue on, by the name the Legion
// facts consumer listens on, so the rows hold the wire's name whatever the package's constant is
// called.
const plansSignalChannel = "dispatch_workflow_plans"

// listenForPlans opens a connection of its own to pool's database, outside the pool, listening on
// the plans' channel, as the listener hub of a Dispatch process does.
func listenForPlans(t *testing.T, pool *store.Pool) *pgx.Conn {
	t.Helper()
	conn, err := pgx.ConnectConfig(t.Context(), pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect a listener: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(t.Context(), "listen "+plansSignalChannel); err != nil {
		t.Fatalf("listen %s: %v", plansSignalChannel, err)
	}
	return conn
}

// requirePlansSignals sends a mark on the plans' channel in a transaction of its own, and requires
// conn to receive the payloads of want, in order, and then the mark, and nothing between them.
// Postgres delivers the notifications of different transactions in the order the transactions
// committed, so what conn receives before the mark is every notification of a transaction that
// committed before it: a signal that must come and does not is met by the mark in its place, and
// so is one that must not come, so no wait here needs a bound of its own (testwait.Context).
func requirePlansSignals(t *testing.T, conn *pgx.Conn, pool *store.Pool, after string, want ...string) {
	t.Helper()
	mark := "the mark after " + after
	if _, err := pool.Exec(t.Context(), `select pg_notify($1, $2)`, plansSignalChannel, mark); err != nil {
		t.Fatalf("send %q on %s: %v", mark, plansSignalChannel, err)
	}
	for _, payload := range append(want, mark) {
		signal, err := conn.WaitForNotification(testwait.Context(t))
		if err != nil {
			t.Fatalf("after %s, wait for %q on %s: %v", after, payload, plansSignalChannel, err)
		}
		if signal.Channel != plansSignalChannel || signal.Payload != payload {
			t.Fatalf("after %s, the next notification is on %s with payload %q, want %s with %q (want %q, then the mark)",
				after, signal.Channel, signal.Payload, plansSignalChannel, payload, want)
		}
	}
}

// TestAPlanWriteSignalsItsIssueAtCommit: a write that makes a plan signals its issue once, at its
// commit and not before; the same write again, which appends no tuple, and a write rolled back
// signal nothing; and a later version of the owner's plan, which replaces the plan's fact and so
// appends its tuples, signals its issue again.
func TestAPlanWriteSignalsItsIssueAtCommit(t *testing.T) {
	t.Parallel()
	pool := storetest.Open(t).Pool
	conn := listenForPlans(t, pool)
	ctx := t.Context()
	id := uuid.NewString()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := PutPlan(ctx, tx, defaultPlan(id, "LEG-1", 1), daemon, minute(0)); err != nil {
		t.Fatalf("write the plan: %v", err)
	}
	requirePlansSignals(t, conn, pool, "a write that has not committed")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the write: %v", err)
	}
	requirePlansSignals(t, conn, pool, "the write's commit", "LEG-1")

	putPlan(t, pool, defaultPlan(id, "LEG-1", 1), minute(1))
	requirePlansSignals(t, conn, pool, "the same write again, which appends nothing")

	rolledBack, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write rolled back: %v", err)
	}
	if _, err := PutPlan(ctx, rolledBack, defaultPlan(uuid.NewString(), "LEG-2", 1), daemon, minute(2)); err != nil {
		t.Fatalf("write the plan rolled back: %v", err)
	}
	if err := rolledBack.Rollback(ctx); err != nil {
		t.Fatalf("roll the write back: %v", err)
	}
	requirePlansSignals(t, conn, pool, "a write rolled back")

	putPlan(t, pool, PlanWrite{
		ID: id, Issue: "LEG-1", Generation: new(int64(1)), Source: new(PlanSourceOwner),
		Document: PlanDocument{ArtifactID: uuid.NewString(), Version: 1},
		Tasks:    []PlanTask{{ID: "work", Steps: []string{"implementer", "reviewer", "acceptance:owner"}}},
	}, minute(3))
	requirePlansSignals(t, conn, pool, "the owner's version 1, which replaces the default plan's fact", "LEG-1")
}
