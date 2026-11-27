package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// askOwed posts the owed route's body as the caller names it, so a refusal can be asked for.
func askOwed(h *harness, req OwedRequest) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/claims/owed", req, nil)
}

// owed is what c's own session is told its claim owes now, asked with the secret its registration
// issued it.
func (c liveClaim) owed(t *testing.T) OwedResponse {
	t.Helper()
	recorder := askOwed(c.h, OwedRequest{ClaimToken: c.token, SessionID: c.session, Secret: c.secret})
	if recorder.Code != http.StatusOK {
		t.Fatalf("owed for %s = %d, want 200; body %s", c.token, recorder.Code, recorder.Body)
	}
	var answer OwedResponse
	decodeInto(t, recorder, &answer)
	return answer
}

// owes asserts c owes work, and that the answer names it as want.
func (c liveClaim) owes(t *testing.T, want string) {
	t.Helper()
	if got := c.owed(t); !got.Owes || got.Work != want {
		t.Fatalf("%s owes %+v, want it to owe %q", c.token, got, want)
	}
}

// owesNothing asserts c owes nothing, and that the answer names no work. The architect that owns
// c is no part of what it owes, so it is not read here.
func (c liveClaim) owesNothing(t *testing.T) {
	t.Helper()
	if got := c.owed(t); got.Owes || got.Work != "" {
		t.Fatalf("%s owes %+v, want nothing and no work named", c.token, got)
	}
}

// claimNow is c's claim as its machine holds it now: the claim the owed route and the completion
// route read (authenticated, supervise.Machine.Claim).
func (c liveClaim) claimNow(t *testing.T) supervise.Claim {
	t.Helper()
	return machineOf(t, c).Claim()
}

// machineOf is the machine the daemon supervises c's claim with.
func machineOf(t *testing.T, c liveClaim) *supervise.Machine {
	t.Helper()
	machine, ok := c.h.supervisor.Machine(c.token)
	if !ok {
		t.Fatalf("no machine for %s", c.token)
	}
	return machine
}

// The defect this route cures (dispatch://INFRA-2746): a reminder state kept per process nagged
// every role whose phase had finished, and started again as open at every restart. The answer is
// read from the record instead, so it follows the workflow as the workflow moves the issue, and a
// restarted process reads what its predecessor would have.
//
// The issue is driven through the daemon's own workflow, one completion at a time, and each role is
// asked at each step: the worker of the current phase, with its work undone, owes that phase and
// the answer names it; a worker whose phase has finished owes nothing, whether the issue moved to
// another role's phase (the implementer and the tester) or stayed in the role's own phase with the
// completion recorded on its row (the reviewer, whose round waits for a review that decides it).
func TestClaimOwedFollowsThePhaseAsTheWorkflowMovesTheIssue(t *testing.T) {
	h, _, _ := newEntryHarness(t)
	ctx := context.Background()
	h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
		Project: testProject, Title: "LEGION-208", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "acme/widgets",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", CheckedHead: "head", Failing: []string{}})
	}); err != nil {
		t.Fatalf("record the pull request: %v", err)
	}

	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	implementer.owes(t, "the implementing phase of LEGION-208")

	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: "handoff-1",
		HandoffID: implementer.writeHandoff("implement", 1), HandoffVersion: 1}))
	assertIssuePhase(t, h, phase.Testing)
	implementer.owesNothing(t)

	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	takeStart(t, h, tester.token, "LEGION-208", claim.RoleTester)
	tester.owes(t, "the testing phase of LEGION-208")

	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
		HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}))
	assertIssuePhase(t, h, phase.Reviewing)
	tester.owesNothing(t)
	implementer.owesNothing(t)

	// The tester's process is lost and the daemon relaunches it on the same session: the new
	// process holds a new secret and remembers nothing, and it is told what its predecessor was.
	tester.secret = h.registered(h.relaunch(tester.token), tester.session).Secret
	tester.owesNothing(t)

	reviewer := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	takeStart(t, h, reviewer.token, "LEGION-208", claim.RoleReviewer)
	reviewer.owes(t, "the reviewing phase of LEGION-208")

	// The reviewer completes and no review has decided its round, so the issue stays in
	// reviewing, the reviewer's phase: its row records the completion the workflow took, and that
	// is what says the work is done.
	completion(t, h.complete(HandoffCompleteRequest{GrantID: reviewer.grant(t), Summary: "reviewed",
		HandoffID: reviewer.writeHandoff("review", 1), HandoffVersion: 1}))
	assertIssuePhase(t, h, phase.Reviewing)
	reviewer.owesNothing(t)
}

// Within the role's own phase the answer reads the entry the claim serves against the entry its
// row stands on, as the completion route's entry fence reads the two (claimServes, entryFence), so
// it never tells a claim it owes a completion that fence refuses (dispatch://INFRA-2746 R1). A
// reviewer level with its row whose row records its completion owes nothing: its round waits for
// a review, and the daemon sends a level claim no entry's work. The same row raised to an entry
// the claim does not serve leaves the claim behind it, and it owes nothing there either, though
// the row still records the completion: the route refuses a behind claim's completion
// HANDOFF_STALE_ENTRY, and the daemon sends that entry's work itself (workflow.OwedEntry, which
// admission, the entry sweep, a ready and the silence repair send it on). It still owes nothing
// once that entry's task is queued to it and no turn has confirmed it, since the claim serves the
// entry before it until a turn of the new task begins.
func TestClaimOwedIsNothingWhileTheClaimIsBehindItsEntry(t *testing.T) {
	h, _, _ := newEntryHarness(t)
	ctx := context.Background()
	seedIssueAt(t, h, "LEGION-208", phase.Reviewing)
	reviewer := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	seedEntry(t, h, "LEGION-208", claim.RoleReviewer, 1)
	takeTask(t, h, reviewer.token, "outbox:entry-1", "review entry 1", 1, 1)
	reviewer.owes(t, "the reviewing phase of LEGION-208")

	if _, err := h.store.Pool().Exec(ctx, `update phases set handoff_commit = 'reviewed'
		where issue = 'LEGION-208' and role = $1`, string(claim.RoleReviewer)); err != nil {
		t.Fatalf("record the reviewer's completion: %v", err)
	}
	reviewer.owesNothing(t)

	if _, err := h.store.Pool().Exec(ctx, `update phases set entry = 2
		where issue = 'LEGION-208' and role = $1`, string(claim.RoleReviewer)); err != nil {
		t.Fatalf("raise the reviewer's row to entry 2: %v", err)
	}
	if serving := reviewer.claimNow(t).ServingEntry(); serving != 1 {
		t.Fatalf("the reviewer serves entry %d, want 1: the case's own premise", serving)
	}
	reviewer.owesNothing(t)

	queueTask(t, h, reviewer.token, "outbox:entry-2", "review entry 2", 2, 1)
	if now := reviewer.claimNow(t); now.GivenEntry != 2 || now.ServingEntry() != 1 {
		t.Fatalf("the reviewer was given entry %d and serves entry %d, want given 2 and serving 1: the task of entry 2 is queued and unconfirmed",
			now.GivenEntry, now.ServingEntry())
	}
	reviewer.owesNothing(t)
}

