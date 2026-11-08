package store

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
)

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
