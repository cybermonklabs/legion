package graphstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// tupleColumns is what every tuple read answers, in the order scanTuples takes them.
const tupleColumns = `seq, from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted`

// CurrentOut answers, for each (kind, to_kind, to_id) out of one node, the newest tuple, and
// answers it only while that newest one asserts: a relation whose last word is a retraction is
// absent rather than answered as ended. An empty kinds means every kind. The statement reads
// through graph_tuples_current, whose column order is the one the distinct on walks, so the
// planner takes it as an index scan and sorts nothing.
func CurrentOut(ctx context.Context, q Queryer, fromKind, fromID string, kinds []string) ([]Tuple, error) {
	rows, err := q.Query(ctx, `
		select `+tupleColumns+` from (
			select distinct on (kind, to_kind, to_id) `+tupleColumns+`
			from graph_tuples
			where from_kind = $1 and from_id = $2 and to_kind <> 'value'
				and ($3::text[] is null or kind = any($3::text[]))
			order by kind, to_kind, to_id, seq desc
		) newest
		where asserted
		order by kind, to_kind, to_id
	`, fromKind, fromID, kindList(kinds))
	if err != nil {
		return nil, fmt.Errorf("read current relations out of %s/%s: %w", fromKind, fromID, err)
	}
	return scanTuples(rows)
}

// CurrentIn answers, for each (kind, from_kind, from_id) into one node, the newest tuple, while
// that newest one asserts. It reads through graph_tuples_inbound, which is graph_tuples_current
// with the ends swapped. A value is no node, so nothing points into one.
func CurrentIn(ctx context.Context, q Queryer, toKind, toID string, kinds []string) ([]Tuple, error) {
	rows, err := q.Query(ctx, `
		select `+tupleColumns+` from (
			select distinct on (kind, from_kind, from_id) `+tupleColumns+`
			from graph_tuples
			where to_kind = $1 and to_id = $2 and to_kind <> 'value'
				and ($3::text[] is null or kind = any($3::text[]))
			order by kind, from_kind, from_id, seq desc
		) newest
		where asserted
		order by kind, from_kind, from_id
	`, toKind, toID, kindList(kinds))
	if err != nil {
		return nil, fmt.Errorf("read current relations into %s/%s: %w", toKind, toID, err)
	}
	return scanTuples(rows)
}

// CurrentValue answers one node's attribute: the newest tuple under that name, whose to_id holds
// the value itself. It binds to_kind = 'value', so the planner takes graph_tuples_values, the one
// read index that keys no column a value can reach. The second result is false where the node
// never carried the attribute and where its newest tuple retracts it, which are one thing to a
// reader: the node has no such attribute now.
func CurrentValue(ctx context.Context, q Queryer, fromKind, fromID, attribute string) (string, bool, error) {
	var value string
	var asserted bool
	err := q.QueryRow(ctx, `
		select to_id, asserted
		from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = 'value'
		order by seq desc
		limit 1
	`, fromKind, fromID, attribute).Scan(&value, &asserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read attribute %s of %s/%s: %w", attribute, fromKind, fromID, err)
	}
	if !asserted {
		return "", false, nil
	}
	return value, true, nil
}

// AsOfOut answers the state out of one node at a moment: for each far end, the newest tuple at
// or before at, asserted ones alone. It reads through graph_tuples_as_of_out, which carries
// every tuple out of a node in time order, attributes among them, so this answers an attribute's
// value at that moment beside the relations.
func AsOfOut(ctx context.Context, q Queryer, fromKind, fromID string, at time.Time, kinds []string) ([]Tuple, error) {
	rows, err := q.Query(ctx, `
		select `+tupleColumns+` from (
			select distinct on (kind, to_kind, to_id) `+tupleColumns+`
			from graph_tuples
			where from_kind = $1 and from_id = $2 and at <= $3
				and ($4::text[] is null or kind = any($4::text[]))
			order by kind, to_kind, to_id, at desc, seq desc
		) newest
		where asserted
		order by kind, to_kind, to_id
	`, fromKind, fromID, at, kindList(kinds))
	if err != nil {
		return nil, fmt.Errorf("read relations out of %s/%s as of %s: %w", fromKind, fromID, at, err)
	}
	return scanTuples(rows)
}

