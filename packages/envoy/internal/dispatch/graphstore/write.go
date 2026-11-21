package graphstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Every writer of this store puts its statements on a write phase's batch (store.WriteBatch) and
// runs nothing itself: a route queues them on its ledger beside the rows they are the facts of,
// so they run in its write phase, back to back, in the order it made its writes, and a writer
// with no write phase of its own runs them at once through store.RunWrite. No statement's result
// is read in Go before a later statement is sent, so each value a later statement needs from an
// earlier one is derived in the later statement's SQL: an event's id (LatestEvent), which nodes
// a node write made or moved, which the node rows themselves carry until the statement that
// states those facts reads them (nodeMarks), and whether a node already stands changed by the
// run the batch is made for (queueChangedByRun).
//
// A rule a write is held to is decided in Go as the write is queued wherever the write itself
// holds all it needs - a state or a visibility outside the values the attribute takes, a node
// reserved for its author of a kind that takes none - and such a refusal queues nothing of the
// write but itself (queueRefusal), so Send answers it before it sends any statement of the write,
// as it answers a node or a writer that cannot be encoded (store.WriteBatch's QueueBuilt). A rule
// that reads the store - the types the store holds (queueSupertagCheck), the project a node
// already stands in (queueNodes), the type rules of the pointer tuples and the visibility of the
// nodes a part_of places (pointerCheck) - is a statement of the write phase, queued where it
// reads what the rule needs, whose callback answers the typed refusal as Send reads its result.
// The statements queued after it have run by then, and the refusal ends the caller's
// transaction, which rolls them back: a caller abandons the transaction of a write that failed,
// whatever the failure.

// queueRefusal queues on b the refusal of a write a rule refused as it was queued, in place of
// every statement of that write: one statement Send never sends, whose arguments' build answers
// err, so Send sends the statements queued before it and then answers err as it stands, and
// nothing of the refused write reaches the store (store.WriteBatch's QueueBuilt).
func queueRefusal(b *store.WriteBatch, err error) {
	b.QueueBuilt("", `select 1`, func() ([]any, error) { return nil, err })
}

// LatestEvent is the cause of a write whose event its write phase appends right before the
// write's own statements: no Go code reads that event's id before the batch is sent, so the
// statements name it as the events id sequence left it (store.LatestEventID), and nothing between
// the event's insert and them may append another event. A node or a tuple given it as its
// SourceEvent, or as its LegacySourceSeq, is written with that id.
const LatestEvent int64 = -1

// causeOf is the SQL a source_event or legacy_source_seq column is written through: the column's
// own value, or the latest event's id for LatestEvent.
func causeOf(column string) string {
	return `(case when ` + column + ` = ` + strconv.FormatInt(LatestEvent, 10) + ` then ` + store.LatestEventID + ` else ` + column + ` end)`
}

// Write writes every node and appends every tuple inside the caller's transaction, at once: its
// statements as one batch of their own (QueueWrite), built for ctx, so a write on a context that
// carries a run of the command bar (store.WithRun) is made for that run. It is for a writer with
// no write phase to queue them on: a backfill, a seed, a delivery's settle, a request's write of
// the reasoning nodes.
func Write(ctx context.Context, tx pgx.Tx, nodes []Node, tuples []Tuple) error {
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		QueueWrite(b, nodes, tuples)
		return nil
	})
}

// QueueWrite queues the write of every node and the append of every tuple on b. It appends only:
// no statement here updates or deletes a tuple row, so the log a read walks is whatever every
// writer before it appended, in the order they appended it.
//
// Each node is written once per call: two rows of nodes carrying one (kind, id) are the caller's
// mistake, and Postgres names it rather than this picking a winner.
//
// The base attributes are stated here and not by each writer, so that a node written through any
// writer of this store carries them: the block index, the comment path, the backfill and the seed
// all reach the store through this call. queueNodes says which nodes they are stated for, and
// they are appended with the caller's own tuples.
//
// This is the whole of the write for a caller that has its cause in hand, or none at all: a
// backfill, a seed, a publisher, or a request whose event its write phase appends just before
// (LatestEvent). A caller whose event is appended after the node row - the comment path and the
// block index, whose projection rows carry a foreign key into graph_nodes and must therefore
// follow the node - holds the write for its event instead (PlanUncaused, QueueNodes,
// QueueCaused), so that the base attributes are appended under that event rather than under the
// sentinel.
//
// A write stating a node's state or visibility at a value that attribute does not take - on a
// node, or in a value tuple of tuples - is refused with an InvalidAttributeValueError, and a node
// reserved for its author whose kind takes no such node with a VisibilityNotAdmittedError
// (checkBaseValues), as it is queued, so it queues its refusal alone (queueRefusal) and stores
// nothing. That holds for a write with no node too.
//
// Every relation the write appends whose kind the pointer type rules hold (pointerRules) is held
// to its rule, and every part_of it places at a kind that takes a node reserved for its author to
// the visibility of both its ends (pointerCheck), between its node write and its append: in Go as
// the write is queued where the write itself holds all the rule asks, and otherwise in statements
// of its own that read what the rule asks once the nodes are written. Beside those reads, each
// lens a part_of of the write places records the artifacts.id of the row at the address of the
// document it places it on, or retracts the one it held where the write places it on no document
// (queueLensArtifactIDs).
//
// On a batch made for a run (store.WriteBatch's Run), the append also states one changed_by_run
// tuple from each node the write marks changed (Node.Changed, Tuple.Changes) to that run's
// interaction, dated, signed and caused as the node's write or the fact that marked it, unless the
// node already stands changed by that run (queueChangedByRun), read once the node rows are held.
//
// Its statements build their arguments when the write phase sends them (store.WriteBatch's
// QueueBuilt), so a node or a writer that cannot be encoded fails Send rather than this call,
// which therefore answers nothing, as a refusal of a rule fails Send too.
func QueueWrite(b *store.WriteBatch, nodes []Node, tuples []Tuple) {
	if err := checkBaseValues(nodes, tuples, nil); err != nil {
		queueRefusal(b, err)
		return
	}
	check, err := planPointerCheck(nodes, tuples, true)
	if err != nil {
		queueRefusal(b, err)
		return
	}
	mark := int64(0)
	if len(nodes) > 0 {
		mark = nextNodeMark()
	}
	queueNodes(b, nodes, mark, len(tuples), func(index int) Tuple { return tuples[index] })
	check.queue(b)
	queueLensArtifactIDs(b, tuples)
	queueAppend(b, appending{
		tuples: len(tuples), tuple: func(index int) Tuple { return tuples[index] }, nodes: nodes, mark: mark,
		run: b.Run(), changes: changedByRunMarks(b.Run(), nodes, nil, tuples),
	})
}

// Uncaused is a write of this store whose cause did not exist while its node write was queued,
// held by its writer until it does. Such a writer orders itself node row, projection row, event,
// tuples: the node first because the projection row's foreign key names it, the event after both
// because it records what the request did, and the tuples last because each one holds the event
// that caused it and no tuple may be updated to learn it afterwards. So its nodes go in at the
// sentinel, marked with what the node write did to each (nodeMarks), and QueueCaused, queued after
// the event in the same transaction, moves them onto it and appends every fact of the write under
// it.
//
// It is the one shape of that write for every writer it applies to: a comment's request (api's
// commentStoreWrite), a version's block index (refs.BlockIndexing), a follow of an ask
// (asks.Followed) and the node a write made for a run names changed (PlanChangedForRun). A writer
// with nothing to state holds the zero Uncaused, for which Caused writes nothing.
type Uncaused struct {
	// Nodes are the nodes the write wrote. QueueCaused moves each its node write made or moved
	// onto the event, and, for a write of PlanUncaused, each still at the sentinel, so a node an
	// earlier write caused keeps its cause.
	Nodes []Node
	// Tuples are the facts the write states, each appended under the event by Caused, the base
	// attributes its node write stated among them, which stand after the first baseAt of these.
	Tuples []UncausedTuple
	// changes are the nodes PlanChangedForRun names changed by the run, each dated and signed as
	// its change was made, for which QueueCaused appends a changed_by_run under the event.
	changes []runChange
	// mark is what the node write left on the rows it made or moved (nodeMarks), 0 for a write
	// that marked none.
	mark int64
	// baseAt is how many of Tuples the base attributes the node write states follow.
	baseAt int
	// sentinel says QueueCaused also moves onto the event every node of Nodes still at the
	// sentinel, as PlanUncaused's writer holds all of them for its cause; a follow's node (the
	// arms) is caused only where that write made it.
	sentinel bool
	// appended, where Counting set it, counts the tuples the append appends (queueAppend).
	appended *int64
}

