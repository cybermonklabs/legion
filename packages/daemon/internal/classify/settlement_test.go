package classify

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A run the decoder took as skipped (intake's decodeChecks, from the settlement's skipped group)
// reaches the fence skipped, with the word GitHub gave it, unless a list names it: the lists come
// first, so a skipped run whose name is in Failing is failed and one whose name is in Cancelled is
// cancelled, as the roles are told. A run in no list that carries no conclusion succeeded, as it
// did before a run could be skipped. Every run is stamped with the settlement's head, and the word
// travels unchanged whatever the run comes to, since it decides nothing.
func TestStampedRunsKeepASkippedRunUnlessAListNamesIt(t *testing.T) {
	const head = "h2"
	got := stampedRuns(SettlementCandidate{Head: head, Generation: 2, Snapshot: "h2-2",
		CheckRuns: []record.AttemptRun{
			{Name: "docs", ID: 1, Conclusion: record.RunSkipped, GitHubConclusion: "skipped"},
			{Name: "e2e", ID: 2, Conclusion: record.RunSkipped, GitHubConclusion: "skipped"},
			{Name: "image", ID: 3, Conclusion: record.RunSkipped, GitHubConclusion: "skipped"},
			{Name: "lint", ID: 4, GitHubConclusion: "success"},
			{Name: "old", ID: 5},
			{Name: "unit", ID: 6, GitHubConclusion: "timed_out"},
		},
		Failing: []string{"e2e", "unit"}, Cancelled: []string{"image"}})
	want := []record.AttemptRun{
		{Name: "docs", ID: 1, Head: head, Conclusion: record.RunSkipped, GitHubConclusion: "skipped"},
		{Name: "e2e", ID: 2, Head: head, Conclusion: record.RunFailed, GitHubConclusion: "skipped"},
		{Name: "image", ID: 3, Head: head, Conclusion: record.RunCancelled, GitHubConclusion: "skipped"},
		{Name: "lint", ID: 4, Head: head, Conclusion: record.RunSucceeded, GitHubConclusion: "success"},
		{Name: "old", ID: 5, Head: head, Conclusion: record.RunSucceeded},
		{Name: "unit", ID: 6, Head: head, Conclusion: record.RunFailed, GitHubConclusion: "timed_out"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stamped runs = %+v, want %+v: a skipped run kept unless a list names it, every word unchanged", got, want)
	}
}
