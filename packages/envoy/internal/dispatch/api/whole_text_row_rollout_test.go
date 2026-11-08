package api

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// currentWindowsOf counts the windows of kind an owner holds that the retirement arm reads as
// current (docs' wholeTextOwner.retire): a vector, and that vector taken at the text the window
// holds now.
func currentWindowsOf(t *testing.T, database *store.Store, kind, ownerKey string) int {
	t.Helper()
	var current int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from embeddings
		where kind = $1 and split_part(id, '#', 1) = $2
		  and embedding is not null and embedded_hash is not null and embedded_hash = content_hash
	`, kind, ownerKey).Scan(&current); err != nil {
		t.Fatalf("count the current windows of %s %s: %v", kind, ownerKey, err)
	}
	return current
}

// wholeTextRowsOf counts the whole-text embeddings rows of a comment: the one row the release
// before this one's trigger wrote and its meaning search reads.
func wholeTextRowsOf(t *testing.T, database *store.Store, commentID string) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from embeddings where kind = 'comment' and id = $1`, commentID).Scan(&rows); err != nil {
		t.Fatalf("count the whole-text rows of comment %s: %v", commentID, err)
	}
	return rows
}

// reconcileBlockIndexPass runs one pass of the block-index reconciliation - the tick the server
// runs every docs.BlockIndexReconcileInterval, here called straight so the test waits for
// nothing - and answers what it did.
func reconcileBlockIndexPass(t *testing.T, deps Deps) docs.BlockIndexTick {
	t.Helper()
	tick, err := deps.Docs.(*docs.Service).ReconcileBlockIndex(context.Background(), docs.BlockIndexCursors{}, nil)
	if err != nil {
		t.Fatalf("one pass of the block-index reconciliation: %v", err)
	}
	return tick
}

