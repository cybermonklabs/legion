package workflow

// A merge forward that lands asks the reviewer to read the merge only where the issue's task has a
// reviewer step that already ran (mergeForwardLanded). A task whose plan has none stands on no
// review of the old head, and no reviewer step runs for it, so nobody would ever approve the new
// head: the reviewer is asked nothing, and the merger goes on at the new head, told no wait for an
// approval, both by the landed message that reaches it in merging and by the task it is started
// with when it enters merging after the landing. The control is the same issue on a plan with a
// reviewer step, where the review is asked and both tell the merger to wait.

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// approvalWait is the words that hold the merger's merge to the reviewer's approval of the head a
// merge forward left, in the landed message (record.MergeForwardMessage) and in its task (task).
const approvalWait = "only once the reviewer's approval of"

// plannedConflictIssue is seedConflictIssue on a run with plan rows: the owner's plan whose steps
// are steps, with task a running at the place of at (the step whose role runs as at, its retro or
// its merge), the Dispatch status of at, and pr as the record holds the pull request.
func plannedConflictIssue(t *testing.T, at phase.Phase, pr record.PullRequest, steps ...string) *pgxpool.Pool {
	t.Helper()
	pool := seedConflictIssue(t, at, PhaseStatus(at), pr)
	seedStepsPlan(t, pool, "LEGION-208", 1, steps...)
	place := record.TaskPlace{Kind: record.PlaceRetro}
	switch at {
	case phase.Merging:
		place = record.TaskPlace{Kind: record.PlaceMerge}
	case phase.Retro:
	default:
		roles := plan.BuiltinRoles()
		for i, word := range steps {
			if role, _, _ := strings.Cut(word, ":"); roles[role].Phase == at {
				place = record.TaskPlace{Kind: record.PlaceStep, Position: i + 1}
			}
		}
		if place.Kind != record.PlaceStep {
			t.Fatalf("the steps %v hold no step that runs as %s", steps, at)
		}
	}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().MoveTask(t.Context(), tx, "LEGION-208", 1, "a", place, record.TaskRunning)
	})
	return pool
}

// askedInReviewing is a pull request whose record holds the implementer's ask of the merge forward
// of conflictHead, made in reviewing while a check the base requires was not green, and not landed.
// Where every step of the task has run, a conflict waits for the merger (cybermonklabs/legion#4979),
// so an ask of the implementer stands there only where it was made at an earlier step.
func askedInReviewing() record.PullRequest {
	pr := settledGreen("lint")
	pr.MergeForward = &record.MergeForward{Head: conflictHead, Base: "main", Role: claim.RoleImplementer, Phase: phase.Reviewing,
		Why: record.ConflictCheckNotGreen, AskedAt: firstConflictRead}
	return pr
}

// toldWho is each merge-forward message as the role it goes to and its step, in the order written.
func toldWho(messages []record.MergeForwardMessage) []string {
	told := make([]string, len(messages))
	for i, message := range messages {
		told[i] = string(message.Role) + " " + string(message.Step)
	}
	return told
}

