package workflow

// One rule says whether the merge of the head the pull request stands at is owed to a role
// (owedAsk): the hold at each start, the completion fence, the landed message and the delivery of
// each row read it, so a worker is held exactly while the merge is owed, and is told to wait only
// if it was held. A merge owed at the head always has its one asked role: a move that ends the ask
// asks the role of the phase it enters where the merge is owed there, and an asked role whose
// claim ended keeps it, the ask sent again at the ready of that role's next launch.

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The plan `implementer reviewer tester`: the implementer is asked in reviewing, and a later read
// finds the head mergeable, which clears the record's confirmed conflict and leaves the ask, which
// the implementer can still act on. The approval then starts the tester, which is held, as the
// fence reads it.
func TestATesterStartedAfterAMergeableReadIsHeldWhileTheAskIsOwed(t *testing.T) {
	pool := plannedConflictIssue(t, phase.Reviewing, settledGreen("lint"), "implementer", "reviewer", "tester")
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main",
		HeadSHA: conflictHead, Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean})
	if pr := recordedPR(t, pool, "LEGION-208"); pr.ConflictingHead != "" || pr.MergeForward == nil {
		t.Fatalf("after the mergeable read the record holds conflicting head %q and ask %+v, want none and the implementer's ask", pr.ConflictingHead, pr.MergeForward)
	}
	applyRefusingNothing(t, pool, readsAfter(3*time.Minute), intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 1,
		State: "approved", CommitID: conflictHead, HeadSHA: conflictHead, Author: "a-writer", AuthorCanWrite: true, Body: "ship it",
		SubmittedAt: firstConflictRead.Add(3 * time.Minute)})
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("after the approval the issue is in %s, want testing", got)
	}
	why := record.ConflictCheckNotGreen
	want := []record.MergeForwardMessage{implementerAsked(phase.Reviewing, why), heldIn(claim.RoleReviewer, phase.Reviewing, why),
		heldIn(claim.RoleTester, phase.Testing, why)}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Errorf("the merge-forward messages are %+v, want %+v: the tester starts while the implementer's ask is owed", got, want)
	}
	if worker, held := heldWorker(storedIssue(t, pool, "LEGION-208"), recordedPR(t, pool, "LEGION-208")); !held || worker != claim.RoleTester {
		t.Errorf("the fence's held rule names %q (held %t), want the tester", worker, held)
	}
}

// On the default steps the implementer, asked in implementing, moves back into planning, which
// stands before the implementer, where no merge forward stands: nobody is asked again, so the
// implementer is told once that the merge is no longer its work, its ask is delivered no more,
// the planner is not held, and the fence tells no worker to wait.
func TestAnAskOfImplementingThatNoPhaseAsksAgainIsWithdrawn(t *testing.T) {
	pool := seedConflictIssue(t, phase.Implementing, "in_progress", settledGreen("lint"))
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleImplementer,
		To: phase.Planning, Reason: "the plan misses a case"})
	if got := issuePhase(t, pool); got != phase.Planning {
		t.Fatalf("after the move back the issue is in %s, want planning", got)
	}
	why := record.ConflictCheckNotGreen
	want := []record.MergeForwardMessage{implementerAsked(phase.Implementing, why), withdrawnIn(phase.Planning, why)}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Errorf("the merge-forward messages are %+v, want %+v: the implementer is told once, and the planner is not held", got, want)
	}
	issue, pr := storedIssue(t, pool, "LEGION-208"), recordedPR(t, pool, "LEGION-208")
	if why := leftMergeForwardAt(issue, false, &pr, claim.RoleImplementer, record.MergeForwardAsk, conflictHead); why == "" {
		t.Errorf("the implementer's ask of %s is still delivered in planning, want it ended", conflictHead)
	}
	if worker, held := heldWorker(issue, pr); held {
		t.Errorf("the fence's held rule names %q, want nobody: no worker was held", worker)
	}
}

