package workflow

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// taskAt is one task these tests seed: its id, its steps (`implementer` where none are named), the
// tasks it needs, where it stands, whether its block says merges, and its pull request: number, 0
// for none, merged (with the merge commit merge-<id>) where merged says so.
type taskAt struct {
	id     string
	steps  []string
	needs  []string
	state  record.TaskState
	place  record.TaskPlace
	merges bool
	number int
	merged bool
}

// seedTasksAt writes the plan of tasks on key at generation, each step with an agent of its own,
// the first task on the issue's own branch and each other on <issue branch>.<id>, and the pull
// request of each that has one on its branch.
func seedTasksAt(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, tasks ...taskAt) {
	t.Helper()
	var rows []planTask
	for _, task := range tasks {
		steps := task.steps
		if steps == nil {
			steps = []string{"implementer"}
		}
		agents := make([]claim.Agent, len(steps))
		for i, step := range steps {
			role, _, _ := strings.Cut(step, ":")
			agents[i] = claim.Agent(role + "-" + task.id)
		}
		rows = append(rows, planTask{id: task.id, steps: steps, agents: agents, needs: task.needs, state: task.state, place: task.place})
	}
	seedTasks(t, pool, key, generation, rows...)
	for i, task := range tasks {
		if task.merges {
			markMerges(t, pool, key, task.id)
		}
		branch := claim.IssueBranch(key)
		if i > 0 {
			branch += "." + task.id
		}
		seedTaskBranch(t, pool, key, task.id, branch)
		if task.number == 0 {
			continue
		}
		pr := record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "sjawhar/legion", Number: task.number, Branch: branch,
			HeadSHA: "head-" + task.id, Failing: []string{}}
		if task.merged {
			pr.State, pr.MergeSHA = record.PullRequestMerged, "merge-"+task.id
		}
		seedPR(t, pool, pr)
	}
}

// mergedFact is GitHub's merge of pull request number, with the merge commit merge-<task>.
func mergedFact(number int, task string) intake.PullRequestMerged {
	return intake.PullRequestMerged{Repo: "sjawhar/legion", Number: number, MergeSHA: "merge-" + task}
}

// statusMove is a person's move of key in Dispatch to status, at seq, under parent ("" for the root
// LEGION-208, ranked U as stepRoot ranks it; a child is ranked V as stepChild ranks it).
func statusMove(key, parent, status string, seq int64) intake.DispatchIssue {
	rank := "V"
	if parent == "" {
		rank = "U"
	}
	return intake.DispatchIssue{Key: key, ResolvedRepo: "sjawhar/legion", Seq: seq, Type: "issue.updated", Status: status,
		Title: key, Parent: parent, Rank: rank}
}

// checksAre fails unless key holds a production check of each merge commit of want and of no
// other, whatever their order.
func checksAre(t *testing.T, pool *pgxpool.Pool, key string, want ...string) {
	t.Helper()
	got := productionChecksOf(t, pool, key)
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the production checks of %s are of the merges %q, want %q", key, got, want)
	}
}

// The leave at the merge that ends the issue opens the check of each merged pull request of the
// run that has none. Of a and e, each merging by itself, e merges first and is done, and the issue
// goes on; then a's merge ends the issue. The issue holds two checks, one of e's merge and one of
// a's, whether or not e's merge opened its own check when it landed.
//
// The fault it catches is a leave that reads one merge of the issue, so a run with two merges keeps
// one of them unchecked (its control: the leave reading the issue's merge pull request alone, e's,
// the last once both are done, under which a's check never opens).
func TestTheLeaveAtTheLastMergeOpensTheCheckOfThatMerge(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Merging)
	seedTasksAt(t, pool, root.Key, root.Generation,
		taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 42},
		taskAt{id: "e", state: record.TaskRunning, place: atMerge, number: 45})
	apply := applyFacts(t, pool, engine)
	mustSpend(t, "the merge of e's pull request", apply("merged-e", mergedFact(45, "e")))
	assertTask(t, tasksOf(t, pool, engine, root.Key), "e", record.TaskDone, atMerge)
	mustSpend(t, "the merge of a's pull request", apply("merged-a", mergedFact(42, "a")))
	if got := issueAt(t, pool, root.Key); got != phase.Done {
		t.Fatalf("%s after a's merge stands in %s, want done: a's merge was its last", root.Key, got)
	}
	checksAre(t, pool, root.Key, "merge-a", "merge-e")
}

