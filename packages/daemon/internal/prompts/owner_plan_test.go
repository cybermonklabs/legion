package prompts

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The owner of an issue picks which roles the steps of its one task name. Its launch's addressing
// lists the roles of the deployment, each with the type of its step (the daemon's
// DeploymentRolesFragment), so its composed prompt names no list of roles. The prompt names the
// owner's own acceptance step, the two operations that give and stop a plan, a stop's answers by
// their codes and that a stop before an issue's first step settles the plan that step would walk,
// the notices that a stopped task, a parent held before its retro and a plan of an earlier owner
// send it, the two stops a task-stopped notice tells of (a failed step's, and another architect's
// stop_task, whose notice names that architect) and the stop it is not sent for (its own), when a
// plan must be given and how a plan
// replaces the default steps once the work started, and the answers a later version gets from the
// workflow (workflow.planGiven): the running task's block, its text included, is refused and the
// unchanged block taken, each refusal names its cause and changes nothing so the same version can
// be given again, and an issue that started its work before Legion took plans takes none. A new
// version of `plan.md` names the token of the text it replaces, since Dispatch refuses a replacing
// upload that names none (envoy's PRECONDITION_REQUIRED), and a child out of the workflow takes a
// new plan only after its rerun, since a version for its done or left task is refused TASK_DONE
// (workflow.planGiven): the rerun, then stop_task, then register_plan. Each is held by its terms:
// a term alone where the term is the rule (a code, an operation, a role), and otherwise the terms
// of the clause that states the rule, in their order. Both architect compositions share that
// part, so both are read. This shows the text is loaded, never that an owner follows it.
func TestTheOwnersPromptListsTheRolesAndTheNewNotices(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tc := range []struct {
		name   string
		isRoot bool
	}{{"root architect", true}, {"sub-architect", false}} {
		t.Run(tc.name, func(t *testing.T) {
			composed := composedText(t, composer, claim.RoleArchitect, tc.isRoot)
			text := foldWhitespace(composed)
			for _, term := range []string{
				"acceptance:owner",
				"register_plan",
				"stop_task",
				"task-stopped",
				"retro-held",
				"plan-not-taken",
				// The hold a stop leaves and what ends or refuses it, by its contract terms: the
				// person's `todo`, the tool that refuses a release, its two refusal codes, and the
				// case of a task that never started, which waits for its gate instead.
				"`todo`",
				"retry_or_escalate",
				"`TASK_STOPPED`",
				"`HELD_FOR_CHILDREN`",
				"never started",
				// stop_task's answers by their codes, and the stop before an issue's first step,
				// which settles the plan that step would walk first.
				"`TASK_NOT_FOUND`",
				"`TASK_DONE`",
				"`TREE_LINGERING`",
				"`ISSUE_OUTSIDE_TREE`",
				"`TASK_STALE_GENERATION`",
				"`TASK_AWAITING_PERSON`",
				"settles the plan",
				// sign_off's answers when the record moved under it, by their codes.
				"`ISSUE_NOT_OWNED`",
				"`SIGNOFF_STALE_GENERATION`",
				"`SIGNOFF_STALE_ENTRY`",
				// A later version's answers by their codes.
				"`PLAN_VERSION_OLD`",
				"`PLAN_DOCUMENT_CHANGED`",
				"`PLAN_STALE_GENERATION`",
			} {
				if !strings.Contains(text, term) {
					t.Errorf("the composed %s prompt omits %q", tc.name, term)
				}
			}
			for _, terms := range [][]string{
				// A root's plan comes before its gate, and a child's before its release.
				{"register_plan", "register_gate"},
				{"register_plan", "release_children"},
				// An issue already at work runs the task default: stop it, then give the plan.
				{"stop_task", "default", "register_plan"},
				// A later version changes only a task that waits or is stopped, refuses a change
				// of the running task, and takes its block unchanged.
				{"later version", "waits", "stopped"},
				{"running task", "`PLAN_REFUSED`", "task <id> is running"},
				{"running task's block", "goes on"},
				// A refusal changes nothing, so the same version is given again, and an issue
				// that started before plans takes none.
				{"changes nothing", "given again"},
				{"started its work before Legion took plans", "`PLAN_REFUSED`"},
				// Another architect's stop names it in the notice; one's own stop sends none.
				{"`stop_task`", "`stoppedBy`"},
				{"no notice", "200", "`stop_task`"},
				// A task-stopped notice reaches the stopped issue's own architect, and the daemon
				// sets back an architect's status write on its own issue (record.OwnsUnder), so
				// that architect sends the end of that work to the architect above it rather than
				// writing a status the daemon sets back for it to write again.
				{"own issue's task stopped", "architect above it", "`envoy_publish`", "sets back"},
				// A new version of plan.md names the token of the text it replaces.
				{"`plan.md` exists", "--precondition-json", "`dispatch doc-read --issue <issue> --artifact plan.md`", "`400 PRECONDITION_REQUIRED`"},
				// A child out of the workflow takes a new plan after its rerun: stop, then give it.
				{"child out of the workflow", "only after `rerun_child`", "`TASK_DONE`"},
				{"after the rerun", "`stop_task`", "`register_plan`", "first step"},
			} {
				if !clauseHolds(composed, terms...) {
					t.Errorf("no clause of the composed %s prompt holds %q, in that order", tc.name, terms)
				}
			}
		})
	}
}

// An owner's plan can leave out the tester or the reviewer, so the merger verifies the steps its
// task's `Steps:` line names rather than a tester's and a reviewer's cycles. A task with no review
// step on a base whose rules require an approving review leaves a merge only a person or the owner
// can unblock, and Legion posts no approval of its own, so the merger waits and tells the task's
// owner which review is missing. This shows the text is loaded, never that a merger follows it.
func TestTheMergerWaitsForAMissingReview(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	text := foldWhitespace(composedText(t, composer, claim.RoleMerger, false))
	for _, phrase := range []string{
		"Verify that each step your task's Steps line names completed (today's line, on a task with no `Task:` line), and that the retro completed.",
		"If your task's Steps line names no reviewer step and the base's rules require an approving review that no step of this task gave, do not merge, do not approve the pull request yourself, and do not return the task. Wait, and tell the owner of the task which review is missing: the rule that requires it, and that the task's steps have no review step. Only the owner or a person can give that review or change the plan.",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("the composed merger prompt omits %q", phrase)
		}
	}
	if retired := "Verify tester and reviewer cycles completed"; strings.Contains(text, retired) {
		t.Errorf("the composed merger prompt still says %q", retired)
	}
}