// AsOfIn answers the state into one node at a moment, as AsOfOut answers it out of one, through
// graph_tuples_as_of_in.
func AsOfIn(ctx context.Context, q Queryer, toKind, toID string, at time.Time, kinds []string) ([]Tuple, error) {
	rows, err := q.Query(ctx, `
		select `+tupleColumns+` from (
			select distinct on (kind, from_kind, from_id) `+tupleColumns+`
			from graph_tuples
			where to_kind = $1 and to_id = $2 and to_kind <> 'value' and at <= $3
				and ($4::text[] is null or kind = any($4::text[]))
			order by kind, from_kind, from_id, at desc, seq desc
		) newest
		where asserted
		order by kind, from_kind, from_id
	`, toKind, toID, at, kindList(kinds))
	if err != nil {
		return nil, fmt.Errorf("read relations into %s/%s as of %s: %w", toKind, toID, at, err)
	}
	return scanTuples(rows)
}

// TuplesIntoNode is the inbox read: every tuple of the named kinds into one node, newest first,
// bounded by limit. It is one statement per kind count, because one statement holding the kinds
// in an array cannot be planned per kind: Postgres takes a node's whole inbox and sorts it,
// which on a node with a long inbox is a scan of the log. A branch per kind is one index scan of
// graph_tuples_as_of_in each, which holds a node's inbound tuples of one kind in time order, and
// Postgres merges the branches in that order (Merge Append) and stops at limit, so the read
// touches about limit rows per kind however long the inbox is.
//
// Naming no kind leaves the statement with no kind to scan by and falls to that scan, so a
// caller names the kinds it wants; TupleKinds answers which ones the store holds.
func TuplesIntoNode(ctx context.Context, q Queryer, toKind, toID string, kinds []string, limit int) ([]Tuple, error) {
	if limit < 1 {
		return nil, fmt.Errorf("read tuples into %s/%s: limit %d is below one row", toKind, toID, limit)
	}
	args := make([]any, 0, 3+len(kinds))
	args = append(args, toKind, toID, limit)
	branches := make([]string, 0, max(len(kinds), 1))
	if len(kinds) == 0 {
		branches = append(branches, inboxBranch(""))
	}
	// A kind named twice is one branch: two identical branches would answer each of that kind's
	// tuples twice.
	named := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		if slices.Contains(named, kind) {
			continue
		}
		named = append(named, kind)
		args = append(args, kind)
		branches = append(branches, inboxBranch(" and kind = $"+strconv.Itoa(len(args))))
	}
	statement := strings.Join(branches, "\nunion all\n")
	if len(branches) > 1 {
		statement += "\norder by at desc, seq desc\nlimit $3"
	}
	rows, err := q.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read tuples into %s/%s: %w", toKind, toID, err)
	}
	return scanTuples(rows)
}

// inboxBranch is one kind's arm of the inbox read, ordered and bounded on its own so the merge
// above it reads no further into any kind than it must.
func inboxBranch(kind string) string {
	return `(select ` + tupleColumns + `
		from graph_tuples
		where to_kind = $1 and to_id = $2 and to_kind <> 'value'` + kind + `
		order by at desc, seq desc
		limit $3)`
}

// TupleKinds answers the kinds the store holds, so a caller binds its kind list from the store
// rather than from a list in Go that a newer writer's kind would not be in.
func TupleKinds(ctx context.Context, q Queryer) ([]string, error) {
	rows, err := q.Query(ctx, `select distinct kind from graph_tuples order by kind`)
	if err != nil {
		return nil, fmt.Errorf("read tuple kinds: %w", err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			return nil, fmt.Errorf("read tuple kinds: %w", err)
		}
		kinds = append(kinds, kind)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tuple kinds: %w", err)
	}
	return kinds, nil
}

