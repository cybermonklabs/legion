package docs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// storedBlockID is the block the comment's anchor pins now, as text, or "none" where the row
// still has none.
func storedBlockID(t *testing.T, service *Service, commentID string) string {
	t.Helper()
	var encoded []byte
	if err := service.store.Pool.QueryRow(context.Background(),
		`select anchor from comments where id = $1`, commentID).Scan(&encoded); err != nil {
		t.Fatalf("load the comment anchor: %v", err)
	}
	var anchor model.Anchor
	if err := json.Unmarshal(encoded, &anchor); err != nil {
		t.Fatalf("decode the comment anchor: %v", err)
	}
	if anchor.BlockID == nil {
		return "none"
	}
	return *anchor.BlockID
}

// BackfillAnchorBlocks writes comment and ask rows while the server serves them, so it takes
// each row's owner lock before it writes that row, as every other serving writer of an item row
// does (api's item-writer census, refs.lockItemOwner). The advisory lock excludes exactly the
// writers that take it, so a writer of an item row that takes no owner lock is the one thing
// main's `for no key update` of the item row covered and this lock does not.
//
// The row here hangs on no document, so its owner is its issue (store.OwnerOf). The test holds
// that owner's lock in a transaction of its own and asks what the backfill does: a backfill that
// takes the lock waits, and one that writes on the pool goes straight past the holder and
// updates the row under it.
func TestBackfillAnchorBlocksWaitsForTheOwnerLockOfTheRowItWrites(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "Introduction.\n\nTarget passage.\n")
	commentID := insertAnchoredComment(t, service, artifactID, "Target passage")

	holder, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder transaction: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := store.LockOwner(ctx, holder, store.IssueOwner("DOC-1")); err != nil {
		t.Fatalf("hold the owner lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, backfillErr := service.BackfillAnchorBlocks(ctx)
		done <- backfillErr
	}()

	select {
	case backfillErr := <-done:
		if backfillErr != nil {
			t.Fatalf("backfill anchor blocks: %v", backfillErr)
		}
		blockID := storedBlockID(t, service, commentID)
		t.Fatalf("the backfill finished while another writer held the owner lock of the row it wrote, "+
			"and the row's block_id is now %v: it writes comments and asks on the pool under no owner "+
			"lock (docs.backfillAnchorBlocks), so a serving writer holding that owner does not exclude "+
			"it. Take the owner lock of each row before the update that writes it.", blockID)
	case <-time.After(3 * time.Second):
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the owner lock: %v", err)
	}
	select {
	case backfillErr := <-done:
		if backfillErr != nil {
			t.Fatalf("backfill anchor blocks after the lock was released: %v", backfillErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the backfill never finished after the owner lock was released")
	}

	wantBlockID, err := service.BlockForQuote(ctx, artifactID, "Target passage")
	if err != nil {
		t.Fatalf("resolve target block: %v", err)
	}
	if blockID := storedBlockID(t, service, commentID); blockID != wantBlockID {
		t.Fatalf("the comment's block_id = %v, want %q: the backfill must still pin the block once it "+
			"holds the lock", blockID, wantBlockID)
	}
}
