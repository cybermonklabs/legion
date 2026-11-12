package workflow

import (
	"strings"
	"testing"
)

// parkRule is the condition dispatch://INFRA-2971 puts beside every move off the queue a shipped
// text authorises: a child is parked only when a person asked for that move, or when its work is
// done, folded into another issue or no longer needed.
const parkRule = "only when a person asked for that move"

// The pr-merged notice tells a root's architect that the close waits on the issues beneath it. A
// notice that offers park_child as the way past that wait, with no condition, tells the architect
// to park unfinished work so the tree can close: the park this rule forbids.
func TestThePrMergedWaitNoticeOffersNoParkWithoutTheRule(t *testing.T) {
	reason := waitingForTree("LEGION-208", 42, []string{"LEGION-209", "LEGION-210"})
	if strings.Contains(reason, "park_child") && !strings.Contains(reason, parkRule) {
		t.Fatalf("the pr-merged notice offers park_child with no condition; a park it names must carry %q: %s", parkRule, reason)
	}
}
