package legiontest

import (
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// TestTheSeededHistoryAnswersFactsForEveryIssueLegionWorked: a seed of the default history, read
// back through metrics.ReadProject and shared out by metrics.ProjectFacts, answers facts for at
// least the 200 issues the checks of this change count, and the four shapes the rig promises are
// in it: an open issue whose hours run to the moment of the computation, an issue whose work
// took no measured time and so holds no phase hours, an issue no Legion role worked and so has
// no entry, and a spread of phases that reaches every column of the board.
func TestTheSeededHistoryAnswersFactsForEveryIssueLegionWorked(t *testing.T) {
	database := storetest.Open(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

	started := time.Now()
	fixture := Seed(t, database.Pool, Spec{Project: "LEGION", Now: now})
	seeded := time.Since(started)

	population, err := metrics.ReadProject(t.Context(), database.Pool, nil, fixture.Project, now)
	if err != nil {
		t.Fatalf("read the project: %v", err)
	}
	facts := metrics.ProjectFacts(population)
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
	// and roots that waited on a person. A fixture whose every issue holds one part states
	// nothing about a computation that shares hours out part by part.
	parts, rounds, waited := 0, 0, 0
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
		if got.Tree == "" {
			t.Fatalf("%s holds the facts %+v and names no tree", key, got)
		}
	}
	if parts == 0 || rounds == 0 || waited == 0 {
		t.Errorf("%d issues hold three parts or more, %d hold two review rounds or more and %d roots waited on a person, want each above zero",
			parts, rounds, waited)
	}

	t.Logf("%d issues seeded in %s, %d with facts, %d open (%d of them with hours above zero), %d with no measured work, %d no role worked; the window runs %s to %s",
		len(fixture.Keys), seeded, len(facts), len(fixture.Open), working, empty, len(fixture.NoPhaseWork),
		fixture.From.Format(time.RFC3339), fixture.To.Format(time.RFC3339))
}
