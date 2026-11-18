package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A reader in a transaction judges whether an architect holds its role by the claim the store
// records (ArchitectRuns), since it has no machine to ask: a claim that failed or retired, and an
// issue with no claim at all, hold no role; an idle release's suspension keeps the role, since the
// next wake resumes the same session with its phase and its task, while an operator's or the
// workflow's suspension does not; a claim that is coming up or at work holds it. The read covers
// the issues of the tree it is given and no other, so the architect of an issue outside it holds
// no role for this walk, whatever its claim says.
func TestAnArchitectHoldsItsRoleInATransactionByTheClaimTheStoreRecords(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	root := "LEGION-1"
	cases := []struct {
		name  string
		issue string
		state supervise.ClaimState
		by    supervise.SuspendSource
		holds bool
	}{
		{name: "a failed claim", issue: "LEGION-2", state: supervise.StateFailed},
		{name: "a retired claim", issue: "LEGION-3", state: supervise.StateRetired},
		{name: "no claim", issue: "LEGION-4"},
		{name: "a claim the idle release suspended", issue: "LEGION-5", state: supervise.StateSuspended, by: supervise.SuspendedByReleased, holds: true},
		{name: "a claim an operator suspended", issue: "LEGION-6", state: supervise.StateSuspended, by: supervise.SuspendedByOperator},
		{name: "a claim the workflow suspended", issue: "LEGION-7", state: supervise.StateSuspended, by: supervise.SuspendedByDaemon},
		{name: "a working claim", issue: "LEGION-8", state: supervise.StateWorking, holds: true},
		{name: "a ready claim", issue: "LEGION-9", state: supervise.StateReady, holds: true},
		{name: "a launching claim", issue: "LEGION-10", state: supervise.StateLaunching, holds: true},
	}
	tree := map[string]Issue{root: {Key: root, Tree: root}}
	for _, tc := range cases {
		tree[tc.issue] = Issue{Key: tc.issue, Tree: root, Parent: &root}
		if tc.state == "" {
			continue
		}
		must(t, st.PutClaim(ctx, supervise.Claim{Token: architectToken(t, tc.issue), Project: "legion", Tree: root,
			Issue: tc.issue, Role: claim.RoleArchitect, Generation: 1, State: tc.state, SuspendedBy: tc.by}))
	}
	outside := "LEGION-50"
	must(t, st.PutClaim(ctx, supervise.Claim{Token: architectToken(t, outside), Project: "legion", Tree: outside,
		Issue: outside, Role: claim.RoleArchitect, Generation: 1, State: supervise.StateWorking}))

	inTx(t, st, func(tx pgx.Tx) {
		runs, err := ArchitectRuns(ctx, records, tx, "legion", tree)
		if err != nil {
			t.Fatalf("read the architect claims of the tree: %v", err)
		}
		for _, tc := range cases {
			if got := runs(architectToken(t, tc.issue)); got != tc.holds {
				t.Errorf("%s (%s by %q) holds its role = %t, want %t", tc.name, tc.state, tc.by, got, tc.holds)
			}
		}
		if runs(architectToken(t, outside)) {
			t.Errorf("the working architect of %s, an issue outside the tree read, holds its role for the walk, want it not read", outside)
		}
	})
}

// An issue whose key makes no claim token has no architect and owns nothing (TreeByKey), so the
// owner walk that starts at it ends at the tree root, from the issue or from its parent: not at
// its own key, which names no claim, and not at the architect above it, though that one holds its
// role.
func TestAWalkFromAnIssueWhoseKeyMakesNoClaimTokenEndsAtTheTreeRoot(t *testing.T) {
	root, parent, keyless := "LEGION-1", "LEGION-2", "legion-3"
	start := Issue{Key: keyless, Tree: root, Parent: &parent}
	tree := TreeByKey([]Issue{{Key: root, Tree: root}, {Key: parent, Tree: root, Parent: &root}, start})
	runs := func(token claim.Token) bool {
		return token == architectToken(t, root) || token == architectToken(t, parent)
	}
	for _, fromParent := range []bool{false, true} {
		got, err := OwningArchitect("legion", tree, start, fromParent, runs)
		if want := architectToken(t, root); err != nil || got != want {
			t.Errorf("the walk from %s (from its parent: %t) ends at %s, %v; want the tree root's %s", keyless, fromParent, got, err, want)
		}
	}
}

// architectToken is the claim token of issue's architect in project legion.
func architectToken(t *testing.T, issue string) claim.Token {
	t.Helper()
	token, err := claim.NewToken("legion", issue, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("claim token of %s's architect: %v", issue, err)
	}
	return token
}
