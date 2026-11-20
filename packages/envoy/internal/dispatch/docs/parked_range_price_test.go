package docs

import (
	"runtime"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
)

// The room budget charges each delete range a room's live document parks at
// bytesOfHeapAParkedRange, which is priced at the heaviest shape measured on
// theorem-labs/ygo v1.55.1-theorem.11: a client of its own for each range, the range's two halves
// parked by two senders. The fork's record of who parked each range (crdt.Doc.parkedDsBy) keeps a
// delete set for each sender, so there it holds twice the ranges the queue does, each in a set of
// its own. A sender is the connection the provider applies an update for, the apply's origin
// (provider/websocket/peer.go:357); weighInto applies with none, which the record keeps nothing
// for, so the frames here are applied each for a sender of its own. The room parks 4,096 ranges,
// the cap, and the live heap they hold, read with the frames freed, must stay within what the
// room budget's charge for the room grew by, plus 1 MiB. At the 32 bytes a range the room was
// charged before, the 4,096 ranges held 4,075,096 bytes of heap for a charge of 131,072.
func TestParkedDeleteRangesHoldNoMoreHeapThanTheirChargeAtTheirHeaviestShape(t *testing.T) {
	service, room, replica, charge := chargedPeerRoom(t)
	defer runtime.KeepAlive(replica)
	room.SetMaxPendingDeleteRanges(maxParkedDeleteRanges)
	type sender struct{ n int }
	const firstClient = 9_000_001
	chargedBefore := chargedWeight(charge)
	before := liveHeap()
	for i := range maxParkedDeleteRanges {
		for half := range 2 {
			frame := deleteRangesPast(crdt.ClientID(firstClient+i), uint64(half), 1)
			origin := &sender{2*i + half}
			called := false
			err := service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: frame}, func() error {
				called = true
				return crdt.ApplyUpdateV1(room, frame, origin)
			})
			if err != nil || !called {
				t.Fatalf("client %d's half %d was refused: %v (called %v)", firstClient+i, half, err, called)
			}
		}
	}
	after := liveHeap()
	runtime.KeepAlive(room)
	held := int64(after) - int64(before)
	charged := chargedWeight(charge) - chargedBefore
	ranges := room.PendingStats().DeleteRanges
	t.Logf("%d clients' ranges parked in halves by two senders each: the room parks %d ranges; its live heap grew %d bytes (%.1f a range) and its budget charge %d",
		maxParkedDeleteRanges, ranges, held, float64(held)/float64(ranges), charged)
	if ranges != maxParkedDeleteRanges {
		t.Fatalf("the room parks %d delete ranges, want %d, one for each client", ranges, maxParkedDeleteRanges)
	}
	if held > charged+(1<<20) {
		t.Errorf("the room's %d parked delete ranges hold %d bytes of heap, past the %d its budget charge grew by plus 1 MiB", ranges, held, charged)
	}
}