// History answers every tuple of one node under one kind, in the order they were written, each
// with its value or its far end, its time, its writer and the event that caused it. It is one
// read for an attribute and a relation alike: an attribute's history is three values under one
// name, a relation's is the asserting and retracting of far ends, and the rows have one shape, so
// a caller cannot tell from the answer which of the two it asked for.
func History(ctx context.Context, q Queryer, fromKind, fromID, kind string) ([]Tuple, error) {
	rows, err := q.Query(ctx, `
		select `+tupleColumns+`
		from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3
		order by seq
	`, fromKind, fromID, kind)
	if err != nil {
		return nil, fmt.Errorf("read history of %s on %s/%s: %w", kind, fromKind, fromID, err)
	}
	return scanTuples(rows)
}

// ReadNode answers one node row, the projection of its tuples, and whether the store holds it.
func ReadNode(ctx context.Context, q Queryer, kind, id string) (Node, bool, error) {
	node, err := scanNode(q.QueryRow(ctx, `
		select `+nodeColumns+`
		from graph_nodes
		where kind = $1 and id = $2
	`, kind, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, false, nil
	}
	if err != nil {
		return Node{}, false, fmt.Errorf("read node %s/%s: %w", kind, id, err)
	}
	return node, true, nil
}

// nodeColumns is what every node read answers, in the order a node scan takes them.
const nodeColumns = `kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event`

// NodesOfKind answers every node of one kind, in id order. It reads through the primary key
// (kind, id), whose leading column it binds, so a kind the store holds few of costs that few
// rows however many nodes the store holds. The block schema's supertag and attribute nodes are
// read this way (pmdoc/schemanodes), which is why the read takes a kind and no node id: a kind
// the build knows nothing of is a row this answers and the caller skips, never a read that has
// to name every node in advance.
func NodesOfKind(ctx context.Context, q Queryer, kind string) ([]Node, error) {
	rows, err := q.Query(ctx, `
		select `+nodeColumns+`
		from graph_nodes
		where kind = $1
		order by id
	`, kind)
	if err != nil {
		return nil, fmt.Errorf("read nodes of kind %s: %w", kind, err)
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
		return nil, fmt.Errorf("read nodes of kind %s: %w", kind, err)
	}
	return nodes, nil
}

// scanNode reads one node row, as ReadNode and NodesOfKind both take it.
func scanNode(row pgx.Row) (Node, error) {
	var node Node
	var attributes, madeBy []byte
	if err := row.Scan(&node.Kind, &node.ID, &node.Supertag, &node.Visibility, &node.State,
		&attributes, &node.MadeAt, &madeBy, &node.SourceEvent); err != nil {
		return Node{}, err
	}
	if err := json.Unmarshal(attributes, &node.Attributes); err != nil {
		return Node{}, fmt.Errorf("decode attributes of node %s/%s: %w", node.Kind, node.ID, err)
	}
	if err := json.Unmarshal(madeBy, &node.MadeBy); err != nil {
		return Node{}, fmt.Errorf("decode writer of node %s/%s: %w", node.Kind, node.ID, err)
	}
	return node, nil
}

// kindList is the kinds parameter a read binds: the empty list is sent as null, which is how
// each statement spells "every kind".
func kindList(kinds []string) any {
	if len(kinds) == 0 {
		return nil
	}
	return kinds
}

func scanTuples(rows pgx.Rows) ([]Tuple, error) {
	defer rows.Close()
	var tuples []Tuple
	for rows.Next() {
		var tuple Tuple
		var who []byte
		if err := rows.Scan(&tuple.Seq, &tuple.FromKind, &tuple.FromID, &tuple.Kind, &tuple.ToKind,
			&tuple.ToID, &tuple.At, &who, &tuple.SourceEvent, &tuple.LegacySourceSeq, &tuple.Asserted); err != nil {
			return nil, fmt.Errorf("read tuple: %w", err)
		}
		if err := json.Unmarshal(who, &tuple.Who); err != nil {
			return nil, fmt.Errorf("decode tuple writer: %w", err)
		}
		tuples = append(tuples, tuple)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tuples: %w", err)
	}
	return tuples, nil
}