// The answer reads the record one way (dispatch://INFRA-2746 R1): every claim it tells it owes a
// completion would have that completion taken at the head the pull request stands at, and in each
// shape in which the end-of-turn reminder reminded a role that owed nothing, the role is told it
// owes nothing. Each subtest builds its shape through the daemon's own workflow — completions the
// route takes, a request_backward_move, the reviewer's own review, a conflict the daemon confirms
// and the merge forward it asks, a retro's completion that moves the issue into merging, the
// synchronize that lands that merge, a hold, a park and a close — then asks claims/owed, then
// posts that claim's completion and holds the route's answer to it: taken, or the named refusal.
// Two merging shapes are an ask this release's workflow does not write, which the record is
// given directly (recordMergeForward): one a daemon before cybermonklabs/legion#4979 wrote with no
// asked role, and one of a head the pull request no longer stands at. An answer of nothing owed
// does not say the route refuses: the route also records a completion of a phase the daemon
// moved the issue out of (workflow's leftItsPhase), and that phase is not reminded (R8). An
// answer of work owed does say the route takes the completion at the head the pull request
// stands at.
func TestClaimOwedIsOwedOnlyWhereTheCompletionRouteTakesACompletion(t *testing.T) {
	t.Run("moved on", func(t *testing.T) {
		h, _, _ := newEntryHarness(t)
		owedIssue(t, h)
		implementer, _ := implemented(t, h)
		implementer.owesNothing(t)
		refused(t, h.complete(completionOf(t, implementer, "implement", headSHA, 1)), http.StatusConflict, "HANDOFF_NOT_CURRENT_PHASE")
	})

	// No review has decided the reviewer's round, so its completion leaves the issue in
	// reviewing, the reviewer's own phase, and the row records that completion.
	t.Run("completion recorded", func(t *testing.T) {
		h, _, _ := newEntryHarness(t)
		owedIssue(t, h)
		_, tester := implemented(t, h)
		reviewer := tested(t, h, tester)
		completion(t, h.complete(completionOf(t, reviewer, "review", headSHA, 1)))
		assertIssuePhase(t, h, phase.Reviewing)
		reviewer.owesNothing(t)
		refused(t, h.complete(completionOf(t, reviewer, "review", headSHA, 1)), http.StatusConflict, "HANDOFF_ALREADY_RECORDED")
	})

	// The hold told the tester to stop and wait for the merge to land, so the tester owes
	// nothing while the ask stands. The hold is the daemon's word to the role and no fence: a
	// completion at the head the pull request stands at is taken all the same.
	t.Run("merge forward asked", func(t *testing.T) {
		h, engine, _ := newEntryHarness(t)
		owedIssue(t, h)
		_, tester := implemented(t, h)
		tester.owes(t, "the testing phase of LEGION-208")
		mergeForwardAsked(t, h, engine, phase.Testing)
		tester.owesNothing(t)
		completion(t, h.complete(completionOf(t, tester, "test", headSHA, 1)))
		assertIssuePhase(t, h, phase.Reviewing)
	})

	t.Run("implementer asked to merge forward", func(t *testing.T) {
		h, engine, _ := newEntryHarness(t)
		owedIssue(t, h)
		implementer, _ := implemented(t, h)
		mergeForwardAsked(t, h, engine, phase.Testing)
		implementer.owesNothing(t)
		refused(t, h.complete(completionOf(t, implementer, "implement", headSHA, 1)), http.StatusConflict, "HANDOFF_NOT_CURRENT_PHASE")
	})

	// An ask made in implementing sends no hold: the merge is part of the implementer's own open
	// work there, and its assignment and its completion stand.
	t.Run("merge forward asked in implementing", func(t *testing.T) {
		h, engine, _ := newEntryHarness(t)
		owedIssue(t, h)
		implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
		mergeForwardAsked(t, h, engine, phase.Implementing)
		implementer.owes(t, "the implementing phase of LEGION-208")
		completion(t, h.complete(completionOf(t, implementer, "implement", headSHA, 1)))
		assertIssuePhase(t, h, phase.Testing)
	})

	// Once the merge lands, the tester owes its phase at the head the merge left: a completion at
	// the head the merge replaced is refused for that head (the merge forward's own fence), and
	// one at the new head, naming a handoff of its own, is taken.
	t.Run("merge forward landed", func(t *testing.T) {
		h, engine, _ := newEntryHarness(t)
		owedIssue(t, h)
		_, tester := implemented(t, h)
		mergeForwardAsked(t, h, engine, phase.Testing)
		mergeForwardLands(t, h, engine)
		tester.owes(t, "the testing phase of LEGION-208")
		refused(t, h.complete(completionOf(t, tester, "test", headSHA, 1)), http.StatusConflict, "HANDOFF_HEAD_MERGED_FORWARD")
		assertIssuePhase(t, h, phase.Testing)
		completion(t, h.complete(completionOf(t, tester, "test", forwardSHA, 2)))
		assertIssuePhase(t, h, phase.Reviewing)
	})

	// The tester's completion at the asked head moves the issue to reviewing, and no move from
	// testing to reviewing asks again, so the record still holds the ask made in testing, not
	// landed. That ask stays the implementer's into reviewing, and the move holds the reviewer it
	// starts rather than leaving it to work the head (workflow's holdStartedWorker,
	// record.MergeForwardHolds), so the reviewer owes nothing while that merge is outstanding. The
	// hold is the daemon's word to the reviewer and no fence: its completion at the head the pull
	// request stands at is taken all the same.
	t.Run("merge forward asked in testing, issue now in reviewing", func(t *testing.T) {
		h, engine, _ := newEntryHarness(t)
		owedIssue(t, h)
		_, tester := implemented(t, h)
		mergeForwardAsked(t, h, engine, phase.Testing)
		reviewer := tested(t, h, tester)
		if asked := recordedMergeForward(t, h); asked == nil || asked.Head != headSHA || asked.Phase != phase.Testing || asked.Landed != "" {
			t.Fatalf("the record's merge forward = %+v, want the ask of %s made in testing, not landed: the case's own premise", asked, headSHA)
		}
		if !queuedMergeForward(t, h, claim.RoleReviewer, record.MergeForwardHold) {
			t.Error("the move into reviewing did not hold the reviewer: the implementer's ask made in testing keeps the merge")
		}
		reviewer.owesNothing(t)
		completion(t, h.complete(completionOf(t, reviewer, "review", headSHA, 1)))
	})

	// The implementer was asked in reviewing (record.MergeForwardAsker), and that ask held the
	// reviewer; its merge has not landed when the retro's completion moves the issue into merging,
	// so the move holds the merger rather than asking it (workflow's holdStartedWorker), and the merger
	// owes nothing while that merge is outstanding (cybermonklabs/legion#4979). The hold is the
	// daemon's word to the merger and no fence: its completion at the head the pull request stands
	// at is taken all the same.
	t.Run("merger held in merging by another role's earlier ask", func(t *testing.T) {
		h, engine, _ := newEntryHarness(t)
		owedIssue(t, h)
		setIssue(t, h, "LEGION-208", func(issue *record.Issue) { issue.Phase = phase.Reviewing })
		mergeForwardAsked(t, h, engine, phase.Reviewing)
		merger := retroIntoMerging(t, h)
		if asked := recordedMergeForward(t, h); asked == nil || asked.Head != headSHA || asked.Asked() != claim.RoleImplementer ||
			asked.Phase != phase.Reviewing || asked.Landed != "" {
			t.Fatalf("the record's merge forward = %+v, want the implementer's ask of %s made in reviewing, not landed: the case's own premise", asked, headSHA)
		}
		merger.owesNothing(t)
		if !queuedMergeForward(t, h, claim.RoleMerger, record.MergeForwardHold) || queuedMergeForward(t, h, claim.RoleMerger, record.MergeForwardAsk) {
			t.Error("the move into merging did not hold the merger, or asked it: the implementer's ask keeps the merge")
		}
		completion(t, h.complete(mergerCompletion(t, merger, headSHA)))
	})

	// A daemon before cybermonklabs/legion#4979 asked the implementer in every phase and recorded no
	// asked role, so an ask it made in merging reads as the implementer's (record.MergeForward.Asked)
	// and holds the merger until it lands. The merger owes merging until that ask is recorded, and
	// nothing after.
	t.Run("merger held by an ask that records no asked role", func(t *testing.T) {
		h, _, _ := newEntryHarness(t)
		owedIssue(t, h)
		merger := retroIntoMerging(t, h)
		merger.owes(t, "the merging phase of LEGION-208")
		recordMergeForward(t, h, record.MergeForward{Head: headSHA, Base: "main", Phase: phase.Merging, Why: record.ConflictRulesUnread, AskedAt: time.Now()})
		merger.owesNothing(t)
		completion(t, h.complete(mergerCompletion(t, merger, headSHA)))
	})

	// The control (cybermonklabs/legion#4979): a conflict confirmed in merging asks the merger, whose
	// merge it is, so the merge forward is its own open work, it owes merging, and its completion at
	// the head the pull request stands at is taken.
	t.Run("merger asked to merge forward in merging", func(t *testing.T) {
		h, engine, _ := newEntryHarness(t)
		owedIssue(t, h)
		merger := retroIntoMerging(t, h)
		mergeForwardAsked(t, h, engine, phase.Merging)
		merger.owes(t, "the merging phase of LEGION-208")
		completion(t, h.complete(mergerCompletion(t, merger, headSHA)))
	})

	// The implementer's ask made in reviewing held the merger when the issue entered merging, and
	// its merge has landed since: nothing holds the merger any more, so it owes merging at the
	// head the merge left, and its completion there is taken.
	t.Run("an earlier ask that has landed", func(t *testing.T) {
		h, engine, _ := newEntryHarness(t)
		owedIssue(t, h)
		setIssue(t, h, "LEGION-208", func(issue *record.Issue) { issue.Phase = phase.Reviewing })
		mergeForwardAsked(t, h, engine, phase.Reviewing)
		merger := retroIntoMerging(t, h)
		merger.owesNothing(t)
		mergeForwardLands(t, h, engine)
		merger.owes(t, "the merging phase of LEGION-208")
		completion(t, h.complete(mergerCompletion(t, merger, forwardSHA)))
	})

	// The record's ask of the implementer, made in reviewing and not landed, is of a head the pull
	// request no longer stands at: it holds no role, so the merger owes merging at the head the
	// pull request stands at, and its completion there is taken.
	t.Run("an ask of another head", func(t *testing.T) {
		h, _, _ := newEntryHarness(t)
		owedIssue(t, h)
		merger := retroIntoMerging(t, h)
		recordMergeForward(t, h, record.MergeForward{Head: otherHeadSHA, Base: "main", Role: claim.RoleImplementer, Phase: phase.Reviewing,
			Why: record.ConflictRulesUnread, AskedAt: time.Now()})
		merger.owes(t, "the merging phase of LEGION-208")
		completion(t, h.complete(mergerCompletion(t, merger, headSHA)))
	})

	t.Run("own backward move", func(t *testing.T) {
		h, _, _ := newEntryHarness(t)
		owedIssue(t, h)
		_, tester := implemented(t, h)
		reviewer := tested(t, h, tester)
		reviewer.owes(t, "the reviewing phase of LEGION-208")
		movesBack(t, h, reviewer, phase.Implementing)
		assertIssuePhase(t, h, phase.Implementing)
		reviewer.owesNothing(t)
		completion(t, h.complete(completionOf(t, reviewer, "review", headSHA, 1)))
	})

	// The reviewer's own request for changes ends its round before its completion lands: the
	// issue goes back to implementing, and the completion that follows is recorded on the
	// reviewer's row as the report of the phase it left.
	t.Run("round ended by its own review", func(t *testing.T) {
		h, engine := newOwedHarness(t)
		owedIssue(t, h)
		_, tester := implemented(t, h)
		reviewer := tested(t, h, tester)
		reviewer.owes(t, "the reviewing phase of LEGION-208")
		reviewersOwnReview(t, h, engine, reviewer, time.Now())
		assertIssuePhase(t, h, phase.Implementing)
		reviewer.owesNothing(t)
		completion(t, h.complete(completionOf(t, reviewer, "review", headSHA, 1)))
	})

	// A finished planner's pod was released and woken: the wake's task names no phase, and it
	// waits on the claim unconfirmed while the issue is in implementing.
	t.Run("pending task with no phase", func(t *testing.T) {
		h, _, _ := newEntryHarness(t)
		planner := planned(t, h)
		queueTask(t, h, planner.token, "outbox:wake", workflow.WakeTask(pods.WakeRoleMessage, "notifications.role.legion-legion-legion-208-planner", workflow.StoppedByRelease), 0, 1)
		if pending := planner.claimNow(t).Pending; pending == nil || pending.Phase != "" || !pending.ConfirmedAt.IsZero() {
			t.Fatalf("the planner holds %+v, want the wake's task of no phase, unconfirmed: the case's own premise", pending)
		}
		planner.owesNothing(t)
		refused(t, h.complete(completionOf(t, planner, "plan", "", 1)), http.StatusConflict, "HANDOFF_NOT_CURRENT_PHASE")
	})

	// R1's first term alone: the tester works the issue's phase, its row records no completion
	// and stands on an entry, and no merge forward holds it, but its claim has served no run and
	// holds only a task that names no phase, which is none of the issue's current work.
	t.Run("serves no run of this generation", func(t *testing.T) {
		h, _, _ := newEntryHarness(t)
		seedIssueAt(t, h, "LEGION-208", phase.Testing)
		seedEntry(t, h, "LEGION-208", claim.RoleTester, 1)
		tester := untaskedClaim(t, h, "LEGION-208", claim.RoleTester)
		holdingTask(t, h, tester, supervise.RequestDeliver{Task: workflow.WakeTask(pods.WakeRoleMessage, "notifications.role.legion-legion-legion-208-tester", workflow.StoppedByRelease),
			ID: "outbox:wake", Generation: 1})
		tester.owesNothing(t)
		refused(t, h.complete(completionOf(t, tester, "test", headSHA, 1)), http.StatusConflict, "HANDOFF_NO_RUN")
	})

	// The reviewer's completion of its review record version 1 is taken, its own review then
	// asks for changes, and it writes version 2 after the issue went back to implementing.
	t.Run("handoff record newer than the last completion", func(t *testing.T) {
		h, engine := newOwedHarness(t)
		owedIssue(t, h)
		_, tester := implemented(t, h)
		reviewer := tested(t, h, tester)
		completion(t, h.complete(completionOf(t, reviewer, "review", headSHA, 1)))
		reviewersOwnReview(t, h, engine, reviewer, time.Now().Add(time.Minute))
		assertIssuePhase(t, h, phase.Implementing)
		reviewer.writeHandoff("review", 2)
		reviewer.owesNothing(t)
		refused(t, h.complete(completionOf(t, reviewer, "review", headSHA, 2)), http.StatusConflict, "HANDOFF_NOT_CURRENT_PHASE")
	})

	// A finished planner ends a turn of work its architect asked for: a turn no task of the
	// daemon's backs, started by the architect's message while the issue is in implementing.
	t.Run("work outside the phase", func(t *testing.T) {
		h, _, _ := newEntryHarness(t)
		planner := planned(t, h)
		turnOfItsOwn(t, planner)
		planner.owesNothing(t)
		refused(t, h.complete(completionOf(t, planner, "plan", "", 1)), http.StatusConflict, "HANDOFF_NOT_CURRENT_PHASE")
	})

	// The workflow holds the issue on the fact that its tester's claim failed, and phase.RoleFor
	// names no role for held. The claim asked with is that tester's: the harness applies the fact
	// and does not fail the claim, so the answer is read through the record's hold alone (a
	// failed claim is answered nothing before the record is read,
	// TestClaimOwedIsNothingOnceTheDaemonHasFailedTheClaim).
	t.Run("a phase no role works", func(t *testing.T) {
		h, engine, _ := newEntryHarness(t)
		owedIssue(t, h)
		_, tester := implemented(t, h)
		applyEntryFact(t, h, engine, "tester-failed", intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleTester, Reason: "the process died"})
		assertIssuePhase(t, h, phase.Held)
		tester.owesNothing(t)
		refused(t, h.complete(completionOf(t, tester, "test", headSHA, 1)), http.StatusConflict, "HANDOFF_NOT_CURRENT_PHASE")
	})

	// The reviewer's own move back to testing raises the tester's row to a second entry, marked
	// as raised by a move back, while the tester's claim still serves the first: the work is to be
	// done again at the same head, so the route refuses the first entry's completion whatever its
	// commit reads (behindEntryClause), and the daemon sends the second entry's task itself.
	t.Run("behind the entry", func(t *testing.T) {
		h, _, _ := newEntryHarness(t)
		owedIssue(t, h)
		_, tester := implemented(t, h)
		reviewer := tested(t, h, tester)
		movesBack(t, h, reviewer, phase.Testing)
		assertIssuePhase(t, h, phase.Testing)
		if row := phaseRowOf(t, h, "LEGION-208", claim.RoleTester); row == nil || row.Entry != 2 || !row.EntryMovedBack {
			t.Fatalf("the tester's row = %+v, want entry 2 raised by a move back: the case's own premise", row)
		}
		if serving := tester.claimNow(t).ServingEntry(); serving != 1 {
			t.Fatalf("the tester serves entry %d, want 1: the case's own premise", serving)
		}
		tester.owesNothing(t)
		refused(t, h.complete(completionOf(t, tester, "test", headSHA, 2)), http.StatusConflict, staleEntryCode)
	})

	t.Run("an open entry with no completion", func(t *testing.T) {
		h, _, _ := newEntryHarness(t)
		owedIssue(t, h)
		_, tester := implemented(t, h)
		tester.owes(t, "the testing phase of LEGION-208")
		completion(t, h.complete(completionOf(t, tester, "test", headSHA, 1)))
		assertIssuePhase(t, h, phase.Reviewing)
	})

	// A person parks the child in Dispatch's backlog: the child leaves the workflow, and the
	// workflow parks its phase in done (workflow's leave), where phase.RoleFor names no role.
	t.Run("parked", func(t *testing.T) {
		h, engine, _ := newEntryHarness(t)
		parent := "LEGION-208"
		h.recordIssue(record.Issue{Key: parent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: parent,
			Project: testProject, Title: parent, Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress"})
		h.recordIssue(record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: parent, Parent: &parent,
			Project: testProject, Title: "LEGION-209", Phase: phase.Testing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress"})
		tester := newLiveClaimIn(t, h, parent, "LEGION-209", claim.RoleTester)
		tester.owes(t, "the testing phase of LEGION-209")
		applyEntryFact(t, h, engine, "parked", intake.DispatchIssue{Key: "LEGION-209", Status: "backlog", Title: "LEGION-209", Parent: parent, ResolvedRepo: "acme/widgets"})
		if child := readIssue(t, h, "LEGION-209"); child == nil || child.Phase != phase.Done {
			t.Fatalf("the parked child = %+v, want it in done: the case's own premise", child)
		}
		tester.owesNothing(t)
		refused(t, h.complete(completionOf(t, tester, "test", headSHA, 1)), http.StatusConflict, "CHECK_LOOK_NOT_SERVED")
	})

	// A person closes the root in Dispatch: its tree lingers (completionPosition.lingers), and
	// the root's phase is done.
	t.Run("tree closed", func(t *testing.T) {
		h, engine, _ := newEntryHarness(t)
		owedIssue(t, h)
		_, tester := implemented(t, h)
		applyEntryFact(t, h, engine, "closed", intake.DispatchIssue{Key: "LEGION-208", Status: "done", Title: "LEGION-208", ResolvedRepo: "acme/widgets"})
		if root := readIssue(t, h, "LEGION-208"); root == nil || !root.Lingers() {
			t.Fatalf("the closed root = %+v, want its tree lingering: the case's own premise", root)
		}
		tester.owesNothing(t)
		refused(t, h.complete(completionOf(t, tester, "test", headSHA, 1)), http.StatusConflict, "CHECK_LOOK_NOT_SERVED")
	})
}

// forwardSHA is the head the implementer's merge of main forward onto headSHA leaves on #42.
const forwardSHA = "f0d0000000000000000000000000000000000aa1"

// otherHeadSHA is a head of #42 the pull request no longer stands at, which a merge forward the
// record keeps can name.
const otherHeadSHA = "a99e0000000000000000000000000000000000ab"

// owedReviewApp is the review App's login in newOwedHarness, so a review the reviewer submits
// through it from its own session is the reviewer's own (workflow's byReviewer).
const owedReviewApp = "legion-review[bot]"

// newOwedHarness is newEntryHarness's workflow with the review App named, as a deployment names
// it, so the reviewer's own review decides its round (workflow's DecidesRound).
func newOwedHarness(t *testing.T) (*harness, *workflow.Engine) {
	t.Helper()
	h, engine, _ := newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: 3, ReviewAppLogin: owedReviewApp})
	return h, engine
}

