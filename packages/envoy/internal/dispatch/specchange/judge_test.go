package specchange

import (
	"reflect"
	"testing"
)

// approvedStatements stands for the approved spec of a repository that holds two current
// statements; every other id is no approved statement.
func approvedStatements(id string) bool {
	return id == "an-order-is-paid-once" || id == "a-bill-names-its-order"
}

// A conclusion is judged against the approved spec and the reviewer's final list (D6, D10): none
// without a conclusion; incomplete, with each reason, when it lacks a statement and a kind or
// no_statement, holds both, names a statement the approved spec does not hold, has no evidence,
// leaves out an outcome of the final list, or is one of two; uncovered when an outcome judged is
// neither required by an approved statement nor restored for the conclusion's statement; and
// complete otherwise. The outcomes judged are the final list's when there is one, else the
// conclusion's own.
func TestJudgeJudgesEachRuleOfAConclusion(t *testing.T) {
	t.Parallel()
	restore := func() *Conclusion {
		return &Conclusion{Block: "c-restore", Statement: "an-order-is-paid-once", Difference: "code_breaks",
			Evidence: []string{"The test fails before the fix."},
			Outcomes: []Outcome{{Text: "A second payment is refused again.", Restored: true}}}
	}
	operational := func() *Conclusion {
		return &Conclusion{Block: "c-deploy", NoStatement: true, Evidence: []string{"The deploy job named the wrong host."}}
	}
	codes := func(judgment Judgment) []string {
		var codes []string
		for _, reason := range judgment.Reasons {
			code := reason.Code
			if reason.Outcome != "" {
				code += " " + reason.Outcome
			}
			if reason.Detail == "" {
				t.Errorf("the reason %s carries no detail", reason.Code)
			}
			codes = append(codes, code)
		}
		return codes
	}
	for _, test := range []struct {
		name       string
		conclusion func() *Conclusion
		final      []Outcome
		state      string
		reasons    []string
	}{
		{"no conclusion", func() *Conclusion { return nil }, nil, JudgmentNone, nil},
		{"no evidence", func() *Conclusion { c := restore(); c.Evidence = nil; return c }, nil,
			JudgmentIncomplete, []string{ReasonNoEvidence}},
		{"a kind without a statement", func() *Conclusion { c := restore(); c.Statement = ""; return c }, nil,
			JudgmentIncomplete, []string{ReasonNoStatementOrKind}},
		{"a statement without a kind", func() *Conclusion { c := restore(); c.Difference = ""; return c }, nil,
			JudgmentIncomplete, []string{ReasonNoStatementOrKind}},
		{"neither a statement nor no_statement", func() *Conclusion { c := operational(); c.NoStatement = false; return c }, nil,
			JudgmentIncomplete, []string{ReasonNoStatementOrKind}},
		{"no_statement beside a statement", func() *Conclusion { c := restore(); c.NoStatement = true; return c }, nil,
			JudgmentIncomplete, []string{ReasonStatementAndNoStatement}},
		{"a statement the approved spec does not hold", func() *Conclusion { c := restore(); c.Statement = "a-refund-is-paid"; return c }, nil,
			JudgmentIncomplete, []string{ReasonStatementNotApproved}},
		{"two conclusions", func() *Conclusion { c := operational(); c.Others = []string{"c-second"}; return c }, []Outcome{},
			JudgmentIncomplete, []string{ReasonTwoConclusions}},
		{"no_statement with evidence and an empty final list", operational, []Outcome{}, JudgmentComplete, nil},
		{"no_statement with evidence and no final list", operational, nil, JudgmentComplete, nil},
		{"an outcome of the final list the conclusion leaves out", restore,
			[]Outcome{{Text: "a second payment is REFUSED again", Restored: true}, {Text: "The refusal names the order.", Statement: "a-bill-names-its-order"}},
			JudgmentIncomplete, []string{ReasonOmitsOutcome + " The refusal names the order."}},
		{"an outcome no approved statement covers", func() *Conclusion {
			c := restore()
			c.Outcomes = append(c.Outcomes, Outcome{Text: "A refund is paid back."})
			return c
		}, nil, JudgmentUncovered, []string{ReasonUncoveredOutcome + " A refund is paid back."}},
		{"an outcome required by a statement the approved spec does not hold", restore,
			[]Outcome{{Text: "A second payment is refused again.", Statement: "a-refund-is-paid"}},
			JudgmentUncovered, []string{ReasonUncoveredOutcome + " A second payment is refused again."}},
		{"a restored outcome of a conclusion that names no statement", func() *Conclusion {
			c := operational()
			c.Outcomes = []Outcome{{Text: "The host is right again.", Restored: true}}
			return c
		}, nil, JudgmentUncovered, []string{ReasonUncoveredOutcome + " The host is right again."}},
		{"an approved statement covers the final list", func() *Conclusion {
			c := restore()
			c.Outcomes = append(c.Outcomes, Outcome{Text: "The refusal names the order."})
			return c
		}, []Outcome{{Text: "A second payment is refused again.", Restored: true}, {Text: "The refusal names the order.", Statement: "a-bill-names-its-order"}},
			JudgmentComplete, nil},
		{"the final list is judged, not the conclusion's own outcomes", func() *Conclusion {
			c := restore()
			c.Outcomes = append(c.Outcomes, Outcome{Text: "Something the reviewer did not list."})
			return c
		}, []Outcome{{Text: "A second payment is refused again.", Restored: true}}, JudgmentComplete, nil},
	} {
		got := Judge(test.conclusion(), test.final, approvedStatements)
		if got.State != test.state || !reflect.DeepEqual(codes(got), test.reasons) {
			t.Errorf("%s: judged %s %v, want %s %v", test.name, got.State, codes(got), test.state, test.reasons)
		}
	}
}

// Every reason of an incomplete conclusion is named, not the first alone.
func TestJudgeNamesEveryReasonOfAnIncompleteConclusion(t *testing.T) {
	t.Parallel()
	conclusion := &Conclusion{Block: "c-many", Statement: "a-refund-is-paid", NoStatement: true, Others: []string{"c-other"}}
	got := Judge(conclusion, []Outcome{{Text: "A refund is paid back."}}, approvedStatements)
	var codes []string
	for _, reason := range got.Reasons {
		codes = append(codes, reason.Code)
	}
	want := []string{ReasonTwoConclusions, ReasonStatementAndNoStatement, ReasonStatementNotApproved, ReasonNoEvidence, ReasonOmitsOutcome}
	if got.State != JudgmentIncomplete || !reflect.DeepEqual(codes, want) {
		t.Errorf("judged %s %v, want %s %v", got.State, codes, JudgmentIncomplete, want)
	}
}
