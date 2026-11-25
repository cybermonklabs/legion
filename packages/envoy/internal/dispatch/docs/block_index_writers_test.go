package docs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// deferredItems is a bullet list of more items than a write indexes in its own request: one
// block for the list and one for each item's paragraph, so it is deferredItems+1 blocks against
// the bound of deferBlockIndexBlocks.
const deferredItems = deferBlockIndexBlocks + 1

// deferredList's last item cites an issue, so the document leaves a refs row of its own: the
// rebuild below is held to writing those and marking work owed, which it can only be seen to
// do where there is a reference to rebuild.
func deferredList(items int) string {
	var body strings.Builder
	for item := 1; item < items; item++ {
		body.WriteString("- x\n")
	}
	body.WriteString("- The last item, which cites dispatch://CORE-2.\n")
	return body.String()
}

// writeDocumentThroughTheRequestPath writes one document the way a request writes it (api's
// storeArtifact): the document's node and then its row, dated with one time
// (graphstore.StatementTime), the text seeded in the writer's transaction, the version row, the
// version's references and blocks indexed from the rendering that transaction wrote, the version's
// event, and then what the node write and the block index stated, appended under that event.
func writeDocumentThroughTheRequestPath(t *testing.T, service *Service, database *store.Store, slug, markdown string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the document write: %v", err)
	}
	defer tx.Rollback(ctx)
	alice := model.Actor{Kind: "user", ID: "alice"}
	refKey := graphstore.ArtifactRefKey("CORE-1", "CORE", "", slug)
	createdAt, err := graphstore.StatementTime(ctx, tx, graphstore.KindArtifact, refKey)
	if err != nil {
		t.Fatalf("read the time of document %s: %v", slug, err)
	}
	documentNode := graphstore.PlanUncaused([]graphstore.Node{graphstore.ArtifactNode(refKey, "CORE", "", alice, createdAt)}, nil)
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueNodes(b, documentNode)
		return nil
	}); err != nil {
		t.Fatalf("write the node of document %s: %v", slug, err)
	}
	var artifactID string
	if err := tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, created_at)
		values ('CORE-1', 'CORE', $1, $1 || '.md', 'doc', false, '{"kind":"user","id":"alice"}', $2)
		returning id::text
	`, slug, createdAt).Scan(&artifactID); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	stored, err := service.SeedText(joined, artifactID, markdown, alice)
	if err != nil {
		t.Fatalf("seed %s: %v", slug, err)
	}
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
	`, artifactID, stored); err != nil {
		t.Fatalf("write version 1 of %s: %v", slug, err)
	}
	_, write, err := service.IndexDocumentReferences(joined, artifactID, 1, stored)
	if err != nil {
		t.Fatalf("index the references of %s: %v", slug, err)
	}
	payload, err := json.Marshal(map[string]any{"artifact": map[string]any{"id": artifactID}})
	if err != nil {
		t.Fatalf("encode the created event's payload: %v", err)
	}
	event, err := documentEvent(ctx, tx, "artifact.created", payload)
	if err != nil {
		t.Fatalf("record the document's event: %v", err)
	}
	if err := causeInWritePhase(joined, tx, documentNode, event); err != nil {
		t.Fatalf("append what the document's node write stated: %v", err)
	}
	if err := causeInWritePhase(joined, tx, write, event); err != nil {
		t.Fatalf("append what the write's block index stated: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the document write: %v", err)
	}
	return artifactID
}

// causeInWritePhase queues the append of what written stated under event, the event that caused
// it, on the write phase of the ledger ctx joins tx to, after every write queued so far, as the
// routes queue it after their event (graphstore.QueueCaused). The node write it moves onto that
// event is queued there as well and runs only once the ledger commits, so an append run at once
// would run before the rows it settles exist. A transaction no ledger joins runs it at once
// (write).
func causeInWritePhase(ctx context.Context, tx pgx.Tx, written graphstore.Uncaused, event int64) error {
	return write(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueCaused(b, written, event)
		return nil
	})
}

// countRows is one count over the document's rows of a table keyed by artifact_id.
func countRows(t *testing.T, database *store.Store, table, artifactID string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from `+table+` where artifact_id = $1`, artifactID).Scan(&count); err != nil {
		t.Fatalf("count the rows of %s for %s: %v", table, artifactID, err)
	}
	return count
}

// countBlockNodes is how many nodes of the store belong to the document, by the ref_key every
// one of their addresses starts with.
func countBlockNodes(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_nodes n
		join artifacts a on a.id = $1::uuid
		where n.kind = $2 and n.id like a.ref_key || '#%'
	`, artifactID, refs.BlockNodeKind).Scan(&count); err != nil {
		t.Fatalf("count the block nodes of %s: %v", artifactID, err)
	}
	return count
}

