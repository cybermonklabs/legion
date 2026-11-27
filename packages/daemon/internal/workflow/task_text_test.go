package workflow

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// textEngine is stepEngine whose runtime places the working copy of an issue's task on branch
// legion/<name> at /work/<name> (Config.WorkspaceDir), so the issue's own at /work/<KEY>.
func textEngine() *Engine {
	engine := stepEngine(nil)
	engine.cfg.WorkspaceDir = func(_ record.Issue, branch string) string { return "/work/" + strings.TrimPrefix(branch, "legion/") }
	return engine
}

// storeTaskBranch writes branch as the branch the row of task a of key's plan at generation holds,
// as the task's first start writes it (AssignTaskBranch).
func storeTaskBranch(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, branch string) {
	t.Helper()
	if tag, err := pool.Exec(t.Context(), `update plan_tasks set branch = $3 where issue = $1 and generation = $2 and task = 'a'`,
		key, int64(generation), branch); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write the branch %q of task a of %s: %v (%d rows)", branch, key, err, tag.RowsAffected())
	}
}

// lastStart is the task of the newest start of role in key's outbox.
func lastStart(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) string {
	t.Helper()
	starts := roleStartsOf(t, pool, key, role)
	if len(starts) == 0 {
		t.Fatalf("%s's outbox holds no start of the %s", key, role)
	}
	return starts[len(starts)-1].Task
}

// completes applies role's completion of its step of LEGION-208 with the handoff record word-n at
// version n, through the completion route (intake.ApplyFact), which writes the role's phases row.
func completes(t *testing.T, pool *pgxpool.Pool, engine *Engine, role claim.Role, word string, n int) {
	t.Helper()
	fact := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: role, Claim: claim.Token(string(role) + "-claim"),
		Summary: "done", Commit: "head", HandoffRecord: word + "-" + string(rune('0'+n)), HandoffVersion: n}
	if role == claim.RoleTester {
		fact.Verdict = "pass"
	}
	if result := applyFacts(t, pool, engine)(string(role)+"-"+word+"-"+string(rune('0'+n)), fact); result.Refusal != nil {
		t.Fatalf("the %s's completion %s version %d was refused: %+v", role, word, n, result.Refusal)
	}
}

// hasPrefix fails the test unless the task starts with want.
func hasPrefix(t *testing.T, who, task, want string) {
	t.Helper()
	if !strings.HasPrefix(task, want) {
		t.Fatalf("the %s's task = %q,\nwant it to start %q", who, task, want)
	}
}

// lacks fails the test if task holds any of words.
func lacks(t *testing.T, who, task string, words ...string) {
	t.Helper()
	for _, word := range words {
		if strings.Contains(task, word) {
			t.Fatalf("the %s's task = %q, want no %q", who, task, word)
		}
	}
}

// daemonsLines fails the test unless the task ends with the owner's text: ` Task text: `, one
// sentence that says the owner wrote it, then text between two fence lines of three backticks,
// the closing one the task's last line, with its line end. It answers the task before that part,
// the daemon's own lines.
func daemonsLines(t *testing.T, who, task, text string) string {
	t.Helper()
	at := strings.LastIndex(task, " "+taskTextLabel+": ")
	fenced := "\n```\n" + text + "\n```\n"
	if at < 0 || !strings.HasSuffix(task, fenced) {
		t.Fatalf("the %s's task = %q,\nwant it to end with the owner's text %q", who, task, fenced)
	}
	if sentence := task[at : len(task)-len(fenced)]; strings.Contains(sentence, "\n") || !strings.Contains(sentence, "owner") {
		t.Fatalf("the %s's task introduces the owner's text with %q, want one sentence that names its owner", who, sentence)
	}
	return task[:at]
}

// withoutRoundsSentences is the reviewer's task without the two sentences of the review round that
// end each task of the reviewer in reviewing, after all the task composed (queuedRow): the commit
// its last deciding review read, none in these tests, and its entry, the reviewer's row's now.
func withoutRoundsSentences(t *testing.T, pool *pgxpool.Pool, task string) string {
	t.Helper()
	tail := reviewedPartOf("none") + reviewEntryPartOf(reviewerRow(t, pool).Entry)
	cut, ok := strings.CutSuffix(task, tail)
	if !ok {
		t.Fatalf("the reviewer's task = %q, want it to end with the review round's sentences %q", task, tail)
	}
	return cut
}

