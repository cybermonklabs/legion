package workflow

// Every role of an issue works in one workspace, so while one role merges the base forward there
// the worker of the phase the issue stands in is told to stop writing it (record.MergeForwardHold),
// and told the new head once the merge lands (record.MergeForwardLanded). A worker that starts in a
// phase while another role's merge forward of the current head is still open is told the same:
// whatever move started it, forward or back, and wherever its task's steps put its phase.

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// heldIn is the hold of the merge forward of conflictHead onto main to role, the worker of at, for
// why: the message that tells role to stop writing the workspace while another role merges.
func heldIn(role claim.Role, at phase.Phase, why record.ConflictWhy) record.MergeForwardMessage {
	return record.MergeForwardMessage{Role: role, Step: record.MergeForwardHold, Head: conflictHead, Base: "main", Phase: at,
		PullRequest: 42, Why: why}
}

// withdrawnIn is the implementer's withdrawal of the merge forward of conflictHead onto main, told
// while the issue stands at at, for why: the message that the merge is no longer its work.
func withdrawnIn(at phase.Phase, why record.ConflictWhy) record.MergeForwardMessage {
	return record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardWithdrawn, Head: conflictHead, Base: "main",
		Phase: at, PullRequest: 42, Why: why}
}

// mergerAsked is the merger's ask of the merge forward of conflictHead onto main in merging, where
// the merge comes next.
func mergerAsked() record.MergeForwardMessage {
	return record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardAsk, Head: conflictHead, Base: "main",
		Phase: phase.Merging, PullRequest: 42, Why: record.ConflictMergeNext}
}

// approvesConflictHead is the reviewer's approval of conflictHead, a minute before the first read
// of the merge state that finds it conflicting.
func approvesConflictHead(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	applyRefusingNothing(t, pool, readsAfter(-time.Minute), intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 1,
		State: "approved", CommitID: conflictHead, HeadSHA: conflictHead, Author: "a-writer", AuthorCanWrite: true, Body: "ship it",
		SubmittedAt: firstConflictRead.Add(-time.Minute)})
}

// waitingOnTheMergeState is a pull request whose CI settled green on conflictHead and whose merge
// state GitHub has not answered yet, so an approval of that head waits on the answer
// (classify.RoundWaitsOnMergeState) and the reads that find the head conflicting decide the round.
func waitingOnTheMergeState() record.PullRequest {
	pr := settledGreen("ci")
	pr.MergeState = record.MergeStateUnknown
	return pr
}

// requiresCurrent is a base whose ruleset requires a pull request to be current with it before it
// merges, with every check it requires green on the head.
var requiresCurrent = &record.BaseRules{Required: []string{"ci"}, Strict: true}

// The plan `implementer reviewer tester`: the reviewer approves the head, and a push to the base
// then makes that head conflict, on a base that requires a branch to be current. The read that
// confirms the conflict asks the implementer to merge the base forward and holds the reviewer, and
// in the same fact the approval of a head held in a confirmed conflict ends the round and starts the
// tester (reviewRound). The tester starts while the implementer merges in the workspace they share,
// so it is held too. Once the merge lands it is told the new head, the reviewer is asked to read
// the merge, and a completion of testing at the head the merge replaced is refused.
func TestATesterStartedAfterTheReviewWhileTheMergeIsOpenIsHeld(t *testing.T) {
	t.Parallel()
	pool := plannedConflictIssue(t, phase.Reviewing, waitingOnTheMergeState(), "implementer", "reviewer", "tester")
	approvesConflictHead(t, pool)
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the approval the issue is in %s, want reviewing: the approval waits on GitHub's merge state", got)
	}
	confirmsConflict(t, pool, requiresCurrent)
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("after the confirmed conflict the issue is in %s, want testing: the approval of a conflicting head ends its round", got)
	}
	why := record.ConflictRequiresCurrent
	want := []record.MergeForwardMessage{implementerAsked(phase.Reviewing, why), heldIn(claim.RoleReviewer, phase.Reviewing, why),
		heldIn(claim.RoleTester, phase.Testing, why)}
	asked := mergeForwardMessages(t, pool)
	if !reflect.DeepEqual(asked, want) {
		t.Errorf("the merge-forward messages are %+v, want %+v: the tester starts while the implementer merges, so it is held", asked, want)
	}

	landsMergeForward(t, pool)
	if got := toldWho(mergeForwardMessages(t, pool)[len(asked):]); !reflect.DeepEqual(got, []string{"tester landed", "reviewer review"}) {
		t.Errorf("the landing in testing queued %v, want the tester told the new head and the reviewer asked to read the merge", got)
	}
	result := applyFacts(t, pool, readsAfter(4*time.Minute))("the tester's pass at the replaced head", intake.HandoffComplete{Generation: 1,
		Issue: "LEGION-208", Role: claim.RoleTester, Claim: "tester-claim", Summary: "tested", Verdict: "pass", Commit: conflictHead,
		HandoffRecord: "test-1", HandoffVersion: 1})
	if code := refusalCode(result); code != "HANDOFF_HEAD_MERGED_FORWARD" {
		t.Errorf("the tester's pass at %s, the head the merge replaced, was answered %q, want HANDOFF_HEAD_MERGED_FORWARD", conflictHead, code)
	}
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Errorf("after the tester's completion at the replaced head the issue is in %s, want testing", got)
	}
}

