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
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Nobody runs a retired document's checks (spec requirement r-constraint-risk), so
// RetireDocument retracts, in its own transaction, each prevented_by the store asserts at a block
// of the document it retires. One risk is prevented by the retired document's check alone, and a
// second by that check and by a check of a document that stands. After the retirement the first
// reads prevented by nothing and the second by the standing check alone; each retraction is dated
// at the retirement's own time, signed by its writer and caused by its document.retired event. The
// retired document's requirement keeps its constrained_by: the retirement retracts the pointers at
// its checks, and no other.
func TestARetirementRetractsThePreventedByOfItsChecks(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	alice := model.Actor{Kind: "user", ID: "alice"}
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	exec("create the project", `insert into projects (key, name, default_assignee) values ('RET', 'Retirements', 'alice')`)
	exec("create the issue", `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('RET-1', 'RET', 1, 'Checks a retirement takes with it', '{"kind":"user","id":"alice"}', 'n')
	`)
	document := func(slug string) (id, refKey string) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ('RET-1', 'RET', $1, $1 || '.md', 'doc', '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`, slug).Scan(&id, &refKey); err != nil {
			t.Fatalf("create the document %s: %v", slug, err)
		}
		return id, refKey
	}
	retiredID, retiredKey := document("verification")
	standingID, standingKey := document("notes")
	retiredCheck, standingCheck, requirement := retiredKey+"#chk", standingKey+"#chk", retiredKey+"#req"
	const (
		retiredRisk = "risk-of-the-retired-check"
		bothRisk    = "risk-of-both-checks"
		constraint  = "constraint-of-the-requirement"
	)
	at := time.Date(2026, 11, 18, 9, 46, 0, 0, time.UTC)
	node := func(kind, id string) graphstore.Node {
		return graphstore.Node{
			Kind: kind, ID: id, Supertag: kind, Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			Project: "RET", MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent,
		}
	}
	tuple := func(fromKind, fromID, kind, toKind, toID string) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: fromKind, FromID: fromID, Kind: kind, ToKind: toKind, ToID: toID,
			At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true,
		}
	}
	write := func(what string, nodes []graphstore.Node, tuples []graphstore.Tuple) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			return graphstore.Write(ctx, tx, nodes, tuples)
		}); err != nil {
			t.Fatalf("write %s: %v", what, err)
		}
	}
	write("the blocks, the risks and the constraint", []graphstore.Node{
		node(refs.BlockNodeKind, retiredCheck), node(refs.BlockNodeKind, standingCheck), node(refs.BlockNodeKind, requirement),
		node(graphstore.KindRisk, retiredRisk), node(graphstore.KindRisk, bothRisk), node(graphstore.KindConstraint, constraint),
	}, nil)
	for _, block := range []struct{ artifact, blockID, id, blockType string }{
		{retiredID, "chk", retiredCheck, graphstore.BlockTypeCheck},
		{standingID, "chk", standingCheck, graphstore.BlockTypeCheck},
		{retiredID, "req", requirement, "requirement"},
	} {
		exec("write the row of "+block.id, `
			insert into document_blocks (artifact_id, block_id, node_id, node_kind, node_address, type, text_sha256, excerpt, changed_version, changed_at)
			values ($1::uuid, $2, $3, $4, $3, $5, '', '', 1, now())
		`, block.artifact, block.blockID, block.id, refs.BlockNodeKind, block.blockType)
	}
	write("the pointers", nil, []graphstore.Tuple{
		tuple(graphstore.KindRisk, retiredRisk, graphstore.TuplePreventedBy, refs.BlockNodeKind, retiredCheck),
		tuple(graphstore.KindRisk, bothRisk, graphstore.TuplePreventedBy, refs.BlockNodeKind, retiredCheck),
		tuple(graphstore.KindRisk, bothRisk, graphstore.TuplePreventedBy, refs.BlockNodeKind, standingCheck),
		tuple(refs.BlockNodeKind, requirement, graphstore.TupleConstrainedBy, graphstore.KindConstraint, constraint),
	})

	var retirement Retirement
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		var err error
		retirement, err = RetireDocument(ctx, tx, retiredID, alice, "The test retires the document that holds the check.")
		return err
	}); err != nil {
		t.Fatalf("retire the document: %v", err)
	}
	var retiredAt time.Time
	if err := database.Pool.QueryRow(ctx, `select retired_at from artifacts where id = $1::uuid`, retiredID).Scan(&retiredAt); err != nil {
		t.Fatalf("read when the document was retired: %v", err)
	}

	current := func(kind, id, tupleKind string) []string {
		t.Helper()
		held, err := graphstore.CurrentOut(ctx, database.Pool, kind, id, []string{tupleKind})
		if err != nil {
			t.Fatalf("read the %s tuples of %s %s: %v", tupleKind, kind, id, err)
		}
		ends := []string{}
		for _, tuple := range held {
			ends = append(ends, tuple.ToKind+"/"+tuple.ToID)
		}
		return ends
	}
	if got := current(graphstore.KindRisk, retiredRisk, graphstore.TuplePreventedBy); len(got) != 0 {
		t.Errorf("after the retirement %s is prevented by %v, want nothing", retiredRisk, got)
	}
	if got := current(graphstore.KindRisk, bothRisk, graphstore.TuplePreventedBy); !slices.Equal(got, []string{refs.BlockNodeKind + "/" + standingCheck}) {
		t.Errorf("after the retirement %s is prevented by %v, want the check of the document that stands alone", bothRisk, got)
	}
	if got := current(refs.BlockNodeKind, requirement, graphstore.TupleConstrainedBy); !slices.Equal(got, []string{graphstore.KindConstraint + "/" + constraint}) {
		t.Errorf("after the retirement the retired document's requirement is held to %v, want its constraint", got)
	}
	for _, risk := range []string{retiredRisk, bothRisk} {
		history, err := graphstore.History(ctx, database.Pool, graphstore.KindRisk, risk, graphstore.TuplePreventedBy)
		if err != nil {
			t.Fatalf("read the prevented_by history of %s: %v", risk, err)
		}
		index := slices.IndexFunc(history, func(tuple graphstore.Tuple) bool { return !tuple.Asserted })
		if index < 0 || slices.ContainsFunc(history[index+1:], func(tuple graphstore.Tuple) bool { return !tuple.Asserted }) {
			t.Fatalf("the prevented_by history of %s is %+v, want one retraction", risk, history)
		}
		retraction := history[index]
		t.Logf("%s: the retraction of its prevented_by at %s, at %s by %+v under event %d", risk, retraction.ToID, retraction.At, retraction.Who, retraction.SourceEvent)
		if retraction.ToID != retiredCheck || !retraction.At.Equal(retiredAt) || !retraction.Who.SameCredential(alice) ||
			retraction.SourceEvent != retirement.Event.ID {
			t.Errorf("%s's prevented_by was retracted as %+v, want at %s, at the retirement's time %s, by alice, under the document.retired event %d",
				risk, retraction, retiredCheck, retiredAt, retirement.Event.ID)
		}
	}
}
