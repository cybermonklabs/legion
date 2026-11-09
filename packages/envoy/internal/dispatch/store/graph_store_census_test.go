package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// graphStoreVersion is the migration that creates the node and tuple store. The two versions
// after it are the test's own pending migrations: the database it migrates records neither, so
// the census reads each as pending and the runner applies the first.
const (
	graphStoreVersion      = 20261109013400
	graphIndexVersion      = 20261109013401
	graphUniqueIndexFile   = "20261109013402_graph_tuples_kind_seq.up.sql"
	graphUniqueIndexNumber = 20261109013402
	graphIndexFile         = "20261109013401_graph_tuples_kind.up.sql"
)

// graphCensusLimit is the size this run calls large. pgmigrate.CensusTableLimit is a gigabyte,
// which no test fills, so each census here is given a limit of its own
// (pgmigrate.CensusOptions.TableLimit) and graph_tuples is filled past twice it. Twice, because
// the census prints a size and its limit in bytes rather than in MiB where the two round to the
// same figure, and the refusals this test reads are the MiB ones.
const graphCensusLimit int64 = 1 << 20

// The index this test asks for, in the shapes the route tells apart. The marked migration is the
// body under the first line that selects the mode, so the unmarked migration is that same index
// with the marker taken off: the one difference between the refusal and the pass below.
const (
	graphIndexBody        = "create index concurrently if not exists graph_tuples_kind on graph_tuples (kind);\n"
	graphMarkedIndexSQL   = "-- pgmigrate:non-transactional\n" + graphIndexBody
	graphUniqueMarkedSQL  = "-- pgmigrate:non-transactional\ncreate unique index concurrently if not exists graph_tuples_kind_seq on graph_tuples (kind, seq);\n"
	graphTuplesIndex      = "graph_tuples_kind"
	graphIndexMigrationID = "migrations/" + graphIndexFile
)

// graphIndexMigration is a pending set of the one migration file name holding sql.
func graphIndexMigration(name, sql string) fstest.MapFS {
	return fstest.MapFS{"migrations/" + name: {Data: []byte(sql)}}
}

// mebibytes writes n as the census writes a size in this band (pgmigrate's humanBytes), so the
// test can ask for a refusal's whole sentence rather than a fragment of it.
func mebibytes(n int64) string {
	return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
}

// logCensusVerdict prints what the census answered about what: its own closing line and every
// refusal, so a reader of the log sees each verdict rather than only that the test passed.
func logCensusVerdict(t *testing.T, what string, report *pgmigrate.Report) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(reportText(report)), "\n")
	t.Logf("%s: %s", what, lines[len(lines)-1])
	for _, reason := range refusals(report) {
		t.Logf("%s: refusal: %s", what, reason)
	}
}

// graphTuplesCensus is the census of the one pending migration the test handed in, and what that
// census read about graph_tuples.
func graphTuplesCensus(t *testing.T, report *pgmigrate.Report) (pgmigrate.MigrationCensus, pgmigrate.TableCensus) {
	t.Helper()
	if len(report.Pending) != 1 {
		t.Fatalf("census read %d pending migrations, want the one the test handed it:\n%s", len(report.Pending), reportText(report))
	}
	entry := report.Pending[0]
	for _, table := range entry.Tables {
		if table.Name != "graph_tuples" {
			continue
		}
		if !table.Exists {
			t.Fatalf("the census did not find graph_tuples in the catalog")
		}
		return entry, table
	}
	t.Fatalf("the census read %+v for %s, none of them graph_tuples", entry.Tables, entry.Migration.Name)
	return entry, pgmigrate.TableCensus{}
}

// censusOfHandBuiltMigrations takes the census of migrations pgmigrate.Load refuses to build, on
// a connection of its own, as store's own census does for a set it can load. The UNIQUE
// concurrent build below is such a migration: no file of this repository can carry one, and what
// the census makes of one is still worth holding.
func censusOfHandBuiltMigrations(t *testing.T, databaseURL string, migrations []pgmigrate.Migration, options pgmigrate.CensusOptions) *pgmigrate.Report {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	report, err := pgmigrate.Census(ctx, conn, migrations, "schema_migrations", options)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	return report
}

