package storesmove

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// mover is one move's settings, as Options gives them with each zero taken to its default.
type mover struct {
	batch, stopAfter, swapAttempts int
}

func newMover(options Options) mover {
	m := mover{batch: options.Batch, stopAfter: options.StopAfter, swapAttempts: options.SwapAttempts}
	if m.batch <= 0 {
		m.batch = DefaultBatch
	}
	if m.swapAttempts <= 0 {
		m.swapAttempts = SwapAttempts
	}
	return m
}

// copy is the move's copy, before its swap: for each store in turn, the tuples of its table that
// the record does not hold, in the order of their seq, m.batch at a time, each batch in a
// transaction of its own (copyBatch) that commits its tuples, their node rows and their record
// together, so a move that stops after any batch leaves no tuple a later move copies again. It
// takes no table lock beyond its reads and its inserts. A tuple that commits below a seq the copy
// has passed is the swap's to move. It answers how many tuples it moved and in how many batches,
// and ErrStopped right after the commit of the m.stopAfter-th batch.
func (m mover) copy(ctx context.Context, conn *pgx.Conn) (copied, batches int, err error) {
	for _, old := range oldStores {
		var after int64
		for {
			moved, last, err := m.copyCommitted(ctx, conn, old, after)
			if err != nil {
				return copied, batches, err
			}
			if moved == 0 {
				break
			}
			copied += moved
			batches++
			after = last
			if m.stopAfter > 0 && batches >= m.stopAfter {
				return copied, batches, fmt.Errorf("%w: %d batches of at most %d tuples, %d tuples", ErrStopped, batches, m.batch, copied)
			}
		}
	}
	return copied, batches, nil
}

