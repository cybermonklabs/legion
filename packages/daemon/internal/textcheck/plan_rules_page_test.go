package textcheck

import (
	"strings"
	"testing"
)

// The concepts page states the rules of an owner's plan that plan.Supported holds a task to, and
// then says Legion refuses any other plan. So it states each rule of the steps: each role named
// once, the implementer among them, before each step that reads its pull request, and after each
// other step of the type implementation, since Legion's retro is done by the implementer
// (plan/validate.go refuses such a step after the implementer, the implementer itself apart). A
// page that leaves a rule out tells its reader a refused plan is taken. The page is read with its
// white space folded, so its line breaks decide nothing.
func TestTheConceptsPageStatesEachRuleOfAPlansSteps(t *testing.T) {
	const path = "docs/site/src/content/docs/legion/concepts.md"
	page := strings.Join(strings.Fields(shippedText(t, path)), " ")
	for _, rule := range []string{
		"some of `planner implementer tester reviewer acceptance:owner` and of the roles the deployment defines, each named once, in the order the owner names them,",
		"with `implementer` among them and before each `tester`, `reviewer` and `acceptance:owner` step, since each of those reads the pull request the implementer opens,",
		"and after each other step of the type implementation, since Legion's retro is done by the implementer,",
	} {
		if !strings.Contains(page, rule) {
			t.Errorf("%s omits %q", path, rule)
		}
	}
}
