package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// outboxWaitsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const outboxWaitsVersion = 20261127174946

// 20261127174946 adds the seven columns of an outbox row's wait (record.OutboxWait) at their
// defaults, which say the row waits for nothing, on every row the outbox holds, and the index
// outbox_wait_for of the rows that wait for something. An earlier release, which names its columns
// in every read and write, runs over the migrated database: its insert lands at the defaults, and
// its retry of a row leaves the wait this release wrote on it as it stands, so the migration needs
// no way back. The negative control is the store before the migration, whose outbox carries no
// such index.
func TestMigration20261127174946AddsTheWaitColumns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, outboxWaitsVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	const index = `select indexdef from pg_indexes where tablename = 'outbox' and indexname = 'outbox_wait_for'`
	if got := rowsAsText(t, store, index); len(got) != 0 {
		t.Fatalf("outbox_wait_for before the migration = %v, want none", got)
	}
	// The earlier release's insert and its retry name these columns and no other (record's
	// Enqueue and RetryOutbox before this release).
	const releaseBeforesInsert = `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token, lease_until)
		values ('notice', 'LEGION-208', '{}', 2, now(), 'the listener refused it', '', null)`
	exec(releaseBeforesInsert)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the outbox's wait columns: %v", err)
	}
	const waits = `select row(attempts, wait_since at time zone 'UTC', wait_logged, wait_step, wait_ready at time zone 'UTC',
		wait_launch, wait_for, wait_woken)::text from outbox order by id`
	none := "(2,,f,0,,0,\"\",f)"
	if got, want := rowsAsText(t, store, waits), []string{none}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the outbox after the migration (attempts and the seven wait columns) = %v, want %v: the row it found, waiting for nothing", got, want)
	}
	exec(releaseBeforesInsert)
	if got, want := rowsAsText(t, store, waits), []string{none, none}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the outbox after the earlier release's insert = %v, want %v: its row at the defaults", got, want)
	}
	exec(`update outbox set wait_since = '2026-11-27T09:00:00Z', wait_logged = true, wait_step = 4,
		wait_ready = '2026-11-27T08:59:00Z', wait_launch = 3, wait_for = 'ready:legion-legion-legion-208-architect', wait_woken = true
		where id = (select min(id) from outbox)`)
	exec(`update outbox set attempts = attempts + 1, next_at = now() + interval '1 minute', last_error = 'refused again',
		lease_token = '', lease_until = null where id = (select min(id) from outbox)`)
	waiting := `(3,"2026-11-27 09:00:00",t,4,"2026-11-27 08:59:00",3,ready:legion-legion-legion-208-architect,t)`
	if got, want := rowsAsText(t, store, waits), []string{waiting, none}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the outbox after the earlier release's retry of a waiting row = %v, want %v: the wait this release wrote, as it stood", got, want)
	}
	got := rowsAsText(t, store, index)
	if len(got) != 1 || !strings.Contains(got[0], "USING btree (wait_for) WHERE (wait_for <> ''::text)") {
		t.Fatalf("outbox_wait_for after the migration = %v, want one btree index on (wait_for) of the rows whose wait_for is not empty", got)
	}
}
