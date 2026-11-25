package replay

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// clockWindow is the fixture whose every time is in 2001 (testdata/windows/clock-far-from-now,
// whose README says what each of its facts makes the pass write).
const clockWindow = "clock-far-from-now"

// notSimulated names each time column a replay's database holds that the simulated clock does not
// write, with the reason. schema_version.applied_at is the moment the template the replay's
// database is copied from was migrated (openReplayDatabase), before the run began: setup, which no
// pass of the window writes.
var notSimulated = map[string]string{
	"schema_version.applied_at": "the migration of the template the replay's own database is copied from, before the run",
}

// A replay runs the daemon's own pass on a simulated clock, and every time the pass writes or
// compares must be that clock's, or the replay's line is ordered by the moment it ran rather than
// by the window (spec R1). The fixture's times are in 2001, so a time from the wall clock — Go's
// time.Now or Postgres's now(), whether a statement names it or a column default supplies it —
// lies twenty-five years outside the window. The test replays the fixture under the rule `main`,
// keeps the replay's database, and reads every timestamp and timestamptz column of every table in
// it from information_schema.columns, as data, so a column a later migration adds is read with
// the rest; it fails naming each table and column holding a value outside the window's span plus
// one day.
//
// It then reads the two values of one pass that the waiting line orders against each other: at
// 11:00 the fixture's pass writes ACME-1's owed start (outbox.created_at, by the engine's release
// of its held phase) and then ACME-1's release mark (issues.released_for_wait_at, by the
// admission pass's release for its new wait), and both stand at the window's end. The line
// orders a mark at its time with id 0 and an owed start at its created_at and id
// (record.Waiting), so the two sort in the order the pass wrote them only where created_at is
// before the mark.
func TestReplayWritesNoWallClockTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := replayDSN(t)
	window := fixtureWindow(t, clockWindow)
	rule := mainRule(t)
	result, err := Run(ctx, RunConfig{Window: window, Rule: rule, Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: dsn, Out: t.TempDir(), KeepDB: true})
	if err != nil {
		t.Fatalf("replay the window %s under the rule main: %v", clockWindow, err)
	}
	if result.Database == "" {
		t.Fatal("the replay names no database, and KeepDB keeps the one it made")
	}
	conn := keptDatabase(t, dsn, result.Database)

	low, high := window.From, window.To.Add(24*time.Hour)
	var outside []string
	read := 0
	for _, column := range timeColumns(t, conn) {
		if _, skip := notSimulated[column.table+"."+column.name]; skip {
			continue
		}
		read++
		var count int64
		var earliest, latest *time.Time
		// A column declared `default '0001-01-01 00:00:00+00'` holds that value for "no time yet", the
		// daemon's own sentinel (a phase not completed, a pull request whose review was never seen):
		// it is no clock's reading, so it is left out; every other value must be the simulated clock's.
		noTime := ""
		if column.zeroDefault {
			noTime = ` and ` + pgx.Identifier{column.name}.Sanitize() + `::timestamptz <> '0001-01-01 00:00:00+00'::timestamptz`
		}
		if err := conn.QueryRow(ctx, fmt.Sprintf(`select count(*), min(%[2]s::timestamptz), max(%[2]s::timestamptz) from %[1]s
			where (%[2]s::timestamptz < $1 or %[2]s::timestamptz > $2)%[3]s`,
			pgx.Identifier{column.table}.Sanitize(), pgx.Identifier{column.name}.Sanitize(), noTime), low, high).Scan(&count, &earliest, &latest); err != nil {
			t.Fatalf("read %s.%s: %v", column.table, column.name, err)
		}
		if count > 0 {
			outside = append(outside, fmt.Sprintf("%s.%s: %d values, from %s to %s", column.table, column.name, count,
				earliest.UTC().Format(time.RFC3339Nano), latest.UTC().Format(time.RFC3339Nano)))
		}
	}
	if read == 0 {
		t.Fatal("the replay's database has no time column to read, so nothing was checked")
	}
	if len(outside) > 0 {
		t.Errorf("the replay wrote %d time columns outside the window %s to %s plus a day, so these times are not the simulated clock's:\n\t%s",
			len(outside), low.Format(time.RFC3339), window.To.Format(time.RFC3339), strings.Join(outside, "\n\t"))
	}

	var mark *time.Time
	if err := conn.QueryRow(ctx, `select released_for_wait_at from issues where key = 'ACME-1'`).Scan(&mark); err != nil {
		t.Fatalf("read ACME-1's release mark: %v", err)
	}
	if mark == nil {
		t.Fatal("ACME-1 carries no release mark at the window's end: the pass of 11:00 gave its slot back for its wait on ACME-4, which nothing ends")
	}
	rows, err := conn.Query(ctx, `select created_at from outbox where issue = 'ACME-1' and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'needsSlot' = 'true' order by created_at, id`)
	if err != nil {
		t.Fatalf("read ACME-1's owed starts: %v", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		t.Fatalf("read ACME-1's owed starts: %v", err)
	}
	if len(owed) != 1 {
		t.Fatalf("ACME-1 owes %d starts at the window's end, want the one its held phase's release wrote at 11:00, which no slot lets run", len(owed))
	}
	if !owed[0].Before(*mark) {
		t.Errorf("ACME-1's owed start was created at %s and its release mark is %s: the pass wrote the start first (workflow's releaseHeldRoot) and the mark after it (admit's releaseSlotsForWaits), and the line sorts a mark before a start of the same time, so the start must be the earlier",
			owed[0].UTC().Format(time.RFC3339Nano), mark.UTC().Format(time.RFC3339Nano))
	}
}

