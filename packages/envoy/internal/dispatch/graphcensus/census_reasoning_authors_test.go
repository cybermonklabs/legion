package graphcensus

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphseedtest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The census holds the author of each reasoning node to a node of the store (D12a of
// the plan): the person and session row sides take, beside the authors the
// projected tables name, the author graphstore.AuthorOf names of the made_by of each lens, finding,
// risk and constraint that holds an asserted authored_by tuple (reasoningAuthored), so an author
// who wrote no comment is no difference, and a session a body named, on the shared bearer or
// beside a pod's workload, is on both sides.
//
// Each node is written as the reasoning write (api's writeReasoningNodes) writes it: the node, its
// part_of, and the writer AuthorOf names of its made_by, that writer's node and facts through
// graphstore.AuthorFacts in the same graphstore.Write, whatever the credential proved (spec v46
// R12). alice writes a pre-mortem on a cookie; a session a broker-issued token proved carol's
// writes a risk under it, which makes the session's node and carol's; dave's personal token writes
// an author-only finding of it, made by the token itself; the shared bearer writes a constraint
// whose made_by names the session its body named, which proves nobody and is its writer all the
// same; and a pod writes a second constraint on a session its body names beside its workload. No
// comment, delivery or ask names any of them, so before this census read the authors of these
// kinds, each of the six author nodes was a node with no row. The census reads zero differences,
// three people and three sessions on both sides.
func TestTheCensusHoldsEachReasoningNodesAuthorToANode(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	owner := func(login string) *string { return &login }

	const (
		lens          = "00000000-0000-4000-8000-0000000001a1"
		risk          = "00000000-0000-4000-8000-0000000001a2"
		constraint    = "00000000-0000-4000-8000-0000000001a3"
		podConstraint = "00000000-0000-4000-8000-0000000001a4"
		proved        = "session-proved-carols"
		claimed       = "session-claimed-on-the-bearer"
		podSession    = "session-named-beside-a-pod"
	)
	finding := graphstore.KindLens + "/" + lens + "#first"
	for _, written := range []struct {
		kind, id, visibility string
		madeBy               model.Actor
		tuples               []graphstore.Tuple
	}{
		{kind: graphstore.KindLens, id: lens, visibility: graphstore.VisibilityProject,
			madeBy: model.Actor{Kind: "user", ID: "alice"},
			tuples: []graphstore.Tuple{
				{Kind: graphstore.TuplePartOf, ToKind: graphstore.KindIssue, ToID: "CORE-1"},
				{Kind: graphstore.TupleHasKind, ToKind: graphstore.KindLensKind, ToID: graphstore.LensKindPreMortem},
			}},
		{kind: graphstore.KindRisk, id: risk, visibility: graphstore.VisibilityProject,
			madeBy: model.Actor{Kind: "session", ID: proved, Owner: owner("carol"), Act: &model.ActorAct{Session: proved}},
			tuples: []graphstore.Tuple{{Kind: graphstore.TuplePartOf, ToKind: graphstore.KindLens, ToID: lens}}},
		{kind: refs.BlockNodeKind, id: finding, visibility: graphstore.VisibilityAuthor,
			madeBy: model.Actor{Kind: "session", Owner: owner("dave")},
			tuples: []graphstore.Tuple{{Kind: graphstore.TuplePartOf, ToKind: graphstore.KindLens, ToID: lens}}},
		{kind: graphstore.KindConstraint, id: constraint, visibility: graphstore.VisibilityProject,
			madeBy: model.Actor{Kind: "session", ID: claimed},
			tuples: []graphstore.Tuple{{Kind: graphstore.TuplePartOf, ToKind: graphstore.KindIssue, ToID: "CORE-1"}}},
		{kind: graphstore.KindConstraint, id: podConstraint, visibility: graphstore.VisibilityProject,
			madeBy: model.Actor{Kind: "session", ID: podSession, Service: owner("system:serviceaccount:legion:worker-a")},
			tuples: []graphstore.Tuple{{Kind: graphstore.TuplePartOf, ToKind: graphstore.KindIssue, ToID: "CORE-1"}}},
	} {
		node := graphstore.Node{
			Kind: written.kind, ID: written.id, Supertag: written.kind,
			Visibility: written.visibility, State: graphstore.StateLive, Project: "CORE",
			MadeAt: at, MadeBy: written.madeBy, SourceEvent: graphstore.NoEvent,
		}
		authors, tuples, err := graphseedtest.AuthoredFacts(node, written.tuples)
		if err != nil {
			t.Fatalf("state the author of %s/%s: %v", written.kind, written.id, err)
		}
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			// A finding reserved for its author is a node of a kind the store takes no such node
			// of until its registry entry is flagged (graphstore.ErrVisibilityNotAdmitted), so its
			// row and its base attributes are seeded as the reasoning write would store them, and
			// the rest of what that write states goes through the store's writer.
			if written.visibility == graphstore.VisibilityAuthor {
				if err := graphseedtest.SeedAuthorOnly(ctx, tx, node); err != nil {
					return err
				}
				return graphstore.Write(ctx, tx, authors, tuples)
			}
			return graphstore.Write(ctx, tx, append([]graphstore.Node{node}, authors...), tuples)
		}); err != nil {
			t.Fatalf("write %s/%s: %v", written.kind, written.id, err)
		}
	}
	for _, session := range []string{claimed, podSession} {
		if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindSession, session); err != nil || !held {
			t.Fatalf("the write that names the session %s as its writer made no node of it (held %t, %v)", session, held, err)
		}
	}

	report := storeCensusOf(t, database)
	for _, kind := range report.Kinds {
		t.Logf("kind=%s rows=%d nodes=%d row differences=%d node differences=%d",
			kind.Kind, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences)
		for _, difference := range differencesOf(kind) {
			t.Errorf("the census reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	if report.Differences() != 0 {
		t.Errorf("the census read %d difference(s), want none", report.Differences())
	}
	if people := censusKind(t, report, graphstore.KindPerson); people.Rows != 3 || people.Nodes != 3 {
		t.Errorf("the census checked %d people against %d person nodes, want alice, carol and dave on both sides",
			people.Rows, people.Nodes)
	}
	if sessions := censusKind(t, report, graphstore.KindSession); sessions.Rows != 3 || sessions.Nodes != 3 {
		t.Errorf("the census checked %d sessions against %d session nodes, want the proved session, the one the shared bearer's body named and the one beside the pod's workload on both sides",
			sessions.Rows, sessions.Nodes)
	}
}
