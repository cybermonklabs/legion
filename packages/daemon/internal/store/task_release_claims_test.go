package store

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// queuedStartClaims are the claims of the `start` rows queued for the root issue, in id order:
// the claims a cancel brought back.
func queuedStartClaims(t *testing.T, st *Store) []string {
	t.Helper()
	rows, err := st.Pool().Query(t.Context(), `select payload->>'claim' from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, rootIssue)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return resumed
}

// Two claims can hold one role: the role's own claim and a task agent's. A cancel resumes the
// claim this release's stop suspended, by its token, and not the other claim of its role, which an
// earlier release left `released`.
func TestAReleaseCancelResumesOnlyTheClaimThisReleaseSuspendedAmongOneRole(t *testing.T) {
	t.Parallel()
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := t.Context()
	openRunningTree(t, st)
	releasedClaim(t, st, claim.RoleImplementer)
	other, err := claim.AgentToken("legion", rootIssue, "implementer-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutClaim(ctx, supervise.Claim{Token: other, Project: "legion", Tree: rootIssue, Issue: rootIssue,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased}); err != nil {
		t.Fatal(err)
	}
	markRelease(t, st, first, boundary, last, 0)
	if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, "legion-legion-legion-208-implementer", boundary); err != nil {
		t.Fatal(err)
	}
	cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	resumed := queuedStartClaims(t, st)
	want := "legion-legion-legion-208-implementer"
	if len(resumed) != 1 || resumed[0] != want || !slices.Equal(cancelled.Resumed, []claim.Role{claim.RoleImplementer}) {
		t.Fatalf("the release cancel resumed tokens %v and roles %v, want only %s, not the earlier release's %s", resumed, cancelled.Resumed, want, other)
	}
}

// Two task agents' claims of one role, each recorded twice by its stop's row, are each recorded
// once on the mark, by its token, with the role it holds: the pod's release record names that role
// once (PodRelease.Roles). A cancel resumes both claims.
func TestAReleaseRecordsAndResumesEachTaskAgentClaimOfOneRole(t *testing.T) {
	t.Parallel()
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := t.Context()
	openRunningTree(t, st)
	markRelease(t, st, first, boundary, last, 0)
	var tokens []string
	var want []ReleasedClaim
	for _, agent := range []claim.Agent{"implementer-2", "implementer-3"} {
		token, err := claim.AgentToken("legion", rootIssue, agent)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: rootIssue, Issue: rootIssue,
			Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, token, boundary); err != nil {
				t.Fatal(err)
			}
		}
		tokens = append(tokens, string(token))
		want = append(want, ReleasedClaim{Token: token, Role: claim.RoleImplementer})
	}
	decision, err := st.IssueReleaseCheck(ctx, "legion", rootIssue, first)
	if err != nil {
		t.Fatalf("read the release's mark through its workspace check: %v", err)
	}
	if !slices.Equal(decision.Suspended, want) {
		t.Fatalf("the mark recorded claims %v, want exactly %v, each once and of the implementer's role", decision.Suspended, want)
	}
	if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if resumed := queuedStartClaims(t, st); !slices.Equal(resumed, tokens) {
		t.Fatalf("the release resumed %v, want exactly %v", resumed, tokens)
	}
}
