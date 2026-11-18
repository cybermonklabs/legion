package embedqueue

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestCoverageFetchesNoWindowRowByRowWhileTheStatisticsLagTheVisibilityMap holds the report to a
// plan that does not visit the heap once for each window row when the statistics planCoverage
// reads (pg_class.relallvisible over relpages) record more of embeddings as all-visible than is.
//
// The statistics change only at a VACUUM or an ANALYZE, and autovacuum runs an ANALYZE of a table
// only once a tenth of its rows (plus 50) changed since the last one, at Postgres's default. So
// after an ANALYZE records, say, half the pages all-visible, writes to one row of each of those
// pages - 97,369 rows of the scale test's corpus at production's size, below that tenth, 99,566 -
// leave no page all-visible while the statistics still record half. planCoverage then chooses
// the index-only plan, which fetches every window row of a page that is not all-visible from the
// heap, one page visit for each row.
//
// INFRA-4241's tester measured it in a builds Job (8 CPUs, 16 GiB) at 2f9a6fc1 on the scale
// test's corpus with every page of embeddings not all-visible: the index-only plan took 3,536 ms
// at production's size and 7,668 ms at twice it, so it reaches the route's 10 s bound at 2.5 times
// production's size, below the 4 times R2 of INFRA-4241 asks for; the bitmap plan took 1,329 ms
// and 3,355 ms in the same state. With statistics that recorded the state, planCoverage chose the
// bitmap plan and Coverage took 1,339 ms and 2,963 ms.
func TestCoverageFetchesNoWindowRowByRowWhileTheStatisticsLagTheVisibilityMap(t *testing.T) {
	opened := storetest.Open(t)
	ctx := context.Background()
	script, err := os.ReadFile(filepath.Join("testdata", "coverage_corpus.sql"))
	if err != nil {
		t.Fatalf("read the corpus script: %v", err)
	}
	// Every session of the database commits synchronously, whatever the server's setting, as the
	// scale test's do (measureCoverageAtScale says why): with asynchronous commits the vacuum below
	// marked no page all-visible in four runs of five on a pod's test server. A session takes the
	// setting when it starts, so the test works on a store opened after it.
	mustExec(t, opened.Pool, `do $$ begin execute format('alter database %I set synchronous_commit = on', current_database()); end $$`)
	database, err := store.Open(ctx, opened.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the database again: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	// No autovacuum or autoanalyze of embeddings sets its visibility map or its statistics while
	// the test makes the state.
	mustExec(t, database.Pool, `alter table embeddings set (autovacuum_enabled = false)`)
	loadCoverageCorpus(t, database.Pool.Config().ConnString(), coverageScale{value: 0.005, text: "0.005"}, repositoryProse(t), string(script))
	mustExec(t, database.Pool, `analyze `+coverageScaleTables)
	recordOnce(t, database)
	mustExec(t, database.Pool, `vacuum (analyze) embeddings`)
	var vacuumed float64
	if err := database.Pool.QueryRow(ctx, coverageVisibleShare).Scan(&vacuumed); err != nil {
		t.Fatalf("read the share of embeddings the statistics record as all-visible: %v", err)
	}
	if vacuumed < 0.99 {
		t.Fatalf("the statistics record %.3f of embeddings as all-visible after its vacuum; the test needs every page", vacuumed)
	}

	// Half the pages written, and an ANALYZE that records it.
	mustExec(t, database.Pool, updateOneRowOfPages("where ((ctid::text::point)[0])::int % 2 = 0"))
	mustExec(t, database.Pool, `analyze embeddings`)
	var recorded float64
	if err := database.Pool.QueryRow(ctx, coverageVisibleShare).Scan(&recorded); err != nil {
		t.Fatalf("read the share of embeddings the statistics record as all-visible: %v", err)
	}
	if recorded < coverageIndexOnlyShare {
		t.Fatalf("the statistics record %.3f of embeddings as all-visible after half its pages were written; the test needs at least %g, the share planCoverage reads the index-only plan from",
			recorded, coverageIndexOnlyShare)
	}
	// Then one row of every other page written, with no ANALYZE after it: no page is all-visible,
	// and the statistics still record the share above.
	mustExec(t, database.Pool, updateOneRowOfPages("where ((ctid::text::point)[0])::int % 2 = 1"))

	var windows int64
	if err := database.Pool.QueryRow(ctx, `select count(*) from embeddings where `+windowItems).Scan(&windows); err != nil {
		t.Fatalf("count the window rows: %v", err)
	}
	tx, err := database.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin the report's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	plan, err := planCoverage(ctx, tx)
	if err != nil {
		t.Fatalf("plan the report as Coverage does: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `explain (analyze, format json) `+plan.query, configuredIdentity).Scan(&raw); err != nil {
		t.Fatalf("explain the %s plan: %v", plan.name, err)
	}
	explained := explaintest.OnePlan(t, raw)
	var root staleStatisticsPlanNode
	if err := json.Unmarshal(explained, &root); err != nil {
		t.Fatalf("decode the %s plan: %v", plan.name, err)
	}
	var fetched float64
	var rowByRow []string
	root.walk(func(node staleStatisticsPlanNode) {
		if node.RelationName != "embeddings" || (node.IndexName != "embeddings_window_embedded" && node.IndexName != "embeddings_window_unembedded") {
			return
		}
		switch node.NodeType {
		case "Index Only Scan":
			fetched += node.HeapFetches
		case "Index Scan":
			// A plain index scan visits the heap for every row it returns.
			fetched += node.ActualRows * node.ActualLoops
			rowByRow = append(rowByRow, node.IndexName)
		}
	})
	// A heap fetch can visit a row version an update left behind, so a plan that fetches row by row
	// can fetch more versions than there are window rows; half the rows is the bound.
	if fetched*2 > float64(windows) {
		t.Errorf("with no page of embeddings all-visible and the statistics recording %.3f, planCoverage chose the %s plan, which made %.0f heap fetches, one row version at a time, to count %d window rows (plain index scans: %v):\n%s",
			recorded, plan.name, fetched, windows, rowByRow, explained)
	}
}

// staleStatisticsPlanNode is what TestCoverageFetchesNoWindowRowByRowWhileTheStatisticsLagTheVisibilityMap
// reads of one node of an EXPLAIN (ANALYZE, FORMAT JSON) plan.
type staleStatisticsPlanNode struct {
	NodeType     string                    `json:"Node Type"`
	RelationName string                    `json:"Relation Name"`
	IndexName    string                    `json:"Index Name"`
	HeapFetches  float64                   `json:"Heap Fetches"`
	ActualRows   float64                   `json:"Actual Rows"`
	ActualLoops  float64                   `json:"Actual Loops"`
	Plans        []staleStatisticsPlanNode `json:"Plans"`
}

// walk calls visit on n and on every node under it, each before its children.
func (n staleStatisticsPlanNode) walk(visit func(staleStatisticsPlanNode)) {
	visit(n)
	for _, child := range n.Plans {
		child.walk(visit)
	}
}
