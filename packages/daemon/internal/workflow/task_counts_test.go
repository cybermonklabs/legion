package workflow

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The returns of a task are counted on that task (record.Store.ReturnCounts), so what the counts
// tell names the task: the architect's notice at review_round_cap and each prompt a work-back hook
// sends start `task <id>: `, since a run with plan rows can hold more than one task and the owner
// must know which one went round. A run with no plan rows has no task to name, and its notice and
// prompts read as they did before plans (the twin below).
//
// The fault this holds: a notice or a prompt that names no task, which with two tasks at one time
// cannot say whose review went round. Its control is the code before this change, under which the
// notice reads `Issue reached review_round_cap=3: …` and the prompt is the hook's own text alone.
func TestTheCapNoticeAndTheHookPromptNameTheTask(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester", "reviewer")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	for range 3 {
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerTesterPassed, record.TaskPlace{}, record.PhaseRow{})
		reviewRequestsChanges(t, pool, engine, root.Key)
	}
	testsFail(t, pool, engine, root.Key, 2)

	want := "task a: Issue reached review_round_cap=3: 3 rounds counted, 0 conflict rounds not counted."
	if blocked := noticesOf(t, pool, root.Key, "pr-blocked"); len(blocked) != 1 || blocked[0].Reason != want {
		t.Fatalf("the pr-blocked notices = %+v, want one whose reason is %q", blocked, want)
	}
	prompts := hookPrompts(t, pool)
	if len(prompts) == 0 {
		t.Fatal("the hook sent no prompt at the 5th return: this proves nothing of what a prompt names")
	}
	for _, prompt := range prompts {
		if !strings.HasPrefix(prompt.Prompt, "task a: ") || strings.TrimPrefix(prompt.Prompt, "task a: ") == "" {
			t.Fatalf("the %s's prompt reads %q, want `task a: ` before the hook's own text", prompt.Role, prompt.Prompt)
		}
	}
	// The issue's message keeps its fixed start, which a reader of a fleet counts the cap by; how
	// it ends is TestTheIssuesMessagesOfATasksCountsNameTheTask's.
	if messages, _ := capPosts(t, pool); len(messages) != 1 || strings.HasPrefix(messages[0], "task ") {
		t.Fatalf("the cap's messages = %q, want the one line with its fixed start", messages)
	}
}

// The twin: an issue with no plan rows names no task, and its notice and prompts read as before.
func TestAnIssueWithNoPlanRowsNamesNoTaskInItsCapNoticeOrHookPrompt(t *testing.T) {
	pool := migratedPool(t)
	engine := workBackEngine(3, Hook{Name: "work-back", On: HookEventWorkBack, Every: 1,
		Roles: []claim.Role{claim.RoleImplementer}, Prompt: saySoFar})
	seedWorkBackIssue(t, pool, headOne)
	for _, round := range []string{"1", "2", "3"} {
		reviewerRequestsChanges(t, pool, engine, round, headOne)
	}

	want := "Issue reached review_round_cap=3: 3 rounds counted, 0 conflict rounds not counted."
	if blocked := noticesOf(t, pool, "LEGION-208", "pr-blocked"); len(blocked) != 1 || blocked[0].Reason != want {
		t.Fatalf("the pr-blocked notices = %+v, want one whose reason is %q", blocked, want)
	}
	prompts := hookPrompts(t, pool)
	if len(prompts) != 3 {
		t.Fatalf("the hook sent %d prompts over three returns, want one at each", len(prompts))
	}
	for _, prompt := range prompts {
		if prompt.Prompt != saySoFar {
			t.Fatalf("the %s's prompt reads %q, want the hook's own text %q", prompt.Role, prompt.Prompt, saySoFar)
		}
	}
}

