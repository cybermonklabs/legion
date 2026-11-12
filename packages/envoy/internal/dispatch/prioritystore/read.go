package prioritystore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// tupleColumns is what every tuple read answers, in the order scanTuples takes them.
const tupleColumns = `seq, from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted`

// nodeColumns is what every node read answers, in the order scanNode takes them.
const nodeColumns = `kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event`

// History answers every tuple of one node under one kind, in the order they were written, each
// with its value or its far end, its time and its writer. It is one read for an attribute and a
// relation alike: an attribute's history is three values under one name, a relation's is the
// asserting and retracting of far ends, and the rows have one shape, so a caller cannot tell from
// the answer which of the two it asked for.
//
// It is written as two legs over one bound, an attribute's and a relation's, because every tuple
// index is partial on to_kind: priority_tuples_values holds the rows where it is KindValue and
// priority_tuples_current the rows where it is not. A single arm binding from_kind, from_id and
// kind alone implies neither partial predicate, so the planner can take neither index and reads
// the whole log. The column is `not null`, so the two legs are every row the one arm answered
// and no row twice.
func History(ctx context.Context, q Queryer, fromKind, fromID, kind string) ([]Tuple, error) {
	rows, err := q.Query(ctx, `
		select `+tupleColumns+` from (
			select `+tupleColumns+`
			from priority_tuples
			where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = '`+KindValue+`'
			union all
			select `+tupleColumns+`
			from priority_tuples
			where from_kind = $1 and from_id = $2 and kind = $3 and to_kind <> '`+KindValue+`'
		) stated
		order by seq
	`, fromKind, fromID, kind)
	if err != nil {
		return nil, fmt.Errorf("read history of %s on %s/%s: %w", kind, fromKind, fromID, err)
	}
	return scanTuples(rows)
}

// Read answers one priority's node row, the projection of its tuples, and whether the store holds
// it. It answers a node whose state is StateGone too: a caller that serves the page of a priority
// somebody deleted reads the row and says so.
func Read(ctx context.Context, q Queryer, id string) (Node, bool, error) {
	node, err := scanNode(q.QueryRow(ctx, `
		select `+nodeColumns+`
		from priority_nodes
		where kind = '`+KindPriority+`' and id = $1
	`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, false, nil
	}
	if err != nil {
		return Node{}, false, fmt.Errorf("read priority %s: %w", id, err)
	}
	return node, true, nil
}

// List answers every live priority with its attributes, in the order they are ranked, which is
// the order the priorities page draws. It reads through the primary key, whose leading column it
// binds, so it costs the priorities the store holds and nothing else. The id breaks a tie,
// because nothing makes a rank unique. The order is the key's own byte order, which is what
// rankKey (rank.go) names and why.
func List(ctx context.Context, q Queryer) ([]Node, error) {
	rows, err := q.Query(ctx, `
		select `+nodeColumns+`
		from priority_nodes
		where kind = '`+KindPriority+`' and state = $1
		order by `+rankKey+`, id
	`, StateLive)
	if err != nil {
		return nil, fmt.Errorf("read the live priorities: %w", err)
	}
	defer rows.Close()
	var nodes []Node
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the live priorities: %w", err)
	}
	return nodes, nil
}

// Timeline answers one priority's whole history, newest first: every attribute it was written
// with, and every `advances` relation asserted or retracted on it, each with its writer and its
// time. It is what the priority page's margin lists, and it is three index scans — the
// attributes written on the node, the relations written on it, and the `advances` tuples into it
// — merged on the log position they were appended at, which is the order they happened in.
//
// The tuples out of the node are read as two legs for the reason History's are: every tuple
// index is partial on to_kind, so one leg binds KindValue and takes priority_tuples_values while
// the other excludes it and takes priority_tuples_current. The column is `not null`, so the two
// are every row out of the node and no row twice. The third leg binds to_kind to KindPriority,
// which implies the inbound index's own predicate.
func Timeline(ctx context.Context, q Queryer, id string) ([]Tuple, error) {
	rows, err := q.Query(ctx, `
		select `+tupleColumns+` from (
			select `+tupleColumns+`
			from priority_tuples
			where from_kind = '`+KindPriority+`' and from_id = $1 and to_kind = '`+KindValue+`'
			union all
			select `+tupleColumns+`
			from priority_tuples
			where from_kind = '`+KindPriority+`' and from_id = $1 and to_kind <> '`+KindValue+`'
			union all
			select `+tupleColumns+`
			from priority_tuples
			where to_kind = '`+KindPriority+`' and to_id = $1 and kind = '`+RelationAdvances+`'
		) stated
		order by seq desc
	`, id)
	if err != nil {
		return nil, fmt.Errorf("read the history of priority %s: %w", id, err)
	}
	return scanTuples(rows)
}

// scanNode reads one node row, as Read and List both take it.
func scanNode(row pgx.Row) (Node, error) {
	var node Node
	var attributes, madeBy []byte
	if err := row.Scan(&node.Kind, &node.ID, &node.Supertag, &node.Visibility, &node.State,
		&attributes, &node.MadeAt, &madeBy, &node.SourceEvent); err != nil {
		return Node{}, err
	}
	if err := json.Unmarshal(attributes, &node.Attributes); err != nil {
		return Node{}, fmt.Errorf("decode attributes of priority %s: %w", node.ID, err)
	}
	if err := json.Unmarshal(madeBy, &node.MadeBy); err != nil {
		return Node{}, fmt.Errorf("decode writer of priority %s: %w", node.ID, err)
	}
	return node, nil
}

func scanTuples(rows pgx.Rows) ([]Tuple, error) {
	defer rows.Close()
	var tuples []Tuple
	for rows.Next() {
		var tuple Tuple
		var who []byte
		if err := rows.Scan(&tuple.Seq, &tuple.FromKind, &tuple.FromID, &tuple.Kind, &tuple.ToKind,
			&tuple.ToID, &tuple.At, &who, &tuple.SourceEvent, &tuple.Asserted); err != nil {
			return nil, fmt.Errorf("read priority tuple: %w", err)
		}
		if err := json.Unmarshal(who, &tuple.Who); err != nil {
			return nil, fmt.Errorf("decode priority tuple writer: %w", err)
		}
		tuples = append(tuples, tuple)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read priority tuples: %w", err)
	}
	return tuples, nil
}
