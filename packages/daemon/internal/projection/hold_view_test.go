package projection

import (
	"context"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A merger the closed design gate holds carries that hold on its view — the gated step, the
// document and the version of it that is with the reviewer — and no other claim carries one.
// Whether the gate holds a merger is the caller's judgement over the record (workflow.HoldsNow,
// over workflow.HeldByDesignGate), handed in as the holds that stand: a claim the caller handed in
// no hold for carries no `hold`, and a projection handed no holds shows none at all.
func TestProjectShowsOnEachMergerTheHoldThatStands(t *testing.T) {
	const project, root, issue = "LEGION", "LEGION-208", "LEGION-215"
	store := projectionStore{
		issues: []record.Issue{
			{Key: root, Project: project, Tree: root, Phase: phase.Implementing, Status: "in_progress",
				Generation: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Rank: root},
			{Key: issue, Project: project, Tree: root, Parent: record.ParentOf(root), Phase: phase.Merging, Status: "retro",
				Generation: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Rank: issue},
		},
		phases: map[string][]record.PhaseRow{issue: {
			{Issue: issue, Role: claim.RoleImplementer, Claim: "legion-215-implementer", Entry: 1},
			{Issue: issue, Role: claim.RoleMerger, Claim: "legion-215-merger", Entry: 1},
		}},
	}
	claims := []supervise.Claim{
		{Token: "legion-215-implementer", Issue: issue, Tree: root, Role: claim.RoleImplementer, State: supervise.StateIdle, Session: "ses_implementer_215"},
		{Token: "legion-215-merger", Issue: issue, Tree: root, Role: claim.RoleMerger, State: supervise.StateIdle, Session: "ses_merger_215"},
	}
	approved := 6
	gate := record.DesignGate{Issue: root, Step: record.DesignStepSpec, ArtifactID: "spec-208", LatestVersion: 7, ApprovedVersion: &approved}

	got, err := Project(context.Background(), Inputs{Store: store, Project: project, Claims: claims,
		Holds: map[claim.Token]record.DesignGate{"legion-215-merger": gate}, Now: projectedAt})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	view := got.Issues[issue]
	want := &api.HoldView{Step: record.DesignStepSpec, ArtifactID: "spec-208", Version: 7}
	if merger := view.Workers[claim.RoleMerger].Claim.Hold; !reflect.DeepEqual(merger, want) {
		t.Errorf("%s's merger carries the hold %+v, want %+v", issue, merger, want)
	}
	if implementer := view.Workers[claim.RoleImplementer].Claim.Hold; implementer != nil {
		t.Errorf("%s's implementer carries the hold %+v, want none: no hold was handed in for it", issue, implementer)
	}

	none, err := Project(context.Background(), Inputs{Store: store, Project: project, Claims: claims, Now: projectedAt})
	if err != nil {
		t.Fatalf("Project with no hold standing: %v", err)
	}
	for role, worker := range none.Issues[issue].Workers {
		if worker.Claim.Hold != nil {
			t.Errorf("%s's %s carries the hold %+v with none handed in, want none", issue, role, worker.Claim.Hold)
		}
	}
}
