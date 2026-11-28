package store

import (
	"context"
	neturl "net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// Every session Dispatch opens on its database runs with JIT off (sessionSettings says why): the
// shared pool, before and after the reset the migration runner gives a connection; each pool
// Pool.separate opens and the connections Listener and Dedicated hand out; a pool or a connection
// opened from Pool.Config or from a pooled connection's own configuration, as the server process
// registry's pool, the migration runner's lock watch and the tests' own pools are; and the
// connections of Census and CheckVersions. The database is given jit = on as its own default
// first, so a session that does not send the setting reads on whatever the server's default is,
// and the control, a plain connection to the same database, must read on: a row that reads off
// then reads what Dispatch sent.
func TestEverySessionDispatchOpensRunsWithJITOff(t *testing.T) {
	ctx := context.Background()
	store, url := migratedToOne(t)
	if _, err := store.Pool.Exec(ctx, `do $$ begin execute format('alter database %I set jit = on', current_database()); end $$`); err != nil {
		t.Fatalf("make jit = on the database's own default: %v", err)
	}
	type querier func(ctx context.Context, sql string, args ...any) pgx.Row
	read := func(t *testing.T, name, want string, query querier) {
		t.Helper()
		var jit string
		if err := query(ctx, "show jit").Scan(&jit); err != nil {
			t.Fatalf("%s: show jit: %v", name, err)
		}
		if jit != want {
			t.Errorf("%s reads jit = %q, want %s", name, jit, want)
		}
	}

	plain, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("control: connect without Dispatch's settings: %v", err)
	}
	t.Cleanup(func() { plain.Close(context.Background()) })
	read(t, "control: a plain connection to the database", "on", plain.QueryRow)
	if t.Failed() {
		t.FailNow()
	}

	off := func(name string, query querier) {
		t.Helper()
		read(t, name, "off", query)
	}
	separate := func(name string, open func() (*pgxpool.Pool, error)) {
		t.Helper()
		pool, err := open()
		if err != nil {
			t.Fatalf("open the %s pool: %v", name, err)
		}
		off("the "+name+" pool", pool.QueryRow)
	}

	off("the shared pool", store.Pool.QueryRow)
	conn, err := store.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a shared connection: %v", err)
	}
	if err := pgmigrate.ResetSession(ctx, conn.Conn()); err != nil {
		conn.Release()
		t.Fatalf("reset the session as the migration runner does: %v", err)
	}
	off("a shared connection after discard all", conn.QueryRow)
	watch, err := pgx.ConnectConfig(ctx, conn.Conn().Config())
	conn.Release()
	if err != nil {
		t.Fatalf("connect from a pooled connection's configuration: %v", err)
	}
	t.Cleanup(func() { watch.Close(context.Background()) })
	off("a connection opened from a pooled connection's configuration", watch.QueryRow)

	separate("document rooms", store.Pool.Rooms)
	separate("document room appends", store.Pool.RoomAppends)
	separate("document compaction", store.Pool.Compaction)
	separate("event horizon", store.Pool.Horizon)
	separate("health", store.Pool.healthPool)
	separate("loop leases", func() (*pgxpool.Pool, error) {
		leases, err := store.Pool.Leases()
		if err != nil {
			return nil, err
		}
		return leases.For("session settings test")
	})
	outbox, err := store.Pool.Outbox()
	if err != nil {
		t.Fatalf("open the events outbox pool: %v", err)
	}
	off("the events outbox pool", outbox.QueryRow)
	trees, err := store.Pool.TextTrees()
	if err != nil {
		t.Fatalf("open the display tree fill pool: %v", err)
	}
	off("the display tree fill pool", trees.QueryRow)

	listener, err := store.Pool.Listener(ctx)
	if err != nil {
		t.Fatalf("open a listener connection: %v", err)
	}
	t.Cleanup(func() { listener.Close(context.Background()) })
	off("a listener connection", listener.QueryRow)
	dedicated, err := store.Pool.Dedicated(ctx, "session settings test")
	if err != nil {
		t.Fatalf("open a dedicated connection: %v", err)
	}
	t.Cleanup(func() { dedicated.Close(context.Background()) })
	off("a dedicated connection", dedicated.QueryRow)

	copied, err := pgxpool.NewWithConfig(ctx, store.Pool.Config())
	if err != nil {
		t.Fatalf("open a pool from Pool.Config: %v", err)
	}
	t.Cleanup(copied.Close)
	off("a pool opened from Pool.Config", copied.QueryRow)

	// CheckVersions' own connection, read from the database's side with no hook in Dispatch: the
	// version table it reads is put behind a view, under the same name, whose filter refuses a
	// session whose jit is not off. CheckVersions finds the table by information_schema.columns,
	// which lists a view's columns too, and reads it in its read-only transaction, so the read
	// answers only on a session that runs with jit = off. The control: the plain connection's read
	// of the same view is refused. The table is put back before the census below reads it.
	swap := []string{
		`create function session_settings_test_jit_off() returns boolean language plpgsql volatile as $$
		begin
			if current_setting('jit') <> 'off' then
				raise exception 'this session runs with jit = %', current_setting('jit');
			end if;
			return true;
		end $$`,
		`alter table ` + versionTable + ` rename to session_settings_test_versions`,
		`create view ` + versionTable + ` as select * from session_settings_test_versions where session_settings_test_jit_off()`,
	}
	back := []string{
		`drop view ` + versionTable,
		`alter table session_settings_test_versions rename to ` + versionTable,
		`drop function session_settings_test_jit_off()`,
	}
	for _, statement := range swap {
		if _, err := store.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("put %s behind a view that refuses jit: %v", versionTable, err)
		}
	}
	var versions int
	if err := plain.QueryRow(ctx, `select count(*) from `+versionTable).Scan(&versions); err == nil || !strings.Contains(err.Error(), "this session runs with jit = on") {
		t.Errorf("control: a plain connection's read of the view answered %d rows and error %v, want the view's refusal of jit = on: else the row below tests nothing", versions, err)
	}
	if _, err := CheckVersions(ctx, url); err != nil {
		t.Errorf("CheckVersions' own connection does not run with jit = off: %v", err)
	}
	for _, statement := range back {
		if _, err := store.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("put %s back: %v", versionTable, err)
		}
	}

	// The census reads its own session's setting: a census counting the settings that are not off
	// refuses its migration unless the census's connection runs with jit = off.
	report, err := census(ctx, url, withCheck("select count(*) from pg_settings where name = 'jit' and setting <> 'off'"), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if got := refusals(report); len(got) != 0 {
		t.Errorf("the census's own connection does not run with jit = off: refusals %v", got)
	}

	// The setting replaces what the connection string asks for the same name.
	asking, err := neturl.Parse(url)
	if err != nil {
		t.Fatalf("parse the database's URL: %v", err)
	}
	query := asking.Query()
	query.Set("jit", "on")
	asking.RawQuery = query.Encode()
	asked, err := ConnConfig(asking.String())
	if err != nil {
		t.Fatalf("ConnConfig of a connection string asking jit = on: %v", err)
	}
	if got := asked.RuntimeParams["jit"]; got != "off" {
		t.Errorf("a connection string asking jit = on leaves %q in the startup parameters, want off", got)
	}
}
