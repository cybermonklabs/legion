package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A room this process holds for a document that moved into its issue's room takes nothing more
// (ErrRoomMoved), and the catch-up that reads the move (catchUpStatement) closes the room, once
// (catchUpMoved, closeMovedRoom): a move whose notice reached no process here
// (AnnounceDocumentRooms) is read by the next catch-up its room is asked for. Read as a replaced
// history instead, the room went to closeReplaced, whose read under the document's lock reads only
// the stored history's bounds, which a move leaves as they were: it closed nothing and asked for
// another catch-up, which read the move again, for as long as the room stayed resident.
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
	if _, err := database.Pool.Exec(ctx, `
		with tree as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('DOC-1', 'DOC', 'tree', 'tree', 'tree', false, '{"kind":"user","id":"alice"}')
			returning id
		), held as (
			insert into issue_trees (issue_key, artifact_id) select 'DOC-1', id from tree
		)
		insert into document_rooms (artifact_id, room) select $1::uuid, id from tree`, document); err != nil {
		t.Fatalf("move the document into its issue's room: %v", err)
	}

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
	if reads := countNotes(notes.reads, document, 100*time.Millisecond); reads != 1 {
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
