package graphstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Write writes every node and appends every tuple inside the caller's transaction. It appends
// only: no statement here updates or deletes a tuple row, so the log a read walks is whatever
// every writer before it appended, in the order they appended it.
//
// Each node is written once per call: two rows of nodes carrying one (kind, id) are the caller's
// mistake, and Postgres names it rather than this picking a winner.
//
// The two base attributes every node carries are stated here and not by each writer, so that a
// node written through any writer of this store carries them: the block index, the comment
// path, the backfill and the seed all reach the store through this call. writeNodes says which
// nodes they are stated for, and the tuples it answers are appended with the caller's own.
//
// This is the whole of the write for a caller that has its cause in hand, or none at all: a
// backfill, a seed, a publisher. A caller whose event is appended after the node row - the
// comment path and the block index, whose projection rows carry a foreign key into graph_nodes
// and must therefore follow the node - takes WriteUncaused instead, so that the base attributes
// are appended under that event rather than under the sentinel.
func Write(ctx context.Context, tx pgx.Tx, nodes []Node, tuples []Tuple) error {
	base, err := writeNodes(ctx, tx, nodes, tuples)
	if err != nil {
		return err
	}
	if len(base) == 0 {
		return appendTuples(ctx, tx, tuples)
	}
	return appendTuples(ctx, tx, append(base, tuples...))
}

// Uncaused is a write of this store whose cause did not exist while it ran, held by its writer
// until it does. Such a writer orders itself node row, projection row, event, tuples: the node
// first because the projection row's foreign key names it, the event after both because it
// records what the request did, and the tuples last because each one holds the event that
// caused it and no tuple may be updated to learn it afterwards. So its nodes go in at the
// sentinel, and Caused, once the event is appended in the same transaction, moves them onto it
// and appends every fact of the write under it.
//
// It is the one shape of that write for every writer it applies to: a comment's request (api's
// commentStoreWrite), a version's block index (refs.IndexBlocks), a follow of an ask
// (asks.Followed) and the settle of a delivery attempt (Reached). A writer with nothing to state
// holds the zero Uncaused, for which Caused writes nothing.
type Uncaused struct {
	// Nodes are the nodes the write made or wrote at the sentinel. Caused moves each that is
	// still at the sentinel onto the event, so a node an earlier write caused keeps its cause.
	Nodes []Node
	// Tuples are the facts the write states, each appended under the event by Caused: the base
	// attributes its node write stated (WriteUncaused) and the writer's own.
	Tuples []UncausedTuple
}

// UncausedTuple is one fact an Uncaused write holds for its cause. LegacyFromCause marks a fact
// whose projection row takes the cause's own id as its source_seq: a block_refs row the block
// index inserts outside a first indexing is written with none, and refs.Stamp or docs'
// stampVersionEvent fills it with that event in the same transaction, so the tuple's
// legacy_source_seq is that event's id, which the writer cannot read back once the tuple is
// appended. A fact no row dates that way keeps the legacy_source_seq its writer gave it.
type UncausedTuple struct {
	Tuple
	LegacyFromCause bool
}

// WriteUncaused writes every node now, before the event that causes the write exists, and
// answers the write held for that event: the nodes, and the base attributes this node write
// states after the writer's own facts, stated, none of which it appends. The base attributes
// are facts of that same write, so they go in with the caller's own under the same event, which
// is what keeps a block a version stated gone carrying the event that deleted it. stated is read
// for one more thing besides: a base attribute the caller states itself is not stated twice.
func WriteUncaused(ctx context.Context, tx pgx.Tx, nodes []Node, stated []Tuple) (Uncaused, error) {
	base, err := writeNodes(ctx, tx, nodes, stated)
	if err != nil {
		return Uncaused{}, err
	}
	write := Uncaused{Nodes: nodes}
	if len(stated)+len(base) > 0 {
		write.Tuples = make([]UncausedTuple, 0, len(stated)+len(base))
	}
	for _, tuple := range stated {
		write.Tuples = append(write.Tuples, UncausedTuple{Tuple: tuple})
	}
	for _, tuple := range base {
		write.Tuples = append(write.Tuples, UncausedTuple{Tuple: tuple})
	}
	return write, nil
}