// A merge of the base forward reaches the merge only once the reviewer approved the new head, and
// only where the merger's task has a reviewer step: a task with none stands on no review, so the
// daemon asks no reviewer (workflow's mergeForwardLanded) and the merger goes on at the new head.
// One composed prompt serves the merger of a task with a reviewer step and of one with none, so
// each block of it (blocks: a paragraph, or an item of a list) that has the reviewer read the merge or
// approve the new head names the reviewer step it holds for, and the prompt tells the merger of a
// task with none to go on at the new head. This shows the text is loaded, never that a merger
// follows it.
func TestTheMergerWaitsForTheApprovalOfAMergeOnlyOnATaskWithAReviewerStep(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	composed := composedText(t, composer, claim.RoleMerger, false)
	approving := 0
	for _, block := range blocks(composed) {
		if !strings.Contains(block, "approve the new head") && !strings.Contains(block, "reviewer's approval of it") &&
			!strings.Contains(block, "reads the merge's resolution") {
			continue
		}
		approving++
		if !strings.Contains(block, "reviewer step") {
			t.Errorf("the composed merger prompt has the reviewer read the merge or approve the new head with no reviewer step named: %q", block)
		}
	}
	if approving == 0 {
		t.Fatal("no block of the composed merger prompt has the reviewer approve the new head: the needles match nothing")
	}
	for _, phrase := range []string{
		"Where it has none, no approval holds the new head",
		"where it has none, go on at the new head",
	} {
		if !strings.Contains(foldWhitespace(composed), phrase) {
			t.Errorf("the composed merger prompt omits %q", phrase)
		}
	}
}

// On a task whose `Steps:` line names no reviewer, the implementer writes the two lines of the PR
// body's brief the reviewer writes on any other task. What it writes there is stated once, in *The
// brief for the human* of `skill://legion-worker/references/pr-body.md`, so the implementer's
// prompt names the case and points there, as the reviewer's prompt does. This shows the text is
// loaded, never that an implementer follows it.
func TestTheImplementerPointsToTheBriefOnATaskWithNoReviewer(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	text := foldWhitespace(composedText(t, composer, claim.RoleImplementer, false))
	pointer := "On a task whose `Steps:` line names no reviewer, also fill `Look at first` and `Not proven / risk` in the live PR body at each round (*The brief for the human* in that file)."
	if !strings.Contains(text, pointer) {
		t.Errorf("the composed implementer prompt omits %q", pointer)
	}
}

// On a task whose `Steps:` line names no reviewer, nobody adjudicates a bot's review thread, so the
// implementer's prompt gives it the duty, held here by its contract terms rather than its wording:
// the paragraph of the composed implementer prompt that names that case and bot threads names the
// retro among the completions it answers them before, `request_backward_move` to `implementing`
// for a thread whose fix the retro cannot push, and the completion's `summary`, in which it names
// each thread it declined to its architect. This shows the text is loaded, never that an
// implementer follows it.
func TestTheImplementerAnswersTheBotThreadsOnATaskWithNoReviewer(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var held []string
	for _, paragraph := range strings.Split(composedText(t, composer, claim.RoleImplementer, false), "\n\n") {
		if paragraph = foldWhitespace(paragraph); strings.Contains(paragraph, "names no reviewer") && strings.Contains(paragraph, "bot thread") {
			held = append(held, paragraph)
		}
	}
	if len(held) == 0 {
		t.Fatal("the composed implementer prompt holds no paragraph on bot threads on a task whose `Steps:` line names no reviewer")
	}
	for _, term := range []string{"retro", "`request_backward_move`", "`implementing`", "`summary`", "declined", "architect"} {
		if !slices.ContainsFunc(held, func(paragraph string) bool { return strings.Contains(paragraph, term) }) {
			t.Errorf("the composed implementer prompt's duty on bot threads on a task with no reviewer names no %s: %q", term, held)
		}
	}
}

// A catch-up that names an issue waiting in `accepting` leaves the root architect its acceptance.
// The daemon part states what to do, in one sentence: sign off a parent only after its last round,
// on the head a parent's sign-off needs, or send it back; the role part points to it rather than
// saying it again, so the composed root prompt holds the parent's head rule once. This shows the
// text is loaded, never that an architect follows it.
func TestTheRootsCatchUpStatesItsAcceptanceOnce(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	text := foldWhitespace(composedText(t, composer, claim.RoleArchitect, true))
	stated := "No worker holds that phase and the notice that moved it there may never have reached you, so read each one's evidence, then `sign_off` a parent only after its last round, on a head that the step before your acceptance passed and at which its checks pass (`skill://legion-architect/references/acceptance.md`, \"The last round\"), or send it back with `request_backward_move`."
	if !strings.Contains(text, stated) {
		t.Errorf("the composed root architect prompt omits %q", stated)
	}
	if n := strings.Count(text, "on a head that the step before your acceptance passed and at which its checks pass"); n != 1 {
		t.Errorf("the composed root architect prompt says the head a parent's sign-off needs %d times, want 1", n)
	}
}

// A planner step of an owner's plan adds no task block. The owner's plan and a planner's plan are
// one document, `plan.md`, and Legion carries out each task block of a version the owner gives, so
// a block the planner wrote there would become work of its own. The planner tells the two cases
// apart by the document: where `plan.md` holds a `plan_task` block with the id its `Task:` line
// names, an owner wrote that plan, and the planner changes no task block, adds none, and writes its
// plan below them as plain sections; where it holds none (the task `default`, or an issue with no
// `Task:` line), the planner writes each task as a `plan_task` block. The old rule, which wrote a
// block in every case, is gone. Each rule is held by the terms of the clause that states it, in
// their order. This shows the text is loaded, never that a planner follows it.
func TestThePlannerAddsNoTaskBlockToAnOwnersPlan(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	composed := composedText(t, composer, claim.RolePlanner, false)
	text := foldWhitespace(composed)
	for _, terms := range [][]string{
		// Read plan.md first, with the command that lists its blocks.
		{"`plan.md`", "first", "`dispatch doc-read --issue <issue> --artifact plan.md --blocks`"},
		// A block whose id the Task: line names is the owner's: change none, add none.
		{"`plan_task`", "`Task:`", "owner", "no `plan_task` block", "add none", "plain sections"},
		// With no such block, the planner writes its tasks as plan_task blocks.
		{"no such block", "`default`", "`Task:`", "`plan_task` block", "`plan.md`"},
	} {
		if !clauseHolds(composed, terms...) {
			t.Errorf("no clause of the composed planner prompt holds %q, in that order", terms)
		}
	}
	if retired := "Write each task of the plan as a `plan_task` block of `plan.md`"; strings.Contains(text, retired) {
		t.Errorf("the composed planner prompt still says %q, which adds a block to an owner's plan", retired)
	}
}

