package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

func TestAStepWorkspaceDeclineReachesItsWorkerBesideAnOlderRoleClaim(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	worker := rig.implementerOfAnotherAgent(t, claim.Agent(claim.RoleTester), supervise.StateIdle, "").Claim().Token
	notices := &outboxPublisher{}
	rig.runner.notices = notices
	rig.rt.ScriptWorkspaceWork(rig.issue.Key, idleRigUnpushed)
	marked, err := rig.st.ReleaseIdleIssue(ctx, "legion", rig.issue.Key, func(view pods.IdleView) (store.IdleReleaseMark, error) {
		view.Phase, view.StepRole = phase.Step, claim.RoleImplementer
		view.Phases = map[claim.Role]pods.IdlePhase{claim.RoleImplementer: {Claim: worker, StepTask: "keyed-task"}}
		return idleReleaseRows(view, workflow.IdleDecision{Release: true, Claims: []store.ReleasedClaim{
			{Token: rig.token, Role: claim.RoleImplementer},
			{Token: worker, Role: claim.RoleImplementer},
		}}, time.Now().UTC())
	})
	if err != nil || !marked {
		t.Fatalf("mark the two-claim release = %t, %v", marked, err)
	}
	rig.drain(t)
	if got := roleMessages(notices, string(worker)); len(got) != 1 {
		t.Fatalf("workspace decline delivered %d messages to step worker %s, want one; all topics: %v", len(got), worker, notices.topics())
	}
	if got := roleMessages(notices, string(rig.token)); len(got) != 0 {
		t.Fatalf("workspace decline reached older role claim %s: %v", rig.token, got)
	}
}

func TestAWorkspaceCheckFallsBackOnlyWhenItsWorkerIsNotLive(t *testing.T) {
	const issue = "LEGION-208"
	older := mustClaimToken(t, issue, claim.RoleImplementer)
	worker := mustClaimToken(t, issue, claim.RoleTester)
	first := mustClaimToken(t, issue, claim.RoleArchitect)
	view := pods.IdleView{Project: "legion", Issue: issue, Tree: issue, Generation: 1,
		Phase: phase.Step, StepRole: claim.RoleImplementer,
		Phases: map[claim.Role]pods.IdlePhase{claim.RoleImplementer: {Claim: worker, StepTask: "keyed-task"}}}
	view.Root.Generation = 1
	for _, writer := range []struct {
		name  string
		write func(pods.IdleView, workflow.IdleDecision, time.Time) (store.IdleReleaseMark, error)
	}{
		{"release", idleReleaseRows},
		{"close", leftPodCloseRows},
	} {
		for _, test := range []struct {
			name string
			role claim.Role
		}{{"worker-live", claim.RoleImplementer}, {"worker-live-at-another-role", claim.RoleTester}, {"worker-not-live", ""}} {
			t.Run(writer.name+"/"+test.name, func(t *testing.T) {
				live := []store.ReleasedClaim{{Token: first, Role: claim.RoleArchitect}, {Token: older, Role: claim.RoleImplementer}}
				want := first
				if test.role != "" {
					live = append(live, store.ReleasedClaim{Token: worker, Role: test.role})
					want = worker
				}
				mark, err := writer.write(view, workflow.IdleDecision{Claims: live}, time.Now().UTC())
				if err != nil || len(mark.Rows) == 0 {
					t.Fatalf("workspace rows = %+v, %v", mark, err)
				}
				var check struct {
					Claim claim.Token `json:"claim"`
				}
				if err := json.Unmarshal(mark.Rows[0].Payload, &check); err != nil {
					t.Fatal(err)
				}
				if check.Claim != want {
					t.Fatalf("workspace check reaches %s, want %s", check.Claim, want)
				}
			})
		}
	}
}

func TestTheSameWorkspaceWorkIsNewsToAnotherClaimOfTheSameRole(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	worker := mustClaimToken(t, rig.issue.Key, claim.RoleTester)
	work := runtime.WorkspaceWork{Head: "unchanged-head", Count: 1}
	at := time.Now().UTC()
	for index, test := range []struct {
		token claim.Token
		want  bool
	}{
		{rig.token, true},
		{worker, true},
		{worker, false},
	} {
		decline, _, _, err := rig.runner.declineFor(ctx, rig.issue.Key, record.IssueWorkspaceCheck{Role: claim.RoleImplementer}, test.token,
			at.Add(time.Duration(index)*time.Minute), work, nil)
		if err != nil {
			t.Fatal(err)
		}
		told, err := rig.st.DeclineLeftPodClose(ctx, "legion", rig.issue.Key, decline)
		if err != nil || told != test.want {
			t.Fatalf("decline %d through %s told = %t, %v; want %t", index, test.token, told, err, test.want)
		}
	}
	rows, err := rig.pool.Query(ctx, `select payload->>'role' from outbox where issue = $1 and kind = 'role_message' order by id`, rig.issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var recipients []string
	for rows.Next() {
		var recipient string
		if err := rows.Scan(&recipient); err != nil {
			t.Fatal(err)
		}
		recipients = append(recipients, recipient)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(recipients) != 2 || recipients[0] != string(rig.token) || recipients[1] != string(worker) {
		t.Fatalf("decline recipients = %v, want [%s %s] once each", recipients, rig.token, worker)
	}
}
