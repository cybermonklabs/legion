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
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Visibility and state values, the two base attributes every node carries. A node is visible to
// its project or to its author alone. Its state is live, which every node a write makes starts
// in, gone, which a node no longer held is stated, or one of proposed, settled and overtaken,
// which a request's write of the node's state marks it with (api's writeNodeState). Each
// attribute takes these values and no other: a write stating any other is refused
// (InvalidAttributeValueError), since a node at a value nobody declared is drawn by no view and
// counted by no census.
const (
	VisibilityProject = "project"
	VisibilityAuthor  = "author"
	StateLive         = "live"
	StateGone         = "gone"
	StateProposed     = "proposed"
	StateSettled      = "settled"
	StateOvertaken    = "overtaken"
)

// nodeStates and visibilities are the values each of the two attributes takes, held once:
// NodeStates answers the first, and CheckAttributeValue holds a write to both.
var (
	nodeStates   = [...]string{StateLive, StateGone, StateProposed, StateSettled, StateOvertaken}
	visibilities = [...]string{VisibilityProject, VisibilityAuthor}
)

// NodeStates is every value the node state takes, a fresh slice for each caller.
func NodeStates() []string { return slices.Clone(nodeStates[:]) }

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
// row's first write left them, Project, which never moves once a write has stated it
// (ErrProjectMoved), and the state and the visibility a write that Restates the node keeps.
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
	// Restates says the write restates a node whose state and visibility its writer does not
	// own: the writer writes the node again for what it does own - a block's text, a comment's
	// anchor, a document's address - and says of the state only whether the node is there
	// (StateLive) or not (StateGone). Over a node the store holds, such a write keeps the stored
	// visibility, and keeps a stored state of live, proposed, settled or overtaken, since a
	// write that marked the node so is not this writer's to undo: it writes its state only over
	// a node stored gone, which comes back live, and writes gone whenever it says gone. A node
	// the store does not hold is made as the writer states it. Every writer that writes a node
	// again for facts of its own restates it: the block index (refs.IndexBlocks), CommentFacts,
	// IssueNode, ArtifactNode, AskNode and MessageNode, the backfill
	// (internal/dispatch/graphcensus), and the seed (cmd/dispatch-seed), which writes its
	// author's node and its followers' session nodes again in later batches. A write that means
	// to move a node's state or visibility leaves it false. WriteArms reads none of it: its
	// upsert moves nothing on a node already there.
	Restates bool
	// KeepsCause says the write keeps the cause the store holds of a node already there, its
	// row's source_event, whatever else of the row it moves. It is for a write that moves only a
	// base attribute of a node another write caused: a request's write of a node's state (api's
	// writeNodeState), at NoEvent, through Write. That write is no cause of the node's other
	// facts, and a row it put at the sentinel would take the event of the next write QueueCaused
	// moves onto it - the next index pass of a block's document, for a version that changed
	// nothing of the block. The base attributes it states still carry its own SourceEvent, so one
	// at NoEvent states each at the sentinel, which no repeat key drops: proposed, live and
	// proposed again are three tuples; they are stated by a statement of their own before the
	// move, since such a row carries no mark (queueKeptCauses). A node the store does not hold is
	// made at the write's own SourceEvent, since it has no cause to keep.
	KeepsCause bool
	// Changed marks a node this write created or changed, which a write made for a run names as
	// changed by it (WithRun): QueueWrite and QueueCaused, and so Write and Caused, on a context
	// holding a run, append one TupleChangedByRun from it to that run's interaction. Its writer
	// sets it, because the store cannot infer it: a block's words are no column of its node, so
	// an edit of them moves nothing a node write compares. A node a write only names - an
	// author's person or session node, a block row a pass gives the node it lacked - is never
	// marked. No read answers it.
	Changed bool
	// ChangedAt and ChangedBy date and sign the changed_by_run a node marked Changed owes, for a
	// writer that changes a node it writes as another write made it: the issue PATCH (api's
	// patchIssue) writes the issue's node as the create and the backfill write it, made at the
	// issue's created_at by its creator, while its tuple says that the PATCH changed the issue,
	// then and by its writer, and the edit of an ask's or a comment's words (api's editAsk and
	// editComment) writes that node the same way, dated at the edit's edited_at. A writer sets
	// both or neither: the zero ChangedAt leaves the tuple dated and signed as the node is, MadeAt
	// and MadeBy, which is every other writer, since the zero actor names a writer of its own (the
	// deployment's shared token) and so cannot say that ChangedBy is unset. No column holds
	// either, and no read answers them.
	ChangedAt time.Time
	ChangedBy model.Actor
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
	// Changes marks a fact that changes the node it goes out of - a pointer a request appends,
	// retracts or moves - so a write made for a run names that node as changed by it, as
	// Node.Changed names a node the write itself writes. No read answers it.
	Changes bool
}

// Queryer is the query surface shared by a pool and a transaction, as internal/dispatch/refs has
// it.
type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
