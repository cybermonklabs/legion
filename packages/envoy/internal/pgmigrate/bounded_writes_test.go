package pgmigrate

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// boundedWritesFixtureSQL is the database the bounded-write census tests judge their rows against.
// tuples is graph_tuples as 20261109013400 creates it, with the unique index on md5(to_id) that an
// attribute's insert names as its conflict target, a unique tag that tuple_tags references, and a
// trigger, tuples_noop, which is not on reviewedTriggers: the census refuses tuples for it unless
// a test reviews it for its own census (tuplesTriggerReviewed). nodes holds a two-column primary
// key, as graph_nodes does, and ruled a rule. Each holds rows, so each is above the one-byte limit
// the tests run the census under (censusOf).
const boundedWritesFixtureSQL = `
create table schema_migrations (version bigint primary key);
insert into schema_migrations (version) values (1);
create table tuples (
  seq bigserial primary key,
  from_kind text not null,
  from_id text not null,
  kind text not null,
  to_kind text not null,
  to_id text not null,
  at timestamptz not null,
  who jsonb not null,
  source_event bigint not null,
  legacy_source_seq bigint,
  asserted boolean not null default true,
  tag text unique
);
create unique index tuples_once on tuples (from_kind, from_id, kind, to_kind, to_id, source_event, asserted)
  where to_kind <> 'value' and source_event <> 0;
create unique index tuples_value_once on tuples (from_kind, from_id, kind, md5(to_id), source_event, asserted)
  where to_kind = 'value' and source_event <> 0;
create table tuple_tags (id int primary key, tag text references tuples (tag));
create function tuples_noop() returns trigger language plpgsql as $$ begin return null; end $$;
create trigger tuples_noop after insert or update on tuples for each row execute function tuples_noop();
create table nodes (kind text not null, id text not null, state text not null, made_at timestamptz, primary key (kind, id));
create table ruled (id int primary key, note text);
create rule ruled_also as on update to ruled do also select 1;
insert into tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
  select 'issue', 'n' || g, 'part_of', 'issue', 'm' || g, now(), '{}', g from generate_series(1, 100) g;
insert into nodes select 'issue', 'n' || g, 'open', now() from generate_series(1, 100) g;
insert into ruled select g, 'x' from generate_series(1, 10) g;
`

// boundedWritesDatabase connects to a database of the test's own, loaded with
// boundedWritesFixtureSQL, on the server DISPATCH_TEST_DATABASE_URL names, and skips the test when
// that variable is unset, as catalogOnlyDatabase does. The database is dropped when the test ends.
func boundedWritesDatabase(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	raw := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run the census's bounded-write tests")
	}
	adminURL, err := url.Parse(raw)
	if err != nil {
		// url.Parse's own error quotes the URL, which carries a credential.
		t.Fatal("DISPATCH_TEST_DATABASE_URL is not a URL")
	}
	adminURL.Path = "/postgres"
	conn := connect(t, ctx, testdb.DatabaseOn(t, adminURL.String(), "pgmigrate_bounded"))
	if _, err := conn.Exec(ctx, boundedWritesFixtureSQL); err != nil {
		t.Fatalf("load the fixture: %v", err)
	}
	return conn
}

// tupleColumns are the columns of tuples an insert of the tests gives, the rest taking their
// defaults.
const tupleColumns = "insert into tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event) values "

// boundedInsert is an insert into tuples of one literal row, the statement a refused row sets
// beside what refuses it.
const boundedInsert = tupleColumns + "('issue', 's1', 'part_of', 'issue', 't1', '2026-11-21T00:00:00Z', '{}', 1)"

// tableNamed is the census of the table name among entry's tables, ok false where it read none.
func tableNamed(entry MigrationCensus, name string) (TableCensus, bool) {
	for _, table := range entry.Tables {
		if table.Name == name && table.Through == "" {
			return table, true
		}
	}
	return TableCensus{}, false
}

// tuplesTriggerReviewed is reviewedTriggers with the fixture's trigger on tuples added, the list a
// census that admits tuples beside that trigger is given (censusReviewing): tuples_noop returns
// null and reads nothing. The list is that census's own, so a census of another test that runs
// meanwhile still refuses tuples for the trigger.
func tuplesTriggerReviewed() []reviewedTrigger {
	return append(slices.Clone(reviewedTriggers), reviewedTrigger{table: "tuples", trigger: "tuples_noop", bounded: "it returns null and reads no table"})
}

