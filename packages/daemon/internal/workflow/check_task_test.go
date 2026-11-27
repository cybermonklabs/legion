package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestTheCheckTaskNamesItsCheckInTheFormAReportSendsBack(t *testing.T) {
	t.Parallel()
	opened := time.Date(2026, 10, 19, 6, 0, 0, 123456000, time.UTC)
	issue := record.Issue{Key: "LEGION-208", Title: "a production check"}
	task := checkTask(issue, record.ProductionCheck{Issue: issue.Key, OpenedAt: opened, Looks: 2}, "merge", "merge-43", false, nil)
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
	{"the bound", wakeBound, boundText(14*24*time.Hour) + " since the check opened",
		"woken by the bound: 14 days since the check opened."},
}

// checkTaskFor is the task of a look woken by cause with detail, last marking the bound's look.
// One issue and one check for every case, so two tasks differ only in their wake.
func checkTaskFor(cause, detail string, last bool) string {
	issue := record.Issue{Key: "LEGION-208", Title: "A production check"}
	check := record.ProductionCheck{Issue: issue.Key, Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43",
		OpenedAt: checkOpened, Looks: 2}
	return checkTask(issue, check, cause, detail, last, nil)
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
	last := checkTaskFor(wakeBound, boundText(14*24*time.Hour)+" since the check opened", true)
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

// A defect a look finds is routed as the tester's prompt routes it (prompts/go/tester.md) and as
// skill://legion-worker/references/findings.md says. A fail of the issue's own requirement starts
// the issue's next change and goes to no controller; a defect outside the issue goes as that
// reference says, and the task names the reference rather than a route of its own. The task is the
// text in front of the tester at every look, so a task that sent every such defect to the tester's
// own controller, or that kept the ask for a decision alone, would overrule the prompt it comes
// with.
func TestTheCheckTaskRoutesADefectAsTheTesterPromptDoes(t *testing.T) {
	t.Parallel()
	for _, last := range []bool{false, true} {
		task := checkTaskFor(wakeMerge, "merge-43", last)
		for _, want := range []string{
			"A fail starts the next change of this issue, so send it to no controller.",
			"A defect outside this issue still goes as skill://legion-worker/references/findings.md says.",
		} {
			if !strings.Contains(task, want) {
				t.Errorf("the check task (last %t) = %q, want it to say %q", last, task, want)
			}
		}
		for _, retired := range []string{"the controller topic your Legion addressing line names",
			"the controller's role token from your Legion addressing line", "only a defect that needs a decision"} {
			if strings.Contains(task, retired) {
				t.Errorf("the check task (last %t) still says %q", last, retired)
			}
		}
	}
}
