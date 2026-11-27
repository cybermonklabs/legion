package graphstore

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// laterState is a state no writer of this release writes: a later release's build marks a
// node proposed. This release serves beside that build during a roll and alone after a rollback,
// so its writers meet nodes that build marked.
const laterState = "proposed"

// TestARestatingWriteKeepsTheValuesItDoesNotOwn holds a write that restates a node (Node.Restates)
// to the rule moveNodes writes it by (R8 of this change). The node is a comment's as
// its one statement writes it (CommentFacts), which restates it. Marked at a later release's
// state and reserved for its author by a plain write, it keeps both when that restating write
// writes it live and project-visible again; a restating write that says gone writes it gone and
// keeps its visibility; a restating write of a node stored gone writes it live; a node stored
// live stays live; and a plain write still moves both. After every write the row and the log of
// its base attributes agree, and the log holds one tuple for each value a write moved and none
// for a value a restating write kept.
func TestARestatingWriteKeepsTheValuesItDoesNotOwn(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}

	restated, _ := CommentFacts(nil, "restated-comment", who, at, NoEvent, "", "", "", nil)
	if !restated.Restates {
		t.Fatal("CommentFacts writes a comment's node that does not restate it: a write of it again would move the state another write gave it")
	}
	states := []string{StateLive}
	visibilities := []string{VisibilityProject}
	// write writes the node a minute after the write before it, so each tuple a write states is
	// dated by that write.
	writes := 0
	write := func(node Node) {
		t.Helper()
		node.MadeAt = at.Add(time.Duration(writes) * time.Minute)
		writes++
		writeNode(t, ctx, database, node)
	}

	// A node the store does not hold is made as the restating write states it.
	write(restated)
	requireBaseLog(t, ctx, database, restated, StateLive, VisibilityProject, states, visibilities)

	marking := restated
	marking.Restates = false
	marking.State, marking.Visibility = laterState, VisibilityAuthor
	write(marking)
	states, visibilities = append(states, laterState), append(visibilities, VisibilityAuthor)
	requireBaseLog(t, ctx, database, restated, laterState, VisibilityAuthor, states, visibilities)

	// Live and project-visible, restated: both kept, nothing stated.
	write(restated)
	requireBaseLog(t, ctx, database, restated, laterState, VisibilityAuthor, states, visibilities)

	// Gone, restated: written gone, the visibility kept.
	gone := restated
	gone.State = StateGone
	write(gone)
	states = append(states, StateGone)
	requireBaseLog(t, ctx, database, restated, StateGone, VisibilityAuthor, states, visibilities)

	// Live over gone, restated: written live, the visibility still kept.
	write(restated)
	states = append(states, StateLive)
	requireBaseLog(t, ctx, database, restated, StateLive, VisibilityAuthor, states, visibilities)

	// Live over live, restated: nothing moves.
	write(restated)
	requireBaseLog(t, ctx, database, restated, StateLive, VisibilityAuthor, states, visibilities)

	// A plain write moves both.
	plain := marking
	plain.Visibility = VisibilityProject
	write(plain)
	states, visibilities = append(states, laterState), append(visibilities, VisibilityProject)
	requireBaseLog(t, ctx, database, restated, laterState, VisibilityProject, states, visibilities)

	// One write of two nodes, handed in out of (kind, id) order, restating one and moving the
	// other: whether a node is restated travels with that node, not with its place in the list.
	t.Run("one write restating one node and moving another", func(t *testing.T) {
		node := func(id string) Node {
			return Node{
				Kind: KindComment, ID: id, Supertag: KindComment,
				Visibility: VisibilityProject, State: laterState,
				MadeAt: at, MadeBy: who, SourceEvent: NoEvent,
			}
		}
		moving, kept := node("mixed-a"), node("mixed-b")
		writeNode(t, ctx, database, moving)
		writeNode(t, ctx, database, kept)

		moving.State, moving.MadeAt = StateLive, at.Add(time.Minute)
		kept.State, kept.MadeAt, kept.Restates = StateLive, at.Add(time.Minute), true
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			return Write(ctx, tx, []Node{kept, moving}, nil)
		}); err != nil {
			t.Fatalf("write the two nodes: %v", err)
		}
		requireBaseLog(t, ctx, database, kept, laterState, VisibilityProject,
			[]string{laterState}, []string{VisibilityProject})
		requireBaseLog(t, ctx, database, moving, StateLive, VisibilityProject,
			[]string{laterState, StateLive}, []string{VisibilityProject})
	})
}

