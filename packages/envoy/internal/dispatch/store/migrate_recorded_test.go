package store

import (
	"context"
	"net/url"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// appliedCheckStatement is the statement a runner sends once for each migration it judges on its
// way to applying it (pgmigrate.RecheckRecorded, through applyMigrationOnce or
// applyNonTransactionalMigration), each inside a locked transaction of its own. Counting it
// counts those transactions. Its text must match what pgmigrate sends byte for byte; a change
// there without a change here would read as zero judgments and pass this test for the wrong
// reason, so the first migrate below, which applies every migration, holds the text to the
// runner's own: it must count one judgment per migration applied.
const appliedCheckStatement = `select name, checksum, applied_checksum from "schema_migrations" where version = $1::bigint`

// countingStatements counts, on every connection of a pool, the queries whose text is one of its
// own.
type countingStatements struct {
	text  string
	count atomic.Int64
}

func (c *countingStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == c.text {
		c.count.Add(1)
	}
	return ctx
}

func (c *countingStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// openCountingStore opens a database of its own and a pool to it, built as openIsolatedStoreWithPool
// builds its own, whose connections count the queries of one text.
func openCountingStore(t *testing.T, text string) (*Store, *countingStatements) {
	t.Helper()
	ctx := context.Background()
	baseURL, err := url.Parse(testDatabaseURL(t))
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	adminURL := *baseURL
	adminURL.Path = "/postgres"
	config, err := pgxpool.ParseConfig(testdb.DatabaseOn(t, adminURL.String(), "dispatch_test"))
	if err != nil {
		t.Fatalf("parse the isolated database's URL: %v", err)
	}
	counter := &countingStatements{text: text}
	config.AfterConnect = scanTimestampsInUTC
	config.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open the isolated pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{Pool: NewPool(pool)}, counter
}

// A boot judges and applies what its prepare did not read as recorded, and nothing else. The
// prepare reads every row of schema_migrations and judges each against the whole set, in one
// transaction under the runner's advisory key (prepareVersionTable, pgmigrate.ReconcileRecorded);
// a migration that judgment read as recorded is applied, so the runner opens no transaction of its
// own for it. Before this, a boot of an already-migrated database opened one locked transaction
// per migration of the whole history - 340 of them on a Dispatch database of 2026-11-27, on every
// boot of every process, and 339 times over in the dispatch-memory suite, which starts a server
// per measured window.
//
// The three migrates below are the three cases: an empty database, where every migration is
// applied and judged; the same set again, where nothing is; and a set with one migration more,
// where that one alone is applied and judged, so the skip cannot be a boot that does nothing.
//
// Control: take the `recorded[migration.Version]` skip out of migrateThrough and the second
// migrate judges all five again (5, not 0), and the third all six (6, not 1), and this fails on
// the first of them.
func TestABootJudgesOnlyTheMigrationsItsPrepareDidNotReadAsRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, judged := openCountingStore(t, appliedCheckStatement)
	set := fstest.MapFS{
		"migrations/0001_a.up.sql": {Data: []byte("create table a (id int)")},
		"migrations/0002_b.up.sql": {Data: []byte("create table b (id int)")},
		"migrations/0003_c.up.sql": {Data: []byte("create table c (id int)")},
		"migrations/0004_d.up.sql": {Data: []byte("create table d (id int)")},
		"migrations/0005_e.up.sql": {Data: []byte("create table e (id int)")},
	}

	if err := store.migrate(ctx, set); err != nil {
		t.Fatalf("the first migrate: %v", err)
	}
	if got := judged.count.Load(); got != int64(len(set)) {
		t.Fatalf("the first migrate judged %d migrations; want one for each of the %d it applied: the statement this test counts is not the one the runner sends, so the count below proves nothing", got, len(set))
	}
	if got := recordedVersionCount(t, store); got != len(set) {
		t.Fatalf("schema_migrations holds %d rows after the first migrate; want %d", got, len(set))
	}

	judged.count.Store(0)
	if err := store.migrate(ctx, set); err != nil {
		t.Fatalf("the second migrate: %v", err)
	}
	if got := judged.count.Load(); got != 0 {
		t.Errorf("a boot of a database that records every migration judged %d of them; want none: the prepare read them all as recorded", got)
	}
	if got := recordedVersionCount(t, store); got != len(set) {
		t.Errorf("schema_migrations holds %d rows after the second migrate; want the same %d", got, len(set))
	}

	grown := fstest.MapFS{"migrations/0006_f.up.sql": {Data: []byte("create table f (id int)")}}
	for name, file := range set {
		grown[name] = file
	}
	judged.count.Store(0)
	if err := store.migrate(ctx, grown); err != nil {
		t.Fatalf("the migrate of the grown set: %v", err)
	}
	if got := judged.count.Load(); got != 1 {
		t.Errorf("the migrate of a set with one migration more judged %d of them; want the one it applies", got)
	}
	if got := recordedVersionCount(t, store); got != len(grown) {
		t.Errorf("schema_migrations holds %d rows after the grown set; want %d: the new migration was not applied", got, len(grown))
	}
	var made bool
	if err := store.Pool.QueryRow(ctx, "select to_regclass('f') is not null").Scan(&made); err != nil {
		t.Fatalf("look for what 0006 makes: %v", err)
	}
	if !made {
		t.Errorf("table f is not there: the boot skipped a migration its prepare did not read as recorded")
	}
}

// recordedVersionCount is how many versions schema_migrations records.
func recordedVersionCount(t *testing.T, store *Store) int {
	t.Helper()
	var rows int
	if err := store.Pool.QueryRow(context.Background(), "select count(*) from schema_migrations").Scan(&rows); err != nil {
		t.Fatalf("count the recorded versions: %v", err)
	}
	return rows
}