// The first report of a run an event ended is taken and moves nothing, with or without plan rows
// (workflow's leftItsPhase), so each phase worker is told to end its turn on it. A report of the
// step at which a task with plan rows was stopped is refused as late (intake's
// LateReportOfStoppedTask): the answer names no result and no cause, sends the agent to
// `dispatch steps`, and keeps the handoff record the report named as used, so each phase worker is
// told what TASK_STOPPED means and that a later run of its step writes a handoff of its own, where
// its phase writes one. Each
// duty is held by the terms of the clause that states it, in their order, so a rewording that
// keeps them passes. This shows the text is loaded, never that a worker follows it.
func TestAPhaseWorkerIsToldWhatALateReportsAnswerMeans(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	duties := [][]string{
		// The first report after an event ended the run is taken and moves nothing: end the turn.
		{"first report", "is no refusal", "taken", "moves nothing", "ended step's report", "end your turn"},
		// The report of the step a task was stopped at is refused, and the answer gives the stop.
		{"stopped", "`TASK_STOPPED`", "reason"},
		// It moved nothing, `dispatch steps` shows the run's end, and its handoff record is used,
		// so a later run writes a handoff of its own.
		{"moved nothing", "`dispatch steps --issue", "handoff record", "used", "later run", "handoff of its own"},
	}
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer} {
		composed := composedText(t, composer, role, false)
		for _, terms := range duties {
			if !clauseHolds(composed, terms...) {
				t.Errorf("no clause of the composed %s prompt holds %q, in that order", role, terms)
			}
		}
	}
}

// The two architect role parts name a step only where the issue's task holds it. An owner's plan
// can leave out the planner, the tester, the reviewer and the owner's own acceptance, so each
// sentence that named one of them as if every issue ran it now says where it runs and, where it
// does not, who holds its duty: the order of the starts is the order of the steps of the issue's
// plan; `accepting` and the sign-off are the owner's only where the task holds `acceptance:owner`;
// the move back names an earlier step's phase; a failed step goes back to the first step of the
// type implementation before it or to the earlier step its completion names. A parent with no `acceptance:owner` step gets no reading
// of its finished whole from its owner, and each part says so in one sentence where it speaks of a
// parent and its children. Each part gives `register_plan` and `stop_task` with the arguments of
// the daemon's routes. The words the parts carried before are gone from both, and from the
// architect's three daemon parts in go/, which Compose joins to them (architect-root.md or
// architect.md, then architect-common.md): a retired phrase there reaches its reader as surely as
// one in a role part. Each of the three is read by its name, so a part that is missing fails the
// read rather than passing with nothing read. This shows the text is loaded, never that an owner
// follows it.
func TestTheArchitectRolePartsNameOnlyTheStepsAnIssuesTaskRuns(t *testing.T) {
	// A parent with no acceptance step gets no reading of its whole: Legion holds it before its
	// retro (`retro-held`), so a parent whose work the owner wants to read is given the step. Held
	// by the terms of the one clause that says so, in their order.
	parentHeld := []string{"parent", "no step `acceptance:owner`", "`retro-held`", "the step `acceptance:owner`"}
	shared := []string{
		"`register_plan` (`issue`, `artifactId`, `version`)",
		"`stop_task` (`issue`, `task`)",
		"in the order of the steps of",
		"holds your step `acceptance:owner`",
		"A task with no `acceptance:owner` step has no `accepting`",
		"`to` the phase of an earlier step of the task, and `reason`",
		"is a gap that a tester step and a reviewer step name and neither refuses",
	}
	retired := []string{
		"fixed workflow table",
		"by the daemon's table",
		"the tester and the reviewer name and neither refuses",
		"`implementing` or `testing`",
		"at the approved head",
		"reviewed head",
		"the planner's `plan` handoff",
		"so its planner may already be reading it",
		"child's planner may already be working",
		"once the issue enters `accepting`",
		"since the reviewer approves only",
		"The reviewer's approval moves this child to `accepting`",
		"is mandatory after your acceptance",
		"Your last act is `sign_off` of this child in `accepting`",
		"a parent only on an approved head",
		"`implementing`, or `testing` where the task has a tester step",
	}
	for _, tc := range []struct {
		part string
		want []string
	}{
		{"architect-root.md", append(slices.Clone(shared), "each issue's `plan.md`", "before `register_gate`")},
		{"architect.md", append(slices.Clone(shared), "the agent of its first step may already be reading it", "The agent of your child's first step may already be working", "given it its plan with `register_plan`")},
	} {
		t.Run(tc.part, func(t *testing.T) {
			part := rolePart(t, tc.part)
			text := foldWhitespace(part)
			for _, phrase := range tc.want {
				if !strings.Contains(text, phrase) {
					t.Errorf("roles/%s omits %q", tc.part, phrase)
				}
			}
			if !clauseHolds(part, parentHeld...) {
				t.Errorf("no clause of roles/%s holds %q, in that order", tc.part, parentHeld)
			}
			for _, phrase := range retired {
				if strings.Contains(text, phrase) {
					t.Errorf("roles/%s still says %q", tc.part, phrase)
				}
			}
		})
	}
	for _, part := range []string{"architect-root.md", "architect.md", "architect-common.md"} {
		body, err := goParts.ReadFile("go/" + part)
		if err != nil {
			t.Fatalf("read the daemon part go/%s: %v", part, err)
		}
		text := foldWhitespace(string(body))
		for _, phrase := range retired {
			if strings.Contains(text, phrase) {
				t.Errorf("go/%s still says %q", part, phrase)
			}
		}
	}
}

// holdsAndRetires fails t for each phrase of want that the folded text of one part omits and for
// each phrase of gone that it still holds, naming the part.
func holdsAndRetires(t *testing.T, part, text string, want, gone []string) {
	t.Helper()
	text = foldWhitespace(text)
	for _, phrase := range want {
		if !strings.Contains(text, phrase) {
			t.Errorf("%s omits %q", part, phrase)
		}
	}
	for _, phrase := range gone {
		if strings.Contains(text, phrase) {
			t.Errorf("%s still says %q", part, phrase)
		}
	}
}

// An owner names the steps of its task in the order they run, and the one rule of that order is
// that the implementer's step stands before each step that reads its pull request
// (plan.Supported). The owner's acceptance reads a tester's handoff and a reviewer's approval only
// where that step stands before the acceptance, since a step after it has written nothing yet.
// Each part is read alone, never in the composition, so one part's words cannot stand for
// another's. This shows the text is loaded, never that an owner follows it.
func TestTheAgentsAreToldNoOrder(t *testing.T) {
	retiredEvidence := "the tester's handoff where the task has a tester step"
	for _, tc := range []struct {
		part       string
		read       func(*testing.T) string
		want, gone []string
	}{
		{
			"go/architect-common.md", func(t *testing.T) string { return goPart(t, "architect-common.md") },
			[]string{"Name the steps of a task in the order they run."},
			[]string{"in that order", retiredEvidence},
		},
		{
			"roles/architect.md", func(t *testing.T) string { return rolePart(t, "architect.md") },
			nil, []string{retiredEvidence},
		},
		{
			"roles/architect-root.md", func(t *testing.T) string { return rolePart(t, "architect-root.md") },
			nil, []string{retiredEvidence},
		},
	} {
		t.Run(tc.part, func(t *testing.T) {
			holdsAndRetires(t, tc.part, tc.read(t), tc.want, tc.gone)
		})
	}
}