// tableFingerprint is a table's rows as a reading that any write of one would change: how many
// there are and the transaction that last wrote each (xmin).
func tableFingerprint(t *testing.T, database *store.Store, table string) string {
	t.Helper()
	var fingerprint string
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*)::text || '|' || coalesce(string_agg(rows.xmin::text, ',' order by rows.xmin::text), '')
		 from `+table+` rows`).Scan(&fingerprint); err != nil {
		t.Fatalf("fingerprint %s: %v", table, err)
	}
	return fingerprint
}

// waitingStatement is the statement the one backend queued on a lock is running, which says
// where a pass is waiting. It is read through the holder's own connection, as every lock read
// of these tests is.
func waitingStatement(t *testing.T, holder pgx.Tx) string {
	t.Helper()
	var running string
	if err := holder.QueryRow(context.Background(), `
		select query from pg_stat_activity
		where datname = current_database() and wait_event_type = 'Lock' and pid <> pg_backend_pid()
		order by query_start limit 1
	`).Scan(&running); err != nil {
		t.Fatalf("read the statement a waiting pass is running: %v", err)
	}
	return running
}

// The two passes that write a block index are store writers: each writes the block's node, the
// tuples that node's rows are the projection of, and those rows themselves, in one transaction.
//
// The rule that tells a writer from a repair is applied first, to every pass of the source tree
// that writes a projected table or the store, by a statement of its own or through the store's
// writer: each pass's input is reported, and a pass the register does not name fails, named with
// the side of the projection it reads, before any database is touched.
//
// A document of more blocks than a write indexes in its own request writes no block row at all,
// as it does today, and the block-index reconciliation then writes all three for it - proven by
// killing the tick's transaction and finding none of them. The tick takes the document's owner
// lock and then its owed row before it writes either, in that order. The item block pass writes
// one block node per block of an item the store holds, with its part_of and its links, and no row
// of the store for an item the store does not hold. The rebuild writes refs and marks work owed
// and writes no row of document_blocks, block_refs or comments.
func TestTheIndexPassesAreStoreWritersAndNoRepairExists(t *testing.T) {
	t.Parallel()
	requireEveryPassReadsItsSource(t)
	if t.Failed() {
		// The source tree holds a pass nobody has said the input of, so whatever the
		// database answers below would be read for a tree this test cannot vouch for.
		t.FailNow()
	}

	ctx := context.Background()
	database, service := blockIndexService(t)
	document := writeDocumentThroughTheRequestPath(t, service, database, "heavy", deferredList(deferredItems))
	settle(t, database, document)

	// The request indexed its references and left its blocks owed, as it does today.
	if rows := countRows(t, database, "document_blocks", document); rows != 0 {
		t.Fatalf("the request wrote %d block rows, want none: the document is past the in-request bound", rows)
	}
	if stands := blockIndexAt(t, database, document); stands != nil {
		t.Fatalf("the request indexed the blocks at version %d, want them left owed", *stands)
	}
	if nodes := countBlockNodes(t, database, document); nodes != 0 {
		t.Fatalf("the request wrote %d block nodes, want none", nodes)
	}

	// Killing the tick's transaction leaves none of the three. The tick logs a document it
	// could not index and carries on, so what it answers is that it indexed none. The hook
	// records the document it killed, because a tick that never reached its write would leave
	// none of the three behind as well, and would prove nothing about the transaction.
	before := highestTuple(t, database)
	var killedDocuments []string
	service.afterBlockIndexWrite = func(artifactID string) error {
		killedDocuments = append(killedDocuments, artifactID)
		return errors.New("kill the tick's transaction")
	}
	killed, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, liveAheadDocuments{})
	if err != nil {
		t.Fatalf("the killed tick: %v", err)
	}
	service.afterBlockIndexWrite = nil
	if !slices.Equal(killedDocuments, []string{document}) {
		t.Fatalf("the kill hook fired for %v, want once for %s: a tick that never reached its write killed nothing",
			killedDocuments, document)
	}
	if killed.Documents != 0 {
		t.Fatalf("the killed tick indexed %d documents, want none", killed.Documents)
	}
	if rows := countRows(t, database, "document_blocks", document); rows != 0 {
		t.Errorf("the killed tick left %d block rows behind, want none", rows)
	}
	if nodes := countBlockNodes(t, database, document); nodes != 0 {
		t.Errorf("the killed tick left %d block nodes behind, want none", nodes)
	}
	if appended := tuplesSince(t, database, document, before); appended != 0 {
		t.Errorf("the killed tick left %d tuples behind, want none", appended)
	}

	// The tick that commits writes all three for that document in one transaction.
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("the tick indexed %d documents, want the deferred one", tick.Documents)
	}
	blocks := countRows(t, database, "document_blocks", document)
	if blocks != deferredItems+1 {
		t.Fatalf("the tick wrote %d block rows of the %d-item list, want %d", blocks, deferredItems, deferredItems+1)
	}
	if nodes := countBlockNodes(t, database, document); nodes != blocks {
		t.Errorf("the tick wrote %d block nodes beside %d block rows, want one node a row", nodes, blocks)
	}
	if appended := tuplesSince(t, database, document, before); appended < blocks {
		t.Errorf("the tick appended %d tuples for %d blocks, want at least one a block", appended, blocks)
	}
	if parents := parentsNow(t, database, document); len(parents) != blocks {
		t.Errorf("the tick left %d blocks with a current parent, want all %d", len(parents), blocks)
	}

	requireTheTickLocksTheOwnerThenTheOwedRow(t, database, service)
	requireTheItemBlockPassWritesTheItemsBlockNodes(t, database, service)
	requireTheRebuildWritesRefsAndNoProjectedRow(t, database, document)
}

// requireTheTickLocksTheOwnerThenTheOwedRow holds each of the two locks the tick takes and reads
// where the tick stops: the document owner's advisory lock first, in its read phase, before it
// has read or written anything of the document's index (lockArtifactOwner), and the owed row
// next, which its write phase takes with the revision it writes (BlockIndexing's queueStand) and
// which it only reaches once the owner is free. The fence is decided under the owner, so a tick
// that could not take the owner leaves the document as it found it.
//
// Where the tick stops says which lock it waits for, not which it already holds, so each step
// also tries the other lock without waiting while the tick waits. Waiting at the owner, the tick
// must hold nothing yet, so the owed row is free to take; waiting at the owed row, it must hold
// the owner already, so the try is refused. A tick that took the two in the other order passes
// neither try.
func requireTheTickLocksTheOwnerThenTheOwedRow(t *testing.T, database *store.Store, service *Service) {
	t.Helper()
	ctx := context.Background()
	// Every document these steps create is issue CORE-1's.
	owner := store.IssueOwner("CORE-1")
	holdTheOwner := func(holder pgx.Tx, _ string) error { return store.LockOwner(ctx, holder, owner) }
	holdTheOwedRow := func(holder pgx.Tx, document string) error {
		_, err := holder.Exec(ctx, `select 1 from document_block_versions where artifact_id = $1 for no key update`, document)
		return err
	}
	tryTheOwedRow := func(probe pgx.Tx, document string) (bool, error) {
		_, err := probe.Exec(ctx, `select 1 from document_block_versions where artifact_id = $1 for no key update nowait`, document)
		var refused *pgconn.PgError
		if errors.As(err, &refused) && refused.Code == "55P03" {
			return false, nil
		}
		return err == nil, err
	}
	tryTheOwner := func(probe pgx.Tx, _ string) (bool, error) {
		taken, err := store.TryLockOwners(ctx, probe, owner)
		return len(taken) == 1, err
	}
	for _, step := range []struct {
		name, slug, wantStatement string
		hold                      func(holder pgx.Tx, document string) error
		// other is the lock the step does not hold, tried without waiting while the tick
		// waits, and otherHeld whether the tick must hold it by then.
		otherName string
		other     func(probe pgx.Tx, document string) (bool, error)
		otherHeld bool
	}{
		{
			name:          "the document owner's lock",
			slug:          "owner-locked",
			hold:          holdTheOwner,
			wantStatement: "as owner_lock",
			otherName:     "the document's owed row",
			other:         tryTheOwedRow,
			otherHeld:     false,
		},
		{
			name:          "the document's owed row",
			slug:          "owed-locked",
			hold:          holdTheOwedRow,
			wantStatement: "document_block_versions",
			otherName:     "the document owner's lock",
			other:         tryTheOwner,
			otherHeld:     true,
		},
	} {
		// A document of its own each step, since the tick indexes the one before it as soon as
		// that step lets it through, and an indexed document is owed nothing to wait for.
		document := createOwedDocument(t, database, service, "CORE-1", "", step.slug, "- one\n- two\n", "")
		settle(t, database, document)
		holder, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the holder of %s: %v", step.name, err)
		}
		// The holder is rolled back however the step ends: a wait below that fails the test leaves
		// the tick queued behind it otherwise. A rollback after the step's own is a no-op.
		testcleanup.Add(t, func() { _ = holder.Rollback(context.Background()) })
		if err := step.hold(holder, document); err != nil {
			holder.Rollback(ctx)
			t.Fatalf("hold %s: %v", step.name, err)
		}
		ticked := make(chan struct{})
		go func() {
			if _, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, liveAheadDocuments{}); err != nil {
				t.Errorf("the tick waiting on %s: %v", step.name, err)
			}
			close(ticked)
		}()
		storetest.WaitForLockWaiters(t, holder, 1)
		if running := waitingStatement(t, holder); !strings.Contains(running, step.wantStatement) {
			t.Errorf("the tick waiting on %s is running %q, want the statement that takes it", step.name, running)
		}
		if rows := countRows(t, database, "document_blocks", document); rows != 0 {
			t.Errorf("the tick wrote %d block rows before it had taken %s, want none", rows, step.name)
		}
		requireTheOtherLock(t, database, document, step.name, step.otherName, step.other, step.otherHeld)
		if err := holder.Rollback(ctx); err != nil {
			t.Fatalf("release %s: %v", step.name, err)
		}
		<-ticked
		if rows := countRows(t, database, "document_blocks", document); rows == 0 {
			t.Errorf("the tick wrote no block row of %s once %s was free, so this step proves nothing",
				step.slug, step.name)
		}
	}
}

// requireTheOtherLock tries, on a transaction of its own and without waiting, the lock the step
// does not hold while the tick waits at the one it does. held says whether the tick must hold
// that lock by then: the try is refused exactly when it does. The try is rolled back at once, so
// a lock it took is never one the tick then waits for.
func requireTheOtherLock(t *testing.T, database *store.Store, document, waitingAt, otherName string,
	other func(probe pgx.Tx, document string) (bool, error), held bool) {
	t.Helper()
	ctx := context.Background()
	probe, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the try of %s: %v", otherName, err)
	}
	taken, tried := other(probe, document)
	if err := probe.Rollback(ctx); err != nil {
		t.Fatalf("end the try of %s: %v", otherName, err)
	}
	switch {
	case tried != nil:
		t.Fatalf("try %s while the tick waits at %s: %v", otherName, waitingAt, tried)
	case held && taken:
		t.Errorf("while the tick waited at %s, %s was free to take: the tick takes the owner's lock, then the owed row, so it holds %s by then",
			waitingAt, otherName, otherName)
	case !held && !taken:
		t.Errorf("while the tick waited at %s, %s was already held: the tick takes the owner's lock, then the owed row, so it holds nothing while it waits for the first",
			waitingAt, otherName)
	}
}

// requireTheItemBlockPassWritesTheItemsBlockNodes runs the item block pass
// (refs.IndexOwedItemBlocks, the tick's third batch) over two comments owed the index of their
// own blocks, each of two paragraphs, the second citing an issue. One is an older build's: the row
// alone, which names no node, and the pass writes no row of graph_nodes or graph_tuples for it,
// since a block node's part_of names an item node the store must hold. The other is this build's,
// whose row names its node: the pass writes a `block` node for each of its blocks, each row
// naming it, a part_of from each to the comment, and the second block's link to the issue, all at
// the sentinel, since the pass records no event. Every document is indexed first, so the tick that
// runs the pass has no document of its own to write, and a row of either table that moved is the
// pass's.
func requireTheItemBlockPassWritesTheItemsBlockNodes(t *testing.T, database *store.Store, service *Service) {
	t.Helper()
	ctx := context.Background()
	drainBlockIndex(t, service, "every document before the item block pass")
	const body = "One.\n\nTwo cites dispatch://CORE-2."
	// An older build's comment: migration 0143's trigger leaves it owed a block index
	// (comment_block_state.owed_at).
	var older string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', $1)
		returning id::text
	`, body).Scan(&older); err != nil {
		t.Fatalf("write the older build's owed comment: %v", err)
	}
	nodes, tuples := tableFingerprint(t, database, "graph_nodes"), tableFingerprint(t, database, "graph_tuples")
	tick := reconcileBlockIndex(t, service, "")
	if tick.ItemBlocks != 1 || tick.Documents != 0 {
		t.Fatalf("the tick indexed the blocks of %d items and %d documents, want the owed comment's alone", tick.ItemBlocks, tick.Documents)
	}
	if blocks := countItemBlocks(t, database, older, false); blocks != 2 {
		t.Fatalf("the item block pass wrote %d blocks of the older build's two-paragraph comment, want 2: it ran over nothing", blocks)
	}
	// A fingerprint is the row count, then every row's xmin: the message prints the counts,
	// since a table of the store holds thousands of rows by now.
	rowsOf := func(fingerprint string) string { return strings.SplitN(fingerprint, "|", 2)[0] }
	if now := tableFingerprint(t, database, "graph_nodes"); now != nodes {
		t.Errorf("the item block pass wrote graph_nodes for a comment the store holds no node of: it held %s rows and left %s, or rewrote one, want none written",
			rowsOf(nodes), rowsOf(now))
	}
	if now := tableFingerprint(t, database, "graph_tuples"); now != tuples {
		t.Errorf("the item block pass wrote graph_tuples for a comment the store holds no node of: it held %s rows and left %s, or rewrote one, want none written",
			rowsOf(tuples), rowsOf(now))
	}

	// This build's comment: its node first, then the row naming it, which the same trigger leaves
	// owed its block index.
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write of this build's comment: %v", err)
	}
	defer tx.Rollback(ctx)
	var marked string
	if err := tx.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&marked); err != nil {
		t.Fatalf("pick the comment's id: %v", err)
	}
	node, _ := graphstore.CommentFacts(nil, marked, model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC(),
		graphstore.NoEvent, "", "", "CORE", nil)
	if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, nil); err != nil {
		t.Fatalf("write the comment's node: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into comments (id, issue_key, author, body, node_kind, node_address, tuples_stored)
		values ($1::uuid, 'CORE-1', '{"kind":"user","id":"alice"}', $2, $3, $4, true)
	`, marked, body, graphstore.KindComment, marked); err != nil {
		t.Fatalf("write this build's owed comment: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit this build's comment: %v", err)
	}
	before := highestTuple(t, database)
	if tick := reconcileBlockIndex(t, service, ""); tick.ItemBlocks != 1 || tick.Documents != 0 {
		t.Fatalf("the tick indexed the blocks of %d items and %d documents, want this build's owed comment alone", tick.ItemBlocks, tick.Documents)
	}
	if blocks := countItemBlocks(t, database, marked, true); blocks != 2 {
		t.Fatalf("the item block pass wrote %d blocks of this build's comment naming their nodes, want 2", blocks)
	}
	var blockNodes int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_nodes where kind = $1 and id like $2`,
		refs.BlockNodeKind, graphstore.KindComment+"/"+marked+"#%").Scan(&blockNodes); err != nil {
		t.Fatalf("count the comment's block nodes: %v", err)
	}
	if blockNodes != 2 {
		t.Errorf("the item block pass wrote %d block nodes of this build's two-paragraph comment, want 2", blockNodes)
	}
	var partOf, links, caused int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) filter (where kind = $3 and to_kind = $4 and to_id = $5),
			count(*) filter (where kind = 'mentions' and to_kind = 'issue' and to_id = 'CORE-2'),
			count(*) filter (where source_event <> $6)
		from graph_tuples
		where seq > $1 and from_kind = $2 and from_id like $7 and asserted
	`, before, refs.BlockNodeKind, graphstore.TuplePartOf, graphstore.KindComment, marked, graphstore.NoEvent,
		graphstore.KindComment+"/"+marked+"#%").Scan(&partOf, &links, &caused); err != nil {
		t.Fatalf("read the tuples the item block pass appended: %v", err)
	}
	if partOf != 2 || links != 1 || caused != 0 {
		t.Errorf("the item block pass appended %d part_of, %d links and %d tuples under an event, want 2, 1 and none: the pass records no event",
			partOf, links, caused)
	}
}

// countItemBlocks is how many block rows of comment hold a position, and of those, where
// naming is set, how many name their node.
func countItemBlocks(t *testing.T, database *store.Store, comment string, naming bool) int {
	t.Helper()
	var blocks int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from comment_blocks
		where comment_id = $1::uuid and position is not null and (not $2 or node_kind is not null)
	`, comment, naming).Scan(&blocks); err != nil {
		t.Fatalf("count the blocks of comment %s: %v", comment, err)
	}
	return blocks
}