// Caused appends what write stated under event, the event that caused it, and moves the nodes it
// wrote onto that event: a node row is the projection of its tuples and may be written again,
// while a tuple may not be updated, which is why the tuples waited for this call rather than
// being stamped after it. event is NoEvent where no event recorded the write, and such a tuple
// carries the sentinel and no legacy_source_seq, as its row carries no source_seq either.
//
// It is a function of this package and not a method of Uncaused, so every call of it outside the
// package is a call of the store's writer by its package's name, which the register of this
// store's writers finds and holds to its lock steps (TestEveryStoreWriteCallSiteIsListed).
//
// The nodes are not written again: causeNodes moves the event onto the rows already written, in
// one statement over their addresses. Writing them again would pay a second node write per node
// for one column, and the upsert's own guard would refuse it, since nothing else about the node
// moved.
func Caused(ctx context.Context, tx pgx.Tx, write Uncaused, event int64) error {
	if err := causeNodes(ctx, tx, write.Nodes, event); err != nil {
		return err
	}
	if len(write.Tuples) == 0 {
		return nil
	}
	tuples := make([]Tuple, len(write.Tuples))
	for index, held := range write.Tuples {
		tuple := held.Tuple
		tuple.SourceEvent = event
		if held.LegacyFromCause && tuple.LegacySourceSeq == nil && event != NoEvent {
			tuple.LegacySourceSeq = &event
		}
		tuples[index] = tuple
	}
	return appendTuples(ctx, tx, tuples)
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

// writeNodes is two statements, each of whose rows arrive in ascending (kind, id): a write
// touching several nodes takes its row locks in that order, so two such writes cannot each hold
// the row the other waits for. The first inserts every node the store does not hold and locks
// every node it does, at the level an update of that row takes, changing nothing in it; the
// second moves supertag, visibility, state, attributes, parties and source_event on each row
// whose columns differ, fills a project the row does not hold yet, and leaves made_at and made_by
// as the row's own first write left them - the row is the projection of its tuples and may be
// written again, and a later write that learns the event id has to be able to move source_event.
// A node written with no project or no parties keeps what its row holds of each: that writer
// knows neither, and is no word that the node has none.
//
// It answers the base attributes this write states as tuples, one attribute at a time. A node
// the insert made is stated `visibility` and `state`, and `project` and `parties` where it
// carries them, which is the node carrying each from its first write. A node that was already
// there is stated each attribute this write moved on its row, each on its own: a write that
// moves one states that one, a write that gives a node an earlier build stored without a project
// its project states `project` alone, and a node written again with nothing moved is stated
// nothing, so a re-index of a document nothing moved appends nothing. They are read and compared
// separately because they move separately - a node hidden from a project keeps its state, and a
// deleted block keeps its visibility - and a row whose column moved while the log said nothing
// would be a row no appended fact explains, which nothing may mend later, since no tuple is ever
// updated. Each tuple carries the time, the writer and the cause of the node write that states
// it.
//
// A write naming a project for a node the row holds in another is refused with ErrProjectMoved,
// whether or not anything else of the node moved, and the row keeps its project either way.
//
// The stored values are read from the row this write updates. The second statement reads them,
// and under READ COMMITTED it reads a snapshot of its own, taken once the first holds every row
// the write touches: what it compares against is the newest committed version of each row, and
// no other writer can move one until this transaction ends. Read in the snapshot of a statement
// that then waited on a row, they would be the row as it stood before the writer it waited on,
// while the write goes over the version that writer committed: one writer states a node gone, a
// second waits on its row and moves it back to live, compares live with live and states nothing,
// and the row says live while its newest state tuple says gone. Locking the rows already there
// in a statement of their own before inserting the new ones would read fresh values too, but it
// takes the rows out of ascending (kind, id) wherever a new node sorts before one already there.
// Two writes racing over a node the store does not hold yet are one case of the same: the
// second's insert waits on the first's, then finds the row there and locks it, and its second
// statement compares against what the first committed. The pass that writes a block's node holds
// its document's owner row before it reaches this.
func writeNodes(ctx context.Context, tx pgx.Tx, nodes []Node, stated []Tuple) ([]Tuple, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	args, err := nodeParams(nodes)
	if err != nil {
		return nil, err
	}
	// marks holds, by each node's position, the base attributes this write states for it: each
	// one the node carries for a node the insert made, each one that moved for a node already
	// there. It is made by the first node that states one, so a write that states none - a
	// re-index of a document nothing moved - makes none.
	var marks []byte
	mark := func(position int64, attributes byte) {
		if marks == nil {
			marks = make([]byte, len(nodes))
		}
		marks[position-1] |= attributes
	}
	made, err := insertNodes(ctx, tx, args, func(position int64) {
		node := nodes[position-1]
		attributes := markVisibility | markState
		if node.Project != "" {
			attributes |= markProject
		}
		if len(node.Parties) > 0 {
			attributes |= markParties
		}
		mark(position, attributes)
	})
	if err != nil {
		return nil, err
	}
	if made < len(nodes) {
		if err := moveNodes(ctx, tx, args, nodes, mark); err != nil {
			return nil, err
		}
	}
	if marks == nil {
		return nil, nil
	}
	own := statedAttributes(stated)
	// Four attributes per node at most, so the slice made here at that size grows none.
	base := make([]Tuple, 0, len(baseAttributes)*len(nodes))
	for index, marked := range marks {
		node := nodes[index]
		if marked&markVisibility != 0 {
			base = statesAttribute(base, own, node, AttributeVisibility, node.Visibility)
		}
		if marked&markState != 0 {
			base = statesAttribute(base, own, node, AttributeState, node.State)
		}
		if marked&markProject != 0 {
			base = statesAttribute(base, own, node, AttributeProject, node.Project)
		}
		if marked&markParties != 0 {
			base = statesAttribute(base, own, node, AttributeParties, PartiesValue(node.Parties))
		}
	}
	return base, nil
}

// The base attributes a node write states for one node, as writeNodes marks them.
const (
	markVisibility byte = 1 << iota
	markState
	markProject
	markParties
)

// incomingNodes is the common table expression both of writeNodes' statements read the nodes
// through, each with its position in the caller's list, from the arrays bound at $1 (nodeBound).
var incomingNodes = `with incoming as (
			select ` + nodeBound + `, ordinality
			from unnest(` + nodeArrays(1) + `)
				with ordinality as node(` + nodeColumns + `, ordinality)
		)`

// insertNodes is writeNodes' first statement: it inserts every node the store does not hold and
// locks every node it does, all in ascending (kind, id), and calls made with the position of
// each node it inserted, answering how many. A conflicting row is locked whether or not the
// update's condition holds, and the condition is false, so a row already there is locked at the
// level an update takes and changed in nothing, which is the lock the second statement's update
// would take anyway.
//
// QueryExecModeDescribeExec rather than the pool's default, here and in moveNodes, because these
// statements must be planned with their parameters in hand. Their rows come from one unnest,
// which the planner estimates at ten rows when it has no values; on the sixth execution of a
// connection PostgreSQL swaps the custom plan for the generic one, and that ten-row estimate
// turns the join of the stored rows against the incoming ones into a nested loop that rescans
// the incoming ones once per node written. Measured on this pod's PostgreSQL 18.6 over a
// 1,999-block document version, which writes 3,998 node rows: 3,009 ms on the generic plan
// against 193 ms on the custom one, and quadratic in the block count rather than linear. An
// unnamed statement is planned with its values every time, so the plan cache never holds one.
func insertNodes(ctx context.Context, tx pgx.Tx, args []any, made func(position int64)) (int, error) {
	rows, err := tx.Query(ctx, incomingNodes+`,
		inserted as (
			insert into graph_nodes (`+nodeColumns+`)
			select `+nodeColumns+` from incoming
			order by kind, id
			on conflict (kind, id) do update set source_event = graph_nodes.source_event
			where false
			returning kind, id
		)
		select incoming.ordinality
		from inserted
		join incoming on incoming.kind = inserted.kind and incoming.id = inserted.id
	`, append([]any{pgx.QueryExecModeDescribeExec}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("write graph nodes: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var position int64
		if err := rows.Scan(&position); err != nil {
			return 0, fmt.Errorf("write graph nodes: %w", err)
		}
		made(position)
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("write graph nodes: %w", err)
	}
	return count, nil
}

// moveNodes is writeNodes' second statement: it moves every node already there whose supertag,
// visibility, state, attributes or parties this write changes, or whose missing project it gives,
// and calls moved with the position of each and the base attributes it moved, comparing against
// the row as its own snapshot holds it, which is the row insertNodes locked. A node insertNodes
// made holds what this write states, so it moves nothing here. It answers ErrProjectMoved for a
// node whose stored project this write names another for, whether or not the update fired: the
// update keeps the stored project, and the refusal ends the caller's transaction.
func moveNodes(ctx context.Context, tx pgx.Tx, args []any, nodes []Node, moved func(position int64, attributes byte)) error {
	rows, err := tx.Query(ctx, incomingNodes+`,
		stored as (
			select held.kind, held.id, held.visibility, held.state, held.project_key, held.parties
			from graph_nodes held
			join incoming on incoming.kind = held.kind and incoming.id = held.id
		),
		written as (
			update graph_nodes set
				supertag = incoming.supertag,
				visibility = incoming.visibility,
				state = incoming.state,
				attributes = incoming.attributes,
				source_event = incoming.source_event,
				project_key = coalesce(graph_nodes.project_key, incoming.project_key),
				parties = coalesce(incoming.parties, graph_nodes.parties)
			from incoming
			where graph_nodes.kind = incoming.kind and graph_nodes.id = incoming.id
			  and (graph_nodes.supertag is distinct from incoming.supertag
			       or graph_nodes.visibility is distinct from incoming.visibility
			       or graph_nodes.state is distinct from incoming.state
			       or graph_nodes.attributes is distinct from incoming.attributes
			       or (graph_nodes.project_key is null and incoming.project_key is not null)
			       or (incoming.parties is not null and graph_nodes.parties is distinct from incoming.parties))
			returning graph_nodes.kind, graph_nodes.id, graph_nodes.visibility, graph_nodes.state,
				graph_nodes.project_key, graph_nodes.parties
		)
		select incoming.ordinality, stored.project_key,
			written.kind is not null and stored.visibility is distinct from written.visibility,
			written.kind is not null and stored.state is distinct from written.state,
			written.kind is not null and stored.project_key is distinct from written.project_key,
			written.kind is not null and stored.parties is distinct from written.parties
		from incoming
		join stored on stored.kind = incoming.kind and stored.id = incoming.id
		left join written on written.kind = incoming.kind and written.id = incoming.id
	`, append([]any{pgx.QueryExecModeDescribeExec}, args...)...)
	if err != nil {
		return fmt.Errorf("write graph nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var position int64
		var storedProject *string
		var movedVisibility, movedState, movedProject, movedParties bool
		if err := rows.Scan(&position, &storedProject, &movedVisibility, &movedState, &movedProject, &movedParties); err != nil {
			return fmt.Errorf("write graph nodes: %w", err)
		}
		if node := nodes[position-1]; storedProject != nil && node.Project != "" && *storedProject != node.Project {
			return fmt.Errorf("write graph node %s/%s in project %s, which the store holds in %s: %w",
				node.Kind, node.ID, node.Project, *storedProject, ErrProjectMoved)
		}
		var attributes byte
		if movedVisibility {
			attributes |= markVisibility
		}
		if movedState {
			attributes |= markState
		}
		if movedProject {
			attributes |= markProject
		}
		if movedParties {
			attributes |= markParties
		}
		if attributes != 0 {
			moved(position, attributes)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("write graph nodes: %w", err)
	}
	return nil
}

// baseAttribute is one base attribute of one node, the key a write is read by to see whether it
// states that attribute itself.
type baseAttribute struct{ kind, id, attribute string }

// statedAttributes are the base attributes the write states itself, read off the tuples it
// handed in. No writer of this tree states one today - each leaves them to the node write - but
// a writer that did would keep that statement: one write states one fact once.
func statedAttributes(tuples []Tuple) map[baseAttribute]struct{} {
	stated := make(map[baseAttribute]struct{}, len(tuples))
	for _, tuple := range tuples {
		if tuple.ToKind != KindValue || !IsBaseAttribute(tuple.Kind) {
			continue
		}
		stated[baseAttribute{tuple.FromKind, tuple.FromID, tuple.Kind}] = struct{}{}
	}
	return stated
}

// statesAttribute adds one base attribute of a node to what this write states, unless the write
// stated it itself.
func statesAttribute(base []Tuple, own map[baseAttribute]struct{}, node Node, attribute, value string) []Tuple {
	if _, held := own[baseAttribute{node.Kind, node.ID, attribute}]; held {
		return base
	}
	return append(base, Tuple{
		FromKind: node.Kind, FromID: node.ID, Kind: attribute,
		ToKind: KindValue, ToID: value,
		At: node.MadeAt, Who: node.MadeBy, SourceEvent: node.SourceEvent, Asserted: true,
	})
}

// appendTuples appends the relations and the attributes in two statements, because the two carry
// different repeat keys: one event states one fact once, so a writer that is retried appends the
// same row again and `do nothing` drops it. Each statement names its own partial unique index by
// that index's exact column list and predicate - a partial index is not a constraint, so
// `on conflict on constraint` cannot name it. Both keys are partial on `source_event <> 0`, so a
// tuple at the sentinel is appended unconditionally.
func appendTuples(ctx context.Context, tx pgx.Tx, tuples []Tuple) error {
	relations, values := splitTuples(tuples)
	if err := appendTupleRows(ctx, tx, tupleInsert(1, "")+relationRepeatKey, relations); err != nil {
		return fmt.Errorf("append graph relations: %w", err)
	}
	if err := appendTupleRows(ctx, tx, tupleInsert(1, "")+valueRepeatKey, values); err != nil {
		return fmt.Errorf("append graph attributes: %w", err)
	}
	return nil
}

func appendTupleRows(ctx context.Context, tx pgx.Tx, statement string, tuples []Tuple) error {
	if len(tuples) == 0 {
		return nil
	}
	args, err := tupleParams(tuples)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, statement, args...)
	return err
}

// encodeAttributes writes an empty object for a node that carries no attribute, since the column
// is `not null` and a nil map encodes as the JSON null, which is a value rather than no value.
func encodeAttributes(attributes map[string]any) ([]byte, error) {
	if attributes == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(attributes)
}

// causeNodes moves the cause onto the nodes a writer stated before its event existed (Caused).
// A request writes a node before the row whose foreign key names it, and its event is appended
// after both, so the node is written with the sentinel and learns its cause here. It is one
// statement over the addresses the writer holds, not a second node write: writing those rows
// again would double the cost of every pass for one column, and the upsert's own guard would
// refuse it anyway, since nothing else about the node moved. Only a node still at the sentinel
// takes the event, so a node an earlier version already caused keeps the cause it had.
func causeNodes(ctx context.Context, tx pgx.Tx, nodes []Node, event int64) error {
	if len(nodes) == 0 || event == NoEvent {
		return nil
	}
	kinds := make([]string, len(nodes))
	ids := make([]string, len(nodes))
	for index, node := range nodes {
		kinds[index], ids[index] = node.Kind, node.ID
	}
	_, err := tx.Exec(ctx, `
		update graph_nodes set source_event = $1
		from unnest($2::text[], $3::text[]) as named(kind, id)
		where graph_nodes.kind = named.kind and graph_nodes.id = named.id
		  and graph_nodes.source_event = 0
	`, event, kinds, ids)
	if err != nil {
		return fmt.Errorf("record what caused the graph nodes: %w", err)
	}
	return nil
}
