// Package graphstore is the one place in the tree that writes `graph_nodes` and `graph_tuples`.
// A node is a thing Dispatch holds; a tuple is one fact stated about it at one moment, by one
// writer, caused by one event. Nothing here updates a tuple, and nothing deletes one but
// EraseNode, the erase's delete of a node with every tuple on both sides of it: a relation that
// ends is a second tuple with Asserted false, and an attribute that changes is a second tuple
// carrying the new value. Every table Dispatch reads today stays where it is and becomes the
// projection of these two (store/migrations/20261109013400_graph_store.up.sql).
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

// MaxAddressBytes bounds one address of the store: a node's id, and each end of a tuple that is
// an address rather than a value. graph_nodes keys a node by (kind, id) and graph_tuples_once,
// the repeat key of a relation, keys both of its ends at once (20261109013400_graph_store.up.sql),
// and a btree key is bounded at about 2,704 bytes on an 8 KiB page. Two ends of 1,024 bytes, the
// two kinds beside them and the source_event and asserted the key also carries stay well under
// that bound, so a relation whose both ends fit is always writable. A value is no address: its
// far end carries no such bound, because the value repeat key digests it with md5 and no read
// index keys it.
//
// A writer that meets an address past this bound writes no node and no tuple for it and stamps
// nothing on the row it projects, and the write it is part of goes on: the old view arm keeps
// answering that row, and no stream stops on it (a role name of 1,400 bytes once stopped an
// issue's outbox for good). The census counts each such address as a class of its own, not as a
// difference (internal/dispatch/graphcensus).
const MaxAddressBytes = 1024

// AddressFits is whether address is inside that bound, the one question every writer of this
// store asks of each node id and each relation end before it writes it.
func AddressFits(address string) bool { return len(address) <= MaxAddressBytes }

// Node is one thing the store holds. Its row is the projection of its own tuples: Write may
// write it again, and a second write moves every column but MadeAt and MadeBy, which stay as the
// row's first write left them, and Project, which never moves once a write has stated it
// (ErrProjectMoved).
//
// Project is the key of the project the node belongs to, and Parties the parties a node no
// project owns is between (PartyPerson, PartySession). Both are base attributes, stated by the
// write that stores the node and projected to graph_nodes.project_key and graph_nodes.parties;
// the empty string and nil state nothing, and leave what the row holds as it is. A node of a
// person, a session or the block schema carries neither.
type Node struct {
	Kind, ID, Supertag, Visibility, State string
	Project                               string
	Parties                               []string
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
