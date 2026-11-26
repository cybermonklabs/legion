package api

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// TestAChangeACrashLeftPendingWakesASubscribedViewOnceTheBootDrains: an eval write killed between
// its commit and its drain leaves its change row in the change log's outbox, and the notification
// its commit sent reached no hub. The row alone wakes nothing: its insert, as the crash left it,
// notifies no hub, so the boot's drain finds it still pending, where any hub's catch-up would have
// moved it first (catchUpChanges drains the outbox before it reads). The boot of the next server
// drains the outbox before it serves (cmd/dispatch's main calls delivery.DrainPending), and its
// drain's notification wakes the hub, which reads the moved change and recomputes a live evals
// view of its project; the time from the drain to the recompute is recorded. A pending row left by
// a writer killed between its commit and its drain is moved by the next drain of any process, or
// else by the next process's boot before it serves (delivery.DrainPending states the bound); this
// test drives the boot's.
func TestAChangeACrashLeftPendingWakesASubscribedViewOnceTheBootDrains(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	createTestProject(t, rig.handler, "HUB")
	frame := evalsLiveView("HUB", DeliveryLiveWindow{Preset: new(28)}, "")
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("e", frame))
	conn.snapshot(t, "e")
	rig.settle(t)

	mark := rig.mark()
	measured := rig.clock.Now().Add(-time.Hour)
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into delivery_changes_pending (kind, repo, ref, t_old, t_new) values ('eval_readings', 'HUB', 'HUB/crash-left', $1, $1)
	`, measured); err != nil {
		t.Fatalf("leave a pending change as a crash between commit and drain leaves it: %v", err)
	}

	drained := time.Now()
	moved, err := delivery.DrainPending(t.Context(), rig.database.Pool)
	if err != nil || moved != 1 {
		t.Fatalf("the boot's drain moved %d, %v; want the one pending change", moved, err)
	}
	rig.waitFor(t, mark, "recompute of the evals view after the boot's drain", recomputeOfView(t, frame))
	woke := time.Since(drained)
	var left int
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from delivery_changes_pending`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d changes are still pending after the boot's drain", left)
	}
	t.Logf("the pending change woke nothing until the boot's drain moved it; the view's recompute began %s after the drain (the hub waits %s of quiet before a recompute)",
		woke.Round(time.Millisecond), viewQuiet)
}
