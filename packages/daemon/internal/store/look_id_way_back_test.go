package store

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// lookIDWayBackExtract is the copy of the way back docs/kubernetes.md gives under "Rolling back the
// release whose look report gives its look id", which internal/textcheck's
// TestTheLookIDWayBackExtractIsTheGuidesBlock holds to the guide's block, line for line. This file
// reads the copy beside it rather than the guide, so the guide stays out of this package's CI
// filter: a test that read the guide would start the whole daemon Go lane on an edit of the guide
// alone.
const lookIDWayBackExtract = "look_id_way_back_extract.txt"

// lookIDWayBackFor is the block the extract carries, everything under its run of `#` lines, as an
// operator who rolls project back runs it: with project's key in place of rollingBackProject. A
// copy with nothing under its header, or with no rollingBackProject to name the project by, stops
// the test by name: a block this cannot find is a block it would pass while running nothing.
func lookIDWayBackFor(t *testing.T, project string) string {
	t.Helper()
	text, err := os.ReadFile(lookIDWayBackExtract)
	if err != nil {
		t.Fatalf("read the way back of %s: %v", lookIDRelease, err)
	}
	body := string(text)
	for strings.HasPrefix(body, "#") {
		_, body, _ = strings.Cut(body, "\n")
	}
	if body = strings.TrimSpace(body); body == "" || !strings.Contains(body, rollingBackProject) {
		t.Fatalf("%s carries no block naming the project by %s under its `#` header: %q", lookIDWayBackExtract,
			rollingBackProject, body)
	}
	return strings.ReplaceAll(body, rollingBackProject, project)
}

// The way back of the release whose look report gives its look id, run for LEGION with every
// daemon stopped, leaves each queued row and each production check of LEGION as the release
// before can take it, and each of another project as this release left it:
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
//     would leave in place. The look id columns stay as they are.
//
// OTHER's rows, of the same shapes, stay as they are. A second run changes nothing. The control is
// the state before the block, which the same reads hold to the fixtures.
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
		// A root re-admitted after its check's merge, which keeps its check as the release before does.
		{"LEGION-300", "LEGION", "LEGION-300", phase.Planning, "in_progress", false},
		// A child that runs again: set back to todo after its check opened.
		{"LEGION-301", "LEGION", "LEGION-300", phase.Implementing, "in_progress", false},
		// A done child.
		{"LEGION-302", "LEGION", "LEGION-300", phase.Done, "done", false},
		// A parked child: its status takes it out of the workflow whatever its phase.
		{"LEGION-303", "LEGION", "LEGION-300", phase.Implementing, "backlog", false},
		// A child that runs again whose check closed already.
		{"LEGION-304", "LEGION", "LEGION-300", phase.Testing, "testing", true},
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
	const queued = `select issue || ' ' || kind || ' ' || coalesce(payload->>'op', payload->>'kind') || ' ' ||
		coalesce(payload->>'task', '-') || ' look ' || coalesce(payload->>'checkLook', '-') ||
		' id ' || coalesce(payload->>'lookId', '-') || ' reissue ' || coalesce(payload->>'lookReissue', '-') from outbox order by id`
	checks := `select issue || ' ' || case when closed_at is null then 'open' when closed_at = '` + closed.Format(time.RFC3339) +
		`'::timestamptz then 'closed before' else 'closed now' end || ' ' || coalesce(outcome, '-') || ' look open ' ||
		look_open::text || ' generation ' || coalesce(opened_generation::text, '-') || ' id ' || coalesce(look_id, '-')
		from production_checks order by issue`
	held := func(what, query string, want []string) {
		t.Helper()
		if got := rowsAsText(t, s, query); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s:\n got %q\nwant %q", what, got, want)
		}
	}
	held("the queued rows before the way back", queued, []string{
		"LEGION-301 supervise start look 2 look 2 id LOOKIDOFLEGION301 reissue true",
		"LEGION-302 supervise start look 3 look 3 id LOOKIDOFLEGION302 reissue -",
		"LEGION-302 supervise start look 1 look 1 id - reissue -",
		"LEGION-301 supervise retire_look - look 2 id - reissue -",
		"LEGION-301 supervise retire_task - look - id - reissue -",
		"LEGION-301 notice phase-finished - look - id kept reissue -",
		"OTHER-301 supervise start look 2 look 2 id LOOKIDOFOTHER301 reissue true",
		"OTHER-301 supervise retire_look - look 2 id - reissue -",
	})
	held("the checks before the way back", checks, []string{
		"LEGION-300 open - look open true generation 2 id LOOKIDOFLEGION300",
		"LEGION-301 open - look open true generation 2 id LOOKIDOFLEGION301",
		"LEGION-302 open - look open true generation 2 id LOOKIDOFLEGION302",
		"LEGION-303 open - look open true generation 2 id LOOKIDOFLEGION303",
		"LEGION-304 closed before recorded look open false generation 2 id LOOKIDOFLEGION304",
		"OTHER-301 open - look open true generation 2 id LOOKIDOFOTHER301",
	})

	wayBack := lookIDWayBackFor(t, "LEGION")
	if _, err := s.Pool().Exec(ctx, wayBack); err != nil {
		t.Fatalf("run the way back for LEGION: %v", err)
	}
	held("the queued rows after the way back: lookId and lookReissue off LEGION's supervise rows, its retire_look row gone, "+
		"every other key, row and project kept", queued, []string{
		"LEGION-301 supervise start look 2 look 2 id - reissue -",
		"LEGION-302 supervise start look 3 look 3 id - reissue -",
		"LEGION-302 supervise start look 1 look 1 id - reissue -",
		"LEGION-301 supervise retire_task - look - id - reissue -",
		"LEGION-301 notice phase-finished - look - id kept reissue -",
		"OTHER-301 supervise start look 2 look 2 id LOOKIDOFOTHER301 reissue true",
		"OTHER-301 supervise retire_look - look 2 id - reissue -",
	})
	held("the checks after the way back: the open check of LEGION's child that runs again closed not_run, every LEGION "+
		"generation cleared, the look ids and OTHER's check kept", checks, []string{
		"LEGION-300 open - look open true generation - id LOOKIDOFLEGION300",
		"LEGION-301 closed now not_run look open false generation - id LOOKIDOFLEGION301",
		"LEGION-302 open - look open true generation - id LOOKIDOFLEGION302",
		"LEGION-303 open - look open true generation - id LOOKIDOFLEGION303",
		"LEGION-304 closed before recorded look open false generation - id LOOKIDOFLEGION304",
		"OTHER-301 open - look open true generation 2 id LOOKIDOFOTHER301",
	})

	const everything = `select payload::text from outbox union all
		select row(issue, closed_at, outcome, look_open, opened_generation, look_id)::text from production_checks order by 1`
	once := rowsAsText(t, s, everything)
	if _, err := s.Pool().Exec(ctx, wayBack); err != nil {
		t.Fatalf("run the way back for LEGION again: %v", err)
	}
	held("the rows after a second run of the way back, which changes nothing", everything, once)
}