func TestAMergeForwardAsksTheReviewerOnlyOfATaskWithAReviewerStep(t *testing.T) {
	t.Parallel()
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		name   string
		steps  []string
		last   phase.Phase // the phase of the plan's last step
		review bool
	}{
		{name: "a plan with no reviewer step", steps: []string{"implementer", "tester"}, last: phase.Testing, review: false},
		{name: "control: a plan with a reviewer step", steps: []string{"implementer", "reviewer"}, last: phase.Reviewing, review: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("the merger in merging", func(t *testing.T) {
				pool := plannedConflictIssue(t, phase.Merging, settledGreen("lint"), tc.steps...)
				confirmsConflict(t, pool, requiresCI)
				asked := len(mergeForwardMessages(t, pool))
				landsMergeForward(t, pool)

				got := mergeForwardMessages(t, pool)[asked:]
				want := []string{"merger landed"}
				if tc.review {
					want = append(want, "reviewer review")
				}
				if !reflect.DeepEqual(toldWho(got), want) {
					t.Fatalf("the landing queued %v, want %v", toldWho(got), want)
				}
				// The text is composed as the outbox composes it at delivery: from the pull request
				// as the record holds it (the daemon's mergeForwardMessage).
				recorded := recordedPR(t, pool, "LEGION-208")
				if told := got[0].Message("LEGION-208", recorded); strings.Contains(told, approvalWait) != tc.review {
					t.Fatalf("the merger was told %q; want a wait for the reviewer's approval: %t", told, tc.review)
				}
				if mf := recorded.MergeForward; mf == nil || mf.Landed != forwardHead || mf.ReviewAsked != tc.review {
					t.Fatalf("the record's merge forward = %+v, want it landed at %s with the review asked: %t", mf, forwardHead, tc.review)
				}
				if got := issuePhase(t, pool); got != phase.Merging {
					t.Fatalf("after the merge landed the issue is in %s, want merging", got)
				}
			})

			// Where every step of the task has run a conflict waits for the merger
			// (cybermonklabs/legion#4979), so a merge forward lands in retro only where the implementer
			// was asked at an earlier step, here the plan's last step, while a required check had no
			// green result, and its merge lands once the issue is in retro.
			t.Run("a merger entering merging after the landing", func(t *testing.T) {
				pr := settledGreen("lint")
				pr.MergeForward = &record.MergeForward{Head: conflictHead, Base: "main", Role: claim.RoleImplementer, Phase: tc.last,
					Why: record.ConflictCheckNotGreen, AskedAt: firstConflictRead}
				pool := plannedConflictIssue(t, phase.Retro, pr, tc.steps...)
				confirmsConflict(t, pool, requiresCI)
				asked := len(mergeForwardMessages(t, pool))
				landsMergeForward(t, pool)
				want := []string{}
				if tc.review {
					want = append(want, "reviewer review")
				}
				if got := toldWho(mergeForwardMessages(t, pool)[asked:]); !reflect.DeepEqual(got, want) {
					t.Fatalf("the landing in retro queued %v, want %v", got, want)
				}

				applyRefusingNothing(t, pool, readsAfter(4*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
					Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: forwardHead})
				if got := issuePhase(t, pool); got != phase.Merging {
					t.Fatalf("after retro's completion the issue is in %s, want merging", got)
				}
				if task := startTask(t, pool, claim.RoleMerger); task == "" || strings.Contains(task, approvalWait) != tc.review {
					t.Fatalf("the merger's task reads %q; want a wait for the reviewer's approval: %t", task, tc.review)
				}
			})
		})
	}
}

// A landed merge forward asks the reviewer to approve the new head only where the reviewer step
// of the issue's task already ran: it stands before the place the issue stands at. Such an issue
// stands on a review of the old head. A reviewer step still to come reviews the head the issue
// stands at when it runs, so it is asked nothing. So under `implementer acceptance:owner reviewer`
// a landing in accepting asks no review, and under `implementer reviewer tester` a landing in
// testing asks one, and the tester is told the landing as before. The controls are the other
// order of each pair. Where every step of the task has run - accepting as the last step, or the
// retro - a conflict waits for the merger, so a merge that lands there is one the implementer was
// asked for at an earlier step (askedInReviewing).
func TestAMergeForwardAsksTheReviewerOnlyWhereItsStepAlreadyRan(t *testing.T) {
	t.Parallel()
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		name  string
		at    phase.Phase
		steps []string
		pr    record.PullRequest
		want  []string
	}{
		{name: "accepting before the reviewer step", at: phase.Accepting,
			steps: []string{"implementer", "acceptance:owner", "reviewer"}, pr: settledGreen("lint"), want: []string{}},
		{name: "control: accepting after the reviewer step", at: phase.Accepting,
			steps: []string{"implementer", "reviewer", "acceptance:owner"}, pr: askedInReviewing(), want: []string{"reviewer review"}},
		{name: "testing after the reviewer step", at: phase.Testing,
			steps: []string{"implementer", "reviewer", "tester"}, pr: settledGreen("lint"), want: []string{"tester landed", "reviewer review"}},
		{name: "control: testing before the reviewer step", at: phase.Testing,
			steps: []string{"implementer", "tester", "reviewer"}, pr: settledGreen("lint"), want: []string{"tester landed"}},
		{name: "retro after a reviewer step that follows the acceptance", at: phase.Retro,
			steps: []string{"implementer", "acceptance:owner", "reviewer"}, pr: askedInReviewing(), want: []string{"reviewer review"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := plannedConflictIssue(t, tc.at, tc.pr, tc.steps...)
			confirmsConflict(t, pool, requiresCI)
			if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Head != conflictHead || mf.Asked() != claim.RoleImplementer {
				t.Fatalf("the record's merge forward in %s = %+v, want the implementer's ask of %s", tc.at, mf, conflictHead)
			}
			asked := len(mergeForwardMessages(t, pool))
			landsMergeForward(t, pool)

			if got := toldWho(mergeForwardMessages(t, pool)[asked:]); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the landing in %s under %v queued %v, want %v", tc.at, tc.steps, got, tc.want)
			}
			review := len(tc.want) > 0 && tc.want[len(tc.want)-1] == "reviewer review"
			if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Landed != forwardHead || mf.ReviewAsked != review {
				t.Fatalf("the record's merge forward = %+v, want it landed at %s with the review asked: %t", mf, forwardHead, review)
			}
			if got := issuePhase(t, pool); got != tc.at {
				t.Fatalf("after the merge landed the issue is in %s, want %s", got, tc.at)
			}
		})
	}
}