// requireTheRebuildWritesRefsAndNoProjectedRow holds refs.RebuildAll to what it is: a writer of
// refs that marks the work the block index owes. A row of document_blocks, block_refs or comments
// written by it would make it a repair of the projection, which is what this fails on.
//
// The refs it writes are read as its own: the request already wrote the document's refs, so a
// count after the rebuild would read those and pass a rebuild that wrote none. They are deleted
// first, and the rebuild must write the same references back from the document's text.
func requireTheRebuildWritesRefsAndNoProjectedRow(t *testing.T, database *store.Store, document string) {
	t.Helper()
	ctx := context.Background()
	written := documentRefs(t, database, document)
	if written == "" {
		t.Fatalf("the request wrote no refs row of %s, so the rebuild has nothing to be seen writing", document)
	}
	if _, err := database.Pool.Exec(ctx, `delete from refs where from_kind = 'artifact' and from_id = $1`, document); err != nil {
		t.Fatalf("delete the refs the request wrote of %s: %v", document, err)
	}
	projected := []string{"document_blocks", "block_refs", "comments"}
	before := map[string]string{}
	for _, table := range projected {
		before[table] = tableFingerprint(t, database, table)
	}
	report, err := refs.RebuildAll(ctx, database.Pool, "https://dispatch.example")
	if err != nil {
		t.Fatalf("rebuild every reference: %v", err)
	}
	if report.BlocksOwed == 0 {
		t.Errorf("the rebuild marked %d documents owed a block index, want the ones it holds", report.BlocksOwed)
	}
	if rebuilt := documentRefs(t, database, document); rebuilt != written {
		t.Errorf("the rebuild wrote the refs of %s as [%s], want the [%s] the request wrote and the test deleted before it ran",
			document, rebuilt, written)
	} else {
		t.Logf("the rebuild wrote the refs of %s back: [%s]", document, rebuilt)
	}
	if report.Edges == 0 {
		t.Errorf("the rebuild left %d references, want the ones it wrote", report.Edges)
	}
	for _, table := range projected {
		if now := tableFingerprint(t, database, table); now != before[table] {
			t.Errorf("the rebuild wrote %s: it read %s and left %s, want it to write refs and mark work owed alone",
				table, before[table], now)
		}
	}
}