// A move back goes to the phase of any earlier step of the issue's task, in the task's own order
// (workflow's backwardRows), so the daemon's parts name no fixed target for it. A head that
// conflicts with its base keeps the issue in its phase, which can be any phase after
// implementing, a planner's among them. A step reads a planner's plan only where a planner step
// stands before it in its task: a step before the planner has no plan to build from. Each part
// is read alone and by its name, so a missing part fails the read. This shows the text is
// loaded, never that an agent follows it.
func TestAMoveBackGoesToAnyEarlierStepInTheGoParts(t *testing.T) {
	toArchitect := []string{"`request_backward_move` with `issue`, `to` (the phase of an earlier step of the task) and `reason`"}
	retiredTo := []string{"`implementing`, or `testing` where the task has a tester step"}
	for _, tc := range []struct {
		part       string
		want, gone []string
	}{
		{
			"architect-common.md",
			[]string{
				"send it back with `request_backward_move` (`issue`, `to` the phase of an earlier step of the task, and `reason`)",
				"the issue stays in the phase the notice names, a phase after `implementing`",
			},
			[]string{
				"`implementing` or, where the task has a tester step, `testing`",
				"the phase the notice names (`testing`, `reviewing`, `accepting`, `retro` or `merging`)",
			},
		},
		{"architect.md", toArchitect, retiredTo},
		{"architect-root.md", toArchitect, retiredTo},
		{
			"worker-common.md",
			[]string{
				"each step after a planner step of its task, and each step of a task with no `Task:` line",
				"A step with no planner step before it in its task has no planner's plan",
			},
			[]string{"`Steps:` line names the planner", "`Steps:` line names no planner"},
		},
	} {
		t.Run(tc.part, func(t *testing.T) {
			holdsAndRetires(t, "go/"+tc.part, goPart(t, tc.part), tc.want, tc.gone)
		})
	}
}

// The architect's role parts say what its daemon parts say of a move back: it goes to the phase of
// any earlier step of the task. Each role part is read alone, and the composition of each
// architect is read whole and folded, since the old clause in a role part is cut by a line break
// and Compose joins the daemon parts to it. This shows the text is loaded, never that an architect
// follows it.
func TestAMoveBackGoesToAnyEarlierStepInTheRoleParts(t *testing.T) {
	moveBack := "send it back with `request_backward_move` (`issue`, `to` the phase of an earlier step of the task, and `reason`)"
	retired := "`implementing`, or `testing` where the task has a tester step"
	for _, part := range []string{"architect.md", "architect-root.md"} {
		t.Run(part, func(t *testing.T) {
			holdsAndRetires(t, "roles/"+part, rolePart(t, part), []string{moveBack}, []string{retired})
		})
	}
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, isRoot := range []bool{true, false} {
		t.Run(fmt.Sprintf("composed, root %t", isRoot), func(t *testing.T) {
			holdsAndRetires(t, fmt.Sprintf("the composed architect prompt (root %t)", isRoot),
				composedText(t, composer, claim.RoleArchitect, isRoot), nil, []string{retired})
		})
	}
}

// A step reads the reports of the steps before it in its task, and nothing of a step that comes
// after it: a planner's plan exists for a step only where a planner step stands before it, and a
// tester's handoff is read by the steps after the tester. A step after the reviewer can send the
// work back, so an approval does not end the change. Each part is read alone, and the composition
// of each step that builds from a plan is read whole, since Compose joins go/worker-common.md into
// it. This shows the text is loaded, never that a worker follows it.
func TestEachStepIsToldTheStepsAroundItInAnyOrder(t *testing.T) {
	planRead := []string{
		"where a planner step comes before yours, or your task has no `Task:` line, it is the issue's `plan.md`",
		"where no planner step comes before yours, it is your task's `Task text:`",
	}
	planRetired := []string{
		"`Steps:` line names a planner,", "where the line names no planner", "`Steps:` line names no planner has no plan handoff",
		"A task with no planner step before yours has no plan handoff for you",
	}
	for _, tc := range []struct {
		part       string
		want, gone []string
	}{
		{
			"tester.md",
			append(slices.Clone(planRead),
				"each step after yours reads what you wrote, and so does the retro",
				"Read the implementation handoff at the version that the `Step <j> (implementer) reported:` line of your task names"),
			append(slices.Clone(planRetired),
				"the reviewer where your task has a reviewer step",
				"Read the implementation handoff at the version your task's `Handoff:` line names"),
		},
		{"core/tester.md", []string{"so a defect comes back before the steps after yours run"}, []string{"so a defect comes back before the review"}},
		{
			"reviewer.md",
			append(slices.Clone(planRead),
				"After your approval, a later step of your task can send the work back, and the new head then comes to your review again"),
			append(slices.Clone(planRetired),
				"After approval, no implementation or further review change may happen",
				"the head changes only when a later step of your task sends the work back",
				"nothing of this tree pushes above it",
				"so the merger merges the head you approved last",
				"the daemon asks you to approve that head only where the merge lands in `accepting`, `retro` or `merging`"),
		},
		{
			"implementer.md",
			append(slices.Clone(planRead),
				"The steps after yours then run on that head, the tester's among them where your task has one"),
			append(slices.Clone(planRetired), "so the tester runs that head",
				"the plan handoff's `requiredSkills` for your role, where a planner step comes before yours"),
		},
		{
			"core/planner.md",
			[]string{"Plan the assigned issue completely for each step that follows yours", "the plan goes to the steps after yours with the choice unmade", "a gap the steps after yours inherit"},
			[]string{"completely for implementation, for each step", "the plan goes to the implementer", "a gap the implementation inherits"},
		},
	} {
		t.Run(tc.part, func(t *testing.T) {
			holdsAndRetires(t, "roles/"+tc.part, rolePart(t, tc.part), tc.want, tc.gone)
		})
	}
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer} {
		t.Run("composed "+string(role), func(t *testing.T) {
			holdsAndRetires(t, "the composed "+string(role)+" prompt", composedText(t, composer, role, false), nil,
				[]string{"`Steps:` line names a planner", "`Steps:` line names the planner", "`Steps:` line names no planner"})
		})
	}
}

// The owner's sign-off moves the task to its next step, and to the retro only where the acceptance
// is the task's last step (workflow's signOff); the retro starts once the task's last step passed,
// whichever step that is. Each daemon part is read alone, by its name. This shows the text is
// loaded, never that an agent follows it.
func TestSignOffMovesTheTaskToItsNextStepInTheGoParts(t *testing.T) {
	moves := "it moves the task to its next step, or to `retro` where your acceptance is its last step"
	for _, tc := range []struct {
		part       string
		want, gone []string
	}{
		{"architect-common.md", []string{"and moves the task to its next step, or to `retro` where your acceptance is its last step"}, []string{"and moves the issue to `retro`"}},
		{"architect.md", []string{moves}, []string{"it moves the issue to `retro`"}},
		{"architect-root.md", []string{moves}, []string{"it moves the issue to `retro`"}},
		{"implementer.md", []string{"once the task's last step passed, whichever step that is"}, []string{"the architect's acceptance where the task has the step `acceptance:owner`"}},
	} {
		t.Run(tc.part, func(t *testing.T) {
			holdsAndRetires(t, "go/"+tc.part, goPart(t, tc.part), tc.want, tc.gone)
		})
	}
}

