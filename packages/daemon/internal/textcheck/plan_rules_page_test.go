package textcheck

import (
	"strings"
	"testing"
)

// The concepts page states the rules of an owner's plan that plan.Supported holds a task to, and
// then says Legion refuses any other plan. This test holds it to the rule of the steps: a task
// names its roles in any order, any of them left out, and a role at two steps, since
// plan/validate.go refuses no task for the order of its steps or for a role it leaves out. It
// holds no words of the page's rule of which roles a task's agents start with. A page that states
// a rule the validator does not hold tells its reader a taken plan is refused. The page is read
// with its white space folded, so its line breaks decide nothing.
func TestTheConceptsPageStatesThatAPlansStepsRunInAnyOrder(t *testing.T) {
	const path = "docs/site/src/content/docs/legion/concepts.md"
	page := strings.Join(strings.Fields(shippedText(t, path)), " ")
	for _, rule := range []string{
		"in any order, any of them left out and a role at two steps.",
	} {
		if !strings.Contains(page, rule) {
			t.Errorf("%s omits %q", path, rule)
		}
	}
	for _, gone := range []string{
		"with `implementer` among them",
		"stands before the task's first `implementer` step",
	} {
		if strings.Contains(page, gone) {
			t.Errorf("%s still states %q, a rule plan/validate.go no longer holds", path, gone)
		}
	}
}
