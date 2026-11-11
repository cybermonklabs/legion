package metrics

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheFactsShareOutEachIssuesBarHours: each hour is counted once. An issue's parts add up to
// its work time less its queued time and less the hours it waited while a child worked (the
// child's own facts count those), an ask's hours are waiting on a person, and the rest go to the
// phases the status history draws, with no observation at all.
func TestTheFactsShareOutEachIssuesBarHours(t *testing.T) {
	root := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(10)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(6), "needs_review").
		statusAt(hour(10), "done").
		asked(hour(1), hour(2))
	child := newIssue("INFRA-2").under("INFRA-1").worked("implementer", hour(3)).closedAt(hour(5)).
		statusAt(hour(3), "in_progress").
		statusAt(hour(5), "done")
	population := fixture(hour(-24), hour(24), root, child)

	facts := ProjectFacts(population)
	work := map[string]WorkTimeIssue{}
	for _, row := range Compute(population, ScopeAll, hour(24)).WorkTime.Issues {
		work[row.Key] = row
	}
	// The root waited on its child from hour 3 to hour 5, and the child waited on none.
	waited := map[string]float64{"INFRA-1": 2, "INFRA-2": 0}
	for key, row := range facts {
		var sum float64
		for _, spent := range row.PhaseHours {
			sum += spent
		}
		want := work[key].Hours - work[key].QueuedHours - waited[key]
		if math.Abs(sum-want) > 0.001 {
			t.Errorf("%s holds %g h by phase, want its bar hours %g: %v", key, sum, want, row.PhaseHours)
		}
	}
	if got := facts["INFRA-1"]; got.PhaseHours[breakdownPerson] != 1 || got.PhaseHours[breakdownChildren] != 0 {
		t.Errorf("the root waited %g h on a person and holds %g h of waiting on children among its parts, want 1 and none: %v",
			got.PhaseHours[breakdownPerson], got.PhaseHours[breakdownChildren], got.PhaseHours)
	}
	if got := facts["INFRA-1"]; got.WorkHours != 10 || got.ReviewRounds != 1 || !got.Root || got.Tree != "INFRA-1" {
		t.Errorf("the root's facts are %+v, want 10 h of work, one review round and its own tree", got)
	}
	if got := facts["INFRA-2"]; got.WorkHours != 2 || got.Root || got.Tree != "INFRA-1" {
		t.Errorf("the child's facts are %+v, want 2 h of work in the root's tree", got)
	}
}

// TestEveryIssueWithPhaseWorkHasFactsWhateverItsHoursAre: an open issue's hours run to the
// population's end, an issue whose work took no measured time keeps an entry with no part, and an
// issue no Legion role worked has no entry at all.
func TestEveryIssueWithPhaseWorkHasFactsWhateverItsHoursAre(t *testing.T) {
	open := newIssue("INFRA-1").worked("implementer", hour(0)).statusAt(hour(0), "in_progress")
	instant := newIssue("INFRA-2").under("INFRA-1").worked("implementer", hour(4)).closedAt(hour(4))
	untouched := newIssue("INFRA-3").under("INFRA-1").closedAt(hour(6))
	facts := ProjectFacts(fixture(hour(-24), hour(8), open, instant, untouched))

	if got := facts["INFRA-1"]; got.WorkHours != 8 {
		t.Errorf("the open issue holds %g h of work, want the eight to the population's end: %+v", got.WorkHours, got)
	}
	got, has := facts["INFRA-2"]
	if !has || len(got.PhaseHours) != 0 || got.WorkHours != 0 {
		t.Errorf("the issue that closed where it opened has facts %+v (%t), want an entry with no part", got, has)
	}
	if _, worked := facts["INFRA-3"]; worked {
		t.Error("an issue no Legion role worked has facts, want none: a reader takes an entry for Legion worked it")
	}
}

