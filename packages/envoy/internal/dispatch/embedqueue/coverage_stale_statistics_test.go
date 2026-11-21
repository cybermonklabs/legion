package embedqueue

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestCoverageFetchesNoWindowRowByRowWhileTheStatisticsLagTheVisibilityMap holds the report to a
// plan that does not visit the heap once for each window row on a table whose statistics
// (pg_class.relallvisible over relpages) record more of embeddings as all-visible than is.
//
// The test builds that table from the scale test's corpus at 0.005 x production's size
// (loadSmallCoverageCorpus), with autovacuum off on embeddings and every issue's source recorded:
// embeddings vacuumed, so every page is all-visible; one row of every even page updated and the
// table analyzed, so the statistics record about half the pages all-visible, and the test fails
// unless they record at least laggingVisibleShare; then one row of every odd page updated with no
// ANALYZE after it, so no page is all-visible while the statistics still record that share. A
// production table can stand so until its next ANALYZE: the statistics change only at a VACUUM
// or an ANALYZE, and autovacuum runs an ANALYZE of a table only once a tenth of its rows (plus
// 50) changed since the last one, at Postgres's default, while one row of each of half the pages
// is fewer (97,369 rows of the scale test's corpus at production's size, below that tenth,
// 99,566). On that table the coverage statement, explained with ANALYZE under planCoverage's
// settings as Coverage runs it, must fetch fewer row versions from the heap through the window
// indexes than half the window rows: an index-only scan fetches each row of a page that is not
// all-visible from the heap, and a plain index scan every row it returns, one page visit for each.
//
// An earlier version of this statement read each window half by an index-only scan of its own
// index whenever the statistics recorded at least 0.3 of the pages all-visible, the share
// laggingVisibleShare holds this table to, and so fetched every window row of this table from the
// heap one at a time. INFRA-4241's tester measured it in a builds Job (8 CPUs, 16 GiB) on the
// scale test's corpus with every page of embeddings not all-visible
// (dispatch://INFRA-4241/artifact/tester-r2-job-2f9a6fc1-log): the index-only plan took 3,536 ms
// at production's size and 7,668 ms at twice it, so it reaches the route's 10 s bound at 2.5 times
// production's size, below the 4 times R2 of INFRA-4241 asks for; the bitmap plan took 1,329 ms
// and 3,355 ms in the same state. With statistics that recorded the state, that version read the
// windows by the bitmap and Coverage took 1,339 ms and 2,963 ms.
func TestCoverageFetchesNoWindowRowByRowWhileTheStatisticsLagTheVisibilityMap(t *testing.T) {
	opened := storetest.Open(t)
	ctx := context.Background()
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
	// the test makes the state. heap is the relation whose pages hold the corpus's rows: the
	// partition of project CORPUS on embeddings partitioned by project, where the visibility map
	// and the statistics the planner prices a partition's scan by are that partition's own.
	heap := disableCorpusAutovacuum(t, database.Pool)
	loadSmallCoverageCorpus(t, database)
	recordOnce(t, database)
	mustExec(t, database.Pool, `vacuum (analyze) embeddings`)
	var vacuumed float64
	if err := database.Pool.QueryRow(ctx, coverageVisibleShare(heap)).Scan(&vacuumed); err != nil {
		t.Fatalf("read the share of %s the statistics record as all-visible: %v", heap, err)
	}
	if vacuumed < 0.99 {
		t.Fatalf("the statistics record %.3f of %s as all-visible after its vacuum; the test needs every page", vacuumed, heap)
	}

	// Half the pages written, and an ANALYZE that records it.
	mustExec(t, database.Pool, updateOneRowOfPages(heap, "where ((ctid::text::point)[0])::int % 2 = 0"))
	mustExec(t, database.Pool, `analyze embeddings`)
	var recorded float64
	if err := database.Pool.QueryRow(ctx, coverageVisibleShare(heap)).Scan(&recorded); err != nil {
		t.Fatalf("read the share of %s the statistics record as all-visible: %v", heap, err)
	}
	if recorded < laggingVisibleShare {
		t.Fatalf("the statistics record %.3f of embeddings as all-visible after half its pages were written; the test needs at least %g (laggingVisibleShare)",
			recorded, laggingVisibleShare)
	}
	// Then one row of every other page written, with no ANALYZE after it: no page is all-visible,
	// and the statistics still record the share above.
	mustExec(t, database.Pool, updateOneRowOfPages(heap, "where ((ctid::text::point)[0])::int % 2 = 1"))

	var windows int64
	if err := database.Pool.QueryRow(ctx, `select count(*) from embeddings where `+windowItems).Scan(&windows); err != nil {
		t.Fatalf("count the window rows: %v", err)
	}
	tx, err := database.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin the report's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := planCoverage(ctx, tx); err != nil {
		t.Fatalf("plan the report as Coverage does: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `explain (analyze, format json) `+coverageQuery, configuredIdentity, everyProjectArg).Scan(&raw); err != nil {
		t.Fatalf("explain the coverage statement: %v", err)
	}
	explained := explaintest.OnePlan(t, raw)
	var fetched float64
	var rowByRow []string
	for _, node := range explaintest.Nodes(t, explained) {
		if node.Relation != "embeddings" || (node.Index != "embeddings_window_embedded" && node.Index != "embeddings_window_unembedded") {
			continue
		}
		switch node.Type {
		case "Index Only Scan":
			fetched += node.HeapFetches
		case "Index Scan":
			// A plain index scan visits the heap for every row it returns.
			fetched += node.Rows * node.Loops
			rowByRow = append(rowByRow, node.Index)
		}
	}
	// A heap fetch can visit a row version an update left behind, so a plan that fetches row by row
	// can fetch more versions than there are window rows; half the rows is the bound.
	if fetched*2 > float64(windows) {
		t.Errorf("with no page of embeddings all-visible and the statistics recording %.3f, the coverage statement made %.0f heap fetches, one row version at a time, to count %d window rows (plain index scans: %v):\n%s",
			recorded, fetched, windows, rowByRow, explained)
	}
}