// The task each agent of an owner's plan is told names its working copy, its task and its step in
// it, each step of the task in order and the report of each earlier step, and, last of what the
// task composes, the task's text inside a fence, which only the reviewer's two sentences of the
// review round follow, or a review step's one sentence of its commit: the reviewer of the task `implementer reviewer` reads the implementer's
// report, and the retro and the merger, which run no step, are told the task and its steps alone,
// and read the runs of the steps from Dispatch.
func TestATaskNamesItsWorkingCopyItsStepAndTheReportsBeforeIt(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
	const continued = "Continue root. Issue: LEGION-208. Phase: "
	const working = " Working copy: branch legion/LEGION-208, directory /work/LEGION-208."
	const steps = " Steps: 1 implementer, 2 reviewer then Legion's retro and merge."
	const text = "Build the widget."

	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	implementer := daemonsLines(t, "implementer", lastStart(t, pool, root.Key, claim.RoleImplementer), text)
	hasPrefix(t, "implementer", implementer, continued+"implementing."+working+" Task: a. Step 1 of 2: implementer."+steps)
	lacks(t, "implementer", implementer, "reported:")

	completes(t, pool, engine, claim.RoleImplementer, "implement", 1)
	reviewer := daemonsLines(t, "reviewer", withoutRoundsSentences(t, pool, lastStart(t, pool, root.Key, claim.RoleReviewer)), text)
	hasPrefix(t, "reviewer", reviewer, continued+"reviewing."+working+" Task: a. Step 2 of 2: reviewer."+steps+
		" Step 1 (implementer) reported: implement version 1."+
		" Handoff: implement version 1 (dispatch handoff-read --issue LEGION-208 --phase implement --version 1).")

	// A worker resumed in its step is told the same standing as the start that began it.
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, root.Key)
		if err != nil {
			return err
		}
		s, err := firstScope(ctx, tx, engine, *issue)
		if err != nil {
			return err
		}
		_, err = engine.StartPromotedWorker(ctx, tx, s, claim.RoleReviewer, false)
		return err
	})

	resumed := daemonsLines(t, "resumed reviewer", withoutRoundsSentences(t, pool, lastStart(t, pool, root.Key, claim.RoleReviewer)), text)
	if want := continued + "reviewing." + working + " Task: a. Step 2 of 2: reviewer." + steps +
		" Step 1 (implementer) reported: implement version 1. Pull request: #42. Resume the existing phase work."; resumed != want {
		t.Fatalf("the resumed reviewer's task before the owner's text = %q,\nwant %q", resumed, want)
	}

	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	retro := lastStart(t, pool, root.Key, claim.RoleImplementer)
	hasPrefix(t, "retro", retro, continued+"retro."+working+" Task: a."+steps)
	lacks(t, "retro", retro, "reported:", "Task text:", "Step 1 of", "Step 2 of")

	move(t, pool, engine, root.Key, TriggerRetroCompleted, record.TaskPlace{}, record.PhaseRow{})
	merger := lastStart(t, pool, root.Key, claim.RoleMerger)
	hasPrefix(t, "merger", merger, continued+"merging."+working+" Task: a."+steps)
	lacks(t, "merger", merger, "reported:", "Task text:", "Step 1 of", "Step 2 of")
}

// The step of a task whose branch is not the issue's own is told the working copy of its own
// task (R4): the line names the task's branch and the directory the runtime places that branch's
// working copy at, beside the issue's, in the start of its step, in a resume of its worker and in
// the retro of the task. The task's pull request #44 is on its own branch.
func TestAStepOfASecondTaskNamesItsOwnWorkingCopy(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
	storeTaskBranch(t, pool, root.Key, root.Generation, "legion/LEGION-208.b")
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 44,
		Branch: "legion/LEGION-208.b", HeadSHA: "head-b", Failing: []string{}})
	const working = " Working copy: branch legion/LEGION-208.b, directory /work/LEGION-208.b."

	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	// Task a's branch is not the issue's own, so its start waits for its join: the end of that join.
	joinsEnd(t, pool, engine, root.Key)
	hasPrefix(t, "implementer", lastStart(t, pool, root.Key, claim.RoleImplementer),
		"Continue root. Issue: LEGION-208. Phase: implementing."+working+" Task: a. Step 1 of 2: implementer.")

	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, root.Key)
		if err != nil {
			return err
		}
		s, err := firstScope(ctx, tx, engine, *issue)
		if err != nil {
			return err
		}
		_, err = engine.StartPromotedWorker(ctx, tx, s, claim.RoleImplementer, false)
		return err
	})
	hasPrefix(t, "resumed implementer", lastStart(t, pool, root.Key, claim.RoleImplementer),
		"Continue root. Issue: LEGION-208. Phase: implementing."+working+" Task: a. Step 1 of 2: implementer.")

	move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	hasPrefix(t, "retro", lastStart(t, pool, root.Key, claim.RoleImplementer), "Continue root. Issue: LEGION-208. Phase: retro."+working+" Task: a.")
}

// The twin: the task that holds the issue's own branch, and a task that ran at the release before
// tasks had branches, whose row holds none, are told today's line, the issue's own working copy.
func TestTheTaskOfTheIssuesBranchIsTodaysLine(t *testing.T) {
	for _, branch := range []string{"legion/LEGION-208", ""} {
		t.Run("the row holds "+strconv.Quote(branch), func(t *testing.T) {
			pool := migratedPool(t)
			engine := textEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
			storeTaskBranch(t, pool, root.Key, root.Generation, branch)

			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			hasPrefix(t, "implementer", lastStart(t, pool, root.Key, claim.RoleImplementer),
				"Continue root. Issue: LEGION-208. Phase: implementing. Working copy: branch legion/LEGION-208, directory /work/LEGION-208. Task: a.")
		})
	}
}