// Where every step of the task has run, the merge comes next (classify.MergeComesNext): the retro,
// and accepting where no step of the task follows the acceptance. There a confirmed conflict asks
// nobody, whatever the base's rules, and waits for merging, where the merger merges the base
// forward (cybermonklabs/legion#4979). In accepting that a step follows, the step still to run
// needs a head the base's checks run on, so the conflict is decided as it is in testing and
// reviewing: a check the base requires that is not green asks the implementer at once. Merging
// asks the merger whatever the task's steps. The controls are the acceptance as the task's last
// step, the retro of a task whose acceptance a step follows, and its merging.
func TestAConflictInAcceptingAsksTheImplementerOnlyWhereAStepFollows(t *testing.T) {
	t.Parallel()
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		name  string
		at    phase.Phase
		steps []string
		why   record.ConflictWhy
	}{
		{name: "accepting before a reviewer step", at: phase.Accepting,
			steps: []string{"implementer", "acceptance:owner", "reviewer"}, why: record.ConflictCheckNotGreen},
		{name: "accepting before a tester step", at: phase.Accepting,
			steps: []string{"implementer", "reviewer", "acceptance:owner", "tester"}, why: record.ConflictCheckNotGreen},
		{name: "control: accepting as the last step", at: phase.Accepting,
			steps: []string{"implementer", "reviewer", "acceptance:owner"}},
		{name: "control: the retro of a task whose acceptance a step follows", at: phase.Retro,
			steps: []string{"implementer", "acceptance:owner", "reviewer"}},
		{name: "control: the merging of a task whose acceptance a step follows", at: phase.Merging,
			steps: []string{"implementer", "acceptance:owner", "reviewer"}, why: record.ConflictMergeNext},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := plannedConflictIssue(t, tc.at, settledGreen("lint"), tc.steps...)
			confirmsConflict(t, pool, requiresCI)
			if tc.why == "" {
				if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked != nil {
					t.Fatalf("the record's merge forward = %+v, want none asked: the conflict waits for the merger", asked)
				}
				assertNothingActed(t, pool, tc.at)
				return
			}
			askedOnce(t, pool, tc.at, PhaseStatus(tc.at), tc.why, "")
		})
	}
}