// Counting answers write with its append counting into appended the tuples it appends once
// QueueCaused runs: the caller's own and the base attributes its node write stated, each once, and
// none a repeat key drops. It is for a writer that reports what it stated, as the backfill's
// object pass does, so the count is the store's own and no caller derives which attributes moved.
func Counting(write Uncaused, appended *int64) Uncaused {
	write.appended = appended
	return write
}

// UncausedTuple is one fact an Uncaused write holds for its cause. LegacyFromCause marks a fact
// whose projection row takes the cause's own id as its source_seq: a block_refs row the block
// index inserts outside a first indexing is written with none, and refs.QueueStamp or docs'
// stampVersionEvent fills it with that event in the same transaction, so the tuple's
// legacy_source_seq is that event's id, which the writer cannot read back once the tuple is
// appended. A fact no row dates that way keeps the legacy_source_seq its writer gave it.
type UncausedTuple struct {
	Tuple
	LegacyFromCause bool
}

// PlanUncaused is a write whose nodes are written before the event that causes it exists, held
// for that event, and planned before anything is queued: the nodes, at the sentinel, the writer's
// own facts, stated, and the mark its node write will leave on the rows it makes or moves
// (nodeMarks), from which the statement QueueCaused queues derives the base attributes that node
// write states, after the writer's own. It writes nothing: the writer queues the node write where
// it makes it (QueueNodes), before the projection row that names the node, and QueueCaused after
// its event, and it holds the write between the two, in its read phase if it has one, so both
// steps are known before the write phase runs. The base attributes are facts of that same write,
// so they go in with the caller's own under the same event, which is what keeps a block a version
// stated gone carrying the event that deleted it. stated is read for one more thing besides: a
// base attribute the caller states itself is not stated twice.
func PlanUncaused(nodes []Node, stated []Tuple) Uncaused {
	write := Uncaused{baseAt: len(stated), sentinel: true}
	if len(nodes) > 0 {
		for index := range nodes {
			nodes[index].SourceEvent = NoEvent
		}
		write.Nodes = nodes
		write.mark = nextNodeMark()
	}
	if len(stated) > 0 {
		write.Tuples = make([]UncausedTuple, 0, len(stated))
	}
	for _, tuple := range stated {
		write.Tuples = append(write.Tuples, UncausedTuple{Tuple: tuple})
	}
	return write
}

// QueueNodes queues the node write of write (PlanUncaused) on b: every node now, at the sentinel,
// each it makes or moves left marked for the append QueueCaused queues. It appends nothing. The
// nodes, and the tuples read for a base attribute the writer states itself, are read when the
// write phase sends the statements, so the writer does not change them after this call, and a
// node that cannot be encoded fails Send rather than this call, which answers nothing
// (QueueWrite). A node or a held tuple at a state or a visibility that attribute does not take,
// and a node reserved for its author of a kind that takes none, are refused as the write is
// queued (checkBaseValues, checkHeldValues), which queues the refusal alone (queueRefusal).
func QueueNodes(b *store.WriteBatch, write Uncaused) {
	if err := checkBaseValues(write.Nodes, nil, nil); err != nil {
		queueRefusal(b, err)
		return
	}
	if err := checkHeldValues(write.Tuples); err != nil {
		queueRefusal(b, err)
		return
	}
	queueNodes(b, write.Nodes, write.mark, len(write.Tuples), func(index int) Tuple {
		return write.Tuples[index].Tuple
	})
}

// checkHeldValues is checkBaseValues of the tuples a write held for its cause holds, each read
// where it stands rather than copied: a version's block index holds a tuple a block.
func checkHeldValues(held []UncausedTuple) error {
	for index := range held {
		if tuple := &held[index].Tuple; tuple.ToKind == KindValue {
			if err := CheckAttributeValue(tuple.Kind, tuple.ToID); err != nil {
				return err
			}
		}
	}
	return nil
}

// heldTuplesRead is whether the check of a write held for its cause, or the run its batch is made
// for, reads any tuple the write holds (QueueCaused): a relation the pointer type rules hold, a
// part_of at a kind that takes a node reserved for its author or out of a lens, or a fact marked
// as changing its node.
func heldTuplesRead(held []UncausedTuple) bool {
	for index := range held {
		tuple := &held[index].Tuple
		if tuple.Changes || tuple.ToKind != KindValue && (pointerRuleOf(tuple.Kind, tuple.FromKind) != nil ||
			placesUnderAuthorOnly(tuple.Kind, tuple.ToKind, tuple.ToID) || placesLens(tuple.Kind, tuple.FromKind)) {
			return true
		}
	}
	return false
}

// Caused is QueueCaused run at once, as a batch of its own built for ctx, for a writer with no
// write phase. Its error is the batch's: a statement that fails, a tuple that cannot be encoded,
// or a refusal of a rule fails Send.
func Caused(ctx context.Context, tx pgx.Tx, write Uncaused, event int64) error {
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		QueueCaused(b, write, event)
		return nil
	})
}

// QueueCaused queues the append of what write stated under event, the event that caused it, and
// the move of the nodes it wrote onto that event: a node row is the projection of its tuples and
// may be written again, while a tuple may not be updated, which is why the tuples waited for this
// call rather than being stamped after it. event is NoEvent where no event recorded the write, and
// such a tuple carries the sentinel and no legacy_source_seq, as its row carries no source_seq
// either; it is LatestEvent for an event the write phase appended right before this.
//
// It is a function of this package and not a method of Uncaused, so every call of it outside the
// package is a call of the store's writer by its package's name, which the register of this
// store's writers finds and holds to its lock steps (TestEveryStoreWriteCallSiteIsListed).
//
// The nodes are not written again: the statement moves the event onto the rows already written,
// over their addresses, as it reads what the node write left on each. Writing them again would pay
// a second node write per node for one column, and the upsert's own guard would refuse it, since
// nothing else about the node moved.
//
// Each tuple is stated under event as the append binds it, read from write where it stands when
// the write phase sends the statements, rather than copied here: a version's block index holds a
// tuple a block. The writer does not change write after this call, and a tuple that cannot be
// encoded fails Send rather than this call, which answers nothing (QueueWrite).
//
// A held tuple that states a node's state or visibility at a value that attribute does not take
// is refused (checkHeldValues) as the write is queued, which queues the refusal alone
// (queueRefusal): a writer appends facts of its own to what PlanUncaused answered (refs'
// BlockIndexing does), and those reach the store through here alone. For the same reason a held
// relation whose kind the pointer type rules hold is held to its rule here, and a held part_of to
// the visibility of its ends (pointerCheck), every end of either read from the store, before the
// append. Those checks, and the run, read the held tuples each stated under event, which are
// copied for them only where one of them reads any (heldTuplesRead), so a version's block index
// made for no run copies none.
//
// On a batch made for a run (store.WriteBatch's Run), it appends with them one changed_by_run
// tuple from each node the write marks changed (Node.Changed, Tuple.Changes) or names changed
// (PlanChangedForRun) to that run's interaction, under event as every fact of the write is, unless
// the node already stands changed by that run. The run is the batch's, not the write's: the node
// write appends nothing, so a writer whose nodes went in on another batch still names the run its
// cause is appended for.
func QueueCaused(b *store.WriteBatch, write Uncaused, event int64) {
	if err := checkHeldValues(write.Tuples); err != nil {
		queueRefusal(b, err)
		return
	}
	legacy := event
	stated := func(index int) Tuple {
		held := write.Tuples[index]
		tuple := held.Tuple
		tuple.SourceEvent = event
		if held.LegacyFromCause && tuple.LegacySourceSeq == nil && event != NoEvent {
			tuple.LegacySourceSeq = &legacy
		}
		return tuple
	}
	var tuples []Tuple
	if heldTuplesRead(write.Tuples) {
		tuples = make([]Tuple, len(write.Tuples))
		for index := range tuples {
			tuples[index] = stated(index)
		}
	}
	check, err := planPointerCheck(write.Nodes, tuples, false)
	if err != nil {
		queueRefusal(b, err)
		return
	}
	check.queue(b)
	queueLensArtifactIDs(b, tuples)
	changes := changedByRunMarks(b.Run(), write.Nodes, write.changes, tuples)
	for index := range changes {
		changes[index].SourceEvent = event
	}
	queueAppend(b, appending{
		tuples: len(write.Tuples), tuple: stated,
		nodes: write.Nodes, mark: write.mark, baseAt: write.baseAt,
		cause: &event, sentinel: write.sentinel, appended: write.appended,
		run: b.Run(), changes: changes,
	})
}

