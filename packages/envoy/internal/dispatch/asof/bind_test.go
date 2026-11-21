package asof_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
)

func mustCut(t *testing.T, raw string) asof.Cut {
	t.Helper()
	cut, err := asof.ParseCut(raw)
	if err != nil {
		t.Fatalf("ParseCut(%q): %v", raw, err)
	}
	return cut
}

// Bind puts the shadows of the tables a statement names in front of the statement's own text. A
// statement that opens its own WITH list gets the shadows as the first members of that list,
// since a statement holds one WITH list and its members may read the shadows only from a later
// place in it. Text inside a literal, a quoted identifier or a dollar-quoted body is not SQL, so a
// "with" there does not open a list and a ")" there does not close a member.
func TestBindKeepsTheStatementsOwnWithList(t *testing.T) {
	cut := mustCut(t, "2026-11-10T00:00:00Z")
	shadows := asof.Shadows(cut, "events")
	if !strings.Contains(shadows, "public.events") || !strings.Contains(shadows, "'2026-11-10T00:00:00.000000Z'::timestamptz") {
		t.Fatalf("Shadows(%s, events) = %q, want the events shadow reading public.events up to the cut's literal", cut, shadows)
	}

	for _, row := range []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "a plain select",
			sql:  "select count(*) from events",
			want: "with " + shadows + "\nselect count(*) from events",
		},
		{
			name: "a leading comment and blank lines stay first",
			sql:  "-- the issue's events\n\n/* newest /* nested */ first */\n  select id from events order by id desc",
			want: "-- the issue's events\n\n/* newest /* nested */ first */\n  with " + shadows + "\nselect id from events order by id desc",
		},
		{
			name: "its own WITH list",
			sql:  "WITH x AS (SELECT id FROM events) SELECT * FROM x",
			want: "WITH " + shadows + ", x AS (SELECT id FROM events) SELECT * FROM x",
		},
		{
			name: "with recursive keeps recursive",
			sql:  "with recursive chain(key, depth) as materialized (select 'A-1', 0 union all select issues.parent_key, chain.depth + 1 from issues join chain on issues.key = chain.key) select key from chain",
			want: "with recursive " + asof.Shadows(cut, "issues") + ", chain(key, depth) as materialized (select 'A-1', 0 union all select issues.parent_key, chain.depth + 1 from issues join chain on issues.key = chain.key) select key from chain",
		},
		{
			name: "search and cycle clauses stay in its list",
			sql:  "with recursive walk(n, m) as (select 1, 1 union all select n + 1, m from walk where n < 3) search depth first by n, m set ordering cycle n, m set looped to 'Y' default 'N' using path, again as not materialized (select n from walk) select n from again where n in (select id from events)",
			want: "with recursive " + shadows + ", walk(n, m) as (select 1, 1 union all select n + 1, m from walk where n < 3) search depth first by n, m set ordering cycle n, m set looped to 'Y' default 'N' using path, again as not materialized (select n from walk) select n from again where n in (select id from events)",
		},
		{
			name: "a string literal or a dollar-quoted body holds with and insert",
			sql:  "select 'with x as (insert into t values (1))' as w, $body$ ) with insert $body$ as b from events",
			want: "with " + shadows + "\nselect 'with x as (insert into t values (1))' as w, $body$ ) with insert $body$ as b from events",
		},
		{
			name: "literals inside its own WITH list hold a closing parenthesis and delete",
			sql:  `with x as (select $$ ) insert into t $$ as b, E'it\'s ) delete' as e, 'a'') update' as s, "update)" from events) select * from x`,
			want: "with " + shadows + `, x as (select $$ ) insert into t $$ as b, E'it\'s ) delete' as e, 'a'') update' as s, "update)" from events) select * from x`,
		},
		{
			name: "parenthesised selects",
			sql:  "(select id from events where type = 'a') union (select id from events where type = 'b')",
			want: "with " + shadows + "\n(select id from events where type = 'a') union (select id from events where type = 'b')",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, err := asof.Bind(row.sql, cut)
			if err != nil {
				t.Fatalf("Bind(%q): %v", row.sql, err)
			}
			if got != row.want {
				t.Errorf("Bind(%q)\n got: %q\nwant: %q", row.sql, got, row.want)
			}
		})
	}
}