// TestARestatingWriteKeepsTheStoredAttributes holds a write that restates a node (Node.Restates)
// to keeping the attributes its row stores, as it keeps the row's visibility (writtenAttributes):
// a restating writer owns the facts it writes the node again for, not the node's attributes,
// which another write gave it. A node a plain write made with attributes keeps them when a
// restating write states other attributes, and when one states none, as every restating writer
// of this tree writes a node; a plain write still moves them.
//
// Neither of those two restating writes moves anything else of the row, so the move's condition
// alone keeps them from updating it, whatever the update would set. A restating write that writes
// the node gone moves its state, so the update runs, and the attributes it sets must still be the
// row's: that step is what fails where the update sets the incoming attributes.
func TestARestatingWriteKeepsTheStoredAttributes(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}

	stored := Node{
		Kind: KindSession, ID: "restated-attributes", Supertag: KindSession,
		Visibility: VisibilityProject, State: StateLive,
		Attributes: map[string]any{"title": "the stored title"},
		MadeAt:     at, MadeBy: who, SourceEvent: NoEvent,
	}
	writeNode(t, ctx, database, stored)
	requireNodeAttributes(t, ctx, database, stored, stored.Attributes, "the plain write that made it")

	restated := stored
	restated.Restates, restated.MadeAt = true, at.Add(time.Minute)
	restated.Attributes = map[string]any{"title": "the restated title"}
	writeNode(t, ctx, database, restated)
	requireNodeAttributes(t, ctx, database, stored, stored.Attributes, "a restating write stating other attributes")

	bare := restated
	bare.Attributes, bare.MadeAt = nil, at.Add(2*time.Minute)
	writeNode(t, ctx, database, bare)
	requireNodeAttributes(t, ctx, database, stored, stored.Attributes, "a restating write stating none")

	gone := restated
	gone.State, gone.MadeAt = StateGone, at.Add(3*time.Minute)
	writeNode(t, ctx, database, gone)
	if row, held, err := ReadNode(ctx, database.Pool, gone.Kind, gone.ID); err != nil || !held || row.State != StateGone {
		t.Fatalf("read node %s/%s after a restating write that writes it gone: state %q, held %t, %v; want it gone, so that write updated the row",
			gone.Kind, gone.ID, row.State, held, err)
	}
	requireNodeAttributes(t, ctx, database, stored, stored.Attributes, "a restating write stating other attributes that writes it gone")

	plain := restated
	plain.Restates, plain.MadeAt = false, at.Add(4*time.Minute)
	writeNode(t, ctx, database, plain)
	requireNodeAttributes(t, ctx, database, stored, plain.Attributes, "a plain write stating other attributes")
}

// requireNodeAttributes fails unless node's row holds want as its attributes after the write
// after names.
func requireNodeAttributes(t *testing.T, ctx context.Context, database *store.Store, node Node, want map[string]any, after string) {
	t.Helper()
	row, held, err := ReadNode(ctx, database.Pool, node.Kind, node.ID)
	if err != nil || !held {
		t.Fatalf("read node %s/%s after %s: held %t, %v", node.Kind, node.ID, after, held, err)
	}
	if !maps.Equal(row.Attributes, want) {
		t.Errorf("after %s, node %s/%s's row holds the attributes %v, want %v: a restating write keeps the attributes the row stores, and only a plain write moves them",
			after, node.Kind, node.ID, row.Attributes, want)
	}
}

// requireBaseLog fails unless node's row holds state and visibility, the log's newest value of
// each is the row's, and the log holds exactly the values states and visibilities, in order: one
// tuple for each write that moved the attribute, and none for a write that kept it.
func requireBaseLog(
	t *testing.T, ctx context.Context, database *store.Store, node Node,
	state, visibility string, states, visibilities []string,
) {
	t.Helper()
	row, held, err := ReadNode(ctx, database.Pool, node.Kind, node.ID)
	if err != nil || !held {
		t.Fatalf("read node %s/%s: held %t, %v", node.Kind, node.ID, held, err)
	}
	if row.State != state || row.Visibility != visibility {
		t.Errorf("node %s/%s's row reads state %q and visibility %q, want %q and %q",
			node.Kind, node.ID, row.State, row.Visibility, state, visibility)
	}
	for _, attribute := range []struct {
		name, row string
		want      []string
	}{
		{AttributeState, row.State, states},
		{AttributeVisibility, row.Visibility, visibilities},
	} {
		current, held, err := CurrentValue(ctx, database.Pool, node.Kind, node.ID, attribute.name)
		if err != nil || !held || current != attribute.row {
			t.Errorf("node %s/%s's row reads %s %q and the log's newest is %q (held %t, %v): the row is not the projection of its tuples",
				node.Kind, node.ID, attribute.name, attribute.row, current, held, err)
		}
		history, err := History(ctx, database.Pool, node.Kind, node.ID, attribute.name)
		if err != nil {
			t.Fatalf("read node %s/%s's %s history: %v", node.Kind, node.ID, attribute.name, err)
		}
		stated := make([]string, len(history))
		for index, tuple := range history {
			stated[index] = tuple.ToID
		}
		if !slices.Equal(stated, attribute.want) {
			t.Errorf("node %s/%s's log states %s %v, want %v: one tuple for each write that moved it, none for one that kept it",
				node.Kind, node.ID, attribute.name, stated, attribute.want)
		}
	}
}