// documentRefs is every refs row out of one document, by its far end, in one order.
func documentRefs(t *testing.T, database *store.Store, document string) string {
	t.Helper()
	var held string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(string_agg(to_kind || ':' || to_id, ', ' order by to_kind, to_id), '')
		from refs where from_kind = 'artifact' and from_id = $1
	`, document).Scan(&held); err != nil {
		t.Fatalf("read the refs of %s: %v", document, err)
	}
	return held
}

// indexPasses is every pass that writes document_blocks, block_refs, comments, graph_nodes or
// graph_tuples, by a statement of its own or through the store's writer (graphstore.Write and
// the calls beside it), keyed by the file and the function the statement or the call lives in,
// with what that pass reads to decide what it writes. A pass that reads the thing itself - a
// document's render, a request's body, a batch of events - is a writer; one that reads a
// projected table or the store to decide what to write into the other is a repair, and this
// ticket holds that no such pass exists. The scan below holds the register complete: a pass
// added to the tree and not named here fails until someone says what it reads, which is the
// question that tells a writer from a repair.
var indexPasses = map[string]string{
	"api/comment_actions.go#commentAction":   "the request naming the action and the comment it acts on",
	"api/comment_actions.go#editComment":     "the edit request's own body",
	"api/comment_actions.go#reopenComment":   "the reopen request",
	"api/comment_create.go#createCommentFor": "the create request's own body",
	"api/comment_delivery.go#replyComment":   "the reply a delivery recorded",
	// The comment's row and its node, and what it states under its comment.created, which the
	// comment route and the comment an issue write leaves both queue: each reads the comment its
	// caller writes, and its own artifacts' addresses (commentStored).
	"api/comment_create.go#queueCommentRow":     "the comment its caller writes: the create request's own body, or the issue write's own words",
	"api/comment_create.go#queueCommentCreated": "the facts the comment's row write stated and the comment.created event it appended",
	// The orphaning of a comment's anchor, which the accept and the reject arms of commentAction
	// and deleteComment queue when their document write finds the anchor's mark gone
	// (docs.ErrAnchorOrphaned). It sets the orphaned key of the anchor the comment's own row
	// holds and keeps every other key, the artifact_id its anchored_to tuple names among them, so
	// it sets no column the store projects (censustest.ProjectedColumns' anchor) and reads no
	// side of a projection to write the other.
	"api/comment_actions.go#recordOrphanedCommentAnchor": "the comment its caller locked and the document write that found its anchor's mark gone",
	// The clear a delete of a comment or a message makes: one update of the post's own row, with
	// an empty body, no suggestion and the delete's mark - the transaction's now() its caller
	// read and the caller the delete is recorded under - and, where an insert suggestion's quote
	// is the comment's own words, that quote key emptied in the anchor the row holds, its other
	// keys kept. No column it sets is one the store projects (censustest.ProjectedColumns), and
	// it reads no side of a projection to write the other.
	"api/post_history.go#clearDeletedPostBody": "the delete request and the post it deletes, which its caller locked",
	// A delivery attempt's settle states the session it reached (graphstore.QueueReached).
	"api/comment_delivery.go#completeCommentDelivery": "the delivery attempt the sender settles",
	"api/message_delivery.go#completeMessageDelivery": "the delivery attempt the sender settles",
	// An answer that flips a stored attempt to sent records the session it reached and, where
	// that flip is what makes the attempt a delivery, the delivered_to tuple, under the answer's
	// own event. It reads the attempt row the answer flipped and no row of either side.
	"api/comment_delivery.go#queueAnsweredDelivery": "the attempt the answer flipped and the answer's own event",
	// A follow of an ask writes the follower row and the following session's node in one
	// statement (graphstore.WriteArms), and each caller that appends an event after the follow
	// moves the node that follow made onto it (graphstore.QueueCaused). Neither reads a row of
	// either side: the follow writes the session its caller names, and the cause the node the
	// follow answered. A follow planned in a write's read phase (asks.PlannedFollow) queues the same
	// statement on that write's batch.
	"asks/followers.go#Follow":         "the ask and the session its caller names",
	"asks/followers.go#queueFollow":    "the ask and the session its caller names",
	"asks/followers.go#Queue":          "the ask and the author its caller's plan names",
	"api/server.go#appendCausingEvent": "the event its caller appends and the facts its caller's write stated before it",
	"api/asks.go#createAskFor":         "the ask the create request opens and the event it appended after the follow",
	// A renewed approval request follows its ask for the requester and moves the node that
	// follow made onto the last event the renewal appended, or leaves it at the sentinel when
	// the renewal appended none.
	"api/reviews.go#renewApprovalAsk": "the open approval request, the requester and the event the renewal appended",
	// The one writer of an ask's answer state, queued (asks.Settled.Queue): the answer on the
	// ask's node, then its row, from the transition its caller names; and each site that appends
	// what a transition held under the event it appended after it (graphstore.QueueCaused).
	"asks/settle.go#Queue":                             "the transition its caller names",
	"api/ask_transitions.go#closeAskTx":                "the transition the answer, resolve or decline request makes and the event it appended",
	"api/issue_ask_lifecycle.go#recordWithdrawnAsksTx": "the asks an issue's close withdraws and the events it appended",
	"api/issue_ask_lifecycle.go#recordRestoredAsksTx":  "the asks an issue's reopen restores and the events it appended",
	"docs/approval_carry.go#answerApprovalAsk":         "the approval the carry answers and the event it appended",
	// A settlement states each copy among a document's ask blocks as a claim on its source, once
	// its version is indexed: the copies its own reconciliation read off the document, and the
	// rows of the version it indexed, which name each copy's block node.
	"docs/ask_blocks.go#claimCopies": "the copies the settlement's reconciliation read and the blocks its version indexed",
	// Beside those claims, in the same name and under the same event, a settlement states on each
	// ask node the readability of its block that the reconciliation read (G1): the reason a block
	// fails the strict read for, as the node's `invalid` value, and that value's retraction once
	// the block reads again. It reads the value each node holds only to state what moved, never a
	// side of a projection to mend the other.
	"docs/ask_blocks.go#stateInvalidReasons": "the readability the settlement's reconciliation read of the document's ask blocks, and the invalid value each of their asks' nodes holds",
	// The catch-up of a follow a process of the release before recorded: the follower rows that
	// build wrote, stated on the ask's node where the node does not state them yet.
	"asks/followers.go#CatchUpFollower": "the follower rows another build recorded and what the ask's node already states of them",
	// The boot step's backfill of the asks and their followers written before this release.
	"graphcensus/backfill_asks.go#writeAskBatch":         "each pre-store asks row's own content",
	"graphcensus/backfill_asks.go#writeAskFollowerBatch": "each pre-store ask_followers row's own content",
	// A settlement states right after each event it appends what waited for it: its version's
	// block index after the artifact.version, and each ask it opened, its node and its follow,
	// after that ask's ask.opened, with what the index of each ask it opened, restored or reworded
	// stated, those asks' ties and the answer states it wrote (graphstore.QueueCaused,
	// causedByEvent).
	"docs/settlement.go#appendEvents": "the facts the settlement's index pass, its asks' nodes, indexes, ties and follows and its answer states stated and the events it appended after them",
	// The node of every ask and message a request or a settlement creates, before its row, in the
	// project or between the parties it belongs to, and caused by the event the create appends.
	"api/reviews.go#openApprovalAsk":             "the approval request the request opens and the ask.opened it appended",
	"docs/ask_blocks.go#createAskBlock":          "the ask block the settlement opens and the document that holds it",
	"api/artifacts.go#queueNewArtifact":          "the document the upload creates, the node the store holds at its address, and the event the upload appends",
	"api/messages.go#createStoredMessage":        "the message the request stores and the event it appended",
	"api/message_delivery.go#insertSessionReply": "the session's reply the request stores and the event it appended",
	"api/broadcasts.go#queueBroadcastCopy":       "each copy the broadcast stores, the event it appended for it and the copy's first delivery attempt",
	// A broadcast's recipients' pages, each written once before any copy's node and caused by the
	// event of the first copy that hangs from it.
	"api/broadcasts.go#queue":      "the pages of the broadcast's recipients its read phase planned",
	"api/broadcasts.go#queueCause": "the page a copy hangs from and the event of that copy",
	// A document's retirement states its artifact node gone, keeping every other fact of the
	// node, and appends that fact under the document.retired event it appends (graphstore.Caused).
	"docs/retire.go#RetireDocument":   "the document the caller retires and the document.retired event it appended",
	"docs/retire.go#writeRetiredNode": "the retired document's row and the node the store holds of it",
	// A person's Keep of an agent's decision states its kept_by and kept_text_sum on the decision's
	// node and writes their projection row, decision_keeps, from the request and the decision the
	// person read. It reads the kept_by the node holds only to retract one naming another person,
	// so the node holds the one Keep the row projects: what moved, never a side mended.
	"api/decisions.go#keepDecisionNode": "the Keep request, the decision the person read, and the kept_by its node holds",
	// The decisions step's restore and conversion of an issue:
	// each version it writes appends its index pass's facts under the version's event, and the
	// moves, the ask nodes' relations and the Keeps it carries onto a decision's node are drawn
	// from what it read of the issue's documents and asks under their locks, mending nothing.
	"docs/decision_restore.go#writeVersion": "the facts the version's index pass stated and the event that recorded the version",
	"docs/decision_restore.go#writeFacts":   "the restore's and the conversion's plan, read off the documents and the asks rows it locked",
	// A person's own write that takes an answered ask's block out of its document states the
	// person's node and the removed_by of each such ask, from the asks rows the write lost the
	// blocks of and the person the write was made for, under the version's event: planned into the
	// version's index pass (removalsOf), and queued on their own for a version no pass plans.
	"docs/removed_by.go#queueRemovals": "the answered asks a person's own write took the blocks of out and the person it was made for",
	// The removal of an untouched document states its node gone before its row goes, caused by
	// the artifact.removed event it appends; and the Inbox rows its own delete removed state, as a
	// person's read state does, the marks they held (graphstore.FromPerson).
	"api/artifact_removal.go#removeArtifact": "the document the request removes, the Inbox rows its own delete removed and the event it appended",
	// The block index's cause: what a version's index pass stated, appended under the event
	// that recorded the version once it is appended (graphstore.QueueCaused), which reads no
	// projection and mends nothing.
	"api/artifacts.go#storeArtifact":                "the facts the upload's index pass stated, the uploaded document and the event the upload appended",
	"api/artifacts.go#commitArtifactVersionEvent":   "the facts the version's index pass stated and the event that recorded the version",
	"api/issue_create.go#createIssue":               "the facts the spec's index pass stated, the issue the request creates and the issue's own event",
	"cmd/dispatch-seed/write_docs.go#writeDocument": "the facts the seeded version's index pass stated and the seed's own plan",
	"docs/block_index.go#indexOwedDocument":         "the facts the tick's index pass stated and the event that recorded the version",
	// The seed's comments: their nodes, their author's person node and each comment's
	// authored_by tuple, all drawn from the plan before any row is written (commentStoreWrite).
	"cmd/dispatch-seed/write_items.go#writeComments": "the seed's own plan",
	// The seed's issues, asks and messages: each one's node, in its project, drawn from the plan
	// before the batch writes its rows.
	"cmd/dispatch-seed/write_core.go#writeIssues":     "the seed's own plan",
	"cmd/dispatch-seed/write_items.go#writeItemNodes": "the seed's own plan",
	"cmd/dispatch-seed/write_items.go#writeMessages":  "the seed's own plan",
	// The seed's asks: the session node of each follower the batch copies, drawn from the plan.
	"cmd/dispatch-seed/write_items.go#writeFollowerNodes": "the seed's own plan",
	"docs/block_index.go#stampVersionEvent":               "the event that recorded the version",
	// The backfill of the rows written before the store existed. Each pass reads the rows of
	// its own kind and nothing else: a row's own content - its time, its author, the
	// document it is in, its own source_seq - is what it writes into the store, and the
	// store holds nothing about those rows for it to read. That is what makes it a writer
	// and no repair: it never reads one side of a projection to decide what to put in the
	// other, and a row whose node is already written it leaves alone rather than mending. The
	// block batch also marks the document each row is in owed this release's block index
	// (store_owed_at), which is the row's own document and no reading of either side; and
	// markStaleDocuments marks each document another release indexed after the store last
	// stated it, read off the two versions the document's own row records (store_version below
	// version). That pass writes neither a projected table nor the store, so it is no entry here.
	"graphcensus/backfill_blocks.go#writeBlockBatch":        "each pre-store document_blocks row's own content and the document it is in",
	"graphcensus/backfill_block_refs.go#writeBlockRefBatch": "each pre-store block_refs row's own content",
	// The comment write both of the comment batch's writers share - the backfill's comment batch
	// and the block-index tick's write of the comments the events after its cursor name whose rows
	// name no node (refs.CommentsNamingNoNode) - each comment's node and facts, read from the
	// row's own content with the event that stored it as its cause, named on that comment's own row
	// (refs.CommentWrite.Queue). It reads no side of a projection to mend the other and
	// leaves alone a row that names its node.
	"refs/comment_nodes.go#Queue": "each comments row whose node the write states, read from the row's own content, and the event that stored it",
	// The backfill of the roles. A role, its page and the page's part_of it have no projection
	// row, so there is no projection to read one side of: the batch's input is the roles the
	// store census holds to a node, which a row addresses now or an event of the log ever
	// addressed (addressedRoles), and it writes the nodes the store lacks of them, leaving a node
	// another write made alone rather than mending it.
	"graphcensus/backfill_roles.go#writeRoleBatch": "the roles a row addresses now or an event ever addressed (addressedRoles)",
	// The backfill of the sessions, the same case as the roles': a session has no projection
	// row, and the batch's input is the sessions the store census holds to a node, which a row
	// names now or an ask.follower_added event ever named (recordedSessions). It writes the nodes
	// the store lacks of them, and a node another write made it leaves alone.
	"graphcensus/backfill_sessions.go#writeSessionBatch": "the sessions a row names now or an ask.follower_added event ever named (recordedSessions)",
	// The backfill of the project of every node: the rows of the issues, documents, asks and
	// messages that their marker does not mark, each read again under its owner lock before its node is
	// written, and the comment and block nodes that name no project, each with the row its
	// project is read from.
	"graphcensus/projects_backfill.go#writeObjectBatch": "the issue, document, ask and message rows their marker does not mark",
	"graphcensus/projects_backfill.go#writeScopeBatch":  "the comment and block nodes that name no project, and the rows their project is read from",
	// The block-index tick's sweep of a document the release before this one removed without
	// stating its node gone. What decides is the store's own row of the node: a document node not
	// gone at an issue's address that no artifacts row stands behind (graphcensus'
	// OrphanedDocumentNodes), which it locks and reads again to find it still so under that lock.
	// The artifact.removed event naming the address is only the cause it states the node gone
	// under, and the sentinel where there is none, as this build's removal does. It reads no
	// projected table, and the store only to state the store's own next fact, so it is no repair.
	"graphcensus/orphans.go#StateOrphanedGone": "each document node not gone at an issue's address that no artifacts row stands behind, the store's row of that node, and the artifact.removed event naming the address as its cause",
	"graphstore/arms.go#nodeArm":               "the nodes its caller states",
	"graphstore/arms.go#claimArms":             "the claims its caller states",
	"graphstore/arms.go#tupleInsert":           "the tuples its caller states",
	// A node write (queueNodes): the insert of the nodes the store lacks, which locks the ones it
	// holds, and the move of the ones it holds. The move reads each row it moves, in the snapshot
	// taken once the insert holds them all, to say which base attributes the write moved: it reads
	// the store's own row to state what it writes into that row, never a projection to mend the
	// store or the store to mend a projection.
	"graphstore/write.go#queueNodes": "the nodes its caller states and the rows of them it moves",
	// queueAppendStatement, one statement of queueAppend's append, is the second half of one write
	// rather than a second look at it: it appends the tuples its caller states, and states the base
	// attributes of the node rows its node write marked and moves them onto the write's cause,
	// which it reads off those rows' marks (graphstore's nodeMarks). It reads the store's own rows
	// its write just wrote, no projection, and mends nothing.
	"graphstore/write.go#queueAppendStatement": "the tuples its caller states and the node rows its own node write marked",
	// FillScopes states the project of a node an earlier build stored with none: it locks and
	// updates the rows of the nodes its caller names, reading of each only whether it carries a
	// project already and its own made_at, made_by and cause, which the fact it states takes. The
	// project itself is its caller's, read off the node's own row of its kind (graphcensus'
	// backfillScopes), so it reads no projection to mend the store.
	"graphstore/scope.go#FillScopes": "the nodes its caller names and the rows of them it fills",
	// The publisher's record of a batch splices the store's arms into its own statement
	// (graphstore.WriteArms): the roles and sessions the batch reached, and its deliveries.
	"outbox/publisher.go#recordBatch": "the batch of events the publisher sent and the destinations each reached",
	// The record of a fallback pass splices in the same arms for each copy the pass published:
	// the copy's delivered_to and the session its topic names.
	"outbox/mentions.go#recordFallbackCopies": "the fallback copies the pass published and the topic each reached",
	// A version's index pass reads its render (PlanBlocks), and of each decision the author its
	// node's decided_by tuple holds, which markdown never carries (refs.DecisionFacts): that tuple
	// is the one record of who decided, read to restate the same tie, never to mend a row; and
	// BlockIndexing.Queue writes what the plan decided. Where another release indexed the document
	// after the store last stated its blocks, the rows it compares the render with no longer hold
	// the blocks and links that release dropped, so the plan also reads what the store still
	// asserts of the document (heldDocumentBlocks); what it states of those - gone, retracted - is
	// decided by the render that does not hold them, as the parents' plan reads the parents the
	// store asserts to state the render's.
	"refs/blocks.go#Queue":                  "the version's render, each decision's stored decided_by, and where another release indexed the document since the store stated it, what the store still asserts against that render",
	"refs/block_nodes.go#queueBlockParents": "the version's render",
	"refs/block_nodes.go#queueBlockRefs":    "the version's render, and each decision's stored decided_by",
	// The typed-node pass: the decision and reading nodes of the version, their values and their
	// authors, from the version's render and the credential the version's write proved, and above
	// the deferral bound their part_of and the rows of their own ties, and the shaped ties of the
	// asks a settlement opened (refs.DecisionIndexing.Queue, refs.OpenedAskTies.Queue). It reads a
	// node's stored values and authors only to state what moved.
	"refs/decision_nodes.go#Queue": "the version's render and the credential its write proved",
	"refs/refs.go#queueStamp":      "the event that recorded the write",
	// Every write of a delivery attempt's row states what that row now holds
	// (graphstore.QueueAttempted), and an insert writes the attempt's node before it, planned with
	// the row it names (graphstore.AttemptOpened) and caused by the insert's event. Each reads the
	// attempt row its own statement wrote, under the lock that statement holds, and what the store
	// holds of that one node to append only what moved, the way a node write reads the row it
	// moves (queueNodes): it never reads one side of a projection to mend the other.
	"api/comment_delivery.go#openCommentDeliveryAttempt":      "the attempt the retry opens",
	"api/comment_delivery.go#take":                            "the attempt the claim takes",
	"api/comment_delivery.go#standDown":                       "the attempt whose claim the sender gives back",
	"api/comment_delivery.go#supersedeCommentDelivery":        "the attempt a later one superseded",
	"api/comment_delivery.go#recordCommentDeliveryResolution": "the resolution the sender recorded on its attempt",
	"api/comment_mention_claims.go#refreshMentionClaims":      "the claims the mention sender wrote again",
	"api/comment_mention_claims.go#releaseMentionClaims":      "the claims the mention sender gave back",
	"api/comment_mention_claims.go#claimQueuedMention":        "the claim a keyed repeat takes",
	"api/delivery_sweep.go#settleUndeliverableMention":        "the attempt the sweep settles undeliverable",
	"api/message_delivery.go#acceptDelivery":                  "the attempt the session accepts",
	"api/message_delivery.go#recordPendingMessageDelivery":    "the stranded attempt a retry in another mode settles",
	"api/message_delivery.go#claimAttempt":                    "the attempt a claim's own statement takes",
	"api/message_delivery.go#insertMessageDeliveryAttempt":    "the attempt the send opens",
	"api/message_delivery.go#replyMessage":                    "the reply callback and the attempt it settles",
	"outbox/mentions.go#settleAbandoned":                      "the abandoned attempts the outbox settles",
	"graphstore/delivery_attempt.go#QueueAttempted":           "the attempt rows its caller names, as its own statement reads them, and what the store holds of each one's node",
	// The backfill of every kind the child that moves the inbox, the messages and the read state
	// moves but the identity mentions of a text. Each batch reads the rows of its own kind that its
	// marker test passes - the rows an older build wrote, which name no node, hold no tuples_stored
	// or no tuples_stored_at, or carry no marked_seq - and writes into the store what each such row's own
	// content says, under the event that recorded the write, then sets the row's marker. It reads
	// no marked row: an item's block node, which a link and its maker hang from, and the placement
	// a block's part_of would repeat, are read off the store, and a row's marker only ever as the
	// test that passes it. So it never reads one side of a projection to mend the other.
	"graphcensus/backfill_inbox.go#writeMessages":            "each messages row that names no node, and the page its target hangs from",
	"graphcensus/backfill_inbox.go#writeCommentPointers":     "each comments row whose tuples_stored is false",
	"graphcensus/backfill_inbox.go#writeBlocks":              "each item block row that names no node, its item's node, and where the store places that block now",
	"graphcensus/backfill_inbox.go#writeLinks":               "each item link row that names no node out of a block whose node the store holds, that node's maker, and the event that recorded the row's item",
	"graphcensus/backfill_inbox.go#writeRelations":           "the citations its caller read off refs rows that hold no tuples_stored, the delivery routes it read off comment_mentions rows that hold none, and the sessions and roles it read off the published_destinations of events that hold no delivered_to to them",
	"graphcensus/backfill_inbox.go#writeAttempts":            "each attempt row that names no node, and what the store holds of the node it writes for it",
	"graphcensus/backfill_inbox_threads.go#writeUserThreads": "each user_threads row that carries no marked_seq, the event of its last reply, and whether its person's participates_in and dismissed into its thread stand, so it states what the row says and retracts a dismissal the row no longer holds",
	"graphcensus/backfill_inbox_threads.go#writeMarks":       "each read-state row whose tuples_stored_at is null",
	"inboxwrite/older.go#RecordOlder":                        "the write an older build made and the event that recorded it, the person an ask it created waits on, who wrote in its thread before it - the store's authored_by and head pointer tuples, the log's ask.answered events and the rows the marker test passes - and the dismissals into the thread the store holds",
	// The index of an item's body, planned in its write's read phase and queued on its batch
	// (refs.ItemIndexing.Queue): its blocks' nodes, their part_of and links, and the item's
	// citations, read off the item's text and compared with what the store holds of its blocks'
	// placement alone (readItemBlockPlaces), as the plan of the block parents compares a
	// document's.
	"refs/item_blocks.go#Queue":   "the item's text",
	"refs/reply_blocks.go#Queue":  "the item's stored text, read from its own row",
	"refs/rebuild.go#queue":       "the source's stored text",
	"refs/rebuild.go#replaceInTx": "the source's stored text",
	// Each site that appends what an item's index stated under the event of the write that
	// indexed it (graphstore.QueueCaused, graphstore.Caused). The ask transition, the reopen's
	// restore, the approval carry's answer and the settlement, listed above, are such sites too.
	"api/asks.go#editAsk":                              "the edit request and the ask.edited it appended",
	"docs/approval_ask.go#rewriteApprovalAsk":          "the rewording a request or a version move wrote and the ask.edited it appended",
	"docs/decided_wording.go#restoreWordings":          "the decided wording the restore wrote back",
	"cmd/dispatch-seed/write_items.go#stateAtSentinel": "the seed's own plan",
	// The one writer of the threads a person takes part in and of what reaches them there: it reads
	// the write its caller stored, the thread that write's head pointer tuples name, and who takes
	// part in that thread - the participates_in tuples into its root, and the login of each
	// user_threads row of it the marker test passes - and, for an ask's create, the person the ask
	// waits on (asks.AskedOfPersonExpression over the ask's own row and its issue's and project's,
	// or as the create read them) - and appends the participation and the deliveries of that one
	// write, through graphstore.QueueToPerson. An answer's catch-up reads its ask's own row and refs
	// rows while their markers are null, which is the marker test passing. It reads no marked row's
	// columns to decide what to write, so it mends nothing and is no repair. The read mark its
	// writer's own row stands at once its upsert ran is that write's own result, which the same
	// statement states as the writer's read_through (graphstore.MarkArm), as a route states the
	// mark its statement stored.
	"inboxwrite/record.go#Queue":        "the write its caller stored and the event that recorded it, the person an ask it creates waits on, the thread's participates_in tuples, the rows of the thread the marker test passes, and the read mark its own upsert left its writer's row at",
	"inboxwrite/thread.go#queueKeepRow": "the writer's own row of the thread, as its own upsert leaves it, and the read mark that row stands at",
	// The participation of each person a comment or a message newly mentions in its thread, which
	// the writer that resolves the mention queues after that write's record: it reads the mentions
	// its own index states and the thread root its record read, and states that write's own facts.
	"api/comment_create.go#queueRecordAndMentions": "the mentions tuples its caller's index states and the thread root that write's record read",
	// The clear of the Agent dismissals a session's reply ends, as migration 20261105093507's
	// trigger ends them: it takes the root the reply climbed to `for key share`, then reads the
	// people whose dismissal row names that root as the rows stand once it holds the root, and the
	// dismissed marks of theirs into it the store holds, retracts those marks and deletes the rows.
	// What it ends is what the reply's own write ends, so it states that write and mends nothing.
	"inboxwrite/thread.go#queueAgentDismissalsClear": "the root message its caller's session reply climbed to, the people whose dismissal row names it once the root is held, and the dismissed marks of theirs into it the store holds",
	// The move of the person open asks wait on: it locks the asks the write that changed who they
	// wait on may have moved, and reads of each who it waits on now and, in the store, whom its arm
	// stands to - each person whose delivery stands, or whose participation their own move of the
	// ask onto themselves stated, which it tells by that move's event and the ask's row
	// (inboxwrite.OwnMove), and what else holds them there - and appends the participation of the
	// person it waits on, with their delivery unless they made the move, and the retractions of the
	// others' through graphstore.ToPersonArms. It reads the store and the log to state what the
	// store holds next, never a projection to mend it, so it is no repair.
	"inboxwrite/waiting.go#QueueMoveWaiting": "the asks its caller's write may have moved, who each waits on, the tuples of whom its arm stands to, and the event and the ask's row that tell a person's own move of the ask",
	// The statements those writers queue to reach a person: the tuples to a person its caller
	// works out in SQL, with the person nodes they name (ToPersonArms), and a person's own marks -
	// the read_through an upsert of their row states in the same statement (MarkArm), and the
	// retraction of the marks of theirs that stand (QueueMarksEnded). Each reads the rows its own
	// statement works out and, for an end, the marks the store holds, to state what moved.
	"graphstore/to_person.go#ToPersonArms":      "the rows its caller's statement works out, each a tuple to a person, and the person nodes they name",
	"graphstore/from_person.go#MarkArm":         "the rows its caller's upsert moved and the read mark each now stands at",
	"graphstore/from_person.go#QueueMarksEnded": "the people and the thread its caller names, and the marks of theirs into it the store holds asserted",
	// The routes that write a person's read state: each states, through graphstore.FromPerson and
	// under the event it appends, the marks its own statement stored, read back from that
	// statement - the rows it moved and the moment each now holds - so each tuple is the
	// request's own write, never a projection read to mend the store.
	"api/replies.go#putThreadRead":        "the read mark the request carries and the row its own update moved",
	"api/inbox_read.go#commitInboxMarks":  "the marks its route's own statements stored and the events it appends for them",
	"api/state.go#putUserAgentState":      "the Clear, the read mark and the replies the request carries, as its own statements stored them",
	"api/ask_snooze.go#announceAskSnooze": "the snooze or the unsnooze its route's own statement stored and the event it appends",
}

// projectedWrite matches one statement of a Go source file that writes a projected table or the
// store.
var projectedWrite = regexp.MustCompile(`(?is)\b(insert into|update|delete from)\s+(document_blocks|block_refs|comments|graph_nodes|graph_tuples)\b`)

// storeWriterCall matches one use of the store's writer from outside its package, called or
// passed on as a value: a pass that writes the store through graphstore holds no statement of its
// own, and a scan of SQL alone would never see it. asks.Follow picks graphstore.WriteArms or
// graphstore.WriteArmsUncaused by its cause and then calls what it picked, so a scan for calls
// alone would read it as no writer, where graphstore's TestEveryStoreWriteCallSiteIsListed reads
// the same use as one. The names are graphstoretest.WriteFunctions, the one list of every function
// a write of the store from outside graphstore goes through, which that test and api's scan of
// the projected tables' writers read too, so a writer the store gains is scanned here with no
// second list to keep: QueueSessionRecorded writes a session's node through arms of its own, and a
// pass that wrote the store through it alone would otherwise be read as no writer at all.
var storeWriterCall = regexp.MustCompile(`\bgraphstore\.(` + strings.Join(graphstoretest.WriteFunctions(), "|") + `)\b`)

// projectionRead matches one read of either side of a projection: a projected table or the
// store. A `delete from` is a write and no read, so the match takes the delete with it, and the
// first group says it is one.
var projectionRead = regexp.MustCompile(`(?is)(\bdelete\s+)?\b(?:from|join)\s+(document_blocks|block_refs|comments|graph_nodes|graph_tuples|graph_tuples_current)\b`)

// goLineComment matches a whole line of Go comment, which the tree writes its reasoning in: one
// of them spells out an update of a projected table in prose, and reading the prose as a
// statement would hold the tree to a pass nobody wrote.
var goLineComment = regexp.MustCompile(`(?m)^[ \t]*//.*$`)

// requireEveryPassReadsItsSource reads every pass of the source tree that writes a projected
// table or the store, by a statement or through the store's writer, and reports what each one
// reads: the input its register entry names, and the sides of a projection its own statements
// read. The scan is what keeps the register honest: a pass the register does not name fails
// here, with the sides it reads, so a repair added later cannot pass unnoticed, and the
// register's own entry is where a reader sees whether the pass reads the thing itself or one
// side of the projection.
func requireEveryPassReadsItsSource(t *testing.T) {
	t.Helper()
	seen := map[string]bool{}
	for _, file := range dispatchSourceFiles(t) {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := goLineComment.ReplaceAllString(string(body), "")
		writes := append(projectedWrite.FindAllStringIndex(text, -1), storeWriterCall.FindAllStringIndex(text, -1)...)
		for _, match := range writes {
			key := passKey(file, enclosingFunction(text, match[0]))
			sides := projectionSides(functionBody(text, match[0]))
			pass, declared := indexPasses[key]
			if !declared {
				t.Errorf("%s writes %q and is in no register of this test; it reads %s: say what it reads, so a reader can tell a writer from a repair",
					key, strings.TrimSpace(text[match[0]:match[1]]), sides)
				continue
			}
			if !seen[key] {
				t.Logf("%s reads %s; its statements read %s", key, pass, sides)
			}
			seen[key] = true
		}
	}
	for key := range indexPasses {
		if !seen[key] {
			t.Errorf("the register names %s, which writes no projected table and no store table: remove it", key)
		}
	}
}

// functionBody is the whole of the function a statement at offset lives in, from its `func`
// line to its closing brace, in a gofmt-clean file.
func functionBody(text string, offset int) string {
	start := strings.LastIndex(text[:offset], "\nfunc ") + 1
	end := strings.Index(text[offset:], "\n}\n")
	if end < 0 {
		return text[start:]
	}
	return text[start : offset+end]
}

// projectionSides names each side of a projection a pass's statements read: a projected table
// (document_blocks, block_refs, comments) or the store (graph_nodes, graph_tuples), once each.
func projectionSides(body string) string {
	var sides []string
	for _, read := range projectionRead.FindAllStringSubmatch(body, -1) {
		if read[1] != "" {
			continue
		}
		side := read[2] + " (the store)"
		switch read[2] {
		case "document_blocks", "block_refs", "comments":
			side = read[2] + " (a projected table)"
		}
		if !slices.Contains(sides, side) {
			sides = append(sides, side)
		}
	}
	if len(sides) == 0 {
		return "neither side of a projection"
	}
	return strings.Join(sides, ", ")
}

// passKey names one pass by the file and the function the statement lives in, with the file cut
// to the path this register writes.
func passKey(file, function string) string {
	path := strings.ReplaceAll(file, "\\", "/")
	for strings.HasPrefix(path, "../") {
		path = strings.TrimPrefix(path, "../")
	}
	if cut := strings.Index(path, "/dispatch/"); cut >= 0 {
		path = path[cut+len("/dispatch/"):]
	}
	name := strings.TrimPrefix(function, "func ")
	if close := strings.Index(name, ") "); strings.HasPrefix(name, "(") && close >= 0 {
		name = name[close+2:]
	}
	if open := strings.IndexByte(name, '('); open >= 0 {
		name = name[:open]
	}
	return path + "#" + name
}
