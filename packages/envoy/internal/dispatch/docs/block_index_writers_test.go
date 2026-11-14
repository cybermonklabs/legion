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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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
// storeArtifact): the text seeded in the writer's transaction, the version row, the version's
// references and blocks indexed from the rendering that transaction wrote, the version's event,
// and then what the block index stated, appended under that event.
func writeDocumentThroughTheRequestPath(t *testing.T, service *Service, database *store.Store, slug, markdown string) string {
	t.Helper()
	ctx := context.Background()
	var artifactID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', $1, $1 || '.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`, slug).Scan(&artifactID); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the document write: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	stored, err := service.SeedText(joined, artifactID, markdown, model.Actor{Kind: "user", ID: "alice"})
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
	var event int64
	if err := tx.QueryRow(ctx, `
		with bumped as (update issues set last_seq = last_seq + 1 where key = 'CORE-1' returning last_seq)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'CORE-1', bumped.last_seq, 'artifact.created', '{"kind":"user","id":"alice"}', $1, false
		from bumped
		returning id
	`, payload).Scan(&event); err != nil {
		t.Fatalf("record the document's event: %v", err)
	}
	if err := write.Caused(ctx, tx, event); err != nil {
		t.Fatalf("append what the write's block index stated: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the document write: %v", err)
	}
	return artifactID
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
// row and then its owed row before it writes either, in that order. The item block pass writes
// no row of the store. The rebuild writes refs and marks work owed and writes no row of
// document_blocks, block_refs or comments.
func TestTheIndexPassesAreStoreWritersAndNoRepairExists(t *testing.T) {
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
	requireTheItemBlockPassWritesNoStoreRow(t, database, service)
	requireTheRebuildWritesNoProjectedRow(t, database)
}

// requireTheTickLocksTheOwnerThenTheOwedRow holds each of the two rows the tick takes and reads
// where the tick stops: the owner row first, before it has written anything, and the owed row
// next, which it only reaches once the owner row is free. Both are taken before any write, so a
// tick that could not take them leaves the document as it found it.
//
// Where the tick stops says which row it waits for, not which it already holds, so each step
// also tries the other row without waiting while the tick waits. Waiting at the owner row, the
// tick must hold nothing yet, so the owed row is free to take; waiting at the owed row, it must
// hold the owner row already, so the try is refused. A tick that took the two in the other order
// passes neither try.
func requireTheTickLocksTheOwnerThenTheOwedRow(t *testing.T, database *store.Store, service *Service) {
	t.Helper()
	ctx := context.Background()
	for _, step := range []struct {
		name, slug, hold, wantStatement string
		// other is the row the step does not hold, tried `nowait` while the tick waits, and
		// otherHeld whether the tick must hold it by then.
		otherName, other string
		otherHeld        bool
	}{
		{
			name:          "the document's owner row",
			slug:          "owner-locked",
			hold:          `select 1 from issues i join artifacts a on a.issue_key = i.key where a.id = $1::uuid for no key update of i`,
			wantStatement: "from issues",
			otherName:     "the document's owed row",
			other:         `select 1 from document_block_versions where artifact_id = $1 for no key update nowait`,
			otherHeld:     false,
		},
		{
			name:          "the document's owed row",
			slug:          "owed-locked",
			hold:          `select 1 from document_block_versions where artifact_id = $1 for no key update`,
			wantStatement: "document_block_versions",
			otherName:     "the document's owner row",
			other:         `select 1 from issues i join artifacts a on a.issue_key = i.key where a.id = $1::uuid for no key update of i nowait`,
			otherHeld:     true,
		},
	} {
		// A document of its own each step, since the tick indexes the one before it as soon as
		// that step lets it through, and an indexed document is owed nothing to wait for.
		document := createOwedDocument(t, database, service, "", step.slug, "- one\n- two\n", "")
		settle(t, database, document)
		holder, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the holder of %s: %v", step.name, err)
		}
		if _, err := holder.Exec(ctx, step.hold, document); err != nil {
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
		if waiters := storetest.WaitForLockWaiters(t, holder, 1, fenceWait); waiters < 1 {
			holder.Rollback(ctx)
			t.Fatalf("the tick never queued behind %s", step.name)
		}
		if running := waitingStatement(t, holder); !strings.Contains(running, step.wantStatement) {
			t.Errorf("the tick waiting on %s is running %q, want the statement that takes it", step.name, running)
		}
		if rows := countRows(t, database, "document_blocks", document); rows != 0 {
			t.Errorf("the tick wrote %d block rows before it had taken %s, want none", rows, step.name)
		}
		requireTheOtherRow(t, database, document, step.name, step.otherName, step.other, step.otherHeld)
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

// requireTheOtherRow tries, on a transaction of its own and without waiting, the row the step
// does not hold while the tick waits at the one it does. held says whether the tick must hold
// that row by then: the try is refused (55P03) exactly when it does. The try is rolled back at
// once, so a row it took is never one the tick then waits for.
func requireTheOtherRow(t *testing.T, database *store.Store, document, waitingAt, otherName, other string, held bool) {
	t.Helper()
	ctx := context.Background()
	probe, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the try of %s: %v", otherName, err)
	}
	_, tried := probe.Exec(ctx, other, document)
	if err := probe.Rollback(ctx); err != nil {
		t.Fatalf("end the try of %s: %v", otherName, err)
	}
	var refused *pgconn.PgError
	switch taken := !errors.As(tried, &refused) || refused.Code != "55P03"; {
	case tried != nil && taken:
		t.Fatalf("try %s while the tick waits at %s: %v", otherName, waitingAt, tried)
	case held && taken:
		t.Errorf("while the tick waited at %s, %s was free to take: the tick takes the owner row, then the owed row, so it holds %s by then",
			waitingAt, otherName, otherName)
	case !held && !taken:
		t.Errorf("while the tick waited at %s, %s was already held (%v): the tick takes the owner row, then the owed row, so it holds nothing while it waits for the first",
			waitingAt, otherName, tried)
	}
}

// requireTheItemBlockPassWritesNoStoreRow runs the item block pass (refs.IndexOwedItemBlocks,
// the tick's third batch) over a comment owed the index of its own blocks, and holds the pass to
// writing no row of graph_nodes and no row of graph_tuples: this child moves no body block of a
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

// requireTheRebuildWritesNoProjectedRow holds refs.RebuildAll to what it is: a writer of refs
// that marks the work the block index owes. A row of document_blocks, block_refs or comments
// written by it would make it a repair of the projection, which is what this fails on.
func requireTheRebuildWritesNoProjectedRow(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
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
	// The comment path's two halves of one store write, split at its event (commentStoreWrite):
	// the node and facts of the comment the request stores, then the same facts under the event
	// that stored it. Neither reads a row of either side.
	"api/comment_create.go#writeNode":      "the comment the create request stores, as its row is written",
	"api/comment_create.go#caused":         "the facts writeNode stated and the event that stored the comment",
	"api/comment_delivery.go#replyComment": "the reply a delivery recorded",
	// A delivery attempt's settle states the session it reached (graphstore.Reached).
	"api/comment_delivery.go#completeCommentDelivery": "the delivery attempt the sender settles",
	"api/message_delivery.go#completeMessageDelivery": "the delivery attempt the sender settles",
	// The seed's comments: their nodes, their author's person node and each comment's
	// authored_by tuple, all drawn from the plan before any row is written (commentStoreWrite).
	"cmd/dispatch-seed/write_items.go#writeComments": "the seed's own plan",
	"docs/block_index.go#stampVersionEvent":          "the event that recorded the version",
	// The backfill of the rows written before the store existed. Each pass reads the rows of
	// its own kind and nothing else: a row's own content - its time, its author, the
	// document it is in, its own source_seq - is what it writes into the store, and the
	// store holds nothing about those rows for it to read. That is what makes it a writer
	// and no repair: it never reads one side of a projection to decide what to put in the
	// other, and a row whose node is already written it leaves alone rather than mending.
	"graphcensus/backfill.go#writeBlockBatch":    "each pre-store document_blocks row's own content",
	"graphcensus/backfill.go#writeBlockRefBatch": "each pre-store block_refs row's own content",
	"graphcensus/backfill.go#writeCommentBatch":  "each pre-store comments row's own content",
	// The backfill of the roles. A role, its page and the page's part_of it have no projection
	// row, so there is no projection to read one side of: the batch's input is the roles the
	// store census holds to a node, which a row addresses now or an event of the log ever
	// addressed (addressedRoles), and it writes the nodes the store lacks of them, leaving a node
	// another write made alone rather than mending it.
	"graphcensus/backfill.go#writeRoleBatch": "the roles a row addresses now or an event ever addressed (addressedRoles)",
	// The erase (R5), the one statement of the tree that removes bytes. Its input is the node
	// the operator named and what the store itself holds about it, which is neither side of a
	// projection read to mend the other: it deletes both sides together rather than bringing
	// one into line with the other, so it is no repair.
	"graphcensus/erase.go#apply": "the node the operator named",
	// The same erase's rewrite of the rows it may take no row out of, which is the other half
	// of that one input: the text of the node the operator named, read before any delete
	// (erasedTexts). It rewrites the block text and the excerpt cut from it together, in one
	// statement, so it never reads either to decide what the other should say.
	"graphcensus/erase.go#redactBlocks": "the text of the node the operator named",
	"graphstore/arms.go#nodeArm":        "the nodes its caller states",
	"graphstore/arms.go#claimArms":      "the claims its caller states",
	"graphstore/arms.go#tupleInsert":    "the tuples its caller states",
	"graphstore/write.go#writeNodes":    "the nodes its caller states",
	// Caused is the second half of one write rather than a second look at it: the nodes its
	// caller has just written take the id of the event that recorded them, which is known only
	// once that event is appended. It reads no projection and mends nothing.
	"graphstore/write.go#Caused": "the nodes its caller stated and the event that recorded the write",
	// The publisher's record of a batch splices the store's arms into its own statement
	// (graphstore.WriteArms): the roles and sessions the batch reached, and its deliveries.
	"outbox/publisher.go#recordBatch":      "the batch of events the publisher sent and the destinations each reached",
	"refs/blocks.go#Caused":                "the facts the version's index pass stated and the event that recorded the version",
	"refs/blocks.go#IndexBlocks":           "the version's render",
	"refs/blocks.go#reconcileBlockParents": "the version's render",
	"refs/blocks.go#replaceBlockRefs":      "the version's render",
	"refs/refs.go#Stamp":                   "the event that recorded the write",
}

// projectedWrite matches one statement of a Go source file that writes a projected table or the
// store.
var projectedWrite = regexp.MustCompile(`(?is)\b(insert into|update|delete from)\s+(document_blocks|block_refs|comments|graph_nodes|graph_tuples)\b`)

// storeWriterCall matches one call of the store's writer from outside its package: a pass that
// writes the store through graphstore holds no statement of its own, and a scan of SQL alone
// would never see it.
var storeWriterCall = regexp.MustCompile(`\bgraphstore\.(Write|WriteStating|Caused|Authored|Reached|WriteArms)\(`)

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
