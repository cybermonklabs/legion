package docs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

func TestBackfillAnchorBlocksPinsResolvableLegacyRowsOnce(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "Introduction.\n\nTarget passage.\n")
	askID := insertAnchoredAsk(t, service, artifactID, "Target passage")
	commentID := insertAnchoredComment(t, service, artifactID, "Target passage")

	first, err := service.BackfillAnchorBlocks(context.Background())
	if err != nil {
		t.Fatalf("backfill anchor blocks: %v", err)
	}
	if first.Asks != 1 || first.Comments != 1 || first.Skipped != 0 {
		t.Fatalf("first backfill = %#v, want both rows updated", first)
	}

	wantBlockID, err := service.BlockForQuote(context.Background(), artifactID, "Target passage")
	if err != nil {
		t.Fatalf("resolve target block: %v", err)
	}
	for _, row := range []struct {
		id    string
		table string
	}{{askID, "asks"}, {commentID, "comments"}} {
		var encoded []byte
		if err := service.store.Pool.QueryRow(context.Background(), "select anchor from "+row.table+" where id = $1", row.id).Scan(&encoded); err != nil {
			t.Fatalf("load %s anchor: %v", row.table, err)
		}
		var anchor model.Anchor
		if err := json.Unmarshal(encoded, &anchor); err != nil {
			t.Fatalf("decode %s anchor: %v", row.table, err)
		}
		if anchor.BlockID == nil || *anchor.BlockID != wantBlockID {
			t.Fatalf("%s block_id = %v, want %q", row.table, anchor.BlockID, wantBlockID)
		}
	}

	second, err := service.BackfillAnchorBlocks(context.Background())
	if err != nil {
		t.Fatalf("repeat backfill anchor blocks: %v", err)
	}
	if second.Asks != 0 || second.Comments != 0 || second.Skipped != 0 {
		t.Fatalf("second backfill = %#v, want idempotent no-op", second)
	}
}

