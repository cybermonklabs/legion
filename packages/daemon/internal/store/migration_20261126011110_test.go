package store

import (
	"context"
	"reflect"
	"testing"
)

// memoryKillsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const memoryKillsVersion = 20261126011110

// memoryKillsRelease names the release whose way back the migration's header carries, as a failure
// to read that block names it.
const memoryKillsRelease = "the release that keeps the memory kills of the issue pods for the kills signal"

// 20261126011110 adds memory_kills, one row per memory kill of a project's issue pods, and
// signal_inputs, the moment the daemon began keeping each input of a signal for a project, and
// changes no table that the release before reads. A kill is one row per project, guard and
// sequence, and a row with no guard, a sequence below 1 or a cause the wire does not spell is
// refused; an input is one row per project and input. Its way back, the last paragraph of the
// migration's header (migrations.WayBack), drops both tables and deletes the migration's version
// row, so the roll-forward's Migrate applies the file again and the first boot after it records
// its moment anew. The negative control is the store before the migration, which holds neither
// table.
func TestTheMemoryKillsArriveWith20261126011110AndTheirWayBackLetsThemComeAgain(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, memoryKillsVersion-1)
	exec := func(statement string, args ...any) error {
		t.Helper()
		_, err := store.Pool().Exec(ctx, statement, args...)
		return err
	}
	const tables = `select count(*)::text from information_schema.tables where table_name in ('memory_kills', 'signal_inputs')`
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the tables before the migration = %v, want neither", got)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the memory kills: %v", err)
	}
	const kill = `insert into memory_kills (project, guard, sequence, at, cause, no_role) values ($1, $2, $3, '2026-11-26T01:00:00Z', $4, $5)`
	for _, kept := range []struct {
		project, guard string
		sequence       int64
		cause          string
		noRole         bool
	}{
		{"LEGION", "guard-7f3a", 1, "", false},
		{"LEGION", "guard-7f3a", 2, "kernel", true},
		{"LEGION", "guard-9b1c", 1, "kernel-protected", false},
		{"ACME", "guard-7f3a", 1, "", true},
	} {
		if err := exec(kill, kept.project, kept.guard, kept.sequence, kept.cause, kept.noRole); err != nil {
			t.Fatalf("keep %+v: %v", kept, err)
		}
	}
	for _, refused := range []struct {
		what     string
		guard    string
		sequence int64
		cause    string
	}{
		{"the same kill again", "guard-7f3a", 1, ""},
		{"a kill of no guard", "", 3, ""},
		{"a kill of sequence 0", "guard-7f3a", 0, ""},
		{"a kill of a cause the wire does not spell", "guard-7f3a", 3, "oom"},
	} {
		if err := exec(kill, "LEGION", refused.guard, refused.sequence, refused.cause, false); err == nil {
			t.Fatalf("%s was kept, want it refused", refused.what)
		}
	}
	const input = `insert into signal_inputs (project, input, kept_since) values ($1, $2, '2026-11-26T01:00:00Z')`
	for _, kept := range [][2]string{{"LEGION", "kills"}, {"ACME", "kills"}, {"LEGION", "queue"}} {
		if err := exec(input, kept[0], kept[1]); err != nil {
			t.Fatalf("record that %s keeps %s: %v", kept[0], kept[1], err)
		}
	}
	if err := exec(input, "LEGION", "kills"); err == nil {
		t.Fatal("a second moment for the kills of LEGION was recorded, want it refused")
	}
	const kept = `select row(project, guard, sequence, cause, no_role)::text from memory_kills order by project, guard, sequence`
	if got, want := rowsAsText(t, store, kept), []string{
		`(ACME,guard-7f3a,1,"",t)`, `(LEGION,guard-7f3a,1,"",f)`, `(LEGION,guard-7f3a,2,kernel,t)`, `(LEGION,guard-9b1c,1,kernel-protected,f)`,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the kept kills = %v, want %v", got, want)
	}

	if err := exec(wayBackOf(t, memoryKillsVersion, memoryKillsRelease)); err != nil {
		t.Fatalf("the way back: %v", err)
	}
	const version = `select count(*)::text from schema_version where version = 20261126011110`
	if got := append(rowsAsText(t, store, tables), rowsAsText(t, store, version)...); !reflect.DeepEqual(got, []string{"0", "0"}) {
		t.Fatalf("after the way back the tables and the version row = %v, want neither", got)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"2"}) {
		t.Fatalf("the tables after the roll-forward = %v, want both again", got)
	}
	const inputs = `select count(*)::text from signal_inputs`
	if got := append(rowsAsText(t, store, kept), rowsAsText(t, store, inputs)...); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("after the roll-forward the tables hold %v, want no kill and no moment: the first boot records its own", got)
	}
}
