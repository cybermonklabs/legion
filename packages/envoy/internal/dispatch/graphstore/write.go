package graphstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Write writes every node and appends every tuple inside the caller's transaction. It appends
// only: no statement here updates or deletes a tuple row, so the log a read walks is whatever
// every writer before it appended, in the order they appended it.
//
// Each node is written once per call: two rows of nodes carrying one (kind, id) are the caller's
// mistake, and Postgres names it rather than this picking a winner.
func Write(ctx context.Context, tx pgx.Tx, nodes []Node, tuples []Tuple) error {
	if err := writeNodes(ctx, tx, nodes); err != nil {
		return err
	}
	return appendTuples(ctx, tx, tuples)
}

// writeNodes is one statement, whose rows arrive in ascending (kind, id): a write touching
// several nodes takes its row locks in that order, so two such writes cannot each hold the row
// the other waits for. The upsert moves supertag, visibility, state, attributes and
// source_event, and leaves made_at and made_by as the row's own first write left them - the row
// is the projection of its tuples and may be written again, and a later write that learns the
// event id has to be able to move source_event.
func writeNodes(ctx context.Context, tx pgx.Tx, nodes []Node) error {
	if len(nodes) == 0 {
		return nil
	}
	kinds := make([]string, len(nodes))
	ids := make([]string, len(nodes))
	supertags := make([]string, len(nodes))
	visibilities := make([]string, len(nodes))
	states := make([]string, len(nodes))
	attributes := make([][]byte, len(nodes))
	madeAts := make([]time.Time, len(nodes))
	madeBys := make([][]byte, len(nodes))
	sourceEvents := make([]int64, len(nodes))
	for index, node := range nodes {
		encoded, err := encodeAttributes(node.Attributes)
		if err != nil {
			return fmt.Errorf("encode attributes of node %s/%s: %w", node.Kind, node.ID, err)
		}
		// Recorded drops the request's credential, which no row ever holds.
		madeBy, err := json.Marshal(node.MadeBy.Recorded())
		if err != nil {
			return fmt.Errorf("encode writer of node %s/%s: %w", node.Kind, node.ID, err)
		}
		kinds[index], ids[index] = node.Kind, node.ID
		supertags[index], visibilities[index], states[index] = node.Supertag, node.Visibility, node.State
		attributes[index], madeAts[index], madeBys[index] = encoded, node.MadeAt, madeBy
		sourceEvents[index] = node.SourceEvent
	}
	_, err := tx.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		select kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event
		from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::jsonb[], $7::timestamptz[], $8::jsonb[], $9::bigint[])
			as node(kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		order by kind, id
		on conflict (kind, id) do update set
			supertag = excluded.supertag,
			visibility = excluded.visibility,
			state = excluded.state,
			attributes = excluded.attributes,
			source_event = excluded.source_event
	`, kinds, ids, supertags, visibilities, states, attributes, madeAts, madeBys, sourceEvents)
	if err != nil {
		return fmt.Errorf("write graph nodes: %w", err)
	}
	return nil
}

// appendTuples appends the relations and the attributes in two statements, because the two carry
// different repeat keys: one event states one fact once, so a writer that is retried appends the
// same row again and `do nothing` drops it. Each statement names its own partial unique index by
// that index's exact column list and predicate - a partial index is not a constraint, so
// `on conflict on constraint` cannot name it. Both keys are partial on `source_event <> 0`, so a
// tuple at the sentinel is appended unconditionally.
func appendTuples(ctx context.Context, tx pgx.Tx, tuples []Tuple) error {
	var relations, values []Tuple
	for _, tuple := range tuples {
		if tuple.ToKind == KindValue {
			values = append(values, tuple)
		} else {
			relations = append(relations, tuple)
		}
	}
	const relationStatement = tupleInsert + `
		on conflict (from_kind, from_id, kind, to_kind, to_id, source_event, asserted)
			where to_kind <> 'value' and source_event <> 0
		do nothing
	`
	const valueStatement = tupleInsert + `
		on conflict (from_kind, from_id, kind, source_event, asserted)
			where to_kind = 'value' and source_event <> 0
		do nothing
	`
	if err := appendTupleRows(ctx, tx, relationStatement, relations); err != nil {
		return fmt.Errorf("append graph relations: %w", err)
	}
	if err := appendTupleRows(ctx, tx, valueStatement, values); err != nil {
		return fmt.Errorf("append graph attributes: %w", err)
	}
	return nil
}

// tupleInsert reads its rows in the order the caller gave them (with ordinality, order by
// position), so the sequence the log is read back in is the order a caller stated its facts.
const tupleInsert = `
	insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
	select from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted
	from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::timestamptz[], $7::jsonb[], $8::bigint[], $9::bigint[], $10::boolean[])
		with ordinality as tuple(from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted, position)
	order by position
`

func appendTupleRows(ctx context.Context, tx pgx.Tx, statement string, tuples []Tuple) error {
	if len(tuples) == 0 {
		return nil
	}
	fromKinds := make([]string, len(tuples))
	fromIDs := make([]string, len(tuples))
	kinds := make([]string, len(tuples))
	toKinds := make([]string, len(tuples))
	toIDs := make([]string, len(tuples))
	ats := make([]time.Time, len(tuples))
	whos := make([][]byte, len(tuples))
	sourceEvents := make([]int64, len(tuples))
	legacySeqs := make([]*int64, len(tuples))
	asserted := make([]bool, len(tuples))
	for index, tuple := range tuples {
		who, err := json.Marshal(tuple.Who.Recorded())
		if err != nil {
			return fmt.Errorf("encode writer of %s %s: %w", tuple.Kind, tuple.FromID, err)
		}
		fromKinds[index], fromIDs[index], kinds[index] = tuple.FromKind, tuple.FromID, tuple.Kind
		toKinds[index], toIDs[index], ats[index] = tuple.ToKind, tuple.ToID, tuple.At
		whos[index], sourceEvents[index] = who, tuple.SourceEvent
		legacySeqs[index], asserted[index] = tuple.LegacySourceSeq, tuple.Asserted
	}
	_, err := tx.Exec(ctx, statement,
		fromKinds, fromIDs, kinds, toKinds, toIDs, ats, whos, sourceEvents, legacySeqs, asserted)
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
