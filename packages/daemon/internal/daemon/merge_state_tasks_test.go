package daemon

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

func TestABasePushReadsEachOwedTasksOwnPullRequest(t *testing.T) {
	for _, tc := range []struct {
		name, secondBase string
		want             []int
	}{
		{"two tasks on the pushed base keep both reads", "main", []int{86, 87}},
		{"a push reads its task and not the judged task on another base", "release", []int{86}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := isolatedOutboxPool(t)
			seedMergeStateTasks(t, pool, tc.secondBase)
			rig := newBatchRig(t, pool, func(_ int32, asked []int, _ http.Header) (int, string) {
				first := batchNode(86, "head-a", "MERGEABLE", "BLOCKED")
				first["headRefName"] = "legion/CAPTURE-1"
				second := batchNode(87, "head-b", "MERGEABLE", "BLOCKED")
				second["headRefName"], second["baseRefName"] = "legion/CAPTURE-1.b", tc.secondBase
				return http.StatusOK, batchAnswer(t, asked, first, second)
			})
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("queries: %v; reader log: %s", rig.askedOf(), rig.logs.String())
				}
			})
			pushOntoMain(t, pool, rig.w, "main-2")
			testwait.Eventually(t, "the base push's batch to answer and end", func() bool {
				return len(rig.askedOf()) > 0 && rig.ended()
			})
			var got []int
			for _, query := range rig.askedOf() {
				got = append(got, query...)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("the base push read task pull requests %v, want %v: issue identity must not collapse task identities or select the other base's task", got, tc.want)
			}
			for _, number := range []int{86, 87} {
				var state string
				if err := pool.QueryRow(t.Context(), "select merge_state from pull_requests where issue = $1 and number = $2", "CAPTURE-1", number).Scan(&state); err != nil {
					t.Fatal(err)
				}
				want := record.MergeStateClean
				if slices.Contains(tc.want, number) {
					want = record.MergeStateBlocked
				}
				if state != string(want) {
					t.Fatalf("task pull request %d recorded %s, want %s", number, state, want)
				}
			}
		})
	}
}

func TestMergeStateQueuesKeepEachTasksPullRequest(t *testing.T) {
	a := record.PullRequestKey{Issue: "CAPTURE-1", Number: 86}
	b := record.PullRequestKey{Issue: "CAPTURE-1", Number: 87}
	pa := record.PullRequest{Issue: a.Issue, Number: a.Number, HeadSHA: "same-head"}
	pb := record.PullRequest{Issue: b.Issue, Number: b.Number, HeadSHA: "same-head"}
	w := &workflowRuntime{mergeStates: newPullRequestReadSet(), baseWake: newWakeSet[string]()}
	w.queueMergeStateFollow(a, &pa)
	w.queueMergeStateFollow(b, &pb)
	w.queueMergeState(a)
	if len(w.mergeStateQueued) != 2 || w.mergeStateQueued[a] != nil || w.mergeStateQueued[b] != &pb || w.mergeStates.waiting() != 2 {
		t.Fatalf("queued = %+v, waiting = %d; want a's climb and b's follow kept separately", w.mergeStateQueued, w.mergeStates.waiting())
	}
	if got, ok := w.mergeStates.take(t.Context()); !ok || !slices.Equal(got, []record.PullRequestKey{a, b}) {
		t.Fatalf("wake = %+v, %t; want both task pull requests", got, ok)
	}
	w.mergeStateRunning = map[record.PullRequestKey]*mergeStateRun{a: {}, b: {}}
	if !w.mergeStateEnds(t.Context(), a) || w.mergeStateRunning[b] == nil || len(w.mergeStateRunning) != 1 {
		t.Fatalf("ending a's ladder left %+v; want b's ladder alone", w.mergeStateRunning)
	}
	repo := ghrepo.MustParse("acme/widgets")
	main, release := baseReadKey(repo, "main"), baseReadKey(repo, "release")
	w.queueBaseRead(repo, "main", map[record.PullRequestKey]string{a: pa.HeadSHA, b: pb.HeadSHA}, false)
	if len(w.baseReads[main].owed) != 2 || len(w.baseReadOf) != 2 {
		t.Fatalf("base owes %+v under %+v; want both tasks even at the same head", w.baseReads[main].owed, w.baseReadOf)
	}
	w.moveBase(main, a, w.baseReads[main].owed[a], repo, "release")
	if len(w.baseReads[main].owed) != 1 || w.baseReadOf[b] != main || w.baseReadOf[a] != release {
		t.Fatalf("moving a left base ownership %+v; want b on main and a on release", w.baseReadOf)
	}
	w.leaveBase(release, a, w.baseReads[release].owed[a].asked)
	if len(w.baseReadOf) != 1 || w.baseReadOf[b] != main || len(w.baseReads[main].owed) != 1 {
		t.Fatalf("ending a's follow left %+v; want b's main follow kept", w.baseReadOf)
	}
}

