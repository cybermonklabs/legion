package graphstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
// earlier one is derived in the later statement's SQL: an event's id (LatestEvent), and which
// nodes a node write made or moved, which the node rows themselves carry until the statement that
// states those facts reads them (nodeMark).

// writeFunctions are the functions a write of graph_nodes and graph_tuples from outside this
// package goes through: Write, WriteUncaused and Caused in a caller's transaction, and QueueWrite,
// QueueNodes and QueueCaused, the same writes queued on a write phase's batch; WriteArms and
// WriteArmsUncaused spliced into a caller's statement; Authored, Reached and SessionRecorded,
// which run those arms for the writers that record an author, a reached session and a session an
// ask names, and QueueAuthored, QueueReached and QueueSessionRecorded, which queue them;
// EraseNode, the erase's delete of a node and its tuples; and FillScopes, the backfill's fill of
// the project of a node stored with none. Each is a function of the package, never a method, so a
// call of one is named by the package wherever it is made.
var writeFunctions = [...]string{"Write", "WriteUncaused", "WriteArms", "WriteArmsUncaused", "Caused",
	"QueueWrite", "QueueNodes", "QueueCaused",
	"Authored", "Reached", "SessionRecorded", "QueueAuthored", "QueueReached", "QueueSessionRecorded",
	"EraseNode", "FillScopes"}

// WriteFunctions is the name of every function of this package a write of the store from outside
// it goes through (writeFunctions), a fresh slice for each caller. It is the one list the source
// scans that hold the store's writers read: this package's register of their call sites
// (TestEveryStoreWriteCallSiteIsListed), api's scan of the writers of the projected tables
// (TestNoProjectionRowIsWrittenWithoutTheStore) and docs' scan of the passes that write the store
// (TestTheIndexPassesAreStoreWritersAndNoRepairExists).
func WriteFunctions() []string { return slices.Clone(writeFunctions[:]) }

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
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error { return QueueWrite(b, nodes, tuples) })
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
func QueueWrite(b *store.WriteBatch, nodes []Node, tuples []Tuple) error {
	mark := int64(0)
	if len(nodes) > 0 {
		mark = nextNodeMark()
	}
	if err := queueNodes(b, nodes, mark, tuples); err != nil {
		return err
	}
	return queueAppend(b, appending{tuples: tuples, nodes: nodes, mark: mark, own: tuples})
}

// Uncaused is a write of this store whose cause did not exist while its node write was queued,
// held by its writer until it does. Such a writer orders itself node row, projection row, event,
// tuples: the node first because the projection row's foreign key names it, the event after both
// because it records what the request did, and the tuples last because each one holds the event
// that caused it and no tuple may be updated to learn it afterwards. So its nodes go in at the
// sentinel, marked with what the node write did to each (nodeMark), and QueueCaused, queued after
// the event in the same transaction, moves them onto it and appends every fact of the write under
// it.
//
// It is the one shape of that write for every writer it applies to: a comment's request (api's
// commentStoreWrite), a version's block index (refs.BlockIndexing), a follow of an ask
// (asks.Followed) and the settle of a delivery attempt (Reached). A writer with nothing to state
// holds the zero Uncaused, for which Caused writes nothing.
type Uncaused struct {
	// Nodes are the nodes the write wrote. QueueCaused moves each its node write made or moved
	// onto the event, and, for a write of PlanUncaused, each still at the sentinel, so a node an
	// earlier write caused keeps its cause.
	Nodes []Node
	// Tuples are the facts the write states, each appended under the event by Caused, the base
	// attributes its node write stated among them, which stand after the first baseAt of these.
	Tuples []UncausedTuple
	// mark is what the node write left on the rows it made or moved (nodeMark), 0 for a write
	// that marked none.
	mark int64
	// baseAt is how many of Tuples the base attributes the node write states follow.
	baseAt int
	// sentinel says QueueCaused also moves onto the event every node of Nodes still at the
	// sentinel, as PlanUncaused's writer holds all of them for its cause; a follow's or a settle's
	// node (the arms) is caused only where that write made it.
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

// WriteUncaused writes the nodes of the write PlanUncaused holds at once, as a batch of their own
// (QueueNodes), for a writer with no write phase to queue them on, and answers that write.
func WriteUncaused(ctx context.Context, tx pgx.Tx, nodes []Node, stated []Tuple) (Uncaused, error) {
	write := PlanUncaused(nodes, stated)
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error { return QueueNodes(b, write) }); err != nil {
		return Uncaused{}, err
	}
	return write, nil
}

