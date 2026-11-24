package admit

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A hand-over that carries the run of an orphan whose plan holds several tasks carries it as it
// carries a run of one task: LEGION-209's task a stands at its review, step 2, and its task b at its
// implementer's step 1, both running, and task c, which needs both and merges, waits, when the
// close of its tree hands it to Legion. Re-admitted as a root of its own at generation 2, it keeps
// its plan, and the step each task stands at is started and recorded again, each on its own
// task's walk: a's reviewer is started in reviewing and b's implementer in implementing, each at
// generation 2 with its task, and a's step 2 and b's step 1 have runs open at generation 2
// (record.Store.StepRuns).
//
// The fault it catches is a carried orphan of several tasks whose re-admission fails as a whole,
// or starts one task of several, since the promotion read one walk of the issue, which a plan of
// several tasks does not give. Its controls: StartStepRecord on the issue's one walk, and the
// promotion's owners read on the issue rather than on each task's copy (workflow's PromotedView),
// each of which fails the hand-over's fact on that walk; the promotion's start of its first owner
// alone, which leaves b's implementer unstarted; and a promoted start of a task's copy that names
// no claim, which the other task's start then takes out (record.Store.EnqueueStart).
func TestACarriedOrphanOfSeveralTasksStartsEachTasksStep(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child, artifact = "LEGION-208", "LEGION-209", "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26"
	until := fixedNow.Add(time.Hour)
	parent := root
	version := record.PlanVersion{Issue: child, Generation: 1, Version: 1, ArtifactID: "artifact-plan-209", Source: record.PlanSourceOwner,
		GivenBy: "legion-legion-legion-208-architect", GivenAt: fixedNow.Add(-time.Hour)}
	tasks := []record.PlanTask{
		{Issue: child, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 2}},
		{Issue: child, Generation: 1, Task: "b", PlanVersion: 1, BlockText: "Build the gadget.", State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 1}},
		{Issue: child, Generation: 1, Task: "c", PlanVersion: 1, BlockText: "Test both.", Needs: []string{"a", "b"}, Merges: true, State: record.TaskWaiting},
	}
	steps := []record.PlanStep{
		{Issue: child, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer"},
		{Issue: child, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: "review", Agent: "reviewer"},
		{Issue: child, Generation: 1, Task: "b", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer-2"},
		{Issue: child, Generation: 1, Task: "c", Position: 1, Role: "tester", Type: "testing", Agent: "tester"},
	}
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		for _, put := range []error{
			records.PutIssue(ctx, tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
			records.PutIssue(ctx, tx, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1}),
			records.PutGate(ctx, tx, record.DesignGate{Issue: root, ArtifactID: artifact, LatestVersion: 2, ApprovedVersion: new(2)}),
			records.PutPlan(ctx, tx, version, tasks, steps),
			records.PutHandover(ctx, tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})

	apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
	if got := issue(t, pool, child); got.Tree != child || got.Generation != 2 {
		t.Fatalf("handed-over child = %#v, want its own tree at generation 2: the fixture must be a carried hand-over", got)
	}

	open := map[record.StepPlace]bool{}
	inTx(t, pool, func(tx pgx.Tx) {
		runs, err := record.NewStore().StepRuns(ctx, tx, []string{child})
		if err != nil {
			t.Fatalf("read the step runs of %s: %v", child, err)
		}
		for _, run := range runs {
			if run.Run == record.RunOpen && run.Generation == 2 {
				open[run.Place] = true
			}
		}
	})
	for _, want := range []record.StepPlace{{Task: "a", Kind: "step", Place: 2}, {Task: "b", Kind: "step", Place: 1}} {
		if !open[want] {
			t.Fatalf("the open step runs of %s at generation 2 = %v, want task %s's step %d among them", child, open, want.Task, want.Place)
		}
	}
	for _, want := range []struct {
		role claim.Role
		at   phase.Phase
	}{{claim.RoleReviewer, phase.Reviewing}, {claim.RoleImplementer, phase.Implementing}} {
		starts := roleStarts(t, pool, child, want.role)
		if !slices.ContainsFunc(starts, func(start record.SuperviseRequest) bool {
			return start.Phase == want.at && start.Generation == 2 && start.Task != ""
		}) {
			t.Fatalf("the starts of %s's %s = %+v, want one in %s at generation 2 that carries its task", child, want.role, starts, want.at)
		}
	}
}
