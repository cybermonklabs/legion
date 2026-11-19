package grapherase

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// ownerLockStatement is how the statement that takes an issue's owner lock starts, the erase's own
// (events.LockOwnerRows) and the one docs.ReadErasedLiveText takes it with (lockArtifactOwner)
// alike: store.LockOwners' advisory lock.
var ownerLockStatement = lockstep.OwnerLockStatement

// untouchedBeforeTheNode are the tables the erase reads nothing of before it holds the node's row:
// the tuples, the record of the erase and the event log, which it reads and writes only once it
// has read the node under its locks. Before then it reads the node's projection rows and its
// document alone, to learn their owner rows (ownerRows).
var untouchedBeforeTheNode = []string{"graph_tuples", "graph_erasures", "events"}

// commentLockStatement is how the statement starts that takes the erased comment's own comments
// row, `for update`, right after its node row (lockProjectionRows).
const commentLockStatement = "select 1 from comments where node_kind = $1 and node_address = $2 order by id for update"

// TestTheEraseTakesTheStoreLockOrder is the erase's half of the check
// c-lock-order. `envoy-dispatch erase` deletes graph_tuples and graph_nodes rows through
// graphstore.EraseNode (apply), which graphstore's TestEveryStoreWriteCallSiteIsListed lists as
// grapherase/erase.go#apply and leaves the proof of its lock steps to this package.
//
// The run erases the comment seedAgreeingStore writes on a document of issue CORE-1, whose text a
// version of a document of a second issue, ACME-1, quotes. That version derives from ACME-1's
// document and not from the comment, so the run lists it and leaves it whole, and it writes under
// one owner, the comment's own, CORE-1: it never queues behind ACME-1's owner lock, which is held
// for the whole run. It takes CORE-1's owner lock, then the comment's node row, then its comments
// row, `for update`, before it reads under its locks or writes anything (lockProjectionRows).
//
// Each of those locks is held in a transaction of its own while the real Erase runs on its own
// connection (followTheErase), after the run without --confirm that printed its digest, which
// waits on none of them. The test lets go of each hold the run queues behind, one at a time,
// reads where the run waits and the tables it holds a lock on each time
// (lockstep.RequireStoppedAt), and holds the order it queued in to the store's lock order
// (lockstep.RequireTheStoreLockOrder): every owner lock before the node row, the node row before
// the projection row, and so no owner lock after the node row. It then reads what the run removed
// and what it left, so the run it followed is the one that committed.
func TestTheEraseTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedAgreeingStore(t, database)
	quoting := seedQuotingDocument(t, database, "ACME")
	quoteTheComment(t, database, quoting, seeded.comment)

	// While the run waits at an owner lock it has read the node's projection rows and its
	// document, and no more: the node row comes next and everything it writes after that. While
	// it waits at the node row it holds the owner lock and has still read nothing of the tuples or
	// the event log.
	acme := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue ACME-1's owner lock", Step: lockstep.StepOwner, Kind: "issue", ID: "ACME-1",
		Statement: ownerLockStatement,
	}, store.IssueOwner("ACME-1"))
	core := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue CORE-1's owner lock", Step: lockstep.StepOwner, Taken: true, Kind: "issue", ID: "CORE-1",
		Statement: ownerLockStatement,
		Unlocked:  append([]string{"graph_nodes"}, untouchedBeforeTheNode...),
	}, store.IssueOwner("CORE-1"))
	node := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the comment's node row", Step: lockstep.StepNode, Taken: true, Kind: commentNodeKind, ID: seeded.comment,
		Statement: "select supertag from graph_nodes where kind = $1 and id = $2 for update",
		Unlocked:  untouchedBeforeTheNode,
	}, `select 1 from graph_nodes where kind = $1 and id = $2 for update`, commentNodeKind, seeded.comment)
	projection := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the comment's comments row", Step: lockstep.StepProjection, Taken: true, Names: node,
		Statement: commentLockStatement,
		Unlocked:  untouchedBeforeTheNode,
	}, `select 1 from comments where id = $1::uuid for update`, seeded.comment)

	erasure := followTheErase(t, database, commentNodeKind, seeded.comment, []*lockstep.Hold{acme, core, node, projection})
	requireTheQuoteLeft(t, database, erasure, seeded.comment, quoting)
}