// The reviewer's approval of a head held in a confirmed conflict ends its round and moves the
// issue to the task's next step (reviewRound). With every check the base requires green on the
// head, the conflict asked nothing when it was confirmed, and the move asks nothing either,
// wherever it goes: into accepting that a tester step follows, where the tester works at that head
// first, or into the retro of a task whose last step is the review. The conflict waits for
// merging, and the move into merging asks the merger (cybermonklabs/legion#4979,
// mergeForwardOnEntry). A move into accepting as the task's last step runs the same code as the
// first case, so it is no case of its own.
func TestAnApprovalOfAConflictingHeadLeavesTheMergeToTheMerger(t *testing.T) {
	t.Parallel()
	every := &record.BaseRules{Required: []string{"lint"}}
	for _, tc := range []struct {
		name  string
		steps []string
		next  phase.Phase
	}{
		{name: "into accepting before a tester step", steps: []string{"implementer", "reviewer", "acceptance:owner", "tester"}, next: phase.Accepting},
		{name: "into the retro, then merging", steps: []string{"implementer", "reviewer"}, next: phase.Retro},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := plannedConflictIssue(t, phase.Reviewing, settledGreen("lint"), tc.steps...)
			confirmsConflict(t, pool, every)
			assertNothingActed(t, pool, phase.Reviewing)
			applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 1,
				State: "approved", CommitID: conflictHead, HeadSHA: conflictHead, Author: "a-writer", AuthorCanWrite: true, Body: "ship it",
				SubmittedAt: firstConflictRead.Add(2 * time.Minute)})
			if got := issuePhase(t, pool); got != tc.next {
				t.Fatalf("after the approval the issue is in %s, want %s: an approval of a conflicting head ends its round", got, tc.next)
			}
			if got := mergeForwardMessages(t, pool); len(got) != 0 {
				t.Fatalf("merge-forward messages %+v after the move into %s, want none: the conflict waits for the merger", got, tc.next)
			}
			if tc.next != phase.Retro {
				return
			}
			applyRefusingNothing(t, pool, readsAfter(3*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
				Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: conflictHead})
			if got := issuePhase(t, pool); got != phase.Merging {
				t.Fatalf("after retro's completion the issue is in %s, want merging", got)
			}
			// The ask names GitHub's merge state of the read it rests on (dispatch://INFRA-2833 R3).
			want := []record.MergeForwardMessage{{Role: claim.RoleMerger, Step: record.MergeForwardAsk, Head: conflictHead,
				Base: "main", Phase: phase.Merging, PullRequest: 42, Why: record.ConflictMergeNext, MergeState: record.MergeStateDirty}}
			if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
				t.Fatalf("merge-forward messages %+v after the move into merging, want %+v", got, want)
			}
		})
	}
}

// completesImplementingAt is the implementer's completion of implementing at conflictHead, two
// minutes after the first conflicting read, without the merge forward asked of that head.
func completesImplementingAt(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
		Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "implemented", Commit: conflictHead, HandoffRecord: "implement-1", HandoffVersion: 1})
}

// implementerAsked is the implementer's ask of the merge forward of conflictHead in at, for why,
// naming GitHub's merge state of the read it rests on (dispatch://INFRA-2833 R3).
func implementerAsked(at phase.Phase, why record.ConflictWhy) record.MergeForwardMessage {
	return record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: conflictHead, Base: "main",
		Phase: at, PullRequest: 42, Why: why, MergeState: record.MergeStateDirty}
}

// sentAgain is ask as askAgain queues it at launch: it carries launch
// (record.MergeForwardMessage.SentLaunch).
func sentAgain(ask record.MergeForwardMessage, launch uint64) record.MergeForwardMessage {
	ask.SentLaunch = launch
	return ask
}

