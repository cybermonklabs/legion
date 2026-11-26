package store

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

func TestAReleaseCancelResumesOnlyTheClaimThisReleaseSuspendedAmongOneRole(t *testing.T) {
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
	if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, "legion-legion-legion-208-implementer", claim.RoleImplementer, boundary); err != nil {
		t.Fatal(err)
	}
	cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.Pool().Query(ctx, `select payload->>'claim' from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, rootIssue)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := "legion-legion-legion-208-implementer"
	if len(resumed) != 1 || resumed[0] != want || len(cancelled.Resumed) != 1 {
		t.Fatalf("the release cancel resumed tokens %v and roles %v, want only %s, not the earlier release's %s", resumed, cancelled.Resumed, want, other)
	}
}

func TestAReleaseRecordsEachSameRoleClaimAndKeepsOneRoleForItsPodRecord(t *testing.T) {
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := t.Context()
	openRunningTree(t, st)
	markRelease(t, st, first, boundary, last, 0)
	var tokens []string
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
			if err := st.ReleaseSuspendedClaim(ctx, "legion", rootIssue, token, claim.RoleImplementer, boundary); err != nil {
				t.Fatal(err)
			}
		}
		tokens = append(tokens, string(token))
	}
	var roles, recorded []string
	if err := st.Pool().QueryRow(ctx, `select suspended, suspended_claims from issue_releases where project = 'legion' and issue = $1`, rootIssue).Scan(&roles, &recorded); err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 || roles[0] != string(claim.RoleImplementer) || len(recorded) != 2 || recorded[0] != tokens[0] || recorded[1] != tokens[1] {
		t.Fatalf("the mark recorded roles %v and claims %v, want one implementer role and exactly %v", roles, recorded, tokens)
	}
	if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Pool().Query(ctx, `select payload->>'claim' from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, rootIssue)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 2 || resumed[0] != tokens[0] || resumed[1] != tokens[1] {
		t.Fatalf("the release resumed %v, want exactly %v", resumed, tokens)
	}
}