func TestTheStartQueuesEachTasksUnknownPullRequest(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedMergeStateTasks(t, pool, "main")
	api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		number, task, branch := 86, "a", "legion/CAPTURE-1"
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/86":
		case "/repos/acme/widgets/pulls/87":
			number, task, branch = 87, "b", "legion/CAPTURE-1.b"
		case "/repos/acme/widgets/pulls/86/reviews", "/repos/acme/widgets/pulls/87/reviews":
			fmt.Fprint(w, "[]")
			return
		default:
			t.Errorf("unexpected start read: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"number":%d,"state":"open","head":{"sha":"head-%s","ref":%q},"base":{"ref":"main"},"mergeable":null,"mergeable_state":"unknown"}`, number, task, branch)
	}})
	w := readRuntime(pool, api, quietLogger())
	w.readIssuesAtStart(t.Context())
	if w.mergeStates.waiting() != 2 || len(w.mergeStateQueued) != 2 {
		t.Fatalf("start queued %+v with %d wakes; want each task's unknown pull request", w.mergeStateQueued, w.mergeStates.waiting())
	}
	want := []record.PullRequestKey{{Issue: "CAPTURE-1", Number: 86}, {Issue: "CAPTURE-1", Number: 87}}
	if got, ok := w.mergeStates.take(t.Context()); !ok || !slices.Equal(got, want) {
		t.Fatalf("start queued %+v, %t; want %+v", got, ok, want)
	}
}

// The two live tasks use distinct roles and agents; their final task waits for both. The
// implementer's last step names b, making b the issue's judged task even when only a's base moves.
func seedMergeStateTasks(t *testing.T, pool *store.Pool, secondBase string) {
	t.Helper()
	records := record.NewStore()
	const issue = "CAPTURE-1"
	version := record.PlanVersion{Issue: issue, Generation: 1, Version: 1, Source: record.PlanSourceOwner, ArtifactID: "plan-artifact", GivenAt: time.Now()}
	tasks := []record.PlanTask{
		{Issue: issue, Generation: 1, Task: "a", PlanVersion: 1, State: record.TaskWaiting},
		{Issue: issue, Generation: 1, Task: "b", PlanVersion: 1, State: record.TaskWaiting},
		{Issue: issue, Generation: 1, Task: "z", PlanVersion: 1, State: record.TaskWaiting, Needs: []string{"a", "b"}},
	}
	steps := []record.PlanStep{
		{Issue: issue, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer"},
		{Issue: issue, Generation: 1, Task: "a", Position: 2, Role: "tester", Type: "test", Agent: "tester"},
		{Issue: issue, Generation: 1, Task: "b", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer"},
		{Issue: issue, Generation: 1, Task: "b", Position: 2, Role: "reviewer", Type: "review", Agent: "reviewer"},
		{Issue: issue, Generation: 1, Task: "z", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer"},
	}
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: issue, Tree: issue, Project: "CAPTURE", Repo: "acme/widgets", GitHubRepo: "acme/widgets", DefaultBranch: "main", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"}); err != nil {
			return err
		}
		if err := records.PutPlan(ctx, tx, version, tasks, steps); err != nil {
			return err
		}
		for i, task := range []string{"a", "b"} {
			branch, err := records.AssignTaskBranch(ctx, tx, issue, 1, task)
			if err != nil {
				return err
			}
			if err := records.MoveTask(ctx, tx, issue, 1, task, record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning); err != nil {
				return err
			}
			if err := records.SetStepWait(ctx, tx, issue, 1, task, record.StepWaitNone, nil); err != nil {
				return err
			}
			base := "main"
			if i == 1 {
				base = secondBase
			}
			if err := records.PutPullRequest(ctx, tx, record.PullRequest{Issue: issue, Repo: "acme/widgets", Number: 86 + i, Branch: branch, Base: base, State: record.PullRequestOpen, HeadSHA: "head-" + task, Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean, Failing: []string{}}); err != nil {
				return err
			}
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: issue, Role: claim.RoleImplementer, Claim: "implement-claim", StepTask: "b", StepPlace: 1})
	}); err != nil {
		t.Fatal(err)
	}
}
