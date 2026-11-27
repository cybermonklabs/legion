package docs

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A room this process holds for a document that moved into its issue's room takes nothing more
// (ErrRoomMoved), and the catch-up that reads the move (catchUpStatement) closes the room, once
// (catchUpMoved, closeMovedRoom): a move whose notice reached no process here
// (AnnounceDocumentRooms) is read by the next catch-up its room is asked for. Read as a replaced
// history instead, the room would go to closeReplaced, whose read under the document's lock reads
// only the stored history's bounds, which a move leaves as they were: it would close nothing and
// ask for another catch-up, which would read the move again, for as long as the room stayed
// resident.
func TestACatchUpThatReadsItsDocumentMovedClosesTheRoomOnce(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	document := createIssueDocument(t, database, 1, "Before.")
	seedServiceText(t, service, document, "Before.\n")
	settleCurrentGeneration(t, service, document)
	if err := service.warmLiveDocument(ctx, document); err != nil {
		t.Fatalf("load the document's room: %v", err)
	}
	if service.srv.GetDoc(document) == nil {
		t.Fatal("the document's room is not resident, so nothing here is tested")
	}
	// The move's rows, written with no announcement: no process hears of it.
	moveIntoIssueRoom(t, database, document)

	// The lane is the test's own, run here turn by turn, so a catch-up the follower asks for again
	// waits on it to be counted.
	notes := watchCatchUps(t, service)
	lane := newRoomSyncLane(roomSyncHandler{})
	service.catchUps.attach(lane)
	service.catchUps.request(document)
	for range 3 {
		work := lane.pop()
		if work == nil {
			break
		}
		work(ctx)
	}
	if reads := countNotes(notes.reads, document); reads != 1 {
		t.Errorf("the catch-ups of the moved document's room read the store %d times, want once", reads)
	}
	if lane.pop() != nil {
		t.Error("the catch-up of the moved document's room asked for another catch-up of it")
	}
	if wake := service.catchUps.wakeIn(); wake != 0 {
		t.Errorf("the catch-up of the moved document's room armed a retry in %s", wake)
	}
	waitFor(t, roomWait(t), "the moved document's room to close", func() bool {
		return service.srv.GetDoc(document) == nil
	})
}

// A settlement whose catch-up reads its room's document moved into its issue's room (catchUpMoved)
// refuses it as a moved document (ErrRoomMoved), not as a stored history replaced under the room
// (ErrRoomBehindHead), and closes the room for a reload (reloadRefused, reloadReplacedRoom), whose
// log line names that refusal. Task A's room stands behind the head, so the settlement's gate runs
// the catch-up, and the move lands once the settlement has resolved the document's room (roomOf)
// and before it reads the store.
func TestASettlementWhoseCatchUpReadsItsDocumentMovedRefusesItAsMoved(t *testing.T) {
	pair := newProcessPair(t)
	pair.leaveTaskABehind(t, 2, model.Actor{Kind: "user", ID: "bob-on-task-b"}, "Bob's paragraph.\n")
	logs := captureLogs(t)
	var moved atomic.Bool
	pair.taskA.beforeSettleStart = func(room string) {
		if room == pair.artifactID && moved.CompareAndSwap(false, true) {
			moveIntoIssueRoom(t, pair.database, room)
		}
	}
	settleNow(pair.taskA, pair.artifactID)
	if !moved.Load() {
		t.Fatal("no settlement of task A's room started, so nothing here is tested")
	}
	refusals := logs.Lines("its settlement found replaced")
	if !strings.Contains(refusals, "the document was moved into its issue's room") || strings.Contains(refusals, "the stored history was replaced") {
		t.Fatalf("the settlement whose catch-up read its document moved logged %q, want its refusal ErrRoomMoved's and no replaced history's", refusals)
	}
}
