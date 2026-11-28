package replay

import (
	"os"
	"testing"
)

// pendingUntilBuilt names each test, each fixture window of TestFixtureWindows and each run of one,
// that was written before the code it judges, with the plan task whose code makes it pass. Each was
// seen failing on its own assertion before that code existed. skipUntilBuilt skips each one by its
// name, so that a run of this package stays a signal for every other test meanwhile. Each task's
// code commit removes its own names, and the list is empty before the pull request goes to review.
//
// The three runs under main of R10's windows that waited on cybermonklabs/legion#5488 (t27 under
// ruling D16) pass since the forward merge that brought it turned main's field on
// (record.SlotRule.StartsNow), and the list names none of them.
var pendingUntilBuilt = map[string]string{}

// runPendingEnv is the environment variable that, set to 1, makes skipUntilBuilt skip nothing, so
// a check can run the pending tests to read what still fails.
const runPendingEnv = "LEGION_REPLAY_RUN_PENDING"

// skipUntilBuilt skips t where pendingUntilBuilt names it, unless LEGION_REPLAY_RUN_PENDING=1
// (runPendingEnv), with which it skips nothing: a run that sets it reads each pending test's own
// failures.
func skipUntilBuilt(t *testing.T) {
	t.Helper()
	if os.Getenv(runPendingEnv) == "1" {
		return
	}
	if task, pending := pendingUntilBuilt[t.Name()]; pending {
		t.Skipf("written before the code it judges; plan task %s builds that code (pendingUntilBuilt)", task)
	}
}
