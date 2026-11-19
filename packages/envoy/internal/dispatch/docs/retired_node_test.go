package docs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A retired document's node stays gone. The retirement states the node gone and keeps the row
// (RetireDocument), so no reader of an artifacts row against its node may read that row as owed a
// live node: the decisions step's first phase (graphcensus.BackfillAsks, which a tick runs where
// graphcensus.AsksOwed says a row is owed) would otherwise state the node live again, and the
// project census would name the row gone, a finding whose cure is that same backfill.
//
// An issue's decisions document, which a build before the store wrote and the first phase gave
// its node, is retired as the decisions step's conversion retires one. Then, before a tick of the
// step and after it, the store holds the document's node gone, AsksOwed reads nothing owed, and
// the project census reads no artifacts row gone.
func TestARetiredDocumentsNodeStaysGoneThroughTheDecisionsStep(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('ACME', 'Acme', 'alice')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
		 values ('ACME-1', 'ACME', 1, 'One', '{"kind":"user","id":"alice"}', 'U')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed issue ACME-1: %v", err)
		}
	}
	var document, refKey string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
		values ('ACME-1', 'ACME', 'decisions-md', 'decisions.md', 'doc', false, 'decisions', '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`).Scan(&document, &refKey); err != nil {
		t.Fatalf("write the decisions document of ACME-1: %v", err)
	}
	if _, err := graphcensus.BackfillAsks(ctx, database); err != nil {
		t.Fatalf("write the document's node as the first phase does: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retirement: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := RetireDocument(ctx, tx, document, DecisionStepActor, "Its sections are decisions of the spec now."); err != nil {
		t.Fatalf("retire the decisions document: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}

	requireStaysGone := func(when string) {
		t.Helper()
		var state string
		if err := database.Pool.QueryRow(ctx, `select state from graph_nodes where kind = $1 and id = $2`,
			graphstore.KindArtifact, refKey).Scan(&state); err != nil {
			t.Fatalf("read the node of %s %s: %v", refKey, when, err)
		}
		if state != graphstore.StateGone {
			t.Errorf("the node of the retired document %s is %s %s, want it gone as its retirement stated it", refKey, state, when)
		}
		if owed, err := graphcensus.AsksOwed(ctx, database.Pool); err != nil || owed {
			t.Errorf("AsksOwed reads %v (%v) %s, want nothing owed: the retired document's row is owed no live node", owed, err, when)
		}
		reports, err := graphcensus.ProjectCensus(ctx, database.Pool)
		if err != nil {
			t.Fatalf("read the project census %s: %v", when, err)
		}
		for _, report := range reports {
			if report.Kind == graphstore.KindArtifact && report.Findings() != 0 {
				t.Errorf("the project census reads %d artifacts row(s) gone and %d other finding(s) %s, want none: the retired document's gone node is the one its row projects",
					report.Gone, report.Findings()-report.Gone, when)
			}
		}
	}
	requireStaysGone("before the tick")
	if err := NewDecisionStep(database, catchUpCommit, nil).Tick(ctx); err != nil {
		t.Fatalf("tick the decisions step: %v", err)
	}
	requireStaysGone("after the tick")
}
