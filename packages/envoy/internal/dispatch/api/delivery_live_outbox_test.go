package api

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// TestAChangeACrashLeftPendingWakesASubscribedViewOnceTheBootDrains: a Legion facts write killed
// between its commit and its drain leaves its change row in the change log's outbox, and the
// notification its commit sent reached no hub. The row alone wakes nothing: a live Legion board
// of its project is not recomputed. The boot of the next server drains the outbox before it
// serves (cmd/dispatch's main calls delivery.DrainPending), and its drain's notification wakes
// the hub, which reads the moved change and recomputes the board; the time from the drain to the
// recompute is recorded. A pending row left by a writer killed between its commit and its drain
// is moved by the next drain of any process, or else by the next process's boot before it serves
// (delivery.DrainPending states the bound); this test drives the boot's.
func TestAChangeACrashLeftPendingWakesASubscribedViewOnceTheBootDrains(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "HUB")
	nameLegionDaemon(t, rig.handler, "HUB", "http://legion-hub.internal:13370")
	seedLegionBoard(t, rig.database, "HUB", rig.clock.Now())
	frame := legionView("HUB")
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("b", frame))
	conn.snapshot(t, "b")
	rig.settle(t)

	mark := rig.mark()
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into delivery_changes_pending (kind, repo, ref) values ('issue_facts', 'HUB', 'HUB-1')
	`); err != nil {
		t.Fatalf("leave a pending change as a crash between commit and drain leaves it: %v", err)
	}
	// A quiet window, since the rig has no wait that ends on an absence without one: settle, and
	// requireNothingSent through it, end on the hub's catch-up (waitNothingOwed's catchUpNow), and
	// every catch-up drains the outbox before it reads the log (catchUpChanges), so either would move
	// this row itself and recompute the board; a read of the hub's state without that barrier
	// (intakeOwes) cannot see a wake still on its way from the listener. Four times the quiet a
	// woken board waits before its recompute is longer than a wake would take to begin one, and a
	// loaded machine can only make the window miss a wake, never fail a hub the row leaves idle.
	time.Sleep(4 * viewQuiet)
	if woken := rig.count(mark, recomputeOfView(t, frame)); woken != 0 {
		t.Fatalf("the Legion board was recomputed %d times before any drain, so this test cannot tell what woke it", woken)
	}

	drained := time.Now()
	moved, err := delivery.DrainPending(t.Context(), rig.database.Pool)
	if err != nil || moved != 1 {
		t.Fatalf("the boot's drain moved %d, %v; want the one pending change", moved, err)
	}
	rig.waitFor(t, mark, "recompute of the Legion board after the boot's drain", recomputeOfView(t, frame))
	woke := time.Since(drained)
	var left int
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from delivery_changes_pending`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d changes are still pending after the boot's drain", left)
	}
	t.Logf("the pending change woke nothing until the boot's drain moved it; the board's recompute began %s after the drain (the hub waits %s of quiet before a recompute)",
		woke.Round(time.Millisecond), viewQuiet)
}

// TestTheHubDrainsBeforeItReadsTheLog: a Legion facts write leaves its change row in
// delivery_changes_pending until a drain moves it into delivery_changes, and the hub drains the
// outbox itself before each read of the change log (deliveryHub.catchUpChanges calls
// delivery.DrainPending). So a pending issue_facts row whose writer did not drain it still moves
// into the change log when the hub wakes to read, and its delta reaches the subscribed `legion`
// view of that project with no drain by hand.
//
// The listener is blind here (dial refuses every connection), so nothing wakes the hub while the
// row sits in the outbox and the test sees the row still pending before catchUpNow wakes it.
// Negative control: remove delivery.DrainPending from deliveryHub.catchUpChanges; catchUpNow
// leaves the row in delivery_changes_pending and sends no delta.
func TestTheHubDrainsBeforeItReadsTheLog(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, func(hub *deliveryHub) {
		hub.dial = func(context.Context) (*pgx.Conn, error) { return nil, context.DeadlineExceeded }
	})
	seedLegionProjects(t, rig.handler, "HUB")
	nameLegionDaemon(t, rig.handler, "HUB", "http://legion-hub.internal:13370")
	now := rig.clock.Now()
	seedLegionBoard(t, rig.database, "HUB", now)

	frame := legionView("HUB")
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("b", frame))
	conn.snapshot(t, "b")
	rig.settle(t)

	// Write one stored facts row with no drain by hand: the trigger inserts an issue_facts row
	// into delivery_changes_pending and nothing into delivery_changes.
	workedAt := now.Add(-5 * time.Hour)
	writeIssueFacts(t, rig.database, "HUB", "HUB-4", "HUB-3", workedAt, workedAt.Add(time.Hour), nil)
	var pending, logged int
	if err := rig.database.Pool.QueryRow(t.Context(), `
		select (select count(*) from delivery_changes_pending where kind = 'issue_facts' and repo = 'HUB' and ref = 'HUB-4'),
		       (select count(*) from delivery_changes where kind = 'issue_facts' and repo = 'HUB' and ref = 'HUB-4')
	`).Scan(&pending, &logged); err != nil {
		t.Fatalf("count pending and logged changes before the hub wakes: %v", err)
	}
	if pending != 1 || logged != 0 {
		t.Fatalf("before the hub wakes: %d pending and %d logged for HUB-4, want 1 pending and 0 logged", pending, logged)
	}

	mark := rig.mark()
	rig.hub.catchUpNow()

	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from delivery_changes_pending`).Scan(&pending); err != nil {
		t.Fatalf("count pending changes after the hub woke: %v", err)
	}
	if pending != 0 {
		t.Fatalf("%d changes are still pending after the hub woke to read the log: the hub did not drain the outbox first", pending)
	}
	rig.waitFor(t, mark, "recompute of the Legion board after the hub drained the outbox", recomputeOfView(t, frame))
	delta := conn.delta(t, "b")
	if _, sent := delta.Maps["issues"]["HUB-4"]; !sent {
		t.Fatalf("the delta after the hub's drain carries the issues entries %v, want HUB-4's", slices.Sorted(maps.Keys(delta.Maps["issues"])))
	}
	t.Log("the hub drained the pending issue_facts change before reading the log and sent HUB-4's entry to the subscribed legion view")
}
