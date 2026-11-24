package projection

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// A parked issue keeps its pull request in the served state: a person parked LEGION-208, a plan of
// one task a, at a's review, so its leave wrote a left at its second step, and a's pull request #41
// is open. The issue serves #41 as its own pull request, the merge's, as it did before tasks had
// merges of their own.
//
// The fault it catches is a parked issue served with no pull request, so the person who parked it
// cannot see the open pull request it left (its control: the tasks that have a merge holding a left
// task only where it stands at its merge, under which the issue serves none).
func TestAParkedIssueServesItsLeftTasksPullRequest(t *testing.T) {
	const key = "LEGION-208"
	ctx := context.Background()
	st := testpg.CloneStore(t, "legion_projection_test")
	records := record.NewStore()
	given := time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC)
	review := record.TaskPlace{Kind: record.PlaceStep, Position: 2}
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		must(t, records.PutIssue(ctx, tx, record.Issue{
			Key: key, Tree: key, Project: "LEGION", Title: "Park a review", Phase: phase.Done, Generation: 1,
			Status: "backlog", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		}))
		v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "artifact-plan-208", Source: record.PlanSourceOwner,
			GivenBy: "legion-legion-legion-208-architect", GivenAt: given}
		must(t, records.PutPlan(ctx, tx, v, []record.PlanTask{{Issue: key, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Do a.",
			State: record.TaskLeft, Place: review}}, []record.PlanStep{
			{Issue: key, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: plan.TypeImplementation, Agent: "implementer"},
			{Issue: key, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: plan.TypeReview, Agent: "reviewer"},
		}))
		branch, err := records.AssignTaskBranch(ctx, tx, key, 1, "a")
		must(t, err)
		return records.PutPullRequest(ctx, tx, record.PullRequest{Issue: key, Repo: "acme/widgets", Number: 41,
			Branch: branch, Base: "main", HeadSHA: "ha", State: record.PullRequestOpen, MergeState: record.MergeStateClean, Generation: 1})
	}); err != nil {
		t.Fatalf("write the parked %s: %v", key, err)
	}
	var state api.State
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		state, err = Project(ctx, Inputs{Tx: tx, Store: records, Project: "LEGION", Now: given.Add(time.Hour)})
		return err
	}); err != nil {
		t.Fatalf("Project: %v", err)
	}
	want := &api.PullRequestView{Repo: "acme/widgets", Number: 41, Head: "ha", State: "open", MergeState: "clean"}
	if got := state.Issues[key].PullRequest; !reflect.DeepEqual(got, want) {
		t.Fatalf("the parked %s serves the pull request %s, want %s", key, wireText(t, got), wireText(t, want))
	}
}