// A held retro reads its own task's pull request. Of a and b, each `implementer` merging by itself,
// with LEGION-209 in the workflow beneath LEGION-208, a passes its step, its retro passes and its
// pull request #42 merges, so a is done; b passes its step and, the last to finish, is held before
// its retro. The child leaves the workflow: b's retro starts, and its task names b's pull request
// #45, not a's #42.
//
// The fault it catches is a retro told of another task's pull request (its control: the held
// retro reading the issue's first pull request in task order, a's #42).
func TestAHeldRetroReadsItsTasksPullRequest(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	stepChild(t, pool, heldParentChild, phase.Implementing, nil)
	seedAAndB(t, pool, engine, root.Key, root.Generation, atStep(1), atStep(1), 45)
	taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")
	taskMoves(t, pool, engine, root.Key, "a", TriggerRetroCompleted, "")
	mustSpend(t, "the merge of a's pull request", applyFacts(t, pool, engine)("merged-a", mergedFact(42, "a")))
	taskMoves(t, pool, engine, root.Key, "b", TriggerImplementationReady, "")
	if b := tasksOf(t, pool, engine, root.Key)["b"]; b.State != record.TaskHeld || b.Place != atRetro {
		t.Fatalf("premise: task b after its step passed is %s at %+v, want held at its retro", b.State, b.Place)
	}
	before := len(startsIn(t, pool, root.Key, phase.RoleFor(phase.Retro), phase.Retro))

	childLeavesDone(t, pool, engine)

	starts := startsIn(t, pool, root.Key, phase.RoleFor(phase.Retro), phase.Retro)
	if len(starts) != before+1 {
		t.Fatalf("the retro starts of %s once the child left = %d, want %d: b's retro starts", root.Key, len(starts), before+1)
	}
	text := starts[len(starts)-1].Task
	if !strings.Contains(text, pullRequestLabel+": sjawhar/legion#45.") || strings.Contains(text, "sjawhar/legion#42") {
		t.Fatalf("the task of b's retro = %q, want it to name b's pull request #45 and not a's #42", text)
	}
}

// A person's done of a child whose last merge waits for the issue beneath it opens the check of
// that merge. LEGION-209's one task a stands at its merge, its pull request #43 merges, and its
// close waits for LEGION-210 beneath it; then a person sets LEGION-209 done. It holds one check, of
// a's merge.
//
// The fault it catches is a merge whose issue a person closed while it waited, left with no check
// (its control: a leave that reads no merge where no task's end ends the issue).
func TestAPersonsDoneWhileTheLastMergeWaitsOpensItsCheck(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	stepRoot(t, pool, phase.Implementing)
	child := stepChild(t, pool, heldParentChild, phase.Merging, nil)
	seedTasksAt(t, pool, child.Key, child.Generation, taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 43})
	parent := child.Key
	seedIssue(t, pool, record.Issue{Key: "LEGION-210", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "LEGION-210", Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "W", LastDispatchSeq: 1})
	apply := applyFacts(t, pool, engine)
	mustSpend(t, "the merge of a's pull request", apply("merged-a", mergedFact(43, "a")))
	if got := issueAt(t, pool, child.Key); got != phase.Merging {
		t.Fatalf("premise: %s after a's merge stands in %s, want merging: its close waits for LEGION-210", child.Key, got)
	}
	checksAre(t, pool, child.Key)

	mustSpend(t, "the person's done", apply("child-done", statusMove(child.Key, "LEGION-208", "done", 2)))
	checksAre(t, pool, child.Key, "merge-a")
}

// A person's done checks each merge of the run, a merge before its task's merge among them. Of a
// and c, each merging by itself, a person merges a's pull request while a stands at its retro,
// which moves no task; c stands at its first step. A person then sets the issue done: it holds one
// check, of a's merge. Where c's pull request merged too while c stood at its step, it holds two.
//
// The fault it catches is a merge whose work is on main left unchecked because the issue's merge is
// read from one task (its control: the leave reading the issue's merge pull request alone, once its
// tasks are written done, c's, the last: no check where c's is open, one where it merged).
func TestAPersonsDoneChecksEachMergeBeforeMerging(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cMerged bool
		want    []string
	}{
		{"a's pull request merged", false, []string{"merge-a"}},
		{"both pull requests merged", true, []string{"merge-a", "merge-c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Implementing)
			seedTasksAt(t, pool, root.Key, root.Generation,
				taskAt{id: "a", state: record.TaskRunning, place: atRetro, number: 42},
				taskAt{id: "c", state: record.TaskRunning, place: atStep(1), number: 45})
			apply := applyFacts(t, pool, engine)
			mustSpend(t, "the merge of a's pull request", apply("merged-a", mergedFact(42, "a")))
			if tc.cMerged {
				mustSpend(t, "the merge of c's pull request", apply("merged-c", mergedFact(45, "c")))
			}
			assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atRetro)
			checksAre(t, pool, root.Key)

			mustSpend(t, "the person's done", apply("root-done", statusMove(root.Key, "", "done", 2)))
			checksAre(t, pool, root.Key, tc.want...)
		})
	}
}

