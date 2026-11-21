package graphstore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Every writer of this store puts its statements on a write phase's batch (store.WriteBatch) and
// runs nothing itself: a route queues them on its ledger beside the rows they are the facts of,
// so they run in its write phase, back to back, in the order it made its writes, and a writer
// with no write phase of its own runs them at once through store.RunWrite. No statement's result
// is read in Go before a later statement is sent, so each value a later statement needs from an
// earlier one is derived in the later statement's SQL: an event's id (LatestEvent), and which
// nodes a node write made or moved, which the node rows themselves carry until the statement that
// states those facts reads them (nodeMarks).

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
// statements as one batch of their own (QueueWrite). It is for a writer with no write phase to
// queue them on: a backfill, a seed, a delivery's settle.
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
// The two base attributes every node carries are stated here and not by each writer, so that a
// node written through any writer of this store carries them: the block index, the comment
// path, the backfill and the seed all reach the store through this call. queueNodes says which
// nodes they are stated for, and they are appended with the caller's own tuples.
//
// This is the whole of the write for a caller that has its cause in hand, or none at all: a
// backfill, a seed, a publisher, or a request whose event its write phase appends just before
// (LatestEvent). A caller whose event is appended after the node row - the comment path and the
// block index, whose projection rows carry a foreign key into graph_nodes and must therefore
// follow the node - holds the write for its event instead (PlanUncaused, QueueNodes,
// QueueCaused), so that the base attributes are appended under that event rather than under the
// sentinel.
//
// Its statements build their arguments when the write phase sends them (store.WriteBatch's
// QueueBuilt), so a node or a writer that cannot be encoded fails Send rather than this call,
// which therefore answers nothing.
func QueueWrite(b *store.WriteBatch, nodes []Node, tuples []Tuple) {
	mark := int64(0)
	if len(nodes) > 0 {
		mark = nextNodeMark()
	}
	queueNodes(b, nodes, mark)
	queueAppend(b, appending{
		tuples: len(tuples), tuple: func(index int) Tuple { return tuples[index] }, nodes: nodes, mark: mark,
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
// commentStoreWrite), a version's block index (refs.BlockIndexing) and a follow of an ask
// (asks.Followed). A writer with nothing to state holds the zero Uncaused, for which Caused
// writes nothing.
type Uncaused struct {
	// Nodes are the nodes the write wrote. QueueCaused moves each its node write made or moved
	// onto the event, and, for a write of PlanUncaused, each still at the sentinel, so a node an
	// earlier write caused keeps its cause.
	Nodes []Node
	// Tuples are the facts the write states, each appended under the event by Caused, the base
	// attributes its node write stated among them, which stand after the first baseAt of these.
	Tuples []UncausedTuple
	// mark is what the node write left on the rows it made or moved (nodeMarks), 0 for a write
	// that marked none.
	mark int64
	// baseAt is how many of Tuples the base attributes the node write states follow.
	baseAt int
	// sentinel says QueueCaused also moves onto the event every node of Nodes still at the
	// sentinel, as PlanUncaused's writer holds all of them for its cause; a follow's node (the
	// arms) is caused only where that write made it.
	sentinel bool
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
//
// It takes nodes as its own: it sets each one's cause to the sentinel where it stands rather than
// copying a version's worth of nodes, so the caller does not use the slice again.
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
// nodes are read when the write phase sends the statements, so the writer does not change them
// after this call, and a node that cannot be encoded fails Send rather than this call, which
// answers nothing (QueueWrite).
func QueueNodes(b *store.WriteBatch, write Uncaused) {
	queueNodes(b, write.Nodes, write.mark)
}

// Caused is QueueCaused run at once, as a batch of its own, for a writer with no write phase. Its
// error is the batch's: a statement that fails, or a tuple that cannot be encoded, fails Send.
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
func QueueCaused(b *store.WriteBatch, write Uncaused, event int64) {
	legacy := event
	queueAppend(b, appending{
		tuples: len(write.Tuples),
		tuple: func(index int) Tuple {
			held := write.Tuples[index]
			tuple := held.Tuple
			tuple.SourceEvent = event
			if held.LegacyFromCause && tuple.LegacySourceSeq == nil && event != NoEvent {
				tuple.LegacySourceSeq = &legacy
			}
			return tuple
		},
		nodes: write.Nodes, mark: write.mark, baseAt: write.baseAt, cause: &event, sentinel: write.sentinel,
	})
}

// The two base attributes, as the kind of the tuple that states each spells it. graph_nodes
// carries a column for each, which is that attribute's projection, and nodeArm states the same
// two for a node the publisher's record makes.
const (
	AttributeVisibility = "visibility"
	AttributeState      = "state"
)

// baseAttributes is the set IsBaseAttribute reads, held once. No other attribute's values may be
// stored under one of these kinds: a value tuple of one of them out of a node reads as that node's
// own visibility or state, whoever wrote it, so an attribute a type declares under one of these
// names is stored under another name (the store name the supertag seed writes, migration
// 20261109051232_graph_supertags). Tests outside this package read a copy
// (graphstoretest.BaseAttributes), which TestGraphstoretestBaseAttributesAreTheStoresOwn holds
// equal to this.
var baseAttributes = [...]string{AttributeVisibility, AttributeState}

// IsBaseAttribute is whether kind is the tuple kind of a base attribute (baseAttributes).
func IsBaseAttribute(kind string) bool { return slices.Contains(baseAttributes[:], kind) }

// The base attributes a node write states for one node, as its mark carries them (nodeMarks).
const (
	markVisibility = 1 << iota
	markState
)

// nodeMarks draws the mark a node write leaves on each row it made or moved, until the statement
// that states that write's facts reads it and moves the row onto its cause (queueAppend): a
// negative source_event, -(mark*markSpan + attributes), where mark names the write and
// attributes are the base attributes it states for the node (markVisibility, markState: both for
// a node it made). No event has a negative id, and a row carries the mark only between two
// statements of one transaction, the node write and the append, which run in one write phase or
// one after the other in a transaction that holds the node's row from the first: no other
// transaction sees it, and the transaction's commit follows the append. That is how the append
// knows which nodes the write made and which it moved, and which attribute of each moved, without
// reading the node write's result back in Go between the two.
//
// The mark is drawn from a counter of the process, so two writes of one transaction never share
// one. A node two writes of one transaction write before the first states its facts carries the
// second's mark, so a writer states the facts of each node write before it writes the same node
// again: every writer here queues its append right after its event.
var nodeMarks atomic.Int64

// nextNodeMark is a mark no earlier write of this process drew, kept far below the bound a
// negative source_event spelling it must stay above.
func nextNodeMark() int64 {
	return nodeMarks.Add(1)%(1<<40) + 1
}

// markSpan is how many values the attributes a mark carries can take, every set of markVisibility
// and markState: a mark is spelled mark*markSpan + attributes, so neither part reaches the other.
const markSpan = (markVisibility | markState) + 1

// markSQL is the source_event a node write leaves on a row: -(mark*markSpan + attributes), mark
// bound as the parameter at markParam.
func markSQL(markParam, attributes string) string {
	return `-(` + markParam + `::bigint * ` + strconv.Itoa(markSpan) + ` + ` + attributes + `)`
}

// markedBy is the condition that row's source_event is the mark the write at markParam left.
func markedBy(row, markParam string) string {
	return `(` + row + `.source_event < 0 and (-` + row + `.source_event) / ` + strconv.Itoa(markSpan) + ` = ` + markParam + `::bigint)`
}

// markAttributes is the base attributes the mark on row's source_event says its write states
// (markVisibility, markState), read where markedBy holds.
func markAttributes(row string) string {
	return `((-` + row + `.source_event) % ` + strconv.Itoa(markSpan) + `)::integer`
}

// queueNodes queues the write of nodes, in two statements, each of whose rows arrive in ascending
// (kind, id): a write touching several nodes takes its row locks in that order, so two such
// writes cannot each hold the row the other waits for. The first inserts every node the store
// does not hold and locks every node it does, at the level an update of that row takes, changing
// nothing in it; the second moves supertag, visibility, state and attributes on each row whose
// columns differ, and leaves made_at and made_by as the row's own first write left them - the row
// is the projection of its tuples and may be written again, and a later write that learns the
// event id has to be able to move source_event. A node the write restates (Node.Restates) keeps
// its row's visibility and attributes, and its row's state unless that state or the node's is
// gone: its writer says only whether the node is there, so the row moves to gone when the writer
// says gone, and to the writer's state only from gone. Both statements leave mark, which the
// write's caller drew (nextNodeMark), on the rows they made or moved (nodeMarks).
//
// The rows carry the base attributes the write states, one attribute at a time, for the append
// to state. A node the insert made states `visibility` and `state`, which is the node carrying
// both from its first write. A node that was already there states `visibility` where this write
// moved its stored visibility, and `state` where this write moved its stored state, each on its
// own: a write that moves one of the two states that one, and a node written again with neither
// moved states neither, so a re-index of a document nothing moved appends nothing. An attribute a
// restating write kept did not move, so nothing is stated of it, and one it moved moved to the
// value the node carries, which is what its tuple states: the log never says a restated node went
// live while its row stayed at the state another write gave it. The two are compared separately
// because they move separately - a node hidden from a project keeps its state, and a deleted
// block keeps its visibility - and a row whose column moved while the log said nothing would be a
// row no appended fact explains, which nothing may mend later, since no tuple is ever updated.
// Each tuple carries the time, the writer and the cause of the node write that states it.
//
// Both stored values are compared on the row the second statement updates. Under READ COMMITTED
// that statement reads a snapshot of its own, taken once the first holds every row the write
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
// second's insert waits on the first's, then finds the row there and locks it, and its second
// statement compares against what the first committed. The pass that writes a block's node holds
// its document's owner lock before it reaches this.
//
// Neither statement joins the nodes it was handed against another list it was handed: each reads
// them once, and the second meets graph_nodes through its primary key. Planned without the
// arrays' sizes, as a cached statement is planned from its sixth run on, a join of two such lists
// is a nested loop that rescans one per row of the other, which measured 3,009 ms against 193 ms
// over a 1,999-block version, so what the write did to each node rides on the node's own row
// instead (nodeMarks).
func queueNodes(b *store.WriteBatch, nodes []Node, mark int64) {
	if len(nodes) == 0 {
		return
	}
	// Each statement builds its arguments when the write phase sends it (store.WriteBatch's
	// QueueBuilt). The move compares six of the insert's arrays, which the insert's build hands it
	// (shared): Send builds statements in the order they were queued, so the insert's comes first.
	var shared []any
	markParam := "$" + strconv.Itoa(len(nodeArrayTypes)+1)
	b.QueueBuilt("write graph nodes", `
		insert into graph_nodes (`+nodeColumns+`)
		select kind, id, supertag, visibility, state, attributes, made_at, made_by, `+markSQL(markParam, strconv.Itoa(markVisibility|markState))+`
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
		return append(args, mark), nil
	})
	// The move reads, beside the five columns it compares, whether the write restates each node
	// (Node.Restates): a node the insert made has no stored state, visibility or attributes to keep.
	b.QueueBuilt("write graph nodes", `
		with incoming as (
			select kind, id, supertag, visibility, state, attributes, restates
			from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::jsonb[], $7::boolean[])
				as node(kind, id, supertag, visibility, state, attributes, restates)
		)
		update graph_nodes set
			supertag = incoming.supertag,
			visibility = `+writtenVisibility+`,
			state = `+writtenState+`,
			attributes = `+writtenAttributes+`,
			source_event = `+markSQL("$8", `case when graph_nodes.visibility is distinct from `+writtenVisibility+` then `+strconv.Itoa(markVisibility)+` else 0 end
				+ case when graph_nodes.state is distinct from `+writtenState+` then `+strconv.Itoa(markState)+` else 0 end`)+`
		from incoming
		where graph_nodes.kind = incoming.kind and graph_nodes.id = incoming.id
		  and (graph_nodes.supertag is distinct from incoming.supertag
		       or graph_nodes.visibility is distinct from `+writtenVisibility+`
		       or graph_nodes.state is distinct from `+writtenState+`
		       or graph_nodes.attributes is distinct from `+writtenAttributes+`)
	`, func() ([]any, error) {
		compared := shared
		shared = nil
		if compared == nil {
			args, err := nodeParams(nodes)
			if err != nil {
				return nil, err
			}
			compared = args[:6:6]
		}
		restates := make([]bool, len(nodes))
		for index := range nodes {
			restates[index] = nodes[index].Restates
		}
		return append(compared, restates, mark), nil
	})
}

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

// appending is one append of a write's tuples (queueAppend): the caller's own, and the base
// attributes its node write states for the nodes it marked.
type appending struct {
	// tuples is how many tuples the caller states itself, and tuple the one at an index, in the
	// order the caller stated them, with the source_event and the legacy_source_seq it is written
	// with. It is read where the append binds it rather than copied out: a version's block index
	// states one tuple a block. The append also reads them for a base attribute the writer states
	// itself, which it does not state twice.
	tuples int
	tuple  func(index int) Tuple
	// nodes are the nodes the write wrote, and mark what its node write left on the rows it made
	// or moved (nodeMarks), 0 for none.
	nodes []Node
	mark  int64
	// baseAt is how many of tuples the base attributes follow.
	baseAt int
	// cause is the event every base attribute and every node moved here takes, nil for a write
	// whose nodes each carry their own (Node.SourceEvent).
	cause *int64
	// sentinel says every node still at the sentinel is moved onto cause too.
	sentinel bool
}

// queueAppend queues the statement that appends a write's tuples and moves its nodes onto their
// cause, one statement up to appendRows of them: the relations first and then the attributes,
// each in the order the caller stated them, with the base attributes the node write states
// standing after the first baseAt of the caller's tuples, one node at a time in the order the
// nodes were given, visibility before state (nodeMarks); and, beside them, every node the node
// write marked set at its cause, with, where sentinel says so, every node still at the sentinel.
// The rows the move reads are the ones the node write already locked, so the statement takes no
// node row the write did not hold before, and it reads the marks as its own snapshot holds them,
// before its own move.
//
// Neither the read of the marks nor the move joins the nodes it was handed to graph_nodes: each
// node probes its own row through graph_nodes' primary key, in a subquery behind an offset 0 the
// planner cannot flatten, as queueNodes' move meets the table through that key. Joined plainly,
// the mark is a filter the planner estimates at one row of a table analyzed near empty, whose
// statistics do not count the rows the node write has just written, and it walks graph_nodes and
// rescans the whole array for each marked row it finds there: a write of 2,000 nodes removed
// 3,998,000 rows by its join filter, and a version of 150,000 blocks would run for hours. An
// update cannot take a lateral on the table it updates, so the move updates the rows its own
// probe found, by their key: the mark and the sentinel are read off the probed row, and the
// update carries no condition of its own on graph_nodes for the planner to estimate at one row
// (TestTheNodeWritesAppendProbesEachNodeOfItsBatchOnce). The probed row is the row the update
// moves, since every row the move sets is one this transaction's node write holds, so no other
// writer moves it in between.
//
// The tuples carry two repeat keys: one event states one fact once, so a writer that is retried
// appends the same row again, and the insert drops it. One insert carries both, so it names no
// conflict target, and every unique index of graph_tuples is its arbiter: the two repeat keys,
// each partial on `source_event <> 0`, so a tuple at the sentinel is appended unconditionally.
//
// A write of more than appendRows tuples and nodes is appended as several such statements in a
// row, each over the next appendRows of them in that same order, so the rows land in the order
// one statement would give them (appendSegments). A statement's arguments are encoded whole
// before any of it is sent, in a buffer that grows by doubling and keeps each earlier size alive
// for the arguments it already holds: one statement over a version's 150,001 block nodes and
// their 150,001 tuples bound some 81 MB of arguments and held 377 MB of encoding at the heap's
// peak.
func queueAppend(b *store.WriteBatch, write appending) {
	if write.tuples == 0 && write.mark == 0 && !(write.sentinel && len(write.nodes) > 0) {
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
}

// appendRows is the most tuples and nodes one statement of an append binds together: a write
// of a few thousand rows, which is every write a request makes, is one statement, and a version
// of 150,001 blocks is 37 of about 2 MB of arguments each.
const appendRows = 8192

// appendOrder is the indexes of the caller's tuples in the order the append writes them, with
// each one's place (its major): the relations first and then the attributes, each in the order the
// caller stated them, and the base attributes the node write states at the place among the
// attributes the caller's baseAt names, baseSlot, which no tuple of the caller takes. before is how
// many of the tuples stand before that place.
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
	// The places are every number from 0 up but baseSlot, so a tuple's index in append order is
	// its place, less one past baseSlot.
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

// appendSegment is one statement of an append: the caller's tuples [tuplesFrom, tuplesTo) in
// append order, and the nodes [nodesFrom, nodesTo) whose base attributes it states and which it
// moves onto their cause.
type appendSegment struct{ tuplesFrom, tuplesTo, nodesFrom, nodesTo int }

// appendSegments cuts an append into statements of at most rows tuples and nodes each, in the
// append's own order: the tuples before the base slot, then the nodes, whose base attributes
// stand at that slot, then the tuples after it. A write of rows or fewer is one statement, and
// one with nothing to append is none.
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

// tupleAt is how many of the caller's tuples stand before place at of the append's order, whose
// first before places are tuples, the next nodes places nodes, and the rest tuples again.
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

// queueAppendStatement queues one statement of an append (queueAppend): the caller's tuples at
// the indexes order names, at their places (majors), and, where nodes is not empty, the base
// attributes those nodes' write states at baseSlot and their move onto their cause. Its arguments
// are built when the write phase sends it (store.WriteBatch's QueueBuilt).
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
	stated := `select ` + tupleSelect + `, major, 0::bigint as minor
		from unnest(` + tupleArrays(1) + `, $11::bigint[])
			as tuple(` + tupleWriteColumns + `, major)`
	if len(nodes) == 0 {
		b.QueueBuilt("append graph tuples", `
			insert into graph_tuples (`+tupleWriteColumns+`)
			select `+tupleWriteColumns+` from (`+stated+`) appended
			order by major, minor
			on conflict do nothing
		`, build)
		return
	}
	// The node arrays follow the tuples' ten and their places, and the mark, the base slot and the
	// sentinel follow the node arrays (appendedNodeParams).
	at := tupleArrayCount + 2
	markParam := "$" + strconv.Itoa(at+appendedNodeArrays)
	baseParam, sentinelParam := "$"+strconv.Itoa(at+appendedNodeArrays+1), "$"+strconv.Itoa(at+appendedNodeArrays+2)
	b.QueueBuilt("append graph tuples", `
		with written as (
			select kind, id, visibility, state, made_at, made_by, cause, skip, position
			from unnest(`+arrayList(at, "text", "text", "text", "text", "timestamptz", "jsonb", "bigint", "integer")+`)
				with ordinality as node(kind, id, visibility, state, made_at, made_by, cause, skip, position)
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
			       `+baseParam+`::bigint as major, marked.position * 2 + attribute.ordinal as minor
			from marked
			cross join lateral (values
				(`+strconv.Itoa(markVisibility)+`, 0, '`+AttributeVisibility+`', marked.visibility),
				(`+strconv.Itoa(markState)+`, 1, '`+AttributeState+`', marked.state)
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
	`, build)
}

// appendedNodeParams are the appendedNodeArrays arrays the append reads nodes through, by
// position: what each node's base attributes state (its kind, id, visibility, state, made_at,
// made_by), the cause it is moved onto and its base attributes are stated under - cause where the
// write has one for all of its nodes, else the node's own - and the base attributes the writer
// states itself (own), which the append leaves out.
func appendedNodeParams(nodes []Node, cause *int64, own map[baseAttribute]struct{}) ([]any, error) {
	kinds := make([]string, len(nodes))
	ids := make([]string, len(nodes))
	visibilities := make([]string, len(nodes))
	states := make([]string, len(nodes))
	madeAts := make([]time.Time, len(nodes))
	madeBys := make([][]byte, len(nodes))
	causes := make([]int64, len(nodes))
	skips := make([]int32, len(nodes))
	var writers writerEncoder
	for index, node := range nodes {
		madeBy, err := writers.encode(node.MadeBy)
		if err != nil {
			return nil, fmt.Errorf("encode writer of node %s/%s: %w", node.Kind, node.ID, err)
		}
		kinds[index], ids[index] = node.Kind, node.ID
		visibilities[index], states[index] = node.Visibility, node.State
		madeAts[index], madeBys[index] = node.MadeAt, madeBy
		causes[index] = node.SourceEvent
		if cause != nil {
			causes[index] = *cause
		}
		if _, held := own[baseAttribute{node.Kind, node.ID, AttributeVisibility}]; held {
			skips[index] |= markVisibility
		}
		if _, held := own[baseAttribute{node.Kind, node.ID, AttributeState}]; held {
			skips[index] |= markState
		}
	}
	return []any{kinds, ids, visibilities, states, madeAts, madeBys, causes, skips}, nil
}

// appendedNodeArrays is how many arrays appendedNodeParams binds.
const appendedNodeArrays = 8

// baseAttribute is one base attribute of one node, the key a write is read by to see whether it
// states that attribute itself.
type baseAttribute struct{ kind, id, attribute string }

// statedAttributes are the base attributes the write states itself, read off the tuples it
// handed in (tuple, of which there are tuples). No writer of this tree states one today - each
// leaves them to the node write - but a writer that did would keep that statement: one write
// states one fact once.
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

// tupleArrays is the parameter list of the tupleArrayCount arrays a tuple append binds, numbered
// from first, in tupleWriteColumns' order (tupleParams).
func tupleArrays(first int) string {
	return arrayList(first, "text", "text", "text", "text", "text", "timestamptz", "jsonb", "bigint", "bigint", "boolean")
}

// tupleArrayCount is how many arrays tupleArrays lists and tupleParams binds.
const tupleArrayCount = 10

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
