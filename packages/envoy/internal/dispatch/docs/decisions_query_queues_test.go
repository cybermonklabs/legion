package docs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheTickSettlesDecisionsQueryDebtsWithoutWork: a database written before this fix can still
// hold rows for an issue's decisions query node in refs_grammar_owed (artifact and scope), in
// refs_forms_owed (artifact), or an owed document_block_versions row. The decisions query holds no
// fragment of a room and no block (R15), so:
//   - ConvertIssue leaves no row of the decisions query in either queue;
//   - ReconcileRefsForms settles a standing refs_forms_owed row for the decisions query without
//     indexing its stored text or owing it a block index;
//   - ReconcileBlockIndex settles standing artifact and scope rows of the decisions query in
//     refs_grammar_owed without owing a block index or walking a scope;
//   - ReconcileBlockIndex settles an already-owed document_block_versions row of the decisions
//     query by standing its block index with no blocks and no text, never indexing its stored
//     rendering (so a reference written inside that stored rendering produces no refs row).
func TestTheTickSettlesDecisionsQueryDebtsWithoutWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	service := blockIndexProcess(t, database, "")
	issue := createUnconvertedIssue(t, database, service, 1, "",
		":::requirement{#r1}\nThe requirement.\n:::\n\n:::decision{#d1}\nFollow #q1 and dispatch://DOC-2.\n:::\n",
		"The verification.\n", "The plan.\n")
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('DOC-2', 'DOC', 2, 'Target', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create target issue DOC-2: %v", err)
	}
	if result, err := service.ConvertIssue(ctx, issue.key, IssueRoomGate, false); err != nil || result.Outcome != IssueConverted {
		t.Fatalf("convert %s: %+v, %v", issue.key, result, err)
	}
	var decisions string
	if err := database.Pool.QueryRow(ctx,
		`select a.id::text from artifacts a where a.issue_key = $1 and `+refs.DecisionsQuerySQL("a"), issue.key,
	).Scan(&decisions); err != nil {
		t.Fatalf("read the decisions query of %s: %v", issue.key, err)
	}
	grammarKinds := func() []string {
		t.Helper()
		rows, err := database.Pool.Query(ctx,
			`select kind from refs_grammar_owed where id = $1::uuid order by kind`, decisions)
		if err != nil {
			t.Fatalf("read refs_grammar_owed of %s: %v", decisions, err)
		}
		kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("scan refs_grammar_owed of %s: %v", decisions, err)
		}
		return kinds
	}
	if kinds := grammarKinds(); len(kinds) > 0 {
		t.Errorf("ConvertIssue left refs_grammar_owed holding %q for the decisions query %s, want none", kinds, decisions)
	}
	if _, owed := formsOwedRows(t, database)["artifact "+decisions]; owed {
		t.Errorf("ConvertIssue left refs_forms_owed holding the decisions query %s, want none", decisions)
	}

	// Simulate rows already present in a database from before this fix: a decisions query version 2
	// whose stored rendering writes both a bare `#id` and a dispatch:// issue reference, leaving
	// document_block_versions.version null (trigger 0122), both rows in refs_grammar_owed (trigger
	// 20261103205041) and an artifact row in refs_forms_owed (trigger 20261117025904).
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 2, 'Follow #q1 and dispatch://DOC-2.' || E'\n', '[{"kind":"system","id":"query-drain"}]')
	`, decisions); err != nil {
		t.Fatalf("plant a version of the decisions query holding references: %v", err)
	}
	if kinds := grammarKinds(); !slices.Equal(kinds, []string{"artifact", "scope"}) {
		t.Fatalf("planted version left refs_grammar_owed holding %q, want artifact and scope", kinds)
	}
	if _, owed := formsOwedRows(t, database)["artifact "+decisions]; !owed {
		t.Fatalf("planted version left no refs_forms_owed row for %s", decisions)
	}
	if at := blockIndexAt(t, database, decisions); at != nil {
		t.Fatalf("planted version left document_block_versions.version = %d, want null (owed)", *at)
	}

	// 1. Forms drain: settles the decisions query's refs_forms_owed row without indexing its text
	//    or touching its block index.
	formsTick := reconcileRefsForms(t, service)
	if _, owed := formsOwedRows(t, database)["artifact "+decisions]; owed {
		t.Errorf("ReconcileRefsForms left the decisions query %s in refs_forms_owed (tick %#v)", decisions, formsTick)
	}
	if citesIssue(t, database, "artifact", decisions, "DOC-2") {
		t.Errorf("ReconcileRefsForms indexed the stored rendering of the decisions query %s: %#v",
			decisions, refsFrom(t, database, "artifact", decisions))
	}

	// 2. Block-index tick: settles the owed decisions query document handed no text (standing its
	//    block index at version 2 with no references and no blocks) and settles its standing
	//    artifact and scope rows in refs_grammar_owed without re-owing its block index.
	reconcileBlockIndex(t, service, "")
	if at := blockIndexAt(t, database, decisions); at == nil || *at != 2 {
		t.Errorf("after one block-index tick document_block_versions.version of %s is %v, want 2", decisions, at)
	}
	var storeOwed bool
	if err := database.Pool.QueryRow(ctx,
		`select store_owed_at is not null from document_block_versions where artifact_id = $1::uuid`, decisions,
	).Scan(&storeOwed); err != nil {
		t.Fatalf("read store_owed_at of %s: %v", decisions, err)
	}
	if storeOwed {
		t.Errorf("after one block-index tick store_owed_at of the decisions query %s is still set, want cleared", decisions)
	}
	if kinds := grammarKinds(); len(kinds) > 0 {
		t.Errorf("after one block-index tick refs_grammar_owed still holds %q for %s, want none", kinds, decisions)
	}
	if citesIssue(t, database, "artifact", decisions, "DOC-2") {
		t.Errorf("ReconcileBlockIndex indexed the stored rendering of the decisions query %s: %#v",
			decisions, refsFrom(t, database, "artifact", decisions))
	}
	if blocks := blockPairsOf(t, database, decisions); len(blocks) > 0 {
		t.Errorf("ReconcileBlockIndex wrote block_refs %#v for the decisions query %s, want none", blocks, decisions)
	}

	// 3. Standing queue rows on their own (with document_block_versions already standing at version
	//    2) are settled by one tick without re-owing document_block_versions.
	if _, err := database.Pool.Exec(ctx, `
		insert into refs_grammar_owed (kind, id) values ('artifact', $1::uuid), ('scope', $1::uuid)
		on conflict (kind, id) do update set enqueued_at = now()
	`, decisions); err != nil {
		t.Fatalf("plant standing refs_grammar_owed rows for %s: %v", decisions, err)
	}
	reconcileBlockIndex(t, service, "")
	if kinds := grammarKinds(); len(kinds) > 0 {
		t.Errorf("after settling standing queue rows refs_grammar_owed still holds %q for %s, want none", kinds, decisions)
	}
	if at := blockIndexAt(t, database, decisions); at == nil || *at != 2 {
		t.Errorf("settling standing queue rows moved document_block_versions.version of %s to %v, want 2 (never re-owed)", decisions, at)
	}
	if err := database.Pool.QueryRow(ctx,
		`select store_owed_at is not null from document_block_versions where artifact_id = $1::uuid`, decisions,
	).Scan(&storeOwed); err != nil {
		t.Fatalf("read store_owed_at of %s after settling standing queue rows: %v", decisions, err)
	}
	if storeOwed {
		t.Errorf("settling standing queue rows set store_owed_at on the decisions query %s, want null", decisions)
	}
}

// TestADrainedUnchangedVersionRestoresRefsFormsOwed: when the query-mark drain writes a version of
// a room's query document (here Verification, after a recorded result) whose bytes match the
// version before it and whose block index stood at that version, restoreReindexQueue puts
// refs_forms_owed back as it stood before the version's insert:
//   - where no row of refs_forms_owed stood before the result, none stands after its drain, and
//     one ReconcileRefsForms pass counts no document and leaves nothing owed;
//   - where a row of refs_forms_owed stood before the result (a real debt left by an older build's
//     write), the drain keeps that row standing.
func TestADrainedUnchangedVersionRestoresRefsFormsOwed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	writeRoomQueryNodes(t, database, room)
	peer := connectPeer(t, server.URL, room.verification, "alice")
	if err := peer.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatalf("a peer's copy of %s's room: %v", room.issue, err)
	}
	stored := roomUpdateCount(t, database, room.tree)
	const verification = ":::check{#c1}\nThe first check holds.\n:::\n"
	tree, err := pmdoc.Parse(verification)
	if err != nil {
		t.Fatalf("parse the Verification document: %v", err)
	}
	want, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render the Verification document: %v", err)
	}
	fragment := peer.Doc.GetXmlFragment(room.verification)
	var updated error
	if _, err := peer.Send(func(txn *crdt.Transaction) { updated = pmdoc.Update(txn, fragment, tree) }); err != nil || updated != nil {
		t.Fatalf("write the Verification document of %s: %v, %v", room.issue, err, updated)
	}
	if err := peer.WaitForText(fragmentMarkdown(room.verification), want, roomWait(t)); err != nil {
		t.Fatalf("a peer's copy of %s's verification: %v", room.issue, err)
	}
	waitForStoredUpdates(t, database, room.tree, stored)
	if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
		t.Fatalf("wait for %s's appends: %v", room.issue, err)
	}
	if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle %s's room: %v", room.issue, err)
	}
	before := newestVersionNumber(t, database, room.verification)
	requireStatedRow(t, database, room.issue, graphstore.QueryVerification, room.verification, before)
	// Drain any forms rows left by the initial SQL seed of createIssueRoom so the queue starts empty.
	reconcileRefsForms(t, service)
	if owed := formsOwedRows(t, database); len(owed) > 0 {
		t.Fatalf("after initial forms drain refs_forms_owed still holds %v, want empty", owed)
	}

	// 1. A recorded result and its drain write an unchanged version of Verification and leave no
	//    row in refs_forms_owed; a forms drain pass then counts no document.
	recordResult(t, database, service, room)
	after := newestVersionNumber(t, database, room.verification)
	if after != before+1 {
		t.Fatalf("the result's drain left Verification at version %d, want %d", after, before+1)
	}
	if owed := formsOwedRows(t, database); len(owed) > 0 {
		t.Errorf("the result's drain left refs_forms_owed holding %v for %s, want none", owed, room.issue)
	}
	formsTick := reconcileRefsForms(t, service)
	if formsTick.Documents != 0 || formsTick.OwedTexts != 0 {
		t.Errorf("ReconcileRefsForms after the result's drain returned %#v, want Documents=0 and OwedTexts=0", formsTick)
	}

	// 2. A refs_forms_owed row that already stood before the result (as an older build's write
	//    leaves it) still stands after the result's drain.
	if _, err := database.Pool.Exec(ctx, `
		insert into refs_forms_owed (kind, id) values ('artifact', $1::uuid)
		on conflict (kind, id) do update set enqueued_at = now()
	`, room.verification); err != nil {
		t.Fatalf("plant a standing refs_forms_owed row for %s: %v", room.verification, err)
	}
	recordResult(t, database, service, room)
	if next := newestVersionNumber(t, database, room.verification); next != after+1 {
		t.Fatalf("the second result's drain left Verification at version %d, want %d", next, after+1)
	}
	if _, stood := formsOwedRows(t, database)["artifact "+room.verification]; !stood {
		t.Errorf("the drain over a standing refs_forms_owed row deleted %s's row, want it kept", room.verification)
	}
}
