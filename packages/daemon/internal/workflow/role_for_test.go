package workflow

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// roleForAnswers is the role that works each phase, written out here rather than read from
// phase.RoleFor, so a phase added to the daemon fails this test until somebody decides what role
// works it. The architect works two of them: admitted, where a root's own architect reads its
// spec and decomposes its tree, and accepting, where it signs the work off. No row of the table
// starts either (Table), and every site that starts, resumes or reports a phase's worker skips
// the architect, which internal/textcheck's TestEveryReaderOfRoleForLeavesTheArchitectUnchanged
// holds for every reader of RoleFor in the daemon.
var roleForAnswers = map[phase.Phase]claim.Role{
	phase.Admitted:       claim.RoleArchitect,
	phase.AwaitingPerson: "",
	phase.Planning:       claim.RolePlanner,
	phase.Implementing:   claim.RoleImplementer,
	phase.Testing:        claim.RoleTester,
	phase.Reviewing:      claim.RoleReviewer,
	phase.Step:           "",
	phase.Accepting:      claim.RoleArchitect,
	phase.Retro:          claim.RoleImplementer,
	phase.Merging:        claim.RoleMerger,
	phase.Done:           "",
	phase.Held:           "",
}

// R1: phase.RoleFor answers the architect for admitted, beside accepting, and this package's
// forwarder of the same name answers what it answers. The phases come from phase.All, so the test
// reads the whole phase set the daemon writes and not a list of its own.
func TestRoleForAnswersEveryPhaseOfPhaseAll(t *testing.T) {
	t.Parallel()
	for _, p := range phase.All {
		want, written := roleForAnswers[p]
		if !written {
			t.Errorf("phase %s is in phase.All and in no entry of this test's table: decide which role works it, and write it here", p)
			continue
		}
		if got := phase.RoleFor(p); got != want {
			t.Errorf("phase.RoleFor(%s) = %q, want %q", p, got, want)
		}
		if got := RoleFor(p); got != want {
			t.Errorf("workflow.RoleFor(%s) = %q, want the %q phase.RoleFor answers: the forwarder restates nothing", p, got, want)
		}
	}
	for p := range roleForAnswers {
		if !slices.Contains(phase.All, p) {
			t.Errorf("this test's table holds phase %s, which phase.All does not: remove it here", p)
		}
	}
}
