package workflow

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// sixtyChecks is a settlement of 60 checks at headOne, shaped as the listener and the decoder give it:
// 25 that did not pass (10 failed with GitHub's word failure, 5 failed timed_out, 5 cancelled, and
// 5 that no group names with a word GitHub gives a run that did not pass: action_required, stale,
// startup_failure), 5 GitHub skipped (3 in the skipped group with the word, 1 by the word alone, as
// a settlement whose skipped group the decoder could not read, and 1 by the group alone, as a
// listener that sends no word), and 30 that passed (20 success, 5 neutral, and 5 with no word,
// "unit tests", which the seeded round holds, among them). 30 commit statuses, 3 in state failure
// or error.
func sixtyChecks() (intake.PullRequestChecks, []string, map[string]string) {
	var runs []record.AttemptRun
	var failing, cancelled []string
	conclusion := map[string]string{}
	id := int64(0)
	add := func(run record.AttemptRun) {
		id++
		run.ID = id
		runs = append(runs, run)
	}
	for i := range 10 {
		name := fmt.Sprintf("f%02d", i)
		add(wordedRun(name, 0, "failure"))
		failing, conclusion[name] = append(failing, name), "failure"
	}
	for i := range 5 {
		name := fmt.Sprintf("t%02d", i)
		add(wordedRun(name, 0, "timed_out"))
		failing, conclusion[name] = append(failing, name), "timed_out"
	}
	for i := range 5 {
		name := fmt.Sprintf("c%02d", i)
		add(wordedRun(name, 0, "cancelled"))
		cancelled, conclusion[name] = append(cancelled, name), "cancelled"
	}
	for i, word := range []string{"action_required", "stale", "startup_failure", "action_required", "stale"} {
		name := fmt.Sprintf("x%02d", i)
		add(wordedRun(name, 0, word))
		conclusion[name] = word
	}
	skipped := []string{"s00", "s01", "s02", "s03", "s04"}
	for _, name := range skipped[:3] {
		add(skippedRun(name, 0))
	}
	add(wordedRun("s03", 0, "skipped"))
	add(record.AttemptRun{Name: "s04", Conclusion: record.RunSkipped})
	for i := range 20 {
		add(wordedRun(fmt.Sprintf("p%02d", i), 0, "success"))
	}
	for i := range 5 {
		add(wordedRun(fmt.Sprintf("n%02d", i), 0, "neutral"))
	}
	for i := range 4 {
		add(record.AttemptRun{Name: fmt.Sprintf("w%02d", i)})
	}
	add(record.AttemptRun{Name: "unit tests"})
	var statuses []record.CommitStatus
	for i := range 30 {
		state := record.CommitStatusState("success")
		switch i {
		case 3:
			state = "failure"
		case 7, 11:
			state = "error"
		}
		statuses = append(statuses, status(fmt.Sprintf("status-%02d", i), state, i))
	}
	fact := intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: headOne, CheckRuns: runs, Generation: 1,
		Snapshot: headOne + "-1", Failing: failing, Cancelled: cancelled, Statuses: statuses}
	return fact, skipped, conclusion
}

// checksTally is what one text names of the head's checks.
type checksTally struct {
	text       string
	notPassed  []texts.CheckConclusion
	skipped    []string
	statuses   []texts.StatusState
	passed     int
	listPassed string
	phase      phase.Phase
	wantPhase  bool
	payload    any
}