// The base attributes, as the kind of the tuple that states each spells it. graph_nodes carries a
// column for each, which is that attribute's projection, and nodeArm states the same four for a
// node the publisher's record makes. Every node carries visibility and state; a node a project
// owns carries project, and one no project owns but parties do carries parties (Node).
const (
	AttributeVisibility = "visibility"
	AttributeState      = "state"
	AttributeProject    = "project"
	AttributeParties    = "parties"
)

// baseAttributes is the set IsBaseAttribute reads, held once. No other attribute's values may be
// stored under one of these kinds: a value tuple of one of them out of a node reads as that node's
// own visibility, state, project or parties, whoever wrote it, so an attribute a type declares
// under one of these names is stored under another name (the store name the supertag seed writes,
// migration 20261109051232_graph_supertags). Tests outside this package read a copy
// (graphstoretest.BaseAttributes), which TestGraphstoretestBaseAttributesAreTheStoresOwn holds
// equal to this.
var baseAttributes = [...]string{AttributeVisibility, AttributeState, AttributeProject, AttributeParties}

// ErrProjectMoved is a write that names a project for a node the store already holds in another:
// a node's project is what every read of it is limited by, so it never moves once stated, and the
// write is refused rather than the node read under two projects' rules over its life. A write
// that names no project moves nothing and is not refused.
var ErrProjectMoved = errors.New("a node's project never moves")

// IsBaseAttribute is whether kind is the tuple kind of a base attribute (baseAttributes).
func IsBaseAttribute(kind string) bool { return slices.Contains(baseAttributes[:], kind) }

// ErrInvalidAttributeValue is a write that states a node's state or its visibility at a value that
// attribute does not take (NodeStates; VisibilityProject and VisibilityAuthor). It is a sentinel
// so a route can answer it as a refusal of the caller's rather than as a failure of the server's;
// what a write is refused with is an InvalidAttributeValueError, which wraps it and names the
// attribute and the value.
var ErrInvalidAttributeValue = errors.New("a node's state or visibility is none of the values it takes")

// InvalidAttributeValueError is ErrInvalidAttributeValue for one value: the base attribute a write
// stated and the value it stated. Its words name both and every value the attribute takes, so a
// misspelled value reads as one.
type InvalidAttributeValueError struct {
	// Attribute is AttributeState or AttributeVisibility.
	Attribute string
	// Value is the value the write stated, which the attribute does not take.
	Value string
}

func (e *InvalidAttributeValueError) Error() string {
	return fmt.Sprintf("the node %s %q is none of %s", e.Attribute, e.Value, strings.Join(attributeValues(e.Attribute), ", "))
}

func (e *InvalidAttributeValueError) Unwrap() error { return ErrInvalidAttributeValue }

// attributeValues is every value a base attribute takes, or nil for one that takes any value: a
// node's project is any project's key, and its parties any parties.
func attributeValues(attribute string) []string {
	switch attribute {
	case AttributeState:
		return nodeStates[:]
	case AttributeVisibility:
		return visibilities[:]
	}
	return nil
}

// CheckAttributeValue refuses a value the base attribute does not take with an
// InvalidAttributeValueError, and answers nil for a value it takes and for any value of an
// attribute that takes any (attributeValues). It is the one rule every write of this store holds
// a node's state and visibility to, and a route that takes either from its caller asks it first.
func CheckAttributeValue(attribute, value string) error {
	values := attributeValues(attribute)
	if values == nil || slices.Contains(values, value) {
		return nil
	}
	return &InvalidAttributeValueError{Attribute: attribute, Value: value}
}

// checkBaseValues holds a write to CheckAttributeValue as it is queued, so a refused write
// queues its refusal alone (queueRefusal) and stores nothing: the state and the visibility of
// every node it writes, and every value tuple and every claim it states itself, of which a state
// or a visibility is all that can be refused. A node reserved for its author whose kind takes no
// such node is refused here too, with a VisibilityNotAdmittedError naming the kind
// (CheckVisibilityAdmitted); a restating node among them, since the visibility it carries is the
// one its insert would make. The first value refused is the answer.
func checkBaseValues(nodes []Node, tuples []Tuple, claims []Claim) error {
	for index := range nodes {
		node := &nodes[index]
		if err := CheckAttributeValue(AttributeState, node.State); err != nil {
			return err
		}
		if err := CheckAttributeValue(AttributeVisibility, node.Visibility); err != nil {
			return err
		}
		if err := CheckVisibilityAdmitted(node.Kind, node.ID, node.Visibility); err != nil {
			return err
		}
	}
	for index := range tuples {
		if tuple := &tuples[index]; tuple.ToKind == KindValue {
			if err := CheckAttributeValue(tuple.Kind, tuple.ToID); err != nil {
				return err
			}
		}
	}
	for index := range claims {
		if claim := &claims[index]; claim.ToKind == KindValue {
			if err := CheckAttributeValue(claim.Kind, claim.ToID); err != nil {
				return err
			}
		}
	}
	return nil
}

// supertagKind is the node kind of a type, whose id is the type's name: the kind every node's
// supertag names a node of (migration 20261109051232). pmdoc/schemanodes.KindSupertag spells it
// for the packages that may import schemanodes, and this one may not.
const supertagKind = "supertag"

// ErrSupertagNotHeld refuses a write of a node whose supertag is no type the store holds: a type
// is a node of the store, a `supertag` row that is not gone, so a node of any other supertag is a
// node of a type nobody declared. It is a sentinel so a route can answer it as a refusal, 409
// SUPERTAG_NOT_HELD; what a write is refused with is a SupertagNotHeldError, which wraps it and
// names each type.
var ErrSupertagNotHeld = errors.New("the store holds no type of that name")

// SupertagNotHeldError is ErrSupertagNotHeld for the supertags of one write that the store holds
// no type of, sorted, each once.
type SupertagNotHeldError struct{ Supertags []string }

func (e *SupertagNotHeldError) Error() string {
	return fmt.Sprintf("the store holds no type %s, so it takes no node of it: a type is a supertag node of the store, "+
		"and a node of a type the store does not hold is refused", strings.Join(e.Supertags, ", "))
}

func (e *SupertagNotHeldError) Unwrap() error { return ErrSupertagNotHeld }

