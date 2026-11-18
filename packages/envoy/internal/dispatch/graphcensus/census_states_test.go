package graphcensus

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// statesDocumentID is the spec TestCensusCountsEveryNodeThatIsNotGone indexes.
const statesDocumentID = "00000000-0000-4000-8000-0000000000e1"

// statesRequirements are the requirement blocks of that spec, one at each value of the node
// state, in document order. The last is the block the second version drops.
var statesRequirements = []struct{ id, state string }{
	{"r-live", graphstore.StateLive},
	{"r-proposed", graphstore.StateProposed},
	{"r-settled", graphstore.StateSettled},
	{"r-overtaken", graphstore.StateOvertaken},
	{"r-gone", graphstore.StateGone},
}

// The census counts every node except those at node state gone (R12 of dispatch://INFRA-1950):
// a node at proposed, settled or overtaken is a node a projection row projects, as one at live
// is, and a move between those values loses no node.
//
// The spec holds five requirement blocks, indexed through the block index (refs.IndexBlocks), and
// a second version drops the fifth, which leaves its node gone and its document_blocks row
// deleted, as the index pass leaves a dropped block. Three of the four it keeps are then moved to
// proposed, settled and overtaken through the store's writer, so the store holds one block at each
// value of the state. graphcensus.StoreCensus, which `envoy-dispatch census --store` runs, reads
// zero differences in both directions; its block line checked as many rows as document_blocks
// holds, four, and compared as many present nodes; no difference names any of the four blocks; and
// the gone block holds no projection row, which is the assertion dispatch://INFRA-1149's census
// keeps. The proposed requirement then moves to settled and the settled one to overtaken, and the
// census reads the same again.
func TestCensusCountsEveryNodeThatIsNotGone(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	var every, kept []string
	for index, requirement := range statesRequirements {
		every = append(every, requirement.id)
		if index < len(statesRequirements)-1 {
			kept = append(kept, requirement.id)
		}
	}
	whole := statesMarkdown(every)
	document := seedIssueDocument(t, database, "CORE", statesDocumentID, whole)
	indexStatesVersion(t, database, document, 1, whole, every)
	dropping := statesMarkdown(kept)
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 2, $2, '[{"kind":"user","id":"alice"}]')
	`, document.artifactID, dropping); err != nil {
		t.Fatalf("write version 2 of %s, which drops r-gone: %v", document.refKey, err)
	}
	indexStatesVersion(t, database, document, 2, dropping, kept)

	states := map[string]string{}
	var moves []stateMove
	for _, requirement := range statesRequirements {
		states[requirement.id] = requirement.state
		if requirement.state != graphstore.StateLive && requirement.state != graphstore.StateGone {
			moves = append(moves, stateMove{requirement.id, requirement.state})
		}
	}
	moveStates(t, database, document, moves...)
	requireStatesHeld(t, database, document, states)
	// The spec's row and its two versions are written by SQL, as a build before their nodes wrote
	// them (dispatch://INFRA-1253): the backfill gives each its node, and reads no block row, every
	// one of which the index named, so the census below reads the blocks as the index left them.
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the spec and its versions: %v", err)
	}
	requireStatesHeld(t, database, document, states)
	requireTheCensusCountsEveryBlockNotGone(t, database, document, "the census over one block at each state", kept)

	// The proposed requirement moves to settled, and the settled one to overtaken.
	moveStates(t, database, document,
		stateMove{"r-proposed", graphstore.StateSettled}, stateMove{"r-settled", graphstore.StateOvertaken})
	states["r-proposed"], states["r-settled"] = graphstore.StateSettled, graphstore.StateOvertaken
	requireStatesHeld(t, database, document, states)
	requireTheCensusCountsEveryBlockNotGone(t, database, document,
		"the census after proposed moved to settled and settled to overtaken", kept)
}

// statesMarkdown is the spec holding the requirement blocks ids, in that order.
func statesMarkdown(ids []string) string {
	blocks := make([]string, len(ids))
	for index, id := range ids {
		blocks[index] = statesRequirement(id)
	}
	return strings.Join(blocks, "\n")
}

// statesRequirement is the markdown of requirement block id.
func statesRequirement(id string) string {
	return ":::requirement{#" + id + "}\nThe requirement " + id + ".\n:::\n"
}

// indexStatesVersion indexes version version of document, holding markdown, through the block
// index as a version write does: refs.IndexBlocks over the requirement blocks ids, and the facts
// it stated appended in the same transaction (graphstore.Caused), here with no event.
func indexStatesVersion(t *testing.T, database *store.Store, document preStoreDocument, version int, markdown string, ids []string) {
	t.Helper()
	ctx := context.Background()
	blocks := make([]model.ArtifactBlock, len(ids))
	for index, id := range ids {
		from := strings.Index(markdown, statesRequirement(id))
		if from < 0 {
			t.Fatalf("version %d of %s holds no block %s", version, document.refKey, id)
		}
		blocks[index] = model.ArtifactBlock{ID: id, Type: "requirement", From: from, To: from + len(statesRequirement(id))}
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the index of version %d: %v", version, err)
	}
	defer tx.Rollback(ctx)
	_, won, written, err := refs.IndexBlocks(ctx, tx, document.artifactID, refs.OneVersion(document.artifactID, version, time.Now().UTC()), markdown, blocks, nil, nil, refs.TypedFacts{})
	if err != nil {
		t.Fatalf("index version %d of %s: %v", version, document.refKey, err)
	}
	if !won {
		t.Fatalf("the index of version %d of %s lost the fence, so it wrote nothing", version, document.refKey)
	}
	if err := graphstore.Caused(ctx, tx, written, graphstore.NoEvent); err != nil {
		t.Fatalf("append what version %d of %s stated: %v", version, document.refKey, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the index of version %d of %s: %v", version, document.refKey, err)
	}
}

// stateMove is one block of the spec and the state it is moved to.
type stateMove struct{ id, state string }

// moveStates writes each block's node at its new state through the store's writer, in one
// transaction, as a route that moves a node's state does: the row moves and a state tuple is
// appended beside it.
func moveStates(t *testing.T, database *store.Store, document preStoreDocument, moves ...stateMove) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC()
	nodes := make([]graphstore.Node, len(moves))
	for index, move := range moves {
		nodes[index] = graphstore.Node{
			Kind: refs.BlockNodeKind, ID: document.refKey + "#" + move.id, Supertag: refs.BlockSupertag,
			Visibility: graphstore.VisibilityProject, State: move.state, Project: "CORE",
			MadeAt: at, MadeBy: censusWriter, SourceEvent: graphstore.NoEvent,
		}
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the state moves: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nodes, nil); err != nil {
		t.Fatalf("move %+v: %v", moves, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the state moves: %v", err)
	}
}

// requireStatesHeld fails unless each block of the spec stands at the state want names for it,
// on its node row and in its newest state tuple, so the census below reads a store holding one
// block at each value it was meant to.
func requireStatesHeld(t *testing.T, database *store.Store, document preStoreDocument, want map[string]string) {
	t.Helper()
	ctx := context.Background()
	for id, state := range want {
		address := document.refKey + "#" + id
		node, held, err := graphstore.ReadNode(ctx, database.Pool, refs.BlockNodeKind, address)
		if err != nil {
			t.Fatalf("read the node of %s: %v", address, err)
		}
		stated, asserted, err := graphstore.CurrentValue(ctx, database.Pool, refs.BlockNodeKind, address, graphstore.AttributeState)
		if err != nil {
			t.Fatalf("read the state of %s: %v", address, err)
		}
		if !held || node.State != state || !asserted || stated != state {
			t.Fatalf("block %s holds a node %t at state %q, its state tuple %q (asserted %t), want %q on both",
				address, held, node.State, stated, asserted, state)
		}
		t.Logf("block %s is at state %s", address, state)
	}
}

// requireTheCensusCountsEveryBlockNotGone takes the store census and fails unless it read zero
// differences in both directions over every kind, each difference named with where it is; its
// block line checked exactly the rows document_blocks holds, which the test counts with its own
// select and which are as many as present or more, and as many present nodes; and the spec's gone
// block holds no projection row.
func requireTheCensusCountsEveryBlockNotGone(t *testing.T, database *store.Store, document preStoreDocument, what string, present []string) {
	t.Helper()
	ctx := context.Background()
	report := storeCensusOf(t, database)
	for _, kind := range report.Kinds {
		t.Logf("%s: kind=%s rows=%d nodes=%d row differences=%d node differences=%d",
			what, kind.Kind, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences)
		for _, difference := range append(append([]Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("%s reads a difference of kind %s at %s: %s", what, kind.Kind, difference.At, difference.Cause)
		}
	}
	if report.Differences() != 0 {
		t.Errorf("%s read %d difference(s), want none", what, report.Differences())
	}
	var rows int64
	if err := database.Pool.QueryRow(ctx, `select count(*) from document_blocks`).Scan(&rows); err != nil {
		t.Fatalf("count the document_blocks rows: %v", err)
	}
	blocks := censusKind(t, report, "block")
	t.Logf("%s: document_blocks holds %d rows; the census checked %d block rows against %d present block nodes",
		what, rows, blocks.Rows, blocks.Nodes)
	if rows < int64(len(present)) || blocks.Rows != rows || blocks.Nodes != rows {
		t.Errorf("%s checked %d block rows against %d present block nodes, want both the %d rows document_blocks holds, %d or more",
			what, blocks.Rows, blocks.Nodes, rows, len(present))
	}
	gone := document.refKey + "#" + statesRequirements[len(statesRequirements)-1].id
	var held int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from document_blocks where node_id = $1 or node_address = $1)
		     + (select count(*) from block_refs where from_node = $1 or node_address = $1)
	`, gone).Scan(&held); err != nil {
		t.Fatalf("count the projection rows of %s: %v", gone, err)
	}
	if held != 0 {
		t.Errorf("%s: the gone block %s holds %d projection row(s), want none", what, gone, held)
	}
	if t.Failed() {
		t.FailNow()
	}
}
