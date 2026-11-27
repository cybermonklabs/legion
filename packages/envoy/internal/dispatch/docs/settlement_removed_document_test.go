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
// close says nothing (Service.documentRemoved). Without the re-read lockArtifactOwner makes
// under the lock, the pass went on with an owner it had read before the removal and failed on the
// version that had gone with it - `read latest document version: no rows in result set`, logged
// ERROR by the retry (retrySettle).
//
// A settlement first takes its lease on the document's pending-settlement row
// (leaseSettlementPending), and the removal's delete of the artifacts row deletes that row with it
// and holds it until it commits, so where the row stands the settlement waits there, in the
// lease's statement, and its pass starts only once the removal has committed
// (TestASettlementThatWaitedBehindARemovalEndsQuietly). Where the document owes no settlement the
// lease updates nothing and waits for nothing, and the pass meets the removal at the owner lock,
// the race the re-read under the lock is for
// (TestASettlementOwingNoneThatWaitedBehindARemovalEndsQuietly).
func TestASettlementThatWaitedBehindARemovalEndsQuietly(t *testing.T) {
	settleBehindAHeldRemoval(t, true)
}

// A settlement runs whether or not its document's row still stands - settleRoomWithin reads no
// row before it begins - and one finds none when another process's settlement committed the
// updates it was armed for and deleted the row first: a re-arm for another process's change a
// catch-up applied (rearmSettle), whose own settlement that process armed too, or a write's timer
// in one process while the resumption's settlement of the same document commits in another. Such
// a settlement's pass is the one that meets a removal at the owner lock.
func TestASettlementOwingNoneThatWaitedBehindARemovalEndsQuietly(t *testing.T) {
	settleBehindAHeldRemoval(t, false)
}

// settleBehindAHeldRemoval runs a settlement of a resident document while a removal of it holds
// the document's owner lock with every row deleted and only its commit left, commits the removal
// once the settlement waits on it, and requires the settlement to end with nothing to settle and
// nothing said. With owes the document's pending-settlement row stands, and the settlement waits
// in its lease's statement; without, the row is deleted first, and it waits for the owner lock in
// the owner lock's own statement (store.OwnerLockStatement, `... as owner_lock`).
func settleBehindAHeldRemoval(t *testing.T, owes bool) {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	service, artifactID := newTestServiceWithDocument(t, database)
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
	wait := "%set leased_until = now()%"
	if !owes {
		if _, err := database.Pool.Exec(ctx, `delete from doc_settlements_pending where artifact_id = $1`, artifactID); err != nil {
			t.Fatalf("delete the document's pending-settlement row: %v", err)
		}
		wait = "%as owner_lock%"
	}
	if pending, err := settlementPending(ctx, database.Pool, artifactID); err != nil || pending != owes {
		t.Fatalf("the document owes a settlement = %v (%v), want %v", pending, err, owes)
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

	// The settlement, waiting where wait says, which is what its backend shows while it waits.
	// Its generation is the room's as it stands now: a settlement armed meanwhile would be the
	// room's own.
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
	waitForLockWait(t, ctx, database, wait, returned)
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

// A settlement of a document removed before it began - its room cold, so the warm-up's load finds
// no row and the settlement stops short there - ends as the removal left it: it reads that the
// document has gone (settlement.documentRemoved), retries nothing, counts no failure and says
// nothing, so no retry warms a room over a row that has gone. That read is settlement work, so it
// runs on the settlements pool the settlement opened, as every other read of it does: the
// settlement takes no connection of the request pool, which admission hands out to requests.
func TestASettlementOfARemovedColdDocumentEndsQuietlyOffTheRequestPool(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, artifactID := newTestServiceWithDocument(t, database)
	service.settle = time.Hour
	// The seed writes the text cold (TestAColdWriteLoadsNoRoom): the settlement below has to warm
	// the room before it reads anything, and the removal leaves that load nothing to read.
	seedServiceText(t, service, artifactID, "# First\n\nA paragraph nobody has settled.\n")
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("the document holds a room; the settlement must find it cold")
	}
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
	if err := removal.Commit(ctx); err != nil {
		t.Fatalf("commit the removal: %v", err)
	}

	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	logs := captureLogs(t)
	before := service.store.Pool.Stat().AcquireCount()
	outcome := service.settleRoomWithin(ctx, artifactID, generation, false)
	took := service.store.Pool.Stat().AcquireCount() - before
	service.followUpSettlement(artifactID, outcome)

	if outcome != (settleOutcome{}) {
		t.Fatalf("the settlement of the removed document ended next=%d error=%v, want it ended with nothing owed: a load that finds no row is a removal, not a fault to retry",
			outcome.next, outcome.err)
	}
	if took != 0 {
		t.Fatalf("the settlement took %d request-pool connection(s), want none: it reads that its document has gone through the settlements pool", took)
	}
	if said := logs.Lines(artifactID); said != "" {
		t.Fatalf("the settlement of the removed document logged %q, want nothing said of a document that has gone", said)
	}
	state.mu.Lock()
	failures, armed := state.settleFailures, service.isSettleTimerArmed(state.settle)
	state.mu.Unlock()
	if failures != 0 || armed {
		t.Fatalf("the room counts %d settlement failure(s) with a retry armed %t, want neither", failures, armed)
	}
}
