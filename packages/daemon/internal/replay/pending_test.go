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
// The three runs under main of R10's windows wait on another pull request rather than a task of
// this plan (t27 under ruling D16): main starts no P0 issue outside the cap until
// cybermonklabs/legion#5488 merges and the forward merge that brings it turns main's field on
// (record.SlotRule.P0AboveCap), so their expected values, R10's, are those of each window's run
// under p0-at-once meanwhile, which passes. That forward merge removes the three names.
var pendingUntilBuilt = map[string]string{
	"TestTheDocumentHasEveryRuleWindowBranchAndMeasure": "t35 (signals at limits) and t36 (no-work admissions)",
	"TestFixtureWindows/p0-at-a-full-cap/main":          d16Main,
	"TestFixtureWindows/p0-at-its-bound/main":           d16Main,
	"TestFixtureWindows/urgent-work-waits/main":         d16Main,
	"TestFixtureWindows/signal-crosses-with-no-event":   "t32 replay integration (the replay's signals and deadlines under a rule; the rule values, the deadline fact and the shared definitions are built)",
	"TestFixtureWindows/no-crossing-due":                "t32 replay integration (the replay's signals and deadlines under a rule; the rule values, the deadline fact and the shared definitions are built)",
	"TestFixtureWindows/p0-inherited-across-projects":   t39Resolved + "; under p0-at-once ACME-2 and ACME-4 wait at the cap",
	"TestFixtureWindows/p0-inherited-ancestor-missing":  t39Resolved + "; p0-at-once replays the window rather than refuse it",
	"TestFixtureWindows/p0-recorded-descendant-update":  t39Resolved + "; under p0-at-once ACME-2 waits at the cap",
	"TestFixtureWindows/one-free-slot-opposite-order":   t39Resolved + "; under p0-at-once ACME-1 takes the free slot, not ACME-2",
}

// t39Resolved is the task of the four windows of spec v36's R10 that rest on the priority Dispatch
// resolves for an issue (pendingUntilBuilt): until it, a replay reads each issue's own priority
// under every rule, so each window's run under main misses the urgent waits and the banded priority
// taken its README works out from the resolved priority, and its run under p0-at-once misses the
// start or the refusal its README gives.
const t39Resolved = "t39 (the replay's resolved priority)"

// d16Main is the task of a run under main of a window of R10 (pendingUntilBuilt).
const d16Main = "t27 under ruling D16: main starts no P0 issue outside the cap until cybermonklabs/legion#5488 merges; " +
	"the window's run under p0-at-once holds R10 meanwhile"

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
