// Package prioritystore is the one place in the tree that writes `priority_nodes` and
// `priority_tuples`. A priority is a node the whole deployment holds, belonging to no project; a
// tuple is one fact stated about it at one moment, by one writer. Nothing here updates a tuple
// and nothing deletes one: a relation that ends is a second tuple with Asserted false, and an
// attribute that changes is a second tuple carrying the new value. The node row is the projection
// of its own tuples, and one transaction moves both
// (store/migrations/20261112024320_priority_nodes.up.sql).
//
// The vocabulary is the node and tuple store's (internal/dispatch/graphstore on
// legion/INFRA-1149), so these rows copy into that store's tables column for column the day it
// lands: a state is StateLive or StateGone, a visibility is VisibilityProject, an attribute is a
// tuple whose ToKind is KindValue and whose ToID is the value itself, and a write no event
// recorded carries NoEvent.
//
// A caller that wants the current state asks CurrentOut, CurrentIn or CurrentValue, one that
// wants how a fact got here asks History, which serves an attribute and a relation alike, and one
// that writes asks Write or the named writes above it (Create, SetAttributes, SetState, Move,
// Advance, Retract). Read, List, IssuesAdvancing, PrioritiesAdvanced and Timeline are those five
// readings bound to the priority vocabulary, for the routes and the page.
package prioritystore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The kind and the supertag a priority node carries, both the one word, and the kind of the node
// at the near end of an `advances` relation, which is an issue of any project.
const (
	KindPriority = "priority"
	KindIssue    = "issue"
)

// KindValue is the ToKind of an attribute tuple: its ToID holds the value itself rather than an
// address.
const KindValue = "value"

// RelationAdvances is the one relation this store holds: work in any project advances a priority.
// It is stated on the issue, so its far end is the priority.
const RelationAdvances = "advances"

// Visibility and state values, the two base attributes every node carries. A priority is visible
// to everyone who can see the deployment, which is what VisibilityProject means for a node that
// belongs to no project, and DELETE writes StateGone rather than removing a row.
const (
	VisibilityProject = "project"
	StateLive         = "live"
	StateGone         = "gone"
)

// The attributes a priority carries. The first four are what a person writes; the last two are
// the base attributes the node row carries a column for, stated as tuples so the history holds
// the moment each moved.
const (
	AttributeTitle       = "title"
	AttributeDescription = "description"
	AttributeOwner       = "owner"
	AttributeRank        = "rank"
	AttributeVisibility  = "visibility"
	AttributeState       = "state"
)

// NoEvent is the SourceEvent of a write no event recorded, which is every write of this store:
// `events` takes one owner, an issue or a project, and a priority is neither, so a priority write
// appends no event row. `events.id` is a bigserial, so no event ever carries 0.
const NoEvent int64 = 0

// The refusals a caller maps to an answer of its own.
var (
	// ErrNoPriority is a write or a read of an id no node row carries.
	ErrNoPriority = errors.New("no priority of that id")
	// ErrPriorityExists is a Create of an id a node row already carries.
	ErrPriorityExists = errors.New("a priority of that id is already written")
	// ErrRankNeighbor is a move whose before or after names this priority, names none, or whose
	// two neighbours are not in that order.
	ErrRankNeighbor = errors.New("rank neighbor")
)

// Node is one priority. Its row is the projection of its own tuples: Attributes holds the current
// title, description, owner and order, and MadeAt and MadeBy stay as the row's first write left
// them however often it is written again.
type Node struct {
	Kind, ID, Supertag, Visibility, State string
	Attributes                            map[string]any
	MadeAt                                time.Time
	MadeBy                                model.Actor
	SourceEvent                           int64
}

// Attribute answers one of the node row's attributes as the text it was written as, and whether
// the row carries it. A caller reads the current title, description, owner or order here rather
// than through CurrentValue, which walks the log for the same answer.
func (n Node) Attribute(name string) (string, bool) {
	held, carried := n.Attributes[name]
	if !carried {
		return "", false
	}
	text, isText := held.(string)
	return text, isText
}

// Tuple is one fact stated about a priority. ToKind KindValue makes it an attribute, whose ToID
// is the value itself; any other ToKind makes it a relation, whose ToKind and ToID address the
// far end.
type Tuple struct {
	Seq              int64 // read-only: the log position a read answers
	FromKind, FromID string
	Kind             string
	ToKind, ToID     string
	At               time.Time
	Who              model.Actor
	SourceEvent      int64
	Asserted         bool
}

// Queryer is the query surface shared by a pool and a transaction, as internal/dispatch/refs has
// it. A write takes a pgx.Tx instead, because it moves the node row and appends its tuples
// together.
type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
