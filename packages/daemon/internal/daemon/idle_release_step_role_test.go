package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// An idle release, and the close of a pod whose issue left the workflow, read the workspace through
// the worker of the issue's open phase where that worker is live, and a decline tells that worker
// (checkClaim, which idleReleaseRows and leftPodCloseRows both read). At step that worker is the
// role of the running step (record.Issue.RoleAt), which the phase alone does not name:
// phase.RoleFor answers none for step. So an issue at step whose checker runs it has its workspace
// read through the checker, and the checker is the role told that the release waits for its push,
// not the first live role. The control is an issue at implementing, whose implementer the phase
// names.
func TestAnIdleReleaseAtAStepReadsThroughTheRoleOfTheStep(t *testing.T) {
	t.Parallel()
	const checker = claim.Role("checker")
	writers := []struct {
		name     string
		decision workflow.IdleDecision
		write    func(pods.IdleView, workflow.IdleDecision, time.Time) (store.IdleReleaseMark, error)
	}{
		{name: "the idle release", decision: workflow.IdleDecision{Release: true}, write: idleReleaseRows},
		{name: "the close of a left pod", decision: workflow.IdleDecision{Close: true}, write: leftPodCloseRows},
	}
	for _, w := range writers {
		for _, c := range []struct {
			name string
			view pods.IdleView
			live []claim.Role
			want claim.Role
		}{
			{name: "a step of the checker", view: pods.IdleView{Phase: phase.Step, StepRole: checker},
				live: []claim.Role{claim.RoleArchitect, checker}, want: checker},
			{name: "implementing (control)", view: pods.IdleView{Phase: phase.Implementing},
				live: []claim.Role{claim.RoleArchitect, claim.RoleImplementer}, want: claim.RoleImplementer},
		} {
			t.Run(w.name+"/"+c.name, func(t *testing.T) {
				c.view.Issue, c.view.Tree, c.view.Generation, c.view.Root.Generation = "LEGION-1", "LEGION-1", 2, 1
				decision := w.decision
				for _, role := range c.live {
					decision.Claims = append(decision.Claims, store.ReleasedClaim{Token: mustClaimToken(t, c.view.Issue, role), Role: role})
				}
				mark, err := w.write(c.view, decision, time.Now())
				if err != nil {
					t.Fatalf("%s: %v", w.name, err)
				}
				if len(mark.Rows) == 0 {
					t.Fatalf("%s wrote no rows", w.name)
				}
				var check struct {
					Role claim.Role `json:"role"`
				}
				if err := json.Unmarshal(mark.Rows[0].Payload, &check); err != nil {
					t.Fatalf("decode the workspace check: %v", err)
				}
				if check.Role != c.want {
					t.Errorf("the workspace check of %s reads through %q, want %q, the worker of the open phase", w.name, check.Role, c.want)
				}
				if !mark.Close && mark.Role != c.want {
					t.Errorf("the release's mark names %q, want %q, the worker of the open phase", mark.Role, c.want)
				}
			})
		}
	}
}

// A left pod can hold two live claims of one role. Its check reads through a named claim and
// its fresh close decision stops each claim once; deriving either stop from the role would
// reach one worker twice and leave the other holding the Sandbox suspension indefinitely.
func TestALeftPodCloseNamesEveryClaimOfOneRole(t *testing.T) {
	t.Parallel()
	view := pods.IdleView{Issue: "LEGION-1", Tree: "LEGION-1", Generation: 2, Phase: phase.Done}
	view.Root.Generation = 1
	claims := []store.ReleasedClaim{
		{Token: mustClaimToken(t, view.Issue, claim.RoleImplementer), Role: claim.RoleTester},
		{Token: mustClaimToken(t, view.Issue, claim.RoleTester), Role: claim.RoleTester},
	}
	decision := workflow.IdleDecision{Close: true, Claims: claims}
	now := time.Now().UTC()
	turn := now.Add(-time.Minute)
	mark, err := leftPodCloseRows(view, decision, now)
	if err != nil {
		t.Fatalf("encode the close's workspace check: %v", err)
	}
	if !mark.Close || len(mark.Rows) != 1 {
		t.Fatalf("the unchecked close = %+v, want its one workspace check alone", mark)
	}
	var check struct {
		Claim      claim.Token  `json:"claim"`
		CloseRoles []claim.Role `json:"closeRoles"`
	}
	if err := json.Unmarshal(mark.Rows[0].Payload, &check); err != nil {
		t.Fatalf("decode the close's workspace check: %v", err)
	}
	if check.Claim != claims[0].Token || len(check.CloseRoles) != 1 || check.CloseRoles[0] != claim.RoleTester {
		t.Fatalf("the close's check names %+v, want claim %s and one tester role", check, claims[0].Token)
	}
	mark, err = leftPodStopRows(view, decision, now, turn)
	if err != nil {
		t.Fatalf("encode the checked close: %v", err)
	}
	if !mark.Close || len(mark.Rows) != len(claims)+1 {
		t.Fatalf("the checked close = %+v, want one stop per claim and the Sandbox suspension", mark)
	}
	for i, c := range claims {
		var stop struct {
			Op        string      `json:"op"`
			Role      claim.Role  `json:"role"`
			Claim     claim.Token `json:"claim"`
			TurnStart *time.Time  `json:"turnStart"`
		}
		if err := json.Unmarshal(mark.Rows[i].Payload, &stop); err != nil {
			t.Fatalf("decode stop %d: %v", i, err)
		}
		if stop.Op != "suspend" || stop.Role != c.Role || stop.Claim != c.Token {
			t.Errorf("close stop %d = %+v, want one suspend naming claim %s of role %s", i, stop, c.Token, c.Role)
		}
		if stop.TurnStart == nil || !stop.TurnStart.Equal(turn) {
			t.Errorf("close stop %d lost its turn fence: %v, want %s", i, stop.TurnStart, turn)
		}
	}
	var suspension struct {
		TurnStart *time.Time `json:"turnStart"`
	}
	if err := json.Unmarshal(mark.Rows[len(claims)].Payload, &suspension); err != nil {
		t.Fatalf("decode the Sandbox suspension: %v", err)
	}
	if suspension.TurnStart == nil || !suspension.TurnStart.Equal(turn) {
		t.Fatalf("the Sandbox suspension lost its turn fence: %v, want %s", suspension.TurnStart, turn)
	}
}
