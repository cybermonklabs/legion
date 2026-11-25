package replay

import (
	"os"
	"testing"
)

// pendingUntilBuilt names each test, and each fixture window of TestFixtureWindows, that was written
// before the code it judges, with the plan task whose code makes it pass. Each was seen failing on
// its own assertion before that code existed. skipUntilBuilt skips each one by its name, so that a
// run of this package stays a signal for every other test meanwhile. Each task's code commit removes
// its own names, and the list is empty before the pull request goes to review.
var pendingUntilBuilt = map[string]string{
	"TestEveryCandidateOfTheDesignIsARule":              "t27 (before-p0) and t32 (signals, signals-backoff)",
	"TestTheDocumentHasEveryRuleWindowBranchAndMeasure": "t35 (signals at limits) and t36 (no-work admissions)",
	"TestFixtureWindows/p0-at-a-full-cap":               "t27",
	"TestFixtureWindows/p0-at-its-bound":                "t27",
	"TestFixtureWindows/urgent-work-waits":              "t27 (main starts a P0 issue above the cap; its urgent waits are t29's, TestTheRecordOfUrgentWorkWaitsGivesItsHandWorkedWaits)",
	"TestFixtureWindows/signal-crosses-with-no-event":   "t32",
	"TestFixtureWindows/no-crossing-due":                "t32",
}

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
