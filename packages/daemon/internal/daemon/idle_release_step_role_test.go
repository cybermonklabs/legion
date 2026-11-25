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
// (checkRole, which idleReleaseRows and leftPodCloseRows both read). At step that worker is the
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
				decision.Roles = c.live
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