// A settlement's anchor refresh owns two fields of an anchor, the quote and the orphan flag. It
// reads each open row's anchor and writes it back, so a write of the whole column erases whatever
// another writer put in that anchor after the refresh read it - here the block id the one-time
// BackfillAnchorBlocks repair pins. The advisory lock holds the settlement in its update of the
// asks, after it read the rows and before it updates the comments, which is the window a loaded
// CI runner hit on its own.
//
// The shape of the test changed with INFRA-636's ruling that the backfill takes the owner lock of
// each row it writes. Before it, the backfill wrote on the pool, so it could pin the comment
// inside that window and the test ran it there, under the name it had then
// ("...KeepsABlockIDPinnedAfterItReadTheRow"); now both writers take the row's owner lock - the
// settlement through its version write (lockArtifactOwner), the backfill through
// refs.LockItemOwner - so the interleaving cannot happen at all and the backfill parks on that
// lock until the settlement commits. What the test proves is that they serialise on the owner and
// that both writes stand: the refresh clears the orphan flag, the backfill pins the block, and
// the row carries both at the end.
func TestAnchorRefreshAndBackfillSerialiseOnTheRowsOwner(t *testing.T) {
	const holdKey = int64(4815162342)
	database, service, artifactID, recorder := tracedTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Introduction.\n\nTarget passage.\n\nOther passage.\n")
	askID := insertAnchoredAsk(t, service, artifactID, "Other passage")
	commentID := insertAnchoredComment(t, service, artifactID, "Target passage")

	// Both rows are stale: their marks are live but their anchors say orphaned, so the refresh
	// has a reason to write each one. The ask's block is already pinned, which is what keeps the
	// backfill off the row the settlement locks first.
	if _, err := service.store.Pool.Exec(context.Background(), `
		update asks
		set anchor = jsonb_set(jsonb_set(anchor, '{orphaned}', to_jsonb(true)), '{block_id}', to_jsonb('already-pinned'::text), true)
		where id = $1
	`, askID); err != nil {
		t.Fatalf("stale the ask anchor: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		update comments set anchor = jsonb_set(anchor, '{orphaned}', to_jsonb(true)) where id = $1
	`, commentID); err != nil {
		t.Fatalf("stale the comment anchor: %v", err)
	}
	wantBlockID, err := service.BlockForQuote(context.Background(), artifactID, "Target passage")
	if err != nil {
		t.Fatalf("resolve target block: %v", err)
	}

	if _, err := service.store.Pool.Exec(context.Background(), `
		create function dispatch_test_hold_anchor_refresh() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock(4815162342);
			return new;
		end;
		$$
	`); err != nil {
		t.Fatalf("create anchor refresh hold function: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		create trigger dispatch_test_hold_anchor_refresh
		before update on asks for each row
		when (new.anchor is distinct from old.anchor)
		execute function dispatch_test_hold_anchor_refresh()
	`); err != nil {
		t.Fatalf("create anchor refresh hold trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = service.store.Pool.Exec(context.Background(), `drop trigger if exists dispatch_test_hold_anchor_refresh on events`)
		_, _ = service.store.Pool.Exec(context.Background(), `drop function if exists dispatch_test_hold_anchor_refresh()`)
	})

	// A content change is what makes the settlement write a version, and a version write is what
	// refreshes the anchors.
	editLiveTree(t, service, artifactID, replaceRun("Introduction.", "Intro."))
	generation := roomGeneration(t, service, artifactID)

	hold, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin anchor refresh hold: %v", err)
	}
	defer hold.Rollback(context.Background())
	if _, err := hold.Exec(context.Background(), `select pg_advisory_xact_lock($1)`, holdKey); err != nil {
		t.Fatalf("hold the anchor refresh: %v", err)
	}

	// The settlement reads the document's owner first in the transaction that then refreshes the
	// anchors, so that read names its backend, and the wait is for that backend behind the hold:
	// another session waiting on the hold's key does not pass for the settlement.
	recorder.reset()
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		service.settleRoom(artifactID, generation)
	}()
	settlement := recorder.connOf(t, artifactID, ownerReadStatement, false).PgConn().PID()
	if _, early := storetest.WaitForQueuedBehind(t, hold, settlement, settled); early {
		t.Fatal("the settlement returned before it reached its anchor refresh")
	}

	// The backfill runs beside the held settlement, in a goroutine, because it no longer runs
	// past it: it takes the comment's owner lock, which the settlement's version write holds
	// (lockArtifactOwner, refs.LockItemOwner), so it parks there until the settlement commits.
	// Its own wait is the proof: its backend, named by its read of the comment's owner
	// (refs.LockItemOwner's first statement), queued behind the settlement's.
	type backfillResult struct {
		report AnchorBlockBackfill
		err    error
	}
	recorder.reset()
	backfilled := make(chan backfillResult, 1)
	go func() {
		report, err := service.BackfillAnchorBlocks(context.Background())
		backfilled <- backfillResult{report: report, err: err}
	}()
	backfiller := recorder.connOf(t, commentID, "select issue_key, artifact_id::text from comments", false).PgConn().PID()
	if result, early := storetest.WaitForQueuedBehindBackend(t, database, settlement, backfiller, backfilled); early {
		t.Fatalf("the backfill returned (%#v, %v) before it queued behind the settlement's owner lock", result.report, result.err)
	}

	if err := hold.Rollback(context.Background()); err != nil {
		t.Fatalf("release the anchor refresh: %v", err)
	}
	<-settled

	result := testwait.Receive(t, "the backfill to finish once the settlement released the comment's owner lock", backfilled)
	if result.err != nil {
		t.Fatalf("backfill anchor blocks after the settlement: %v", result.err)
	}
	backfill := result.report
	if backfill.Asks != 0 || backfill.Comments != 1 {
		t.Fatalf("backfill after the settlement = %#v, want the comment alone", backfill)
	}

	var encoded []byte
	if err := service.store.Pool.QueryRow(context.Background(), `select anchor from comments where id = $1`, commentID).Scan(&encoded); err != nil {
		t.Fatalf("load comment anchor: %v", err)
	}
	var anchor model.Anchor
	if err := json.Unmarshal(encoded, &anchor); err != nil {
		t.Fatalf("decode comment anchor: %v", err)
	}
	if anchor.BlockID == nil || *anchor.BlockID != wantBlockID {
		t.Fatalf("comment block_id after the settlement = %v, want %q", anchor.BlockID, wantBlockID)
	}
	if anchor.Orphaned {
		t.Fatalf("comment anchor stayed orphaned, so the settlement never refreshed it")
	}
}

func TestBlockForQuoteLeavesTopLevelCrossBlockAnchorsUnpinned(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "One.\n\nTwo.\n")

	blockID, err := service.BlockForQuote(context.Background(), artifactID, "One. Two.")
	if err != nil {
		t.Fatalf("resolve cross-block quote: %v", err)
	}
	if blockID != "" {
		t.Fatalf("cross-block quote block_id = %q, want empty", blockID)
	}
}
