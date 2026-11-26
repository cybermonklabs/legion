package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestTheCheckTaskNamesItsCheckInTheFormAReportSendsBack(t *testing.T) {
	opened := time.Date(2026, 10, 19, 6, 0, 0, 123456000, time.UTC)
	issue := record.Issue{Key: "LEGION-208", Title: "a production check"}
	task := checkTask(issue, record.ProductionCheck{Issue: issue.Key, OpenedAt: opened, Looks: 2}, "merge", "merge-43", false)
	_, value, ok := strings.Cut(task, "check_opened_at `")
	if !ok {
		t.Fatal("the task gives no check identity to report")
	}
	value, _, ok = strings.Cut(value, "`")
	if !ok {
		t.Fatal("the task gives no closing delimiter for the check identity")
	}
	got, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || !got.Equal(opened) {
		t.Fatalf("reported check identity %q = %s (%v), want %s", value, got, err, opened)
	}
	if value != "2026-10-19T06:00:00.123456Z" {
		t.Fatalf("wire identity = %q, want RFC3339 with its microseconds", value)
	}
}

// checkTaskWakes are the four wakes a look can have, each written as its own caller writes it:
// the merge that opened the check (openCheck, the merge commit as the detail), the wake the
// checking agent named on a pending report (checkReported, the report's summary), the retry of a
// look nobody reported (endLook, its sentence and the cause it was given), and the check's bound
// (checkBound). The last three are the ones a reader of the task must tell apart, so each case
// names the words the task has to carry.
var checkTaskWakes = []struct {
	name   string
	cause  string
	detail string
	woke   string
}{
	{"the merge", wakeMerge, "merge-43", "woken by the merge."},
	{"the agent's own wake", wakeNamed, "production runs yesterday's build; the release train runs at 09:00",
		"woken by the wake named at the last look — production runs yesterday's build; the release train runs at 09:00."},
	{"the retry of a look nobody reported", wakeNamed, "the retry of a look nobody reported: look bound",
		"woken by the wake named at the last look — the retry of a look nobody reported: look bound."},
	{"the bound", wakeBound, boundText(14*24*time.Hour) + " since the issue closed",
		"woken by the bound: 14 days since the issue closed."},
}

// checkTaskFor is the task of a look woken by cause with detail, last marking the bound's look.
// One issue and one check for every case, so two tasks differ only in their wake.
func checkTaskFor(cause, detail string, last bool) string {
	issue := record.Issue{Key: "LEGION-208", Title: "A production check"}
	check := record.ProductionCheck{Issue: issue.Key, Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43",
		OpenedAt: checkOpened, Looks: 2}
	return checkTask(issue, check, cause, detail, last)
}

// The task tells the checking agent what woke this look, and nothing else does: the session may be
// a fresh one on a woken pod, with no memory of the report it sent. A look the agent's own wake
// started names the summary that report gave, and one the retry started names the retry, so a
// reader can tell the look it asked for from the look it was given after saying nothing.
func TestTheCheckTaskNamesWhatWokeItsOwnLook(t *testing.T) {
	t.Parallel()
	for _, tc := range checkTaskWakes {
		t.Run(tc.name, func(t *testing.T) {
			task := checkTaskFor(tc.cause, tc.detail, false)
			if !strings.Contains(task, tc.woke) {
				t.Errorf("the task of a look woken by %s = %q, want it to say %q", tc.name, task, tc.woke)
			}
			// The first line is the one a resumed session tells the check from its last phase task
			// by, and the skills and the legion tool's description quote it.
			if want := "Production check of LEGION-208: A production check.\n"; !strings.HasPrefix(task, want) {
				t.Errorf("the task of a look woken by %s opens %q, want it to open %q", tc.name, task, want)
			}
		})
	}
}

