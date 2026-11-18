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

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
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
	if err := graphstore.Caused(ctx, tx, write, event); err != nil {
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
	requireTheRebuildWritesRefsAndNoProjectedRow(t, database, document)
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
		document := createOwedDocument(t, database, service, "CORE-1", "", step.slug, "- one\n- two\n", "")
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
	// An edit of a comment's words writes its row from the request's body, and, made for a run,
	// restates the comment's node from that row it locked and caused by its own event, so the
	// run's change of the comment is named (changed_by_run), as an edit of an ask's words names
	// the ask's. Neither reads a row of either side to decide what the other says.
	"api/comment_actions.go#editComment": "the edit request's own body, the comment's row it locked and the comment's own event",
	"api/asks.go#editAsk":                "the edit request's own body, the ask's row it locked and the ask's own event",
	// The comment path's two halves of one store write, split at its event (commentStoreWrite):
	// the node and facts of the comment the request stores, then the same facts under the event
	// that stored it. Neither reads a row of either side.
	"api/comment_create.go#writeNode":      "the comment the create request stores, as its row is written",
	"api/comment_create.go#caused":         "the facts writeNode stated and the event that stored the comment",
	"api/comment_delivery.go#replyComment": "the reply a delivery recorded",
	// A delivery attempt's settle states the session it reached (graphstore.Reached).
	"api/comment_delivery.go#completeCommentDelivery": "the delivery attempt the sender settles",
	"api/message_delivery.go#completeMessageDelivery": "the delivery attempt the sender settles",
	// An answer that flips a stored attempt to sent records the session it reached and, where
	// that flip is what makes the attempt a delivery, the delivered_to tuple, under the answer's
	// own event. It reads the attempt row the answer flipped and no row of either side.
	"api/comment_delivery.go#recordAnsweredDelivery": "the attempt the answer flipped and the answer's own event",
	// A follow of an ask writes the follower row and the following session's node in one
	// statement (graphstore.WriteArms), and each caller that appended an event after the follow
	// moves the node that follow made onto it (graphstore.Caused). Neither reads a row of either
	// side: the follow writes the session its caller names, and the cause the node the follow
	// answered.
	"asks/followers.go#Follow":               "the ask and the session its caller names",
	"api/ask_followers.go#changeAskFollower": "the follow request and the event it appended after the follow",
	"api/asks.go#createAskFor":               "the ask the create request opens and the event it appended after the follow",
	// A renewed approval request follows its ask for the requester and moves the node that
	// follow made onto the last event the renewal appended, or leaves it at the sentinel when
	// the renewal appended none.
	"api/reviews.go#renewApprovalAsk": "the open approval request, the requester and the event the renewal appended",
	"docs/ask_blocks.go#causeFollows": "the follows a settlement made and the ask.opened events it appended after them",
	// The shaped ties of an ask block whose ask a settlement opened, stated beside the ask's row
	// and caused by its ask.opened (refs.IndexAskTies), when that settlement writes no version.
	"docs/ask_blocks.go#causeOpenedAskTies": "the ties writeOpenedAskTies stated of the asks the settlement opened and the ask.opened events it appended",
	// The one writer of an ask's answer state (asks.Settle, dispatch://INFRA-1150): the answer on
	// the ask's node, then its row, from the transition its caller names; and each site that
	// appends what a transition held under the event it appended after it (graphstore.Caused).
	"asks/settle.go#Settle":                            "the transition its caller names",
	"api/ask_transitions.go#closeAskTx":                "the transition the answer, resolve or decline request makes and the event it appended",
	"api/issue_ask_lifecycle.go#recordWithdrawnAsksTx": "the asks an issue's close withdraws and the events it appended",
	"api/issue_ask_lifecycle.go#restoreIssueAsksTx":    "the asks an issue's reopen restores and the events it appended",
	"docs/approval_carry.go#answerApprovalAsk":         "the approval the carry answers and the event it appended",
	"docs/ask_blocks.go#causeSettledAnswers":           "the transitions a settlement made and the events it appended after them",
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
	"graphcensus/backfill.go#writeAskBatch":         "each pre-store asks row's own content",
	"graphcensus/backfill.go#writeAskFollowerBatch": "each pre-store ask_followers row's own content",
	// The node of every ask and message a request or a settlement creates, before its row, in the
	// project or between the parties it belongs to, and caused by the event the create appends.
	"api/reviews.go#openApprovalAsk":             "the approval request the request opens and the ask.opened it appended",
	"docs/ask_blocks.go#createAskBlock":          "the ask block the settlement opens and the document that holds it",
	"api/messages.go#writeMessageNode":           "the message the request stores, its issue, and the root of the thread it answers",
	"api/messages.go#createStoredMessage":        "the message the request stores and the event it appended",
	"api/message_delivery.go#insertSessionReply": "the session's reply the request stores and the event it appended",
	"api/broadcasts.go#writeBroadcast":           "each copy the broadcast stores and the event it appended for it",
	// The nodes of dispatch://INFRA-1950's reasoning kinds - a lens, its findings, a risk and a
	// constraint - written with their tuples and their author in one store write (graphstore.Write)
	// from the request alone: they have no projection row, so there is no side of one to read.
	"api/reasoning_write.go#writeReasoningNodes": "the request's nodes and tuples, its credential and the project of the issue it names",
	// One pointer tuple out of a node of an issue, appended, retracted or retargeted through the
	// store's writer from the request alone: the tuple has no projection row either.
	"api/reasoning_write.go#writeReasoningPointer": "the request's tuple, the node it goes out of and the far ends it names",
	// One node's state, written or settled through the store's writer from the request and the
	// node's own row: the state is a base attribute of the node, which no projection row holds.
	"api/reasoning_write.go#writeNodeState": "the request's state, the node's own row and the issue it stands under",
	// An issue's own field write made for a run: the issue's node, restated from the PATCH and the
	// issue's row it locked, and caused by the issue's own event, so the run's change of the issue
	// is named (changed_by_run). It reads no row of either side to decide what the other says.
	"api/issue_patch.go#patchIssue": "the PATCH's own body, the issue's row it locked and the issue's own event",
	// An issue's relations its create and its PATCH state (dispatch://INFRA-1253 R9, R10): the
	// claims and tuples the request names, read against the issue's own row, the issue_links rows
	// it holds and the tuples its node states only to state what moved, and the node of the item
	// its filed_from names where the store held none, from that item's own row, as the backfill
	// writes it. Neither reads one side of a projection to mend the other.
	"api/issue_relations.go#stateIssueRelations": "the request's relations and the issue's own event",
	"api/issue_relations.go#stateFiledFromNode":  "the row of the item the request's filed_from names",
	// A run of the command bar: its interaction node, its author's node and their tuples, written
	// from the POST alone before the going_requests row that projects them, and each frame's value,
	// written from the frame its writer hands it before the going_events row that projects it.
	// Neither reads a row of either side to decide what the other should say.
	"api/going.go#recordGoingRequest": "the POST's question, its project and the credential it proved",
	"api/going.go#appendGoingEvent":   "the frame its writer hands it and the run's last sequence",
	// The backfill's writer for one run a pod of the release before recorded with no node: its
	// input is that going_requests row's own content and the run's going_events rows, as the other
	// backfill passes read each pre-store row's own, and a run whose node is already written it
	// leaves alone rather than mending.
	"graphcensus/interactions.go#writeRun": "one going_requests row an earlier build recorded with no node, and that run's going_events rows",
	// The backfill's passes over what a process of the release before writes of a run during a
	// roll: the frames it appended with no value, whose input is each going_events row's own
	// content, found by an anti-join with the run's frame values and stated from the row, never
	// read back; and a run whose row it deleted, whose input is that the row is gone and the node
	// as the store holds it, of which it moves the state alone, never mending a row from the store.
	"graphcensus/backfill.go#writeRunFrames": "each going_events row an earlier build appended with no frame value, and the run's node row it takes first",
	"graphcensus/backfill.go#writeGoneRun":   "the interaction node whose going_requests row an earlier build deleted, and that the row is gone",
	// A document's retirement states its artifact node gone, keeping every other fact of the
	// node, and appends that fact under the document.retired event it appends (graphstore.Caused).
	// A removal of an untouched document states its node gone through the same writer, with each
	// changed_by_run the node states now retracted - the store's own relations, which no
	// projection holds, retracted because the document they name has gone - and appends those
	// facts under the artifact.removed event it appends.
	"docs/retire.go#RetireDocument":   "the document the caller retires and the document.retired event it appended",
	"docs/retire.go#writeRetiredNode": "the retired or removed document's row, the node the store holds of it and the facts its caller states beside it",
	// A person's Keep of an agent's decision states its kept_by and kept_text_sum on the decision's
	// node and writes their projection row, decision_keeps, from the request and the decision the
	// person read. It reads the kept_by the node holds only to retract one naming another person,
	// so the node holds the one Keep the row projects: what moved, never a side mended.
	"api/decisions.go#keepDecisionNode": "the Keep request, the decision the person read, and the kept_by its node holds",
	// The decisions step's restore and conversion of an issue (dispatch://INFRA-1150 R6 to R9):
	// each version it writes appends its index pass's facts under the version's event, and the
	// moves, the ask nodes' relations and the Keeps it carries onto a decision's node are drawn
	// from what it read of the issue's documents and asks under their locks, mending nothing.
	"docs/decision_restore.go#writeVersion": "the facts the version's index pass stated and the event that recorded the version",
	"docs/decision_restore.go#writeFacts":   "the restore's and the conversion's plan, read off the documents and the asks rows it locked",
	// The block index's cause: what a version's index pass stated, appended under the event
	// that recorded the version once it is appended (graphstore.Caused), which reads no
	// projection and mends nothing.
	"api/artifacts.go#storeArtifact":                "the facts the upload's index pass stated, the uploaded document and the event the upload appended",
	"api/artifacts.go#commitArtifactVersionEvent":   "the facts the version's index pass stated and the event that recorded the version",
	"api/issue_create.go#createIssue":               "the facts the spec's index pass stated, the issue the request creates and the issue's own event",
	"cmd/dispatch-seed/write_docs.go#writeDocument": "the facts the seeded version's index pass stated and the seed's own plan",
	"docs/block_index.go#indexOwedDocument":         "the facts the tick's index pass stated and the event that recorded the version",
	"docs/settlement.go#commitVersion":              "the facts the settled version's index pass stated and the event that recorded it",
	// An issue room's one pass, by a settlement of the room or its reconciliation: what it stated
	// of each document's blocks, under the event that recorded that document's version.
	"docs/block_index.go#causeRoomFacts": "the facts the room's index pass stated, by document, and the event that recorded each document's version",
	// The facts of the documents and versions a write stores (dispatch://INFRA-1253): a later
	// version's bytes on the node its row names, which reads that row's node column and nothing of
	// the store; the node of each version every version insert writes, from the row its own insert
	// returned; a removal's document and versions stated gone, read off the nodes the store holds
	// of them so each keeps its supertag and its values, the document's through
	// docs.WriteRemovedNode with each changed_by_run its node states retracted, which reads the
	// store to state what it writes into that same node and mends no projection from it; and an
	// issue's tree artifact, its node and its part_of written from the issue its caller names,
	// before the rows naming it.
	"api/artifacts.go#statedVersionBytes":          "the upload's own bytes and the node its document's row names",
	"docs/mutation.go#WriteVersionNode":            "the version row its write inserted and that row's document",
	"api/artifact_removal.go#removedDocumentNodes": "the document the removal takes and the nodes the store holds of it and its versions",
	"api/artifact_removal.go#removeArtifact":       "the facts removedDocumentNodes stated, the changed_by_run its document's node states among them, and the artifact.removed event it appended",
	"api/artifacts.go#createIssueTree":             "the issue its caller names and the slugs its documents hold",
	// The seed's comments: their nodes, their author's person node and each comment's
	// authored_by tuple, all drawn from the plan before any row is written (commentStoreWrite).
	"cmd/dispatch-seed/write_items.go#writeComments": "the seed's own plan",
	// The seed's issues, asks and messages: each one's node, in its project, drawn from the plan
	// before the batch writes its rows.
	"cmd/dispatch-seed/write_core.go#writeIssues":     "the seed's own plan",
	"cmd/dispatch-seed/write_items.go#writeItemNodes": "the seed's own plan",
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
	"graphcensus/backfill.go#writeBlockBatch":    "each pre-store document_blocks row's own content and the document it is in",
	"graphcensus/backfill.go#writeBlockRefBatch": "each pre-store block_refs row's own content",
	"graphcensus/backfill.go#writeCommentBatch":  "each pre-store comments row's own content",
	// The backfill of the roles. A role, its page and the page's part_of it have no projection
	// row, so there is no projection to read one side of: the batch's input is the roles the
	// store census holds to a node, which a row addresses now or an event of the log ever
	// addressed (addressedRoles), and it writes the nodes the store lacks of them, leaving a node
	// another write made alone rather than mending it.
	"graphcensus/backfill.go#writeRoleBatch": "the roles a row addresses now or an event ever addressed (addressedRoles)",
	// The backfill of the sessions, the same case as the roles': a session has no projection
	// row, and the batch's input is the sessions the store census holds to a node, which a row
	// names now or an ask.follower_added event ever named (recordedSessions). It writes the nodes
	// the store lacks of them, and a node another write made it leaves alone.
	"graphcensus/backfill.go#writeSessionBatch": "the sessions a row names now or an ask.follower_added event ever named (recordedSessions)",
	// The backfill of the project of every node: the rows of the issues, documents, asks and
	// messages that no node marks, and the comment and block nodes that name no project, each
	// with the row its project is read from.
	"graphcensus/projects.go#writeObjectBatch": "the issue, document, ask and message rows no node marks",
	"graphcensus/projects.go#writeScopeBatch":  "the comment and block nodes that name no project, and the rows their project is read from",
	// The backfill of the versions: each artifact_versions row whose node the store does not hold,
	// found by an anti-join on graph_nodes since the row names no node, and its document's row,
	// which give the node its address, its project and whether it is a query's version.
	"graphcensus/census_versions.go#writeVersionBatch": "the artifact_versions rows no version node stands for, and their documents' rows",
	// The backfill of the waits a process of the release before wrote: each issue_links row naming
	// no node, its own content, read again under its issue's row, and that issue's own row, whose
	// node the object pass wrote first.
	"graphcensus/census_issues.go#writeIssueLinkBatch": "each pre-store issue_links row's own content and its issue's row",
	// The erase (R5), the one statement of the tree that removes bytes. Its input is the node
	// the operator named and what the store itself holds about it, which is neither side of a
	// projection read to mend the other: it deletes both sides together, the store's through
	// graphstore.EraseNode, rather than bringing one into line with the other, so it is no repair.
	"grapherase/erase.go#apply": "the node the operator named",
	// The same erase's end of the versions it deletes (dispatch://INFRA-1253): the node of each
	// version the listing named, read off the store and stated gone with the supertag and
	// attributes the store holds of it. Its input is that listing and the store's own node, so it
	// never reads one side of a projection to mend the other.
	"grapherase/erase.go#endErasedVersions": "the versions the listing of the node the operator named holds",
	// The same erase's rewrite of the rows it may take no row out of, which is the other half
	// of that one input: the text of the node the operator named, read before any delete
	// (readTexts). It writes back the block text and the excerpt cut from it as one read
	// computed both (readAncestors), so it never reads either to decide what the other should
	// say.
	"grapherase/redaction.go#redactBlocks": "the text of the node the operator named",
	"graphstore/arms.go#nodeArm":           "the nodes its caller states",
	"graphstore/arms.go#claimArms":         "the claims its caller states",
	"graphstore/arms.go#tupleInsert":       "the tuples its caller states",
	// EraseNode is the erase's delete of a node and the tuples its caller listed: it reads the
	// tuples left of the node to refuse a run whose set moved, and deletes the node its one
	// caller names.
	"graphstore/erase.go#EraseNode": "the node its caller names",
	// RedactValues is the erase's rewrite of the quote of an erased block in the text values of the
	// blocks holding it: it reads nothing, and writes the values its one caller computed from the
	// listing of the node the operator named (grapherase's readAncestorValues).
	"graphstore/erase.go#RedactValues": "the redacted values its caller names, by seq",
	// The two seeds other packages' tests share (graphstoretest), which no deployment builds. Each
	// writes rows the store's writers refuse - a node reserved for its author of a kind no read
	// registry flags, the type of a kind only a test writes - from the nodes or the names its test
	// hands it. SeedAuthorOnly reads the store's own row of each node only to know whether to
	// insert it or move its visibility, never a projection, so neither mends one side from the
	// other.
	"graphstore/graphstoretest/graphstoretest.go#SeedAuthorOnly": "the nodes its test hands it, and whether the store holds each",
	"graphstore/graphstoretest/graphstoretest.go#SeedSupertags":  "the type names its test hands it",
	// A node write's two statements (writeNodes): the insert of the nodes the store lacks, which
	// locks the ones it holds, and the move of the ones it holds. The move reads each row it
	// moves, in the snapshot taken once the insert holds them all, to say which base attributes
	// the write moved: it reads the store's own row to state what it writes into that row, never
	// a projection to mend the store or the store to mend a projection.
	"graphstore/write.go#insertNodes": "the nodes its caller states",
	"graphstore/write.go#moveNodes":   "the nodes its caller states and the rows of them it moves",
	// causeNodes is the second half of one write rather than a second look at it: the nodes its
	// caller has just written take the id of the event that recorded them, which is known only
	// once that event is appended (graphstore.Caused). It reads no projection and mends nothing.
	"graphstore/write.go#causeNodes": "the nodes its caller stated and the event that recorded the write",
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
	// The block index reads the version's render, and of each decision the author its node's
	// decided_by tuple holds, which markdown never carries (refs.DecisionFacts): that tuple is
	// the one record of who decided, read to restate the same tie, never to mend a row. It also
	// retracts the pointer tuples at each block the render drops (retractGonePointers), which it
	// reads off the store: the store's own relations, which no projection holds, retracted because
	// the render no longer holds their block, so it mends neither side from the other. Where
	// another release indexed the document after the store last stated its blocks (refs'
	// standsAtVersion), the rows it compares the render with no longer hold the blocks and links
	// that release dropped, so it also reads what the store still asserts of the document
	// (heldDocumentBlocks); what it states of those - gone, retracted - is decided by the render
	// that does not hold them, as reconcileBlockParents reads the parents the store asserts to
	// state the render's. Of each block whose row it writes first and whose node records its
	// authors (dispatch://INFRA-1253's T7), it reads the authors of the version that first holds
	// it, off that version's own row, and whether the node states its authors already, off the
	// store's own authored_by, which it reads only to add none where the node's first write did.
	"refs/blocks.go#IndexBlocks": "the version's render, the authors of the versions its blocks first stand in, each decision's stored decided_by, the pointer tuples the store holds at each block the render drops, and where another release indexed the document since the store stated it, what the store still asserts against that render",
	// The parents of a version's blocks, from its render. An authored node's first parent is its
	// author's (D4 of dispatch://INFRA-1253's plan): the targets its parent attribute, its serves
	// pointer and its From: line name in the render, read where they stand - a conversation item's
	// issue and R2's test, a block's document - and after that its parent moves only where its
	// parent attribute names another target than the parent value the store holds of it, which is
	// read with its placement and its part_of to state what moved, never to mend a row.
	"refs/block_nodes.go#reconcileBlockParents": "the version's render, and of each authored node the placement, part_of and parent value the store holds of it",
	"refs/blocks.go#replaceBlockRefs":           "the version's render, and each decision's stored decided_by",
	// The typed-node pass: the decision and reading nodes of the version, their values and their
	// authors, from the version's render and the credential the version's write proved, and above
	// the deferral bound their part_of and the rows of their own ties, through the two passes
	// above. It reads a node's stored values and authors only to state what moved.
	"refs/decision_nodes.go#IndexDecisionNodes": "the version's render and the credential its write proved",
	"refs/refs.go#Stamp":                        "the event that recorded the write",
}

