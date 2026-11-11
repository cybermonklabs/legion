package api

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// writeLegionFacts writes the stored facts of three issues of the project, as the facts consumer
// would have written them, and answers the oldest updated_at of the rows. The rows are written by
// hand rather than computed, so a check reads the numbers it wrote: the board serves stored rows
// and computes none of them, which is the whole of what these tests hold it to.
func writeLegionFacts(t *testing.T, database *store.Store, project string, now time.Time) time.Time {
	t.Helper()
	oldest := now.Add(-4 * time.Minute)
	closed := now.Add(-2 * time.Hour)
	first, last := now.Add(-6*time.Hour), now.Add(-time.Hour)
	rows := []legionfacts.Row{{
		Project: project, Issue: project + "-4", OpenedAt: now.Add(-9 * time.Hour),
		HoursByPhase: map[string]float64{"implementing": 3.5, "reviewing": 1.25},
		WorkHours:    4.75, ReviewRounds: 2, RootKey: project + "-3", TreeDepth: 1,
		SlotHours: 4, WaitingReason: "", WorkFirstAt: &first, WorkLastAt: &last,
		UpdatedAt: now.Add(-2 * time.Minute),
	}, {
		Project: project, Issue: project + "-3", OpenedAt: now.Add(-12 * time.Hour),
		HoursByPhase: map[string]float64{"design": 2},
		WorkHours:    2, ReviewRounds: 0, RootKey: project + "-3", TreeDepth: 0,
		SlotHours: 6, WaitingReason: "waiting for a child", UpdatedAt: oldest,
	}, {
		// A closed issue, which is on no card and carries an entry all the same.
		Project: project, Issue: project + "-7", OpenedAt: now.Add(-30 * time.Hour),
		ClosedAt: &closed, HoursByPhase: map[string]float64{}, WorkHours: 0, RootKey: project + "-7",
		UpdatedAt: now.Add(-3 * time.Minute),
	}}
	if _, err := legionfacts.Write(t.Context(), database.Pool, rows); err != nil {
		t.Fatalf("write the stored facts of %s: %v", project, err)
	}
	return oldest
}

// writeLegionFactsCursor writes the facts consumer's three cursor rows, each stamped at the moment
// given, which is what the header's facts_cursor_age_seconds is measured from.
func writeLegionFactsCursor(t *testing.T, database *store.Store, at time.Time) {
	t.Helper()
	legionExec(t, database, `
		insert into issue_facts_cursor (name, position, updated_at)
		select name, 1, $2 from unnest($1::text[]) as given(name)
	`, []string{"events", "legion_observations", "delivery_changes"}, at)
}