// queueSupertagCheck queues the read that refuses a write of any node whose supertag the store
// holds no type of (SupertagNotHeldError): one statement over the primary key of graph_nodes for
// the distinct supertags of the nodes given, queued before the node write, whose callback answers
// the refusal. Nothing holds the types in memory: a type a migration or an operator writes is taken
// by the next write of every process, and one deleted is refused by it, with no restart. The arms a
// caller splices into a statement of its own (WriteArms) run no such read: they write the person,
// session, role and page nodes alone, of supertags the store seeds, and the publisher's record
// that splices them into its one statement has no statement before it to refuse in.
func queueSupertagCheck(b *store.WriteBatch, nodes []Node) {
	// A write's nodes carry one or two supertags, a block index's every block `block`, so the
	// distinct ones are gathered by a scan of the few already held rather than a set.
	supertags := make([]string, 0, 2)
	for index := range nodes {
		if supertag := nodes[index].Supertag; !slices.Contains(supertags, supertag) {
			supertags = append(supertags, supertag)
		}
	}
	const what = "read the types of the nodes written"
	b.Queue(what, `
		select named.id from unnest($1::text[]) as named(id)
		where not exists (
			select 1 from graph_nodes held
			where held.kind = '`+supertagKind+`' and held.id = named.id and held.state <> '`+StateGone+`')
		order by named.id`, supertags).Query(func(rows pgx.Rows) error {
		missing, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if len(missing) > 0 {
			return &SupertagNotHeldError{Supertags: missing}
		}
		return nil
	})
}

// The base attributes a node write states for one node, as its mark carries them (nodeMarks).
const (
	markVisibility = 1 << iota
	markState
	markProject
	markParties
)

// markMade is the attributes a node write states for a node it makes, in the SQL of a select over
// the arrays it binds (nodeArrays): visibility and state always, and project and parties where the
// node carries each.
var markMade = strconv.Itoa(markVisibility|markState) +
	` + case when project_key is not null then ` + strconv.Itoa(markProject) + ` else 0 end` +
	` + case when parties is not null then ` + strconv.Itoa(markParties) + ` else 0 end`

// nodeMarks draws the mark a node write leaves on each row it made or moved, until the statement
// that states that write's facts reads it and moves the row onto its cause (queueAppend): a
// negative source_event, -(mark*markSpan + attributes), where mark names the write and attributes
// are the base attributes it states for the node. No event has a negative id, and a row carries the
// mark only between two statements of one transaction, the node write and the append. A node
// already there whose cause the write keeps (Node.KeepsCause) is the one row a move does not mark:
// the mark would take the place of the source_event it keeps. Its move states what it moved of it
// in the move's own statement instead (queueNodes).
var nodeMarks atomic.Int64

// nextNodeMark is a mark no earlier write of this process drew, kept far below the bound a
// negative source_event spelling it must stay above.
func nextNodeMark() int64 {
	return nodeMarks.Add(1)%(1<<40) + 1
}

// markSpan is how many values the four attributes a mark carries can take, so neither the mark nor
// its attributes reaches the other.
const markSpan = (markVisibility | markState | markProject | markParties) + 1

// markSQL is the source_event a node write leaves on a row: -(mark*markSpan + attributes), mark
// bound as the parameter at markParam.
func markSQL(markParam, attributes string) string {
	return `-(` + markParam + `::bigint * ` + strconv.Itoa(markSpan) + ` + ` + attributes + `)`
}

// markedBy is the condition that row's source_event is the mark the write at markParam left.
func markedBy(row, markParam string) string {
	return `(` + row + `.source_event < 0 and (-` + row + `.source_event) / ` + strconv.Itoa(markSpan) + ` = ` + markParam + `::bigint)`
}

// markAttributes is the base attributes the mark on row's source_event says its write states,
// read where markedBy holds.
func markAttributes(row string) string {
	return `((-` + row + `.source_event) % ` + strconv.Itoa(markSpan) + `)::integer`
}

// BaseAttributeValues is the rows of a `values` list that states a node's base attributes, one
// row for each in mark order (nodeMarks): the attribute's mark bit, its ordinal and its tuple kind,
// read as attribute(bit, ordinal, kind, ...), and then, for each list of columns, the SQL that
// reads that attribute's value off the row, in baseAttributes' order. baseAttributes is that
// order, so an attribute's bit is 1 << its index (markVisibility, markState, markProject,
// markParties). It is the one spelling of that list for every statement that states a node's base
// attributes off its row: the move of a write that keeps a node's cause (queueNodes), the append's
// based (queueAppend) and graphstoretest's SeedAuthorOnly.
func BaseAttributeValues(columns ...[len(baseAttributes)]string) string {
	rows := make([]string, len(baseAttributes))
	for ordinal, attribute := range baseAttributes {
		row := `(` + strconv.Itoa(1<<ordinal) + `, ` + strconv.Itoa(ordinal) + `, '` + attribute + `'`
		for _, values := range columns {
			row += `, ` + values[ordinal]
		}
		rows[ordinal] = row + `)`
	}
	return strings.Join(rows, `,
			`)
}

// markedAttributeValues are the values rows the append's based reads each base attribute of a
// marked node through (BaseAttributeValues), rendered once.
var markedAttributeValues = BaseAttributeValues([...]string{`marked.visibility`, `marked.state`, `marked.project`, `marked.parties`})