// copyCommitted runs one batch of the copy (copyBatch) in a transaction of its own and commits it,
// or rolls back one that moved nothing.
func (m mover) copyCommitted(ctx context.Context, conn *pgx.Conn, old oldStore, after int64) (int, int64, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("the stores' copy: begin a batch of %s: %w", old.tuples, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	moved, last, err := m.copyBatch(ctx, tx, old, after)
	if err != nil || moved == 0 {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("the stores' copy: commit a batch of %s: %w", old.tuples, err)
	}
	return moved, last, nil
}

// copyAll moves, in tx, every tuple of both stores that the record does not hold, m.batch at a
// time, as the swap and the rehearsal do inside their one transaction. It answers how many it
// moved.
func (m mover) copyAll(ctx context.Context, tx pgx.Tx) (int, error) {
	copied := 0
	for _, old := range oldStores {
		var after int64
		for {
			moved, last, err := m.copyBatch(ctx, tx, old, after)
			if err != nil {
				return copied, err
			}
			if moved == 0 {
				break
			}
			copied += moved
			after = last
		}
	}
	return copied, nil
}

// copyBatch moves, in tx, the next m.batch tuples of old's table above the seq after that the
// record does not hold, in the order of their seq, and answers how many it moved and the seq of the
// last. One batch writes:
//
//   - before a batch of the priority store, draws from the graph's sequence until it passes the
//     greatest seq of priority_tuples (drawPast), so each tuple it moves takes a seq above every
//     revision the release before answered of an issue's advances set;
//   - each tuple through graphstore with its from, kind, to, at, who, source_event and asserted,
//     under a new seq, in the order of its old seq (writeBatch);
//   - again, from its old row, the node row of each node of old's kinds that those tuples come from
//     or point at, as a projection alone (graphstore.Node.Projection), since each fact of the row
//     arrives as a moved tuple, in this batch, an earlier one or a later one;
//   - for each run and plan that enters the store in this batch, its visibility and state, which
//     the old row holds as columns and no tuple, as value tuples dated at its made_at, by its
//     made_by, under its cause; and, for one whose row holds no project attribute, its issue's
//     project as that attribute, dated the same. Its project_key is its project;
//   - the record of the store and the old seq of each tuple it moved.
func (m mover) copyBatch(ctx context.Context, tx pgx.Tx, old oldStore, after int64) (int, int64, error) {
	tuples, seqs, err := readTupleBatch(ctx, tx, old, after, m.batch)
	if err != nil || len(tuples) == 0 {
		return 0, 0, err
	}
	if old.name == priorityStore.name {
		if err := drawPast(ctx, tx); err != nil {
			return 0, 0, err
		}
	}
	nodes, stated, err := readNodes(ctx, tx, old, tuples)
	if err != nil {
		return 0, 0, err
	}
	if err := writeBatch(ctx, tx, old, nodes, stated, tuples, seqs); err != nil {
		return 0, 0, err
	}
	return len(tuples), seqs[len(seqs)-1], nil
}

// readTupleBatch reads the next limit tuples of old's table above the seq after that the record
// does not hold, in the order of their seq, with their old seqs. It reads the table through its
// primary key from after on, and probes the record through its own, stores_moved_seqs_pkey.
func readTupleBatch(ctx context.Context, tx pgx.Tx, old oldStore, after int64, limit int) ([]graphstore.Tuple, []int64, error) {
	rows, err := tx.Query(ctx, `
		select t.seq, t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.at, t.who, t.source_event, t.asserted
		from `+old.tuples+` t
		where t.seq > $1
		  and not exists (
			select 1 from stores_moved_seqs moved where moved.store = $2 and moved.seq = t.seq)
		order by t.seq
		limit $3
	`, after, old.name, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("the stores' copy: read a batch of %s: %w", old.tuples, err)
	}
	defer rows.Close()
	var tuples []graphstore.Tuple
	var seqs []int64
	for rows.Next() {
		var seq int64
		var who []byte
		var tuple graphstore.Tuple
		if err := rows.Scan(&seq, &tuple.FromKind, &tuple.FromID, &tuple.Kind, &tuple.ToKind, &tuple.ToID,
			&tuple.At, &who, &tuple.SourceEvent, &tuple.Asserted); err != nil {
			return nil, nil, fmt.Errorf("the stores' copy: read a tuple of %s: %w", old.tuples, err)
		}
		if err := json.Unmarshal(who, &tuple.Who); err != nil {
			return nil, nil, fmt.Errorf("the stores' copy: decode the writer of %s seq %d: %w", old.tuples, seq, err)
		}
		tuples = append(tuples, tuple)
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("the stores' copy: read a batch of %s: %w", old.tuples, err)
	}
	return tuples, seqs, nil
}

// drawPast draws from the graph's sequence with nextval until it passes the greatest seq of
// priority_tuples, the max of that table's primary key. A revision the release before answered of
// an issue's advances set is the old seq of a tuple of that table, and is tested by equality
// (api's refuseAdvancesRevision): so every tuple the move appends after this draw, the newest
// tuple of each issue's set among them, takes a seq above every such revision, and no revision the
// store answers after the move equals one the release before answered. Where the sequence has
// passed it already, as in a store the backfill filled, it draws one value.
func drawPast(ctx context.Context, tx pgx.Tx) error {
	var greatest int64
	if err := tx.QueryRow(ctx, `select coalesce(max(seq), 0) from priority_tuples`).Scan(&greatest); err != nil {
		return fmt.Errorf("the stores' copy: read the greatest seq of priority_tuples: %w", err)
	}
	var drawn int64
	if err := tx.QueryRow(ctx, `select nextval(pg_get_serial_sequence('graph_tuples', 'seq'))`).Scan(&drawn); err != nil {
		return fmt.Errorf("the stores' copy: draw from the graph's sequence: %w", err)
	}
	for drawn <= greatest {
		if err := tx.QueryRow(ctx, `
			select max(nextval(pg_get_serial_sequence('graph_tuples', 'seq'))) from generate_series(1, $1::bigint)
		`, greatest-drawn+1).Scan(&drawn); err != nil {
			return fmt.Errorf("the stores' copy: draw the graph's sequence past %d: %w", greatest, err)
		}
	}
	return nil
}

// readNodes reads, from old's node table, the row of each node of old's kinds that tuples come
// from or point at, as the projection node the batch writes again (graphstore.Node.Projection),
// and the base attributes the batch states for each run and plan that enters the store with it.
func readNodes(ctx context.Context, tx pgx.Tx, old oldStore, tuples []graphstore.Tuple) ([]graphstore.Node, []graphstore.Tuple, error) {
	type key struct{ kind, id string }
	var keys []key
	for _, tuple := range tuples {
		for _, end := range [...]key{{tuple.FromKind, tuple.FromID}, {tuple.ToKind, tuple.ToID}} {
			if slices.Contains(old.kinds, end.kind) && !slices.Contains(keys, end) {
				keys = append(keys, end)
			}
		}
	}
	if len(keys) == 0 {
		return nil, nil, nil
	}
	kinds, ids := make([]string, len(keys)), make([]string, len(keys))
	for index, named := range keys {
		kinds[index], ids[index] = named.kind, named.id
	}
	// The node table's primary key reads each row, and graph_nodes_pkey each node the store holds.
	rows, err := tx.Query(ctx, `
		select o.kind, o.id, o.supertag, o.visibility, o.state, o.attributes, o.made_at, o.made_by, o.source_event,
		       exists (select 1 from graph_nodes held where held.kind = o.kind and held.id = o.id)
		from `+old.nodes+` o
		where (o.kind, o.id) in (select * from unnest($1::text[], $2::text[]))
		order by o.kind, o.id
	`, kinds, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("the stores' copy: read the node rows of %s: %w", old.nodes, err)
	}
	type read struct {
		node       graphstore.Node
		held       bool
		hasProject bool
		issue      string
	}
	var nodes []read
	for rows.Next() {
		var row read
		var attributes, madeBy []byte
		node := &row.node
		if err := rows.Scan(&node.Kind, &node.ID, &node.Supertag, &node.Visibility, &node.State, &attributes,
			&node.MadeAt, &madeBy, &node.SourceEvent, &row.held); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("the stores' copy: read a node row of %s: %w", old.nodes, err)
		}
		if node.Attributes, err = decodeAttributes(attributes); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("the stores' copy: decode the attributes of %s/%s: %w", node.Kind, node.ID, err)
		}
		if err := json.Unmarshal(madeBy, &node.MadeBy); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("the stores' copy: decode the writer of %s/%s: %w", node.Kind, node.ID, err)
		}
		node.Projection = true
		if old.name == workflowStore.name {
			row.issue, _ = node.Attributes[workflowstore.AttributeIssue].(string)
			var project any
			project, row.hasProject = node.Attributes[workflowstore.AttributeProject]
			node.Project, _ = project.(string)
		}
		nodes = append(nodes, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("the stores' copy: read the node rows of %s: %w", old.nodes, err)
	}

	// A run or a plan that holds no project attribute takes its issue's, read through
	// issues_pkey.
	var issues []string
	for _, row := range nodes {
		if old.name == workflowStore.name && !row.hasProject && row.issue != "" && !slices.Contains(issues, row.issue) {
			issues = append(issues, row.issue)
		}
	}
	projects := map[string]string{}
	if len(issues) > 0 {
		rows, err := tx.Query(ctx, `select key, project_key from issues where key = any($1)`, issues)
		if err != nil {
			return nil, nil, fmt.Errorf("the stores' copy: read the projects of the runs' and plans' issues: %w", err)
		}
		for rows.Next() {
			var issue, project string
			if err := rows.Scan(&issue, &project); err != nil {
				rows.Close()
				return nil, nil, fmt.Errorf("the stores' copy: read the project of an issue: %w", err)
			}
			projects[issue] = project
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, fmt.Errorf("the stores' copy: read the projects of the runs' and plans' issues: %w", err)
		}
	}

	written := make([]graphstore.Node, len(nodes))
	var stated []graphstore.Tuple
	for index, row := range nodes {
		node := row.node
		if old.name == workflowStore.name && !row.hasProject {
			node.Project = projects[row.issue]
		}
		written[index] = node
		if old.name != workflowStore.name || row.held {
			continue
		}
		stated = append(stated,
			statedValue(node, graphstore.AttributeVisibility, node.Visibility),
			statedValue(node, graphstore.AttributeState, node.State))
		if !row.hasProject && node.Project != "" {
			stated = append(stated, statedValue(node, graphstore.AttributeProject, node.Project))
		}
	}
	return written, stated, nil
}

