package specchange

import (
	"fmt"
	"strings"
)

// The judgments of a conclusion (Judge).
const (
	// JudgmentNone is a spec that holds no conclusion block.
	JudgmentNone = "none"
	// JudgmentIncomplete is a conclusion that breaks a rule of D6; its reasons name each break.
	JudgmentIncomplete = "incomplete"
	// JudgmentUncovered is a conclusion that keeps every rule while an outcome judged is
	// behaviour no approved statement covers, which only the person's approval of the whole spec
	// lets land (R26).
	JudgmentUncovered = "uncovered"
	// JudgmentComplete is a conclusion that keeps every rule and covers every outcome judged.
	JudgmentComplete = "complete"
)

// The codes of the reasons a judgment gives.
const (
	ReasonNoStatementOrKind       = "no_statement_or_kind"
	ReasonStatementAndNoStatement = "statement_and_no_statement"
	ReasonStatementNotApproved    = "statement_not_approved"
	ReasonNoEvidence              = "no_evidence"
	ReasonOmitsOutcome            = "omits_outcome"
	ReasonTwoConclusions          = "two_conclusions"
	ReasonUncoveredOutcome        = "uncovered_outcome"
)

// Reason is one reason of a judgment: its code, one sentence on it, and the outcome it names,
// for omits_outcome and uncovered_outcome alone.
type Reason struct {
	Code    string
	Detail  string
	Outcome string
}

// Judgment is a conclusion's judgment and each of its reasons, in the order Judge names them.
type Judgment struct {
	State   string
	Reasons []Reason
}

// Judge judges conclusion (nil for a spec that holds none) against final, the reviewer's final
// list at the change's head (nil where none is recorded; an empty list recorded is a list), and
// approved, which says whether the approved spec of the repository holds an id as a current
// statement (D6, D10 of the work-units plan).
//
// A conclusion is incomplete when it is one of two or more in its version, when it names neither
// a statement with a difference nor no_statement, when it names no_statement beside a statement or
// a difference, when it names a statement the approved spec does not hold, when it has no
// evidence, and for each outcome of the final list that no Outcome: line of it holds, by key
// (OutcomeKey): the final list is the reviewer's, which the author cannot write, so the author's
// conclusion must answer every outcome the reviewer found. Every reason is named, not the first
// alone.
//
// A conclusion that keeps those rules is uncovered when an outcome judged is neither required by
// an approved statement nor restored for the conclusion's own statement: behaviour no statement
// covers yet (R26). The outcomes judged are the final list's when one is given, since the
// reviewer's markers are the ones the author cannot choose, and else the conclusion's own. It is
// complete otherwise, so a no_statement conclusion with evidence and an empty final list is
// complete.
func Judge(conclusion *Conclusion, final []Outcome, approved func(id string) bool) Judgment {
	if conclusion == nil {
		return Judgment{State: JudgmentNone}
	}
	var reasons []Reason
	add := func(code, outcome, detail string, args ...any) {
		reasons = append(reasons, Reason{Code: code, Detail: fmt.Sprintf(detail, args...), Outcome: outcome})
	}
	if len(conclusion.Others) > 0 {
		add(ReasonTwoConclusions, "", "The spec holds more than one conclusion: %s and %s. Keep one conclusion.",
			conclusion.Block, strings.Join(conclusion.Others, ", "))
	}
	named := conclusion.Statement != "" || conclusion.Difference != ""
	switch {
	case conclusion.NoStatement && named:
		add(ReasonStatementAndNoStatement, "", "The conclusion sets no_statement and also names a statement or a difference. Remove one of them.")
	case conclusion.NoStatement:
	case !named:
		add(ReasonNoStatementOrKind, "", "The conclusion names no statement and no difference, and it does not set no_statement. Name both, or set no_statement.")
	case conclusion.Statement == "":
		add(ReasonNoStatementOrKind, "", "The conclusion names the difference %s and no statement. Name the approved statement.", conclusion.Difference)
	case conclusion.Difference == "":
		add(ReasonNoStatementOrKind, "", "The conclusion names the statement %s and no difference. Name the difference.", conclusion.Statement)
	}
	if conclusion.Statement != "" && !approved(conclusion.Statement) {
		add(ReasonStatementNotApproved, "", "The approved spec holds no current statement %s. Name an approved statement.", conclusion.Statement)
	}
	if len(conclusion.Evidence) == 0 {
		add(ReasonNoEvidence, "", "The conclusion has no Evidence: paragraph with text. Write the evidence.")
	}
	if final != nil {
		listed := make(map[string]bool, len(conclusion.Outcomes))
		for _, outcome := range conclusion.Outcomes {
			listed[OutcomeKey(outcome.Text)] = true
		}
		for _, outcome := range final {
			if !listed[OutcomeKey(outcome.Text)] {
				add(ReasonOmitsOutcome, outcome.Text, "The reviewer's final list holds the outcome %q, and no Outcome: line of the conclusion holds it. Add it to the conclusion.", outcome.Text)
			}
		}
	}
	if len(reasons) > 0 {
		return Judgment{State: JudgmentIncomplete, Reasons: reasons}
	}
	judged := conclusion.Outcomes
	if final != nil {
		judged = final
	}
	restores := conclusion.Statement != "" && !conclusion.NoStatement
	for _, outcome := range judged {
		switch {
		case outcome.Statement != "" && approved(outcome.Statement):
		case outcome.Restored && restores:
		default:
			add(ReasonUncoveredOutcome, outcome.Text, "No approved statement covers the outcome %q. The change waits for the person's approval of the whole spec.", outcome.Text)
		}
	}
	if len(reasons) > 0 {
		return Judgment{State: JudgmentUncovered, Reasons: reasons}
	}
	return Judgment{State: JudgmentComplete}
}