// The plan `implementer acceptance:owner tester`: in accepting that a step follows, a required
// check that is not green asks the implementer at once, and nobody is held, since the architect
// writes no workspace. The owner's sign-off then starts the tester while that merge is open, so the
// tester is held, and told the new head once the merge lands.
func TestATesterStartedByTheSignOffWhileTheMergeIsOpenIsHeld(t *testing.T) {
	t.Parallel()
	pool := plannedConflictIssue(t, phase.Accepting, settledGreen("lint"), "implementer", "acceptance:owner", "tester")
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	engine := readsAfter(2 * time.Minute)
	applyRefusingNothing(t, pool, engine, rootsSignOff(t, pool, engine, "LEGION-208"))
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("after the owner's sign-off the issue is in %s, want testing", got)
	}
	why := record.ConflictCheckNotGreen
	want := []record.MergeForwardMessage{implementerAsked(phase.Accepting, why), heldIn(claim.RoleTester, phase.Testing, why)}
	asked := mergeForwardMessages(t, pool)
	if !reflect.DeepEqual(asked, want) {
		t.Errorf("the merge-forward messages are %+v, want %+v: the tester starts while the implementer merges, so it is held", asked, want)
	}
	landsMergeForward(t, pool)
	if got := toldWho(mergeForwardMessages(t, pool)[len(asked):]); !reflect.DeepEqual(got, []string{"tester landed"}) {
		t.Errorf("the landing in testing queued %v, want the tester told the new head", got)
	}
}

// The plan `implementer planner tester`: the implementer completes at a conflicting head without
// the merge it was asked for in implementing, and the move into the planning after it asks again
// (mergeForwardOnEntry). The planner works in the workspace the implementer merges in, so it is
// held, it is told the new head once the merge lands, and its completion at the head the merge
// replaced is refused as a tester's would be; its completion at the new head moves the issue on.
func TestAPlannerAfterTheImplementerIsHeldAndFenced(t *testing.T) {
	t.Parallel()
	pool := plannedConflictIssue(t, phase.Implementing, settledGreen("lint"), "implementer", "planner", "tester")
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	completesImplementingAt(t, pool)
	if got := issuePhase(t, pool); got != phase.Planning {
		t.Fatalf("after the implementer's completion the issue is in %s, want planning", got)
	}
	why := record.ConflictCheckNotGreen
	want := []record.MergeForwardMessage{implementerAsked(phase.Implementing, why), implementerAsked(phase.Planning, why),
		heldIn(claim.RolePlanner, phase.Planning, why)}
	asked := mergeForwardMessages(t, pool)
	if !reflect.DeepEqual(asked, want) {
		t.Errorf("the merge-forward messages are %+v, want %+v: the planner works in the workspace the implementer merges in", asked, want)
	}

	landsMergeForward(t, pool)
	if got := toldWho(mergeForwardMessages(t, pool)[len(asked):]); !reflect.DeepEqual(got, []string{"planner landed"}) {
		t.Errorf("the landing in planning queued %v, want the planner told the new head", got)
	}
	planned := func(commit string) intake.HandoffComplete {
		return intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RolePlanner, Claim: "planner-claim", Summary: "planned",
			Commit: commit, HandoffRecord: "plan-" + commit, HandoffVersion: 1}
	}
	result := applyFacts(t, pool, readsAfter(4*time.Minute))("the planner at the replaced head", planned(conflictHead))
	if code := refusalCode(result); code != "HANDOFF_HEAD_MERGED_FORWARD" {
		t.Fatalf("the planner's completion at %s, the head the merge replaced, was answered %q, want HANDOFF_HEAD_MERGED_FORWARD", conflictHead, code)
	}
	if got := issuePhase(t, pool); got != phase.Planning {
		t.Fatalf("after the planner's completion at the replaced head the issue is in %s, want planning", got)
	}
	applyRefusingNothing(t, pool, readsAfter(5*time.Minute), planned(forwardHead))
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("after the planner's completion at %s, the head the merge left, the issue is in %s, want testing", forwardHead, got)
	}
}

