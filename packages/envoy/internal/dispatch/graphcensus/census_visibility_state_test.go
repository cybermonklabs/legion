package graphcensus

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheCensusHoldsEachNodesVisibilityAndStateToItsNewestTuples is the census of the visibility
// and the state each run, lens, finding, risk, constraint and lens kind carries
// (baseAttributeCensus): a node's row is the projection of its own base-attribute tuples, so the
// census holds the graph_nodes columns to the newest tuple of each, both ways. A run and a lens
// written through the store's writer agree. Then, one break at a time, the census reads: a column
// that differs from its newest tuple (the lens moved to settled on its row alone, which the digest
// names as the state alone, live on the row side and settled on the node's, the visibility the same
// on both), a run at a visibility other than author with its tuple saying the same, a node whose
// columns no tuple stands behind (a risk row written with no tuple), and a tuple whose node has no
// row (a constraint's state with no node). Each is named with its kind and address, and the
// attribute, by the digest that names each attribute and carries its value's md5.
func TestTheCensusHoldsEachNodesVisibilityAndStateToItsNewestTuples(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	run := olderPodRun(t, database, alice, "", "who answers this?", "Half an ")
	giveRunItsNode(t, database, run)
	const (
		lens       = "00000000-0000-4000-8000-0000000002a1"
		risk       = "00000000-0000-4000-8000-0000000002a2"
		constraint = "00000000-0000-4000-8000-0000000002a3"
	)
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lens: %v", err)
	}
	if err := graphstore.Write(ctx, tx, []graphstore.Node{{
		Kind: graphstore.KindLens, ID: lens, Supertag: graphstore.KindLens,
		Visibility: graphstore.VisibilityProject, State: graphstore.StateLive, Project: "CORE",
		MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent,
	}}, nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("write the lens: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the lens: %v", err)
	}

	agreed := storeCensusOf(t, database)
	for _, name := range []string{"interaction_visibility_state", "lens_visibility_state"} {
		if held := censusKind(t, agreed, name); held.Rows != 1 || held.Nodes != 1 || held.RowDifferences+held.NodeDifferences != 0 {
			t.Fatalf("the census of the agreeing store reads %s at %d rows, %d nodes, %+v and %+v, want one each and no difference",
				name, held.Rows, held.Nodes, held.ReportedRows, held.ReportedNodes)
		}
	}
	for _, name := range []string{"lens_kind_visibility_state", "constraint_visibility_state"} {
		if held := censusKind(t, agreed, name); held.Rows == 0 || held.RowDifferences+held.NodeDifferences != 0 {
			t.Fatalf("the census reads the catalogue's %s at %d rows, %+v and %+v, want its seeded nodes and no difference",
				name, held.Rows, held.ReportedRows, held.ReportedNodes)
		}
	}

	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	exec("move the lens to settled on its row alone",
		`update graph_nodes set state = 'settled' where kind = $1 and id = $2`, graphstore.KindLens, lens)
	exec("state the run project-visible and move its row there",
		`insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		 values ($1, $2, 'visibility', 'value', 'project', now(), '{"kind": "user", "id": "alice"}', 0, true)`,
		graphstore.KindInteraction, run)
	exec("move the run's row to project", `update graph_nodes set visibility = 'project' where kind = $1 and id = $2`,
		graphstore.KindInteraction, run)
	exec("write a risk row no tuple stands behind",
		`insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		 values ($1, $2, $1, 'project', 'live', '{}', now(), '{"kind": "user", "id": "alice"}', 0)`, graphstore.KindRisk, risk)
	exec("state a constraint's state with no node row",
		`insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		 values ($1, $2, 'state', 'value', 'live', now(), '{"kind": "user", "id": "alice"}', 0, true)`,
		graphstore.KindConstraint, constraint)

	broken := storeCensusOf(t, database)
	digestOf := func(attribute, value string) string {
		sum := md5.Sum([]byte(value))
		return attribute + ":" + hex.EncodeToString(sum[:])
	}
	stateAlone := digestOf(graphstore.AttributeState, graphstore.StateLive) + " and the node " +
		digestOf(graphstore.AttributeVisibility, graphstore.VisibilityProject) + " " + digestOf(graphstore.AttributeState, "settled")
	for _, want := range []struct {
		kind, at, cause, carries string
		row                      bool
	}{
		{"lens_visibility_state", "lens " + lens, "the row and its node disagree", stateAlone, true},
		{"interaction_visibility_state", "interaction " + run, "the row and its node disagree", "visibility_fixed:broken", true},
		{"risk_visibility_state", "risk " + risk, "a live node no row projects", "", false},
		{"constraint_visibility_state", "constraint " + constraint, "the row names a node the store does not hold", "", true},
	} {
		held := censusKind(t, broken, want.kind)
		found := held.ReportedNodes
		if want.row {
			found = held.ReportedRows
		}
		if !hasDifference(found, want.at, want.cause) {
			t.Errorf("the census reads %s differences %+v and %+v, want one at %s: %s", want.kind, held.ReportedRows, held.ReportedNodes, want.at, want.cause)
			continue
		}
		for _, difference := range found {
			if difference.At == want.at && !strings.Contains(difference.Cause, want.carries) {
				t.Errorf("the census names %s at %s with %q, which does not name %q", want.kind, want.at, difference.Cause, want.carries)
			}
			if difference.At == want.at {
				t.Logf("%s: %s: %s", want.kind, difference.At, difference.Cause)
			}
		}
		if held.RowDifferences+held.NodeDifferences != 1 {
			t.Errorf("the census reads %d differences of %s, want the one break", held.RowDifferences+held.NodeDifferences, want.kind)
		}
	}
}
