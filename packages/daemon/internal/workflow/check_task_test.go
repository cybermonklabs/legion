package workflow

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// checkTaskWakes are the four wakes a look can have, each written as its own caller writes it:
// the merge that opened the check (openCheck, the merge commit as the detail), the wake the
// checking agent named on a pending report (checkReported, the report's summary), the retry of a
// look nobody reported (endLook, retryOfLook and the cause it was given), and the check's bound
// (checkBound). The last three are the ones a reader of the task must tell apart, so each case
// names the fields the task has to carry for it.
var checkTaskWakes = []struct {
	name    string
	cause   string
	detail  string
	wokenBy string
	retryOf string
	summary string
}{
	{"the merge", wakeMerge, "merge-43", texts.WokenByMerge, "", ""},
	{"the agent's own wake", wakeNamed, "production runs yesterday's build; the release train runs at 09:00", texts.WokenByNamed, "",
		"production runs yesterday's build; the release train runs at 09:00"},
	{"the retry of a look nobody reported", wakeNamed, retryOfLook + "look bound", texts.WokenByRetry, "look bound", ""},
	{"the bound", wakeBound, boundText(14*24*time.Hour) + " since the issue closed", texts.WokenByBound, "", ""},
}

// checkTaskFor is the task of a look woken by cause with detail, last marking the bound's look.
// One issue and one check for every case, so two tasks differ only in their wake, the check's
// bound 14 days from its opening.
func checkTaskFor(cause, detail string, last bool) (texts.CheckLookRecord, string) {
	issue := record.Issue{Key: "LEGION-208", Title: "A production check"}
	check := record.ProductionCheck{Issue: issue.Key, Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43",
		OpenedAt: checkOpened, BoundAt: checkOpened.Add(14 * 24 * time.Hour), Looks: 2, Role: claim.RoleTester}
	return CheckLook(issue, check, cause, detail, last), checkTask(issue, check, cause, detail, last)
}

// The task tells the checking agent what woke this look, and nothing else does: the session may be
// a fresh one on a woken pod, with no memory of the report it sent. A look the agent's own wake
// started carries the summary that report gave in a block by the checking role, and one the retry
// started names the retry and its cause, so a reader can tell the look it asked for from the look
// it was given after saying nothing. The first line is the one a resumed session tells the check
// from its last phase task by, and the legion tool's description quotes it.
func TestTheCheckTaskNamesWhatWokeItsOwnLook(t *testing.T) {
	for _, tc := range checkTaskWakes {
		t.Run(tc.name, func(t *testing.T) {
			look, task := checkTaskFor(tc.cause, tc.detail, false)
			read := readTaskOf(t, task)
			if want := "Production check of LEGION-208: A production check."; read.lines[0] != want {
				t.Errorf("the task of a look woken by %s opens %q, want it to open %q", tc.name, read.lines[0], want)
			}
			if look.WokenBy != tc.wokenBy || look.RetryOf != tc.retryOf || read.fields["woken_by"] != tc.wokenBy {
				t.Errorf("the look woken by %s names woken_by %q and retry_of %q, want %q and %q", tc.name, look.WokenBy, look.RetryOf, tc.wokenBy, tc.retryOf)
			}
			if tc.summary == "" {
				if len(read.blocks) != 0 {
					t.Errorf("the look woken by %s carries the blocks %+v, want none", tc.name, read.blocks)
				}
				return
			}
			if block := read.block(t, "summary"); block.text != tc.summary || block.attrs["by"] != string(claim.RoleTester) {
				t.Errorf("the summary block is %+v, want the report's summary by the tester", block)
			}
		})
	}
}

