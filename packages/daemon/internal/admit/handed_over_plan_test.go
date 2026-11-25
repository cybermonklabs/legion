package admit

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A hand-over that carries its child's run carries the child's plan with it (record.Store's
// CarryPlan with carried true): the child's new generation holds the plan of the old one, its task
// where it stood, at its state, its place and its counts, so the child goes on at the step it stood
// at, as its phase, its gate and its pull request do
// (TestAHandedOverChildCarriesItsPhaseGateAndPullRequest). The old generation's plan stays as it
// was.
func TestAHandedOverOrphanKeepsItsPlan(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child, artifact = "LEGION-208", "LEGION-209", "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26"
	until := fixedNow.Add(time.Hour)
	parent := root
	version := record.PlanVersion{Issue: child, Generation: 1, Version: 2, ArtifactID: "artifact-plan-209", Source: record.PlanSourceOwner,
		GivenBy: "legion-legion-legion-208-architect", GivenAt: fixedNow.Add(-time.Hour)}
	task := record.PlanTask{Issue: child, Generation: 1, Task: "a", PlanVersion: 2, BlockText: "Build the widget.", State: record.TaskRunning,
		Place:  record.TaskPlace{Kind: record.PlaceStep, Position: 2},
		Counts: record.ReturnCounts{Pass: 2, Rounds: 1, ConflictRounds: 1, BackwardMoves: map[string]int{"reviewing-implementing": 1}, CapTold: 1}}
	steps := record.NameAgents(nil, nil, nil, []record.PlanStep{
		{Issue: child, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
		{Issue: child, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: "review"},
	})
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		for _, put := range []error{
			records.PutIssue(ctx, tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
			records.PutIssue(ctx, tx, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Reviewing, Generation: 1, Status: "retro", Rank: "B", LastDispatchSeq: 1}),
			records.PutGate(ctx, tx, record.DesignGate{Issue: root, ArtifactID: artifact, LatestVersion: 2, ApprovedVersion: new(2)}),
			records.PutPullRequest(ctx, tx, record.PullRequest{Issue: child, Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-209", HeadSHA: "head-42", Generation: 1, State: record.PullRequestOpen}),
			records.PutHandover(ctx, tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
			records.PutPlan(ctx, tx, version, []record.PlanTask{task}, steps),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})

	apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
	if got := issue(t, pool, child); got.Tree != child || got.Generation != 2 || got.Phase != phase.Reviewing {
		t.Fatalf("handed-over child = %#v, want its own tree at generation 2, still reviewing: the fixture must be a carried hand-over", got)
	}

	carried := version
	carried.Generation = 2
	carriedTask := task
	carriedTask.Generation = 2
	want := &record.StoredPlan{Version: carried, Tasks: []record.PlanTask{carriedTask}, Steps: map[string][]record.PlanStep{}}
	for _, step := range steps {
		step.Generation = 2
		want.Steps[step.Task] = append(want.Steps[step.Task], step)
	}
	old := &record.StoredPlan{Version: version, Tasks: []record.PlanTask{task}, Steps: map[string][]record.PlanStep{"a": steps}}
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		got, err := records.Plan(ctx, tx, child, 2)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("the plan of the carried child's new generation = %#v, %v, want its task where it stood: %#v", got, err, want)
		}
		got, err = records.Plan(ctx, tx, child, 1)
		if err != nil || !reflect.DeepEqual(got, old) {
			t.Fatalf("the plan of the child's old generation = %#v, %v, want it as it was: %#v", got, err, old)
		}
	})
}