// owedIssue records LEGION-208 in implementing on run 1, with its open pull request #42 on
// legion/LEGION-208 onto main at headSHA, the head GitHub's stand-in answers for it.
func owedIssue(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
		Project: testProject, Title: "LEGION-208", Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress"})
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "acme/widgets",
			Number: 42, Branch: "legion/LEGION-208", Base: "main", HeadSHA: headSHA, CheckedHead: headSHA, Failing: []string{}})
	}); err != nil {
		t.Fatalf("record the pull request: %v", err)
	}
}

// retroIntoMerging has the implementer's completion of LEGION-208's retro at headSHA taken, with
// its rating of the plan, which moves the issue from retro into merging, the one move into it, and
// so runs that move's step for a merge forward (workflow's mergeForwardOnEntry). The issue is put
// in retro first, as the reviewer's approval and the architect's sign-off leave it: neither asks
// anything of a merge forward, and nothing is asked in accepting or retro. It has the merger take
// the start the move queued, and answers the merger.
func retroIntoMerging(t *testing.T, h *harness) liveClaim {
	t.Helper()
	setIssue(t, h, "LEGION-208", func(issue *record.Issue) { issue.Phase = phase.Retro })
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "retro done", Commit: headSHA,
		PlanRating: new(3), PlanReason: new("The plan fit the work.")}))
	assertIssuePhase(t, h, phase.Merging)
	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	takeStart(t, h, merger.token, "LEGION-208", claim.RoleMerger)
	return merger
}

