package workflow

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// rootResumeStepLines is every line of the three files the root architect's prompt is composed of
// (roles/architect-root.md, go/architect-root.md and go/architect-common.md), read from disk as
// texts_steps_test.go reads the prompts, each prefixed with its file.
func rootResumeStepLines(t *testing.T) []string {
	t.Helper()
	var lines []string
	for _, path := range []string{"roles/architect-root.md", "go/architect-root.md", "go/architect-common.md"} {
		body, err := os.ReadFile("../prompts/" + path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			lines = append(lines, path+": "+line)
		}
	}
	return lines
}

// resumeLineAdmits says whether a line of the root architect's prompt can apply to task: false
// where the line names a condition that the task's own fields rule out.
func resumeLineAdmits(line string, task texts.RootResumeRecord) bool {
	open := task.GateOpen != nil && *task.GateOpen
	armed := task.GateArmed != nil && *task.GateArmed
	withPerson := len(task.WithPerson) > 0 || task.ApprovalRequest > 0
	switch {
	case strings.Contains(line, "nothing waits on you") && len(task.WaitingOnYou) > 0,
		strings.Contains(line, "nothing is with the person") && withPerson,
		strings.Contains(line, "asks no hand-back") && len(task.WithPerson) == 0,
		strings.Contains(line, "no `gate_open`") && task.GateOpen != nil,
		(strings.Contains(line, "the gate is open") || strings.Contains(line, "`gate_open: true`")) && !open,
		strings.Contains(line, "`gate_armed: false`") && (task.GateArmed == nil || armed):
		return false
	}
	return true
}

// Main's resumeTask (workflow/awaiting_reconcile.go at d46ec4e0) ends every resume that its last
// switch sends to `default` with " Continue.": nothing still with the person, and the gate in
// question absent (`case gate == nil`), open, or closed under a design gate that is not armed. It
// writes " Continue." after " Waiting on you: %s; read each thread and reply." as well, since that
// sentence is written before the switch whatever the switch then says:
//
//	if len(yours) > 0 { " Waiting on you: %s; read each thread and reply." }
//	switch { …; case gateClosed && armed: " When the %s is ready, hand the request back …"; default: " Continue." }
//
// So the resume of a question asked before any step of the gate was registered, which a person
// replied to in its thread, read "A person answered your ask ask-q on LEGION-208, or replied in
// its thread; read it. Waiting on you: ask ask-q; read each thread and reply. Continue.", and the
// resume under a closed gate that is not armed read "… The design gate is closed at version 5, as
// no registration of that version has opened it: when the spec is ready, register it at its
// current version, which opens the gate with no approval. Continue."
//
// INFRA-3686's R2: "It gives no step that the reader's role prompt or a skill gives, and a step
// that a text gives today goes into the reader's prompt where the prompt does not give it." The
// root architect's prompt must therefore give the step to continue the tree's work for each of
// these resumes, with no condition main's " Continue." did not have.
func TestARootResumeThatMainEndedWithContinueIsToldToContinue(t *testing.T) {
	answered := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	words := &record.ActWords{Text: "Use the smaller cut.", By: "user:alice", At: answered}
	waiting := record.Awaiting{Ask: "ask-q", AnsweredAt: &answered, AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q"}
	replied := []record.RootAsk{{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: true, ByTree: true, Seq: 8,
		WaitingOn: "agent", PersonSeq: 8, PersonWords: words}}
	answeredAsk := []record.RootAsk{{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: false, ByTree: true, Seq: 8,
		WaitingOn: "agent", PersonAnswerSeq: 8, PersonAnswerWords: words}}
	closed := &record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 5}
	cases := []struct {
		name  string
		gate  *record.DesignGate
		asks  []record.RootAsk
		armed bool
		main  string
	}{
		{"no gate, an ask waiting on you", nil, replied, true,
			"A person answered your ask ask-q on LEGION-208, or replied in its thread; read it. Waiting on you: ask ask-q; read each thread and reply. Continue."},
		{"a closed gate that is not armed", closed, answeredAsk, false,
			"A person answered your ask ask-q on LEGION-208, or replied in its thread; read it. The design gate is closed at version 5, as no " +
				"registration of that version has opened it: when the spec is ready, register it at its current version, which opens the gate " +
				"with no approval. Continue."},
	}
	continues := regexp.MustCompile(`(?i)\bcontinue\b`)
	resumeContext := regexp.MustCompile("`gate_|`waiting_on_you`|with the person|waits on you")
	lines := rootResumeStepLines(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := RootResume("LEGION-208", waiting, record.DesignStepSpec, tc.gate, nil, tc.asks, tc.armed)
			if len(task.WithPerson) > 0 || task.ApprovalRequest > 0 {
				t.Fatalf("the resume lists something still with the person (%v, %d), so main's switch would not reach `default`",
					task.WithPerson, task.ApprovalRequest)
			}
			if tc.gate != nil && (task.GateOpen == nil || *task.GateOpen || task.GateArmed == nil || *task.GateArmed) {
				t.Fatalf("the resume does not name a closed gate that is not armed: gate_open %v, gate_armed %v", task.GateOpen, task.GateArmed)
			}
			var applies []string
			for _, line := range lines {
				if continues.MatchString(line) && resumeContext.MatchString(line) && resumeLineAdmits(line, task) {
					applies = append(applies, line)
				}
			}
			if len(applies) == 0 {
				t.Errorf("no line of the root architect's prompt gives the step to continue the tree's work for this resume, "+
					"where main's resumeTask ended %q:\n%s", tc.main, taskText(task))
			}
		})
	}
}

