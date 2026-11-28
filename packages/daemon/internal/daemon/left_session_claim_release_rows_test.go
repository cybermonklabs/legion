package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The rows of the plan's decision table (dispatch://INFRA-3144/artifact/plan-md, "Decision table
// of the executor") that the row of a session the daemon left behind reaches and that leave the
// claim: the left session is the current session of another tree's claim or of the project
// controller's claim, the operator holds the issue's tree, Dispatch names no actor for the claim,
// Dispatch shows no claim, and the daemon records nothing for the issue. TestReleaseClaimRow's
// holders and record subtests run each of these for the move's row alone, so a left session's row
// that released the claim of a session another owner's claim runs now, or of a tree the operator
// holds, passed every other test of the change. Each row sends no DELETE, ends, and leaves the
// issue's record with the entry the table names.
func TestALeftSessionsReleaseLeavesEveryClaimItsTableRowsLeave(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		session       string
		shown         dispatch.Issue
		unrecorded    bool
		operatorHolds bool
		// earlier writes the entry an earlier release left, which the row clears.
		earlier bool
		left    record.ClaimLeftReason
		holder  string
		logged  string
	}{
		{name: "its session is the current session of another tree's claim", session: "ses-elsewhere",
			shown: dispatch.Issue{Claim: sessionClaim("ses-elsewhere")}, left: record.ClaimLeftOtherTree, holder: "session:ses-elsewhere"},
		{name: "its session is the project controller's current session", session: claimControllerSession,
			shown: dispatch.Issue{Claim: sessionClaim(claimControllerSession)}, left: record.ClaimLeftController,
			holder: "session:" + claimControllerSession},
		{name: "the operator holds the issue's tree", session: claimTreeLeftSession,
			shown: dispatch.Issue{Claim: sessionClaim(claimTreeLeftSession)}, operatorHolds: true, left: record.ClaimLeftOperatorTree,
			holder: "session:" + claimTreeLeftSession},
		{name: "Dispatch names no actor for the claim", session: claimTreeLeftSession, shown: dispatch.Issue{ClaimUnread: true},
			left: record.ClaimLeftUnread, logged: claimUnreadLine},
		{name: "Dispatch shows no claim, which clears the entry an earlier release wrote", session: claimTreeLeftSession,
			shown: dispatch.Issue{}, earlier: true},
		{name: "the daemon records nothing for the issue", session: claimTreeLeftSession,
			shown: dispatch.Issue{Claim: sessionClaim(claimTreeLeftSession)}, unrecorded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shown := tc.shown
			shown.Status = "backlog"
			rig := newClaimReleaseRig(t, shown, true, nil)
			if !tc.unrecorded {
				rig.records(outOfWorkflowIssue())
			}
			if tc.operatorHolds {
				openTreeUnderOperator(t, rig.pool)
			}
			if tc.earlier {
				earlier := record.ClaimLeft{Holder: "user:alice", Reason: record.ClaimLeftPerson, At: time.Now().Add(-time.Hour).UTC()}
				if err := store.Transact(context.Background(), rig.pool, func(ctx context.Context, tx pgx.Tx) error {
					return record.NewStore().RecordClaimLeft(ctx, tx, claimIssue, &earlier)
				}); err != nil {
					t.Fatalf("record the earlier entry of %s: %v", claimIssue, err)
				}
			}
			rig.dueRow(0, record.ClaimRelease{Session: tc.session})
			rig.run()

			if got := rig.client.releases(); len(got) != 0 {
				t.Fatalf("claim releases = %v, want none", got)
			}
			if left := rig.left(); left != 0 {
				t.Fatalf("rows left = %d, want the release row ended", left)
			}
			rig.wantClaimLeft(tc.left, tc.holder)
			executorLoggedOnce(t, rig.logs, tc.logged)
		})
	}
}