// An ask made in implementing covers implementing alone (mergeForwardOnEntry), and a move to where
// the merge comes next asks nothing at a conflicting head (classify.MergeComesNext). A parent whose
// last step is the implementer's is held before its retro while an issue beneath it is still in
// the workflow (holdBeforeRetro). Neither the hold nor its release into the retro once that issue
// leaves (StartHeldRetro) asks the merge forward again, and neither tells the architect a
// checks-red notice: the ask of implementing stays on the record. So the parent reaches merging as
// a parent that moved into its retro at once does: the move into merging asks the merger to merge
// the base forward (record.ConflictMergeNext), asks the implementer nothing and holds no merger,
// and the architect's one checks-red notice names merging. Had the hold or its release asked the
// implementer again, the merge would have stayed the implementer's, and the move into merging
// would hold the merger (holdStartedWorker) rather than ask it. The case is the parent with an issue
// beneath it, held and then released when that issue leaves; the control is the same parent with
// nothing beneath it, which moves into its retro at the implementer's completion.
func TestAHeldParentReleasedIntoItsRetroLeavesTheMergeToTheMerger(t *testing.T) {
	t.Parallel()
	unread := &record.BaseRules{Unread: "HTTP 502"}
	inImplementing := implementerAsked(phase.Implementing, record.ConflictRulesUnread)
	// asksNothingAgain is LEGION-208 standing in at after what happened last, with want the
	// merge-forward messages written, the implementer's ask of implementing still on the record,
	// and no checks-red notice.
	asksNothingAgain := func(t *testing.T, pool *pgxpool.Pool, after string, at phase.Phase, want []record.MergeForwardMessage) {
		t.Helper()
		if got := issuePhase(t, pool); got != at {
			t.Fatalf("after %s LEGION-208 is in %s, want %s", after, got, at)
		}
		if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
			t.Fatalf("after %s the merge-forward messages are %+v, want %+v: where the merge comes next the implementer is asked nothing again", after, got, want)
		}
		if told := capNotices(t, pool, "checks-red"); told != 0 {
			t.Fatalf("after %s the architect has %d checks-red notices, want none", after, told)
		}
		if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Role != claim.RoleImplementer || mf.Phase != phase.Implementing {
			t.Fatalf("after %s the record's merge forward = %+v, want the implementer's ask of implementing", after, mf)
		}
	}
	for _, tc := range []struct {
		name  string
		child bool
	}{
		{name: "held before its retro, then released into it", child: true},
		{name: "control: nothing beneath it, into its retro at once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := plannedConflictIssue(t, phase.Implementing, settledGreen("lint"), "implementer")
			// The release starts the retro only in a tree that runs (StartHeldRetro, record.TreeRunning).
			seedTreeLifecycle(t, pool, "LEGION-208")
			if tc.child {
				stepChild(t, pool, heldParentChild, phase.Implementing, nil)
			}
			confirmsConflict(t, pool, unread)
			completesImplementingAt(t, pool)
			want := []record.MergeForwardMessage{inImplementing}
			if tc.child {
				// The hold and its release make no move's call (holdBeforeRetro, StartHeldRetro), so
				// the implementer is told of the merger at the move into merging.
				asksNothingAgain(t, pool, "the implementer's completion", phase.Held, want)
				childLeavesDone(t, pool, readsAfter(3*time.Minute))
				asksNothingAgain(t, pool, "the release into the retro", phase.Retro, want)
			} else {
				// The move into the retro asks nobody, since the merge comes next, so the implementer
				// is told there that the merge is no longer its work.
				want = append(want, withdrawnIn(phase.Retro, record.ConflictRulesUnread))
				asksNothingAgain(t, pool, "the implementer's completion", phase.Retro, want)
			}

			applyRefusingNothing(t, pool, readsAfter(4*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
				Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: conflictHead})
			if got := issuePhase(t, pool); got != phase.Merging {
				t.Fatalf("after the retro's completion LEGION-208 is in %s, want merging", got)
			}
			want = append(want, mergerAsked())
			if tc.child {
				want = append(want, withdrawnIn(phase.Merging, record.ConflictMergeNext))
			}
			if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
				t.Fatalf("after the move into merging the merge-forward messages are %+v, want %+v: the merger is asked, the implementer told once that the merge is no longer its work, and no merger is held",
					got, want)
			}
			if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Role != claim.RoleMerger || mf.Phase != phase.Merging ||
				mf.Why != record.ConflictMergeNext {
				t.Fatalf("the record's merge forward = %+v, want the merger's ask in merging for the merge that comes next", mf)
			}
			if told := capNotices(t, pool, "checks-red"); told != 1 {
				t.Fatalf("the architect has %d checks-red notices, want one", told)
			}
			if reason, named := checksRedNotice(t, pool); named != phase.Merging || !strings.Contains(reason, "the merger is asked to merge main forward") {
				t.Fatalf("the checks-red notice names phase %q with reason %q, want merging and the merger asked", named, reason)
			}
		})
	}
}

