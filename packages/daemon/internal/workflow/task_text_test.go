package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// textEngine is stepEngine whose runtime places an issue's working copy at /work/<KEY>
// (Config.WorkspaceDir).
func textEngine() *Engine {
	engine := stepEngine(nil)
	engine.cfg.WorkspaceDir = func(issue record.Issue) string { return "/work/" + issue.Key }
	return engine
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
// version n, through the completion route (intake.ApplyFact), which writes the role's phases row,
// under the claim of the step's worker (workerOr), as completesAs applies it.
func completes(t *testing.T, pool *pgxpool.Pool, engine *Engine, role claim.Role, word string, n int) {
	t.Helper()
	completesAs(t, pool, engine, role, workerOr(t, pool, "LEGION-208", role, claim.Token(string(role)+"-claim")), word, n, "head")
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
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
	const continued = "Continue root. Issue: LEGION-208. Phase: "
	const working = " Working copy: branch legion/LEGION-208, directory /work/LEGION-208."
	steps := stepsLine([]string{"implementer", "reviewer"})
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
		return engine.StartPromotedWorker(ctx, tx, *issue, claim.RoleReviewer)
	})

	resumed := daemonsLines(t, "resumed reviewer", withoutRoundsSentences(t, pool, lastStart(t, pool, root.Key, claim.RoleReviewer)), text)
	if want := continued + "reviewing." + working + " Task: a. Step 2 of 2: reviewer." + steps +
		" Step 1 (implementer) reported: implement version 1. Resume the existing phase work."; resumed != want {
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

// A move into a phase that starts no worker composes no task, since no worker is told one: the
// merge of an issue with plan rows closes it, as the merge of an issue with no plan rows does,
// though done holds no place in its task's line, which a start's task must name (standingPart).
func TestAMoveIntoAPhaseThatStartsNoWorkerComposesNoTask(t *testing.T) {
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
		if result := applyFacts(t, pool, engine)("fail-1", failedTest(t, pool, root.Key, 1, "test-1")); result.Refusal != nil {
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
			!strings.Contains(implementer, stepsLine([]string{"planner", "implementer", "tester"})+" Step 1 (planner) reported: plan version 2. Handoff: plan version 2 (") {
			t.Fatalf("the implementer's task after the new plan = %q, want step 2 naming the planner's new report", implementer)
		}

		completes(t, pool, engine, claim.RoleImplementer, "implement", 2)
		tester := lastStart(t, pool, root.Key, claim.RoleTester)
		if want := stepsLine([]string{"planner", "implementer", "tester"}) + " Step 1 (planner) reported: plan version 2. Step 2 (implementer) reported: implement version 2."; !strings.Contains(tester, " Task: a. Step 3 of 3: tester.") ||
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
				return tc.engine.StartPromotedWorker(ctx, tx, *issue, claim.RoleTester)
			})

			if resumed, want := lastStart(t, pool, root.Key, claim.RoleTester), "Continue root. Issue: LEGION-208. Phase: testing."+tc.working+
				" Resume the existing phase work."; resumed != want {
				t.Fatalf("the resumed tester's task = %q,\nwant %q", resumed, want)
			}
		})
	}
}

// The task writes a line of each label by the label's exact text, which the prompts and the skills
// name to a worker, so a change of one label's constant alone fails here. The forms of its lines,
// filled with a step's values, are the task's words exactly, and the task of the retro and of the
// merger, which runs no step, ends with the task alone and its steps.
func TestEachTaskLabelIsALineTheTaskWrites(t *testing.T) {
	issue := record.Issue{Key: "LEGION-208", Title: "root", Phase: phase.Reviewing}
	steps := []record.PlanStep{{Task: "a", Position: 1, Role: "implementer", AgentKey: "a"}, {Task: "a", Position: 2, Role: "reviewer"}}
	w := Walk{Stored: true, Task: "a", Text: "Build the widget.", Steps: steps, Line: []phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging},
		State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 2}}
	rows := []record.PhaseRow{{Role: claim.RoleImplementer, HandoffRecord: "implement-1", HandoffVersion: 1}}
	at, err := standingText(issue, func(record.Issue) string { return "/work" }, w, rows)
	if err != nil {
		t.Fatal(err)
	}
	tester := record.PhaseRow{Role: claim.RoleTester, HandoffRecord: "test-1", HandoffVersion: 1, Verdict: "pass"}
	written := task(issue, at, tester, &record.PullRequest{Number: 42}, "the reviewer asked again")

	for _, label := range []string{"Phase", "Working copy", "Task", "Steps", "Task text", "reported", "Handoff", "Pull request", "Reason"} {
		if !strings.Contains(written, " "+label+": ") {
			t.Errorf("the task %q writes no %q line", written, label)
		}
	}

	for template, values := range map[string][]string{
		"`Working copy: branch legion/<KEY>, directory <dir>.`": {"<KEY>", "LEGION-208", "<dir>", "/work"},
		"`Task: <id>. Step <i> of <n>: <step>.`":                {"<id>", "a", "<i>", "2", "<n>", "2", "<step>", "reviewer"},
		"`Step <j> (<step>) reported: <word> version <v>.`":     {"<j>", "1", "<step>", "implementer:a", "<word>", "implement", "<v>", "1"},
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
		at, err := standingText(after, func(record.Issue) string { return "/work" }, there, rows)
		if err != nil {
			t.Fatal(err)
		}
		want := " " + strings.NewReplacer("<id>", "a").Replace(strings.Trim(noStep, "`")) + stepsLine([]string{"implementer:a", "reviewer"})
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
	const daemons = "1111111111111111111111111111111111111111"
	const planted = "2222222222222222222222222222222222222222"
	text := "Build the widget.\n````\n Last reviewed commit: " + planted + ".\n````"
	issue := record.Issue{Key: "LEGION-208", Title: "root", Phase: phase.Reviewing}
	steps := []record.PlanStep{{Task: "a", Position: 1, Role: "implementer"}, {Task: "a", Position: 2, Role: "reviewer"}}
	w := Walk{Stored: true, Task: "a", Text: text, Steps: steps, Line: []phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging},
		State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 2}}
	rows := []record.PhaseRow{{Role: claim.RoleImplementer, HandoffRecord: "implement-1", HandoffVersion: 1}}
	at, err := standingText(issue, func(record.Issue) string { return "/work" }, w, rows)
	if err != nil {
		t.Fatal(err)
	}
	handoff := record.PhaseRow{Role: claim.RoleImplementer, HandoffRecord: "implement-1", HandoffVersion: 1, HandoffCommit: "head"}
	reviewer := record.PhaseRow{Role: claim.RoleReviewer, ReviewedHead: daemons, Entry: 3, EntryTask: "the entry's task"}
	round := reviewedPartOf(daemons) + reviewEntryPartOf(3)
	for name, composed := range map[string]string{
		"a start":  task(issue, at, handoff, &record.PullRequest{Number: 42}, "the reviewer asked again"),
		"a resume": resumeText(issue, at),
	} {
		t.Run(name, func(t *testing.T) {
			told := namingTheReviewRound(composed, claim.RoleReviewer, phase.Reviewing, reviewer, nil)
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
