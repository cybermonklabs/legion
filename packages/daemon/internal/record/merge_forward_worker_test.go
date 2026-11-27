package record

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The hold and the landed of a merge forward go to the worker of the phase they name, the role
// phase.RoleFor gives it, where that role works the head: the planner, the tester, the reviewer
// and the merger. The workflow addresses them by that rule, and a row is held to the same rule
// where it is queued, so a hold or a landed naming one phase and going to the worker of another
// is refused, and the planner's is taken as the tester's is.
func TestAHoldOrALandedGoesToTheWorkerOfThePhaseItNames(t *testing.T) {
	now := time.Date(2026, 11, 23, 22, 0, 0, 0, time.UTC)
	message := func(role claim.Role, step MergeForwardStep, at phase.Phase) MergeForwardMessage {
		m := MergeForwardMessage{Role: role, Step: step, Head: "c0de", Base: "main", Phase: at, PullRequest: 42, Why: ConflictCheckNotGreen}
		if step == MergeForwardLanded {
			m.Landed = "d00d"
		}
		return m
	}
	for _, step := range []MergeForwardStep{MergeForwardHold, MergeForwardLanded} {
		for _, tc := range []struct {
			role  claim.Role
			at    phase.Phase
			taken bool
		}{
			{role: claim.RolePlanner, at: phase.Planning, taken: true},
			{role: claim.RoleTester, at: phase.Testing, taken: true},
			{role: claim.RoleReviewer, at: phase.Reviewing, taken: true},
			{role: claim.RoleMerger, at: phase.Merging, taken: true},
			{role: claim.RoleTester, at: phase.Reviewing},
			{role: claim.RoleReviewer, at: phase.Testing},
			{role: claim.RolePlanner, at: phase.Testing},
			{role: claim.RoleMerger, at: phase.Retro},
		} {
			t.Run(string(step)+" to the "+string(tc.role)+" in "+string(tc.at), func(t *testing.T) {
				_, err := NewOutboxRow("LEGION-208", message(tc.role, step, tc.at), now)
				if taken := err == nil; taken != tc.taken {
					t.Fatalf("NewOutboxRow of a %s to the %s naming %s: err = %v, want taken %t", step, tc.role, tc.at, err, tc.taken)
				}
			})
		}
	}
	// The architect works accepting, and no hold or landed reaches it: the ask's checks-red notice
	// tells it.
	for _, step := range []MergeForwardStep{MergeForwardHold, MergeForwardLanded} {
		if worker, told := MergeForwardWorker(step, phase.Accepting); told || worker != claim.RoleArchitect {
			t.Errorf("MergeForwardWorker(%s, accepting) = %q, %t; want the architect, told nothing", step, worker, told)
		}
	}
}

// The withdrawal of an ask tells the implementer that the merge forward it was asked for is no
// longer its work (workflow's mergeForward and withdrawUnowed). It goes to the implementer alone,
// names the head, the base, the phase the issue stands in, the pull request and why the act was
// taken, as the ask and the hold of the same act do, and names no head that landed.
func TestAWithdrawalGoesToTheImplementerAlone(t *testing.T) {
	now := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	withdrawn := MergeForwardWithdrawn
	for _, tc := range []struct {
		name    string
		payload MergeForwardMessage
		taken   bool
	}{
		{name: "to the implementer", payload: MergeForwardMessage{Role: claim.RoleImplementer, Step: withdrawn, Head: "c0de", Base: "main", Phase: phase.Merging, PullRequest: 42, Why: ConflictMergeNext}, taken: true},
		{name: "to the merger", payload: MergeForwardMessage{Role: claim.RoleMerger, Step: withdrawn, Head: "c0de", Base: "main", Phase: phase.Merging, PullRequest: 42, Why: ConflictMergeNext}},
		{name: "to the tester", payload: MergeForwardMessage{Role: claim.RoleTester, Step: withdrawn, Head: "c0de", Base: "main", Phase: phase.Testing, PullRequest: 42, Why: ConflictCheckNotGreen}},
		{name: "naming a head that landed", payload: MergeForwardMessage{Role: claim.RoleImplementer, Step: withdrawn, Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Merging, PullRequest: 42, Why: ConflictMergeNext}},
		{name: "with no why", payload: MergeForwardMessage{Role: claim.RoleImplementer, Step: withdrawn, Head: "c0de", Base: "main", Phase: phase.Merging, PullRequest: 42}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOutboxRow("LEGION-208", tc.payload, now)
			if taken := err == nil; taken != tc.taken {
				t.Fatalf("NewOutboxRow of %+v: err = %v, want taken %t", tc.payload, err, tc.taken)
			}
		})
	}
}
