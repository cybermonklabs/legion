package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A hand-over that carries its child's run carries each pull request the record holds for the
// child (record.Store.TaskPullRequests), and not only the one of the task that merges:
// LEGION-209's task a is done, with its pull request #42 open on legion/LEGION-209, as each task's
// pull request stays open until the merge of the task that merges closes it; its task b, which
// needs a and merges, tests at its step 1, with #43 open on legion/LEGION-209.b. Each row holds a
// head run and the generation 1 of the CI settlement that stands. Re-admitted as a root of its own
// at generation 2, each row, a's and b's, holds no head run, as after a new generation's clear
// (record.ClearGeneration), and keeps its settlement's generation 1. That generation is the
// listener's and not the issue's, so the carry keeps it (admit.go readmit, and
// TestAnEarlierSettlementOfACarriedPullRequestsHeadChangesNothing).
//
// The fault it catches is a re-admission that carries one task's row and leaves the other with its
// head run, whose replay would put the old fence back. Its controls: the carry of
// MergePullRequest's row alone, so a's #42 keeps its run; and the carry of the issue's first row
// in task order (the read by issue alone this change deletes), so b's #43 keeps its run.
func TestAReadmittedOrphanCarriesEachTasksPullRequest(t *testing.T) {
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
		{Issue: child, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting},
		{Issue: child, Generation: 1, Task: "b", PlanVersion: 1, BlockText: "Test the widget.", Needs: []string{"a"}, Merges: true, State: record.TaskWaiting},
	}
	steps := []record.PlanStep{
		{Issue: child, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer"},
		{Issue: child, Generation: 1, Task: "b", Position: 1, Role: "tester", Type: "testing", Agent: "tester"},
	}
	run := &record.HeadRun{Anchor: record.Judgement{HeadSHA: "head-old"}}
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		for _, put := range []error{
			records.PutIssue(ctx, tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
			records.PutIssue(ctx, tx, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "B", LastDispatchSeq: 1}),
			records.PutGate(ctx, tx, record.DesignGate{Issue: root, ArtifactID: artifact, LatestVersion: 2, ApprovedVersion: new(2)}),
			records.PutPlan(ctx, tx, version, tasks, steps),
			records.PutHandover(ctx, tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
		// Each task takes its branch as its first start gives it one, in task id order: a the
		// child's own, b legion/LEGION-209.b. Then a is done and b runs at its tester's step 1,
		// whose start, and so whose join, ended before the hand-over (the wait for its join the
		// store's move writes is ended).
		first := record.TaskPlace{Kind: record.PlaceStep, Position: 1}
		for _, task := range []struct {
			id, branch string
			state      record.TaskState
		}{{"a", "legion/LEGION-209", record.TaskDone}, {"b", "legion/LEGION-209.b", record.TaskRunning}} {
			if got, err := records.AssignTaskBranch(ctx, tx, child, 1, task.id); err != nil || got != task.branch {
				t.Fatalf("give task %s its branch = %q, %v, want %q", task.id, got, err, task.branch)
			}
			if err := records.MoveTask(ctx, tx, child, 1, task.id, first, task.state); err != nil {
				t.Fatalf("move task %s to %s: %v", task.id, task.state, err)
			}
		}
		if err := records.SetStepWait(ctx, tx, child, 1, "b", record.StepWaitNone, nil); err != nil {
			t.Fatalf("end the wait of task b's join: %v", err)
		}
		for _, put := range []record.PullRequest{
			{Issue: child, Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-209", HeadSHA: "head-a", Generation: 1, Run: run, State: record.PullRequestOpen},
			{Issue: child, Repo: "acme/widgets", Number: 43, Branch: "legion/LEGION-209.b", HeadSHA: "head-b", Generation: 1, Run: run, State: record.PullRequestOpen},
		} {
			if err := records.PutPullRequest(ctx, tx, put); err != nil {
				t.Fatalf("seed #%d: %v", put.Number, err)
			}
		}
	})

	apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
	if got := issue(t, pool, child); got.Tree != child || got.Generation != 2 {
		t.Fatalf("handed-over child = %#v, want its own tree at generation 2: the fixture must be a carried hand-over", got)
	}

	inTx(t, pool, func(tx pgx.Tx) {
		prs, err := record.NewStore().TaskPullRequests(ctx, tx, child)
		if err != nil || len(prs) != 2 || prs[0].Task != "a" || prs[1].Task != "b" {
			t.Fatalf("the pull requests of %s after the hand-over = %+v, %v, want #42 of task a and #43 of task b", child, prs, err)
		}
		for _, pr := range prs {
			if pr.Generation != 1 || pr.Run != nil {
				t.Fatalf("task %s's #%d after the hand-over stands at generation %d with run %+v, want its settlement's generation 1 kept and no run", pr.Task, pr.Number, pr.Generation, pr.Run)
			}
		}
	})
}