// sixtyCheckTexts is each text of the sixty-check head: the CI settlement, the review-stuck notice and the
// approved-round-stuck message of an approval GitHub will not merge from, the phase assignment of
// a retried reviewer that names that stuck round, and the wake task of a released worker the
// settlement woke.
func sixtyCheckTexts(t *testing.T) []checksTally {
	t.Helper()
	fact, _, _ := sixtyChecks()
	pool := migratedPool(t)
	seedMergeStateRound(t, pool, record.MergeStateBlocked)
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	apply("settled", fact)
	apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: headOne,
		HeadSHA: headOne, Author: testReviewApp, Body: reviewBody("ship it", headOne)})
	pr := recordedPR(t, pool, "LEGION-208")

	settled := CISettledMessage("LEGION-208", pr, phase.Reviewing)
	var told *texts.CISettledRecord
	for _, message := range roleMessageRows(t, pool, "LEGION-208") {
		if message.Kind != texts.CISettled {
			continue
		}
		rec, err := message.Record()
		if err != nil {
			t.Fatal(err)
		}
		told = rec.(*texts.CISettledRecord)
	}
	if told == nil {
		t.Fatalf("no ci-settled message was queued for the sixty-check settlement")
	}
	stuck := reviewStuckNotices(t, pool)
	if len(stuck) != 1 {
		t.Fatalf("%d review-stuck notices, want one", len(stuck))
	}
	approved, found := stuckApprovedRound(t, pool)
	if !found {
		t.Fatalf("no approved-round-stuck message")
	}
	assignment := PhaseAssignment(record.Issue{Key: "LEGION-208", Title: "root", Phase: phase.Reviewing}, record.PhaseRow{}, &pr,
		PhaseMove{Retried: "architect", Stuck: &StuckRound{Cause: texts.CauseMergeState, Head: headOne, MergeState: string(record.MergeStateBlocked)}})
	kept := keptStatuses(pr, headOne)
	wakeFact := fact
	wakeFact.Statuses = kept
	wake := SettlementWake("LEGION-208", phase.Testing, "sjawhar/legion", wakeFact)

	deref := func(n *int) int {
		if n == nil {
			return -1
		}
		return *n
	}
	return []checksTally{
		{"ci-settled (builder)", settled.NotPassed, settled.Skipped, settled.NotPassedStatuses, settled.Passed, settled.ListPassed, settled.Phase, true, settled},
		{"ci-settled (queued)", told.NotPassed, told.Skipped, told.NotPassedStatuses, told.Passed, told.ListPassed, told.Phase, true, told},
		{"review-stuck", stuck[0].NotPassed, stuck[0].Skipped, stuck[0].NotPassedStatuses, deref(stuck[0].Passed), stuck[0].ListPassed, stuck[0].Phase, false, stuck[0]},
		{"approved-round-stuck", approved.NotPassed, approved.Skipped, approved.NotPassedStatuses, approved.Passed, approved.ListPassed, "", false, approved},
		{"phase-assignment", assignment.NotPassed, assignment.Skipped, assignment.NotPassedStatuses, deref(assignment.Passed), assignment.ListPassed, assignment.Phase, false, assignment},
		{"pod-wake", wake.NotPassed, wake.Skipped, wake.NotPassedStatuses, deref(wake.Passed), wake.ListPassed, wake.Phase, true, wake},
	}
}

// dispatch://INFRA-3686 R3 with dispatch://INFRA-4135 R1 (the check c-daemon-texts-are-facts):
// each text of a head of 60 checks, 25 failed or cancelled or concluded another word that is no
// pass and 5 skipped, names the 25 with GitHub's word, the 5 skipped as skipped, counts the 30
// that passed with the 27 statuses that passed, names gh pr checks with the pull request and its
// repository, and the settlement and the wake name the phase.
func TestEveryTextOfASixtyCheckHeadNamesItsSkippedChecksSkipped(t *testing.T) {
	_, skipped, conclusion := sixtyChecks()
	for _, got := range sixtyCheckTexts(t) {
		if !slices.Equal(got.skipped, skipped) {
			t.Errorf("%s names %v skipped, want %v", got.text, got.skipped, skipped)
		}
		if len(got.notPassed) != 25 {
			t.Errorf("%s names %d checks that did not pass, want 25: %v", got.text, len(got.notPassed), got.notPassed)
		}
		for _, check := range got.notPassed {
			if want := conclusion[check.Check]; check.Conclusion != want {
				t.Errorf("%s names %s %q, want %q", got.text, check.Check, check.Conclusion, want)
			}
		}
		if len(got.statuses) != 3 {
			t.Errorf("%s names %d statuses that did not pass, want 3", got.text, len(got.statuses))
		}
		if got.passed != 57 {
			t.Errorf("%s counts %d that passed, want the 30 checks and the 27 statuses, 57", got.text, got.passed)
		}
		if got.listPassed != "gh pr checks 42 --repo sjawhar/legion" {
			t.Errorf("%s lists them with %q", got.text, got.listPassed)
		}
		if got.wantPhase && got.phase == "" {
			t.Errorf("%s names no phase", got.text)
		}
	}
}

// R8 of dispatch://INFRA-3686: "Where an approved requirement of another issue fixes the words of
// a text, a fact or a step it gives ... that requirement holds ... a fact or a step that it has
// the text give stays as a field". dispatch://INFRA-4135's R1 (done, held by main; its spec is
// covered by the approval of dispatch://INFRA-3118's spec, whose R1 is "One approval covers the
// class"): the "CI settled" line and the line of the notice for a head GitHub will not merge "say
// once, where any check is `skipped`, that a skipped check ran nothing on that head", and the task
// that wakes a released worker on a settlement "names each skipped check `skipped` too, says once
// that a skipped check ran nothing". Main's #4541 puts that fact in each of those texts
// (workflow/review.go's skippedRanNothing, "a skipped check ran nothing on this head"). Here each
// text of a head with a skipped check carries it as a field.
func TestEachTextOfAHeadWithASkippedCheckSaysOnceThatItRanNothing(t *testing.T) {
	for _, got := range sixtyCheckTexts(t) {
		if got.text == "phase-assignment" {
			continue // INFRA-4135 R1 names the CI settled line, the notice of the head, and the wake task
		}
		raw, err := json.Marshal(got.payload)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(raw))
		if !strings.Contains(lower, "ran nothing") && !strings.Contains(lower, "ran_nothing") {
			t.Errorf("%s names %v skipped and no field of it says that a skipped check ran nothing on that head: %.600s", got.text, got.skipped, raw)
		}
	}
}