// A move into a phase that starts no worker composes no task, since no worker is told one: the
// merge of an issue with plan rows closes it, as the merge of an issue with no plan rows does,
// though done holds no place in its task's line, which a start's task must name (standingPart).
func TestAMoveIntoAPhaseThatStartsNoWorkerComposesNoTask(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerRetroCompleted, record.TaskPlace{}, record.PhaseRow{})
	starts := len(superviseRowsOf(t, pool, root.Key, "start"))

	if result := applyFacts(t, pool, engine)("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-42"}); result.Refusal != nil {
		t.Fatalf("the merge was refused: %+v", result.Refusal)
	}
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Done {
		t.Fatalf("%s stands in %s after its merge, want done", root.Key, issue.Phase)
	}
	for _, start := range superviseRowsOf(t, pool, root.Key, "start")[starts:] {
		if start.CheckLook == 0 {
			t.Fatalf("the merge wrote the start %+v, want only its production check's: done starts no phase worker", start)
		}
	}
}

// Each earlier step of a task has its report line when a later step starts, in the two shapes in
// which the row that holds the report is written again: a return to the implementer, whose next
// run's report the reviewer reads, and a return by returnTo to the planner, whose new plan the
// implementer and the tester read.
func TestATaskNamesTheNewReportOfAStepThatRanAgain(t *testing.T) {
	t.Parallel()
	t.Run("implementer reviewer, the review fails and the implementer runs again", func(t *testing.T) {
		pool := migratedPool(t)
		engine := textEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		completes(t, pool, engine, claim.RoleImplementer, "implement", 1)
		completes(t, pool, engine, claim.RoleReviewer, "review", 1)

		reviewRequestsChanges(t, pool, engine, root.Key)
		again := lastStart(t, pool, root.Key, claim.RoleImplementer)
		if !strings.Contains(again, " Task: a. Step 1 of 2: implementer.") || !strings.Contains(again, " Handoff: review version 1 (") {
			t.Fatalf("the implementer's task after the return = %q, want step 1 with the failed review's report", again)
		}
		lacks(t, "returned implementer", again, "reported:")

		completes(t, pool, engine, claim.RoleImplementer, "implement", 2)
		reviewer := lastStart(t, pool, root.Key, claim.RoleReviewer)
		if !strings.Contains(reviewer, " Task: a. Step 2 of 2: reviewer.") || !strings.Contains(reviewer, " Step 1 (implementer) reported: implement version 2.") {
			t.Fatalf("the reviewer's task of the second round = %q, want step 2 naming the implementer's new report", reviewer)
		}
		lacks(t, "second round's reviewer", reviewer, "reported: implement version 1.")
	})

	t.Run("planner implementer tester, returnTo 1 returns the task to the planner", func(t *testing.T) {
		pool := migratedPool(t)
		engine := textEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "planner", "implementer", "tester")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		completes(t, pool, engine, claim.RolePlanner, "plan", 1)
		completes(t, pool, engine, claim.RoleImplementer, "implement", 1)
		if result := applyFacts(t, pool, engine)("fail-1", failedTest(root.Key, 1, "test-1")); result.Refusal != nil {
			t.Fatalf("the tester's fail naming step 1 was refused: %+v", result.Refusal)
		}
		planner := lastStart(t, pool, root.Key, claim.RolePlanner)
		if !strings.Contains(planner, " Task: a. Step 1 of 3: planner.") {
			t.Fatalf("the planner's task after the return = %q, want step 1", planner)
		}
		lacks(t, "returned planner", planner, "reported:")

		completes(t, pool, engine, claim.RolePlanner, "plan", 2)
		implementer := lastStart(t, pool, root.Key, claim.RoleImplementer)
		if !strings.Contains(implementer, " Task: a. Step 2 of 3: implementer.") ||
			!strings.Contains(implementer, " Steps: 1 planner, 2 implementer, 3 tester then Legion's retro and merge. Step 1 (planner) reported: plan version 2. Handoff: plan version 2 (") {
			t.Fatalf("the implementer's task after the new plan = %q, want step 2 naming the planner's new report", implementer)
		}

		completes(t, pool, engine, claim.RoleImplementer, "implement", 2)
		tester := lastStart(t, pool, root.Key, claim.RoleTester)
		if want := " Steps: 1 planner, 2 implementer, 3 tester then Legion's retro and merge. Step 1 (planner) reported: plan version 2. Step 2 (implementer) reported: implement version 2."; !strings.Contains(tester, " Task: a. Step 3 of 3: tester.") ||
			!strings.Contains(tester, want) {
			t.Fatalf("the tester's task of the second pass = %q, want step 3 holding %q", tester, want)
		}
	})
}