// On the default steps, a worker starts while another role's merge forward is open in two ways,
// and is held in both: the tester's completion at the head the implementer was asked to merge,
// which main took and still takes, starts the reviewer; and the reviewer's own move back starts
// the tester. A move back into planning, which on the default steps stands before the
// implementer, starts the planner the same way.
func TestADefaultStepThatStartsWhileTheMergeIsOpenIsHeld(t *testing.T) {
	t.Parallel()
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	why := record.ConflictCheckNotGreen

	t.Run("the move into reviewing after the tester's completion", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
		confirmsConflict(t, pool, requiresCI)
		applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
			Role: claim.RoleTester, Claim: "tester-claim", Summary: "tested", Verdict: "pass", Commit: conflictHead,
			HandoffRecord: "test-1", HandoffVersion: 1})
		if got := issuePhase(t, pool); got != phase.Reviewing {
			t.Fatalf("after the tester's pass the issue is in %s, want reviewing", got)
		}
		want := []record.MergeForwardMessage{implementerAsked(phase.Testing, why), heldIn(claim.RoleTester, phase.Testing, why),
			heldIn(claim.RoleReviewer, phase.Reviewing, why)}
		if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
			t.Errorf("the merge-forward messages are %+v, want %+v: the reviewer starts while the implementer merges", got, want)
		}
	})

	for _, tc := range []struct {
		name string
		from phase.Phase
		by   claim.Role
		to   phase.Phase
	}{
		{name: "the reviewer's move back into testing", from: phase.Reviewing, by: claim.RoleReviewer, to: phase.Testing},
		{name: "the tester's move back into planning", from: phase.Testing, by: claim.RoleTester, to: phase.Planning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedConflictIssue(t, tc.from, PhaseStatus(tc.from), settledGreen("lint"))
			confirmsConflict(t, pool, requiresCI)
			applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.BackwardMove{Issue: "LEGION-208", Requester: tc.by, To: tc.to,
				Reason: "the work needs another look"})
			if got := issuePhase(t, pool); got != tc.to {
				t.Fatalf("after the move back the issue is in %s, want %s", got, tc.to)
			}
			want := []record.MergeForwardMessage{implementerAsked(tc.from, why), heldIn(tc.by, tc.from, why),
				heldIn(RoleFor(tc.to), tc.to, why)}
			if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
				t.Errorf("the merge-forward messages are %+v, want %+v: the %s starts while the implementer merges", got, want, RoleFor(tc.to))
			}
		})
	}
}

