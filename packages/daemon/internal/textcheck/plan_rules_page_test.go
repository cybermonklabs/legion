package textcheck

import (
	"strings"
	"testing"
)

// The concepts page states the rules of an owner's plan that plan.Supported holds a task to, and
// then says Legion refuses any other plan. This test holds it to two of the rules of the steps: the
// implementer among them, before each step that reads its pull request, and after each other step
// of the type implementation, since Legion's retro is done by the implementer (plan/validate.go
// refuses such a step after the implementer, the implementer itself apart). It holds no words of
// the page's rule of which roles a task's agents start with. A page that leaves a rule out tells
// its reader a refused plan is taken. The page is read with its white space folded, so its line
// breaks decide nothing.
func TestTheConceptsPageStatesWhereTheImplementerStandsInAPlansSteps(t *testing.T) {
	const path = "docs/site/src/content/docs/legion/concepts.md"
	page := strings.Join(strings.Fields(shippedText(t, path)), " ")
	for _, rule := range []string{
		"with `implementer` among them and before each `tester`, `reviewer` and `acceptance:owner` step, since each of those reads the pull request the implementer opens,",
		"and after each other step of the type implementation, since Legion's retro is done by the implementer,",
	} {
		if !strings.Contains(page, rule) {
			t.Errorf("%s omits %q", path, rule)
		}
	}
}
