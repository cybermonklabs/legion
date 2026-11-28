package refs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A document a write made for a run created is listed by the run's references in both directions,
// and the run's count is the list behind it (spec requirement r-interaction, R4).
//
// A run of alice's creates an issue and its spec, so the write marks the issue's node and the
// document's node changed by the run: one changed_by_run out of each, to the run's interaction.
// The store keys the document's node by its ref_key, which is the from end that tuple names, while
// every other edge out of a document names it by its uuid. As alice, the run's ?to= lists both
// changed_by_run edges, the document resolved as every from end of a document is, by its uuid, and
// the run's backlink count is the number of rows that list holds. The document's ?from=, read by
// its uuid as GET /api/v1/references reads it, lists the document's changed_by_run to the run, and
// the issue's ?from= its own. Bob may not see alice's run, so neither ?from= lists it to him.
func TestARunsChangedByRunFromADocumentIsListedInBothDirectionsAndCounted(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	alice, bob := model.Actor{Kind: "user", ID: "alice"}, model.Actor{Kind: "user", ID: "bob"}
	at := time.Now().UTC().Truncate(time.Microsecond)
	const (
		project = "RUNDOC"
		issue   = "RUNDOC-1"
		run     = "5a1e0000-1e45-4000-8000-0000000000d1"
	)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the issue's rows: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, project)
	createReferenceIssue(t, tx, issue, project)
	document, refKey := createReferenceArtifact(t, tx, issue, project, "spec")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the issue's rows: %v", err)
	}

	interactionNodes, interactionTuples := graphstore.InteractionFacts(graphstore.Interaction{
		ID: run, Author: alice, Project: project, Words: "File the cache issue with its spec.", At: at, EndsAt: at.Add(time.Minute),
	})
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, interactionNodes, interactionTuples)
	}); err != nil {
		t.Fatalf("write alice's run: %v", err)
	}
	// The issue create's own write of the two nodes, made for the run (api's createIssue).
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(store.WithRun(ctx, run), tx, []graphstore.Node{
			graphstore.Created(graphstore.IssueNode(issue, project, alice, at)),
			graphstore.Created(graphstore.ArtifactNode(refKey, project, "", alice, at)),
		}, nil)
	}); err != nil {
		t.Fatalf("write the issue and its spec for the run: %v", err)
	}
	marked, err := graphstore.CurrentIn(ctx, database.Pool, graphstore.KindInteraction, run, []string{graphstore.TupleChangedByRun})
	if err != nil {
		t.Fatalf("read the changed_by_run tuples into the run: %v", err)
	}
	var from []string
	for _, tuple := range marked {
		from = append(from, tuple.FromKind+"/"+tuple.FromID)
	}
	slices.Sort(from)
	if want := []string{"artifact/" + refKey, "issue/" + issue}; !slices.Equal(from, want) {
		t.Fatalf("the run holds changed_by_run tuples from %v, want %v: the document by its ref_key, as the store keys its node", from, want)
	}

	changedEdges := func(who model.Actor, query Query) []string {
		t.Helper()
		edges, err := Edges(ctx, database.Pool, who, query)
		if err != nil {
			t.Fatalf("read the %s edges of %s %s as %s: %v", query.Direction, query.Kind, query.ID, who.ID, err)
		}
		var listed []string
		for _, edge := range edges {
			if edge.Kind == graphstore.TupleChangedByRun {
				listed = append(listed, edge.Node.Kind+"/"+edge.Node.ID)
			}
		}
		slices.Sort(listed)
		return listed
	}

	into := changedEdges(alice, Query{Direction: "in", Kind: graphstore.KindInteraction, ID: run})
	t.Logf("alice's ?to= of her run lists the changed_by_run edges from %v", into)
	if want := []string{"artifact/" + document, "issue/" + issue}; !slices.Equal(into, want) {
		t.Errorf("alice's ?to= of her run lists the changed_by_run edges from %v, want %v: the document by its uuid, and the issue", into, want)
	}
	counts, err := BacklinkCounts(ctx, database.Pool, alice, graphstore.KindInteraction, []string{run}, nil)
	if err != nil {
		t.Fatalf("count the run's backlinks as alice: %v", err)
	}
	edges, err := Edges(ctx, database.Pool, alice, Query{Direction: "in", Kind: graphstore.KindInteraction, ID: run})
	if err != nil {
		t.Fatalf("read alice's ?to= of her run: %v", err)
	}
	t.Logf("the run's backlink count is %d, and its ?to= lists %d edges", counts[run], len(edges))
	if counts[run] != len(edges) || counts[run] != 2 {
		t.Errorf("the run's backlink count is %d and its ?to= lists %d edges, want both 2: a count is the list behind it", counts[run], len(edges))
	}

	interaction := []string{graphstore.KindInteraction + "/" + run}
	for _, read := range []struct {
		name  string
		query Query
	}{
		{"the document's ?from=", Query{Direction: "out", Kind: "artifact", ID: document}},
		{"the issue's ?from=", Query{Direction: "out", Kind: "issue", ID: issue}},
	} {
		if out := changedEdges(alice, read.query); !slices.Equal(out, interaction) {
			t.Errorf("alice's %s lists the changed_by_run edges to %v, want %v", read.name, out, interaction)
		}
		if out := changedEdges(bob, read.query); len(out) != 0 {
			t.Errorf("bob's %s lists the changed_by_run edges to %v, want none: alice's run is hers alone", read.name, out)
		}
	}
}