// A worker that starts again while another role's merge forward of the head is open is held as a
// worker that a move starts is: the architect's retry of a held phase and the operator's restart of
// a phase's worker each start the worker of the phase again (retryPhase), and the hold the ask
// sent earlier may never have reached it - a hold to a claim that failed ends undelivered - or may
// be in a session the restart does not keep. On the default steps, the implementer is asked in
// testing and the tester is held; the tester's claim fails, which holds the issue; the retry, or
// the restart, starts the tester again, and it is held again.
func TestAWorkerStartedAgainWhileTheMergeIsOpenIsHeld(t *testing.T) {
	t.Parallel()
	why := record.ConflictCheckNotGreen
	for _, tc := range []struct {
		name  string
		start intake.Fact
	}{
		{name: "the architect's retry of the held phase", start: intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision, Reason: "the claim failed"}},
		{name: "the operator's restart of the phase's worker", start: intake.RestartPhase{Issue: "LEGION-208", Role: claim.RoleTester}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
			confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
			applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleTester})
			if got := issuePhase(t, pool); got != phase.Held {
				t.Fatalf("after the tester's claim failed the issue is in %s, want held", got)
			}
			applyRefusingNothing(t, pool, readsAfter(3*time.Minute), tc.start)
			if got := issuePhase(t, pool); got != phase.Testing {
				t.Fatalf("after %s the issue is in %s, want testing", tc.name, got)
			}
			want := []record.MergeForwardMessage{implementerAsked(phase.Testing, why), heldIn(claim.RoleTester, phase.Testing, why),
				heldIn(claim.RoleTester, phase.Testing, why)}
			if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
				t.Errorf("after %s the merge-forward messages are %+v, want %+v: the tester starts again while the implementer merges", tc.name, got, want)
			}
		})
	}
}

// A worker the daemon launches again on its own while another role's merge forward of the head is
// open is held at the ready of that launch, by the rule that holds a worker a move or a retry
// starts (holdStartedWorker): the launch may run a fresh session - its process died and was
// relaunched within its budgets, a released pod was woken, or its session could not be resumed
// (supervise's startFresh) - which holds none of its predecessor's messages, so the hold the ask
// sent may be in none of them. On the default steps, the implementer is asked in testing and the
// tester is held; the ready of the tester's launch 2, with no move and no retry, holds it again.
// The asked implementer's own ready holds nobody, and a ready once the merge landed holds nobody.
func TestAWorkerRelaunchedWhileTheMergeIsOpenIsHeldAtItsReady(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		landed bool
		ready  claim.Role
		want   []string
	}{
		{name: "the tester's relaunch is held", ready: claim.RoleTester, want: []string{"tester hold"}},
		{name: "control: the asked implementer's relaunch holds nobody", ready: claim.RoleImplementer},
		{name: "control: a relaunch once the merge landed holds nobody", landed: true, ready: claim.RoleTester},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testerHeldWhileTheImplementerMerges(t)
			if tc.landed {
				landsMergeForward(t, pool)
			}
			before := len(mergeForwardMessages(t, pool))
			applyRefusingNothing(t, pool, readsAfter(3*time.Minute), intake.ClaimReady{Issue: "LEGION-208", Role: tc.ready, Launch: 2})
			var held []string
			for _, message := range mergeForwardMessages(t, pool)[before:] {
				if message.Step == record.MergeForwardHold {
					held = append(held, string(message.Role)+" "+string(message.Step))
				}
			}
			if !reflect.DeepEqual(held, tc.want) {
				t.Errorf("the ready of the %s's launch 2 queued the holds %v, want %v", tc.ready, held, tc.want)
			}
		})
	}
}

// testerHeldWhileTheImplementerMerges is LEGION-208 on the default steps in testing, whose head
// GitHub confirms conflicting with a base that requires a green ci: the implementer is asked to
// merge the base forward, and the tester is held. It fails the test unless those are the two
// merge-forward messages written.
func testerHeldWhileTheImplementerMerges(t *testing.T) *pgxpool.Pool {
	t.Helper()
	why := record.ConflictCheckNotGreen
	pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	if got, want := mergeForwardMessages(t, pool), []record.MergeForwardMessage{implementerAsked(phase.Testing, why),
		heldIn(claim.RoleTester, phase.Testing, why)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the confirmed conflict the merge-forward messages are %+v, want %+v", got, want)
	}
	return pool
}