// TestAWindowedItemKeepsItsWholeTextRowUntilItsWindowIsCurrent proves the rule a rolling release
// rests on. While a release overlaps two builds, the older one's meaning search reads a
// whole-text embeddings row per item (kind comment) and the newer one reads the windows of the
// blocks it writes (kind comment_block), so the newer build retires a whole-text row only once
// its owner holds a window of the matching kind whose vector is current: the row the older
// build's reader depends on stays, alive and current, while the window is not current, and goes
// once it is. An item therefore leaves the older build's meaning search only once the newer
// build can find it.
//
// This is the row "a windowed item keeps the whole-text row the older build's meaning search
// reads until its window is current", which drove no browser at
// packages/dispatch/e2e/two-builds.multiprocess.e2e.ts until INFRA-1554 moved it here, where the
// meaning search can be driven and neither the 30-second block-index tick nor the embed queue's
// scans are waited out: ReconcileBlockIndex and the queue's batches are called directly, in the
// row's own order. docs/block_index_test.go's TestRetirementWaitsForACurrentWindow is its
// neighbour at the layer below, which proves the retirement arm's own rule without a search.
func TestAWindowedItemKeepsItsWholeTextRowUntilItsWindowIsCurrent(t *testing.T) {
	const query = "wibbleflorp"
	const body = "The canary pours the astrolabe over the quiet wharf.\n\nA second paragraph."
	ctx := context.Background()
	embedder := &fakeEmbedder{vectors: map[string][]float32{query: queryVector()}}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "ROLL", "Two builds at once", "Body text.")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": body}, "alice")
	windowKind := refs.ItemComment.WindowKind()
	owner := refs.ItemComment.WindowOwnerKey(comment.ID)

	// The window first, and stale before the whole-text row exists. The retirement arm deletes a
	// whole-text row whenever its owner holds one current window, and does not ask whether that
	// row is embedded, so a run that enqueued the row first would be racing its own queue
	// against the arm. With no window current, the arm cannot take the row, so what follows is
	// decided by the queue alone.
	embedWindowsAs(t, database, embedder, windowKind, owner, angledVector(0.9, 2))
	processAllPending(t, database, embedder)
	if current := currentWindowsOf(t, database, windowKind, owner); current != 1 {
		t.Fatalf("current windows of %s = %d, want the comment's one embedded window", owner, current)
	}

	// A window waiting for its vector: the state every item is in between this build's write and
	// the queue's pass over it. dead keeps the queue from embedding it again while the retirement
	// arm is given its chance; the vector it already holds stays, as a stale window's does, so
	// the newer build still finds the comment through it.
	if _, err := database.Pool.Exec(ctx, `
		update embeddings set embedded_hash = null, dead = true
		where kind = $1 and split_part(id, '#', 1) = $2
	`, windowKind, owner); err != nil {
		t.Fatalf("make the comment's window stale: %v", err)
	}
	if current := currentWindowsOf(t, database, windowKind, owner); current != 0 {
		t.Fatalf("current windows of %s = %d after the window was made stale, want none", owner, current)
	}

	// The row the previous build's trigger wrote for this comment, which this build no longer
	// writes at a write (migration 20261102233627 dropped that trigger): the same call the
	// trigger made, over the body the comment holds.
	if _, err := database.Pool.Exec(ctx, `
		select embeddings_enqueue('comment', c.id::text, c.body) from comments c where c.id = $1::uuid
	`, comment.ID); err != nil {
		t.Fatalf("write the whole-text row of comment %s: %v", comment.ID, err)
	}
	processAllPending(t, database, embedder)

	// Two passes of the reconciliation over it, and the row the older build reads is still
	// there: alive, with its vector and the comment's own text.
	for pass := 1; pass <= 2; pass++ {
		if tick := reconcileBlockIndexPass(t, deps); tick.Retired != 0 {
			t.Fatalf("pass %d with no current window retired %d whole-text rows (%+v), want none", pass, tick.Retired, tick)
		}
	}
	var alive, embedded, atItsText, holdsTheBody bool
	if err := database.Pool.QueryRow(ctx, `
		select not dead, embedding is not null, embedded_hash = content_hash,
		       text_snapshot = (select body from comments where id = $2::uuid)
		from embeddings where kind = 'comment' and id = $1
	`, comment.ID, comment.ID).Scan(&alive, &embedded, &atItsText, &holdsTheBody); err != nil {
		t.Fatalf("read the whole-text row of comment %s after two passes: %v", comment.ID, err)
	}
	if !alive || !embedded || !atItsText || !holdsTheBody {
		t.Fatalf("the older build's meaning row after two passes: not dead = %t, has a vector = %t, embedded at its text = %t, holds the comment's body = %t; want all four",
			alive, embedded, atItsText, holdsTheBody)
	}

	// The hit the newer build already serves for the same comment, so both builds find it while
	// the row is there. The query shares no word with the comment, so only the meaning leg can
	// answer it.
	requireNoKeywordHit(t, "q="+query)
	response := searchResponse(t, handler, "q="+query)
	if response.Degraded != "" {
		t.Fatalf("Degraded = %q while the whole-text row stands, want empty", response.Degraded)
	}
	searchHit(t, response, "comment", comment.ID)

	// Once the window is current again, the first pass after that takes the whole-text row.
	if _, err := database.Pool.Exec(ctx, `
		update embeddings set dead = false, embedded_hash = content_hash
		where kind = $1 and split_part(id, '#', 1) = $2
	`, windowKind, owner); err != nil {
		t.Fatalf("make the comment's window current again: %v", err)
	}
	if current := currentWindowsOf(t, database, windowKind, owner); current != 1 {
		t.Fatalf("current windows of %s = %d once the window was embedded at its text, want one", owner, current)
	}
	if tick := reconcileBlockIndexPass(t, deps); tick.Retired != 1 {
		t.Fatalf("the first pass behind a current window retired %d whole-text rows (%+v), want the comment's one", tick.Retired, tick)
	}
	if rows := wholeTextRowsOf(t, database, comment.ID); rows != 0 {
		t.Fatalf("whole-text rows of comment %s behind a current window = %d, want none", comment.ID, rows)
	}
}
