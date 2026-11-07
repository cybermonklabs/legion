package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The reset a table-driven test runs between its rows leaves the row of one table behind, and it
// has to: internal/store decides what to migrate from `select coalesce(max(version), 0)` over
// schema_version, so a clone whose copy was emptied reports itself unmigrated and the next
// daemon boot runs the whole chain again over a schema that already carries it — which fails,
// the chain not being idempotent. Everything a row wrote goes, parents and children alike, so
// the next row's keys are free and a whole-table count answers for it alone.
func TestTheRowResetEmptiesTheRowsAndKeepsTheMigrations(t *testing.T) {
	ctx := context.Background()
	pool, reset := tableDatabase(t)
	version, applied := migrationState(t, pool)
	if applied == 0 {
		t.Fatal("the clone carries no applied migration, so this test cannot tell a kept bookkeeping from an empty one")
	}

	// An issue and a slot of it: a parent and a child across a foreign key, so an order that
	// deleted the parent first would fail here rather than in some later test's row.
	records := record.NewStore()
	issue := "RESET-1"
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, bootIssue(issue, issue, "in_progress", phase.Admitted)); err != nil {
			return err
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: issue, Index: 0, AdmittedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("write the row's issue and slot: %v", err)
	}
	if rows := countIn(t, pool, "issues") + countIn(t, pool, "slots"); rows != 2 {
		t.Fatalf("the issue and its slot did not land: %d rows, want 2", rows)
	}

	reset(t)

	for _, table := range []string{"issues", "slots"} {
		if rows := countIn(t, pool, table); rows != 0 {
			t.Errorf("%s holds %d rows after the reset, want none", table, rows)
		}
	}
	if nowVersion, nowApplied := migrationState(t, pool); nowVersion != version || nowApplied != applied {
		t.Errorf("schema_version after the reset is max %d over %d rows, want max %d over %d: a store opened on this database would migrate it again",
			nowVersion, nowApplied, version, applied)
	}

	// The same row's keys are free again, which is what lets a table's rows keep spelling one
	// issue key between them.
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutIssue(ctx, tx, bootIssue(issue, issue, "in_progress", phase.Admitted))
	}); err != nil {
		t.Fatalf("write %s again after the reset: %v", issue, err)
	}
}

// migrationState is the highest migration version the database records and how many it records,
// which is what internal/store reads to decide whether it has anything to apply.
func migrationState(t *testing.T, pool *pgxpool.Pool) (version, applied int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		"select coalesce(max(version), 0)::bigint, count(*) from schema_version").Scan(&version, &applied); err != nil {
		t.Fatalf("read the database's migration bookkeeping: %v", err)
	}
	return version, applied
}

// countIn is the number of rows table holds.
func countIn(t *testing.T, pool *pgxpool.Pool, table string) int64 {
	t.Helper()
	var rows int64
	if err := pool.QueryRow(context.Background(), `select count(*) from "`+table+`"`).Scan(&rows); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return rows
}

// tableDatabase is one migrated database for a table-driven test's whole table, with the reset
// its rows run between them.
//
// A test whose fixture call stands inside its t.Run closure clones one database per row, and the
// package held about seventy of those. A clone of the daemon's schema costs 203.8 ms on this
// issue's pod (measured over ten, pool open included) against 9.2 ms for emptying every table of
// one, so a ten-row table pays 2.04 s for its databases or 0.29 s for one and nine resets.
//
// The rows keep the keys they spell. They are written as if each had a database to itself —
// claiming "LEGION-1", reporting on "LEGION-2", asserting `select count(*)` over a whole table —
// and the reset is what keeps that true: a row starts on a database holding none of the previous
// row's rows, so its keys do not collide and a whole-table count still answers for it. Nothing
// about what a row asserts changes.
func tableDatabase(t *testing.T) (*pgxpool.Pool, func(t *testing.T)) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	return pool, func(t *testing.T) {
		t.Helper()
		resetDatabase(t, pool)
	}
}