// A worker the daemon sends its phase's work again while another role's merge forward of the head
// is open is held with that work, by the rule that holds a worker a move starts: the silence
// repair's send to a worker up between turns with no task, which a held worker that stopped as its
// hold says is, and admission's send of the entry a worker is behind. Each is work that tells the
// worker to go on in the workspace the merge writes. On the default steps, the implementer is
// asked in testing and the tester is held; each send holds the tester once more.
func TestAWorkerSentItsWorkAgainWhileTheMergeIsOpenIsHeld(t *testing.T) {
	t.Parallel()
	why := record.ConflictCheckNotGreen
	for _, tc := range []struct {
		name string
		send func(ctx context.Context, tx pgx.Tx, e *Engine, issue record.Issue, row record.PhaseRow) error
	}{
		{name: "the silence repair", send: func(ctx context.Context, tx pgx.Tx, e *Engine, issue record.Issue, row record.PhaseRow) error {
			return e.repairSilence(ctx, tx, issue, issue, record.RoleRun{Claim: &record.RoleClaim{GivenEntry: row.Entry}}, row,
				intake.WorkerSilent{Issue: "LEGION-208", Role: claim.RoleTester, Generation: 1, LastStep: e.now().Add(-2 * time.Hour), Bound: time.Hour},
				e.now())
		}},
		{name: "admission's send of an owed entry", send: func(ctx context.Context, tx pgx.Tx, e *Engine, issue record.Issue, row record.PhaseRow) error {
			return e.SendOwedEntry(ctx, tx, issue, claim.RoleTester, row)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testerHeldWhileTheImplementerMerges(t)
			engine := readsAfter(3 * time.Minute)
			applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
				issue, err := engine.store.Issue(ctx, tx, "LEGION-208")
				if err != nil {
					return err
				}
				row, err := engine.phaseRow(ctx, tx, "LEGION-208", claim.RoleTester)
				if err != nil {
					return err
				}
				return tc.send(ctx, tx, engine, *issue, row)
			})
			want := []record.MergeForwardMessage{implementerAsked(phase.Testing, why), heldIn(claim.RoleTester, phase.Testing, why),
				heldIn(claim.RoleTester, phase.Testing, why)}
			if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
				t.Errorf("after %s the merge-forward messages are %+v, want %+v: the tester is sent work while the implementer merges", tc.name, got, want)
			}
		})
	}
}

// The plan `implementer reviewer tester`: a merge forward asked in reviewing lands, the head it
// left conflicts in its turn, and a second merge forward of that head is asked, still in
// reviewing. The reviewer's approval of that head starts the tester while the second merge is
// open, so the tester is held, and its hold tells it not to complete at the head the pull request
// stands at. A completion of testing at the head the first merge replaced is refused, and the
// refusal the route answers is the fence whose values say what it tells (mergedForwardFence): it
// answers the tester, names the head the first merge replaced and the head it left, and tells the
// tester to wait for the open merge of main onto the head the pull request stands at (Wait),
// rather than to work at that head now, which words of its own say apart.
func TestARefusalTellsATesterHeldAtItsStartToWaitForTheOpenMerge(t *testing.T) {
	t.Parallel()
	pool := testerStartedWhileTheSecondMergeIsAsked(t)
	issue, pr := storedIssue(t, pool, "LEGION-208"), recordedPR(t, pool, "LEGION-208")
	fence, fenced := mergedForwardRefusal(issue, claim.RoleTester, phase.Testing, conflictHead, &pr)
	if !fenced || fence.Role != claim.RoleTester || !fence.Wait || fence.Head != forwardHead || fence.Base != "main" ||
		fence.Replaced != conflictHead || fence.Landed != forwardHead {
		t.Errorf("the fence of the tester's completion at %s is %+v (fenced %t), want the tester told to wait for the open merge of main onto %s, the head that replaced %s",
			conflictHead, fence, fenced, forwardHead, conflictHead)
	}
	work := fence
	work.Wait = false
	if work.Message() == fence.Message() {
		t.Errorf("the refusal tells waiting for the merge and working at once in the same words: %q", fence.Message())
	}

	result := applyFacts(t, pool, readsAfter(7*time.Minute))("the tester at the first replaced head", intake.HandoffComplete{Generation: 1,
		Issue: "LEGION-208", Role: claim.RoleTester, Claim: "tester-claim", Summary: "tested", Verdict: "pass", Commit: conflictHead,
		HandoffRecord: "test-1", HandoffVersion: 1})
	if code := refusalCode(result); code != "HANDOFF_HEAD_MERGED_FORWARD" {
		t.Fatalf("the tester's completion at %s was answered %q (%+v), want HANDOFF_HEAD_MERGED_FORWARD", conflictHead, code, result.Refusal)
	}
	if result.Refusal.Message != fence.Message() {
		t.Errorf("the tester's completion at %s was answered %q, want the refusal of that fence, %q", conflictHead, result.Refusal.Message, fence.Message())
	}
}

