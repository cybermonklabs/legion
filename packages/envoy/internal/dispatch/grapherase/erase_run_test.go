package grapherase

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphseedtest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// runWords are the words of the question every run seedRun records asks.
const runWords = "What does the spec say?"

// seedRun records one run of the command bar asked by author, with two frames, and gives it its
// interaction node and their frame values as this build's frame path does for a run with no node
// (graphstore.WriteRun). It answers the run's id.
func seedRun(t *testing.T, database *store.Store, author model.Actor) string {
	t.Helper()
	ctx := context.Background()
	recorded, err := json.Marshal(author.Recorded())
	if err != nil {
		t.Fatalf("encode the run's author: %v", err)
	}
	var run string
	if err := database.Pool.QueryRow(ctx, `
		insert into going_requests (author, project, words, ends_at)
		values ($1::jsonb, 'CORE', $2, now() + interval '30 seconds')
		returning id::text
	`, recorded, runWords).Scan(&run); err != nil {
		t.Fatalf("record the run: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into going_events (request_id, sequence, name, data)
		values ($1::uuid, 1, 'text', to_jsonb('It says '::text)), ($1::uuid, 2, 'end', '{}'::jsonb)
	`, run); err != nil {
		t.Fatalf("append the run's frames: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the run's node: %v", err)
	}
	defer tx.Rollback(ctx)
	if written, err := graphstore.WriteRun(ctx, tx, run); err != nil || !written.Held {
		t.Fatalf("write the run's node: held %t, %v", written.Held, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the run's node: %v", err)
	}
	return run
}

// markChangedByRun writes the node kind:id again, as the store holds it, on a context carrying
// run, as a write made for the run writes a node it changed (store.WithRun, Node.Changed): the
// store appends one changed_by_run tuple from the node to the run's interaction.
func markChangedByRun(t *testing.T, database *store.Store, kind, id, run string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write for run %s: %v", run, err)
	}
	defer tx.Rollback(ctx)
	node, held, err := graphstore.ReadNode(ctx, tx, kind, id)
	if err != nil || !held {
		t.Fatalf("read %s:%s: held %t, %v", kind, id, held, err)
	}
	node.Changed, node.Restates = true, true
	if err := graphstore.Write(store.WithRun(ctx, run), tx, []graphstore.Node{node}, nil); err != nil {
		t.Fatalf("write %s:%s for run %s: %v", kind, id, run, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the write for run %s: %v", run, err)
	}
}

// runRows counts what the store and the run's two tables hold of run: its going_requests row, its
// going_events rows, its interaction node, and every tuple out of it or into it.
func runRows(t *testing.T, database *store.Store, run string) (requests, events, nodes, tuples int64) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from going_requests where id = $1::uuid),
		       (select count(*) from going_events where request_id = $1::uuid),
		       (select count(*) from graph_nodes where kind = $2 and id = $3),
		       (select count(*) from graph_tuples where (from_kind = $2 and from_id = $3) or (to_kind = $2 and to_id = $3))
	`, run, graphstore.KindInteraction, run).Scan(&requests, &events, &nodes, &tuples); err != nil {
		t.Fatalf("count what the store holds of run %s: %v", run, err)
	}
	return requests, events, nodes, tuples
}

// TestTheEraseOfARunLeavesNoRowAndNoBackfillWritesItBack is `envoy-dispatch erase --node
// interaction:<uuid>`: a run's two tables hold no node_kind and node_address, so the erase holds
// an entry of its own for the run (runTables), and deletes its going_events rows and its
// going_requests row in its own transaction, beside the node and its tuples, or a backfill would
// write the run back from its rows. A comment changed by a write made for the run carries a
// changed_by_run tuple into it; no row of the comment states that kind
// (graphcensus.DigestedTuple), so it is no blocker and the erase deletes it with the node.
//
// The run's words are its text: a read of its own address answers them whole as the node's Text,
// and a read that shapes that text stores its display tree (texttrees), keyed by the words. The
// erase reads them off the node (readTexts) and deletes that tree, and every row that holds them
// - the node's row, its words value, its going_requests row - is the run's own, so the listing
// leaves none. Afterwards no row, no node, no tuple and no stored tree of the run is left, a
// backfill writes none of it back, and the census reads zero differences.
func TestTheEraseOfARunLeavesNoRowAndNoBackfillWritesItBack(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	run := seedRun(t, database, model.Actor{Kind: "user", ID: "alice"})
	markChangedByRun(t, database, commentNodeKind, seeded.comment, run)
	if changed, err := graphstore.CurrentIn(ctx, database.Pool, graphstore.KindInteraction, run,
		[]string{graphstore.TupleChangedByRun}); err != nil || len(changed) != 1 || changed[0].FromID != seeded.comment {
		t.Fatalf("the run is pointed at by %+v (%v), want the comment's one changed_by_run", changed, err)
	}
	if requests, events, nodes, _ := runRows(t, database, run); requests != 1 || events != 2 || nodes != 1 {
		t.Fatalf("the run holds %d going_requests rows, %d going_events rows and %d nodes before the erase, want 1, 2 and 1",
			requests, events, nodes)
	}
	if err := texttrees.New(database.Pool).Store(ctx, pmdoc.DisplayLatest, runWords); err != nil {
		t.Fatalf("store the display tree of the run's words: %v", err)
	}
	sum := pmdoc.TextSum(runWords)
	trees := func() int64 {
		t.Helper()
		var held int64
		if err := database.Pool.QueryRow(ctx, `select count(*) from text_trees where sum = $1`, sum[:]).Scan(&held); err != nil {
			t.Fatalf("count the stored display trees of the run's words: %v", err)
		}
		return held
	}
	if held := trees(); held != 1 {
		t.Fatalf("text_trees holds %d tree(s) of the run's words before the erase, want the one stored, so this test would prove nothing", held)
	}

	listing := dryRun(t, database, graphstore.KindInteraction, run)
	t.Logf("the listing of the run's erase:\n%s", strings.Join(listing.Listing, "\n"))
	if len(listing.Blockers) != 0 {
		t.Fatalf("the erase of the run is blocked by %v, want no blocker: a changed_by_run is no row's to state", listing.Blockers)
	}
	for _, line := range [][]string{
		{"delete going_requests (id)=(" + run + ")"},
		{"delete going_events (request_id, sequence)=(" + run + ",1)"},
		{"delete going_events (request_id, sequence)=(" + run + ",2)"},
		{"comment:" + seeded.comment + " " + graphstore.TupleChangedByRun + " interaction:" + run},
	} {
		if !listed(listing, line...) {
			t.Errorf("the listing names no line holding %q", line)
		}
	}
	if listed(listing, "leave ") {
		t.Errorf("the listing leaves a row holding the run's words, all of which are the run's own:\n%s", strings.Join(listing.Listing, "\n"))
	}

	erasure, err := confirmedErase(t, database, graphstore.KindInteraction, run, time.Millisecond)
	if err != nil {
		t.Fatalf("erase the run: %v", err)
	}
	if !erasure.Applied {
		t.Fatalf("the confirmed erase of the run removed nothing: blockers %v, mismatch %t", erasure.Blockers, erasure.Mismatch())
	}
	if requests, events, nodes, tuples := runRows(t, database, run); requests+events+nodes+tuples != 0 {
		t.Fatalf("after the erase the run holds %d going_requests rows, %d going_events rows, %d nodes and %d tuples, want none",
			requests, events, nodes, tuples)
	}
	if _, held, err := graphstore.ReadNode(ctx, database.Pool, commentNodeKind, seeded.comment); err != nil || !held {
		t.Fatalf("the comment changed by the run reads held %t (%v) after the run's erase, want it standing", held, err)
	}
	if held := trees(); held != 0 {
		t.Errorf("after the erase text_trees holds %d tree(s) of the run's words, want none", held)
	}
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill after the erase: %v", err)
	}
	if requests, events, nodes, tuples := runRows(t, database, run); requests+events+nodes+tuples != 0 {
		t.Fatalf("the backfill after the erase wrote back %d going_requests rows, %d going_events rows, %d nodes and %d tuples of the run, want none",
			requests, events, nodes, tuples)
	}
	census, err := graphcensus.StoreCensus(ctx, database.Pool.Config().ConnString(), false)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	for _, kind := range census.Kinds {
		for _, difference := range append(append([]graphcensus.Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("the census after the erase reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
}

// TestTheEraseOfARunOrAReasoningNodeHoldsItsOnlyAuthorToTheirNode: the census's person and session
// row sides find the author of a run through its going_requests row, and the author of a lens, a
// risk or a constraint through the node's current authored_by (graphcensus's reasoningAuthored),
// and the erase of the node takes both, while the author's node stands: a session's for good,
// since no erase removes one, and a person's until an erase of that person. So the erase records
// that author as the node's writer (Erasure.Writers), as it records an ask's, a decision's and a
// reading's, and the census reads the author off that record (graphcensus's erasedWriters).
//
// Each node here is its author's only record: no comment, ask or delivery names any of them. A
// token the secrets broker scoped to erin asks a run, which makes the session's node and erin's,
// tied by the session's alias_of; frank invokes a pre-mortem lens on CORE-1, gwen's personal token
// writes a risk under it, and hal states a constraint on CORE-1, each with its author's node and
// authored_by (graphstore.AuthorFacts) as the reasoning write states them. The run, the risk, the
// constraint and the lens are erased in turn: each erasure record names that node's author, the
// census reads zero differences before the erases and after each of them, and at the end nothing
// but those records and the session's alias_of names any of the authors, whose nodes all stand.
func TestTheEraseOfARunOrAReasoningNodeHoldsItsOnlyAuthorToTheirNode(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedAgreeingStore(t, database)
	const (
		session    = "session-proved-erins"
		lens       = "00000000-0000-4000-8000-0000000003b1"
		risk       = "00000000-0000-4000-8000-0000000003b2"
		constraint = "00000000-0000-4000-8000-0000000003b3"
	)
	owner := func(login string) *string { return &login }
	run := seedRun(t, database, model.Actor{Kind: "session", ID: session, Owner: owner("erin"), Act: &model.ActorAct{Session: session}})
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for _, written := range []struct {
		kind, id string
		author   model.Actor
		tuples   []graphstore.Tuple
	}{
		{graphstore.KindLens, lens, model.Actor{Kind: "user", ID: "frank"}, []graphstore.Tuple{
			{Kind: graphstore.TuplePartOf, ToKind: graphstore.KindIssue, ToID: "CORE-1"},
			{Kind: graphstore.TupleHasKind, ToKind: graphstore.KindLensKind, ToID: graphstore.LensKindPreMortem},
		}},
		{graphstore.KindRisk, risk, model.Actor{Kind: "session", Owner: owner("gwen")}, []graphstore.Tuple{
			{Kind: graphstore.TuplePartOf, ToKind: graphstore.KindLens, ToID: lens},
		}},
		{graphstore.KindConstraint, constraint, model.Actor{Kind: "user", ID: "hal"}, []graphstore.Tuple{
			{Kind: graphstore.TuplePartOf, ToKind: graphstore.KindIssue, ToID: "CORE-1"},
		}},
	} {
		node := graphstore.Node{
			Kind: written.kind, ID: written.id, Supertag: written.kind,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive, Project: "CORE",
			MadeAt: at, MadeBy: written.author, SourceEvent: graphstore.NoEvent,
		}
		authors, tuples, err := graphseedtest.AuthoredFacts(node, written.tuples)
		if err != nil {
			t.Fatalf("state the author of %s:%s: %v", written.kind, written.id, err)
		}
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			return graphstore.Write(ctx, tx, append([]graphstore.Node{node}, authors...), tuples)
		}); err != nil {
			t.Fatalf("write %s:%s: %v", written.kind, written.id, err)
		}
	}
	requireCensusZero(t, database, "before the erases")

	for _, erased := range []struct {
		kind, id string
		writer   ErasedWriter
	}{
		{graphstore.KindInteraction, run, ErasedWriter{Kind: graphstore.KindSession, ID: session}},
		{graphstore.KindRisk, risk, ErasedWriter{Kind: graphstore.KindPerson, ID: "gwen"}},
		{graphstore.KindConstraint, constraint, ErasedWriter{Kind: graphstore.KindPerson, ID: "hal"}},
		{graphstore.KindLens, lens, ErasedWriter{Kind: graphstore.KindPerson, ID: "frank"}},
	} {
		requireErased(t, database, erased.kind, erased.id)
		if writers := erasureWriters(t, database, erased.kind, erased.id); !slices.Equal(writers, []ErasedWriter{erased.writer}) {
			t.Errorf("the erasure record of %s:%s names the writers %+v, want its author %+v", erased.kind, erased.id, writers, erased.writer)
		}
		requireCensusZero(t, database, "after the erase of "+erased.kind+":"+erased.id)
	}
	people := []string{"erin", "frank", "gwen", "hal"}
	var naming int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from graph_tuples
		        where ((to_kind = $1 and to_id = any($2)) or (to_kind = $3 and to_id = $4))
		          and not (from_kind = $3 and from_id = $4 and kind = $5))
		     + (select count(*) from going_requests)
	`, graphstore.KindPerson, people, graphstore.KindSession, session, graphstore.TupleAliasOf).Scan(&naming); err != nil {
		t.Fatalf("count what still names the authors: %v", err)
	}
	if naming != 0 {
		t.Fatalf("after the erases %d tuple(s) or run row(s) still name an author beside the session's alias_of, want none, so only the erasure records hold them", naming)
	}
	authors := []ErasedWriter{{Kind: graphstore.KindSession, ID: session}}
	for _, login := range people {
		authors = append(authors, ErasedWriter{Kind: graphstore.KindPerson, ID: login})
	}
	for _, author := range authors {
		if held := readTypeNode(t, database, author.Kind, author.ID); !held.row {
			t.Errorf("after the erases the store holds no node %s:%s, want every author standing", author.Kind, author.ID)
		}
	}
}

// TestTheEraseOfANodeIsRefusedWhileANodeOfAStandingKindStandsUnderIt: a lens, a finding of one, a
// risk and a constraint stand where their current part_of says, with no projection row behind
// them, and every read finds the issue each stands under by walking that tuple up
// (refs.StandingIssueSQL), so an erase of the node one stands under would leave it standing under
// nothing. A finding and a risk stand under a pre-mortem lens, a second lens is invoked on the
// finding, whose part_of names it lens_block, the kind the graph addresses it by, and a third on
// the risk. The erase of the lens names the finding and the risk, the erase of the finding the
// lens on it, and the erase of the risk the lens on it, each with that reason, and removes
// nothing. The finding is a block node whose id names the lens: its listing takes no document
// from its id and lists the tuple into it under its lens_block spelling. Once the lens on the
// finding is moved to stand on the issue, the retraction and the assertion of its part_of into
// the finding stand in the log and block nothing; and once the lens on the risk is erased, the
// finding, the risk and the lens are erased in turn, and no tuple into the finding is left under
// either spelling.
func TestTheEraseOfANodeIsRefusedWhileANodeOfAStandingKindStandsUnderIt(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedAgreeingStore(t, database)
	const (
		lens   = "00000000-0000-4000-8000-0000000003a1"
		risk   = "00000000-0000-4000-8000-0000000003a2"
		onIt   = "00000000-0000-4000-8000-0000000003a3"
		onRisk = "00000000-0000-4000-8000-0000000003a4"
		block  = "first"
	)
	finding := graphstore.KindLens + "/" + lens + "#" + block
	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	node := func(kind, id, supertag string) graphstore.Node {
		return graphstore.Node{
			Kind: kind, ID: id, Supertag: supertag,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive, Project: "CORE",
			MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent,
		}
	}
	partOf := func(fromKind, fromID, toKind, toID string) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: fromKind, FromID: fromID, Kind: graphstore.TuplePartOf, ToKind: toKind, ToID: toID,
			At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true,
		}
	}
	write := func(what string, nodes []graphstore.Node, tuples []graphstore.Tuple) {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin %s: %v", what, err)
		}
		if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("write %s: %v", what, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %s: %v", what, err)
		}
	}
	// The lens is written first, with its kind, since a risk stands only under a pre-mortem lens
	// and the store reads the lens's kind off the store when it holds the risk's part_of to it.
	write("the lens", []graphstore.Node{node(graphstore.KindLens, lens, graphstore.KindLens)}, []graphstore.Tuple{
		partOf(graphstore.KindLens, lens, graphstore.KindIssue, "CORE-1"),
		{
			FromKind: graphstore.KindLens, FromID: lens, Kind: graphstore.TupleHasKind,
			ToKind: graphstore.KindLensKind, ToID: graphstore.LensKindPreMortem,
			At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true,
		},
	})
	write("its finding, its risk and a lens on each", []graphstore.Node{
		node(refs.BlockNodeKind, finding, refs.BlockSupertag),
		node(graphstore.KindLens, onIt, graphstore.KindLens),
		node(graphstore.KindLens, onRisk, graphstore.KindLens),
		node(graphstore.KindRisk, risk, graphstore.KindRisk),
	}, []graphstore.Tuple{
		partOf(refs.BlockNodeKind, finding, graphstore.KindLens, lens),
		partOf(graphstore.KindRisk, risk, graphstore.KindLens, lens),
		partOf(graphstore.KindLens, onIt, graphstore.KindLensBlock, finding),
		partOf(graphstore.KindLens, onRisk, graphstore.KindRisk, risk),
	})

	const standsHere = " states " + graphstore.TuplePartOf + " into this node, which is where it stands"
	for _, refused := range []struct {
		kind, id string
		under    []string
	}{
		{graphstore.KindLens, lens, []string{refs.BlockNodeKind + ":" + finding, graphstore.KindRisk + ":" + risk}},
		{refs.BlockNodeKind, finding, []string{graphstore.KindLens + ":" + onIt}},
		{graphstore.KindRisk, risk, []string{graphstore.KindLens + ":" + onRisk}},
	} {
		erasure, err := confirmedErase(t, database, refused.kind, refused.id, time.Millisecond)
		if err != nil {
			t.Fatalf("erase %s:%s: %v", refused.kind, refused.id, err)
		}
		t.Logf("the erase of %s:%s is blocked by %v", refused.kind, refused.id, erasure.Blockers)
		if erasure.Applied {
			t.Fatalf("the erase of %s:%s removed it while %v stood under it", refused.kind, refused.id, refused.under)
		}
		for _, under := range refused.under {
			named := false
			for _, blocker := range erasure.Blockers {
				named = named || strings.HasPrefix(blocker, under+standsHere)
			}
			if !named {
				t.Errorf("the erase of %s:%s names no blocker for %s standing under it among %v", refused.kind, refused.id, under, erasure.Blockers)
			}
		}
		if _, held, err := graphstore.ReadNode(ctx, database.Pool, refused.kind, refused.id); err != nil || !held {
			t.Fatalf("the refused erase left %s:%s held %t (%v), want it standing", refused.kind, refused.id, held, err)
		}
	}

	findingListing := dryRun(t, database, refs.BlockNodeKind, finding)
	if !listed(findingListing, "lens:"+onIt+" "+graphstore.TuplePartOf+" "+graphstore.KindLensBlock+":"+finding) {
		t.Errorf("the listing of the finding's erase names no tuple into it under its lens_block spelling:\n%s",
			strings.Join(findingListing.Listing, "\n"))
	}
	for _, line := range findingListing.Listing {
		if strings.Contains(line, "document_block_texts") || strings.Contains(line, "live documents whose history is replaced: 1") {
			t.Errorf("the listing of the finding's erase takes a document from its id: %s", line)
		}
	}

	// The lens on the finding moves to stand on the issue: its part_of into the finding is
	// retracted, and that history stays in the log, under the lens_block spelling, until the
	// finding's erase deletes it.
	moved := partOf(graphstore.KindLens, onIt, graphstore.KindLensBlock, finding)
	moved.Asserted = false
	write("the move of the lens on the finding", nil, []graphstore.Tuple{moved, partOf(graphstore.KindLens, onIt, graphstore.KindIssue, "CORE-1")})
	for _, erased := range []struct{ kind, id string }{
		{graphstore.KindLens, onRisk}, {refs.BlockNodeKind, finding}, {graphstore.KindRisk, risk}, {graphstore.KindLens, lens},
	} {
		erasure, err := confirmedErase(t, database, erased.kind, erased.id, time.Millisecond)
		if err != nil {
			t.Fatalf("erase %s:%s: %v", erased.kind, erased.id, err)
		}
		if !erasure.Applied {
			t.Fatalf("the erase of %s:%s removed nothing: blockers %v", erased.kind, erased.id, erasure.Blockers)
		}
		if _, held, err := graphstore.ReadNode(ctx, database.Pool, erased.kind, erased.id); err != nil || held {
			t.Fatalf("after its erase %s:%s reads held %t (%v), want it gone", erased.kind, erased.id, held, err)
		}
	}
	if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindLens, onIt); err != nil || !held {
		t.Errorf("the lens moved off the finding reads held %t (%v) after the finding's erase, want it standing on the issue", held, err)
	}
	var into int64
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples where to_id = $1 and to_kind in ($2, $3)
	`, finding, refs.BlockNodeKind, graphstore.KindLensBlock).Scan(&into); err != nil {
		t.Fatalf("count the tuples into the erased finding: %v", err)
	}
	if into != 0 {
		t.Errorf("the erased finding leaves %d tuple(s) into it, want none under either spelling", into)
	}
}
