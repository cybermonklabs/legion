package embedqueue

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestCoverageReadsNoWindowsTextWhileFewPagesOfEmbeddingsAreAllVisible holds the report to its two
// window indexes on the table production has between two vacuums: pages written since the last
// vacuum are not all-visible, and an ANALYZE (autovacuum runs one once a tenth of the table's rows
// changed, at Postgres's default) records that share in pg_class.relallvisible, which is what the
// planner prices an index-only scan's heap fetches by.
//
// The test builds that table from the scale test's corpus at 0.005 x production's size
// (loadSmallCoverageCorpus), with autovacuum off on embeddings and every issue's source recorded,
// and fills the counts once after that load, whose triggers were off: one row of each page of
// embeddings updated, so no page is all-visible, then the table analyzed, so the statistics record
// fewer than a quarter of its pages all-visible, and the test fails unless they do. Coverage, run
// there, reads the kept counts and each window kind's oldest lacking row as the first entry of each
// window index (coverageRead), and must read embeddings by no sequential scan, which would run the
// blank test's regex over every window's text again, the cost the window indexes exist to save,
// and must read both window indexes, embeddings_window_embedded_since and
// embeddings_window_unembedded_since.
//
// That property is the one a planner left to its estimates breaks. It cannot estimate either half
// of the window rows: each predicate compares two columns (embedded_hash with content_hash), which
// it prices at its default selectivity, so it takes the embedded half, about 99% of the rows, for
// half a percent of them, and the unembedded half, about 1%, for nearly all of them (at
// production's size: 4,868 rows estimated against 974,072 read, and 977,422 against 9,840). While
// every page is all-visible that misestimate costs nothing; once statistics record about a third
// of the pages as not all-visible, a planner left free reads the unembedded half by a sequential
// scan of embeddings. INFRA-4241's tester measured Coverage on the scale test's corpus in a builds
// Job (8 CPUs, 16 GiB) with an earlier version of this statement, which left the planner free, the
// statistics analyzed after one row of a share of the pages was updated
// (dispatch://INFRA-4241/artifact/tester-scale-job-b3756b91-log):
//
//	pages not all-visible       none     23%     34%      55%      all
//	production's size (1 x)   269 ms   580 ms   3129 ms  3497 ms  4290 ms
//	twice it (2 x)            652 ms  1313 ms   6250 ms  6914 ms  8519 ms
//
// From 34% on the plan holds the sequential scan; the same statement with sequential and bitmap
// scans off took 930 ms and 2085 ms at 1 x, and 1878 ms and 4170 ms at 2 x, at 34% and at all.
// By the scale test's own two-point fit the route reaches its 10 s bound in that state at 3.2
// times production's size with 34% of the pages not all-visible, and at 2.3 times with all of
// them, where R2 of INFRA-4241 asks for at least 4.
//
// Coverage runs on a pool of one connection, so the statistics of its own statement are the ones
// read: pg_stat_force_next_flush makes that backend flush them before it next answers.
func TestCoverageReadsNoWindowsTextWhileFewPagesOfEmbeddingsAreAllVisible(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	// No autovacuum sets the visibility map while the test reads it.
	mustExec(t, database.Pool, `alter table embeddings set (autovacuum_enabled = false)`)
	loadSmallCoverageCorpus(t, database)
	recordOnce(t, database)
	fillCounts(t, database)

	url := database.Pool.Config().ConnString()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse the database's URL: %v", err)
	}
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a pool of one connection: %v", err)
	}
	t.Cleanup(pool.Close)
	single := &store.Store{Pool: store.NewPool(pool)}

	// One row of each page of embeddings updated, so no page is all-visible, and the statistics
	// say so.
	mustExec(t, single.Pool, `update embeddings e set pending_since = e.pending_since
		from (select distinct on ((ctid::text::point)[0]) ctid from embeddings
		      order by (ctid::text::point)[0], ctid) s
		where e.ctid = s.ctid`)
	mustExec(t, single.Pool, `analyze embeddings`)
	var visible, pages int64
	if err := single.Pool.QueryRow(ctx, `select relallvisible::bigint, relpages::bigint from pg_class where oid = 'embeddings'::regclass`).Scan(&visible, &pages); err != nil {
		t.Fatalf("read embeddings' statistics: %v", err)
	}
	if pages == 0 || visible*4 > pages {
		t.Fatalf("the statistics record %d of embeddings' %d pages all-visible; the test needs fewer than a quarter", visible, pages)
	}

	before := embeddingsScans(t, single)
	if _, err := Coverage(ctx, single, configuredIdentity); err != nil {
		t.Fatalf("coverage: %v", err)
	}
	after := embeddingsScans(t, single)
	if after.sequential != before.sequential {
		t.Errorf("the report read embeddings by %d sequential scans with %d of its %d pages all-visible, running the blank test over every window's text; want none",
			after.sequential-before.sequential, visible, pages)
	}
	if after.embedded == before.embedded {
		t.Errorf("the report read nothing from %s with %d of embeddings' %d pages all-visible", windowEmbeddedIndex, visible, pages)
	}
	if after.unembedded == before.unembedded {
		t.Errorf("the report read nothing from %s with %d of embeddings' %d pages all-visible", windowUnembeddedIndex, visible, pages)
	}
}

// embeddingScans is how often embeddings was read by a sequential scan and each window index by
// an index scan, as the statistics hold them.
type embeddingScans struct {
	sequential, embedded, unembedded int64
}

// embeddingsScans flushes the statistics of the one connection of single's pool, then reads
// embeddings' scans from them in a statement of its own.
func embeddingsScans(t *testing.T, single *store.Store) embeddingScans {
	t.Helper()
	ctx := context.Background()
	mustExec(t, single.Pool, `select pg_stat_force_next_flush()`)
	var scans embeddingScans
	if err := single.Pool.QueryRow(ctx, `select
		(select seq_scan from pg_stat_user_tables where relid = 'embeddings'::regclass),
		(select idx_scan from pg_stat_user_indexes where indexrelid = $1::regclass),
		(select idx_scan from pg_stat_user_indexes where indexrelid = $2::regclass)`, windowEmbeddedIndex, windowUnembeddedIndex).
		Scan(&scans.sequential, &scans.embedded, &scans.unembedded); err != nil {
		t.Fatalf("read embeddings' scans: %v", err)
	}
	return scans
}