// queueNodes queues the write of nodes, in statements each of whose rows arrive in ascending
// (kind, id): a write touching several nodes takes its row locks in that order, so two such
// writes cannot each hold the row the other waits for. The first refuses a node of a type the
// store does not hold (queueSupertagCheck); the next inserts every node the store does not hold
// and locks every node it does, at the level an update of that row takes, changing nothing in
// it; the last moves supertag, visibility, state, attributes and parties on each row whose
// columns differ, fills a project the row does not hold yet, and leaves made_at and made_by as
// the row's own first write left them - the row is the projection of its tuples and may be
// written again, and a later write that learns the event id has to be able to move source_event.
// A node written with no project or no parties keeps what its row holds of each: that writer
// knows neither, and is no word that the node has none. A node the write restates
// (Node.Restates) keeps its row's visibility and attributes, and its row's state unless that
// state or the node's is gone: its writer says only whether the node is there, so the row moves
// to gone when the writer says gone, and to the writer's state only from gone. The insert and the
// move leave mark, which the write's caller drew (nextNodeMark), on the rows they made or moved
// (nodeMarks), but on a node already there whose cause the write keeps (Node.KeepsCause): its row
// keeps its source_event whatever else of it moves, and the move states each base attribute it
// moved of that row itself, in the same statement, at the node's own time, writer and
// SourceEvent. Each statement builds its arguments only as the write phase sends it
// (store.WriteBatch's QueueBuilt), so a large version holds encoded arguments one statement at a
// time; stated and tuple are the writer's own tuples, read for a base attribute it states itself.
//
// A write naming a project for a node the row holds in another is refused with ErrProjectMoved,
// whether or not anything else of the node moved, and the row keeps its project either way: a
// statement between the two reads, under the locks the first took, each node named in another
// project than its row's, and its result fails the batch as Send reads it, which ends the
// caller's transaction.
//
// The rows carry the base attributes the write states, one attribute at a time, for the append
// to state. A node the insert made states `visibility` and `state`, and `project` and `parties`
// where it carries them, which is the node carrying each from its first write. A node that was
// already there states each attribute this write moved on its row, each on its own: a write that
// moves one states that one, a write that gives a node an earlier build stored without a project
// its project states `project` alone, and a node written again with nothing moved states
// nothing, so a re-index of a document nothing moved appends nothing. An attribute a restating
// write kept did not move, so nothing is stated of it, and one it moved moved to the value the
// node carries, which is what its tuple states: the log never says a restated node went live
// while its row stayed at the state another write gave it. They are compared separately because
// they move separately - a node hidden from a project keeps its state, and a deleted block keeps
// its visibility - and a row whose column moved while the log said nothing would be a row no
// appended fact explains, which nothing may mend later, since no tuple is ever updated. Each
// tuple carries the time, the writer and the cause of the node write that states it.
//
// The stored values are compared on the row the last statement updates. Under READ COMMITTED
// that statement reads a snapshot of its own, taken once the insert holds every row the write
// touches: what it compares against is the newest committed version of each row, and no other
// writer can move one until this transaction ends. Read in the snapshot of a statement that then
// waited on a row, they would be the row as it stood before the writer it waited on, while the
// write goes over the version that writer committed: one writer states a node gone, a second
// waits on its row and moves it back to live, compares live with live and states nothing, and the
// row says live while its newest state tuple says gone. What a restating write keeps is read off
// that same row, so one that waited on a write marking the node otherwise keeps the state that
// write committed, rather than writing live over it and stating nothing. Locking the rows already
// there in a statement of their own before inserting the new ones would read fresh values too,
// but it takes the rows out of ascending (kind, id) wherever a new node sorts before one already
// there. Two writes racing over a node the store does not hold yet are one case of the same: the
// second's insert waits on the first's, then finds the row there and locks it, and its later
// statements compare against what the first committed. The pass that writes a block's node holds
// its document's owner lock before it reaches this.
//
// No statement joins the nodes it was handed against another list it was handed: each reads
// them once, and the later ones meet graph_nodes through its primary key. Planned without the
// arrays' sizes, as a cached statement is planned from its sixth run on, a join of two such lists
// is a nested loop that rescans one per row of the other, which measured 3,009 ms against 193 ms
// over a 1,999-block version, so what the write did to each node rides on the node's own row
// instead (nodeMarks). The move of a write that keeps a node's cause reads the rows it keeps
// beside the ones it moves, and such a write - a request's write of a node's state - writes a
// node and the few that stand under it.
func queueNodes(b *store.WriteBatch, nodes []Node, mark int64, stated int, tuple func(index int) Tuple) {
	if len(nodes) == 0 {
		return
	}
	queueSupertagCheck(b, nodes)
	// A write that keeps a node's cause binds, beside the arrays every move binds, the time, the
	// writer and the cause of each node, which the insert binds too (keptCauseNodeMove).
	keepsCause := slices.ContainsFunc(nodes, func(node Node) bool { return node.KeepsCause })
	var shared, projectArgs, scopeArgs, provenance []any
	markParam := "$" + strconv.Itoa(len(nodeArrayTypes)+1)
	b.QueueBuilt("write graph nodes", `
		insert into graph_nodes (`+nodeColumns+`)
		select kind, id, supertag, visibility, state, attributes, made_at, made_by, `+markSQL(markParam, markMade)+`,
		       project_key, string_to_array(parties, ' ')
		from unnest(`+nodeArrays(1)+`) as node(`+nodeColumns+`)
		order by kind, id
		on conflict (kind, id) do update set source_event = graph_nodes.source_event
		where false
	`, func() ([]any, error) {
		args, err := nodeParams(nodes)
		if err != nil {
			return nil, err
		}
		shared = args[:6:6]
		projectArgs = []any{args[0], args[1], args[9]}
		scopeArgs = []any{args[9], args[10]}
		if keepsCause {
			provenance = args[6:9:9]
		}
		return append(args, mark), nil
	})
	if slices.ContainsFunc(nodes, func(node Node) bool { return node.Project != "" }) {
		// Each incoming node probes its own row through graph_nodes' primary key, behind an offset
		// 0 the planner cannot flatten. This keeps the array scan at one pass after the preceding
		// insert added rows that table statistics cannot yet describe.
		b.QueueBuilt("write graph nodes", `
			select incoming.kind, incoming.id, incoming.project_key, held.project_key
			from unnest($1::text[], $2::text[], $3::text[]) as incoming(kind, id, project_key)
			cross join lateral (
				select g.project_key from graph_nodes g
				where g.kind = incoming.kind and g.id = incoming.id
				offset 0
			) held
			where incoming.project_key is not null and held.project_key <> incoming.project_key
			order by incoming.kind, incoming.id
			limit 1
		`, func() ([]any, error) {
			args := projectArgs
			projectArgs = nil
			if args != nil {
				return args, nil
			}
			all, err := nodeParams(nodes)
			if err != nil {
				return nil, err
			}
			return []any{all[0], all[1], all[9]}, nil
		}).QueryRow(func(row pgx.Row) error {
			var kind, id, named, held string
			switch err := row.Scan(&kind, &id, &named, &held); {
			case errors.Is(err, pgx.ErrNoRows):
				return nil
			case err != nil:
				return err
			}
			return fmt.Errorf("write graph node %s/%s in project %s, which the store holds in %s: %w",
				kind, id, named, held, ErrProjectMoved)
		})
	}
	// The move reads, beside the columns it compares, whether the write restates each node
	// (Node.Restates): a node the insert made has no stored state, visibility or attributes to keep.
	//
	// A node already there whose base attribute the caller states itself - a document made where
	// an orphaned node stands states that node gone and then live in tuples of its own - is marked
	// even where its row moves nothing, so the append moves it onto the cause the caller's
	// statements carry (states), and neither statement states an attribute the caller states. A
	// node the insert above just made carries the insert's mark (markMade) already, which names
	// every base attribute it carries, so it takes no second mark: an empty one over it would leave
	// its visibility, its project and its parties stated by nothing.
	//
	// A write that keeps a node's cause binds two arrays more: whether it keeps each node's cause,
	// and the base attributes of each node the writer states itself, which its move states nothing
	// of (keptCauseNodeMove).
	move := nodeMove
	if keepsCause {
		move = keptCauseNodeMove
	}
	b.QueueBuilt("write graph nodes", move, func() ([]any, error) {
		compared, scopes, made := shared, scopeArgs, provenance
		shared, scopeArgs, provenance = nil, nil, nil
		if compared == nil || scopes == nil || keepsCause && made == nil {
			args, err := nodeParams(nodes)
			if err != nil {
				return nil, err
			}
			if compared == nil {
				compared = args[:6:6]
			}
			if scopes == nil {
				scopes = []any{args[9], args[10]}
			}
			if made == nil {
				made = args[6:9:9]
			}
		}
		own := statedAttributes(stated, tuple)
		restates := make([]bool, len(nodes))
		states := make([]bool, len(nodes))
		var keeps []bool
		var skips []int32
		if keepsCause {
			keeps, skips = make([]bool, len(nodes)), make([]int32, len(nodes))
		}
		for index := range nodes {
			node := &nodes[index]
			skip := ownedAttributes(own, node.Kind, node.ID)
			restates[index], states[index] = node.Restates, skip != 0
			if keepsCause {
				keeps[index], skips[index] = node.KeepsCause, skip
			}
		}
		args := append(compared, restates, mark, scopes[0], scopes[1], states)
		if !keepsCause {
			return args, nil
		}
		// The time, the writer and the cause the move states a kept node's attributes at are the
		// node's own, the arrays the insert binds (nodeParams).
		return append(args, keeps, made[0], made[1], made[2], skips), nil
	})
}

