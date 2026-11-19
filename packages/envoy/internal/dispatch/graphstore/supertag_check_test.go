package graphstore

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// seedSupertags writes a supertag node for each of names, the type a test's own kind of node
// carries: a node write takes a node only of a type the store holds (missingSupertagsSQL), and no
// migration seeds a type for a kind that only this package's tests write.
func seedSupertags(t *testing.T, ctx context.Context, database *store.Store, names ...string) {
	t.Helper()
	if _, err := database.Pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		select 'supertag', named.id, 'supertag', 'project', 'live', '{}', now(), '{"kind": "system", "id": "graphstore-test"}', 0
		from unnest($1::text[]) as named(id)
		on conflict (kind, id) do nothing`, names); err != nil {
		t.Fatalf("seed the supertags %v: %v", names, err)
	}
}

// TestAWriteTakesANodeOnlyOfATypeTheStoreHolds holds every node write to the types the store
// holds (dispatch://INFRA-1950/spec#b-r-lens-catalogue): a node whose supertag has no `supertag`
// row, or one that is gone, refuses the whole write with ErrSupertagNotHeld, a
// SupertagNotHeldError naming each such type, through Write and WriteUncaused alike. The refusal
// fails the write's batch and its caller abandons the transaction (Write), so once it rolls back
// nothing of the write is stored, the node of a held type written beside it included. Once the
// type's row is written, the same write is taken, with nothing restarted and nothing read again
// but the row: the store holds no copy of its types.
func TestAWriteTakesANodeOnlyOfATypeTheStoreHolds(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	const unheld, alsoUnheld = "graphstore_type_unheld", "graphstore_type_also_unheld"
	node := func(supertag, id string) Node {
		return Node{
			Kind: supertag, ID: id, Supertag: supertag, Visibility: VisibilityProject, State: StateLive,
			MadeAt: at, MadeBy: who, SourceEvent: NoEvent,
		}
	}
	held := node(blockKind, "TYPE/spec#held")
	typed := node(unheld, "typed")
	other := node(alsoUnheld, "other")
	writers := []struct {
		name  string
		write func(context.Context, pgx.Tx, []Node) error
	}{
		{"Write", func(ctx context.Context, tx pgx.Tx, nodes []Node) error {
			return Write(ctx, tx, nodes, nil)
		}},
		{"WriteUncaused", func(ctx context.Context, tx pgx.Tx, nodes []Node) error {
			write, err := WriteUncaused(ctx, tx, nodes, nil)
			if err != nil {
				return err
			}
			return Caused(ctx, tx, write, NoEvent)
		}},
	}
	refused := func(t *testing.T, nodes []Node, want ...string) {
		t.Helper()
		for _, writer := range writers {
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			err = writer.write(ctx, tx, nodes)
			requireSupertagNotHeld(t, err, want...)
			if err := tx.Rollback(ctx); err != nil {
				t.Fatalf("%s: roll back the transaction of the refused write: %v", writer.name, err)
			}
			for _, written := range nodes {
				requireNoNode(t, ctx, database, written.Kind, written.ID)
			}
		}
	}

	refused(t, []Node{held, typed}, unheld)
	refused(t, []Node{other, held, typed}, alsoUnheld, unheld)

	seedSupertags(t, ctx, database, unheld)
	refused(t, []Node{held, typed, other}, alsoUnheld)
	writeNode(t, ctx, database, typed)
	if stored, found, err := ReadNode(ctx, database.Pool, typed.Kind, typed.ID); err != nil || !found || stored.Supertag != unheld {
		t.Fatalf("the node of the type now held reads %+v (found %t, %v), want it written", stored, found, err)
	}

	// A type the store holds gone is no type it holds.
	if _, err := database.Pool.Exec(ctx, `update graph_nodes set state = $2 where kind = 'supertag' and id = $1`, unheld, StateGone); err != nil {
		t.Fatalf("mark the type gone: %v", err)
	}
	again := node(unheld, "again")
	refused(t, []Node{again}, unheld)
	if _, err := database.Pool.Exec(ctx, `update graph_nodes set state = $2 where kind = 'supertag' and id = $1`, unheld, StateLive); err != nil {
		t.Fatalf("mark the type live: %v", err)
	}
	writeNode(t, ctx, database, again)
}

// requireSupertagNotHeld fails unless err is the store's refusal of a write of nodes of types it
// does not hold: the sentinel a route answers 409 SUPERTAG_NOT_HELD for, and the typed refusal
// naming exactly want, in words.
func requireSupertagNotHeld(t *testing.T, err error, want ...string) {
	t.Helper()
	if !errors.Is(err, ErrSupertagNotHeld) {
		t.Fatalf("the write answered %v, want ErrSupertagNotHeld", err)
	}
	var refusal *SupertagNotHeldError
	if !errors.As(err, &refusal) {
		t.Fatalf("the refusal %v is no SupertagNotHeldError", err)
	}
	if !slices.Equal(refusal.Supertags, want) {
		t.Fatalf("the refusal names the types %v, want %v", refusal.Supertags, want)
	}
	for _, name := range want {
		if !strings.Contains(refusal.Error(), name) {
			t.Fatalf("the refusal %q does not name the type %s", refusal.Error(), name)
		}
	}
}
