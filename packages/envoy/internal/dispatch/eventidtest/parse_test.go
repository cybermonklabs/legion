package eventidtest

import "testing"

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