// nodeMove and keptCauseNodeMove are queueNodes' move, each built once: the move of a write that
// keeps no node's cause, and the move of one that keeps a node's cause (Node.KeepsCause), which
// states each base attribute it moved of such a node itself, in the same statement. Both bind the
// insert's arrays as queueNodes hands them and the mark at $8; the second binds five arrays more,
// from $12: whether it keeps each node's cause, the time, the writer and the cause each node's
// attributes are stated at, and the base attributes the writer states itself (ownedAttributes),
// which the move leaves out, as the append does.
var nodeMove, keptCauseNodeMove = func() (string, string) {
	movedMark := markSQL("$8", `case when graph_nodes.visibility is distinct from `+writtenVisibility+` then `+strconv.Itoa(markVisibility)+` else 0 end
				+ case when graph_nodes.state is distinct from `+writtenState+` then `+strconv.Itoa(markState)+` else 0 end
				+ case when `+fillsProject+` then `+strconv.Itoa(markProject)+` else 0 end
				+ case when `+movesParties+` then `+strconv.Itoa(markParties)+` else 0 end`)
	move := `
		update graph_nodes set
			supertag = incoming.supertag,
			visibility = ` + writtenVisibility + `,
			state = ` + writtenState + `,
			attributes = ` + writtenAttributes + `,
			project_key = coalesce(graph_nodes.project_key, incoming.project_key),
			parties = coalesce(incoming.parties, graph_nodes.parties),
			source_event = %s
		from %s
		where graph_nodes.kind = incoming.kind and graph_nodes.id = incoming.id
		  and (graph_nodes.supertag is distinct from incoming.supertag
		       or graph_nodes.visibility is distinct from ` + writtenVisibility + `
		       or graph_nodes.state is distinct from ` + writtenState + `
		       or graph_nodes.attributes is distinct from ` + writtenAttributes + `
		       or ` + fillsProject + `
		       or ` + movesParties + `
		       or (incoming.states and not ` + markedBy("graph_nodes", "$8") + `))`
	return `
		with incoming as (
			select kind, id, supertag, visibility, state, attributes, restates, project_key,
			       string_to_array(parties, ' ') as parties, states
			from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::jsonb[], $7::boolean[], $9::text[], $10::text[], $11::boolean[])
				as node(kind, id, supertag, visibility, state, attributes, restates, project_key, parties, states)
		)` + fmt.Sprintf(move, movedMark, `incoming`), `
		with incoming as (
			select kind, id, supertag, visibility, state, attributes, restates, project_key,
			       string_to_array(parties, ' ') as parties, states, keeps_cause, made_at, made_by, cause, skip, position
			from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::jsonb[], $7::boolean[], $9::text[], $10::text[], $11::boolean[],
			            ` + arrayList(12, "boolean", "timestamptz", "jsonb", "bigint", "integer") + `) with ordinality
				as node(kind, id, supertag, visibility, state, attributes, restates, project_key, parties, states,
				        keeps_cause, made_at, made_by, cause, skip, position)
		),
		kept as (
			select held.kind, held.id, held.visibility, held.state, held.project_key, held.parties
			from graph_nodes held
			join incoming on incoming.kind = held.kind and incoming.id = held.id
			where incoming.keeps_cause
		),
		moved as (` + fmt.Sprintf(move, `case when incoming.keeps_cause then graph_nodes.source_event else `+movedMark+` end`,
			`incoming left join kept on kept.kind = incoming.kind and kept.id = incoming.id`) + `
			returning graph_nodes.kind, graph_nodes.id, graph_nodes.visibility, graph_nodes.state,
			          graph_nodes.project_key, array_to_string(graph_nodes.parties, ' ') as parties,
			          incoming.keeps_cause, incoming.made_at, incoming.made_by, incoming.cause, incoming.skip, incoming.position,
			          kept.visibility as kept_visibility, kept.state as kept_state,
			          kept.project_key as kept_project, array_to_string(kept.parties, ' ') as kept_parties
		)
		insert into graph_tuples (` + tupleWriteColumns + `)
		select moved.kind, moved.id, attribute.kind, '` + KindValue + `', attribute.value, moved.made_at, moved.made_by,
		       ` + causeOf("moved.cause") + `, null::bigint, true
		from moved
		cross join lateral (values
			` + BaseAttributeValues(
			[...]string{`moved.visibility`, `moved.state`, `moved.project_key`, `moved.parties`},
			[...]string{`moved.kept_visibility`, `moved.kept_state`, `moved.kept_project`, `moved.kept_parties`}) + `
		) as attribute(bit, ordinal, kind, value, kept)
		where moved.keeps_cause and attribute.value is distinct from attribute.kept and attribute.value is not null
		  and moved.skip & attribute.bit = 0
		order by moved.position, attribute.ordinal
		on conflict do nothing
	`
}()

// fillsProject and movesParties are whether the move gives a row the project it does not hold
// yet, and the parties it holds otherwise or not at all, read as writtenVisibility is.
const (
	fillsProject = `(graph_nodes.project_key is null and incoming.project_key is not null)`
	movesParties = `(incoming.parties is not null and graph_nodes.parties is distinct from incoming.parties)`
)

// writtenVisibility, writtenState and writtenAttributes are the visibility, the state and the
// attributes the move writes a row at, read off the row its update holds (graph_nodes, before the
// update) and the node it was handed (incoming): the node's own, but for a node the write restates
// (Node.Restates), which keeps the row's visibility and attributes, and the row's state unless the
// row's or the node's is gone. A state that moves therefore moves to the node's own, the value the
// append states of it. A restating writer owns only the facts it writes the node again for - a
// block's text and place, a comment's anchor - and none of the node's attributes, which another
// write gives the node as it may give it a state: no writer that restates a node states any, so
// the empty object such a node carries would otherwise erase what that other write stored. The
// visibility and the state are each spelled once and read three times, by the update's set, by its
// mark and by its condition, and the attributes twice, by the set and the condition, since no base
// attribute states them: a row a restating write keeps as it is takes no update.
const (
	writtenVisibility = `case when incoming.restates then graph_nodes.visibility else incoming.visibility end`
	writtenState      = `case when incoming.restates and incoming.state <> '` + StateGone + `'
				and graph_nodes.state <> '` + StateGone + `'
			then graph_nodes.state else incoming.state end`
	writtenAttributes = `case when incoming.restates then graph_nodes.attributes else incoming.attributes end`
)

// appending is one append of a write's tuples (queueAppend): the caller's own, the base
// attributes its node write states for the nodes it marked, and the changed_by_run each node it
// changed owes the run it is made for.
type appending struct {
	// tuples is how many tuples the caller states itself, and tuple returns the one at an index.
	// They stay with their writer until the write phase sends this statement.
	tuples int
	tuple  func(index int) Tuple
	// nodes are the nodes the write wrote, and mark what its node write left on the rows it made
	// or moved, 0 for none.
	nodes []Node
	mark  int64
	// baseAt is how many of tuples the base attributes follow.
	baseAt int
	// cause is the event every base attribute and every node moved here takes, nil for a write
	// whose nodes each carry their own (Node.SourceEvent).
	cause *int64
	// sentinel says every node still at the sentinel is moved onto cause too.
	sentinel bool
	// appended, where Counting set it, counts the tuples the append appends (queueAppend).
	appended *int64
	// run is the run of the command bar the write is made for, and changes the changed_by_run each
	// node the write changed owes it (changedByRunMarks), none for a write made for no run.
	run     string
	changes []Tuple
}

// queueAppend queues the statements that append a write's tuples and move its nodes onto their
// cause: the relations first and then the attributes, each in the order the caller stated them,
// with the base attributes the node write states standing after the first baseAt of the caller's
// tuples, one node at a time in the order the nodes were given, visibility, state, project,
// parties (nodeMarks); and, beside them, every node the node write marked set at its cause, with,
// where sentinel says so, every node still at the sentinel. It queues one statement per
// appendRows of the caller's tuples and nodes (appendSegments), which keep exactly that order
// while limiting an encoded statement's peak memory.
//
// A write made for a run also appends one changed_by_run from each node of changes to the run's
// interaction, after every fact the write states and every base attribute its node write states,
// in a statement of its own (queueChangedByRun), so the statements that move the nodes onto their
// cause stay the append's first.
func queueAppend(b *store.WriteBatch, write appending) {
	if write.tuples == 0 && len(write.changes) == 0 && write.mark == 0 && !(write.sentinel && len(write.nodes) > 0) {
		return
	}
	order, majors, before, baseSlot := appendOrder(write.tuples, write.tuple, write.baseAt)
	nodes := write.nodes
	if write.mark == 0 && !write.sentinel {
		nodes = nil
	}
	own := statedAttributes(write.tuples, write.tuple)
	for _, segment := range appendSegments(len(order), before, len(nodes), appendRows) {
		queueAppendStatement(b, order[segment.tuplesFrom:segment.tuplesTo], majors[segment.tuplesFrom:segment.tuplesTo],
			nodes[segment.nodesFrom:segment.nodesTo], write, own, baseSlot)
	}
	if len(write.changes) > 0 {
		queueChangedByRun(b, write.changes, write.run, write.appended)
	}
}

// appendRows is the most tuples and nodes one statement of an append binds together.
const appendRows = 8192

