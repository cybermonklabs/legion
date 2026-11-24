package projection

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// The state serves each task of an issue's plan, in task id order, from the plan rows the store
// holds and the issue's running steps, each with where it stands, the act at its place, what the
// step there waits for, its own pull request and its own counts. The plan of LEGION-208 has eight
// tasks, each with the steps `implementer reviewer`:
//
//   - a is done at its second step, and ran at the release before tasks had branches of their
//     own, so its row holds no branch and it works on the issue's branch;
//   - b runs the reviewer's step, its agent r, on its own branch, with its own counts, and its
//     own pull request, whose review round waits on GitHub: the reviewer approved the head, which
//     GitHub holds `blocked`;
//   - c waits for a and b, and has not started;
//   - d stands at its first step, which waits for the end of its task's join;
//   - h is held at its first step, its owner having escalated the failed claim;
//   - m is the task that merges, which needs c, d, h, x and y, stopped at its first step with a
//     pull request of its own;
//   - x stands at its first step, which waits for its agent k;
//   - y runs its first step on the agent k.
//
// The fault it catches is a served state that names no task, or names each task's act, wait or
// pull request from the issue rather than from the task: its controls are the projection that
// serves no tasks, which fails the first check, and the issue's pull request served on each task,
// the merge's, under which b's own pull request is m's.
func TestTheStateServesEachTaskOfAnIssue(t *testing.T) {
	const key = "LEGION-208"
	ctx := context.Background()
	st := testpg.CloneStore(t, "legion_projection_test")
	records := record.NewStore()
	given := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	since := given.Add(time.Minute)

	type row struct {
		id     string
		state  record.TaskState
		place  record.TaskPlace
		needs  []string
		agents [2]claim.Agent
		counts record.ReturnCounts
	}
	step1, step2 := record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskPlace{Kind: record.PlaceStep, Position: 2}
	rows := []row{
		{id: "a", state: record.TaskDone, place: step2, agents: [2]claim.Agent{"implementer", "reviewer"}, counts: record.ReturnCounts{Pass: 1}},
		{id: "b", state: record.TaskRunning, place: step2, agents: [2]claim.Agent{"implementer-2", "r"}, counts: record.ReturnCounts{Pass: 3, Rounds: 2}},
		{id: "c", state: record.TaskWaiting, needs: []string{"a", "b"}, agents: [2]claim.Agent{"implementer-3", "reviewer-3"}},
		{id: "d", state: record.TaskRunning, place: step1, agents: [2]claim.Agent{"implementer-4", "reviewer-4"}},
		{id: "h", state: record.TaskRunning, place: step1, agents: [2]claim.Agent{"implementer-5", "reviewer-5"}},
		{id: "m", state: record.TaskRunning, place: step1, needs: []string{"c", "d", "h", "x", "y"}, agents: [2]claim.Agent{"implementer-6", "reviewer-6"}},
		{id: "x", state: record.TaskRunning, place: step1, agents: [2]claim.Agent{"k", "reviewer-7"}},
		{id: "y", state: record.TaskRunning, place: step1, agents: [2]claim.Agent{"k", "reviewer-8"}},
	}
	branches := map[string]string{}
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		must(t, records.PutIssue(ctx, tx, record.Issue{
			Key: key, Tree: key, Project: "LEGION", Title: "Serve each task", Phase: phase.Implementing, Generation: 1,
			Status: "in_progress", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		}))
		v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "artifact-plan-208", Source: record.PlanSourceOwner,
			GivenBy: "legion-legion-legion-208-architect", GivenAt: given}
		var tasks []record.PlanTask
		var steps []record.PlanStep
		for _, r := range rows {
			tasks = append(tasks, record.PlanTask{Issue: key, Generation: 1, Task: r.id, PlanVersion: 1, BlockText: "Do " + r.id + ".",
				Needs: r.needs, State: r.state, Place: r.place, Counts: r.counts})
			key0 := ""
			if r.agents[0] == "k" {
				key0 = "k"
			}
			steps = append(steps,
				record.PlanStep{Issue: key, Generation: 1, Task: r.id, Position: 1, Role: "implementer", Type: plan.TypeImplementation, AgentKey: key0, Agent: r.agents[0]},
				record.PlanStep{Issue: key, Generation: 1, Task: r.id, Position: 2, Role: "reviewer", Type: plan.TypeReview, Agent: r.agents[1]})
		}
		must(t, records.PutPlan(ctx, tx, v, tasks, steps))
		for _, id := range []string{"b", "d", "h", "m", "x", "y"} {
			branch, err := records.AssignTaskBranch(ctx, tx, key, 1, id)
			must(t, err)
			branches[id] = branch
		}
		must(t, records.HoldTask(ctx, tx, key, 1, "h", record.HoldEscalated))
		must(t, records.StopTask(ctx, tx, key, 1, "m", "its owner stopped it"))
		must(t, records.SetStepWait(ctx, tx, key, 1, "d", record.StepWaitJoin, &since))
		must(t, records.SetStepWait(ctx, tx, key, 1, "x", record.StepWaitAgent, &since))
		must(t, records.PutPullRequest(ctx, tx, record.PullRequest{Issue: key, Repo: "acme/widgets", Number: 42,
			Branch: branches["b"], Base: "main", HeadSHA: "hb", State: record.PullRequestOpen, MergeState: record.MergeStateBlocked, Generation: 1}))
		must(t, records.PutPullRequest(ctx, tx, record.PullRequest{Issue: key, Repo: "acme/widgets", Number: 40,
			Branch: branches["m"], Base: "main", HeadSHA: "hm", State: record.PullRequestOpen, MergeState: record.MergeStateClean, Generation: 1}))
		// The reviewer's row runs b's step, whose review approved the head b's pull request stands
		// at: the round waits on GitHub's merge state for it, which the record holds `blocked`.
		must(t, records.PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: claim.RoleReviewer, Claim: "legion-legion-legion-208-r",
			Decision: &record.ReviewDecision{State: "approved", Body: "ship it", Head: "hb"}, StepTask: "b", StepPlace: 2}))
		return nil
	}); err != nil {
		t.Fatalf("write the plan rows of %s: %v", key, err)
	}
	if branches["b"] != "legion/LEGION-208.b" {
		t.Fatalf("b's stored branch is %q, want legion/LEGION-208.b: the fixture proves nothing of the branch it serves", branches["b"])
	}

	var state api.State
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		state, err = Project(ctx, Inputs{Tx: tx, Store: records, Project: "LEGION", Now: given.Add(time.Hour)})
		return err
	}); err != nil {
		t.Fatalf("Project: %v", err)
	}
	view := state.Issues[key]

	none := []string{}
	want := []api.TaskView{
		{ID: "a", State: "done", Needs: none, Kind: "step", Phase: "done", Branch: "legion/LEGION-208", Position: 2,
			Counts: api.TaskCountsView{Pass: 1}},
		{ID: "b", State: "running", Needs: none, Kind: "step", Phase: "reviewing", Branch: branches["b"], Position: 2,
			Role: "reviewer", Agent: "r",
			PullRequest: &api.PullRequestView{Repo: "acme/widgets", Number: 42, Head: "hb", State: "open", MergeState: "blocked", ReviewDecision: "approved"},
			MergeWait:   &api.MergeWaitView{WaitsOn: "hb", MergeState: "blocked"},
			Counts:      api.TaskCountsView{Pass: 3, Rounds: 2}},
		{ID: "c", State: "waiting", Needs: []string{"a", "b"}},
		{ID: "d", State: "running", Needs: none, Kind: "step", Phase: "implementing", Branch: branches["d"], Position: 1,
			Role: "implementer", Agent: "implementer-4", Wait: "join"},
		{ID: "h", State: "held", Needs: none, Kind: "step", Phase: "implementing", Branch: branches["h"], Position: 1,
			Role: "implementer", Agent: "implementer-5", HoldReason: "escalated"},
		{ID: "m", State: "stopped", Needs: []string{"c", "d", "h", "x", "y"}, Kind: "step", Phase: "implementing", Branch: branches["m"], Position: 1,
			Role: "implementer", Agent: "implementer-6", StoppedReason: "its owner stopped it",
			PullRequest: &api.PullRequestView{Repo: "acme/widgets", Number: 40, Head: "hm", State: "open", MergeState: "clean"}},
		{ID: "x", State: "running", Needs: none, Kind: "step", Phase: "implementing", Branch: branches["x"], Position: 1,
			Role: "implementer", Agent: "k", Wait: "agent", WaitsFor: &api.AgentStepView{Agent: "k", Task: "y", Position: 1}},
		{ID: "y", State: "running", Needs: none, Kind: "step", Phase: "implementing", Branch: branches["y"], Position: 1,
			Role: "implementer", Agent: "k"},
	}
	if !reflect.DeepEqual(view.Tasks, want) {
		t.Fatalf("%s serves the tasks\n%s\nwant\n%s", key, wireText(t, view.Tasks), wireText(t, want))
	}
	// The issue's own pull request is the merge's, m's, and none of b's: b's review round is
	// served on b alone.
	merge := &api.PullRequestView{Repo: "acme/widgets", Number: 40, Head: "hm", State: "open", MergeState: "clean"}
	if !reflect.DeepEqual(view.PullRequest, merge) || view.MergeWait != nil {
		t.Fatalf("%s serves the pull request %s and the merge wait %s, want the merge's %s and none",
			key, wireText(t, view.PullRequest), wireText(t, view.MergeWait), wireText(t, merge))
	}
}

// wireText is value as the state's wire writes it, for a failure's message.
func wireText(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %#v: %v", value, err)
	}
	return string(encoded)
}

// must fails the test on err.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