// The architect's role parts say what its daemon parts say of a sign-off: it moves the task to its
// next step, and to the retro only where the acceptance is the task's last step, so the retro
// starts once the task's last step passes and the steps after the acceptance run without the
// owner. Each role part is read alone, and each architect's composition is read whole and folded,
// since the daemon parts join it. This shows the text is loaded, never that an architect follows
// it.
func TestSignOffMovesTheTaskToItsNextStepInTheRoleParts(t *testing.T) {
	moves := "which moves the task to its next step, or to retro where your acceptance is its last step"
	for _, tc := range []struct {
		part       string
		want, gone []string
	}{
		{
			"architect.md",
			[]string{
				moves,
				"the daemon starts the implementer on it once the task's last step passes",
				"you start none of the steps after your acceptance in its task, its retro or its merge",
				"a later step that sends the work back to a step before your acceptance brings the child to your acceptance again",
			},
			[]string{"which moves the child to retro", "once you sign off", "your last act is `sign_off`", "then run without you"},
		},
		{"architect-root.md", []string{moves}, []string{"(which moves it to retro)"}},
	} {
		t.Run(tc.part, func(t *testing.T) {
			holdsAndRetires(t, "roles/"+tc.part, rolePart(t, tc.part), tc.want, tc.gone)
		})
	}
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, isRoot := range []bool{true, false} {
		t.Run(fmt.Sprintf("composed, root %t", isRoot), func(t *testing.T) {
			holdsAndRetires(t, fmt.Sprintf("the composed architect prompt (root %t)", isRoot),
				composedText(t, composer, claim.RoleArchitect, isRoot), nil,
				[]string{"and moves the issue to `retro`", "which moves the child to retro", "(which moves it to retro)"})
		})
	}
}

// `accepting`, `retro` and `merging` hold no slot wherever the task places the acceptance, so no
// part says the review comes before them (record.InSlotlessPhase reads the phase, never the
// review's place). A parent is held before its retro wherever its last step is not the owner's
// acceptance, and a planner's plan goes to the steps after it, whichever they are. What a merge
// forward asks in those phases is TestTheMergeForwardPartsReadTheTasksOwnSteps's. Each part is read
// alone, by its name. This shows the text is loaded, never that an agent follows it.
func TestNoPartPutsTheAcceptancePastTheReview(t *testing.T) {
	for _, tc := range []struct {
		part       string
		read       func(*testing.T) string
		want, gone []string
	}{
		{
			"go/architect-common.md", func(t *testing.T) string { return goPart(t, "architect-common.md") },
			[]string{
				"a stop in `accepting`, `retro` or `merging` holds none, and a stop in any other phase keeps the slot it holds",
				"a parent whose task passed its last step, which is not your `acceptance:owner` step",
				"It moves the issue on to its task's next step, `accepting` where your step `acceptance:owner` comes next.",
			},
			[]string{"past its review", "Past review", "a stop before `accepting` keeps it", "no `acceptance:owner` step to wait in"},
		},
		{
			"go/worker-common.md", func(t *testing.T) string { return goPart(t, "worker-common.md") },
			nil,
			[]string{"Past review"},
		},
		{
			"roles/core/reviewer.md", func(t *testing.T) string { return rolePart(t, "core/reviewer.md") },
			nil,
			[]string{"past its review"},
		},
		{
			"roles/planner.md", func(t *testing.T) string { return rolePart(t, "planner.md") },
			[]string{"hands the choice to the steps after yours, unseen"},
			[]string{"hands the choice to whoever implements it"},
		},
	} {
		t.Run(tc.part, func(t *testing.T) {
			holdsAndRetires(t, tc.part, tc.read(t), tc.want, tc.gone)
		})
	}
}

// Each sentence that the acceptance of these words showed false says what the daemon does. The
// daemon takes a move back only from the role that works the issue's phase (workflow's
// backwardRefusal), so the architect asks that role for one outside `accepting`; the merger's own
// move back is the other way out of `merging`, and a merged child waits there for the issues
// beneath it; the implementer resolves the threads the reviewer accepted; and the sentence for a
// step with no planner step before it stands outside the paragraph that is for the steps working
// from a planner's plan. Each part is read alone, by its name. This shows the text is loaded, never
// that an agent follows it.
func TestTheSentencesTheAcceptanceShowedFalseSayWhatTheDaemonDoes(t *testing.T) {
	for _, tc := range []struct {
		part       string
		want, gone []string
	}{
		{
			"architect-common.md",
			[]string{
				"ask the role that works the ancestor's phase, the implementer in `retro` or the merger in `merging`, to take it back with `request_backward_move`",
				"The daemon takes a move back only from the role that works the issue's phase, so yours is refused outside `accepting`.",
				"so ask the reviewer, which works `reviewing`, to send the issue back with `request_backward_move` to `implementing` and the reason",
				"a red required check is the implementer's, so ask the reviewer to send the issue back with `request_backward_move` to `implementing`",
			},
			[]string{
				"Take the ancestor back with `request_backward_move`",
				"both of which take a push, so `request_backward_move` to `implementing` with the reason",
				"a red required check is the implementer's, `request_backward_move` to `implementing`",
			},
		},
		{
			"merger.md",
			[]string{
				"That is the one move forward out of `merging`, beside your own `request_backward_move` back",
				"a child with an issue beneath it still in the workflow stays there until the last of them is done or parked",
			},
			[]string{"That is the one move out of `merging`", "and closes then; a child closes at once"},
		},
		{"reviewer.md", []string{"the implementer resolves the threads you tell it you accepted"}, []string{"resolve the ones you answered"}},
	} {
		t.Run(tc.part, func(t *testing.T) {
			holdsAndRetires(t, "go/"+tc.part, goPart(t, tc.part), tc.want, tc.gone)
		})
	}
	t.Run("worker-common.md", func(t *testing.T) {
		const scope = "the rest of this paragraph is for the steps that work from it"
		const noPlanner = "A step with no planner step before it in its task has no planner's plan"
		text := goPart(t, "worker-common.md")
		scoped := 0
		for _, paragraph := range strings.Split(text, "\n\n") {
			if !strings.Contains(paragraph, scope) {
				continue
			}
			scoped++
			if strings.Contains(paragraph, noPlanner) {
				t.Errorf("go/worker-common.md: the paragraph that says %q also holds %q, which is for the steps outside it", scope, noPlanner)
			}
		}
		if scoped != 1 {
			t.Errorf("go/worker-common.md: %d paragraphs say %q, want 1", scoped, scope)
		}
		holdsAndRetires(t, "go/worker-common.md", text, []string{noPlanner}, nil)
	})
}