// On the default steps a conflict confirmed in merging, with a check the base requires not green,
// asks the merger. The merger moves the issue back into testing, where the merger's ask is owed by
// nobody: the implementer is asked in testing, for R0's reason with no rules read at the move, and
// the tester is held.
func TestTheMergersMoveBackAsksTheRoleOfThePhaseItEnters(t *testing.T) {
	pool := seedConflictIssue(t, phase.Merging, "retro", settledGreen("lint"))
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleMerger,
		To: phase.Testing, Reason: "the merge needs a test of the base's change"})
	if got := issuePhase(t, pool); got != phase.Testing {
		t.Fatalf("after the merger's move back the issue is in %s, want testing", got)
	}
	why := record.ConflictRulesUnread
	want := []record.MergeForwardMessage{mergerAsked(), implementerAsked(phase.Testing, why), heldIn(claim.RoleTester, phase.Testing, why)}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Errorf("the merge-forward messages are %+v, want %+v: the implementer is asked in testing and the tester is held", got, want)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Asked() != claim.RoleImplementer || mf.Phase != phase.Testing {
		t.Errorf("the record's merge forward = %+v, want the implementer's ask in testing", mf)
	}
}

// implementerReady is the ready of the implementer's claim at launch, which the daemon applies once
// for each launch of the claim (intake.ClaimReady).
func implementerReady(launch uint64) intake.ClaimReady {
	return intake.ClaimReady{Issue: "LEGION-208", Role: claim.RoleImplementer, Launch: launch}
}

// asksDelivered is the daemon's executor delivering the record's ask of conflictHead to the
// asked role's launch: the ask's rows leave the outbox, as a delivered row does, and the record's
// ask keeps that launch (record.MergeForward.SentLaunch), as the row's end records it
// (MergeForwardAskEnded).
func asksDelivered(t *testing.T, pool *pgxpool.Pool, launch uint64) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `delete from outbox where kind = $1 and issue = 'LEGION-208' and payload->>'step' = $2`,
		string(record.OutboxKindMergeForward), string(record.MergeForwardAsk)); err != nil {
		t.Fatalf("deliver the ask's rows: %v", err)
	}
	pr := recordedPR(t, pool, "LEGION-208")
	if pr.MergeForward == nil {
		t.Fatalf("the record holds no ask to deliver")
	}
	delivered := *pr.MergeForward
	delivered.SentLaunch = launch
	pr.MergeForward = &delivered
	seedPR(t, pool, pr)
}

// On the default steps the implementer is asked in testing, the tester is held, and the ask's row
// is delivered to the implementer's launch 1. The implementer's claim fails: the merge is still
// owed to it, so the tester stays held, the failure sends no merge-forward message, and the
// architect is told once, in the worker-died notice its failure sends. The ready of the
// implementer's launch 2, which no row reaches, asks it again, once, and the tester stays held; a
// merge that lands then tells the tester the new head.
func TestAnAskOwedToAFailedClaimIsSentAgainAtItsNextLaunch(t *testing.T) {
	pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	asksDelivered(t, pool, 1)
	applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleImplementer})
	why := record.ConflictCheckNotGreen
	asked := []record.MergeForwardMessage{heldIn(claim.RoleTester, phase.Testing, why)}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, asked) {
		t.Errorf("after the implementer's claim failed the merge-forward messages are %+v, want %+v: the failure sends none", got, asked)
	}
	issue, pr := storedIssue(t, pool, "LEGION-208"), recordedPR(t, pool, "LEGION-208")
	if issue.Phase != phase.Testing || pr.MergeForward == nil {
		t.Fatalf("after the implementer's claim failed the issue is in %s and the ask is %+v, want testing and the ask", issue.Phase, pr.MergeForward)
	}
	if worker, held := heldWorker(issue, pr); !held || worker != claim.RoleTester {
		t.Errorf("after the failure the fence's held rule names %q (held %t), want the tester: the merge is still owed", worker, held)
	}
	// The notice is queued on the issue's row, which the outbox routes to the architect that owns
	// the issue, as every notice of the issue is routed; the issue is not held, so no held notice
	// reaches the controller.
	if died := noticesOfKind(t, pool, "LEGION-208", "worker-died"); len(died) != 1 || died[0].Role != claim.RoleImplementer {
		t.Errorf("the worker-died notices of LEGION-208 are %+v, want one, naming the implementer's failure", died)
	}
	if held := noticesOfKind(t, pool, "LEGION-208", "held"); len(held) != 0 {
		t.Errorf("the held notices of LEGION-208 are %+v, want none: the implementer does not work testing", held)
	}

	applyRefusingNothing(t, pool, readsAfter(3*time.Minute), implementerReady(2))
	again := []record.MergeForwardMessage{heldIn(claim.RoleTester, phase.Testing, why), sentAgain(implementerAsked(phase.Testing, why), 2, 1)}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, again) {
		t.Errorf("after the ready of the implementer's launch 2 the merge-forward messages are %+v, want %+v: the ask once more", got, again)
	}
	issue, pr = storedIssue(t, pool, "LEGION-208"), recordedPR(t, pool, "LEGION-208")
	if pr.MergeForward == nil || pr.MergeForward.SentLaunch != 2 {
		t.Errorf("after the ready of launch 2 the record's ask is %+v, want it sent at launch 2", pr.MergeForward)
	}
	if worker, held := heldWorker(issue, pr); !held || worker != claim.RoleTester {
		t.Errorf("after the ready the fence's held rule names %q (held %t), want the tester", worker, held)
	}
	landsMergeForward(t, pool)
	if got := toldWho(mergeForwardMessages(t, pool)[len(again):]); !reflect.DeepEqual(got, []string{"tester landed"}) {
		t.Errorf("the landing queued %v, want the tester told the new head", got)
	}
}

