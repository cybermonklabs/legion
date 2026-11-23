package eventidtest

import (
	"strings"
	"testing"
)

func TestAnalyzeReadsNestedDynamicSQL(t *testing.T) {
	for _, row := range []struct {
		name, sql string
		want      Finding
	}{
		{"constant execute is read", `do $$ begin execute 'select nextval(''events_id_seq'')'; end $$`, Finding{Kind: SequenceCall, Name: "nextval"}},
		{"constant execute without a draw is allowed", `do $$ begin execute 'select 1'; end $$`, Finding{}},
		{"for execute is dynamic", `do $$ declare query text := 'select 1'; record record; begin for record in execute query loop end loop; end $$`, Finding{Kind: DynamicSQL, Name: "execute"}},
		{"open execute is dynamic", `do $$ declare query text := 'select 1'; cursor refcursor; begin open cursor for execute query; end $$`, Finding{Kind: DynamicSQL, Name: "execute"}},
		{"return query execute is dynamic", `create function f() returns setof integer language plpgsql as $$ declare query text := 'select 1'; begin return query execute query; end $$`, Finding{Kind: DynamicSQL, Name: "execute"}},
		{"a static return query is no dynamic SQL", `create function f() returns setof integer language plpgsql as $$ begin return query select 1; end $$`, Finding{}},
		{"a static open is no dynamic SQL", `do $$ declare cursor refcursor; begin open cursor for select 1; end $$`, Finding{}},
		{"a static return query is still read", `create function f() returns setof bigint language plpgsql as $$ begin return query select nextval('events_id_seq'); end $$`, Finding{Kind: SequenceCall, Name: "nextval"}},
		{"a restart of the sequence", `alter sequence events_id_seq restart with 1`, Finding{Kind: SequenceAlter, Name: "alter sequence"}},
		{"a negative increment of the qualified sequence", `alter sequence public.events_id_seq increment by -1`, Finding{Kind: SequenceAlter, Name: "alter sequence"}},
		{"a restart inside a do block", `do $$ begin alter sequence events_id_seq restart; end $$`, Finding{Kind: SequenceAlter, Name: "alter sequence"}},
		{"a rename of the sequence", `alter sequence events_id_seq rename to spare_seq`, Finding{Kind: SequenceAlter, Name: "alter sequence"}},
		{"a move of the sequence", `alter sequence events_id_seq set schema spare`, Finding{Kind: SequenceAlter, Name: "alter sequence"}},
		{"an owner change of the sequence", `alter sequence events_id_seq owner to someone`, Finding{Kind: SequenceAlter, Name: "alter sequence"}},
		{"a new default for events.id", `alter table events alter column id set default 0`, Finding{Kind: SequenceAlter, Name: "alter events.id default"}},
		{"another sequence's restart", `alter sequence issues_n_seq restart`, Finding{}},
		{"ts_stat runs its constant query", `select ts_stat('select nextval(''events_id_seq'')::text::tsvector')`, Finding{Kind: SequenceCall, Name: "nextval"}},
		{"ts_stat of a built query is dynamic", `select * from ts_stat(format($2::text, $1::text))`, Finding{Kind: DynamicSQL, Name: "ts_stat"}},
		{"ts_stat of a harmless constant", `select * from ts_stat('select to_tsvector(''a'')')`, Finding{}},
		{"ts_rewrite runs its select", `select ts_rewrite('a'::tsquery, 'select nextval(''events_id_seq'')::text::tsquery, ''b''::tsquery')`, Finding{Kind: SequenceCall, Name: "nextval"}},
		{"ts_rewrite of three tsqueries runs nothing", `select ts_rewrite('a'::tsquery, 'a'::tsquery, 'b'::tsquery)`, Finding{}},
	} {
		t.Run(row.name, func(t *testing.T) {
			analysis, err := Analyze(row.sql)
			if err != nil {
				t.Fatal(err)
			}
			if row.want == (Finding{}) {
				if len(analysis.Findings) != 0 {
					t.Fatalf("findings = %#v, want none", analysis.Findings)
				}
				return
			}
			for _, found := range analysis.Findings {
				if found.Kind == row.want.Kind && found.Name == row.want.Name {
					return
				}
			}
			t.Fatalf("findings = %#v, want %#v", analysis.Findings, row.want)
		})
	}
}

