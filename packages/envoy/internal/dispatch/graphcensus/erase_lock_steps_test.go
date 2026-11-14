package graphcensus

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// ownerLockStatement is how the statement that takes an issue's owner row starts, the erase's own
// (lockOwnerRows) and the one docs.ReadErasedLiveText takes it with (lockArtifactOwner) alike.
const ownerLockStatement = "select closed_at is null from issues where key = $1 for no key update"

// untouchedBeforeTheNode are the tables the erase reads nothing of before it holds the node's row:
// the tuples, the record of the erase and the event log, which it reads and writes only once it
// has read the node under its locks. It reads the embedding store before then, to learn the owner
// rows of the rows of it that it deletes (embeddedOwners).
var untouchedBeforeTheNode = []string{"graph_tuples", "graph_erasures", "events"}

// commentLockStatement is how the statement starts that takes the erased comment's own comments
// row, `for update`, right after its node row (lockProjectionRows).
const commentLockStatement = "select 1 from comments where node_kind = $1 and node_address = $2 order by id for update"

// TestTheEraseTakesTheStoreLockOrder is the erase's half of dispatch://INFRA-1149's
// c-lock-order. `envoy-dispatch erase` writes graph_tuples and graph_nodes with statements of
// its own (apply), which graphstore's TestEveryStoreWriteCallSiteIsListed lists as
// graphcensus/erase.go#apply and leaves the proof of its lock steps to this package.
//
// The run erases the comment seedAgreeingStore writes on a document of issue CORE-1, whose text a
// version of a document of a second issue, ACME-1, quotes. So it writes under two owner rows: the
// comment's own, CORE-1's, and ACME-1's, which the version it deletes and the live state it reads
// belong to. It takes them in the order of their owner keys (lockInStoreOrder), issue:ACME-1
// before issue:CORE-1, the other way round from the comment's own owner first; then the comment's
// node row; then its comments row, `for update`, before it reads under its locks or writes
// anything (lockProjectionRows). Its LiveText is docs.ReadErasedLiveText, the writer cmd/dispatch
// hands it, which takes the document's owner row again after the node row and has to find this
// transaction holding it already.
//
// Each of those four rows is held in a transaction of its own while the real Erase runs on its
// own connection (followTheErase). The test lets go of each hold the run queues behind, one at a
// time, reads where the run waits and the tables it holds a lock on each time (requireStoppedAt),
// and holds the order it queued in to the store's lock order (requireTheStoreLockOrder): every
// owner row before the node row, the node row before the projection row, and so no owner row
// after the node row. The owner rows themselves come in ascending owner key
// (requireOwnersAscending). It then reads what the run removed, so the run it followed is the one
// that committed.
func TestTheEraseTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedAgreeingStore(t, database)
	quoting := seedQuotingDocument(t, database, "ACME")
	quoteTheComment(t, database, quoting, seeded.comment)

	// While the run waits at an owner row it has read the rows it erases, the documents that hold
	// their text and the embedding rows that do, and no more: the node row comes next and
	// everything it writes after that. While it waits at the node row it holds the owner rows and
	// has still read nothing of the tuples or the event log.
	acme := holdRow(t, database, &batchHold{
		name: "issue ACME-1's row", step: stepOwner, taken: true, kind: "issue", id: "ACME-1",
		statement: ownerLockStatement,
		unlocked:  append([]string{"graph_nodes"}, untouchedBeforeTheNode...),
	}, `select 1 from issues where key = $1 for update`, "ACME-1")
	core := holdRow(t, database, &batchHold{
		name: "issue CORE-1's row", step: stepOwner, taken: true, kind: "issue", id: "CORE-1",
		statement: ownerLockStatement,
		unlocked:  append([]string{"graph_nodes"}, untouchedBeforeTheNode...),
	}, `select 1 from issues where key = $1 for update`, "CORE-1")
	node := holdRow(t, database, &batchHold{
		name: "the comment's node row", step: stepNode, taken: true, kind: commentNodeKind, id: seeded.comment,
		statement: "select supertag from graph_nodes where kind = $1 and id = $2 for update",
		unlocked:  untouchedBeforeTheNode,
	}, `select 1 from graph_nodes where kind = $1 and id = $2 for update`, commentNodeKind, seeded.comment)
	projection := holdRow(t, database, &batchHold{
		name: "the comment's comments row", step: stepProjection, taken: true,
		statement: commentLockStatement,
		unlocked:  untouchedBeforeTheNode,
	}, `select 1 from comments where id = $1::uuid for update`, seeded.comment)

	erasure := followTheErase(t, database, seeded.comment, []*batchHold{acme, core, node, projection})
	requireTheQuoteErased(t, database, erasure, seeded.comment, quoting)
}