// On the default steps the merger is asked in merging, its row is delivered to its launch 1, and
// its claim fails, which holds the issue. The architect's retry starts the merger again and sends
// nothing itself: the ready of the merger's launch 2, which no row reaches, asks it again, once.
func TestAMergersAskIsSentAgainAtTheReadyOfItsRetriedLaunch(t *testing.T) {
	pool := seedConflictIssue(t, phase.Merging, "retro", settledGreen("lint"))
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	asksDelivered(t, pool, 1)
	applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleMerger})
	if got := issuePhase(t, pool); got != phase.Held {
		t.Fatalf("after the merger's claim failed the issue is in %s, want held", got)
	}
	applyRefusingNothing(t, pool, readsAfter(3*time.Minute), intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision, Reason: "go on"})
	if got := mergeForwardMessages(t, pool); len(got) != 0 {
		t.Errorf("after the retry the merge-forward messages are %+v, want none: the retry's launch is not ready yet", got)
	}
	applyRefusingNothing(t, pool, readsAfter(4*time.Minute), intake.ClaimReady{Issue: "LEGION-208", Role: claim.RoleMerger, Launch: 2})
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, []record.MergeForwardMessage{sentAgain(mergerAsked(), 2, 1)}) {
		t.Errorf("after the ready of the merger's launch 2 the merge-forward messages are %+v, want the merger's ask sent again", got)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != 2 {
		t.Errorf("after the ready of launch 2 the record's ask is %+v, want it sent at launch 2", mf)
	}
}

// One launch is told an owed ask once: a second ready of the launch the ask was last sent at, and
// a ready of an earlier launch, send nothing. The daemon applies one ready for each launch, so this
// holds the record's own mark of the launch it sent the ask at.
func TestAReadyOfALaunchTheAskWasSentAtSendsItNoMore(t *testing.T) {
	pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	asksDelivered(t, pool, 1)
	applyRefusingNothing(t, pool, readsAfter(2*time.Minute), implementerReady(2))
	why := record.ConflictCheckNotGreen
	want := []record.MergeForwardMessage{heldIn(claim.RoleTester, phase.Testing, why), sentAgain(implementerAsked(phase.Testing, why), 2, 1)}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the ready of the implementer's launch 2 the merge-forward messages are %+v, want %+v", got, want)
	}
	applyRefusingNothing(t, pool, readsAfter(3*time.Minute), implementerReady(2), implementerReady(1))
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Errorf("after a second ready of launch 2 and a ready of launch 1 the merge-forward messages are %+v, want %+v as before", got, want)
	}
}

// The implementer is asked in testing, and its claim fails before the ask's row is delivered: the
// row stays queued for the implementer, and its delivery reaches the next launch. So the ready of
// the implementer's launch 2 writes no second row, and the record keeps no launch it was sent at.
func TestAReadyWhileTheAsksRowIsQueuedWritesNoSecondAsk(t *testing.T) {
	pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleImplementer})
	applyRefusingNothing(t, pool, readsAfter(3*time.Minute), implementerReady(2))
	why := record.ConflictCheckNotGreen
	want := []record.MergeForwardMessage{implementerAsked(phase.Testing, why), heldIn(claim.RoleTester, phase.Testing, why)}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Errorf("after the ready of the implementer's launch 2 the merge-forward messages are %+v, want %+v: the queued row reaches that launch", got, want)
	}
	if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.SentLaunch != 0 {
		t.Errorf("after the ready the record's ask is %+v, want it sent at no launch's ready", mf)
	}
}

