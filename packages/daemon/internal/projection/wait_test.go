package projection

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim whose worker's wait stands carries that wait on its view — the four facts the
// declaration's answer named — and no other claim carries one. Whether a wait stands is the
// caller's judgement over the record (workflow.StandingWaitsNow), handed in as the waits that
// stand: a claim whose wait the caller did not hand in, because its worker declared none or
// because its row has no end yet but no longer stands, carries no `wait`, and a projection handed
// no waits shows none at all.
func TestProjectShowsOnEachClaimTheWaitThatStands(t *testing.T) {
	const project, issue = "LEGION", "LEGION-208"
	store := projectionStore{
		issues: []record.Issue{{Key: issue, Project: project, Tree: issue, Phase: phase.Implementing, Status: "in_progress",
			Generation: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Rank: issue}},
		phases: map[string][]record.PhaseRow{issue: {
			{Issue: issue, Role: claim.RolePlanner, Claim: "legion-208-planner", Entry: 1},
			{Issue: issue, Role: claim.RoleImplementer, Claim: "legion-208-implementer", Entry: 3},
		}},
	}
	claims := []supervise.Claim{
		{Token: "legion-208-architect", Issue: issue, Role: claim.RoleArchitect, State: supervise.StateReady, Session: "ses_architect_208"},
		{Token: "legion-208-planner", Issue: issue, Role: claim.RolePlanner, State: supervise.StateIdle, Session: "ses_planner_208"},
		{Token: "legion-208-implementer", Issue: issue, Role: claim.RoleImplementer, State: supervise.StateWorking, Session: "ses_implementer_208"},
	}
	// The record's times come back in whatever zone the store read them in; the view is UTC.
	east := time.FixedZone("UTC+2", 2*60*60)
	wait := record.ClaimWait{ID: "wait-208", Project: "legion", Token: "legion-208-implementer", Issue: issue,
		Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing, PhaseEntry: 3, TakenEntry: 3,
		Session: "ses_implementer_208", WaitingOn: "the review of LEGION-207's pull request",
		WakeAt: time.Date(2026, 9, 23, 2, 0, 0, 0, east), DeclaredAt: time.Date(2026, 9, 22, 23, 50, 0, 123456000, time.UTC)}

	got, err := Project(context.Background(), nil, store, project, nil, claims, nil,
		map[claim.Token]record.ClaimWait{wait.Token: wait}, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	view := got.Issues[issue]
	want := &api.WaitView{ID: "wait-208", WaitingOn: "the review of LEGION-207's pull request",
		WakeAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC), DeclaredAt: time.Date(2026, 9, 22, 23, 50, 0, 123456000, time.UTC)}
	if implementer := view.Workers[claim.RoleImplementer].Claim.Wait; !reflect.DeepEqual(implementer, want) {
		t.Errorf("%s's implementer carries the wait %+v, want %+v", issue, implementer, want)
	}
	if planner := view.Workers[claim.RolePlanner].Claim.Wait; planner != nil {
		t.Errorf("%s's planner carries the wait %+v, want none: it declared none", issue, planner)
	}
	if view.Architect == nil || view.Architect.Wait != nil {
		t.Errorf("%s's architect = %+v, want its claim with no wait", issue, view.Architect)
	}

	none, err := Project(context.Background(), nil, store, project, nil, claims, nil, nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project with no wait standing: %v", err)
	}
	for role, worker := range none.Issues[issue].Workers {
		if worker.Claim.Wait != nil {
			t.Errorf("%s's %s carries the wait %+v with none handed in, want none", issue, role, worker.Claim.Wait)
		}
	}
}
