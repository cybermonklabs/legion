package docs

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
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

// deferredItems is a bullet list of more items than a write indexes in its own request. Each item
// is two blocks, the list item and its paragraph: R7 of the issue-tree spec nests a moved block
// "under a container that has no words of its own: a `callout` or a list item", so the renderer
// records each list item's offset (renderer.list in pmdoc/render.go) and the block index gives
// the item a node and a row of its own beside its paragraph's. With the list's own block that is
// deferredBlocks, against the bound of deferBlockIndexBlocks.
const deferredItems = deferBlockIndexBlocks + 1

// deferredBlocks is how many blocks deferredList(deferredItems) holds: the list, and each item
// with its paragraph.
const deferredBlocks = 1 + 2*deferredItems

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

// writeDocumentThroughTheRequestPath writes one document the way this build's request writes it
// (api's storeArtifact): the document's node and its part_of its issue written before its row,
// the row naming that node, both dated with one time (graphstore.StatementTime), the text seeded
// in the writer's transaction, the version row, the version's references and blocks indexed from
// the rendering that transaction wrote, the version's node (WriteVersionNode), the version's
// event, and then what the node writes and the block index stated, appended under that event.
func writeDocumentThroughTheRequestPath(t *testing.T, service *Service, database *store.Store, slug, markdown string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the document write: %v", err)
	}
	defer tx.Rollback(ctx)
	writer := model.Actor{Kind: "user", ID: "alice"}
	refKey := graphstore.ArtifactRefKey("CORE-1", "CORE", "", slug)
	createdAt, err := graphstore.StatementTime(ctx, tx, graphstore.KindArtifact, refKey)
	if err != nil {
		t.Fatalf("read the time of document %s: %v", slug, err)
	}
	documentNode := graphstore.PlanUncaused(
		[]graphstore.Node{graphstore.Created(graphstore.ArtifactNode(refKey, "CORE", "", writer, createdAt))},
		[]graphstore.Tuple{graphstore.ArtifactPartOf(refKey, "CORE-1", writer, createdAt)})
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueNodes(b, documentNode)
		return nil
	}); err != nil {
		t.Fatalf("write the node of document %s: %v", slug, err)
	}
	var artifactID string
	if err := tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, node_kind, node_address, created_at)
		values ('CORE-1', 'CORE', $1, $1 || '.md', 'doc', false, '{"kind":"user","id":"alice"}', $2, $3, $4)
		returning id::text
	`, slug, graphstore.KindArtifact, refKey, createdAt).Scan(&artifactID); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	stored, err := service.SeedText(joined, artifactID, markdown, writer)
	if err != nil {
		t.Fatalf("seed %s: %v", slug, err)
	}
	var versionAt time.Time
	var owner graphstore.VersionOwner
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions as v (artifact_id, number, markdown, authors)
		values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
		returning created_at, `+graphstore.VersionOwnerColumns("v.artifact_id"),
		artifactID, stored).Scan(append([]any{&versionAt}, owner.Targets()...)...); err != nil {
		t.Fatalf("write version 1 of %s: %v", slug, err)
	}
	_, write, err := service.IndexDocumentReferences(joined, artifactID, 1, stored)
	if err != nil {
		t.Fatalf("index the references of %s: %v", slug, err)
	}
	// The version's node, queued after its block index's block nodes on the writer's ledger
	// (joined), held for the event below with the document's own node, as storeArtifact holds it.
	versionNode, err := WriteVersionNode(joined, tx, owner, 1, writer, versionAt)
	if err != nil {
		t.Fatalf("write the node of version 1 of %s: %v", slug, err)
	}
	documentNode = graphstore.Joined(documentNode, versionNode)
	payload, err := json.Marshal(map[string]any{"artifact": map[string]any{"id": artifactID}})
	if err != nil {
		t.Fatalf("encode the created event's payload: %v", err)
	}
	event, err := documentEvent(ctx, tx, "artifact.created", payload)
	if err != nil {
		t.Fatalf("record the document's event: %v", err)
	}
	if err := causeInWritePhase(joined, tx, documentNode, event); err != nil {
		t.Fatalf("append what the document's and its version's node writes stated: %v", err)
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

// blockRowTypes is how many rows of document_blocks the document holds of each block type.
func blockRowTypes(t *testing.T, database *store.Store, artifactID string) map[string]int {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select type, count(*) from document_blocks where artifact_id = $1 group by type`, artifactID)
	if err != nil {
		t.Fatalf("count the block rows of %s by type: %v", artifactID, err)
	}
	defer rows.Close()
	types := map[string]int{}
	for rows.Next() {
		var blockType string
		var count int
		if err := rows.Scan(&blockType, &count); err != nil {
			t.Fatalf("scan a block type of %s: %v", artifactID, err)
		}
		types[blockType] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count the block rows of %s by type: %v", artifactID, err)
	}
	return types
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
// no row of the store. The rebuild writes refs and marks work owed and writes no row of
// document_blocks, block_refs or comments.
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
	// The rows are the list's, each item's and each item's paragraph's (deferredBlocks): R7 makes
	// each list item a node beside the paragraph it holds.
	blocks := countRows(t, database, "document_blocks", document)
	types := blockRowTypes(t, database, document)
	if blocks != deferredBlocks {
		t.Fatalf("the tick wrote %d block rows of the %d-item list (%v by type), want %d: the list, and each item and its paragraph",
			blocks, deferredItems, types, deferredBlocks)
	}
	if want := map[string]int{"bullet_list": 1, "list_item": deferredItems, "paragraph": deferredItems}; !maps.Equal(types, want) {
		t.Errorf("the tick wrote block rows of %v by type, want %v", types, want)
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
	requireTheItemBlockPassWritesNoStoreRow(t, database, service)
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

// requireTheItemBlockPassWritesNoStoreRow runs the item block pass (refs.IndexOwedItemBlocks,
// the tick's third batch) over a comment owed the index of its own blocks, and holds the pass to
// writing no row of graph_nodes and no row of graph_tuples: this build moves no body block of a
// comment, a message or an ask into the store. Every document is indexed first, so the tick
// that runs the pass has no document of its own to write, and a row of either table that moved
// is the pass's.
func requireTheItemBlockPassWritesNoStoreRow(t *testing.T, database *store.Store, service *Service) {
	t.Helper()
	ctx := context.Background()
	drainBlockIndex(t, service, "every document before the item block pass")
	// A comment of an older build: the row alone, which migration 0143's trigger leaves owed
	// a block index (comment_block_state.owed_at). Two paragraphs, one citing an issue, so the
	// pass has blocks and a block reference to write.
	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', E'One.\n\nTwo cites dispatch://CORE-2.')
		returning id::text
	`).Scan(&comment); err != nil {
		t.Fatalf("write the owed comment: %v", err)
	}
	nodes, tuples := tableFingerprint(t, database, "graph_nodes"), tableFingerprint(t, database, "graph_tuples")
	tick := reconcileBlockIndex(t, service, "")
	if tick.ItemBlocks != 1 || tick.Documents != 0 {
		t.Fatalf("the tick indexed the blocks of %d items and %d documents, want the owed comment's alone", tick.ItemBlocks, tick.Documents)
	}
	var blocks int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from comment_blocks where comment_id = $1::uuid`, comment).Scan(&blocks); err != nil {
		t.Fatalf("count the comment's blocks: %v", err)
	}
	if blocks != 2 {
		t.Fatalf("the item block pass wrote %d blocks of the two-paragraph comment, want 2: it ran over nothing", blocks)
	}
	// A fingerprint is the row count, then every row's xmin: the message prints the counts,
	// since a table of the store holds thousands of rows by now.
	rowsOf := func(fingerprint string) string { return strings.SplitN(fingerprint, "|", 2)[0] }
	if now := tableFingerprint(t, database, "graph_nodes"); now != nodes {
		t.Errorf("the item block pass wrote graph_nodes: it held %s rows and left %s, or rewrote one, want no row of the store written",
			rowsOf(nodes), rowsOf(now))
	}
	if now := tableFingerprint(t, database, "graph_tuples"); now != tuples {
		t.Errorf("the item block pass wrote graph_tuples: it held %s rows and left %s, or rewrote one, want no row of the store written",
			rowsOf(tuples), rowsOf(now))
	}
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
	"api/comment_actions.go#reopenComment":   "the reopen request",
	"api/comment_create.go#createCommentFor": "the create request's own body",
	// The comment's row, written from the comment its caller writes - the create request's own
	// body and the visibility its route decided, or the issue write's own words - with the node
	// and node columns it names; it reads no row of either side.
	"api/comment_create.go#queueCommentRow": "the comment its caller writes: the create request's own body and the visibility its route decided, or the issue write's own words",
	// What the comment states, caused by its comment.created event: its node, its base attributes,
	// the artifacts it is anchored to and owned by, and who wrote it (the author's node and the
	// authored_by tuple), all from the comment the create stored; it reads no row of either side.
	"api/comment_create.go#queueCommentCreated": "the facts the comment's row write stated and the comment.created event it appended",
	// An edit of a comment's words writes its row from the request's body, and, made for a run,
	// restates the comment's node from that row it locked and caused by its own event, so the
	// run's change of the comment is named (changed_by_run), as an edit of an ask's words names
	// the ask's. Neither reads a row of either side to decide what the other says.
	"api/comment_actions.go#editComment": "the edit request's own body, the comment's row it locked and the comment's own event",
	"api/asks.go#editAsk":                "the edit request's own body, the ask's row it locked and the ask's own event",
	// A comment's anchor block (T17b of the issue-tree plan): the anchored_in its
	// comments.anchor's block_id projects, kept by each write that moves the anchor's block or its
	// orphan flag. An accept, a reject or a delete that finds a suggestion's mark gone writes the
	// anchor orphaned from the request and the comment's row it locked, and retracts the block that
	// row names; the refresh writes the quote and orphan flag it read off the document's marks, and
	// retracts or states the block the row it updated names; the anchor block backfill pins the
	// block the live document holds the legacy anchor's quote in, and states that block where the
	// row it pinned stands. None reads the store to decide what a row says, nor a row to mend the
	// store: each states what its own write moved.
	"api/comment_actions.go#orphanCommentAnchor":      "the accept, reject or delete request, the comment's row it locked and the block that row's anchor names",
	"docs/anchor_refresh.go#stateRefreshedAnchoredIn": "the anchors the refresh orphaned or found again off the document's marks, the rows it updated and the events it appended",
	"docs/anchor_blocks.go#pinCommentAnchorBlock":     "the live document's block that holds the legacy anchor's quote, and the row it pins it on",
	"api/comment_delivery.go#replyComment":            "the reply a delivery recorded",
	// The orphaning of a comment's node anchor (this issue's spec R31): a write that takes a node
	// gone, or appends its text, marks each comment anchored on that node orphaned and retracts the
	// anchored_in and, for a comment that answers nothing, the part_of that comment's anchor states,
	// in its own batch (docs.NodeAnchorOrphans). The comments are read in that write's read phase off
	// the rows whose node anchor names one of its nodes, and what it orphans follows from the write's
	// own nodes and text: it mends neither side of a projection from the other.
	"docs/anchor_refresh.go#Queue": "the comments whose node anchor names a node the write takes gone or whose text it moves, read in that write's read phase, and the write's own time and writer",
	// An edit of an issue message's words and a delete of a message or a comment, each made for a
	// run, restate the post's node from the row they locked, caused by their own event, so the
	// run's change of the post is named (changed_by_run). None reads a row of either side to
	// decide what the other says: the clear of the post's row is clearDeletedPostBody's, below.
	"api/message_corrections.go#editMessage":   "the edit request's own body, the message's row it locked and the message's own event",
	"api/message_corrections.go#deleteMessage": "the delete request, the message's row it locked and the message's own event",
	"api/comment_delete.go#deleteComment":      "the delete request, the comment's row it locked and the comment's own event",
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
	// rows of the version it indexed, which name each copy's block node. It retracts the
	// copied_from of each block that holds one and is no longer an ask block, and reads the
	// copied_from each block node of the document holds only to find those blocks, a relation no
	// table projects, never a side of a projection to mend the other.
	"docs/ask_blocks.go#claimCopies": "the copies the settlement's reconciliation read, the blocks its version indexed, and the copied_from each block node of the document holds",
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
	// after that ask's ask.opened, with those asks' ties and the answer states it wrote
	// (graphstore.QueueCaused, causedByEvent).
	"docs/settlement.go#appendEvents": "the facts the settlement's index pass, its asks' nodes, ties and follows and its answer states stated and the events it appended after them",
	// The node of every ask and message a request or a settlement creates, before its row, in the
	// project or between the parties it belongs to, and caused by the event the create appends.
	"api/reviews.go#openApprovalAsk":             "the approval request the request opens and the ask.opened it appended",
	"docs/ask_blocks.go#createAskBlock":          "the ask block the settlement opens and the document that holds it",
	"api/artifacts.go#queueNewArtifact":          "the document the upload creates, the node the store holds at its address, and the event the upload appends",
	"api/messages.go#createStoredMessage":        "the message the request stores and the event it appended",
	"api/message_delivery.go#insertSessionReply": "the session's reply the request stores and the event it appended",
	"api/broadcasts.go#writeBroadcast":           "each copy the broadcast stores and the event it appended for it",
	// The nodes of the reasoning kinds - a lens, its findings, a risk and a
	// constraint - written with their tuples and their author in one store write (graphstore.Write)
	// from the request alone: they have no projection row, so there is no side of one to read. It
	// reads each parent's row again under the issue's row only to refuse a parent gone since the
	// route found it (refuseGoneParents), which decides no fact it writes.
	"api/reasoning_write.go#writeReasoningNodes": "the request's nodes and tuples, its credential, the project of the issue it names and the state of each node it writes under",
	// One pointer tuple out of a node of an issue, appended, retracted or retargeted through the
	// store's writer from the request alone: the tuple has no projection row either. It reads the
	// node's row again under the issue's row only to refuse a node gone since the route found it.
	"api/reasoning_write.go#writeReasoningPointer": "the request's tuple, the node it goes out of and the far ends it names",
	// One node's state, written or settled through the store's writer from the request and the
	// node's own row: the state is a base attribute of the node, which no projection row holds. A
	// node taken gone takes its whole part_of subtree gone in the same write, each node from its own
	// row, which the write reads through the current part_of tuples (graphstore.WriteState).
	"api/reasoning_write.go#writeNodeState": "the request's state, the node's own row, the rows of every node under a node it takes gone, and the issue it stands under",
	// A node's move (PATCH /api/v1/issues/{key}/nodes/{id}) states, at the sentinel, the
	// retraction of the part_of the node held and a part_of to the parent the request names, its
	// rank among that parent's children and a block's stored placement, as its route planned them
	// under the owner locks of the issues the node and its parent stand under; it writes a moved
	// block's parent_block_id from that same parent, the projection of the part_of it states, and
	// marks those issues' queries owed. It reads graph_tuples only for the seq its own new part_of
	// took, the placement_seq the route answers: it reads no side of a projection to mend the other.
	"api/node_move.go#queueNodeMove": "the move its route planned under the issues' owner locks - the node, the part_of it held, the parent and the rank the request places it at, and whether it stores a block's placement - and the seq of the part_of its own write appended",
	// A node's delete (DELETE /api/v1/issues/{key}/nodes/{id}, this issue's spec R7) states, at the
	// sentinel, each node it takes gone that no version states gone and the retraction of every
	// tuple out of what it takes, as its route planned them under the owner locks of the issues
	// they stand under, with the orphaning of each comment anchored on a node it takes and the head
	// rows of what it states. The plan reads the nodes and tuples the store holds under the node
	// the request deletes in order to take them gone: the store read to write the store, no
	// projection to mend it, nor the store to mend a projection.
	"api/node_delete.go#queueNodeDelete": "the delete its route planned under the issues' owner locks - each node it takes gone and every tuple out of what it takes, as the store holds them - and the comments anchored on those nodes",
	// An issue's own field write made for a run: the issue's node, restated from the write's own
	// fields and the issue's row it locked, and caused by the issue's own event, so the run's change
	// of the issue is named (changed_by_run). It reads no row of either side to decide what the
	// other says.
	"api/issue_patch.go#writeIssue": "the write's own fields (the PATCH's body, or a status change step's), the issue's row it locked and the issue's own event",
	// An issue's relations its create and its PATCH state (R9, R10 of the issue-tree spec): the
	// claims and tuples the request names, read against the issue's own row, the issue_links rows
	// it holds and the tuples its node states only to state what moved, and the node of the item
	// its filed_from names where the store held none, from that item's own row, as the backfill
	// writes it. Neither reads one side of a projection to mend the other.
	"api/issue_relations.go#queueIssueRelations": "the request's relations and the issue's own event",
	"api/issue_relations.go#stateFiledFromNode":  "the row of the item the request's filed_from names",
	// A session's recorded result and its retraction (the result node, T11 of the issue-tree plan):
	// the record states the result's node, its values and its tuples from the request and the
	// check block it resolved under the owner locks, and the retraction states the retracted
	// value of the result its request names where its read phase found the result's node; neither
	// mends a projection.
	"api/results.go#writeRecordedResult":                "the result the request records, the check block it resolved and the event it appended",
	"api/results_retract.go#writeCheckResultRetraction": "the retraction the request records, whether its result's node stands and the event it appended",
	// A query's version a write owes where it moved the query's record or its document's
	// rendering and wrote that document no version (planRevisions, writeRevisions): the version's
	// node, after the version's row and before its version_renders row, and its facts caused under
	// an event. It decides the version from the record of each query of the issue and the render of
	// its document, both read at one cut of the writer's own transaction, against the document's
	// newest version, and writes a new version from that read: it reads no projection row to mend
	// the store, nor the store to mend a projection. The drain (drainMark) writes it for the issue a
	// mark names - a result's, a retraction's or an issue room's settlement's - once that write has
	// committed and the fill of the record digests the issue's versions owe has committed after it,
	// under the mark's event.
	"docs/query_marks.go#drainMark": "the issue's query mark, and each query's record and its document's render at a cut of the drain's transaction, its newest version and the mark's event",
	// R2's daemon-thread step re-places a block that stands under a message of a thread the Legion
	// daemon's session began under its issue's node, from the part_of and its seq it reads again
	// under the issue's owner lock: it mends no projection.
	"docs/daemon_thread_step.go#queueDaemonThreadMoves": "the blocks that stand under a message of a daemon thread, each with the seq of its part_of, read again under its issue's owner lock",
	// R6's conversion states the issue's tree, query nodes and moves from the rooms it read under
	// their locks and the rows those rooms' blocks held, in one transaction an issue.
	"docs/issue_conversion.go#writeTree":           "the issue the conversion converts and the tree it draws",
	"docs/issue_conversion.go#writeQueries":        "the issue's converted documents and whether it holds a decisions query",
	"docs/issue_conversion.go#writeDecisionsQuery": "the issue the conversion converts and the decisions query it creates",
	"docs/issue_conversion.go#writeMoves":          "each moved block's old and new address, and the authored_by, kept_by and kept_text_sum tuples its old node states, else the authors of the first version that held it",
	"docs/issue_conversion.go#moveAsks":            "the asks rows whose block the converted documents hold, and the block ids the conversion re-minted",
	"docs/issue_conversion.go#movePointers":        "the tuples, refs rows and block_refs rows at the moved blocks' old addresses, and the check_results rows of the converted documents' checks",
	"docs/issue_conversion.go#finish":              "the facts the conversion's retirement and room passes stated, and the artifact.version it appended",
	// A component's node and its relations (R9 of the issue-tree spec): an import states them from
	// the model it fetched, read against the part_of and depends_on each node states now only to
	// state what moved, and the components it drops from the source's rows as they stood; a
	// source's removal states its components gone from the rows its delete is about to take.
	// Neither reads one side of a projection to mend the other.
	"architecture/importer.go#stateComponents":  "the model the import fetched and the source's components before it",
	"architecture/importer.go#RetireComponents": "the components the source's removal deletes",
	// A run of the command bar: its interaction node, its author's node and their tuples, written
	// from the POST alone before the going_requests row that projects them, and each frame's value,
	// written from the frame its writer hands it before the going_events row that projects it.
	// Neither reads a row of either side to decide what the other should say.
	"api/going.go#recordGoingRequest": "the POST's question, its project and the credential it proved",
	"api/going.go#appendGoingEvent":   "the frame its writer hands it and the run's last sequence",
	// The one writer of a run's node from its row (graphstore.WriteRun), for a run a pod of the
	// release before recorded with no node: the first frame this build writes of such a run writes
	// it first (lockGoingRun), and the backfill writes it for each such run it finds
	// (writeOwedRun). Its input is that going_requests row's own content and the run's going_events
	// rows, as the other backfill passes read each pre-store row's own, and a run whose node is
	// already written it leaves alone rather than mending.
	"api/going.go#lockGoingRun":                 "one going_requests row an earlier build recorded with no node, and that run's going_events rows",
	"graphcensus/backfill_runs.go#writeOwedRun": "one going_requests row an earlier build recorded with no node, and that run's going_events rows",
	// The sweep of a run an hour after it was asked (goRunGone): it moves the state of the run's
	// interaction node alone, from the node as the store holds it, to gone, and then deletes the
	// run's rows. Its input is the run's age, which its row or its node gives, and the node itself;
	// it reads no row of either side to decide what the other should say, and mends neither.
	"api/going.go#goRunGone": "the interaction node of a run more than an hour old, as the store holds it, and the run's going_requests row",
	// The backfill's passes over what a process of the release before writes of a run during a
	// roll: the frames it appended with no value, whose input is each going_events row's own
	// content, found by an anti-join with the run's frame values and stated from the row, never
	// read back; and a run whose row it deleted, whose input is that the row is gone and the node
	// as the store holds it, of which it moves the state alone, never mending a row from the store.
	// Beside them, the retraction of each prevented_by a process of the release before left
	// asserted at a check it dropped or at a check of a document it retired: the store's own
	// relations, read as the store reads them now (refs.PreventionRetractions), with no projection
	// read to mend another.
	"graphcensus/backfill_runs.go#writeRunFrames":               "each going_events row an earlier build appended with no frame value, and the run's node row it takes first",
	"graphcensus/backfill_runs.go#writeGoneRun":                 "the interaction node whose going_requests row an earlier build deleted, and that the row is gone",
	"graphcensus/backfill_preventions.go#writeEndedPreventions": "the prevented_by tuples the store asserts now at a check whose block node is gone or whose document is retired",
	// A document's retirement states its artifact node gone, keeping every other fact of the
	// node, retracts each prevented_by the store asserts at a block of the document - the store's
	// own relations, which no projection holds, retracted because nobody runs a retired document's
	// checks (retiredPreventions) - and appends those facts under the document.retired event it
	// appends (graphstore.Caused).
	"docs/retire.go#RetireDocument":   "the document the caller retires, the prevented_by tuples the store asserts at its blocks, and the document.retired event it appended",
	"docs/retire.go#writeRetiredNode": "the retired document's row, the node the store holds of it and the facts its caller states beside it",
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
	// the artifact.removed event it appends, with each changed_by_run the node states now
	// retracted - the store's own relations, retracted because the document they name has gone. The
	// lenses on the document that the delete leaves stale go gone with all beneath them, read as the
	// stale lenses on its node (refs.StaleLensesOn) and what stands beneath each
	// (graphstore.WriteState), where it read one standing there.
	"api/artifact_removal.go#removeArtifact": "the document the request removes, the changed_by_run its node states, the lenses that stand on its node, and the event it appended",
	// The block index's cause: what a version's index pass stated, appended under the event
	// that recorded the version once it is appended (graphstore.QueueCaused), which reads no
	// projection and mends nothing.
	"api/artifacts.go#storeArtifact":                "the facts the upload's index pass stated, the uploaded document and the event the upload appended",
	"api/artifacts.go#commitArtifactVersionEvent":   "the facts the version's index pass stated and the event that recorded the version",
	"api/issue_create.go#createIssue":               "the facts the spec's index pass stated, the issue the request creates and the issue's own event",
	"cmd/dispatch-seed/write_docs.go#writeDocument": "the facts the seeded version's index pass stated and the seed's own plan",
	"docs/block_index.go#indexOwedDocument":         "the facts the tick's index pass stated and the event that recorded the version",
	// An issue room's one pass, by a settlement of the room or its reconciliation: what it stated
	// of each document's blocks, under the event that recorded that document's version.
	"docs/block_index.go#causeRoomFacts": "the facts the room's index pass stated, by document, and the event that recorded each document's version",
	// The facts of the documents and versions a write stores: the node of each
	// version every version insert writes, from the version its write queued and what that
	// version's node takes of its document's row (ReadVersionOwner); a removal's document and
	// versions stated gone, read off the nodes the store holds of them so each keeps its supertag
	// and its values, the document's with each changed_by_run its node states retracted, and
	// whether a lens stands on the document's node, off the part_of tuples into it, for the removal
	// to state the lenses its delete leaves stale gone through the sweep's statement
	// (graphcensus/orphans.go#StateStaleLensesGone) (removedDocumentNodes), which reads the store to
	// state what it writes into those same nodes and mends no projection from it; and an issue's
	// tree artifact, its node and its part_of written from the issue its caller names, before the
	// rows naming it; and an issue born converted's decisions query node, its node and its part_of
	// written the same way, from the issue its create makes, before the row naming it.
	"docs/mutation.go#WriteVersionNode":            "the version its write queued and what the version's node takes of its document's row",
	"api/artifact_removal.go#removedDocumentNodes": "the document the removal takes, the nodes the store holds of it and its versions, and the part_of tuples into its node",
	"api/artifacts.go#createIssueTree":             "the issue its caller names and the slugs its documents hold",
	"api/issue_create.go#createDecisionsQuery":     "the issue its caller creates and the slugs its documents hold",
	// The seed's comments: their nodes, their author's person node and each comment's
	// authored_by tuple, all drawn from the plan before any row is written (commentStoreWrite).
	"cmd/dispatch-seed/write_items.go#writeComments": "the seed's own plan",
	// The seed's issues, asks and messages: each one's node, in its project, drawn from the plan
	// before the batch writes its rows.
	"cmd/dispatch-seed/write_core.go#writeIssues":     "the seed's own plan",
	"cmd/dispatch-seed/write_items.go#writeItemNodes": "the seed's own plan",
	// The head rows of the seed's asks and messages, the nodes writeItemNodes wrote for each batch.
	"cmd/dispatch-seed/write_items.go#writeAsks":     "the seed's own plan",
	"cmd/dispatch-seed/write_items.go#writeMessages": "the seed's own plan",
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
	// markStaleDocuments marks each document no pass of this build's rule has stated at its
	// version, read off the record the document's own row holds (stated_version and stated_rule).
	// That pass writes neither a projected table nor the store, so it is no entry here.
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
	// The backfill of the versions: each artifact_versions row whose node the store does not hold,
	// found by an anti-join on graph_nodes since the row names no node, and its document's row,
	// which give the node its address, its project and whether it is a query's version.
	"graphcensus/census_versions.go#writeVersionBatch": "the artifact_versions rows no version node stands for, and their documents' rows",
	// The same nodes written by the block-index pass that drains a mark of its documents (docs'
	// indexRenderedVersion: the reconciliation's pass on either mark, a writer's on this build's,
	// up to missingVersionNodesBound): the same read of the same rows, limited to the versions of
	// the documents the pass covers, less the version its own writer writes the node of, each
	// row's own content and its document's row, so it mends no projection from the store either.
	"graphcensus/census_versions.go#Queue": "the artifact_versions rows of the documents the draining pass covers that no version node stands for, and their documents' rows, read in the pass's read phase (PlanMissingVersionNodes)",
	// The backfill of the waits a process of the release before wrote: each issue_links row naming
	// no node, its own content, read again under its issue's row, and that issue's own row, whose
	// node the object pass wrote first.
	"graphcensus/census_issues.go#writeIssueLinkBatch": "each pre-store issue_links row's own content and its issue's row",
	// The backfill of the comments' anchor blocks (T17b of the issue-tree plan): each comments
	// row's own anchor, read again under its owner row, and the anchored_in its node states now,
	// found by an anti-join since the row names no tuple: the tuple is the marker, as a version's
	// node is the versions pass's. It states only what levels that node with its own row.
	"graphcensus/census_anchors.go#writeAnchorBatch": "each comments row's own anchor, read again under its owner row, and the anchored_in its node states",
	// The ask block pointer backfill compares the marked row with the current asked_on tuple.
	"graphcensus/census_citations_asks.go#writeAskedOnBatch": "each marked asks row's block_artifact_id and block_id, read again under its owner with the asked_on its node states",
	// The backfill of the approval requests' documents (R14 of the issue-tree spec): each marked
	// approval ask's approval.documents, read again under its owner row, and the approves tuples
	// its node states now, found by an anti-join both ways since the row names no tuple: the tuple
	// is the marker, as an anchor's is. It states only what levels that node with its own row.
	"graphcensus/census_approvals.go#writeApprovesBatch": "each marked approval ask's approval.documents, read again under its owner row, and the approves its node states",
	// A move of an approval request's documents: the versions its caller moved the row to, which
	// the rewrite's own arguments name, and the ask's node address, read to say whether the request
	// names a node at all. It reads no side of a projection to mend the other.
	"docs/approval_ask.go#rewriteApprovalAsk": "the documents its caller moved the request to and the request's node address",
	// The backfill's Keep pass (R32 of the issue-tree spec): each Keep's decision_keeps row and the
	// kept_text_sum its decision's node states, read again under the issue's owner row, each held
	// to the sum of the decision's words with shaped, which a release before summed, and both written
	// to the sum of the same words without it from that rule alone. It reads neither side to mend
	// the other: a side that holds any other sum is left as it stands.
	"graphcensus/census_keeps.go#writeKeepSumBatch": "each Keep's decision_keeps row, its node's kept_text_sum and its decision's words, read again under its owner row",
	// The backfill of the references a document makes: each refs row of a document whose row
	// names its node, its own content, read again under its document's owner row, and the
	// mentions that node states now, found by an anti-join both ways since refs names no tuple: the
	// tuple is the marker, as an anchor's is. It states only the tuple of a row that holds none, and
	// retracts only a tuple whose row is gone.
	"graphcensus/census_refs.go#writeReferenceBatch": "each refs row of a document whose row names its node, read again under its owner row, and the mentions that node states",
	// The backfill of the components a process of the release before wrote (R9 of the issue-tree
	// spec): each components row its marker does not mark, its own content and the parent it
	// names, read again under its source's row, with the part_of its node holds now, so it states
	// only the part_of that moved into that same node; each component_depends row naming no node,
	// its own content, under the same row; and each issue_component_members row naming no node, its
	// own content, read again under its issue's row. None reads one side of a projection to mend
	// the other.
	"graphcensus/census_components.go#writeComponentBatch":            "each components row its marker does not mark, its own content and its snapshot's time",
	"graphcensus/census_components.go#writeComponentDependsBatch":     "each pre-store component_depends row's own content and its component's row",
	"graphcensus/census_components.go#writeIssueComponentMemberBatch": "each pre-store issue_component_members row's own content and its issue's row",
	// The backfill of the results and the retractions a process of the release before recorded
	// with no node (backfillResults): each result row whose node the store does not hold, read
	// again under the owner rows of its check's issue, and each retraction whose value its result's
	// node does not state, read again the same way. The store is read only to find the rows it
	// holds no node of, never to mend a projection from it.
	"graphcensus/census_results.go#writeResultBatch":           "each result row whose node the store does not hold, read again under its issue's owner row",
	"graphcensus/census_results.go#writeResultRetractionBatch": "each retraction whose value its result's node does not state, read again under its issue's owner row",
	// The block-index tick's sweep of a document the release before this one removed without
	// stating its node gone. What decides is the store's own row of the node: a document node not
	// gone at an issue's address that no artifacts row stands behind (graphcensus'
	// OrphanedDocumentNodes), which it locks and reads again to find it still so under that lock.
	// The artifact.removed event naming the address is only the cause it states the node gone
	// under, and the sentinel where there is none, as this build's removal does. It states nothing
	// beneath the node gone: the stale lenses on the document are StateStaleLensesGone's. It reads
	// no projected table, and the store only to state the store's own next fact, so it is no repair.
	"graphcensus/orphans.go#StateOrphanedGone": "each document node not gone at an issue's address that no artifacts row stands behind, the store's row of that node, and the artifact.removed event naming the address as its cause",
	// The block-index tick's sweep that states every stale lens gone, after its orphan sweep: what
	// decides is the lens's own recorded row (graphstore.AttributeArtifactID) against the artifacts
	// row at its document's address (refs.StaleSQL), the lens's own fact and the address's own row,
	// neither of which is the other side of a projection, and what stands under each stale lens goes
	// gone with it from its own row (graphstore.WriteState). It mends nothing, so it is no repair.
	"graphcensus/orphans.go#StateStaleLensesGone": "each lens's recorded row and the artifacts row at its document's address, the rows of every node beneath each stale one, and the database's clock",
	// The store's own record, beside its write of a lens's part_of, of the row of the document the
	// lens is placed on (graphstore.AttributeArtifactID): the lens's part_of the write states, the
	// artifacts row at the address of the document it names - a document's id, or the address before
	// the `#` of a block's id - the identity of the document, which no node of the store carries, and
	// the value the lens's row held. It states the lens's own next fact from its own placement, and
	// mends neither side of a projection, so it is no repair.
	"graphstore/lens_row.go#queueLensArtifactIDs": "the part_of tuples the write places lenses by, the artifacts row at the address of the document each names, and the artifact_id each lens holds",
	"graphstore/arms.go#nodeArm":                  "the nodes its caller states",
	"graphstore/arms.go#claimArms":                "the claims its caller states",
	"graphstore/arms.go#tupleInsert":              "the tuples its caller states",
	// The two seeds other packages' tests share (graphstoretest), which no deployment builds. Each
	// writes rows the store's writers refuse - a node reserved for its author of a kind no read
	// registry flags, the type of a kind only a test writes - from the nodes or the names its test
	// hands it. SeedAuthorOnly reads the store's own row of each node only to know whether to
	// insert it or move its visibility, never a projection, so neither mends one side from the
	// other.
	"graphstore/graphseedtest/graphseedtest.go#SeedAuthorOnly": "the nodes its test hands it, and whether the store holds each",
	"graphstore/graphseedtest/graphseedtest.go#SeedSupertags":  "the type names its test hands it",
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
	"graphstore/append.go#queueAppendStatement": "the tuples its caller states and the node rows its own node write marked",
	// queueChangedByRun, the last statement of an append made for a run, appends the changed_by_run
	// each node the write changed owes the run, reading only whether the node already holds an
	// asserted one to that run's interaction, which the store's own tuples say: no projection.
	"graphstore/append.go#queueChangedByRun": "the changed_by_run tuples the write owes its run, and the newest changed_by_run each node holds to that run",
	// armlessAppend, the armless half of an append's statement (graphstore.ArmlessFresh), appends
	// with no conflict arm each tuple its caller states whose node is one of the kind it names that
	// the write's own node insert made, which it reads off the mark that insert left on the node's
	// row (markInserted), and leaves every other tuple to the statement's own insert, which keeps
	// the arm. It reads the store's own rows its write just wrote, no projection, and mends nothing.
	"graphstore/append.go#armlessAppend": "the tuples its caller states and the marks its own node write left on the node rows it made",
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
	// BlockIndexing.Queue writes what the plan decided. It also retracts the pointer tuples at each
	// block the render drops (retractGonePointers), which it reads off the store: the store's own
	// relations, which no projection holds, retracted because the render no longer holds their
	// block, so it mends neither side from the other. Where another release indexed the document
	// after the store last stated its blocks, the rows it compares the render with no longer hold
	// the blocks and links that release dropped, so the plan also reads what the store still
	// asserts of the document (heldDocumentBlocks); what it states of those - gone, retracted - is
	// decided by the render that does not hold them, as the parents' plan reads the parents the
	// store asserts to state the render's. Of each block whose row it writes first and whose node
	// records its authors (T7 of the issue-tree plan), it reads the authors of the version that
	// first holds it, off that version's own row, and whether the node states its authors already,
	// off the store's own authored_by, which it reads only to add none where the node's first write
	// did.
	"refs/blocks.go#Queue": "the version's render, the authors of the versions its blocks first stand in, each decision's stored decided_by, the pointer tuples the store holds at each block the render drops, and where another release indexed the document since the store stated it, what the store still asserts against that render",
	// The parents of a version's blocks, from its render. An authored node's first parent is its
	// author's (D4 of the issue-tree plan): the targets its parent attribute, its serves pointer
	// and its From: line name in the render, read where they stand - a conversation item's issue
	// and R2's test, a block's document - and after that its parent moves only where its parent
	// attribute names another target than the parent value the store holds of it, which is read
	// with its placement and its part_of to state what moved, never to mend a row.
	"refs/block_nodes.go#queueBlockParents": "the version's render, and of each authored node the placement, part_of and parent value the store holds of it",
	"refs/block_nodes.go#queueBlockRefs":    "the version's render, and each decision's stored decided_by",
	// The move of each surviving block_refs row whose pair the version's render hangs on another
	// node now - a block retyped between an ask and a decision keeps its id and its tie, which
	// hangs on the ask's node or on the block's own - to that node, beside its tuple's move
	// (BlockIndexing.rehome). The node the render hangs the pair on decides where the row goes; the
	// node the row names now is read only to tell a row that moved from one that did not, as the
	// reconcile reads the rows it drops, never a side mended.
	"refs/block_nodes.go#queueRehomed": "the version's render, and the node each of the document's surviving block_refs rows names",
	// The rows of the blocks a pass writes (document_blocks, document_block_texts), from its own
	// plan: the block index queues them inside its revision fence (BlockIndexing.Queue), and the
	// typed-node pass of a deferred version queues the rows of the typed blocks it writes and
	// removes (refs.DecisionIndexing.Queue). It reads no row to mend another.
	"refs/blocks.go#queueRows": "the version's render, as the pass's plan holds it",
	// The typed-node pass: the decision and reading nodes of the version, their values and their
	// authors, from the version's render and the credential the version's write proved, and above
	// the deferral bound their part_of and the rows of their own ties, and the shaped ties of the
	// asks a settlement opened (refs.DecisionIndexing.Queue, refs.OpenedAskTies.Queue). It reads a
	// node's stored values and authors only to state what moved.
	"refs/decision_nodes.go#Queue": "the version's render and the credential its write proved",
	"refs/refs.go#queueStamp":      "the event that recorded the write",
	// rebuild-refs' reconcile of one source: a document's text, read under its owner lock, and
	// what the reconcile moved of that document's refs rows, whose tuples it states at the
	// sentinel beside them (refs.Replacement.ArtifactMentions). It reads no side of a projection to
	// mend the other: both follow from the text.
	"refs/rebuild.go#replaceInTx": "the source's text, read under the lock an edit of it takes, and the refs rows its reconcile moved",
	// The head write of the blocks of a comment an item index writes, moves or removes
	// (refs.ItemIndexing.Queue), which reads the store's comment_blocks rows and texts the index
	// queued before it in the same transaction, as every writer's head write reads the store
	// (graphstore.HeadRowsSQL).
	"refs/item_blocks.go#queueBlockHeads": "the item index's own plan: the blocks it writes, moves and removes",
	// The head projection's own writer that writes no store row: the fill of an issue's rows
	// (graphcensus.FillIssueHeads), which reads the store under the issue's owner lock and head fill
	// lock and writes the rows the store gives, as every writer's head write does
	// (graphstore.HeadRowsSQL).
	"graphcensus/census_head.go#fillIssueHeads": "the store's nodes and tuples of the issue's tree, read under its owner lock and its head fill lock",
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

// headWriterCall matches one call of the head write (graphstore.QueueHeads, WriteHeads,
// QueueHeadMark), which writes issue_head_nodes or issue_head_marks, the head projection of the
// store: a pass that calls it is a writer of that projection, which this register names as it
// names the writers of the store.
var headWriterCall = regexp.MustCompile(`\bgraphstore\.(QueueHeads|WriteHeads|QueueHeadMark)\b`)

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
		writes = append(writes, headWriterCall.FindAllStringIndex(text, -1)...)
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
