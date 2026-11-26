// The whole daemon walks the plans an owner gives where the owner does a step of its own, where a
// task has no pull request, where a failed step returns its work, and where the fresh agent of
// acceptance or a role the plan defines does a step.
//
// Each test boots the whole daemon over the package's broker, the real store, the real outbox and
// the real Dispatch of this package's tests (ownerStepsRig, owner_steps_rig_test.go), and walks
// each issue through the transports production has: each role's agent over the worker stream and
// the daemon's HTTP routes, the owner through its own routes (complete_step, sign_off), a person's
// review and the merge as GitHub's envelopes on the stream. What is stood in for lies outside the
// walk: GitHub's REST API (stepRecordsGitHub), Envoy's listener, which takes every notice and hook
// prompt the outbox publishes (noticeListener), and the panes and the slots, which the fake runtime
// launches and answers for and the test plays.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	stepplan "github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The owner does a step of its own: the plan `implementer:owner`. No claim of the implementer is
// created; the outbox writes the owner's slot for the implementer's App and asks whether the slot
// holds it, and while the runtime answers that it does not yet, the owner is told nothing. Once
// the slot holds it, the owner is told its step as one notice owner-step. Its complete_step of
// step 1, with its handoff record written in its own session and the pull request open, moves the
// issue to the retro, which is the owner's too (record.PlaceWorker), done as the implementer whose
// step was the owner's (record.RetroActsAs). The end of step 1 queues the write of the owner's slot
// back to its own App, which writes nothing, since the retro already runs as the implementer, and
// the retro is told as an owner-step once the outbox has written the slot for the implementer's App
// and the slot holds it, the App that can resolve the pull request's threads. The owner's
// complete_step of the retro with its rating moves the issue to merging, the retro's end writes the
// slot back to the owner's own App, and the merge ends it at done. The records of step 1 and of the
// retro name the owner's claim and its session, and no implementer was ever launched.
//
// The control is a mutation of the release's code, run by hand: start's branch of the owner's
// place taken out of workflow/effects.go. The step's start then launches the owner's claim again
// as a worker's start, in place of telling it, and the owner's turn to report step 1 finds its
// claim launching: the test fails there.
func TestTheOwnerDoesAStep(t *testing.T) {
	r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 1, config.Project{})
	owner := r.worker(claim.RoleArchitect)
	r.givePlan("implementer:owner")
	r.rt.ScriptSlotHolds(runtime.ErrSlotCredentialPending)
	r.openGate(phase.Implementing)
	if !testwait.Holds(stepRecordsWait, 50*time.Millisecond, func() bool { return r.slotAsked("SlotHoldsCredential", owner.token, claim.RoleImplementer) }) {
		t.Fatalf("the outbox never asked whether %s's slot holds the implementer's App: the implementer was launched %v (its claim recorded: %t), and the owner was told %d owner-step notices",
			owner.token, r.launchesOfRole(claim.RoleImplementer), r.claimRecorded(r.token(claim.RoleImplementer)), len(r.ownerNotices(record.OwnerStepNotice)))
	}
	if r.slotWrites(owner.token) == 0 {
		t.Fatalf("the outbox asked whether %s's slot holds the implementer's App without writing it: calls %+v", owner.token, r.slotCalls())
	}
	if testwait.Holds(liveSettle, 100*time.Millisecond, func() bool { return len(r.ownerNotices(record.OwnerStepNotice)) > 0 }) {
		t.Fatalf("the owner of %s was told its step %+v while its slot does not hold the implementer's App", r.key, r.ownerNotices(record.OwnerStepNotice))
	}
	r.rt.ScriptSlotHolds(nil)
	told := r.waitOwnerNotices(record.OwnerStepNotice, 1)
	if step := told[0].OwnerStep; told[0].Role != claim.RoleImplementer || told[0].Phase != phase.Implementing || step == nil || step.Task != "a" || step.Kind != "step" || step.Place != 1 {
		t.Fatalf("the owner of %s was told %+v, want the owner-step of step 1, implementer:owner", r.key, told[0])
	}
	if launches := r.launchesOfRole(claim.RoleImplementer); len(launches) != 0 || r.claimRecorded(r.token(claim.RoleImplementer)) {
		t.Fatalf("%s's step of the owner launched an implementer %v (its claim recorded: %t), want no implementer claim", r.key, launches,
			r.claimRecorded(r.token(claim.RoleImplementer)))
	}

	r.github.open("legion/"+r.key, r.number)
	written := r.slotWrites(owner.token)
	handoff, version := r.writeHandoff(owner.session, "implement")
	if status, body := r.ownerReports(api.CompleteStepRequest{Step: 1, Summary: "implemented", Commit: stepRecordsHead,
		HandoffID: handoff, HandoffVersion: version}); status != http.StatusOK {
		t.Fatalf("the owner's complete_step of step 1 of %s = %d %s, want 200", r.key, status, body)
	}
	r.waitPhase(phase.Retro)
	told = r.waitOwnerNotices(record.OwnerStepNotice, 2)
	if told[1].Role != "" || told[1].Phase != phase.Retro {
		t.Fatalf("the owner of %s was told %+v at the retro, want the owner-step of task a's retro", r.key, told[1])
	}
	if r.slotWrites(owner.token) == written {
		t.Fatalf("the retro of %s was told with no write of the owner's slot since step 1, want the outbox to write it for the implementer's App: calls %+v", r.key, r.slotCalls())
	}
	asked := r.rt.CallsOf("SlotHoldsCredential")
	if last := asked[len(asked)-1]; last.Claim != owner.token || last.Role != claim.RoleImplementer {
		t.Fatalf("the retro of %s was told after the outbox asked whether %s's slot holds the App of %s, want the implementer's: calls %+v",
			r.key, last.Claim, last.Role, r.slotCalls())
	}
	written = r.slotWrites(owner.token)
	if status, body := r.rateRetro(); status != http.StatusOK {
		t.Fatalf("the owner's complete_step of %s's retro = %d %s, want 200", r.key, status, body)
	}
	r.waitPhase(phase.Merging)
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the retro's end to write "+string(owner.token)+"'s slot back to its own App", func() bool {
		return r.slotWrites(owner.token) > written
	})
	r.merge()
	r.waitRecordsOf(owner, 1)
	if launches := r.launchesOfRole(claim.RoleImplementer); len(launches) != 0 || r.claimRecorded(r.token(claim.RoleImplementer)) {
		t.Fatalf("%s launched an implementer %v by done, want none: the owner did step 1 and the retro", r.key, launches)
	}
}

