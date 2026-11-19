package graphstore

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheStoreRefusesAStateOrVisibilityOutsideItsValues holds every write of the store to the
// values the node state and the node visibility take (dispatch://INFRA-1950/spec#b-r-state-closed).
// A node, a value tuple its writer states and a claim at any other value are refused with
// ErrInvalidAttributeValue, as an InvalidAttributeValueError naming the attribute and the value,
// before the write's first statement: the transaction the refused write ran in still commits, and
// holds no node row and no tuple of that node, nor any tuple carrying the refused value. Each of
// the five states is written, and reads back from the row and from the log alike.
func TestTheStoreRefusesAStateOrVisibilityOutsideItsValues(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	const (
		misspelledState      = "propsed"
		misspelledVisibility = "public"
		stateRefusal         = `the node state "propsed" is none of live, gone, proposed, settled, overtaken`
		visibilityRefusal    = `the node visibility "public" is none of project, author`
	)

	if want := []string{"live", "gone", "proposed", "settled", "overtaken"}; !slices.Equal(NodeStates(), want) {
		t.Fatalf("NodeStates() = %v, want %v", NodeStates(), want)
	}

	misstated := func(node Node) Node {
		node.State = misspelledState
		return node
	}
	stateTuple := func(node Node, value string) Tuple {
		return Tuple{
			FromKind: node.Kind, FromID: node.ID, Kind: AttributeState, ToKind: KindValue, ToID: value,
			At: at, Who: who, SourceEvent: NoEvent, Asserted: true,
		}
	}
	runArms := func(ctx context.Context, tx pgx.Tx, nodes []Node, tuples []Tuple, claims []Claim) error {
		arms, params, err := WriteArms(1, nodes, tuples, claims)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "with "+arms+" select 1", params...)
		return err
	}

	for index, refused := range []struct {
		name      string
		attribute string
		value     string
		message   string
		write     func(context.Context, pgx.Tx, Node) error
	}{
		{"Write, a node at a state it does not take", AttributeState, misspelledState, stateRefusal,
			func(ctx context.Context, tx pgx.Tx, node Node) error {
				return Write(ctx, tx, []Node{misstated(node)}, nil)
			}},
		{"Write, a node at a visibility it does not take", AttributeVisibility, misspelledVisibility, visibilityRefusal,
			func(ctx context.Context, tx pgx.Tx, node Node) error {
				node.Visibility = misspelledVisibility
				return Write(ctx, tx, []Node{node}, nil)
			}},
		{"Write, a state tuple its writer states beside the node", AttributeState, misspelledState, stateRefusal,
			func(ctx context.Context, tx pgx.Tx, node Node) error {
				return Write(ctx, tx, []Node{node}, []Tuple{stateTuple(node, misspelledState)})
			}},
		{"Write, a state tuple its writer states with no node", AttributeState, misspelledState, stateRefusal,
			func(ctx context.Context, tx pgx.Tx, node Node) error {
				return Write(ctx, tx, nil, []Tuple{stateTuple(node, misspelledState)})
			}},
		{"WriteUncaused, a node at a state it does not take", AttributeState, misspelledState, stateRefusal,
			func(ctx context.Context, tx pgx.Tx, node Node) error {
				write, err := WriteUncaused(ctx, tx, []Node{misstated(node)}, nil)
				if err != nil {
					return err
				}
				return Caused(ctx, tx, write, NoEvent)
			}},
		{"Caused, a state tuple its writer holds for the cause", AttributeState, misspelledState, stateRefusal,
			func(ctx context.Context, tx pgx.Tx, node Node) error {
				return Caused(ctx, tx, Uncaused{Tuples: []UncausedTuple{{Tuple: stateTuple(node, misspelledState)}}}, NoEvent)
			}},
		{"WriteArms, a node at a state it does not take", AttributeState, misspelledState, stateRefusal,
			func(ctx context.Context, tx pgx.Tx, node Node) error {
				return runArms(ctx, tx, []Node{misstated(node)}, nil, nil)
			}},
		{"WriteArms, a state tuple its writer states", AttributeState, misspelledState, stateRefusal,
			func(ctx context.Context, tx pgx.Tx, node Node) error {
				return runArms(ctx, tx, []Node{node}, []Tuple{stateTuple(node, misspelledState)}, nil)
			}},
		{"WriteArms, a claim of a state it does not take", AttributeState, misspelledState, stateRefusal,
			func(ctx context.Context, tx pgx.Tx, node Node) error {
				return runArms(ctx, tx, nil, nil, []Claim{{
					FromKind: node.Kind, FromID: node.ID, Kind: AttributeState, ToKind: KindValue, ToID: misspelledState,
					At: at, Who: who, SourceEvent: NoEvent,
				}})
			}},
	} {
		t.Run(refused.name, func(t *testing.T) {
			node := IssueNode("REFUSED-"+string(rune('A'+index)), "CORE", who, at)
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx)
			err = refused.write(ctx, tx, node)
			requireValueRefused(t, err, refused.attribute, refused.value, refused.message)
			// A statement that failed would have aborted the transaction, and one that ran would
			// have stored what it wrote: the commit and the reads below see neither.
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("the transaction of the refused write does not commit, so the write sent a statement: %v", err)
			}
			requireNoNode(t, ctx, database, node.Kind, node.ID)
		})
	}
	var carrying int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from graph_tuples where to_kind = $1 and to_id = any($2)`,
		KindValue, []string{misspelledState, misspelledVisibility}).Scan(&carrying); err != nil {
		t.Fatalf("count the tuples carrying a refused value: %v", err)
	}
	if carrying != 0 {
		t.Errorf("%d tuples carry %q or %q, want none", carrying, misspelledState, misspelledVisibility)
	}

	t.Run("VisibilityFor, a visibility the store does not hold", func(t *testing.T) {
		visibility, err := VisibilityFor(misspelledVisibility, who)
		if visibility != "" {
			t.Errorf("VisibilityFor(%q) = %q, want none", misspelledVisibility, visibility)
		}
		requireValueRefused(t, err, AttributeVisibility, misspelledVisibility, visibilityRefusal)
	})

	for _, state := range NodeStates() {
		t.Run("a node written "+state, func(t *testing.T) {
			node := IssueNode("STATE-"+state, "CORE", who, at)
			node.State = state
			writeNode(t, ctx, database, node)
			held, found, err := ReadNode(ctx, database.Pool, node.Kind, node.ID)
			if err != nil || !found || held.State != state {
				t.Errorf("the row of a node written %s reads %q (found %t, %v)", state, held.State, found, err)
			}
			logged, stated, err := CurrentValue(ctx, database.Pool, node.Kind, node.ID, AttributeState)
			if err != nil || !stated || logged != state {
				t.Errorf("the log of a node written %s reads %q (stated %t, %v)", state, logged, stated, err)
			}
		})
	}
}

// requireValueRefused fails unless err is the store's refusal of value for attribute: the sentinel
// a route answers 400 INVALID_ATTRIBUTE_VALUE for, and the typed refusal naming both, in words.
func requireValueRefused(t *testing.T, err error, attribute, value, message string) {
	t.Helper()
	if !errors.Is(err, ErrInvalidAttributeValue) {
		t.Fatalf("the write answered %v, want ErrInvalidAttributeValue", err)
	}
	var refusal *InvalidAttributeValueError
	if !errors.As(err, &refusal) {
		t.Fatalf("the write answered %v, which is no InvalidAttributeValueError", err)
	}
	if refusal.Attribute != attribute || refusal.Value != value {
		t.Errorf("the refusal names %s %q, want %s %q", refusal.Attribute, refusal.Value, attribute, value)
	}
	if refusal.Error() != message {
		t.Errorf("the refusal reads %q, want %q", refusal.Error(), message)
	}
}

// requireNoNode fails unless the store holds no row of the node and no tuple out of it or into it.
func requireNoNode(t *testing.T, ctx context.Context, database *store.Store, kind, id string) {
	t.Helper()
	var rows, tuples int
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from graph_nodes where kind = $1 and id = $2),
		       (select count(*) from graph_tuples
		        where (from_kind = $1 and from_id = $2) or (to_kind = $1 and to_id = $2))`,
		kind, id).Scan(&rows, &tuples); err != nil {
		t.Fatalf("read what the store holds of %s/%s: %v", kind, id, err)
	}
	if rows != 0 || tuples != 0 {
		t.Errorf("the store holds %d rows and %d tuples of %s/%s after its write was refused, want none", rows, tuples, kind, id)
	}
}