// A read at a cut writes nothing. Bind refuses a statement that writes before the store sends
// it, and the refusal names the statement so the route that sent it can be found. The rule is
// the statement's form: select ... for update locks rows but writes none, so it passes.
func TestABoundStatementThatWritesIsRefused(t *testing.T) {
	cut := mustCut(t, "2026-11-10T00:00:00Z")
	long := "delete from events where type in ('issue.created', 'issue.updated', 'issue.closed', 'ask.opened')"

	for _, row := range []struct {
		name      string
		sql       string
		statement string
	}{
		{name: "insert", sql: "insert into events (type) values ('x')", statement: "insert into events (type) values ('x')"},
		{name: "update", sql: "UPDATE issues SET title = 'x'", statement: "UPDATE issues SET title = 'x'"},
		{name: "delete", sql: "  -- clean up\n  delete from events", statement: "delete from events"},
		{name: "merge", sql: "merge into issues using projects on false when not matched then do nothing", statement: "merge into issues using projects on false when not matched then do nothing"},
		{
			name:      "a data-modifying member of its own WITH list",
			sql:       "with m as (insert into events (type) values ('x') returning id) select id from m",
			statement: "with m as (insert into events (type) values ('x') returning id) select id from m",
		},
		{name: "a write after its own WITH list", sql: "with x as (select 1) delete from events", statement: "with x as (select 1) delete from events"},
		{name: "a second statement after a read", sql: "select 1; delete from events", statement: "select 1; delete from events"},
		{name: "a second statement after a setting", sql: "set local statement_timeout = '15s'; delete from events", statement: "set local statement_timeout = '15s'; delete from events"},
		{name: "a long statement is named by its first 80 characters", sql: long, statement: long[:80]},
	} {
		t.Run(row.name, func(t *testing.T) {
			bound, err := asof.Bind(row.sql, cut)
			var refused *asof.RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Bind(%q) = %q, %v; want an *asof.RefusedError", row.sql, bound, err)
			}
			if refused.Statement != row.statement {
				t.Errorf("Bind(%q) refusal names %q, want %q", row.sql, refused.Statement, row.statement)
			}
			if !strings.Contains(err.Error(), row.statement) {
				t.Errorf("Bind(%q) refusal %q does not name the statement %q", row.sql, err.Error(), row.statement)
			}
		})
	}

	t.Run("select for update is a read by this rule, not a refusal", func(t *testing.T) {
		const sql = "select id from issues where key = $1 for update"
		bound, err := asof.Bind(sql, cut)
		if err != nil {
			t.Fatalf("Bind(%q): %v, want it bound", sql, err)
		}
		if want := "with " + asof.Shadows(cut, "issues") + "\n" + sql; bound != want {
			t.Errorf("Bind(%q)\n got: %q\nwant: %q", sql, bound, want)
		}
	})
}

