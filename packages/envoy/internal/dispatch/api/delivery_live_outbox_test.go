package api

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// TestAChangeACrashLeftPendingWakesASubscribedViewOnceTheBootDrains: a Legion facts write killed
// between its commit and its drain leaves its change row in the change log's outbox, and the
// notification its commit sent reached no hub. The row alone wakes nothing: a live Legion board
// of its project is not recomputed. The boot of the next server drains the outbox before it
// serves (cmd/dispatch's main calls delivery.DrainPending), and its drain's notification wakes
// the hub, which reads the moved change and recomputes the board, in well under the 5 s this test
// waits; the time from the drain to the recompute is recorded. A pending row left by a writer
// killed between its commit and its drain is moved by the next drain of any process, or else by
// the next process's boot before it serves (delivery.DrainPending states the bound); this test
// drives the boot's.
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
