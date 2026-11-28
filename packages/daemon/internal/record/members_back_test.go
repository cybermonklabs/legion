package record

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The selection of the members the first boot brings back (membersBack) names how each member's
// tree left from its root's records, in its order: a close at the merge, the operator's hold of the
// tree or suspension of a claim of the root, a failed claim of the root, the root's own status, and
// the return of a root that still owes its members. It takes a member held from a phase as standing
// in that phase, and it leaves out a member at admitted, one whose phase worker's own exit
// suspended it, and one under a root that runs and owes nothing. Each case is one root, with one
// member in implementing whose implementer the daemon suspended unless the case says otherwise.
func TestEachMemberBackNamesHowItsTreeLeft(t *testing.T) {
	const root, member = "CAPTURE-1", "CAPTURE-2"
	for _, tc := range []struct {
		name string
		// edit changes the world of the case from the root at backlog, out of Legion's work.
		edit func(w *membersBackWorld)
		want []MemberBack
	}{
		{name: "a root set to backlog", want: []MemberBack{{Key: member, Phase: phase.Implementing, Root: root, Left: "backlog"}}},
		{name: "a root closed at its merge", edit: func(w *membersBackWorld) {
			w.issues[0].status = "done"
			w.merged[root] = true
		}, want: []MemberBack{{Key: member, Phase: phase.Implementing, Root: root, Left: TreeLeftMerge}}},
		{name: "a root whose tree the operator holds", edit: func(w *membersBackWorld) { w.operator[root] = true },
			want: []MemberBack{{Key: member, Phase: phase.Implementing, Root: root, Left: TreeLeftOperator}}},
		{name: "a root whose architect the operator suspended", edit: func(w *membersBackWorld) {
			w.claims[roleOf{issue: root, role: claim.RoleArchitect}] = backClaim{state: supervise.StateSuspended, by: supervise.SuspendedByOperator}
		}, want: []MemberBack{{Key: member, Phase: phase.Implementing, Root: root, Left: TreeLeftOperator}}},
		{name: "a root whose architect failed", edit: func(w *membersBackWorld) {
			w.claims[roleOf{issue: root, role: claim.RoleArchitect}] = backClaim{state: supervise.StateFailed}
		}, want: []MemberBack{{Key: member, Phase: phase.Implementing, Root: root, Left: TreeLeftFailure}}},
		{name: "a root back in work that owes its members", edit: func(w *membersBackWorld) {
			w.issues[0].phase, w.issues[0].status, w.issues[0].membersOwed = phase.Admitted, "todo", true
		}, want: []MemberBack{{Key: member, Phase: phase.Implementing, Root: root, Left: TreeLeftReturned}}},
		{name: "a member held from testing", edit: func(w *membersBackWorld) {
			w.issues[1].phase, w.issues[1].heldFrom = phase.Held, phase.Testing
			w.claims[roleOf{issue: member, role: claim.RoleTester}] = backClaim{state: supervise.StateSuspended, by: supervise.SuspendedByDaemon}
		}, want: []MemberBack{{Key: member, Phase: phase.Held, Root: root, Left: "backlog"}}},
		{name: "a root that runs and owes nothing", edit: func(w *membersBackWorld) {
			w.issues[0].phase, w.issues[0].status = phase.Implementing, "in_progress"
		}},
		{name: "a member at admitted", edit: func(w *membersBackWorld) { w.issues[1].phase = phase.Admitted }},
		{name: "a member its worker's own exit suspended", edit: func(w *membersBackWorld) {
			w.claims[roleOf{issue: member, role: claim.RoleImplementer}] = backClaim{state: supervise.StateSuspended, by: supervise.SuspendedByExit}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := membersBackWorld{
				issues: []backIssue{
					{key: root, tree: root, status: "backlog", phase: phase.Done},
					{key: member, tree: root, status: "in_progress", phase: phase.Implementing},
				},
				claims: map[roleOf]backClaim{
					{issue: member, role: claim.RoleImplementer}: {state: supervise.StateSuspended, by: supervise.SuspendedByDaemon},
				},
				merged: map[string]bool{}, operator: map[string]bool{}, handedOver: map[string]bool{}, claimedBy: map[string]string{},
			}
			if tc.edit != nil {
				tc.edit(&world)
			}
			if got := membersBack(world); !slices.Equal(got, tc.want) {
				t.Fatalf("membersBack = %+v, want %+v", got, tc.want)
			}
		})
	}
}