// timeColumn is one column of a table of the replay's database, and whether the migrations declare
// it with the year-1 default that stands for "no time yet" (zeroDefault).
type timeColumn struct {
	table, name string
	zeroDefault bool
}

// timeColumns is every timestamp and timestamptz column of every table of conn's schema, read
// from information_schema.columns, so the list is the database's and not this test's; a column
// whose declared default is the year-1 instant is marked zeroDefault.
func timeColumns(t *testing.T, conn *pgx.Conn) []timeColumn {
	t.Helper()
	rows, err := conn.Query(context.Background(), `select c.table_name, c.column_name,
			coalesce(c.column_default like '''0001-01-01 00:00:00%', false)
		from information_schema.columns c
		join information_schema.tables tables on tables.table_schema = c.table_schema and tables.table_name = c.table_name
		where c.table_schema = current_schema() and tables.table_type = 'BASE TABLE'
			and c.data_type in ('timestamp with time zone', 'timestamp without time zone')
		order by c.table_name, c.column_name`)
	if err != nil {
		t.Fatalf("list the time columns of the replay's database: %v", err)
	}
	columns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (timeColumn, error) {
		var column timeColumn
		err := row.Scan(&column.table, &column.name, &column.zeroDefault)
		return column, err
	})
	if err != nil {
		t.Fatalf("list the time columns of the replay's database: %v", err)
	}
	return columns
}

// keptDatabase connects to the database the replay kept, database on dsn's server, with the
// session's time zone UTC so a timestamp column reads as the UTC time it was written as, and drops
// the database when the test ends (dropKeptDatabase).
func keptDatabase(t *testing.T, dsn, database string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	dropKeptDatabase(t, dsn, database)
	url, err := testdb.DatabaseURL(dsn, database)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect to the replay's database %s: %v", database, err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	if _, err := conn.Exec(ctx, "set time zone 'UTC'"); err != nil {
		t.Fatalf("set the session's time zone: %v", err)
	}
	return conn
}

// dropKeptDatabase drops the database the replay kept, database on dsn's server, when the test ends.
func dropKeptDatabase(t *testing.T, dsn, database string) {
	t.Helper()
	admin, err := testdb.DatabaseURL(dsn, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Errorf("connect to drop the replay's database %s: %v", database, err)
			return
		}
		defer conn.Close(ctx)
		if err := testdb.Drop(ctx, conn, pgx.Identifier{database}.Sanitize()); err != nil {
			t.Errorf("drop the replay's database %s: %v", database, err)
		}
	})
}