// Nothing the daemon reads from a deployment wakes a check any more: no Dispatch `issue.deployed`,
// no Dispatch build, no boot of the daemon's own build and no deploy workflow run. So no look's
// task may name one of them or tell its reader to wait in the session for a deploy, which would
// hold the pod for a wake that never comes. The look ends with the report, and the agent's own
// `wake_at` starts the next one.
func TestNoCheckTaskNamesADeployWakeOrTellsItsReaderToWait(t *testing.T) {
	t.Parallel()
	retired := []string{"issue.deployed", "deploy workflow", "daemon's boot", "Dispatch serving build",
		"Dispatch's build", "the deploy's event", "wakes the check on", "Wait for no deploy",
		"await no reply", "deploy role"}
	for _, tc := range checkTaskWakes {
		for _, last := range []bool{false, true} {
			task := checkTaskFor(tc.cause, tc.detail, last)
			for _, phrase := range retired {
				if strings.Contains(task, phrase) {
					t.Errorf("the task of a look woken by %s (last %t) still says %q", tc.name, last, phrase)
				}
			}
			if want := "Wait for nothing in this session: end the turn with the report."; !strings.Contains(task, want) {
				t.Errorf("the task of a look woken by %s (last %t) = %q, want it to say %q", tc.name, last, task, want)
			}
		}
	}
}

// A pending on a look that is not the check's last names when the next look is due, and the route
// refuses a pending without it (CHECK_WAKE_REQUIRED). Besides that wake, only the bound's look and
// the retry of a look nobody reported start one (checkBound, endLook), and the task says so. On the
// bound's look no look follows, the pending closes the check as not run, and a time there is
// refused (CHECK_WAKE_UNEXPECTED), so that task asks for none and says what to write before
// reporting it.
func TestTheCheckTaskAsksForAWakeOnEveryPendingButTheLastLooks(t *testing.T) {
	t.Parallel()
	const asks = "A pending report also carries wake_at, an RFC 3339 time: when you want your next look."
	task := checkTaskFor(wakeMerge, "merge-43", false)
	if !strings.Contains(task, asks) {
		t.Errorf("the task of a look that is not the last = %q, want it to ask %q", task, asks)
	}
	for _, want := range []string{
		"Besides it, only the look at the check's bound and the retry of a look nobody reported start one.",
		"the daemon stops this process and keeps this session",
		"a task naming the summary you gave as what woke it",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("the task of a look that is not the last = %q, want it to say %q", task, want)
		}
	}
	if retired := "Nothing else starts one."; strings.Contains(task, retired) {
		t.Errorf("the task of a look that is not the last still says %q, though the bound and a retry start a look too", retired)
	}
	last := checkTaskFor(wakeBound, boundText(14*24*time.Hour)+" since the issue closed", true)
	if strings.Contains(last, asks) {
		t.Errorf("the last look's task asks for a wake: %q", last)
	}
	for _, want := range []string{
		"This is the last look: no look follows.",
		"This last look's pending carries no wake_at and closes the check as not run",
	} {
		if !strings.Contains(last, want) {
			t.Errorf("the last look's task = %q, want it to say %q", last, want)
		}
	}
}

// bannedCheckLimits are limits of the last look that the production check's rule no longer has:
// each keeps the last look's pending, or its close as not run, for a merge that production does
// not carry. Each is joined from halves, as internal/prompts's retiredCheckSentences are, so a
// search of the repository for one of them finds the texts that state it and never this test.
// internal/prompts and internal/textcheck keep their own copies, since a test package cannot
// import another package's test helpers.
var bannedCheckLimits = []string{
	"not carrying" + " the merge alone",
	"did not carry the merge" + " within the bound",
	"never carried" + " the merge",
}

const (
	// whetherOrNotCarried are the words that keep a fail clause's mention of production from
	// limiting the fail.
	whetherOrNotCarried = "whether or not production carries the merge"
	// lastLookPending is the limit of the last look's pending: no requirement that only the deploy
	// can show, since such a requirement still unobserved there is a fail.
	lastLookPending = "pending only for an issue with none of those requirements"
)

// taskClauses are the clauses of a check task, cut as internal/prompts's clauses cuts a prompt:
// the task folded to one line, then cut after every ". " and every "; ". The task has no blank
// line, so it is one paragraph.
func taskClauses(task string) []string {
	var found []string
	for _, sentence := range strings.SplitAfter(strings.Join(strings.Fields(task), " "), ". ") {
		found = append(found, strings.SplitAfter(sentence, "; ")...)
	}
	return found
}

