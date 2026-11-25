//go:build multiprocess

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// Five readers on A, each on a socket of its own, place their cursors at the same moment, as tabs
// do when a document loads for several people or every tab reconnects after a deploy. Bob on B
// must see all five within a second, as a reader on A does. A room relaying only the newest of
// its pending awareness changes loses the others until each reader's next heartbeat (15 s in
// y-protocols), and these readers send none, so such a room never shows bob all five. The second
// is held only by the row's twin in benchtime's timing lane (withinLive).
func TestFiveReadersPlacingCursorsAtOnceAreAllSeenOnTheOtherProcess(t *testing.T) {
	fiveReadersPlacingCursorsAtOnceAreAllSeenOnTheOtherProcess(t, benchtime.EveryRun)
}

// TestFiveReadersPlacingCursorsAtOnceAreAllSeenOnTheOtherProcessOnTheClock is the row above in
// benchtime's timing lane, where the five cursors reaching carol on A and bob on B are held to
// liveWithin of their placing (withinLive). The product's promise is README.md's Cursors and
// presence ("each send carries every reader who moved within that window ... so readers moving
// at the same moment all cross rather than the last of them"), at the spec's "at once", the one
// second the plan pins (#2414).
func TestFiveReadersPlacingCursorsAtOnceAreAllSeenOnTheOtherProcessOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), fiveReadersPlacingCursorsAtOnceAreAllSeenOnTheOtherProcess)
}

// fiveReadersPlacingCursorsAtOnceAreAllSeenOnTheOtherProcess is the scene of the two rows above;
// lane says whether its readings of the clock are held to their figures (benchtime.Lane).
func fiveReadersPlacingCursorsAtOnceAreAllSeenOnTheOtherProcess(t *testing.T, lane benchtime.Lane) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "SIM")
	pair.a.createProject(t, project, "Simultaneous cursors")
	_, artifactID := pair.a.createIssueIn(t, project, "Five at once", "x")
	bob := pair.openSyncedEditor(t, pair.b, "bob", artifactID, "x\n")
	const readers = 5
	editors := make([]*docstest.Peer, readers)
	for i := range editors {
		editors[i] = pair.openSyncedEditor(t, pair.a, fmt.Sprintf("reader%d", i), artifactID, "x\n")
	}
	carol := pair.openSyncedEditor(t, pair.a, "carol", artifactID, "x\n")
	onB := make([]awarenessBase, readers)
	for i := range editors {
		onB[i] = pair.b.relayedAwareness(t, artifactID, uint64(8000+i))
	}
	for i, editor := range editors {
		if err := editor.SetAwareness(uint64(8000+i), cursorState(fmt.Sprintf("reader%d", i), i)); err != nil {
			t.Fatalf("reader %d's cursor: %v", i, err)
		}
	}
	placed := time.Now()
	all := func(states map[uint64]json.RawMessage) bool {
		for i := range readers {
			if !holdsState(uint64(8000+i), cursorState(fmt.Sprintf("reader%d", i), i))(states) {
				return false
			}
		}
		return true
	}
	if err := carol.WaitForAwareness(t, all); err != nil {
		t.Fatalf("carol on A did not see all five readers on A: %v", err)
	}
	withinLive(t, lane, placed, "the five readers' cursors reaching carol on A")
	if err := bob.WaitForAwareness(t, all); err != nil {
		held := []uint64{}
		for client := range bob.AwarenessStates() {
			held = append(held, client)
		}
		t.Fatalf("five readers on A placed their cursors at once: carol on A saw all five, bob on B holds only %v", held)
	}
	withinLive(t, lane, placed, "the five readers' cursors reaching bob on B")
	for i := range editors {
		pair.b.awaitRelayedAwareness(t, artifactID, uint64(8000+i), false, onB[i],
			fmt.Sprintf("reader %d's cursor reaching bob on B", i))
	}
}