// An issue with no plan rows, which started before plans existed, is told today's task with the
// working copy line after its continue line and no other new line: the prompts read a task
// with no `Task:` line as such an issue. With no runtime to place the working copy (a nil
// Config.WorkspaceDir), the line names the branch alone.
func TestATaskOfAnIssueWithNoPlanRowsIsTodaysTextWithItsWorkingCopy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		engine  *Engine
		working string
	}{
		{"a runtime places the working copy", textEngine(), " Working copy: branch legion/LEGION-208, directory /work/LEGION-208."},
		{"no runtime places it", stepEngine(nil), " Working copy: branch legion/LEGION-208."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			root := stepRoot(t, pool, phase.Implementing)
			completes(t, pool, tc.engine, claim.RoleImplementer, "implement", 1)
			tester := lastStart(t, pool, root.Key, claim.RoleTester)
			if want := "Continue root. Issue: LEGION-208. Phase: testing." + tc.working +
				" Handoff: implement version 1 (dispatch handoff-read --issue LEGION-208 --phase implement --version 1). Handoff commit: head. Pull request: #42."; tester != want {
				t.Fatalf("the tester's task = %q,\nwant %q", tester, want)
			}
			applyInTx(t, pool, tc.engine, func(ctx context.Context, tx pgx.Tx) error {
				issue, err := tc.engine.store.Issue(ctx, tx, root.Key)
				if err != nil {
					return err
				}
				s, err := firstScope(ctx, tx, tc.engine, *issue)
				if err != nil {
					return err
				}
				_, err = tc.engine.StartPromotedWorker(ctx, tx, s, claim.RoleTester, false)
				return err
			})

			if resumed, want := lastStart(t, pool, root.Key, claim.RoleTester), "Continue root. Issue: LEGION-208. Phase: testing."+tc.working+
				" Pull request: #42. Resume the existing phase work."; resumed != want {
				t.Fatalf("the resumed tester's task = %q,\nwant %q", resumed, want)
			}
		})
	}
}

// The agent of a step is told the line of its own task and, at the first step of a task that
// needs other tasks, that the task starts from their work. Of the example plan a to e (a and b need
// nothing, c needs b, d needs a and c, and e, which needs d, is the one task no task needs, so the
// one that merges), d's first step is told it starts from the joined work of a and c, which are
// done, and d's steps line ends with its last step, since d does not merge; d's second step names
// no need. e's first step names its one need in the singular, and its steps line, and its retro's,
// ends with Legion's retro and merge. a, which needs nothing, is told no need and the line of a
// task that does not merge. A step of a plan of one task is told the text it was told before tasks
// had needs. Each composition is the engine's: the walk of the task as Engine.ScopeOf reads it from
// the plan rows, with the task's place set, through standingPart.
//
// The fault it catches is the agent of a task that does not merge told that Legion runs a retro
// and a merge after its last step, or the agent of a first step not told the work its task starts
// from (its control: the steps line that always ends with the retro and the merge).
func TestTheFirstStepOfATaskWithNeedsNamesThem(t *testing.T) {
	told := stepLines
	ends := endsWith

	t.Run("the example plan a to e", func(t *testing.T) {
		pool := migratedPool(t)
		engine := textEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"impl-a", "rev-a"}, state: record.TaskDone, place: atStep(2)},
			planTask{id: "b", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"impl-b", "rev-b"}, state: record.TaskDone, place: atStep(2)},
			planTask{id: "c", steps: []string{"implementer:cd", "reviewer:r"}, agents: []claim.Agent{"cd", "r"}, needs: []string{"b"}, state: record.TaskDone, place: atStep(2)},
			planTask{id: "d", steps: []string{"implementer:cd", "reviewer:r"}, agents: []claim.Agent{"cd", "r"}, needs: []string{"a", "c"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "e", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"d"}, state: record.TaskWaiting})

		ends(t, "first step of d", told(t, pool, engine, root.Key, "d", atStep(1)),
			" Task: d. Step 1 of 2: implementer:cd. Steps: 1 implementer:cd, 2 reviewer:r. This task starts from the joined work of tasks a, c, which are done."+
				" Open the pull request of this task against legion/LEGION-208.join: gh pr create --base legion/LEGION-208.join.")
		second := told(t, pool, engine, root.Key, "d", atStep(2))
		ends(t, "second step of d", second, " Task: d. Step 2 of 2: reviewer:r. Steps: 1 implementer:cd, 2 reviewer:r.")
		lacks(t, "second step of d", second, "starts from")

		ends(t, "first step of e", told(t, pool, engine, root.Key, "e", atStep(1)),
			" Task: e. Step 1 of 1: tester. Steps: 1 tester then Legion's retro and merge. This task starts from the work of task d, which is done.")
		retro := told(t, pool, engine, root.Key, "e", record.TaskPlace{Kind: record.PlaceRetro})
		ends(t, "retro of e", retro, " Task: e. Steps: 1 tester then Legion's retro and merge.")
		lacks(t, "retro of e", retro, "starts from")

		first := told(t, pool, engine, root.Key, "a", atStep(1))
		ends(t, "first step of a", first, " Task: a. Step 1 of 2: implementer. Steps: 1 implementer, 2 reviewer.")
		lacks(t, "first step of a", first, "starts from", "retro")
	})

	t.Run("a plan of one task", func(t *testing.T) {
		pool := migratedPool(t)
		engine := textEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
		if lines, want := told(t, pool, engine, root.Key, "a", atStep(1)),
			" Working copy: branch legion/LEGION-208, directory /work/LEGION-208. Task: a. Step 1 of 2: implementer. Steps: 1 implementer, 2 reviewer then Legion's retro and merge."; lines != want {
			t.Fatalf("the first step's task of a plan of one task = %q,\nwant %q", lines, want)
		}
	})
}

// stepLines is the daemon's lines of the task of the agent of task of key's plan at place, in the
// phase that place derives (issuePhaseOf), as the engine composes them (standingPart) on the scope
// of the task as Engine.ScopeOf reads it, with the task running at place.
func stepLines(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, task string, place record.TaskPlace) string {
	t.Helper()
	var lines string
	onTask(t, pool, engine, key, task, func(ctx context.Context, tx pgx.Tx, s TaskScope) error {
		s.walk.State, s.walk.Place = record.TaskRunning, place
		s.issue.Phase = issuePhaseOf(s.walk, place, record.TaskRunning)
		at, err := engine.standingPart(ctx, tx, s, taskPull{})
		lines = at.lines
		return err
	})
	return lines
}