// TestTheEraseTakesTheCommentsRowBeforeItTouchesItsWindows holds the erase to taking the comment's
// own comments row right after its node row, before it writes anything (lockProjectionRows), which
// is what orders it against the block index tick's window arm. That arm takes no owner row: it
// takes the comment's row `for no key update` and then writes the comment's windows and their
// window state (refs.WriteItemWindows), keys the erase deletes (forgetEmbeddings).
//
// The comment is indexed and windowed first, as the arm leaves it. A transaction stands for the
// arm: it holds the comment's row `for no key update` while the erase starts, and the test reads
// that the erase waits at the statement that takes that row, holding no write's lock on the
// embedding store, so it has deleted no window and no window state. The holder then runs the
// arm's write of the comment's windows and commits, and the erase goes on, removes the comment
// and its windows with it, and commits. An erase that took the row only where it deletes it would
// wait there holding the rows it deleted from the embedding store, which the arm's write then
// waits for: one of the two ends `deadlock detected`.
func TestTheEraseTakesTheCommentsRowBeforeItTouchesItsWindows(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	windowKind, ownerKey := refs.ItemComment.WindowKind(), refs.ItemComment.WindowOwnerKey(seeded.comment)
	windowTheComment(t, database, seeded.comment)
	if windows := itemWindows(t, database, windowKind, ownerKey); windows == 0 {
		t.Fatalf("the comment holds no window to erase, so this test would prove nothing")
	}

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the window arm's transaction: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `select 1 from comments where id = $1::uuid for no key update`, seeded.comment); err != nil {
		t.Fatalf("hold comment %s's row as the window arm does: %v", seeded.comment, err)
	}
	pid, answered := startErase(t, database, commentNodeKind, seeded.comment)
	if early, done := storetest.WaitForQueuedBehind(t, holder, pid, answered, lockstep.Wait); done {
		t.Fatalf("the erase answered (err %v) without waiting on the comment's row, which the window arm held", early.err)
	}
	waited := strings.ToLower(lockstep.WaitingStatement(t, database, pid))
	written := tablesWrittenBy(t, ctx, database, pid)
	t.Logf("the erase waits behind the window arm at %q, holding more than a read's lock on %v", waited, written)
	if !strings.HasPrefix(waited, commentLockStatement) {
		t.Errorf("the erase waits behind the window arm at %q, which is not the statement that takes the comment's row right after its node row (it starts %q)",
			waited, commentLockStatement)
	}
	for _, table := range []string{"embeddings", "embedding_window_states"} {
		if slices.Contains(written, table) {
			t.Errorf("the erase waits at the comment's row holding a write's lock on %s: it deleted rows the window arm writes before it took the row the arm holds (it holds more than a read's lock on %v)",
				table, written)
		}
	}

	if windowed, err := refs.WriteItemWindows(ctx, holder, refs.ItemComment, seeded.comment); err != nil || !windowed {
		t.Fatalf("the window arm's write of the comment's windows, while the erase waited = %v, %v; want it written", windowed, err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the window arm's write: %v", err)
	}
	var answer eraseAnswer
	select {
	case answer = <-answered:
	case <-time.After(lockstep.Wait):
		t.Fatalf("the erase did not answer within %s of the window arm's commit", lockstep.Wait)
	}
	if answer.err != nil {
		t.Fatalf("the erase failed once the window arm committed: %v", answer.err)
	}
	if !answer.erasure.Applied {
		t.Fatalf("the erase removed nothing once the window arm committed: %+v", answer.erasure)
	}
	if windows := itemWindows(t, database, windowKind, ownerKey); windows != 0 {
		t.Errorf("after the erase the comment still has %d window row(s) and window state row(s), want none", windows)
	}
}

// windowTheComment indexes the blocks of comment and writes its windows, committed, as the block
// index tick leaves a comment: the index of its body (refs.IndexItem), then the window arm's write
// (refs.WriteItemWindows).
func windowTheComment(t *testing.T, database *store.Store, comment string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the comment's index: %v", err)
	}
	defer tx.Rollback(ctx)
	var body string
	if err := tx.QueryRow(ctx, `select body from comments where id = $1::uuid for no key update`, comment).Scan(&body); err != nil {
		t.Fatalf("read the body of comment %s: %v", comment, err)
	}
	if _, err := refs.IndexItem(ctx, tx, refs.ItemComment, comment, body, time.Now(), nil, nil); err != nil {
		t.Fatalf("index the blocks of comment %s: %v", comment, err)
	}
	if windowed, err := refs.WriteItemWindows(ctx, tx, refs.ItemComment, comment); err != nil || !windowed {
		t.Fatalf("window comment %s = %v, %v; want its windows written", comment, windowed, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the comment's index: %v", err)
	}
}