// Each sentence the acceptance's last round showed false agrees with what the daemon and the other
// parts say. An approval of a head GitHub holds blocked leaves the issue in `reviewing` (workflow's
// reviewRound, classify.RoundWaitsOnMergeState), so the architect's part no longer says the
// approval ends the round whatever CI says. The reviewer waits, before it approves, for a required
// check still running and for a `ci_check` run, so its part says so where it says the daemon does
// the rest of the waiting. The reviewer pushes nothing (roles/reviewer.md), so the shared update
// and push steps say they are not its own; the merger pushes its merge of the base. The production
// check, opened once the pull request merges, records an `after_merge` result (workflow's openCheck
// and checkTask), so the root's last round asks none. Each part is read alone, by its name. This
// shows the text is loaded, never that an agent follows it.
func TestTheSentencesTheLastAcceptanceRoundShowedFalseAgreeWithTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		part       string
		read       func(*testing.T) string
		want, gone []string
	}{
		{
			"go/architect-common.md", func(t *testing.T) string { return goPart(t, "architect-common.md") },
			[]string{"An approved head that GitHub holds blocked, on a check or a review the base requires, keeps the issue in `reviewing` until that merge state comes clean."},
			[]string{"ends the round whatever CI says"},
		},
		{
			"go/reviewer.md", func(t *testing.T) string { return goPart(t, "reviewer.md") },
			[]string{
				"Before you approve, you wait only for what the rules above name, a required check still `queued` or `in_progress` and a `ci_check` run still going; the daemon does the rest of the waiting",
				"So once your read finds the head's required checks and its `ci_check` runs concluded, there is nothing more of CI to wait for or decide.",
			},
			[]string{"You wait for none of them", "So once you have read the head's checks there is nothing of CI to wait for or decide."},
		},
		{
			"go/worker-common.md", func(t *testing.T) string { return goPart(t, "worker-common.md") },
			[]string{
				"Do its five steps in that turn, in this order; the reviewer creates no commit and pushes nothing, so each commit and push that the steps and this paragraph name is not its own, and the merger pushes the merge of the base it made.",
				"Where your role pushes (the reviewer pushes nothing), push the issue branch yourself",
			},
			[]string{"Push the issue branch yourself", "the reviewer and the merger create no commit and push nothing", "the reviewer and the merger push nothing"},
		},
		{
			"roles/architect-root.md", func(t *testing.T) string { return rolePart(t, "architect-root.md") },
			[]string{"no `after_merge` result is asked there, since the production check records it after the close"},
			[]string{"for an `after_merge` check, on the branch the root's pull request merged into"},
		},
	} {
		t.Run(tc.part, func(t *testing.T) {
			holdsAndRetires(t, tc.part, tc.read(t), tc.want, tc.gone)
		})
	}
}

// The daemon does none of the merger's reads for it, though it reads the head, the reviews and the
// checks for its own record (reviewRound, tellChecks). The reviewer's review ends its round with or
// without its completion (workflow's reviewRound). A completion that comes after the review is
// recorded on the round the review ended (reportEndedStep). Each part is read alone, by its name.
// This shows the text is loaded, never that an agent follows it.
func TestThePartsSayWhatEndsAPhaseAndWhatTheDaemonReads(t *testing.T) {
	for _, tc := range []struct {
		part       string
		want, gone []string
	}{
		{
			"merger.md",
			[]string{"It makes none of your reads of the head you merge: its checks, and the reviewer's approval where your task has a reviewer step."},
			[]string{"it reads neither the head you merge, nor its checks, nor the reviewer's approval"},
		},
		{
			"reviewer.md",
			[]string{
				"Your review ends the round whether or not your completion has come: a `REQUEST_CHANGES` at once, and an approval once GitHub reports the approved head one the issue may advance on",
				"a completion that comes after it is recorded on the round it ended, so complete all the same.",
			},
			[]string{"Your approval does not end the round on its own", "whichever of the two is second"},
		},
	} {
		t.Run(tc.part, func(t *testing.T) {
			holdsAndRetires(t, "go/"+tc.part, goPart(t, tc.part), tc.want, tc.gone)
		})
	}
}

// The fresh readers of the newest head showed more sentences false, and each says now what the
// daemon does. In `accepting` no worker holds the phase (RoleFor gives the architect), though a
// merge forward asks the implementer and the reviewer for work there. The architect's move back is
// refused outside `accepting` (backwardRefusal), so it asks the role that works an ancestor's phase.
// A tester's fail goes to the step its `return_to` names. Besides a pending report's `wake_at`,
// the bound and the retry of an unreported look start a look (check.go). A turn's working copy can
// hold another role's edits, a tester may build from a copy outside the workspace, a session is
// kept past the close for a production check, the legion tool has operations beside the three, a
// root holds its slot only through its wait on a person, and the merge of a root whose tree runs,
// or of a child with issues beneath it in the workflow, does not write `done` at once. Each part
// is read alone, by its name. This shows the text is loaded, never that an agent follows it.
func TestTheSentencesTheReadersOfTheNewestHeadShowedFalseSayWhatTheDaemonDoes(t *testing.T) {
	const resumed = "holds edits made before this turn, your own resumed ones or another role's"
	const onlyYours = "holds only your own resumed edits"
	for _, tc := range []struct {
		part       string
		read       func(*testing.T) string
		want, gone []string
	}{
		{
			"go/architect-common.md", func(t *testing.T) string { return goPart(t, "architect-common.md") },
			[]string{
				"where no worker holds the phase and the acceptance is yours",
				"ask the role that works the ancestor's phase, the implementer in `retro` or the merger in `merging`, to take the ancestor back with `request_backward_move`",
			},
			[]string{"where no worker runs", "; take the ancestor back with `request_backward_move`"},
		},
		{
			"go/architect-root.md", func(t *testing.T) string { return goPart(t, "architect-root.md") },
			[]string{"No worker holds that phase"}, []string{"No worker runs there"},
		},
		{
			"go/architect.md", func(t *testing.T) string { return goPart(t, "architect.md") },
			[]string{"No worker holds that phase"}, []string{"No worker runs there"},
		},
		{
			"go/tester.md", func(t *testing.T) string { return goPart(t, "tester.md") },
			[]string{
				"A `pending` report on a look before the last also names `wake_at`",
				"Besides it, only the bound's last look and the retry of a look nobody reported start one",
			},
			[]string{"Nothing else starts one."},
		},
		{"go/worker-common.md", func(t *testing.T) string { return goPart(t, "worker-common.md") }, []string{resumed}, []string{onlyYours}},
		{"roles/implementer.md", func(t *testing.T) string { return rolePart(t, "implementer.md") }, []string{resumed}, []string{onlyYours}},
		{"roles/planner.md", func(t *testing.T) string { return rolePart(t, "planner.md") }, []string{resumed}, []string{onlyYours}},
		{
			"roles/tester.md", func(t *testing.T) string { return rolePart(t, "tester.md") },
			[]string{resumed, "the daemon sends the issue back to the first implementation step before yours, or to the earlier step your completion's `return_to` names"},
			[]string{onlyYours, "the daemon sends the issue back to the implementer."},
		},
		{
			"roles/mechanics/headless.md", func(t *testing.T) string { return rolePart(t, "mechanics/headless.md") },
			[]string{
				"`LEGION_WORKSPACE` is the only workspace you change, and a copy outside it that a tester builds from between assignments changes nothing there",
				"Your session is kept until your issue closes, and past the close for a production check that comes to it",
				"`request_backward_move`, and the operations this prompt names for one case, such as `ready_for_update`",
			},
			[]string{"is the only place you work", "and `request_backward_move` — and nothing of yours runs"},
		},
		{
			"roles/architect.md", func(t *testing.T) string { return rolePart(t, "architect.md") },
			[]string{"where no worker holds the phase and the acceptance is yours"}, []string{"where no worker runs"},
		},
		{
			"roles/architect-root.md", func(t *testing.T) string { return rolePart(t, "architect-root.md") },
			[]string{
				"Your root holds its admission slot throughout that wait",
				"except that a root whose tree still runs, and a child with an issue beneath it still in the workflow, first wait in `merging` for those issues",
				"the production check, where the project opens one, follows in the tester's own session",
			},
			[]string{"and the tester's production check follows in its own session"},
		},
	} {
		t.Run(tc.part, func(t *testing.T) {
			holdsAndRetires(t, tc.part, tc.read(t), tc.want, tc.gone)
		})
	}
}

