// A test binary of this module takes a database of its own per test, and `create database …
// strategy file_copy` forces a checkpoint before the copy and one after it, with `drop database …
// with (force)` forcing one more. One server runs every package's tests and has one checkpointer,
// so those forced checkpoints queue: a create waits on IPC/CheckpointDone behind the requests
// ahead of it, and `daemon-go` job 138679625811 waited out internal/store's 15-minute timeout
// inside one. internal/store asks for 731 of them in a run and internal/workflow for 508.
//
// This file takes the creates away rather than queueing them: a database a test gives back is
// reset and handed to the next test of that shape, so a run creates as many databases as it holds
// at once rather than one per test. A shape is what the test asked for — an empty database, or a
// copy of a template a package migrated — and a database of one shape is never given to a taker of
// another.
//
// Contamination is the risk this trades for, so a database returns to the free list only after it
// is reset AND read back against the shape's template: the same schema (fingerprint), the same
// rows in schema_version, and no row in any other table. A database that fails that read is
// dropped, never reused, and the next taker gets a fresh one. The planted-contamination test of
// this package leaves a row, a moved sequence and an altered table behind, and reads that the next
// taker sees none of it.

package testdb

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TemplateCopy is the URL of a database copied from template, named <prefix>_<random>, from this
// binary's pool: a test takes one, and gives it back reset and read against the template when it
// ends. internal/testpg's Clone is its caller, for the template that package migrates; a test of
// an empty database takes Database instead. dsn is what LEGION_TEST_PG_DSN holds, and the URL
// carries the pool keys PoolBounded gives it.
func TemplateCopy(t testing.TB, dsn, template, prefix string) string {
	t.Helper()
	at, giveBack, err := take(dsn, shape{template: template, prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := giveBack(); err != nil {
			t.Error(err)
		}
	})
	bounded, err := PoolBounded(dsn, at)
	if err != nil {
		t.Fatal(err)
	}
	return bounded
}

// poolBound is how long a reset or a reading of one database may take before the database is
// dropped instead. A test that left a connection open holds a lock the truncate waits for, and a
// cleanup that waited for it would hang the package rather than fail it; the forced drop that
// follows ends that connection, as every drop of a test database does.
const poolBound = 30 * time.Second

// shape is what a taker asks for: the template a database is copied from, "" for the server's own
// template1, and the prefix its name carries, which names the package in the server's logs.
type shape struct {
	template string
	prefix   string
}

// pool is the databases of each shape that no test holds, and the fingerprint each shape's
// template read when the first database of it was made.
var pool struct {
	mu    sync.Mutex
	free  map[shape][]string
	print map[shape]databaseContent
	made  map[string]bool
	on    map[string]*pgxpool.Pool
}

// databaseContent is what a database of a shape must read as when it comes back: the fingerprint
// of its schema, and the rows of schema_version, which a migrated template holds and a reset
// keeps. Every other table is empty, which reset leaves and verify reads.
type databaseContent struct {
	fingerprint string
	versions    string
}

// take is the URL of a database of this shape for the test, from the free list where one waits and
// fresh otherwise, and the function that resets, reads back and frees it. The caller registers
// that function as the test's cleanup.
func take(dsn string, s shape) (string, func() error, error) {
	admin, err := adminPool(dsn)
	if err != nil {
		return "", nil, err
	}
	name, err := takeFree(admin, s)
	if err != nil {
		return "", nil, err
	}
	if name == "" {
		if name, err = createDatabase(admin, s); err != nil {
			return "", nil, err
		}
	}
	at, err := DatabaseURL(dsn, name)
	if err != nil {
		return "", nil, err
	}
	return at, func() error { return give(admin, s, name) }, nil
}

// takeFree is a database of the shape that no test holds, or "" where none waits.
func takeFree(admin *pgxpool.Pool, s shape) (string, error) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	free := pool.free[s]
	if len(free) == 0 {
		return "", nil
	}
	name := free[len(free)-1]
	pool.free[s] = free[:len(free)-1]
	return name, nil
}

