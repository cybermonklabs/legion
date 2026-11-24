package api

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A check run's status and conclusion print as GitHub wrote them where GitHub's documentation names
// them (ciStatuses, ciConclusions), and otherwise as a fixed phrase naming the field; the verdict is
// decided from the word as it came either way, so an undocumented conclusion is still a fail.
func TestACheckRunsDocumentedWordsPrintAsTheyCame(t *testing.T) {
	check := model.CheckBlock{BlockID: "c1", When: "before_merge", Text: "The contracts job passes at the head."}
	head := delivery.NewestHead{Repo: "acme/widgets", Number: 7, HeadSHA: strings.Repeat("ab", 20)}
	words := lineWordsFor(nil, "")
	for _, status := range []string{"queued", "in_progress", "waiting", "requested", "pending"} {
		ci, result := ciAnswerOf(check, "contracts", head, &ciRun{id: 1, name: "contracts", status: status}, words)
		if result != nil || ci.Case != ciCaseNotCompleted || !strings.Contains(ci.Detail, "CI run contracts is "+status+" at ") {
			t.Errorf("a run %s: case %q, detail %q, result %v; want not_completed naming the status as it came", status, ci.Case, ci.Detail, result)
		}
	}
	for conclusion, verdict := range map[string]string{
		"success": model.CIStatePass, "failure": model.CIStateFail, "timed_out": model.CIStateFail,
		"action_required": model.CIStateFail, "startup_failure": model.CIStateFail, "stale": model.CIStateFail,
		"neutral": model.CIStateNoResult, "skipped": model.CIStateNoResult, "cancelled": model.CIStateNoResult,
	} {
		ci, result := ciAnswerOf(check, "contracts", head, &ciRun{id: 2, name: "contracts", status: "completed", conclusion: conclusion}, words)
		if ci.State != verdict || ci.Case != conclusion || !strings.Contains(ci.Detail, " concluded "+conclusion+" at ") {
			t.Errorf("a run concluded %s: state %q, case %q, detail %q; want %s naming the conclusion as it came", conclusion, ci.State, ci.Case, ci.Detail, verdict)
		}
		if result != nil && !strings.HasPrefix(result.Observed, "conclusion: "+conclusion) {
			t.Errorf("a run concluded %s observed %q, want the conclusion as it came", conclusion, result.Observed)
		}
	}

	const conclusionPhrase = "(the conclusion is not one GitHub documents)"
	ci, result := ciAnswerOf(check, "contracts", head, &ciRun{id: 3, name: "contracts", status: "completed", conclusion: "superseded"}, words)
	if ci.State != model.CIStateFail || ci.Case != conclusionPhrase || !strings.Contains(ci.Detail, " concluded "+conclusionPhrase+" at ") ||
		result == nil || result.Verdict != model.CIStateFail || !strings.HasPrefix(result.Observed, "conclusion: "+conclusionPhrase) {
		t.Errorf("a run concluded superseded: state %q, case %q, detail %q, result %+v; want a fail naming the conclusion by its phrase", ci.State, ci.Case, ci.Detail, result)
	}
	ci, result = ciAnswerOf(check, "contracts", head, &ciRun{id: 4, name: "contracts", status: "paused"}, words)
	if result != nil || ci.Case != ciCaseNotCompleted || !strings.Contains(ci.Detail, "CI run contracts is (the status is not one GitHub documents) at ") {
		t.Errorf("a run paused: case %q, detail %q; want not_completed naming the status by its phrase", ci.Case, ci.Detail)
	}
}
