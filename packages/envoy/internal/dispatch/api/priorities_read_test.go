// priorities_read_test.go reads the whole body of GET /api/v1/priorities (priority_queries.go)
// against the database store/storetest's Open clones from this package's migrated template,
// through the handler the neighbouring tests drive (newTestHandlerWithStore, dispatchRequest).
//
// The body is three readings at once: every live priority in rank order with the work that
// advances it counted over the projects the request named, and the two faults — work in flight
// under no priority, and priority work parked in `backlog` or `icebox`. So the fixture is one
// deployment's worth of work: issues of three projects under six priorities, in every status the
// counts separate, with a claim, an open ask, work closed inside the seven-day window and work
// closed outside it, and the two faults in both their shapes. The rows that hold each fault list
// to its fifty are written in bulk (priority_fixture_test.go), because a list cut at fifty needs
// more than fifty faults to cut.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// bulkFaultProject is the project whose bulk-written issues hold each fault list past the fifty
// rows it carries beside its count (priorityFaultRows), and bulkFaultRows is how many of each
// fault it writes.
const (
	bulkFaultProject = "BULK"
	bulkFaultRows    = 60
)

func TestPrioritiesRead(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	for _, project := range []string{"CORE", "OPS", "WEB"} {
		createTestProject(t, handler, project)
	}
	// The six priorities, in the order they are written, which is the order they are ranked and
	// the order the body draws them.
	written := []struct{ id, title, description, owner string }{
		{"ship-the-daemon", "Ship the daemon", "# Why\n\nThe daemon runs every workspace.", "alice"},
		{"cut-the-queue", "Cut the queue", "Nobody waits a day for a review.", "bob"},
		{"hold-the-line", "Hold the line", "The oldest work is finished first.", "alice"},
		{"keep-the-lights-on", "Keep the lights on", "The deployment answers every hour.", "bob"},
		{"park-the-bus", "Park the bus", "Work that waits says what it waits on.", "alice"},
		{"two-at-once", "Two at once", "Work may advance more than one priority.", "bob"},
	}
	ids := make([]string, 0, len(written))
	for _, priority := range written {
		ids = append(ids, createTestPriority(t, handler, map[string]any{
			"id": priority.id, "title": priority.title,
			"description": priority.description, "owner": priority.owner,
		}, "alice").ID)
	}

	// The work under the first priority: four open issues in four statuses over three projects,
	// one of them claimed and one carrying an open ask, and one issue closed inside the
	// seven-day window.
	coreRoot := seedPriorityIssue(t, handler, "CORE", "Daemon work", "in_progress", "", []string{"ship-the-daemon"})
	coreChild := seedPriorityIssue(t, handler, "CORE", "Daemon packaging", "todo", coreRoot, nil)
	seedPriorityIssue(t, handler, "OPS", "Runner work", "testing", "", []string{"ship-the-daemon"})
	webRoot := seedPriorityIssue(t, handler, "WEB", "Dashboard work", "needs_review", "", []string{"ship-the-daemon"})
	seedPriorityIssue(t, handler, "WEB", "Dashboard groundwork", "done", webRoot, nil)
	if claimed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+coreRoot+"/claim",
		map[string]any{}, "alice"); claimed.Code != http.StatusOK {
		t.Fatalf("claim %s: status=%d body=%s", coreRoot, claimed.Code, claimed.Body.String())
	}
	if asked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+coreChild+"/asks",
		map[string]any{"question": "Which packaging?"}, "alice"); asked.Code != http.StatusCreated {
		t.Fatalf("ask on %s: status=%d body=%s", coreChild, asked.Code, asked.Body.String())
	}

	// A priority every member of which stands in `todo`.
	queueRoot := seedPriorityIssue(t, handler, "CORE", "Queue work", "todo", "", []string{"cut-the-queue"})
	seedPriorityIssue(t, handler, "CORE", "Queue groundwork", "todo", queueRoot, nil)

	// Two priorities whose work is finished: one whose newest member event is eight days old,
	// which is outside the seven-day window, against one six days old, which is inside it.
	staleWork := seedPriorityIssue(t, handler, "CORE", "Old finished work", "done", "", []string{"hold-the-line"})
	recentWork := seedPriorityIssue(t, handler, "OPS", "Recent finished work", "done", "", []string{"keep-the-lights-on"})
	eightDaysAgo := time.Now().UTC().Add(-8 * 24 * time.Hour)
	sixDaysAgo := time.Now().UTC().Add(-6 * 24 * time.Hour)
	ageSeededIssue(t, database, staleWork, eightDaysAgo)
	ageSeededIssue(t, database, recentWork, sixDaysAgo)

	// An issue that advances a priority through its parent and stands in `backlog`: the second
	// fault, whose row says when it was moved and who moved it.
	parkedParent := seedPriorityIssue(t, handler, "CORE", "Parked work's parent", "todo", "", []string{"park-the-bus"})
	parkedChild := seedPriorityIssue(t, handler, "CORE", "Parked work", "todo", parkedParent, nil)
	moveTestIssue(t, handler, parkedChild, "backlog", "bob")

	// An issue both arms are true of: it states one priority itself and inherits another from
	// its parent, and its row in the parked fault names both.
	bothParent := seedPriorityIssue(t, handler, "WEB", "Both arms' parent", "todo", "", []string{"park-the-bus"})
	bothArms := seedPriorityIssue(t, handler, "WEB", "Both arms", "todo", bothParent, []string{"two-at-once"})
	moveTestIssue(t, handler, bothArms, "backlog", "alice")

	// The first fault: an issue in each of the four statuses work runs in, advancing no
	// priority. The `retro` one is a root whose child carries the relation, which is a fault
	// because a relation on a child places nothing above it.
	unplaced := map[string]string{
		"in_progress":  seedPriorityIssue(t, handler, "CORE", "Unplaced work in progress", "in_progress", "", nil),
		"testing":      seedPriorityIssue(t, handler, "OPS", "Unplaced work in testing", "testing", "", nil),
		"needs_review": seedPriorityIssue(t, handler, "WEB", "Unplaced work in review", "needs_review", "", nil),
		"retro":        seedPriorityIssue(t, handler, "CORE", "Unplaced root in retro", "retro", "", nil),
	}
	seedPriorityIssue(t, handler, "CORE", "The child that carries the relation", "todo",
		unplaced["retro"], []string{"two-at-once"})

	// The bulk rows that hold each fault list past its fifty.
	bulkUnplaced, bulkParked := seedBulkFaults(t, database, "park-the-bus")

	// The whole body, read as the page reads it.
	body := readPriorities(t, handler, "")
	if body.Empty != "" {
		t.Fatalf("the body of a deployment holding six priorities says %q, want no such sentence", body.Empty)
	}
	if got := priorityIDsOf(body); !slices.Equal(got, ids) {
		t.Fatalf("the priorities are drawn %v, want the rank order %v", got, ids)
	}
	for index, priority := range written {
		row := body.Priorities[index]
		if row.Description != priority.description || row.Owner != priority.owner {
			t.Fatalf("%s is drawn with description %q and owner %q, want %q and %q",
				row.ID, row.Description, row.Owner, priority.description, priority.owner)
		}
	}

	// The first priority's work: the counts by status over three projects, the claim, the open
	// ask, the issue closed inside the window and the newest event under it.
	ship := priorityRow(t, body, "ship-the-daemon")
	assertStatusCounts(t, ship, map[string]int{"in_progress": 1, "todo": 1, "testing": 1, "needs_review": 1})
	if ship.Work.Claimed != 1 || ship.Work.OpenAsks != 1 || ship.Work.ClosedLast7Days != 1 {
		t.Fatalf("ship-the-daemon's work = claimed %d, open asks %d, closed in seven days %d, want 1, 1 and 1",
			ship.Work.Claimed, ship.Work.OpenAsks, ship.Work.ClosedLast7Days)
	}
	if ship.Work.NewestEventAt == nil {
		t.Fatal("ship-the-daemon carries no newest event, want the newest event on an issue that advances it")
	}

	// A comment on a member moves the newest event, although it writes no issue row.
	beforeComment := *ship.Work.NewestEventAt
	if commented := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+coreChild+"/comments",
		map[string]any{"body": "The packaging is ready."}, "bob"); commented.Code != http.StatusCreated {
		t.Fatalf("comment on %s: status=%d body=%s", coreChild, commented.Code, commented.Body.String())
	}
	afterComment := priorityRow(t, readPriorities(t, handler, ""), "ship-the-daemon").Work.NewestEventAt
	if afterComment == nil || !afterComment.After(beforeComment) {
		t.Fatalf("the newest event under ship-the-daemon is %v after a comment on %s, want it past %v",
			afterComment, coreChild, beforeComment)
	}

	// A priority every member of which stands in `todo`.
	assertStatusCounts(t, priorityRow(t, body, "cut-the-queue"), map[string]int{"todo": 2})

	// The eight-day member against the six-day one: the older work is absent from the closed
	// count, and each priority's newest event is its own member's.
	stale := priorityRow(t, body, "hold-the-line")
	if stale.Work.ClosedLast7Days != 0 {
		t.Fatalf("hold-the-line counts %d issues closed in the last seven days, want none: its work closed eight days ago",
			stale.Work.ClosedLast7Days)
	}
	assertNewestEvent(t, stale, eightDaysAgo)
	recent := priorityRow(t, body, "keep-the-lights-on")
	if recent.Work.ClosedLast7Days != 1 {
		t.Fatalf("keep-the-lights-on counts %d issues closed in the last seven days, want the one closed six days ago",
			recent.Work.ClosedLast7Days)
	}
	assertNewestEvent(t, recent, sixDaysAgo)

	// The first fault: the four hand-written issues and the bulk rows, counted whole and by
	// project, with the newest fifty drawn.
	wantUnplaced := len(unplaced) + bulkFaultRows
	if body.WorkUnderNoPriority.Count != wantUnplaced {
		t.Fatalf("the work in flight under no priority = %d, want %d", body.WorkUnderNoPriority.Count, wantUnplaced)
	}
	if got := body.WorkUnderNoPriority.ByProject; got["CORE"] != 2 || got["OPS"] != 1 ||
		got["WEB"] != 1 || got[bulkFaultProject] != bulkFaultRows {
		t.Fatalf("the work in flight under no priority by project = %v, want CORE 2, OPS 1, WEB 1 and %s %d",
			got, bulkFaultProject, bulkFaultRows)
	}
	if len(body.WorkUnderNoPriority.Issues) != priorityFaultRows {
		t.Fatalf("the work in flight under no priority draws %d rows, want %d",
			len(body.WorkUnderNoPriority.Issues), priorityFaultRows)
	}
	drawn := map[string]model.PriorityIssue{}
	for _, issue := range body.WorkUnderNoPriority.Issues {
		drawn[issue.Key] = issue
		if issue.Advances == nil || len(issue.Advances) != 0 {
			t.Fatalf("%s is listed as work under no priority and advances %v, want the empty list",
				issue.Key, advanceEntries(issue.Advances))
		}
	}
	for status, key := range unplaced {
		row, listed := drawn[key]
		if !listed {
			t.Fatalf("the work in flight under no priority does not draw %s, which stands in %s", key, status)
		}
		if row.Status != status {
			t.Fatalf("the work in flight under no priority draws %s in %q, want %s", key, row.Status, status)
		}
	}

	// The second fault: the issue that advances through its parent, with the moment it was
	// moved and the actor who moved it, and no reason beside them.
	wantParked := 2 + bulkFaultRows
	if body.PriorityWorkParked.Count != wantParked {
		t.Fatalf("the parked priority work = %d, want %d", body.PriorityWorkParked.Count, wantParked)
	}
	if len(body.PriorityWorkParked.Issues) != priorityFaultRows {
		t.Fatalf("the parked priority work draws %d rows, want %d",
			len(body.PriorityWorkParked.Issues), priorityFaultRows)
	}
	// Each parked row carries its `advances` entries as the issue read answers them: own or
	// inherited from an issue above it.
	parked := parkedRow(t, body, parkedChild)
	if got := advanceEntries(parked.Advances); parked.Status != "backlog" ||
		!slices.Equal(got, []string{"park-the-bus inherited"}) {
		t.Fatalf("%s is parked in %q advancing %v, want backlog under park-the-bus inherited, which its parent states",
			parkedChild, parked.Status, got)
	}
	if parked.MovedAt == nil || parked.MovedBy == nil || *parked.MovedBy != (model.Actor{Kind: "user", ID: "bob"}) {
		t.Fatalf("%s was moved at %v by %v, want the moment and the person the events record",
			parkedChild, parked.MovedAt, parked.MovedBy)
	}
	if parked.MovedAt.After(time.Now().UTC().Add(time.Minute)) || parked.MovedAt.Before(eightDaysAgo) {
		t.Fatalf("%s says it was moved at %v, want the moment of the write this test made", parkedChild, parked.MovedAt)
	}
	assertNoReason(t, handler)

	// Both arms of one issue: its own relation and the one it inherits, named together on its
	// row and counted under each priority.
	together := parkedRow(t, body, bothArms)
	if got := advanceEntries(together.Advances); !slices.Equal(got, []string{"park-the-bus inherited", "two-at-once own"}) {
		t.Fatalf("%s is parked under %v, want the priority it inherits and the one it states, each marked",
			bothArms, got)
	}
	// The two parked issues both advance park-the-bus — one through its parent, one through
	// the parent it shares with its own relation — and the bulk rows state it themselves.
	if got := body.PriorityWorkParked.ByPriority; got["two-at-once"] != 1 || got["park-the-bus"] != 2+bulkFaultRows {
		t.Fatalf("the parked work by priority = %v, want two-at-once 1 and park-the-bus %d",
			got, 2+bulkFaultRows)
	}

	// Each fault list's count against its fifty rows and their order, read over the project
	// those rows belong to: the count is every faulted issue and the rows are the newest of
	// them, by the time each issue last changed and by the time each was moved.
	bulk := readPriorities(t, handler, "project="+bulkFaultProject)
	if bulk.WorkUnderNoPriority.Count != bulkFaultRows || len(bulk.WorkUnderNoPriority.Issues) != priorityFaultRows {
		t.Fatalf("the bulk project's work under no priority = %d issues drawn %d, want %d drawn %d",
			bulk.WorkUnderNoPriority.Count, len(bulk.WorkUnderNoPriority.Issues), bulkFaultRows, priorityFaultRows)
	}
	if got := faultKeys(bulk.WorkUnderNoPriority.Issues); !slices.Equal(got, bulkUnplaced[:priorityFaultRows]) {
		t.Fatalf("the bulk project's work under no priority is drawn %v, want the newest %d by the time each issue changed: %v",
			got, priorityFaultRows, bulkUnplaced[:priorityFaultRows])
	}
	if bulk.PriorityWorkParked.Count != bulkFaultRows || len(bulk.PriorityWorkParked.Issues) != priorityFaultRows {
		t.Fatalf("the bulk project's parked work = %d issues drawn %d, want %d drawn %d",
			bulk.PriorityWorkParked.Count, len(bulk.PriorityWorkParked.Issues), bulkFaultRows, priorityFaultRows)
	}
	if got := faultKeys(bulk.PriorityWorkParked.Issues); !slices.Equal(got, bulkParked[:priorityFaultRows]) {
		t.Fatalf("the bulk project's parked work is drawn %v, want the newest %d by the time each was moved: %v",
			got, priorityFaultRows, bulkParked[:priorityFaultRows])
	}

	// `?project=`: the counts and the two fault lists narrow, and the list of priorities does
	// not, since a priority belongs to no project.
	narrowed := readPriorities(t, handler, "project=CORE")
	if got := priorityIDsOf(narrowed); !slices.Equal(got, ids) {
		t.Fatalf("the narrowed read draws the priorities %v, want all six: a priority belongs to no project", got)
	}
	assertStatusCounts(t, priorityRow(t, narrowed, "ship-the-daemon"), map[string]int{"in_progress": 1, "todo": 1})
	if row := priorityRow(t, narrowed, "ship-the-daemon"); row.Work.Claimed != 1 || row.Work.ClosedLast7Days != 0 {
		t.Fatalf("ship-the-daemon narrowed to CORE = claimed %d, closed in seven days %d, want 1 and 0",
			row.Work.Claimed, row.Work.ClosedLast7Days)
	}
	if narrowed.WorkUnderNoPriority.Count != 2 || len(narrowed.WorkUnderNoPriority.ByProject) != 1 {
		t.Fatalf("the work under no priority narrowed to CORE = %d by %v, want the two of that project alone",
			narrowed.WorkUnderNoPriority.Count, narrowed.WorkUnderNoPriority.ByProject)
	}
	if got := faultKeys(narrowed.PriorityWorkParked.Issues); narrowed.PriorityWorkParked.Count != 1 ||
		!slices.Equal(got, []string{parkedChild}) {
		t.Fatalf("the parked work narrowed to CORE = %d rows %v, want %s alone",
			narrowed.PriorityWorkParked.Count, got, parkedChild)
	}

	// `?summary=true`: the two counts alone, which is what the navigation rail reads.
	summary := readPriorities(t, handler, "summary=true")
	if len(summary.Priorities) != 0 {
		t.Fatalf("the summary draws %d priorities, want none", len(summary.Priorities))
	}
	if summary.WorkUnderNoPriority.Count != wantUnplaced || summary.PriorityWorkParked.Count != wantParked {
		t.Fatalf("the summary counts %d and %d, want the whole read's %d and %d",
			summary.WorkUnderNoPriority.Count, summary.PriorityWorkParked.Count, wantUnplaced, wantParked)
	}
	if len(summary.WorkUnderNoPriority.Issues) != 0 || len(summary.PriorityWorkParked.Issues) != 0 {
		t.Fatalf("the summary draws %d and %d rows, want none of either",
			len(summary.WorkUnderNoPriority.Issues), len(summary.PriorityWorkParked.Issues))
	}
	if len(summary.WorkUnderNoPriority.ByProject) != 0 || len(summary.PriorityWorkParked.ByPriority) != 0 {
		t.Fatalf("the summary groups %v and %v, want neither grouping",
			summary.WorkUnderNoPriority.ByProject, summary.PriorityWorkParked.ByPriority)
	}

	// A deployment that has written no priority: the empty list, the sentence saying why, and
	// no error.
	empty, _ := newTestHandlerWithStore(t)
	emptyBody := readPriorities(t, empty, "")
	if len(emptyBody.Priorities) != 0 || emptyBody.Empty != priorityNoneWritten {
		t.Fatalf("the body of a deployment with no priority = %d priorities, empty %q, want none and %q",
			len(emptyBody.Priorities), emptyBody.Empty, priorityNoneWritten)
	}
	if emptyBody.WorkUnderNoPriority.Count != 0 || emptyBody.PriorityWorkParked.Count != 0 {
		t.Fatalf("the body of a deployment with no priority counts %d and %d faults, want none of either",
			emptyBody.WorkUnderNoPriority.Count, emptyBody.PriorityWorkParked.Count)
	}
}