// createDatabase makes a database of the shape and records what the shape's template reads as,
// from the first one made, which no test has touched.
func createDatabase(admin *pgxpool.Pool, s shape) (string, error) {
	name, err := RandomName(s.prefix)
	if err != nil {
		return "", err
	}
	// A copy of the template's files. Created the default way, from PostgreSQL 15, every page of
	// the template goes into the write-ahead log, which docs/testing.md measured and this module
	// holds itself to ("A database a Go test creates").
	statement := "create database " + name + " strategy file_copy"
	if s.template != "" {
		statement += " template " + s.template
	}
	ctx := context.Background()
	if _, err := admin.Exec(ctx, statement); err != nil {
		return "", fmt.Errorf("create %s: %w", name, err)
	}
	pool.mu.Lock()
	if pool.made == nil {
		pool.made = map[string]bool{}
	}
	pool.made[name] = true
	_, known := pool.print[s]
	pool.mu.Unlock()
	if known {
		return name, nil
	}
	content, err := readContent(admin, name)
	if err != nil {
		_ = dropDatabase(admin, name)
		return "", err
	}
	pool.mu.Lock()
	if pool.print == nil {
		pool.print = map[shape]databaseContent{}
	}
	if _, known := pool.print[s]; !known {
		pool.print[s] = content
	}
	pool.mu.Unlock()
	return name, nil
}

// give resets name, reads it back against its shape's template, and frees it for the next taker.
// A database that fails either step is dropped rather than handed on, so a test never takes a
// database another test left something in. A binary whose TestMain does not call Main drops each
// database as its test ends, as before the pool: nothing would drop a pooled one when its tests
// end.
func give(admin *pgxpool.Pool, s shape, name string) error {
	shared.mu.Lock()
	pooling := shared.main
	shared.mu.Unlock()
	if !pooling {
		return drop(admin, name, nil)
	}
	if err := reset(admin, s, name); err != nil {
		return drop(admin, name, err)
	}
	pool.mu.Lock()
	want, known := pool.print[s]
	pool.mu.Unlock()
	if !known {
		return drop(admin, name, fmt.Errorf("no template reading of this shape was recorded"))
	}
	if err := verify(admin, name, want); err != nil {
		return drop(admin, name, err)
	}
	pool.mu.Lock()
	if pool.free == nil {
		pool.free = map[shape][]string{}
	}
	pool.free[s] = append(pool.free[s], name)
	pool.mu.Unlock()
	return nil
}

// drop takes name out of the pool for the reason given. The reason is not an error of the test
// that gave the database back: the next taker gets a fresh one, so the run carries on.
func drop(admin *pgxpool.Pool, name string, reason error) error {
	pool.mu.Lock()
	delete(pool.made, name)
	pool.mu.Unlock()
	closePooled(name)
	if err := dropDatabase(admin, name); err != nil {
		return fmt.Errorf("drop %s, which %v: %w", name, reason, err)
	}
	return nil
}

// reset empties name for the next taker of its shape. A copy of a migrated template keeps its
// schema, so every table but schema_version — the one the template holds rows in — is truncated
// and its sequences set back. A copy of template1 holds no schema of its own, and a test of it
// creates one (a migration test migrates it step by step), so its schemas are dropped and public
// made again. Either way verify reads the result back before the database is handed on.
func reset(admin *pgxpool.Pool, s shape, name string) error {
	at, err := on(admin, name)
	if err != nil {
		return err
	}
	ctx, stop := context.WithTimeout(context.Background(), poolBound)
	defer stop()
	if s.template == "" {
		var schemas []string
		rows, err := at.Query(ctx, `select format('%I', nspname) from pg_namespace
			where nspname not in ('pg_catalog', 'information_schema') and nspname !~ '^pg_'`)
		if err != nil {
			return fmt.Errorf("read the schemas of %s: %w", name, err)
		}
		for rows.Next() {
			var schema string
			if err := rows.Scan(&schema); err != nil {
				rows.Close()
				return err
			}
			schemas = append(schemas, schema)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		sort.Strings(schemas)
		for _, schema := range schemas {
			if _, err := at.Exec(ctx, "drop schema "+schema+" cascade"); err != nil {
				return fmt.Errorf("drop the schema %s of %s: %w", schema, name, err)
			}
		}
		if _, err := at.Exec(ctx, "create schema public"); err != nil {
			return fmt.Errorf("make the public schema of %s again: %w", name, err)
		}
		return nil
	}
	var tables []string
	rows, err := at.Query(ctx, tableNames)
	if err != nil {
		return fmt.Errorf("read the tables of %s: %w", name, err)
	}
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, table)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(tables) > 0 {
		sort.Strings(tables)
		if _, err := at.Exec(ctx, "truncate table "+strings.Join(tables, ", ")+" restart identity cascade"); err != nil {
			return fmt.Errorf("empty the tables of %s: %w", name, err)
		}
	}
	return nil
}

