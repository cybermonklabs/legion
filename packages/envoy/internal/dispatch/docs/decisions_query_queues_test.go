package docs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheTickSettlesDecisionsQueryDebtsWithoutWork: a database the release before wrote can still
// hold rows for an issue's Decisions query node in the grammar and forms queues, or an owed
// document_block_versions row. The Decisions query holds no fragment of a room and no block (R15):
//   - ConvertIssue leaves no queue rows for the Decisions query;
//   - each forms pass settles its listed row without indexing stored text or owing a block index;
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
	row := "artifact " + decisions
	for queue, owed := range map[string]map[string]time.Time{
		"refs_forms_owed":                formsOwedRows(t, database),
		"refs_decisions_forms_owed":      decisionsFormsOwedRows(t, database),
		"refs_decisions_forms_seed_owed": decisionsFormsSeedOwedRows(t, database),
	} {
		if _, held := owed[row]; held {
			t.Errorf("ConvertIssue left %s holding %s, want none", queue, row)
		}
	}

	// Simulate rows already present in a database the release before wrote: a decisions query
	// version 2 whose stored rendering writes both a bare `#id` and a dispatch:// issue reference,
	// leaving document_block_versions.version null (trigger 0122), both rows in refs_grammar_owed
	// (trigger 20261103205041) and an artifact row in refs_forms_owed (trigger 20261117025904).
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
	if _, err := database.Pool.Exec(ctx, `insert into refs_decisions_forms_seed_owed (kind, id) values ('artifact', $1::uuid)`, decisions); err != nil {
		t.Fatalf("plant a bootstrap row for %s: %v", decisions, err)
	}

	// 1. Forms drain: settles the decisions query's refs_forms_owed row without indexing its text
	//    or touching its block index.
	formsTick := reconcileRefsForms(t, service)
	if _, owed := formsOwedRows(t, database)["artifact "+decisions]; owed {
		t.Errorf("ReconcileRefsForms left the decisions query %s in refs_forms_owed (tick %#v)", decisions, formsTick)
	}
	if decisionsFormsOwedRows(t, database)[row].IsZero() || decisionsFormsSeedOwedRows(t, database)[row].IsZero() {
		t.Fatal("the forms pass should relay the existing Decisions query to the live queue and leave its seed row")
	}
	reconcileDecisionsForms(t, service)
	if !decisionsFormsOwedRows(t, database)[row].IsZero() || decisionsFormsSeedOwedRows(t, database)[row].IsZero() {
		t.Fatal("the live Decisions pass should settle its row alone without indexing")
	}
	if tick, err := service.ReconcileRefsForms(ctx, refs.RefsDecisionsFormsSeedOwed, refs.Owed{}, newFormsLiveAhead()); err != nil || tick.OwedTexts != 0 {
		t.Fatalf("the seed pass left debt for the Decisions query: %+v, %v", tick, err)
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

// TestADrainedUnchangedVersionRestoresRefsFormsOwed: the query-mark drain writes a version
// whose bytes match the preceding version when a check's placement changes without moving its
// text. If the block index stood at the preceding version, restoreReindexQueue restores
// refs_forms_owed to its prior state and preserves independent Decisions debt.
func TestADrainedUnchangedVersionRestoresRefsFormsOwed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	writeRoomQueryNodes(t, database, room)
	settleVerification(t, ctx, database, service, server.URL, room)
	before := savedVersion(t, database, room.verification)
	requireStatedRow(t, database, room.issue, graphstore.QueryVerification, room.verification, before)
	// Drain any forms rows left by the initial SQL seed of createIssueRoom so the queue starts empty.
	reconcileRefsForms(t, service)
	if owed := formsOwedRows(t, database); len(owed) > 0 {
		t.Fatalf("after initial forms drain refs_forms_owed still holds %v, want empty", owed)
	}
	if owed := decisionsFormsOwedRows(t, database); len(owed) > 0 {
		t.Fatalf("after initial forms drain refs_decisions_forms_owed holds %v, want empty", owed)
	}
	if owed := decisionsFormsSeedOwedRows(t, database); len(owed) > 0 {
		t.Fatalf("after initial forms drain refs_decisions_forms_seed_owed holds %v, want empty", owed)
	}

	// 1. A check's placement changes the record without changing the document's bytes. The drain
	//    writes the version but leaves no forms debt.
	restateCheck := func(placement string) {
		leaveCheckPlacementOwed(t, database, room, placement)
		service.DrainOwedQueries(ctx, room.issue)
	}
	restateCheck(refs.PlacementStored)
	after := savedVersion(t, database, room.verification)
	if after != before+1 {
		t.Fatalf("the check placement's drain left Verification at version %d, want %d", after, before+1)
	}
	if owed := formsOwedRows(t, database); len(owed) > 0 {
		t.Errorf("the result's drain left refs_forms_owed holding %v for %s, want none", owed, room.issue)
	}
	if owed := decisionsFormsOwedRows(t, database); len(owed) > 0 {
		t.Errorf("the result's drain left refs_decisions_forms_owed holding %v, want none", owed)
	}
	if owed := decisionsFormsSeedOwedRows(t, database); len(owed) > 0 {
		t.Errorf("the result's drain left refs_decisions_forms_seed_owed holding %v, want none", owed)
	}
	formsTick := reconcileRefsForms(t, service)
	if formsTick.Documents != 0 || formsTick.OwedTexts != 0 {
		t.Errorf("ReconcileRefsForms after the result's drain returned %#v, want Documents=0 and OwedTexts=0", formsTick)
	}

	// 2. A pre-existing forms debt (as an older build's write leaves it) still stands after the drain.
	if _, err := database.Pool.Exec(ctx, `
		insert into refs_forms_owed (kind, id) values ('artifact', $1::uuid)
		on conflict (kind, id) do update set enqueued_at = now()
	`, room.verification); err != nil {
		t.Fatalf("plant a standing refs_forms_owed row for %s: %v", room.verification, err)
	}
	restateCheck(refs.PlacementAuthored)
	if next := savedVersion(t, database, room.verification); next != after+1 {
		t.Fatalf("the second result's drain left Verification at version %d, want %d", next, after+1)
	}
	if _, stood := formsOwedRows(t, database)["artifact "+room.verification]; !stood {
		t.Errorf("the drain over a standing refs_forms_owed row deleted %s's row, want it kept", room.verification)
	}

	// 3. With no older forms row, the next version's relay must leave independent Decisions debt.
	if _, err := database.Pool.Exec(ctx, `delete from refs_forms_owed where kind = 'artifact' and id = $1::uuid`, room.verification); err != nil {
		t.Fatalf("relay the standing forms row of %s: %v", room.verification, err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into refs_decisions_forms_seed_owed (kind, id) values ('artifact', $1::uuid)`, room.verification); err != nil {
		t.Fatalf("plant a bootstrap debt for %s: %v", room.verification, err)
	}
	row := "artifact " + room.verification
	live, seed := decisionsFormsOwedRows(t, database)[row], decisionsFormsSeedOwedRows(t, database)[row]
	if live.IsZero() || seed.IsZero() {
		t.Fatalf("the standing Decisions debt was not planted: live %v, seed %v", live, seed)
	}
	restateCheck(refs.PlacementStored)
	if next := savedVersion(t, database, room.verification); next != after+2 {
		t.Fatalf("the third result's drain left Verification at version %d, want %d", next, after+2)
	}
	if got := decisionsFormsOwedRows(t, database)[row]; !got.Equal(live) {
		t.Errorf("the drain changed standing live Decisions debt from %v to %v", live, got)
	}
	if got := decisionsFormsSeedOwedRows(t, database)[row]; !got.Equal(seed) {
		t.Errorf("the drain changed standing seed Decisions debt from %v to %v", seed, got)
	}
}

// leaveCheckPlacementOwed changes c1's placement value, a fact in its approval record but not in
// its markdown. The tuple and the mark commit together, as on the node route. The drain must write
// a new version with unchanged markdown; no result node can trigger that version anymore.
func leaveCheckPlacementOwed(t *testing.T, database *store.Store, room issueRoom, placement string) {
	t.Helper()
	ctx := context.Background()
	address := refKeyOf(t, database, room.tree) + "#c1"
	err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
			FromKind: refs.BlockNodeKind, FromID: address, Kind: refs.AttributePlacement, ToKind: graphstore.KindValue, ToID: placement,
			At: time.Now().UTC(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: true,
		}}); err != nil {
			return err
		}
		var marked bool
		if err := store.RunWrite(ctx, tx, func(batch *store.WriteBatch) error {
			QueueQueryMark(batch, room.issue, graphstore.NoEvent, &marked)
			return nil
		}); err != nil {
			return err
		}
		if !marked {
			t.Errorf("%s took no query mark after changing %s's placement", room.issue, address)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("change %s's placement to %s: %v", address, placement, err)
	}
}