// resetDatabase empties every table of pool's database that carries test rows, leaving the
// migration bookkeeping alone.
//
// schema_version is not emptied: internal/store reads `select coalesce(max(version), 0)` from it
// to decide what to migrate, so a clone whose copy was emptied reports itself unmigrated and the
// next daemon boot runs the whole chain again over a schema that already has it — which fails,
// since the chain is not idempotent (0033_tree_lifecycle.up.sql adds a column). Read on a fresh
// clone of the template, it is the only table of the 34 that carries a row at all, so the rule
// "empty what holds test rows" and the rule "empty everything but schema_version" name the same
// set today; the check below holds them to it.
//
// The tables are emptied with `delete from`, child before parent. No key of this schema needs
// that order today: thirteen of its fifteen foreign keys are `on delete cascade`, and the two
// that are `on delete set null` point at nullable columns, so a flat delete in catalogue order
// empties it too. The order is defensive and costs one catalogue read per test binary: it is
// what keeps this fixture working when a key that restricts is added. `truncate … cascade`
// would need no order at all and costs 118.9 ms against the deletes' 9.2 ms on this pod, which
// is most of a clone again.
func resetDatabase(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	statements, err := resetStatements(context.Background(), pool)
	if err != nil {
		t.Fatalf("read the tables to reset: %v", err)
	}
	if _, err := pool.Exec(context.Background(), statements); err != nil {
		t.Fatalf("reset the test's database between rows: %v", err)
	}
}

// resetOnce memoises the reset for the test binary: every clone copies one template, so the
// tables and their dependencies are the same for all of them.
var (
	resetOnce       sync.Once
	resetStatement  string
	resetReadFailed error
)

// resetStatements is the semicolon-joined `delete from` for every table but schema_version, read
// once from the first database it is asked about.
func resetStatements(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	resetOnce.Do(func() {
		ordered, err := tablesChildFirst(ctx, pool)
		if err != nil {
			resetReadFailed = err
			return
		}
		deletes := make([]string, 0, len(ordered))
		for _, table := range ordered {
			deletes = append(deletes, `delete from "`+table+`"`)
		}
		resetStatement = strings.Join(deletes, "; ")
	})
	return resetStatement, resetReadFailed
}

// tablesChildFirst lists the public tables of the database, schema_version left out, with every
// table before the tables it references, so deleting them in that order violates no foreign key.
// A cycle of references, which this schema has none of, is reported rather than ordered.
func tablesChildFirst(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `select tablename from pg_tables where schemaname = 'public'`)
	if err != nil {
		return nil, err
	}
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		if name != "schema_version" {
			tables[name] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	references, err := pool.Query(ctx, `
		select child.relname, parent.relname
		from pg_constraint c
		join pg_class child on child.oid = c.conrelid
		join pg_class parent on parent.oid = c.confrelid
		join pg_namespace n on n.oid = child.relnamespace
		where c.contype = 'f' and n.nspname = 'public'`)
	if err != nil {
		return nil, err
	}
	parents := map[string][]string{}
	for references.Next() {
		var child, parent string
		if err := references.Scan(&child, &parent); err != nil {
			references.Close()
			return nil, err
		}
		if child != parent && tables[child] && tables[parent] {
			parents[child] = append(parents[child], parent)
		}
	}
	references.Close()
	if err := references.Err(); err != nil {
		return nil, err
	}

	// Depth-first over the reference edges, each table emitted after the children that reference
	// it have been: the order deletes run in.
	const (
		unseen = iota
		open
		done
	)
	state := map[string]int{}
	var order []string
	var visit func(string) error
	visit = func(table string) error {
		switch state[table] {
		case done:
			return nil
		case open:
			return fmt.Errorf("the schema's foreign keys form a cycle through %s, which no delete order empties", table)
		}
		state[table] = open
		for _, parent := range parents[table] {
			if err := visit(parent); err != nil {
				return err
			}
		}
		state[table] = done
		order = append(order, table)
		return nil
	}
	names := make([]string, 0, len(tables))
	for table := range tables {
		names = append(names, table)
	}
	for _, table := range names {
		if err := visit(table); err != nil {
			return nil, err
		}
	}
	// visit emits a parent before the child that references it; deleting needs the reverse.
	for left, right := 0, len(order)-1; left < right; left, right = left+1, right-1 {
		order[left], order[right] = order[right], order[left]
	}
	return order, nil
}