// A table above the limit is admitted for bounded writes, of each shape a seed of a large table
// takes: an insert of literal rows, a constant, a negative one, NULL, a cast of a constant to
// jsonb, now() and current_timestamp among its values; an insert with ON CONFLICT DO NOTHING and
// no target, and one whose target is an expression index's elements and predicate; an update by a
// one-column primary key in (…), by a two-column one (a, b) in ((…), …), by a = … and b = …, and
// by a key cast to the key's own type; and several bounded writes in one migration beside a SET
// LOCAL lock_timeout. Each table is above the one-byte limit, the census admits it with no
// refusal, counts the statements that touch it, names its enabled trigger, which the test reviews
// for its own census (tuplesTriggerReviewed), and the report prints the admission. Each migration
// then runs on the fixture, in a transaction the test rolls back, so no shape admitted here is one
// Postgres refuses.
func TestCensusAdmitsATableAboveTheLimitForEachBoundedWriteShape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := boundedWritesDatabase(t, ctx)
	reviewed := tuplesTriggerReviewed()
	for _, row := range []struct {
		name, sql, table string
		statements       int
	}{
		{"an insert of literal rows", tupleColumns +
			`('issue', 'a1', 'part_of', 'issue', 'b1', '2026-11-21T00:00:00Z', '{"kind":"person","id":"p1"}'::jsonb, -1),
			('issue', 'a2', 'title', 'value', 'A title', now(), '{}', 2),
			('issue', 'a3', 'status', 'value', 'open', current_timestamp, '{}', 3)`, "tuples", 1},
		{"an insert with on conflict do nothing and no target", tupleColumns +
			"('issue', 'n1', 'part_of', 'issue', 'm1', now(), '{}', 1) on conflict do nothing", "tuples", 1},
		{"an insert with on conflict on an expression index's target and predicate do nothing", tupleColumns +
			"('issue', 'a4', 'title', 'value', 'A title', now(), '{}', 4) on conflict (from_kind, from_id, kind, md5(to_id), source_event, asserted) where to_kind = 'value' and source_event <> 0 do nothing", "tuples", 1},
		{"an update by a one-column primary key in a list", "update tuples set asserted = false, legacy_source_seq = 7 where seq in (1, 2, 3)", "tuples", 1},
		{"an update by a two-column primary key in a list of rows", "update nodes as n set state = 'closed', made_at = now() where (kind, id) in (('issue', 'n1'), ('issue', 'n2'))", "nodes", 1},
		{"an update by each column of a two-column primary key", "update nodes set state = 'held' where kind = 'issue' and id = 'n3'", "nodes", 1},
		{"an update by a key cast to the key's type", "update tuples set asserted = true where seq = '5'::bigint", "tuples", 1},
		{"bounded writes beside a set local lock_timeout", "set local lock_timeout = '5s'; " + boundedInsert + "; update tuples set legacy_source_seq = 9 where seq = 4;", "tuples", 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			pending := censusReviewing(t, ctx, conn, []string{row.sql}, reviewed)
			entry := pending[0]
			table, ok := tableNamed(entry, row.table)
			if !ok || !table.Exists || table.Bytes <= 1 {
				t.Fatalf("the census read %s as %+v (found %v); want it in the database and above the one-byte limit", row.table, table, ok)
			}
			if entry.TableSizeExemption != "" || entry.ConcurrentIndex || len(entry.Refusals) != 0 || table.BoundedWrites != row.statements {
				t.Fatalf("exemption %q, concurrent index %v, refusals %q, bounded writes %d; want no other exemption, no refusal and %s admitted for %d bounded writes",
					entry.TableSizeExemption, entry.ConcurrentIndex, entry.Refusals, table.BoundedWrites, row.table, row.statements)
			}
			var wantTriggers []string
			if row.table == "tuples" {
				wantTriggers = []string{"tuples_noop"}
			}
			if !slices.Equal(table.Triggers, wantTriggers) {
				t.Errorf("triggers %q, want %q", table.Triggers, wantTriggers)
			}
			var out strings.Builder
			(&Report{VersionTable: "schema_migrations", SchemaVersion: 1, TableLimit: 1, LongTransaction: CensusLongTransaction, Pending: pending}).Write(&out)
			if !strings.Contains(out.String(), "touches "+row.table+": ") || !strings.Contains(out.String(), ", above the 1 bytes limit, admitted for bounded writes: ") ||
				!strings.Contains(out.String(), "each an insert of at most 1,000 literal rows or an update of at most 1,000 rows by their primary key, so its rows were not counted; enabled triggers, which run for each row those writes write: ") {
				t.Errorf("report:\n%s", out.String())
			}
			for _, line := range strings.Split(out.String(), "\n") {
				if strings.Contains(line, "admitted for bounded writes") {
					t.Log(line)
				}
			}
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, row.sql); err != nil {
				t.Fatalf("the admitted migration failed on the fixture: %v", err)
			}
		})
	}
}