// A task with no step of the type implementation: the plan `tester` runs the tester with the
// task's text, and no pull request is ever opened. The tester's pass starts the retro, which is the
// owner's (record.PlaceWorker): the owner is told it as one notice owner-step and no implementer is
// launched. The owner's complete_step of the retro with its rating ends the issue at done, with no
// merger launched and no production check opened, though the project checks production with the
// tester; the issue's Dispatch status went testing, retro, done; and the record of the retro names
// the owner and carries the rating.
//
// The control is a mutation of the release's code, run by hand: the row from the retro to done
// taken out (workflow's noPullRequest answering false, hasPullRequest true). The retro's end then
// moves the issue to merging and launches a merger, which waits for a merge nothing opens, and the
// wait for done fails.
func TestAPlanWithOnlyTheTester(t *testing.T) {
	r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 1,
		config.Project{ProductionCheck: claim.RoleTester})
	r.givePlan("tester")
	r.openGate(phase.Testing)
	tester := r.worker(claim.RoleTester)
	task := tester.take().Message
	if !strings.Contains(task, "Make the change the issue asks for.") {
		t.Fatalf("the tester of %s was told %q, want it to hold task a's own text", r.key, task)
	}
	r.finish(tester, "test", testCompletion, phase.Retro)

	told := r.waitOwnerNotices(record.OwnerStepNotice, 1)
	if step := told[0].OwnerStep; told[0].Role != "" || told[0].Phase != phase.Retro || step == nil || step.Task != "a" || step.Kind != "retro" {
		t.Fatalf("the owner of %s was told %+v, want the owner-step of task a's retro", r.key, told[0])
	}
	if launches := r.launchesOfRole(claim.RoleImplementer); len(launches) != 0 {
		t.Fatalf("%s's retro launched an implementer %v, want none: the retro of a task with no step of the type implementation is the owner's", r.key, launches)
	}
	if status, body := r.rateRetro(); status != http.StatusOK {
		t.Fatalf("the owner's complete_step of %s's retro = %d %s, want 200", r.key, status, body)
	}
	r.waitPhase(phase.Done)

	if launches := r.launchesOfRole(claim.RoleMerger); len(launches) != 0 {
		t.Fatalf("%s, which has no pull request, launched a merger %v, want none", r.key, launches)
	}
	if testwait.Holds(liveSettle, 100*time.Millisecond, r.checkOpened) {
		t.Fatalf("%s, done with no merge, opened a production check", r.key)
	}
	waitRead(r.stepRecordsRig, "the Dispatch statuses of "+r.key, "testing, retro, done", time.Second, r.dispatchStatuses, func(statuses []string) error {
		if want := []string{"testing", "retro", "done"}; !slices.Equal(statuses, want) {
			return fmt.Errorf("the daemon wrote the statuses %v, want %v", statuses, want)
		}
		return nil
	})
	owner := r.worker(claim.RoleArchitect)
	r.waitRecordsOf(owner)
}

// waitRecordsOf waits for the read of the walk's issue's steps to hold, of task a, the record of
// each step of steps and the record of the retro, each ended passed and each naming c's claim as
// its agent and c's session; the retro carries deploymentRating and deploymentReason.
func (r *ownerStepsRig) waitRecordsOf(c *stepClaimant, steps ...int) {
	r.t.Helper()
	waitRead(r.stepRecordsRig, "the records of "+r.key+"'s steps of "+string(c.token), "Dispatch", time.Second, func() (deploymentStepsRead, error) {
		var read deploymentStepsRead
		return read, r.personRead("/api/v1/issues/"+r.key+"/steps", &read)
	}, func(read deploymentStepsRead) error {
		var records []deploymentStepRead
		for _, listed := range read.Tasks {
			records = append(records, listed.Steps...)
		}
		for _, step := range append(slices.Clone(steps), 0) {
			kind, name := "step", fmt.Sprintf("step %d", step)
			if step == 0 {
				kind, name = "retro", "the retro"
			}
			at := slices.IndexFunc(records, func(got deploymentStepRead) bool { return got.Kind == kind && (step == 0 || got.Step == step) })
			if at < 0 {
				return fmt.Errorf("the read holds no record of %s", name)
			}
			got := records[at]
			switch {
			case got.Task != "a" || got.EndedAt == nil || got.Result != "passed":
				return fmt.Errorf("%s is of task %q, ended %v with result %q; want task a, ended passed", name, got.Task, got.EndedAt, got.Result)
			case got.Agent != string(c.token) || got.Session != c.session:
				return fmt.Errorf("%s names the agent %q in session %q, want %s in %s", name, got.Agent, got.Session, c.token, c.session)
			case step == 0 && (got.Rating == nil || *got.Rating != deploymentRating || got.RatingReason == nil || *got.RatingReason != deploymentReason):
				return fmt.Errorf("the retro names the rating %v and the reason %v, want %d and %q", deref(got.Rating), deref(got.RatingReason),
					deploymentRating, deploymentReason)
			}
		}
		return nil
	})
}

