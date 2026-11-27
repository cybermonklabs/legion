package testwait

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// siblingChildEnv tells the re-executed test binary to run the child tests below, which skip in an
// ordinary run. The parent names the children it wants with -test.run.
const siblingChildEnv = "TESTWAIT_SIBLING_CHILD"

// siblingChildTimeout is the -test.timeout the children run under: 60 s, well above the flat 30 s
// margin a wait of the whole budget gives up at (margin), so a hung wait really waits in the child
// rather than beginning already spent. It is a miniature of CI's own shape, where that same flat
// margin leaves a hung wait holding nearly the whole budget: here it holds half of 60 s, where
// daemon-go-race's -timeout 30m leaves it holding 29 min 30 s, so the rig understates the case.
const siblingChildTimeout = 60 * time.Second

// siblingWork is how long the sibling child test works before it reports: 35 s, more than the
// 30 s a hung wait of the whole budget leaves after it gives up, and less than the whole deadline.
// So the sibling reports when it can start at once, and is still running at the alarm when a hung
// wait ran before it and held the binary to the margin.
const siblingWork = 35 * time.Second

// siblingParentNeeds is what a parent row needs of its own budget to run a child to its alarm and
// read it: the child's whole deadline and a little for building the output. A parent with less
// says so and skips rather than dying in its own alarm with nothing read. CI's own budgets
// (-timeout 15m, and 30m under the race detector) are far above it.
const siblingParentNeeds = siblingChildTimeout + 20*time.Second

// A wait whose fact never comes costs the test that waits, and no other: the test queued behind it
// in the same binary still reports its own verdict line. Before the share rule it did not. The
// hung wait held the binary until the margin before the deadline, failed there by name as the
// margin intends, and Go's alarm then panicked the binary with the test behind it still running,
// so that test's verdict was never printed: one wait, every other verdict of the package lost (one
// measured instance: daemon-go-race leg 3, job 140311446907, 47 top-level tests and 9 subtests
// lost to one wait of 1,729.51 s).
//
// The child binary runs two tests, one after the other, as a package's tests queue: one waits for
// a condition that never holds, then one works for siblingWork and reports. Every reading comes
// from the child's own output by name - which test ran first, each one's own verdict line, and
// whether Go's alarm panicked the binary - so a run that did not queue them in that order reports
// that rather than a verdict.
func TestAHungWaitLeavesItsSiblingsVerdictReported(t *testing.T) {
	child := runSiblingChild(t, "TestAWaitThatNeverHoldsChild", "TestSiblingReportsItsOwnVerdictChild")
	if !strings.Contains(child.output, "gave up waiting for the fact that never comes") {
		t.Errorf("the child's hung wait did not give up by name; its output was:\n%s", child.output)
	}
	if child.first != "--- FAIL: TestAWaitThatNeverHoldsChild" {
		t.Errorf("the waiting child's own verdict was %q, want %q: the wait must fail by name at its own bound, whatever becomes of the test behind it",
			child.first, "--- FAIL: TestAWaitThatNeverHoldsChild")
	}
	if child.sibling != "--- PASS: TestSiblingReportsItsOwnVerdictChild" {
		// The cause is read from the run, not assumed: with the alarm, the wait held the binary and
		// the test behind it died running; without it, that test failed or went silent on its own,
		// and the message must not blame a deadline the child never reached.
		cause := "that test failed or printed no verdict on its own, with no alarm in the child"
		if child.panicked {
			cause = "the wait held the binary until Go's alarm panicked it with that test still running"
		}
		t.Errorf("the test queued behind the hung wait reported %q, want %q: %s (child exit %v). The child's output was:\n%s",
			child.sibling, "--- PASS: TestSiblingReportsItsOwnVerdictChild", cause, child.exit, child.output)
	}
	if child.panicked {
		t.Errorf("Go's alarm panicked the child binary, so whatever was still running lost its verdict; its output was:\n%s", child.output)
	}
}

