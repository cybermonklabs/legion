package workflow

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A pull request opened on a branch is recorded for the task whose branch it is
// (record.StoredPlan.TaskOfBranch): each of two tasks that started on a branch of its own keeps its
// pull request beside the other's, the one task of a plan that started before tasks had branches
// of their own takes the pull request on the issue's branch, and an issue with no plan rows records
// its pull request under the task "". A reopen of a task's pull request is that task's again and
// leaves the other task's row as it was.
func TestAPullRequestOpenedIsRecordedForTheTaskWhoseBranchItIsOn(t *testing.T) {
	ctx := t.Context()
	pool := migratedPool(t)
	openTree(t, pool)
	parent := "LEGION-208"
	for _, key := range []string{"LEGION-209", "LEGION-210", "LEGION-211"} {
		seedIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: parent,
			Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	}
	records := record.NewStore()
	given := time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)
	// LEGION-209's plan holds tasks a and b, each started on a branch of its own; LEGION-210's holds
	// one task, x, whose row holds no branch, as a task the release before started holds none;
	// LEGION-211 has no plan rows.
	seedRecord(t, pool, func(tx pgx.Tx) error {
		for key, ids := range map[string][]string{"LEGION-209": {"a", "b"}, "LEGION-210": {"x"}} {
			version := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "artifact-" + key, Source: record.PlanSourceOwner,
				GivenBy: claim.Token("legion-legion-legion-208-architect"), GivenAt: given}
			var tasks []record.PlanTask
			for _, id := range ids {
				tasks = append(tasks, record.PlanTask{Issue: key, Generation: 1, Task: id, PlanVersion: 1, BlockText: "Do " + id + ".", State: record.TaskWaiting})
			}
			if err := records.PutPlan(ctx, tx, version, tasks, nil); err != nil {
				return err
			}
		}
		for _, task := range []string{"a", "b"} {
			if _, err := records.AssignTaskBranch(ctx, tx, "LEGION-209", 1, task); err != nil {
				return err
			}
		}
		return nil
	})
	engine := testEngine(config.DesignGateRootIssues, nil)
	open := func(id string, number int, branch string, at time.Time, reopened bool) {
		t.Helper()
		if _, err := intake.ApplyFact(ctx, pool, "github", id, intake.PullRequestOpened{Repo: "acme/widgets", Number: number, Branch: branch, HeadRepo: "acme/widgets",
			Base: "main", HeadSHA: "head-" + id, UpdatedAt: at, Reopened: reopened}, engine, admissionStub{}); err != nil {
			t.Fatalf("apply the pull request %s: %v", id, err)
		}
	}
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	open("a-opened", 41, "legion/LEGION-209", at, false)
	open("b-opened", 42, "legion/LEGION-209.b", at.Add(time.Minute), false)
	open("x-opened", 43, "legion/LEGION-210", at, false)
	open("alone-opened", 44, "legion/LEGION-211", at, false)
	open("a-reopened", 41, "legion/LEGION-209", at.Add(2*time.Minute), true)

	for _, want := range []struct {
		issue, task, head string
		number            int
	}{
		{"LEGION-209", "a", "head-a-reopened", 41}, {"LEGION-209", "b", "head-b-opened", 42},
		{"LEGION-210", "x", "head-x-opened", 43}, {"LEGION-211", "", "head-alone-opened", 44},
	} {
		var pr *record.PullRequest
		seedRecord(t, pool, func(tx pgx.Tx) error {
			var err error
			pr, err = records.PullRequestOfTask(ctx, tx, want.issue, want.task)
			return err
		})
		if pr == nil || pr.Number != want.number || pr.HeadSHA != want.head {
			t.Fatalf("the pull request of task %q of %s = %+v, want #%d at %s: a pull request is its branch's task's", want.task, want.issue, pr, want.number, want.head)
		}
	}
	var every []record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		every, err = records.TaskPullRequests(ctx, tx, "LEGION-209")
		return err
	})
	if len(every) != 2 {
		t.Fatalf("the pull requests of LEGION-209 = %+v, want a's and b's alone", every)
	}
}
