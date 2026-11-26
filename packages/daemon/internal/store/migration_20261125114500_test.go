package store

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// phaseStepKeyVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const phaseStepKeyVersion = 20261125114500

// phaseStepKeyRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const phaseStepKeyRelease = "the release that keys a phases row by its step"

// 20261125114500 keys phases by the issue, the role and the step the row runs, so a role that
// stands at two steps of a task keeps a row for each. Every row the release before left keeps its
// step, and an issue's rows with no step keep one row for each role. The release before writes a
// row with an upsert whose conflict target is (issue, role), which no unique index of exactly those
// columns answers after the migration, so that write fails until the way back writes that key
// again: the cost the header names, which is why every daemon on the database moves together.
//
// The way back, the last paragraph of the migration's header (migrations.WayBack), keeps the row
// of each role with the highest entry, writes the key (issue, role) again, so the release before's
// upsert lands, and deletes the version row, so the roll forward runs the migration again.
func TestAPhasesRowIsTheRowOfAStepAndTheWayBackFoldsARolesRows(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, phaseStepKeyVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// The release before's one write of a phases row, with the columns this test reads (record's
	// PutPhase names every column, and the same conflict target).
	const releaseBeforesUpsert = `insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, step_task, step_place) values `
	const releaseBeforesTarget = ` on conflict (issue, role) do update set claim = excluded.claim, entry = excluded.entry`
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'a plan', 'reviewing', 1, 'in_progress', 'A', 1),
		('LEGION-209', 'LEGION-209', 'LEGION', 'no plan', 'testing', 1, 'in_progress', 'B', 1)`)
	exec(releaseBeforesUpsert + `('LEGION-208', 'implementer', 'a', 'head', 0, '', 1, 't', 1), ('LEGION-208', 'reviewer', 'r', '', 0, '', 2, 't', 2),
		('LEGION-209', 'tester', 'te', '', 0, '', 1, '', 0)` + releaseBeforesTarget)
	if _, err := store.Pool().Exec(ctx, releaseBeforesUpsert+`('LEGION-208', 'implementer', 'a', '', 0, '', 3, 't', 3)`); err == nil {
		t.Fatal("a second row of the implementer was taken before the migration, want the key (issue, role) to refuse it: the fixture is not the old shape")
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the phase step key: %v", err)
	}

	const key = `select string_agg(a.attname, ',' order by k.n) from pg_index i
		cross join lateral unnest(i.indkey) with ordinality k(attnum, n)
		join pg_attribute a on a.attrelid = i.indrelid and a.attnum = k.attnum
		where i.indrelid = 'phases'::regclass and i.indisprimary`
	if got, want := rowsAsText(t, store, key), []string{"issue,role,step_task,step_place"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the key of phases after the migration = %v, want %v", got, want)
	}
	const rows = `select row(issue, role, claim, entry, step_task, step_place)::text from phases order by issue, role, step_task, step_place`
	before := []string{"(LEGION-208,implementer,a,1,t,1)", "(LEGION-208,reviewer,r,2,t,2)", "(LEGION-209,tester,te,1,\"\",0)"}
	if got := rowsAsText(t, store, rows); !reflect.DeepEqual(got, before) {
		t.Fatalf("the rows after the migration = %v, want each as the release before left it: %v", got, before)
	}
	// The row of the implementer's second step is a row of its own.
	exec(`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, step_task, step_place)
		values ('LEGION-208', 'implementer', 'a', '', 0, '', 3, 't', 3)`)
	_, err := store.Pool().Exec(ctx, releaseBeforesUpsert+`('LEGION-209', 'tester', 'te', '', 0, '', 2, '', 0)`+releaseBeforesTarget)
	if err == nil || !strings.Contains(err.Error(), "no unique or exclusion constraint matching the ON CONFLICT specification") {
		t.Fatalf("the release before's upsert over the new key = %v, want it refused for its conflict target", err)
	}
	t.Logf("the release before's upsert over the new key is refused: %v", err)

	exec(wayBackOf(t, phaseStepKeyVersion, phaseStepKeyRelease))
	if got, want := rowsAsText(t, store, key), []string{"issue,role"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the key of phases after the way back = %v, want %v", got, want)
	}
	folded := []string{"(LEGION-208,implementer,a,3,t,3)", "(LEGION-208,reviewer,r,2,t,2)", "(LEGION-209,tester,te,1,\"\",0)"}
	if got := rowsAsText(t, store, rows); !reflect.DeepEqual(got, folded) {
		t.Fatalf("the rows after the way back = %v, want the implementer's row of the highest entry alone: %v", got, folded)
	}
	exec(releaseBeforesUpsert + `('LEGION-209', 'tester', 'te', '', 0, '', 2, '', 0)` + releaseBeforesTarget)
	if got := rowsAsText(t, store, `select entry::text from phases where issue = 'LEGION-209'`); !reflect.DeepEqual(got, []string{"2"}) {
		t.Fatalf("the tester's entry after the release before's upsert = %v, want 2", got)
	}
	if got := rowsAsText(t, store, `select count(*)::text from schema_version where version = 20261125114500`); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the version rows of the migration after the way back = %v, want none", got)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward: %v", err)
	}
	if got, want := rowsAsText(t, store, key), []string{"issue,role,step_task,step_place"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the key of phases after the roll forward = %v, want %v", got, want)
	}
}

// The witness the way back of the release that records each step keeps (step_run_witness,
// 20261117004117) reads one phases row of each role: the implementer's pass in a scalar subquery,
// and each role's entry as one key of an object. A role at two steps of a task has a row for each
// under the key by step, so that block fails on such an issue while the key stands, and a rollback
// past both releases runs this migration's way back first, which keeps the row of each role with
// the highest entry (docs/kubernetes.md); the witness then reads that row.
func TestTheStepRecordsWayBackRunsOnceTheStepKeysWayBackFoldedARolesRows(t *testing.T) {
	s := newRollBackStore(t)
	s.issue("LEGION-208", "LEGION-208", "implementing", "", 1)
	s.exec(`update phases set entry = 1, step_task = 't', step_place = 1 where issue = 'LEGION-208' and role = 'implementer'`)
	s.exec(`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, pass, entry, step_task, step_place)
		values ('LEGION-208', 'implementer', '', '', 0, '', 1, 3, 't', 3)`)
	s.row("LEGION-208", "step", 2, "open")

	stepRecords := wayBackOf(t, migrations.StepRecordsRollForward, stepRecordsRelease)
	_, err := s.store.Pool().Exec(t.Context(), stepRecords)
	if err == nil || !strings.Contains(err.Error(), "more than one row returned by a subquery used as an expression") {
		t.Fatalf("the step records' way back over a role's two rows = %v, want it refused for the witness's one row of each role", err)
	}
	t.Logf("the step records' way back before the step key's: %v", err)
	if got := rowsAsText(t, s.store, `select count(*)::text from step_starts_rollback`); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the refused way back kept %v witnesses, want none", got)
	}

	s.exec(wayBackOf(t, phaseStepKeyVersion, phaseStepKeyRelease))
	s.exec(stepRecords)
	if got := rowsAsText(t, s.store, `select concat_ws(' ', witness->>'pass', witness->'entries'->>'implementer') from step_starts_rollback
		where issue = 'LEGION-208'`); !reflect.DeepEqual(got, []string{"1 3"}) {
		t.Fatalf("the witness after the step key's way back = %v, want the pass and the entry of the row it kept, %q", got, "1 3")
	}
}
