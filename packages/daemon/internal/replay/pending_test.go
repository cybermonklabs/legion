package replay

import "testing"

// pendingUntilBuilt names each test, and each fixture window of TestFixtureWindows, that was written
// before the code it judges, with the plan task whose code makes it pass. Each was seen failing on
// its own assertion before that code existed. skipUntilBuilt skips each one by its name, so that a
// run of this package stays a signal for every other test meanwhile. Each task's code commit removes
// its own names, and the list is empty before the pull request goes to review.
var pendingUntilBuilt = map[string]string{
	"TestEveryCandidateOfTheDesignIsARule":              "t27 (before-p0) and t32 (signals, signals-backoff)",
	"TestTheDocumentHasEveryRuleWindowBranchAndMeasure": "t29 (urgent waits), t35 (signals at limits) and t36 (no-work admissions)",
	"TestFixtureWindows/p0-at-a-full-cap":               "t27",
	"TestFixtureWindows/p0-at-its-bound":                "t27",
	"TestFixtureWindows/urgent-work-waits":              "t29",
	"TestFixtureWindows/signal-crosses-with-no-event":   "t32",
	"TestFixtureWindows/no-crossing-due":                "t32",
}

// skipUntilBuilt skips t where pendingUntilBuilt names it.
func skipUntilBuilt(t *testing.T) {
	t.Helper()
	if task, pending := pendingUntilBuilt[t.Name()]; pending {
		t.Skipf("written before the code it judges; plan task %s builds that code (pendingUntilBuilt)", task)
	}
}
