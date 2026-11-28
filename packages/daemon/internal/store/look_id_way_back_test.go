package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The way back of the release whose look report gives its look id, the last paragraph of the
// header of lookIDVersion (20261127171352_production_check_look_id, migrations.WayBack), run for
// LEGION (LEGION in place of rollingBackProject) with every daemon stopped, leaves each queued row
// and each production check of LEGION as the release before can take it, and each of another
// project as this release left it:
//
//   - lookId and lookReissue go off each supervise row that carries them (the release before
//     decodes a payload strictly, DisallowUnknownFields), and every other key of the row stays.
//   - Each retire_look row goes (the release before knows no such op), and a row of another op, and
//     a row of another kind that carries a lookId key, stay.
//   - The open check of a child that runs again closes not_run, its look closed: the release
//     before closes a child's check at its re-entry, never at a later leave, so under it that check
//     would start looks again after the child's next leave. The open check of a root in the
//     workflow, of a done child and of a parked child stays open, and a check that closed already
//     keeps its close.
//   - opened_generation goes null on each check, which the release before's upsert of a new check
//     would leave in place.
//   - The look id, the look and the check instance it was made for go null on each open check
//     whose issue is out of the workflow (a done issue or a parked one): a start of the current look
//     that the release before runs would otherwise leave the check holding that look's id as the
//     current one with a delivery that holds none, so after the roll forward the boot would give the
//     look no new task and the send would drop the old one. The open check of a root in the
//     workflow keeps its look id: the root's checking role can hold that id from the task of the
//     look and report with it while the root runs (decision 7 of dispatch://INFRA-3518), and the
//     release before sends that role no task of the look while the run holds the root. A check the
//     block closes, and a check that closed already, keeps its look id.
//
// OTHER's rows, of the same shapes, stay as they are. A second run changes nothing. The control is
// the state before the block, which the same reads hold to the fixtures.
//
// docs/kubernetes.md carries the same block under "Rolling back the release whose look report gives
// its look id", and internal/textcheck's TestTheLookIDWayBackIsOneTextInTheHeaderAndTheGuide holds
// the two to one text. This file reads the header alone, so the guide stays out of this package's
// CI filter: a test that read the guide would start the whole daemon Go lane on an edit of the
// guide alone.
func TestTheWayBackStripsTheLookIDRowsAndClosesTheChecksOfChildrenThatRunAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := migratedStore(t)
	opened := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	closed := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for _, fixture := range []struct {
		key, project, tree string
		phase              phase.Phase
		status             string
		closed             bool
	}{
		// A root re-admitted after its check's merge, which keeps its check as the release before does,
		// and keeps its look id, which its checking role can still report with while the root runs.
		{"LEGION-300", "LEGION", "LEGION-300", phase.Planning, "in_progress", false},
		// A child that runs again: set back to todo after its check opened. Its check closes with its
		// look id.
		{"LEGION-301", "LEGION", "LEGION-300", phase.Implementing, "in_progress", false},
		// A done child, which loses its look id.
		{"LEGION-302", "LEGION", "LEGION-300", phase.Done, "done", false},
		// A parked child: its status takes it out of the workflow whatever its phase. It loses its look
		// id.
		{"LEGION-303", "LEGION", "LEGION-300", phase.Implementing, "backlog", false},
		// A child that runs again whose check closed already, which keeps its look id.
		{"LEGION-304", "LEGION", "LEGION-300", phase.Testing, "testing", true},
		// A done root whose check is open: the look id goes, and the roll forward's boot gives the
		// open look a new one.
		{"LEGION-305", "LEGION", "LEGION-305", phase.Done, "done", false},
		// A done root whose check closed: a closed check keeps its look id.
		{"LEGION-306", "LEGION", "LEGION-306", phase.Done, "done", true},
		// Another project's done root whose check is open, which keeps its look id.
		{"OTHER-300", "OTHER", "OTHER-300", phase.Done, "done", false},
		// Another project's child that runs again.
		{"OTHER-301", "OTHER", "OTHER-300", phase.Implementing, "in_progress", false},
	} {
		issue := record.Issue{Key: fixture.key, Project: fixture.project, Tree: fixture.tree, Repo: "acme/widgets",
			GitHubRepo: "acme/widgets", DefaultBranch: "main", Title: "look id", Phase: fixture.phase, Generation: 3,
			Status: fixture.status}
		check := record.ProductionCheck{Issue: fixture.key, Project: fixture.project, Tree: fixture.tree, Repo: "acme/widgets",
			Number: 42, MergeSHA: "merge", OpenedAt: opened, BoundAt: opened.Add(14 * 24 * time.Hour), Role: claim.RoleTester,
			Looks: 2, LookOpen: true, LastWakeKind: "merge", LookID: "LOOKIDOF" + strings.ReplaceAll(fixture.key, "-", ""),
			LookIDLook: 2, LookIDOpenedAt: opened, OpenedGeneration: 2}
		if fixture.closed {
			check.LookOpen, check.ClosedAt, check.Outcome = false, &closed, record.CheckRecorded
		}
		if err := Transact(ctx, s.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			if err := record.NewStore().PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			return record.NewStore().PutProductionCheck(ctx, tx, check)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct{ kind, issue, payload string }{
		{"supervise", "LEGION-301", `{"op":"start","tree":"LEGION-300","role":"tester","task":"look 2","checkLook":2,"checkOpenedAt":"2026-11-27T09:00:00Z","lookId":"LOOKIDOFLEGION301","lookReissue":true}`},
		{"supervise", "LEGION-302", `{"op":"start","tree":"LEGION-300","role":"tester","task":"look 3","checkLook":3,"checkOpenedAt":"2026-11-27T09:00:00Z","lookId":"LOOKIDOFLEGION302"}`},
		{"supervise", "LEGION-302", `{"op":"start","tree":"LEGION-300","role":"tester","task":"look 1","checkLook":1}`},
		{"supervise", "LEGION-301", `{"op":"retire_look","tree":"LEGION-300","role":"tester","checkLook":2,"checkOpenedAt":"2026-11-27T09:00:00Z","reported":false}`},
		{"supervise", "LEGION-301", `{"op":"retire_task","tree":"LEGION-300","role":"tester","generation":3,"phase":"testing"}`},
		{"notice", "LEGION-301", `{"kind":"phase-finished","lookId":"kept"}`},
		{"supervise", "OTHER-301", `{"op":"start","tree":"OTHER-300","role":"tester","task":"look 2","checkLook":2,"checkOpenedAt":"2026-11-27T09:00:00Z","lookId":"LOOKIDOFOTHER301","lookReissue":true}`},
		{"supervise", "OTHER-301", `{"op":"retire_look","tree":"OTHER-300","role":"tester","checkLook":2,"checkOpenedAt":"2026-11-27T09:00:00Z","reported":true}`},
	} {
		if _, err := s.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, $3::jsonb, 0, now(), '')`, row.kind, row.issue, row.payload); err != nil {
			t.Fatalf("queue %s's %s row: %v", row.issue, row.kind, err)
		}
	}
	const queued = `select issue || ' ' || kind || ' ' || payload::text from outbox order by id`
	checks := `select issue || ' ' || case when closed_at is null then 'open' when closed_at = '` + closed.Format(time.RFC3339) +
		`'::timestamptz then 'closed before' else 'closed now' end || ' ' || coalesce(outcome, '-') || ' look open ' ||
		look_open::text || ' generation ' || coalesce(opened_generation::text, '-') || ' id ' || coalesce(look_id, '-') ||
		' made for look ' || coalesce(look_id_look::text, '-') || ' of ' || case when look_id_opened_at is null then '-'
		when look_id_opened_at = '` + opened.Format(time.RFC3339) + `'::timestamptz then 'this check' else 'another check' end
		from production_checks order by issue`
	held := func(what, query string, want []string) {
		t.Helper()
		if got := rowsAsText(t, s, query); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s:\n got %q\nwant %q", what, got, want)
		}
	}
	held("the queued rows before the way back", queued, []string{
		`LEGION-301 supervise {"op": "start", "role": "tester", "task": "look 2", "tree": "LEGION-300", "lookId": "LOOKIDOFLEGION301", "checkLook": 2, "lookReissue": true, "checkOpenedAt": "2026-11-27T09:00:00Z"}`,
		`LEGION-302 supervise {"op": "start", "role": "tester", "task": "look 3", "tree": "LEGION-300", "lookId": "LOOKIDOFLEGION302", "checkLook": 3, "checkOpenedAt": "2026-11-27T09:00:00Z"}`,
		`LEGION-302 supervise {"op": "start", "role": "tester", "task": "look 1", "tree": "LEGION-300", "checkLook": 1}`,
		`LEGION-301 supervise {"op": "retire_look", "role": "tester", "tree": "LEGION-300", "reported": false, "checkLook": 2, "checkOpenedAt": "2026-11-27T09:00:00Z"}`,
		`LEGION-301 supervise {"op": "retire_task", "role": "tester", "tree": "LEGION-300", "phase": "testing", "generation": 3}`,
		`LEGION-301 notice {"kind": "phase-finished", "lookId": "kept"}`,
		`OTHER-301 supervise {"op": "start", "role": "tester", "task": "look 2", "tree": "OTHER-300", "lookId": "LOOKIDOFOTHER301", "checkLook": 2, "lookReissue": true, "checkOpenedAt": "2026-11-27T09:00:00Z"}`,
		`OTHER-301 supervise {"op": "retire_look", "role": "tester", "tree": "OTHER-300", "reported": true, "checkLook": 2, "checkOpenedAt": "2026-11-27T09:00:00Z"}`,
	})
	held("the checks before the way back", checks, []string{
		"LEGION-300 open - look open true generation 2 id LOOKIDOFLEGION300 made for look 2 of this check",
		"LEGION-301 open - look open true generation 2 id LOOKIDOFLEGION301 made for look 2 of this check",
		"LEGION-302 open - look open true generation 2 id LOOKIDOFLEGION302 made for look 2 of this check",
		"LEGION-303 open - look open true generation 2 id LOOKIDOFLEGION303 made for look 2 of this check",
		"LEGION-304 closed before recorded look open false generation 2 id LOOKIDOFLEGION304 made for look 2 of this check",
		"LEGION-305 open - look open true generation 2 id LOOKIDOFLEGION305 made for look 2 of this check",
		"LEGION-306 closed before recorded look open false generation 2 id LOOKIDOFLEGION306 made for look 2 of this check",
		"OTHER-300 open - look open true generation 2 id LOOKIDOFOTHER300 made for look 2 of this check",
		"OTHER-301 open - look open true generation 2 id LOOKIDOFOTHER301 made for look 2 of this check",
	})

	wayBack := strings.ReplaceAll(wayBackOf(t, lookIDVersion, lookIDRelease), rollingBackProject, "LEGION")
	if _, err := s.Pool().Exec(ctx, wayBack); err != nil {
		t.Fatalf("run the way back for LEGION: %v", err)
	}
	held("the queued rows after the way back: lookId and lookReissue off LEGION's supervise rows, its retire_look row gone, "+
		"every other key, row and project kept", queued, []string{
		`LEGION-301 supervise {"op": "start", "role": "tester", "task": "look 2", "tree": "LEGION-300", "checkLook": 2, "checkOpenedAt": "2026-11-27T09:00:00Z"}`,
		`LEGION-302 supervise {"op": "start", "role": "tester", "task": "look 3", "tree": "LEGION-300", "checkLook": 3, "checkOpenedAt": "2026-11-27T09:00:00Z"}`,
		`LEGION-302 supervise {"op": "start", "role": "tester", "task": "look 1", "tree": "LEGION-300", "checkLook": 1}`,
		`LEGION-301 supervise {"op": "retire_task", "role": "tester", "tree": "LEGION-300", "phase": "testing", "generation": 3}`,
		`LEGION-301 notice {"kind": "phase-finished", "lookId": "kept"}`,
		`OTHER-301 supervise {"op": "start", "role": "tester", "task": "look 2", "tree": "OTHER-300", "lookId": "LOOKIDOFOTHER301", "checkLook": 2, "lookReissue": true, "checkOpenedAt": "2026-11-27T09:00:00Z"}`,
		`OTHER-301 supervise {"op": "retire_look", "role": "tester", "tree": "OTHER-300", "reported": true, "checkLook": 2, "checkOpenedAt": "2026-11-27T09:00:00Z"}`,
	})
	held("the checks after the way back: the open check of LEGION's child that runs again closed not_run, every LEGION "+
		"generation cleared, the look id of each LEGION check left open whose issue is out of the workflow cleared, and the "+
		"look ids of LEGION's root that runs again, of LEGION's closed checks and of OTHER's checks kept", checks, []string{
		"LEGION-300 open - look open true generation - id LOOKIDOFLEGION300 made for look 2 of this check",
		"LEGION-301 closed now not_run look open false generation - id LOOKIDOFLEGION301 made for look 2 of this check",
		"LEGION-302 open - look open true generation - id - made for look - of -",
		"LEGION-303 open - look open true generation - id - made for look - of -",
		"LEGION-304 closed before recorded look open false generation - id LOOKIDOFLEGION304 made for look 2 of this check",
		"LEGION-305 open - look open true generation - id - made for look - of -",
		"LEGION-306 closed before recorded look open false generation - id LOOKIDOFLEGION306 made for look 2 of this check",
		"OTHER-300 open - look open true generation 2 id LOOKIDOFOTHER300 made for look 2 of this check",
		"OTHER-301 open - look open true generation 2 id LOOKIDOFOTHER301 made for look 2 of this check",
	})

	const everything = `select payload::text from outbox union all
		select row(issue, closed_at, outcome, look_open, opened_generation, look_id, look_id_look, look_id_opened_at)::text
		from production_checks order by 1`
	once := rowsAsText(t, s, everything)
	if _, err := s.Pool().Exec(ctx, wayBack); err != nil {
		t.Fatalf("run the way back for LEGION again: %v", err)
	}
	held("the rows after a second run of the way back, which changes nothing", everything, once)
}