// A child parked while a merge of its run was to come, and set done later, gets the check of that
// merge once it has landed, whenever it landed. (1) A child of one task a, parked at a's review with
// its pull request #43 open, whose pull request a person then merges. (2) A child parked while its
// last merge waits for the issue beneath it, #43 merged. (3) The same with the merge landing after
// the park, from a merge queue. Each is set done after: one check, of a's merge.
//
// The fault it catches is a parked issue's merge never checked in production (its controls: the
// tasks that have a merge read without the left tasks, and a left task counted only at its merge,
// under which case (1) opens none).
func TestAParkedChildSetDoneChecksItsMerge(t *testing.T) {
	for _, tc := range []struct {
		name        string
		at          record.TaskPlace
		steps       []string
		phase       phase.Phase
		beneath     bool
		mergedFirst bool
	}{
		{"parked at its review, merged by a person", atStep(2), []string{"implementer", "reviewer"}, phase.Reviewing, false, false},
		{"parked while its last merge waits", atMerge, nil, phase.Merging, true, true},
		{"parked with its merge in a queue", atMerge, nil, phase.Merging, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			stepRoot(t, pool, phase.Implementing)
			child := stepChild(t, pool, heldParentChild, tc.phase, nil)
			seedTasksAt(t, pool, child.Key, child.Generation, taskAt{id: "a", steps: tc.steps, state: record.TaskRunning, place: tc.at, number: 43})
			if tc.beneath {
				parent := child.Key
				seedIssue(t, pool, record.Issue{Key: "LEGION-210", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
					Tree: "LEGION-208", Project: "LEGION", Title: "LEGION-210", Parent: &parent, Phase: phase.Implementing, Generation: 1,
					Status: "in_progress", Rank: "W", LastDispatchSeq: 1})
			}
			apply := applyFacts(t, pool, engine)
			if tc.mergedFirst {
				mustSpend(t, "the merge of a's pull request", apply("merged-a", mergedFact(43, "a")))
			}
			mustSpend(t, "the park", apply("child-backlog", statusMove(child.Key, "LEGION-208", "backlog", 2)))
			if task := tasksOf(t, pool, engine, child.Key)["a"]; task.State != record.TaskLeft || task.Place != tc.at {
				t.Fatalf("premise: task a after the park is %s at %+v, want left at %+v", task.State, task.Place, tc.at)
			}
			checksAre(t, pool, child.Key)
			if !tc.mergedFirst {
				mustSpend(t, "the merge of a's pull request", apply("merged-a", mergedFact(43, "a")))
			}
			mustSpend(t, "the done", apply("child-done", statusMove(child.Key, "LEGION-208", "done", 3)))
			checksAre(t, pool, child.Key, "merge-a")
		})
	}
}

// A park opens no check: a child whose task a stands at its retro with its pull request #43 merged
// (a merge before merging, with no check) is parked at backlog. It holds no check, and the park's
// suspension of its pod is queued.
//
// The fault it catches is a check opened under a park, whose suspension then stops its look (its
// control: the leave opening the checks of its merges at any status, under which it holds one).
func TestAParkOpensNoCheck(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	stepRoot(t, pool, phase.Implementing)
	child := stepChild(t, pool, heldParentChild, phase.Retro, nil)
	seedTasksAt(t, pool, child.Key, child.Generation, taskAt{id: "a", state: record.TaskRunning, place: atRetro, number: 43, merged: true})
	mustSpend(t, "the park", applyFacts(t, pool, engine)("child-backlog", statusMove(child.Key, "LEGION-208", "backlog", 2)))
	checksAre(t, pool, child.Key)
	if rows := issueSuspensions(t, pool, child.Key); len(rows) != 1 {
		t.Fatalf("the issue_suspend rows of %s after the park = %+v, want one", child.Key, rows)
	}
}

