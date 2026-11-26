package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestATickLocksNoOwnerOfACommentThatNamesANode holds the block-index tick's read of the events
// after its build's cursor (readEventsAfterCursor) to the owner locks it takes: those of the
// comments the events name whose row names no node, and no other. Every tick reads the event of
// each comment written since its cursor, and the row of a comment this build writes names its node
// from the write that stores it, so a tick that locked the owner of every comment its page names
// would wait for, and hold up, the writers of every issue a recent comment was written on, though
// it has nothing to write for any of those comments.
//
// The comment is written after this build's cursor as the release before this one writes one
// (commentAsThePreviousRelease), and then given its node by the write the tick and the backfill
// give one (refs.CommentsNamingNoNode, refs.CommentWrite's Queue), so its row names its node while
// its comment.created is still unread. The test then holds the owner lock of the comment's issue,
// CORE-1, and runs the tick's read: it must answer, having read that event and written no comment,
// with no backend waiting on an owner lock.
func TestATickLocksNoOwnerOfACommentThatNamesANode(t *testing.T) {
	t.Parallel()
	database, service := blockIndexService(t)
	ctx := context.Background()

	// This build's first tick that reads a proven horizon starts the build's cursor at it, so the
	// comment's event below is after the cursor.
	appendIssueEvent(t, database)
	awaitedBlockIndexTick(t, service)
	cursor := blockIndexEventCursor(t, database, "")
	comment, created := commentAsThePreviousRelease(t, database, model.Actor{Kind: "user", ID: "Bob"}, "",
		"A remark whose row names its node before the tick reads its event.")
	if created <= cursor {
		t.Fatalf("the comment's event %d is not after this build's cursor %d, so this test proves nothing", created, cursor)
	}
	giveTheCommentItsNode(t, database, comment, created)
	if !commentNamesItsNode(t, database, comment) {
		t.Fatalf("comment %s names no node after its write, so this test proves nothing", comment)
	}
	horizon, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if _, err := service.blockIndexHorizon().Await(horizon); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder of issue CORE-1's owner lock: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := store.LockOwner(ctx, holder, store.IssueOwner("CORE-1")); err != nil {
		t.Fatalf("hold issue CORE-1's owner lock: %v", err)
	}

	type answer struct {
		tick BlockIndexTick
		err  error
	}
	answered := make(chan answer, 1)
	go func() {
		var tick BlockIndexTick
		_, _, err := service.readEventsAfterCursor(ctx, &tick)
		answered <- answer{tick, err}
	}()
	deadline := time.Now().Add(time.Minute)
	for {
		select {
		case got := <-answered:
			if got.err != nil {
				t.Fatalf("the tick's read of the events after its build's cursor: %v", got.err)
			}
			if got.tick.Events == 0 || got.tick.Comments != 0 {
				t.Errorf("the tick read %d event(s) and gave %d comment(s) their nodes, want the comment's event read and no comment written: its row names its node already",
					got.tick.Events, got.tick.Comments)
			}
			return
		default:
		}
		if waiting, _ := storetest.OwnerLockWaiters(t, database); waiting > 0 {
			if err := holder.Rollback(ctx); err != nil {
				t.Fatalf("release issue CORE-1's owner lock: %v", err)
			}
			got := <-answered
			t.Fatalf("the tick waited on an owner lock while this test held issue CORE-1's, whose one comment the events after the cursor name already names its node (once released it read %d event(s), gave %d comment(s) their nodes, error %v): it locks the owner of a comment it has nothing to write",
				got.tick.Events, got.tick.Comments, got.err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tick's read of the events after its build's cursor neither answered nor waited on an owner lock within a minute")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// giveTheCommentItsNode gives comment its node, the facts its row projects and who wrote it, with
// event, the event that stored it, as their cause, in a transaction of its own and under the
// comment's owner lock, as the tick and the backfill give a comments row that names no node its
// node (refs.LockCommentOwners, refs.CommentsNamingNoNode, refs.CommentWrite's Queue).
func giveTheCommentItsNode(t *testing.T, database *store.Store, comment string, event int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write of comment %s's node: %v", comment, err)
	}
	defer tx.Rollback(ctx)
	ids := []string{comment}
	if err := refs.LockCommentOwners(ctx, tx, ids); err != nil {
		t.Fatalf("lock the owner of comment %s: %v", comment, err)
	}
	write, err := refs.CommentsNamingNoNode(ctx, tx, ids, []int64{event})
	if err != nil {
		t.Fatalf("read comment %s's row: %v", comment, err)
	}
	if write.Comments() != 1 {
		t.Fatalf("the write of comment %s's node names %d comment(s), want that one", comment, write.Comments())
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		write.Queue(b)
		return nil
	}); err != nil {
		t.Fatalf("write comment %s's node: %v", comment, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit comment %s's node: %v", comment, err)
	}
}
