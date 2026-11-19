package graphstore

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A node row is the projection of its own tuples (R1 of this change, and this
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

// authorOnlyTestKind is a node kind this package's tests admit as one that takes a node reserved
// for its author, through the hand-off the read registry's init calls (AdmitAuthorOnly). The
// store takes such a node of no kind the registry does not flag, and this package's tests load no
// registry, so a test that proves how the writer moves a node's visibility writes a node of this
// kind rather than of a kind the store refuses `author` on.
const authorOnlyTestKind = "graphstore_author_only_test"

func init() { AdmitAuthorOnly(authorOnlyTestKind) }

func TestABaseAttributeThatMovesIsStatedInTheLog(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedSupertags(t, ctx, database, authorOnlyTestKind)
	at := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	node := Node{
		Kind: authorOnlyTestKind, ID: "base-attribute-move", Supertag: authorOnlyTestKind,
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
	value, held, err := CurrentValue(ctx, database.Pool, node.Kind, node.ID, AttributeVisibility)
	if err != nil {
		t.Fatalf("read the node's visibility out of the log: %v", err)
	}
	if !held || value != row {
		t.Errorf("the row's visibility is %q and the log's newest value is %q (held %t): the row is not the projection of its tuples",
			row, value, held)
	}
	history, err := History(ctx, database.Pool, node.Kind, node.ID, AttributeVisibility)
	if err != nil {
		t.Fatalf("read the visibility history: %v", err)
	}
	if len(history) != 2 {
		t.Errorf("the log holds %d visibility tuples after two writes that each stated one, want 2", len(history))
	}
}

// TestAWriteThatWaitedOnANodeStatesTheStateItMoved interleaves two writers of one node's state.
// The first writes it gone and stays open; the second writes it live and queues behind the first's
// row; the first commits, and then the second. The second moved the row from the gone the first
// committed back to live, so the row reads live and the newest state tuple must read live too. A
// write that compared against the row as its statement's snapshot held it - live, from before the
// first writer committed - saw nothing move and stated nothing, and left the row live under a log
// whose newest state is gone.
func TestAWriteThatWaitedOnANodeStatesTheStateItMoved(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	node := Node{
		Kind: KindPerson, ID: "state-interleaved", Supertag: "person",
		Visibility: VisibilityProject, State: StateLive,
		MadeAt: at, MadeBy: who, SourceEvent: NoEvent,
	}
	writeNode(t, ctx, database, node)

	gone := node
	gone.State, gone.MadeAt = StateGone, at.Add(time.Minute)
	first, release := beginHolder(t, ctx, database)
	if err := Write(ctx, first, []Node{gone}, nil); err != nil {
		t.Fatalf("write the node gone: %v", err)
	}
	live := node
	live.MadeAt = at.Add(2 * time.Minute)
	conn, _, done := startPeopleWrite(t, ctx, database, func(ctx context.Context, tx pgx.Tx) error {
		return Write(ctx, tx, []Node{live}, nil)
	})
	requireQueued(t, first, conn, done, "the write of the node live while another transaction writes it gone")
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the write of the node gone: %v", err)
	}
	release()
	requireCommitted(t, done, "the write of the node live after the write of it gone committed")

	var row string
	if err := database.Pool.QueryRow(ctx,
		`select state from graph_nodes where kind = $1 and id = $2`, node.Kind, node.ID).Scan(&row); err != nil {
		t.Fatalf("read the node row: %v", err)
	}
	if row != StateLive {
		t.Fatalf("the row's state reads %q after the second write committed, want %q", row, StateLive)
	}
	value, held, err := CurrentValue(ctx, database.Pool, node.Kind, node.ID, AttributeState)
	if err != nil {
		t.Fatalf("read the node's state out of the log: %v", err)
	}
	if !held || value != row {
		t.Errorf("the row's state is %q and the log's newest state is %q (held %t): the second write compared against the row as it stood before the first committed",
			row, value, held)
	}
	history, err := History(ctx, database.Pool, node.Kind, node.ID, AttributeState)
	if err != nil {
		t.Fatalf("read the state history: %v", err)
	}
	states := make([]string, len(history))
	for index, tuple := range history {
		states[index] = tuple.ToID
	}
	if want := []string{StateLive, StateGone, StateLive}; !slices.Equal(states, want) {
		t.Errorf("the state history reads %v, want %v: one tuple for each write that moved the state", states, want)
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