// The fifth round's readers showed five more sentences false. In `accepting` no worker holds the
// phase, so the reviewer of a merge forward tells no phase's worker there
// (TestTheMergeForwardPartsReadTheTasksOwnSteps pins that sentence now);
// the architect accepts no head whose code it reads red, which a re-run of a flaky check is not;
// only the implementer fixes a defect, and the other roles send it to the implementer; a
// production-check look comes to the implementer as a task that names no phase; and the
// completion reads the handoff's `head`. Each part is read alone, by its name. This shows the text
// is loaded, never that an agent follows it.
func TestTheSentencesTheFifthRoundsReadersShowedFalseSayWhatTheDaemonDoes(t *testing.T) {
	for _, tc := range []struct {
		part       string
		read       func(*testing.T) string
		want, gone []string
	}{
		{
			"go/architect-common.md", func(t *testing.T) string { return goPart(t, "architect-common.md") },
			[]string{
				"So does your own read before you accept: you accept no head whose code you read red, and send it back with `request_backward_move`.",
			},
			[]string{"to tell you and the phase's worker what it decided", "a red head is one you send back"},
		},
		{
			"go/implementer.md", func(t *testing.T) string { return goPart(t, "implementer.md") },
			[]string{"every task it sends for them names one", "the look of a production check still yours, below, comes as a task that names no phase"},
			[]string{"and every task it sends names one"},
		},
		{
			"go/worker-common.md", func(t *testing.T) string { return goPart(t, "worker-common.md") },
			[]string{"nothing checks the body's shape beyond its being a JSON object and the `head` the completion reads"},
			[]string{"beyond its being a JSON object —"},
		},
		{
			"roles/mechanics/headless.md", func(t *testing.T) string { return rolePart(t, "mechanics/headless.md") },
			[]string{"the implementer fixes it, and any other role sends it to the implementer with its evidence"},
			[]string{"Fix a clear defect in code this tree touches or depends on in this tree's own pull request"},
		},
	} {
		t.Run(tc.part, func(t *testing.T) {
			holdsAndRetires(t, tc.part, tc.read(t), tc.want, tc.gone)
		})
	}
}

// A merge forward reads the task's own steps (workflow's mergeForwardLanded and
// mergeForwardOnEntry, classify.MergeComesNext), joined with cybermonklabs/legion#4979's rule that
// the merger merges the base forward in `merging`. The reviewer is asked to approve the new head
// only where its step already ran, and a reviewer step still to come reviews that head when it
// runs. Where every step of the task has run - `retro`, and `accepting` only where no step of the
// task follows the acceptance - a conflict waits for the merger, whatever the base requires; in
// `accepting` where a step follows, the implementer is asked as in `testing` and `reviewing`.
// Each part is read alone, by its name. This shows the text is loaded, never that an agent follows
// it.
func TestTheMergeForwardPartsReadTheTasksOwnSteps(t *testing.T) {
	for _, tc := range []struct {
		part       string
		read       func(*testing.T) string
		want, gone []string
	}{
		{
			"go/architect-common.md", func(t *testing.T) string { return goPart(t, "architect-common.md") },
			[]string{
				"The implementer is asked before `merging`, and the merger in `merging`, whose merge it is.",
				"`merge-next`: the issue stands in `merging`.",
				"Where every step of the task has run, in `retro` or in `accepting` with no step of its task after yours, the conflict waits for the merger whatever the base requires",
				"The reviewer tells you what it decided, and tells the phase's worker too where a worker holds the phase.",
				"A reviewer step after yours reviews the new head when it runs.",
				// The round that waits on GitHub's merge state, read at a conflicting head.
				"an approval of a head whose conflict is confirmed then ends the round, the task's later steps go on at that head, and once the issue enters `merging` the daemon asks the merger to merge the base forward, with a `checks-red` notice.",
			},
			[]string{
				"the issue stands in `accepting`, `retro` or `merging` (`merge-next`)",
				"the daemon acts once the issue moves into `accepting`, `retro` or `merging`",
				"In `accepting`, `retro` or `merging`, where the task has a reviewer step",
				"The daemon acts once the issue reaches a phase where the merge comes next.",
				"the implementer before the review, and in `merging` the merger",
				"in `accepting` and `retro` the conflict waits for the merger",
				// The round that waits on GitHub's merge state, as the sentence stood before.
				"once the issue moves into `accepting`, `retro` or `merging`, the daemon asks the implementer to merge the base forward",
				"once the issue moves past its review, into `accepting`, `retro` or `merging`",
			},
		},
		{
			"go/worker-common.md", func(t *testing.T) string { return goPart(t, "worker-common.md") },
			[]string{
				"It acts only when the next step needs a mergeable head, which is so in four cases. The issue stands in `merging`.",
				"Where every step of the task has run, in `retro` or in `accepting` with no step of the task after it, the merge comes next, so the conflict waits for the merger whatever the base requires.",
				"A reviewer step still to come reviews the new head when it runs.",
			},
			[]string{
				"the issue stands in `accepting`, `retro` or `merging`;",
				"In `accepting`, `retro` or `merging`, where the task has a reviewer step",
				"The merge comes next: the issue stands in `retro` or `merging`",
				"In `accepting` and `retro` the work is past its review and the merge comes next",
			},
		},
		{
			"roles/core/reviewer.md", func(t *testing.T) string { return rolePart(t, "core/reviewer.md") },
			[]string{"When a merge of the base lands after your step ran and no review round is open"},
			[]string{"while the issue stands in `accepting`, `retro` or `merging`", "while the issue stands past its review (`accepting`, `retro` or `merging`)"},
		},
	} {
		t.Run(tc.part, func(t *testing.T) {
			holdsAndRetires(t, tc.part, tc.read(t), tc.want, tc.gone)
		})
	}
}

