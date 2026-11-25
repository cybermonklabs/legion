package graphstore

import (
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// appending is one append of a write's tuples (queueAppend): the caller's own, the base
// attributes its node write states for the nodes it marked, and the changed_by_run each node it
// changed owes the run it is made for.
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
	// appended, where Counting set it, counts the tuples the append appends (queueAppend).
	appended *int64
	// run is the run of the command bar the write is made for, and changes the changed_by_run each
	// node the write changed owes it (changedByRunMarks), none for a write made for no run.
	run     string
	changes []Tuple
}

// queueAppend queues the statement that appends a write's tuples and moves its nodes onto their
// cause, one statement up to appendRows of them: the relations first and then the attributes,
// each in the order the caller stated them, with the base attributes the node write states
// standing after the first baseAt of the caller's tuples, one node at a time in the order the
// nodes were given, visibility, state, project, parties (nodeMarks); and, beside them, every node
// the node write marked set at its cause, with, where sentinel says so, every node still at the
// sentinel. The rows the move reads are the ones the node write already locked, so the statement
// takes no node row the write did not hold before, and it reads the marks as its own snapshot
// holds them, before its own move.
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

// markedAttributeValues are the values rows the append's based reads each base attribute of a
// marked node through (BaseAttributeValues), rendered once.
var markedAttributeValues = BaseAttributeValues([...]string{`marked.visibility`, `marked.state`, `marked.project`, `marked.parties`})

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
	stated := orderedTuples(tupleArrayCount + 1)
	if len(nodes) == 0 {
		counted(b.QueueBuilt("append graph tuples", `
			insert into graph_tuples (`+tupleWriteColumns+`)
			select `+tupleWriteColumns+` from (`+stated+`) appended
			where `+KeptHeadRows+`
			order by major, minor
			on conflict do nothing
		`, build), write.appended)
		return
	}
	// The node arrays follow the tuples' ten and their places, and the mark, the base slot and the
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
			where graph_nodes.kind = moved.kind and graph_nodes.id = moved.id and `+KeptHeadRows+`
		)
		insert into graph_tuples (`+tupleWriteColumns+`)
		select `+tupleWriteColumns+` from (
			`+stated+`
			union all
			select `+tupleWriteColumns+`, major, minor from based
		) appended
		where `+KeptHeadRows+`
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
			`+orderedTuples(majorsAt)+`
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
		where `+KeptHeadRows+`
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

// orderedTuples is the select of the tuples an append binds from the first parameter on
// (tupleParams, tupleArrays), each at the place in the append the bigint array bound at majors
// names, and first among the rows that share that place.
func orderedTuples(majors int) string {
	return `select ` + tupleSelect + `, major, 0::bigint as minor
		from unnest(` + tupleArrays(1) + `, $` + strconv.Itoa(majors) + `::bigint[])
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

// appendedNodeParams are the appendedNodeArrays arrays the append reads nodes through, by
// position: what each node's base attributes state (its kind, id, visibility, state, project,
// parties, made_at, made_by), the cause it is moved onto and its base attributes are stated under
// - cause where the write has one for all of its nodes, else the node's own - and the base
// attributes the writer states itself (own), which the append leaves out.
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
	var writers writerEncoder
	for index, node := range nodes {
		madeBy, err := writers.encode(node.MadeBy)
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

// statedAttributes are the base attributes the write states itself, read off the tuples it
// handed in (tuple, of which there are tuples): a document made where an orphaned node stands,
// and the backfill's write of such a document, state its node gone and then live themselves. One
// write states one fact once, so the node write states none of those attributes beside them.
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
