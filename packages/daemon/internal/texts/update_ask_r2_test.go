package texts

import (
	"strings"
	"testing"
	"time"
)

// dispatch://INFRA-4203's R2 (done; its spec is covered by the person's approval of
// dispatch://INFRA-3118's spec, whose R1 is "One approval covers the class"): "The steps are in the
// update message (`supervise.UpdateTask`) and in the role prompts' paragraph on update rounds
// ... Each place that gives them gives them in this order. Step 1: push every commit to keep to
// the issue's branch. Step 2: let each subagent and background job finish ... Step 3: put on
// Dispatch a scratch file needed as evidence, the step of today that stays. Step 4: call
// `ready_for_update`. Step 5: end the turn. The steps also say four things. ... If background work
// ends after the answer, the role pushes what it made and calls `ready_for_update` again with the
// same round, before it ends that turn."
//
// R8 of dispatch://INFRA-3686: "a fact or a step that it has the text give stays as a field".
// The ask keeps steps 1, 2 and 4 as fields (push_to, background_work, answer). Here it keeps steps
// 3 and 5 and the answer again after late background work as well, each a field of its own.
func TestTheUpdateAskGivesEachStepOfINFRA4203R2(t *testing.T) {
	ask := NewUpdateAsk("LEGION-209", "round-2187820f-74bb-a446-9f66-3a37df0bfd29", "pod-b4442a92",
		[]string{"4c1b8e2a6f9d"}, []string{"account"}, time.Date(2026, 11, 13, 12, 30, 0, 0, time.UTC))
	text, err := Render(ask)
	if err != nil {
		t.Fatalf("render the update ask: %v", err)
	}
	lower := strings.ToLower(text)
	for _, step := range []struct {
		what  string
		words []string
	}{
		{"step 3, a scratch file needed as evidence put on Dispatch", []string{"evidence", "scratch"}},
		{"step 5, end the turn", []string{"end_turn", "end_the_turn", "end your turn", "turn_end"}},
		{"after the answer, push what late background work made and answer again with the same round", []string{"again", "answer_again", "re_answer"}},
	} {
		found := false
		for _, word := range step.words {
			found = found || strings.Contains(lower, word)
		}
		if !found {
			t.Errorf("the update ask gives no field for INFRA-4203 R2's %s; its text:\n%s", step.what, text)
		}
	}
}
