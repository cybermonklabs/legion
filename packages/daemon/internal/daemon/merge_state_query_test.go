package daemon

import "testing"

// A pull request a base's batch reads over GraphQL is the pull request GET /pulls/{number}
// answers (githubPullRequest): GitHub's GraphQL state OPEN is the REST state `open`, and MERGED
// and CLOSED are both `closed`, which only Merged tells apart, since REST answers a merged pull
// request `closed` too. Each GraphQL state is spelled here as GitHub spells it, so a converter
// that compares the state with any other spelling reads an open or a merged pull request as one
// that was closed without a merge.
func TestAGraphQLStateReadsAsTheRESTStateAndWhetherItMerged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		state  string
		rest   string
		merged bool
	}{
		{state: "OPEN", rest: "open", merged: false},
		{state: "MERGED", rest: "closed", merged: true},
		{state: "CLOSED", rest: "closed", merged: false},
	} {
		got := graphQLPull{Number: 86, State: tc.state}.githubPullRequest()
		if got.State != tc.rest || got.Merged != tc.merged {
			t.Errorf("GraphQL state %s reads as REST state %q, merged %t; want %q, merged %t", tc.state, got.State, got.Merged, tc.rest, tc.merged)
		}
	}
}
