package workflow

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// assertBasesMovedByNumber fails unless the commit each recorded pull request's base was last
// pushed to is the one want names it, by its number: the pull requests of one issue's tasks share
// the issue, so the number tells them apart.
func assertBasesMovedByNumber(t *testing.T, pool *pgxpool.Pool, want map[int]string) {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select number, base_moved from pull_requests order by number")
	if err != nil {
		t.Fatalf("read the moved bases: %v", err)
	}
	defer rows.Close()
	got := map[int]string{}
	for rows.Next() {
		var number int
		var moved string
		if err := rows.Scan(&number, &moved); err != nil {
			t.Fatalf("read the moved bases: %v", err)
		}
		got[number] = moved
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the moved bases: %v", err)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("moved bases = %v, want %v", got, want)
	}
}

// assertPushRecorded fails unless the pull request of task of key records the push that left its
// branch at after.
func assertPushRecorded(t *testing.T, pool *pgxpool.Pool, key, task, after string) {
	t.Helper()
	pushes := pullRequestOfTask(t, pool, key, task).Pushes
	if !slices.ContainsFunc(pushes, func(push record.ClassifiedPush) bool { return push.SHA == after }) {
		t.Fatalf("the pull request of task %s records the pushes %+v, want the push to %s among them", task, pushes, after)
	}
}

// A push on a task's branch is also a push on the base of each pull request based on that branch.
// Task c needs b and z needs c, so z is the task that merges and c's pull request, #45 on
// legion/LEGION-208.c, is based on b's branch, legion/LEGION-208.b. b is done and keeps its pull
// request #44 open against main; c stands at its tester's step. A push on b's branch records the
// commit it left that branch at on #45 (record.PullRequest.BaseMoved), asks once for GitHub's merge
// state of c's pull request, named by its issue and its task c, since c stands in testing, and is
// recorded on b's own pull request, as each push on a pull request's branch is. A push on c's
// branch, on which no pull request is based, marks no base and asks for no read. No task moves and
// nothing is queued.
//
// A base push asks its reads as one batch of the base it moved (basePush,
// Config.ReadBaseMergeStates), and a fact of one pull request asks its read apiece
// (Config.ReadMergeState), so the test hears both: a read of either is a read asked, and the
// batch must be of b's branch, the base that moved. A test that hears ReadMergeState alone hears
// none of a base push's reads. Each read names the task of its pull request, since the issue's
// tasks each have a pull request of their own and the daemon reads the one named.
//
// The fault it catches is a pull request whose base moved and whose merge state the daemon still
// reads as clean: a push on a task's branch decodes as the push of that branch's own pull request
// (intake's decodePush: the branch is an issue's), never as a base push. Its control: the push's
// handling without the base push, where #45's base stays unmarked and no read is asked. It also
// catches a read asked by the issue alone, which the daemon reads as the task the record judges
// rather than c (its control: basePush appending TaskPullRequest{Issue: own.Issue} with no task).
func TestAPushOnTheBranchOfANeedMovesTheBaseOfItsDependent(t *testing.T) {
	pool := migratedPool(t)
	engine := stepEngine(nil)
	var asked, bases []string
	engine.cfg.ReadMergeState = func(issue, task string) { asked = append(asked, issue+" "+task) }
	engine.cfg.ReadBaseMergeStates = func(repo, base string, owed []TaskPullRequest) {
		for _, pull := range owed {
			asked = append(asked, pull.Issue+" "+pull.Task)
		}
		bases = append(bases, repo+" "+base)
	}
	root := seedNeedAndDependent(t, pool)
	apply := applyFacts(t, pool, engine)

	mustSpend(t, "the push on b's branch", apply("push-b", intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208.b",
		Before: "head-b", After: "b-moved", Pusher: "a-writer", Sender: "a-writer"}))
	assertBasesMovedByNumber(t, pool, map[int]string{42: "", 44: "", 45: "b-moved"})
	assertPushRecorded(t, pool, root.Key, "b", "b-moved")
	if want := []string{root.Key + " c"}; !slices.Equal(asked, want) {
		t.Fatalf("the merge state reads asked = %v after the push on b's branch, want %v", asked, want)
	}
	if want := []string{"sjawhar/legion legion/LEGION-208.b"}; !slices.Equal(bases, want) {
		t.Fatalf("the bases read by batch = %v after the push on b's branch, want %v", bases, want)
	}

	mustSpend(t, "the push on c's branch", apply("push-c", intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208.c",
		Before: "head-c", After: "c-moved", Pusher: "a-writer", Sender: "a-writer"}))
	assertBasesMovedByNumber(t, pool, map[int]string{42: "", 44: "", 45: "b-moved"})
	assertPushRecorded(t, pool, root.Key, "c", "c-moved")
	if want := []string{root.Key + " c"}; !slices.Equal(asked, want) {
		t.Fatalf("the merge state reads asked = %v after the push on c's branch, want %v", asked, want)
	}
	if want := []string{"sjawhar/legion legion/LEGION-208.b"}; !slices.Equal(bases, want) {
		t.Fatalf("the bases read by batch = %v after the push on c's branch, want %v: no pull request is based on c's branch", bases, want)
	}

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "b", record.TaskDone, atStep(1))
	assertTask(t, tasks, "c", record.TaskRunning, atStep(2))
	assertTask(t, tasks, "z", record.TaskWaiting, record.TaskPlace{})
	assertOutboxKinds(t, pool, []string{})
}

