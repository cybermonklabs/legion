package store

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Two claims of one issue can hold one role (record.WorkerClaim). The release's mark records the
// token of each claim its stops suspended (issue_releases.suspended, 20261124213823), and the check
// that the pod went on
// working after the mark reads those tokens: a claim of the same role that this release did not
// suspend — here `…-implementer-2`, which the daemon suspended for a stop of its step — is no claim
// the release suspended and something resumed since, so it cancels nothing, at every idle release
// of the pod. A claim the release did suspend and that is no longer suspended as `released` still
// cancels it. The fault: a match by role read the second claim as the first one resumed, and
// cancelled the release at each idle period, so the pod was never released.
func TestAClaimOfTheSameRoleTheReleaseDidNotSuspendCancelsNothing(t *testing.T) {
	const second = claim.Token("legion-legion-legion-208-implementer-2")
	for _, tc := range []struct {
		name    string
		resumed bool
		want    ReleaseAct
		cause   ReleaseCause
	}{
		{name: "a second claim of the role the daemon suspended", want: ReleaseActs},
		{name: "control: the claim the release suspended, resumed since", resumed: true, want: ReleaseCancel, cause: ReleaseCauseOvertaken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			releasedClaim(t, st, claim.RoleImplementer)
			if err := st.PutClaim(ctx, supervise.Claim{Token: second, Project: "legion", Tree: rootIssue, Issue: rootIssue,
				Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, State: supervise.StateSuspended,
				SuspendedBy: supervise.SuspendedByDaemon}); err != nil {
				t.Fatalf("seed the second implementer: %v", err)
			}
			_, boundary, _ := releaseThroughTheStore(t, st, rootIssue)
			if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, "legion-legion-legion-208-implementer", boundary); err != nil {
				t.Fatalf("record the implementer's suspension: %v", err)
			}
			if tc.resumed {
				if _, err := st.Pool().Exec(ctx, `update claims set state = 'ready', suspended_by = '' where token = 'legion-legion-legion-208-implementer'`); err != nil {
					t.Fatalf("resume the released implementer: %v", err)
				}
			}

			stop, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
			if err != nil {
				t.Fatalf("IssueReleaseStop: %v", err)
			}
			if stop.Act != tc.want || stop.Cause != tc.cause {
				t.Fatalf("the role stop = %s for %s, want %s for %s", stop.Act, stop.Cause, tc.want, tc.cause)
			}
		})
	}
}
