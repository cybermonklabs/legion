package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// nameLegionDaemon names project's daemon, which is what brings the board.
func nameLegionDaemon(t *testing.T, handler http.Handler, project, stateURL string) {
	t.Helper()
	written := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/"+project+"/legion/daemon",
		map[string]any{"state_url": stateURL}, "alice")
	if written.Code != http.StatusOK {
		t.Fatalf("name %s's daemon: status=%d body=%s", project, written.Code, written.Body.String())
	}
}

// legionExec runs one seeding statement.
func legionExec(t *testing.T, database *store.Store, sql string, args ...any) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("seed (%s): %v", sql, err)
	}
}

// seedLegionIssue writes one Dispatch issue of the project, which is where a card's title,
// priority and parent come from (the plan's D13).
func seedLegionIssue(t *testing.T, database *store.Store, project string, number int, title, rank string, priority *int, parent string) string {
	t.Helper()
	key := fmt.Sprintf("%s-%d", project, number)
	var parentKey *string
	if parent != "" {
		parentKey = &parent
	}
	legionExec(t, database, `insert into issues (key, project_key, number, title, status, rank, priority, parent_key, created_by)
		values ($1, $2, $3, $4, 'in_progress', $5, $6, $7, '{"kind":"user","id":"alice"}'::jsonb)`,
		key, project, number, title, rank, priority, parentKey)
	return key
}

// seedLegionObservation writes one observation as the sampler would have recorded it.
func seedLegionObservation(t *testing.T, database *store.Store, project, issue, field, value string, at time.Time) {
	t.Helper()
	legionExec(t, database, `insert into legion_observations (project, issue_key, field, value, observed_at)
		values ($1, $2, $3, $4::jsonb, $5)`, project, issue, field, value, at)
}

// seedLegionRead writes one read of the project's daemon, the freshness the header shows.
func seedLegionRead(t *testing.T, database *store.Store, project string, at time.Time, failure string) {
	t.Helper()
	var message *string
	if failure != "" {
		message = &failure
	}
	legionExec(t, database, `insert into legion_reads (project, read_at, ok, error, bytes)
		values ($1, $2, $3, $4, 1024)`, project, at, failure == "", message)
}

// seedLegionBoard writes one project's board: a waiting line whose first root was answered, an
// issue a blocker link holds, a root in design with an unstarted child, a child in a turn, a root
// waiting on a person, a held issue, and an issue that finished. It is the state one read of a
// daemon would have recorded, written as the sampler records it.
func seedLegionBoard(t *testing.T, database *store.Store, project string, now time.Time) {
	t.Helper()
	ago := func(hours float64) time.Time { return now.Add(-time.Duration(hours * float64(time.Hour))) }
	p1 := 1
	p0 := 0
	keys := []struct {
		number        int
		title, rank   string
		priority      *int
		parent, phase string
	}{
		{10, "The answered root", "a", &p0, "", "awaiting_person"},
		{1, "The queued root", "b", &p1, "", "admitted"},
		{2, "The blocked root", "c", nil, "", "admitted"},
		{3, "The design root", "d", &p1, "", "admitted"},
		{4, "The implementer's child", "e", nil, project + "-3", "implementing"},
		{5, "The person's root", "f", nil, "", "awaiting_person"},
		{6, "The held issue", "g", nil, "", "held"},
		{7, "The finished issue", "h", nil, "", "done"},
		{8, "The unstarted child", "i", nil, project + "-3", "admitted"},
	}
	for _, issue := range keys {
		key := seedLegionIssue(t, database, project, issue.number, issue.title, issue.rank, issue.priority, issue.parent)
		seedLegionObservation(t, database, project, key, legion.FieldPhase, `"`+issue.phase+`"`, ago(3))
	}
	// The two issues holding a slot, one of them a child: nothing takes a slot holder for a root.
	seedLegionObservation(t, database, project, project+"-3", legion.FieldSlot, `0`, ago(5))
	seedLegionObservation(t, database, project, project+"-4", legion.FieldSlot, `1`, ago(4))
	// The implementer is in a turn; its reviewer's claim ended.
	seedLegionObservation(t, database, project, project+"-4", legion.ClaimFieldPrefix+"implementer", `"working"`, ago(2))
	seedLegionObservation(t, database, project, project+"-4", legion.ClaimFieldPrefix+"reviewer", `"idle"`, ago(2))
	// What each root in awaiting_person waits on: one answered, which is why the daemon's line
	// holds it first, and one still with the person.
	seedLegionObservation(t, database, project, project+"-10", legion.FieldWaitingOn,
		`{"kind":"ask","since":"2026-11-03T09:00:00Z","answeredAt":"2026-11-03T10:00:00Z"}`, ago(6))
	seedLegionObservation(t, database, project, project+"-5", legion.FieldWaitingOn,
		`{"kind":"gate","since":"2026-11-03T08:00:00Z"}`, ago(6))
	seedLegionObservation(t, database, project, legion.AdmissionKey, legion.FieldAdmission, fmt.Sprintf(`{
		"cap": 4,
		"active": [%q, %q],
		"waiting": [%q, %q],
		"blocked": [{"issue": %q, "blockedBy": [%q]}]
	}`, project+"-3", project+"-4", project+"-10", project+"-1", project+"-2", project+"-99"), ago(3))
	seedLegionRead(t, database, project, ago(0.01), "")
}

// legionBoardOf is the board the route answers for project.
func legionBoardOf(t *testing.T, handler http.Handler, project string) legionBoardResponse {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/"+project+"/legion/board", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET the board of %s: status=%d body=%s", project, response.Code, response.Body.String())
	}
	return decodeBody[legionBoardResponse](t, response)
}

