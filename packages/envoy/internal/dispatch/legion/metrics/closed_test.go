package metrics

import (
	"reflect"
	"testing"
	"time"
)

// TestARunsEndIsAnInputTheFactsAreClosedAt: an issue's facts are closed at the newest input they
// read (newestInput), and the end of a run of a step of a role the deployment defines is one: the
// fallback timeline turns a failed run's return into implementing there (fallbackMarks). The open
// issue below was last written to at hour 4.25, the `in_progress` its checker's step starts with
// (Issue.stepStatus), and the checker's run fails and returns its task at hour 30, the next day,
// with nothing written after it. Closed at hour 4.25, its facts would be computed at the UTC
// midnight after that, hour 24, when the run had not ended, and grown from there the checker's
// bar would run on for ever: 32 hours at hour 36 rather than the 26 the run held, and the
// implementer's bar would stand still at 2 hours rather than grow from the return on.
func TestARunsEndIsAnInputTheFactsAreClosedAt(t *testing.T) {
	end := hour(30)
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).statusAt(hour(0), "in_progress").
		handedOff("implement", hour(2)).statusAt(hour(2), "testing").worked("tester", hour(2)).
		handedOff("test", hour(4)).statusAt(hour(4.25), "in_progress").
		stepRan("checker", hour(4), end, true)
	population := fixture(hour(-24), hour(36), issue)
	if got, want := timelineOf(population, issue), []string{"implementing 0-2", "testing 2-4", "step:checker 4-30", "implementing 30-36"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v", got, want)
	}
	// The computation itself reads the run's end: the checker holds the 26 hours its run held.
	if checker := factsAt(population, nil)[issue.Key].Bars["checker"].Hours; checker != 26 {
		t.Fatalf("the computation at hour 36 gives the checker %g hours, want 26", checker)
	}
	closed, held := ProjectFactsClosed(population, nil)[issue.Key]
	if !held {
		t.Fatalf("no closed facts of %s", issue.Key)
	}
	if closed.AsOf.Before(end) {
		t.Errorf("the facts are closed at %s, before the run's end at %s", closed.AsOf.UTC(), end.UTC())
	}
	for _, moment := range []time.Time{hour(31), hour(36), hour(60)} {
		want := factsAt(population.at(moment), nil)[issue.Key]
		got := closed.FactsAt(moment)
		for _, role := range []string{"checker", "implementer", "tester"} {
			if got.Bars[role].Hours != want.Bars[role].Hours {
				t.Errorf("at hour %g the %s bar = %g hours closed and grown, want the computation's %g",
					moment.Sub(base).Hours(), role, got.Bars[role].Hours, want.Bars[role].Hours)
			}
		}
	}
}
