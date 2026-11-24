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
	"github.com/sjawhar/legion/daemon/internal/testwait"
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
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
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
			// The move writes the wait of a start for its join; this seed stands for a task whose join ended.
			if err := records.SetStepWait(ctx, tx, "CAPTURE-1", 1, task.id, record.StepWaitNone, nil); err != nil {
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
	t.Parallel()
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

	_, err := w.readIssuePullRequest(context.Background(), "CAPTURE-1", "")
	reasons := noticeReasons(t, pool, "pull-request-missing")
	if len(reasons) != 1 || !strings.Contains(reasons[0], "legion/CAPTURE-1.b") || !strings.Contains(reasons[0], "handoff-b") {
		t.Fatalf("the read told pull-request-missing %q, want one naming b's branch legion/CAPTURE-1.b and its handoff commit", reasons)
	}
	if err != nil || len(listed) != 1 || listed[0] != "acme:legion/CAPTURE-1.b" {
		t.Fatalf("the read listed the open pull requests on %v, %v; want b's branch alone, no error", listed, err)
	}

	b := record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-1", Repo: "acme/widgets", Number: 87, Branch: "legion/CAPTURE-1.b", HeadSHA: "handoff-b", Failing: []string{}}
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error { return record.NewStore().PutPullRequest(ctx, tx, b) }); err != nil {
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

// The read a completion queues judges the pull request of the task whose step completed
// (handoffCompleted's task), not the task the implementer's phases row names. CAPTURE-1's task a
// stands at its review with its pull request #86 open on legion/CAPTURE-1, while task b stands at
// its implementer's step, whose phases row is the issue's newest implementer row. When a's
// reviewer completes, the read reads #86, and lists no branch for b's pull request.
//
// The fault it catches is a reviewer's completion of one task whose read judges another task's
// pull request, so the review's own pull request is not read when its next step is decided (its
// control: the completion queued without its task).
func TestTheReadAfterACompletionJudgesTheCompletingTasksPullRequest(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedTwoTasks(t, pool)
	github := newIssueReadStandIn(t, time.Date(2026, 11, 23, 1, 0, 0, 0, time.UTC))
	w := readRuntime(pool, github.URL, quietLogger())
	w.handedOff = newHandedOffSet()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		w.readHandedOff(ctx)
		close(stopped)
	}()

	w.handoffCompleted("CAPTURE-1", "a")
	testwait.Eventually(t, "the completion's read to ask GitHub for a pull request of CAPTURE-1", func() bool {
		return github.count("/repos/acme/widgets/pulls/86")+github.count("/repos/acme/widgets/pulls") > 0
	})
	cancel()
	<-stopped
	if read, listed := github.count("/repos/acme/widgets/pulls/86"), github.count("/repos/acme/widgets/pulls"); read != 1 || listed != 0 {
		t.Fatalf("the read after a's reviewer completed read #86 %d times and listed the open pull requests of a branch %d times, want #86 once and no list: GitHub was read %v",
			read, listed, github.counts())
	}
}

// The daemon's start reads the open pull request of each task of an issue (readIssuesAtStart), not
// one pull request of the issue: GitHub can lose the deliveries of either task's pull request while
// the daemon is down. CAPTURE-1's task a has its pull request #86 open on legion/CAPTURE-1, and its
// task b its #87 open on legion/CAPTURE-1.b, and the start reads each of them once.
//
// The fault it catches is a start read queued by the issue alone, which judges the task the
// implementer's phases row names, b, and leaves a's pull request as its lost webhooks left it (its
// control: the start queuing each issue's key alone).
func TestTheStartReadReadsTheOpenPullRequestOfEachTask(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedTwoTasks(t, pool)
	b := record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-1", Repo: "acme/widgets", Number: 87, Branch: "legion/CAPTURE-1.b", HeadSHA: "pushed", Failing: []string{}}
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error { return record.NewStore().PutPullRequest(ctx, tx, b) }); err != nil {
		t.Fatalf("record b's pull request: %v", err)
	}
	github := newIssueReadStandIn(t, time.Date(2026, 11, 23, 1, 0, 0, 0, time.UTC))
	w := readRuntime(pool, github.URL, quietLogger())

	w.readIssuesAtStart(context.Background())
	if a, b := github.count("/repos/acme/widgets/pulls/86"), github.count("/repos/acme/widgets/pulls/87"); a != 1 || b != 1 {
		t.Fatalf("the start read a's #86 %d times and b's #87 %d times, want each once: GitHub was read %v", a, b, github.counts())
	}
}