// endsWith fails the test unless the lines of who's task end with want.
func endsWith(t *testing.T, who, lines, want string) {
	t.Helper()
	if !strings.HasSuffix(lines, want) {
		t.Fatalf("the %s's task = %q,\nwant it to end %q", who, lines, want)
	}
}

// seedBranches gives each of tasks of key's plan its branch as the starts of a plan give them:
// the first task the issue's own branch, each other legion/<KEY>.<id> (seedTaskBranch).
func seedBranches(t *testing.T, pool *pgxpool.Pool, key string, tasks ...string) {
	t.Helper()
	for i, task := range tasks {
		branch := "legion/" + key
		if i > 0 {
			branch += "." + task
		}
		seedTaskBranch(t, pool, key, task, branch)
	}
}

// The first step of a task whose join GitHub refused as a conflict is told to repair it, right
// after the sentence that names the tasks it starts from: of tasks a and c, which are done, d
// needs both, and the join of c into d's join branch conflicted. d's pull request is based on that
// join branch (record.StoredPlan.BaseOf), so d's first step is told that GitHub could not join the
// work of task c into its join branch, the commands that repair the join there and push the join
// branch, and then the command that starts its own work on its branch from the repaired join; d's
// second step is not told it. f has no join branch, since none of its steps implements: its join
// conflicted on two needs, and it is told to join both, each with its branch, on its own branch
// and to push that. A task whose join ended with no conflict is told nothing of a join.
//
// The fault it catches is a conflict the agent of the first step is not told, or told to repair on
// the task's branch where the task has a join branch, so its pull request shows its needs' work as
// its own (its controls: the conflict sentence dropped, under which d's first step names no join
// to repair; the join branch left out of the sentence, under which d's sentence fails).
func TestTheFirstStepOfATaskWhoseJoinConflictedIsToldToRepairIt(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "c", steps: []string{"implementer:c"}, agents: []claim.Agent{"impl-c"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "d", steps: []string{"implementer:d", "reviewer:r"}, agents: []claim.Agent{"impl-d", "rev-d"}, needs: []string{"a", "c"},
			state: record.TaskRunning, place: atStep(1)},
		planTask{id: "f", steps: []string{"tester:f"}, agents: []claim.Agent{"test-f"}, needs: []string{"a", "c"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "g", steps: []string{"tester:g"}, agents: []claim.Agent{"test-g"}, needs: []string{"a", "c"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "z", steps: []string{"implementer:z"}, agents: []claim.Agent{"impl-z"}, needs: []string{"d", "f", "g"}, state: record.TaskWaiting})
	seedBranches(t, pool, root.Key, "a", "c", "d", "f", "g")
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		if err := store.SetJoinConflicts(t.Context(), tx, root.Key, root.Generation, "d", []string{"c"}); err != nil {
			return err
		}
		return store.SetJoinConflicts(t.Context(), tx, root.Key, root.Generation, "f", []string{"a", "c"})
	})

	first := stepLines(t, pool, engine, root.Key, "d", atStep(1))
	const repair = " This task starts from the joined work of tasks a, c, which are done." +
		" GitHub could not join the work of task c into the join branch legion/LEGION-208.d.join, the base of this task's pull request:" +
		" their changes conflict. Before your own work, repair that join on legion/LEGION-208.d.join, so the pull request shows this task's own work alone:" +
		" `jj -R /work/LEGION-208.d git fetch`, `jj -R /work/LEGION-208.d bookmark track legion/LEGION-208.d.join@origin`," +
		" then `jj -R /work/LEGION-208.d new legion/LEGION-208.d.join@origin legion/LEGION-208.c@origin -m \"LEGION-208: join the work of task c\"`," +
		" repair each path that `jj -R /work/LEGION-208.d resolve --list` lists, then `jj -R /work/LEGION-208.d bookmark set legion/LEGION-208.d.join -r @`" +
		" and `jj -R /work/LEGION-208.d git push --bookmark legion/LEGION-208.d.join`." +
		" Then start your own work on the branch legion/LEGION-208.d from the repaired join:" +
		" `jj -R /work/LEGION-208.d new 'heads(legion/LEGION-208.d@origin | legion/LEGION-208.d.join@origin)'`," +
		" and repair each path that `jj -R /work/LEGION-208.d resolve --list` lists there; your first commit on legion/LEGION-208.d is from that join."
	if !strings.Contains(first, repair) {
		t.Fatalf("the first step of d's task = %q,\nwant it to hold %q", first, repair)
	}
	lacks(t, "second step of d", stepLines(t, pool, engine, root.Key, "d", atStep(2)), "GitHub could not join")

	const both = " GitHub could not join the work of task a, task c into this branch: their changes conflict." +
		" Before your own work, join it in your working copy: `jj -R /work/LEGION-208.f git fetch`," +
		" then `jj -R /work/LEGION-208.f new @- legion/LEGION-208@origin legion/LEGION-208.c@origin -m \"LEGION-208: join the work of task a, task c\"`," +
		" repair each path that `jj -R /work/LEGION-208.f resolve --list` lists, then `jj -R /work/LEGION-208.f bookmark set legion/LEGION-208.f -r @`" +
		" and `jj -R /work/LEGION-208.f git push --bookmark legion/LEGION-208.f`. That join is the first commit of your step:" +
		" start your own work after it with `jj -R /work/LEGION-208.f new`."
	if f := stepLines(t, pool, engine, root.Key, "f", atStep(1)); !strings.Contains(f, both) {
		t.Fatalf("the first step of f's task = %q,\nwant it to hold %q", f, both)
	}
	lacks(t, "first step of g", stepLines(t, pool, engine, root.Key, "g", atStep(1)), "GitHub could not join")
}

