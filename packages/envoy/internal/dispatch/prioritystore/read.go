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

// CurrentOut answers, for each (kind, to_kind, to_id) out of one node, the newest tuple, and
// answers it only while that newest one asserts: a relation whose last word is a retraction is
// absent rather than answered as ended. An empty kinds means every kind. The statement reads
// through priority_tuples_current, whose column order is the one the distinct on walks, so the
// planner takes it as an index scan and sorts nothing.
func CurrentOut(ctx context.Context, q Queryer, fromKind, fromID string, kinds []string) ([]Tuple, error) {
	rows, err := q.Query(ctx, `
		select `+tupleColumns+` from (
			select distinct on (kind, to_kind, to_id) `+tupleColumns+`
			from priority_tuples
			where from_kind = $1 and from_id = $2 and to_kind <> '`+KindValue+`'
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
// that newest one asserts. It reads through priority_tuples_inbound, which is
// priority_tuples_current with the ends swapped. A value is no node, so nothing points into one.
func CurrentIn(ctx context.Context, q Queryer, toKind, toID string, kinds []string) ([]Tuple, error) {
	rows, err := q.Query(ctx, `
		select `+tupleColumns+` from (
			select distinct on (kind, from_kind, from_id) `+tupleColumns+`
			from priority_tuples
			where to_kind = $1 and to_id = $2 and to_kind <> '`+KindValue+`'
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
// the value itself. It binds to_kind = 'value', so the planner takes priority_tuples_values, the
// one read index that keys no column a value can reach. The second result is false where the node
// never carried the attribute and where its newest tuple retracts it, which are one thing to a
// reader: the node has no such attribute now.
func CurrentValue(ctx context.Context, q Queryer, fromKind, fromID, attribute string) (string, bool, error) {
	var value string
	var asserted bool
	err := q.QueryRow(ctx, `
		select to_id, asserted
		from priority_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = '`+KindValue+`'
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

// History answers every tuple of one node under one kind, in the order they were written, each
// with its value or its far end, its time and its writer. It is one read for an attribute and a
// relation alike: an attribute's history is three values under one name, a relation's is the
// asserting and retracting of far ends, and the rows have one shape, so a caller cannot tell from
// the answer which of the two it asked for.
func History(ctx context.Context, q Queryer, fromKind, fromID, kind string) ([]Tuple, error) {
	rows, err := q.Query(ctx, `
		select `+tupleColumns+`
		from priority_tuples
		where from_kind = $1 and from_id = $2 and kind = $3
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

// IssuesAdvancing answers the issues that advance one priority: the current `advances` relations
// into it, each tuple's FromID an issue key of any project. A relation whose newest tuple
// retracts it is absent.
func IssuesAdvancing(ctx context.Context, q Queryer, id string) ([]Tuple, error) {
	return CurrentIn(ctx, q, KindPriority, id, []string{RelationAdvances})
}

// PrioritiesAdvanced answers the priorities one issue advances: the current `advances` relations
// out of it, each tuple's ToID a priority id. It is the issue's own relations alone; what a child
// inherits from the issues above it is the caller's walk.
func PrioritiesAdvanced(ctx context.Context, q Queryer, issueKey string) ([]Tuple, error) {
	return CurrentOut(ctx, q, KindIssue, issueKey, []string{RelationAdvances})
}

// Timeline answers one priority's whole history, newest first: every attribute it was written
// with, and every `advances` relation asserted or retracted on it, each with its writer and its
// time. It is what the priority page's margin lists, and it is two index scans — the tuples out
// of the node and the `advances` tuples into it — merged on the log position they were appended
// at, which is the order they happened in.
func Timeline(ctx context.Context, q Queryer, id string) ([]Tuple, error) {
	rows, err := q.Query(ctx, `
		select `+tupleColumns+` from (
			select `+tupleColumns+`
			from priority_tuples
			where from_kind = '`+KindPriority+`' and from_id = $1
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

// kindList is the kinds parameter a read binds: the empty list is sent as null, which is how each
// statement spells "every kind".
func kindList(kinds []string) any {
	if len(kinds) == 0 {
		return nil
	}
	return kinds
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