// The read applies a missing pull request (intake.PullRequestMissing) only for a task whose step
// stands at implementing unheld: a held task waits on its owner, whose decision restarts its
// implementer, and no step works on the pull request the fact would ask its architect for.
// CAPTURE-1's task b, whose implementer completed at handoff-b with no pull request open on
// legion/CAPTURE-1.b, is held; the read of b's pull request lists b's branch, applies no fact and
// tells nobody. The workflow tells nothing for such a fact either (its pullRequestMissing reads the
// task held), so the fact the read no longer applies is what this test reads.
//
// The fault it catches is a held step read as implementing, so the read applies a fact for a task
// that waits on its owner (its control: the read of implementing that takes a held step).
func TestAHeldTaskIsNotToldItsPullRequestIsMissing(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedTwoTasks(t, pool)
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().HoldTask(context.Background(), tx, "CAPTURE-1", 1, "b", record.HoldEscalated)
	}); err != nil {
		t.Fatalf("hold task b: %v", err)
	}
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

	_, err := w.readIssuePullRequest(context.Background(), "CAPTURE-1", "b")
	if err != nil || len(listed) != 1 || listed[0] != "acme:legion/CAPTURE-1.b" {
		t.Fatalf("the read listed the open pull requests on %v, %v; want b's branch alone, no error", listed, err)
	}
	if applied, reasons := headReads(t, pool, "pull-request-missing"), noticeReasons(t, pool, "pull-request-missing"); applied != 0 || len(reasons) != 0 {
		t.Fatalf("the read applied %d missing pull requests and told %q, want none: task b is held", applied, reasons)
	}
}

// The read of an issue's pull request judges, of a plan of any count of tasks, the task a
// completion names, else the task of the step its implementer's phases row runs
// (record.PhaseRow.StepTask), else, for a row raised before the plan, which names no step, the task
// that merges (record.StoredPlan.MergingTask); and the issue's own work, the zero task, where the
// issue has no plan rows. A named task the plan does not hold judges none, and so does a row naming
// no step of a plan in which no task merges.
//
// The fault it catches is a plan of one task judged by its count of tasks rather than by the task
// named, and a row raised before a plan of several tasks judging none, so the read of such an issue
// reads no pull request (its control: the read with no fallback to the task that merges).
func TestTheReadJudgesTheNamedTaskElseTheImplementersElseTheOneThatMerges(t *testing.T) {
	t.Parallel()
	one := &record.StoredPlan{Tasks: []record.PlanTask{{Task: "a"}}}
	chain := &record.StoredPlan{Tasks: []record.PlanTask{{Task: "a"}, {Task: "b", Needs: []string{"a"}}}}
	apart := &record.StoredPlan{Tasks: []record.PlanTask{{Task: "a"}, {Task: "b"}}}
	for _, tc := range []struct {
		name             string
		stored           *record.StoredPlan
		row, named, want string
		judged           bool
	}{
		{"no plan rows: the issue's own work", nil, "", "", "", true},
		{"one task, named", one, "a", "a", "a", true},
		{"one task, the row raised before the plan", one, "", "", "a", true},
		{"one task, a task the plan does not hold named", one, "a", "z", "", false},
		{"two tasks, named", chain, "a", "b", "b", true},
		{"two tasks, the implementer's row", chain, "a", "", "a", true},
		{"two tasks, the row raised before the plan: the task that merges", chain, "", "", "b", true},
		{"two tasks that no task needs, the row raised before the plan: none merges", apart, "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, judged := judgedTask(tc.stored, record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleImplementer, StepTask: tc.row}, tc.named)
			if task.Task != tc.want || judged != tc.judged {
				t.Fatalf("the read judges task %q (%t), want %q (%t)", task.Task, judged, tc.want, tc.judged)
			}
		})
	}
}