// Each task of the returns of a failed step, each as an issue of one task (r.issues, in order):
//
//  1. `implementer tester reviewer`: a person's review requests changes once, and the task returns
//     to step 1 in the claims and the sessions of its implementer and its reviewer, with no launch
//     of either; the implementer's task holds the review's report. The next review approves, and
//     the issue walks to done.
//  2. `tester`: the test fails, no step of the type implementation stands before it, so the task
//     stops, the issue is held, and the owner is told once, naming task a and step 1.
//  3. `implementer tester reviewer`, under review_round_cap 3 and the hook a deployment that writes
//     no `hooks:` key runs: three reviews request changes, then two tests fail. The owner is told
//     pr-blocked once, at the third review, and no more; the hook prompts the implementer, the
//     tester and the reviewer at the fifth return and not before; and step 1 runs again after it.
//  4. `implementer tester acceptance:owner`: the owner's complete_step of step 3 fails it with
//     returnTo 2, and the task returns to step 2: the tester works it again, and the implementer is
//     told nothing and launched no more.
//
// The control is a mutation of the stored plan, run by hand: the type of task 1's implementer step
// set to review in plan_steps before its review requests changes. No step of the type
// implementation stands before the reviewer then, so the task stops, held, and the wait for task 1
// to return to implementing fails.
func TestEachTaskOfVReturnAsAnIssue(t *testing.T) {
	proof := newLiveProofOn(t, workflowNATS(t))
	proof.cfg.ReviewRoundCap = 3
	proof.cfg.Hooks = config.DefaultHooks()
	r := newOwnerStepsRig(t, proof, startScratchDispatchOn(t, scratchDispatchOptions{}), 4, config.Project{})

	r.on(r.issues[0].key)
	r.returnsToItsImplementer()

	r.on(r.issues[1].key)
	r.givePlan("tester")
	r.openGate(phase.Testing)
	tester := r.worker(claim.RoleTester)
	tester.take()
	r.finish(tester, "test", failedTestCompletion, phase.Held)
	if state, kind, position := r.taskOf(); state != string(record.TaskStopped) || kind != "step" || position != 1 {
		t.Fatalf("after the tester of `tester` failed, %s's task a is %s at %s %d, want stopped at step 1", r.key, state, kind, position)
	}
	stopped := r.waitOwnerNotices("task-stopped", 1)
	if stopped[0].Role != claim.RoleTester || stopped[0].Phase != phase.Testing {
		t.Fatalf("the owner of %s was told %+v, want a task-stopped of step 1's role tester at testing", r.key, stopped[0])
	}

	r.on(r.issues[2].key)
	r.returnsFiveTimes()

	r.on(r.issues[3].key)
	r.ownersFailReturnsToTheTester()

	r.on(r.issues[1].key)
	if stopped := r.ownerNotices("task-stopped"); len(stopped) != 1 {
		t.Fatalf("the owner of %s was told %d task-stopped notices by the end, want the one", r.key, len(stopped))
	}
}

// returnsToItsImplementer is task 1 of TestEachTaskOfVReturnAsAnIssue on the walk's issue.
func (r *ownerStepsRig) returnsToItsImplementer() {
	t := r.t
	t.Helper()
	r.givePlan("implementer", "tester", "reviewer")
	r.openGate(phase.Implementing)
	r.runStep("implementer", answerPass, phase.Testing)
	r.runStep("tester", answerPass, phase.Reviewing)
	implementer, reviewer := r.worker(claim.RoleImplementer), r.worker(claim.RoleReviewer)
	r.runStep("reviewer", answerFail, phase.Implementing)
	if state, kind, position := r.taskOf(); state != string(record.TaskRunning) || kind != "step" || position != 1 {
		t.Fatalf("after the review requested changes, %s's task a is %s at %s %d, want running at step 1", r.key, state, kind, position)
	}
	implementer.take()
	r.finish(implementer, "implement", implementCompletion, phase.Testing)
	r.runStep("tester", answerPass, phase.Reviewing)
	r.runStep("reviewer", answerPass, phase.Retro)
	for _, c := range []*stepClaimant{implementer, reviewer} {
		if launches := r.launchesOfRole(c.role); len(launches) != 1 {
			t.Fatalf("%s's %s was launched %v by its second round, want its first launch alone: the task returned to the same agents", r.key, c.role, launches)
		}
		if got := r.d.claim(c.token).Session; got != c.session {
			t.Fatalf("%s's %s holds the session %q in its second round, want %q, its first round's", r.key, c.role, got, c.session)
		}
	}
	r.retro()
	r.merge()
}

