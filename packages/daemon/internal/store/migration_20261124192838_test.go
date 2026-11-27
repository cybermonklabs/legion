package store

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// deploymentRolesVersion is the version of the migration this file tests, so a test can migrate
// through everything before it whatever lands beside it.
const deploymentRolesVersion = 20261124192838

// wayBackOpening opens the way back in a migration's header, and wayBackClosing ends it: what
// stands between them is what psql reads on its standard input.
const (
	wayBackOpening = "PGPASSFILE=<pgpass file> psql '<postgres_url>' -X -v ON_ERROR_STOP=1 <<'SQL'"
	wayBackClosing = "SQL"
)

// heredocWayBackOf is the way back in the header of migration version as an operator copies it
// out of the file: the lines between the psql line and the heredoc's end, each without the comment
// marker and the header's own indent. A header with no such block, or a block that never ends,
// stops the test, since running nothing would pass every assertion that the way back refuses. It
// is not migrations.WayBack (wayBackOf), which reads a header whose last paragraph is the block
// itself: this header ends with the heredoc's closing line.
func heredocWayBackOf(t *testing.T, version int) string {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if migration.Version != version {
			continue
		}
		var block []string
		opened := false
		for _, line := range strings.Split(migration.SQL, "\n") {
			comment, isComment := strings.CutPrefix(line, "--")
			if !isComment {
				break
			}
			switch text := strings.TrimSpace(comment); {
			case !opened && text == wayBackOpening:
				opened = true
			case opened && text == wayBackClosing:
				if len(block) == 0 {
					t.Fatalf("the way back in the header of %s holds no statement", migration.Name)
				}
				return strings.Join(block, "\n")
			case opened:
				block = append(block, strings.TrimPrefix(comment, "   "))
			}
		}
		t.Fatalf("the header of %s holds no way back that opens with %q and ends with %q", migration.Name, wayBackOpening, wayBackClosing)
	}
	t.Fatalf("no migration of version %d is embedded", version)
	return ""
}

// runWayBack runs sql as psql runs it under ON_ERROR_STOP, on a connection of its own: a
// statement that fails ends the run with its error, and the transaction it stood in is rolled
// back.
func runWayBack(t *testing.T, store *Store, sql string) error {
	t.Helper()
	ctx := context.Background()
	conn, release, err := store.Pool().acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the way back: %v", err)
	}
	defer release()
	defer conn.Release()
	_, err = conn.Exec(ctx, sql)
	if err != nil {
		if _, rollback := conn.Exec(ctx, "rollback"); rollback != nil {
			t.Fatalf("roll back the failed way back: %v", rollback)
		}
	}
	return err
}