// itemWindows counts the window rows of one item and the window state row kept beside them.
func itemWindows(t *testing.T, database *store.Store, windowKind, ownerKey string) int64 {
	t.Helper()
	var windows int64
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from embeddings where kind = $1 and split_part(id, '#', 1) = $2)
		     + (select count(*) from embedding_window_states where kind = $1 and owner_key = $2)
	`, windowKind, ownerKey).Scan(&windows); err != nil {
		t.Fatalf("count the windows of %s: %v", ownerKey, err)
	}
	return windows
}

// tablesWrittenBy names every table backend pid holds a table lock on stronger than a read's - the
// ACCESS SHARE of a select and the ROW SHARE of a select that locks rows - in name order, which for
// a run stopped on a lock is every table its transaction has inserted into, updated or deleted
// from so far. The tuple lock a backend takes on the row it waits for is no table lock, and is left
// out.
func tablesWrittenBy(t *testing.T, ctx context.Context, database *store.Store, pid uint32) []string {
	t.Helper()
	rows, err := database.Pool.Query(ctx, `
		select distinct c.relname::text from pg_locks l
		join pg_class c on c.oid = l.relation
		where l.pid = $1 and l.locktype = 'relation' and c.relkind in ('r', 'p')
		  and l.mode not in ('AccessShareLock', 'RowShareLock')
		order by 1`, pid)
	if err != nil {
		t.Fatalf("read the tables backend %d has written: %v", pid, err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the tables backend %d has written: %v", pid, err)
	}
	return tables
}

// blockLockStatement is how the statement starts that takes an erased block's own document_blocks
// rows, `for update`, right after its node row (lockProjectionRows).
const blockLockStatement = "select 1 from document_blocks where node_kind = $1 and node_address = $2 order by artifact_id, block_id for update"

// TestTheEraseTakesABlocksRowsBeforeItTouchesItsText holds the erase of a block node to taking the
// block's own projection rows right after its node row (lockProjectionRows), the path a comment's
// erase does not take. A block projects onto its document_blocks row and onto the block_refs rows
// out of it, and its whole text stands in document_block_texts, which carries a foreign key to
// artifacts alone and which the erase deletes before either (eraseBlockTexts).
//
// The block is seedAgreeingStore's inner block, which holds the document's one block reference,
// with its text written into document_block_texts as a version's index writes it. A transaction
// holds the block's document_blocks row `for update`, as the block index writers under the
// revision fence take it (graphcensus's heldBlockRows), and the erase starts: the test reads that
// it waits at the statement that takes that row having written nothing, so no block text and no
// embedding row. Once the row is let go the erase removes the block whole: its document_blocks
// row, its block_refs row, its block text, every tuple out of it and into it, and its node.
func TestTheEraseTakesABlocksRowsBeforeItTouchesItsText(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	artifact := seeded.document.artifactID
	if tag, err := database.Pool.Exec(ctx, `
		insert into document_block_texts (artifact_id, block_id, version, text)
		select artifact_id, block_id, changed_version, excerpt from document_blocks
		where artifact_id = $1::uuid and block_id = 'inner'
	`, artifact); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write the text of block %s: %d rows, %v", seeded.inner, tag.RowsAffected(), err)
	}
	if before := blockHeld(t, database, artifact, "inner", seeded.inner); before.rows == 0 || before.refs == 0 ||
		before.texts == 0 || before.tuples == 0 || before.nodes == 0 {
		t.Fatalf("the store holds %+v of block %s, want its document_blocks row, a block_refs row, its text, tuples and its node, so this test would prove nothing",
			before, seeded.inner)
	}

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder of the block's document_blocks row: %v", err)
	}
	defer holder.Rollback(ctx)
	if tag, err := holder.Exec(ctx, `
		select 1 from document_blocks where artifact_id = $1::uuid and block_id = 'inner' for update
	`, artifact); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("hold the document_blocks row of block %s: %d rows, %v", seeded.inner, tag.RowsAffected(), err)
	}
	pid, answered := startErase(t, database, refs.BlockNodeKind, seeded.inner)
	if early, done := storetest.WaitForQueuedBehind(t, holder, pid, answered, lockstep.Wait); done {
		t.Fatalf("the erase answered (err %v) without waiting on the block's document_blocks row, which another transaction held", early.err)
	}
	waited := strings.ToLower(lockstep.WaitingStatement(t, database, pid))
	written := tablesWrittenBy(t, ctx, database, pid)
	t.Logf("the erase waits behind the block's document_blocks row at %q, holding more than a read's lock on %v", waited, written)
	if !strings.HasPrefix(waited, blockLockStatement) {
		t.Errorf("the erase waits behind the block's document_blocks row at %q, which is not the statement that takes the block's rows right after its node row (it starts %q)",
			waited, blockLockStatement)
	}
	if len(written) > 0 {
		t.Errorf("the erase waits at the block's document_blocks row holding a write's lock on %v: it wrote before it took the row, where it should have written nothing yet, neither the block's text in document_block_texts nor a row of embeddings",
			written)
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("let go of the block's document_blocks row: %v", err)
	}
	var answer eraseAnswer
	select {
	case answer = <-answered:
	case <-time.After(lockstep.Wait):
		t.Fatalf("the erase did not answer within %s of the release of the block's document_blocks row", lockstep.Wait)
	}
	if answer.err != nil {
		t.Fatalf("the erase failed once the block's document_blocks row was let go: %v", answer.err)
	}
	if !answer.erasure.Applied {
		t.Fatalf("the erase removed nothing once the block's document_blocks row was let go: %+v", answer.erasure)
	}
	if after := blockHeld(t, database, artifact, "inner", seeded.inner); after != (heldBlock{}) {
		t.Errorf("after the erase the store holds %+v of block %s, want nothing", after, seeded.inner)
	}
}

// heldBlock is what the store and its projection hold of one block node: its document_blocks rows,
// the block_refs rows out of it, its texts, the tuples out of it and into it, and its node row.
type heldBlock struct {
	rows, refs, texts, tuples, nodes int64
}

// blockHeld reads what the store and its projection hold of block blockID of document artifact,
// whose node is address.
func blockHeld(t *testing.T, database *store.Store, artifact, blockID, address string) heldBlock {
	t.Helper()
	var held heldBlock
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from document_blocks where node_kind = $3 and node_address = $4),
		       (select count(*) from block_refs where node_kind = $3 and node_address = $4),
		       (select count(*) from document_block_texts where artifact_id = $1::uuid and block_id = $2),
		       (select count(*) from graph_tuples
		        where (from_kind = $3 and from_id = $4) or (to_kind = $3 and to_id = $4)),
		       (select count(*) from graph_nodes where kind = $3 and id = $4)
	`, artifact, blockID, refs.BlockNodeKind, address).Scan(&held.rows, &held.refs, &held.texts, &held.tuples, &held.nodes); err != nil {
		t.Fatalf("read what the store holds of block %s: %v", address, err)
	}
	return held
}

