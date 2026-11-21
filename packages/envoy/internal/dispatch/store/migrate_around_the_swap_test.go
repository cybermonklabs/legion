package store

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// laterMigrations is the embedded migrations with later migrations added, each a file name under
// migrations/ and its text: the release after the partitioning, as a later author writes it.
type laterMigrations struct {
	later fstest.MapFS
}

func (m laterMigrations) Open(name string) (fs.File, error) {
	if _, held := m.later[name]; held {
		return m.later.Open(name)
	}
	return migrationFiles.Open(name)
}

func (m laterMigrations) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(migrationFiles, name)
	if err != nil || name != "migrations" {
		return entries, err
	}
	added, err := fs.ReadDir(m.later, "migrations")
	if err != nil {
		return nil, err
	}
	return append(entries, added...), nil
}

// withLaterMigration is the embedded migrations and one later migration of the text given.
func withLaterMigration(text string) laterMigrations {
	return laterMigrations{later: fstest.MapFS{
		"migrations/29991231000000_embeddings_later.up.sql": {Data: []byte(text)},
	}}
}

// A migration after the partitioning runs, on the template's order (MigrateAroundTheSwap), on
// embeddings partitioned by project, as it runs on a deployment that swapped before it: an index
// a later migration builds on embeddings is the partitioned table's index, built on every
// partition, and still there after the swap. The control is Migrate's own order, every migration
// and then the fill, which a fresh database takes: the same migration builds its index on the
// table the swap then drops, and the index is gone, which a template made that way would hand
// every test without a word.
func TestTheTemplatesOrderRunsALaterMigrationOnThePartitionedTable(t *testing.T) {
	ctx := context.Background()
	later := withLaterMigration(`create index embeddings_later on embeddings (kind, project_key)`)
	readIndex := func(database *Store) (kind string, partitions int) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `
			select coalesce((select relkind::text from pg_class where oid = to_regclass('embeddings_later')), ''),
			       (select count(*) from pg_inherits where inhparent = to_regclass('embeddings_later'))`).
			Scan(&kind, &partitions); err != nil {
			t.Fatalf("read embeddings_later: %v", err)
		}
		return kind, partitions
	}

	around := openEmptyTestStore(t)
	if err := around.migrateAroundTheSwap(ctx, later); err != nil {
		t.Fatalf("migrate around the swap with a later migration: %v", err)
	}
	requirePartitioned(t, around, true)
	if kind, partitions := readIndex(around); kind != "I" || partitions == 0 {
		t.Errorf("after the template's order, embeddings_later is relkind %q over %d partitions, want the partitioned table's index (I) on every partition", kind, partitions)
	}

	all := openEmptyTestStore(t)
	if err := all.migrate(ctx, later); err != nil {
		t.Fatalf("migrate with a later migration: %v", err)
	}
	if _, err := all.FillEmbeddingProjectKeys(ctx, ""); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if kind, _ := readIndex(all); kind != "" {
		t.Errorf("after every migration and then the swap, embeddings_later is relkind %q, want it gone with the table the swap dropped: the control cannot tell the two orders apart", kind)
	}
}

// A later concurrent index build on embeddings, the shape store/README.md teaches for a table past
// the census limit, fails on the template's order as it fails a deployment's boot: Postgres builds
// no index concurrently on a partitioned table (SQLSTATE 0A000). On Migrate's own order it builds,
// on the table the swap then drops, which is the order a template must not take.
func TestALaterConcurrentIndexOnEmbeddingsFailsOnTheTemplatesOrder(t *testing.T) {
	ctx := context.Background()
	later := withLaterMigration("-- pgmigrate:non-transactional\ncreate index concurrently if not exists embeddings_later on embeddings (kind, project_key)")
	around := openEmptyTestStore(t)
	err := around.migrateAroundTheSwap(ctx, later)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "0A000" {
		t.Errorf("a later concurrent index on embeddings, on the template's order = %v, want SQLSTATE 0A000, as a deployment's boot fails", err)
	}
	all := openEmptyTestStore(t)
	if err := all.migrate(ctx, later); err != nil {
		t.Errorf("the same migration on Migrate's own order = %v, want it built on the unpartitioned table: the control cannot tell the two orders apart", err)
	}
}

// The census refuses a marked concurrent index build on embeddings once embeddings is partitioned,
// rather than exempting it from the size limit as it exempts one on a plain table: the build
// would fail the boot (0A000), so the census names the path that builds one. The control is the
// same migration over the unpartitioned table, which the census admits as a concurrent index.
func TestTheCensusRefusesAConcurrentIndexOnThePartitionedEmbeddings(t *testing.T) {
	ctx := context.Background()
	later := fstest.MapFS{"migrations/29991231000000_embeddings_later.up.sql": {
		Data: []byte("-- pgmigrate:non-transactional\ncreate index concurrently if not exists embeddings_later on embeddings (kind, project_key)"),
	}}
	options := pgmigrate.CensusOptions{TableLimit: pgmigrate.CensusTableLimit}
	judge := func(database *Store) pgmigrate.MigrationCensus {
		t.Helper()
		report, err := census(ctx, database.Pool.Config().ConnString(), later, options)
		if err != nil {
			t.Fatalf("census of the later concurrent index: %v", err)
		}
		if len(report.Pending) != 1 {
			t.Fatalf("the census read %d pending migrations, want the one handed it", len(report.Pending))
		}
		return report.Pending[0]
	}

	partitioned := openEmptyTestStore(t)
	if err := partitioned.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := partitioned.FillEmbeddingProjectKeys(ctx, ""); err != nil {
		t.Fatal(err)
	}
	requirePartitioned(t, partitioned, true)
	entry := judge(partitioned)
	if entry.ConcurrentIndex || len(entry.Refusals) != 1 || !strings.Contains(entry.Refusals[0], "which is partitioned") || !strings.Contains(entry.Refusals[0], "0A000") {
		t.Errorf("the census of a concurrent index on the partitioned embeddings: concurrent index %v, refusals %q; want one refusal that names the partitioned table and 0A000", entry.ConcurrentIndex, entry.Refusals)
	}

	plain := openEmptyTestStore(t)
	if err := plain.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if entry := judge(plain); !entry.ConcurrentIndex || len(entry.Refusals) != 0 {
		t.Errorf("the census of the same index on the unpartitioned embeddings: concurrent index %v, refusals %q; want it admitted as a concurrent index, so the refusal above is the partitioning's", entry.ConcurrentIndex, entry.Refusals)
	}
}