// A table above the limit stays refused wherever a statement of the migration that touches it is
// no bounded write, and the refusal names the shape that refused it: an insert whose rows a query
// gives, an update with no WHERE, an update by a column outside the primary key, a lock table
// beside a bounded insert, an insert of one row more than BoundedWriteRows, an update that sets a
// column of the primary key or one a foreign key references, an insert of a value that is no
// literal, an upsert, an update by a key compared with a constant that would cast the key, an
// update of a table with a rule, a bounded insert beside a DO block or after the migration's own
// SET of search_path, a bounded insert after an earlier pending migration that changes the table,
// and a bounded insert into a table whose enabled trigger is not on reviewedTriggers. The census
// reads each table above the one-byte limit; a refusal says the size, the rule and why the
// migration's writes are not bounded, and the table is not admitted.
func TestCensusRefusesATableAboveTheLimitForEveryWriteThatIsNotBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := boundedWritesDatabase(t, ctx)
	oversized := tupleColumns + strings.Repeat("('issue', 'o', 'part_of', 'issue', 'p', now(), '{}', 1), ", BoundedWriteRows) +
		"('issue', 'o', 'part_of', 'issue', 'p', now(), '{}', 1)"
	for _, row := range []struct {
		name       string
		migrations []string
		table      string
		says       string
	}{
		{"an insert … select", []string{"insert into tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event) select from_kind, from_id, 'title', to_kind, to_id, at, who, source_event from tuples where seq = 1"},
			"tuples", "and an insert whose rows come from a query, not from a VALUES list, is not one"},
		{"an update with no where clause", []string{"update tuples set asserted = false"}, "tuples", "and an update with no WHERE clause is not one"},
		{"an update by a column that is not the primary key", []string{"update tuples set asserted = false where from_id = 'n1'"},
			"tuples", "and an update by from_id, which is not its primary key (seq), is not one"},
		{"a lock table beside a bounded insert", []string{"lock table tuples in share row exclusive mode; " + boundedInsert}, "tuples", "and lock table is not one"},
		{"an insert of one row more than the bound", []string{oversized},
			"tuples", fmt.Sprintf("and an insert of %s rows, above the %s a bounded write may hold, is not one", Thousands(BoundedWriteRows+1), Thousands(BoundedWriteRows))},
		{"an update that sets a primary key column", []string{"update tuples set seq = 1000 where seq = 1"}, "tuples", "and an update that sets seq, a column of its primary key (seq), is not one"},
		{"an update that sets a column a foreign key references", []string{"update tuples set tag = 'x' where seq = 1"}, "tuples", "and an update that sets tag, which a foreign key references, is not one"},
		{"an insert of a value that is no literal", []string{tupleColumns + "('issue', 'a', 'part_of', 'issue', 'b', now(), jsonb_build_object('kind', 'person'), 1)"},
			"tuples", "and an insert whose values hold something other than a literal"},
		{"an insert with on conflict do update", []string{boundedInsert + " on conflict (tag) do update set asserted = false"}, "tuples", "and an insert whose ON CONFLICT does anything but DO NOTHING is not one"},
		{"an update by a key compared with a numeric constant", []string{"update tuples set asserted = false where seq = 1.5"},
			"tuples", "and an update whose key values are not literals its primary key's index is searched by"},
		{"an update of a table with a rule", []string{"update ruled set note = 'y' where id = 1"}, "ruled", "and ruled has a rule, which can run any statement in place of a write or beside it"},
		{"a bounded insert beside a DO block", []string{"do $$ begin end $$; " + boundedInsert}, "tuples", "and the migration runs a DO block, and the census cannot read what such a statement changes of the table before the writes run"},
		{"a bounded insert after the migration's own set of search_path", []string{"set local search_path = public; " + boundedInsert},
			"tuples", "and a statement of the migration can change what its names resolve to"},
		{"a bounded insert after an earlier pending migration that alters the table", []string{"alter table tuples add column extra text", boundedInsert},
			"tuples", "and the earlier pending migration 0002_row.up.sql touches tuples with more than bounded writes"},
		{"a bounded insert into a table whose enabled trigger is not on the reviewed list", []string{boundedInsert},
			"tuples", "and tuples has the enabled trigger tuples_noop, which is not on the census's reviewed list (reviewedTriggers)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			pending := censusOf(t, ctx, conn, row.migrations)
			entry := pending[len(pending)-1]
			table, ok := tableNamed(entry, row.table)
			if !ok || !table.Exists || table.Bytes <= 1 {
				t.Fatalf("the census read %s as %+v (found %v); want it in the database and above the one-byte limit", row.table, table, ok)
			}
			if entry.TableSizeExemption != "" || table.BoundedWrites != 0 || !refusalSays(entry, row.table+" is ", aboveTheLimit, boundedWritesRule, row.says) {
				t.Fatalf("exemption %q, bounded writes %d, refusals %q; want %s refused on its size, saying %q", entry.TableSizeExemption, table.BoundedWrites, entry.Refusals, row.table, row.says)
			}
			t.Logf("%s: %q", entry.Migration.Name, entry.Refusals)
		})
	}
}