// graphTuplesIndexState is the index's oid and whether Postgres has it as valid; the oid is 0
// when no index of that name exists.
func graphTuplesIndexState(t *testing.T, store *Store) (oid uint32, valid bool) {
	t.Helper()
	if err := store.Pool.QueryRow(context.Background(), `
		select coalesce(i.indexrelid, 0), coalesce(i.indisvalid, false)
		from (select to_regclass($1) as id) r
		left join pg_index i on i.indexrelid = r.id`, graphTuplesIndex).Scan(&oid, &valid); err != nil {
		t.Fatalf("read %s: %v", graphTuplesIndex, err)
	}
	return oid, valid
}

// A tuple log grown past the census's size limit still takes a new index, by the one route this
// repository has for it: a migration whose first line is exactly -- pgmigrate:non-transactional
// and whose body is one non-unique CREATE INDEX CONCURRENTLY IF NOT EXISTS (store/README.md, "A
// table past the census limit still takes a new index"). That route is why graph_tuples and
// graph_nodes are unpartitioned, so the test reads their relkind too: a partitioned table takes
// no CREATE INDEX CONCURRENTLY.
//
// The whole route is read against a real Postgres: graph_tuples is filled past a limit this run
// sets itself, the index as an ordinary migration is refused on the table's size, the same index
// under the marker passes the census and is applied for real, a second application of it does
// nothing, and a UNIQUE build under the same marker is refused like the ordinary migration.
//
// The run creates no event trigger, so it needs no superuser, which the test server's login is
// not. It reads pg_event_trigger at the end instead, since an enabled DDL event trigger is what
// would void this exemption.
func TestAnIndexIsStillPossibleOnceTheLogPassesTheCensusLimit(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, graphStoreVersion)
	if got := schemaVersion(t, store); got != graphStoreVersion {
		t.Fatalf("schema version is %d, want the graph store's own %d", got, graphStoreVersion)
	}
	databaseURL := store.Pool.Config().ConnString()

	if pgmigrate.CensusTableLimit != 1<<30 {
		t.Fatalf("pgmigrate.CensusTableLimit is %d bytes, want 1 GiB: the route this test reads exists for that number", pgmigrate.CensusTableLimit)
	}
	t.Logf("pgmigrate.CensusTableLimit is %d bytes (%d GiB); this run's own limit is %d bytes (%s)",
		pgmigrate.CensusTableLimit, pgmigrate.CensusTableLimit>>30, graphCensusLimit, mebibytes(graphCensusLimit))

	// Both tables come from the graph store's own migration, and each is a plain table: relkind
	// r, neither a partitioned relation (p) nor a partition of one.
	for _, table := range []string{"graph_nodes", "graph_tuples"} {
		var relkind string
		var partition bool
		if err := store.Pool.QueryRow(ctx, `
			select coalesce(c.relkind::text, ''), coalesce(c.relispartition, false)
			from (select to_regclass($1) as id) r
			left join pg_class c on c.oid = r.id`, table).Scan(&relkind, &partition); err != nil {
			t.Fatalf("read pg_class of %s: %v", table, err)
		}
		switch {
		case relkind == "":
			t.Fatalf("%s does not exist at version %d", table, graphStoreVersion)
		case relkind != "r":
			t.Fatalf("%s has pg_class.relkind %q, not a plain table (r)", table, relkind)
		case partition:
			t.Fatalf("%s is a partition of another table", table)
		}
		t.Logf("%s exists at version %d with pg_class.relkind %q and relispartition %v: a plain table", table, graphStoreVersion, relkind, partition)
	}

	// The fill is read back from pg_total_relation_size, the figure the census itself reads, so
	// what follows is measured rather than assumed.
	const batch = 10000
	var rows, filled int64
	for filled <= 2*graphCensusLimit {
		if rows >= 20*batch {
			t.Fatalf("graph_tuples is still %d bytes after %d rows, under the %d the test needs", filled, rows, 2*graphCensusLimit)
		}
		if _, err := store.Pool.Exec(ctx, `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
			select 'comment', 'comment-' || g, 'part_of', 'issue', 'CORE-' || g, now(),
				'{"kind":"user","id":"filler"}'::jsonb, g
			from generate_series($1::bigint, $2::bigint) as g`, rows+1, rows+batch); err != nil {
			t.Fatalf("fill graph_tuples: %v", err)
		}
		rows += batch
		if err := store.Pool.QueryRow(ctx, "select pg_total_relation_size('graph_tuples')").Scan(&filled); err != nil {
			t.Fatalf("read pg_total_relation_size of graph_tuples: %v", err)
		}
	}
	t.Logf("graph_tuples holds %d tuples and pg_total_relation_size reads %d bytes (%s), past this run's %s limit",
		rows, filled, mebibytes(filled), mebibytes(graphCensusLimit))

	options := pgmigrate.CensusOptions{TableLimit: graphCensusLimit}

	// The index as an ordinary migration - the marked migration's body with its first line taken
	// off - is refused on the table's size. That refusal is what the marked shape exists for.
	report, err := census(ctx, databaseURL, graphIndexMigration(graphIndexFile, graphIndexBody), options)
	if err != nil {
		t.Fatalf("census of the unmarked index migration: %v", err)
	}
	logCensusVerdict(t, "the index as an ordinary migration", report)
	entry, table := graphTuplesCensus(t, report)
	if table.Bytes < filled {
		t.Errorf("the census read graph_tuples as %d bytes, below the %d the test filled it to", table.Bytes, filled)
	}
	wantRefusal := fmt.Sprintf("graph_tuples is %s, above the %s a table a migration touches may be", mebibytes(table.Bytes), mebibytes(graphCensusLimit))
	if len(entry.Refusals) != 1 || entry.Refusals[0] != wantRefusal {
		t.Fatalf("refusals = %v, want one naming the table and its size: %q", entry.Refusals, wantRefusal)
	}

	// The same index under the marker passes: its scans hold off no ordinary write, so the size
	// limit does not apply to it, which the census says in ConcurrentIndex.
	report, err = census(ctx, databaseURL, graphIndexMigration(graphIndexFile, graphMarkedIndexSQL), options)
	if err != nil {
		t.Fatalf("census of the marked index migration: %v", err)
	}
	logCensusVerdict(t, "the same index under -- pgmigrate:non-transactional", report)
	entry, table = graphTuplesCensus(t, report)
	if report.Refused() {
		t.Fatalf("the marked build was refused: %v", refusals(report))
	}
	if !entry.ConcurrentIndex {
		t.Fatalf("the census did not read %s as a marked concurrent index build: %+v", entry.Migration.Name, entry)
	}
	if table.Bytes <= graphCensusLimit {
		t.Fatalf("the census read graph_tuples as %d bytes, within the %d limit, so nothing was proved about a table above it", table.Bytes, graphCensusLimit)
	}
	t.Logf("the census read the marked build as ConcurrentIndex over graph_tuples at %d bytes (%s)", table.Bytes, mebibytes(table.Bytes))

	// The build itself, on a connection the test holds, through the executor the store's runner
	// applies a marked migration with (applyNonTransactionalMigration). The test holds that
	// session so it can ask it what the runner left behind: a build outside a transaction leaves
	// the session idle with no transaction id assigned, where a runner that had wrapped the
	// statement would leave an open transaction holding one.
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect for the build: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	marked := pgmigrate.Migration{Version: graphIndexVersion, Name: graphIndexFile, SQL: graphMarkedIndexSQL, NonTransactional: true}
	if err := pgmigrate.ExecNonTransactional(ctx, conn, marked, nil); err != nil {
		t.Fatalf("build the index: %v", err)
	}
	var assigned *string
	if err := conn.QueryRow(ctx, "select txid_current_if_assigned()::text").Scan(&assigned); err != nil {
		t.Fatalf("read txid_current_if_assigned(): %v", err)
	}
	if assigned != nil {
		t.Fatalf("the session that ran the build holds transaction %s, so the build ran inside a transaction", *assigned)
	}
	if status := conn.PgConn().TxStatus(); status != 'I' {
		t.Fatalf("the session that ran the build is in transaction status %q, want idle", string(status))
	}
	t.Logf("the session that ran the build is idle and txid_current_if_assigned() is null: no transaction was held open around it")

	built, valid := graphTuplesIndexState(t, store)
	if built == 0 || !valid {
		t.Fatalf("%s after the build: oid %d, valid %v", graphTuplesIndex, built, valid)
	}

	// The same migration a second time, through the real runner, which runs the statement again
	// because no version records it yet: IF NOT EXISTS makes that second run do nothing, and the
	// index Postgres has is the one the first run built, still valid.
	set := migrationsThrough(t, graphStoreVersion)
	set[graphIndexMigrationID] = &fstest.MapFile{Data: []byte(graphMarkedIndexSQL)}
	if err := store.migrate(ctx, set); err != nil {
		t.Fatalf("apply the marked migration through the runner: %v", err)
	}
	if got := schemaVersion(t, store); got != graphIndexVersion {
		t.Fatalf("the runner recorded version %d, want %d", got, graphIndexVersion)
	}
	rebuilt, valid := graphTuplesIndexState(t, store)
	if rebuilt != built || !valid {
		t.Fatalf("the second application changed %s: oid %d -> %d, valid %v", graphTuplesIndex, built, rebuilt, valid)
	}
	t.Logf("the runner applied %s and recorded version %d; %s is oid %d and valid, the oid the first run built", graphIndexFile, graphIndexVersion, graphTuplesIndex, rebuilt)

	// A UNIQUE build validates the rows it scans, so it gets no size exception. No file can carry
	// one: the loader refuses it before any database is read.
	if _, err := census(ctx, databaseURL, graphIndexMigration(graphUniqueIndexFile, graphUniqueMarkedSQL), options); err == nil || !strings.Contains(err.Error(), "non-unique") {
		t.Fatalf("loading a unique build under the marker = %v, want a refusal naming the rule", err)
	} else {
		t.Logf("a unique build under the marker as a migration file: %v", err)
	}
	report = censusOfHandBuiltMigrations(t, databaseURL, []pgmigrate.Migration{{
		Version:          graphUniqueIndexNumber,
		Name:             graphUniqueIndexFile,
		SQL:              graphUniqueMarkedSQL,
		NonTransactional: true,
	}}, options)
	logCensusVerdict(t, "a unique build under the marker", report)
	entry, table = graphTuplesCensus(t, report)
	if entry.ConcurrentIndex {
		t.Errorf("the census gave a unique build the concurrent index exception: %+v", entry)
	}
	wantRefusal = fmt.Sprintf("graph_tuples is %s, above the %s a table a migration touches may be", mebibytes(table.Bytes), mebibytes(graphCensusLimit))
	if len(entry.Refusals) != 1 || entry.Refusals[0] != wantRefusal {
		t.Fatalf("refusals = %v, want one naming the table and its size: %q", entry.Refusals, wantRefusal)
	}

	// An enabled DDL event trigger voids the exemption every verdict above rests on, so the run
	// ends by reading which ones this database holds.
	var triggers string
	if err := store.Pool.QueryRow(ctx, `
		select coalesce(string_agg(evtname || ' on ' || evtevent, ', ' order by evtname), 'none')
		from pg_event_trigger where evtenabled <> 'D'`).Scan(&triggers); err != nil {
		t.Fatalf("read pg_event_trigger: %v", err)
	}
	t.Logf("enabled DDL event triggers in this database: %s", triggers)
}
