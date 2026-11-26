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
// (record.DecodeOutboxPayload, DisallowUnknownFields), and then checks its kind and its op. So it
// cannot run the controller's notices of a pause and of a resume, or a pause's stop of a role. Nor
// can it run a start that carries the pause or the wake's kind. The way back deletes the notices
// and the stops, and takes the two keys off each start. Every other row stays as it was. The
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
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the outbox after the way back = %q, want %q. The pause's notices and stops go, "+
			"the two keys come off each start, and every other row stays", got, want)
	}
	const kept = `select (select count(*) from information_schema.tables
		where table_name in ('project_pauses', 'project_pause_stops', 'project_pause_pod_holds'))::text
		|| ' ' || (select count(*) from schema_version where version = 20261115194357)::text`
	if got := rowsAsText(t, store, kept); !reflect.DeepEqual(got, []string{"3 1"}) {
		t.Fatalf("the pause's tables and the migration's version rows after the way back = %v, want all three tables and the row", got)
	}
}