// The look names its facts: the look, whether it is the last, the rule of every look's verdict
// (not_met_requirement) and, on the last alone, the rest of that rule (unobserved_requirement and
// pending, dispatch://INFRA-4533 R1), the pull request, the merge commit (none where the record
// holds none, which the reader then reads from GitHub), and the check's bound in whole days with
// the time of the look at that bound.
func TestTheCheckTaskNamesTheFactsOfItsLook(t *testing.T) {
	look, _ := checkTaskFor(wakeBound, boundText(14*24*time.Hour)+" since the issue closed", true)
	want := texts.CheckLookRecord{Issue: "LEGION-208", Title: "A production check", Kind: texts.CheckLook, Look: 2, WokenBy: texts.WokenByBound,
		Last: true, NotMetRequirement: texts.LookNotMetRequirement, UnobservedRequirement: texts.LastLookUnobservedRequirement, Pending: texts.LastLookPending,
		PullRequest: "sjawhar/legion#43", MergeCommit: "merge-43", BoundDays: 14, LastLook: checkOpened.Add(14 * 24 * time.Hour).UTC()}
	if !reflect.DeepEqual(look, want) {
		t.Fatalf("the bound's look = %+v, want %+v", look, want)
	}
	for _, tc := range checkTaskWakes {
		before, task := checkTaskFor(tc.cause, tc.detail, false)
		if before.UnobservedRequirement != "" || before.Pending != "" ||
			strings.Contains(task, "unobserved_requirement") || readTaskOf(t, task).fields["pending"] != "" {
			t.Fatalf("a look before the last, woken by %s, = %+v, want no rule of the last look's verdict, which only the last look states:\n%s", tc.name, before, task)
		}
		if read := readTaskOf(t, task); before.NotMetRequirement != texts.LookNotMetRequirement || read.fields["not_met_requirement"] != texts.LookNotMetRequirement {
			t.Fatalf("a look before the last, woken by %s, = %+v, want the rule of every look's verdict (not_met_requirement: %s):\n%s", tc.name, before, texts.LookNotMetRequirement, task)
		}
	}
	issue := record.Issue{Key: "LEGION-208", Title: "A production check"}
	unknown := checkTask(issue, record.ProductionCheck{Issue: issue.Key, Repo: "sjawhar/legion", Number: 43, Looks: 1}, wakeMerge, "", false)
	if read := readTaskOf(t, unknown); read.fields["merge_commit"] != "" || strings.Contains(unknown, "merge_commit") {
		t.Fatalf("the look of a check whose record holds no merge commit = %q, want no merge commit named", unknown)
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
	// lastLookPending is the limit of the last look's pending in the checking role's prompt: no
	// requirement that only the deploy can show, since such a requirement still unobserved there
	// is a fail.
	lastLookPending = "only for an issue with no"
)

// ruleClauses are the clauses of text, a check task or a composed prompt, cut as
// internal/prompts's clauses cuts a prompt: each blank-line paragraph folded to one line, then cut
// after every ". " and every "; ".
func ruleClauses(text string) []string {
	var found []string
	for _, paragraph := range strings.Split(text, "\n\n") {
		for _, sentence := range strings.SplitAfter(strings.Join(strings.Fields(paragraph), " "), ". ") {
			found = append(found, strings.SplitAfter(sentence, "; ")...)
		}
	}
	return found
}

// isFailClause reports whether clause makes a requirement still unobserved a fail. A text may name
// the last look as "when no look follows this one" and call a fail a defect, so the clause holds
// "unobserved" and "fail" or "defect".
func isFailClause(clause string) bool {
	return strings.Contains(clause, "unobserved") && (strings.Contains(clause, "fail") || strings.Contains(clause, "defect"))
}

// limitFaults are the ways clause breaks the production check's rule, one message each: it states
// a limit the rule no longer has (bannedCheckLimits), or it is a fail clause (isFailClause) that
// still holds "production carries the merge" when every whetherOrNotCarried is taken out of it.
func limitFaults(clause string) []string {
	var faults []string
	for _, phrase := range bannedCheckLimits {
		if strings.Contains(clause, phrase) {
			faults = append(faults, fmt.Sprintf("says %q, a limit the rule no longer has: a requirement still unobserved at the last look is a fail %s: %q", phrase, whetherOrNotCarried, clause))
		}
	}
	if isFailClause(clause) && strings.Contains(strings.ReplaceAll(clause, whetherOrNotCarried, ""), "production carries the merge") {
		faults = append(faults, fmt.Sprintf("limits the fail of a requirement still unobserved by whether production carries the merge, but that requirement is a fail %s: %q", whetherOrNotCarried, clause))
	}
	return faults
}

// checkingPrompt is the composed prompt of a role a look reaches, as its pane reads it: every part
// Compose names, read back and joined in order, as reviewerPrompt reads the reviewer's.
type checkingPrompt struct {
	role claim.Role
	text string
}

// checkingPrompts are the composed prompts of the two roles a look reaches: the tester, which the
// project's production_check names, and the implementer, which keeps every check already open at
// the release that gave the check to the tester. The rules of the check are theirs, not the
// task's (CheckLook).
func checkingPrompts(t *testing.T) []checkingPrompt {
	t.Helper()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	var found []checkingPrompt
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer} {
		parts, err := composer.Compose(role, false)
		if err != nil {
			t.Fatalf("compose the %s's prompt: %v", role, err)
		}
		var prompt strings.Builder
		for _, path := range parts.RolePromptPaths {
			part, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read role prompt part %s: %v", path, err)
			}
			prompt.Write(part)
		}
		found = append(found, checkingPrompt{role: role, text: prompt.String()})
	}
	return found
}