// isTaskFailClause reports whether clause makes a requirement still unobserved a fail. The task
// names the last look as "when no look follows this one", and it calls a fail a defect, so the
// clause holds "unobserved" and "fail" or "defect".
func isTaskFailClause(clause string) bool {
	return strings.Contains(clause, "unobserved") && (strings.Contains(clause, "fail") || strings.Contains(clause, "defect"))
}

// A merge production carries is no pass until each requirement of the issue that only the deploy
// can show is observed as that requirement names it, and observing something else is never that
// requirement observed. So on every look the task's pass asks for each such requirement. A look
// before the last pends a merge production does not carry yet. It also pends a merge production
// carries with such a requirement still unobserved: a check told that pending is for the merge
// alone has no verdict for that case but the pass it is not (a check once reported pass at its
// last look with its requirement's own condition never met). The last look has no such pending.
// There a requirement still unobserved is a fail whether or not production carries the merge, and
// the pending is only for an issue with none of those requirements.
func TestTheCheckTaskPendsADeployedMergeWhoseRequirementIsStillUnobserved(t *testing.T) {
	t.Parallel()
	const pendsAnUnobservedRequirement = "pending when production does not carry the merge yet, or carries it with one of those requirements still unobserved and a look still to follow,"
	for _, last := range []bool{false, true} {
		task := checkTaskFor(wakeMerge, "merge-43", last)
		for _, want := range []string{
			"observe each requirement of the issue that only the deploy can show, read from the spec version that bound the review, each as the requirement names it: observing something else is never that requirement observed.",
			"pass only once you drove the changed path in production and observed every one of those requirements,",
		} {
			if !strings.Contains(task, want) {
				t.Errorf("the check task (last %t) = %q, want it to say %q", last, task, want)
			}
		}
		if !last && !strings.Contains(task, pendsAnUnobservedRequirement) {
			t.Errorf("the task of a look before the last = %q, want it to say %q", task, pendsAnUnobservedRequirement)
		}
		if last && strings.Contains(task, pendsAnUnobservedRequirement) {
			t.Errorf("the last look's task says %q, but on the last look a requirement still unobserved is a fail %s: %q", pendsAnUnobservedRequirement, whetherOrNotCarried, task)
		}
		if last && !strings.Contains(task, lastLookPending) {
			t.Errorf("the last look's task = %q, want it to say %q", task, lastLookPending)
		}
	}
}

// The last look ends the check, so a requirement only the deploy can show that is still
// unobserved there is a fail, never a pending, whether or not production carries the merge. A
// requirement observed as not met is a fail on every look. The fail clause says so once, the same
// on every look, in terms of the look that follows, and the last look's task adds that no look
// follows. So the task of every wake and of both looks is read clause by clause (taskClauses). No
// clause states a limit the rule no longer has (bannedCheckLimits). A fail clause
// (isTaskFailClause) holds no "production carries the merge" outside the words "whether or not
// production carries the merge". One fail clause at least holds those words, and one holds
// "observed as not met", so the test cannot pass on a task that dropped the rule.
func TestTheLastLookFailsARequirementStillUnobservedWhetherOrNotProductionCarriesTheMerge(t *testing.T) {
	t.Parallel()
	for _, tc := range checkTaskWakes {
		for _, last := range []bool{false, true} {
			task := checkTaskFor(tc.cause, tc.detail, last)
			statements, observedNotMet := 0, 0
			for _, clause := range taskClauses(task) {
				for _, phrase := range bannedCheckLimits {
					if strings.Contains(clause, phrase) {
						t.Errorf("the task of a look woken by %s (last %t) says %q, a limit the rule no longer has: a requirement still unobserved at the last look is a fail %s: %q", tc.name, last, phrase, whetherOrNotCarried, clause)
					}
				}
				if !isTaskFailClause(clause) {
					continue
				}
				if strings.Contains(clause, whetherOrNotCarried) {
					statements++
				}
				if strings.Contains(clause, "observed as not met") {
					observedNotMet++
				}
				if strings.Contains(strings.ReplaceAll(clause, whetherOrNotCarried, ""), "production carries the merge") {
					t.Errorf("the task of a look woken by %s (last %t) limits the fail of a requirement still unobserved by whether production carries the merge, but that requirement is a fail %s: %q", tc.name, last, whetherOrNotCarried, clause)
				}
			}
			if statements == 0 {
				t.Errorf("the task of a look woken by %s (last %t) = %q, want a clause that makes a requirement still unobserved when no look follows a fail %s", tc.name, last, task, whetherOrNotCarried)
			}
			if observedNotMet == 0 {
				t.Errorf("the task of a look woken by %s (last %t) = %q, want a fail clause for a requirement observed as not met", tc.name, last, task)
			}
		}
	}
}

