package workflow

import (
	"strings"
	"testing"
)

// R8 of dispatch://INFRA-3686 keeps dispatch://INFRA-4533's R1 whole (the architect's ruling):
// "Each text below states R7. A deploy-only requirement that a look observes as not met is a
// `fail`. One that is still not observed at the last look is a `fail` too, whether or not
// production carries the merge. The last look's `pending` is only for an issue that has no
// deploy-only requirement. These texts state it: (1) `checkTask`'s task, ...". R7 of
// dispatch://INFRA-904 (approved v85): "A requirement that only the deploy can show is part of the
// production check, and one not met there, or still not observed at the last look, fails that
// check."
//
// The last look's task gives the second and the third (unobserved_requirement: fail, pending:
// only_with_no_deploy_only_requirement). No look's task gives the first: a deploy-only
// requirement a look observes as not met is a fail, on every look, the last among them. Main's
// task said it on every look ("a requirement observed as not met ... is a defect"). Here every
// look's task states it.
func TestEveryLooksTaskSaysARequirementObservedAsNotMetFails(t *testing.T) {
	for _, tc := range checkTaskWakes {
		for _, last := range []bool{false, true} {
			_, task := checkTaskFor(tc.cause, tc.detail, last)
			lower := strings.ToLower(task)
			if !strings.Contains(lower, "not_met") && !strings.Contains(lower, "not met") {
				t.Errorf("the task of a look woken by %s (last %t) does not state that a deploy-only requirement the look observes as not met is a fail:\n%s",
					tc.name, last, task)
			}
		}
	}
}
