package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// waitForInvalidIndex waits for the INVALID catalog entry a concurrent build writes before it
// waits for the transactions it must see out, and returns that index's oid. A build's ungranted
// wait is on a virtualxid rather than on the relation, so waitForQueuedLock cannot see it.
func waitForInvalidIndex(t *testing.T, store *Store, name string) uint32 {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(pgmigrate.LockTimeout)
	for {
		var oid uint32
		var valid bool
		err := store.Pool.QueryRow(ctx, `select i.indexrelid, i.indisvalid from pg_index i
			join pg_class c on c.oid = i.indexrelid where c.relname = $1`, name).Scan(&oid, &valid)
		switch {
		case err == nil && !valid:
			return oid
		case err == nil || errors.Is(err, pgx.ErrNoRows):
			// Not built yet, or already valid on a database this test cannot have reached.
		default:
			t.Fatalf("read %s from pg_index: %v", name, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no INVALID %s appeared while the concurrent build waited", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMigrateConcurrentIndexHoldsMigrationLockAndRetriesAnUnrecordedBuild(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	set := fstest.MapFS{
		"migrations/0001_things.up.sql": {Data: []byte("create table things (kind text)")},
		"migrations/0002_index.up.sql":  {Data: []byte("-- pgmigrate:non-transactional\ncreate index concurrently if not exists things_kind on things (kind);")},
	}
	if err := store.migrate(ctx, fstest.MapFS{"migrations/0001_things.up.sql": set["migrations/0001_things.up.sql"]}); err != nil {
		t.Fatal(err)
	}
	// The event trigger runs in the build's own session and checks the lock while it builds.
	// Recording the version fails once, after the concurrent statement has already committed.
	if _, err := store.Pool.Exec(ctx, `
		create function require_migration_lock() returns event_trigger language plpgsql as $$
		begin
			if not exists (select from pg_locks where pid = pg_backend_pid() and locktype = 'advisory'
				and classid = 0 and objid = 8150001 and objsubid = 1 and granted) then
				raise exception 'concurrent build lost the migration lock';
			end if;
		end $$;
		create event trigger require_migration_lock on ddl_command_start when tag in ('CREATE INDEX') execute function require_migration_lock();
		create function refuse_index_version() returns trigger language plpgsql as $$
		begin
			if current_setting('lock_timeout') <> '5s' then
				raise exception 'version record lost its lock_timeout';
			end if;
			raise exception 'version record deliberately failed';
		end $$;
		create trigger refuse_index_version before insert on schema_migrations for each row execute function refuse_index_version();
	`); err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx, set); err == nil || !strings.Contains(err.Error(), "version record deliberately failed") {
		t.Fatalf("migrate = %v, want failure after the build", err)
	}
	var oid uint32
	if err := store.Pool.QueryRow(ctx, "select 'things_kind'::regclass::oid").Scan(&oid); err != nil {
		t.Fatal(err)
	}
	if got := schemaVersion(t, store); got != 1 {
		t.Fatalf("failed version record advanced to %d", got)
	}
	if _, err := store.Pool.Exec(ctx, "drop trigger refuse_index_version on schema_migrations"); err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx, set); err != nil {
		t.Fatalf("retry after version recording failed: %v", err)
	}
	var retriedOID uint32
	if err := store.Pool.QueryRow(ctx, "select 'things_kind'::regclass::oid").Scan(&retriedOID); err != nil {
		t.Fatal(err)
	}
	if retriedOID != oid || schemaVersion(t, store) != 2 {
		t.Fatalf("retry rebuilt the index or failed to record version: oid %d -> %d, version %d", oid, retriedOID, schemaVersion(t, store))
	}
	// Closing the owned connection must release even a failed attempt's session-level lock.
	var locked bool
	if err := store.Pool.QueryRow(ctx, "select pg_try_advisory_xact_lock(8150001)").Scan(&locked); err != nil || !locked {
		t.Fatalf("migration lock leaked: %v, %v", locked, err)
	}
}