// At the merge that ends the issue, the notice to the architect above names no pull request that
// merge closes. LEGION-209's plan: x, done at its last step with its pull request #43 open, and a,
// which needs x and merges, at its merge with #45. a's merge ends the issue and, in its own
// transaction, queues the close of #43: the child-closed notice reads "LEGION-209 is done".
//
// The fault it catches is a notice that tells the architect of a pull request the daemon is closing
// (its control: the notice without its filter of the queued closes, which names #43 open).
func TestTheNoticeAtTheMergeThatEndsTheIssueNamesNoPullRequestTheMergeCloses(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	stepRoot(t, pool, phase.Implementing)
	child := stepChild(t, pool, heldParentChild, phase.Merging, nil)
	seedTasksAt(t, pool, child.Key, child.Generation,
		taskAt{id: "x", state: record.TaskDone, place: atStep(1), number: 43},
		taskAt{id: "a", needs: []string{"x"}, state: record.TaskRunning, place: atMerge, number: 45})
	mustSpend(t, "the merge of a's pull request", applyFacts(t, pool, engine)("merged-a", mergedFact(45, "a")))
	if closes := closesOf(t, pool, child.Key); len(closes) != 1 || !reflect.DeepEqual(closes[0].Numbers, []int{43}) {
		t.Fatalf("premise: the closes of pull requests of %s after a's merge = %+v, want one of #43", child.Key, closes)
	}
	told := noticesOfKind(t, pool, child.Key, "child-closed")
	if len(told) != 1 || told[0].Reason != child.Key+" is done" {
		t.Fatalf("the child-closed notices of %s = %+v, want one reading %q", child.Key, told, child.Key+" is done")
	}
}

// leaveByTasksLines is how many times logged says an issue whose tasks are done or left leaves
// the workflow through the one place that writes that leave (leaveByTasks).
func leaveByTasksLines(logged *bytes.Buffer, key string) int {
	lines := 0
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "each task of the issue is done or left, and it leaves the workflow") && strings.Contains(line, "issue="+key) {
			lines++
		}
	}
	return lines
}