// The mirror of the row above, over the same two children with the hung wait replaced by one whose
// fact already holds: both children pass and the binary exits 0. It is what tells a reader that
// the rig can report a verdict at all, so the row above failing is the hung wait and not the rig.
func TestASiblingsVerdictIsReportedWhenNoWaitHangs(t *testing.T) {
	child := runSiblingChild(t, "TestAWaitThatHoldsAtOnceChild", "TestSiblingReportsItsOwnVerdictChild")
	if child.first != "--- PASS: TestAWaitThatHoldsAtOnceChild" {
		t.Errorf("the waiting child's own verdict was %q, want %q: its fact holds at once", child.first, "--- PASS: TestAWaitThatHoldsAtOnceChild")
	}
	if child.sibling != "--- PASS: TestSiblingReportsItsOwnVerdictChild" {
		t.Errorf("the test queued behind reported %q, want %q (panic in the child: %t; child exit %v): the rig cannot show a queued test reporting, so the row beside this one proves nothing. The child's output was:\n%s",
			child.sibling, "--- PASS: TestSiblingReportsItsOwnVerdictChild", child.panicked, child.exit, child.output)
	}
	if child.exit != nil || child.panicked {
		t.Errorf("the child binary did not exit 0 with nothing hung (exit %v, panic %t); its output was:\n%s", child.exit, child.panicked, child.output)
	}
}

// childRun is what a parent reads of one child binary: its whole output, the verdict line of the
// test that ran first and of the sibling (empty where the binary printed none), whether Go's alarm
// panicked it, and its exit error.
type childRun struct {
	output   string
	first    string
	sibling  string
	panicked bool
	exit     error
}

// runSiblingChild runs this binary again over first and sibling, under siblingChildTimeout. The
// children are serial, so go test runs them one after the other in the order they are declared,
// which is the queue a package's tests are in: a test that holds the binary delays the tests after
// it. It is read, not assumed: the run fails here unless the child's own output shows first
// beginning before sibling.
func runSiblingChild(t *testing.T, first, sibling string) childRun {
	t.Helper()
	// Remaining, not WholeRemaining: the guard only has to be enough, it is enough under either
	// rule, and it names a function a tree without the share rule also has, so the same file can
	// be planted in both trees of a forcing.
	if left := Remaining(t); left < siblingParentNeeds {
		t.Skipf("this row runs a child binary to its own %s deadline, so it needs about %s of the test binary's budget and has %s: run it under a longer -timeout",
			siblingChildTimeout, siblingParentNeeds, left.Round(time.Second))
	}
	child := exec.Command(os.Args[0], "-test.run=^("+first+"|"+sibling+")$", "-test.timeout="+siblingChildTimeout.String(), "-test.v")
	child.Env = append(os.Environ(), siblingChildEnv+"=1")
	combined, err := child.CombinedOutput()
	output := string(combined)
	run := childRun{output: output, exit: err, panicked: strings.Contains(output, "panic: test timed out")}
	firstRun := strings.Index(output, "=== RUN   "+first+"\n")
	siblingRun := strings.Index(output, "=== RUN   "+sibling+"\n")
	if firstRun < 0 || siblingRun < 0 || firstRun > siblingRun {
		t.Fatalf("the child did not queue %s before %s (their === RUN lines are at %d and %d, -1 for a line the child never printed), so this run proves nothing about a test queued behind a wait; its output was:\n%s",
			first, sibling, firstRun, siblingRun, output)
	}
	run.first = verdictOf(output, first)
	run.sibling = verdictOf(output, sibling)
	return run
}

// verdictOf is the child's own verdict line for the test named name, without its duration, and
// empty where the child printed none - the binary having died before it reported.
func verdictOf(output, name string) string {
	found := regexp.MustCompile(`--- (?:PASS|FAIL|SKIP): ` + regexp.QuoteMeta(name) + ` \(`).FindString(output)
	return strings.TrimSuffix(found, " (")
}

// TestAWaitThatNeverHoldsChild is the hung wait: its fact never comes, so it holds the binary to
// its own bound and fails there. It is serial, so the sibling declared after it waits for it.
func TestAWaitThatNeverHoldsChild(t *testing.T) {
	siblingChild(t)
	Eventually(t, "the fact that never comes", func() bool { return false })
}

// TestAWaitThatHoldsAtOnceChild is the hung wait's mirror: the same wait over a fact that is
// already true, which holds the binary for no time at all.
func TestAWaitThatHoldsAtOnceChild(t *testing.T) {
	siblingChild(t)
	Eventually(t, "the fact that is already true", func() bool { return true })
}

// TestSiblingReportsItsOwnVerdictChild works for siblingWork and reports. It is the verdict the
// parent reads: a test that did nothing wrong, queued behind a wait that hangs.
func TestSiblingReportsItsOwnVerdictChild(t *testing.T) {
	siblingChild(t)
	time.Sleep(siblingWork)
}

// siblingChild skips a child test in an ordinary run of this package.
func siblingChild(t *testing.T) {
	t.Helper()
	if os.Getenv(siblingChildEnv) == "" {
		t.Skip(siblingChildEnv + " is not set: this test is a child of " + fmt.Sprintf("%s", "TestAHungWaitLeavesItsSiblingsVerdictReported"))
	}
}
