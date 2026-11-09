package graphstore

import (
	"context"
	"encoding/json"
	"fmt"

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
	args, err := nodeParams(nodes)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		insert into graph_nodes (`+nodeColumns+`)
		`+nodeRows(1)+`
		on conflict (kind, id) do update set
			supertag = excluded.supertag,
			visibility = excluded.visibility,
			state = excluded.state,
			attributes = excluded.attributes,
			source_event = excluded.source_event
	`, args...)
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
	relations, values := splitTuples(tuples)
	if err := appendTupleRows(ctx, tx, tupleInsert(1)+relationRepeatKey, relations); err != nil {
		return fmt.Errorf("append graph relations: %w", err)
	}
	if err := appendTupleRows(ctx, tx, tupleInsert(1)+valueRepeatKey, values); err != nil {
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
