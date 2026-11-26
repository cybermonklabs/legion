package store

import (
	"context"
	"strings"
	"testing"
)

// projectReleaseVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const projectReleaseVersion = 20261115073241

// The project release's migration adds its record (project_releases) and the source its stops
// write (claims.suspended_by `project_release`), on a database holding the rows a deployment has
// today: a claim of every source the check took before, the controller's own (main's
// 20261115060739) among them, an idle release in flight with its rows, and a queued delivery.
// Before it, a claim suspended by the project release is refused and the record has no table,
// which is the negative control read first. After it, every row it found stands as it was, the new
// source is taken and a source that is none is still refused, and a record written with its three
// required columns reads back unfinished, unended and with an empty result; a boot is the one
// thing its end may name.
func TestTheProjectReleaseMigrationAppliesOverTodaysRows(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, projectReleaseVersion-1)
	// claimOf seeds the claim of role on issue, a member of LEGION-208's tree, in state and suspended
	// by by, under the token the daemon gives it.
	claimOf := func(issue, role, state, by string) error {
		_, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
			values ($1, 'legion', 'LEGION-208', $2, $3, 1, $4, '', $5, 0, 0, 0, 0, $6)`,
			"legion-legion-"+strings.ToLower(issue)+"-"+role, issue, role, "ses-"+role, state, by)
		return err
	}
	today := []struct{ role, state, by string }{
		{"architect", "working", ""},
		{"planner", "suspended", "unknown"},
		{"implementer", "suspended", "operator"},
		{"tester", "suspended", "daemon"},
		{"reviewer", "suspended", "exit"},
		{"merger", "suspended", "released"},
	}
	for _, c := range today {
		if err := claimOf("LEGION-208", c.role, c.state, c.by); err != nil {
			t.Fatalf("seed the %s suspended by %q as today's daemon writes it: %v", c.role, c.by, err)
		}
	}
	// The controller's own suspension, which main's 20261115060739 took before this migration: a
	// list here that left it out would refuse this row and the upgrade with it.
	if err := claimOf("LEGION-210", "architect", "suspended", "controller"); err != nil {
		t.Fatalf("seed an architect the controller suspended, as main's daemon writes it: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('issue_bundle', 'LEGION-208', '{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"merger"}', 0, now(), ''),
		('supervise', 'LEGION-208', '{"op":"release","tree":"LEGION-208","role":"merger","generation":1}', 0, now(), ''),
		('issue_release', 'LEGION-208', '{"tree":"LEGION-208","generation":1,"treeGeneration":1}', 0, now(), ''),
		('notice', 'LEGION-208', '{}', 0, now(), '')`); err != nil {
		t.Fatalf("queue the rows of an idle release in flight and a notice: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into issue_releases
		(project, issue, generation, first_row, boundary_row, last_row, started_row, role, suspended, kind, marked_at)
		select 'legion', 'LEGION-208', 1, min(id), min(id) + 1, min(id) + 2, 0, 'merger', '{merger}', 'idle', now() from outbox`); err != nil {
		t.Fatalf("mark the idle release in flight: %v", err)
	}

	err := store.Pool().QueryRow(ctx, `select count(*) from project_releases`).Scan(new(int))
	if err == nil || !strings.Contains(err.Error(), "project_releases") {
		t.Fatalf("reading project_releases before the migration = %v, want the table to be missing: it is what the migration exists for", err)
	}
	if err := claimOf("LEGION-209", "architect", "suspended", "project_release"); err == nil || !strings.Contains(err.Error(), "claims_suspended_by_check") {
		t.Fatalf("a claim suspended by the project release before the migration = %v, want the check to refuse it", err)
	}
	t.Logf("before the migration there is no record and the check refuses the source: %v", err)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the project release over today's rows: %v", err)
	}

	for _, c := range today {
		var state, by string
		if err := store.Pool().QueryRow(ctx, `select state, suspended_by from claims where token = $1`,
			"legion-legion-legion-208-"+c.role).Scan(&state, &by); err != nil {
			t.Fatalf("read the %s the migration found: %v", c.role, err)
		}
		if state != c.state || by != c.by {
			t.Fatalf("after the migration the %s reads %s by %q, want %s by %q as it stood", c.role, state, by, c.state, c.by)
		}
	}
	var controllers string
	if err := store.Pool().QueryRow(ctx, `select suspended_by from claims where token = 'legion-legion-legion-210-architect'`).Scan(&controllers); err != nil {
		t.Fatalf("read the architect the controller suspended: %v", err)
	}
	if controllers != "controller" {
		t.Fatalf("after the migration the architect the controller suspended reads %q, want controller as it stood", controllers)
	}
	var rows, marks int
	if err := store.Pool().QueryRow(ctx, `select (select count(*) from outbox), (select count(*) from issue_releases)`).Scan(&rows, &marks); err != nil {
		t.Fatalf("count the rows and marks the migration found: %v", err)
	}
	if rows != 4 || marks != 1 {
		t.Fatalf("after the migration the outbox holds %d rows and %d marks, want the 4 and the 1 it found", rows, marks)
	}
	t.Logf("after the migration every claim, row and mark it found stands as it was")

	if err := claimOf("LEGION-209", "architect", "suspended", "project_release"); err != nil {
		t.Fatalf("a claim suspended by the project release after the migration: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `update claims set suspended_by = 'someone' where token = 'legion-legion-legion-209-architect'`); err == nil ||
		!strings.Contains(err.Error(), "claims_suspended_by_check") {
		t.Fatalf("a source that is none after the migration = %v, want the check to refuse it", err)
	}

	var id int64
	var result string
	var finished, ended, endedBy *string
	if err := store.Pool().QueryRow(ctx, `insert into project_releases (project, started_at, bound_at) values ('legion', now(), now())
		returning id, finished_at::text, ended_at::text, ended_by, result::text`).Scan(&id, &finished, &ended, &endedBy, &result); err != nil {
		t.Fatalf("record a release with its required columns: %v", err)
	}
	if finished != nil || ended != nil || endedBy != nil || result != "{}" {
		t.Fatalf("a release recorded with its required columns reads finished %v, ended %v by %v, result %s; want none of them and {}",
			finished, ended, endedBy, result)
	}
	if _, err := store.Pool().Exec(ctx, `update project_releases set ended_at = now(), ended_by = 'operator' where id = $1`, id); err == nil ||
		!strings.Contains(err.Error(), "ended_by") {
		t.Fatalf("a release ended by an operator = %v, want the check to refuse it: only a boot ends a release", err)
	}
	if _, err := store.Pool().Exec(ctx, `update project_releases set ended_at = now(), ended_by = 'boot' where id = $1`, id); err != nil {
		t.Fatalf("end a release at a boot: %v", err)
	}
}