// returnsFiveTimes is task 3 of TestEachTaskOfVReturnAsAnIssue on the walk's issue.
func (r *ownerStepsRig) returnsFiveTimes() {
	t := r.t
	t.Helper()
	r.givePlan("implementer", "tester", "reviewer")
	r.openGate(phase.Implementing)
	for round := 1; round <= 3; round++ {
		r.runStep("implementer", answerPass, phase.Testing)
		r.runStep("tester", answerPass, phase.Reviewing)
		r.runStep("reviewer", answerFail, phase.Implementing)
		if prompts := r.hookPrompts(); len(prompts) != 0 {
			t.Fatalf("the hook prompted %v after return %d of %s, want nothing before the fifth", prompts, round, r.key)
		}
	}
	blocked := r.waitOwnerNotices("pr-blocked", 1)
	if !strings.Contains(blocked[0].Reason, "review_round_cap=3") {
		t.Fatalf("the owner of %s was told %q at the third review, want a pr-blocked naming review_round_cap=3", r.key, blocked[0].Reason)
	}
	r.runStep("implementer", answerPass, phase.Testing)
	r.runStep("tester", answerFail, phase.Implementing)
	if prompts := r.hookPrompts(); len(prompts) != 0 {
		t.Fatalf("the hook prompted %v after the fourth return of %s, want nothing before the fifth", prompts, r.key)
	}
	r.runStep("implementer", answerPass, phase.Testing)
	r.runStep("tester", answerFail, phase.Implementing)

	want := []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer}
	var prompts map[claim.Role][]string
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the hook's prompts of "+r.key+"'s fifth return", func() bool {
		prompts = r.hookPrompts()
		return len(prompts) == len(want)
	})
	// The prompt of the fifth return is the message record.HookPrompt.Message writes from the
	// firing's values: the hook and its words as the deployment gives them, the move from testing
	// back to implementing, the hook's count 5, the task's counts of each move back (three from the
	// review, two from the tests) and the issue's pull request.
	prompt := record.HookPrompt{Hook: "work-back", From: phase.Testing, To: phase.Implementing, Count: 5,
		Counts:      map[string]int{phase.Pair(phase.Reviewing, phase.Implementing): 3, phase.Pair(phase.Testing, phase.Implementing): 2},
		PullRequest: fmt.Sprintf("%s#%d", liveRepo, r.number), Prompt: r.hookWords("work-back")}.Message(r.key)
	for _, role := range want {
		if got := prompts[role]; !slices.Equal(got, []string{prompt}) {
			t.Fatalf("the hook prompted %s's %s with %q, want the one prompt of the fifth return from testing to implementing %q", r.key, role, got, prompt)
		}
	}
	r.runStep("implementer", answerPass, phase.Testing)
	if blocked := r.ownerNotices("pr-blocked"); len(blocked) != 1 {
		t.Fatalf("the owner of %s was told %d pr-blocked notices by step 1's sixth run, want the one of the third review", r.key, len(blocked))
	}
}

// ownersFailReturnsToTheTester is task 4 of TestEachTaskOfVReturnAsAnIssue on the walk's issue.
func (r *ownerStepsRig) ownersFailReturnsToTheTester() {
	t := r.t
	t.Helper()
	r.givePlan("implementer", "tester", "acceptance:owner")
	r.openGate(phase.Implementing)
	r.runStep("implementer", answerPass, phase.Testing)
	r.runStep("tester", answerPass, phase.Accepting)
	implementer, tester := r.worker(claim.RoleImplementer), r.worker(claim.RoleTester)
	told := r.waitOwnerNotices(record.OwnerStepNotice, 1)
	if step := told[0].OwnerStep; told[0].Phase != phase.Accepting || step == nil || step.Kind != "step" || step.Place != 3 {
		t.Fatalf("the owner of %s was told %+v, want the owner-step of step 3, its acceptance", r.key, told[0])
	}
	if status, body := r.ownerReports(api.CompleteStepRequest{Step: 3, Summary: "the widget does not render", Verdict: "fail",
		Commit: stepRecordsHead, ReturnTo: new(2)}); status != http.StatusOK {
		t.Fatalf("the owner's complete_step failing step 3 of %s with returnTo 2 = %d %s, want 200", r.key, status, body)
	}
	r.waitPhase(phase.Testing)
	if state, kind, position := r.taskOf(); state != string(record.TaskRunning) || kind != "step" || position != 2 {
		t.Fatalf("after the owner's fail with returnTo 2, %s's task a is %s at %s %d, want running at step 2", r.key, state, kind, position)
	}
	tester.take()
	r.finish(tester, "test", testCompletion, phase.Accepting)
	if pending := len(implementer.prompts); pending != 0 {
		t.Fatalf("%s's implementer was told %d tasks after the owner's fail returned the task to step 2, want none: step 1 does not run again", r.key, pending)
	}
	if launches := r.launchesOfRole(claim.RoleImplementer); len(launches) != 1 {
		t.Fatalf("%s's implementer was launched %v by the task's return to step 2, want its first launch alone", r.key, launches)
	}
}

