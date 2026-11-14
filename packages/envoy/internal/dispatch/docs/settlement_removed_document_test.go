package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// removalClears is what a removal deletes of an untouched document before the artifacts row
// itself, in the order api.removeArtifact's own statements run (artifactRemovalClears): the rows
// whose foreign key to artifacts(id) carries no cascade. The rest of that route's statements
// clear tables no row of this fixture's document can be in.
var removalClears = []string{
	`delete from artifact_versions where artifact_id = $1`,
	`delete from doc_updates where artifact_id = $1`,
	`delete from doc_snapshots where artifact_id = $1`,
	`delete from doc_checkpoints where artifact_id = $1`,
	`delete from artifacts where id = $1`,
}

// A settlement pass reads the document's artifacts row before it waits for the owner row that
// orders it against a removal (lockArtifactOwner), and a removal holds that row from its first
// read to its commit (api.removeArtifact): a pass that read the row while the removal still held
// the lock meets, the moment the removal commits, a document whose every row has gone.
//
// Such a pass has nothing to settle, and ends quietly: it reads no version of a document that is
// not there, counts no failure against the room and says nothing in the log, as the room's own
// close says nothing (documentRemoved). Without the re-read lockArtifactOwner makes under the
// lock, the pass went on with an owner it had read before the removal and failed on the version
// that had gone with it - `read latest document version: no rows in result set`, logged ERROR by
// the retry (retrySettle).
func TestASettlementThatWaitedBehindARemovalEndsQuietly(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, artifactID := newTestServiceOn(t, database)
	// Only the settlement this test runs settles this room, at the moment it chooses.
	service.settle = time.Hour
	// The room the settlement reads, with the document's text in it, as every room a settlement
	// runs over holds it: the seed writes the text cold (TestAColdWriteLoadsNoRoom) and the room
	// is loaded here as the settlement's own warm-up loads one (settleRoomWithin).
	seedServiceText(t, service, artifactID, "# First\n\nA paragraph nobody has settled.\n")
	if err := service.srv.Apply(ctx, artifactID, func(*crdt.Doc, func(func(*crdt.Transaction))) {}); err != nil &&
		!errors.Is(err, websocket.ErrNoChanges) {
		t.Fatalf("load the document's room: %v", err)
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("the document has no room for a settlement to read")
	}

	// The removal, held open at the point its commit is all that is left: the document's owner
	// row, then its room, then the document's rows (api.removeArtifact, in that order).
	removal, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the removal: %v", err)
	}
	defer removal.Rollback(context.Background())
	if _, _, err := lockArtifactOwner(ctx, removal, artifactID); err != nil {
		t.Fatalf("lock the document's owner row for the removal: %v", err)
	}
	if err := LockDocumentRoom(ctx, removal, artifactID); err != nil {
		t.Fatalf("lock the document's room for the removal: %v", err)
	}
	for _, statement := range removalClears {
		if _, err := removal.Exec(ctx, statement, artifactID); err != nil {
			t.Fatalf("run the removal's statement %q: %v", statement, err)
		}
	}

	// The settlement, which reads the artifacts row the removal has deleted but not committed,
	// and then waits for the issue row the removal holds. Its generation is the room's as it
	// stands now: a settlement armed meanwhile would be the room's own.
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	logs := captureLogs(t)
	outcomes := make(chan settleOutcome, 1)
	returned := make(chan error, 1)
	go func() {
		outcome := service.settleRoomWithin(context.Background(), artifactID, generation, false)
		outcomes <- outcome
		returned <- outcome.err
	}()
	waitForLockWait(t, ctx, database, "%closed_at is null from issues%", returned)
	if err := removal.Commit(ctx); err != nil {
		t.Fatalf("commit the removal: %v", err)
	}

	outcome := <-outcomes
	// What the timer's settlement does with the outcome, which is where a stop short is logged.
	service.followUpSettlement(artifactID, outcome)
	said := logs.Lines(artifactID)
	if outcome.next != settleEnded || outcome.err != nil || said != "" {
		t.Fatalf("the settlement of the removed document ended next=%d error=%v and logged %q; want it ended with nothing to settle, and nothing said of a document that has gone",
			outcome.next, outcome.err, said)
	}
	// The room the removal's own eviction closes, which is what this process does after the
	// commit (api.removeArtifact): it closes over a row that has gone, and the service's shutdown
	// settles nothing of it.
	if err := service.Evict(ctx, artifactID); err != nil {
		t.Fatalf("close the room of the removed document: %v", err)
	}
}