// tableNames is every ordinary table a test could have written, schema_version apart: the one
// table a migrated template holds rows in, which a reset keeps and verify reads back.
const tableNames = `select format('%I.%I', n.nspname, c.relname)
	from pg_class c join pg_namespace n on n.oid = c.relnamespace
	where c.relkind = 'r' and n.nspname not in ('pg_catalog', 'information_schema')
		and n.nspname !~ '^pg_' and c.relname <> 'schema_version'`

// verify reads name back against what its shape's template read as: the same schema, the same
// rows of schema_version, and no row in any other table.
func verify(admin *pgxpool.Pool, name string, want databaseContent) error {
	got, err := readContent(admin, name)
	if err != nil {
		return err
	}
	if got.fingerprint != want.fingerprint {
		return fmt.Errorf("its schema reads %s, and the template's %s", got.fingerprint, want.fingerprint)
	}
	if got.versions != want.versions {
		return fmt.Errorf("its schema_version reads %s, and the template's %s", got.versions, want.versions)
	}
	return nil
}

// readContent is the fingerprint of a database's schema, the rows of its schema_version, and a
// refusal where any other table holds a row. The fingerprint names every table, view, sequence,
// index, column and type of the schemas a test can write, every function of them, every extension
// and every setting the database carries (`alter database … set`, which outlives every schema), so
// a table altered, a function left behind or a setting changed reads as another schema.
func readContent(admin *pgxpool.Pool, name string) (databaseContent, error) {
	at, err := on(admin, name)
	if err != nil {
		return databaseContent{}, err
	}
	ctx, stop := context.WithTimeout(context.Background(), poolBound)
	defer stop()
	var content databaseContent
	if err := at.QueryRow(ctx, fingerprintQuery).Scan(&content.fingerprint); err != nil {
		return databaseContent{}, fmt.Errorf("read the schema of %s: %w", name, err)
	}
	// PostgreSQL resolves every relation a statement names before it runs any of it, so a database
	// with no schema_version — a copy of template1 — is asked whether it has one first.
	var versioned bool
	if err := at.QueryRow(ctx, `select to_regclass('public.schema_version') is not null`).Scan(&versioned); err != nil {
		return databaseContent{}, fmt.Errorf("read whether %s holds schema_version: %w", name, err)
	}
	if versioned {
		if err := at.QueryRow(ctx, versionsQuery).Scan(&content.versions); err != nil {
			return databaseContent{}, fmt.Errorf("read the versions of %s: %w", name, err)
		}
	}
	var rows int64
	var holder string
	if err := at.QueryRow(ctx, rowsQuery).Scan(&rows, &holder); err != nil {
		return databaseContent{}, fmt.Errorf("count the rows of %s: %w", name, err)
	}
	if rows > 0 {
		return databaseContent{}, fmt.Errorf("%d rows stand in it, the first in %s", rows, holder)
	}
	return content, nil
}