// TestTheEraseTakesTheCommentsRowBeforeItTouchesItsWindows holds the erase to taking the comment's
// own comments row right after its node row, before it writes anything (lockProjectionRows), which
// is what orders it against the block index tick's window arm. That arm takes no owner row: it
// takes the comment's row `for no key update` and then writes the comment's windows and their
// window state (refs.WriteItemWindows), keys the erase deletes (forgetEmbeddings).
//
// The comment is indexed and windowed first, as the arm leaves it. A transaction stands for the
// arm: it holds the comment's row `for no key update` while the erase starts, and the test reads
// that the erase waits at the statement that takes that row, holding no lock on the embedding
// store but the ACCESS SHARE its read before the owner rows takes (embeddedOwners), so it has
// deleted no window and no window state. The holder then runs the arm's write of the comment's
// windows and commits, and the erase goes on, removes the comment and its windows with it, and
// commits. An erase that took the row only where it deletes it would wait there holding the rows it
// deleted from the embedding store, which the arm's write then waits for: one of the two ends
// `deadlock detected`.
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
	if early, done := storetest.WaitForQueuedBehind(t, holder, pid, answered, batchLockWait); done {
		t.Fatalf("the erase answered (err %v) without waiting on the comment's row, which the window arm held", early.err)
	}
	waited := strings.ToLower(waitingStatement(t, database, pid))
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
	case <-time.After(batchLockWait):
		t.Fatalf("the erase did not answer within %s of the window arm's commit", batchLockWait)
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
// revision fence take it (heldBlockRows), and the erase starts: the test reads that it waits at
// the statement that takes that row having written nothing, so no block text and no embedding
// row. Once the row is let go the erase removes the block whole: its document_blocks row, its
// block_refs row, its block text, every tuple out of it and into it, and its node.
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
	if early, done := storetest.WaitForQueuedBehind(t, holder, pid, answered, batchLockWait); done {
		t.Fatalf("the erase answered (err %v) without waiting on the block's document_blocks row, which another transaction held", early.err)
	}
	waited := strings.ToLower(waitingStatement(t, database, pid))
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
	case <-time.After(batchLockWait):
		t.Fatalf("the erase did not answer within %s of the release of the block's document_blocks row", batchLockWait)
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