// TestTheEraseTakesTheOwnersOfABlockAndAWindowQuotingItBeforeItsNode holds the erase of a block to
// the owner rows of what it writes beyond the block's own rows, and to no owner row of what it
// leaves. The erased block is seedAgreeingStore's inner block, of a document of issue CORE-1. Its
// text stands in the excerpt of the outer block that holds it, which redactBlocks rewrites, and in
// a window of its own document covering it, which forgetEmbeddings deletes: both derive from the
// block, and both are written under CORE-1's row, which the erase takes before the block's node
// row, as it takes the block's own document_blocks row after it.
//
// The same text stands in a block's excerpt of a document of issue ACME-1 and in a window of a
// document of issue BETA-1. Those derive from their own documents, not from the block: the erase
// lists them and leaves them whole, and so never queues behind ACME-1's row, BETA-1's row or
// ACME-1's quoting block row, all held for the whole run.
//
// The outer block's row is held too, as a projection row the erase writes after the block's own:
// the run queues behind it last, at the statement that rewrites its excerpt. The test then reads
// that the outer excerpt holds the erase's marker where the text was and that the window is gone,
// so the run it followed is the one that wrote them, and that ACME-1's excerpt and BETA-1's
// window are as they were, each on the listing.
func TestTheEraseTakesTheOwnersOfABlockAndAWindowQuotingItBeforeItsNode(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	const text = "Block inner."
	// The outer block's excerpt is its own text cut short, and a container's text is the
	// document cut across the blocks inside it, so the inner block's text stands in it at a
	// line of its own, as the renderer writes it (refs.IndexBlocks).
	if tag, err := database.Pool.Exec(ctx, `
		update document_blocks set excerpt = 'Block outer holds' || chr(10) || $2 where artifact_id = $1::uuid and block_id = 'outer'
	`, seeded.document.artifactID, text); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write the outer block's excerpt around the inner block's text: %d rows, %v", tag.RowsAffected(), err)
	}
	quoteInAWindow(t, database, seeded.document.artifactID, "inner", text)
	excerpted := seedQuotingDocument(t, database, "ACME")
	windowed := seedQuotingDocument(t, database, "BETA")
	quoteInABlock(t, database, excerpted, text)
	quoteInAWindow(t, database, windowed.artifactID, "b1", text)

	ownerHold := func(issue string, taken bool) *lockstep.Hold {
		hold := &lockstep.Hold{
			Name: "issue " + issue + "'s owner lock", Step: lockstep.StepOwner, Taken: taken, Kind: "issue", ID: issue,
			Statement: ownerLockStatement,
		}
		if taken {
			hold.Unlocked = append([]string{"graph_nodes"}, untouchedBeforeTheNode...)
		}
		return lockstep.HoldOwner(t, database, hold, store.IssueOwner(issue))
	}
	acme, beta, core := ownerHold("ACME-1", false), ownerHold("BETA-1", false), ownerHold("CORE-1", true)
	node := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the inner block's node row", Step: lockstep.StepNode, Taken: true, Kind: refs.BlockNodeKind, ID: seeded.inner,
		Statement: "select supertag from graph_nodes where kind = $1 and id = $2 for update",
		Unlocked:  untouchedBeforeTheNode,
	}, `select 1 from graph_nodes where kind = $1 and id = $2 for update`, refs.BlockNodeKind, seeded.inner)
	inner := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the inner block's document_blocks row", Step: lockstep.StepProjection, Taken: true, Names: node,
		Statement: blockLockStatement,
		Unlocked:  untouchedBeforeTheNode,
	}, `select 1 from document_blocks where artifact_id = $1::uuid and block_id = 'inner' for update`, seeded.document.artifactID)
	outer := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the outer block's row, whose excerpt quotes the inner block", Step: lockstep.StepProjection, Taken: true,
		Statement: "update document_blocks held set excerpt =",
		Unlocked:  []string{"graph_erasures"},
	}, `select 1 from document_blocks where artifact_id = $1::uuid and block_id = 'outer' for update`, seeded.document.artifactID)
	quote := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the block row of " + excerpted.refKey + " quoting the same text", Step: lockstep.StepProjection,
	}, `select 1 from document_blocks where artifact_id = $1::uuid and block_id = 'quote' for update`, excerpted.artifactID)

	erasure := followTheErase(t, database, refs.BlockNodeKind, seeded.inner, []*lockstep.Hold{acme, beta, core, node, inner, outer, quote})

	if !erasure.Applied || len(erasure.Versions) != 0 || len(erasure.Documents) != 0 {
		t.Errorf("the erase answered applied %t, the versions %+v and the documents %+v, want it applied with neither: the text stands in no version and no stored history",
			erasure.Applied, erasure.Versions, erasure.Documents)
	}
	var outerExcerpt, quoteExcerpt string
	var ownWindow, otherWindow bool
	if err := database.Pool.QueryRow(ctx, `
		select (select excerpt from document_blocks where artifact_id = $1::uuid and block_id = 'outer'),
		       (select excerpt from document_blocks where artifact_id = $2::uuid and block_id = 'quote'),
		       exists (select 1 from embeddings where kind = $3 and split_part(id, '#', 1) = $1::uuid::text),
		       exists (select 1 from embeddings where kind = $3 and split_part(id, '#', 1) = $4::text)
	`, seeded.document.artifactID, excerpted.artifactID, refs.DocumentWindowKind, windowed.artifactID).Scan(
		&outerExcerpt, &quoteExcerpt, &ownWindow, &otherWindow); err != nil {
		t.Fatalf("read the blocks and the windows that quoted the block: %v", err)
	}
	if want := "Block outer holds\n" + erasure.marker(); outerExcerpt != want || ownWindow {
		t.Errorf("after the erase the outer excerpt reads %q and the document's own window stands: %t; want %q and no window: the run the test followed is not one that wrote them",
			outerExcerpt, ownWindow, want)
	}
	if quoteExcerpt != "Alice wrote: "+text || !otherWindow {
		t.Errorf("after the erase %s's excerpt reads %q and %s's window stands: %t; want %q and the window: the erase changed rows of other nodes",
			excerpted.refKey, quoteExcerpt, windowed.refKey, otherWindow, "Alice wrote: "+text)
	}
	for _, left := range [][]string{
		{"leave public.document_blocks", excerpted.artifactID, "excerpt"},
		{"leave public.embeddings", windowed.artifactID, "text_snapshot"},
	} {
		if !listed(erasure, left...) {
			t.Errorf("the listing holds no line with %q, so it does not name a row it left:\n%s", left, strings.Join(erasure.Listing, "\n"))
		}
	}
}