// TestTheFactsCountTheSlotHoursAndTheDepthInTheTree: the slot hours are the stretch the issue's
// own `slot` observations say it held a slot, and the depth is the child_of links between the
// issue and its tree's root.
func TestTheFactsCountTheSlotHoursAndTheDepthInTheTree(t *testing.T) {
	// The root took slot 1 at hour 2 and gave it up at hour 7, so it held one for five hours.
	root := newIssue("INFRA-1").worked("architect", hour(0)).closedAt(hour(12)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(12), "done").
		observe(legion.FieldSlot, hour(2), "1").
		observe(legion.FieldSlot, hour(7), "null")
	child := newIssue("INFRA-2").under("INFRA-1").worked("implementer", hour(3)).closedAt(hour(9)).
		statusAt(hour(3), "in_progress").
		statusAt(hour(9), "done")
	grandchild := newIssue("INFRA-3").under("INFRA-2").worked("tester", hour(5)).closedAt(hour(8)).
		statusAt(hour(5), "in_progress").
		statusAt(hour(8), "done")
	facts := ProjectFacts(fixture(hour(-24), hour(24), root, child, grandchild))

	if got := facts["INFRA-1"]; got.SlotHours != 5 {
		t.Errorf("the root held a slot for %g h, want the five from hour 2 to hour 7: %+v", got.SlotHours, got)
	}
	if got := facts["INFRA-2"]; got.SlotHours != 0 {
		t.Errorf("the child held a slot for %g h, want none: it has no slot observation of its own", got.SlotHours)
	}
	depths := map[string]int{"INFRA-1": 0, "INFRA-2": 1, "INFRA-3": 2}
	for key, want := range depths {
		got := facts[key]
		if got.Tree != "INFRA-1" || got.TreeDepth != want {
			t.Errorf("%s is %d links below %s, want %d below INFRA-1", key, got.TreeDepth, got.Tree, want)
		}
	}
}

// TestTheFactsNameWhenAnIssueOpenedClosedAndWasWorked: the issue's own row gives the opening and
// the close, the bar spans give the first and the last moment of its measured work, and an issue
// whose work took no measured time has neither moment.
func TestTheFactsNameWhenAnIssueOpenedClosedAndWasWorked(t *testing.T) {
	worked := newIssue("INFRA-1").openedAt(hour(-2)).worked("implementer", hour(1)).closedAt(hour(6)).
		statusAt(hour(1), "in_progress").
		statusAt(hour(6), "done")
	instant := newIssue("INFRA-2").under("INFRA-1").openedAt(hour(0)).
		worked("implementer", hour(4)).closedAt(hour(4))
	open := newIssue("INFRA-3").under("INFRA-1").openedAt(hour(1)).
		worked("tester", hour(2)).statusAt(hour(2), "in_progress")
	facts := ProjectFacts(fixture(hour(-24), hour(8), worked, instant, open))

	got := facts["INFRA-1"]
	if !got.OpenedAt.Equal(hour(-2)) || got.ClosedAt == nil || !got.ClosedAt.Equal(hour(6)) {
		t.Errorf("the worked issue opened at %s and closed at %v, want hour -2 and hour 6", got.OpenedAt, got.ClosedAt)
	}
	if got.WorkFirstAt == nil || !got.WorkFirstAt.Equal(hour(1)) || got.WorkLastAt == nil || !got.WorkLastAt.Equal(hour(6)) {
		t.Errorf("its work ran from %v to %v, want hour 1 to hour 6", got.WorkFirstAt, got.WorkLastAt)
	}
	if got := facts["INFRA-2"]; got.WorkFirstAt != nil || got.WorkLastAt != nil {
		t.Errorf("the issue whose work took no measured time ran from %v to %v, want neither moment: %+v",
			got.WorkFirstAt, got.WorkLastAt, got)
	}
	if got := facts["INFRA-3"]; got.ClosedAt != nil || got.WorkLastAt == nil || !got.WorkLastAt.Equal(hour(8)) {
		t.Errorf("the open issue closed at %v and was last worked at %v, want no close and the population's end",
			got.ClosedAt, got.WorkLastAt)
	}
}

