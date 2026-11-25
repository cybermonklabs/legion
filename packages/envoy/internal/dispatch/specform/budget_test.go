package specform

import (
	"fmt"
	"strings"
	"testing"
)

// The judges of the ways out of the texts of one Check read at most maxWayBytes together: the
// findings of the first statements of a document of 600 statements, each with stops that no space
// follows, give their ways out, and those of its last statement say that Dispatch read its budget
// (documentNote), each still refused and named. Control: a document of 10 such statements reads
// within the budget, and no finding of it says so.
func TestTheWaysOutOfADocumentStopAtItsBudget(t *testing.T) {
	const body = "Dispatch.Keeps it.Each.Record ends.It.Stops here.Then.Again it.Runs on.And.On it.Goes back.Now."
	judged := func(count int) (first, last []Finding) {
		texts := make([]Text, count)
		for index := range texts {
			texts[index] = Text{Node: fmt.Sprintf("statement-%d", index+1), Kind: KindStatement, Body: body, Where: where}
		}
		for _, finding := range check(planted(), Input{Texts: texts}) {
			switch finding.Node {
			case texts[0].Node:
				first = append(first, finding)
			case texts[count-1].Node:
				last = append(last, finding)
			}
		}
		return first, last
	}
	noted := func(findings []Finding) (count int) {
		for _, finding := range findings {
			if strings.Contains(finding.Ways, documentNote) {
				count++
			}
		}
		return count
	}
	offered := func(findings []Finding) (count int) {
		for _, finding := range findings {
			if finding.Ways != "" && !strings.Contains(finding.Ways, documentNote) {
				count++
			}
		}
		return count
	}
	first, last := judged(600)
	if len(first) == 0 || len(last) != len(first) || offered(first) == 0 {
		t.Fatalf("the first statement gives %d findings, %d with ways out, and the last %d, want as many, and ways out", len(first), offered(first), len(last))
	}
	if noted(first) != 0 {
		t.Errorf("%d of the first statement's findings say that the budget is read, want none", noted(first))
	}
	if noted(last) != offered(first) || offered(last) != 0 {
		t.Errorf("%d of the last statement's findings say that the budget is read and %d give ways out, want %d and none", noted(last), offered(last), offered(first))
	}
	first, last = judged(10)
	if noted(first)+noted(last) != 0 || offered(last) != offered(first) {
		t.Errorf("the control, 10 statements: %d findings say that the budget is read, and the last statement gives %d ways out, want none and %d",
			noted(first)+noted(last), offered(last), offered(first))
	}
}