// TestTheEraseTakesTheOwnersOfABlockAndAWindowQuotingItBeforeItsNode holds the erase to the owner
// rows of what it writes beyond the documents it lists. The comment's text stands in no version, no
// block text and no stored history of another document, so the erase lists no document and ends no
// version. It stands in one block's excerpt alone in a document of issue ACME-1, which redactBlocks
// rewrites, and in one embedding window alone of a document of issue BETA-1, which forgetEmbeddings
// deletes. The version write and the block index tick write a block row under its document's owner,
// and the window arm a document's windows (refs.WriteDocumentWindows), so the erase takes both
// owner rows, with the comment's own, CORE-1's, before the comment's node row (erasedRead), in the
// order of their keys: ACME-1, BETA-1, CORE-1.
//
// The block row in ACME-1's document is held too, as a projection row the erase writes after the
// comment's own: the run queues behind it last, at the statement that rewrites its excerpt. The
// test then reads that the excerpt holds the erase's marker and not the text, and that the window
// is gone, so the run it followed is the one that wrote them.
func TestTheEraseTakesTheOwnersOfABlockAndAWindowQuotingItBeforeItsNode(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedAgreeingStore(t, database)
	excerpted := seedQuotingDocument(t, database, "ACME")
	windowed := seedQuotingDocument(t, database, "BETA")
	quoteTheCommentInABlock(t, database, excerpted, seeded.comment)
	quoteTheCommentInAWindow(t, database, windowed, seeded.comment)

	ownerHold := func(issue string) *batchHold {
		return holdRow(t, database, &batchHold{
			name: "issue " + issue + "'s row", step: stepOwner, taken: true, kind: "issue", id: issue,
			statement: ownerLockStatement,
			unlocked:  append([]string{"graph_nodes"}, untouchedBeforeTheNode...),
		}, `select 1 from issues where key = $1 for update`, issue)
	}
	acme, beta, core := ownerHold("ACME-1"), ownerHold("BETA-1"), ownerHold("CORE-1")
	node := holdRow(t, database, &batchHold{
		name: "the comment's node row", step: stepNode, taken: true, kind: commentNodeKind, id: seeded.comment,
		statement: "select supertag from graph_nodes where kind = $1 and id = $2 for update",
		unlocked:  untouchedBeforeTheNode,
	}, `select 1 from graph_nodes where kind = $1 and id = $2 for update`, commentNodeKind, seeded.comment)
	comment := holdRow(t, database, &batchHold{
		name: "the comment's comments row", step: stepProjection, taken: true,
		statement: commentLockStatement,
		unlocked:  untouchedBeforeTheNode,
	}, `select 1 from comments where id = $1::uuid for update`, seeded.comment)
	block := holdRow(t, database, &batchHold{
		name: "the block row of " + excerpted.refKey + " quoting the comment", step: stepProjection, taken: true,
		statement: "update document_blocks held set excerpt =",
		unlocked:  []string{"graph_erasures"},
	}, `select 1 from document_blocks where artifact_id = $1::uuid and block_id = 'quote' for update`, excerpted.artifactID)

	erasure := followTheErase(t, database, seeded.comment, []*batchHold{acme, beta, core, node, comment, block})

	if !erasure.Applied || len(erasure.Versions) != 0 || len(erasure.Documents) != 0 {
		t.Errorf("the erase answered applied %t, the versions %+v and the documents %+v, want it applied with neither: the text stands in no version and no document's live state",
			erasure.Applied, erasure.Versions, erasure.Documents)
	}
	var excerpt string
	var windowStands bool
	if err := database.Pool.QueryRow(context.Background(), `
		select (select excerpt from document_blocks where artifact_id = $1::uuid and block_id = 'quote'),
		       exists (select 1 from embeddings where kind = $2 and split_part(id, '#', 1) = $3)
	`, excerpted.artifactID, refs.DocumentWindowKind, windowed.artifactID).Scan(&excerpt, &windowStands); err != nil {
		t.Fatalf("read the block and the window that quoted the comment: %v", err)
	}
	if marker := erasure.marker(); excerpt != "Alice wrote: "+marker || windowStands {
		t.Errorf("after the erase the excerpt of %s reads %q and the window of %s stands: %t; want %q and no window: the run the test followed is not one that wrote them",
			excerpted.refKey, excerpt, windowed.refKey, windowStands, "Alice wrote: "+marker)
	}
}