// An architect's move back counts no review round: only a review's request for changes, or the
// fail of a step of the type review, counts one (workflow's recordRound, held by
// TestAFailedStepReturnsAndNoCountStopsATask). A move back at a conflicting head counts no
// conflict round either, since a conflict round is a review round. So no part says the move
// counts either. Each part is read alone, by its name. This shows the text is loaded, never that
// an owner follows it.
func TestTheArchitectsMoveBackCountsNoReviewRound(t *testing.T) {
	retired := []string{
		"counts a review round",
		"counting a review round",
		"or a conflict round where the record holds the head conflicting with its base",
		"or a conflict round, which `review_round_cap` leaves out",
	}
	for _, part := range []string{"architect-common.md", "architect-root.md", "architect.md"} {
		t.Run(part, func(t *testing.T) {
			holdsAndRetires(t, "go/"+part, goPart(t, part), nil, retired)
		})
	}
}

// The plan review's loop resolves each blocking issue in the plan and sends a repeated finding to
// the architect, so the open-decision paragraph says a check's finding is no decision to wait on,
// never that a finding does not block the plan. The planner part is read alone, by its name. This
// shows the text is loaded, never that an agent follows it.
func TestThePlannerResolvesACheckFindingInThePlan(t *testing.T) {
	holdsAndRetires(t, "roles/planner.md", rolePart(t, "planner.md"),
		[]string{
			"A check's finding is not one: resolve it in the plan by the loop above, and send it to the architect only when a round repeats it.",
			"A rejection of the goal or of a requirement is never one you proceed past, as above.",
		},
		[]string{"a check's other findings never block the plan"})
}

// Each sentence that held only in the default steps (planner, implementer, tester, reviewer, then
// the owner's acceptance) holds for every order the plan validator allows (plan.Supported): the
// planner at any place, the tester, the reviewer and the acceptance in any order after the
// implementer, and each of them left out. A planner after the implementer plans no code that
// came before it; a task with no reviewer step costs no review and names no reviewer's approval;
// a reviewer or an implementer can be the task's last step, which moves the task to retro, not to
// a next step (workflow's any-order steps); a step after the reviewer can push; only the step
// right after the planner is told its plan by a `Handoff:` line, the others by a `reported:` line
// (workflow's task text); and the architect's sign_off moves a phase too. Each part is read alone,
// by its name, and each old sentence is gone. This shows the text is loaded, never that an agent
// follows it.
func TestTheSentencesTrueOnlyInTheDefaultStepsHoldForEveryOrder(t *testing.T) {
	for _, tc := range []struct {
		part       string
		read       func(*testing.T) string
		want, gone []string
	}{
		{
			"roles/core/planner.md", func(t *testing.T) string { return rolePart(t, "core/planner.md") },
			[]string{
				"An implementer step after yours must prove the change on that surface before its phase completes. A tester step after yours must be able to re-run what you named.",
				"For four kinds of requirement it names these, before any code where an implementer step follows yours.",
			},
			[]string{"since the implementer must prove the change on that surface", "it names these before any code, because"},
		},
		{
			"roles/implementer.md", func(t *testing.T) string { return rolePart(t, "implementer.md") },
			[]string{
				"A later round costs a CI pass, and another review where your task has a reviewer step.",
				"The steps after yours then run on that head, the tester's among them where your task has one.",
			},
			[]string{"because a later round costs a CI pass and another review", "so the steps after yours, the tester's among them, run on that head"},
		},
		{
			"roles/reviewer.md", func(t *testing.T) string { return rolePart(t, "reviewer.md") },
			nil,
			[]string{"no step of Legion's moves the head after your approval but those two"},
		},
		{
			"roles/architect.md", func(t *testing.T) string { return rolePart(t, "architect.md") },
			[]string{"Where your issue's task has your `acceptance:owner` step, its sign-off then waits for its last round."},
			[]string{"Your issue's sign-off then waits for its last round, whose head"},
		},
		{
			"roles/architect-root.md", func(t *testing.T) string { return rolePart(t, "architect-root.md") },
			[]string{"Before `merging`, from the task's first step through retro, register with `register_gate` each step your addressing line names."},
			[]string{"Before `merging` (planning through retro)"},
		},
		{
			"go/architect-common.md", func(t *testing.T) string { return goPart(t, "architect-common.md") },
			[]string{
				"Where the review is the task's last step, it starts the retro, or holds a parent before it (`retro-held`, above).",
				"Once it is open, the daemon records it and moves the issue to the task's next step. Where the implementer's step is the task's last, it starts the retro, or holds a parent before it.",
			},
			[]string{"moves the issue on to its task's next step with no further notice", "moves the issue to the next step of its task once it is open"},
		},
		{
			"go/implementer.md", func(t *testing.T) string { return goPart(t, "implementer.md") },
			[]string{
				"The task's next step, or its retro where yours is the last step, starts only after that round's `handoff_complete`.",
				"The completion is applied, and the task moves on to its next step, or toward its retro where yours is the last step.",
			},
			[]string{"and the task's next step starts only after that round's", "the completion is applied and the issue moves to the task's next step"},
		},
		{
			"go/merger.md", func(t *testing.T) string { return goPart(t, "merger.md") },
			[]string{
				"the reviewer's approval where your task has a reviewer step.",
				"When your push lands, it tells you the old head and the new one, and, where your task has a reviewer step, asks the reviewer to read the merge and approve the new head.",
			},
			[]string{"its checks and the reviewer's approval: the merge is yours", "it tells you the old head and the new one and asks the reviewer to read the merge"},
		},
		{
			"go/worker-common.md", func(t *testing.T) string { return goPart(t, "worker-common.md") },
			[]string{
				"and the architect's `sign_off`.",
				"So the plan version your task names, in a `Handoff:` or a `reported:` line, is a receipt for the record the daemon accepted, not the plan.",
			},
			[]string{"`request_backward_move` and from the GitHub events", "so your task's `Handoff: plan version <n>` line is the receipt"},
		},
	} {
		t.Run(tc.part, func(t *testing.T) {
			holdsAndRetires(t, tc.part, tc.read(t), tc.want, tc.gone)
		})
	}
}