// TestTheBoardCarriesOneStoredEntryPerIssueLegionWorked: the issues map is the project's stored
// rows, one entry for every issue with Legion phase work and none for an issue no Legion role
// worked, open and closed alike and whatever an issue's hours are. Each entry carries the hours,
// the work time, the review rounds, the root and the depth in the tree the row holds, because the
// board serves the row and computes nothing.
func TestTheBoardCarriesOneStoredEntryPerIssueLegionWorked(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	// Five trees, because this issue's check k4 counts at least twenty entries and fails below
	// them: a seed that answers fifteen proves the rule and not the check.
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: "LEGION", Trees: 5, Now: now})
	nameLegionDaemon(t, handler, fixture.Project, "http://legion-legion.internal:13370")
	rows, err := legionfacts.ComputeProject(t.Context(), database.Pool, fixture.Project, now)
	if err != nil {
		t.Fatalf("compute the project's facts: %v", err)
	}
	if _, err := legionfacts.Write(t.Context(), database.Pool, rows); err != nil {
		t.Fatalf("write the project's facts: %v", err)
	}
	stored, err := legionfacts.Read(t.Context(), database.Pool, fixture.Project)
	if err != nil {
		t.Fatalf("read the facts back: %v", err)
	}

	board := legionBoardOf(t, handler, fixture.Project)
	for _, key := range fixture.PhaseWork {
		if _, has := board.Issues[key]; !has {
			t.Errorf("%s carries Legion phase work and the board holds no entry of it", key)
		}
	}
	for _, key := range fixture.NoPhaseWork {
		if entry, has := board.Issues[key]; has {
			t.Errorf("the board holds the entry %+v for %s, which no Legion role worked", entry, key)
		}
	}
	if len(board.Issues) != len(fixture.PhaseWork) {
		t.Errorf("the board holds %d entries, want the %d issues with Legion phase work",
			len(board.Issues), len(fixture.PhaseWork))
	}
	// Every entry is the stored row, value for value.
	for _, key := range slices.Sorted(maps.Keys(board.Issues)) {
		entry, row := board.Issues[key], stored[key]
		if !maps.Equal(entry.HoursByPhase, row.HoursByPhase) {
			t.Errorf("%s: the board's hours = %v, the stored row's = %v", key, entry.HoursByPhase, row.HoursByPhase)
		}
		if entry.WorkHours != row.WorkHours || entry.ReviewRounds != row.ReviewRounds {
			t.Errorf("%s: the board says %g work hours over %d review rounds, the stored row %g over %d",
				key, entry.WorkHours, entry.ReviewRounds, row.WorkHours, row.ReviewRounds)
		}
		if entry.RootKey != row.RootKey || entry.TreeDepth != row.TreeDepth {
			t.Errorf("%s: the board puts it at depth %d of %s, the stored row at depth %d of %s",
				key, entry.TreeDepth, entry.RootKey, row.TreeDepth, row.RootKey)
		}
		if !entry.OpenedAt.Equal(row.OpenedAt) || !entry.UpdatedAt.Equal(row.UpdatedAt) {
			t.Errorf("%s: the board's moments = %s/%s, the stored row's = %s/%s",
				key, entry.OpenedAt, entry.UpdatedAt, row.OpenedAt, row.UpdatedAt)
		}
	}
	// Open and closed issues both carry an entry, and so does an issue whose work took no
	// measured time.
	var open, closed int
	for _, entry := range board.Issues {
		if entry.ClosedAt == nil {
			open++
			continue
		}
		closed++
	}
	if open == 0 || closed == 0 {
		t.Errorf("the board holds %d open and %d closed entries, want both kinds", open, closed)
	}
	zeroHour := 0
	for _, key := range fixture.NoMeasuredWork {
		entry, has := board.Issues[key]
		if !has {
			t.Errorf("%s took no measured time and the board holds no entry of it", key)
			continue
		}
		zeroHour++
		if len(entry.HoursByPhase) != 0 {
			t.Errorf("%s took no measured time and the board gives it the hours %v", key, entry.HoursByPhase)
		}
	}
	// The floor this issue's check k4 counts against: a board of fewer entries proves the rule
	// and not the check, so the seed above is what must grow when this fails.
	if len(board.Issues) < 20 {
		t.Errorf("the board carries %d entries, want the 20 check k4 counts against", len(board.Issues))
	}
	t.Logf("project %s: %d issues seeded, %d with Legion phase work, %d with none; the board carries %d entries, %d open, %d closed, %d with no measured hours",
		fixture.Project, len(fixture.Keys), len(fixture.PhaseWork), len(fixture.NoPhaseWork),
		len(board.Issues), open, closed, zeroHour)
}

// TestTheBoardPushedLiveIsTheBoardTheGETAnswers: the body a subscriber is pushed is the body the
// GET route answers, field by field, for a board whose issues map carries stored rows. The issues
// map is sent entry by entry (live_frames.go's maps class), so this is what holds that encoding to
// the one answer the route gives.
//
// computed_at is left out of the comparison: it is the moment of the compute, and the two answers
// are computed at two moments.
func TestTheBoardPushedLiveIsTheBoardTheGETAnswers(t *testing.T) {
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	now := rig.clock.Now()
	seedLegionBoard(t, rig.database, "INFRA", now)
	writeLegionFacts(t, rig.database, "INFRA", now)

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("b", legionView("INFRA")))
	pushed := conn.snapshot(t, 5*time.Second, "b").Body
	answered := getBody(t, rig.handler, windowTarget(t, legionBoardView, pushed, ""))

	live, served := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	if err := json.Unmarshal(pushed, &live); err != nil {
		t.Fatalf("read the pushed body: %v", err)
	}
	if err := json.Unmarshal(answered, &served); err != nil {
		t.Fatalf("read the answered body: %v", err)
	}
	if got, want := slices.Sorted(maps.Keys(live)), slices.Sorted(maps.Keys(served)); !slices.Equal(got, want) {
		t.Fatalf("the pushed body writes the fields %v and the GET route's %v", got, want)
	}
	compared, differed := 0, 0
	for _, name := range slices.Sorted(maps.Keys(live)) {
		if name == "computed_at" {
			continue
		}
		compared++
		if !bytes.Equal(live[name], served[name]) {
			differed++
			t.Errorf("%s: the pushed body holds %s and the GET route's %s", name, live[name], served[name])
		}
	}
	entries := map[string]json.RawMessage{}
	if err := json.Unmarshal(live["issues"], &entries); err != nil {
		t.Fatalf("read the pushed body's issues: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the pushed board carries no issues entry, so the comparison proves nothing of the map")
	}
	t.Logf("a board of %d issues entries: %d fields compared, %d differed", len(entries), compared, differed)
}

