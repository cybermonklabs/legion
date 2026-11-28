package api

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Two claims of one issue can hold one role: here the tester's own claim, which an operator
// suspended, and key a's claim, named implementer, which took the tester's role for its tester
// step and which the daemon's idle release suspended. Both are stranded, and each takes the move
// its own suspender calls for: the operator's suspension is a suspended worker's, and the
// release is a pod that never came back. The suspender is read by the claim's token, in the
// daemon's own boot (SuspendersOf) and in `legion state` (SuspendersOfOperatorClaims); a lookup
// by issue and role holds one suspender for the two and gives both claims one move.
func TestTwoStrandedClaimsOfOneRoleEachTakeTheirOwnMove(t *testing.T) {
	const issue = "LEGION-208"
	own := claim.Token("legion-legion-legion-208-tester")
	keyed := claim.Token("legion-legion-legion-208-implementer")
	stranded := []supervise.Claim{
		{Token: own, Issue: issue, Role: claim.RoleTester, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByOperator},
		{Token: keyed, Issue: issue, Role: claim.RoleTester, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased},
	}
	views := StrandedClaimsOf(stranded)
	if views[0].Token != own || views[1].Token != keyed {
		t.Fatalf("the state's stranded claims = %+v, want each named by its own token", views)
	}
	want := map[StrandedMove][]string{
		StrandedResume:           {},
		StrandedSuspendedWorkers: {"LEGION-208 tester"},
		StrandedReleasedNoResume: {"LEGION-208 tester (claim legion-legion-legion-208-implementer)"},
		StrandedRetry:            {},
		StrandedWorkflowStart:    {},
	}
	listed := make([]OperatorClaim, len(stranded))
	for i, c := range stranded {
		listed[i] = OperatorClaim{Token: c.Token, Issue: c.Issue, Role: c.Role, State: string(c.State), SuspendedBy: string(c.SuspendedBy)}
	}
	for _, tc := range []struct {
		reader    string
		suspended Suspenders
	}{
		{"the daemon's boot", SuspendersOf(stranded)},
		{"legion state", SuspendersOfOperatorClaims(listed)},
	} {
		t.Run(tc.reader, func(t *testing.T) {
			if got := views[0].Move(tc.suspended.Of(views[0])); got != StrandedSuspendedWorkers {
				t.Errorf("the move of the operator's suspended tester %s = %q, want %q", own, got, StrandedSuspendedWorkers)
			}
			if got := views[1].Move(tc.suspended.Of(views[1])); got != StrandedReleasedNoResume {
				t.Errorf("the move of the released keyed agent %s on the tester step = %q, want %q", keyed, got, StrandedReleasedNoResume)
			}
			if got := StrandedByMove(views, tc.suspended); !reflect.DeepEqual(got, want) {
				t.Errorf("the stranded claims by move = %v, want %v: each claim takes its own move", got, want)
			}
		})
	}
}

// Two claims of one role on one issue that take one move are named apart in the boot's line and
// in `legion state` (StrandedByMove): the role's own claim, whose token the issue and the role
// spell, by its issue and role, and the claim of an agent named for another role, which holds the
// role for its step, by its token beside them, which is what `legion claims` takes. A name by issue
// and role alone gives the two one name, and the operator cannot tell which to act on.
func TestTwoStrandedClaimsOfOneRoleInOneMoveAreNamedApart(t *testing.T) {
	const issue = "LEGION-208"
	stranded := []supervise.Claim{
		{Token: "legion-legion-legion-208-tester", Issue: issue, Role: claim.RoleTester, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByOperator},
		{Token: "legion-legion-legion-208-implementer", Issue: issue, Role: claim.RoleTester, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByOperator},
	}
	got := StrandedByMove(StrandedClaimsOf(stranded), SuspendersOf(stranded))[StrandedSuspendedWorkers]
	want := []string{"LEGION-208 tester", "LEGION-208 tester (claim legion-legion-legion-208-implementer)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the suspended workers = %q, want %q: two claims of one role, each named apart", got, want)
	}
}