// A step of the role acceptance with no key is a fresh agent's, not the owner's: the plan
// `implementer acceptance` launches a claim of the role acceptance after the implementer's step,
// whose prompt holds the shared role prompt roles/acceptance.md and the daemon's part of it, and
// the owner is told no owner-step. The agent's pass, with the commit it read, starts the retro of
// the implementer, the agent of the task's last step of the type implementation, and the merge ends
// the walk at done.
//
// The control is a mutation of the release's code, run by hand: prompts.Compose without its case
// of acceptance. Each launch of the claim then fails on its prompt (`unsupported role
// "acceptance"`) until its launch failures run out, the issue is held, and no agent of acceptance
// ever comes up to pass: the wait for the issue to stand at accepting, or for the agent's launch,
// fails.
func TestAFreshAgentAccepts(t *testing.T) {
	r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 1, config.Project{})
	r.givePlan("implementer", "acceptance")
	r.openGate(phase.Implementing)
	r.runStep("implementer", answerPass, phase.Accepting)
	acceptance := r.worker(claim.Role("acceptance"))
	spec := lastLaunch(t, r.rt, acceptance.token)
	shared, own := filepath.Join(r.cfg.StateDir, "prompts", "shared", "acceptance.md"), filepath.Join(r.cfg.StateDir, "prompts", "go", "acceptance.md")
	if spec.Role != "acceptance" || !slices.Contains(spec.Prompt.RolePromptPaths, shared) || !slices.Contains(spec.Prompt.RolePromptPaths, own) {
		t.Fatalf("%s's agent of acceptance launched as %s with the prompt paths %v, want the role acceptance with %s and %s", r.key, spec.Role,
			spec.Prompt.RolePromptPaths, shared, own)
	}
	acceptance.take()
	if told := r.ownerNotices(record.OwnerStepNotice); len(told) != 0 {
		t.Fatalf("the owner of %s was told %+v, want no owner-step: acceptance with no key is a fresh agent's", r.key, told)
	}
	r.finish(acceptance, "", api.HandoffCompleteRequest{Summary: "every criterion holds", Commit: stepRecordsHead, Verdict: "pass"}, phase.Retro)
	r.retro()
	r.merge()
}

// planWithCoder is the text of a plan.md whose task a names `coder checker`, after the role blocks
// of coder, of the type implementation on the model m-one, and of checker, of the type testing on
// the model m-two.
func planWithCoder() string {
	return ":::plan_role{#coder type=\"implementation\" model=\"m-one\"}\n# Coder\n\nWrite the change.\n:::\n\n" +
		":::plan_role{#checker type=\"testing\" model=\"m-two\"}\n# Checker\n\nCheck the change.\n:::\n\n" + deploymentPlanOf("coder", "checker")
}

// The retro of a task whose last step of the type implementation is a role the plan defines runs
// as that role, on its model: the plan `coder checker`, coder of the type implementation on m-one.
// The coder's step and the checker's pass, and the retro is the coder's (record.PlaceWorker): the
// coder's claim takes it, its launch spec still of the role coder on m-one, the implementer's claim
// never launched; the coder's completion of the retro with its rating moves the issue to merging,
// and the merge ends the walk at done.
//
// The control is a mutation of the release's code, run by hand: the retro's role read as the
// implementer (workflow's Walk.RoleAt at the retro answering phase.RoleFor(phase.Retro)). The
// coder's claim is then suspended and launched again as the implementer, on no model, and the
// check of the retro's launch spec fails; where that launch has not come within the wait, the wait
// for the retro to reach the coder's claim fails, naming the issue's launches.
func TestTheRetroOfARoleOfThePlanRunsOnItsModel(t *testing.T) {
	proof := newLiveProofOn(t, workflowNATS(t))
	proof.cfg.Models = []string{"m-one", "m-two"}
	r := newOwnerStepsRig(t, proof, startScratchDispatchOn(t, scratchDispatchOptions{}), 1, config.Project{})
	if status, body := r.registerPlan(r.document("plan.md", planWithCoder()), 1); status != http.StatusOK {
		t.Fatalf("register_plan of %s's plan `coder checker` = %d %s, want 200", r.key, status, body)
	}
	r.openGate(phase.Step)
	coder := r.worker("coder")
	coder.take()
	r.github.open("legion/"+r.key, r.number)
	r.finish(coder, "", api.HandoffCompleteRequest{Summary: "coded", Commit: stepRecordsHead, Verdict: "pass"}, phase.Step)
	checker := r.worker("checker")
	checker.take()
	r.finish(checker, "", api.HandoffCompleteRequest{Summary: "every check holds", Commit: stepRecordsHead, Verdict: "pass"}, phase.Retro)

	// The retro reaches the coder's claim either as a task in the session it runs, or as a launch
	// of the claim, as the role coder still, which resumes that session in a new process.
	launched := len(launchSpecsOf(r.rt, coder.token))
	if !testwait.Holds(stepRecordsWait, 50*time.Millisecond, func() bool {
		return len(coder.prompts) > 0 || len(launchSpecsOf(r.rt, coder.token)) > launched
	}) {
		var launches []string
		for _, call := range r.launchesOf(r.key) {
			launches = append(launches, fmt.Sprintf("%s as %s (%s)", call.Spec.Claim, call.Spec.Role, call.Method))
		}
		t.Fatalf("the retro of %s never reached %s; the issue stands in %s, and its launches are %v", r.key, coder.token,
			r.d.state().Issues[r.key].Phase, launches)
	}
	if spec := lastLaunch(t, r.rt, coder.token); spec.Role != "coder" || spec.Model != "m-one" {
		t.Fatalf("%s's retro runs on the launch of %s as the role %q on the model %q, want the role coder on m-one", r.key, coder.token, spec.Role, spec.Model)
	}
	coder.take()
	if launches := r.launchesOfRole(claim.RoleImplementer); len(launches) != 0 {
		t.Fatalf("%s's retro launched an implementer %v, want none: the coder does the retro", r.key, launches)
	}
	r.finish(coder, "", r.retroCompletion(&record.PlanRating{Rating: deploymentRating, Reason: deploymentReason}), phase.Merging)
	r.merge()
}