func TestMigrateTransactionalFailureStillRollsBackTheWholeMigration(t *testing.T) {
	store := openEmptyTestStore(t)
	set := fstest.MapFS{"migrations/0001_fail.up.sql": {Data: []byte("create table rolled_back (id integer); select 1 / 0;")}}
	if err := store.migrate(context.Background(), set); err == nil {
		t.Fatal("failing migration succeeded")
	}
	var exists bool
	if err := store.Pool.QueryRow(context.Background(), "select to_regclass('rolled_back') is not null").Scan(&exists); err != nil || exists {
		t.Fatalf("transactional DDL survived failure: %v, %v", exists, err)
	}
	if schemaVersion(t, store) != 0 {
		t.Fatal("failed transactional migration recorded its version")
	}
}

func TestWindowOwnerMigrationKeepsAnExistingValidPartialIndex(t *testing.T) {
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 20261102233625)
	ctx := context.Background()
	// This is the index already present where the original form of 20261102233625 ran.
	if _, err := store.Pool.Exec(ctx, `create index embeddings_window_owner on embeddings
		(kind, split_part(id, '#', 1)) where kind in ('document_block', 'comment_block', 'message_block', 'ask_part')`); err != nil {
		t.Fatal(err)
	}
	var before uint32
	if err := store.Pool.QueryRow(ctx, "select 'embeddings_window_owner'::regclass::oid").Scan(&before); err != nil {
		t.Fatal(err)
	}
	migrateThrough(t, store, 20261102233630)
	var after uint32
	var valid, partial bool
	if err := store.Pool.QueryRow(ctx, `select indexrelid, indisvalid, indpred is not null
		from pg_index where indexrelid = 'embeddings_window_owner'::regclass`).Scan(&after, &valid, &partial); err != nil {
		t.Fatal(err)
	}
	if after != before || !valid || !partial {
		t.Fatalf("existing index changed: oid %d -> %d, valid %v, partial %v", before, after, valid, partial)
	}
}

func TestWindowOwnerMigrationBuildsAValidPartialIndex(t *testing.T) {
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 20261102233630)
	var valid, partial bool
	if err := store.Pool.QueryRow(context.Background(), `select indisvalid, indpred is not null
		from pg_index where indexrelid = 'embeddings_window_owner'::regclass`).Scan(&valid, &partial); err != nil {
		t.Fatal(err)
	}
	if !valid || !partial {
		t.Fatalf("new owner index must be valid and partial over window kinds: valid %v, partial %v", valid, partial)
	}
}

// INFRA-1973's own sequence at the layer that records a version: a concurrent build cancelled at
// the runner's lock_timeout leaves an INVALID index of the migration's own name, and the retry
// drops that leftover, builds the index again and records the migration's version.
func TestMigrateRecordsTheVersionAfterATimedOutConcurrentBuild(t *testing.T) {
	ctx := context.Background()
	store, _ := migratedToOne(t)
	logs := captureLogs(t)
	set := fstest.MapFS{
		"migrations/0001_things.up.sql": censusBase["migrations/0001_things.up.sql"],
		"migrations/0002_things_kind.up.sql": {Data: []byte(
			"-- pgmigrate:non-transactional\ncreate index concurrently if not exists things_kind on things (kind);")},
	}
	// ROW EXCLUSIVE does not conflict with the build's SHARE UPDATE EXCLUSIVE, but the open
	// write holds a snapshot the build must see out, so the first attempt is cancelled at its
	// lock_timeout with its INVALID index already in the catalog.
	held, _ := holdWrite(t, store)
	migrated := make(chan error, 1)
	go func() { migrated <- store.migrate(ctx, set) }()
	leftover := waitForInvalidIndex(t, store, "things_kind")
	// Past the first attempt's lock_timeout and into the pause, then let the writer go.
	time.Sleep(pgmigrate.LockTimeout + pgmigrate.LockTimeoutRetryPause/2)
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-migrated; err != nil {
		t.Fatalf("migrate after the timed-out concurrent build: %v", err)
	}
	if got := schemaVersion(t, store); got != 2 {
		t.Errorf("schema version = %d, want 2: the migration completed but its version went unrecorded", got)
	}
	var rebuilt uint32
	var valid bool
	if err := store.Pool.QueryRow(ctx, `select indexrelid, indisvalid from pg_index
		where indexrelid = 'things_kind'::regclass`).Scan(&rebuilt, &valid); err != nil {
		t.Fatalf("read things_kind from pg_index: %v", err)
	}
	if !valid {
		t.Error("things_kind is still INVALID after the retry")
	}
	if rebuilt == leftover {
		t.Errorf("things_kind kept the leftover's oid %d: the INVALID index was not dropped and built again", leftover)
	}
	out := logs.String()
	for _, want := range []string{
		"migration=0002_things_kind.up.sql", "attempt=1", "the runner tries it again",
		"dropping the INVALID index a failed concurrent build left",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "attempt=3") {
		t.Errorf("the build timed out on every attempt:\n%s", out)
	}
}

