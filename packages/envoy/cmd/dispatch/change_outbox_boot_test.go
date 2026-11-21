package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestDispatchBootMovesTheChangesACrashLeftPendingBeforeItServes: two change rows an eval and a
// Legion write recorded in the change log's outbox, their process killed between its commit and its
// drain while no other process was alive, are moved into delivery_changes by the next server's boot
// before it serves: the boot logs that it moved both before it logs that it listens, and the log
// holds each at the moment it was recorded, nothing left pending. Its drain's notification wakes
// every hub listening by then, and this process's refresher reads the moved rows on its first pass
// (api's TestAChangeACrashLeftPendingWakesASubscribedViewOnceTheBootDrains holds a hub to the
// wake).
func TestDispatchBootMovesTheChangesACrashLeftPendingBeforeItServes(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	recorded := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := database.Pool.Exec(ctx, `
		insert into delivery_changes_pending (at, kind, repo, ref)
		values ($1, 'eval_readings', 'CRASH', 'CRASH/left-pending'), ($1, 'issue_facts', 'CRASH', 'CRASH-1')
	`, recorded); err != nil {
		t.Fatalf("leave two pending changes as a crash between commit and drain leaves them: %v", err)
	}

	started := time.Now()
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{})
	process.waitHealthy(t)
	served := time.Since(started)
	output := process.Output.String()
	drained := strings.Index(output, `msg="dispatch: moved the delivery changes left pending into the change log" moved=2`)
	listening := strings.Index(output, process.listeningLine())
	if drained < 0 || listening < 0 || drained > listening {
		t.Errorf("the boot's output holds its drain of both pending changes at %d and its listening line at %d, want the drain first:\n%s", drained, listening, output)
	}
	var moved int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from delivery_changes where repo = 'CRASH' and ref in ('CRASH/left-pending', 'CRASH-1') and at = $1
	`, recorded).Scan(&moved); err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Errorf("the change log holds %d of the two moved changes at their recorded moment, want both", moved)
	}
	var left int
	if err := database.Pool.QueryRow(ctx, `select count(*) from delivery_changes_pending where repo = 'CRASH'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d of the crash's changes are still pending after the boot", left)
	}
	t.Logf("the boot moved both pending changes before it served; it served %s after it started", served.Round(time.Millisecond))
}
