package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// projectPauseVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const projectPauseVersion = 20261115194357

// The pause's three tables do not exist before the migration, and neither does a resume's stuck
// clock: the negative control is each refused as the undefined table (42P01) and column (42703)
// it is, since any other error would satisfy a weaker control. After it every resume the database
// already held starts its clock at its own wake (clock_at = at), a resume inserted without a
// clock gets the same, and the tables refuse every row their checks exist for: a stop's claim
// must be a role token of the pause's project, while its issue, and a held pod's, may be another
// project's.
func TestTheProjectPauseRecordsWith20261115194357(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, projectPauseVersion-1)

	if _, err := store.Pool().Exec(ctx, `select 1 from project_pauses`); err == nil || !strings.Contains(err.Error(), "42P01") {
		t.Fatalf("reading project_pauses before the migration = %v, want SQLSTATE 42P01, the undefined table the migration adds", err)
	}
	if _, err := store.Pool().Exec(ctx, `update pod_resumes set clock_at = at`); err == nil || !strings.Contains(err.Error(), "42703") {
		t.Fatalf("writing clock_at before the migration = %v, want SQLSTATE 42703, the undefined column the migration adds", err)
	}

	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	seeded := map[string]time.Time{"LEGION-208": at.Add(-3 * time.Hour), "LEGION-209": at.Add(-time.Hour)}
	for issue, woke := range seeded {
		if _, err := store.Pool().Exec(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at)
			values (0, 'legion', $1, 'implementer', 'delivery', $2)`, issue, woke); err != nil {
			t.Fatalf("seed the resume of %s: %v", issue, err)
		}
	}
	if _, err := store.Pool().Exec(ctx, `update pod_resumes set first_turn_at = at + interval '1 minute' where issue = 'LEGION-209'`); err != nil {
		t.Fatalf("stamp LEGION-209's first turn: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the project pause: %v", err)
	}

	for issue, woke := range seeded {
		var clock time.Time
		if err := store.Pool().QueryRow(ctx, `select clock_at from pod_resumes where issue = $1`, issue).Scan(&clock); err != nil {
			t.Fatalf("read the clock of %s's resume: %v", issue, err)
		}
		if !clock.Equal(woke) {
			t.Fatalf("the clock of %s's resume = %s, want its wake %s", issue, clock, woke)
		}
	}
	t.Logf("after the migration every resume's clock starts at its wake")

	// A daemon of the earlier release names clock_at nowhere: its insert still writes a row, with
	// the clock at the wake.
	var clock time.Time
	if err := store.Pool().QueryRow(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at)
		values (0, 'legion', 'LEGION-210', 'tester', 'checks', $1) returning clock_at`, at).Scan(&clock); err != nil {
		t.Fatalf("insert a resume naming no clock: %v", err)
	}
	if !clock.Equal(at) {
		t.Fatalf("a resume inserted with no clock has clock %s, want its wake %s", clock, at)
	}

	exec := func(sql string, args ...any) error {
		_, err := store.Pool().Exec(ctx, sql, args...)
		return err
	}
	refused := func(what, sqlstate, sql string, args ...any) {
		t.Helper()
		if err := exec(sql, args...); err == nil || !strings.Contains(err.Error(), sqlstate) {
			t.Fatalf("%s = %v, want SQLSTATE %s", what, err, sqlstate)
		}
	}
	pauseRow := `insert into project_pauses (project, began_at, by_kind, by_session, reason) values ($1, $2, $3, $4, $5)`

	refused("a controller's pause with no session", "23514", pauseRow, "LEGION", at, "controller", "", "outage")
	refused("an operator's pause with a session", "23514", pauseRow, "LEGION", at, "operator", "session-1", "outage")
	refused("a pause whose reason is white space", "23514", pauseRow, "LEGION", at, "operator", "", " \t\n")
	refused("a pause of a project token rather than its key", "23514", pauseRow, "legion", at, "operator", "", "outage")
	refused("a pause by someone else", "23514", pauseRow, "LEGION", at, "reviewer", "", "outage")

	var paused int64
	if err := store.Pool().QueryRow(ctx, pauseRow+` returning id`, "LEGION", at, "controller", "session-1", "the provider is down").Scan(&paused); err != nil {
		t.Fatalf("write the controller's pause: %v", err)
	}
	refused("a second open pause of the project", "23505", pauseRow, "LEGION", at, "operator", "", "again")
	if err := exec(pauseRow, "INFRA", at, "operator", "", "another project's own pause"); err != nil {
		t.Fatalf("pause another project beside the paused one: %v", err)
	}

	refused("an end with no one who ended it", "23514", `update project_pauses set ended_at = $2 where id = $1`, paused, at)
	refused("who ended it with no end", "23514", `update project_pauses set ended_by_kind = 'operator', ended_by_session = '' where id = $1`, paused)
	refused("an end by the controller with no session", "23514",
		`update project_pauses set ended_at = $2, ended_by_kind = 'controller', ended_by_session = '' where id = $1`, paused, at)
	refused("an end by the operator with a session", "23514",
		`update project_pauses set ended_at = $2, ended_by_kind = 'operator', ended_by_session = 'session-1' where id = $1`, paused, at)
	refused("an end by someone else", "23514",
		`update project_pauses set ended_at = $2, ended_by_kind = 'reviewer', ended_by_session = '' where id = $1`, paused, at)

	stopRow := `insert into project_pause_stops (pause_id, project, claim, issue, role, asked_at) values ($1, 'LEGION', $2, 'LEGION-208', 'implementer', $3)`
	if err := exec(stopRow, paused, "legion-legion-legion-208-implementer", at); err != nil {
		t.Fatalf("open the stop of the implementer: %v", err)
	}
	refused("a second open stop of one claim by one pause", "23505", stopRow, paused, "legion-legion-legion-208-implementer", at)
	refused("a stop ended in no known way", "23514", `update project_pause_stops set ended_at = $2, ended_as = 'forgotten' where pause_id = $1`, paused, at)
	refused("a cancelled stop that owed a launch", "23514",
		`update project_pause_stops set ended_at = $2, ended_as = 'cancelled', launch_owed = true where pause_id = $1`, paused, at)
	refused("a stop of a pause no row holds", "23503", stopRow, paused+100, "legion-legion-legion-208-tester", at)
	stopOf := `insert into project_pause_stops (pause_id, project, claim, issue, role, asked_at) values ($1, $2, $3, $4, $5, $6)`
	refused("a stop naming no claim", "23514", stopOf, paused, "LEGION", "", "LEGION-208", "tester", at)
	refused("a stop naming no role", "23514", stopOf, paused, "LEGION", "legion-legion-legion-208-tester", "LEGION-208", "", at)
	refused("a stop of something that is not an issue key", "23514", stopOf, paused, "LEGION", "legion-legion-legion-208-tester", "legion-208", "tester", at)
	refused("a stop of a claim under another project's token", "23514", stopOf, paused, "LEGION", "legion-infra-infra-208-tester", "INFRA-208", "tester", at)
	refused("a stop of a claim whose token only begins with the project's", "23514", stopOf, paused, "LEGION", "legion-legionx-legion-208-tester", "LEGION-208", "tester", at)
	refused("a stop of a claim of no role token", "23514", stopOf, paused, "LEGION", "legion-208-tester", "LEGION-208", "tester", at)
	refused("a stop naming its pause under another project", "23503", stopOf, paused, "INFRA", "legion-infra-infra-208-tester", "INFRA-208", "tester", at)
	// The operator's spawn makes a claim of another project's issue under this project's token,
	// and this project's pause stops it.
	if err := exec(stopOf, paused, "LEGION", "legion-legion-infra-208-tester", "INFRA-208", "tester", at); err != nil {
		t.Fatalf("open the stop of a claim of another project's issue under the pause's project: %v", err)
	}
	refused("a stop naming no project", "23502", stopOf, paused, nil, "legion-legion-legion-208-tester", "LEGION-208", "tester", at)
	refused("an end of a stop that says no way it ended", "23514", `update project_pause_stops set ended_at = $2 where pause_id = $1`, paused, at)
	refused("a way a stop ended with no end", "23514", `update project_pause_stops set ended_as = 'stopped' where pause_id = $1`, paused)

	holdRow := `insert into project_pause_pod_holds (pause_id, project, issue, held_at) values ($1, $2, $3, $4)`
	if err := exec(holdRow, paused, "LEGION", "LEGION-208", at); err != nil {
		t.Fatalf("hold the pod of LEGION-208: %v", err)
	}
	refused("a second open hold of one pod", "23505", holdRow, paused, "LEGION", "LEGION-208", at)
	// The pod of a claim of another project's issue, held by the pause of the claim's project.
	if err := exec(holdRow, paused, "LEGION", "INFRA-208", at); err != nil {
		t.Fatalf("hold the pod of a claim of another project's issue under the pause's project: %v", err)
	}
	refused("a hold naming its pause under another project", "23503", holdRow, paused, "INFRA", "INFRA-208", at)
	refused("a hold ended for no reason", "23514", `update project_pause_pod_holds set ended_at = $2 where pause_id = $1`, paused, at)
	refused("a hold ended for a reason it does not know", "23514",
		`update project_pause_pod_holds set ended_at = $2, ended_why = 'forgotten' where pause_id = $1`, paused, at)
	refused("a hold of something that is not an issue key", "23514", holdRow, paused, "LEGION", "LEGION-x", at)

	refused("a held resume that pairs with a release", "23514",
		`insert into pod_resumes (release_id, project, issue, role, kind, at, pause_id) values (7, 'legion', 'LEGION-211', 'tester', 'pause', $1, $2)`, at, paused)
	refused("a resume held by a pause no row holds", "23503",
		`insert into pod_resumes (release_id, project, issue, role, kind, at, pause_id) values (0, 'legion', 'LEGION-211', 'tester', 'pause', $1, $2)`, at, paused+100)
	if err := exec(`insert into pod_resumes (release_id, project, issue, role, kind, at, pause_id) values (0, 'legion', 'LEGION-211', 'tester', 'pause', $1, $2)`, at, paused); err != nil {
		t.Fatalf("record a resume the pause holds: %v", err)
	}

	// An end with who ended it is a row the table takes, and the project may be paused again.
	if err := exec(`update project_pauses set ended_at = $2, ended_by_kind = 'operator', ended_by_session = '' where id = $1`, paused, at.Add(time.Hour)); err != nil {
		t.Fatalf("end the pause: %v", err)
	}
	if err := exec(pauseRow, "LEGION", at.Add(2*time.Hour), "operator", "", "the provider is down again"); err != nil {
		t.Fatalf("pause the project again once its pause ended: %v", err)
	}
	t.Logf("after the migration the pause's tables refuse every row their checks exist for and take the rest")
}
