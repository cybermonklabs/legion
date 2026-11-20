package classify

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// An earlier settlement of a head can name fewer checks than a later one: the listener settles a
// commit once every check it has seen there is terminal, and a check that starts later is in the
// later settlement alone. Such a settlement compares equal to the later one's runs, check by check
// (CompareAttemptSets ignores a name only the stored side holds), and its lower generation is what
// makes it stale while the fence stands at the head. Once the fence is reset - a reopen, or a new
// generation's clear - the generation is gone with it, and the runs the pull request remembers
// settling for the head (record.PullRequest.SettledRuns) are what must keep that earlier
// settlement, delivered late or replayed from a give-up, from putting the head back to its earlier
// verdict: here, from turning the head that failed lint into one that passed every check.
func TestALateSettlementNamingFewerChecksOfAHeadWhoseFenceWasResetChangesNothing(t *testing.T) {
	earlier := SettlementCandidate{Head: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 5}}, Generation: 1,
		Snapshot: "head-1", Failing: []string{}, Cancelled: []string{}}
	later := SettlementCandidate{Head: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 5}, {Name: "lint", ID: 7}}, Generation: 2,
		Snapshot: "head-2", Failing: []string{"lint"}, Cancelled: []string{}}

	settled, applied := ApplyCheckSettlement(record.PullRequest{HeadSHA: "head", Failing: []string{}}, later)
	if !applied || !reflect.DeepEqual(settled.Failing, []string{"lint"}) {
		t.Fatalf("the later settlement applied %v with failing %v; want it applied, lint failing", applied, settled.Failing)
	}
	if got, applied := ApplyCheckSettlement(settled, earlier); applied || !reflect.DeepEqual(got.Failing, []string{"lint"}) {
		t.Fatalf("with the fence at head the earlier settlement applied %v, failing %v; want it stale", applied, got.Failing)
	}
	cleared := settled
	cleared.CheckedHead, cleared.Failing, cleared.Cancelled, cleared.CheckRuns, cleared.Statuses = "", []string{}, []string{}, []record.AttemptRun{}, []record.CommitStatus{}
	for name, reset := range map[string]record.PullRequest{"a reopen": Reopen(settled, "head"), "a new generation's clear": cleared} {
		if got, applied := ApplyCheckSettlement(reset, earlier); applied {
			t.Errorf("after %s the earlier settlement naming only ci applied, failing %v at %q; want it stale against the remembered ci and lint runs",
				name, got.Failing, got.CheckedHead)
		}
	}
}