// The first step of a task whose join branch GitHub could not merge into the task's branch, since
// the task's own work there conflicts with what its needs did since, is told to merge it there, on
// the task's branch, and the join branch takes none of that repair: of tasks a and c, which are
// done, h and k each need both. h's join branch did not merge into its branch, and h is told the
// commands of that merge and the branch to push. k's join conflicted on c too, and its join branch
// did not merge: it is told first to repair the join of c on its join branch, then to merge the
// repaired join branch into its branch. A task whose join branch merged is told no such merge.
//
// The fault it catches is that conflict told as a conflict of the needs on the join branch, which
// holds their work already (its control: the unmerged join left out of the task's text, under which
// h's first step names no merge).
func TestTheFirstStepOfATaskWhoseJoinBranchDidNotMergeIsToldToMergeIt(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "c", steps: []string{"implementer:c"}, agents: []claim.Agent{"impl-c"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "h", steps: []string{"implementer:h"}, agents: []claim.Agent{"impl-h"}, needs: []string{"a", "c"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "k", steps: []string{"implementer:k"}, agents: []claim.Agent{"impl-k"}, needs: []string{"a", "c"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "z", steps: []string{"implementer:z"}, agents: []claim.Agent{"impl-z"}, needs: []string{"h", "k"}, state: record.TaskWaiting})
	seedBranches(t, pool, root.Key, "a", "c", "h", "k")
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		if err := store.SetJoinUnmerged(t.Context(), tx, root.Key, root.Generation, "h", true); err != nil {
			return err
		}
		if err := store.SetJoinConflicts(t.Context(), tx, root.Key, root.Generation, "k", []string{"c"}); err != nil {
			return err
		}
		return store.SetJoinUnmerged(t.Context(), tx, root.Key, root.Generation, "k", true)
	})

	const merge = " This task starts from the joined work of tasks a, c, which are done." +
		" GitHub could not merge the join branch legion/LEGION-208.h.join into this branch: its changes conflict with this task's own work." +
		" Before your own work, merge it here: `jj -R /work/LEGION-208.h git fetch`," +
		" then `jj -R /work/LEGION-208.h new legion/LEGION-208.h@origin legion/LEGION-208.h.join@origin -m \"LEGION-208: join the work of tasks a, c\"`," +
		" repair each path that `jj -R /work/LEGION-208.h resolve --list` lists, then `jj -R /work/LEGION-208.h bookmark set legion/LEGION-208.h -r @`" +
		" and `jj -R /work/LEGION-208.h git push --bookmark legion/LEGION-208.h`." +
		" That merge is the first commit of your step, and the join branch takes none of it: start your own work after it with `jj -R /work/LEGION-208.h new`."
	if h := stepLines(t, pool, engine, root.Key, "h", atStep(1)); !strings.Contains(h, merge) {
		t.Fatalf("the first step of h's task = %q,\nwant it to hold %q", h, merge)
	}

	const repairThenMerge = " GitHub could not join the work of task c into the join branch legion/LEGION-208.k.join, the base of this task's pull request:" +
		" their changes conflict. Before your own work, repair that join on legion/LEGION-208.k.join, so the pull request shows this task's own work alone:" +
		" `jj -R /work/LEGION-208.k git fetch`, `jj -R /work/LEGION-208.k bookmark track legion/LEGION-208.k.join@origin`," +
		" then `jj -R /work/LEGION-208.k new legion/LEGION-208.k.join@origin legion/LEGION-208.c@origin -m \"LEGION-208: join the work of task c\"`," +
		" repair each path that `jj -R /work/LEGION-208.k resolve --list` lists, then `jj -R /work/LEGION-208.k bookmark set legion/LEGION-208.k.join -r @`" +
		" and `jj -R /work/LEGION-208.k git push --bookmark legion/LEGION-208.k.join`." +
		" Then merge the repaired join into this branch, which GitHub could not do: its changes conflict with this task's own work:" +
		" `jj -R /work/LEGION-208.k new legion/LEGION-208.k@origin legion/LEGION-208.k.join@origin -m \"LEGION-208: join the work of tasks a, c\"`," +
		" repair each path that `jj -R /work/LEGION-208.k resolve --list` lists, then `jj -R /work/LEGION-208.k bookmark set legion/LEGION-208.k -r @`" +
		" and `jj -R /work/LEGION-208.k git push --bookmark legion/LEGION-208.k`." +
		" That merge is the first commit of your step, and the join branch takes none of it: start your own work after it with `jj -R /work/LEGION-208.k new`."
	if k := stepLines(t, pool, engine, root.Key, "k", atStep(1)); !strings.Contains(k, repairThenMerge) {
		t.Fatalf("the first step of k's task = %q,\nwant it to hold %q", k, repairThenMerge)
	}
	lacks(t, "first step of d", stepLines(t, pool, engine, root.Key, "d", atStep(1)), "GitHub could not")
}