// TestTheEraseStartsAgainWhenItsTextMovesUnderAnOwnerItHasNotTaken holds lockInStoreOrder to what
// it does when the owner rows it read before taking them are not all the ones it writes under
// once it holds them. The run reads its owners while only the comment's own issue, CORE-1, stands
// under the comment's text, and is held at CORE-1's row; a version of ACME-1's document quoting
// the comment then commits, and ACME-1's row is held before CORE-1's is let go. Under its locks
// the run reads ACME-1's document as one it rewrites, under an owner it did not take. A run that
// went on would take ACME-1's row after the comment's node row, when it reads that document's live
// state (Stores.LiveText), and queue behind it holding graph_nodes. This one rolls back and starts
// again, so it queues behind ACME-1's row at the start of its next try, having read nothing of the
// store, and once let go it erases the quoting version with the rest.
func TestTheEraseStartsAgainWhenItsTextMovesUnderAnOwnerItHasNotTaken(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	quoting := seedQuotingDocument(t, database, "ACME")

	core := holdRow(t, database, &batchHold{name: "issue CORE-1's row", step: stepOwner, statement: ownerLockStatement},
		`select 1 from issues where key = $1 for update`, "CORE-1")
	pid, answered := startErase(t, database, commentNodeKind, seeded.comment)
	if early, done := storetest.WaitForQueuedBehind(t, core.tx, pid, answered, batchLockWait); done {
		t.Fatalf("the erase answered (err %v) without waiting on issue CORE-1's row, which another transaction held", early.err)
	}
	requireStoppedAt(t, ctx, database, pid, "the erase", core)
	quoteTheComment(t, database, quoting, seeded.comment)
	acme := holdRow(t, database, &batchHold{
		name: "issue ACME-1's row", step: stepOwner, statement: ownerLockStatement,
		unlocked: append([]string{"graph_nodes"}, untouchedBeforeTheNode...),
	}, `select 1 from issues where key = $1 for update`, "ACME-1")
	core.release()
	if early, done := storetest.WaitForQueuedBehind(t, acme.tx, pid, answered, batchLockWait); done {
		t.Fatalf("the erase answered (err %v) without waiting on issue ACME-1's row, the owner of %s, which came to quote the comment once the erase had read its owner rows",
			early.err, quoting.refKey)
	}
	requireStoppedAt(t, ctx, database, pid, "the erase", acme)
	acme.release()

	var answer eraseAnswer
	select {
	case answer = <-answered:
	case <-time.After(batchLockWait):
		t.Fatalf("the erase did not answer within %s of the release of issue ACME-1's row", batchLockWait)
	}
	if answer.err != nil {
		t.Fatalf("the erase failed once issue ACME-1's row was let go: %v", answer.err)
	}
	requireTheQuoteErased(t, database, answer.erasure, seeded.comment, quoting)
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

// quoteTheCommentInABlock writes one block row of document, naming no node and indexed from no
// version, whose excerpt quotes the body of comment, and no block text for it: the excerpt is the
// one place document holds the text.
func quoteTheCommentInABlock(t *testing.T, database *store.Store, document quotingDocument, comment string) {
	t.Helper()
	tag, err := database.Pool.Exec(context.Background(), `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		select $1::uuid, 'quote', $2::text || '#quote', 'paragraph', '', 'Alice wrote: ' || body, 1, now()
		from comments where id::text = $3
	`, document.artifactID, document.refKey, comment)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write the block of %s that quotes comment %s: %d rows, %v", document.refKey, comment, tag.RowsAffected(), err)
	}
}

// quoteTheCommentInAWindow writes one embedding window of document whose text quotes the body of
// comment: the window is the one place document holds the text.
func quoteTheCommentInAWindow(t *testing.T, database *store.Store, document quotingDocument, comment string) {
	t.Helper()
	tag, err := database.Pool.Exec(context.Background(), `
		insert into embeddings (kind, id, text_snapshot, content_hash)
		select $1::text, $2::text || '#b1', 'Alice wrote: ' || body, 'hash'
		from comments where id::text = $3
	`, refs.DocumentWindowKind, document.artifactID, comment)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write the window of %s that quotes comment %s: %d rows, %v", document.refKey, comment, tag.RowsAffected(), err)
	}
}

// requireTheQuoteErased reads what the erase answered and what it left: it is applied, it lists
// version 1 of the quoting document as one it ended, and the comment's node row, its comments row
// and that version are gone, with one erasure naming the comment. The run a test followed is then
// the one that committed.
func requireTheQuoteErased(t *testing.T, database *store.Store, erasure *Erasure, comment string, quoting quotingDocument) {
	t.Helper()
	if !erasure.Applied || len(erasure.Versions) != 1 || erasure.Versions[0].Document != quoting.refKey || erasure.Versions[0].Issue != quoting.issue {
		t.Errorf("the erase answered applied %t and the versions %+v, want it applied and version 1 of %s, %s's, on the list",
			erasure.Applied, erasure.Versions, quoting.refKey, quoting.issue)
	}
	var nodeStands, commentStands, versionStands bool
	var erasures int64
	if err := database.Pool.QueryRow(context.Background(), `
		select exists (select 1 from graph_nodes where kind = $1 and id = $2),
		       exists (select 1 from comments where id::text = $2),
		       exists (select 1 from artifact_versions where artifact_id::text = $3),
		       (select count(*) from graph_erasures where node_kind = $1 and node_id = $2)
	`, commentNodeKind, comment, quoting.artifactID).Scan(&nodeStands, &commentStands, &versionStands, &erasures); err != nil {
		t.Fatalf("read what the erase left: %v", err)
	}
	if nodeStands || commentStands || versionStands || erasures != 1 {
		t.Errorf("after the erase the comment's node row stands: %t, its comments row: %t, the quoting version: %t, and %d erasure(s) name it; want none standing and one erasure: the run the test followed is not one that committed",
			nodeStands, commentStands, versionStands, erasures)
	}
}