// A planner step after the implementer works on a branch whose pull request is open, so it is read
// for a conflict as every later step is: a head push and a base push in its planning each ask for a
// read (classify.MergeStateReadOwed). An ask made in implementing is asked again on the move into
// that planning, as on a move into testing, with the planner's hold. The control is a planner
// before the implementer, whose
// planning comes before any pull request work and asks nothing.
func TestAPlannerAfterTheImplementerIsReadForAConflictAndAskedTheMerge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		steps []string
		want  []string
	}{
		{name: "a planner after the implementer", steps: []string{"implementer", "planner", "reviewer"}, want: []string{"LEGION-208"}},
		{name: "control: a planner before the implementer", steps: []string{"planner", "implementer", "reviewer"}, want: []string{}},
	} {
		t.Run(tc.name+", a head push", func(t *testing.T) {
			pool := plannedConflictIssue(t, phase.Planning, settledGreen("ci"), tc.steps...)
			asked := []string{}
			applyRefusingNothing(t, pool, readingEngine(&asked), intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", Base: "main", HeadSHA: headTwo, Before: conflictHead, Sender: "legion-implementer[bot]"})
			if !reflect.DeepEqual(asked, tc.want) {
				t.Fatalf("the head push in planning under %v asked reads of %v, want %v", tc.steps, asked, tc.want)
			}
		})
		t.Run(tc.name+", a base push", func(t *testing.T) {
			pool := plannedConflictIssue(t, phase.Planning, settledGreen("ci"), tc.steps...)
			asked := []string{}
			applyRefusingNothing(t, pool, readingEngine(&asked), basePushOntoMain)
			if !reflect.DeepEqual(asked, tc.want) {
				t.Fatalf("the base push in planning under %v asked reads of %v, want %v", tc.steps, asked, tc.want)
			}
		})
	}

	t.Run("an ask made in implementing is asked again in the planning after it", func(t *testing.T) {
		pool := plannedConflictIssue(t, phase.Implementing, settledGreen("lint"), "implementer", "planner", "reviewer")
		confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
		completesImplementingAt(t, pool)
		if got := issuePhase(t, pool); got != phase.Planning {
			t.Fatalf("after the implementer's completion the issue is in %s, want planning", got)
		}
		want := []record.MergeForwardMessage{implementerAsked(phase.Implementing, record.ConflictCheckNotGreen),
			implementerAsked(phase.Planning, record.ConflictCheckNotGreen), heldIn(claim.RolePlanner, phase.Planning, record.ConflictCheckNotGreen)}
		if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
			t.Fatalf("merge-forward messages %+v, want %+v: the planning after the implementer asks again and holds the planner", got, want)
		}
		if _, stays := checksRedNotice(t, pool); stays != phase.Planning {
			t.Fatalf("the checks-red notice names %s, want planning, the phase the issue stays in", stays)
		}
	})
}