// An issue that ran under the release before the owner did a step of its own goes on. The store
// holds every migration below plan_roles, applied by hand (storeBelow), and two issues of that
// release on the project's default steps, `planner implementer tester reviewer acceptance:owner`,
// written with plain SQL in its columns (seedOnTheDefaultSteps): one at accepting, step 5, the
// owner's acceptance, with its architect's claim a launch that release began in the session it
// recorded; and one at its retro, with its architect's and its implementer's claims so. This
// release's migrations then run on it, as a deploy runs them, and the whole daemon boots on it and
// resumes each claim in its session. Neither issue starts again: each stands where that release
// left it, and no planner is launched. The owner of the first signs it off with sign_off, as the
// release before told it to, which moves it to its retro; the implementer's retro moves each issue
// to merging, and the merge ends each at done, each claim in the session the release before
// recorded.
//
// The control is a mutation of the seed, run by hand: the first issue's stored phase removed, the
// issue written at admitted with Dispatch's status in_progress, as the in-flight test's control is.
// The boot then starts that issue again from its start: its root takes its slot and starts its
// own first phase, so the issue stands at admitted after the boot, not at accepting, and the check
// fails.
func TestTheOwnerStepsUpgradeGoesOn(t *testing.T) {
	r := newOwnerStepsUpgradeRig(t)
	accepting, retro := r.walks[0], r.walks[1]
	for _, issue := range r.walks {
		if got := string(r.d.state().Issues[issue.key].Phase); got != string(issue.stands) {
			t.Fatalf("%s stands in %q after the boot, want %s, where the release before left it", issue.key, got, issue.stands)
		}
		if err := r.wentOnFrom(issue); err != nil {
			t.Fatal(err)
		}
	}

	r.on(accepting.key)
	r.signOff(accepting.key)
	r.retro()
	r.merge()

	r.on(retro.key)
	r.retro()
	r.merge()

	for _, issue := range r.walks {
		if err := r.wentOnFrom(issue); err != nil {
			t.Fatal(err)
		}
		for role, kept := range issue.sessions {
			if got := r.d.claim(r.tokenOf(issue.key, role)).Session; got != kept.id {
				t.Fatalf("%s's %s holds the session %q at done, want %q, the one the release before recorded", issue.key, role, got, kept.id)
			}
		}
	}
}

// wentOnFrom says whether issue, which the release before left on the default steps, went on from
// where it stood: no planner, the agent of step 1, was launched for it, and its first launch of the
// role that works its place is a resume of the session the release before recorded: the
// architect's for accepting, the owner's step, and the implementer's for the retro. The error names
// the first that does not hold.
func (r *inFlightRig) wentOnFrom(issue *inFlightIssue) error {
	r.t.Helper()
	working := claim.RoleImplementer
	if issue.stands == phase.Accepting {
		working = claim.RoleArchitect
	}
	var first *fake.Call
	for _, launch := range r.launchesOf(issue.key) {
		if launch.Spec.Role == claim.RolePlanner {
			return fmt.Errorf("%s, left in %s by the release before, started again at step 1: the daemon launched its planner (%s)",
				issue.key, issue.stands, launch.Method)
		}
		if first == nil && launch.Spec.Role == working {
			first = &launch
		}
	}
	if kept := issue.sessions[working]; first == nil || first.Method != "Resume" || first.Spec.ResumeSessionFile != kept.file {
		return fmt.Errorf("%s, left in %s, first launched its %s as %+v, want a resume of the session file %s the release before recorded",
			issue.key, issue.stands, working, first, kept.file)
	}
	return nil
}

// newOwnerStepsUpgradeRig is the store the release before leaves, every migration below
// plan_roles (storeBelow), seeded with an issue at accepting and an issue at its retro, each on the
// default steps (seedOnTheDefaultSteps), migrated to this release's schema, with the daemon booted
// on it at the real Dispatch of this package's tests and each issue's recorded claims resumed in
// their sessions.
func newOwnerStepsUpgradeRig(t *testing.T) *inFlightRig {
	t.Helper()
	proof := newLiveProofOn(t, workflowNATS(t))
	st, dsn, _ := storeBelow(t, planRolesMigration)
	proof.st, proof.cfg.PostgresDSN = st, dsn
	proof.cfg.Project = "OU" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	proof.cfg.AdmissionCap = 16
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &inFlightRig{stepRecordsRig: &stepRecordsRig{liveProof: proof, dispatch: startScratchDispatchOn(t, scratchDispatchOptions{}),
		github: newStepRecordsGitHub(t), claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16)}}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Owner steps upgrade"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	// Two titles of the in-flight run, which Dispatch takes as two issues of no near-copy title.
	for place, stood := range []phase.Phase{phase.Accepting, phase.Retro} {
		title := inFlightTitles[4+place]
		status := workflow.PhaseStatus(stood)
		var created createdIssue
		r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": title}, http.StatusCreated, &created)
		r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": status, "labels": []string{"legion"}}, http.StatusOK, &created)
		issue := &inFlightIssue{walkIssue: walkIssue{key: created.Key, title: title, number: int(number) + 1 + place},
			stands: stood, sessions: map[claim.Role]inFlightSession{}}
		r.issues = append(r.issues, issue.walkIssue)
		r.github.open("legion/"+issue.key, issue.number)
		r.seedInFlight(issue, stood, status, created)
		r.seedOnTheDefaultSteps(issue)
		r.walks = append(r.walks, issue)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate the store the release before left to this release's schema: %v", err)
	}
	r.boot()
	for _, issue := range r.walks {
		r.on(issue.key)
		for role := range issue.sessions {
			r.resume(issue, role)
		}
	}
	return r
}