// Two tasks of one issue keep their own counts of returns: of tasks a and b, each `implementer
// reviewer` with agents of their own, both at their review, a on the issue's branch with its pull
// request #42 and b on legion/LEGION-208.b with #45, b's review requests changes once and
// a's three times. a counts 3 rounds and 3 passes, b 1 and 1; the owner is told review_round_cap
// once, in a notice that names task a, and nothing of b. With the hook a deployment runs that
// names none (config.DefaultHooks), a's fifth return prompts the implementer and the reviewer of
// a alone, each prompt naming task a.
//
// The fault it catches is counts that one task shares with another (its control: the counts read
// and written for the plan's first task whatever the task, which raises a's rounds to 4 with b's
// one return).
func TestTwoTasksOfOneIssueKeepTheirOwnCounts(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a", "reviewer:ar"}, agents: []claim.Agent{"impl-a", "rev-a"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "b", steps: []string{"implementer:b", "reviewer:br"}, agents: []claim.Agent{"impl-b", "rev-b"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "c", steps: []string{"implementer:c"}, agents: []claim.Agent{"impl-c"}, needs: []string{"a", "b"}, state: record.TaskWaiting})
	seedTaskBranch(t, pool, root.Key, "b", "legion/LEGION-208.b")
	seedTaskPR(t, pool, engine, root.Key, "b", 45)

	taskMoves(t, pool, engine, root.Key, "b", TriggerReviewRejected, changesBody)
	taskMoves(t, pool, engine, root.Key, "a", TriggerReviewRejected, changesBody)
	for range 2 {
		taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")
		taskMoves(t, pool, engine, root.Key, "a", TriggerReviewRejected, changesBody)
	}

	tasks := tasksOf(t, pool, engine, root.Key)
	if a := tasks["a"].Counts; a.Rounds != 3 || a.Pass != 3 {
		t.Fatalf("task a counts %d rounds and %d passes, want 3 and 3: its own three returns", a.Rounds, a.Pass)
	}
	if b := tasks["b"].Counts; b.Rounds != 1 || b.Pass != 1 {
		t.Fatalf("task b counts %d rounds and %d passes, want 1 and 1: its own one return", b.Rounds, b.Pass)
	}
	notices := noticesOf(t, pool, root.Key, "pr-blocked")
	if len(notices) != 1 || !strings.HasPrefix(notices[0].Reason, "task a: Issue reached review_round_cap=3") {
		t.Fatalf("the pr-blocked notices of %s = %+v, want the one of review_round_cap naming task a", root.Key, notices)
	}

	for range 2 {
		taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")
		taskMoves(t, pool, engine, root.Key, "a", TriggerReviewRejected, changesBody)
	}
	prompts := hookPrompts(t, pool)
	claims := map[claim.Role]claim.Token{}
	for _, prompt := range prompts {
		if !strings.HasPrefix(prompt.Prompt, "task a: ") {
			t.Fatalf("the hook prompt %+v names no task a: want each prompt of a's fifth return to name task a", prompt)
		}
		claims[prompt.Role] = prompt.Claim
	}
	if len(prompts) != 3 || claims[claim.RoleImplementer] != agentClaim(t, root.Key, "impl-a") || claims[claim.RoleReviewer] != agentClaim(t, root.Key, "rev-a") {
		t.Fatalf("the hook prompts of %s = %+v, want one firing whose implementer and reviewer are a's agents impl-a and rev-a", root.Key, prompts)
	}
}

// The messages a task's counts post on the issue name that task too: of tasks a and b, each
// `implementer reviewer` with agents of their own, both at their review, b's review requests
// changes once and a's five times, so a reaches review_round_cap at its third round, and its fifth
// return fires the hook a deployment runs that names none (config.DefaultHooks). The cap's message
// and the firing's message keep the fixed start a reader of a fleet greps them out by, and each
// ends `Task: a.`: a's counts, said as a's and not b's. An issue with no plan rows posts both lines
// as before (TestReviewRoundCapPostsOneMessageAndNoticeForTheThirdRound, TestAFiringReachesEachRoleItNames).
//
// The fault it catches is a message that tells the count of one task of several as the issue's,
// so its reader cannot say whose rounds or returns they were (its control: the task's sentence
// left off each message).
func TestTheIssuesMessagesOfATasksCountsNameTheTask(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a", "reviewer:ar"}, agents: []claim.Agent{"impl-a", "rev-a"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "b", steps: []string{"implementer:b", "reviewer:br"}, agents: []claim.Agent{"impl-b", "rev-b"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "c", steps: []string{"implementer:c"}, agents: []claim.Agent{"impl-c"}, needs: []string{"a", "b"}, state: record.TaskWaiting})
	seedTaskBranch(t, pool, root.Key, "b", "legion/LEGION-208.b")
	seedTaskPR(t, pool, engine, root.Key, "b", 45)

	taskMoves(t, pool, engine, root.Key, "b", TriggerReviewRejected, changesBody)
	taskMoves(t, pool, engine, root.Key, "a", TriggerReviewRejected, changesBody)
	for range 4 {
		taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")
		taskMoves(t, pool, engine, root.Key, "a", TriggerReviewRejected, changesBody)
	}

	capped := "Issue reached review_round_cap=3: 3 rounds counted, 0 conflict rounds not counted. Task: a."
	if messages, _ := capPosts(t, pool); !sameStrings(messages, []string{capped}) {
		t.Fatalf("the cap's messages on %s = %q, want the one message %q, naming task a", root.Key, messages, capped)
	}
	fired := "Legion hook work-back fired at 5 moves back: reviewing to implementing. Prompted: implementer, tester, reviewer. Task: a."
	if posted := firingMessages(t, pool); !sameStrings(posted, []string{fired}) {
		t.Fatalf("the hook's messages on %s = %q, want the one message %q, naming task a", root.Key, posted, fired)
	}
}
