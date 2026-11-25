package workflow

import (
	"strings"
	"testing"
)

// R8 of dispatch://INFRA-3686 keeps dispatch://INFRA-4533's R1 whole (the architect's ruling),
// whose first sentence is "Each text below states R7", and R7 of dispatch://INFRA-904 (approved
// v85) is two sentences: "A requirement that only the deploy can show is part of the production
// check, and one not met there, or still not observed at the last look, fails that check. A last
// look that nobody reported gives no verdict, and the daemon closes it as not run, which is no
// pass." The last look's task gives the first sentence in three fields and nothing of the second:
// that an unreported last look closes the check as not run, the one consequence of that look's
// silence, is in the checking role's prompt alone. Main's last-look task said it ("This last look's
// pending carries no wake_at and closes the check as not run").
func TestTheLastLooksTaskSaysAnUnreportedLastLookClosesTheCheckAsNotRun(t *testing.T) {
	for _, tc := range checkTaskWakes {
		_, task := checkTaskFor(tc.cause, tc.detail, true)
		lower := strings.ToLower(task)
		if !strings.Contains(lower, "not_run") && !strings.Contains(lower, "not run") {
			t.Errorf("the last look's task, woken by %s, does not state R7's second sentence (an unreported last look closes the check as not run):\n%s", tc.name, task)
		}
	}
}
