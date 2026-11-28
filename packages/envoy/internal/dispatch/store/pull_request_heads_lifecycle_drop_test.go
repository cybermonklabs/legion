// pull_request_heads_lifecycle_drop_test.go holds the record of each write that opens or closes a
// row of pull_request_heads (pull_request_heads_lifecycle) to its one reader: the former heads
// install reads it, and the migration after the install drops it, its triggers and its function,
// so no write of pull_request_heads appends to a table nothing reads.
package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// lifecycleRecordObjects is each object of the record that a database holds: the table, its index
// and its identity's sequence (pg_class), the triggers on pull_request_heads (pg_trigger) and their
// function (pg_proc), each as kind and name.
func lifecycleRecordObjects(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.Pool.Query(context.Background(), `
		with here as (select oid from pg_namespace where nspname = current_schema())
		select 'relation ' || relname from pg_class, here
		where relname like 'pull\_request\_heads\_lifecycle%' and relnamespace = here.oid
		union all
		select 'trigger ' || fired.tgname from pg_trigger fired
		where fired.tgrelid = 'pull_request_heads'::regclass and fired.tgname like 'pull\_request\_heads\_lifecycle%'
		union all
		select 'function ' || proname from pg_proc, here
		where proname like 'pull\_request\_heads\_lifecycle%' and pronamespace = here.oid
		order by 1`)
	if err != nil {
		t.Fatalf("read the record's objects: %v", err)
	}
	objects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the record's objects: %v", err)
	}
	return objects
}