// appendOrder is the caller's tuple indexes in append order, with their positions. before is the
// number of caller tuples before the base-attribute slot.
func appendOrder(tuples int, tuple func(index int) Tuple, baseAt int) (order []int, majors []int64, before int, baseSlot int64) {
	relations := 0
	for index := range tuples {
		if tuple(index).ToKind != KindValue {
			relations++
		}
	}
	places := make([]int64, tuples)
	relation, value := int64(0), int64(relations)
	baseSlot = int64(relations)
	for index := range tuples {
		if index == baseAt {
			baseSlot = value
			value++
		}
		if tuple(index).ToKind == KindValue {
			places[index] = value
			value++
		} else {
			places[index] = relation
			relation++
		}
	}
	if baseAt >= tuples {
		baseSlot = value
	}
	order = make([]int, tuples)
	majors = make([]int64, tuples)
	for index, major := range places {
		at := major
		if major > baseSlot {
			at--
		}
		order[at], majors[at] = index, major
	}
	return order, majors, int(baseSlot), baseSlot
}

// appendSegment is one statement of an append: caller tuples and nodes in their append-order
// ranges.
type appendSegment struct{ tuplesFrom, tuplesTo, nodesFrom, nodesTo int }

// appendSegments cuts an append into statements of at most rows tuples and nodes each.
func appendSegments(tuples, before, nodes, rows int) []appendSegment {
	total := tuples + nodes
	var segments []appendSegment
	for start := 0; start < total; start += rows {
		end := min(start+rows, total)
		segments = append(segments, appendSegment{
			tuplesFrom: tupleAt(start, before, nodes), tuplesTo: tupleAt(end, before, nodes),
			nodesFrom: min(max(start-before, 0), nodes), nodesTo: min(max(end-before, 0), nodes),
		})
	}
	return segments
}

// tupleAt is how many caller tuples stand before append position at, where nodes occupy the range
// [before, before+nodes).
func tupleAt(at, before, nodes int) int {
	switch {
	case at <= before:
		return at
	case at <= before+nodes:
		return before
	default:
		return at - nodes
	}
}

// queueAppendStatement queues one segmented append. Its arguments build at Send, and the node
// reads probe each node by its primary key instead of joining the batch to graph_nodes.
//
// The tuples carry two repeat keys: one event states one fact once, so a writer that is retried
// appends the same row again, and the insert drops it. One insert carries both, so it names no
// conflict target, and every unique index of graph_tuples is its arbiter: the two repeat keys,
// each partial on `source_event <> 0`, so a tuple at the sentinel is appended unconditionally.
func queueAppendStatement(b *store.WriteBatch, order []int, majors []int64, nodes []Node, write appending, own map[baseAttribute]struct{}, baseSlot int64) {
	build := func() ([]any, error) {
		params, err := tupleParamsOf(len(order), func(index int) Tuple { return write.tuple(order[index]) })
		if err != nil {
			return nil, err
		}
		params = append(params, majors)
		if len(nodes) == 0 {
			return params, nil
		}
		nodeArgs, err := appendedNodeParams(nodes, write.cause, own)
		if err != nil {
			return nil, err
		}
		return append(append(params, nodeArgs...), write.mark, baseSlot, write.sentinel), nil
	}
	stated := orderedTuples(1, tupleArrayCount+1)
	if len(nodes) == 0 {
		counted(b.QueueBuilt("append graph tuples", `
			insert into graph_tuples (`+tupleWriteColumns+`)
			select `+tupleWriteColumns+` from (`+stated+`) appended
			order by major, minor
			on conflict do nothing
		`, build), write.appended)
		return
	}
	// The node arrays follow the tuples' ten and their places, then the mark, base slot and
	// sentinel follow the node arrays (appendedNodeParams).
	at := tupleArrayCount + 2
	markParam := "$" + strconv.Itoa(at+appendedNodeArrays)
	baseParam, sentinelParam := "$"+strconv.Itoa(at+appendedNodeArrays+1), "$"+strconv.Itoa(at+appendedNodeArrays+2)
	counted(b.QueueBuilt("append graph tuples", `
		with written as (
			select kind, id, visibility, state, project, parties, made_at, made_by, cause, skip, position
			from unnest(`+arrayList(at, "text", "text", "text", "text", "text", "text", "timestamptz", "jsonb", "bigint", "integer")+`)
				with ordinality as node(kind, id, visibility, state, project, parties, made_at, made_by, cause, skip, position)
		),
		marked as (
			select written.*, `+markAttributes("held")+` & ~written.skip as attributes
			from written
			cross join lateral (
				select g.source_event from graph_nodes g
				where g.kind = written.kind and g.id = written.id
				offset 0
			) held
			where `+markedBy("held", markParam)+`
		),
		based as (
			select marked.kind as from_kind, marked.id as from_id, attribute.kind, '`+KindValue+`' as to_kind,
			       attribute.value as to_id, marked.made_at as at, marked.made_by as who,
			       `+causeOf("marked.cause")+` as source_event, null::bigint as legacy_source_seq, true as asserted,
			       `+baseParam+`::bigint as major, marked.position * `+strconv.Itoa(len(baseAttributes))+` + attribute.ordinal as minor
			from marked
			cross join lateral (values
				`+markedAttributeValues+`
			) as attribute(bit, ordinal, kind, value)
			where marked.attributes & attribute.bit <> 0
		),
		settled as (
			update graph_nodes set source_event = `+causeOf("moved.cause")+`
			from (
				select written.kind, written.id, written.cause
				from written
				cross join lateral (
					select g.source_event from graph_nodes g
					where g.kind = written.kind and g.id = written.id
					offset 0
				) held
				where `+markedBy("held", markParam)+`
				   or `+sentinelParam+`::boolean and held.source_event = `+strconv.FormatInt(NoEvent, 10)+`
				      and written.cause <> `+strconv.FormatInt(NoEvent, 10)+`
			) moved
			where graph_nodes.kind = moved.kind and graph_nodes.id = moved.id
		)
		insert into graph_tuples (`+tupleWriteColumns+`)
		select `+tupleWriteColumns+` from (
			`+stated+`
			union all
			select `+tupleWriteColumns+`, major, minor from based
		) appended
		order by major, minor
		on conflict do nothing
	`, build), write.appended)
}