// mergerCompletion is merger's completion of merging at commit: merging ends with no handoff
// record, so the completion names its commit alone.
func mergerCompletion(t *testing.T, merger liveClaim, commit string) HandoffCompleteRequest {
	t.Helper()
	return HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "merged", Commit: commit}
}

// recordMergeForward writes asked as the merge forward the record holds for LEGION-208's pull
// request, which keeps all else it holds: an ask this release's workflow does not write, as a
// daemon before cybermonklabs/legion#4979 wrote one with no asked role, or one of a head the pull
// request no longer stands at that no synchronize landed.
func recordMergeForward(t *testing.T, h *harness, asked record.MergeForward) {
	t.Helper()
	_, _, pr := handoffRecord(t, h, "LEGION-208")
	if pr == nil {
		t.Fatal("the record holds no pull request for LEGION-208")
	}
	pr.MergeForward = &asked
	ctx := context.Background()
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, *pr)
	}); err != nil {
		t.Fatalf("record the merge forward %+v: %v", asked, err)
	}
}

// completionOf is c's completion of the phase whose handoff word is word, at commit, naming
// version of that handoff, which c writes under its own session first; a tester's carries a pass.
// Each call mints a grant of its own, as each handoff_complete of the legion tool does.
func completionOf(t *testing.T, c liveClaim, word, commit string, version int) HandoffCompleteRequest {
	t.Helper()
	request := HandoffCompleteRequest{GrantID: c.grant(t), Summary: word + " done", Commit: commit,
		HandoffID: c.writeHandoff(word, version), HandoffVersion: version}
	if word == "test" {
		request.Verdict = "pass"
	}
	return request
}

