package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// phaseVerdictGoneVersion is the version of the migration this file tests.
const phaseVerdictGoneVersion = 20261125000602

// 20261125000602 keeps phases.verdict, which the release before reads and writes by name, gives it
// the empty string for a default, which each row this release inserts takes, and adds
// phases.states_serial at 0 on every row the release before wrote. The way back its header gives
// (migrations.WayBack), run as an operator runs it through psql, empties every verdict, drops
// states_serial, deletes the keys head, waits_for, waiting and failing from each queued notice,
// which the release before has no field for, keeping the rest of what the notice says, and deletes
// the migration's own row; the roll-forward's Migrate then applies the migration again, and only it.
func TestThePhaseVerdictGoneMigrationAndItsWayBack(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, phaseVerdictGoneVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("run %q: %v", statement, err)
		}
	}
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('ACME-1', 'ACME-1', 'ACME', 'root', 'testing', 1, 'testing', 'A', 3)`)
	// The release before's write of a tester's row names the verdict.
	exec(`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, summary)
		values ('ACME-1', 'tester', 'tester-claim', 'head', 0, 'pass', 'tested')`)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the phase verdict's removal: %v", err)
	}
	const columns = `select column_name::text from information_schema.columns where table_schema = current_schema()
		and table_name = 'phases' and column_name in ('verdict', 'states_serial') order by column_name`
	if got, want := rowsAsText(t, store, columns), []string{"states_serial", "verdict"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the phases columns after the migration = %v, want %v: verdict kept for the release before", got, want)
	}
	if got, want := rowsAsText(t, store, `select row(role, verdict, states_serial, summary)::text from phases`), []string{"(tester,pass,0,tested)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the tester's row after the migration = %v, want %v: kept as the release before wrote it, at serial 0", got, want)
	}
	// A row this release inserts names no verdict and takes the default, which the release before
	// reads as no verdict.
	exec(`insert into phases (issue, role, claim, handoff_commit, rounds, summary) values ('ACME-1', 'reviewer', 'reviewer-claim', '', 0, '')`)
	if got, want := rowsAsText(t, store, `select row(role, verdict)::text from phases where role = 'reviewer'`), []string{"(reviewer,\"\")"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the reviewer's row this release inserted = %v, want %v", got, want)
	}
	// This release's tester's notices of a move back and of a wait carry the testing move's head,
	// wait and checks as fields, and a notice of another kind carries none of them.
	exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error) values
		('notice', 'ACME-1', '{"kind": "phase-finished", "role": "tester", "phase": "testing", "reason": "k2 does not pass", "head": "h1", "failing": [{"check": "k2", "state": "no", "reason": "fail"}]}', 0, now(), ''),
		('notice', 'ACME-1', '{"kind": "phase-finished", "role": "tester", "phase": "testing", "reason": "the move waits for k1", "head": "h1", "waits_for": "checks", "waiting": ["k1"]}', 0, now(), ''),
		('notice', 'ACME-1', '{"kind": "held", "role": "tester", "reason": "escalated"}', 0, now(), '')`)
	const notices = `select payload::text from outbox where kind = 'notice' order by id`

	wayBack, err := migrations.WayBack(phaseVerdictGoneVersion)
	if err != nil {
		t.Fatalf("read the way back of %d: %v", phaseVerdictGoneVersion, err)
	}
	exec(wayBack)
	if got, want := rowsAsText(t, store, columns), []string{"verdict"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the phases columns after the way back = %v, want %v", got, want)
	}
	if got, want := rowsAsText(t, store, `select row(role, verdict)::text from phases order by role`), []string{"(reviewer,\"\")", "(tester,\"\")"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the phase rows after the way back = %v, want %v: every verdict emptied", got, want)
	}
	if got, want := rowsAsText(t, store, notices), []string{
		`{"kind": "phase-finished", "role": "tester", "phase": "testing", "reason": "k2 does not pass"}`,
		`{"kind": "phase-finished", "role": "tester", "phase": "testing", "reason": "the move waits for k1"}`,
		`{"kind": "held", "role": "tester", "reason": "escalated"}`,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the queued notices after the way back = %v, want %v: head, waits_for, waiting and failing deleted, the rest kept", got, want)
	}
	// The release before's write after the way back takes, naming the verdict.
	exec(`update phases set verdict = 'fail' where issue = 'ACME-1' and role = 'tester'`)
	if got, want := rowsAsText(t, store, `select count(*)::text from schema_version where version = 20261125000602`), []string{"0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the migration's rows after the way back = %v, want %v", got, want)
	}

	applied, err := store.Migrate(ctx)
	if err != nil || applied != 1 {
		t.Fatalf("the roll-forward's migrate applied %d, %v; want the phase verdict's removal alone", applied, err)
	}
	if got, want := rowsAsText(t, store, columns), []string{"states_serial", "verdict"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the phases columns after the roll-forward = %v, want %v", got, want)
	}
}