// testerStartedWhileTheSecondMergeIsAsked is the issue of
// TestARefusalTellsATesterHeldAtItsStartToWaitForTheOpenMerge on the steps implementer, reviewer,
// tester: a first merge forward landed, the head it left conflicts in its turn and a second merge
// forward of that head is asked in reviewing, and the reviewer's approval of that head started the
// tester, in testing, while the second merge has not landed. It fails the test unless the issue
// stands in testing and the record holds that second ask.
func testerStartedWhileTheSecondMergeIsAsked(t *testing.T) *pgxpool.Pool {
	t.Helper()
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	pool := plannedConflictIssue(t, phase.Reviewing, settledGreen("lint"), "implementer", "reviewer", "tester")
	confirmsConflict(t, pool, requiresCI)
	landsMergeForward(t, pool)
	again := conflictingRead(requiresCI)
	again.HeadSHA = forwardHead
	applyRefusingNothing(t, pool, readsAfter(4*time.Minute), again)
	applyRefusingNothing(t, pool, readsAfter(5*time.Minute), again)
	applyRefusingNothing(t, pool, readsAfter(6*time.Minute), intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 2,
		State: "approved", CommitID: forwardHead, HeadSHA: forwardHead, Author: "a-writer", AuthorCanWrite: true, Body: "ship it",
		SubmittedAt: firstConflictRead.Add(6 * time.Minute)})
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("after the approval of %s the issue is in %s, want testing", forwardHead, got)
	}
	if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Head != forwardHead || asked.Landed != "" || asked.Phase != phase.Reviewing {
		t.Fatalf("the record's merge forward = %+v, want the second ask, of %s in reviewing, not landed", asked, forwardHead)
	}
	return pool
}

// The pull request of that issue then closes without a merge while the second merge forward is
// asked and has not landed. The close keeps the record's ask, but a closed pull request gets no
// push and no landing, so nothing owes the merge and no worker waits for it: the held rule names
// nobody, and the tester's completion at the head the first merge replaced is still refused, with
// the refusal that tells it to work at the head the pull request stands at rather than to wait for
// a landed message that cannot come (mergedForwardFence.Wait).
func TestARefusalOnAClosedPullRequestTellsNoWorkerToWait(t *testing.T) {
	t.Parallel()
	pool := testerStartedWhileTheSecondMergeIsAsked(t)
	applyRefusingNothing(t, pool, readsAfter(8*time.Minute), closedAt(forwardHead, firstConflictRead.Add(8*time.Minute)))
	issue, pr := storedIssue(t, pool, "LEGION-208"), recordedPR(t, pool, "LEGION-208")
	if pr.State != record.PullRequestClosed || pr.MergeForward == nil || pr.MergeForward.Head != forwardHead || issue.Phase != phase.Testing {
		t.Fatalf("after the close the pull request is %s with the ask %+v and the issue in %s, want it closed with the second ask kept, in testing",
			pr.State, pr.MergeForward, issue.Phase)
	}
	if worker, held := heldWorker(issue, pr); held {
		t.Errorf("on the closed pull request the held rule names the %s, want nobody: no landing can come", worker)
	}
	fence, fenced := mergedForwardRefusal(issue, claim.RoleTester, phase.Testing, conflictHead, &pr)
	if !fenced || fence.Wait || fence.Replaced != conflictHead || fence.Landed != forwardHead {
		t.Errorf("the fence of the tester's completion at %s on the closed pull request is %+v (fenced %t), want it refused and told to work at %s, not to wait",
			conflictHead, fence, fenced, forwardHead)
	}
}

