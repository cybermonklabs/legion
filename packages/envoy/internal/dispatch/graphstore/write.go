package graphstore

import (
	"context"
	"encoding/json"
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
// and must therefore follow the node - takes WriteStating instead, so that the base attributes
// are appended under that event rather than under the sentinel.
func Write(ctx context.Context, tx pgx.Tx, nodes []Node, tuples []Tuple) error {
	base, err := WriteStating(ctx, tx, nodes, tuples)
	if err != nil {
		return err
	}
	if len(base) == 0 {
		return appendTuples(ctx, tx, tuples)
	}
	return appendTuples(ctx, tx, append(base, tuples...))
}

// WriteStating writes every node and answers the base attributes the write states, appending
// none of them: Write split at the one seam a caller needs, which is between writing the row and
// appending the facts under it.
//
// It exists for the writer whose event does not exist while it runs. Such a writer orders itself
// node row, projection row, event, tuples: the node first because the projection row's foreign
// key names it, the event after both because it records what the request did, and the tuples
// last because each one holds the event that caused it and no tuple may be updated to learn it
// afterwards. The base attributes are facts of that same write, so they go in that last step
// with the caller's own tuples, under the same event, in one Write(ctx, tx, nil, tuples).
//
// stated is what the caller states itself, read for the one thing writeNodes needs it for: a
// base attribute the caller states is not stated twice.
func WriteStating(ctx context.Context, tx pgx.Tx, nodes []Node, stated []Tuple) ([]Tuple, error) {
	return writeNodes(ctx, tx, nodes, stated)
}

// The two base attributes, as the kind of the tuple that states each spells it. graph_nodes
// carries a column for each, which is that attribute's projection, and nodeArm states the same
// two for a node the publisher's record makes.
const (
	attributeVisibility = "visibility"
	attributeState      = "state"
)

// baseAttributes is the set BaseAttributes answers and IsBaseAttribute reads, held once.
var baseAttributes = [...]string{attributeVisibility, attributeState}

// BaseAttributes is the tuple kind of every base attribute, a fresh slice for each caller. No
// other attribute's values may be stored under one of these kinds: a value tuple of one of them
// out of a node reads as that node's own visibility or state, whoever wrote it, so an attribute a
// type declares under one of these names is stored under another name
// (pmdoc/schemanodes.StoreName).
func BaseAttributes() []string { return slices.Clone(baseAttributes[:]) }

// IsBaseAttribute is whether kind is the tuple kind of a base attribute (BaseAttributes), asked
// without building the list.
func IsBaseAttribute(kind string) bool { return slices.Contains(baseAttributes[:], kind) }

// writeNodes is one statement, whose rows arrive in ascending (kind, id): a write touching
// several nodes takes its row locks in that order, so two such writes cannot each hold the row
// the other waits for. The upsert moves supertag, visibility, state, attributes and
// source_event, and leaves made_at and made_by as the row's own first write left them - the row
// is the projection of its tuples and may be written again, and a later write that learns the
// event id has to be able to move source_event.
//
// It answers the base attributes this write states as tuples, one attribute at a time. A node
// the insert made (xmax = 0) is stated `visibility` and `state`, which is the node carrying both
// from its first write. A node that was already there is stated `visibility` where this write
// moved its stored visibility, and `state` where this write moved its stored state, each on its
// own: a write that moves one of the two states that one, and a node written again with neither
// moved is stated neither, so a re-index of a document nothing moved appends nothing. The two
// are read and compared separately because they move separately - a node hidden from a project
// keeps its state, and a deleted block keeps its visibility - and a row whose column moved while
// the log said nothing would be a row no appended fact explains, which nothing may mend later,
// since no tuple is ever updated. Each tuple carries the time, the writer and the cause of the
// node write that states it.
//
// Both stored values are read in the same statement as the write, so the comparison is of one
// snapshot: two writes racing over one node can state one value twice, which this store takes by
// design (a second backfill run does the same). They cannot lose one, because the two writers of
// a node queue on its row here, and the pass that writes a block's node holds its document's
// owner row before it reaches this.
func writeNodes(ctx context.Context, tx pgx.Tx, nodes []Node, stated []Tuple) ([]Tuple, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	args, err := nodeParams(nodes)
	if err != nil {
		return nil, err
	}
	// QueryExecModeDescribeExec rather than the pool's default, because this statement must
	// be planned with its parameters in hand. Its rows come from one unnest, which the
	// planner estimates at ten rows when it has no values; on the sixth execution of a
	// connection PostgreSQL swaps the custom plan for the generic one, and that ten-row
	// estimate turns the join of stored against incoming into a nested loop that rescans
	// incoming once per node written. Measured on this pod's PostgreSQL 18.6 over a
	// 1,999-block document version, which writes 3,998 node rows: 3,009 ms on the generic
	// plan against 193 ms on the custom one, and quadratic in the block count rather than
	// linear. An unnamed statement is planned with its values every time, so the plan cache
	// never holds one.
	rows, err := tx.Query(ctx, `
		with incoming as (
			select `+nodeColumns+`, ordinality
			from unnest(`+nodeArrays(1)+`)
				with ordinality as node(`+nodeColumns+`, ordinality)
		),
		stored as (
			select held.kind, held.id, held.visibility, held.state
			from graph_nodes held
			join incoming on incoming.kind = held.kind and incoming.id = held.id
		),
		written as (
			insert into graph_nodes (`+nodeColumns+`)
			select `+nodeColumns+` from incoming order by kind, id
			on conflict (kind, id) do update set
				supertag = excluded.supertag,
				visibility = excluded.visibility,
				state = excluded.state,
				attributes = excluded.attributes,
				source_event = excluded.source_event
			where graph_nodes.supertag is distinct from excluded.supertag
			   or graph_nodes.visibility is distinct from excluded.visibility
			   or graph_nodes.state is distinct from excluded.state
			   or graph_nodes.attributes is distinct from excluded.attributes
			returning kind, id, visibility, state, xmax = 0 as made
		)
		select incoming.ordinality, written.made,
			stored.visibility is distinct from written.visibility,
			stored.state is distinct from written.state
		from written
		join incoming on incoming.kind = written.kind and incoming.id = written.id
		left join stored on stored.kind = written.kind and stored.id = written.id
		order by incoming.ordinality
	`, append([]any{pgx.QueryExecModeDescribeExec}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("write graph nodes: %w", err)
	}
	defer rows.Close()
	var base []Tuple
	var own map[baseAttribute]struct{}
	for rows.Next() {
		var position int64
		var made, movedVisibility, movedState bool
		if err := rows.Scan(&position, &made, &movedVisibility, &movedState); err != nil {
			return nil, fmt.Errorf("write graph nodes: %w", err)
		}
		if !made && !movedVisibility && !movedState {
			continue
		}
		if own == nil {
			own = statedAttributes(stated)
			// Two attributes per node at most. The slice is made at that size by the
			// first node that states one, so a write that states none - a re-index of a
			// document nothing moved - makes none, and one that states thousands grows
			// none.
			base = make([]Tuple, 0, 2*len(nodes))
		}
		node := nodes[position-1]
		if made || movedVisibility {
			base = statesAttribute(base, own, node, attributeVisibility, node.Visibility)
		}
		if made || movedState {
			base = statesAttribute(base, own, node, attributeState, node.State)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("write graph nodes: %w", err)
	}
	return base, nil
}

// baseAttribute is one base attribute of one node, the key a write is read by to see whether it
// states that attribute itself.
type baseAttribute struct{ kind, id, attribute string }

// statedAttributes are the base attributes the write states itself, read off the tuples it
// handed in. The comment path and the backfill state both of them beside the event that caused
// the write, and that statement is theirs to keep: one write states one fact once.
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

// Caused moves the cause onto the nodes a writer stated before its event existed. A request
// writes a node before the row whose foreign key names it, and its event is appended after
// both, so the node is written with the sentinel and learns its cause here. It is one statement
// over the addresses the writer holds, not a second node write: writing those rows again would
// double the cost of every pass for one column, and the upsert's own guard would refuse it
// anyway, since nothing else about the node moved. Only a node still at the sentinel takes the
// event, so a node an earlier version already caused keeps the cause it had.
func Caused(ctx context.Context, tx pgx.Tx, nodes []Node, event int64) error {
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