// legionColumnKeys is the keys of one column's cards, in the board's own order.
func legionColumnKeys(t *testing.T, board legionBoardResponse, id string) []string {
	t.Helper()
	for _, column := range board.Columns {
		if column.ID != id {
			continue
		}
		keys := make([]string, len(column.Cards))
		for index, card := range column.Cards {
			keys[index] = card.Key
		}
		return keys
	}
	t.Fatalf("the board has no column %q", id)
	return nil
}

// legionCard is the card for key, wherever it is.
func legionCard(t *testing.T, board legionBoardResponse, key string) legionBoardCard {
	t.Helper()
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			if card.Key == key {
				return card
			}
		}
	}
	t.Fatalf("the board holds no card for %s", key)
	return legionBoardCard{}
}

// TestTheLegionBoardPutsEachIssueInOneColumn: the Queue column is the daemon's own waiting line,
// in its order, then the issues a blocker link holds; every other issue the daemon reports is in
// the column its phase names; an issue the tree has not started is on no card and is counted on
// its root's; and the header carries the slots in use, the cap, the queue's length and the last
// read.
func TestTheLegionBoardPutsEachIssueInOneColumn(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	seedLegionBoard(t, database, "INFRA", now)

	board := legionBoardOf(t, handler, "INFRA")
	if board.Project != "INFRA" {
		t.Fatalf("the board names project %q, want INFRA", board.Project)
	}
	if len(board.Columns) != len(legionBoardColumnOrder) {
		t.Fatalf("the board has %d columns, want the %d the view draws", len(board.Columns), len(legionBoardColumnOrder))
	}
	for index, column := range board.Columns {
		want := legionBoardColumnOrder[index]
		if column.ID != want.id || column.Label != want.label {
			t.Fatalf("column %d is %q/%q, want %q/%q", index, column.ID, column.Label, want.id, want.label)
		}
	}
	// The waiting line's own order, which the view does not re-sort: the answered root first,
	// though Dispatch's rank would put the queued root there, and the blocked one after both.
	if got := legionColumnKeys(t, board, legionColumnQueue); !slices.Equal(got, []string{"INFRA-10", "INFRA-1", "INFRA-2"}) {
		t.Errorf("the queue holds %v, want the waiting line then the blocked issue", got)
	}
	for id, want := range map[string][]string{
		legionColumnDesign:       {"INFRA-3"},
		legionColumnImplementing: {"INFRA-4"},
		legionColumnAwaiting:     {"INFRA-5"},
		legionColumnHeld:         {"INFRA-6"},
		legionColumnPlanning:     {},
		legionColumnTesting:      {},
		legionColumnReviewing:    {},
		legionColumnAccepting:    {},
		legionColumnRetro:        {},
		legionColumnMerging:      {},
	} {
		if got := legionColumnKeys(t, board, id); !slices.Equal(got, want) {
			t.Errorf("the %s column holds %v, want %v", id, got, want)
		}
	}
	// The finished issue and the child its tree has not started are on no card; the root counts
	// the child.
	for _, gone := range []string{"INFRA-7", "INFRA-8"} {
		for _, column := range board.Columns {
			if slices.ContainsFunc(column.Cards, func(card legionBoardCard) bool { return card.Key == gone }) {
				t.Errorf("%s has a card of its own, in %s", gone, column.ID)
			}
		}
	}
	design := legionCard(t, board, "INFRA-3")
	if design.Children != 1 {
		t.Errorf("the design root counts %d children its tree has not started, want the one", design.Children)
	}
	if design.Slot == nil || *design.Slot != 0 || design.Title != "The design root" || design.Priority == nil || *design.Priority != 1 {
		t.Errorf("the design root's card = %+v, want slot 0 with Dispatch's title and priority", design)
	}
	child := legionCard(t, board, "INFRA-4")
	if !slices.Equal(child.Roles, []string{"implementer"}) {
		t.Errorf("the child's roles in a turn = %v, want the implementer alone", child.Roles)
	}
	if child.Tree != "INFRA-3" || child.Slot == nil || *child.Slot != 1 {
		t.Errorf("the child's card = %+v, want slot 1 on its root's tree", child)
	}
	if child.PhaseSince == nil || child.PhaseSince.After(now) {
		t.Errorf("the child's phase began %v, want the moment its phase was recorded", child.PhaseSince)
	}
	blocked := legionCard(t, board, "INFRA-2")
	if !slices.Equal(blocked.BlockedBy, []string{"INFRA-99"}) {
		t.Errorf("the blocked card names %v, want the issue holding it", blocked.BlockedBy)
	}
	person := legionCard(t, board, "INFRA-5")
	if person.WaitingOn == nil || person.WaitingOn.Kind != "gate" || person.WaitingOn.Since == nil || person.WaitingOn.AnsweredAt != nil {
		t.Errorf("the waiting root waits on %+v, want an unanswered gate with its since", person.WaitingOn)
	}
	if answered := legionCard(t, board, "INFRA-10"); answered.WaitingOn == nil || answered.WaitingOn.AnsweredAt == nil {
		t.Errorf("the answered root's wait = %+v, want the moment a person answered it", answered.WaitingOn)
	}
	header := board.Header
	if header.SlotsInUse != 2 || header.SlotCap == nil || *header.SlotCap != 4 || header.QueueLength != 3 {
		t.Errorf("the header = %+v, want 2 slots of 4 in use and a queue of 3", header)
	}
	if !header.ReadOK || header.ReadAt == nil || header.ReadError != nil {
		t.Errorf("the header's read = %+v, want the successful read the sampler recorded", header)
	}
}

