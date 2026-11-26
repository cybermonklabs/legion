package contracts

import (
	"fmt"
	"strings"
	"testing"
)

// CheckStepAnswers answers the first rule a step record's answers break, and the entry that
// breaks it, in the order both callers' refusals name them. More than StepAnswersMax entries comes
// first. Then, entry by entry, a model outside the form of an agent's text, fewer than 1 call, and
// a model an earlier entry named. A list with no entry breaks none.
func TestCheckStepAnswersAnswersTheFirstRuleTheAnswersBreak(t *testing.T) {
	type answer struct {
		model string
		calls int64
	}
	pair := func(a answer) (string, int64) { return a.model, a.calls }
	most := make([]answer, StepAnswersMax+1)
	for i := range most {
		most[i] = answer{fmt.Sprintf("acme/model-%02d", i), 1}
	}
	for _, tc := range []struct {
		name    string
		answers []answer
		fault   StepAnswersFault
		entry   int
	}{
		{"no list", nil, StepAnswersHold, -1},
		{"an empty list", []answer{}, StepAnswersHold, -1},
		{"the most entries", most[:StepAnswersMax], StepAnswersHold, -1},
		{"a model of 128 characters", []answer{{strings.Repeat("x", 128), 1}}, StepAnswersHold, -1},
		{"one entry more", most, StepAnswersTooMany, -1},
		{"one entry more, the first outside the form", append([]answer{{"a b", 1}}, most[1:]...), StepAnswersTooMany, -1},
		{"a model with a space", []answer{{"acme/model-a", 2}, {"a b", 1}}, StepAnswerModelForm, 1},
		{"an empty model", []answer{{"", 1}}, StepAnswerModelForm, 0},
		{"a model of 129 characters", []answer{{strings.Repeat("x", 129), 1}}, StepAnswerModelForm, 0},
		{"a count of 0", []answer{{"acme/model-a", 2}, {"acme/x", 0}}, StepAnswerNoCall, 1},
		{"a count below 0", []answer{{"acme/x", -1}}, StepAnswerNoCall, 0},
		{"a model twice", []answer{{"acme/x", 2}, {"acme/y", 1}, {"acme/x", 1}}, StepAnswerModelTwice, 2},
		{"a model outside the form with no call", []answer{{"a b", 0}}, StepAnswerModelForm, 0},
		{"a model twice, the second with no call", []answer{{"acme/x", 1}, {"acme/x", 0}}, StepAnswerNoCall, 1},
	} {
		if fault, entry := CheckStepAnswers(tc.answers, pair); fault != tc.fault || entry != tc.entry {
			t.Errorf("%s: CheckStepAnswers = %d at entry %d, want %d at entry %d", tc.name, fault, entry, tc.fault, tc.entry)
		}
	}
}
