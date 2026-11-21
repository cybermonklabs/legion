package refs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAnAncestorWalkAsSeenCostsAHiddenParentWhatAMissingOneCosts: the near-copy gate of a create
// reads its parent's chain before the create knows the caller sees the parent. Read as its caller
// sees it (IssueAncestorsAsSeen), PRIV-3, whose chain is PRIV-2 and PRIV-1, answers [] to every
// caller who does not see PRIV, as PRIV-9, which no row holds, answers to everyone; a caller who
// sees PRIV reads the chain IssueAncestors reads. Control: the walk with no clause answers the
// chain to alice too.
func TestAnAncestorWalkAsSeenCostsAHiddenParentWhatAMissingOneCosts(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	seedVerificationProjects(t, tx)
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank, parent_key) values
			('PRIV-1', 'PRIV', 1, 'Root', '{"kind":"user","id":"carol@example.com"}', 'U', null),
			('PRIV-2', 'PRIV', 2, 'Middle', '{"kind":"user","id":"carol@example.com"}', 'V', 'PRIV-1'),
			('PRIV-3', 'PRIV', 3, 'Leaf', '{"kind":"user","id":"carol@example.com"}', 'W', 'PRIV-2')
	`); err != nil {
		t.Fatalf("seed PRIV's chain: %v", err)
	}
	carol := model.Actor{Kind: "user", ID: seededCarol}
	writeSeedNodes(t, tx, []graphstore.Node{
		graphstore.IssueNode("PRIV-1", seededPrivate, carol, time.Now()),
		graphstore.IssueNode("PRIV-2", seededPrivate, carol, time.Now()),
		graphstore.IssueNode("PRIV-3", seededPrivate, carol, time.Now()),
	}, nil)
	whole, err := IssueAncestors(ctx, tx, "PRIV-3")
	if err != nil || !slices.Equal(whole, []string{"PRIV-2", "PRIV-1"}) {
		t.Fatalf("IssueAncestors(PRIV-3) = %v, %v; want [PRIV-2 PRIV-1]", whole, err)
	}
	sees := map[string]bool{"bob": true, "carol": true, "carol's token that lists PRIV": true, "bob's token that lists PRIV": true, "the server": true}
	for _, caller := range seededCallers() {
		reader := caller.reader(t, tx)
		missing, err := IssueAncestorsAsSeen(ctx, tx, reader, "PRIV-9")
		if err != nil || len(missing) != 0 {
			t.Fatalf("%s: IssueAncestorsAsSeen(PRIV-9) = %v, %v; want []", caller.name, missing, err)
		}
		hidden, err := IssueAncestorsAsSeen(ctx, tx, reader, "PRIV-3")
		if err != nil {
			t.Fatalf("%s: IssueAncestorsAsSeen(PRIV-3): %v", caller.name, err)
		}
		want := []string{}
		if sees[caller.name] {
			want = whole
		}
		if !slices.Equal(hidden, want) {
			t.Errorf("%s: IssueAncestorsAsSeen(PRIV-3) = %v, want %v", caller.name, hidden, want)
		}
	}
}