func TestAnalyzeSeparatesTheSafeCurrvalCall(t *testing.T) {
	analysis, err := Analyze(`select pg_catalog.currval(pg_catalog.pg_get_serial_sequence('events', 'id')); select currval('other_seq')`)
	if err != nil {
		t.Fatal(err)
	}
	var safe, unsafe bool
	for _, finding := range analysis.Findings {
		if finding.Kind == SequenceCall && finding.Name == "currval" {
			if finding.Safe {
				safe = true
			} else {
				unsafe = true
			}
		}
	}
	if !safe || !unsafe {
		t.Fatalf("currval findings = %#v, want one safe and one unsafe call", analysis.Findings)
	}
}

// TestAnalyzeReadsWhichSequenceACallDraws: a nextval or setval of one plain literal naming
// another sequence draws no event id, and every other spelling of the sequence is unsafe.
func TestAnalyzeReadsWhichSequenceACallDraws(t *testing.T) {
	for _, row := range []struct {
		sql  string
		safe bool
	}{
		{`select nextval('other_seq')`, true},
		{`select pg_catalog.nextval(' "public" . "other_seq" ')`, true},
		{`select setval('other_seq', 1, false)`, true},
		{`select nextval('Issues_N_Seq')`, true},
		{`select nextval('events_id_seq')`, false},
		{`select nextval('EVENTS_ID_SEQ')`, false},
		{`select nextval('"events_id_seq"')`, false},
		{`select nextval('public.events_id_seq')`, false},
		{`select setval(' public . "events_id_seq" ', 1)`, false},
		{`select nextval('16402')`, false},
		{`select nextval(E'events\137id\137seq')`, false},
		{`select nextval('events\137id\137seq')`, false},
		{`select nextval('other_seq'::regclass)`, false},
		{`select nextval(16402)`, false},
		{`select nextval($1)`, false},
		{`select util.nextval('other_seq')`, false},
		{`select "PG_CATALOG".nextval('other_seq')`, false},
		{`select nextval('other' || '_seq')`, false},
		{`select nextval('a.b.other_seq')`, false},
	} {
		analysis, err := Analyze(row.sql)
		if err != nil {
			t.Errorf("%s: %v", row.sql, err)
			continue
		}
		var calls []Finding
		for _, finding := range analysis.Findings {
			if finding.Kind == SequenceCall {
				calls = append(calls, finding)
			}
		}
		if len(calls) != 1 || calls[0].Safe != row.safe {
			t.Errorf("%s: sequence calls %#v, want one with Safe %t", row.sql, calls, row.safe)
		}
	}
}

// TestAnalyzeNamesTheStatementThatHoldsANestedFinding: a finding in a routine body is named by the
// statement that defines the routine, with its offset, never by the body's own text or by the
// guard's wrapping of an expression; a call in a statement's own text is placed at the call.
func TestAnalyzeNamesTheStatementThatHoldsANestedFinding(t *testing.T) {
	const sql = "select 1;\n\ncreate function f() returns bigint language plpgsql as $$ begin return nextval('events_id_seq'); end $$;\nselect\n  nextval('events_id_seq')"
	analysis, err := Analyze(sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.Findings) != 2 {
		t.Fatalf("findings = %#v, want the routine's and the select's", analysis.Findings)
	}
	routine, call := analysis.Findings[0], analysis.Findings[1]
	if !strings.HasPrefix(routine.Statement, "create function f()") || routine.Location != strings.Index(sql, "create function") {
		t.Errorf("the routine's finding is %#v, want the create function statement at %d", routine, strings.Index(sql, "create function"))
	}
	if call.Location != strings.LastIndex(sql, "nextval") {
		t.Errorf("the select's finding is at %d, want its call at %d", call.Location, strings.LastIndex(sql, "nextval"))
	}
}

func TestAnalyzeReadsDerivedForeignEventTable(t *testing.T) {
	analysis, err := Analyze(`create foreign table event_children () inherits (events) server foreign_server`)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range analysis.Findings {
		if finding.Kind == DerivedEventTable {
			return
		}
	}
	t.Fatalf("findings = %#v, want a derived event table", analysis.Findings)
}
