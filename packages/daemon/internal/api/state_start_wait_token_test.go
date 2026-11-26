package api

import (
	"slices"
	"testing"
)

// startWaitTokenDropped is the `dropped` of the band of startWaitTokenContract's entry, the line it
// writes, named here as a literal so a strip applied beside the table's loop fails this test.
const startWaitTokenDropped = "the claim tokens of the issues' startWaitsForPod, served as the roles of its starts, each once"

// A caller one contract below the start-wait token's entry is told of that entry's band in one
// line, among the lines of the bands above the contract it is served at. It reads the band's
// number from the registry (entryNumber), so it holds no entry to be the newest.
func TestACallerBelowTheStartWaitTokenEntryIsToldOfItsBand(t *testing.T) {
	state := populatedState(t)
	if entryNumber(startWaitTokenContract) <= entryNumber(strandedTokenContract) {
		t.Fatalf("the start-wait token's entry is numbered %d, at or below the stranded token's %d: this test reads the stranded token's band below it",
			entryNumber(startWaitTokenContract), entryNumber(strandedTokenContract))
	}
	below := entryNumber(startWaitTokenContract) - 1
	_, narrowings := stateNarrowedStating(t, state, statePodAddr+":41000", podClaims(0), below)
	want := narrowingsExpected(state, below)
	if !slices.Contains(want, startWaitTokenDropped) {
		t.Fatalf("a caller at %d expects the lines %v, want the start-wait token band's %q among them", below, want, startWaitTokenDropped)
	}
	wantNarrowings(t, narrowings, below, want)
	if got := droppedAt(t, entryNumber(startWaitTokenContract)); got != startWaitTokenDropped {
		t.Errorf("the band at the start-wait token's entry drops %q, want %q", got, startWaitTokenDropped)
	}
}