// seedOnTheDefaultSteps writes, with plain SQL in the columns of the release before, what that
// release adds to an issue seedInFlight left in accepting or in its retro once it took the
// project's default steps: version 0 of the source default, with no document and no giver, its one
// task default whose block names no steps, running at step 5, the acceptance of the key owner, or
// at its retro, each of the five steps with the type of its role and the name that release gave its
// agent as it wrote the step (record.NameAgents, none for the step of the key owner), the
// implementer's row the row of step 2, and the claims' plugin contract, the owner's plan's.
func (r *inFlightRig) seedOnTheDefaultSteps(issue *inFlightIssue) {
	r.t.Helper()
	key := issue.key
	kind, position := "step", 5
	if issue.stands == phase.Retro {
		kind, position = "retro", 0
	}
	r.exec(`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
		values ($1, 1, 0, 'default', '', '', now() - interval '5 hours')`, key)
	r.exec(`insert into plan_tasks (issue, generation, task, plan_version, block_text, steps_by_default, state, running_kind, position)
		values ($1, 1, 'default', 0, 'The project''s default steps.', true, 'running', $2, $3)`, key, kind, position)
	steps := record.NameAgents(nil, nil, nil, []record.PlanStep{
		{Issue: key, Generation: 1, Task: "default", Position: 1, Role: "planner", Type: stepplan.TypePlanning},
		{Issue: key, Generation: 1, Task: "default", Position: 2, Role: "implementer", Type: stepplan.TypeImplementation},
		{Issue: key, Generation: 1, Task: "default", Position: 3, Role: "tester", Type: stepplan.TypeTesting},
		{Issue: key, Generation: 1, Task: "default", Position: 4, Role: "reviewer", Type: stepplan.TypeReview},
		{Issue: key, Generation: 1, Task: "default", Position: 5, Role: "acceptance", Type: stepplan.TypeReview, AgentKey: "owner"},
	})
	for _, step := range steps {
		r.exec(`insert into plan_steps (issue, generation, task, position, role, type, agent_key, agent)
			values ($1, 1, 'default', $2, $3, $4, $5, $6)`, key, step.Position, step.Role, string(step.Type), step.AgentKey, string(step.Agent))
	}
	r.exec(`update phases set step_task = 'default', step_place = 2 where issue = $1 and role = 'implementer'`, key)
	r.exec(`update claims set plugin_contract = $2 where issue = $1`, key, api.OwnerPlanContract)
}

// failedTestCompletion is the tester's completion of a round it fails, naming no step to return to.
var failedTestCompletion = api.HandoffCompleteRequest{Summary: "a criterion fails", Verdict: "fail"}

// ownerReports is the owner of the walk's issue, its own architect, answering in a turn of its own
// and reporting req, a step of its own (complete_step), under a grant minted for it now; it answers
// the daemon's status and body.
func (r *ownerStepsRig) ownerReports(req api.CompleteStepRequest) (int, string) {
	r.t.Helper()
	owner := r.worker(claim.RoleArchitect)
	owner.ownTurn()
	req.GrantID, req.Issue = owner.grant(), r.key
	status, body := r.d.request(http.MethodPost, "/legion/v1/steps/complete", req, false)
	return status, string(body)
}

// rateRetro is the owner's report of the retro of the walk's issue (step 0, ownerReports): the
// commit it stands on and the rating of the plan with its reason, deploymentRating and
// deploymentReason.
func (r *ownerStepsRig) rateRetro() (int, string) {
	r.t.Helper()
	return r.ownerReports(api.CompleteStepRequest{Step: 0, Summary: "retro written", Commit: stepRecordsHead,
		PlanRating: new(deploymentRating), PlanReason: new(deploymentReason)})
}

// ownerNotices is every notice of kind the outbox has published to the topic of the walk's issue's
// own architect, the owner of its task, in order.
func (r *ownerStepsRig) ownerNotices(kind record.NoticeKind) []record.Notice {
	r.t.Helper()
	var notices []record.Notice
	for _, published := range r.notices.acceptedOn(notify.RoleTopicPrefix + string(r.token(claim.RoleArchitect))) {
		var notice record.Notice
		if err := json.Unmarshal([]byte(published.Payload), &notice); err != nil {
			r.t.Fatalf("decode the notice %q published to the owner of %s: %v", published.Payload, r.key, err)
		}
		if notice.Kind == kind {
			notices = append(notices, notice)
		}
	}
	return notices
}

