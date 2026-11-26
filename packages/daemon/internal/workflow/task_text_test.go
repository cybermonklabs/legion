package workflow

import (
	"context"
	"reflect"
	"slices"
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
// review round follow: the reviewer of the task `implementer reviewer` reads the implementer's
// report. The retro and the merger, which run no step, are told the task and its steps, and read
// the runs of the steps from Dispatch. The retro is told the task's text last too, as a step is,
// and the merger is told none.
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

	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
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
		_, err = engine.StartPromotedWorker(ctx, tx, *issue, claim.RoleReviewer, false)
		return err
	})

	resumed := daemonsLines(t, "resumed reviewer", withoutRoundsSentences(t, pool, lastStart(t, pool, root.Key, claim.RoleReviewer)), text)
	if want := continued + "reviewing." + working + " Task: a. Step 2 of 2: reviewer." + steps +
		" Step 1 (implementer) reported: implement version 1. Resume the existing phase work."; resumed != want {
		t.Fatalf("the resumed reviewer's task before the owner's text = %q,\nwant %q", resumed, want)
	}

	move(t, pool, engine, root.Key, TriggerReviewApproved, "", record.PhaseRow{})
	retro := daemonsLines(t, "retro", lastStart(t, pool, root.Key, claim.RoleImplementer), text)
	hasPrefix(t, "retro", retro, continued+"retro."+working+" Task: a."+steps)
	lacks(t, "retro", retro, "reported:", "Step 1 of", "Step 2 of")

	move(t, pool, engine, root.Key, TriggerRetroCompleted, "", record.PhaseRow{})
	merger := lastStart(t, pool, root.Key, claim.RoleMerger)
	hasPrefix(t, "merger", merger, continued+"merging."+working+" Task: a."+steps)
	lacks(t, "merger", merger, "reported:", "Task text:", "Step 1 of", "Step 2 of")
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
	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerRetroCompleted, "", record.PhaseRow{})
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
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
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
		move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
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
				_, err = tc.engine.StartPromotedWorker(ctx, tx, *issue, claim.RoleTester, false)
				return err
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
	t.Parallel()
	issue := record.Issue{Key: "LEGION-208", Title: "root", Phase: phase.Reviewing}
	steps := []record.PlanStep{{Task: "a", Position: 1, Role: "implementer"}, {Task: "a", Position: 2, Role: "reviewer"}}
	w := Walk{Stored: true, Task: "a", Text: "Build the widget.", Steps: steps, Line: []phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging}}
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
	// steps, with no step and no step's report after them. The retro's then ends with the task's
	// text, as a step's does, and the merger's with its steps.
	const noStep = "`Task: <id>.`"
	for p, owners := range map[phase.Phase]string{phase.Retro: ownersText(w.Text), phase.Merging: ""} {
		after := issue
		after.Phase = p
		at, err := standingText(after, func(record.Issue) string { return "/work" }, w, rows)
		if err != nil {
			t.Fatal(err)
		}
		want := " " + strings.NewReplacer("<id>", "a").Replace(strings.Trim(noStep, "`")) + " Steps: 1 implementer, 2 reviewer then Legion's retro and merge."
		if !strings.HasSuffix(at.lines, want) || at.owners != owners {
			t.Errorf("the task of %s is %q then %q, want its lines to end %q, the words of %s and the task's steps, then %q", p, at.lines, at.owners, want, noStep, owners)
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
	w := Walk{Stored: true, Task: "a", Text: text, Steps: steps, Line: []phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging}}
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

// A start's task a store keeps takes a later version's text of its task in place of the owner's
// part it ends with, and nothing else of it moves (WithTaskText): the daemon's lines before it, a
// reason that quotes the sentence that opens the owner's part and a fence as long as the owner's,
// and a reviewer's round after it stay as they were, so the task is the one the start would compose
// with the later text. A start, a resume and the retro's task each take it. A task that holds the
// later text already is answered as it is, and so is a task with no owner's part: the merger's,
// and one of an issue with no plan rows.
func TestAStoredTaskTakesALaterTextInPlaceOfItsOwnersPart(t *testing.T) {
	t.Parallel()
	const was = "Build the widget.\n````\nThe owner's own fence.\n````"
	const now = "Build the widget, and its manual."
	dir := func(record.Issue) string { return "/work" }
	steps := []record.PlanStep{{Task: "a", Position: 1, Role: "implementer"}, {Task: "a", Position: 2, Role: "reviewer"}}
	walk := func(text string) Walk {
		return Walk{Stored: true, Task: "a", Text: text, Steps: steps, Line: []phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging}}
	}
	rows := []record.PhaseRow{{Role: claim.RoleImplementer, HandoffRecord: "implement-1", HandoffVersion: 1}}
	handoff := record.PhaseRow{Role: claim.RoleImplementer, HandoffRecord: "implement-1", HandoffVersion: 1, HandoffCommit: "head"}
	reviewer := record.PhaseRow{Role: claim.RoleReviewer, ReviewedHead: "1111111111111111111111111111111111111111", Entry: 3, EntryTask: "the entry's task"}
	// The fence of was's text is five backticks long, and the reason quotes the opening sentence
	// before a fence of the same length, which only the owner's own part may be read as.
	reason := "the owner quoted" + ownersOpening + "`````\nnot the owner's part\n`````\nin its reason"
	composed := func(p phase.Phase, text string, compose func(record.Issue, Standing) string) string {
		t.Helper()
		issue := record.Issue{Key: "LEGION-208", Title: "root", Phase: p}
		at, err := standingText(issue, dir, walk(text), rows)
		if err != nil {
			t.Fatal(err)
		}
		return namingTheReviewRound(compose(issue, at), RoleFor(p), p, reviewer)
	}
	start := func(issue record.Issue, at Standing) string {
		return task(issue, at, handoff, &record.PullRequest{Number: 42}, reason)
	}
	for _, tc := range []struct {
		name    string
		at      phase.Phase
		compose func(record.Issue, Standing) string
		retold  bool
	}{
		{"a reviewer's start", phase.Reviewing, start, true},
		{"an implementer's resume", phase.Implementing, resumeText, true},
		{"the retro's start", phase.Retro, start, true},
		{"the merger's start", phase.Merging, start, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := composed(tc.at, was, tc.compose), composed(tc.at, now, tc.compose)
			if tc.retold == (before == after) {
				t.Fatalf("the task composed with each text = %q and %q, want them to differ %v", before, after, tc.retold)
			}
			if got := WithTaskText(before, now); got != after || HoldsTaskText(before) != tc.retold {
				t.Fatalf("WithTaskText(%q) = %q, holding an owner's part %v,\nwant the task composed with the later text %q, %v", before, got, HoldsTaskText(before), after, tc.retold)
			}
			if got := WithTaskText(after, now); got != after {
				t.Fatalf("WithTaskText of the task that holds the later text = %q, want it as it is", got)
			}
		})
	}
	// A task a release before stored opens its owner's part with other words: ownersPart reads the
	// part by its label and its fence, so the task takes the later text as well, and the rewrite
	// writes today's opening. The fault is a read by the words of today's opening, which parses no
	// such task, so a later version's text never reaches it.
	reviewing := composed(phase.Reviewing, was, start)
	at := strings.LastIndex(reviewing, ownersOpening)
	const earlier = " " + taskTextLabel + ": the text of the task's block, in words a release before wrote, between the two fence lines below.\n"
	stored := reviewing[:at] + earlier + reviewing[at+len(ownersOpening):]
	if got, want := WithTaskText(stored, now), composed(phase.Reviewing, now, start); !HoldsTaskText(stored) || got != want {
		t.Fatalf("WithTaskText of a task stored with an earlier opening = %q, holding an owner's part %v,\nwant %q, true", got, HoldsTaskText(stored), want)
	}
	// An operator's task that ends with a fenced text of its own, with no label on the line before
	// it, holds no owner's part: the label, not the fence alone, says where the part starts.
	unlabelled := reviewing[:at] + " Here is the log.\n" + reviewing[at+len(ownersOpening):]
	if got := WithTaskText(unlabelled, now); got != unlabelled || HoldsTaskText(unlabelled) {
		t.Fatalf("WithTaskText of a task whose fenced text has no label = %q, holding an owner's part %v, want it whole, false", got, HoldsTaskText(unlabelled))
	}
	unplanned := record.Issue{Key: "LEGION-208", Title: "root", Phase: phase.Implementing}
	today := task(unplanned, WorkingCopy(unplanned, dir), handoff, nil, reason)
	if got := WithTaskText(today, now); got != today || HoldsTaskText(today) {
		t.Fatalf("WithTaskText of the task of an issue with no plan rows = %q, holding an owner's part %v, want it whole, false", got, HoldsTaskText(today))
	}
}

