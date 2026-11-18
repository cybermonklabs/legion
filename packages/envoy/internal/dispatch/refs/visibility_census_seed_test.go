package refs_test

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func init() { refs.SeedStoreThroughItsWriters = seedStoreThroughItsWriters }

// seedStoreThroughItsWriters writes the rows that address the people and agents of a running
// deployment - a comment a person wrote, a comment a session wrote and an issue routed to a
// role - and then runs the writer that states the store from such rows, graphcensus's backfill:
// the person, the session, the role and the role's page are each a node that writer makes from
// one of those rows, as it makes them for a database written before the store existed. Then it
// writes what a person reasoning about that issue writes (seedReasoning) and one run of the
// command bar that person asked (seedRun). No kind is named here; which kinds the store then
// holds is what the census reads from graph_nodes.
func seedStoreThroughItsWriters(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('CENSUS', 'Census project', 'alice')`,
		`insert into issues (key, project_key, number, title, created_by, rank, route)
		 values ('CENSUS-1', 'CENSUS', 1, 'Census issue', '{"kind":"user","id":"alice"}', 'U', 'role:census-role')`,
		`insert into comments (issue_key, author, body)
		 values ('CENSUS-1', '{"kind":"user","id":"alice"}', 'A person''s remark.')`,
		`insert into comments (issue_key, author, body)
		 values ('CENSUS-1', '{"kind":"session","id":"census-session"}', 'A session''s remark.')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the rows that address people and agents: %v", err)
		}
	}
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("state the store from those rows through its backfill: %v", err)
	}
	seedReasoning(t, ctx, database)
	seedRun(t, ctx, database)
}

// seedRun records one run of the command bar alice asked, as a going_requests row, and states it
// in the store through the writer that states a run from its row (graphcensus.WriteRun): the
// interaction node, its words and its authored_by, in the shape the going POST writes them, since
// both write graphstore.InteractionFacts through graphstore.Write. The POST's own writer is a
// method of api's server, which imports this package, so this external test package takes the
// store write it wraps, from the row as a release before this one leaves it.
func seedRun(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into going_requests (author, project, words, ends_at)
		values ('{"kind":"user","id":"alice"}', 'CENSUS', 'What did the census ask?', now() + interval '30 seconds')
		returning id::text
	`).Scan(&id); err != nil {
		t.Fatalf("record a run of the command bar: %v", err)
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		held, err := graphcensus.WriteRun(ctx, tx, id)
		if err == nil && !held {
			err = fmt.Errorf("the record holds no run %s", id)
		}
		return err
	}); err != nil {
		t.Fatalf("state the run in the store through its writer: %v", err)
	}
}

// seedReasoning writes, under the census issue, a pre-mortem lens with one finding and one risk,
// and a constraint, as the reasoning write of api states them (writeReasoningNodes): each node
// through the store's own writer (graphstore.Write), with its part_of tuple to what it stands
// under, the lens's has_kind and the finding's text, in one transaction. That write is api's,
// which imports this package, so this external test package states the same nodes and tuples
// through the store write it wraps.
func seedReasoning(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	const (
		lens       = "ce050000-1e45-4000-8000-000000000001"
		risk       = "ce050000-1e45-4000-8000-000000000002"
		constraint = "ce050000-1e45-4000-8000-000000000003"
		issue      = "CENSUS-1"
	)
	finding := graphstore.KindLens + "/" + lens + "#census-finding"
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	alice := model.Actor{Kind: "user", ID: "alice"}
	node := func(kind, id string) graphstore.Node {
		return graphstore.Node{
			Kind: kind, ID: id, Supertag: kind, Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			Project: "CENSUS", MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent,
		}
	}
	tuple := func(fromKind, fromID, kind, toKind, toID string) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: fromKind, FromID: fromID, Kind: kind, ToKind: toKind, ToID: toID,
			At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true,
		}
	}
	nodes := []graphstore.Node{
		node(graphstore.KindConstraint, constraint),
		node(graphstore.KindLens, lens),
		node(graphstore.KindRisk, risk),
		node(refs.BlockNodeKind, finding),
	}
	nodes[0].Attributes = map[string]any{graphstore.AttributeStandard: "The census standard."}
	nodes[2].Attributes = map[string]any{graphstore.AttributeHeading: "The census risk."}
	tuples := []graphstore.Tuple{
		tuple(graphstore.KindLens, lens, graphstore.TuplePartOf, graphstore.KindIssue, issue),
		tuple(graphstore.KindLens, lens, graphstore.TupleHasKind, graphstore.KindLensKind, graphstore.LensKindPreMortem),
		tuple(refs.BlockNodeKind, finding, graphstore.TuplePartOf, graphstore.KindLens, lens),
		tuple(refs.BlockNodeKind, finding, graphstore.AttributeText, graphstore.KindValue, "The census finding."),
		tuple(graphstore.KindRisk, risk, graphstore.TuplePartOf, graphstore.KindLens, lens),
		tuple(graphstore.KindConstraint, constraint, graphstore.TuplePartOf, graphstore.KindIssue, issue),
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, nodes, tuples)
	}); err != nil {
		t.Fatalf("write a lens, its finding, its risk and a constraint through the store's writer: %v", err)
	}
}
