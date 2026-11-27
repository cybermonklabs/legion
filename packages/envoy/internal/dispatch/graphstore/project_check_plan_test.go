package graphstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestANodeWritesProjectCheckReadsItsBatchOnce holds the node write's project check (queueNodes)
// to one read of the batch's nodes, however the planner joins them. The check runs after the
// batch's own insert, so the nodes it reads back are rows graph_nodes' statistics do not count
// yet. Over a table analyzed while it held few nodes and none that names a project, those
// statistics put about one row in graph_nodes_project, and a plain join of the incoming array to
// graph_nodes was planned in CI as a nested loop that walks graph_nodes_project and reads the
// whole array again for each row it finds: a settlement of 150,000 block nodes held that one
// statement for minutes. Which join the planner takes over such statistics turns on costs a test
// cannot pin (on this test's 2,000 nodes it takes a hash join), so the test turns the hash and
// merge joins off and leaves the planner the nested loop CI's plan used.
//
// The test writes 100 nodes that name no project, analyzes graph_nodes, writes 2,000 new nodes in
// a project in one transaction, and in that transaction reads the plan Postgres runs for the check
// the write sent, with its own arguments (EXPLAIN ANALYZE): the scan of the incoming array, the
// plan's one function scan, runs once and reads each node once. It counts what the plan read and
// times nothing. Its control is the check before it probed each node by its key: the plain join,
// whose array scan runs once for each row of graph_nodes_project.
func TestANodeWritesProjectCheckReadsItsBatchOnce(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	who := model.Actor{Kind: "user", ID: "alice"}
	at := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	people := make([]Node, 100)
	for index := range people {
		people[index] = PersonNode(fmt.Sprintf("plan-person-%d", index+1), who, at, NoEvent)
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error { return Write(ctx, tx, people, nil) }); err != nil {
		t.Fatalf("write %d nodes that name no project: %v", len(people), err)
	}
	if _, err := database.Pool.Exec(ctx, `analyze graph_nodes`); err != nil {
		t.Fatalf("analyze graph_nodes while no node of it names a project: %v", err)
	}
	check := &projectCheckStatement{}
	traced := storetest.TracedStore(t, database, check)
	const batch = 2000
	nodes := make([]Node, batch)
	for index := range nodes {
		nodes[index] = IssueNode(fmt.Sprintf("PLAN-%d", index+1), "PLAN", who, at)
	}
	// The plan Postgres runs for the check depends on how the statement is planned: with the
	// batch's own arrays (a custom plan, as the first executions of a prepared statement are) or
	// once for any arrays (the generic plan a prepared statement settles on after five, which knows
	// no array's length). pgx prepares and caches the statements it sends, so a settlement of many
	// batches runs the generic plan. The test reads both.
	plans := map[string][]map[string]json.RawMessage{}
	errRolledBack := errors.New("rolled back")
	err := pgx.BeginFunc(ctx, traced.Pool, func(tx pgx.Tx) error {
		if err := Write(ctx, tx, nodes, nil); err != nil {
			return err
		}
		sql, args := check.read()
		if sql == "" {
			return errors.New("the node write sent no project check")
		}
		if _, err := tx.Exec(ctx, "prepare project_check as "+sql); err != nil {
			return fmt.Errorf("prepare the project check: %w", err)
		}
		execute := "explain (analyze, format json) execute project_check" + argumentLiterals(t, args)
		for _, planned := range []struct{ name, mode string }{
			{"custom", "force_custom_plan"},
			{"generic", "force_generic_plan"},
		} {
			if _, err := tx.Exec(ctx, "set local plan_cache_mode = "+planned.mode); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "set local enable_hashjoin = off"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "set local enable_mergejoin = off"); err != nil {
				return err
			}
			var plan []byte
			if err := tx.QueryRow(ctx, execute).Scan(&plan); err != nil {
				return fmt.Errorf("explain the project check's %s plan: %w", planned.name, err)
			}
			var explained []map[string]json.RawMessage
			if err := json.Unmarshal(plan, &explained); err != nil || len(explained) != 1 {
				return fmt.Errorf("decode the project check's %s plan: %d plan(s), %v", planned.name, len(explained), err)
			}
			plans[planned.name] = explained
		}
		return errRolledBack
	})
	if !errors.Is(err, errRolledBack) {
		t.Fatalf("write %d nodes and explain their project check: %v", batch, err)
	}
	for _, name := range []string{"custom", "generic"} {
		var loops, rows []float64
		var shape []string
		for _, node := range planNodes(t, plans[name][0]["Plan"]) {
			shape = append(shape, fmt.Sprintf("%s %s %s x%.0f rows %.0f", node.NodeType, node.Relation, node.Index, node.ActualLoops, node.ActualRows))
			if node.NodeType == "Function Scan" {
				loops = append(loops, node.ActualLoops)
				rows = append(rows, node.ActualLoops*node.ActualRows)
			}
		}
		t.Logf("the project check's %s plan: %s", name, strings.Join(shape, "; "))
		if len(loops) != 1 || loops[0] != 1 || rows[0] != batch {
			t.Errorf("the project check's %s plan scanned the incoming array %v time(s), reading %v row(s) in all; want one scan reading the batch's %d nodes once. Its plan: %s",
				name, loops, rows, batch, strings.Join(shape, "; "))
		}
	}
}

// projectCheckStatement is a pgx tracer that keeps the node write's project check, the one
// statement of a batch that reads the incoming nodes' projects back, with the arguments it was
// sent with.
type projectCheckStatement struct {
	mu   sync.Mutex
	sql  string
	args []any
}

func (c *projectCheckStatement) read() (string, []any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sql, c.args
}

func (*projectCheckStatement) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (*projectCheckStatement) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (*projectCheckStatement) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (c *projectCheckStatement) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if !strings.Contains(data.SQL, "as incoming(kind, id, project_key)") {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sql, c.args = data.SQL, data.Args
}

func (*projectCheckStatement) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