// Each door by which an issue's tasks put it in done leaves through one place, which checks each
// merge of the run. Of a, merged earlier with no check, and c, each merging by itself, the issue
// ends at each door in turn: c's merge in a running tree; c's merge waiting for the issue beneath,
// closed once that issue leaves; the boot's sweep of a merge GitHub reported while the daemon was
// down; a version that removes a stopped third task once c merged; and c's merge in a lingering
// tree. At each door the issue stands done, it leaves through the one place once, and it holds a
// check of each merge, a's and c's.
//
// The faults it catches are a door that leaves by itself, so a later rule of that leave misses it
// (its control: a door calling leave itself, under which the one place logs nothing there), and a
// lingering tree's last merge that checks only itself (its control: that branch opening the check of
// its own merge alone, under which a's check is missing).
func TestEachDoorFromTheTasksLeavesThroughOnePlace(t *testing.T) {
	merged := taskAt{id: "a", state: record.TaskDone, place: atMerge, number: 42, merged: true, merges: true}
	for _, tc := range []struct {
		name string
		door func(t *testing.T, pool *pgxpool.Pool, engine *Engine) string
	}{
		{"c's merge in a running tree", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) string {
			root := stepRoot(t, pool, phase.Merging)
			seedTasksAt(t, pool, root.Key, root.Generation, merged, taskAt{id: "c", state: record.TaskRunning, place: atMerge, number: 45, merges: true})
			mustSpend(t, "the merge of c's pull request", applyFacts(t, pool, engine)("merged-c", mergedFact(45, "c")))
			return root.Key
		}},
		{"c's merge once the issue beneath left", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) string {
			root := stepRoot(t, pool, phase.Merging)
			stepChild(t, pool, heldParentChild, phase.Implementing, nil)
			seedTasksAt(t, pool, root.Key, root.Generation, merged, taskAt{id: "c", state: record.TaskRunning, place: atMerge, number: 45, merges: true})
			mustSpend(t, "the merge of c's pull request", applyFacts(t, pool, engine)("merged-c", mergedFact(45, "c")))
			if got := issueAt(t, pool, root.Key); got != phase.Merging {
				t.Fatalf("premise: %s after c's merge stands in %s, want merging: its close waits for %s", root.Key, got, heldParentChild)
			}
			childLeavesDone(t, pool, engine)
			return root.Key
		}},
		{"the boot's sweep of c's merge", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) string {
			root := stepRoot(t, pool, phase.Merging)
			seedTasksAt(t, pool, root.Key, root.Generation, merged,
				taskAt{id: "c", state: record.TaskRunning, place: atMerge, number: 45, merged: true, merges: true})
			applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error { return engine.sweepMerged(ctx, tx) })
			return root.Key
		}},
		{"a version that removes the stopped s", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) string {
			root := stepRoot(t, pool, phase.Merging)
			seedTasksAt(t, pool, root.Key, root.Generation, merged,
				taskAt{id: "c", state: record.TaskRunning, place: atMerge, number: 45, merges: true},
				taskAt{id: "s", state: record.TaskRunning, place: atStep(1)})
			apply := applyFacts(t, pool, engine)
			if result := apply("stop-s", ownersStop(t, root, "s")); result.Refusal != nil {
				t.Fatalf("the owner's stop of s was refused: %+v", result.Refusal)
			}
			mustSpend(t, "the merge of c's pull request", apply("merged-c", mergedFact(45, "c")))
			if got := issueAt(t, pool, root.Key); got == phase.Done {
				t.Fatalf("premise: %s after c's merge stands done, want it held for the stopped s", root.Key)
			}
			aBlock, cBlock := keyed(t, "a", "", []string{"implementer"}), keyed(t, "c", "", []string{"implementer"})
			aBlock.Merges, cBlock.Merges = true, true
			mustTake(t, "version 2, which removes s", apply("version-2", laterVersion(t, root.Key, root.Generation, 2, aBlock, cBlock)))
			return root.Key
		}},
		{"c's merge in a lingering tree", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) string {
			until := stepClock.Add(time.Hour)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
				Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until, LastDispatchSeq: 1})
			child := stepChild(t, pool, heldParentChild, phase.Merging, nil)
			seedTasksAt(t, pool, child.Key, child.Generation, taskAt{id: "a", state: record.TaskDone, place: atMerge, number: 43, merged: true, merges: true},
				taskAt{id: "c", state: record.TaskRunning, place: atMerge, number: 45, merges: true})
			mustSpend(t, "the merge of c's pull request", applyFacts(t, pool, engine)("merged-c", mergedFact(45, "c")))
			return child.Key
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine, logged := loggedEngine()
			key := tc.door(t, pool, engine)
			if got := issueAt(t, pool, key); got != phase.Done {
				t.Fatalf("%s after the door stands in %s, want done", key, got)
			}
			if lines := leaveByTasksLines(logged, key); lines != 1 {
				t.Fatalf("%s left through the one place of the close by its tasks %d times, want once:\n%s", key, lines, logged.String())
			}
			checksAre(t, pool, key, "merge-a", "merge-c")
		})
	}
}

