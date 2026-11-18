package refs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// censusAuthorKinds is every node kind dispatch://INFRA-1950 lets a writer reserve for its author:
// the kinds whose registry entry carries the author flag (nodeKind.authorOnly), and no other.
var censusAuthorKinds = []string{
	graphstore.KindConstraint, graphstore.KindInteraction, graphstore.KindLens, graphstore.KindLensBlock, graphstore.KindRisk,
}

// requireEachAuthorKindAnswersItsAuthorAlone is the assertion c-visibility-kinds adds to the census
// of the registry (dispatch://INFRA-1950/spec#b-r-visibility-complete). It runs once the census has
// held the registry to the kinds the store resolves, on the store seedStoreThroughItsWriters
// wrote, so a kind the registry does not hold fails there, named, before anything here is written.
//
// It writes, through the store's own writer (graphstore.Write, the write the reasoning routes and
// the going POST of api both wrap), one node of each of lens, lens_block, risk and constraint at
// `project` and one at `author`, under the census issue the seed wrote, and one run, whose
// interaction node InteractionFacts always reserves for its author. It reads each node through
// Node as a second person and as its author, and each node of those kinds the seed's writers
// wrote for alice too, and logs each kind, each reader and what each read found, so an absence
// stands beside the read that finds the same node. The second person finds each `project` node
// and no `author` node; the author finds both. A lens_kind is written at
// `project` and read by both, and a write of one at `author` is refused, naming the kind, and
// stores nothing. The registry's author flag is on for exactly the five kinds above, which are
// the kinds it hands the store (authorOnlyKinds, AdmitAuthorOnly), and Node is one of the reads
// that take the acting person as an argument (storeReads), so no read of a node learns its reader
// from anywhere else.
func requireEachAuthorKindAnswersItsAuthorAlone(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	for _, kind := range VisibilityKinds() {
		flagged, want := nodeRegistry[kind].authorOnly, slices.Contains(censusAuthorKinds, kind)
		t.Logf("the registry's author flag of %s is %t", kind, flagged)
		if flagged != want {
			t.Errorf("the registry's author flag of %s is %t, want %t: it is on for exactly %v", kind, flagged, want, censusAuthorKinds)
		}
	}
	// The registry hands the store what it flags (AdmitAuthorOnly). This test binary also hands it
	// the two kinds of visibility_comparison_test.go, so the store's own list is read for the five
	// alone.
	if handed := authorOnlyKinds(); !slices.Equal(handed, censusAuthorKinds) {
		t.Errorf("the registry hands the store the author-only kinds %v, want exactly %v", handed, censusAuthorKinds)
	}
	for _, kind := range censusAuthorKinds {
		if !slices.Contains(graphstore.AuthorOnlyKinds(), kind) {
			t.Errorf("the store takes no node of %s reserved for its author: it takes %v", kind, graphstore.AuthorOnlyKinds())
		}
	}
	if viewing, _ := storeReads(t); !slices.Contains(viewing, "Node") {
		t.Errorf("Node is no read of the graph that takes the acting person as an argument: the reads that do are %v", viewing)
	}

	alice, bob := model.Actor{Kind: "user", ID: "alice"}, model.Actor{Kind: "user", ID: "bob"}
	at := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	const issue = "CENSUS-1"
	node := func(kind, id, visibility string, attributes map[string]any) graphstore.Node {
		return graphstore.Node{
			Kind: kind, ID: id, Supertag: kind, Visibility: visibility, State: graphstore.StateLive,
			Project: "CENSUS", Attributes: attributes, MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent,
		}
	}
	tuple := func(fromKind, fromID, kind, toKind, toID string) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: fromKind, FromID: fromID, Kind: kind, ToKind: toKind, ToID: toID,
			At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true,
		}
	}
	write := func(nodes []graphstore.Node, tuples []graphstore.Tuple) error {
		return pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error { return graphstore.Write(ctx, tx, nodes, tuples) })
	}

	// One node of each kind at each visibility: the read is of the node and not of its kind, so
	// each pair is two ids of one kind, the lens's finding and its risk standing under the lens of
	// their own visibility.
	type written struct{ kind, id, visibility string }
	var reads []written
	for index, visibility := range []string{graphstore.VisibilityProject, graphstore.VisibilityAuthor} {
		lens := fmt.Sprintf("ce051000-1e45-4000-8000-00000000000%d", index+1)
		risk := fmt.Sprintf("ce052000-1e45-4000-8000-00000000000%d", index+1)
		constraint := fmt.Sprintf("ce053000-1e45-4000-8000-00000000000%d", index+1)
		finding := graphstore.KindLens + "/" + lens + "#" + visibility + "-finding"
		nodes := []graphstore.Node{
			node(graphstore.KindConstraint, constraint, visibility, map[string]any{graphstore.AttributeStandard: "The " + visibility + " standard."}),
			node(graphstore.KindLens, lens, visibility, nil),
			node(graphstore.KindRisk, risk, visibility, map[string]any{graphstore.AttributeHeading: "The " + visibility + " risk."}),
			node(BlockNodeKind, finding, visibility, nil),
		}
		tuples := []graphstore.Tuple{
			tuple(graphstore.KindLens, lens, graphstore.TuplePartOf, graphstore.KindIssue, issue),
			tuple(graphstore.KindLens, lens, graphstore.TupleHasKind, graphstore.KindLensKind, graphstore.LensKindPreMortem),
			tuple(BlockNodeKind, finding, graphstore.TuplePartOf, graphstore.KindLens, lens),
			tuple(BlockNodeKind, finding, graphstore.AttributeText, graphstore.KindValue, "The "+visibility+" finding."),
			tuple(graphstore.KindRisk, risk, graphstore.TuplePartOf, graphstore.KindLens, lens),
			tuple(graphstore.KindConstraint, constraint, graphstore.TuplePartOf, graphstore.KindIssue, issue),
		}
		if err := write(nodes, tuples); err != nil {
			t.Fatalf("write a lens, its finding, its risk and a constraint at %s through the store's writer: %v", visibility, err)
		}
		reads = append(reads,
			written{graphstore.KindLens, lens, visibility},
			written{graphstore.KindLensBlock, finding, visibility},
			written{graphstore.KindRisk, risk, visibility},
			written{graphstore.KindConstraint, constraint, visibility})
	}
	run := graphstore.Interaction{
		ID: "ce054000-1e45-4000-8000-000000000001", Author: alice, Project: "CENSUS",
		Words: "Which census node is alice's alone?", At: at, EndsAt: at.Add(30 * time.Second),
	}
	runNodes, runTuples := graphstore.InteractionFacts(run)
	if err := write(runNodes, runTuples); err != nil {
		t.Fatalf("write a run of the command bar through the store's writer: %v", err)
	}
	reads = append(reads, written{graphstore.KindInteraction, run.ID, graphstore.VisibilityAuthor})

	// A lens kind is a node of the catalogue: one at `project` is read by everyone, and a write of
	// one reserved for its author is refused before it sends a statement, naming the kind.
	const catalogued, reserved = "census-kind", "census-kind-reserved"
	if err := write([]graphstore.Node{
		node(graphstore.KindLensKind, catalogued, graphstore.VisibilityProject, map[string]any{graphstore.AttributeWords: "The census kind."}),
	}, nil); err != nil {
		t.Fatalf("write a lens kind at project through the store's writer: %v", err)
	}
	reads = append(reads, written{graphstore.KindLensKind, catalogued, graphstore.VisibilityProject})
	err := write([]graphstore.Node{
		node(graphstore.KindLensKind, reserved, graphstore.VisibilityAuthor, map[string]any{graphstore.AttributeWords: "The reserved census kind."}),
	}, nil)
	t.Logf("the write of a lens_kind at author answers: %v", err)
	var refused *graphstore.VisibilityNotAdmittedError
	if !errors.As(err, &refused) || refused.Kind != graphstore.KindLensKind {
		t.Errorf("the write of a lens_kind at author answers %v, want the refusal naming the kind %s", err, graphstore.KindLensKind)
	}
	if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindLensKind, reserved); err != nil || held {
		t.Errorf("the refused lens_kind is held %t (%v), want nothing of it stored", held, err)
	}

	// The nodes of the five kinds that seedStoreThroughItsWriters wrote for alice - a lens, its
	// finding, its risk and a constraint at `project` (seedReasoning), and the run graphcensus's
	// WriteRun states from its row (seedRun) - are read beside the ones above.
	ids := make([]string, 0, len(reads))
	for _, wrote := range reads {
		ids = append(ids, wrote.id)
	}
	rows, err := database.Pool.Query(ctx, `
		select case when kind = $1 then $2 else kind end, id, visibility from graph_nodes
		where (kind = any($3) or (kind = $1 and id like $4))
		  and made_by->>'kind' = 'user' and made_by->>'id' = $5 and not (id = any($6))
		order by kind, id`,
		BlockNodeKind, graphstore.KindLensBlock, []string{graphstore.KindConstraint, graphstore.KindInteraction, graphstore.KindLens, graphstore.KindRisk},
		graphstore.KindLens+"/%#%", alice.ID, ids)
	if err != nil {
		t.Fatalf("read the nodes the seed wrote for alice: %v", err)
	}
	seeded, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (written, error) {
		var wrote written
		return wrote, row.Scan(&wrote.kind, &wrote.id, &wrote.visibility)
	})
	if err != nil {
		t.Fatalf("read the nodes the seed wrote for alice: %v", err)
	}
	t.Logf("the seed's writers wrote for alice %+v", seeded)
	reads = append(reads, seeded...)

	for _, wrote := range reads {
		for _, reader := range []struct {
			name string
			who  model.Actor
		}{{"the second person bob", bob}, {"its author alice", alice}} {
			got, found, err := Node(ctx, database.Pool, reader.who, wrote.kind, wrote.id, true)
			if err != nil {
				t.Fatalf("read the %s node %s as %s: %v", wrote.kind, wrote.id, reader.name, err)
			}
			t.Logf("%s at %s, %s: %s reads found=%t kind=%q id=%q visibility=%q ref=%q",
				wrote.kind, wrote.visibility, wrote.id, reader.name, found, got.Kind, got.ID, got.Visibility, got.Ref)
			want := wrote.visibility == graphstore.VisibilityProject || reader.who.SameCredential(alice)
			switch {
			case found && !want:
				t.Errorf("%s: the second person bob was served the node %s that its author alice reserved for herself: %+v",
					wrote.kind, wrote.id, got)
			case !found && want:
				t.Errorf("%s: %s was served nothing of the %s node %s, which is %s to read", wrote.kind, reader.name,
					wrote.visibility, wrote.id, reader.name)
			case found && (got.Kind != wrote.kind || got.Visibility != wrote.visibility):
				t.Errorf("%s: %s read the node %s as kind %q at %q, want kind %q at %q", wrote.kind, reader.name, wrote.id,
					got.Kind, got.Visibility, wrote.kind, wrote.visibility)
			}
		}
	}
}