// pendsAMergeNotCarried reports whether clause, a clause of a checking role's prompt, gives a
// pending for a merge production does not carry.
func pendsAMergeNotCarried(clause string) bool {
	return strings.Contains(clause, "`pending`") && strings.Contains(clause, "does not carry the merge")
}

// A merge production carries is no pass until each requirement of the issue that only the deploy
// can show is observed as that requirement names it, and observing something else is never that
// requirement observed. So the pass of the checking role's prompt covers each such requirement. A
// look before the last pends a merge production does not carry yet. It also pends a merge
// production carries with such a requirement still unobserved: a check told that pending is for
// the merge alone has no verdict for that case but the pass it is not (a check once reported pass
// at its last look with its requirement's own condition never met). The last look has no such
// pending. There a requirement still unobserved is a fail whether or not production carries the
// merge, and the pending is only for an issue with none of those requirements. The look's record
// says whether it is the last (`last`), and the checking role's prompt gives the rules: every
// clause of it that pends a merge production does not carry, or carries with a requirement still to
// observe, keeps that pending to a look with a look still to follow and names no last look, and one
// clause at least limits the last look's pending to an issue with no such requirement.
func TestALookBeforeTheLastPendsADeployedMergeWhoseRequirementIsStillUnobserved(t *testing.T) {
	for _, last := range []bool{false, true} {
		look, task := checkTaskFor(wakeMerge, "merge-43", last)
		if read := readTaskOf(t, task); look.Last != last || (read.fields["last"] == "true") != last {
			t.Errorf("the look (last %t) records last %t and its task names last %q", last, look.Last, read.fields["last"])
		}
	}
	const pendsAnUnobservedRequirement = "`pending` when production does not carry the merge yet, or carries it with a requirement"
	for _, prompt := range checkingPrompts(t) {
		folded := strings.Join(strings.Fields(prompt.text), " ")
		for _, want := range []string{
			"`pass` covers the changed path and each requirement of the issue that only the deploy can show, read from the spec version that bound the review",
			"as the requirement names it",
			"Observing something else is never that requirement observed",
		} {
			if !strings.Contains(folded, want) {
				t.Errorf("the composed %s prompt does not say %q", prompt.role, want)
			}
		}
		pends, lastPending := 0, 0
		for _, clause := range ruleClauses(prompt.text) {
			if strings.Contains(clause, pendsAnUnobservedRequirement) {
				pends++
			}
			if pendsAMergeNotCarried(clause) && (!strings.Contains(clause, "a look still to follow") || strings.Contains(clause, "last")) {
				t.Errorf("the composed %s prompt gives a pending for a merge production does not carry without keeping it to a look with a look still to follow, but on the last look a requirement still unobserved is a fail %s: %q", prompt.role, whetherOrNotCarried, clause)
			}
			if strings.Contains(clause, "last") && strings.Contains(clause, "`pending`") && strings.Contains(clause, lastLookPending) {
				lastPending++
			}
		}
		if pends == 0 {
			t.Errorf("the composed %s prompt holds no clause that says %q", prompt.role, pendsAnUnobservedRequirement)
		}
		if lastPending == 0 {
			t.Errorf("the composed %s prompt holds no clause that gives the last look's pending %q requirement that only the deploy can show", prompt.role, lastLookPending)
		}
	}
}