const fingerprintQuery = `select coalesce(md5(string_agg(entry, E'\n' order by entry)), '') from (
	select format('rel %s.%s %s', n.nspname, c.relname, c.relkind) as entry
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where n.nspname not in ('pg_catalog', 'information_schema') and n.nspname !~ '^pg_'
	union all
	select format('col %s.%s.%s %s %s', n.nspname, c.relname, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull)
		from pg_attribute a join pg_class c on c.oid = a.attrelid join pg_namespace n on n.oid = c.relnamespace
		where a.attnum > 0 and not a.attisdropped
			and n.nspname not in ('pg_catalog', 'information_schema') and n.nspname !~ '^pg_'
	union all
	select format('con %s.%s %s', n.nspname, t.relname, co.conname)
		from pg_constraint co join pg_class t on t.oid = co.conrelid join pg_namespace n on n.oid = t.relnamespace
		where n.nspname not in ('pg_catalog', 'information_schema') and n.nspname !~ '^pg_'
	union all
	select format('fun %s.%s', n.nspname, p.proname)
		from pg_proc p join pg_namespace n on n.oid = p.pronamespace
		where n.nspname not in ('pg_catalog', 'information_schema') and n.nspname !~ '^pg_'
	union all
	select format('ext %s', extname) from pg_extension
	union all
	select format('sch %s', nspname) from pg_namespace
		where nspname not in ('pg_catalog', 'information_schema') and nspname !~ '^pg_'
	union all
	select format('set %s', setting) from pg_db_role_setting d, unnest(d.setconfig) setting
		where d.setdatabase = (select oid from pg_database where datname = current_database())
) reading`

// versionsQuery is the versions schema_version records, which a migrated template holds and a
// reset keeps; readContent asks it only of a database that holds the table.
const versionsQuery = `select coalesce(string_agg(version::text, ',' order by version), '') from public.schema_version`

// rowsQuery counts the rows of every ordinary table but schema_version, in one statement, and
// names the first table that holds one.
const rowsQuery = `select coalesce(sum(n), 0)::bigint, coalesce(min(t) filter (where n > 0), '') from (
	select format('%I.%I', n.nspname, c.relname) as t,
		(xpath('/row/c/text()', query_to_xml(format('select count(*) as c from %I.%I', n.nspname, c.relname), false, true, '')))[1]::text::bigint as n
	from pg_class c join pg_namespace n on n.oid = c.relnamespace
	where c.relkind = 'r' and n.nspname not in ('pg_catalog', 'information_schema')
		and n.nspname !~ '^pg_' and c.relname <> 'schema_version'
) counted`

// on returns the pool this file holds on a database it resets and reads, opened once and closed
// by Main. The databases are the pool's own, so there are as many of these as the pool holds at
// once, not one per test.
func on(admin *pgxpool.Pool, name string) (*pgxpool.Pool, error) {
	pool.mu.Lock()
	if at, open := pool.on[name]; open {
		pool.mu.Unlock()
		return at, nil
	}
	pool.mu.Unlock()
	config := admin.Config().ConnConfig
	at, err := pgxpool.New(context.Background(), fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		config.User, config.Password, config.Host, config.Port, name))
	if err != nil {
		return nil, fmt.Errorf("open %s to read it back: %w", name, err)
	}
	pool.mu.Lock()
	if open, already := pool.on[name]; already {
		pool.mu.Unlock()
		at.Close()
		return open, nil
	}
	if pool.on == nil {
		pool.on = map[string]*pgxpool.Pool{}
	}
	pool.on[name] = at
	pool.mu.Unlock()
	return at, nil
}

// closePooled closes the pools this file holds on name, so the database can be dropped.
func closePooled(name string) {
	pool.mu.Lock()
	at, open := pool.on[name]
	delete(pool.on, name)
	pool.mu.Unlock()
	if open {
		at.Close()
	}
}

// pooledDatabases is every database this file made and still holds, and it closes the pools on
// them: Main drops them when the binary's tests end.
func pooledDatabases() []string {
	pool.mu.Lock()
	names := make([]string, 0, len(pool.made))
	for name := range pool.made {
		names = append(names, name)
	}
	pools := pool.on
	pool.made, pool.free, pool.on = nil, nil, nil
	pool.mu.Unlock()
	for _, at := range pools {
		at.Close()
	}
	sort.Strings(names)
	return names
}