// implemented has the implementer's completion at headSHA taken, which moves LEGION-208 from
// implementing to testing, and has the tester take the start that move queued. It answers both
// claims.
func implemented(t *testing.T, h *harness) (implementer, tester liveClaim) {
	t.Helper()
	implementer = newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	completion(t, h.complete(completionOf(t, implementer, "implement", headSHA, 1)))
	assertIssuePhase(t, h, phase.Testing)
	tester = newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	takeStart(t, h, tester.token, "LEGION-208", claim.RoleTester)
	return implementer, tester
}

// tested has tester's pass at headSHA taken, which moves LEGION-208 to reviewing, and has the
// reviewer take the start that move queued. It answers the reviewer.
func tested(t *testing.T, h *harness, tester liveClaim) liveClaim {
	t.Helper()
	completion(t, h.complete(completionOf(t, tester, "test", headSHA, 1)))
	assertIssuePhase(t, h, phase.Reviewing)
	reviewer := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	takeStart(t, h, reviewer.token, "LEGION-208", claim.RoleReviewer)
	return reviewer
}

// planned records LEGION-208 in planning on run 1 and has its planner's completion taken, which
// moves the issue to implementing. It answers the planner, whose turn goes on.
func planned(t *testing.T, h *harness) liveClaim {
	t.Helper()
	seedIssueAt(t, h, "LEGION-208", phase.Planning)
	planner := newLiveClaim(t, h, "LEGION-208", claim.RolePlanner)
	planner.owes(t, "the planning phase of LEGION-208")
	completion(t, h.complete(completionOf(t, planner, "plan", "", 1)))
	assertIssuePhase(t, h, phase.Implementing)
	return planner
}

// turnOfItsOwn ends c's turn, which retires the task it held, and starts a turn no task of the
// daemon's backs, as an Envoy message to c's session starts one.
func turnOfItsOwn(t *testing.T, c liveClaim) {
	t.Helper()
	machine := machineOf(t, c)
	ctx := context.Background()
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: c.token}); err != nil {
		t.Fatalf("end the turn of %s: %v", c.token, err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: c.token}); err != nil {
		t.Fatalf("start a turn of %s's own: %v", c.token, err)
	}
}

// movesBack is c's own request_backward_move of its issue to to, as the legion tool sends it.
func movesBack(t *testing.T, h *harness, c liveClaim, to phase.Phase) {
	t.Helper()
	recorder := h.request(http.MethodPost, "/legion/v1/phase/backward",
		PhaseBackwardRequest{GrantID: c.grant(t), To: to, Reason: "the fix does not cover the empty list"}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the move back of %s to %s = %d: %s", c.token, to, recorder.Code, recorder.Body)
	}
}

// reviewersOwnReview is reviewer's own review of #42 at headSHA asking for changes, submitted at
// submitted through the review App, with the Legion footer the reviewer's template has it write:
// its own session, the head it read and the entry of its round.
func reviewersOwnReview(t *testing.T, h *harness, engine *workflow.Engine, reviewer liveClaim, submitted time.Time) {
	t.Helper()
	footer := fmt.Sprintf(`<!-- legion: {"session":%q,"phase":"review","head":%q,"entry":"%d"} -->`,
		reviewer.session, headSHA, rowEntry(t, h, "LEGION-208", claim.RoleReviewer))
	applyEntryFact(t, h, engine, "review-changes-requested", intake.PullRequestReview{Repo: "acme/widgets", Number: 42, ID: 1, SubmittedAt: submitted,
		State: "changes_requested", CommitID: headSHA, HeadSHA: headSHA, Author: owedReviewApp, Body: "The empty list is not covered.\n\n" + footer})
}