// queueChangedByRun queues the append of the changed_by_run tuples a write made for run owes it
// (changedByRunMarks), one from each node of changes to the run's interaction, but for a node
// that stands changed by that run already: one whose newest changed_by_run tuple to the run's
// interaction asserts it, read as every relation as it stands now is read (CurrentTupleRead),
// through graph_tuples_as_of_in, which keys the run's interaction and then the kind. The tuple is
// appended once for each node and run, in the order changes names the nodes, and a write of no
// run queues nothing for it (queueAppend). The read takes no lock: what orders two writes of one
// node for one run is the lock each takes before this statement runs - the node's own row, which
// the node write holds until the transaction ends, or the owner lock (store.LockOwner) a
// pointer's writer takes first - so the second reads, in this statement's snapshot, the tuple the
// first committed. Its arguments build at Send, as every statement of the append's do.
func queueChangedByRun(b *store.WriteBatch, changes []Tuple, run string, appended *int64) {
	majorsAt := tupleArrayCount + 1
	runParam := "$" + strconv.Itoa(majorsAt+1)
	counted(b.QueueBuilt("append graph tuples", `
		with changes as (
			`+orderedTuples(1, majorsAt)+`
		),
		changes_held as (
			select from_kind, from_id from (
				`+CurrentTupleRead(`t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
		`t.to_kind = '`+KindInteraction+`' and t.to_id = `+runParam+`::text and t.kind = '`+TupleChangedByRun+`'
					and (t.from_kind, t.from_id) in (select from_kind, from_id from changes)`)+`
			) newest
			where asserted
		)
		insert into graph_tuples (`+tupleWriteColumns+`)
		select `+tupleWriteColumns+` from (
			select `+tupleWriteColumns+`, major, minor from changes change
			where not exists (
				select 1 from changes_held held where held.from_kind = change.from_kind and held.from_id = change.from_id)
		) appended
		order by major, minor
		on conflict do nothing
	`, func() ([]any, error) {
		params, err := tupleParams(changes)
		if err != nil {
			return nil, err
		}
		majors := make([]int64, len(changes))
		for index := range majors {
			majors[index] = int64(index)
		}
		return append(params, majors, run), nil
	}), appended)
}

// orderedTuples is the select of the tuples an append binds from the parameter first on
// (tupleParams, tupleArrays), each at the place in the append the bigint array bound at majors
// names, and first among the rows that share that place.
func orderedTuples(first, majors int) string {
	return `select ` + tupleSelect + `, major, 0::bigint as minor
		from unnest(` + tupleArrays(first) + `, $` + strconv.Itoa(majors) + `::bigint[])
			as tuple(` + tupleWriteColumns + `, major)`
}

// counted has queued add the rows it inserts to appended, where appended is not nil.
func counted(queued store.Queued, appended *int64) {
	if appended == nil {
		return
	}
	queued.Exec(func(tag pgconn.CommandTag) error {
		*appended += tag.RowsAffected()
		return nil
	})
}

// appendedNodeParams are the ten arrays a segmented append reads nodes through: their four base
// attributes, provenance, cause and the base attributes the writer stated itself.
func appendedNodeParams(nodes []Node, cause *int64, own map[baseAttribute]struct{}) ([]any, error) {
	kinds := make([]string, len(nodes))
	ids := make([]string, len(nodes))
	visibilities := make([]string, len(nodes))
	states := make([]string, len(nodes))
	projects := make([]string, len(nodes))
	parties := make([]string, len(nodes))
	madeAts := make([]time.Time, len(nodes))
	madeBys := make([][]byte, len(nodes))
	causes := make([]int64, len(nodes))
	skips := make([]int32, len(nodes))
	for index, node := range nodes {
		madeBy, err := json.Marshal(node.MadeBy.Recorded())
		if err != nil {
			return nil, fmt.Errorf("encode writer of node %s/%s: %w", node.Kind, node.ID, err)
		}
		value, err := PartiesValue(node.Parties)
		if err != nil {
			return nil, fmt.Errorf("node %s/%s: %w", node.Kind, node.ID, err)
		}
		kinds[index], ids[index] = node.Kind, node.ID
		visibilities[index], states[index] = node.Visibility, node.State
		projects[index], parties[index] = node.Project, value
		madeAts[index], madeBys[index] = node.MadeAt, madeBy
		causes[index], skips[index] = node.SourceEvent, ownedAttributes(own, node.Kind, node.ID)
		if cause != nil {
			causes[index] = *cause
		}
	}
	return []any{kinds, ids, visibilities, states, projects, parties, madeAts, madeBys, causes, skips}, nil
}

// appendedNodeArrays is how many arrays appendedNodeParams binds.
const appendedNodeArrays = 10

// baseAttribute is one base attribute of one node, the key a write is read by to see whether it
// states that attribute itself.
type baseAttribute struct{ kind, id, attribute string }

// statedAttributes are the base attributes the write states itself, read from the tuple source at
// Send so it neither copies nor encodes a version's tuples while it queues them.
func statedAttributes(tuples int, tuple func(index int) Tuple) map[baseAttribute]struct{} {
	stated := make(map[baseAttribute]struct{})
	for index := range tuples {
		held := tuple(index)
		if held.ToKind != KindValue || !IsBaseAttribute(held.Kind) {
			continue
		}
		stated[baseAttribute{held.FromKind, held.FromID, held.Kind}] = struct{}{}
	}
	return stated
}

// ownedAttributes is the base attributes of the node kind/id the writer states itself (own,
// statedAttributes), each at its bit in mark order, the order baseAttributes lists them in
// (nodeMarks): what a node write's statements state nothing of for that node.
func ownedAttributes(own map[baseAttribute]struct{}, kind, id string) int32 {
	var bits int32
	for bit, attribute := range baseAttributes {
		if _, held := own[baseAttribute{kind, id, attribute}]; held {
			bits |= 1 << bit
		}
	}
	return bits
}

// valueTupleOf is one value tuple of a node as the write that made the node states it, carrying
// that write's time, writer and cause: an attribute its row carries (AttributeTuples).
func valueTupleOf(node Node, attribute, value string) Tuple {
	return Tuple{
		FromKind: node.Kind, FromID: node.ID, Kind: attribute,
		ToKind: KindValue, ToID: value,
		At: node.MadeAt, Who: node.MadeBy, SourceEvent: node.SourceEvent, Asserted: true,
	}
}

// AttributeTuples is one value tuple of each of attributes that node's row carries, in the order
// named, as the write that makes the node states it (valueTupleOf). A row is the projection of its
// own tuples (Node), so every writer that carries an attribute on a node's row states its value as
// a tuple of the node in the same write, which the history and the as-of reads answer: the going
// POST's run (InteractionFacts), the catalogue's kinds (AddLensKind) and the reasoning nodes a
// request writes (api's writeReasoningNodes). An attribute the row does not carry states nothing.
// Every such attribute is a string attribute of its type, so its value is the text itself.
func AttributeTuples(node Node, attributes ...string) []Tuple {
	tuples := make([]Tuple, 0, len(attributes))
	for _, attribute := range attributes {
		if value, carried := node.Attributes[attribute]; carried {
			tuples = append(tuples, valueTupleOf(node, attribute, fmt.Sprint(value)))
		}
	}
	return tuples
}

// tupleArrays is the parameter list of the tupleArrayCount arrays a tuple append binds, numbered
// from first, in tupleWriteColumns' order (tupleParams).
func tupleArrays(first int) string {
	return arrayList(first, "text", "text", "text", "text", "text", "timestamptz", "jsonb", "bigint", "bigint", "boolean")
}

// tupleArrayCount is how many arrays tupleArrays lists and tupleParams binds.
const tupleArrayCount = 10

// tupleParamsOf binds a segmented append directly from its tuple source without first copying the
// segment into a []Tuple.
func tupleParamsOf(count int, tuple func(index int) Tuple) ([]any, error) {
	fromKinds := make([]string, count)
	fromIDs := make([]string, count)
	kinds := make([]string, count)
	toKinds := make([]string, count)
	toIDs := make([]string, count)
	ats := make([]time.Time, count)
	whos := make([][]byte, count)
	sourceEvents := make([]int64, count)
	legacySeqs := make([]*int64, count)
	asserted := make([]bool, count)
	for index := range count {
		held := tuple(index)
		who, err := json.Marshal(held.Who.Recorded())
		if err != nil {
			return nil, fmt.Errorf("encode writer of %s %s: %w", held.Kind, held.FromID, err)
		}
		fromKinds[index], fromIDs[index], kinds[index] = held.FromKind, held.FromID, held.Kind
		toKinds[index], toIDs[index], ats[index] = held.ToKind, held.ToID, held.At
		whos[index], sourceEvents[index] = who, held.SourceEvent
		legacySeqs[index], asserted[index] = held.LegacySourceSeq, held.Asserted
	}
	return []any{fromKinds, fromIDs, kinds, toKinds, toIDs, ats, whos, sourceEvents, legacySeqs, asserted}, nil
}

// encodeAttributes writes an empty object for a node that carries no attribute, since the column
// is `not null` and a nil map encodes as the JSON null, which is a value rather than no value.
func encodeAttributes(attributes map[string]any) ([]byte, error) {
	if attributes == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(attributes)
}

// tupleSelect is the select list of an unnest of tuples as tupleWriteColumns names them, each
// cause column read through causeOf, so a tuple whose cause is the latest event is written with
// that event's id.
var tupleSelect = `from_kind, from_id, kind, to_kind, to_id, at, who, ` +
	causeOf("source_event") + ` as source_event, ` + causeOf("legacy_source_seq") + ` as legacy_source_seq, asserted`