// The production case INFRA-1973 reports, against the real migration set: a killed boot left an
// INVALID embeddings_window_owner behind, and 20261102233630 must drop it, build the index again
// and record its version instead of refusing on every later boot.
func TestWindowOwnerMigrationDropsAnInvalidLeftoverAndRebuilds(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 20261102233625)
	// Two window rows under one owner key, which a UNIQUE build of the owner index cannot
	// validate: the build fails over them and leaves exactly the INVALID index a killed or
	// timed-out build of the real migration leaves.
	if _, err := store.Pool.Exec(ctx, `insert into embeddings (kind, id, text_snapshot, content_hash) values
		('document_block', 'owner#0', 'first block', 'hash-first'),
		('document_block', 'owner#1', 'second block', 'hash-second')`); err != nil {
		t.Fatalf("seed two window rows of one owner: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `create unique index concurrently embeddings_window_owner on embeddings
		(kind, split_part(id, '#', 1)) where kind in ('document_block', 'comment_block', 'message_block', 'ask_part')`); err == nil {
		t.Fatal("the unique build over two rows of one owner succeeded; it must fail and leave an INVALID index")
	}
	var leftover uint32
	var validBefore bool
	if err := store.Pool.QueryRow(ctx, `select indexrelid, indisvalid from pg_index
		where indexrelid = 'embeddings_window_owner'::regclass`).Scan(&leftover, &validBefore); err != nil {
		t.Fatalf("read the leftover embeddings_window_owner from pg_index: %v", err)
	}
	if validBefore {
		t.Fatal("the fixture left a VALID embeddings_window_owner: the migration would have nothing to recover from")
	}
	migrateThrough(t, store, 20261102233630)
	var rebuilt uint32
	var valid, partial, onEmbeddings bool
	if err := store.Pool.QueryRow(ctx, `select indexrelid, indisvalid, indpred is not null, indrelid = 'embeddings'::regclass
		from pg_index where indexrelid = 'embeddings_window_owner'::regclass`).Scan(&rebuilt, &valid, &partial, &onEmbeddings); err != nil {
		t.Fatalf("read embeddings_window_owner from pg_index: %v", err)
	}
	if !valid || !partial || !onEmbeddings {
		t.Errorf("rebuilt owner index: valid %v, partial %v, on embeddings %v", valid, partial, onEmbeddings)
	}
	if rebuilt == leftover {
		t.Errorf("embeddings_window_owner kept the leftover's oid %d: the INVALID index was not dropped and built again", leftover)
	}
	if got := schemaVersion(t, store); got != 20261102233630 {
		t.Errorf("schema version = %d, want 20261102233630", got)
	}
}