// A root parked and then set done while its tree lingers opens the check of each merge of its run.
// LEGION-208, a root of one task a, is parked at triage at a's review, with its pull request #42
// open: its tree lingers, and a is left at its review. A person merges #42, which moves no task, and
// then sets the root done: it holds one check, of #42's merge, whose first look is a wake of the
// checking role's suspended claim.
//
// The fault it catches is a merge of a lingering root never checked in production (its control:
// the leave of a lingering root returning before it reads its merges, under which it holds none).
func TestARootParkedThenSetDoneInItsLingerChecksItsMerge(t *testing.T) {
	pool := migratedPool(t)
	engine, logged := loggedEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	seedTasksAt(t, pool, root.Key, root.Generation,
		taskAt{id: "a", steps: []string{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(2), number: 42})
	apply := applyFacts(t, pool, engine)
	mustSpend(t, "the park", apply("root-triage", statusMove(root.Key, "", "triage", 2)))
	if parked := issueOf(t, pool, engine, root.Key); !parked.Lingers() {
		t.Fatalf("premise: %s after its park does not linger", root.Key)
	}
	mustSpend(t, "the merge of #42", apply("merged-a", mergedFact(42, "a")))
	checksAre(t, pool, root.Key)
	logged.Reset()
	mustSpend(t, "the done", apply("root-done", statusMove(root.Key, "", "done", 3)))
	checksAre(t, pool, root.Key, "merge-a")
	if !strings.Contains(logged.String(), "production check opened") || !strings.Contains(logged.String(), "firstLook=wake") {
		t.Fatalf("the log of the done = %q, want the check opened with its first look a wake", logged.String())
	}
}

// An issue in merging is finished where each task that stands at its merge has its pull request
// merged, read on the issue as the store holds it, with no copy for a task. Of a and c, each merging
// by itself and each at its merge: with a's pull request merged and c's open the phase is not
// finished, and with both merged it is, and its worker is then owed no turn. With a done at its merge
// and c at its merge with its pull request open it is not finished. An issue with no plan rows in
// merging, its one pull request merged, is finished, as before tasks had merges of their own.
//
// The fault it catches is a merger started again, or one owed a turn, for a merge GitHub already
// made, or one not started for a merge still to come (its control: the phase read from the pull
// request of the task of a copy, which the stored issue does not name, so it is never finished).
func TestAnIssueInMergingIsFinishedWhenEachTaskAtItsMergeMerged(t *testing.T) {
	finished := func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) (bool, bool) {
		t.Helper()
		var done, owed bool
		onStored(t, pool, engine, key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) (err error) {
			if done, err = PhaseFinished(ctx, engine.store, tx, issue); err != nil {
				return err
			}
			_, owed, err = PhaseWorkerOwedTurn(ctx, engine.store, tx, issue, claim.RoleMerger)
			return err
		})
		return done, owed
	}
	for _, tc := range []struct {
		name   string
		a      taskAt
		merged bool
		want   bool
	}{
		{"a's merged and c's open", taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 42, merged: true}, false, false},
		{"both merged", taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 42, merged: true}, true, true},
		{"a done at its merge and c's open", taskAt{id: "a", state: record.TaskDone, place: atMerge, number: 42, merged: true}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Merging)
			seedTasksAt(t, pool, root.Key, root.Generation, tc.a,
				taskAt{id: "c", state: record.TaskRunning, place: atMerge, number: 45, merged: tc.merged})
			done, owed := finished(t, pool, engine, root.Key)
			if done != tc.want || owed == tc.want {
				t.Fatalf("%s's merging is finished %t, its merger owed a turn %t, want finished %t", root.Key, done, owed, tc.want)
			}
		})
	}
	t.Run("no plan rows", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Merging)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestMerged, Issue: root.Key, Repo: "sjawhar/legion", Number: 42,
			Branch: "legion/LEGION-208", HeadSHA: "head", MergeSHA: "merge-42", Failing: []string{}})
		if done, _ := finished(t, pool, engine, root.Key); !done {
			t.Fatalf("%s, with no plan rows, its one pull request merged, is not finished, want finished", root.Key)
		}
	})
}

// A merge GitHub already made starts no merger, whatever another task's merge waits for. Of a and c,
// each merging by itself, a stands at its merge with its pull request #42 open, and a person merged
// c's #45 while c stood at its retro. c's retro passes, and c reaches its merge on its merged pull
// request: the step record of c's merge names no agent, no merger is started, and c is done.
//
// The fault it catches is a merger handed a merge GitHub already made (its control: the start of
// c's merge reading whether the issue's merging is finished, which a's open pull request keeps not
// finished, so the merger is named for c's merge).
func TestAMergeGitHubMadeStartsNoMergerWhileAnotherTaskWaitsAtItsMerge(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Retro)
	seedTasksAt(t, pool, root.Key, root.Generation,
		taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 42},
		taskAt{id: "c", state: record.TaskRunning, place: atRetro, number: 45, merged: true})
	taskMoves(t, pool, engine, root.Key, "c", TriggerRetroCompleted, "")
	assertTask(t, tasksOf(t, pool, engine, root.Key), "c", record.TaskDone, atMerge)
	var merges []record.StepRecord
	for _, write := range stepWrites(t, pool, root.Key) {
		if write.Task == "c" && write.Kind == record.PlaceMerge {
			merges = append(merges, write)
		}
	}
	if len(merges) == 0 {
		t.Fatalf("the step records of %s hold no merge of c", root.Key)
	}
	for _, write := range merges {
		if write.Agent != "" {
			t.Fatalf("the step record of c's merge names the agent %q, want none: GitHub made the merge", write.Agent)
		}
	}
	if starts := roleStartsOf(t, pool, root.Key, claim.RoleMerger); len(starts) != 0 {
		t.Fatalf("the merger's starts of %s = %+v, want none", root.Key, starts)
	}
}