// A later version that changes only the text of a running task is taken (plan.RunningChange): the
// task goes on at its step on its steps with its counts, the issue does not move, the one row
// queued is the run's plan node, which sends the running agent nothing, and nothing else is
// written for the running task: the stored start task of the implementer's entry is as it was, and
// its send rewrites it with the new text (WithTaskText). The plan in force names step 1, the
// implementer, as the act that runs (record.StoredPlan.Running), and the log of the version names
// a as a task whose text alone it changed.
func TestALaterVersionChangesTheTextOfARunningTaskAlone(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine, logged := loggedEngine()
	apply := applyFacts(t, pool, engine)
	root := stepRoot(t, pool, phase.Admitted)
	key, gen := root.Key, root.Generation
	mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "tester", "reviewer"))))
	move(t, pool, engine, key, TriggerGateOpened, "", record.PhaseRow{})

	before := planStandingOf(t, pool, engine, key, gen)
	started := roleRowOf(t, pool, engine, key, claim.RoleImplementer)
	if !strings.Contains(started.EntryTask, "\nBuild the widget.\n") {
		t.Fatalf("the implementer's stored start task %q holds no text of version 1", started.EntryTask)
	}
	manual := givenTask(t, "Build the widget, and its manual.", "implementer", "tester", "reviewer")
	mustTake(t, "version 2 with a's text alone changed", apply(planID(key, gen, 2), ownersPlan(t, key, gen, 2, manual)))
	if line := loggedLine(logged.String(), "workflow: an owner's plan is taken", "version=2"); !strings.Contains(line, "changed=[] text_only=[a]") {
		t.Fatalf("the log of version 2 = %q, want it to name no changed task and a as a task whose text alone changed", line)
	}
	kept := planStandingOf(t, pool, engine, key, gen)
	implementing := []record.RunningStep{{Task: "a", Place: record.TaskPlace{Kind: record.PlaceStep, Position: 1}, Role: claim.RoleImplementer}}
	if running, err := kept.plan.Running(); err != nil || !reflect.DeepEqual(running, implementing) {
		t.Fatalf("the plan after version 2 names the running acts %+v, %v; want %+v: step 1, the implementer", running, err, implementing)
	}
	if !reflect.DeepEqual(kept.versions, []string{"1 owner", "2 owner"}) {
		t.Fatalf("the versions after version 2 = %v, want 1 and 2", kept.versions)
	}
	if task, was := kept.task(t), before.task(t); task.State != record.TaskRunning || task.Place != was.Place || task.BlockText != manual.Text ||
		!reflect.DeepEqual(kept.plan.Steps, before.plan.Steps) || !sameReturnCounts(task.Counts, was.Counts) {
		t.Fatalf("task a after version 2 = %+v, want it running at %+v on its steps with version 2's text, its counts as they were", task, was.Place)
	}
	if !reflect.DeepEqual(kept.issue, before.issue) || kept.outbox != before.outbox+1 {
		t.Fatalf("version 2 moved %s or queued %d outbox rows, want no move and one row, the plan node", key, kept.outbox-before.outbox)
	}
	// Nothing is written for the running task: its stored start task is the one version 1's start
	// wrote. WithTaskText, the rewrite the send-time read of a held task makes (supervise's
	// Deps.TaskText), gives that task version 2's text and not version 1's; the send itself is
	// tested through the whole daemon (running_task_text_test.go, the sent stored implementer).
	if kept := roleRowOf(t, pool, engine, key, claim.RoleImplementer); kept.Entry != started.Entry || kept.EntryTask != started.EntryTask {
		t.Fatalf("the implementer's stored start task after version 2 = entry %d %q, want entry %d as version 1's start wrote it", kept.Entry, kept.EntryTask, started.Entry)
	}
	if sent := WithTaskText(started.EntryTask, manual.Text); !strings.Contains(sent, "\nBuild the widget, and its manual.\n") || strings.Contains(sent, "\nBuild the widget.\n") {
		t.Fatalf("the send's rewrite of the implementer's stored start task = %q, want version 2's text and not version 1's", sent)
	}
}

// loggedLine is the line of logged that holds each of parts, "" where none does.
func loggedLine(logged string, parts ...string) string {
	for _, line := range strings.Split(logged, "\n") {
		if !slices.ContainsFunc(parts, func(part string) bool { return !strings.Contains(line, part) }) {
			return line
		}
	}
	return ""
}
