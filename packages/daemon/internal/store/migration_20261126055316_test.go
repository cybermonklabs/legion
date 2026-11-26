package store

import (
	"context"
	"reflect"
	"testing"
)

// signalDeadlinesVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const signalDeadlinesVersion = 20261126055316

// signalDeadlinesRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const signalDeadlinesRelease = "the release that keeps each project's one deadline of admission by signals"

// 20261126055316 adds signal_deadlines, each project's one deadline with the fence that names each
// of its firings. Its way back drops the table, deletes this version row and deletes the marks of
// the firings the daemon applied (source 'signals', `signal-deadline:<project>:<fence>`), and no
// other mark: the roll-forward starts each project's fence again from 1, so a firing of a fence that
// fired before the rollback must apply as the new fact it is, not be taken for a duplicate.
func TestTheSignalDeadlinesArriveWith20261126055316AndTheirWayBackLetsTheirFencesFireAgain(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, signalDeadlinesVersion-1)
	exec := func(statement string, args ...any) error {
		_, err := store.Pool().Exec(ctx, statement, args...)
		return err
	}
	const tables = `select count(*)::text from information_schema.tables where table_name = 'signal_deadlines'`
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the signal deadlines: %v", err)
	}
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"1"}) {
		t.Fatalf("signal_deadlines after the migration = %v, want the table", got)
	}
	for _, statement := range []string{
		`insert into signal_deadlines (project, due, fence) values ('LEGION', '2026-11-26T11:00:00Z', 3)`,
		`insert into processed_events (source, event_id) values ('signals', 'signal-deadline:LEGION:1'), ('signals', 'signal-deadline:LEGION:2'),
			('signals', 'signal-deadline:ACME:1'), ('daemon', 'boot:LEGION:1'), ('api', 'signal-deadline:LEGION:1')`,
	} {
		if err := exec(statement); err != nil {
			t.Fatalf("seed the deadline and its firings (%s): %v", statement, err)
		}
	}

	if err := exec(wayBackOf(t, signalDeadlinesVersion, signalDeadlinesRelease)); err != nil {
		t.Fatalf("the way back: %v", err)
	}
	const version = `select count(*)::text from schema_version where version = 20261126055316`
	if got := append(rowsAsText(t, store, tables), rowsAsText(t, store, version)...); !reflect.DeepEqual(got, []string{"0", "0"}) {
		t.Fatalf("after the way back the table and the version row = %v, want neither", got)
	}
	marks := `select source || ' ' || event_id from processed_events order by source, event_id`
	if got, want := rowsAsText(t, store, marks), []string{"api signal-deadline:LEGION:1", "daemon boot:LEGION:1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the way back processed_events holds %v, want %v: every firing's mark goes and no other", got, want)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"1"}) {
		t.Fatalf("signal_deadlines after the roll-forward = %v, want the table again", got)
	}
}