// projectedWrite matches one statement of a Go source file that writes a projected table or the
// store.
var projectedWrite = regexp.MustCompile(`(?is)\b(insert into|update|delete from)\s+(document_blocks|block_refs|comments|graph_nodes|graph_tuples)\b`)

// storeWriterCall matches one use of the store's writer from outside its package, called or
// passed on as a value: a pass that writes the store through graphstore holds no statement of its
// own, and a scan of SQL alone would never see it. asks.Follow picks graphstore.WriteArms or
// graphstore.WriteArmsUncaused by its cause and then calls what it picked, so a scan for calls
// alone would read it as no writer, where graphstore's TestEveryStoreWriteCallSiteIsListed reads
// the same use as one. The names are every exported function of graphstore that writes the store
// or builds the statement that does, the list that test holds as storeWriteFunctions:
// SessionRecorded writes a session's node through arms of its own, and a pass that wrote the
// store through it alone would otherwise be read as no writer at all, and FillScopes is the
// backfill's fill of the project of a node stored with none.
var storeWriterCall = regexp.MustCompile(`\bgraphstore\.(Write|WriteUncaused|Caused|Authored|Reached|SessionRecorded|WriteArms|WriteArmsUncaused|EraseNode|RedactValues|FillScopes|Retarget|QueueFrameTuple)\b`)

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
