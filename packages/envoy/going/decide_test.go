package going_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/going"
)

// The decision asks three choice questions: every action offered plus none, each carrying its
// description whole as what it is chosen on; the page, items named or none; and chart or prose. An
// action missing from the choices is one the model can never pick.
func TestTheDecisionOffersEveryActionAndNone(t *testing.T) {
	actions := []going.Action{
		{Name: "dispatch_issue", Description: "Create a native Dispatch issue."},
		{Name: "dispatch_claim", Description: "Claim a Dispatch issue, or release it."},
	}
	questions := going.DecisionQuestions(actions)
	if got := slices.Sorted(maps.Keys(questions)); !slices.Equal(got, []string{going.QuestionAction, going.QuestionContext, going.QuestionDisplay}) {
		t.Fatalf("the decision asks %v", got)
	}
	for name, want := range map[string][]string{
		going.QuestionAction:  {"dispatch_claim", "dispatch_issue", going.ActionNone},
		going.QuestionContext: {going.ContextNamed, going.ContextNone, going.ContextPage},
		going.QuestionDisplay: {going.DisplayChart, going.DisplayProse},
	} {
		question := questions[name]
		var criteria map[string]string
		if err := json.Unmarshal(question.Criteria, &criteria); err != nil {
			t.Fatalf("%s: the criteria are no object of strings: %v", name, err)
		}
		if question.Type != "choice" || !slices.Equal(slices.Sorted(maps.Keys(criteria)), want) {
			t.Errorf("%s is a %s question over %v, want a choice over %v", name, question.Type, slices.Sorted(maps.Keys(criteria)), want)
		}
		if name != going.QuestionAction {
			continue
		}
		for _, action := range actions {
			if criteria[action.Name] != action.Description {
				t.Errorf("%s is chosen on %q, want its own description %q", action.Name, criteria[action.Name], action.Description)
			}
		}
	}
}

// The state carries the words as typed and every part of the page they were typed on.
func TestTheDecisionStateCarriesTheWordsAndThePage(t *testing.T) {
	page := going.PageContext{Path: "/issues/DEMO-12/artifacts/spec-md", Project: "DEMO", Issue: "DEMO-12", Block: "b-4f1a"}
	var state struct {
		Words string            `json:"words"`
		Page  going.PageContext `json:"page"`
	}
	if err := json.Unmarshal(going.DecisionState(`close "this" one`, page), &state); err != nil {
		t.Fatal(err)
	}
	if state.Words != `close "this" one` || state.Page != page {
		t.Errorf("the state reads %+v, want the words and the page as given", state)
	}
}
