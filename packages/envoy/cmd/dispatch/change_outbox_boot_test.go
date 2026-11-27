package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestDispatchBootMovesTheChangesACrashLeftPendingBeforeItServes: two change rows a Legion facts
// write and a Legion filings write recorded in the change log's outbox, their process killed
// between its commit and its drain while no other process was alive, are moved into
// delivery_changes by the next server's boot before it serves: the boot logs that it moved both
// before it logs that it listens, and the log holds each stamped at the drain's own time
// (migration 20261127133240), nothing left pending. Its drain's notification wakes every hub
// TestAChangeACrashLeftPendingWakesASubscribedViewOnceTheBootDrains holds a hub to the wake).
func TestDispatchBootMovesTheChangesACrashLeftPendingBeforeItServes(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	recorded := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := database.Pool.Exec(ctx, `
		insert into delivery_changes_pending (at, kind, repo, ref)
		values ($1, 'issue_filings', 'CRASH', 'CRASH-2'), ($1, 'issue_facts', 'CRASH', 'CRASH-1')
	`, recorded); err != nil {
		t.Fatalf("leave two pending changes as a crash between commit and drain leaves them: %v", err)
	}

	var beforeBoot time.Time
	if err := database.Pool.QueryRow(ctx, `select clock_timestamp()`).Scan(&beforeBoot); err != nil {
		t.Fatal(err)
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
		select count(*) from delivery_changes
		where repo = 'CRASH' and ref in ('CRASH-2', 'CRASH-1') and at >= $1
	`, beforeBoot).Scan(&moved); err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Errorf("the change log holds %d of the two moved changes stamped at or after boot (%s), want both", moved, beforeBoot)
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