// twoTrees seeds two Legion trees of one project, each a root its architect opened with one
// child its implementer worked and closed: enough that a read of one tree's issues covers one
// tree and leaves the other unread.
func twoTrees(t *testing.T) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", sql, err)
		}
	}
	exec(`insert into projects (key, name) values ('INFRA', 'INFRA')`)
	session := func(title string) string {
		return fmt.Sprintf(`{"kind":"session","id":"s-%s","origin":{"session_title":%q}}`, title, title)
	}
	event := func(issue string, seq int, kind, actor, payload string, at time.Time) {
		t.Helper()
		exec(`insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			values ($1, $2, $3, $4::jsonb, $5::jsonb, false, $6)`, issue, seq, kind, actor, payload, at)
	}
	tree := func(number int, rank string, opened float64) {
		t.Helper()
		root := fmt.Sprintf("INFRA-%d", number)
		child := fmt.Sprintf("INFRA-%d", number+1)
		exec(`insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
			values ($1, 'INFRA', $2, 'A root', 'in_progress', $3, '{"kind":"user","id":"alice"}'::jsonb, $4)`,
			root, number, rank+"a", after(opened-1))
		exec(`insert into issues (key, project_key, number, title, status, rank, parent_key, created_by, created_at, closed_at)
			values ($1, 'INFRA', $2, 'A child', 'done', $3, $4, '{"kind":"user","id":"alice"}'::jsonb, $5, $6)`,
			child, number+1, rank+"b", root, after(opened), after(opened+10))
		event(root, 1, "issue.created", `{"kind":"user","id":"alice"}`, `{"status":"triage"}`, after(opened-1))
		event(root, 2, "comment.created", session("Legion architect · "+root), `{"status":"in_progress"}`, after(opened))
		event(child, 1, "issue.created", session("Legion architect · "+root),
			fmt.Sprintf(`{"status":"triage","parent":%q,"ancestors":[%q]}`, root, root), after(opened))
		event(child, 2, "comment.created", session("Legion implementer · "+child), `{"status":"in_progress"}`, after(opened+1))
		event(child, 3, "ask.opened", session("Legion implementer · "+child), `{"id":"ask-`+child+`"}`, after(opened+2))
		event(child, 4, "ask.answered", `{"kind":"user","id":"alice"}`, `{"id":"ask-`+child+`"}`, after(opened+3))
		event(child, 5, "issue.updated", `{"kind":"user","id":"alice"}`, `{"status":"needs_review"}`, after(opened+8))
		event(child, 6, "issue.closed", session("Legion merger · "+child), `{"status":"done"}`, after(opened+10))
		exec(`insert into handoffs (issue_key, phase, version, body, author, created_at)
			values ($1, 'implement', 1, 'done', '{"kind":"session","id":"s-impl"}'::jsonb, $2)`, child, after(opened+8))
		for _, observed := range []struct {
			value string
			at    float64
		}{{legion.PhaseImplementing, opened + 1}, {legion.PhaseReviewing, opened + 8}, {legion.PhaseDone, opened + 10}} {
			exec(`insert into legion_observations (project, issue_key, field, value, observed_at)
				values ('INFRA', $1, $2, $3::jsonb, $4)`,
				child, legion.FieldPhase, fmt.Sprintf("%q", observed.value), after(observed.at))
		}
	}
	tree(1, "m", 0)
	tree(5, "p", 4)
	return database
}

// TestTheFactsOfNamedIssuesAreTheFactsOfTheWholeProject: ReadIssues reads one tree where
// ReadProject reads every issue of the project, and the facts of the issues asked for are the
// same either way, field by field. A consumer can therefore recompute one issue's row without
// reading a whole project's event history.
func TestTheFactsOfNamedIssuesAreTheFactsOfTheWholeProject(t *testing.T) {
	database := twoTrees(t)
	now := after(48)
	keys := []string{"INFRA-1", "INFRA-2"}

	startedProject := time.Now()
	project, err := ReadProject(t.Context(), database.Pool, nil, "INFRA", now)
	projectRead := time.Since(startedProject)
	if err != nil {
		t.Fatalf("read the project: %v", err)
	}
	startedIssues := time.Now()
	named, err := ReadIssues(t.Context(), database.Pool, nil, "INFRA", keys, now)
	issuesRead := time.Since(startedIssues)
	if err != nil {
		t.Fatalf("read the named issues: %v", err)
	}

	whole, part := ProjectFacts(project), ProjectFacts(named)
	if len(whole) != 4 {
		t.Fatalf("the project's facts hold %d issues (%v), want the four with Legion work", len(whole), slices.Sorted(maps.Keys(whole)))
	}
	for _, key := range keys {
		want, got := whole[key], part[key]
		if !maps.Equal(want.PhaseHours, got.PhaseHours) || want.WorkHours != got.WorkHours ||
			want.ReviewRounds != got.ReviewRounds || want.Root != got.Root || want.Tree != got.Tree {
			t.Errorf("%s reads %+v from its own tree and %+v from the whole project, want one answer", key, got, want)
		}
		if want.WorkHours == 0 {
			t.Errorf("%s holds no work hours in either read, which proves nothing: %+v", key, want)
		}
	}
	// The other tree is outside the keys asked for, so its issues carry no Legion work this read
	// gathered and no facts: that is what makes the two reads different reads.
	if _, read := part["INFRA-6"]; read {
		t.Errorf("the facts of the tree nobody asked for are %+v, want none", part["INFRA-6"])
	}
	t.Logf("%d issues compared; ReadProject took %s and ReadIssues took %s", len(keys), projectRead, issuesRead)
}
