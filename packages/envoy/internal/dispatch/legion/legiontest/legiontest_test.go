package legiontest

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// TestTheSeededHistoryAnswersFactsForEveryIssueLegionWorked: a seed of the default history, read
// back through metrics.ReadProject and shared out by metrics.ProjectFactsClosed, grown to the
// read's end, answers facts for at least the 200 issues the checks of this change count, and the
// four shapes the rig promises are in it: an open issue whose hours run to the moment of the
// computation, an issue whose work took no measured time and so holds no phase hours, an issue no
// Legion role worked and so has no entry, and a spread of phases that reaches every column of the
// board.
func TestTheSeededHistoryAnswersFactsForEveryIssueLegionWorked(t *testing.T) {
	database := storetest.Open(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

	started := time.Now()
	fixture := Seed(t, database.Pool, Spec{Project: "LEGION", Now: now, AwaitingChildren: true})
	seeded := time.Since(started)

	population, err := metrics.ReadProject(t.Context(), database.Pool, fixture.Project, now)
	if err != nil {
		t.Fatalf("read the project: %v", err)
	}
	facts := map[string]metrics.IssueFacts{}
	for key, closed := range metrics.ProjectFactsClosed(population, nil) {
		facts[key] = closed.FactsAt(population.To)
	}
	if len(facts) < 200 {
		t.Fatalf("the facts hold %d issues of the %d seeded, want at least the 200 the checks count",
			len(facts), len(fixture.Keys))
	}
	if len(facts) != len(fixture.PhaseWork) {
		t.Errorf("the facts hold %d issues and the fixture names %d with Legion phase work, want one number",
			len(facts), len(fixture.PhaseWork))
	}
	for _, key := range fixture.PhaseWork {
		if _, has := facts[key]; !has {
			t.Fatalf("%s carries Legion phase work and has no facts entry", key)
		}
	}

	working := 0
	for _, key := range fixture.Open {
		got, has := facts[key]
		if !has {
			t.Fatalf("the open issue %s has no facts entry", key)
		}
		if got.WorkHours > 0 && len(got.PhaseHours) > 0 {
			working++
		}
	}
	if working == 0 {
		t.Errorf("none of the %d open issues holds work hours above zero, want the hours they have run to %s",
			len(fixture.Open), now.Format(time.RFC3339))
	}

	empty := 0
	for _, key := range fixture.NoMeasuredWork {
		got, has := facts[key]
		if !has {
			t.Fatalf("the issue %s whose work took no measured time has no facts entry", key)
		}
		if len(got.PhaseHours) == 0 && got.WorkHours == 0 {
			empty++
		} else {
			t.Errorf("%s holds %+v, want an entry with no phase hours at all", key, got)
		}
	}
	if empty == 0 {
		t.Error("no issue of the history holds an entry with no phase hours, which the facts' own rule needs")
	}

	for _, key := range fixture.NoPhaseWork {
		if got, has := facts[key]; has {
			t.Fatalf("%s has the facts %+v and no Legion role worked it, want no entry: a reader takes an entry for Legion worked it",
				key, got)
		}
	}

	for phase, column := range legion.PhaseColumns {
		if len(fixture.Phases[phase]) == 0 {
			t.Errorf("the history reaches no issue in phase %q, so the board's %q column is never reached", phase, column)
		}
	}

	// What the history is for: hours that land in more than one part, review rounds above one,
	// and roots that waited on a person and on children. A fixture whose every issue holds one
	// part states nothing about a computation that shares hours out part by part.
	parts, rounds, waited, waitedChildren := 0, 0, 0, 0
	for key, got := range facts {
		if len(got.PhaseHours) >= 3 {
			parts++
		}
		if got.ReviewRounds >= 2 {
			rounds++
		}
		if got.Root && got.PhaseHours[legion.ColumnAwaiting] > 0 {
			waited++
		}
		if got.Root && got.PhaseHours[legion.ColumnAwaitingChildren] > 0 {
			waitedChildren++
		}
		if got.Tree == "" {
			t.Fatalf("%s holds the facts %+v and names no tree", key, got)
		}
	}
	if parts == 0 || rounds == 0 || waited == 0 || waitedChildren == 0 {
		t.Errorf("%d issues hold three parts or more, %d hold two review rounds or more, %d roots waited on a person and %d roots hold awaiting_children hours, want each above zero",
			parts, rounds, waited, waitedChildren)
	}

	t.Logf("%d issues seeded in %s, %d with facts, %d open (%d of them with hours above zero), %d with no measured work, %d no role worked; the window runs %s to %s",
		len(fixture.Keys), seeded, len(facts), len(fixture.Open), working, empty, len(fixture.NoPhaseWork),
		fixture.From.Format(time.RFC3339), fixture.To.Format(time.RFC3339))
}

// TestEveryFinishedIssueOfATreeTheSamplerNeverSawHasWorkTime: before a project's first
// observation the metrics read an issue as out of the workflow from a `triage` status write to the
// next status write, and the work time leaves those hours out (metrics' outOfWorkflowByStatus).
// Every issue of the history is filed in `triage`, so an issue of a tree the sampler never saw
// has work time only because the seeder hands it to Legion (handOver). Over a history with trees
// the sampler saw and trees it never saw, parked children of both kinds among them, the rows the
// facts consumer computes for a whole project (legionfacts.ComputeProject) give every finished
// issue of an unobserved tree whose life, from its filing to its close, has a length a work time
// above zero. The child that closed where it opened has no such life and is left out.
func TestEveryFinishedIssueOfATreeTheSamplerNeverSawHasWorkTime(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	spec := Spec{Project: "UNSEEN", Trees: 4, Unobserved: 2, InBacklogOrIcebox: true, Now: now}
	fixture := Seed(t, database.Pool, spec)

	rows, _, err := legionfacts.ComputeProject(ctx, database.Pool, fixture.Project, now)
	if err != nil {
		t.Fatalf("compute the facts of %s: %v", fixture.Project, err)
	}
	// A tree the sampler saw holds an observation of some issue of it; the rows name each issue's
	// tree by its root.
	read, err := database.Pool.Query(ctx, `select distinct issue_key from legion_observations where project = $1`, fixture.Project)
	if err != nil {
		t.Fatalf("read the observed issues of %s: %v", fixture.Project, err)
	}
	defer read.Close()
	observedIssues := map[string]bool{}
	for read.Next() {
		var key string
		if err := read.Scan(&key); err != nil {
			t.Fatalf("scan an observed issue: %v", err)
		}
		observedIssues[key] = true
	}
	if err := read.Err(); err != nil {
		t.Fatalf("read the observed issues of %s: %v", fixture.Project, err)
	}
	observedTrees := map[string]bool{}
	for _, row := range rows {
		if observedIssues[row.Issue] {
			observedTrees[row.Tree] = true
		}
	}
	var unobserved []string
	for _, root := range fixture.Roots {
		if !observedTrees[root] {
			unobserved = append(unobserved, root)
		}
	}
	if len(unobserved) != spec.Unobserved {
		t.Fatalf("%d trees of %s hold no observation (%v), want the %d the spec names unobserved",
			len(unobserved), fixture.Project, unobserved, spec.Unobserved)
	}

	checked := 0
	var idle []string
	for _, row := range rows {
		if !slices.Contains(unobserved, row.Tree) || row.ClosedAt == nil || !row.ClosedAt.After(row.OpenedAt) {
			continue
		}
		checked++
		if row.WorkHours <= 0 {
			started := "never"
			if row.StartedAt != nil {
				started = row.StartedAt.Format(time.RFC3339)
			}
			idle = append(idle, row.Issue+" (filed "+row.OpenedAt.Format(time.RFC3339)+", work started "+started+
				", closed "+row.ClosedAt.Format(time.RFC3339)+")")
		}
	}
	if checked == 0 {
		t.Fatalf("no finished issue of the unobserved trees %v has a life of any length, so this test examines nothing", unobserved)
	}
	if len(idle) > 0 {
		t.Errorf("%d of the %d finished issues of the unobserved trees %v have no work time, so they read out of the workflow from their filing in `triage` to their close: %s",
			len(idle), checked, unobserved, strings.Join(idle, "; "))
	}
	t.Logf("%d finished issues of the %d unobserved trees %v checked, %d of them with no work time", checked, len(unobserved), unobserved, len(idle))
}
