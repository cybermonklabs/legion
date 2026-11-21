package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// R8 of dispatch://INFRA-3686: "Where an approved requirement of another issue fixes the words of
// a text, a fact or a step it gives ... that requirement holds ... a fact or a step that it has
// the text give stays as a field".
//
// dispatch://INFRA-4108's R3 (done, held by main since #4581; a child of dispatch://INFRA-3118,
// whose approved R1 is "One approval covers the class"): "Three texts say why the session was
// forgotten: the message to the issue, its architect and the controller; the fresh agent's own
// prompt; and the reason the claim keeps with the forgotten session. Each says that the pod's
// launcher could not look the session up and that Oh My Pi did not resume it by its id, and names
// the session."
//
// The notice to the architect and the controller is the session-forgotten record (workflow's
// SessionForgottenNotice). It names the session, the role and the phase; why the session was
// forgotten is only in the two prompts (architect-common.md, controller-root.md). Main's notice
// said it (workflow/engine.go's sessionLost at a4cd4949: "its pod's launcher is older than this
// release and could not look the session up by its id, and Oh My Pi did not resume it by that
// id"). Here the notice carries that fact as a field, as the CI texts carry INFRA-4135's
// (skipped_ran_nothing).
func TestASessionForgottenNoticeSaysWhyTheSessionWasForgotten(t *testing.T) {
	notice := SessionForgottenNotice("LEGION-208", claim.RoleTester, phase.Testing, "01a1ed54-c1b4-73b0-89f1-a9327401d421")
	raw, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(raw))
	for _, fact := range []struct{ what, words string }{
		{"the pod's launcher could not look the session up", "launcher"},
		{"Oh My Pi did not resume it by its id", "resume"},
	} {
		if !strings.Contains(lower, fact.words) {
			t.Errorf("the session-forgotten notice gives no field that says %s (INFRA-4108 R3): %s", fact.what, raw)
		}
	}
}

// R8 of dispatch://INFRA-3686, with dispatch://INFRA-4533's R1 (done, held by main since #4654; a
// child of dispatch://INFRA-904, whose spec Sami approved at version 15 and whose R7 it states):
// "Each text below states R7. ... One that is still not observed at the last look is a `fail`
// too, whether or not production carries the merge. The last look's `pending` is only for an
// issue that has no deploy-only requirement. These texts state it: (1) `checkTask`'s task, ...".
// The last look's task gives `last: true` and no field of the rule; the rule is in the checking
// role's prompt alone.
func TestTheLastLooksTaskStatesTheRuleOfItsVerdict(t *testing.T) {
	for _, tc := range checkTaskWakes {
		_, task := checkTaskFor(tc.cause, tc.detail, true)
		lower := strings.ToLower(task)
		if !strings.Contains(lower, "unobserved") && !strings.Contains(lower, "not_observed") {
			t.Errorf("the last look's task, woken by %s, states nothing of R7 (a deploy-only requirement still unobserved at the last look is a fail; pending only for an issue with none):\n%s", tc.name, task)
		}
	}
}
