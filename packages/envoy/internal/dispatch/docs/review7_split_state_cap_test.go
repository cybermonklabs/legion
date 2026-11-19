// Scratch reproduction for review 7 of sjawhar/legion PR #2544 at 7e798f23 (R7Table), package docs.
// Asserts the correct behaviour, so it FAILS where the defect stands.
//
// The PR body's table, state cap row, delete's-splits column: "a split's item headers are state
// bytes, counted by the same Bytes field". ygo's Bytes counts content bytes only
// (crdt.ApplyBudget.Bytes, doc.go:276-284; applyTally.integrated adds contentBytes), and a split
// adds no content, so a room the state cap leaves no byte (Bytes: 0) still takes a delete-only
// update whose splits add megabytes of encoded state; the room's tracked state, which the library's
// report raises after the apply (noteApply), ends past maxRoomStateBytes.
package docs

import (
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

func TestR7SplitsOfADeleteTakeTheRoomPastTheStateCap(t *testing.T) {
	const run, cuts = 600_000, 200_000
	service, room, replica := peerUpdateRoom(t, "# "+strings.Repeat("a", run)+"\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	// The room charged as the state cap leaves it nothing, as TestACountAtItsAllowance... does.
	service.chargeLoadedRoom("room", replica, maxRoomStateBytes)
	browser, text := peerDocument(t, room)
	browser.Transact(func(txn *crdt.Transaction) {
		for j := cuts - 1; j >= 0; j-- {
			text.Delete(txn, 2*j+1, 1)
		}
	})
	update := crdt.EncodeStateAsUpdateV1(browser, room.StateVector())
	encodedBefore := len(crdt.EncodeStateAsUpdateV1(room, nil))
	trackedBefore := service.roomWeighingOf(room).roomStateBytes()
	allowance := peerApplyAllowance(int64(len(update)), 0, trackedBefore, heldStructs(room), false, peerPressure{})
	err, called := weighInto(service, room, update)
	encodedAfter := len(crdt.EncodeStateAsUpdateV1(room, nil))
	trackedAfter := service.roomWeighingOf(room).roomStateBytes()
	t.Logf("delete-only update of %d mid-run cuts, %d bytes; allowance Bytes=%d Split=%d; hook=%v applied=%t",
		cuts, len(update), allowance.budget.Bytes, allowance.budget.Split, err, called)
	t.Logf("encoded state %d -> %d bytes (+%d); tracked state %d -> %d (cap %d, %+d past it); structs %d",
		encodedBefore, encodedAfter, encodedAfter-encodedBefore, trackedBefore, trackedAfter, maxRoomStateBytes,
		trackedAfter-maxRoomStateBytes, heldStructs(room))
	if err == nil && trackedAfter > maxRoomStateBytes {
		t.Fatalf("a room with no state-cap headroom (Bytes %d) admitted a delete whose splits added %d bytes of state, leaving it %d past the %d cap",
			allowance.budget.Bytes, encodedAfter-encodedBefore, trackedAfter-maxRoomStateBytes, maxRoomStateBytes)
	}
}