// TestTheBoardHeaderTellsNoLegionWorkFromNoBackfill: the header's two facts fields are read apart,
// so a reader can tell a project with no Legion work from a project whose backfill has not run.
// A project with no Legion work has no stored row and a cursor that has moved: facts_updated_at is
// null and facts_cursor_age_seconds is an age. A project whose consumer has never run has no
// cursor row at all, and the age is null for every project of that deployment.
func TestTheBoardHeaderTellsNoLegionWorkFromNoBackfill(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA", "LINUX")
	for _, project := range []string{"INFRA", "LINUX"} {
		nameLegionDaemon(t, handler, project, "http://legion-"+project+".internal:13370")
	}
	// timestamptz holds microseconds, so the moments are written at that resolution and the rows
	// come back as they went in.
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedLegionBoard(t, database, "INFRA", now)
	oldest := writeLegionFacts(t, database, "INFRA", now)

	// No pass has ever committed: neither project's board knows an age, and the project with
	// stored rows still names the oldest of them.
	worked, idle := legionBoardOf(t, handler, "INFRA"), legionBoardOf(t, handler, "LINUX")
	if worked.Header.FactsUpdatedAt == nil || !worked.Header.FactsUpdatedAt.Equal(oldest) {
		t.Errorf("INFRA's facts_updated_at = %v, want the oldest stored row's %s", worked.Header.FactsUpdatedAt, oldest)
	}
	if worked.Header.FactsCursorAgeSeconds != nil || idle.Header.FactsCursorAgeSeconds != nil {
		t.Errorf("the cursor ages = %v and %v before any pass, want null for both",
			worked.Header.FactsCursorAgeSeconds, idle.Header.FactsCursorAgeSeconds)
	}
	if idle.Header.FactsUpdatedAt != nil {
		t.Errorf("LINUX's facts_updated_at = %v, want null: it has no stored row", idle.Header.FactsUpdatedAt)
	}

	// A pass has run and moved the cursors 90 seconds ago. LINUX now reads as a project with no
	// Legion work rather than as one nothing has computed yet.
	writeLegionFactsCursor(t, database, now.Add(-90*time.Second))
	worked, idle = legionBoardOf(t, handler, "INFRA"), legionBoardOf(t, handler, "LINUX")
	for _, board := range []legionBoardResponse{worked, idle} {
		age := board.Header.FactsCursorAgeSeconds
		if age == nil || *age < 89 || *age > 150 {
			t.Errorf("%s's facts_cursor_age_seconds = %v, want about the 90 seconds since the pass", board.Project, age)
		}
	}
	if worked.Header.FactsUpdatedAt == nil || !worked.Header.FactsUpdatedAt.Equal(oldest) {
		t.Errorf("INFRA's facts_updated_at = %v, want the oldest stored row's %s", worked.Header.FactsUpdatedAt, oldest)
	}
	if idle.Header.FactsUpdatedAt != nil {
		t.Errorf("LINUX's facts_updated_at = %v, want null: it has no stored row", idle.Header.FactsUpdatedAt)
	}
	t.Logf("before any pass: INFRA %d rows and no age, LINUX no row and no age — nothing has been computed. After one pass 90 s ago: INFRA %d rows and an age of %d s, LINUX no row and an age of %d s — LINUX has no Legion work, and its facts are as fresh as INFRA's",
		len(worked.Issues), len(worked.Issues), *worked.Header.FactsCursorAgeSeconds, *idle.Header.FactsCursorAgeSeconds)
}

// TestTheLegionBoardsOldAddressIsGone: the board answers under the project path and nowhere else,
// which is the one address the repository's API shape rule leaves.
func TestTheLegionBoardsOldAddressIsGone(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	seedLegionBoard(t, database, "INFRA", time.Now().UTC())

	for _, gone := range []string{"/api/v1/legion/board", "/api/v1/legion/board?project=INFRA"} {
		response := dispatchRequest(t, handler, http.MethodGet, gone, nil, "alice")
		if response.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404: the address is gone", gone, response.Code)
		}
	}
	board := legionBoardOf(t, handler, "INFRA")
	if board.Project != "INFRA" {
		t.Fatalf("the project-path board names %q, want INFRA", board.Project)
	}
	if len(board.Columns) != len(legionBoardColumnOrder) {
		t.Fatalf("the project-path board has %d columns, want the %d the view draws",
			len(board.Columns), len(legionBoardColumnOrder))
	}
	cards := 0
	for _, column := range board.Columns {
		cards += len(column.Cards)
	}
	t.Logf("GET /api/v1/legion/board answers 404 at both of its old spellings; GET /api/v1/projects/INFRA/legion/board answers %d columns holding %d cards",
		len(board.Columns), cards)
}