// TestTheEraseStartsAgainWhenItsTextMovesUnderAnOwnerItHasNotTaken holds lockInStoreOrder to what
// it does when the owners it read before locking them are not all the ones it writes under once
// it holds their locks. The run reads its owners while the comment stands on CORE-1's document,
// and is held at CORE-1's owner lock. The transaction holding that lock moves the comment onto
// ACME-1's document, ACME-1's owner lock is held, and that transaction commits, which lets
// CORE-1's lock go. No route moves a comment today; the statement stands for any write that puts
// a node under another owner between the run's two reads. Under its locks the run reads the
// comment under an owner it did not take. A run that went on would write the comment's rows under
// ACME-1 holding no lock on it. This one rolls back and starts again, so it queues behind
// ACME-1's owner lock at the start of its next try, having read nothing of the store, and once
// let go it erases the comment.
func TestTheEraseStartsAgainWhenItsTextMovesUnderAnOwnerItHasNotTaken(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	moved := seedQuotingDocument(t, database, "ACME")

	core := lockstep.HoldOwner(t, database, &lockstep.Hold{Name: "issue CORE-1's owner lock", Step: lockstep.StepOwner, Statement: ownerLockStatement},
		store.IssueOwner("CORE-1"))
	pid, answered := startErase(t, database, commentNodeKind, seeded.comment)
	if early, done := storetest.WaitForQueuedBehind(t, core.Tx, pid, answered, lockstep.Wait); done {
		t.Fatalf("the erase answered (err %v) without waiting on issue CORE-1's owner lock, which another transaction held", early.err)
	}
	lockstep.RequireStoppedAt(t, ctx, database, pid, "the erase", core)
	if tag, err := core.Tx.Exec(ctx, `
		update comments set artifact_id = $2::uuid, anchor = jsonb_build_object('artifact_id', $2::text) where id = $1::uuid
	`, seeded.comment, moved.artifactID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("move comment %s onto %s: %d rows, %v", seeded.comment, moved.refKey, tag.RowsAffected(), err)
	}
	acme := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue ACME-1's owner lock", Step: lockstep.StepOwner, Statement: ownerLockStatement,
		Unlocked: append([]string{"graph_nodes"}, untouchedBeforeTheNode...),
	}, store.IssueOwner("ACME-1"))
	if err := core.Tx.Commit(ctx); err != nil {
		t.Fatalf("commit the move of comment %s: %v", seeded.comment, err)
	}
	if early, done := storetest.WaitForQueuedBehind(t, acme.Tx, pid, answered, lockstep.Wait); done {
		t.Fatalf("the erase answered (err %v) without waiting on issue ACME-1's owner lock, the owner the comment moved under once the erase had read its owners",
			early.err)
	}
	lockstep.RequireStoppedAt(t, ctx, database, pid, "the erase", acme)
	acme.Release()

	var answer eraseAnswer
	select {
	case answer = <-answered:
	case <-time.After(lockstep.Wait):
		t.Fatalf("the erase did not answer within %s of the release of issue ACME-1's owner lock", lockstep.Wait)
	}
	if answer.err != nil {
		t.Fatalf("the erase failed once issue ACME-1's owner lock was let go: %v", answer.err)
	}
	requireTheCommentErased(t, database, answer.erasure, seeded.comment)
}

