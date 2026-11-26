package store

import (
	"context"
	"reflect"
	"testing"
)

// projectPauseRelease names the release whose way back the header of projectPauseVersion carries,
// as a failure to read that block names it.
const projectPauseRelease = "the release that pauses a project"

// The way back of the release that pauses a project is the last paragraph of its migration's header
// (migrations.WayBack). The release before decodes an outbox payload strictly
// (record.DecodeOutboxPayload, DisallowUnknownFields), and then checks its kind and its op. So the
// block deletes the controller's notices of a pause and of a resume, and each pause's stop of a
// role. It takes the pause and the wake's kind off each start. This test checks which rows the block
// deletes, which it strips and which it keeps as they were. Three rows it keeps carry what a
// statement matches under another kind, so a statement that lost its kind would take one. The
// migration's tables, its columns and its version row stay too. The release before reads none of
// them, and the roll forward reads them again. The negative control is the header from before its
// three statements were one paragraph. Its way back was the last statement alone, which leaves the
// notices and the stops.
func TestTheProjectPauseWayBackLeavesWhatTheReleaseBeforeReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedStore(t)
	queue := func(kind, payload string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-209', $2::jsonb, 0, now(), '')`, kind, payload); err != nil {
			t.Fatalf("queue a %s row: %v", kind, err)
		}
	}
	const role = "legion-legion-legion-209-implementer"
	queue("controller_notice", `{"kind": "project-paused", "project": "LEGION"}`)
	queue("controller_notice", `{"kind": "project-resumed", "project": "LEGION"}`)
	queue("controller_notice", `{"kind": "tick"}`)
	queue("supervise", `{"op": "pause_stop", "claim": "`+role+`", "pause": 1}`)
	queue("supervise", `{"op": "start", "claim": "`+role+`", "pause": 1, "wakeKind": "rule", "generation": 3}`)
	queue("supervise", `{"op": "start", "claim": "`+role+`", "wakeKind": "rule", "generation": 4}`)
	queue("supervise", `{"op": "start", "claim": "`+role+`", "generation": 5}`)
	queue("supervise", `{"op": "stop", "claim": "`+role+`"}`)
	// What a statement matches, under another kind than the statement's.
	queue("notice", `{"kind": "project-paused"}`)
	queue("dispatch_message", `{"op": "pause_stop", "claim": "`+role+`"}`)
	queue("dispatch_message", `{"op": "start", "pause": 1}`)

	if _, err := store.Pool().Exec(ctx, wayBackOf(t, projectPauseVersion, projectPauseRelease)); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	const queued = `select kind || ' ' || payload::text from outbox order by id`
	if got, want := rowsAsText(t, store, queued), []string{
		`controller_notice {"kind": "tick"}`,
		`supervise {"op": "start", "claim": "` + role + `", "generation": 3}`,
		`supervise {"op": "start", "claim": "` + role + `", "generation": 4}`,
		`supervise {"op": "start", "claim": "` + role + `", "generation": 5}`,
		`supervise {"op": "stop", "claim": "` + role + `"}`,
		`notice {"kind": "project-paused"}`,
		`dispatch_message {"op": "pause_stop", "claim": "` + role + `"}`,
		`dispatch_message {"op": "start", "pause": 1}`,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the outbox after the way back = %q, want %q. The pause's notices and stops go, "+
			"the two keys come off each start, and every other row stays, whatever it carries", got, want)
	}
	const kept = `select (select count(*) from information_schema.tables
		where table_name in ('project_pauses', 'project_pause_stops', 'project_pause_pod_holds'))::text
		|| ' ' || (select count(*) from information_schema.columns where (table_name, column_name) in
			(('pod_resumes', 'clock_at'), ('pod_resumes', 'pause_id'), ('outbox', 'held_by_pause')))::text
		|| ' ' || (select count(*) from schema_version where version = 20261115194357)::text`
	if got := rowsAsText(t, store, kept); !reflect.DeepEqual(got, []string{"3 3 1"}) {
		t.Fatalf("the pause's tables, columns and version rows after the way back = %v, want all three tables, "+
			"all three columns and the row", got)
	}
}