// PlanUncaused is a write whose nodes are written before the event that causes it exists, held
// for that event, and planned before anything is queued: the nodes, at the sentinel, the writer's
// own facts, stated, and the mark its node write will leave on the rows it makes or moves
// (nodeMark), from which the statement QueueCaused queues derives the base attributes that node
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
		write.Nodes = make([]Node, len(nodes))
		for index, node := range nodes {
			node.SourceEvent = NoEvent
			write.Nodes[index] = node
		}
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
// each it makes or moves left marked for the append QueueCaused queues. It appends nothing.
func QueueNodes(b *store.WriteBatch, write Uncaused) error {
	stated := make([]Tuple, len(write.Tuples))
	for index, held := range write.Tuples {
		stated[index] = held.Tuple
	}
	return queueNodes(b, write.Nodes, write.mark, stated)
}

// Caused is QueueCaused run at once, as a batch of its own, for a writer with no write phase.
func Caused(ctx context.Context, tx pgx.Tx, write Uncaused, event int64) error {
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error { return QueueCaused(b, write, event) })
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
func QueueCaused(b *store.WriteBatch, write Uncaused, event int64) error {
	tuples := make([]Tuple, len(write.Tuples))
	for index, held := range write.Tuples {
		tuple := held.Tuple
		tuple.SourceEvent = event
		if held.LegacyFromCause && tuple.LegacySourceSeq == nil && event != NoEvent {
			cause := event
			tuple.LegacySourceSeq = &cause
		}
		tuples[index] = tuple
	}
	return queueAppend(b, appending{
		tuples: tuples, nodes: write.Nodes, mark: write.mark, baseAt: write.baseAt,
		cause: &event, sentinel: write.sentinel, own: tuples, appended: write.appended,
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

// baseAttributes is the set BaseAttributes answers and IsBaseAttribute reads, held once.
var baseAttributes = [...]string{AttributeVisibility, AttributeState, AttributeProject, AttributeParties}

// ErrProjectMoved is a write that names a project for a node the store already holds in another:
// a node's project is what every read of it is limited by, so it never moves once stated, and the
// write is refused rather than the node read under two projects' rules over its life. A write
// that names no project moves nothing and is not refused.
var ErrProjectMoved = errors.New("a node's project never moves")

// BaseAttributes is the tuple kind of every base attribute, a fresh slice for each caller. No
// other attribute's values may be stored under one of these kinds: a value tuple of one of them
// out of a node reads as that node's own visibility, state, project or parties, whoever wrote it,
// so an attribute a type declares under one of these names is stored under another name
// (pmdoc/schemanodes.StoreName).
func BaseAttributes() []string { return slices.Clone(baseAttributes[:]) }

// IsBaseAttribute is whether kind is the tuple kind of a base attribute (BaseAttributes), asked
// without building the list.
func IsBaseAttribute(kind string) bool { return slices.Contains(baseAttributes[:], kind) }

// The base attributes a node write states for one node, as nodeMark carries them, and
// markAttributes the room the mark leaves for them.
const (
	markVisibility = 1 << iota
	markState
	markProject
	markParties
	markAttributes
)

// markMade is the attributes a node write states for a node it makes, in the SQL of a select over
// the arrays it binds (nodeArrays): visibility and state always, and project and parties where the
// node carries each.
var markMade = strconv.Itoa(markVisibility|markState) +
	` + case when project_key is not null then ` + strconv.Itoa(markProject) + ` else 0 end` +
	` + case when parties is not null then ` + strconv.Itoa(markParties) + ` else 0 end`

// nodeMark is what a node write leaves on each row it made or moved, until the statement that
// states that write's facts reads it and moves the row onto its cause (queueAppend): a negative
// source_event, -(mark*markAttributes + attributes), where mark names the write and attributes
// are the base attributes it states for the node (markVisibility, markState, and markProject and
// markParties for a node it made carrying them or a node already there it gave them). No
// event has a negative id, and a row carries the mark only between two statements of one
// transaction, the node write and the append, which run in one write phase or one after the other
// in a transaction that holds the node's row from the first: no other transaction sees it, and
// the transaction's commit follows the append. That is how the append knows which nodes the write
// made and which it moved, and which attribute of each moved, without reading the node write's
// result back in Go between the two.
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

// markSQL is the source_event a node write leaves on a row: -(mark*markAttributes + attributes),
// mark bound as the parameter at markParam.
func markSQL(markParam, attributes string) string {
	return `-(` + markParam + `::bigint * ` + strconv.Itoa(markAttributes) + ` + ` + attributes + `)`
}

// markedBy is the condition that row's source_event is the mark the write at markParam left.
func markedBy(row, markParam string) string {
	return `(` + row + `.source_event < 0 and (-` + row + `.source_event) / ` + strconv.Itoa(markAttributes) + ` = ` + markParam + `::bigint)`
}

// markedAttributes is the base attributes the mark on row's source_event names.
func markedAttributes(row string) string {
	return `((-` + row + `.source_event) % ` + strconv.Itoa(markAttributes) + `)::integer`
}

// queueNodes queues the write of nodes, in statements each of whose rows arrive in ascending
// (kind, id): a write touching several nodes takes its row locks in that order, so two such
// writes cannot each hold the row the other waits for. The first inserts every node the store
// does not hold and locks every node it does, at the level an update of that row takes, changing
// nothing in it; the last moves supertag, visibility, state, attributes and parties on each row
// whose columns differ, fills a project the row does not hold yet, and leaves made_at and made_by
// as the row's own first write left them - the row is the projection of its tuples and may be
// written again, and a later write that learns the event id has to be able to move source_event.
// A node written with no project or no parties keeps what its row holds of each: that writer
// knows neither, and is no word that the node has none. A node the write restates
// (Node.Restates) keeps its row's visibility, and its row's state unless that state or the node's
// is gone: its writer says only whether the node is there, so the row moves to gone when the
// writer says gone, and to the writer's state only from gone. Both statements leave mark, which
// the write's caller drew (nextNodeMark), on the rows they made or moved (nodeMark).
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
// second's insert waits on the first's, then finds the row there and locks it, and its later
// statements compare against what the first committed. The pass that writes a block's node holds
// its document's owner lock before it reaches this.
//
// No statement joins the nodes it was handed against another list it was handed: each reads
// them once, and the later ones meet graph_nodes through its primary key. Planned without the
// arrays' sizes, as a cached statement is planned from its sixth run on, a join of two such lists
// is a nested loop that rescans one per row of the other, which measured 3,009 ms against 193 ms
// over a 1,999-block version, so what the write did to each node rides on the node's own row
// instead (nodeMark).
func queueNodes(b *store.WriteBatch, nodes []Node, mark int64, stated []Tuple) error {
	if len(nodes) == 0 {
		return nil
	}
	args, err := nodeParams(nodes)
	if err != nil {
		return err
	}
	markParam := "$" + strconv.Itoa(len(nodeArrayTypes)+1)
	b.Queue("write graph nodes", `
		insert into graph_nodes (`+nodeColumns+`)
		select kind, id, supertag, visibility, state, attributes, made_at, made_by, `+markSQL(markParam, markMade)+`,
		       project_key, string_to_array(parties, ' ')
		from unnest(`+nodeArrays(1)+`) as node(`+nodeColumns+`)
		order by kind, id
		on conflict (kind, id) do update set source_event = graph_nodes.source_event
		where false
	`, append(args, mark)...)
	projects, parties := args[9], args[10]
	if slices.ContainsFunc(nodes, func(node Node) bool { return node.Project != "" }) {
		b.Queue("write graph nodes", `
			select incoming.kind, incoming.id, incoming.project_key, held.project_key
			from unnest($1::text[], $2::text[], $3::text[]) as incoming(kind, id, project_key)
			join graph_nodes held on held.kind = incoming.kind and held.id = incoming.id
			where incoming.project_key is not null and held.project_key is not null
			  and held.project_key <> incoming.project_key
			order by incoming.kind, incoming.id
			limit 1
		`, args[0], args[1], projects).QueryRow(func(row pgx.Row) error {
			var kind, id, named, held string
			switch err := row.Scan(&kind, &id, &named, &held); {
			case errors.Is(err, pgx.ErrNoRows):
				return nil
			case err != nil:
				return err
			}
			return fmt.Errorf("write graph node %s/%s in project %s, which the store holds in %s: %w", kind, id, named, held, ErrProjectMoved)
		})
	}
	// The move reads, beside the columns it compares, whether the write restates each node
	// (Node.Restates): a node the insert made has no stored state or visibility to keep.
	restates := make([]bool, len(nodes))
	for index := range nodes {
		restates[index] = nodes[index].Restates
	}
	// A node already there whose base attribute the caller states itself - a document made where
	// an orphaned node stands states that node gone and then live in tuples of its own - moves
	// nothing of its own row's base attributes, but is marked all the same (with no attribute), so
	// the append moves it onto the cause the caller's statements carry. A node the insert above
	// just made carries the insert's mark (markMade) already, which names every base attribute it
	// carries, so it takes no second mark: an empty one over it would leave its visibility, its
	// project and its parties stated by nothing.
	own := statedAttributes(stated)
	states := make([]bool, len(nodes))
	for index, node := range nodes {
		for _, attribute := range baseAttributes {
			if _, held := own[baseAttribute{node.Kind, node.ID, attribute}]; held {
				states[index] = true
			}
		}
	}
	b.Queue("write graph nodes", `
		with incoming as (
			select kind, id, supertag, visibility, state, attributes, restates, project_key,
			       string_to_array(parties, ' ') as parties, states
			from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::jsonb[], $7::boolean[], $9::text[], $10::text[], $11::boolean[])
				as node(kind, id, supertag, visibility, state, attributes, restates, project_key, parties, states)
		)
		update graph_nodes set
			supertag = incoming.supertag,
			visibility = `+writtenVisibility+`,
			state = `+writtenState+`,
			attributes = incoming.attributes,
			project_key = coalesce(graph_nodes.project_key, incoming.project_key),
			parties = coalesce(incoming.parties, graph_nodes.parties),
			source_event = `+markSQL("$8", `case when graph_nodes.visibility is distinct from `+writtenVisibility+` then `+strconv.Itoa(markVisibility)+` else 0 end
				+ case when graph_nodes.state is distinct from `+writtenState+` then `+strconv.Itoa(markState)+` else 0 end
				+ case when `+fillsProject+` then `+strconv.Itoa(markProject)+` else 0 end
				+ case when `+movesParties+` then `+strconv.Itoa(markParties)+` else 0 end`)+`
		from incoming
		where graph_nodes.kind = incoming.kind and graph_nodes.id = incoming.id
		  and (graph_nodes.supertag is distinct from incoming.supertag
		       or graph_nodes.visibility is distinct from `+writtenVisibility+`
		       or graph_nodes.state is distinct from `+writtenState+`
		       or graph_nodes.attributes is distinct from incoming.attributes
		       or `+fillsProject+`
		       or `+movesParties+`
		       or (incoming.states and not `+markedBy("graph_nodes", "$8")+`))
	`, args[0], args[1], args[2], args[3], args[4], args[5], restates, mark, projects, parties, states)
	return nil
}

// fillsProject and movesParties are whether the move gives a row the project it does not hold
// yet, and the parties it holds otherwise or not at all, read as writtenVisibility is.
const (
	fillsProject = `(graph_nodes.project_key is null and incoming.project_key is not null)`
	movesParties = `(incoming.parties is not null and graph_nodes.parties is distinct from incoming.parties)`
)

// writtenVisibility and writtenState are the visibility and the state the move writes a row at,
// read off the row its update holds (graph_nodes, before the update) and the node it was handed
// (incoming): the node's own, but for a node the write restates (Node.Restates), which keeps the
// row's visibility, and the row's state unless the row's or the node's is gone. A state that
// moves therefore moves to the node's own, the value the append states of it. Each is spelled
// once and read three times, by the update's set, by its mark and by its condition, so a row a
// restating write keeps as it is takes no update.
const (
	writtenVisibility = `case when incoming.restates then graph_nodes.visibility else incoming.visibility end`
	writtenState      = `case when incoming.restates and incoming.state <> '` + StateGone + `'
				and graph_nodes.state <> '` + StateGone + `'
			then graph_nodes.state else incoming.state end`
)

// appending is one append of a write's tuples (queueAppend): the caller's own, and the base
// attributes its node write states for the nodes it marked.
type appending struct {
	// tuples are the caller's own, each with the source_event and the legacy_source_seq it is
	// written with.
	tuples []Tuple
	// nodes are the nodes the write wrote, and mark what its node write left on the rows it made
	// or moved (nodeMark), 0 for none.
	nodes []Node
	mark  int64
	// baseAt is how many of tuples the base attributes follow.
	baseAt int
	// cause is the event every base attribute and every node moved here takes, nil for a write
	// whose nodes each carry their own (Node.SourceEvent).
	cause *int64
	// sentinel says every node still at the sentinel is moved onto cause too.
	sentinel bool
	// own is what the writer states itself, read for a base attribute it states, which the
	// append does not state twice.
	own []Tuple
	// appended, where not nil, counts the tuples the statement appends.
	appended *int64
}

// queueAppend queues the one statement that appends a write's tuples and moves its nodes onto
// their cause: the relations first and then the attributes, each in the order the caller stated
// them, with the base attributes the node write states standing after the first baseAt of the
// caller's tuples, one node at a time in the order the nodes were given, visibility, state,
// project, parties (nodeMark); and, beside them, every node the node write marked set at its
// cause, with, where sentinel says so, every node still at the sentinel. The rows the move reads
// are the ones the node write already locked, so the statement takes no node row the write did
// not hold before, and it reads the marks as its own snapshot holds them, before its own move.
//
// The tuples carry two repeat keys: one event states one fact once, so a writer that is retried
// appends the same row again, and the insert drops it. One insert carries both, so it names no
// conflict target, and every unique index of graph_tuples is its arbiter: the two repeat keys,
// each partial on `source_event <> 0`, so a tuple at the sentinel is appended unconditionally.
func queueAppend(b *store.WriteBatch, write appending) error {
	if len(write.tuples) == 0 && write.mark == 0 && !(write.sentinel && len(write.nodes) > 0) {
		return nil
	}
	relations, values := 0, 0
	for _, tuple := range write.tuples {
		if tuple.ToKind == KindValue {
			values++
		} else {
			relations++
		}
	}
	// Each tuple's place in the append: the relations first, then the attributes, and the base
	// attributes at the place among the attributes the caller's baseAt names.
	order := make([]int64, len(write.tuples))
	relation, value, baseSlot := int64(0), int64(relations), int64(relations)
	for index, tuple := range write.tuples {
		if index == write.baseAt {
			baseSlot = value
			value++
		}
		if tuple.ToKind == KindValue {
			order[index] = value
			value++
		} else {
			order[index] = relation
			relation++
		}
	}
	if write.baseAt >= len(write.tuples) {
		baseSlot = value
	}
	params, err := tupleParams(write.tuples)
	if err != nil {
		return err
	}
	params = append(params, order)
	stated := `select ` + tupleSelect + `, major, 0::bigint as minor
		from unnest(` + tupleArrays(1) + `, $11::bigint[])
			as tuple(` + tupleWriteColumns + `, major)`
	if write.mark == 0 && !write.sentinel {
		counted(b.Queue("append graph tuples", `
			insert into graph_tuples (`+tupleWriteColumns+`)
			select `+tupleWriteColumns+` from (`+stated+`) appended
			order by major, minor
			on conflict do nothing
		`, params...), write.appended)
		return nil
	}
	nodeArgs, err := appendedNodeParams(write)
	if err != nil {
		return err
	}
	at := len(params) + 1
	params = append(params, nodeArgs...)
	markParam := "$" + strconv.Itoa(at+len(nodeArgs))
	params = append(params, write.mark, baseSlot, write.sentinel)
	baseParam, sentinelParam := "$"+strconv.Itoa(at+len(nodeArgs)+1), "$"+strconv.Itoa(at+len(nodeArgs)+2)
	counted(b.Queue("append graph tuples", `
		with written as (
			select kind, id, visibility, state, project, parties, made_at, made_by, cause, skip, position
			from unnest(`+arrayList(at, "text", "text", "text", "text", "text", "text", "timestamptz", "jsonb", "bigint", "integer")+`)
				with ordinality as node(kind, id, visibility, state, project, parties, made_at, made_by, cause, skip, position)
		),
		marked as (
			select written.*, `+markedAttributes("held")+` & ~written.skip as attributes
			from written join graph_nodes held on held.kind = written.kind and held.id = written.id
			where `+markedBy("held", markParam)+`
		),
		based as (
			select marked.kind as from_kind, marked.id as from_id, attribute.kind, '`+KindValue+`' as to_kind,
			       attribute.value as to_id, marked.made_at as at, marked.made_by as who,
			       `+causeOf("marked.cause")+` as source_event, null::bigint as legacy_source_seq, true as asserted,
			       `+baseParam+`::bigint as major, marked.position * `+strconv.Itoa(len(baseAttributes))+` + attribute.ordinal as minor
			from marked
			cross join lateral (values
				(`+strconv.Itoa(markVisibility)+`, 0, '`+AttributeVisibility+`', marked.visibility),
				(`+strconv.Itoa(markState)+`, 1, '`+AttributeState+`', marked.state),
				(`+strconv.Itoa(markProject)+`, 2, '`+AttributeProject+`', marked.project),
				(`+strconv.Itoa(markParties)+`, 3, '`+AttributeParties+`', marked.parties)
			) as attribute(bit, ordinal, kind, value)
			where marked.attributes & attribute.bit <> 0
		),
		settled as (
			update graph_nodes set source_event = `+causeOf("written.cause")+`
			from written
			where graph_nodes.kind = written.kind and graph_nodes.id = written.id
			  and (`+markedBy("graph_nodes", markParam)+`
			       or `+sentinelParam+`::boolean and graph_nodes.source_event = `+strconv.FormatInt(NoEvent, 10)+`
			          and written.cause <> `+strconv.FormatInt(NoEvent, 10)+`)
		)
		insert into graph_tuples (`+tupleWriteColumns+`)
		select `+tupleWriteColumns+` from (
			`+stated+`
			union all
			select `+tupleWriteColumns+`, major, minor from based
		) appended
		order by major, minor
		on conflict do nothing
	`, params...), write.appended)
	return nil
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

// appendedNodeParams are the ten arrays the append reads a write's nodes through, by position:
// what each node's base attributes state (its kind, id, visibility, state, project, parties,
// made_at, made_by), the cause it is moved onto and its base attributes are stated under, and the
// base attributes the writer states itself, which the append leaves out. A node carrying no
// project or no parties binds the empty string, which no mark names: the append states each only
// where its node write marked it (markMade, fillsProject, movesParties).
func appendedNodeParams(write appending) ([]any, error) {
	own := statedAttributes(write.own)
	kinds := make([]string, len(write.nodes))
	ids := make([]string, len(write.nodes))
	visibilities := make([]string, len(write.nodes))
	states := make([]string, len(write.nodes))
	projects := make([]string, len(write.nodes))
	parties := make([]string, len(write.nodes))
	madeAts := make([]time.Time, len(write.nodes))
	madeBys := make([][]byte, len(write.nodes))
	causes := make([]int64, len(write.nodes))
	skips := make([]int32, len(write.nodes))
	for index, node := range write.nodes {
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
		causes[index] = node.SourceEvent
		if write.cause != nil {
			causes[index] = *write.cause
		}
		// baseAttributes lists the four in mark order: visibility, state, project, parties.
		for bit, attribute := range baseAttributes {
			if _, held := own[baseAttribute{node.Kind, node.ID, attribute}]; held {
				skips[index] |= 1 << bit
			}
		}
	}
	return []any{kinds, ids, visibilities, states, projects, parties, madeAts, madeBys, causes, skips}, nil
}

// baseAttribute is one base attribute of one node, the key a write is read by to see whether it
// states that attribute itself.
type baseAttribute struct{ kind, id, attribute string }

// statedAttributes are the base attributes the write states itself, read off the tuples it
// handed in: a document made where an orphaned node stands, and the backfill's write of such a
// document, state its node gone and then live themselves. One write states one fact once, so the
// node write states none of those attributes beside them.
func statedAttributes(tuples []Tuple) map[baseAttribute]struct{} {
	stated := make(map[baseAttribute]struct{})
	for _, tuple := range tuples {
		if tuple.ToKind != KindValue || !IsBaseAttribute(tuple.Kind) {
			continue
		}
		stated[baseAttribute{tuple.FromKind, tuple.FromID, tuple.Kind}] = struct{}{}
	}
	return stated
}

// tupleArrays is the parameter list of the ten arrays a tuple append binds, numbered from first,
// in tupleWriteColumns' order (tupleParams).
func tupleArrays(first int) string {
	return arrayList(first, "text", "text", "text", "text", "text", "timestamptz", "jsonb", "bigint", "bigint", "boolean")
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