// tableRowCounts is how many rows each table of the schema holds.
func tableRowCounts(t *testing.T, store *Store) map[string]int {
	t.Helper()
	ctx := context.Background()
	rows, err := store.Pool.Query(ctx, `
		select table_name::text from information_schema.tables
		where table_schema = current_schema() and table_type = 'BASE TABLE' order by 1`)
	if err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	if len(tables) == 0 {
		t.Fatal("the schema lists no table, so no count can show a table that grew")
	}
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := store.Pool.QueryRow(ctx, `select count(*) from `+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil {
			t.Fatalf("count the rows of %s: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}

// TestTheLifecycleRecordIsGoneOnceTheInstallHasReadIt: the former heads install reads the record
// of each write that opens or closes a row of pull_request_heads once, to find where each pull
// request's last closed period began, and nothing reads it after. Through the install the record
// is there, its table, its index, its sequence, its two triggers and its function; once every
// migration has applied, none of them is. A write of pull_request_heads then still works: CORE-1's
// #70 closes and a new open row is written, and the only tables that hold more rows after them are
// pull_request_heads itself, the former heads record's two (moves, pending) and the marks
// (check_states_owed), which the triggers the record does not own append to.
func TestTheLifecycleRecordIsGoneOnceTheInstallHasReadIt(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	installFixture(t, store)
	migrateThrough(t, store, formerHeadsInstall(t))
	want := []string{
		"function pull_request_heads_lifecycle_keep",
		"relation pull_request_heads_lifecycle",
		"relation pull_request_heads_lifecycle_number",
		"relation pull_request_heads_lifecycle_seq_seq",
		"trigger pull_request_heads_lifecycle_insert",
		"trigger pull_request_heads_lifecycle_update",
	}
	if through := lifecycleRecordObjects(t, store); !slices.Equal(through, want) {
		t.Fatalf("through the install the database holds the record's objects %v, want %v: this test reads the catalog for nothing the record is", through, want)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if left := lifecycleRecordObjects(t, store); len(left) != 0 {
		t.Errorf("once every migration has applied the database still holds %v: the record the install read lives on, and its triggers append a row on every write that opens or closes a row of pull_request_heads", left)
	}

	before := tableRowCounts(t, store)
	for _, write := range []struct{ what, sql string }{
		{"close #70", `update pull_request_heads set state = 'closed' where repo = 'acme/widgets' and number = 70`},
		{"write #72 open", `insert into pull_request_heads (repo, number, head_sha, state, issue_key) values ('acme/widgets', 72, '` + installH2 + `', 'open', 'CORE-1')`},
	} {
		if _, err := store.Pool.Exec(ctx, write.sql); err != nil {
			t.Fatalf("%s once every migration has applied: %v", write.what, err)
		}
	}
	after := tableRowCounts(t, store)
	var grew []string
	for table, count := range after {
		if count > before[table] {
			grew = append(grew, table)
		}
	}
	slices.Sort(grew)
	if want := []string{"check_states_owed", "pull_request_former_heads_moves", "pull_request_former_heads_pending", "pull_request_heads"}; !slices.Equal(grew, want) {
		t.Errorf("the two writes of pull_request_heads added rows to %v, want %v alone: a write appends to a table beside the former heads record and the marks", grew, want)
	}
}

// TestTheLifecycleRecordDropGivesWayToAReaderThatThenWritesTheHeads: the drop of the record's
// triggers takes ACCESS EXCLUSIVE on pull_request_heads. A transaction that read pull_request_heads
// before the drop asked for it, and then writes a row of it, runs that write's triggers, the
// record's among them while it is there, which append to the record's table. The drop asks for
// pull_request_heads before the record's table, so it holds nothing that write waits for: the write
// and its commit go through while the drop waits, and the drop then applies. Asked the other way,
// the drop would hold the record's table while it waited for the reader, and the reader's trigger
// would wait for the drop: a cycle Postgres breaks by failing one of them.
func TestTheLifecycleRecordDropGivesWayToAReaderThatThenWritesTheHeads(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	installFixture(t, store)
	migrateThrough(t, store, formerHeadsInstall(t))
	conn, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect the reader: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	reader, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the reader: %v", err)
	}
	defer reader.Rollback(context.Background())
	var open int
	if err := reader.QueryRow(ctx, `select count(*) from pull_request_heads where state = 'open'`).Scan(&open); err != nil || open != 2 {
		t.Fatalf("the reader reads the open rows: %d (%v), want 2", open, err)
	}

	migrated := make(chan error, 1)
	go func() { migrated <- store.Migrate(ctx) }()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var waits bool
		if err := store.Pool.QueryRow(ctx, `
			select exists (select 1 from pg_locks where relation = 'pull_request_heads'::regclass
				and mode = 'AccessExclusiveLock' and not granted)`).Scan(&waits); err != nil {
			t.Fatalf("read the lock waits: %v", err)
		}
		if waits {
			break
		}
		select {
		case err := <-migrated:
			t.Fatalf("the migrations ended (%v) while the reader held pull_request_heads, before any asked for it in ACCESS EXCLUSIVE", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no migration asked for pull_request_heads in ACCESS EXCLUSIVE while the reader held it")
		}
	}

	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	asked := time.Now()
	if _, err := reader.Exec(bounded, `update pull_request_heads set state = 'closed' where repo = 'acme/widgets' and number = 70`); err != nil {
		t.Fatalf("the reader's write of #70 while the drop waited for it: %v; want it to go through", err)
	}
	if err := reader.Commit(bounded); err != nil {
		t.Fatalf("commit the reader: %v", err)
	}
	t.Logf("the reader wrote and committed %s after it asked", time.Since(asked).Round(time.Millisecond))
	if err := <-migrated; err != nil {
		t.Fatalf("the migrations after the reader committed: %v", err)
	}
	if left := lifecycleRecordObjects(t, store); len(left) != 0 {
		t.Fatalf("once every migration has applied the database still holds %v, want none of the record", left)
	}
	var state string
	if err := store.Pool.QueryRow(ctx, `select state from pull_request_heads where repo = 'acme/widgets' and number = 70`).Scan(&state); err != nil || !strings.EqualFold(state, "closed") {
		t.Fatalf("#70 is %q (%v) after the reader's write, want closed", state, err)
	}
}