// seedNeedAndDependent seeds LEGION-208 in testing with a plan in which task c needs b and z needs
// c: b is done and keeps its pull request #44 open against main, and c stands at its tester's
// step with its pull request #45 on legion/LEGION-208.c, based on b's branch legion/LEGION-208.b.
func seedNeedAndDependent(t *testing.T, pool *pgxpool.Pool) record.Issue {
	t.Helper()
	root := stepRoot(t, pool, phase.Testing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "b", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "c", steps: []string{"implementer", "tester"}, agents: []claim.Agent{"implementer-2", "tester"}, needs: []string{"b"},
			state: record.TaskRunning, place: atStep(2)},
		planTask{id: "z", steps: []string{"reviewer"}, agents: []claim.Agent{"reviewer"}, needs: []string{"c"}, state: record.TaskWaiting})
	for _, pr := range []struct {
		task   string
		number int
		base   string
	}{{"b", 44, "main"}, {"c", 45, "legion/LEGION-208.b"}} {
		branch := "legion/" + root.Key + "." + pr.task
		seedTaskBranch(t, pool, root.Key, pr.task, branch)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: pr.number,
			Branch: branch, Base: pr.base, HeadSHA: "head-" + pr.task, Failing: []string{}})
	}
	return root
}

// A fact of one task's pull request asks for a read of that pull request's merge state by its
// issue and its task (askMergeState, Config.ReadMergeState): the issue's tasks each have a pull
// request of their own, and the daemon reads the one named. c's #45 moving its head, with c in
// testing, owes a read by the gate (classify.MergeStateReadOwed), and the read asked names
// LEGION-208 and c.
//
// The fault it catches is a read asked by the issue alone, which the daemon reads as the task the
// record judges rather than c, so c's head is not read (its control: askMergeState calling
// ReadMergeState(key, "")).
func TestAFactOfATasksPullRequestAsksTheReadOfThatTasksPullRequest(t *testing.T) {
	pool := migratedPool(t)
	engine := stepEngine(nil)
	var asked []string
	engine.cfg.ReadMergeState = func(issue, task string) { asked = append(asked, issue+" "+task) }
	root := seedNeedAndDependent(t, pool)
	moved := intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 45, Branch: "legion/LEGION-208.c", HeadSHA: "head-c", Before: "head-b"}
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error { return engine.askMergeState(ctx, tx, moved) })
	if want := []string{root.Key + " c"}; !slices.Equal(asked, want) {
		t.Fatalf("the merge state reads asked = %v after c's pull request moved its head, want %v", asked, want)
	}
}
