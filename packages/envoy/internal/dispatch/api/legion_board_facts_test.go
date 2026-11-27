package api

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// writeLegionFacts writes the stored facts of three issues of the project, as the facts consumer
// would have written them, and answers the oldest updated_at of the rows the board of
// seedLegionBoard serves. The third row, which that board does not serve, is the oldest of the
// three, so a header that counted it would read an older facts_updated_at than the one answered.
// The rows are written by hand rather than computed, so a check reads the numbers it wrote: the
// board serves stored rows and computes none of them, which is the whole of what these tests hold
// it to.
func writeLegionFacts(t *testing.T, database *store.Store, project string, now time.Time) time.Time {
	t.Helper()
	oldest := now.Add(-4 * time.Minute)
	closed := now.Add(-2 * time.Hour)
	first, last := now.Add(-6*time.Hour), now.Add(-time.Hour)
	rows := []legionfacts.Row{{
		Project: project, Issue: project + "-4", WaitingReason: "", UpdatedAt: now.Add(-2 * time.Minute),
		ClosedFacts: metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{
			OpenedAt:   now.Add(-9 * time.Hour),
			PhaseHours: map[string]float64{"implementing": 3.5, "reviewing": 1.25},
			WorkHours:  4.75, ReviewRounds: 2, Tree: project + "-3", TreeDepth: 1,
			SlotHours: 4, WorkFirstAt: &first, WorkLastAt: &last,
		}},
	}, {
		Project: project, Issue: project + "-3", WaitingReason: "waiting for a child", UpdatedAt: oldest,
		ClosedFacts: metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{
			OpenedAt:   now.Add(-12 * time.Hour),
			PhaseHours: map[string]float64{"design": 2},
			WorkHours:  2, ReviewRounds: 0, Root: true, Tree: project + "-3", TreeDepth: 0,
			SlotHours: 6,
		}},
	}, {
		// A closed issue of a tree no card stands in (seedLegionBoard's finished issue), which
		// carries no entry and moves no facts_updated_at, though it is the oldest row: the board
		// serves the facts of the open issues and of the issues it shows alone.
		Project: project, Issue: project + "-7", UpdatedAt: now.Add(-5 * time.Minute),
		ClosedFacts: metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{
			OpenedAt: now.Add(-30 * time.Hour), ClosedAt: &closed, PhaseHours: map[string]float64{}, WorkHours: 0,
			Root: true, Tree: project + "-7",
		}},
	}}
	if _, err := legionfacts.WriteProject(t.Context(), database.Pool,
		legionfacts.ProjectWrite{Project: project, Rows: rows, Whole: true}); err != nil {
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

// TestTheBoardCarriesOneStoredEntryPerIssueLegionWorked: over the facts' own rig, whose every
// tree holds an open child on a card, the board shows every tree, so the issues map is the
// project's stored rows: one entry for every issue with Legion phase work and none for an issue no
// Legion role worked, open and closed alike, whatever an issue's hours are, on a card or not.
// Each entry carries the hours, the work time, the review rounds, the root and the depth in the
// tree the row holds as of the moment of the read, because the board serves the row and computes
// nothing but the close of each open part at its own clock (legionfacts.Row.At).
func TestTheBoardCarriesOneStoredEntryPerIssueLegionWorked(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	// Five trees, because this issue's check k4 counts at least twenty entries and fails below
	// them: a seed that answers fifteen proves the rule and not the check.
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: "LEGION", Trees: 5, Now: now})
	nameLegionDaemon(t, handler, fixture.Project, "http://legion-legion.internal:13370")
	rows, _, err := legionfacts.ComputeProject(t.Context(), database.Pool, fixture.Project, now)
	if err != nil {
		t.Fatalf("compute the project's facts: %v", err)
	}
	if _, err := legionfacts.WriteProject(t.Context(), database.Pool,
		legionfacts.ProjectWrite{Project: fixture.Project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the project's facts: %v", err)
	}
	board := legionBoardOf(t, handler, fixture.Project)
	// The rows of the issues the board carries as the board's read closed them, at the moment its
	// body is computed as of: the one computation the read makes (legionfacts.Row.At).
	stored, err := legionfacts.Read(t.Context(), database.Pool, fixture.Project, slices.Collect(maps.Keys(board.Issues)), nil, board.ComputedAt)
	if err != nil {
		t.Fatalf("read the facts back: %v", err)
	}
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
		if !maps.Equal(entry.HoursByPhase, row.PhaseHours) {
			t.Errorf("%s: the board's hours = %v, the stored row's = %v", key, entry.HoursByPhase, row.PhaseHours)
		}
		if entry.WorkHours != row.WorkHours || entry.ReviewRounds != row.ReviewRounds {
			t.Errorf("%s: the board says %g work hours over %d review rounds, the stored row %g over %d",
				key, entry.WorkHours, entry.ReviewRounds, row.WorkHours, row.ReviewRounds)
		}
		if entry.RootKey != row.Tree || entry.TreeDepth != row.TreeDepth {
			t.Errorf("%s: the board puts it at depth %d of %s, the stored row at depth %d of %s",
				key, entry.TreeDepth, entry.RootKey, row.TreeDepth, row.Tree)
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

// TestTheBoardCarriesTheFactsOfTheIssuesItShowsAndNoOther: the issues map holds the stored facts
// of every open issue with Legion phase work and of every issue the board shows, and of no other
// issue (dispatch://INFRA-2916, R22): the issue of every card, every issue of a tree a card stands
// in, whose stored root is that card's tree, and every open row. So a child of a shown tree the
// daemon names on no card, or names nowhere at all, carries its entry, and so does an open issue
// on no card in no shown tree, a parked one, whose facts no other route serves; a closed issue of
// a tree the daemon no longer shows carries none, nor does any closed issue of a tree the daemon
// never showed: their facts are the items route's, by close day, and the board's body follows the
// open work and what the daemon runs rather than the project's history.
//
// The header's facts_updated_at is the oldest updated_at of the rows the answer serves, so a row
// the board does not serve, the oldest of the project's, moves it not at all.
func TestTheBoardCarriesTheFactsOfTheIssuesItShowsAndNoOther(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC().Truncate(time.Microsecond)
	// The board seedLegionBoard draws: cards for INFRA-10, -1 and -2 (the queue), -3 (design), its
	// child -4 (implementing), -5 (with a person), -6 (held), and -11 and -13 (waiting on
	// children); INFRA-7 is done and on no card, the root of a tree no card stands in, INFRA-8 is
	// -3's child its tree has not started, -12 is -11's, and -14 is -13's closed child.
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
		// A child of a shown tree the daemon names on no card.
		row("INFRA-8", "INFRA-3", 1, 2*time.Minute, nil),
		// A child of a shown tree the daemon names nowhere: it closed before the daemon's reads.
		row("INFRA-15", "INFRA-3", 1, 6*time.Minute, &closed),
		// A closed issue of a tree the daemon no longer shows: done, on no card, its own root.
		row("INFRA-7", "INFRA-7", 0, 7*time.Minute, &closed),
		// A tree the daemon never showed, the oldest rows of the project.
		row("INFRA-20", "INFRA-20", 0, 9*time.Minute, &closed),
		row("INFRA-21", "INFRA-20", 1, 8*time.Minute, &closed),
		// An open issue on no card and in no shown tree: parked, its own root.
		row("INFRA-30", "INFRA-30", 0, time.Minute, nil),
	}
	if _, err := legionfacts.WriteProject(t.Context(), database.Pool,
		legionfacts.ProjectWrite{Project: "INFRA", Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the stored facts of INFRA: %v", err)
	}

	board := legionBoardOf(t, handler, "INFRA")
	want := []string{"INFRA-15", "INFRA-3", "INFRA-30", "INFRA-4", "INFRA-6", "INFRA-8"}
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
	t.Logf("before any pass: INFRA %d rows and no age, LINUX no row and no age — nothing has been computed. After one pass 90 s ago: INFRA %d rows and an age of %s s, LINUX no row and an age of %s s — LINUX has no Legion work, and its facts are as fresh as INFRA's",
		beforePass, len(worked.Issues), ageSeconds(worked.Header.FactsCursorAgeSeconds),
		ageSeconds(idle.Header.FactsCursorAgeSeconds))
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
// The fifth issue is a parent in `awaiting_children` (dispatch://INFRA-3320), and the sixth its
// open child, which its tree has not started. The daemon lists such a parent as blocked on its
// open children, so where a block is recorded a blocker link of it names the parent, which would
// queue any other issue that has not started its work. The parent's card stands in Waiting on
// children all the same, with and without a block, and its stored row says the same column, its
// hours under it, and the same reason its card waits for: held by its child where the block
// names it, ready for the pass that returns it where no block was read. Its child is counted on
// its card.
func TestACardsWaitingReasonEqualsTheStoredRowsWithAndWithoutAnAdmissionBlock(t *testing.T) {
	// child is the key of the unstarted child, which carries no card and whose stored row must
	// say it waits for nothing, and above is the card that counts it. They are the same two
	// issues in both cases below, because neither admission block names either of them. parent
	// is the issue waiting on its children, whose open child the block names as what holds it.
	const child, above, parent = "INFRA-4", "INFRA-3", "INFRA-5"
	for _, test := range []struct {
		what      string
		admission string
		want      map[string]string
	}{
		{
			what: "no read recorded an admission block",
			want: map[string]string{"INFRA-1": legion.WaitingReady, "INFRA-2": legion.WaitingReady, "INFRA-3": "", parent: legion.WaitingReady},
		},
		{
			what: "a read recorded one",
			admission: `{"cap": 2, "active": [], "waiting": ["INFRA-1"],
				"blocked": [{"issue": "INFRA-2", "blockedBy": ["INFRA-9"]}, {"issue": "INFRA-5", "blockedBy": ["INFRA-6"]}]}`,
			want: map[string]string{"INFRA-1": legion.WaitingReady, "INFRA-2": legion.WaitingBlocked, "INFRA-3": "", parent: legion.WaitingBlocked},
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
			// reports `admitted` with no slot of its own: its tree has not started it. The fifth
			// is a root waiting on its children, and the sixth its child, `admitted` as the
			// fourth is.
			for _, issue := range []struct {
				number              int
				rank, phase, parent string
			}{
				{1, "a", legion.PhaseAdmitted, ""},
				{2, "b", legion.PhaseAdmitted, ""},
				{3, "c", legion.PhaseImplementing, ""},
				{4, "d", legion.PhaseAdmitted, "INFRA-3"},
				{5, "e", legion.PhaseAwaitingChildren, ""},
				{6, "f", legion.PhaseAdmitted, parent},
			} {
				key := seedLegionIssue(t, database, "INFRA", issue.number, "An issue in "+issue.phase, issue.rank, nil, issue.parent)
				seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"`+issue.phase+`"`, hourAgo)
				seedLegionHandoff(t, database, key, "plan", hourAgo)
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

			// The parent waiting on its children: its card stands in Waiting on children, whether
			// or not a blocker link of the block names it, and its stored row keeps its hours under
			// that same column. Its reason is held to its stored row's above.
			column := ""
			for _, each := range board.Columns {
				if slices.ContainsFunc(each.Cards, func(card legionBoardCard) bool { return card.Key == parent }) {
					column = each.ID
				}
			}
			if column != legion.ColumnAwaitingChildren {
				t.Errorf("%s waits on its children and its card stands in %q, want %q", parent, column, legion.ColumnAwaitingChildren)
			}
			waited := board.Issues[parent].HoursByPhase
			if waited[legion.ColumnAwaitingChildren] <= 0 {
				t.Errorf("%s waits on its children and its stored row holds its hours as %v, want them under %q",
					parent, waited, legion.ColumnAwaitingChildren)
			}
			if waiting := legionCard(t, board, parent); waiting.Children != 1 {
				t.Errorf("%s counts %d unstarted children, want its one open child", parent, waiting.Children)
			}
			t.Logf("%s: the card and the stored row of each of INFRA-1, INFRA-2, INFRA-3 and %s answer the same reason, and %s, which stands under %s's card on no card of its own, waits for nothing on both sides while %s counts %d unstarted child; %s stands in %s with %v hours stored under that column",
				test.what, parent, child, above, above, counting.Children, parent, column, waited)
		})
	}
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
	if len(board.Columns) != len(legion.Columns) {
		t.Fatalf("the project-path board has %d columns, want the %d the view draws",
			len(board.Columns), len(legion.Columns))
	}
	t.Logf("GET /api/v1/legion/board answers 404 at both of its old spellings; GET /api/v1/projects/INFRA/legion/board answers %d columns holding %d cards",
		len(board.Columns), countBoardCards(board))
}