// 20261124192838 lets the store hold a role a deployment defines: a claim, a phases row and an
// idle release of a name of the role grammar, `^[a-z][a-z0-9-]{0,31}$`, and an issue that stands
// at the phase step, where such a role works, or is held from it. A name outside the grammar is
// still refused by the same checks, and the controller's claim keeps its own shape. Before the
// migration the claim of checker is refused: that refusal is the control which shows each write
// after it is the migration's to take. A parent that waits on its open children stands across the
// migration and its way back, since 20261114232555_awaiting_children wrote that phase into the
// check before this file: a phase list that left it out would refuse the whole migration.
//
// The way back in the header refuses while a row of such a role or such an issue stands, since
// the restored checks would refuse that row with no word of which it is, and it names the first
// row it finds: the issue at step, then the issue held from it, then the parent that waits on its
// children to start step, then the claim, the phases row and the idle release of checker. With
// those gone it restores the checks and deletes the migration's version row, leaving the
// controller's claim, the release of an unowned pod and the waiting parent, which name no role of
// the deployment, standing; and the next Migrate applies the file again.
func TestTheStoreTakesADeploymentRoleAndTheStepPhase(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, deploymentRolesVersion-1)
	exec := func(sql string) error {
		_, err := store.Pool().Exec(ctx, sql)
		return err
	}
	mustExec := func(what, sql string) {
		t.Helper()
		if err := exec(sql); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	claimOf := func(token, issue, role string) string {
		return `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ('` + token + `', 'legion', '` + issue + `', '` + issue + `', '` + role + `', 1, 'ses', '', 'working', 0, 0, 0, 0)`
	}
	issueAt := func(key, at, heldFrom string) string {
		held := "null"
		if heldFrom != "" {
			held = "'" + heldFrom + "'"
		}
		return `insert into issues (key, tree, project, title, phase, held_from, generation, status, rank, last_dispatch_seq)
			values ('` + key + `', '` + key + `', 'LEGION', 'an issue', '` + at + `', ` + held + `, 1, 'in_progress', 'A', 1)`
	}
	phasesRowOf := func(issue, role string) string {
		return `insert into phases (issue, role, claim, handoff_commit, rounds, verdict)
			values ('` + issue + `', '` + role + `', '', '', 0, '')`
	}
	waitingParent := func(key, start string) string {
		return `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
			children_wait_start, children_wait_task, children_wait_reason, children_wait_since)
			values ('` + key + `', '` + key + `', 'LEGION', 'a parent', 'awaiting_children', 1, 'in_progress', 'A', 1,
			'` + start + `', 'the held task', 'a child is open', now())`
	}
	releaseOf := func(issue, role, kind string) string {
		return `insert into issue_releases (project, issue, generation, first_row, boundary_row, last_row, started_row, role, kind)
			values ('legion', '` + issue + `', 1, 18, 20, 22, 0, '` + role + `', '` + kind + `')`
	}
	refusedBy := func(what, sql, constraint string) {
		t.Helper()
		if err := exec(sql); err == nil || !strings.Contains(err.Error(), constraint) {
			t.Fatalf("%s = %v, want %s to refuse it", what, err, constraint)
		}
	}

	// What stands across the migration: the controller's claim, an issue at work and an issue
	// whose pod was released with no role to name.
	mustExec("the controller's claim", `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ('legion-legion-controller', 'legion', '', '', 'controller', 1, 'ses', '', 'working', 0, 0, 0, 0)`)
	mustExec("an issue at work", issueAt("LEGION-208", "implementing", ""))
	mustExec("an issue of an unowned pod", issueAt("LEGION-210", "implementing", ""))
	mustExec("the release of an unowned pod", releaseOf("LEGION-210", "", "unowned"))
	mustExec("a parent that waits on its open children", waitingParent("LEGION-207", "testing"))

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the deployment's roles: %v", err)
	}

	mustExec("the claim of checker", claimOf("legion-legion-legion-208-checker", "LEGION-208", "checker"))
	mustExec("the phases row of checker", phasesRowOf("LEGION-208", "checker"))
	mustExec("the idle release of checker", releaseOf("LEGION-208", "checker", "idle"))
	mustExec("an issue at step", `update issues set phase = 'step' where key = 'LEGION-208'`)
	mustExec("an issue held from step", issueAt("LEGION-209", "held", "step"))
	mustExec("a parent that waits on its children to start step", waitingParent("LEGION-213", "step"))

	long := "c" + strings.Repeat("x", 32)
	for _, refusal := range []struct{ what, sql, constraint string }{
		{"a claim of Checker", claimOf("legion-legion-legion-208-Checker", "LEGION-208", "Checker"), "claims_role_check"},
		{"a claim of a role of 33 characters", claimOf("legion-legion-legion-208-"+long, "LEGION-208", long), "claims_role_check"},
		{"the controller's claim on an issue", claimOf("legion-legion-legion-208-controller", "LEGION-208", "controller"), "claims_controller_shape"},
		{"a phases row of Checker", phasesRowOf("LEGION-208", "Checker"), "phases_role_check"},
		{"an idle release of Checker", releaseOf("LEGION-211", "Checker", "idle"), "issue_releases_role_check"},
		{"an unowned release that names checker", releaseOf("LEGION-211", "checker", "unowned"), "issue_releases_role_check"},
		{"an issue at a phase of no name", issueAt("LEGION-212", "stepping", ""), "issues_phase_check"},
	} {
		refusedBy(refusal.what, refusal.sql, refusal.constraint)
	}

	wayBack := heredocWayBackOf(t, deploymentRolesVersion)
	for _, stand := range []struct {
		named, then string
	}{
		{"issue LEGION-208", `update issues set phase = 'implementing' where key = 'LEGION-208'`},
		{"issue LEGION-209", `update issues set held_from = 'implementing' where key = 'LEGION-209'`},
		{"issue LEGION-213", `update issues set children_wait_start = 'implementing' where key = 'LEGION-213'`},
		{"claim legion-legion-legion-208-checker", `delete from claims where token = 'legion-legion-legion-208-checker'`},
		{"checker on LEGION-208", `delete from phases where issue = 'LEGION-208' and role = 'checker'`},
		{"idle release of LEGION-208", `delete from issue_releases where issue = 'LEGION-208'`},
	} {
		err := runWayBack(t, store, wayBack)
		if err == nil || !strings.Contains(err.Error(), stand.named) {
			t.Fatalf("the way back with %s standing = %v, want a refusal that names it", stand.named, err)
		}
		t.Logf("the way back refuses: %v", err)
		mustExec("clear "+stand.named, stand.then)
	}
	if err := runWayBack(t, store, wayBack); err != nil {
		t.Fatalf("the way back with no row of a role of the deployment: %v", err)
	}

	var controllers, unowned, waiting, recorded int
	if err := store.Pool().QueryRow(ctx, `select
		(select count(*) from claims where token = 'legion-legion-controller'),
		(select count(*) from issue_releases where issue = 'LEGION-210' and kind = 'unowned'),
		(select count(*) from issues where key = 'LEGION-207' and phase = 'awaiting_children'),
		(select count(*) from schema_version where version = $1)`, deploymentRolesVersion).Scan(&controllers, &unowned, &waiting, &recorded); err != nil {
		t.Fatalf("read the store after the way back: %v", err)
	}
	if controllers != 1 || unowned != 1 || waiting != 1 || recorded != 0 {
		t.Fatalf("after the way back: %d controller claims, %d unowned releases, %d waiting parents, %d version rows; want 1, 1, 1 and 0",
			controllers, unowned, waiting, recorded)
	}
	refusedBy("a claim of checker after the way back", claimOf("legion-legion-legion-208-checker", "LEGION-208", "checker"), "claims_role_check")
	refusedBy("a phases row of checker after the way back", phasesRowOf("LEGION-208", "checker"), "phases_role_check")
	refusedBy("an idle release of checker after the way back", releaseOf("LEGION-208", "checker", "idle"), "issue_releases_role_check")
	refusedBy("an issue at step after the way back", `update issues set phase = 'step' where key = 'LEGION-208'`, "issues_phase_check")
	refusedBy("an issue held from step after the way back", `update issues set held_from = 'step' where key = 'LEGION-209'`, "issues_held_from_check")

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate again after the way back: %v", err)
	}
	mustExec("the claim of checker once the store is migrated again", claimOf("legion-legion-legion-208-checker", "LEGION-208", "checker"))
}
