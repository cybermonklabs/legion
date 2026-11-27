package api

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/plan"
)

// register_plan checks a plan its owner wrote before the fact lock, and its client waits as long
// as the daemon takes, so the check of what no release can carry out (plan.Validate) runs under the
// daemon's own bound (checkPlan, planCheckBound). A check that runs past its bound refuses the plan
// with this release's causes (plan.Supported) and, in Validate's place, one cause of the whole plan
// that names the bound; a check whose request ended answers the request's error and no refusal,
// which the route answers 503; and a check inside its bound answers both checks' causes, as the
// route did before it had a bound. A bound of 0 has passed before Validate first reads its context,
// so the first case needs no slow plan: Validate stops at any read once its context has ended
// (plan's TestAnEndedContextStopsTheCheckOfCycles). The fault is a check under the request's
// context alone, which runs as long as its plan costs; the control is checkPlan with its
// context.WithTimeout taken out, under which the first case answers Supported's cause alone, not
// cut.
func TestTheCheckOfAPlanStopsAtTheDaemonsBound(t *testing.T) {
	deployment := plan.Deployment{Roles: plan.BuiltinRoles()}
	owned := plan.Plan{Tasks: []plan.Task{{ID: "a", Owner: "architect"}}}
	notYet := plan.Refusal{Task: "a", Reason: "task a names owner. This release of Legion does not carry it out yet."}

	want := []plan.Refusal{notYet, {Reason: "Legion stopped checking the plan after 0s, the most time it gives the check of one plan, so this answer may not name every cause the plan has. " +
		"Give a plan with fewer tasks, or with fewer parts nested under each other and fewer needs among them."}}
	if refusals, cut, err := checkPlan(t.Context(), owned, deployment, 0); err != nil || !cut || !reflect.DeepEqual(refusals, want) {
		t.Fatalf("checkPlan(a bound that has passed) = %+v, cut %v, error %v; want %+v, cut, and no error", refusals, cut, err, want)
	}

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if refusals, cut, err := checkPlan(ended, owned, deployment, planCheckBound); !errors.Is(err, context.Canceled) || cut || refusals != nil {
		t.Fatalf("checkPlan(a request that ended) = %+v, cut %v, error %v; want no refusal, not cut, and %v", refusals, cut, err, context.Canceled)
	}

	unresolved := plan.Plan{Tasks: []plan.Task{{ID: "a", Owner: "architect", Unresolved: []string{"x"}}}}
	want = []plan.Refusal{notYet, {Task: "a", Reason: "task a needs x, which names no task of this plan."}}
	if refusals, cut, err := checkPlan(t.Context(), unresolved, deployment, planCheckBound); err != nil || cut || !reflect.DeepEqual(refusals, want) {
		t.Fatalf("checkPlan(a plan inside its bound) = %+v, cut %v, error %v; want %+v, not cut, and no error", refusals, cut, err, want)
	}
}
