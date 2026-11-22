package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// seedTwoTasks records CAPTURE-1 in implementing with a plan of two tasks, each started in id
// order, so a takes the issue's branch legion/CAPTURE-1 and b legion/CAPTURE-1.b
// (record.Store.AssignTaskBranch): a stands at its review with its pull request #86 open on its
// branch, and b at its implementer's step, which completed at the commit handoff-b, as the
// implementer's phases row names (record.PhaseRow.StepTask).
func seedTwoTasks(t *testing.T, pool *store.Pool) {
	t.Helper()
	ctx := context.Background()
	records := record.NewStore()
	v := record.PlanVersion{Issue: "CAPTURE-1", Generation: 1, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner, GivenAt: time.Now()}
	rows := []record.PlanTask{
		{Issue: "CAPTURE-1", Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Task a.", State: record.TaskWaiting},
		{Issue: "CAPTURE-1", Generation: 1, Task: "b", PlanVersion: 1, BlockText: "Task b.", State: record.TaskWaiting},
	}
	steps := []record.PlanStep{
		{Issue: "CAPTURE-1", Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer"},
		{Issue: "CAPTURE-1", Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: "review", Agent: "reviewer"},
		{Issue: "CAPTURE-1", Generation: 1, Task: "b", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer-2"},
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: "CAPTURE-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "CAPTURE-1",
			Project: "CAPTURE", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}); err != nil {
			return err
		}
		if err := records.PutPlan(ctx, tx, v, rows, steps); err != nil {
			return err
		}
		for _, task := range []struct {
			id string
			at int
		}{{"a", 2}, {"b", 1}} {
			if _, err := records.AssignTaskBranch(ctx, tx, "CAPTURE-1", 1, task.id); err != nil {
				return err
			}
			if err := records.MoveTask(ctx, tx, "CAPTURE-1", 1, task.id, record.TaskPlace{Kind: record.PlaceStep, Position: task.at}, record.TaskRunning); err != nil {
				return err
			}
		}
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-1", Repo: "acme/widgets", Number: 86,
			Branch: "legion/CAPTURE-1", HeadSHA: "code", Failing: []string{}}); err != nil {
			return err
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleImplementer, Claim: "implement-claim",
			HandoffCommit: "handoff-b", Summary: "implemented", StepTask: "b", StepPlace: 1})
	}); err != nil {
		t.Fatalf("seed CAPTURE-1 and its two tasks: %v", err)
	}
}

// A missing pull request is the completing implementer's task's own. CAPTURE-1's task a is at its
// review with its pull request #86 open on legion/CAPTURE-1; its task b's implementer completed at
// handoff-b, and GitHub lists no open pull request on b's branch legion/CAPTURE-1.b. The per-issue
// read lists b's branch alone and tells the architect pull-request-missing naming
// legion/CAPTURE-1.b and handoff-b. The record of each task's pull request is that task's own: a
// read of b's open #87 once it is recorded answers #87, and a read of a's #86 answers #86.
//
// The fault it catches is a's open pull request read as b's, so b stays in implementing and
// nobody is told (its control: the read on the issue's branch and the issue's first pull request,
// a's #86, so no notice is written).
func TestAMissingPullRequestIsOfTheCompletingTask(t *testing.T) {
	pool := isolatedOutboxPool(t)
	seedTwoTasks(t, pool)
	var listed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.URL.Path, "/repos/acme/widgets/pulls") && r.URL.Query().Get("state") == "open" {
			listed = append(listed, r.URL.Query().Get("head"))
			_, _ = w.Write([]byte("[]"))
			return
		}
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	w := readRuntime(pool, server.URL, quietLogger())

	_, err := w.readIssuePullRequest(context.Background(), "CAPTURE-1")
	reasons := noticeReasons(t, pool, "pull-request-missing")
	if len(reasons) != 1 || !strings.Contains(reasons[0], "legion/CAPTURE-1.b") || !strings.Contains(reasons[0], "handoff-b") {
		t.Fatalf("the read told pull-request-missing %q, want one naming b's branch legion/CAPTURE-1.b and its handoff commit", reasons)
	}
	if err != nil || len(listed) != 1 || listed[0] != "acme:legion/CAPTURE-1.b" {
		t.Fatalf("the read listed the open pull requests on %v, %v; want b's branch alone, no error", listed, err)
	}

	b := record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-1", Repo: "acme/widgets", Number: 87, Branch: "legion/CAPTURE-1.b", HeadSHA: "handoff-b", Failing: []string{}}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error { return record.NewStore().PutPullRequest(context.Background(), tx, b) }); err != nil {
		t.Fatalf("record b's pull request: %v", err)
	}
	for _, want := range []struct {
		task   string
		number int
	}{{"a", 86}, {"b", 87}} {
		got, err := w.openRecord(context.Background(), record.PullRequest{Issue: "CAPTURE-1", Task: want.task, Number: want.number})
		if err != nil || got.Number != want.number || got.Task != want.task {
			t.Fatalf("the open record of task %s's #%d = #%d of task %q, %v; want its own row", want.task, want.number, got.Number, got.Task, err)
		}
	}
}