// quotingDocument is the one document of an issue of a project of its own, which quotes the
// comment seedAgreeingStore writes once a quote of it is written: issue is the issue's key.
type quotingDocument struct {
	issue, artifactID, refKey string
}

// seedQuotingDocument writes project, its issue <project>-1 and the issue's document, which holds
// no version and no block yet. A project's key below CORE has its issue's owner key sort before
// issue:CORE-1, the owner of the erased comment.
func seedQuotingDocument(t *testing.T, database *store.Store, project string) quotingDocument {
	t.Helper()
	ctx := context.Background()
	document := quotingDocument{issue: project + "-1"}
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ($1, $1, 'alice')
	`, project); err != nil {
		t.Fatalf("seed project %s: %v", project, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, 'Quoting', '{"kind":"user","id":"alice"}', 'U')
	`, document.issue, project); err != nil {
		t.Fatalf("seed issue %s: %v", document.issue, err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, $2, 'notes', 'notes.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`, document.issue, project).Scan(&document.artifactID, &document.refKey); err != nil {
		t.Fatalf("create %s's document: %v", document.issue, err)
	}
	return document
}

// quoteTheComment writes version 1 of document, whose markdown quotes the body of comment.
func quoteTheComment(t *testing.T, database *store.Store, document quotingDocument, comment string) {
	t.Helper()
	tag, err := database.Pool.Exec(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1::uuid, 1, 'Alice wrote: ' || body, '[{"kind":"user","id":"alice"}]'::jsonb
		from comments where id::text = $2
	`, document.artifactID, comment)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write the version of %s that quotes comment %s: %d rows, %v", document.refKey, comment, tag.RowsAffected(), err)
	}
}

// quoteInABlock writes one block row of document, naming no node and indexed from no version,
// whose excerpt quotes text, and no block text for it: the excerpt is the one place document
// holds the text.
func quoteInABlock(t *testing.T, database *store.Store, document quotingDocument, text string) {
	t.Helper()
	tag, err := database.Pool.Exec(context.Background(), `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, 'quote', $2::text || '#quote', 'paragraph', '', 'Alice wrote: ' || $3::text, 1, now())
	`, document.artifactID, document.refKey, text)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write the block of %s that quotes %q: %d rows, %v", document.refKey, text, tag.RowsAffected(), err)
	}
}

// quoteInAWindow writes one embedding window of the document artifactID, covering block, whose
// text quotes text.
func quoteInAWindow(t *testing.T, database *store.Store, artifactID, block, text string) {
	t.Helper()
	tag, err := database.Pool.Exec(context.Background(), `
		insert into embeddings (kind, id, text_snapshot, content_hash, covers)
		values ($1::text, $2::text || '#' || $3::text, 'Alice wrote: ' || $4::text, 'hash', array[$3::text])
	`, refs.DocumentWindowKind, artifactID, block, text)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write the window of %s that quotes %q: %d rows, %v", artifactID, text, tag.RowsAffected(), err)
	}
}

// requireTheQuoteLeft reads what the erase answered and what it left: it is applied and ended no
// version, the comment is gone (requireTheCommentErased), and version 1 of the quoting document,
// which derives from that document and not from the comment, stands as it was written and is on
// the listing as a row the run left.
func requireTheQuoteLeft(t *testing.T, database *store.Store, erasure *Erasure, comment string, quoting quotingDocument) {
	t.Helper()
	if !erasure.Applied || len(erasure.Versions) != 0 {
		t.Errorf("the erase answered applied %t and the versions %+v, want it applied and no version ended: %s's version is another node's text",
			erasure.Applied, erasure.Versions, quoting.refKey)
	}
	requireTheCommentErased(t, database, erasure, comment)
	var markdown string
	if err := database.Pool.QueryRow(context.Background(),
		`select markdown from artifact_versions where artifact_id = $1::uuid and number = 1`, quoting.artifactID).Scan(&markdown); err != nil {
		t.Fatalf("read version 1 of %s after the erase: %v", quoting.refKey, err)
	}
	if markdown != "Alice wrote: A remark on the spec." {
		t.Errorf("after the erase version 1 of %s reads %q, want it as it was written", quoting.refKey, markdown)
	}
	if !listed(erasure, "leave public.artifact_versions", "["+quoting.refKey+" version 1 of "+quoting.issue+"]", "markdown") {
		t.Errorf("the listing does not name version 1 of %s as a row it left:\n%s", quoting.refKey, strings.Join(erasure.Listing, "\n"))
	}
}

// requireTheCommentErased reads that the comment's node row and its comments row are gone, with
// one erasure naming the comment. The run a test followed is then the one that committed.
func requireTheCommentErased(t *testing.T, database *store.Store, erasure *Erasure, comment string) {
	t.Helper()
	if !erasure.Applied {
		t.Errorf("the erase of comment %s removed nothing", comment)
	}
	var nodeStands, commentStands bool
	var erasures int64
	if err := database.Pool.QueryRow(context.Background(), `
		select exists (select 1 from graph_nodes where kind = $1 and id = $2),
		       exists (select 1 from comments where id::text = $2),
		       (select count(*) from graph_erasures where node_kind = $1 and node_id = $2)
	`, commentNodeKind, comment).Scan(&nodeStands, &commentStands, &erasures); err != nil {
		t.Fatalf("read what the erase left: %v", err)
	}
	if nodeStands || commentStands || erasures != 1 {
		t.Errorf("after the erase the comment's node row stands: %t, its comments row: %t, and %d erasure(s) name it; want neither standing and one erasure: the run the test followed is not one that committed",
			nodeStands, commentStands, erasures)
	}
}

// eraseAnswer is what one run of Erase answered.
type eraseAnswer struct {
	erasure *Erasure
	err     error
}

// startErase runs Erase of node kind:id as an operator does: without --confirm first, on the
// test's goroutine, which takes no lock and so waits on no hold, and then with the digest that
// run printed off the test's goroutine, on the connection the erase opens for itself. It answers
// the confirmed run's backend process id and the channel its answer arrives on. The erase reads
// each document's live state through docs.ReadErasedLiveText, which takes a document's owner row
// and then its room's lock; the documents here hold no stored history, so the erase lists no
// document whose history it replaces. Its sweep of the display trees waits a millisecond, which
// no lock step here rests on.
func startErase(t *testing.T, database *store.Store, kind, id string) (uint32, <-chan eraseAnswer) {
	t.Helper()
	listing := dryRun(t, database, kind, id)
	answered := make(chan eraseAnswer, 1)
	go func() {
		erasure, err := Erase(context.Background(), database.Pool.Config().ConnString(), kind, id, listing.Digest, time.Millisecond)
		if erasure != nil {
			for _, document := range erasure.Documents {
				t.Errorf("document %s has a stored history, which this test never writes", document.Document)
			}
			if erasure.Mismatch() {
				t.Errorf("the confirmed run read a listing of digest %s, not the %s the run before it printed:\n%s",
					erasure.Digest, listing.Digest, strings.Join(erasure.Listing, "\n"))
			}
		}
		answered <- eraseAnswer{erasure, err}
	}()
	return eraseBackendIn(t, context.Background(), database), answered
}

// followTheErase runs Erase over kind:id while holds hold their rows (startErase), and follows it
// as graphcensus's followTheBackfill follows the backfill: it lets go of each hold the run's
// backend queues behind, one at a time, reading where the run waits each time
// (lockstep.RequireStoppedAt), holds the order the run queued in to the store's lock order
// (lockstep.RequireTheStoreLockOrder) and its owner rows to ascending owner key
// (requireOwnersAscending), and answers what the run answered. The order is read before a failed
// run fails the test, since a run that takes a step out of order can fail on it, and the order is
// what names the step.
func followTheErase(t *testing.T, database *store.Store, kind, id string, holds []*lockstep.Hold) *Erasure {
	t.Helper()
	ctx := context.Background()
	pid, answered := startErase(t, database, kind, id)
	var queued []*lockstep.Hold
	deadline := time.Now().Add(lockstep.Wait)
	for {
		select {
		case answer := <-answered:
			lockstep.RequireTheStoreLockOrder(t, "the erase", holds, queued)
			requireOwnersAscending(t, queued)
			if answer.err != nil {
				t.Fatalf("the erase failed after it queued behind %s: %v", lockstep.HoldNames(queued), answer.err)
			}
			storetest.RequireNoMarkedCause(t, database, "the erase")
			return answer.erasure
		default:
		}
		var behind []*lockstep.Hold
		for _, hold := range holds {
			if hold.Released() {
				continue
			}
			waits, err := storetest.QueuedBehind(ctx, hold.Tx, pid)
			if err != nil {
				t.Fatalf("read what the erase waits on: %v", err)
			}
			if waits {
				behind = append(behind, hold)
			}
		}
		switch len(behind) {
		case 0:
			if time.Now().After(deadline) {
				t.Fatalf("the erase queued behind none of the holds still held and did not answer within %s (it queued behind %s so far)",
					lockstep.Wait, lockstep.HoldNames(queued))
			}
			time.Sleep(2 * time.Millisecond)
		case 1:
			lockstep.RequireStoppedAt(t, ctx, database, pid, "the erase", behind[0])
			queued = append(queued, behind[0])
			behind[0].Release()
			deadline = time.Now().Add(lockstep.Wait)
		default:
			t.Fatalf("the erase queued behind %d holds at once (%s), which one statement cannot do", len(behind), lockstep.HoldNames(behind))
		}
	}
}

// requireOwnersAscending holds the owner rows the run queued behind to the order lockInStoreOrder
// takes several of them in, ascending owner key as events.lockOwners spells it. The store's lock
// order puts the owner rows first and leaves their order among themselves to the writer, so
// lockstep.RequireTheStoreLockOrder does not read it.
func requireOwnersAscending(t *testing.T, queued []*lockstep.Hold) {
	t.Helper()
	var previous *lockstep.Hold
	for _, hold := range queued {
		if hold.Step != lockstep.StepOwner {
			continue
		}
		if previous != nil && hold.Kind+":"+hold.ID < previous.Kind+":"+previous.ID {
			t.Errorf("the erase took %s before %s: it takes its owner rows in ascending owner key, so %s:%s before %s:%s (queued in order: %s)",
				previous.Name, hold.Name, hold.Kind, hold.ID, previous.Kind, previous.ID, lockstep.HoldNames(queued))
		}
		previous = hold
	}
}

// eraseBackendIn is the backend process id of the erase's own connection, read as eraseBackend
// reads it but among the backends of database alone: another package's erase test can run beside
// this one on the same server, under the same application name, and a backend of its run is not
// this run's.
func eraseBackendIn(t *testing.T, ctx context.Context, database *store.Store) uint32 {
	t.Helper()
	deadline := time.Now().Add(lockstep.Wait)
	for {
		var pid uint32
		err := database.Pool.QueryRow(ctx, `
			select pid from pg_stat_activity
			where application_name = $1 and datname = current_database() and pid <> pg_backend_pid()
			order by backend_start desc limit 1`, eraseApplicationName).Scan(&pid)
		if err == nil {
			return pid
		}
		if err != pgx.ErrNoRows {
			t.Fatalf("read the erase's backend: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the erase opened no connection within %s", lockstep.Wait)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