// mergeForwardAsked has the workflow confirm that #42's head conflicts with main, as the daemon
// confirms it (dispatch://INFRA-4374): a first CONFLICTING read, and a second one a minute later.
// The base's rules are left unread, which is a reason the daemon acts on a confirmed conflict
// (record.ConflictRulesUnread). The issue keeps its phase, in; the role the daemon asks there -
// the implementer before merging, the merger in merging (cybermonklabs/legion#4979) - is asked to
// merge main forward, the worker of in is held where the hold reaches it and it is not the role
// asked, and the record holds the ask of headSHA made of that role in in, not landed.
func mergeForwardAsked(t *testing.T, h *harness, engine *workflow.Engine, in phase.Phase) {
	t.Helper()
	first := time.Now()
	for i, at := range []time.Time{first, first.Add(time.Minute)} {
		applyEntryFact(t, h, engine, fmt.Sprintf("conflict-read-%d", i), intake.PullRequestMergeability{Repo: "acme/widgets", Number: 42, Base: "main",
			HeadSHA: headSHA, Mergeable: record.MergeabilityConflicting, MergeState: record.MergeStateDirty, ReadAt: at})
	}
	assertIssuePhase(t, h, in)
	// Written out rather than read from record.MergeForwardAsker, so an asker that routes wrong
	// fails here instead of setting the expectation it is checked against.
	asker := claim.RoleImplementer
	if in == phase.Merging {
		asker = claim.RoleMerger
	}
	if asked := recordedMergeForward(t, h); asked == nil || asked.Head != headSHA || asked.Role != asker || asked.Phase != in || asked.Landed != "" {
		t.Fatalf("the record's merge forward = %+v, want the ask of %s made of the %s in %s, not landed", asked, headSHA, asker, in)
	}
	if !queuedMergeForward(t, h, asker, record.MergeForwardAsk) {
		t.Fatalf("the %s was not asked to merge main forward", asker)
	}
	worker := phase.RoleFor(in)
	if held, want := queuedMergeForward(t, h, worker, record.MergeForwardHold),
		in != phase.Implementing && worker != asker && record.MergeForwardReaches(record.MergeForwardHold, worker); held != want {
		t.Fatalf("the %s of %s was held = %t, want %t", worker, in, held, want)
	}
}

// mergeForwardLands is GitHub's synchronize of #42 from headSHA to forwardSHA, the implementer's
// merge of main forward landing, which the workflow records as the head that merge left
// (record.MergeForward.Landed).
func mergeForwardLands(t *testing.T, h *harness, engine *workflow.Engine) {
	t.Helper()
	applyEntryFact(t, h, engine, "merge-forward-landed", intake.PullRequestSynchronized{Repo: "acme/widgets", Number: 42,
		Branch: "legion/LEGION-208", Base: "main", HeadSHA: forwardSHA, Before: headSHA, Sender: "legion-implementer[bot]"})
	if asked := recordedMergeForward(t, h); asked == nil || asked.Head != headSHA || asked.Landed != forwardSHA {
		t.Fatalf("the record's merge forward = %+v, want the ask of %s landed at %s", asked, headSHA, forwardSHA)
	}
}

// recordedMergeForward is the merge forward the record holds for LEGION-208's pull request now,
// nil while none stands.
func recordedMergeForward(t *testing.T, h *harness) *record.MergeForward {
	t.Helper()
	_, _, pr := handoffRecord(t, h, "LEGION-208")
	if pr == nil {
		t.Fatal("the record holds no pull request for LEGION-208")
	}
	return pr.MergeForward
}

// queuedMergeForward says whether the outbox holds a merge-forward message of step to role on
// LEGION-208.
func queuedMergeForward(t *testing.T, h *harness, role claim.Role, step record.MergeForwardStep) bool {
	t.Helper()
	var queued int
	if err := h.store.Pool().QueryRow(context.Background(), `select count(*) from outbox
		where kind = $1 and issue = 'LEGION-208' and payload->>'role' = $2 and payload->>'step' = $3`,
		string(record.OutboxKindMergeForward), string(role), string(step)).Scan(&queued); err != nil {
		t.Fatalf("count the merge-forward messages of step %s to the %s: %v", step, role, err)
	}
	return queued > 0
}

// A claim of a role whose phase another role works, or whose issue stands where no role works,
// owes nothing, and the answer names no work: what each role was handed is no part of it. The
// architect works no phase of its own — accepting is its sign_off, not a phase it completes — so it
// owes nothing even in accepting.
func TestClaimOwedIsNothingForARoleWhosePhaseAnotherRoleWorks(t *testing.T) {
	h, _, _ := newEntryHarness(t)
	for _, tc := range []struct {
		issue string
		at    phase.Phase
		role  claim.Role
	}{
		{issue: "LEGION-301", at: phase.Reviewing, role: claim.RoleTester},
		{issue: "LEGION-302", at: phase.Done, role: claim.RoleMerger},
		{issue: "LEGION-303", at: phase.Accepting, role: claim.RoleImplementer},
		{issue: "LEGION-304", at: phase.Accepting, role: claim.RoleArchitect},
		{issue: "LEGION-305", at: phase.Done, role: claim.RoleImplementer},
	} {
		t.Run(string(tc.role)+" in "+string(tc.at), func(t *testing.T) {
			seedIssueAt(t, h, tc.issue, tc.at)
			newLiveClaim(t, h, tc.issue, tc.role).owesNothing(t)
		})
	}
}

// The merger merges (dispatch://INFRA-714): merging is its working phase, with no wait for the
// daemon's merge after it, so a merger whose issue stands in merging with its pull request open
// owes that phase, and the answer names it. The merged pull request is what finishes merging
// (workflow.PhaseFinished), and a merged root stands in merging until the rest of its tree
// leaves, so once its pull request is merged the merger owes nothing though the phase stands.
func TestClaimOwedIsTheMergeUntilTheMergersPullRequestIsMerged(t *testing.T) {
	h, _, _ := newEntryHarness(t)
	seedIssueAt(t, h, "LEGION-208", phase.Merging)
	recordPullRequest(t, h, record.PullRequestOpen)

	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	merger.owes(t, "the merging phase of LEGION-208")

	recordPullRequest(t, h, record.PullRequestMerged)
	assertIssuePhase(t, h, phase.Merging)
	merger.owesNothing(t)
}

// A claim the daemon has failed owes nothing, whatever its phase: the failure is the daemon's own
// word that the claim's work is its architect's to decide. This is what ends the reminding of a
// session whose provider refuses its turns with a status that cannot clear (dispatch://INFRA-264,
// dispatch://INFRA-4508): the report of a reminder's own turn, which ran no prompt of the
// daemon's, is sent over `POST /legion/v1/claims/turn-outcome` marked unprompted with the status
// the provider refused it with, a 400 fails the claim at once, and the one delayed reminder a
// refused turn arms asks this route before it goes. A run of refusals fails no claim, so a
// refusal that cannot clear is the turn-outcome path to a failure.
func TestClaimOwedIsNothingOnceTheDaemonHasFailedTheClaim(t *testing.T) {
	h, _, _ := newEntryHarness(t)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	tester.owes(t, "the testing phase of LEGION-208")

	recorder, answer := h.turnOutcome(claim.TurnOutcomeRequest{
		ClaimToken: tester.token, SessionID: tester.session, Secret: tester.secret,
		Generation: h.stored(tester.token).Generation, Sequence: 1, Outcome: claim.TurnRefused,
		Message: "400 the provider refused the reminder's turn", HTTPStatus: http.StatusBadRequest, Unprompted: true,
	})
	if recorder.Code != http.StatusOK || !answer.Failed {
		t.Fatalf("the refused reminder's turn = %d %+v, want 200 with the claim failed; body %s", recorder.Code, answer, recorder.Body)
	}
	if state := h.stored(tester.token).State; state != supervise.StateFailed {
		t.Fatalf("the refused reminder's turn left %s %s, want it failed", tester.token, state)
	}
	// The answer is the whole of nothing: no work, and no architect named for a claim the daemon
	// has failed.
	if got := tester.owed(t); got != (OwedResponse{}) {
		t.Fatalf("the failed %s is told %+v, want nothing at all", tester.token, got)
	}
}