// A ready of the implementer's later launch asks it again only while the merge of the head is
// owed to it: an ask that was withdrawn, and one that landed, are sent nothing at that ready. The
// control's ask is owed, and is sent again.
func TestAReadyAsksAgainOnlyWhileTheMergeIsOwed(t *testing.T) {
	why := record.ConflictCheckNotGreen
	rules := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		name  string
		seed  func(t *testing.T) *pgxpool.Pool
		again []record.MergeForwardMessage
	}{
		{name: "control: the implementer's ask made in testing", seed: func(t *testing.T) *pgxpool.Pool {
			pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
			confirmsConflict(t, pool, rules)
			return pool
		}, again: []record.MergeForwardMessage{sentAgain(implementerAsked(phase.Testing, why), 2, 1)}},
		{name: "the ask of implementing, withdrawn by the move back into planning", seed: func(t *testing.T) *pgxpool.Pool {
			pool := seedConflictIssue(t, phase.Implementing, "in_progress", settledGreen("lint"))
			confirmsConflict(t, pool, rules)
			applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleImplementer,
				To: phase.Planning, Reason: "the plan misses a case"})
			if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || !mf.Withdrawn {
				t.Fatalf("after the move back the record's ask is %+v, want it withdrawn", mf)
			}
			return pool
		}},
		{name: "the ask made in testing, landed", seed: func(t *testing.T) *pgxpool.Pool {
			pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
			confirmsConflict(t, pool, rules)
			landsMergeForward(t, pool)
			return pool
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := tc.seed(t)
			asksDelivered(t, pool, 1)
			before := len(mergeForwardMessages(t, pool))
			applyRefusingNothing(t, pool, readsAfter(5*time.Minute), implementerReady(2))
			got := mergeForwardMessages(t, pool)[before:]
			if !slices.Equal(got, tc.again) {
				t.Errorf("the ready of the implementer's launch 2 queued %+v, want %+v", got, tc.again)
			}
		})
	}
}

// On the default steps the tester moves back into planning, which stands before the implementer,
// while the implementer's ask made in testing is owed: the planner is held, a merge that lands
// tells it the new head, and its completion at the head the merge replaced is refused.
func TestAPlannerBeforeTheImplementerIsToldAndFencedWhileTheMergeIsOwed(t *testing.T) {
	pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleTester,
		To: phase.Planning, Reason: "the plan misses a case"})
	why := record.ConflictCheckNotGreen
	asked := mergeForwardMessages(t, pool)
	if want := []record.MergeForwardMessage{implementerAsked(phase.Testing, why), heldIn(claim.RoleTester, phase.Testing, why),
		heldIn(claim.RolePlanner, phase.Planning, why)}; !reflect.DeepEqual(asked, want) {
		t.Errorf("after the move back the merge-forward messages are %+v, want %+v", asked, want)
	}
	landsMergeForward(t, pool)
	if got := toldWho(mergeForwardMessages(t, pool)[len(asked):]); !reflect.DeepEqual(got, []string{"planner landed"}) {
		t.Errorf("the landing in planning queued %v, want the planner told the new head", got)
	}
	result := applyFacts(t, pool, readsAfter(4*time.Minute))("the planner at the replaced head", intake.HandoffComplete{Generation: 1,
		Issue: "LEGION-208", Role: claim.RolePlanner, Claim: "planner-claim", Summary: "planned", Commit: conflictHead,
		HandoffRecord: "plan-1", HandoffVersion: 1})
	if code := refusalCode(result); code != "HANDOFF_HEAD_MERGED_FORWARD" {
		t.Errorf("the planner's completion at %s, the head the merge replaced, was answered %q, want HANDOFF_HEAD_MERGED_FORWARD", conflictHead, code)
	}
}