// An ask of the implementer made in implementing covers implementing alone, so the move out of
// implementing asks again into a phase where a step of the task still runs before the merge
// (mergeForwardOnEntry). The control, `implementer tester`, asks again in testing, with the
// tester's hold and the checks-red notice. Where every step of the task has run, the merge comes
// next (classify.MergeComesNext) and the conflict waits for the merger
// (cybermonklabs/legion#4979). An owner's plan with no tester and no reviewer step, `implementer`
// or `implementer acceptance:owner`, takes the implementer's completion at the conflicting head
// without the merge straight into retro or accepting: that move asks nobody, holds nobody and
// tells the architect nothing, and the implementer is told once that the merge is no longer its
// work; the move into merging, after the owner's sign-off where the plan holds one, asks the
// merger.
func TestAnAskOfImplementingIsAskedAgainOnlyWhereAStepStillRuns(t *testing.T) {
	t.Parallel()
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	ask := func(role claim.Role, step record.MergeForwardStep, at phase.Phase, why record.ConflictWhy) record.MergeForwardMessage {
		return record.MergeForwardMessage{Role: role, Step: step, Head: conflictHead, Base: "main", Phase: at, PullRequest: 42, Why: why,
			MergeState: record.MergeStateDirty}
	}
	inImplementing := ask(claim.RoleImplementer, record.MergeForwardAsk, phase.Implementing, record.ConflictCheckNotGreen)
	// completes is LEGION-208 on the plan steps, asked in implementing to merge the base forward
	// onto the conflicting head, and its implementer's completion of implementing at that head
	// without the merge.
	completes := func(t *testing.T, steps ...string) *pgxpool.Pool {
		t.Helper()
		pool := plannedConflictIssue(t, phase.Implementing, settledGreen("lint"), steps...)
		confirmsConflict(t, pool, requiresCI)
		askedOnce(t, pool, phase.Implementing, "in_progress", record.ConflictCheckNotGreen, "")
		completesImplementingAt(t, pool)
		return pool
	}

	t.Run("control: the plan implementer tester", func(t *testing.T) {
		pool := completes(t, "implementer", "tester")
		if got := issuePhase(t, pool); got != phase.Testing {
			t.Fatalf("after the implementer's completion the issue is in %s, want testing", got)
		}
		want := []record.MergeForwardMessage{inImplementing,
			ask(claim.RoleImplementer, record.MergeForwardAsk, phase.Testing, record.ConflictCheckNotGreen),
			ask(claim.RoleTester, record.MergeForwardHold, phase.Testing, record.ConflictCheckNotGreen)}
		if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
			t.Fatalf("after the move into testing the merge-forward messages are %+v, want %+v", got, want)
		}
		if got := capNotices(t, pool, "checks-red"); got != 1 {
			t.Fatalf("after the move into testing the architect has %d checks-red notices, want 1", got)
		}
	})

	for _, tc := range []struct {
		name  string
		steps []string
		next  phase.Phase
		// signsOff is whether the plan holds the owner's sign-off, which takes the issue from
		// accepting into retro.
		signsOff bool
	}{
		{name: "the plan implementer", steps: []string{"implementer"}, next: phase.Retro},
		{name: "the plan implementer acceptance:owner", steps: []string{"implementer", "acceptance:owner"}, next: phase.Accepting, signsOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := completes(t, tc.steps...)
			if got := issuePhase(t, pool); got != tc.next {
				t.Fatalf("after the implementer's completion the issue is in %s, want %s", got, tc.next)
			}
			want := []record.MergeForwardMessage{inImplementing, withdrawnIn(tc.next, record.ConflictCheckNotGreen)}
			if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
				t.Fatalf("after the move into %s the merge-forward messages are %+v, want %+v", tc.next, got, want)
			}
			if got := capNotices(t, pool, "checks-red"); got != 0 {
				t.Fatalf("after the move into %s the architect has %d checks-red notices, want 0", tc.next, got)
			}
			if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Role != claim.RoleImplementer || mf.Phase != phase.Implementing || !mf.Withdrawn {
				t.Fatalf("the record's merge forward = %+v, want the implementer's ask of implementing, withdrawn", mf)
			}

			if tc.signsOff {
				engine := readsAfter(3 * time.Minute)
				applyRefusingNothing(t, pool, engine, rootsSignOff(t, pool, engine, "LEGION-208"))
				if got := issuePhase(t, pool); got != phase.Retro {
					t.Fatalf("after the owner's sign-off the issue is in %s, want retro", got)
				}
			}
			applyRefusingNothing(t, pool, readsAfter(4*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
				Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: conflictHead})
			if got := issuePhase(t, pool); got != phase.Merging {
				t.Fatalf("after retro's completion the issue is in %s, want merging", got)
			}
			want = append(want, mergerAsked())
			if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
				t.Fatalf("after the move into merging the merge-forward messages are %+v, want %+v: the merger is asked, with no second withdrawal", got, want)
			}
		})
	}
}
