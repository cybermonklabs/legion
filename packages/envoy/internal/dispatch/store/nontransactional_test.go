package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/logtest"
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

// migrationKeyRows selects the rows of pg_locks that are advisory locks on migrationLockKey in
// the current database, with the key as $1. Both helpers below count through it, so the shape of
// an advisory-key row - locktype, database, classid, objid, objsubid - is written once and the
// two counts cannot drift apart over what they call a lock on this key.
const migrationKeyRows = `from pg_locks where locktype = 'advisory'
	and database = (select oid from pg_database where datname = current_database())
	and classid = 0 and objid = $1::bigint and objsubid = 1`

// migrationKeyLocks counts the advisory locks held on migrationLockKey in this store's database.
// It reads pg_locks through store.Pool, which is where a leaked key would be: pg_locks is the
// whole server's, so a pooled connection sees the rows of every session, its own included, and
// the leak these tests are about is a pooled connection handed back while it still holds the key.
func migrationKeyLocks(t *testing.T, store *Store) int {
	t.Helper()
	var held int
	if err := store.Pool.QueryRow(context.Background(), "select count(*) "+migrationKeyRows, migrationLockKey).Scan(&held); err != nil {
		t.Fatalf("count the migration key's advisory locks: %v", err)
	}
	return held
}

