package workflow

import (
	"strings"
	"testing"
)

// Main's checkTask (workflow/check.go at d46ec4e0) said on every look, the last or not: "a
// requirement observed as not met, or still unobserved when no look follows this one, whether or
// not production carries the merge, is a defect". INFRA-3686's R8 keeps dispatch://INFRA-4533's R1
// whole (the architect's ruling), whose first sentence is "Each text below states R7", and
// INFRA-4533's R3 has the test hold R7 "for the task of each look before it"; R7 of
// dispatch://INFRA-904 is "A requirement that only the deploy can show is part of the production
// check, and one not met there, or still not observed at the last look, fails that check." The
// branch's task of a look before the last says only not_met_requirement: fail.
func TestALookBeforeTheLastStatesTheLastLooksFailOfAnUnobservedRequirement(t *testing.T) {
	for _, tc := range checkTaskWakes {
		_, task := checkTaskFor(tc.cause, tc.detail, false)
		if !strings.Contains(task, "unobserved") {
			t.Errorf("the task of a look before the last, woken by %s, does not state that a requirement still unobserved at the last look is a fail (main's every-look clause \"or still unobserved when no look follows this one, whether or not production carries the merge, is a defect\"):\n%s", tc.name, task)
		}
	}
}