// On the default steps the implementer is asked in testing and its merge lands, and then the
// branch moves back to the head the merge replaced, as a forced push moves it: the record's ask
// has landed, and its confirmed conflict is of the head the pull request stands at again. A landed
// ask is owed to nobody (openAsk), so the tester's completion and the reviewer's approval at that
// head move the issue on with nobody held, and the move into merging asks the merger to merge the
// base forward onto that head, once, and holds it for no ask. A later read that finds the head
// still conflicting asks nothing more: the merger owes the merge.
func TestABranchBackAtTheHeadALandedMergeReplacedIsTheMergersToMergeForward(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
	confirmsConflict(t, pool, requiresCI)
	landsMergeForward(t, pool)
	applyRefusingNothing(t, pool, readsAfter(4*time.Minute), intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", Base: "main", HeadSHA: conflictHead, Before: forwardHead, Sender: "legion-implementer[bot]"})
	if pr := recordedPR(t, pool, "LEGION-208"); pr.HeadSHA != conflictHead || pr.ConflictingHead != conflictHead ||
		pr.MergeForward == nil || pr.MergeForward.Landed != forwardHead {
		t.Fatalf("after the branch moved back the record holds head %s, conflicting head %s and ask %+v, want %s twice and the ask landed at %s",
			pr.HeadSHA, pr.ConflictingHead, pr.MergeForward, conflictHead, forwardHead)
	}
	why := record.ConflictCheckNotGreen
	before := []record.MergeForwardMessage{implementerAsked(phase.Testing, why), heldIn(claim.RoleTester, phase.Testing, why),
		{Role: claim.RoleTester, Step: record.MergeForwardLanded, Head: conflictHead, Landed: forwardHead, Base: "main",
			Phase: phase.Testing, PullRequest: 42, Why: why}}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, before) {
		t.Errorf("after the branch moved back the merge-forward messages are %+v, want %+v", got, before)
	}

	applyRefusingNothing(t, pool, readsAfter(5*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
		Role: claim.RoleTester, Claim: "tester-claim", Summary: "tested", Verdict: "pass", Commit: conflictHead,
		HandoffRecord: "test-1", HandoffVersion: 1})
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the tester's pass at %s the issue is in %s, want reviewing", conflictHead, got)
	}
	applyRefusingNothing(t, pool, readsAfter(6*time.Minute), intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 1,
		State: "approved", CommitID: conflictHead, HeadSHA: conflictHead, Author: "a-writer", AuthorCanWrite: true, Body: "ship it",
		SubmittedAt: firstConflictRead.Add(6 * time.Minute)})
	if got := issuePhase(t, pool); got != phase.Accepting {
		t.Fatalf("after the reviewer's approval of %s the issue is in %s, want accepting", conflictHead, got)
	}
	applyRefusingNothing(t, pool, readsAfter(7*time.Minute), rootsSignOff(t, pool, readsAfter(7*time.Minute), "LEGION-208"))
	if got := issuePhase(t, pool); got != phase.Retro {
		t.Fatalf("after the sign-off the issue is in %s, want retro", got)
	}
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, before) {
		t.Errorf("after the moves into reviewing, accepting and the retro the merge-forward messages are %+v, want %+v: nobody is held for a landed ask",
			got, before)
	}

	applyRefusingNothing(t, pool, readsAfter(8*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
		Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: conflictHead})
	if got := issuePhase(t, pool); got != phase.Merging {
		t.Fatalf("after the retro's completion the issue is in %s, want merging", got)
	}
	want := append(slices.Clip(before), mergerAsked())
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Errorf("after the move into merging the merge-forward messages are %+v, want %+v: the merger asked once, and held for nothing", got, want)
	}
	issue, pr := storedIssue(t, pool, "LEGION-208"), recordedPR(t, pool, "LEGION-208")
	if mf := pr.MergeForward; mf == nil || mf.Asked() != claim.RoleMerger || mf.Head != conflictHead || mf.Landed != "" {
		t.Errorf("the record's merge forward = %+v, want the merger's open ask of %s", mf, conflictHead)
	}
	if why := leftMergeForwardAt(issue, false, &pr, claim.RoleMerger, record.MergeForwardAsk, conflictHead); why != "" {
		t.Errorf("the merger's ask is undelivered (%s), want it delivered", why)
	}

	applyRefusingNothing(t, pool, readsAfter(9*time.Minute), conflictingRead(requiresCI))
	if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
		t.Errorf("after a later read finds %s conflicting the merge-forward messages are %+v, want %+v as before", conflictHead, got, want)
	}
}