// Main's resumeTask (workflow/awaiting_reconcile.go at d46ec4e0) gives the step to register the
// document at its current version only where the gate in question is closed and not armed: its
// gate switch writes
//
//	case gateOpen && !armed: " The design gate is open at version %d: %s, so act on it as on design-approved."
//	…
//	case !armed: " The design gate is closed at version %d, as no registration of that version has opened it: when the %s is ready, register it at its current version, which opens the gate with no approval."
//
// so a resume whose gate is open under a design gate that is not armed (a person approved the
// version the root waited on, and the deployment's design gate was off by the resume) read "Your
// spec version 5 on LEGION-208 is registered, and the design gate is off, so it needs no approval.
// The design gate is open at version 5: this resume is your design-approved wake, so act on it as
// on design-approved. Continue.", and was never told to register the document again.
// INFRA-3686's R2: "a step that a text gives today goes into the reader's prompt where the prompt
// does not give it" — the step with main's condition, so a line of the root architect's prompt
// that gives that step for `gate_armed: false` must hold main's condition that the gate is closed.
func TestTheUnarmedGatesRegisterStepHoldsMainsClosedCondition(t *testing.T) {
	answered := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	five := 5
	open := &record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 5,
		ApprovedVersion: &five}
	waiting := record.Awaiting{GateVersion: 5, AnsweredAt: &answered, AnsweredBy: record.AnsweredByApproval, AnsweredDetail: "version 5"}
	task := RootResume("LEGION-208", waiting, record.DesignStepSpec, open, nil, nil, false)
	if task.GateOpen == nil || !*task.GateOpen || task.GateArmed == nil || *task.GateArmed {
		t.Fatalf("the resume does not name an open gate that is not armed: gate_open %v, gate_armed %v", task.GateOpen, task.GateArmed)
	}
	register := regexp.MustCompile(`register it at its current version`)
	closedCondition := regexp.MustCompile("closed|not open|`gate_open: false`")
	for _, line := range rootResumeStepLines(t) {
		if strings.Contains(line, "`gate_armed: false`") && register.MatchString(line) && !closedCondition.MatchString(line) {
			t.Errorf("a line gives the step to register the document at its current version to every resume whose gate is not armed, "+
				"this one among them, whose gate is open; main gave that step only where the gate was closed (\" The design gate is "+
				"closed at version %%d, as no registration of that version has opened it: …\"), and for an open gate said \" The design "+
				"gate is open at version %%d: this resume is your design-approved wake\":\n%s\nthe task:\n%s", line, taskText(task))
		}
	}
}

// Main's resumeTask (workflow/awaiting_reconcile.go at d46ec4e0) says of an approval that answered
// the wait whether a person approved, from armed alone, whatever gate the step in question holds:
//
//	case armed && gateOff: "The design gate on %s was off when your wait was answered, so no person approved your %s %s then; …"
//	case armed:            "Your %s %s on %s was approved."
//	case gateOpen:         "Your %s %s on %s is registered, and the design gate is off, so it needs no approval."
//	default:               "The design gate on %s is off, so no approval of your %s %s is asked for."
//
// A wait on one step's approval is answered as soon as that step's document is approved
// (answerOpenGates, waitGateOpen reads the waited document's gate alone), and where the deployment
// gates two steps and the root has not registered the second yet, the resume's step in question
// is that second step, which holds no gate row (gateInQuestion's nil). Main then said "Your spec
// version 3 on LEGION-208 was approved." where the design gate is armed, and "The design gate on
// LEGION-208 is off, so no approval of your spec version 3 is asked for." where it is not.
// go/architect-root.md tells the two apart by `gate_armed` alone ("`approved`: … where the design
// gate is armed (`gate_armed: true`), a person approved the document at `version`, … where it is
// not armed, the design gate is off, so no person approves it"). INFRA-3686's R2 has the text state
// "the state that the reader acts on that its event has"; this round's change leaves gate_armed
// out wherever the step has no gate row, so the two resumes must still differ.
func TestAnApprovalResumeOfAStepWithNoGateSaysWhetherAPersonApproved(t *testing.T) {
	answered := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	three := 3
	steps := []record.DesignStep{record.DesignStepDecisions, record.DesignStepSpec}
	gates := map[record.DesignStep]record.DesignGate{record.DesignStepDecisions: {Issue: "LEGION-208", Step: record.DesignStepDecisions,
		ArtifactID: "decisions-208", LatestVersion: 3, ApprovedVersion: &three}}
	step, gate := gateInQuestion(steps, gates)
	if gate != nil {
		t.Fatalf("gateInQuestion = %s, %+v; want the unregistered spec step with no gate row", step, *gate)
	}
	waiting := record.Awaiting{GateVersion: 3, AnsweredAt: &answered, AnsweredBy: record.AnsweredByApproval, AnsweredDetail: "version 3"}
	armed := taskText(RootResume("LEGION-208", waiting, step, gate, []string{"decisions-208"}, nil, true))
	off := taskText(RootResume("LEGION-208", waiting, step, gate, []string{"decisions-208"}, nil, false))
	if armed == off {
		t.Errorf("an approval resume of a step with no gate reads the same whether or not the design gate is armed, where main "+
			"said \"Your spec version 3 on LEGION-208 was approved.\" under an armed gate and \"The design gate on LEGION-208 is off, "+
			"so no approval of your spec version 3 is asked for.\" under one that is not, and the root architect's prompt tells "+
			"them apart by `gate_armed` alone:\n%s", armed)
	}
}
