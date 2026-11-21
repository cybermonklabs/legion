package record

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A reader in a transaction judges whether an architect holds its role by the claim the store
// records (ArchitectRuns), since it has no machine to ask: a claim that failed or retired, and an
// issue with no claim at all, hold no role; an idle release's suspension keeps the role, since the
// next wake resumes the same session with its phase and its task, while an operator's or the
// workflow's suspension does not; a claim that is coming up or at work holds it. The read covers
// the issues it is given and no other, so the architect of an issue outside them holds no role for
// this walk, whatever its claim says.
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
	keys := []string{root}
	for _, tc := range cases {
		keys = append(keys, tc.issue)
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
		runs, err := ArchitectRuns(ctx, records, tx, "legion", keys)
		if err != nil {
			t.Fatalf("read the architect claims of the issues: %v", err)
		}
		for _, tc := range cases {
			if got := runs(architectToken(t, tc.issue)); got != tc.holds {
				t.Errorf("%s (%s by %q) holds its role = %t, want %t", tc.name, tc.state, tc.by, got, tc.holds)
			}
		}
		if runs(architectToken(t, outside)) {
			t.Errorf("the working architect of %s, an issue outside the read, holds its role for the walk, want it not read", outside)
		}
	})
}

// An issue whose key makes no claim token has no architect and owns nothing, so the owner walk
// passes it, from the issue or from its parent: it reaches the nearest architect above it on the
// Dispatch chain whose claim holds its role, and, where none does, the issue's gate root.
func TestAWalkPassesAnIssueWhoseKeyMakesNoClaimToken(t *testing.T) {
	root := chainIssue("LEGION-1", "LEGION-1", "", phase.Implementing, "in_progress")
	parent := chainIssue("LEGION-2", "LEGION-1", "LEGION-1", phase.Implementing, "in_progress")
	keyless := chainIssue("legion-3", "LEGION-1", "LEGION-2", phase.Planning, "in_progress")
	issues := []Issue{root, parent, keyless}
	chains := chainsOf(issues, copyOf(issues...))
	for _, tc := range []struct {
		name    string
		holding []string
		owner   string
	}{
		{"the parent's architect holds its role", []string{root.Key, parent.Key}, parent.Key},
		{"no architect above it holds its role", nil, root.Key},
	} {
		runs := func(token claim.Token) bool {
			for _, key := range tc.holding {
				if token == architectToken(t, key) {
					return true
				}
			}
			return false
		}
		for _, fromParent := range []bool{false, true} {
			got, err := OwningArchitect("legion", chains, keyless.Key, fromParent, runs)
			if want := architectToken(t, tc.owner); err != nil || got != want {
				t.Errorf("%s: the walk from %s (from its parent: %t) ends at %s, %v; want %s", tc.name, keyless.Key, fromParent, got, err, want)
			}
		}
	}
}

// The owner walk reads the Dispatch chain the copy holds, never the stored tree (R18 of
// dispatch://INFRA-3320). An issue whose record still names the tree it left belongs to the
// architects of the chain it stands on now; a walk that reaches the end of what the copy shows of
// an unknown chain before an architect that holds its role has no owner yet (ErrChainUnknown), and
// an architect found below the gap owns the issue whatever stands above it.
func TestTheOwnerWalkReadsTheChainAndWaitsOnAnUnknownOne(t *testing.T) {
	stored := chainIssue("LEGION-10", "LEGION-10", "", phase.Implementing, "in_progress")
	now := chainIssue("LEGION-20", "LEGION-20", "", phase.Implementing, "in_progress")
	moved := chainIssue("LEGION-11", "LEGION-10", "LEGION-20", phase.Planning, "in_progress")
	gapped := chainIssue("LEGION-31", "LEGION-31", "LEGION-30", phase.Planning, "in_progress")
	under := chainIssue("LEGION-32", "LEGION-31", "LEGION-31", phase.Testing, "testing")
	issues := []Issue{stored, now, moved, gapped, under}
	// The copy holds no row of LEGION-30, LEGION-31's parent: its chain is unknown.
	copied := copyOf(stored, now, moved, gapped, under)
	chains := chainsOf(issues, copied)
	holding := map[claim.Token]bool{architectToken(t, stored.Key): true, architectToken(t, now.Key): true}
	runs := func(token claim.Token) bool { return holding[token] }

	if got := OwningIssue("legion", chains, moved.Key, false, runs); got != now.Key {
		t.Errorf("the owner of %s, stored in the tree of %s and under %s in Dispatch, = %q; want %s", moved.Key, stored.Key, now.Key, got, now.Key)
	}
	if _, err := OwningArchitect("legion", chains, under.Key, true, runs); !errors.Is(err, ErrChainUnknown) {
		t.Errorf("the walk from %s's parent on a chain the copy cannot show to its top = %v, want ErrChainUnknown", under.Key, err)
	}
	holding[architectToken(t, gapped.Key)] = true
	if got := OwningIssue("legion", chains, under.Key, true, runs); got != gapped.Key {
		t.Errorf("the walk from %s's parent, whose architect holds its role below the gap, = %q; want %s", under.Key, got, gapped.Key)
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