// waitOwnerNotices waits for the outbox to have published count notices of kind to the owner of the
// walk's issue, and answers them; it fails where more were published.
func (r *ownerStepsRig) waitOwnerNotices(kind record.NoticeKind, count int) []record.Notice {
	r.t.Helper()
	var notices []record.Notice
	testwait.WithinPolling(r.t, stepRecordsWait, 100*time.Millisecond, fmt.Sprintf("%d %s notices to the owner of %s", count, kind, r.key), func() bool {
		notices = r.ownerNotices(kind)
		return len(notices) >= count
	})
	if len(notices) != count {
		r.t.Fatalf("the owner of %s was told %d %s notices, want %d: %+v", r.key, len(notices), kind, count, notices)
	}
	return notices
}

// slotCalls is each call the daemon made of the runtime's slot credentials
// (runtime.SlotCredentials), in order, as `<method> <claim> <role>`; a write, which names no role
// since the runtime judges it, as `<method> <claim>`.
func (r *ownerStepsRig) slotCalls() []string {
	var calls []string
	for _, call := range r.rt.Calls() {
		if call.Method == "WriteSlotCredential" || call.Method == "SlotHoldsCredential" {
			calls = append(calls, strings.TrimSpace(fmt.Sprintf("%s %s %s", call.Method, call.Claim, call.Role)))
		}
	}
	return calls
}

// slotAsked says whether the daemon has asked the runtime whether the slot of token holds the App
// of role (runtime.SlotCredentials.SlotHoldsCredential).
func (r *ownerStepsRig) slotAsked(method string, token claim.Token, role claim.Role) bool {
	return slices.Contains(r.slotCalls(), fmt.Sprintf("%s %s %s", method, token, role))
}

// slotWrites is how many writes of the slot of token the daemon has asked of the runtime
// (runtime.SlotCredentials.WriteSlotCredential).
func (r *ownerStepsRig) slotWrites(token claim.Token) int {
	writes := 0
	for _, call := range r.rt.CallsOf("WriteSlotCredential") {
		if call.Claim == token {
			writes++
		}
	}
	return writes
}

// claimRecorded says whether the daemon's store records a claim of token.
func (r *ownerStepsRig) claimRecorded(token claim.Token) bool {
	r.t.Helper()
	var recorded bool
	if err := r.st.Pool().QueryRow(context.Background(), `select exists (select 1 from claims where token = $1)`, string(token)).Scan(&recorded); err != nil {
		r.t.Fatalf("read whether the claim %s is recorded: %v", token, err)
	}
	return recorded
}

// hookPrompts is every message the outbox has published to the topic of a worker's claim of the
// walk's issue, by the claim's role. The walk that reads it makes no move that sends a worker any
// other message (no conflict, no update round, no check run, no note), so each is a prompt of the
// hook, and the test compares each whole.
func (r *ownerStepsRig) hookPrompts() map[claim.Role][]string {
	r.t.Helper()
	prompts := map[claim.Role][]string{}
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer} {
		for _, published := range r.notices.acceptedOn(notify.RoleTopicPrefix + string(r.token(role))) {
			prompts[role] = append(prompts[role], published.Message)
		}
	}
	return prompts
}

// hookWords is the words of the hook named name as the daemon's settings give them.
func (r *ownerStepsRig) hookWords(name string) string {
	r.t.Helper()
	for _, hook := range r.cfg.Hooks {
		if hook.Name == name {
			return hook.Prompt
		}
	}
	r.t.Fatalf("the settings of %s name no hook %s", r.key, name)
	return ""
}

// taskOf is task a of the walk's issue at generation 1 as its plan_tasks row holds it: its state,
// the kind of the act it runs and its position.
func (r *ownerStepsRig) taskOf() (string, string, int) {
	r.t.Helper()
	var state, kind string
	var position int
	if err := r.st.Pool().QueryRow(context.Background(), `select state, running_kind, position from plan_tasks
		where issue = $1 and generation = 1 and task = 'a'`, r.key).Scan(&state, &kind, &position); err != nil {
		r.t.Fatalf("read %s's task a: %v", r.key, err)
	}
	return state, kind, position
}

// dispatchStatuses is each status the daemon wrote on the walk's issue in Dispatch, in order, as
// the issue's event log holds its writes under the daemon's session (dispatch.DaemonSession): each
// issue.updated, and the issue.closed Dispatch records for a write of done. A status written again
// in a row is read once.
func (r *ownerStepsRig) dispatchStatuses() ([]string, error) {
	var events []struct {
		Type  string `json:"type"`
		Actor struct {
			ID string `json:"id"`
		} `json:"actor"`
		Payload struct {
			Status string `json:"status"`
		} `json:"payload"`
	}
	if err := r.personRead("/api/v1/issues/"+r.key+"/events?order=asc&limit=200", &events); err != nil {
		return nil, err
	}
	var statuses []string
	for _, event := range events {
		if event.Type != "issue.updated" && event.Type != "issue.closed" || event.Actor.ID != dispatch.DaemonSession(r.key) || event.Payload.Status == "" {
			continue
		}
		if len(statuses) == 0 || statuses[len(statuses)-1] != event.Payload.Status {
			statuses = append(statuses, event.Payload.Status)
		}
	}
	return statuses, nil
}
