package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A root that waited on a question before any step of its design gate was registered resumes with
// no gate row (gateInQuestion answers nil for the unregistered step). Main's resumeTask
// (workflow/awaiting_reconcile.go at d46ec4e0) writes no gate sentence for it (`case gate ==
// nil:`) and ends " Continue." where nothing is with the person: "A person answered your ask ask-q
// on LEGION-208, or replied in its thread; read it. Continue." INFRA-3686's R2: "a step that a
// text gives today goes into the reader's prompt where the prompt does not give it".
// go/architect-root.md says the task names the gate's state "wherever that step has a gate"; a
// task that says gate_open: false and gate_armed: true for a step with no gate is read by
// roles/architect-root.md as "the gate is armed and closed", whose step is to request approval
// again and register that version, in place of main's " Continue.".
func TestARootResumeOfAStepWithNoGateNamesNoGateState(t *testing.T) {
	answered := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	words := &record.ActWords{Text: "Use the smaller cut.", By: "user:alice", At: answered}
	asks := []record.RootAsk{{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: false, ByTree: true, Seq: 8,
		WaitingOn: "agent", PersonAnswerSeq: 8, PersonAnswerWords: words}}
	waiting := record.Awaiting{Ask: "ask-q", AnsweredAt: &answered, AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q"}
	text := resumeTask("LEGION-208", waiting, record.DesignStepSpec, nil, nil, asks, true)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "gate_open:") || strings.HasPrefix(line, "gate_armed:") {
			t.Errorf("the resume of a step with no gate names %q, a gate state go/architect-root.md gives only "+
				"\"wherever that step has a gate\"; main named no gate and said \" Continue.\":\n%s", line, text)
		}
	}
}