// The last look's task gives its pending only for an issue with none of the requirements that only
// the deploy can show, since for any other issue a requirement still unobserved there is a fail.
// The task says that pending carries no wake_at. Before that pending, the task has the tester write
// on the pull request why the check did not run within the bound. No clause of the last look's task
// keeps a pending for a merge production does not carry, the case a look before the last pends. The
// task of a look before the last says nothing of a close as not run, since its pending names the
// time of the next look.
func TestTheLastLooksPendingIsOnlyForAnIssueWithNoneOfThoseRequirements(t *testing.T) {
	t.Parallel()
	const closes = "This last look's pending carries no wake_at and closes the check as not run: before reporting it, write on the pull request why the check did not run within the bound."
	for _, tc := range checkTaskWakes {
		last := checkTaskFor(tc.cause, tc.detail, true)
		for _, want := range []string{lastLookPending, closes} {
			if !strings.Contains(last, want) {
				t.Errorf("the task of the last look woken by %s = %q, want it to say %q", tc.name, last, want)
			}
		}
		for _, clause := range taskClauses(last) {
			if strings.Contains(clause, "pending") && strings.Contains(clause, "does not carry the merge") {
				t.Errorf("the task of the last look woken by %s keeps a pending for a merge production does not carry, but that pending is only for an issue with no requirement that only the deploy can show: %q", tc.name, clause)
			}
		}
		before := checkTaskFor(tc.cause, tc.detail, false)
		if strings.Contains(before, "closes the check as not run") {
			t.Errorf("the task of a look before the last, woken by %s, says its pending closes the check as not run, but only the last look's pending closes it: %q", tc.name, before)
		}
	}
}

// A defect a look finds is routed as the tester's prompt routes it (prompts/go/tester.md) and as
// skill://legion-worker/references/findings.md says: a defect no tree owns goes to the controller
// of the project its repository maps to, and to the tester's own project's controller only where
// Dispatch maps the repository to none, and an ask covers a defect that needs a person's decision,
// a deploy window or work much larger than the tester's own, and any other finding that needs a
// person's authority, taste or risk appetite. The task is the text in front of the tester at every
// look, so a task that sent every such defect to the tester's own controller, or that kept the ask
// for a decision alone, would overrule the prompt it comes with.
func TestTheCheckTaskRoutesADefectAsTheTesterPromptDoes(t *testing.T) {
	for _, last := range []bool{false, true} {
		task := checkTaskFor(wakeMerge, "merge-43", last)
		for _, want := range []string{
			"when no tree owns that code, to the controller of the project that repository maps to, or to your own project's controller, the one your Legion addressing line names, when Dispatch maps it to none:",
			"a defect that needs a person's decision, a deploy window or work much larger than your own, and any other finding that needs a person's authority, taste or risk appetite, is a dispatch ask",
		} {
			if !strings.Contains(task, want) {
				t.Errorf("the check task (last %t) = %q, want it to say %q", last, task, want)
			}
		}
		for _, retired := range []string{"the controller's role token from your Legion addressing line", "only a defect that needs a decision"} {
			if strings.Contains(task, retired) {
				t.Errorf("the check task (last %t) still says %q", last, retired)
			}
		}
	}
}