// On the default steps, an issue whose reviewed head conflicts with a base that requires it to be
// current goes on to merging as before: the read that confirms the conflict asks the implementer
// and holds the reviewer, with one checks-red notice; the approval moves the issue into accepting,
// whose architect writes no workspace, the sign-off into the retro of the implementer it asked,
// and the retro's completion into merging, where the merger waits for the implementer's merge.
// Nothing else is sent, no move back is counted, and the landing in merging tells the merger and
// asks the reviewer to read the merge. One message more than main's is sent, at the ready of a
// launch the daemon makes of the merger by itself while the merge is open: the merger is held
// again (holdSentWorker), where main sends no message at a ready. Its check uses t.Errorf, so the
// landing is still read on main.
func TestTheDefaultStepsSendWhatTheySentWhenAReviewedHeadConflicts(t *testing.T) {
	t.Parallel()
	pool := seedConflictIssue(t, phase.Reviewing, "needs_review", waitingOnTheMergeState())
	approvesConflictHead(t, pool)
	confirmsConflict(t, pool, requiresCurrent)
	why := record.ConflictRequiresCurrent
	want := []record.MergeForwardMessage{implementerAsked(phase.Reviewing, why), heldIn(claim.RoleReviewer, phase.Reviewing, why)}
	sent := func(after string, at phase.Phase) {
		t.Helper()
		if got := issuePhase(t, pool); got != at {
			t.Fatalf("after %s the issue is in %s, want %s", after, got, at)
		}
		if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
			t.Fatalf("after %s the merge-forward messages are %+v, want %+v", after, got, want)
		}
		if told := capNotices(t, pool, "checks-red"); told != 1 {
			t.Fatalf("after %s the architect has %d checks-red notices, want 1", after, told)
		}
		if moves := backwardMoves(t, pool); len(moves) != 0 {
			t.Fatalf("after %s the moves back counted are %v, want none", after, moves)
		}
	}
	sent("the confirmed conflict", phase.Accepting)

	engine := readsAfter(2 * time.Minute)
	applyRefusingNothing(t, pool, engine, rootsSignOff(t, pool, engine, "LEGION-208"))
	sent("the sign-off", phase.Retro)

	applyRefusingNothing(t, pool, readsAfter(3*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
		Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: conflictHead})
	want = append(want, heldIn(claim.RoleMerger, phase.Merging, why))
	sent("the retro's completion", phase.Merging)

	applyRefusingNothing(t, pool, readsAfter(3*time.Minute+30*time.Second), intake.ClaimReady{Issue: "LEGION-208", Role: claim.RoleMerger, Launch: 2})
	if got, held := mergeForwardMessages(t, pool), append(slices.Clone(want), heldIn(claim.RoleMerger, phase.Merging, why)); !reflect.DeepEqual(got, held) {
		t.Errorf("after the ready of the merger's launch 2 the merge-forward messages are %+v, want %+v: the ready holds the merger again", got, held)
	}

	asked := len(mergeForwardMessages(t, pool))
	applyRefusingNothing(t, pool, readsAfter(4*time.Minute), intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", Base: "main", HeadSHA: forwardHead, Before: conflictHead, Sender: "legion-implementer[bot]"})
	if got := toldWho(mergeForwardMessages(t, pool)[asked:]); !reflect.DeepEqual(got, []string{"merger landed", "reviewer review"}) {
		t.Fatalf("the landing in merging queued %v, want the merger told and the reviewer asked to read the merge", got)
	}
}

