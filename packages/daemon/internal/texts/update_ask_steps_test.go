package texts

import (
	"strings"
	"testing"
	"time"
)

// The update round ask after cybermonklabs/legion#4556 (dispatch://INFRA-4203, done, its spec
// covered by the person's approval of dispatch://INFRA-3118, whose R1 says "One approval covers the
// class"). INFRA-4203's R2: "The steps are in the update message (`supervise.UpdateTask`) and in
// the role prompts' paragraph on update rounds", step 2 being "let each subagent and background job
// finish, or stop one that cannot end before the round ends, and commit and push what it made",
// and "The steps also say four things. The round does not end early while a role reports
// background work. ...". Its R3 names this record's field: "where dispatch://INFRA-3686's change
// has merged, the field of the update ask's record that says every answer brings the round's end
// forward (`every_answer_brings_it_forward`)" says the new rule, which its R1 gives: "every answer
// brings the moment forward only when no role reports background work".
//
// dispatch://INFRA-3686's R8: "a fact or a step that it has the text give stays as a field", and
// its decisions: "The update round ask keeps, where dispatch://INFRA-4203 lands, the step of its R2
// (let subagents and background jobs finish before `ready_for_update`)".
//
// So the ask, as its reader reads it (Render: the record's fields in the encoder's form), states the
// background-work step and the background-work hold on the round's early end as fields, beside
// goes_with_pod, which names only what the pod takes at the end.

func updateAskText(t *testing.T) []string {
	t.Helper()
	ask := NewUpdateAsk("LEGION-209", "round-2187820f-74bb-a446-9f66-3a37df0bfd29", "pod-b4442a92",
		[]string{"4c1b8e2a6f9d"}, []string{"account"}, time.Date(2026, 11, 13, 12, 30, 0, 0, time.UTC))
	text, err := Render(ask)
	if err != nil {
		t.Fatalf("render the update ask: %v", err)
	}
	return strings.Split(text, "\n")
}

// backgroundLines is each line of the ask, other than goes_with_pod's, that names a subagent or
// background work.
func backgroundLines(lines []string) []string {
	var found []string
	for _, line := range lines {
		field := strings.TrimSpace(line)
		if strings.HasPrefix(field, "goes_with_pod") {
			continue
		}
		if lower := strings.ToLower(field); strings.Contains(lower, "background") || strings.Contains(lower, "subagent") {
			found = append(found, field)
		}
	}
	return found
}

func TestTheUpdateAskKeepsTheStepOfItsBackgroundWork(t *testing.T) {
	lines := updateAskText(t)
	if len(backgroundLines(lines)) == 0 {
		t.Errorf("the update ask gives no field for INFRA-4203 R2's step 2 (let each subagent and background job finish, or stop it, and push what it made, before ready_for_update); its text:\n%s",
			strings.Join(lines, "\n"))
	}
}

func TestTheUpdateAskDoesNotSayEveryAnswerBringsTheEndForwardWhateverRuns(t *testing.T) {
	lines := updateAskText(t)
	for _, line := range lines {
		if strings.TrimSpace(line) == "every_answer_brings_it_forward: true" && len(backgroundLines(lines)) == 0 {
			t.Errorf("the update ask states %q and no field says that an answer brings the end forward only while no role reports background work (INFRA-4203 R1, R3); its text:\n%s",
				strings.TrimSpace(line), strings.Join(lines, "\n"))
		}
	}
}