// waitForQueuedMigrationKey gives want runners at most bound to queue for migrationLockKey and
// returns how many of them Postgres was seen holding ungranted. It fails nothing when none
// appear, because the two worlds this file tells apart queue differently: a runner that waits by
// polling pg_try_advisory_lock (lockMigrations) has nothing open between two tries and so shows
// in no catalog at all, while a runner blocked in pg_advisory_lock - the wait these tests refuse
// - is an ungranted advisory row here.
//
// The bound is what makes both worlds reach the release the same way: the blocking one returns
// as soon as its rows appear, the polling one when the bound runs out, and in both the key is
// released with every runner already queued for it, which is the order under test. The bound is
// far longer than the milliseconds two goroutines need to reach their first try, and nothing the
// polling world shows could confirm that arrival instead.
func waitForQueuedMigrationKey(t *testing.T, store *Store, want int, bound time.Duration) int {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(bound)
	for {
		var queued int
		if err := store.Pool.QueryRow(ctx, "select count(*) "+migrationKeyRows+" and not granted", migrationLockKey).Scan(&queued); err != nil {
			t.Fatalf("count the sessions queued for the migration key: %v", err)
		}
		if queued >= want || time.Now().After(deadline) {
			return queued
		}
		time.Sleep(10 * time.Millisecond)
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
	// This run was transactional throughout, so both of its pooled connections had to unlock
	// before they went back: nothing closed one out of the pool to cover for them.
	if held := migrationKeyLocks(t, store); held != 0 {
		t.Fatalf("migration lock leaked: %d advisory locks on the key after a transactional run", held)
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
	// Every connection that took the key gives it back, the failed attempt's included: the two
	// transactional sites unlock before they release, and the concurrent build's owned connection
	// closes.
	if held := migrationKeyLocks(t, store); held != 0 {
		t.Fatalf("migration lock leaked: %d advisory locks on the key after migrate returned", held)
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
	// Both sites of this run held the key on a pooled connection - the version table's and the
	// failing migration's - and nothing hijacked and closed one afterwards, so this is where a
	// connection handed back to the pool while it still holds the key shows up.
	if held := migrationKeyLocks(t, store); held != 0 {
		t.Errorf("migration lock leaked: %d advisory locks on the key after a failed migration", held)
	}
}

// The pod stopped mid-migration INFRA-1973 is about, at the layer that holds the key. The sleep
// is still in flight when the context is cancelled, so pgx's deadline watcher closes the
// connection over the migration statement's own failure, and BeginFunc's rollback then fails on
// a connection already closed; a cancel landing between two statements reaches the same place by
// the rollback's own failure instead (pgx's tx.go, die). Either way the deferred unlock finds a
// closed connection, and the session took the key with it, so this leaves no advisory lock
// behind and logs no ERROR saying the key was not released - the unlock is told the connection
// was closed before it and says so instead.
func TestACancelledMigrationEndsTheKeyWithItsSessionAndLogsNoError(t *testing.T) {
	store := openEmptyTestStore(t)
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	set := fstest.MapFS{"migrations/0001_slow.up.sql": {Data: []byte("create table slow (id integer); select pg_sleep(30);")}}
	migrated := make(chan error, 1)
	go func() { migrated <- store.migrate(ctx, set) }()
	waitForRunningStatement(t, store, "pg_sleep(30)")
	cancel()
	if err := <-migrated; !errors.Is(err, context.Canceled) {
		t.Fatalf("migrate = %v, want the cancellation", err)
	}
	// The key goes with the session, and the session goes when the server reads the end of the
	// socket pgx closed, which is not the same instant. Everything else here is already settled.
	deadline := time.Now().Add(pgmigrate.LockTimeout)
	for {
		held := migrationKeyLocks(t, store)
		if held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("migration lock leaked: %d advisory locks on the key after a cancelled migration", held)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var errorMessages []string
	closedConnection := false
	for _, record := range records.Records() {
		if record.Level >= slog.LevelError {
			errorMessages = append(errorMessages, record.Message)
		}
		if strings.Contains(record.Message, "connection was closed before the unlock") {
			closedConnection = true
		}
	}
	if len(errorMessages) > 0 {
		t.Errorf("a cancelled migration logged errors: %v", errorMessages)
	}
	if !closedConnection {
		t.Error("the unlock did not meet the closed connection a cancelled transaction leaves, so this run proves nothing about what it logs")
	}
	if schemaVersion(t, store) != 0 {
		t.Error("the cancelled migration recorded its version")
	}
}

// waitForRunningStatement waits for a backend of this database other than the caller's to be
// running a statement that holds substring, which is how a test reaches into a migration while
// it is still open. It bounds the wait like every other wait here.
func waitForRunningStatement(t *testing.T, store *Store, substring string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(pgmigrate.LockTimeout)
	for {
		var running int
		if err := store.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and pid <> pg_backend_pid()
			and state = 'active' and query like '%' || $1 || '%'`, substring).Scan(&running); err != nil {
			t.Fatalf("look for a backend running %s: %v", substring, err)
		}
		if running > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend ran %s", substring)
		}
		time.Sleep(10 * time.Millisecond)
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
		"dropping an INVALID index of this migration's own name",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "attempt=3") {
		t.Errorf("the build timed out on every attempt:\n%s", out)
	}
}

// leaveInvalidWindowOwner seeds two window rows under one owner key and fails a UNIQUE
// concurrent build of embeddings_window_owner over them, which leaves exactly the INVALID index
// a killed or timed-out build of 20261102233630 leaves behind, and returns that leftover's oid
// (as pgmigrate's leaveInvalidIndex does for its own table). Both window-owner tests start from
// this one fixture, so a correction to it reaches both.
func leaveInvalidWindowOwner(t *testing.T, store *Store) uint32 {
	t.Helper()
	ctx := context.Background()
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
	var valid bool
	if err := store.Pool.QueryRow(ctx, `select indexrelid, indisvalid from pg_index
		where indexrelid = 'embeddings_window_owner'::regclass`).Scan(&leftover, &valid); err != nil {
		t.Fatalf("read the leftover embeddings_window_owner from pg_index: %v", err)
	}
	if valid {
		t.Fatal("the fixture left a VALID embeddings_window_owner: the migration would have nothing to recover from")
	}
	return leftover
}

// checkRebuiltWindowOwner is what 20261102233630 must leave behind once it has recovered from
// leaveInvalidWindowOwner's leftover: a VALID partial index of that name on embeddings, and a
// different index from the leftover, since recovering means dropping that one and building
// again. Both window-owner tests check the rebuild through here.
func checkRebuiltWindowOwner(t *testing.T, store *Store, leftover uint32) {
	t.Helper()
	var rebuilt uint32
	var valid, partial, onEmbeddings bool
	if err := store.Pool.QueryRow(context.Background(), `select indexrelid, indisvalid, indpred is not null, indrelid = 'embeddings'::regclass
		from pg_index where indexrelid = 'embeddings_window_owner'::regclass`).Scan(&rebuilt, &valid, &partial, &onEmbeddings); err != nil {
		t.Fatalf("read embeddings_window_owner from pg_index: %v", err)
	}
	if !valid || !partial || !onEmbeddings {
		t.Errorf("rebuilt owner index: valid %v, partial %v, on embeddings %v", valid, partial, onEmbeddings)
	}
	if rebuilt == leftover {
		t.Errorf("embeddings_window_owner kept the leftover's oid %d: the INVALID index was not dropped and built again", leftover)
	}
}

// The production case INFRA-1973 reports, against the real migration set: a killed boot left an
// INVALID embeddings_window_owner behind, and 20261102233630 must drop it, build the index again
// and record its version instead of refusing on every later boot.
func TestWindowOwnerMigrationDropsAnInvalidLeftoverAndRebuilds(t *testing.T) {
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 20261102233625)
	leftover := leaveInvalidWindowOwner(t, store)
	migrateThrough(t, store, 20261102233630)
	checkRebuiltWindowOwner(t, store, leftover)
	if got := schemaVersion(t, store); got != 20261102233630 {
		t.Errorf("schema version = %d, want 20261102233630", got)
	}
}

// bootRaces is how many times TestTwoBootsAtOnceOverAnInvalidLeftoverNeverDeadlock runs the race,
// each on a database of its own, one race after another.
//
// One run proves little. The two boots take the key for one migration after another, and most
// runs end with them level at the concurrent index - both waiting for the same key, which is safe
// however that wait is taken. The run that deadlocks is the one where a boot takes the key for
// several migrations before the other is granted it once, so the second is still inside an
// earlier migration's transaction when the first starts its CONCURRENTLY statement, and which run
// that is, is up to the order Postgres wakes waiters in. Measured on PostgreSQL 18.6 with
// applyMigrationOnce's wait made blocking again: one run in three, one in eight and one in
// sixteen caught it, so twenty runs catch it in better than nine cases out of ten.
//
// The races run one at a time on purpose. Forty pools and twenty migration sets at once are a
// load of their own on a server the rest of this package shares, and the migration tests beside
// this one measure lock waits against a five-second bound.
const bootRaces = 20

// Two Dispatch processes booting at once over the INVALID leftover INFRA-1973 reports. One of
// them holds the migration advisory key while 20261102233630 drops that leftover and builds
// embeddings_window_owner again, both CONCURRENTLY; the other waits for the key.
//
// A runner that waited for the key inside Postgres - pg_advisory_xact_lock in a transaction, or a
// blocking pg_advisory_lock on a session - would keep the snapshot its blocked statement took,
// and a concurrent index build waits for every older snapshot, so the holder would wait for the
// waiter and the waiter for the holder: PostgreSQL 18.6 answers that cycle with "deadlock
// detected" (SQLSTATE 40P01) and one of the two boots fails on it. Polling for the key
// (lockMigrations) leaves nothing open between two tries, so both boots finish.
//
// The second runner is a Store of its own on the same database, not a goroutine on the first
// one's pool, because two booting processes are what this is about.
func TestTwoBootsAtOnceOverAnInvalidLeftoverNeverDeadlock(t *testing.T) {
	started := time.Now()
	t.Cleanup(func() { t.Logf("%d boot races took %s", bootRaces, time.Since(started).Round(time.Millisecond)) })
	for attempt := 1; attempt <= bootRaces; attempt++ {
		t.Run(fmt.Sprintf("boot-race-%d", attempt), func(t *testing.T) {
			ctx := context.Background()
			first := openEmptyTestStore(t)
			migrateThrough(t, first, 20261102233625)
			leftover := leaveInvalidWindowOwner(t, first)
			second, err := Open(ctx, first.Pool.Config().ConnString())
			if err != nil {
				t.Fatalf("open the second booting process's store: %v", err)
			}
			t.Cleanup(second.Pool.Close)
			booted := make(chan error, 2)
			release := make(chan struct{})
			for _, booting := range []*Store{first, second} {
				go func(booting *Store) {
					<-release
					booted <- booting.migrate(ctx, migrationFiles)
				}(booting)
			}
			close(release)
			for range 2 {
				if err := <-booted; err != nil {
					t.Errorf("one of two boots at once: %v", err)
				}
			}
			checkRebuiltWindowOwner(t, first, leftover)
			var records int
			if err := first.Pool.QueryRow(ctx, "select count(*) from schema_migrations where version = 20261102233630").Scan(&records); err != nil {
				t.Fatalf("count the owner migration's version records: %v", err)
			}
			if records != 1 {
				t.Errorf("schema_migrations holds %d records of 20261102233630, want 1", records)
			}
			// Two runners took the key at every migration of the set; both boots having
			// returned, neither session may still hold it.
			if held := migrationKeyLocks(t, first); held != 0 {
				t.Errorf("migration lock leaked: %d advisory locks on the key after two boots", held)
			}
		})
	}
}

// A boot already blocked on the migration key when the concurrent build starts. That order is
// the one a runner waiting for the key inside Postgres deadlocks on, and it is the one
// TestTwoBootsAtOnceOverAnInvalidLeftoverNeverDeadlock does not reach at this site: in a plain
// race the two boots arrive at the concurrent migration together, so the loser's wait begins
// after the winner's build has taken the snapshot it waits for older transactions out of, and
// the build never waits for the loser. With applyNonTransactionalMigration's wait made blocking
// again, forty of those races passed.
//
// A third session of the test's own holds the key first, so both runners are queued for it
// before either can build. When that session lets go, the winner starts CREATE INDEX
// CONCURRENTLY while the loser is still inside the statement it was waiting in - a statement
// whose snapshot is older than the build's phase, which the build must therefore see out, while
// the loser waits for the key the builder holds. PostgreSQL 18.6 ends that cycle with "deadlock
// detected" (SQLSTATE 40P01), which is no lock timeout and so fails the boot that gets it
// (RetryLockTimeouts). Polling for the key (lockMigrations) keeps no snapshot between two tries,
// so both runners finish.
//
// The two runners call applyMigration rather than migrate because migrate's first wait for the
// key is createVersionTable's: with a third session holding the key both boots would queue
// there instead, and the winner would then hold the key from that wait right through its build,
// so the loser could never be waiting at the site this test is about.
func TestABootAlreadyWaitingForTheKeyDoesNotDeadlockTheConcurrentBuild(t *testing.T) {
	ctx := context.Background()
	first, databaseURL := migratedToOne(t)
	set := fstest.MapFS{
		"migrations/0001_things.up.sql": censusBase["migrations/0001_things.up.sql"],
		"migrations/0002_things_kind.up.sql": {Data: []byte(
			"-- pgmigrate:non-transactional\ncreate index concurrently if not exists things_kind on things (kind);")},
	}
	migrations, err := pgmigrate.Load(set, "migrations")
	if err != nil {
		t.Fatalf("load the set the two runners apply: %v", err)
	}
	pending := migrations[1]
	if !pending.NonTransactional || pending.Version != 2 {
		t.Fatalf("the pending migration is %s at version %d, non-transactional %v; want the concurrent 0002",
			pending.Name, pending.Version, pending.NonTransactional)
	}
	// A session of the test's own, not one of the pools': it holds the key while both runners
	// queue for it, and no pooled connection may be left holding a session-level key.
	holder, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect the session that holds the key first: %v", err)
	}
	t.Cleanup(func() { holder.Close(context.WithoutCancel(ctx)) })
	if _, err := holder.Exec(ctx, "select pg_advisory_lock($1)", migrationLockKey); err != nil {
		t.Fatalf("take the migration key on the test's own session: %v", err)
	}
	second, err := Open(ctx, first.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the second booting process's store: %v", err)
	}
	t.Cleanup(second.Pool.Close)
	applied := make(chan error, 2)
	for _, booting := range []*Store{first, second} {
		go func(booting *Store) { applied <- booting.applyMigration(ctx, pending) }(booting)
	}
	queued := waitForQueuedMigrationKey(t, first, 2, 2*time.Second)
	t.Logf("%d of the 2 runners were waiting for the key inside Postgres when it was released", queued)
	if _, err := holder.Exec(ctx, "select pg_advisory_unlock($1)", migrationLockKey); err != nil {
		t.Fatalf("release the key the test's own session holds: %v", err)
	}
	holder.Close(ctx)
	for range 2 {
		if err := <-applied; err != nil {
			t.Errorf("a runner queued for the key before the build began: %v", err)
		}
	}
	var valid bool
	if err := first.Pool.QueryRow(ctx, `select indisvalid from pg_index
		where indexrelid = 'things_kind'::regclass`).Scan(&valid); err != nil {
		t.Fatalf("read things_kind from pg_index: %v", err)
	}
	if !valid {
		t.Error("things_kind is INVALID: the concurrent build did not finish")
	}
	var records int
	if err := first.Pool.QueryRow(ctx, "select count(*) from schema_migrations where version = $1::bigint", pending.Version).Scan(&records); err != nil {
		t.Fatalf("count the migration's version records: %v", err)
	}
	if records != 1 {
		t.Errorf("schema_migrations holds %d records of version %d, want 1", records, pending.Version)
	}
}