// TestTheMergingColumnHoldsMergingAndAwaitingMerge: Merging holds `merging` and `awaiting_merge`
// alone, and `retro` has the Retro column to itself, between Accepting and Merging, so time spent
// in retro is never shown under Merging.
func TestTheMergingColumnHoldsMergingAndAwaitingMerge(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	for index, phase := range []string{legion.PhaseRetro, legion.PhaseMerging, legion.PhaseAwaitingMerge} {
		key := seedLegionIssue(t, database, "INFRA", index+1, "An issue in "+phase, string(rune('a'+index)), nil, "")
		seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"`+phase+`"`, now.Add(-time.Hour))
	}
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	board := legionBoardOf(t, handler, "INFRA")
	ids := make([]string, len(board.Columns))
	for index, column := range board.Columns {
		ids[index] = column.ID
	}
	if retro := slices.Index(ids, legionColumnRetro); retro < 1 || retro+1 >= len(ids) ||
		ids[retro-1] != legionColumnAccepting || ids[retro+1] != legionColumnMerging {
		t.Errorf("the columns are %v, want retro between accepting and merging", ids)
	}
	if got := legionColumnKeys(t, board, legionColumnRetro); !slices.Equal(got, []string{"INFRA-1"}) {
		t.Errorf("the Retro column holds %v, want the issue in retro alone", got)
	}
	if got := legionColumnKeys(t, board, legionColumnMerging); !slices.Equal(got, []string{"INFRA-2", "INFRA-3"}) {
		t.Errorf("the Merging column holds %v, want the issues in merging and awaiting_merge", got)
	}
}

// TestAnIssueAtAStepIsOnTheStepColumn: an issue at `step`, where a role the deployment defines
// works a step of its task, is on one card, in the Step column between Reviewing and Accepting. No
// column of Legion's own phases is its: such a step is of any type the deployment names, so
// neither Testing nor Reviewing says what it is, and a phase the board placed in no column would
// put the issue on no card while its role works it.
func TestAnIssueAtAStepIsOnTheStepColumn(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	key := seedLegionIssue(t, database, "INFRA", 1, "An issue at a step of the checker", "a", nil, "")
	seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"step"`, now.Add(-time.Hour))
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	board := legionBoardOf(t, handler, "INFRA")
	ids := make([]string, len(board.Columns))
	var in []string
	for index, column := range board.Columns {
		ids[index] = column.ID
		if slices.ContainsFunc(column.Cards, func(card legionBoardCard) bool { return card.Key == key }) {
			in = append(in, column.ID)
		}
	}
	if !slices.Equal(in, []string{"step"}) {
		t.Fatalf("%s at step is on cards of the columns %v, want one card, in the step column", key, in)
	}
	if step := slices.Index(ids, "step"); step < 1 || step+1 >= len(ids) ||
		ids[step-1] != legionColumnReviewing || ids[step+1] != legionColumnAccepting {
		t.Errorf("the columns are %v, want step between reviewing and accepting", ids)
	}
}

