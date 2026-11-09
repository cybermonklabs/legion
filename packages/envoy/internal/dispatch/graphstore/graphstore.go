// Package graphstore is the one place in the tree that writes `graph_nodes` and `graph_tuples`.
// A node is a thing Dispatch holds; a tuple is one fact stated about it at one moment, by one
// writer, caused by one event. Nothing here updates a tuple and nothing deletes one: a relation
// that ends is a second tuple with Asserted false, and an attribute that changes is a second
// tuple carrying the new value. Every table Dispatch reads today stays where it is and becomes
// the projection of these two (store/migrations/20261109013400_graph_store.up.sql).
//
// A read answers the tuples alone, never the projection: a caller that wants the current state
// asks CurrentOut, CurrentIn or CurrentValue, one that wants the state at a moment asks AsOfOut
// or AsOfIn, and one that wants how a fact got here asks History, which serves an attribute and
// a relation alike - a caller cannot tell from the answer's shape which of the two it asked for.
package graphstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Visibility and state values, the two base attributes every node carries.
const (
	VisibilityProject = "project"
	VisibilityAuthor  = "author"
	StateLive         = "live"
	StateGone         = "gone"
)

// KindValue is the to_kind of an attribute tuple: its ToID holds the value itself rather than
// an address.
const KindValue = "value"

// NoEvent is the source_event of a write no event recorded. `events.id` is a bigserial, so no
// event ever carries 0, and both repeat keys are partial on `source_event <> 0`: a tuple at this
// sentinel is appended unconditionally, since there is no event to state a fact twice.
const NoEvent int64 = 0

// Node is one thing the store holds. Its row is the projection of its own tuples: Write may
// write it again, and a second write moves every column but MadeAt and MadeBy, which stay as the
// row's first write left them.
type Node struct {
	Kind, ID, Supertag, Visibility, State string
	Attributes                            map[string]any
	MadeAt                                time.Time
	MadeBy                                model.Actor
	SourceEvent                           int64
}

// Tuple is one fact stated about a node. ToKind KindValue makes it an attribute, whose ToID is
// the value itself; any other ToKind makes it a relation, whose ToKind and ToID address the far
// end. LegacySourceSeq is the `source_seq` of the row a projection's own write came from, and is
// nil for a tuple no legacy row backs.
type Tuple struct {
	Seq              int64 // read-only: the log position a read answers
	FromKind, FromID string
	Kind             string
	ToKind, ToID     string
	At               time.Time
	Who              model.Actor
	SourceEvent      int64
	LegacySourceSeq  *int64
	Asserted         bool
}

// Queryer is the query surface shared by a pool and a transaction, as internal/dispatch/refs has
// it.
type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