// An implementation step of a task whose pull request has a base other than the issue's default
// branch (record.StoredPlan.BaseOf) is told that base, and the command that opens the pull request
// against it: of a and b, which are done, c needs b, d needs a and c, and z, which needs d, is the
// task that merges. c's implementer is told b's branch, and d's implementer d's join branch, while
// c's reviewer is told no base. a, which needs nothing, z, the task that merges, and a step of a
// plan of one task are told none.
//
// The fault it catches is an implementer that opens its pull request against the default branch,
// so the review of its task reads the work of its needs (its control: the base sentence dropped,
// under which c's implementer is told no base).
func TestAnImplementationStepNamesItsPullRequestsBase(t *testing.T) {
	const opens = "Open the pull request of this task against"
	t.Run("a plan of several tasks", func(t *testing.T) {
		pool := migratedPool(t)
		engine := textEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, state: record.TaskDone, place: atStep(1)},
			planTask{id: "c", steps: []string{"implementer:c", "reviewer:r"}, agents: []claim.Agent{"impl-c", "rev-c"}, needs: []string{"b"},
				state: record.TaskRunning, place: atStep(1)},
			planTask{id: "d", steps: []string{"implementer:d"}, agents: []claim.Agent{"impl-d"}, needs: []string{"a", "c"}, state: record.TaskWaiting},
			planTask{id: "z", steps: []string{"implementer:z"}, agents: []claim.Agent{"impl-z"}, needs: []string{"d"}, state: record.TaskWaiting})
		seedBranches(t, pool, root.Key, "a", "b", "c", "d", "z")

		endsWith(t, "implementer of c", stepLines(t, pool, engine, root.Key, "c", atStep(1)),
			" Open the pull request of this task against legion/LEGION-208.b: gh pr create --base legion/LEGION-208.b.")
		lacks(t, "reviewer of c", stepLines(t, pool, engine, root.Key, "c", atStep(2)), opens)
		endsWith(t, "implementer of d", stepLines(t, pool, engine, root.Key, "d", atStep(1)),
			" Open the pull request of this task against legion/LEGION-208.d.join: gh pr create --base legion/LEGION-208.d.join.")
		lacks(t, "implementer of a", stepLines(t, pool, engine, root.Key, "a", atStep(1)), opens)
		lacks(t, "implementer of z", stepLines(t, pool, engine, root.Key, "z", atStep(1)), opens)
	})

	t.Run("a plan of one task", func(t *testing.T) {
		pool := migratedPool(t)
		engine := textEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
		lacks(t, "implementer of a", stepLines(t, pool, engine, root.Key, "a", atStep(1)), opens)
	})
}

