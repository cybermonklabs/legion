package store

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/eventidtest"
)

// migrationEventIDOffences reads a whole migration with PostgreSQL's parser. Each offence names the
// migration and the line of the call, or of the statement that holds it (a routine's body, a
// constant EXECUTE), with that statement's text. A sequence call that draws no event id
// (eventidtest.Finding.Safe: store.LatestEventID's pg_catalog.currval read, a nextval or setval of
// one plain literal naming another sequence) is no offence, as it is none in the Go guard.
func migrationEventIDOffences(path, sql string) []string {
	analysis, err := eventidtest.Analyze(sql)
	if err != nil {
		return []string{path + ": cannot parse SQL: " + err.Error()}
	}
	var offences []string
	for _, finding := range analysis.Findings {
		at := fmt.Sprintf("%s:%d: ", path, 1+strings.Count(sql[:min(max(finding.Location, 0), len(sql))], "\n"))
		switch finding.Kind {
		case eventidtest.SequenceCall:
			if finding.Safe {
				continue
			}
			offences = append(offences, at+"calls "+finding.Name+" outside the broker's event insert path: "+finding.Statement)
		case eventidtest.SequenceAlter:
			offences = append(offences, at+finding.Name+" of the events id sequence or default: "+finding.Statement)
		case eventidtest.DynamicSQL:
			offences = append(offences, at+"runs SQL a static guard cannot read ("+finding.Name+"): "+finding.Statement)
		}
	}
	return offences
}

// TestNoMigrationDrawsAnEventIDOutsideTheInsertDefault is the migrations' half of
// api.TestOnlyAnEventInsertsDefaultDrawsAnEventID. A migration can define a function, view or
// trigger that draws an id before an event writer holds RowExclusiveLock on events, so every
// migration is parsed whole and every nested statement is read.
func TestNoMigrationDrawsAnEventIDOutsideTheInsertDefault(t *testing.T) {
	t.Run("the parser reads every wrapped and routine statement", func(t *testing.T) {
		for _, row := range []struct {
			name, sql string
			want      int
		}{
			{"explain", `explain analyze select query_to_xml('select next' || 'val(16402)', false, false, '')`, 1},
			{"set then select", `set local statement_timeout = '1s'; select query_to_xml('select next' || 'val(16402)', false, false, '')`, 1},
			{"values", `values (query_to_xml('select next' || 'val(16402)', false, false, ''))`, 1},
			{"copy query", `copy (select query_to_xml('select next' || 'val(16402)', false, false, '')) to stdout`, 1},
			{"transaction block", `begin; do $$ declare query text := 'select 1'; begin execute query; end $$; commit`, 1},
			{"dynamic execute", `do $$ begin execute 'select nextval(' || 16402 || ')'; end $$`, 1},
			{"standard strings off", `set local standard_conforming_strings = off; select nextval('events\137id\137seq')`, 1},
			{"XML query", `select query_to_xml('select next' || 'val(16402)', false, false, '')`, 1},
			{"XML schema query", `select query_to_xmlschema('select 1', false, false, '', '')`, 1},
			{"cursor query", `select cursor_to_xml('cursor', 1, false, false, '')`, 1},
			{"cursor schema query", `select cursor_to_xmlschema('cursor', 1, false, false, '', '')`, 1},
			{"SQL function", `create function f() returns bigint language sql as $$ select nextval('events_id_seq') $$`, 1},
			{"PL function", `create function f() returns void language plpgsql as $$ begin execute 'select nextval(''events_id_seq'')'; end $$`, 1},
			{"prepared execute", `prepare read_events as select 1; execute read_events`, 0},
			{"trigger execute", `create trigger events_before before insert on events for each row execute function f()`, 0},
			{"the latest event id read", `select pg_catalog.currval(pg_catalog.pg_get_serial_sequence('events', 'id'))`, 0},
			{"another sequence by literal", `select nextval('other_seq'); select setval('other_seq', 1)`, 0},
			{"another table's sequence default", `create table others (id bigint primary key default nextval('others_id_seq'))`, 0},
			{"a bare currval", `select currval('events_id_seq')`, 1},
			{"a restart of the sequence", `alter sequence events_id_seq restart with 1;`, 1},
			{"a negative increment", `alter sequence public.events_id_seq increment by -1;`, 1},
			{"ts_stat of a draw", `select ts_stat('select nextval(''events_id_seq'')::text::tsvector');`, 1},
			{"a static return query", `create function f() returns setof integer language plpgsql as $$ begin return query select 1; end $$`, 0},
			{"a static open", `create function f() returns void language plpgsql as $$ declare c refcursor; begin open c for select 1; end $$`, 0},
		} {
			if got := migrationEventIDOffences("m.sql", row.sql); len(got) != row.want {
				t.Errorf("%s: %d offences %q, want %d", row.name, len(got), got, row.want)
			}
		}
	})

	t.Run("an offence names the migration's line and its own statement", func(t *testing.T) {
		const sql = "-- a header\nselect 1;\n\ncreate function f() returns void language plpgsql as $$\nbegin\n  perform nextval('events_id_seq');\nend $$;\n"
		got := migrationEventIDOffences("m.sql", sql)
		if len(got) != 1 || !strings.HasPrefix(got[0], "m.sql:4: calls nextval") || strings.Contains(got[0], "event-id guard") {
			t.Fatalf("offences = %q, want one at m.sql:4 naming the create function statement, with none of the guard's own wrapping", got)
		}
	})

	var offences []string
	read := 0
	if err := fs.WalkDir(migrationFiles, "migrations", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".sql") {
			return err
		}
		data, err := migrationFiles.ReadFile(path)
		if err != nil {
			return err
		}
		read++
		offences = append(offences, migrationEventIDOffences(path, string(data))...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if read == 0 {
		t.Fatal("read no migration: this guard would pass by seeing nothing")
	}
	if len(offences) > 0 {
		t.Fatalf("a migration draws an event id or runs unreadable SQL:\n\t%s\n"+
			"Let the insert's default draw every event id (events.Broker.Append): the safe horizon dates a "+
			"writer by the RowExclusiveLock its insert takes before the default draws (events/horizon.go), "+
			"and an id drawn any other way is held where the lock table cannot show it.", strings.Join(offences, "\n\t"))
	}
	t.Logf("read %d migration files", read)
}