// PostgreSQL parses and analyses each member of a WITH list for each statement, so Bind writes
// only the shadows of the tables a statement names, in the order of the shadows. A table is named
// by a word, in any case, or by a quoted identifier in the table's own case. A word in a string
// literal, a dollar-quoted body or a comment names nothing, and a statement that names no
// shadowed table is sent as it is written, though a write is refused still.
func TestBindWritesOnlyTheShadowsAStatementNames(t *testing.T) {
	cut := mustCut(t, "2026-11-10T00:00:00Z")

	for _, row := range []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "a statement that names asks and issues gets those two shadows, in the shadows' order",
			sql:  "select a.id from asks a join issues i on i.key = a.issue_key",
			want: "with " + asof.Shadows(cut, "issues", "asks") + "\nselect a.id from asks a join issues i on i.key = a.issue_key",
		},
		{
			name: "a word in capitals and a quoted identifier in the table's case name their tables",
			sql:  `select * from ISSUES join "asks" on true join "Events" on true`,
			want: "with " + asof.Shadows(cut, "issues", "asks") + "\n" + `select * from ISSUES join "asks" on true join "Events" on true`,
		},
		{
			name: "its own WITH list gets only the shadows its members name",
			sql:  "with mine as (select key from issues) select key from mine",
			want: "with " + asof.Shadows(cut, "issues") + ", mine as (select key from issues) select key from mine",
		},
		{
			name: "a statement that names no shadowed table",
			sql:  "select login from people where login = $1",
			want: "select login from people where login = $1",
		},
		{
			name: "its own WITH list that names no shadowed table",
			sql:  "with x as (select 1 as n) select n from x",
			want: "with x as (select 1 as n) select n from x",
		},
		{
			name: "table names inside a string literal, a dollar-quoted body and comments",
			sql:  "-- events\nselect 'issues' as a, E'asks' as b, $q$ comments $q$ as c /* messages */ from people",
			want: "-- events\nselect 'issues' as a, E'asks' as b, $q$ comments $q$ as c /* messages */ from people",
		},
		{
			// PostgreSQL goes on with an E'...' string across a newline and a second quote, and
			// reads the second part with escapes too: the literal is x' and events is a table.
			name: "an E string that goes on after a newline",
			sql:  "select e'x'\n'\\'' as a from events where 'y' = 'y'",
			want: "with " + asof.Shadows(cut, "events") + "\nselect e'x'\n'\\'' as a from events where 'y' = 'y'",
		},
		{
			name: "a -- comment that a carriage return ends",
			sql:  "select 1 -- n\rfrom events",
			want: "with " + asof.Shadows(cut, "events") + "\nselect 1 -- n\rfrom events",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, err := asof.Bind(row.sql, cut)
			if err != nil {
				t.Fatalf("Bind(%q): %v", row.sql, err)
			}
			if got != row.want {
				t.Errorf("Bind(%q)\n got: %q\nwant: %q", row.sql, got, row.want)
			}
		})
	}

	t.Run("a write that names no shadowed table is refused", func(t *testing.T) {
		const sql = "insert into people (login) values ('x')"
		bound, err := asof.Bind(sql, cut)
		var refused *asof.RefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("Bind(%q) = %q, %v; want an *asof.RefusedError", sql, bound, err)
		}
	})
}

// The meaning search sets its statement's bounds with set local and wraps a read in a
// savepoint. Those statements read and set no table, so they reach PostgreSQL as written: a
// WITH list in front of them is a syntax error.
func TestBindPassesStatementsThatReadNoTable(t *testing.T) {
	cut := mustCut(t, "2026-11-10T00:00:00Z")
	for _, sql := range []string{
		"set local statement_timeout = '15s'",
		"SET LOCAL hnsw.ef_search = 400",
		"show statement_timeout",
		"savepoint sp_1",
		"release savepoint sp_1",
		"rollback to savepoint sp_1",
	} {
		t.Run(sql, func(t *testing.T) {
			bound, err := asof.Bind(sql, cut)
			if err != nil {
				t.Fatalf("Bind(%q): %v, want it passed", sql, err)
			}
			if bound != sql {
				t.Errorf("Bind(%q) = %q, want the statement unchanged", sql, bound)
			}
		})
	}
}

// The escapes of a U&"..." identifier can spell the name of any table, which Bind does not read
// from them, so Bind refuses a statement that holds one rather than send it unbound. A U&'...'
// string is a literal, which names no table, and passes.
func TestBindRefusesAUnicodeEscapedIdentifier(t *testing.T) {
	cut := mustCut(t, "2026-11-10T00:00:00Z")
	for _, sql := range []string{`select * from U&"\0065vents"`, `select * from people join u&"asks" on true`} {
		t.Run(sql, func(t *testing.T) {
			bound, err := asof.Bind(sql, cut)
			var refused *asof.RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Bind(%q) = %q, %v; want an *asof.RefusedError", sql, bound, err)
			}
			if refused.Statement != sql {
				t.Errorf("Bind(%q) refusal names %q, want the statement", sql, refused.Statement)
			}
		})
	}

	const literal = `select U&'\0065vents' as a from people`
	if bound, err := asof.Bind(literal, cut); err != nil || bound != literal {
		t.Errorf("Bind(%q) = %q, %v; want the statement unchanged", literal, bound, err)
	}
}
