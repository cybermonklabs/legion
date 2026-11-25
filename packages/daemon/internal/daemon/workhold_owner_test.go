package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	planwords "github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The hold over an owner's plan (t-17's rows on main's code): a plan whose first step is not
// planning (H37), the starts a move back and a restart of a stopped task write (H38), what the
// owner's acceptance starts (H43), and the architect's own acceptance, which no notice and no
// catch-up asks for while the hold stands (H45, decision 21). Each runs the daemon's engine,
// admission and outbox over the work-hold rig (newWorkHoldRig).

// ownersChild is the child of LEGION-208 the rows that need one stand it in.
const ownersChild = "LEGION-209"

// acceptanceAsked is the clause of a catch-up's reason that asks its architect to accept
// LEGION-208, waiting in accepting. It is written out here, never read from the code under test.
const acceptanceAsked = "; in accepting, waiting for your acceptance, which no worker gives: LEGION-208 " +
	"(read the evidence, then sign_off or request_backward_move)"

// tokenOf is the token of key's role in the rig's project.
func (r *workHoldRig) tokenOf(key string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.tokenProject, key, role)
	if err != nil {
		r.t.Fatalf("the %s token of %s: %v", role, key, err)
	}
	return token
}

// branchMade takes key's branch as made: the rig serves no GitHub, so the issue_branch row a
// child's first phase queues, which its roles' starts wait behind, is finished by hand, as its
// executor finishes it once GitHub holds the branch.
func (r *workHoldRig) branchMade(key string) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.ctx, `delete from outbox where issue = $1 and kind = $2`, key, string(record.OutboxKindIssueBranch)); err != nil {
		r.t.Fatalf("finish the branch row of %s: %v", key, err)
	}
}

// planned writes key's plan at generation 1, as a walk leaves it: version 2 of plan-artifact, given
// by the tree's architect, with one task a whose steps are words (`role` or `role:key`), standing
// in state at place (a task waiting stands at no act).
func (r *workHoldRig) planned(key string, place record.TaskPlace, state record.TaskState, words ...string) {
	r.t.Helper()
	roles := planwords.BuiltinRoles()
	steps := make([]record.PlanStep, len(words))
	for i, word := range words {
		role, agent, _ := strings.Cut(word, ":")
		steps[i] = record.PlanStep{Issue: key, Generation: 1, Task: "a", Position: i + 1, Role: role, Type: roles[role].Type, AgentKey: agent}
	}
	version := record.PlanVersion{Issue: key, Generation: 1, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
		GivenBy: r.token(claim.RoleArchitect), GivenAt: r.clock.Now()}
	task := record.PlanTask{Issue: key, Generation: 1, Task: "a", PlanVersion: 2, BlockText: "Build the widget.", Merges: true, State: record.TaskWaiting}
	r.inTx(func(tx pgx.Tx) error {
		if err := r.records.PutPlan(r.ctx, tx, version, []record.PlanTask{task}, steps); err != nil || state == record.TaskWaiting {
			return err
		}
		return r.records.MoveTask(r.ctx, tx, key, 1, "a", place, state)
	})
}

// laterPlan is version 3 of the root's plan, which changes task a's block and keeps its steps,
// given by the tree's architect.
func (r *workHoldRig) laterPlan(words ...string) intake.PlanGiven {
	steps := make([]planwords.Step, len(words))
	for i, word := range words {
		role, agent, _ := strings.Cut(word, ":")
		steps[i] = planwords.Step{Role: role, Key: agent}
	}
	return intake.PlanGiven{Issue: r.tree.key, Generation: 1, GivenBy: r.token(claim.RoleArchitect), Plan: planwords.Plan{
		ArtifactID: "plan-artifact", Version: 3,
		Tasks: []planwords.Task{{ID: "a", Merges: true, Steps: steps, Text: "Build the widget, and cover the empty list."}}}}
}

// stopTask is the owner's stop_task of the root's task a, the fact its route applies.
func (r *workHoldRig) stopTask() {
	r.t.Helper()
	r.apply("stop-task", intake.TaskStopped{Issue: r.tree.key, Task: "a", By: r.token(claim.RoleArchitect), Generation: 1})
	if held := r.issue(); held.Hold == nil || held.Hold.Reason != record.HoldTaskStopped {
		r.t.Fatalf("the root after its owner's stop is %s with hold %+v, want it held for the stopped task", held.Phase, held.Hold)
	}
}

