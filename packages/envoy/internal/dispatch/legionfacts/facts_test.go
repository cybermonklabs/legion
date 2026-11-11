package legionfacts

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// history is one test's seeded Legion history: the database it was written to, what the rig says
// it wrote, and the moment every computation of it is made as of.
type history struct {
	database *store.Store
	fixture  legiontest.Fixture
	now      time.Time
}

// seed writes the rig's history of trees trees, and zero asks for its default size.
func seed(t *testing.T, trees int) history {
	t.Helper()
	database := storetest.Open(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: "LEGION", Trees: trees, Now: now})
	return history{database: database, fixture: fixture, now: now}
}

// TestEveryIssueLegionWorkedGetsAStoredRowAndNoOtherIssueDoes: a sweep of the whole project
// writes one row for each issue the rig says carries Legion phase work, and none for the issues
// it says no role worked. A row means Legion worked the issue, which is what a reader of the
// table takes it for.
func TestEveryIssueLegionWorkedGetsAStoredRowAndNoOtherIssueDoes(t *testing.T) {
	seeded := seed(t, 0)
	project := seeded.fixture.Project

	rows, err := ComputeProject(t.Context(), seeded.database.Pool, project, seeded.now)
	if err != nil {
		t.Fatalf("compute the project's facts: %v", err)
	}
	if _, err := WriteProject(t.Context(), seeded.database.Pool, ProjectWrite{Project: project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	stored, err := Read(t.Context(), seeded.database.Pool, project)
	if err != nil {
		t.Fatalf("read the facts back: %v", err)
	}

	for _, key := range seeded.fixture.PhaseWork {
		if _, has := stored[key]; !has {
			t.Errorf("%s carries Legion phase work and has no stored row", key)
		}
	}
	for _, key := range seeded.fixture.NoPhaseWork {
		if row, has := stored[key]; has {
			t.Errorf("%s has the stored row %+v, and no Legion role worked it", key, row)
		}
	}
	if len(stored) != len(seeded.fixture.PhaseWork) {
		t.Errorf("the project holds %d stored rows, want the %d issues with Legion phase work",
			len(stored), len(seeded.fixture.PhaseWork))
	}
	t.Logf("project %s over %s to %s: %d issues seeded, %d with Legion phase work, %d with none, %d rows stored",
		project, seeded.fixture.From.Format(time.RFC3339), seeded.fixture.To.Format(time.RFC3339),
		len(seeded.fixture.Keys), len(seeded.fixture.PhaseWork), len(seeded.fixture.NoPhaseWork), len(stored))
}

// TestEachStoredFactEqualsTheSourceItComesFrom: the opening, the close, the slot hours, the depth
// in the tree and the waiting reason of each written row are what their own sources answer — the
// issue's own row, the rig's record of the slot it wrote, the parent chain, and the card rule
// over the newest observations of the issue.
func TestEachStoredFactEqualsTheSourceItComesFrom(t *testing.T) {
	seeded := seed(t, 6)
	project := seeded.fixture.Project
	// The rig writes no admission block, so without one every issue is queued by nothing and
	// every reason reads the same. This is the daemon's own block: the open child of the first
	// tree stands in the waiting line, and a blocker link holds the reviewed child.
	queued, blocked, blocker := seeded.fixture.Keys[3], seeded.fixture.Keys[2], seeded.fixture.Keys[1]
	seedAdmission(t, seeded, legion.Admission{
		Waiting: []string{queued},
		Blocked: []legion.BlockedIssue{{Issue: blocked, BlockedBy: []string{blocker}}},
	})

	rows, err := ComputeProject(t.Context(), seeded.database.Pool, project, seeded.now)
	if err != nil {
		t.Fatalf("compute the project's facts: %v", err)
	}
	if _, err := WriteProject(t.Context(), seeded.database.Pool, ProjectWrite{Project: project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	stored, err := Read(t.Context(), seeded.database.Pool, project)
	if err != nil {
		t.Fatalf("read the facts back: %v", err)
	}

	opened, closed, parents := issueRows(t, seeded)
	reasons := cardReasons(t, seeded)
	for _, key := range slices.Sorted(maps.Keys(stored)) {
		row := stored[key]
		if !row.OpenedAt.Equal(opened[key]) {
			t.Errorf("%s opened at %s in its row and at %s in the issues table", key, row.OpenedAt, opened[key])
		}
		switch {
		case row.ClosedAt == nil && closed[key] != nil:
			t.Errorf("%s closed at %s in the issues table and is open in its row", key, closed[key])
		case row.ClosedAt != nil && closed[key] == nil:
			t.Errorf("%s closed at %s in its row and is open in the issues table", key, row.ClosedAt)
		case row.ClosedAt != nil && !row.ClosedAt.Equal(*closed[key]):
			t.Errorf("%s closed at %s in its row and at %s in the issues table", key, row.ClosedAt, closed[key])
		}
		if want := seeded.fixture.SlotHours[key]; row.SlotHours != want {
			t.Errorf("%s held a slot for %g h in its row, want the %g h the rig wrote", key, row.SlotHours, want)
		}
		if want := depthOf(key, parents); row.TreeDepth != want {
			t.Errorf("%s is %d links below its root in its row, want the %d its parent chain holds",
				key, row.TreeDepth, want)
		}
		if row.WaitingReason != reasons[key] {
			t.Errorf("%s waits for %q in its row, want the card rule's %q", key, row.WaitingReason, reasons[key])
		}
	}
	// The block above is what makes the reasons differ from each other: a check over rows that
	// all read "" would pass on a rule that answered nothing at all.
	if reasons[queued] != legion.WaitingReady || reasons[blocked] != legion.WaitingBlocked {
		t.Fatalf("the queued issue waits for %q and the blocked one for %q, want %q and %q",
			reasons[queued], reasons[blocked], legion.WaitingReady, legion.WaitingBlocked)
	}
	t.Logf("%d rows compared against their own sources; %s waits for %q and %s for %q",
		len(stored), queued, reasons[queued], blocked, reasons[blocked])
}

// TestWriteRewritesNothingThatDidNotMove: the same rows written twice change no row the second
// time, whatever moment the second write carries, and a row whose one fact moved is the only key
// the write answers.
func TestWriteRewritesNothingThatDidNotMove(t *testing.T) {
	seeded := seed(t, 4)
	project := seeded.fixture.Project

	rows, err := ComputeProject(t.Context(), seeded.database.Pool, project, seeded.now)
	if err != nil {
		t.Fatalf("compute the project's facts: %v", err)
	}
	first, err := WriteProject(t.Context(), seeded.database.Pool, ProjectWrite{Project: project, Rows: rows, Whole: true})
	if err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	if len(first) != len(rows) {
		t.Fatalf("the first write changed %d rows, want all %d of them", len(first), len(rows))
	}
	written, err := Read(t.Context(), seeded.database.Pool, project)
	if err != nil {
		t.Fatalf("read the facts back: %v", err)
	}

	// A later moment on every row: updated_at alone is no change, so the second pass must write
	// nothing at all.
	later := seeded.now.Add(time.Hour)
	for index := range rows {
		rows[index].UpdatedAt = later
	}
	again, err := WriteProject(t.Context(), seeded.database.Pool, ProjectWrite{Project: project, Rows: rows, Whole: true})
	if err != nil {
		t.Fatalf("write the facts again: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("the second write of the same rows changed %d rows (%v), want none", len(again), again)
	}
	unmoved, err := Read(t.Context(), seeded.database.Pool, project)
	if err != nil {
		t.Fatalf("read the facts after the second write: %v", err)
	}
	for key, row := range unmoved {
		if !row.UpdatedAt.Equal(written[key].UpdatedAt) {
			t.Errorf("%s was last written at %s and now at %s, want the moment of the write that changed it",
				key, written[key].UpdatedAt, row.UpdatedAt)
		}
	}

	moved := rows[len(rows)/2]
	rows[len(rows)/2].ReviewRounds = moved.ReviewRounds + 1
	changed, err := WriteProject(t.Context(), seeded.database.Pool, ProjectWrite{Project: project, Rows: rows, Whole: true})
	if err != nil {
		t.Fatalf("write the moved row: %v", err)
	}
	if !slices.Equal(changed, []string{moved.Issue}) {
		t.Errorf("the write answered %v, want the one key %s whose review rounds moved", changed, moved.Issue)
	}
	after, err := ReadIssues(t.Context(), seeded.database.Pool, project, []string{moved.Issue})
	if err != nil {
		t.Fatalf("read the moved row: %v", err)
	}
	if got := after[moved.Issue]; got.ReviewRounds != moved.ReviewRounds+1 || !got.UpdatedAt.Equal(later) {
		t.Errorf("%s holds %d review rounds written at %s, want %d at %s",
			moved.Issue, got.ReviewRounds, got.UpdatedAt, moved.ReviewRounds+1, later)
	}
}

// TestTheStoredRowsReadBackFieldByField: Read and ReadIssues answer what WriteProject wrote,
// every field of every row.
func TestTheStoredRowsReadBackFieldByField(t *testing.T) {
	seeded := seed(t, 4)
	project := seeded.fixture.Project

	rows, err := ComputeProject(t.Context(), seeded.database.Pool, project, seeded.now)
	if err != nil {
		t.Fatalf("compute the project's facts: %v", err)
	}
	if _, err := WriteProject(t.Context(), seeded.database.Pool, ProjectWrite{Project: project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	whole, err := Read(t.Context(), seeded.database.Pool, project)
	if err != nil {
		t.Fatalf("read the project's facts: %v", err)
	}
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.Issue)
	}
	named, err := ReadIssues(t.Context(), seeded.database.Pool, project, keys[:len(keys)/2])
	if err != nil {
		t.Fatalf("read the named facts: %v", err)
	}

	if len(whole) != len(rows) {
		t.Fatalf("the project holds %d stored rows, want the %d written", len(whole), len(rows))
	}
	if len(named) != len(keys)/2 {
		t.Fatalf("the named read answered %d rows, want the %d asked for", len(named), len(keys)/2)
	}
	for _, want := range rows {
		if got := whole[want.Issue]; !sameRow(got, want) {
			t.Errorf("%s reads back as\n%s\nwant\n%s", want.Issue, show(got), show(want))
		}
	}
	for key, got := range named {
		if want := whole[key]; !sameRow(got, want) {
			t.Errorf("%s reads back from the named read as\n%s\nwant\n%s", key, show(got), show(want))
		}
	}
	t.Logf("%d rows read back whole and %d by name, field by field", len(whole), len(named))
}

// sameRow is whether two rows hold the same facts, each field compared as its own type: the times
// by their instant, since a read answers them in the database's own zone.
func sameRow(got, want Row) bool {
	return rowDifference(got, want) == ""
}

// rowDifference names the first field two rows hold different values of, with both values, and ""
// where they hold the same facts. It is where the field list is stated, so a check that counts
// differing rows and a check that prints one read the same comparison.
//
// The hours of want are read as the table stores them (storedHours), because the one caller that
// compares a computed row to a stored one answers an empty map where the computation answered a
// nil one.
func rowDifference(got, want Row) string {
	fields := []struct {
		name string
		same bool
		got  any
		want any
	}{
		{"the project", got.Project == want.Project, got.Project, want.Project},
		{"the issue", got.Issue == want.Issue, got.Issue, want.Issue},
		{"opened_at", got.OpenedAt.Equal(want.OpenedAt), got.OpenedAt.UTC(), want.OpenedAt.UTC()},
		{"closed_at", sameTime(got.ClosedAt, want.ClosedAt), utc(got.ClosedAt), utc(want.ClosedAt)},
		{"hours_by_phase", maps.Equal(got.HoursByPhase, storedHours(want.HoursByPhase)), got.HoursByPhase, want.HoursByPhase},
		{"work_hours", got.WorkHours == want.WorkHours, got.WorkHours, want.WorkHours},
		{"review_rounds", got.ReviewRounds == want.ReviewRounds, got.ReviewRounds, want.ReviewRounds},
		{"root_key", got.RootKey == want.RootKey, got.RootKey, want.RootKey},
		{"tree_depth", got.TreeDepth == want.TreeDepth, got.TreeDepth, want.TreeDepth},
		{"slot_hours", got.SlotHours == want.SlotHours, got.SlotHours, want.SlotHours},
		{"waiting_reason", got.WaitingReason == want.WaitingReason, got.WaitingReason, want.WaitingReason},
		{"work_first_at", sameTime(got.WorkFirstAt, want.WorkFirstAt), utc(got.WorkFirstAt), utc(want.WorkFirstAt)},
		{"work_last_at", sameTime(got.WorkLastAt, want.WorkLastAt), utc(got.WorkLastAt), utc(want.WorkLastAt)},
		{"updated_at", got.UpdatedAt.Equal(want.UpdatedAt), got.UpdatedAt.UTC(), want.UpdatedAt.UTC()},
	}
	for _, field := range fields {
		if !field.same {
			return fmt.Sprintf("%s is %v and %v", field.name, field.got, field.want)
		}
	}
	return ""
}

// sameTime is whether two optional moments are the same moment, or both absent.
func sameTime(got, want *time.Time) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return got.Equal(*want)
}

// show is one row written for a failure message, its times in UTC so two zones read alike.
func show(row Row) string {
	return fmt.Sprintf("opened %s closed %v hours %v work %g rounds %d root %s depth %d slot %g waiting %q work %v to %v updated %s",
		row.OpenedAt.UTC(), utc(row.ClosedAt), row.HoursByPhase, row.WorkHours, row.ReviewRounds,
		row.RootKey, row.TreeDepth, row.SlotHours, row.WaitingReason,
		utc(row.WorkFirstAt), utc(row.WorkLastAt), row.UpdatedAt.UTC())
}

func utc(at *time.Time) any {
	if at == nil {
		return nil
	}
	return at.UTC()
}

// seedAdmission writes the daemon's admission block as the sampler writes it: one row of the
// project under legion.AdmissionKey, which the rig itself writes none of.
func seedAdmission(t *testing.T, seeded history, admission legion.Admission) {
	t.Helper()
	value, err := json.Marshal(admission)
	if err != nil {
		t.Fatalf("encode the admission block: %v", err)
	}
	if _, err := seeded.database.Pool.Exec(t.Context(), `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		values ($1, $2, $3, $4::jsonb, $5)
	`, seeded.fixture.Project, legion.AdmissionKey, legion.FieldAdmission, string(value), seeded.now); err != nil {
		t.Fatalf("seed the admission block: %v", err)
	}
}

// issueRows is what the issues table holds of the project: when each issue opened, when it closed
// and its parent. They are the sources opened_at, closed_at and tree_depth come from.
func issueRows(t *testing.T, seeded history) (map[string]time.Time, map[string]*time.Time, map[string]string) {
	t.Helper()
	rows, err := seeded.database.Pool.Query(t.Context(), `
		select key, created_at, closed_at, coalesce(parent_key, '')
		from issues where project_key = $1
	`, seeded.fixture.Project)
	if err != nil {
		t.Fatalf("read the issues: %v", err)
	}
	defer rows.Close()
	opened := map[string]time.Time{}
	closed := map[string]*time.Time{}
	parents := map[string]string{}
	for rows.Next() {
		var key, parent string
		var created time.Time
		var closedAt *time.Time
		if err := rows.Scan(&key, &created, &closedAt, &parent); err != nil {
			t.Fatalf("scan an issue: %v", err)
		}
		opened[key], closed[key], parents[key] = created, closedAt, parent
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the issues: %v", err)
	}
	return opened, closed, parents
}

// depthOf is how many parent links lie between the issue and the top of its chain, which is the
// depth the rig's trees hold: zero for a root and one for each child it filed.
func depthOf(key string, parents map[string]string) int {
	depth := 0
	for at := key; parents[at] != ""; at = parents[at] {
		depth++
	}
	return depth
}

// cardReasons is why each issue of the project waits, worked out from the newest observation of
// each of its fields, as the board reads them (api/legion_board.go's readObservations), and the
// admission block the test wrote. It is the rule's own answer from the rule's own sources, which
// is what a stored waiting_reason must equal. A project whose daemon recorded no admission block
// is the third state of legion.Queued, which the board queues by Dispatch's rank.
func cardReasons(t *testing.T, seeded history) map[string]string {
	t.Helper()
	rows, err := seeded.database.Pool.Query(t.Context(), `
		select distinct on (issue_key, field) issue_key, field, value, observed_at
		from legion_observations
		where project = $1
		order by issue_key, field, observed_at desc
	`, seeded.fixture.Project)
	if err != nil {
		t.Fatalf("read the observations: %v", err)
	}
	defer rows.Close()
	phases := map[string]string{}
	since := map[string]time.Time{}
	slots := map[string]*int{}
	waits := map[string]*legion.Wait{}
	var admission legion.Admission
	admitted := false
	for rows.Next() {
		var key, field string
		var value []byte
		var at time.Time
		if err := rows.Scan(&key, &field, &value, &at); err != nil {
			t.Fatalf("scan an observation: %v", err)
		}
		switch {
		case key == legion.AdmissionKey && field == legion.FieldAdmission:
			if err := json.Unmarshal(value, &admission); err != nil {
				t.Fatalf("decode the admission block: %v", err)
			}
			admitted = true
		case field == legion.FieldPhase:
			phases[key], since[key] = legion.JSONString(value), at
		case field == legion.FieldSlot:
			var slot *int
			if err := json.Unmarshal(value, &slot); err != nil {
				t.Fatalf("decode the slot of %s: %v", key, err)
			}
			slots[key] = slot
		case field == legion.FieldWaitingOn:
			var wait struct {
				Kind       string     `json:"kind"`
				Since      *time.Time `json:"since"`
				AnsweredAt *time.Time `json:"answeredAt"`
			}
			if err := json.Unmarshal(value, &wait); err != nil {
				t.Fatalf("decode the wait of %s: %v", key, err)
			}
			waits[key] = &legion.Wait{Kind: wait.Kind, Since: wait.Since, AnsweredAt: wait.AnsweredAt}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the observations: %v", err)
	}
	places := map[string]int{}
	for _, key := range admission.Waiting {
		places[key] = len(places) + 1
	}
	blockers := map[string][]string{}
	for _, blocked := range admission.Blocked {
		blockers[blocked.Issue] = blocked.BlockedBy
	}
	reasons := map[string]string{}
	for _, key := range seeded.fixture.Keys {
		var place *int
		if numbered, named := places[key]; named {
			place = &numbered
		}
		var phaseSince *time.Time
		if at, named := since[key]; named {
			phaseSince = &at
		}
		waiting := legion.CardWaiting(legion.WaitingInput{
			Phase:        phases[key],
			Column:       legion.Column(phases[key], slots[key], queuedBy(admitted, place != nil || len(blockers[key]) > 0)),
			WaitingPlace: place,
			BlockedBy:    blockers[key],
			PhaseSince:   phaseSince,
			WaitingOn:    waits[key],
		})
		if waiting != nil {
			reasons[key] = waiting.Reason
		}
	}
	return reasons
}

// queuedBy is what the test's own sources know of one issue's place in the daemon's line, in the
// shape the column rule reads (legion.Queued): nothing at all where no observation recorded an
// admission block, and otherwise whether that block names the issue.
func queuedBy(admitted, named bool) legion.Queued {
	switch {
	case !admitted:
		return legion.QueuedUnread
	case named:
		return legion.QueuedYes
	default:
		return legion.QueuedNo
	}
}
