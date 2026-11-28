package api

import (
	"bytes"
	"encoding/json"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// writeLegionFacts writes the stored facts of four issues of seedLegionBoard's project INFRA, as
// the facts consumer would have written them, and answers the oldest updated_at of the rows that
// board serves. Each open row is closed at a moment before now with the growth the computation
// would store: INFRA-4, the child in a turn, grows its work time, its slot hours, its implementing
// hours and the end of its open bar, and facts the board does not carry (its hours in backlog or
// icebox, its implementer's bar, its reviewer's idle hours); INFRA-8, the unstarted child, grows
// hours in the queue column it holds none in yet; and INFRA-3, the design root, grows only its
// hours in backlog or icebox while it waits for a child, a fact the board does not carry, so every
// value of its entry stands still. The fourth row, which the board does not serve, is the oldest
// of the four, so a header that counted it would read an older facts_updated_at than the one
// answered. The rows are written by hand rather than computed, so a check reads the numbers it
// wrote: the board serves stored rows and computes none of them, which is the whole of what these
// tests hold it to.
func writeLegionFacts(t *testing.T, database *store.Store, now time.Time) time.Time {
	t.Helper()
	oldest := now.Add(-4 * time.Minute)
	closed := now.Add(-2 * time.Hour)
	childAsOf, queuedAsOf, rootAsOf := now.Add(-3*time.Hour), now.Add(-2*time.Hour), now.Add(-5*time.Hour)
	childFirst, queuedFirst := now.Add(-6*time.Hour), now.Add(-4*time.Hour)
	rows := []legionfacts.Row{{
		Project: "INFRA", Issue: "INFRA-4", UpdatedAt: now.Add(-2 * time.Minute),
		ClosedFacts: metrics.ClosedFacts{
			IssueFacts: metrics.IssueFacts{
				OpenedAt:   now.Add(-9 * time.Hour),
				PhaseHours: map[string]float64{"implementing": 3.5, "reviewing": 1.25},
				WorkHours:  4.75, ReviewRounds: 2, Tree: "INFRA-3", TreeDepth: 1, SlotHours: 4,
				WorkFirstAt: &childFirst, WorkLastAt: &childAsOf,
			},
			AsOf: childAsOf,
			Growth: metrics.Growth{
				"work_hours": 1, "slot_hours": 1, "phase_hours.implementing": 1, "work_last_at": 1,
				"backlog_or_icebox_hours": 1, "bars.implementer.hours": 1, "idle_by_role.reviewer": 1,
			},
		},
	}, {
		Project: "INFRA", Issue: "INFRA-8", UpdatedAt: now.Add(-3 * time.Minute),
		ClosedFacts: metrics.ClosedFacts{
			IssueFacts: metrics.IssueFacts{
				OpenedAt: now.Add(-7 * time.Hour), PhaseHours: map[string]float64{}, Tree: "INFRA-3", TreeDepth: 1,
				WorkFirstAt: &queuedFirst, WorkLastAt: &queuedAsOf,
			},
			AsOf:   queuedAsOf,
			Growth: metrics.Growth{"phase_hours.queue": 1},
		},
	}, {
		Project: "INFRA", Issue: "INFRA-3", WaitingReason: "waiting for a child", UpdatedAt: oldest,
		ClosedFacts: metrics.ClosedFacts{
			IssueFacts: metrics.IssueFacts{
				OpenedAt: now.Add(-12 * time.Hour), PhaseHours: map[string]float64{"design": 2},
				WorkHours: 2, Root: true, Tree: "INFRA-3", SlotHours: 6,
			},
			AsOf:   rootAsOf,
			Growth: metrics.Growth{"backlog_or_icebox_hours": 1},
		},
	}, {
		// A closed issue of a tree no card stands in (seedLegionBoard's finished issue), which
		// carries no entry and moves no facts_updated_at, though it is the oldest row: the board
		// serves the facts of the open issues and of the issues it shows alone.
		Project: "INFRA", Issue: "INFRA-7", UpdatedAt: now.Add(-5 * time.Minute),
		ClosedFacts: metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{
			OpenedAt: now.Add(-30 * time.Hour), ClosedAt: &closed, PhaseHours: map[string]float64{}, WorkHours: 0,
			Root: true, Tree: "INFRA-7",
		}},
	}}
	if _, err := legionfacts.WriteProject(t.Context(), database.Pool,
		legionfacts.ProjectWrite{Project: "INFRA", Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the stored facts of INFRA: %v", err)
	}
	return oldest
}

// writeLegionFactsCursor writes the cursor row of each of the facts consumer's three feeds, each
// stamped at the moment given, which is what the header's facts_cursor_age_seconds is measured
// from (the row of the observations read ahead of a held one is left out: CursorAge reads the
// oldest stamp of the rows there are).
func writeLegionFactsCursor(t *testing.T, database *store.Store, at time.Time) {
	t.Helper()
	legionExec(t, database, `
		insert into issue_facts_cursor (name, position, updated_at)
		select name, 1, $2 from unnest($1::text[]) as given(name)
	`, []string{"events", "legion_observations", "delivery_changes"}, at)
}

// TestEveryStoredColumnIsReadOrNamedAsNotRead inventories the migrated table against the stored
// row and the board's wire shape. A new column must have a reader or a reason to stay off the board.
func TestEveryStoredColumnIsReadOrNamedAsNotRead(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	rows, err := database.Pool.Query(t.Context(), `
		select column_name from information_schema.columns
		where table_schema = current_schema() and table_name = 'issue_facts'
		order by ordinal_position
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	// These columns are not fields of the board entry. Most still have a reader in the stored row.
	offBoard := map[string]string{
		"project_key": "Project", "issue_key": "Issue",
		"backlog_or_icebox_hours": "ParkedHours", "queued_hours": "QueuedHours",
		"started_at": "StartedAt", "merged_at": "MergedAt", "bars": "Bars",
		"idle_by_role": "IdleByRole", "slot_wait_hours": "SlotWaitHours",
		"sent_back": "SentBack", "implementing_after_first_review": "ImplementingHoursAfterFirstReview",
		"testing_after_first_review": "TestingHoursAfterFirstReview", "children": "Children", "repo": "Repo",
	}
	// The rollout uses this column in SQL, so neither read needs it as a Go field.
	const rolloutColumn = "written_build"
	boardFields := map[string]bool{}
	for _, field := range liveJSONFields(reflect.TypeFor[legionBoardFacts]()) {
		boardFields[field.name] = true
	}
	seen := map[string]bool{}
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		seen[column] = true
		switch {
		case boardFields[column] || column == rolloutColumn:
		case offBoard[column] != "":
			if _, ok := reflect.TypeFor[legionfacts.Row]().FieldByName(offBoard[column]); !ok {
				t.Errorf("issue_facts.%s names missing legionfacts.Row field %s", column, offBoard[column])
			}
		default:
			t.Errorf("issue_facts.%s is neither read nor named as not read", column)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for column := range offBoard {
		if !seen[column] {
			t.Errorf("issue_facts has no column %s, which the inventory lists", column)
		}
	}
	for column := range boardFields {
		if !seen[column] {
			t.Errorf("the board exposes %s, but issue_facts has no such column", column)
		}
	}
	if !seen[rolloutColumn] {
		t.Errorf("issue_facts has no column %s, which the rollout reads", rolloutColumn)
	}
}

// TestTheBoardCarriesOneStoredEntryPerIssueLegionWorked: over the facts' own rig, whose every
// tree holds an open child on a card, the board shows every tree, so the issues map is the
// project's stored rows: one entry for every issue with Legion phase work and none for an issue no
// Legion role worked, open and closed alike, whatever an issue's hours are, on a card or not.
// Each entry carries the hours, the work time, the review rounds, the root and the depth in the
// tree the row holds as of the moment of the read, because the board serves the row and computes
// nothing but the close of each open part at its own clock (legionfacts.Row.At).
func TestTheBoardCarriesOneStoredEntryPerIssueLegionWorked(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	// Five trees, because this issue's check k4 counts at least twenty entries and fails below
	// them: a seed that answers fifteen proves the rule and not the check.
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: "LEGION", Trees: 5, Now: now})
	if len(fixture.PhaseWork) == 0 || len(fixture.NoPhaseWork) == 0 || len(fixture.NoMeasuredWork) == 0 {
		t.Fatal("the seeded history must exercise issues with phase work, without phase work, and with no measured hours")
	}
	nameLegionDaemon(t, handler, fixture.Project, "http://legion-legion.internal:13370")
	rows, _, err := legionfacts.ComputeProject(t.Context(), database.Pool, fixture.Project, now)
	if err != nil {
		t.Fatalf("compute the project's facts: %v", err)
	}
	// Give every row a known write time so the expected entry does not have to read it back.
	for i := range rows {
		rows[i].UpdatedAt = now
	}
	if _, err := legionfacts.WriteProject(t.Context(), database.Pool,
		legionfacts.ProjectWrite{Project: fixture.Project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the project's facts: %v", err)
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
	// The expected values come from the rows given to the writer, not the board's read functions.
	for _, row := range rows {
		entry, found := board.Issues[row.Issue]
		if !found {
			t.Errorf("the board has no entry for stored row %s", row.Issue)
			continue
		}
		elapsed := max(0, board.ComputedAt.Sub(row.AsOf)).Hours()
		hours := map[string]float64{}
		for phase, value := range row.PhaseHours {
			hours[phase] = value
		}
		for name, rate := range row.Growth {
			if phase, ok := strings.CutPrefix(name, "phase_hours."); ok {
				hours[phase] += elapsed * rate
				if math.Round(hours[phase]*1e4) <= 0 {
					delete(hours, phase)
				}
			}
		}
		if len(entry.HoursByPhase) != len(hours) {
			t.Errorf("%s: phase hours = %v, want %v", row.Issue, entry.HoursByPhase, hours)
		}
		for phase, want := range hours {
			if got, held := entry.HoursByPhase[phase]; !held || math.Abs(got-want) > storedPrecision {
				t.Errorf("%s: %s hours = %g (present: %t), want %g", row.Issue, phase, got, held, want)
			}
		}
		wantWork := row.WorkHours + elapsed*row.Growth["work_hours"]
		if math.Abs(entry.WorkHours-wantWork) > storedPrecision || entry.ReviewRounds != row.ReviewRounds {
			t.Errorf("%s: work hours and review rounds = %g/%d, want %g/%d", row.Issue, entry.WorkHours, entry.ReviewRounds, wantWork, row.ReviewRounds)
		}
		if entry.RootKey != row.Tree || entry.TreeDepth != row.TreeDepth {
			t.Errorf("%s: tree and depth = %s/%d, want %s/%d", row.Issue, entry.RootKey, entry.TreeDepth, row.Tree, row.TreeDepth)
		}
		if !entry.OpenedAt.Equal(row.OpenedAt) || !entry.UpdatedAt.Equal(row.UpdatedAt) {
			t.Errorf("%s: opened and updated = %s/%s, want %s/%s", row.Issue, entry.OpenedAt, entry.UpdatedAt, row.OpenedAt, row.UpdatedAt)
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
	if len(board.Issues) < boardEntriesFloor {
		t.Errorf("the board carries %d entries, want the %d check k4 counts against",
			len(board.Issues), boardEntriesFloor)
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
// The push and GET use different clocks. Compare fields other than computed_at and issues as
// written, then grow the push's issues entries to the GET's moment before comparing the map.
func TestTheBoardPushedLiveIsTheBoardTheGETAnswers(t *testing.T) {
	t.Parallel()
	rig := newClockedHubRig(t, &fakeHubClock{offset: -time.Hour})
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	now := rig.clock.Now().Truncate(time.Microsecond)
	seedLegionBoard(t, rig.database, "INFRA", now)
	writeLegionFacts(t, rig.database, now)

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("b", legionView("INFRA")))
	pushed := conn.snapshot(t, "b").Body
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
		if name == "computed_at" || name == "issues" {
			continue
		}
		compared++
		if !bytes.Equal(live[name], served[name]) {
			differed++
			t.Errorf("%s: the pushed body holds %s and the GET route's %s", name, live[name], served[name])
		}
	}
	var snapshot, get legionBoardResponse
	if err := json.Unmarshal(pushed, &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(answered, &get); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(slices.Sorted(maps.Keys(snapshot.Issues)), slices.Sorted(maps.Keys(get.Issues))) {
		t.Fatalf("push carries %v, GET carries %v", slices.Sorted(maps.Keys(snapshot.Issues)), slices.Sorted(maps.Keys(get.Issues)))
	}
	grew := 0
	for key, held := range snapshot.Issues {
		if (held.Growth == nil) != (held.AsOf == nil) {
			t.Errorf("%s: snapshot must give a growth rate and as_of together", key)
			continue
		}
		want := get.Issues[key]
		atRead := closeLegionEntry(held, get.ComputedAt)
		if held.Growth != nil && (atRead.WorkHours != held.WorkHours || !maps.Equal(atRead.HoursByPhase, held.HoursByPhase)) {
			grew++
		}
		checkLegionEntry(t, key, want, atRead)
	}
	if grew == 0 {
		t.Fatal("no growing entry was compared at the GET's clock")
	}
	t.Logf("compared %d fields (%d differences) and %d issues entries (%d grew)", compared, differed, len(snapshot.Issues), grew)
}

// TestTheBoardCarriesTheFactsOfTheIssuesItShowsAndNoOther: the issues map holds the stored facts
// of every open issue with Legion phase work and of every issue the board shows, and of no other
// issue (dispatch://INFRA-2916, R22): the issue of every card, every issue of a tree a card stands
// in, whose stored root is that card's tree, and every open row. So a child of a shown tree the
// daemon names on no card, or names nowhere at all, carries its entry, and so does an open issue
// on no card in no shown tree, one in backlog or icebox, whose facts no other route serves; a
// closed issue of a tree the daemon no longer shows carries none, nor does any closed issue of a
// tree the daemon never showed: the board's body follows the open work and what the daemon runs
// rather than the project's history.
//
// The header's facts_updated_at is the oldest updated_at of the rows the answer serves, so a row
// the board does not serve, the oldest of the project's, moves it not at all.
func TestTheBoardCarriesTheFactsOfTheIssuesItShowsAndNoOther(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC().Truncate(time.Microsecond)
	// The board seedLegionBoard draws: cards for INFRA-10, -1 and -2 (the queue), -3 (design), its
	// child -4 (implementing), -5 (with a person) and -6 (held); INFRA-7 is done and on no card,
	// the root of a tree no card stands in, and INFRA-8 is -3's child its tree has not started.
	seedLegionBoard(t, database, "INFRA", now)
	closed := now.Add(-26 * time.Hour)
	row := func(issue, root string, depth int, updated time.Duration, closedAt *time.Time) legionfacts.Row {
		return legionfacts.Row{
			Project: "INFRA", Issue: issue, UpdatedAt: now.Add(-updated),
			ClosedFacts: metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{
				OpenedAt: now.Add(-48 * time.Hour), ClosedAt: closedAt,
				PhaseHours: map[string]float64{"implementing": 1}, WorkHours: 1, Root: root == issue, Tree: root,
				TreeDepth: depth,
			}},
		}
	}
	rows := []legionfacts.Row{
		row("INFRA-3", "INFRA-3", 0, 5*time.Minute, nil),
		row("INFRA-4", "INFRA-3", 1, 4*time.Minute, nil),
		row("INFRA-6", "INFRA-6", 0, 3*time.Minute, nil),
		// The daemon shows this issue, but not the root of its stored row. The closed read must
		// include it by card key, not by root.
		row("INFRA-5", "INFRA-50", 1, 3*time.Minute, &closed),
		// A child of a shown tree the daemon names on no card.
		row("INFRA-8", "INFRA-3", 1, 2*time.Minute, nil),
		// A child of a shown tree the daemon names nowhere: it closed before the daemon's reads.
		row("INFRA-12", "INFRA-3", 1, 6*time.Minute, &closed),
		// A closed issue of a tree the daemon no longer shows: done, on no card, its own root.
		row("INFRA-7", "INFRA-7", 0, 7*time.Minute, &closed),
		// A tree the daemon never showed, the oldest rows of the project.
		row("INFRA-20", "INFRA-20", 0, 9*time.Minute, &closed),
		row("INFRA-21", "INFRA-20", 1, 8*time.Minute, &closed),
		// An open issue on no card and in no shown tree: in backlog or icebox, its own root.
		row("INFRA-30", "INFRA-30", 0, time.Minute, nil),
	}
	if _, err := legionfacts.WriteProject(t.Context(), database.Pool,
		legionfacts.ProjectWrite{Project: "INFRA", Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the stored facts of INFRA: %v", err)
	}

	board := legionBoardOf(t, handler, "INFRA")
	want := []string{"INFRA-12", "INFRA-3", "INFRA-30", "INFRA-4", "INFRA-5", "INFRA-6", "INFRA-8"}
	got := slices.Sorted(maps.Keys(board.Issues))
	if !slices.Equal(got, want) {
		t.Errorf("the board carries the entries of %v, want those of the open issues and the issues it shows, %v", got, want)
	}
	if _, carded := legionCardHeld(board, "INFRA-30"); carded {
		t.Error("INFRA-30 has a card of its own, so its entry proves nothing of an open issue on no card")
	}
	if _, carded := legionCardHeld(board, "INFRA-8"); carded {
		t.Error("INFRA-8 has a card of its own, so its entry proves nothing of a child on no card")
	}
	if _, carded := legionCardHeld(board, "INFRA-5"); !carded {
		t.Error("INFRA-5 must have a card to exercise the closed read by card key")
	}
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			if card.Tree == "INFRA-50" {
				t.Error("the board shows INFRA-50's tree, so INFRA-5 can be read by root")
			}
		}
	}
	if entry := board.Issues["INFRA-5"]; entry.ClosedAt == nil || !entry.ClosedAt.Equal(closed) || entry.RootKey != "INFRA-50" {
		t.Errorf("INFRA-5 = %+v, want the closed row rooted at INFRA-50", entry)
	}
	oldestShown := now.Add(-6 * time.Minute)
	if board.Header.FactsUpdatedAt == nil || !board.Header.FactsUpdatedAt.Equal(oldestShown) {
		t.Errorf("facts_updated_at = %v, want %s, the oldest updated_at of the rows the board serves",
			board.Header.FactsUpdatedAt, oldestShown)
	}
	t.Logf("the project stores %d rows; the board shows %d cards and carries %d entries %v, the oldest updated %s",
		len(rows), countBoardCards(board), len(board.Issues), got, board.Header.FactsUpdatedAt)
}

// TestTheBoardHeaderTellsNoLegionWorkFromNoBackfill: the header's two facts fields are read apart,
// so a reader can tell a project with no Legion work from a project whose backfill has not run.
// A project with no Legion work has no stored row and a cursor that has moved: facts_updated_at is
// null and facts_cursor_age_seconds is an age. A project whose consumer has never run has no
// cursor row at all, and the age is null for every project of that deployment.
func TestTheBoardHeaderTellsNoLegionWorkFromNoBackfill(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA", "LINUX")
	for _, project := range []string{"INFRA", "LINUX"} {
		nameLegionDaemon(t, handler, project, "http://legion-"+project+".internal:13370")
	}
	// timestamptz holds microseconds, so the moments are written at that resolution and the rows
	// come back as they went in.
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedLegionBoard(t, database, "INFRA", now)
	oldest := writeLegionFacts(t, database, now)

	// No pass has ever committed: neither project's board knows an age, and the project with
	// stored rows still names the oldest of them.
	worked, idle := legionBoardOf(t, handler, "INFRA"), legionBoardOf(t, handler, "LINUX")
	beforePass := len(worked.Issues)
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
	cursorAt := now.Add(-90 * time.Second)
	writeLegionFactsCursor(t, database, cursorAt)
	worked, idle = legionBoardOf(t, handler, "INFRA"), legionBoardOf(t, handler, "LINUX")
	for _, board := range []legionBoardResponse{worked, idle} {
		requireLegionCursorAge(t, board, cursorAt)
	}
	if worked.Header.FactsUpdatedAt == nil || !worked.Header.FactsUpdatedAt.Equal(oldest) {
		t.Errorf("INFRA's facts_updated_at = %v, want the oldest stored row's %s", worked.Header.FactsUpdatedAt, oldest)
	}
	if idle.Header.FactsUpdatedAt != nil {
		t.Errorf("LINUX's facts_updated_at = %v, want null: it has no stored row", idle.Header.FactsUpdatedAt)
	}
	t.Logf("before any pass: INFRA %d rows and no age, LINUX no row and no age — nothing has been computed. After one pass 90 s ago: INFRA %d rows and an age of %s s, LINUX no row and an age of %s s — LINUX has no Legion work, and its facts are as fresh as INFRA's",
		beforePass, len(worked.Issues), ageSeconds(worked.Header.FactsCursorAgeSeconds),
		ageSeconds(idle.Header.FactsCursorAgeSeconds))
}

func TestTheHeaderAnswersTheOldestCursorsAge(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC().Truncate(time.Microsecond)
	oldest := now.Add(-4 * time.Minute)
	legionExec(t, database, `
		insert into issue_facts_cursor (name, position, updated_at) values
		('events', 1, $1), ('legion_observations', 1, $2), ('delivery_changes', 1, $3)
	`, now.Add(-time.Minute), oldest, now.Add(-2*time.Minute))
	requireLegionCursorAge(t, legionBoardOf(t, handler, "INFRA"), oldest)
}

// The board gives the clock it used to read the cursor, so the test needs no wall-clock window.
func requireLegionCursorAge(t *testing.T, board legionBoardResponse, cursorAt time.Time) {
	t.Helper()
	want := int(board.ComputedAt.Sub(cursorAt).Seconds())
	if got := board.Header.FactsCursorAgeSeconds; got == nil || *got != want {
		t.Errorf("%s's facts_cursor_age_seconds = %s, want %d at the board's clock", board.Project, ageSeconds(got), want)
	}
}

// ageSeconds is a header's facts_cursor_age_seconds for a log line: the seconds it holds, or
// "no" where it holds none. The checks above are not fatal, so a line that dereferenced the
// pointer would panic on a failure and take the package's whole run with it.
func ageSeconds(age *int) string {
	if age == nil {
		return "no"
	}
	return strconv.Itoa(*age)
}

// legionCardHeld is the card for key and whether the board holds one at all, which the unstarted
// child below turns on: an issue the board draws no card for is on no column.
func legionCardHeld(board legionBoardResponse, key string) (legionBoardCard, bool) {
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			if card.Key == key {
				return card, true
			}
		}
	}
	return legionBoardCard{}, false
}

// TestACardsWaitingReasonEqualsTheStoredRowsWithAndWithoutAnAdmissionBlock: one issue's card and
// one issue's stored row answer the same reason, because both read the one waiting line
// (legion.WaitingLine), the one column rule (legion.ColumnOf) and the one card rule
// (legion.CardWaiting).
//
// The case that broke is a project whose daemon recorded no admission block: the board queues
// every carded `admitted` issue holding no slot in Dispatch's rank order, so its card reads
// `ready`, while the stored row read no waiting line at all. The fallback is in the column rule
// now (legion.QueuedUnread), so both readers answer it. The second project has a block, which is
// what holds the first case to a rule rather than to a coincidence of two empty answers.
//
// The fourth issue is the case the two readers answered differently after that: an `admitted`
// child holding no slot that stands under a card. The board draws it no card and counts it on
// the card above it, so it waits for nothing, while the stored row read the fallback for an
// issue of its own and said `ready`. One line answers both now, with a block recorded and
// without one.
//
// The last is a parent in `awaiting_children`, the root of a tree of the facts' own rig whose root
// waited on its children (legiontest.Spec's AwaitingChildren). It stands in its own column, out of
// the line and out of the Queue, and both readers answer `blocked` for it: on the open child the
// block names, and on none where no block was read.
func TestACardsWaitingReasonEqualsTheStoredRowsWithAndWithoutAnAdmissionBlock(t *testing.T) {
	t.Parallel()
	// child is the key of the unstarted child, which carries no card and whose stored row must
	// say it waits for nothing, and above is the card that counts it. They are the same two
	// issues in both cases below, because neither admission block names either of them. parent
	// and openChild are the rig's root and its open child, the first and the fourth issue of a
	// tree the rig numbers from 11, after the issues below (legiontest.Spec's IssueOffset).
	const child, above = "INFRA-4", "INFRA-3"
	const parent, openChild = "INFRA-11", "INFRA-14"
	for _, test := range []struct {
		what      string
		admission string
		want      map[string]string
	}{
		{
			what: "no read recorded an admission block",
			want: map[string]string{
				"INFRA-1": legion.WaitingReady, "INFRA-2": legion.WaitingReady, "INFRA-3": "", parent: legion.WaitingBlocked,
			},
		},
		{
			what: "a read recorded one",
			admission: `{"cap": 2, "active": [], "waiting": ["INFRA-1"],
				"blocked": [{"issue": "INFRA-2", "blockedBy": ["INFRA-9"]}, {"issue": "` + parent + `", "blockedBy": ["` + openChild + `"]}]}`,
			want: map[string]string{
				"INFRA-1": legion.WaitingReady, "INFRA-2": legion.WaitingBlocked, "INFRA-3": "", parent: legion.WaitingBlocked,
			},
		},
	} {
		t.Run(test.what, func(t *testing.T) {
			handler, database := newTestHandlerWithStore(t)
			seedLegionProjects(t, handler, "INFRA")
			nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
			now := time.Now().UTC()
			hourAgo := now.Add(-time.Hour)
			// Three roots the daemon reports, each with a handoff, which is what gives an issue
			// a stored row (legion/metrics/population.go's hasPhaseWork). Two wait for a slot
			// and the third works. The fourth issue is a child of the third, which the daemon
			// reports `admitted` with no slot of its own: its tree has not started it.
			for _, issue := range []struct {
				number              int
				rank, phase, parent string
			}{
				{1, "a", legion.PhaseAdmitted, ""},
				{2, "b", legion.PhaseAdmitted, ""},
				{3, "c", legion.PhaseImplementing, ""},
				{4, "d", legion.PhaseAdmitted, "INFRA-3"},
			} {
				key := seedLegionIssue(t, database, "INFRA", issue.number, "An issue in "+issue.phase, issue.rank, nil, issue.parent)
				seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"`+issue.phase+`"`, hourAgo)
				seedLegionHandoff(t, database, key, "plan", hourAgo)
			}
			if root, open := seedAwaitingChildrenTree(t, database, now, now.Add(-30*time.Minute)); root != parent || open != openChild {
				t.Fatalf("the rig's tree has the root %s and the open child %s, want %s and %s", root, open, parent, openChild)
			}
			if test.admission != "" {
				seedLegionObservation(t, database, "INFRA", legion.AdmissionKey, legion.FieldAdmission, test.admission, hourAgo)
			}
			seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

			// The stored rows as the facts consumer writes them, which is the one computation of
			// a waiting_reason (legionfacts.ComputeProject).
			rows, _, err := legionfacts.ComputeProject(t.Context(), database.Pool, "INFRA", now)
			if err != nil {
				t.Fatalf("compute the project's facts: %v", err)
			}
			if _, err := legionfacts.WriteProject(t.Context(), database.Pool,
				legionfacts.ProjectWrite{Project: "INFRA", Rows: rows, Whole: true}); err != nil {
				t.Fatalf("write the facts: %v", err)
			}

			board := legionBoardOf(t, handler, "INFRA")
			for _, key := range slices.Sorted(maps.Keys(test.want)) {
				card := legionCard(t, board, key)
				reason := ""
				if card.Waiting != nil {
					reason = card.Waiting.Reason
				}
				stored, held := board.Issues[key]
				if !held {
					t.Fatalf("%s has no stored row on the board, and Legion worked it", key)
				}
				if reason != stored.WaitingReason {
					t.Errorf("%s waits for %q on its card and %q in its stored row", key, reason, stored.WaitingReason)
				}
				if reason != test.want[key] {
					t.Errorf("%s waits for %q on its card, want %q", key, reason, test.want[key])
				}
			}
			// The parent stands in its own column in both cases, and its wait names the open child
			// the block names, or none where no block was read.
			if got := legionColumnKeys(t, board, legion.ColumnAwaitingChildren); !slices.Equal(got, []string{parent}) {
				t.Errorf("the awaiting_children column holds %v, want the parent %s alone", got, parent)
			}
			blockers := []string{}
			if test.admission != "" {
				blockers = []string{openChild}
			}
			if waiting := legionCard(t, board, parent).Waiting; waiting != nil && !slices.Equal(waiting.BlockedBy, blockers) {
				t.Errorf("the parent waits on %v, want %v", waiting.BlockedBy, blockers)
			}

			// The unstarted child: the board draws it no card and counts it on the card above
			// it, so the one line answers that it waits for nothing, and its stored row says so
			// too.
			if card, held := legionCardHeld(board, child); held {
				t.Errorf("%s stands under %s and the board draws it a card of its own, in the %s phase",
					child, above, card.Phase)
			}
			row, held := board.Issues[child]
			if !held {
				t.Fatalf("%s has no stored row on the board, and Legion worked it", child)
			}
			if row.WaitingReason != "" {
				t.Errorf("%s is on no card and its stored row says it waits for %q, want nothing",
					child, row.WaitingReason)
			}
			counting := legionCard(t, board, above)
			if counting.Children != 1 {
				t.Errorf("%s counts %d unstarted children, want the one %s is", above, counting.Children, child)
			}
			t.Logf("%s: the card and the stored row of each of INFRA-1, INFRA-2, INFRA-3 and %s answer the same reason, and %s, which stands under %s's card on no card of its own, waits for nothing on both sides while %s counts %d unstarted child",
				test.what, parent, child, above, above, counting.Children)
		})
	}
}