// statedValue is one base attribute the copy states of a run or a plan as it enters the store:
// dated at the node's made_at, by its made_by, under its cause.
func statedValue(node graphstore.Node, attribute, value string) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: node.Kind, FromID: node.ID, Kind: attribute,
		ToKind: graphstore.KindValue, ToID: value,
		At: node.MadeAt, Who: node.MadeBy, SourceEvent: node.SourceEvent, Asserted: true,
	}
}

// decodeAttributes reads a node row's attributes as the store's writer takes them, each number
// kept as the text it was written as (json.Number), so the row the store writes holds the values
// the old row held, a plan's tasks and a run's generation among them.
func decodeAttributes(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var attributes map[string]any
	if err := decoder.Decode(&attributes); err != nil {
		return nil, err
	}
	return attributes, nil
}

// writeBatch writes one batch in tx, as one write phase (store.RunWrite): the node rows and the
// stated values first (graphstore.QueueWrite), then the moved tuples in the order of their old
// seq, then the record of their old seqs. graphstore appends a write's relations ahead of its value
// tuples, so the moved tuples go in as runs of one class each, relations or values, one write a
// run, and each node's tuples keep the order of their old seq. Last come the head rows of every node
// the batch wrote or appended a tuple out of (graphstore.QueueHeads), as every writer of the store
// writes them: an `advances` tuple is a relation out of an issue's node, whose head row holds it.
func writeBatch(ctx context.Context, tx pgx.Tx, old oldStore, nodes []graphstore.Node, stated, tuples []graphstore.Tuple, seqs []int64) error {
	err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		if len(nodes) > 0 || len(stated) > 0 {
			graphstore.QueueWrite(b, nodes, stated)
		}
		for start := 0; start < len(tuples); {
			end := start + 1
			for end < len(tuples) && isValue(tuples[end]) == isValue(tuples[start]) {
				end++
			}
			graphstore.QueueWrite(b, nil, tuples[start:end])
			start = end
		}
		graphstore.QueueHeads(b, graphstore.HeadsOf(nodes, slices.Concat(stated, tuples)))
		b.Queue("record the tuples the stores' move copied", `
			insert into stores_moved_seqs (store, seq) select $1, unnest($2::bigint[])
		`, old.name, seqs)
		return nil
	})
	if err != nil {
		return fmt.Errorf("the stores' copy: write a batch of %s (seqs %d to %d): %w", old.tuples, seqs[0], seqs[len(seqs)-1], err)
	}
	return nil
}

// isValue is whether tuple is an attribute's value tuple rather than a relation.
func isValue(tuple graphstore.Tuple) bool { return tuple.ToKind == graphstore.KindValue }