// A tester holding an open look of its closed issue's production check owes that look, and the
// answer names it; once the check closes, it owes nothing. Done runs no phase, so the look is the
// whole of what it can owe.
func TestClaimOwedIsTheProductionCheckLookItsTesterHolds(t *testing.T) {
	h, _, _ := newEntryHarness(t)
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
	tester := newCheckLookClaim(t, h, "LEGION-208", claim.RoleTester, 2)
	tester.owes(t, "the production check of LEGION-208")

	seedCheck(t, h, "LEGION-208", claim.RoleTester, true, false)
	tester.owesNothing(t)
}

// The three sources disagree here. The look the claim serves decides only whether its turn is on
// an open look, and a report reaches the check route only when it names its check
// (checkOpenedAt). A merged close spares the tester's
// claim and opens a look behind its running testing turn: the record holds the look open for the
// tester, while the tester still serves its testing task. A report from that turn is refused as no
// report on production (CHECK_LOOK_NOT_SERVED), so the turn owes nothing — not the look, and not
// the testing phase of an issue that is done. Once the look's own task starts a turn, it owes the
// look.
func TestClaimOwedIsNoLookItsTesterHasNotTaken(t *testing.T) {
	h, _, tester, _ := mergedCloseBehindATestingTurn(t)
	tester.owesNothing(t)

	tester.worksLook(t, 1, recordedCheck(t, h, "LEGION-209").OpenedAt)
	tester.owes(t, "the production check of LEGION-209")
}

// The daemon's own delivery of a relaunch's record is no work of the claim's (supervise.HoldsTask,
// dispatch://INFRA-3276): it carries the sentence of what the last let-go ended and no run, no
// entry and no look, so a claim holding it is answered by the record and by what it was left
// serving, as a claim holding nothing is, whether or not a turn has confirmed that delivery. A
// tester left serving a look of a done issue's open check owes that look. A tester in its own open
// phase that was left serving no run serves none (claimServes), so it owes nothing: the completion
// route refuses its completion HANDOFF_NO_RUN (dispatch://INFRA-2746 R1).
func TestClaimOwedReadsTheRelaunchDeliveryAsNoWork(t *testing.T) {
	opened := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	for _, confirmed := range []time.Time{{}, opened.Add(time.Hour)} {
		relaunch := &supervise.Delivery{ID: "relaunch:owed", Task: "Your previous process ended because the process died.", ConfirmedAt: confirmed}
		if !relaunch.Relaunch() || supervise.HoldsTask(relaunch) {
			t.Fatalf("%s reads as a task of the claim's, want the daemon's own delivery of a relaunch's record", relaunch.ID)
		}
		serving := supervise.Claim{Role: claim.RoleTester, Pending: relaunch, ServingCheckLook: supervise.CheckLook{Look: 2, OpenedAt: opened}}
		done := completionPosition{issue: &record.Issue{Key: "LEGION-208", Phase: phase.Done},
			check: &record.ProductionCheck{Issue: "LEGION-208", OpenedAt: opened, Role: claim.RoleTester, Looks: 2, LookOpen: true}}
		if got, want := owedBy(serving, done, "", workflow.RoleFor), (OwedResponse{Owes: true, Work: "the production check of LEGION-208"}); got != want {
			t.Errorf("a tester serving a look and holding %s (confirmed %v) owes %+v, want %+v", relaunch.ID, !confirmed.IsZero(), got, want)
		}
		working := supervise.Claim{Role: claim.RoleTester, Pending: relaunch}
		inPhase := completionPosition{issue: &record.Issue{Key: "LEGION-208", Phase: phase.Testing, Generation: 1}}
		if serving := working.ServingRun(); serving != 0 {
			t.Fatalf("a tester holding %s (confirmed %v) serves run %d, want none: the case's own premise", relaunch.ID, !confirmed.IsZero(), serving)
		}
		if got := owedBy(working, inPhase, "", workflow.RoleFor); got != (OwedResponse{}) {
			t.Errorf("a tester in its open phase holding %s (confirmed %v) and serving no run owes %+v, want nothing", relaunch.ID, !confirmed.IsZero(), got)
		}
	}
}

// A phase task leaves the look a claim served in place (supervise's ServingCheck), and the
// completion route takes a phase's completion from such a claim unless its report names its check
// (checkOpenedAt). So a tester that served a look of a check that has since closed, and holds a
// confirmed testing task in testing again, a root reopened in its linger, owes the testing phase,
// not nothing; while the check still holds its served look open, it owes that look.
func TestClaimOwedIsThePhaseOnceTheCheckHasLeftTheLookItsTesterServed(t *testing.T) {
	opened := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	closed := opened.Add(2 * time.Hour)
	testing := &supervise.Delivery{ID: "testing:3", Task: "Test the change.", Generation: 2, Entry: 3, ConfirmedAt: opened.Add(3 * time.Hour)}
	tester := supervise.Claim{Role: claim.RoleTester, Pending: testing, GivenEntry: 3,
		ServingCheckLook: supervise.CheckLook{Look: 2, OpenedAt: opened}}
	if served := tester.ServingCheck(); served.Look != 2 {
		t.Fatalf("the tester's confirmed testing task serves look %d, want the look it served before, 2, kept in place", served.Look)
	}
	check := func(closedAt *time.Time) completionPosition {
		return completionPosition{issue: &record.Issue{Key: "LEGION-208", Phase: phase.Testing, Generation: 2}, entry: 3,
			check: &record.ProductionCheck{Issue: "LEGION-208", OpenedAt: opened, ClosedAt: closedAt, Role: claim.RoleTester, Looks: 2, LookOpen: closedAt == nil}}
	}
	if got, want := owedBy(tester, check(&closed), "", workflow.RoleFor), (OwedResponse{Owes: true, Work: "the testing phase of LEGION-208"}); got != want {
		t.Errorf("a tester whose served look's check closed, holding a confirmed testing task in testing, owes %+v, want %+v", got, want)
	}
	if got, want := owedBy(tester, check(nil), "", workflow.RoleFor), (OwedResponse{Owes: true, Work: "the production check of LEGION-208"}); got != want {
		t.Errorf("a tester whose served look the check still holds open owes %+v, want %+v", got, want)
	}
}