// architectsCall is the owner's request from accepting that request builds from the root's
// architect and its run of accepting, as the route reads them (api's signOff and phaseBackward),
// applied as the route applies it: recorded, never refused.
func (r *workHoldRig) architectsCall(name string, request func(by claim.Token, entry record.StepEntry) intake.Fact) {
	r.t.Helper()
	var entry record.StepEntry
	r.inTx(func(tx pgx.Tx) error {
		var err error
		entry, err = r.records.OpenStep(r.ctx, tx, r.tree.key)
		return err
	})
	r.apply(name, request(r.token(claim.RoleArchitect), entry))
}

// signOff is the architect's sign_off of the root standing in accepting.
func (r *workHoldRig) signOff() {
	r.t.Helper()
	r.architectsCall("sign-off", func(by claim.Token, entry record.StepEntry) intake.Fact {
		return intake.SignOff{Issue: r.tree.key, By: by, Generation: 1, Entry: entry}
	})
}

// reviewApproved leaves the root's review round approved and its head ready to settle it: the
// reviewer's decision approves head1, which GitHub reads clean with its checks concluded, so the
// reviewer's completion (reviewerCompletes) moves the root on, as the review's approval moves it.
func (r *workHoldRig) reviewApproved() {
	r.t.Helper()
	r.inTx(func(tx pgx.Tx) error {
		if err := r.records.PutPullRequest(r.ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: r.tree.key, Repo: "acme/widgets",
			Number: 42, Branch: "legion/" + r.tree.key, HeadSHA: "head1", CheckedHead: "head1", MergeState: record.MergeStateClean,
			Failing: []string{}}); err != nil {
			return err
		}
		return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: r.tree.key, Role: claim.RoleReviewer, Claim: r.token(claim.RoleReviewer),
			Decision: &record.ReviewDecision{State: "approved", Head: "head1"}})
	})
}

// reviewerCompletes is the reviewer's handoff of its approved round, which lands as its route
// applies it.
func (r *workHoldRig) reviewerCompletes() {
	r.t.Helper()
	r.apply("review-handoff", intake.HandoffComplete{Issue: r.tree.key, Role: claim.RoleReviewer, Claim: r.token(claim.RoleReviewer),
		Generation: 1, Summary: "approved", Commit: "head1", HandoffRecord: "review-1", HandoffVersion: 1})
}

// architectReady is the ready of the root architect's launch, whose fact queues its catch-up.
func (r *workHoldRig) architectReady() {
	r.t.Helper()
	r.apply("ready", intake.ClaimReady{Issue: r.tree.key, Role: claim.RoleArchitect, Launch: r.machine(claim.RoleArchitect).Claim().Generation})
}

// architectRestarts stops the root's architect, as the daemon stops it, and launches it again
// while the gate stays closed, its agent ready once more: the hold holds no start of an architect.
// Its ready's catch-up is what a relaunched architect reads of its tree (claimReady).
func (r *workHoldRig) architectRestarts() {
	r.t.Helper()
	token := r.token(claim.RoleArchitect)
	r.handle(claim.RoleArchitect, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByDaemon,
		Reason: "the root's architect is taken down for the test"})
	launched := r.launches(claim.RoleArchitect)
	r.enqueue(record.SuperviseRequest{Op: "start", Tree: r.tree.key, Role: claim.RoleArchitect, Generation: 1, Task: "Resume the tree."})
	r.drain()
	if got := r.launches(claim.RoleArchitect); got != launched+1 {
		r.t.Fatalf("the architect's start launched it %d times with the gate closed, want once", got-launched)
	}
	readyClaimAs(r.t, r.sup, token, claim.RoleArchitect, "ses-architect")
}

// noticesTo is every notice the outbox published to role's topic, in order.
func (r *workHoldRig) noticesTo(role claim.Role) []record.Notice {
	r.t.Helper()
	topic := notify.RoleTopicPrefix + string(r.token(role))
	_, delivered := r.publisher.snapshot()
	var notices []record.Notice
	for _, publish := range delivered {
		if notice, ok := publish.payload.(record.Notice); ok && publish.topic == topic {
			notices = append(notices, notice)
		}
	}
	return notices
}