// readPriorities reads GET /api/v1/priorities with the query the caller names.
func readPriorities(t *testing.T, handler http.Handler, query string) model.Priorities {
	t.Helper()
	target := "/api/v1/priorities"
	if query != "" {
		target += "?" + query
	}
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return decodeBody[model.Priorities](t, response)
}

// priorityIDsOf is the ids a body draws, in the order it draws them.
func priorityIDsOf(body model.Priorities) []string {
	ids := make([]string, 0, len(body.Priorities))
	for _, row := range body.Priorities {
		ids = append(ids, row.ID)
	}
	return ids
}

// priorityRow is one priority of a body, by its id.
func priorityRow(t *testing.T, body model.Priorities, id string) model.PriorityRow {
	t.Helper()
	for _, row := range body.Priorities {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("the body draws no priority %q: %v", id, priorityIDsOf(body))
	return model.PriorityRow{}
}

// parkedRow is one row of the parked fault, by its key.
func parkedRow(t *testing.T, body model.Priorities, key string) model.PriorityIssue {
	t.Helper()
	for _, issue := range body.PriorityWorkParked.Issues {
		if issue.Key == key {
			return issue
		}
	}
	t.Fatalf("the parked work draws no row for %s: %v", key, faultKeys(body.PriorityWorkParked.Issues))
	return model.PriorityIssue{}
}

// faultKeys is the keys one fault list draws, in the order it draws them.
func faultKeys(issues []model.PriorityIssue) []string {
	keys := make([]string, 0, len(issues))
	for _, issue := range issues {
		keys = append(keys, issue.Key)
	}
	return keys
}

// assertStatusCounts holds one priority's work to the counts it carries: every status of the
// lifecycle is answered, those the caller named at their count and every other at 0.
func assertStatusCounts(t *testing.T, row model.PriorityRow, want map[string]int) {
	t.Helper()
	if len(row.Work.ByStatus) != len(model.IssueStatuses) {
		t.Fatalf("%s counts %d statuses, want every status of the lifecycle: %v",
			row.ID, len(row.Work.ByStatus), row.Work.ByStatus)
	}
	for _, status := range model.IssueStatuses {
		if got := row.Work.ByStatus[status]; got != want[status] {
			t.Fatalf("%s counts %d issues in %s, want %d (%v)", row.ID, got, status, want[status], row.Work.ByStatus)
		}
	}
}

// assertNewestEvent holds a priority's newest event to the moment its members' events were aged
// to, within the minute the aging and the read are apart.
func assertNewestEvent(t *testing.T, row model.PriorityRow, want time.Time) {
	t.Helper()
	if row.Work.NewestEventAt == nil {
		t.Fatalf("%s carries no newest event, want one at %v", row.ID, want)
	}
	if row.Work.NewestEventAt.Sub(want).Abs() > time.Minute {
		t.Fatalf("%s's newest event is %v, want the %v its members' events carry",
			row.ID, row.Work.NewestEventAt, want)
	}
}

// assertNoReason reads the parked fault's rows as the bytes the route answered, and holds each
// to the fields the shape names: a status change records no reason, so no row carries one
// (model.PriorityIssue).
func assertNoReason(t *testing.T, handler http.Handler) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/priorities", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the priorities: status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		PriorityWorkParked struct {
			Issues []map[string]json.RawMessage `json:"issues"`
		} `json:"priority_work_parked"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the priorities: %v", err)
	}
	if len(body.PriorityWorkParked.Issues) == 0 {
		t.Fatal("the parked work draws no row; a reading of its fields proves nothing")
	}
	for _, issue := range body.PriorityWorkParked.Issues {
		for field := range issue {
			if field == "reason" {
				t.Fatalf("a parked row carries a reason: %v", issue)
			}
			// The priorities a row advances are its `advances` entries alone, never a bare
			// list of ids beside them.
			if field == "priorities" {
				t.Fatalf("a parked row carries a bare priorities list: %v", issue)
			}
		}
		for _, field := range []string{"moved_at", "moved_by", "advances", "changed_at"} {
			if _, carried := issue[field]; !carried {
				t.Fatalf("a parked row carries no %s: %v", field, issue)
			}
		}
	}
}

// seedPriorityIssue writes one issue through the routes and leaves it in the status the caller
// named: a create carries no status, so the status is a write of its own, which is also what
// records the `issue.updated` event the parked fault reads.
func seedPriorityIssue(t *testing.T, handler http.Handler, project, title, status, parent string, advances []string) string {
	t.Helper()
	body := map[string]any{"project": project, "title": title, "force": true}
	if parent != "" {
		body["parent"] = parent
	}
	if len(advances) > 0 {
		body["advances"] = advances
	}
	key := createAdvancingIssue(t, handler, body, "alice").Key
	if status != "" {
		moveTestIssue(t, handler, key, status, "alice")
	}
	return key
}

// moveTestIssue writes one issue's status through PATCH /api/v1/issues/{key}.
func moveTestIssue(t *testing.T, handler http.Handler, key, status, login string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key,
		map[string]any{"status": status}, login)
	if response.Code != http.StatusOK {
		t.Fatalf("move %s to %s: status=%d body=%s", key, status, response.Code, response.Body.String())
	}
}

// ageSeededIssue moves one issue and every event under it back to a moment of the caller's: the
// seven-day window and the newest event under a priority are read off the stored columns, and a
// test cannot wait days for them.
func ageSeededIssue(t *testing.T, database *store.Store, key string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		update issues
		set created_at = $2::timestamptz, updated_at = $2::timestamptz,
		    closed_at = case when closed_at is null then null else $2::timestamptz end
		where key = $1
	`, key, at); err != nil {
		t.Fatalf("age %s: %v", key, err)
	}
	if _, err := database.Pool.Exec(ctx, `update events set created_at = $2 where issue_key = $1`, key, at); err != nil {
		t.Fatalf("age %s's events: %v", key, err)
	}
}