// The route never answers another claim's work, and it refuses exactly as claims/activity does:
// another claim's token, a wrong secret and a claim the daemon does not hold are each
// claim.InvalidSecret, and a body naming no claim is a 400 naming the field. It holds the session
// too, which activity does not read: a session the claim does not record is refused the same way.
func TestClaimOwedRefusesACallerThatCannotProveItsClaim(t *testing.T) {
	h, _, _ := newEntryHarness(t)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	tester.owes(t, "the testing phase of LEGION-208")

	for _, tc := range []struct {
		name string
		req  OwedRequest
	}{
		{name: "another claim's token", req: OwedRequest{ClaimToken: tester.token, SessionID: implementer.session, Secret: implementer.secret}},
		{name: "a wrong secret", req: OwedRequest{ClaimToken: tester.token, SessionID: tester.session, Secret: "not-the-secret"}},
		{name: "a wrong session", req: OwedRequest{ClaimToken: tester.token, SessionID: implementer.session, Secret: tester.secret}},
		{name: "a claim the daemon does not hold", req: OwedRequest{ClaimToken: "legion-legion-legion-999-tester", SessionID: tester.session, Secret: tester.secret}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantRefusal(t, askOwed(h, tc.req), claim.InvalidSecret.Status, claim.InvalidSecret.Message)
		})
	}
	t.Run("a body naming no claim", func(t *testing.T) {
		wantRefusal(t, askOwed(h, OwedRequest{SessionID: tester.session, Secret: tester.secret}), http.StatusBadRequest, "claimToken is required")
	})
}

// A daemon that cannot say which role works a phase answers 500, not "nothing owed": the answer
// says only what the record says, and an empty answer would give a guess as the record's word.
// The reminder reads the 500 as a read that failed, which reminds no one (dispatch://INFRA-2746
// R7); the daemon's silence sweep sends a quiet worker that owes its phase its task again.
func TestClaimOwedIsAFailureWhereTheDaemonCannotReadWhoWorksThePhase(t *testing.T) {
	h := newHarness(t)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	recorder := askOwed(h, OwedRequest{ClaimToken: tester.token, SessionID: tester.session, Secret: tester.secret})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("owed on a daemon with no RoleFor = %d, want 500; body %s", recorder.Code, recorder.Body)
	}
}

// unreadableIssues is a record store whose read of an issue fails, as a database that is down or
// refuses the read fails it.
type unreadableIssues struct{ record.Store }

func (unreadableIssues) Issue(context.Context, pgx.Tx, string) (*record.Issue, error) {
	return nil, errors.New("the database refused the read")
}

// A daemon that cannot read the claim's issue record answers a failure, not "nothing owed": an
// empty answer on every failed read would give the store's failure as the record's word that the
// claim owes nothing. The reminder reads the failure as a read that failed, which reminds no one
// (dispatch://INFRA-2746 R7), and the daemon's silence sweep sends a quiet worker that owes its
// phase its task again.
func TestClaimOwedIsAFailureWhereTheDaemonCannotReadTheIssueRecord(t *testing.T) {
	h := newHarness(t)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	serveOwedOver(h, h.store.Pool(), unreadableIssues{record.NewStore()})
	recorder := askOwed(h, OwedRequest{ClaimToken: tester.token, SessionID: tester.session, Secret: tester.secret})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("owed on a daemon whose issue read fails = %d, want 500; body %s", recorder.Code, recorder.Body)
	}
}

// The answer as the route writes it, built through owedBy from a tester's claim serving run 1 of
// an issue in testing on run 1, which its tree root's architect owns, so the fixture the contracts
// mirror is held to is the handler's own body.
func TestClaimOwedResponseGolden(t *testing.T) {
	position := completionPosition{issue: &record.Issue{Key: "LEGION-208", Phase: phase.Testing, Generation: 1}}
	golden(t, "owed.json", owedBy(supervise.Claim{Role: claim.RoleTester, ServingGeneration: 1}, position, "legion-legion-legion-208-architect", workflow.RoleFor))
}

// The answer names the architect that owns the claim now, the end of the owner walk the daemon's
// own notices about the claim's issue take (record.OwnerWalk.Architect from the issue itself): a
// child's worker is owned by the child's own architect while that claim holds its role, and once
// the child's architect is retired, by the nearest architect above it whose claim does, here the
// tree root's. It is named whether or not the claim owes. An architect's own ask names none, and
// neither does a worker on an issue the workflow does not record, which has no tree to walk.
func TestClaimOwedNamesTheArchitectThatOwnsTheClaim(t *testing.T) {
	h, _, _ := newEntryHarness(t)
	seedTree(t, h, "LEGION-208", "LEGION-209")
	// A worker owes its phase only while its claim serves the issue's current run (claimServes),
	// and every task it is handed carries that run, so the tree stands on a run of its own, as
	// admission's first start of it leaves it.
	for _, key := range []string{"LEGION-208", "LEGION-209"} {
		setIssue(t, h, key, func(issue *record.Issue) { issue.Generation = 1 })
	}
	rootArchitect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	childArchitect := newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RoleArchitect)
	planner := newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RolePlanner)
	tester := newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RoleTester)

	planning := "the planning phase of LEGION-209"
	if got, want := planner.owed(t), (OwedResponse{Owes: true, Work: planning, Architect: childArchitect.token}); got != want {
		t.Fatalf("the child's planner is told %+v, want %+v", got, want)
	}
	// The tester owes nothing while the child plans, and the architect that owns it is named all
	// the same.
	if got, want := tester.owed(t), (OwedResponse{Architect: childArchitect.token}); got != want {
		t.Fatalf("the child's tester is told %+v, want %+v", got, want)
	}
	// The child's own architect owns no architect this answer names.
	if got := childArchitect.owed(t); got != (OwedResponse{}) {
		t.Fatalf("the child's architect is told %+v, want nothing at all", got)
	}

	machine, ok := h.supervisor.Machine(childArchitect.token)
	if !ok {
		t.Fatalf("no machine for %s", childArchitect.token)
	}
	if err := machine.Handle(h.ctx, supervise.RequestStop{Claim: childArchitect.token, By: supervise.SuspendedByOperator}); err != nil {
		t.Fatalf("stop %s: %v", childArchitect.token, err)
	}
	if state := h.stored(childArchitect.token).State; state != supervise.StateRetired {
		t.Fatalf("the stopped child architect is %s, want retired: the case's own premise", state)
	}
	if got, want := planner.owed(t), (OwedResponse{Owes: true, Work: planning, Architect: rootArchitect.token}); got != want {
		t.Fatalf("the child's planner, its architect retired, is told %+v, want %+v", got, want)
	}
	if got := rootArchitect.owed(t); got != (OwedResponse{}) {
		t.Fatalf("the root's architect is told %+v, want nothing at all", got)
	}

	stray := newLiveClaim(t, h, "LEGION-999", claim.RoleImplementer)
	if got := stray.owed(t); got != (OwedResponse{}) {
		t.Fatalf("a worker on an unrecorded issue is told %+v, want nothing at all", got)
	}
}