// TestTheLegionBoardQueuesByRankUntilADaemonRecordsItsWaitingLine: before any read recorded an
// admission block — every read a daemon before this release answered — the Queue column is the
// roots waiting for a slot in Dispatch's rank order, which is the order the daemon's own line
// follows once no root's wait has been answered.
func TestTheLegionBoardQueuesByRankUntilADaemonRecordsItsWaitingLine(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	seedLegionIssue(t, database, "INFRA", 1, "Second by rank", "b", nil, "")
	seedLegionIssue(t, database, "INFRA", 2, "First by rank", "a", nil, "")
	seedLegionIssue(t, database, "INFRA", 3, "A child of the first", "c", nil, "INFRA-2")
	for _, key := range []string{"INFRA-1", "INFRA-2", "INFRA-3"} {
		seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"admitted"`, now.Add(-time.Hour))
	}
	seedLegionRead(t, database, "INFRA", now, "")

	board := legionBoardOf(t, handler, "INFRA")
	if got := legionColumnKeys(t, board, legionColumnQueue); !slices.Equal(got, []string{"INFRA-2", "INFRA-1"}) {
		t.Fatalf("the queue holds %v, want the two roots in Dispatch's rank order", got)
	}
	if board.Header.SlotCap != nil {
		t.Errorf("the header names a cap of %d, want none until a read records the admission block", *board.Header.SlotCap)
	}
	if root := legionCard(t, board, "INFRA-2"); root.Children != 1 {
		t.Errorf("the first root counts %d unstarted children, want the one", root.Children)
	}
}

// newAdmissionShapeState is a state document as a daemon on dispatch://INFRA-980 serves it: every
// issue holding a slot in admission.active, children included; waiting ordered by tree root; a
// blocked parent waiting for its children; a new admission.trees; a new per-issue tree; and a
// higher contract version.
const newAdmissionShapeState = `{
  "daemon": {"project": "INFRA", "contract": 47, "schema": 212},
  "admission": {
    "cap": 6,
    "active": ["INFRA-1", "INFRA-2"],
    "waiting": ["INFRA-3"],
    "trees": ["INFRA-1"],
    "blocked": [{"issue": "INFRA-4", "blockedBy": ["INFRA-2"], "waitingForChildren": true}]
  },
  "issues": {
    "INFRA-1": {"key": "INFRA-1", "phase": "admitted", "status": "in_progress", "tree": "INFRA-1",
      "slot": {"index": 0, "admittedAt": "2026-11-03T10:00:00Z"}},
    "INFRA-2": {"key": "INFRA-2", "phase": "implementing", "status": "in_progress", "tree": "INFRA-1",
      "slot": {"index": 1, "admittedAt": "2026-11-03T10:05:00Z"},
      "workers": {"implementer": {"claim": {"session": "s-1", "state": "working"}, "rounds": 1}}},
    "INFRA-3": {"key": "INFRA-3", "phase": "admitted", "status": "todo", "tree": "INFRA-3"},
    "INFRA-4": {"key": "INFRA-4", "phase": "admitted", "status": "todo", "tree": "INFRA-1"}
  },
  "settings": {"admission_cap": 6, "review_round_cap": 3, "gates": {"design": true}}
}`

// TestTheLegionBoardReadsADaemonOnTheNewAdmissionShape: the board is built from what the loose
// decode records of a daemon that has rolled onto dispatch://INFRA-980, so a child holding a slot
// is a card of its own on its root's tree and no slot holder is taken for a root.
func TestTheLegionBoardReadsADaemonOnTheNewAdmissionShape(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	// The root first: its children name it as their parent, which the issues table holds them to.
	for _, issue := range []struct {
		number      int
		title, rank string
		parent      string
	}{
		{1, "The root", "a", ""},
		{2, "The child in a turn", "b", "INFRA-1"},
		{3, "The waiting root", "c", ""},
		{4, "The blocked child", "d", "INFRA-1"},
	} {
		seedLegionIssue(t, database, "INFRA", issue.number, issue.title, issue.rank, nil, issue.parent)
	}
	var state legion.State
	if err := json.Unmarshal([]byte(newAdmissionShapeState), &state); err != nil {
		t.Fatalf("decode the new state shape: %v", err)
	}
	if _, err := legion.RecordRead(t.Context(), database.Pool, "INFRA", now.Add(-time.Hour),
		len(newAdmissionShapeState), state.Observations()); err != nil {
		t.Fatalf("record the new state shape: %v", err)
	}

	board := legionBoardOf(t, handler, "INFRA")
	if got := legionColumnKeys(t, board, legionColumnQueue); !slices.Equal(got, []string{"INFRA-3", "INFRA-4"}) {
		t.Errorf("the queue holds %v, want the waiting root then the blocked child", got)
	}
	if got := legionColumnKeys(t, board, legionColumnDesign); !slices.Equal(got, []string{"INFRA-1"}) {
		t.Errorf("the design column holds %v, want the root holding a slot", got)
	}
	child := legionCard(t, board, "INFRA-2")
	if child.Tree != "INFRA-1" || child.Slot == nil || *child.Slot != 1 {
		t.Errorf("the child's card = %+v, want the slot it holds on the tree the daemon named", child)
	}
	if !slices.Equal(child.Roles, []string{"implementer"}) {
		t.Errorf("the child's card = %+v, want the implementer in a turn", child)
	}
	if header := board.Header; header.SlotsInUse != 2 || header.SlotCap == nil || *header.SlotCap != 6 {
		t.Errorf("the header = %+v, want the two slots the issues hold of the cap the daemon named", header)
	}
	if blocked := legionCard(t, board, "INFRA-4"); !slices.Equal(blocked.BlockedBy, []string{"INFRA-2"}) {
		t.Errorf("the blocked child names %v, want the issue the daemon says holds it", blocked.BlockedBy)
	}
	// The settings the daemon reports, with where each came from: Dispatch models none of their
	// values, so each is read here as a reader of one expects it.
	var admissionCap int
	if err := json.Unmarshal(board.Settings["admission_cap"], &admissionCap); err != nil || admissionCap != 6 {
		t.Errorf("the board's admission_cap setting = %s (%v), want the 6 the daemon reports", board.Settings["admission_cap"], err)
	}
	var gates map[string]bool
	if err := json.Unmarshal(board.Settings["gates"], &gates); err != nil || !gates["design"] {
		t.Errorf("the board's gates setting = %s (%v), want the object the daemon sent", board.Settings["gates"], err)
	}
}

// TestTheLegionBoardOfADaemonThatNeverAnsweredIsEmptyWithItsFailure: a project whose daemon has a
// setting but has never answered shows the board with its columns empty and the read's failure in
// the header (the plan's A4 case).
func TestTheLegionBoardOfADaemonThatNeverAnsweredIsEmptyWithItsFailure(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	seedLegionIssue(t, database, "INFRA", 1, "Nothing has been read of it", "a", nil, "")
	seedLegionRead(t, database, "INFRA", now, "the Legion daemon could not be read: dial tcp: connection refused")

	board := legionBoardOf(t, handler, "INFRA")
	for _, column := range board.Columns {
		if len(column.Cards) != 0 {
			t.Errorf("the %s column holds %d cards, want none: nothing has been read", column.ID, len(column.Cards))
		}
	}
	header := board.Header
	if header.ReadOK || header.ReadError == nil || header.ReadAt == nil {
		t.Fatalf("the header = %+v, want the failed read it names", header)
	}
	if header.SlotsInUse != 0 || header.SlotCap != nil || header.QueueLength != 0 {
		t.Errorf("the header = %+v, want no slot, no cap and an empty queue", header)
	}
	if board.Settings == nil || len(board.Settings) != 0 {
		t.Errorf("the board's settings = %v, want an empty object rather than null", board.Settings)
	}
}

// fakeLegionDaemon is a Legion daemon stand-in serving the state document the test last handed it,
// so a test drives the sampler's own passes (legion.Sampler) and reads the board they leave.
type fakeLegionDaemon struct {
	*httptest.Server
	mu   sync.Mutex
	body string
}

// newFakeLegionDaemon starts a daemon stand-in and names it project's daemon.
func newFakeLegionDaemon(t *testing.T, handler http.Handler, project string) *fakeLegionDaemon {
	t.Helper()
	daemon := &fakeLegionDaemon{}
	daemon.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != legion.StatePath {
			http.NotFound(w, r)
			return
		}
		daemon.mu.Lock()
		body := daemon.body
		daemon.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(daemon.Close)
	nameLegionDaemon(t, handler, project, daemon.URL)
	return daemon
}

// serves sets the state document the daemon answers from its next read on.
func (d *fakeLegionDaemon) serves(body string) {
	d.mu.Lock()
	d.body = body
	d.mu.Unlock()
}

// pass serves body and runs one pass of the sampler over it.
func (d *fakeLegionDaemon) pass(t *testing.T, database *store.Store, body string) {
	t.Helper()
	d.serves(body)
	legion.NewSampler(database.Pool).Pass(t.Context())
}

// TestTheBoardAndTheMetricsDropWhatTheNewestReadNoLongerCarries: the board's cards are the newest
// good read's, and an issue the daemon stopped carrying is still a Legion issue for both readers'
// tree walk (dispatch://INFRA-1077's spec, "The top issue of a tree": the daemon reports an issue
// Dispatch observed a phase of). The first read carries three issues of one chain, with the root's
// architect and implementer in a turn; the second no longer carries the middle issue or the
// implementer's claim. The board then shows neither, and the bottom issue stays in the root's
// tree on the board and in the metrics: its chain climbs through the middle issue, which left the
// daemon's state after Dispatch observed it in `testing` and so is still Legion's.
func TestTheBoardAndTheMetricsDropWhatTheNewestReadNoLongerCarries(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	daemon := newFakeLegionDaemon(t, handler, "INFRA")
	seedLegionIssue(t, database, "INFRA", 1, "The root", "a", nil, "")
	seedLegionIssue(t, database, "INFRA", 2, "The middle issue", "b", nil, "INFRA-1")
	seedLegionIssue(t, database, "INFRA", 3, "The bottom issue", "c", nil, "INFRA-2")

	daemon.pass(t, database, `{"issues":{
		"INFRA-1":{"key":"INFRA-1","phase":"implementing","slot":{"index":0},"architect":{"state":"working"},
			"workers":{"implementer":{"claim":{"state":"working"}}}},
		"INFRA-2":{"key":"INFRA-2","phase":"testing","workers":{"tester":{"claim":{"state":"working"}}}},
		"INFRA-3":{"key":"INFRA-3","phase":"planning","workers":{"planner":{"claim":{"state":"working"}}}}}}`)
	board := legionBoardOf(t, handler, "INFRA")
	if got := legionColumnKeys(t, board, legionColumnTesting); !slices.Equal(got, []string{"INFRA-2"}) {
		t.Fatalf("after the first read the testing column holds %v, want INFRA-2", got)
	}
	if roles := legionCard(t, board, "INFRA-1").Roles; !slices.Equal(roles, []string{"architect", "implementer"}) {
		t.Fatalf("after the first read INFRA-1's roles are %v, want the architect and the implementer", roles)
	}
	if tree := legionCard(t, board, "INFRA-3").Tree; tree != "INFRA-1" {
		t.Fatalf("after the first read INFRA-3 is in tree %q, want INFRA-1, through the middle issue", tree)
	}

	daemon.pass(t, database, `{"issues":{
		"INFRA-1":{"key":"INFRA-1","phase":"implementing","slot":{"index":0},"architect":{"state":"working"}},
		"INFRA-3":{"key":"INFRA-3","phase":"planning","workers":{"planner":{"claim":{"state":"working"}}}}}}`)
	board = legionBoardOf(t, handler, "INFRA")
	if !board.Header.ReadOK {
		t.Fatalf("the header = %+v, want the second read recorded as a good read", board.Header)
	}
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			if card.Key == "INFRA-2" {
				t.Errorf("the board holds INFRA-2 in the %s column, want no card: the newest read does not carry it", column.ID)
			}
		}
	}
	if roles := legionCard(t, board, "INFRA-1").Roles; !slices.Equal(roles, []string{"architect"}) {
		t.Errorf("INFRA-1's roles are %v, want the architect alone: the newest read carries no implementer's claim", roles)
	}
	if tree := legionCard(t, board, "INFRA-3").Tree; tree != "INFRA-1" {
		t.Errorf("the board puts INFRA-3 in tree %q, want INFRA-1: the middle issue left the daemon's state and is still Legion's", tree)
	}
	now := time.Now().UTC()
	population, err := metrics.Read(t.Context(), database.Pool, nil, "INFRA", now.Add(-14*24*time.Hour), now)
	if err != nil {
		t.Fatalf("read the metrics population: %v", err)
	}
	if root := population.TreeRoot("INFRA-3"); root != "INFRA-1" {
		t.Errorf("the metrics put INFRA-3 in tree %q, want INFRA-1, as the board does", root)
	}
}

// TestOneBoardAnswerReadsItsCardsAndItsTreesFromOneSnapshot: a board answer takes its cards and
// the daemon's half of its tree walk from one repeatable-read transaction (readSampled), so a
// sampler commit during the answer moves both or neither. The commit here lands, on another
// connection, the moment the board's first statement reading legion_current ends: it moves the
// child from `planning` in INFRA-1's tree to `testing` in a tree of its own. The answer is the
// first read's whole and the next answer the second read's whole, where a read outside that
// transaction would have put the child's `planning` card in the second read's tree.
func TestOneBoardAnswerReadsItsCardsAndItsTreesFromOneSnapshot(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	commits := storetest.NewCommitBetweenReads("legion_current")
	handler, _, _ := newTestServer(t, testServerOptions{store: storetest.TracedStore(t, database, commits)})
	seedLegionProjects(t, handler, "INFRA")
	daemon := newFakeLegionDaemon(t, handler, "INFRA")
	seedLegionIssue(t, database, "INFRA", 1, "The root", "a", nil, "")
	seedLegionIssue(t, database, "INFRA", 2, "The child", "b", nil, "INFRA-1")
	daemon.pass(t, database, `{"issues":{
		"INFRA-1":{"key":"INFRA-1","phase":"implementing","tree":"INFRA-1"},
		"INFRA-2":{"key":"INFRA-2","phase":"planning","tree":"INFRA-1"}}}`)

	commits.Arm(func() {
		daemon.pass(t, database, `{"issues":{
			"INFRA-1":{"key":"INFRA-1","phase":"implementing","tree":"INFRA-1"},
			"INFRA-2":{"key":"INFRA-2","phase":"testing","tree":"INFRA-2"}}}`)
	})
	board := legionBoardOf(t, handler, "INFRA")
	if !commits.Fired() {
		t.Fatal("no statement of the board's answer read legion_current, so no commit landed during it")
	}
	if got := legionColumnKeys(t, board, legionColumnPlanning); !slices.Equal(got, []string{"INFRA-2"}) {
		t.Errorf("the answer the commit landed in holds %v in planning, want INFRA-2: its card is the first read's", got)
	}
	if tree := legionCard(t, board, "INFRA-2").Tree; tree != "INFRA-1" {
		t.Errorf("the answer the commit landed in puts INFRA-2's card in tree %q, want INFRA-1: the first read's, as its card is", tree)
	}

	board = legionBoardOf(t, handler, "INFRA")
	if got := legionColumnKeys(t, board, legionColumnTesting); !slices.Equal(got, []string{"INFRA-2"}) {
		t.Errorf("the next answer holds %v in testing, want INFRA-2: the commit's", got)
	}
	if tree := legionCard(t, board, "INFRA-2").Tree; tree != "INFRA-2" {
		t.Errorf("the next answer puts INFRA-2 in tree %q, want INFRA-2: the commit's", tree)
	}
}

// TestOneBoardAnswerReadsItsHeaderAndItsCardsFromOneSnapshot: a board answer takes its header
// (legion_reads) and its cards (legion_current) from one snapshot, so a sampler commit during the
// answer moves both or neither. The newest read before the answer failed, which the header names
// over the cards of the good read before it; the commit, a good read that moves INFRA-1 to
// `testing`, lands on another connection the moment the board's first statement reading either
// table ends. The answer is the failed read's header over `planning`, and the next answer the
// commit's good header over `testing`, where reading the two tables at two moments would have
// named the failure over the cards of the read after it.
func TestOneBoardAnswerReadsItsHeaderAndItsCardsFromOneSnapshot(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	commits := storetest.NewCommitBetweenReads("legion_reads", "legion_current")
	handler, _, _ := newTestServer(t, testServerOptions{store: storetest.TracedStore(t, database, commits)})
	seedLegionProjects(t, handler, "INFRA")
	daemon := newFakeLegionDaemon(t, handler, "INFRA")
	seedLegionIssue(t, database, "INFRA", 1, "The issue", "a", nil, "")
	daemon.pass(t, database, `{"issues":{"INFRA-1":{"key":"INFRA-1","phase":"planning","tree":"INFRA-1"}}}`)
	daemon.pass(t, database, `not json`)

	commits.Arm(func() {
		daemon.pass(t, database, `{"issues":{"INFRA-1":{"key":"INFRA-1","phase":"testing","tree":"INFRA-1"}}}`)
	})
	board := legionBoardOf(t, handler, "INFRA")
	if !commits.Fired() {
		t.Fatal("no statement of the board's answer read legion_reads or legion_current, so no commit landed during it")
	}
	if board.Header.ReadOK || board.Header.ReadError == nil {
		t.Errorf("the answer the commit landed in has the header %+v, want the failed read the commit came after", board.Header)
	}
	if got := legionColumnKeys(t, board, legionColumnPlanning); !slices.Equal(got, []string{"INFRA-1"}) {
		t.Errorf("the answer the commit landed in holds %v in planning, want INFRA-1: the cards under the failed read's header are the good read's before it", got)
	}

	board = legionBoardOf(t, handler, "INFRA")
	if !board.Header.ReadOK {
		t.Errorf("the next answer has the header %+v, want the commit's good read", board.Header)
	}
	if got := legionColumnKeys(t, board, legionColumnTesting); !slices.Equal(got, []string{"INFRA-1"}) {
		t.Errorf("the next answer holds %v in testing, want INFRA-1: the commit's", got)
	}
}

// TestARecordPostgresRefusesIsAFailedReadAndTheBoardKeepsTheLastGoodRead: a read the daemon
// answered and Postgres refused to store — a wait whose text carries a lone surrogate escape, which
// jsonb refuses as no JSON — is a failed read. The board's header names the refusal, its cards are
// still the last good read's, and no row says the refused read succeeded: the read's row and its
// observations are one transaction.
func TestARecordPostgresRefusesIsAFailedReadAndTheBoardKeepsTheLastGoodRead(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	daemon := newFakeLegionDaemon(t, handler, "INFRA")
	seedLegionIssue(t, database, "INFRA", 1, "The implementer's issue", "a", nil, "")
	seedLegionIssue(t, database, "INFRA", 2, "The tester's issue", "b", nil, "")

	daemon.pass(t, database, `{"issues":{
		"INFRA-1":{"key":"INFRA-1","phase":"implementing","workers":{"implementer":{"claim":{"state":"working"}}}},
		"INFRA-2":{"key":"INFRA-2","phase":"testing","workers":{"tester":{"claim":{"state":"working"}}}}}}`)
	if header := legionBoardOf(t, handler, "INFRA").Header; !header.ReadOK {
		t.Fatalf("after the first read the header = %+v, want a good read", header)
	}
	// The next read moves INFRA-1 on, and puts INFRA-2 in a wait Postgres refuses to store.
	daemon.pass(t, database, `{"issues":{
		"INFRA-1":{"key":"INFRA-1","phase":"reviewing","workers":{"reviewer":{"claim":{"state":"working"}}}},
		"INFRA-2":{"key":"INFRA-2","phase":"awaiting_person","waitingOn":{"kind":"ask","detail":"\ud800"}}}}`)

	board := legionBoardOf(t, handler, "INFRA")
	header := board.Header
	if header.ReadOK || header.ReadError == nil || !strings.HasPrefix(*header.ReadError, "record: ") ||
		!strings.Contains(*header.ReadError, "SQLSTATE 22P02") {
		t.Errorf("the header = %+v (read_error %v), want the read failed, its error the refusal of its record", header, deref(header.ReadError))
	}
	for column, want := range map[string][]string{
		legionColumnImplementing: {"INFRA-1"},
		legionColumnTesting:      {"INFRA-2"},
		legionColumnReviewing:    {},
		legionColumnAwaiting:     {},
	} {
		if got := legionColumnKeys(t, board, column); !slices.Equal(got, want) {
			t.Errorf("the %s column holds %v, want %v: the last good read's cards", column, got, want)
		}
	}
	if roles := legionCard(t, board, "INFRA-1").Roles; !slices.Equal(roles, []string{"implementer"}) {
		t.Errorf("INFRA-1's roles are %v, want the last good read's implementer", roles)
	}

	rows, err := database.Pool.Query(t.Context(), `select ok, coalesce(error, '') from legion_reads where project = 'INFRA' order by id`)
	if err != nil {
		t.Fatalf("read the reads: %v", err)
	}
	var reads []string
	for rows.Next() {
		var ok bool
		var failure string
		if err := rows.Scan(&ok, &failure); err != nil {
			t.Fatalf("scan a read: %v", err)
		}
		reads = append(reads, fmt.Sprintf("ok=%v %s", ok, failure))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the reads: %v", err)
	}
	if len(reads) != 2 || reads[0] != "ok=true " || !strings.HasPrefix(reads[1], "ok=false record: ") {
		t.Errorf("legion_reads = %q, want the good read and then the refused one, and no row saying the refused read succeeded", reads)
	}
}

// TestTheLegionBoardOfAProjectWithNoDaemonIsRefused: a project that names no daemon has no board,
// which is what shows no view.
func TestTheLegionBoardOfAProjectWithNoDaemonIsRefused(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA", "LINUX")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/LINUX/legion/board", nil, "alice")
	if response.Code != http.StatusNotFound {
		t.Fatalf("the board of a project with no daemon: status=%d body=%s", response.Code, response.Body.String())
	}
	var refusal struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}
	if refusal.Code != "LEGION_NOT_CONFIGURED" {
		t.Fatalf("the refusal's code = %q, want LEGION_NOT_CONFIGURED", refusal.Code)
	}
}

// TestEachProjectsLegionBoardIsItsOwn: two projects, each with a daemon of its own, are answered
// their own boards (the spec's A4).
func TestEachProjectsLegionBoardIsItsOwn(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA", "LINUX")
	now := time.Now().UTC()
	for _, project := range []string{"INFRA", "LINUX"} {
		nameLegionDaemon(t, handler, project, "http://legion-"+project+".internal:13370")
		seedLegionBoard(t, database, project, now)
	}

	for _, project := range []string{"INFRA", "LINUX"} {
		board := legionBoardOf(t, handler, project)
		for _, column := range board.Columns {
			for _, card := range column.Cards {
				if key := card.Key; len(key) < len(project) || key[:len(project)] != project {
					t.Fatalf("%s's board holds %s", project, key)
				}
			}
		}
		if board.Header.QueueLength != 3 {
			t.Errorf("%s's queue is %d long, want its own three", project, board.Header.QueueLength)
		}
	}
}

// TestEachProjectsDaemonAnswersItsOwnLegionBoardAndMetrics is the spec's A4, "A test with two
// projects, each with a fake daemon, answers each project its own board and metrics", read
// through one pass of the sampler over two daemons. Two facts here cross from LINUX to INFRA, and
// neither is INFRA's: a LINUX worker files an issue into INFRA, which is a LINUX session's filing,
// and the LINUX daemon's state names an INFRA issue in review, which is LINUX's record and nothing
// INFRA's daemon reported. So each board shows its own daemon's read, and each project's metrics
// count its own issue, its own sessions' filings and its own daemon's observations.
func TestEachProjectsDaemonAnswersItsOwnLegionBoardAndMetrics(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA", "LINUX")
	infra := newFakeLegionDaemon(t, handler, "INFRA")
	linux := newFakeLegionDaemon(t, handler, "LINUX")
	start := time.Now().UTC()
	seedLegionIssue(t, database, "INFRA", 1, "INFRA's issue", "a", nil, "")
	seedLegionIssue(t, database, "LINUX", 1, "LINUX's issue", "a", nil, "")
	seedLegionRoleEvent(t, database, "INFRA-1", "s-infra-1", "implementer", "INFRA-1", start.Add(-2*time.Hour))
	seedLegionRoleEvent(t, database, "LINUX-1", "s-linux-1", "implementer", "LINUX-1", start.Add(-2*time.Hour))
	// The cross-project filing: LINUX-1's implementer creates INFRA-2.
	seedLegionIssue(t, database, "INFRA", 2, "Filed by LINUX's implementer", "b", nil, "")
	legionExec(t, database, `insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ('INFRA-2', 1, 'issue.created', $1::jsonb, '{"status":"triage"}', false, $2)`,
		`{"kind":"session","id":"s-linux-1","origin":{"session_title":"Legion implementer · LINUX-1"}}`, start.Add(-time.Hour))

	infra.serves(`{"issues":{
		"INFRA-1":{"key":"INFRA-1","phase":"implementing","workers":{"implementer":{"claim":{"state":"working"}}}}}}`)
	linux.serves(`{"issues":{
		"LINUX-1":{"key":"LINUX-1","phase":"testing","workers":{"tester":{"claim":{"state":"working"}}}},
		"INFRA-1":{"key":"INFRA-1","phase":"reviewing","workers":{"reviewer":{"claim":{"state":"working"}}}}}}`)
	legion.NewSampler(database.Pool).Pass(t.Context())

	infraBoard := legionBoardOf(t, handler, "INFRA")
	if got := legionColumnKeys(t, infraBoard, legionColumnImplementing); !slices.Equal(got, []string{"INFRA-1"}) {
		t.Errorf("INFRA's implementing column holds %v, want INFRA-1 as INFRA's daemon reports it", got)
	}
	if got := legionColumnKeys(t, infraBoard, legionColumnReviewing); len(got) != 0 {
		t.Errorf("INFRA's reviewing column holds %v, want none: LINUX's daemon put INFRA-1 there, and it is no source of INFRA's board", got)
	}
	if roles := legionCard(t, infraBoard, "INFRA-1").Roles; !slices.Equal(roles, []string{"implementer"}) {
		t.Errorf("INFRA-1's roles on INFRA's board are %v, want INFRA's implementer alone", roles)
	}
	linuxBoard := legionBoardOf(t, handler, "LINUX")
	if got := legionColumnKeys(t, linuxBoard, legionColumnTesting); !slices.Equal(got, []string{"LINUX-1"}) {
		t.Errorf("LINUX's testing column holds %v, want LINUX-1 as LINUX's daemon reports it", got)
	}
	if roles := legionCard(t, linuxBoard, "LINUX-1").Roles; !slices.Equal(roles, []string{"tester"}) {
		t.Errorf("LINUX-1's roles on LINUX's board are %v, want LINUX's tester alone", roles)
	}

	// Each project's issue closes after the reads, so the observations stand inside its life.
	closeLegionIssue(t, database, "INFRA-1", time.Now().UTC())
	closeLegionIssue(t, database, "LINUX-1", time.Now().UTC())
	now := time.Now().UTC()
	for _, one := range []struct {
		project string
		issue   string
		filed   []string
	}{
		{"INFRA", "INFRA-1", nil},
		{"LINUX", "LINUX-1", []string{"INFRA-2 by implementer of LINUX-1, " + metrics.ClassOutOfTree}},
	} {
		population, err := metrics.Read(t.Context(), database.Pool, nil, one.project, start.Add(-24*time.Hour), now.Add(time.Hour))
		if err != nil {
			t.Fatalf("read %s's metrics population: %v", one.project, err)
		}
		report := metrics.Compute(population, metrics.ScopeAll, now)
		if report.Finished != 1 || len(report.Rework.Issues) != 1 || report.Rework.Issues[0].Key != one.issue {
			t.Fatalf("%s's report finished %d issues, %+v, want its own %s alone", one.project, report.Finished, report.Rework.Issues, one.issue)
		}
		if rounds := report.Rework.Issues[0].ReviewRounds; rounds != 0 {
			t.Errorf("%s's %s has %d review rounds, want none: %s's daemon never reported it in review", one.project, one.issue, rounds, one.project)
		}
		var filed []string
		for _, row := range report.Splitting.Filed {
			filed = append(filed, fmt.Sprintf("%s by %s of %s, %s", row.Key, row.Filer, row.FilerIssue, row.Class))
		}
		if !slices.Equal(filed, one.filed) {
			t.Errorf("%s's report holds the filings %v, want %v: a filing belongs to the project of the session that filed it", one.project, filed, one.filed)
		}
	}
}

// closeLegionIssue closes one seeded issue, which is what brings its whole parent chain into the
// metrics' read scope (legion/metrics/population.go's scopeKeys: the issues that could have
// finished in the window and their ancestors).
func closeLegionIssue(t *testing.T, database *store.Store, key string, at time.Time) {
	t.Helper()
	legionExec(t, database, `update issues set status = 'done', closed_at = $2 where key = $1`, key, at)
}

// seedLegionRoleEvent writes one event of a session titled `Legion <role> · <titled>` on the
// issue. It is Legion phase work with no observation behind it where titled is the issue's own
// key; where it names another issue the event is a Legion role writing on an issue it does not
// work — a tracking issue it reports under — which is no phase work of this issue at all.
func seedLegionRoleEvent(t *testing.T, database *store.Store, issue, session, role, titled string, at time.Time) {
	t.Helper()
	seedSessionEvent(t, database, issue, session, "Legion "+role+" · "+titled, at)
}

// seedSessionEvent writes one event of the session on the issue under title, the way Dispatch
// learns a session's title: the actor's origin carries it, and the newest of them is the
// session's title (delivery/agents.go's ResolveSessionTitles). A title of no Legion shape is an
// ordinary session's, which the phase-work rule reads and finds no role in.
func seedSessionEvent(t *testing.T, database *store.Store, issue, session, title string, at time.Time) {
	t.Helper()
	legionExec(t, database, `insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		select $1, coalesce(max(seq), 0) + 1, 'message.created',
			jsonb_build_object('kind', 'session', 'id', $2::text,
				'origin', jsonb_build_object('session_title', $3::text)),
			'{}', false, $4
		from events where issue_key = $1`, issue, session, title, at)
}