// noticesOfKind is the notices of kind among notices.
func noticesOfKind(notices []record.Notice, kind record.NoticeKind) []record.Notice {
	var of []record.Notice
	for _, notice := range notices {
		if notice.Kind == kind {
			of = append(of, notice)
		}
	}
	return of
}

// noticeRowsOf is how many notices of kind the outbox holds for the root.
func (r *workHoldRig) noticeRowsOf(kind record.NoticeKind) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from outbox where issue = $1 and kind = $2 and payload->>'kind' = $3`,
		r.tree.key, string(record.OutboxKindNotice), string(kind)).Scan(&n); err != nil {
		r.t.Fatalf("count the %s notices: %v", kind, err)
	}
	return n
}

// builtCatchUps is every catch-up the outbox holds for the root, as it was composed.
func (r *workHoldRig) builtCatchUps() []record.Notice {
	r.t.Helper()
	var notices []record.Notice
	for _, row := range catchUpRows(r.t, r.pool, r.tree.key) {
		var notice record.Notice
		if err := json.Unmarshal(row.Payload, &notice); err != nil {
			r.t.Fatalf("decode the catch-up row %d: %v", row.ID, err)
		}
		notices = append(notices, notice)
	}
	return notices
}

// wantNoAcceptanceAsked fails t where notice, in its text or in any field of it, asks its
// architect to start its own acceptance: the reason's words for it, and the tools that end it.
func wantNoAcceptanceAsked(t *testing.T, what string, notice record.Notice) {
	t.Helper()
	whole, err := json.Marshal(notice)
	if err != nil {
		t.Fatalf("encode %s: %v", what, err)
	}
	for _, asked := range []string{"waiting for your acceptance", "sign_off", "request_backward_move"} {
		if strings.Contains(notice.Reason, asked) || strings.Contains(string(whole), asked) {
			t.Fatalf("%s asks for the architect's acceptance (%q) while the work hold stands:\n%s", what, asked, whole)
		}
	}
}

// wantTheTreeListed fails t where catch-up, the ordinary catch-up of the root, does not state the
// root standing in accepting.
func wantTheTreeListed(t *testing.T, what string, catchUp record.Notice) {
	t.Helper()
	if catchUp.CatchUp == nil || len(catchUp.CatchUp.Issues) != 1 || catchUp.CatchUp.Issues[0].Key != "LEGION-208" ||
		catchUp.CatchUp.Issues[0].Phase != phase.Accepting {
		t.Fatalf("%s states %+v, want the tree with LEGION-208 in accepting, as every catch-up states it", what, catchUp.CatchUp)
	}
}

// H37: a member whose plan is `implementer reviewer`, released and not started, stands at admitted
// under its tree's closed gate. Admission's pass offers it no slot, so its first step does not
// start, under `merge` (record.Waiting) and under `work`; the gate's opening offers it one, and its
// implementer starts once.
func firstStepStartsOnlyAtTheOpening(t *testing.T, hold config.Hold) {
	t.Helper()
	r := newWorkHoldRig(t, legionTree(phase.Implementing, hold))
	r.live(claim.RoleArchitect)
	parent := r.tree.key
	putOutboxIssue(t, r.pool, r.records, record.Issue{Key: ownersChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: r.tree.project, Tree: r.tree.key, Parent: &parent, Title: "a released child", Phase: phase.Admitted, Generation: 1,
		Status: "todo", Rank: "V", LastDispatchSeq: 1})
	r.planned(ownersChild, record.TaskPlace{}, record.TaskWaiting, "implementer", "reviewer")
	implementer := r.tokenOf(ownersChild, claim.RoleImplementer)

	r.close(4)
	r.apply("tick", intake.ControllerTick{})
	r.drain()
	if child := readIssue(t, r.pool, ownersChild); child.Phase != phase.Admitted {
		t.Fatalf("%s stands in %s after admission's pass with the gate closed, want admitted: its first step does not start", ownersChild, child.Phase)
	}
	if starts := startRequestsOf(t, r.pool, ownersChild, "start"); len(starts) != 0 {
		t.Fatalf("admission's pass with the gate closed queued the starts %+v of %s, want none", starts, ownersChild)
	}
	if got := len(launchesOf(r.rt, implementer)); got != 0 {
		t.Fatalf("the runtime started %s's implementer %d times with the gate closed, want none", ownersChild, got)
	}

	r.approve(4, 12)
	r.apply("tick", intake.ControllerTick{})
	r.branchMade(ownersChild)
	r.drain()
	if child := readIssue(t, r.pool, ownersChild); child.Phase != phase.Implementing {
		t.Fatalf("%s stands in %s once the gate opened, want implementing, its plan's first step", ownersChild, child.Phase)
	}
	if got := len(launchesOf(r.rt, implementer)); got != 1 {
		t.Fatalf("the runtime started %s's implementer %d times once the gate opened, want once", ownersChild, got)
	}
	for _, start := range startRequestsOf(t, r.pool, ownersChild, "start") {
		if start.Role == claim.RoleImplementer {
			t.Fatalf("%s's implementer's start %+v still waits once the gate opened, want it run", ownersChild, start)
		}
	}
}

func TestMergeHoldStartsAFirstStepThatIsNotPlanningOnlyAtTheOpening(t *testing.T) {
	firstStepStartsOnlyAtTheOpening(t, config.HoldMerge)
}

func TestWorkHoldStartsAFirstStepThatIsNotPlanningOnlyAtTheOpening(t *testing.T) {
	firstStepStartsOnlyAtTheOpening(t, config.HoldWork)
}

// wantTheImplementerWaits fails t where the root's implementer has started while the gate is
// closed, or where the start the root's move wrote for it is not waiting; after the move, the root
// stands in at.
func (r *workHoldRig) wantTheImplementerWaits(at phase.Phase, move string) {
	r.t.Helper()
	if got := r.issue(); got.Phase != at {
		r.t.Fatalf("the root after %s stands in %s, want %s: the move is recorded", move, got.Phase, at)
	}
	if got := r.launches(claim.RoleImplementer); got != 0 {
		r.t.Fatalf("the runtime started the implementer %d times after %s with the gate closed, want none", got, move)
	}
	if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleImplementer); got != 1 {
		r.t.Fatalf("the outbox holds %d starts of the implementer after %s with the gate closed, want the one it wrote, waiting", got, move)
	}
}

// wantTheImplementerStarted fails t where the root's implementer did not start once since the gate
// opened with the task of the start that waited, which names says.
func (r *workHoldRig) wantTheImplementerStarted(says string) {
	r.t.Helper()
	if got := r.launches(claim.RoleImplementer); got != 1 {
		r.t.Fatalf("the runtime started the implementer %d times once the gate opened, want once", got)
	}
	if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleImplementer); got != 0 {
		r.t.Fatalf("%d starts of the implementer still wait once the gate opened, want each run", got)
	}
	if c := r.machine(claim.RoleImplementer).Claim(); c.Pending == nil || !strings.Contains(c.Pending.Task, says) {
		r.t.Fatalf("the started implementer holds %+v, want the task of its start, naming %q", c.Pending, says)
	}
}

// H38: a running member under `work`. Its tester, at work when the gate closes, moves the root
// back to implementing in that turn; and, in the other case, a later version of the owner's plan
// restarts the task its owner stopped, at its first step. Each writes the implementer's start,
// which waits while the gate is closed and runs once it opens.
func TestWorkHoldHoldsTheStartsOfAMoveBackAndARestartUntilTheOpening(t *testing.T) {
	t.Run("a move back", func(t *testing.T) {
		r := newWorkHoldRig(t, legionTree(phase.Testing, config.HoldWork))
		r.live(claim.RoleArchitect)
		r.inTx(func(tx pgx.Tx) error {
			return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: r.tree.key, Role: claim.RoleTester, Claim: r.token(claim.RoleTester), Entry: 1})
		})
		r.working(claim.RoleTester)
		r.close(4)
		r.drain()
		r.apply("back", intake.BackwardMove{Issue: r.tree.key, Requester: claim.RoleTester, To: phase.Implementing,
			Reason: "the empty list is not covered"})
		endTurn(r.ctx, t, r.machine(claim.RoleTester), r.token(claim.RoleTester))
		r.drain()
		r.wantHeld(claim.RoleTester, 1, "after its turn")
		r.wantTheImplementerWaits(phase.Implementing, "the tester's move back")

		r.approve(4, 12)
		r.drain()
		r.wantTheImplementerStarted("the empty list is not covered")
	})

	t.Run("a restart of a stopped step", func(t *testing.T) {
		r := newWorkHoldRig(t, legionTree(phase.Testing, config.HoldWork))
		r.live(claim.RoleArchitect)
		r.planned(r.tree.key, record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning, "implementer", "tester")
		r.stopTask()
		r.close(4)
		r.drain()
		r.apply("plan-3", r.laterPlan("implementer", "tester"))
		r.drain()
		r.wantTheImplementerWaits(phase.Implementing, "the later version of the plan")

		r.approve(4, 12)
		r.drain()
		r.wantTheImplementerStarted("the owner gave version 3 of the plan while task a was stopped")
	})
}

// H43: the owner's acceptance, its task's step `acceptance:owner`, under `work`. Once the gate
// closes, the architect's sign-off of a pass, and in the other case its move back of a fail to
// implementing, is recorded as ever: the root moves. What the move starts, the retro's implementer
// or the implementer's next round, waits while the gate is closed and runs once it opens.
func TestWorkHoldHoldsWhatTheOwnersAcceptanceStartsUntilTheOpening(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report func(r *workHoldRig)
		at     phase.Phase
		says   string
	}{
		{name: "a pass: the sign-off", report: (*workHoldRig).signOff, at: phase.Retro, says: "LEGION-208"},
		{name: "a fail: the move back to implementing", report: func(r *workHoldRig) {
			r.architectsCall("back", func(by claim.Token, entry record.StepEntry) intake.Fact {
				return intake.BackwardMove{Issue: r.tree.key, Requester: claim.RoleArchitect, To: phase.Implementing,
					Reason: "the acceptance found the empty list uncovered", By: by, Generation: 1, Entry: entry}
			})
		}, at: phase.Implementing, says: "the acceptance found the empty list uncovered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Accepting, config.HoldWork))
			r.live(claim.RoleArchitect)
			r.planned(r.tree.key, record.TaskPlace{Kind: record.PlaceStep, Position: 3}, record.TaskRunning, "implementer", "reviewer", "acceptance:owner")
			r.close(4)
			r.drain()
			tc.report(r)
			r.drain()
			r.wantTheImplementerWaits(tc.at, tc.name)

			r.approve(4, 12)
			r.drain()
			r.wantTheImplementerStarted(tc.says)
		})
	}
}

// ownersPlaceCases are the two owner's places H45 moves an issue into: the accepting of today's
// line, which an issue with no plan rows walks, and a plan's step `acceptance:owner`.
var ownersPlaceCases = []struct {
	name  string
	steps []string
}{
	{name: "an issue with no plan rows"},
	{name: "an issue whose plan has acceptance:owner", steps: []string{"implementer", "reviewer", "acceptance:owner"}},
}

// inReviewOfAnOwnersPlan is a root standing in reviewing under hold, before its owner's place
// (steps nil walks today's line), its architect at work and its review round approved, which the
// reviewer's completion settles (reviewApproved).
func inReviewOfAnOwnersPlan(t *testing.T, hold config.Hold, steps []string) *workHoldRig {
	t.Helper()
	r := newWorkHoldRig(t, legionTree(phase.Reviewing, hold))
	r.live(claim.RoleArchitect)
	if steps != nil {
		r.planned(r.tree.key, record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning, steps...)
	}
	r.reviewApproved()
	return r
}

// H45 and decision 21: the reviewer's approval moves the root into its owner's place, and its
// handoff lands while the gate is closed under `work`, during the close's drain. The architect
// gets no phase-finished notice of its own acceptance and nothing that asks for that acceptance,
// while its other notices arrive: here a child-status written after the phase-finished. At the
// opening the notice goes, the root still standing in accepting, and the architect accepts: its
// sign-off moves the root into its retro, whose implementer starts.
func TestWorkHoldTellsNoArchitectOfItsOwnAcceptanceUntilTheOpening(t *testing.T) {
	for _, tc := range ownersPlaceCases {
		t.Run(tc.name, func(t *testing.T) {
			r := inReviewOfAnOwnersPlan(t, config.HoldWork, tc.steps)
			r.close(4)
			r.runOnce()
			r.reviewerCompletes()
			if got := r.issue(); got.Phase != phase.Accepting {
				t.Fatalf("the root after the reviewer's handoff stands in %s, want accepting: the approval is recorded", got.Phase)
			}
			r.enqueue(record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: ownersChild + " is todo; it runs again under LEGION-208"})
			r.drain()
			told := r.noticesTo(claim.RoleArchitect)
			if got := noticesOfKind(told, "phase-finished"); len(got) != 0 {
				t.Fatalf("the architect was told %+v with the gate closed, want no phase-finished notice of its own acceptance", got)
			}
			if got := noticesOfKind(told, "child-status"); len(got) != 1 {
				t.Fatalf("the architect was told %d child-status notices with the gate closed, want the later notice delivered", len(got))
			}
			for _, notice := range told {
				wantNoAcceptanceAsked(t, "the "+string(notice.Kind)+" notice the architect was told with the gate closed", notice)
			}
			if got := r.noticeRowsOf("phase-finished"); got != 1 {
				t.Fatalf("the outbox holds %d phase-finished notices with the gate closed, want the one of the approval, waiting", got)
			}

			r.approve(4, 12)
			r.drain()
			if got := noticesOfKind(r.noticesTo(claim.RoleArchitect), "phase-finished"); len(got) != 1 || got[0].Role != claim.RoleReviewer ||
				got[0].Phase != phase.Reviewing {
				t.Fatalf("the architect was told the phase-finished notices %+v once the gate opened, want the reviewer's, once", got)
			}
			if got := r.noticeRowsOf("phase-finished"); got != 0 {
				t.Fatalf("the outbox holds %d phase-finished notices once the gate opened, want none", got)
			}
			r.signOff()
			r.drain()
			if got := r.issue(); got.Phase != phase.Retro {
				t.Fatalf("the root after the architect's sign-off stands in %s, want retro", got.Phase)
			}
			if got := r.launches(claim.RoleImplementer); got != 1 {
				t.Fatalf("the runtime started the retro's implementer %d times once the architect accepted, want once", got)
			}
		})
	}
}

// Decision 21 at the opening: the architect was told the phase-finished notice of its own
// acceptance before the gate closed, and was launched again during the hold, its ready's catch-up
// naming no acceptance: the catch-up is all a fresh agent whose session was lost reads of its tree,
// and the rest of the catch-up's words are the same. At the opening it is told again that the
// acceptance is its own, by the phase-finished notice of the step before it, and it accepts.
func TestWorkHoldTellsTheArchitectOfItsOwnAcceptanceAtTheOpening(t *testing.T) {
	for _, tc := range ownersPlaceCases {
		t.Run(tc.name, func(t *testing.T) {
			r := inReviewOfAnOwnersPlan(t, config.HoldWork, tc.steps)
			r.reviewerCompletes()
			r.drain()
			if got := noticesOfKind(r.noticesTo(claim.RoleArchitect), "phase-finished"); len(got) != 1 {
				t.Fatalf("the architect was told %d phase-finished notices with the gate open, want the reviewer's", len(got))
			}
			r.close(4)
			r.drain()
			r.architectRestarts()
			r.architectReady()
			r.drain()
			catchUps := noticesOfKind(r.noticesTo(claim.RoleArchitect), "catch-up")
			if len(catchUps) != 1 {
				t.Fatalf("the restarted architect was told %d catch-ups, want the one its ready queued", len(catchUps))
			}
			wantNoAcceptanceAsked(t, "the catch-up the restarted architect was told", catchUps[0])

			r.approve(4, 12)
			r.drain()
			told := noticesOfKind(r.noticesTo(claim.RoleArchitect), "phase-finished")
			if len(told) != 2 || told[1].Role != claim.RoleReviewer || told[1].Phase != phase.Reviewing {
				t.Fatalf("the architect was told the phase-finished notices %+v, want the reviewer's again once the gate opened", told)
			}
			r.signOff()
			r.drain()
			if got := r.issue(); got.Phase != phase.Retro {
				t.Fatalf("the root after the architect's sign-off stands in %s, want retro", got.Phase)
			}
		})
	}
}

// Decision 13 beside H45: under `merge` nothing of this changes. The reviewer's handoff that moves
// the root into its owner's place with the gate closed tells the architect at once.
func TestMergeHoldTellsTheArchitectOfItsOwnAcceptanceAtOnce(t *testing.T) {
	for _, tc := range ownersPlaceCases {
		t.Run(tc.name, func(t *testing.T) {
			r := inReviewOfAnOwnersPlan(t, config.HoldMerge, tc.steps)
			r.close(4)
			r.runOnce()
			r.reviewerCompletes()
			r.drain()
			if got := noticesOfKind(r.noticesTo(claim.RoleArchitect), "phase-finished"); len(got) != 1 || got[0].Role != claim.RoleReviewer {
				t.Fatalf("under merge the architect was told the phase-finished notices %+v with the gate closed, want the reviewer's, once", got)
			}
		})
	}
}

// Decision 21: the ordinary catch-up still arrives while the work hold stands, and asks for no
// acceptance of the architect's own, in its text or in any field: built at the architect's ready
// while the hold stands, and, in the other case, built with the gate open and delivered after the
// gate closed. It still states the root in accepting.
func TestWorkHoldCatchUpAsksNoAcceptanceWhileTheHoldStands(t *testing.T) {
	t.Run("built at the architect's ready while the hold stands", func(t *testing.T) {
		r := newWorkHoldRig(t, legionTree(phase.Accepting, config.HoldWork))
		r.live(claim.RoleArchitect)
		r.close(4)
		r.drain()
		r.architectReady()
		built := r.builtCatchUps()
		if len(built) != 1 {
			t.Fatalf("the ready queued %d catch-ups, want one", len(built))
		}
		wantNoAcceptanceAsked(t, "the catch-up the ready built", built[0])
		r.drain()
		published := noticesOfKind(r.noticesTo(claim.RoleArchitect), "catch-up")
		if len(published) != 1 {
			t.Fatalf("the architect was told %d catch-ups, want the one its ready queued", len(published))
		}
		wantNoAcceptanceAsked(t, "the catch-up the architect was told", published[0])
		wantTheTreeListed(t, "the catch-up the architect was told", published[0])
	})

	t.Run("built with the gate open and delivered after the gate closed", func(t *testing.T) {
		r := newWorkHoldRig(t, legionTree(phase.Accepting, config.HoldWork))
		r.live(claim.RoleArchitect)
		r.architectReady()
		built := r.builtCatchUps()
		if len(built) != 1 || !strings.HasSuffix(built[0].Reason, acceptanceAsked) {
			t.Fatalf("the ready with the gate open queued the catch-ups %+v, want one asking for the acceptance: %q", built, acceptanceAsked)
		}
		r.close(4)
		r.drain()
		published := noticesOfKind(r.noticesTo(claim.RoleArchitect), "catch-up")
		if len(published) != 1 {
			t.Fatalf("the architect was told %d catch-ups, want the one its ready queued", len(published))
		}
		wantNoAcceptanceAsked(t, "the catch-up the architect was told once the gate closed", published[0])
		wantTheTreeListed(t, "the catch-up the architect was told once the gate closed", published[0])
	})
}

// Decision 21's twin under `merge`: nothing of this changes, and the catch-up the architect's ready
// builds with the gate closed asks for its acceptance as ever.
func TestMergeHoldCatchUpAsksForTheAcceptanceAsEver(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Accepting, config.HoldMerge))
	r.live(claim.RoleArchitect)
	r.close(4)
	r.drain()
	r.architectReady()
	r.drain()
	published := noticesOfKind(r.noticesTo(claim.RoleArchitect), "catch-up")
	if len(published) != 1 || !strings.HasSuffix(published[0].Reason, acceptanceAsked) {
		t.Fatalf("under merge the architect was told the catch-ups %+v, want one asking for the acceptance: %q", published, acceptanceAsked)
	}
}