// seedBulkFaults writes the rows that hold each fault list past the fifty it draws: bulkFaultRows
// issues in flight under no priority, and as many standing in `backlog` under one priority, each
// with the `issue.updated` event that says when it was moved there. It answers the keys of each,
// in the order the fault lists draw them — the unplaced by the time each issue last changed, the
// parked by the time each was moved, both newest first.
func seedBulkFaults(t *testing.T, database *store.Store, priority string) (unplaced, parked []string) {
	t.Helper()
	// An hour back, one minute apart, so every bulk row is older than the issues the test
	// writes through the routes and no two share a moment.
	base := time.Now().UTC().Add(-time.Hour)
	inFlight := priorityInFlightStatuses
	issues := make([][]any, 0, 2*bulkFaultRows)
	events := make([][]any, 0, bulkFaultRows)
	tuples := make([][]any, 0, bulkFaultRows)
	moved := seededStatusMovePayload(t, "backlog")
	for index := range bulkFaultRows {
		at := base.Add(-time.Duration(index) * time.Minute)
		unplacedKey := seededIssueKey(bulkFaultProject, index+1)
		issues = append(issues, seededIssueRow{
			key: unplacedKey, project: bulkFaultProject, number: index + 1,
			title:  fmt.Sprintf("Bulk work in flight %02d", index),
			status: inFlight[index%len(inFlight)], rank: seededRank(index), at: at,
		}.values())
		unplaced = append(unplaced, unplacedKey)

		parkedKey := seededIssueKey(bulkFaultProject, bulkFaultRows+index+1)
		issues = append(issues, seededIssueRow{
			key: parkedKey, project: bulkFaultProject, number: bulkFaultRows + index + 1,
			title: fmt.Sprintf("Bulk parked work %02d", index), status: "backlog",
			rank: seededRank(bulkFaultRows + index), at: at, lastSeq: 1,
		}.values())
		events = append(events, seededEvent(parkedKey, 1, "issue.updated", moved, at))
		tuples = append(tuples, seededAdvancesTuple(parkedKey, priority, at))
		parked = append(parked, parkedKey)
	}
	copySeededRows(t, database, "projects", seededProjectColumns,
		[][]any{seededProject(bulkFaultProject, 2*bulkFaultRows, base)})
	copySeededRows(t, database, "issues", seededIssueColumns, issues)
	copySeededRows(t, database, "events", seededEventColumns, events)
	copySeededRows(t, database, "priority_tuples", seededTupleColumns, tuples)
	return unplaced, parked
}
