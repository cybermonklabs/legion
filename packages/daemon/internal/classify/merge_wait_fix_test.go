package classify

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// While a fix of the task runs beside its review, an approval that stands on the head GitHub
// reports `blocked` waits on nothing where that state waits quietly or ends the round: the check
// that blocks it is the fix's to make pass, and the task's end waits for every check a CI job runs.
// So the round does not wait on the merge state, and the state served for it shows no wait. A
// `blocked` head that is told keeps the wait beside a fix: here GitHub holds a review request open
// on a MERGEABLE head whose checks concluded, which no fix causes. Every other state GitHub would
// not merge from keeps the wait too, and with no fix `blocked` waits as before.
func TestARunningFixEndsTheWaitOfABlockedApproval(t *testing.T) {
	reviewer := record.PhaseRow{Decision: &record.ReviewDecision{State: "approved", Head: "x"}}
	requested := func(reviews int) func(*record.PullRequest) {
		return func(pr *record.PullRequest) {
			pr.Mergeability, pr.HeadChecks, pr.ReviewsRequested = record.MergeabilityMergeable, record.HeadChecksConcluded, &reviews
			pr.CheckedHead, pr.CheckRuns = "x", []record.AttemptRun{{Name: "unit", ID: 1, Head: "x", Conclusion: record.RunSucceeded}}
		}
	}
	for _, tc := range []struct {
		name  string
		state record.MergeState
		fix   int
		read  func(*record.PullRequest)
		waits bool
	}{
		{"blocked beside a fix", record.MergeStateBlocked, 1, nil, false},
		{"blocked with no fix", record.MergeStateBlocked, 0, nil, true},
		{"blocked beside a fix, a review request open", record.MergeStateBlocked, 1, requested(1), true},
		{"blocked with no fix, a review request open", record.MergeStateBlocked, 0, requested(1), true},
		{"blocked beside a fix, nothing holding it", record.MergeStateBlocked, 1, requested(0), false},
		{"behind beside a fix", record.MergeStateBehind, 1, nil, true},
		{"dirty beside a fix", record.MergeStateDirty, 1, nil, true},
		{"draft beside a fix", record.MergeStateDraft, 1, nil, true},
		{"unknown beside a fix", record.MergeStateUnknown, 1, nil, true},
		{"clean beside a fix", record.MergeStateClean, 1, nil, false},
	} {
		issue := record.Issue{Phase: phase.Reviewing, RunningFix: tc.fix}
		pr := &record.PullRequest{State: record.PullRequestOpen, HeadSHA: "x", MergeState: tc.state}
		if tc.read != nil {
			tc.read(pr)
		}
		if got := RoundWaitsOnMergeState(issue, reviewer, pr); got != tc.waits {
			t.Errorf("%s: an approval at merge state %q with running fix %d waits = %v, want %v", tc.name, tc.state, tc.fix, got, tc.waits)
		}
	}
}
