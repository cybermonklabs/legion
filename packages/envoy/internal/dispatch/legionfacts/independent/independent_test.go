package independent_test

import (
	"maps"
	"math"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts/independent"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// hourTolerance is how far apart two readings of one hour figure may stand: 0.1 hour, which is
// what R1 holds the two sides of its check to. Every count beside it is compared exactly.
const hourTolerance = 0.1

// issueFloor is the least number of issues one comparison proves anything over, which R1's check
// states: below it the fixture is too small to have reached the shapes the rules differ on.
const issueFloor = 200

// TestEveryFactEqualsTheIndependentReadingOfTheRequirement is the check of R1 itself: the one
// computation of the per-issue facts (metrics.ProjectFacts) against a reading written from the
// requirement's words (package independent), over one history of more than 200 issues with
// Legion phase work.
//
// It fails where the two disagree on any fact, below the floor of issues, where the history's
// issue with no measured work is missing from either side, and where the two sides read
// different scopes or different windows — a comparison of two scopes proves nothing about
// either.
func TestEveryFactEqualsTheIndependentReadingOfTheRequirement(t *testing.T) {
	database := storetest.Open(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: "LEGION", Now: now})

	population, err := metrics.ReadProject(t.Context(), database.Pool, fixture.Project, now)
	if err != nil {
		t.Fatalf("read the project's population: %v", err)
	}
	computed := metrics.ProjectFacts(population)
	computedScope := population.Project
	computedWindow := independent.Window{From: population.From, To: population.To}

	scope := independent.Scope{Project: fixture.Project}
	window := independent.Window{To: now}
	reading, err := independent.Read(t.Context(), database.Pool, scope, window)
	if err != nil {
		t.Fatalf("read the project independently: %v", err)
	}

	// The two sides are read for the same scope and the same window, or the comparison below
	// compares two different questions.
	if scope.String() != computedScope {
		t.Fatalf("the reading covered the scope %s and the computation the scope %s",
			scope, computedScope)
	}
	if window.String() != computedWindow.String() {
		t.Fatalf("the reading used the window %s and the computation the window %s",
			window, computedWindow)
	}

	compared, zeroHours, differences := 0, 0, 0
	for _, key := range slices.Sorted(maps.Keys(union(reading.Issues, computed))) {
		mine, read := reading.Issues[key]
		theirs, computedIt := computed[key]
		switch {
		case !read:
			t.Errorf("%s has facts from the computation and none from the reading", key)
			differences++
			continue
		case !computedIt:
			t.Errorf("%s has facts from the reading and none from the computation", key)
			differences++
			continue
		}
		compared++
		if mine.ZeroHours() {
			zeroHours++
		}
		differences += compareFacts(t, key, mine, theirs)
	}

	if compared < issueFloor {
		t.Errorf("the two sides compared %d issues, want at least %d", compared, issueFloor)
	}
	// The history holds an issue whose work took no measured time, and R1 keeps it. A reading
	// that dropped it would compare every other issue and say nothing about that rule.
	if zeroHours == 0 {
		t.Errorf("no issue of the %d compared has hours that are all zero, and the history holds %d",
			compared, len(fixture.NoMeasuredWork))
	}
	if differences > 0 {
		t.Errorf("%d facts differ between the reading and the computation", differences)
	}
	t.Logf("the independent reading covered scope %s over window %s; the computation covered scope %s over window %s",
		scope, window, computedScope, computedWindow)
	t.Logf("%d issues compared, %d of them with hours that are all zero, %d differences",
		compared, zeroHours, differences)
}

// compareFacts reads each of one issue's five facts against the other side's, and answers how
// many of them differ: the two hour figures to a tenth of an hour, and every count, the root
// flag and the tree root exactly.
func compareFacts(t *testing.T, key string, mine independent.Facts, theirs metrics.IssueFacts) int {
	t.Helper()
	differences := 0
	if math.Abs(mine.WorkHours-theirs.WorkHours) > hourTolerance {
		t.Errorf("%s worked %g h by the reading and %g h by the computation",
			key, mine.WorkHours, theirs.WorkHours)
		differences++
	}
	if mine.ReviewRounds != theirs.ReviewRounds {
		t.Errorf("%s has %d review rounds by the reading and %d by the computation",
			key, mine.ReviewRounds, theirs.ReviewRounds)
		differences++
	}
	if mine.Root != theirs.Root {
		t.Errorf("%s is a root by the reading (%t) and by the computation (%t)", key, mine.Root, theirs.Root)
		differences++
	}
	if mine.Tree != theirs.Tree {
		t.Errorf("%s is in tree %s by the reading and in tree %s by the computation",
			key, mine.Tree, theirs.Tree)
		differences++
	}
	// A phase one side left out holds no hours of that phase, so the two are compared over
	// every phase either side names.
	for _, phase := range slices.Sorted(maps.Keys(union(mine.PhaseHours, theirs.PhaseHours))) {
		if math.Abs(mine.PhaseHours[phase]-theirs.PhaseHours[phase]) > hourTolerance {
			t.Errorf("%s spent %g h in %s by the reading and %g h by the computation",
				key, mine.PhaseHours[phase], phase, theirs.PhaseHours[phase])
			differences++
		}
	}
	return differences
}

// union is every key either side holds.
func union[Left, Right any](left map[string]Left, right map[string]Right) map[string]struct{} {
	keys := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		keys[key] = struct{}{}
	}
	for key := range right {
		keys[key] = struct{}{}
	}
	return keys
}