// Who a merge forward tells, in each order a task's steps run in: the ask goes to the role asked,
// the hold and the landed to the worker of the phase the issue stands in (phase.RoleFor) where it
// is a role that works the head - the planner, the tester, the reviewer or the merger - and the
// review to the reviewer whose step already ran, which tells what it decided to the architect and
// to the worker of that phase (record.MergeForwardMessage.Tells), the architect alone in
// accepting. Each row is the recipients a confirmed conflict and the landing of its merge queue,
// written out. The planner is held and told wherever its step follows the implementer; every other
// recipient, and how many messages each gets, is as before.
func TestAMergeForwardTellsTheWorkerOfThePhaseInEveryOrder(t *testing.T) {
	t.Parallel()
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		name  string
		steps []string // none: no plan rows, today's line
		at    phase.Phase
		pr    record.PullRequest
		ask   []string
		land  []string
		// tells is the worker the review tells beside the architect, where the landing asks one.
		tells claim.Role
	}{
		{name: "the default steps, implementing", at: phase.Implementing, pr: settledGreen("lint"),
			ask: []string{"implementer ask"}, land: []string{}},
		{name: "the default steps, testing", at: phase.Testing, pr: settledGreen("lint"),
			ask: []string{"implementer ask", "tester hold"}, land: []string{"tester landed"}},
		{name: "the default steps, reviewing", at: phase.Reviewing, pr: settledGreen("lint"),
			ask: []string{"implementer ask", "reviewer hold"}, land: []string{"reviewer landed"}},
		{name: "the default steps, accepting", at: phase.Accepting, pr: askedInReviewing(),
			ask: []string{}, land: []string{"reviewer review"}},
		{name: "the default steps, retro", at: phase.Retro, pr: askedInReviewing(),
			ask: []string{}, land: []string{"reviewer review"}, tells: claim.RoleImplementer},
		{name: "the default steps, merging", at: phase.Merging, pr: settledGreen("lint"),
			ask: []string{"merger ask"}, land: []string{"merger landed", "reviewer review"}, tells: claim.RoleMerger},
		{name: "implementer reviewer tester, testing", steps: []string{"implementer", "reviewer", "tester"}, at: phase.Testing, pr: settledGreen("lint"),
			ask: []string{"implementer ask", "tester hold"}, land: []string{"tester landed", "reviewer review"}, tells: claim.RoleTester},
		{name: "implementer reviewer tester, reviewing", steps: []string{"implementer", "reviewer", "tester"}, at: phase.Reviewing, pr: settledGreen("lint"),
			ask: []string{"implementer ask", "reviewer hold"}, land: []string{"reviewer landed"}},
		{name: "implementer acceptance:owner reviewer, accepting", steps: []string{"implementer", "acceptance:owner", "reviewer"}, at: phase.Accepting, pr: settledGreen("lint"),
			ask: []string{"implementer ask"}, land: []string{}},
		{name: "implementer acceptance:owner reviewer, reviewing", steps: []string{"implementer", "acceptance:owner", "reviewer"}, at: phase.Reviewing, pr: settledGreen("lint"),
			ask: []string{"implementer ask", "reviewer hold"}, land: []string{"reviewer landed"}},
		{name: "implementer planner reviewer, planning", steps: []string{"implementer", "planner", "reviewer"}, at: phase.Planning, pr: settledGreen("lint"),
			ask: []string{"implementer ask", "planner hold"}, land: []string{"planner landed"}},
		{name: "implementer reviewer planner, planning", steps: []string{"implementer", "reviewer", "planner"}, at: phase.Planning, pr: settledGreen("lint"),
			ask: []string{"implementer ask", "planner hold"}, land: []string{"planner landed", "reviewer review"}, tells: claim.RolePlanner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pool *pgxpool.Pool
			if len(tc.steps) == 0 {
				pool = seedConflictIssue(t, tc.at, PhaseStatus(tc.at), tc.pr)
			} else {
				pool = plannedConflictIssue(t, tc.at, tc.pr, tc.steps...)
			}
			confirmsConflict(t, pool, requiresCI)
			asked := mergeForwardMessages(t, pool)
			if got := toldWho(asked); !reflect.DeepEqual(got, tc.ask) {
				t.Errorf("the confirmed conflict in %s queued %v, want %v", tc.at, got, tc.ask)
			}
			landsMergeForward(t, pool)
			landed := mergeForwardMessages(t, pool)[len(asked):]
			if got := toldWho(landed); !reflect.DeepEqual(got, tc.land) {
				t.Errorf("the landing in %s queued %v, want %v", tc.at, got, tc.land)
			}
			for _, message := range landed {
				if message.Step == record.MergeForwardReview && message.Tells() != tc.tells {
					t.Errorf("the reviewer's review in %s tells %q beside the architect, want %q", tc.at, message.Tells(), tc.tells)
				}
			}
		})
	}
}
