// check_states_head_owners_test.go holds the settler to the pull requests a state event names
// (checkStatesAtHeads): each open pull request at the head, and each open pull request that was at
// it, whichever of them is at it now. The rig is check_states_test.go's.
package api

import (
	"slices"
	"testing"
)

// TestAStateAtAnOpenHeadNamesEachOpenPullRequestThatWasAtIt: #71 opens at H1, and #70 comes to H1
// and moves on to H2. #71 stays open at H1, so H1 is an open head of #71 and a former head of #70,
// which is open too: the tester's fail of k1 at H1 publishes there once, naming both.
func TestAStateAtAnOpenHeadNamesEachOpenPullRequestThatWasAtIt(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	upsertHead(t, rig, 71, eachHeadH1, rig.issue, "A change")
	settleOwed(t, rig)
	upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
	settleOwed(t, rig)
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	settleOwed(t, rig)

	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", eachHeadH1)
	events := settleOwed(t, rig)
	wantEvents(t, "the tester's fail of k1 at H1", events, stateWant{"k1", eachHeadH1, "no", "fail"})
	if !slices.Equal(events[0].Payload.PullRequests, []int{70, 71}) {
		t.Fatalf("the state of k1 at H1, where #71 is and #70 was, names the pull requests %v, want [70 71]",
			events[0].Payload.PullRequests)
	}
}
