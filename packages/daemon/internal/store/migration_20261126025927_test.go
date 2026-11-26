package store

import (
	"context"
	"reflect"
	"testing"
)

// approvalAsksVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const approvalAsksVersion = 20261126025927

// approvalAsksRelease names the release whose way back the migration's header carries, as a failure
// to read that block names it.
const approvalAsksRelease = "the release that keeps each ask, issue assignee and default assignee of a project for the approvals signal"

// 20261126025927 adds approval_asks, approval_assignees and approval_default_assignees, which the
// approvals signal reads. Each is keyed by project, refuses an empty project, a sequence below its
// least and a default assignee that names nobody. Its way back drops the three tables and the
// approvals row of signal_inputs, leaves every other input's row and deletes this version row, so
// a roll-forward runs the file again.
func TestTheApprovalAsksArriveWith20261126025927AndTheirWayBackLetsThemComeAgain(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, approvalAsksVersion-1)
	exec := func(statement string, args ...any) error {
		_, err := store.Pool().Exec(ctx, statement, args...)
		return err
	}
	const tables = `select count(*)::text from information_schema.tables where table_name in ('approval_asks', 'approval_assignees', 'approval_default_assignees')`
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the approvals tables before the migration = %v, want none", got)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the approval asks: %v", err)
	}
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"3"}) {
		t.Fatalf("the approvals tables after the migration = %v, want the three", got)
	}
	for _, valid := range []struct{ statement string }{
		{`insert into approval_asks (project, ask, issue, open, human, asked_of, seq) values ('LEGION', 'ask-1', 'LEGION-1', true, true, '', 1)`},
		{`insert into approval_asks (project, ask, issue, open, human, asked_of, seq) values ('ACME', 'ask-1', 'ACME-1', false, false, 'dana', 7)`},
		{`insert into approval_assignees (project, issue, assignee, seq) values ('LEGION', 'LEGION-1', '', 1)`},
		{`insert into approval_default_assignees (project, assignee, seq, at) values ('LEGION', 'dana', 0, '2026-11-26T09:00:00Z')`},
	} {
		if err := exec(valid.statement); err != nil {
			t.Fatalf("insert a valid row (%s): %v", valid.statement, err)
		}
	}
	for name, statement := range map[string]string{
		"an ask kept twice in one project":          `insert into approval_asks (project, ask, issue, open, human, asked_of, seq) values ('LEGION', 'ask-1', 'LEGION-1', true, true, '', 2)`,
		"an ask of no project":                      `insert into approval_asks (project, ask, issue, open, human, asked_of, seq) values ('', 'ask-2', 'LEGION-1', true, true, '', 1)`,
		"an ask of no issue":                        `insert into approval_asks (project, ask, issue, open, human, asked_of, seq) values ('LEGION', 'ask-2', '', true, true, '', 1)`,
		"an ask at no sequence":                     `insert into approval_asks (project, ask, issue, open, human, asked_of, seq) values ('LEGION', 'ask-2', 'LEGION-1', true, true, '', 0)`,
		"an assignee at no sequence":                `insert into approval_assignees (project, issue, assignee, seq) values ('LEGION', 'LEGION-2', 'dana', 0)`,
		"an assignee of an issue kept twice":        `insert into approval_assignees (project, issue, assignee, seq) values ('LEGION', 'LEGION-1', 'dana', 2)`,
		"a default assignee naming nobody":          `insert into approval_default_assignees (project, assignee, seq, at) values ('ACME', '', 0, '2026-11-26T09:00:00Z')`,
		"a second default assignee of a project":    `insert into approval_default_assignees (project, assignee, seq, at) values ('LEGION', 'erin', 1, '2026-11-26T09:00:00Z')`,
		"a default assignee at a negative sequence": `insert into approval_default_assignees (project, assignee, seq, at) values ('ACME', 'dana', -1, '2026-11-26T09:00:00Z')`,
	} {
		if err := exec(statement); err == nil {
			t.Errorf("%s was accepted, want it refused", name)
		}
	}
	if err := exec(`insert into signal_inputs (project, input, kept_since) values ('LEGION', 'approvals', '2026-11-26T09:00:00Z'), ('LEGION', 'kills', '2026-11-26T09:00:00Z')`); err != nil {
		t.Fatalf("insert the signal inputs: %v", err)
	}

	if err := exec(wayBackOf(t, approvalAsksVersion, approvalAsksRelease)); err != nil {
		t.Fatalf("the way back: %v", err)
	}
	const version = `select count(*)::text from schema_version where version = 20261126025927`
	if got := append(rowsAsText(t, store, tables), rowsAsText(t, store, version)...); !reflect.DeepEqual(got, []string{"0", "0"}) {
		t.Fatalf("after the way back the approvals tables and the version row = %v, want neither", got)
	}
	if got, want := rowsAsText(t, store, `select input from signal_inputs where project = 'LEGION' order by input`), []string{"kills"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the way back the signal inputs = %v, want %v: the approvals row goes with its tables and no other input's does", got, want)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"3"}) {
		t.Fatalf("the approvals tables after the roll-forward = %v, want the three again", got)
	}
}