// A step's task carries its phase, working copy, task, steps and reports. Retro and merge
// carry the task and its steps without a current step, owner text or earlier report.
func TestATaskWritesItsLabelsAndStepValues(t *testing.T) {
	t.Parallel()
	issue := record.Issue{Key: "LEGION-208", Title: "root", Phase: phase.Reviewing}
	steps := []record.PlanStep{{Task: "a", Position: 1, Role: "implementer"}, {Task: "a", Position: 2, Role: "reviewer"}}
	w := Walk{Stored: true, Task: "a", Branch: "legion/LEGION-208", Text: "Build the widget.", Steps: steps,
		Line:  []phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging},
		State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 2}}
	rows := []record.PhaseRow{{Role: claim.RoleImplementer, HandoffRecord: "implement-1", HandoffVersion: 1}}
	at, err := standingText(issue, func(record.Issue, string) string { return "/work" }, w, rows, planTold{}, &record.PullRequest{Number: 42})
	if err != nil {
		t.Fatal(err)
	}
	tester := record.PhaseRow{Role: claim.RoleTester, HandoffRecord: "test-1", HandoffVersion: 1, Verdict: "pass"}
	written := task(issue, at, tester, "the reviewer asked again")

	for _, label := range []string{"Phase", "Working copy", "Task", "Steps", "Task text", "reported", "Handoff", "Pull request", "Reason"} {
		if !strings.Contains(written, " "+label+": ") {
			t.Errorf("the task %q writes no %q line", written, label)
		}
	}

	for template, values := range map[string][]string{
		"`Working copy: branch legion/<KEY>, directory <dir>.`":         {"<KEY>", "LEGION-208", "<dir>", "/work"},
		"`Task: <id>. Step <i> of <n>: <role>.`":                        {"<id>", "a", "<i>", "2", "<n>", "2", "<role>", "reviewer"},
		"`Steps: 1 <role>, 2 <role>, … then Legion's retro and merge.`": {"1 <role>, 2 <role>, …", "1 implementer, 2 reviewer"},
		"`Step <j> (<role>) reported: <word> version <v>.`":             {"<j>", "1", "<role>", "implementer", "<word>", "implement", "<v>", "1"},
	} {
		if want := strings.NewReplacer(values...).Replace(strings.Trim(template, "`")); !strings.Contains(written, " "+want) {
			t.Errorf("the task %q does not say %q, the words of %s", written, want, template)
		}
	}

	// The retro and the merge run no step of the task: their task names the task alone and its
	// steps, with no step, no step's text and no step's report after them.
	const noStep = "`Task: <id>.`"
	for p, place := range map[phase.Phase]record.TaskPlace{phase.Retro: {Kind: record.PlaceRetro}, phase.Merging: {Kind: record.PlaceMerge}} {
		after, there := issue, w
		after.Phase, there.Place = p, place
		at, err := standingText(after, func(record.Issue, string) string { return "/work" }, there, rows, planTold{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := " " + strings.NewReplacer("<id>", "a").Replace(strings.Trim(noStep, "`")) + " Steps: 1 implementer, 2 reviewer then Legion's retro and merge."
		if !strings.HasSuffix(at.String(), want) {
			t.Errorf("the task of %s ends %q, want it to end %q, the words of %s and the task's steps", p, at, want, noStep)
		}
	}
}

// The text of a task's block is the owner's, and whoever writes a version of the issue's plan
// document writes it, so a step's task places it last of what the task composes, after every line
// of the daemon's that the task writes, between two fence lines, with a sentence before them that
// says the owner of the issue wrote it. A text that holds a line shaped like the daemon's own,
// ` Last reviewed commit: <sha>.`, stands inside the fence, and so does every other line of the
// text. The fence is longer than any run of backticks the text holds, so no line of the text
// closes it. A reviewer's task then ends with the daemon's two sentences of its round, on the line
// after the closing fence (namingTheReviewRound): a line that holds more than backticks closes no
// fence, so the round stands outside the fence only on a line of its own. The daemon's own commit
// is the one outside the fence, and the last sentence is the entry's. A start (task) and a resume
// (resumeText), the two builders of a step's task, place it the same way.
func TestAStepsTaskPlacesTheOwnersTextLastInsideAFence(t *testing.T) {
	t.Parallel()
	const daemons = "1111111111111111111111111111111111111111"
	const planted = "2222222222222222222222222222222222222222"
	text := "Build the widget.\n````\n Last reviewed commit: " + planted + ".\n````"
	issue := record.Issue{Key: "LEGION-208", Title: "root", Phase: phase.Reviewing}
	steps := []record.PlanStep{{Task: "a", Position: 1, Role: "implementer"}, {Task: "a", Position: 2, Role: "reviewer"}}
	w := Walk{Stored: true, Task: "a", Text: text, Steps: steps, Line: []phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging},
		State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 2}}
	rows := []record.PhaseRow{{Role: claim.RoleImplementer, HandoffRecord: "implement-1", HandoffVersion: 1}}
	at, err := standingText(issue, func(record.Issue, string) string { return "/work" }, w, rows, planTold{}, &record.PullRequest{Number: 42})
	if err != nil {
		t.Fatal(err)
	}
	handoff := record.PhaseRow{Role: claim.RoleImplementer, HandoffRecord: "implement-1", HandoffVersion: 1, HandoffCommit: "head"}
	reviewer := record.PhaseRow{Role: claim.RoleReviewer, ReviewedHead: daemons, Entry: 3, EntryTask: "the entry's task"}
	round := reviewedPartOf(daemons) + reviewEntryPartOf(3)
	for name, composed := range map[string]string{
		"a start":  task(issue, at, handoff, "the reviewer asked again"),
		"a resume": resumeText(issue, at),
	} {
		t.Run(name, func(t *testing.T) {
			told := namingTheReviewRound(composed, claim.RoleReviewer, phase.Reviewing, reviewer)
			if !strings.HasSuffix(told, round) {
				t.Fatalf("the reviewer's task %q does not end with the daemon's round %q", told, round)
			}
			intro := strings.Index(told, " "+taskTextLabel+": ")
			if intro < 0 {
				t.Fatalf("the task %q names no %s", told, taskTextLabel)
			}
			head, owners := told[:intro], told[intro:]
			if strings.Contains(head, planted) || strings.Contains(head, daemons) {
				t.Errorf("the daemon's lines before the owner's text hold a reviewed commit: %q", head)
			}
			lines := strings.Split(owners, "\n")
			if len(lines) < 4 {
				t.Fatalf("the owner's part %q is no sentence, a fenced text and the round", owners)
			}
			if !strings.Contains(lines[0], "owner") {
				t.Errorf("the sentence before the fence, %q, does not say the owner wrote the text", lines[0])
			}
			// The text's longest run of backticks is four, so a fence of five or more is one no
			// line of the text closes.
			fence := lines[1]
			if strings.Trim(fence, "`") != "" || len(fence) < 5 {
				t.Errorf("the fence %q is not a line of backticks longer than the text's own ````", fence)
			}
			if closing, last := lines[len(lines)-2], lines[len(lines)-1]; closing != fence || last != round {
				t.Errorf("the reviewer's task ends with the lines %q and %q, want the fence %q that closes the owner's text, then the daemon's round %q on a line of its own",
					closing, last, fence, round)
			}
			if inside := strings.Join(lines[2:len(lines)-2], "\n"); inside != text {
				t.Errorf("the fence holds %q, want the task's text %q whole", inside, text)
			}
		})
	}
}
