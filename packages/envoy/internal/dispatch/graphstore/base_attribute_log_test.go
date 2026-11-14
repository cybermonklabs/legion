package graphstore

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A node row is the projection of its own tuples (R1 of dispatch://INFRA-1149, and this
// package's own doc): every fact is appended, an attribute that changes is a second tuple
// carrying the new value, and a tuple holds the write that caused it. The two base attributes
// every node carries, `visibility` and `state`, are attribute tuples under that rule (R8), so
// the log and the row have to answer the same thing about them.
//
// The half below reads the store the way a reader does - through CurrentValue and History,
// which answer the tuples alone - and compares that with the row, which is what the visibility
// predicate and the census read. A build where the two disagree has a row no appended fact
// explains, and the log cannot be corrected afterwards, because nothing may update a tuple.
//
// The other half, that every base attribute tuple carries the write that caused it, is
// TestEveryBaseAttributeTupleCarriesTheWriteThatCausedIt in internal/dispatch/api: the cause a
// base attribute carries is decided by the order a request writes its node, its projection row
// and its event in, so that half drives the real comment create path and reads the event out of
// `events`.

func TestABaseAttributeThatMovesIsStatedInTheLog(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	node := Node{
		Kind: KindPerson, ID: "base-attribute-move", Supertag: "person",
		Visibility: VisibilityProject, State: StateLive,
		MadeAt: at, MadeBy: who, SourceEvent: NoEvent,
	}
	writeNode(t, ctx, database, node)

	moved := node
	moved.Visibility = VisibilityAuthor
	moved.MadeAt = at.Add(time.Minute)
	writeNode(t, ctx, database, moved)

	var row string
	if err := database.Pool.QueryRow(ctx,
		`select visibility from graph_nodes where kind = $1 and id = $2`,
		node.Kind, node.ID).Scan(&row); err != nil {
		t.Fatalf("read the node row: %v", err)
	}
	if row != VisibilityAuthor {
		t.Fatalf("the row's visibility reads %q after the second write, want %q", row, VisibilityAuthor)
	}
	value, held, err := CurrentValue(ctx, database.Pool, node.Kind, node.ID, attributeVisibility)
	if err != nil {
		t.Fatalf("read the node's visibility out of the log: %v", err)
	}
	if !held || value != row {
		t.Errorf("the row's visibility is %q and the log's newest value is %q (held %t): the row is not the projection of its tuples",
			row, value, held)
	}
	history, err := History(ctx, database.Pool, node.Kind, node.ID, attributeVisibility)
	if err != nil {
		t.Fatalf("read the visibility history: %v", err)
	}
	if len(history) != 2 {
		t.Errorf("the log holds %d visibility tuples after two writes that each stated one, want 2", len(history))
	}
}

func writeNode(t *testing.T, ctx context.Context, database *store.Store, node Node) {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := Write(ctx, tx, []Node{node}, nil); err != nil {
		t.Fatalf("write the node: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