// eraseAnswer is what one run of Erase answered.
type eraseAnswer struct {
	erasure *Erasure
	err     error
}

// startErase runs a confirmed Erase of node kind:id off the test's goroutine, on the connection the
// erase opens for itself, and answers that connection's backend process id and the channel the
// run's answer arrives on. Its LiveText is docs.ReadErasedLiveText, the writer cmd/dispatch hands
// the erase, which takes a document's owner row and then its room's lock; the documents here hold
// no stored history, so it answers nil for each once it holds them. Its count of the term
// statistics counts nothing, which no lock step here rests on.
func startErase(t *testing.T, database *store.Store, kind, id string) (uint32, <-chan eraseAnswer) {
	t.Helper()
	stores := Stores{
		LiveText: func(ctx context.Context, tx pgx.Tx, artifactID string, texts []string, marker string) (*LiveText, error) {
			live, err := docs.ReadErasedLiveText(ctx, tx, artifactID, texts, marker)
			if err != nil || live == nil {
				return nil, err
			}
			t.Errorf("document %s has a stored history, which this test never writes", artifactID)
			return nil, nil
		},
		TermStatistics: func(context.Context, pgx.Tx, []string) (TermRecount, error) {
			return TermRecount{}, nil
		},
		TreeStoreBound: time.Millisecond,
	}
	answered := make(chan eraseAnswer, 1)
	go func() {
		erasure, err := Erase(context.Background(), database.Pool.Config().ConnString(), kind, id, true, stores)
		answered <- eraseAnswer{erasure, err}
	}()
	return eraseBackendIn(t, context.Background(), database), answered
}

// followTheErase runs Erase over comment while holds hold their rows (startErase), and follows it
// as followTheBackfill follows the backfill: it lets go of each hold the run's backend queues
// behind, one at a time, reading where the run waits each time (requireStoppedAt), holds the order
// the run queued in to the store's lock order (requireTheStoreLockOrder) and its owner rows to
// ascending owner key (requireOwnersAscending), and answers what the run answered. The order is
// read before a failed run fails the test, since a run that takes a step out of order can fail on
// it, and the order is what names the step.
func followTheErase(t *testing.T, database *store.Store, comment string, holds []*batchHold) *Erasure {
	t.Helper()
	ctx := context.Background()
	pid, answered := startErase(t, database, commentNodeKind, comment)
	var queued []*batchHold
	deadline := time.Now().Add(batchLockWait)
	for {
		select {
		case answer := <-answered:
			requireTheStoreLockOrder(t, "the erase", holds, queued)
			requireOwnersAscending(t, queued)
			if answer.err != nil {
				t.Fatalf("the erase failed after it queued behind %s: %v", holdNames(queued), answer.err)
			}
			return answer.erasure
		default:
		}
		var behind []*batchHold
		for _, hold := range holds {
			if hold.released {
				continue
			}
			waits, err := storetest.QueuedBehind(ctx, hold.tx, pid)
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
					batchLockWait, holdNames(queued))
			}
			time.Sleep(2 * time.Millisecond)
		case 1:
			requireStoppedAt(t, ctx, database, pid, "the erase", behind[0])
			queued = append(queued, behind[0])
			behind[0].release()
			deadline = time.Now().Add(batchLockWait)
		default:
			t.Fatalf("the erase queued behind %d holds at once (%s), which one statement cannot do", len(behind), holdNames(behind))
		}
	}
}

// requireOwnersAscending holds the owner rows the run queued behind to the order lockInStoreOrder
// takes several of them in, ascending owner key as events.lockOwners spells it. The store's lock
// order puts the owner rows first and leaves their order among themselves to the writer, so
// requireTheStoreLockOrder does not read it.
func requireOwnersAscending(t *testing.T, queued []*batchHold) {
	t.Helper()
	var previous *batchHold
	for _, hold := range queued {
		if hold.step != stepOwner {
			continue
		}
		if previous != nil && hold.kind+":"+hold.id < previous.kind+":"+previous.id {
			t.Errorf("the erase took %s before %s: it takes its owner rows in ascending owner key, so %s:%s before %s:%s (queued in order: %s)",
				previous.name, hold.name, hold.kind, hold.id, previous.kind, previous.id, holdNames(queued))
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
	deadline := time.Now().Add(batchLockWait)
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
			t.Fatalf("the erase opened no connection within %s", batchLockWait)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