// The last look ends the check, so a requirement only the deploy can show that is still
// unobserved there is a fail, never a pending, whether or not production carries the merge. A
// requirement observed as not met is a fail on every look. The task of every wake and of both
// looks states the facts of its look, its record says whether it is the last, and no clause of it
// states a limit the rule no longer has (bannedCheckLimits) or limits a fail by whether production
// carries the merge (limitFaults). The rule is the checking role's prompt's, read clause by
// clause (ruleClauses): no clause of it breaks the rule either, one fail clause at least makes a
// requirement still unobserved at the last look a fail whether or not production carries the
// merge, and one makes a requirement observed as not met a fail, so the test cannot pass on a
// prompt that dropped the rule.
func TestTheLastLookFailsARequirementStillUnobservedWhetherOrNotProductionCarriesTheMerge(t *testing.T) {
	for _, tc := range checkTaskWakes {
		for _, last := range []bool{false, true} {
			look, task := checkTaskFor(tc.cause, tc.detail, last)
			if look.Last != last {
				t.Errorf("the look woken by %s (last %t) records last %t", tc.name, last, look.Last)
			}
			for _, clause := range ruleClauses(task) {
				for _, fault := range limitFaults(clause) {
					t.Errorf("the task of a look woken by %s (last %t) %s", tc.name, last, fault)
				}
			}
		}
	}
	for _, prompt := range checkingPrompts(t) {
		statements, observedNotMet := 0, 0
		for _, clause := range ruleClauses(prompt.text) {
			for _, fault := range limitFaults(clause) {
				t.Errorf("the composed %s prompt %s", prompt.role, fault)
			}
			if !isFailClause(clause) {
				continue
			}
			if strings.Contains(clause, whetherOrNotCarried) && strings.Contains(clause, "last") {
				statements++
			}
			if strings.Contains(clause, "observed as not met") {
				observedNotMet++
			}
		}
		if statements == 0 {
			t.Errorf("the composed %s prompt holds no clause that makes a requirement still unobserved at the last look a fail %s", prompt.role, whetherOrNotCarried)
		}
		if observedNotMet == 0 {
			t.Errorf("the composed %s prompt holds no fail clause for a requirement observed as not met", prompt.role)
		}
	}
}

// The last look's pending is only for an issue with none of the requirements that only the deploy
// can show, since for any other issue a requirement still unobserved there is a fail. That pending
// carries no wake_at and closes the check as not run, and before it the checking agent writes on
// the pull request why the check did not run within the bound. The task of the last look, for
// every wake, says it is the last (`last: true`), and no clause of it keeps a pending for a merge
// production does not carry, the case a look before the last pends. The task of a look before the
// last says nothing of a close as not run, since its pending names the time of the next look. The
// checking role's prompt gives the rules: one clause at least keeps the last look's pending to an
// issue with no such requirement, one says that pending carries no wake_at, closes the check as
// not run and is preceded by the pull request's account of why the check did not run within the
// bound, and no clause of the last look gives a pending for a merge production does not carry.
func TestTheLastLooksPendingIsOnlyForAnIssueWithNoneOfThoseRequirements(t *testing.T) {
	for _, tc := range checkTaskWakes {
		look, last := checkTaskFor(tc.cause, tc.detail, true)
		if read := readTaskOf(t, last); !look.Last || read.fields["last"] != "true" {
			t.Errorf("the last look woken by %s records last %t and its task names last %q, want true", tc.name, look.Last, read.fields["last"])
		}
		for _, clause := range ruleClauses(last) {
			if strings.Contains(clause, "pending") && strings.Contains(clause, "does not carry the merge") {
				t.Errorf("the task of the last look woken by %s keeps a pending for a merge production does not carry, but that pending is only for an issue with no requirement that only the deploy can show: %q", tc.name, clause)
			}
		}
		if _, before := checkTaskFor(tc.cause, tc.detail, false); strings.Contains(before, "closes the check as not run") {
			t.Errorf("the task of a look before the last, woken by %s, says its pending closes the check as not run, but only the last look's pending closes it: %q", tc.name, before)
		}
	}
	for _, prompt := range checkingPrompts(t) {
		limited, closes := 0, 0
		for _, clause := range ruleClauses(prompt.text) {
			if !strings.Contains(clause, "last") || !strings.Contains(clause, "`pending`") {
				continue
			}
			if strings.Contains(clause, lastLookPending) {
				limited++
			}
			if strings.Contains(clause, "no `wake_at`") && strings.Contains(clause, "closes the check as not run") && strings.Contains(clause, "why the check did not run within the bound") {
				closes++
			}
			if pendsAMergeNotCarried(clause) {
				t.Errorf("the composed %s prompt keeps the last look's pending for a merge production does not carry, but that pending is only for an issue with no requirement that only the deploy can show: %q", prompt.role, clause)
			}
		}
		if limited == 0 {
			t.Errorf("the composed %s prompt holds no clause that gives the last look's pending %q requirement that only the deploy can show", prompt.role, lastLookPending)
		}
		if closes == 0 {
			t.Errorf("the composed %s prompt holds no clause that says the last look's pending carries no `wake_at` and closes the check as not run, with why the check did not run within the bound written on the pull request first", prompt.role)
		}
	}
}
