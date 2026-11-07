package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The two heads a conflict round runs between: the head the daemon judged conflicting with its
// base, and the head the implementer's forward merge leaves, which answers it.
const (
	conflictedHead = "conflicted-head"
	mergedHead     = "merged-head"
)

// conflictRoundEngine drives the merge queue's own returns (MergeQueueRole) under a
// review_round_cap of 3, the default a deployment ships with: readyEngine caps nothing and
// testEngine serves no merge queue, and a conflict round is read against the cap.
func conflictRoundEngine() *Engine {
	return New(record.NewStore(), Config{
		Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), MergeQueueRole: "merge-queue",
		ReviewRoundCap: 3, MaxFixAttempts: 3, Linger: time.Hour, ProductionCheckRole: claim.RoleTester,
		ReviewAppLogin: testReviewApp, Repositories: testRepositories,
		Clock: func() time.Time { return time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC) },
	}, nil)
}

// implementerConflictRounds is how many of the implementer's rounds the record holds as caused by
// a conflict with the base, which review_round_cap does not count.
func implementerConflictRounds(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var rounds int
	if err := pool.QueryRow(context.Background(), "select conflict_rounds from phases where issue = 'LEGION-208' and role = 'implementer'").Scan(&rounds); err != nil {
		t.Fatalf("read the implementer's conflict rounds: %v", err)
	}
	return rounds
}

// conflictingHead is the head the record holds as conflicting with its base.
func conflictingHead(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var head string
	if err := pool.QueryRow(context.Background(), "select conflicting_head from pull_requests where issue = 'LEGION-208'").Scan(&head); err != nil {
		t.Fatalf("read the head the record holds conflicting: %v", err)
	}
	return head
}

// capPosts is what review_round_cap has told so far: every Dispatch message naming the cap, and
// how many pr-blocked notices the architect was sent.
func capPosts(t *testing.T, pool *pgxpool.Pool) (messages []string, blocked int) {
	t.Helper()
	for _, body := range messageBodies(t, pool) {
		if strings.HasPrefix(body, "Issue reached review_round_cap=") {
			messages = append(messages, body)
		}
	}
	if err := pool.QueryRow(context.Background(),
		"select count(*) from outbox where kind = 'notice' and payload->>'kind' = 'pr-blocked'").Scan(&blocked); err != nil {
		t.Fatalf("count the architect's pr-blocked notices: %v", err)
	}
	return messages, blocked
}

// standsIn puts the issue back in the phase the next fact finds it in, as the work coming round
// again would: the moves below are each made from a phase the one before it left, and nothing of
// the phases in between is what this test is about.
func standsIn(t *testing.T, pool *pgxpool.Pool, at phase.Phase, status string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: at, Generation: 1, Status: status, Rank: "U"})
}

// A backward move made while the daemon's record holds this head as conflicting with its base
// counts against review_round_cap for none of the ways it is made (dispatch://INFRA-1442). The
// cap bounds a loop of rejected work; a head that stopped merging into its base is the daemon's
// own news, answered by one forward merge, and a tree reached the cap on three rounds of which
// two were conflicts and none was a rejection (dispatch://INFRA-1265).
//
// Here four moves back are made at the conflicting head, one for each way the daemon makes one:
// the return the merge queue's own state makes (MergeQueueConflict), the queue's conflict removal
// (MergeFailed), the conflict the two-minute required-checks pass finds once the entry is out of
// the queue (PullRequestMergeability), and a tester's fail at that same head. Each raises the
// implementer's rounds, because a completion's key is built from that number (api/handoff.go),
// and none raises the number the cap counts, so with review_round_cap=3 and two counted rounds
// already behind the issue the four write no cap message and no pr-blocked notice.
//
// Then the implementer's forward merge moves the recorded head, which is what answers a conflict,
// and the reviewer's request for changes at the new head is the first counted round since: the
// mark no longer equals the recorded head, so nothing excuses it, it is the third counted round,
// and the cap's message names both numbers.
func TestAConflictRoundCountsAgainstNoCap(t *testing.T) {
	pool := migratedPool(t)
	engine := conflictRoundEngine()
	apply := applyFacts(t, pool, engine)
	standsIn(t, pool, phase.AwaitingMerge, "retro")
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: conflictedHead})
	// Two rounds a reviewer asked for stand behind the issue already, so the cap is one counted
	// round away: anything the four conflict rounds below counted would reach it.
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Rounds: 2})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim",
		HandoffCommit: conflictedHead, Summary: "READY #42 at " + conflictedHead + " for LEGION-208"})

	for _, move := range []struct {
		name string
		// at and status are where the move is made from, and from, where it leaves the issue.
		at, from phase.Phase
		status   string
		id       string
		fact     intake.Fact
	}{
		{
			name: "the return the merge queue's own state makes", at: phase.AwaitingMerge, status: "retro", from: phase.Implementing,
			id: "merge-queue-conflict:LEGION-208:" + conflictedHead,
			fact: intake.MergeQueueConflict{Issue: "LEGION-208", Number: 42, Head: conflictedHead,
				Reason: "the head conflicts with main: GitHub's merge queue reports its entry unmergeable while the entry is built on main's own tip base-tip; merge main forward"},
		},
		{
			name: "the queue's conflict removal", at: phase.AwaitingMerge, status: "retro", from: phase.Implementing,
			id: "merge-failed:LEGION-208:" + conflictedHead,
			fact: intake.MergeFailed{Issue: "LEGION-208", Number: 42, Head: conflictedHead,
				Reason: "the merge queue took pull request #42 out at READY's head " + conflictedHead + " (merge_conflict)"},
		},
		{
			name: "the conflict the two-minute pass finds once the entry is out of the queue", at: phase.AwaitingMerge, status: "retro", from: phase.Implementing,
			id:   "mergeability:LEGION-208:conflicting",
			fact: intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", Mergeable: record.MergeabilityConflicting},
		},
		{
			name: "a tester round failed at that head", at: phase.Testing, status: "testing", from: phase.Implementing,
			id: "tester-failed:LEGION-208:" + conflictedHead,
			fact: intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim",
				Summary: "the base's change is not in the head", Verdict: "fail", HandoffRecord: "test-at-" + conflictedHead, HandoffVersion: 1},
		},
	} {
		standsIn(t, pool, move.at, move.status)
		if move.at == phase.Testing {
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
		}
		if result := apply(move.id, move.fact); result.Refusal != nil {
			t.Fatalf("%s = %+v", move.name, result.Refusal)
		}
		if got := issuePhase(t, pool); got != move.from {
			t.Fatalf("%s left the issue in %s, want %s", move.name, got, move.from)
		}
		if got := conflictingHead(t, pool); got != conflictedHead {
			t.Fatalf("after %s the record holds %q conflicting, want %q", move.name, got, conflictedHead)
		}
	}
	if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != 6 || conflicts != 4 {
		t.Fatalf("rounds = %d of which %d conflict rounds, want the seeded two plus the four conflict rounds, all four excused", rounds, conflicts)
	}
	if messages, blocked := capPosts(t, pool); len(messages) != 0 || blocked != 0 {
		t.Fatalf("the four conflict rounds posted %q and %d pr-blocked notices, want neither: two rounds are counted, and the cap is 3", messages, blocked)
	}

	// The implementer answers the conflict with one forward merge, which moves the recorded head.
	// Nothing writes the mark here: it no longer equals the head, and that is what ends the
	// exemption.
	if result := apply("forward-merge:LEGION-208:"+mergedHead, intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", Before: conflictedHead, Sender: "implementer", HeadSHA: mergedHead,
		UpdatedAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)}); result.Refusal != nil {
		t.Fatalf("the implementer's forward merge = %+v", result.Refusal)
	}
	if got := conflictingHead(t, pool); got != conflictedHead {
		t.Fatalf("after the forward merge the record holds %q conflicting, want the old head %q kept: the head moving off it is what ends the exemption", got, conflictedHead)
	}

	standsIn(t, pool, phase.Reviewing, "needs_review")
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", HandoffCommit: "review-1"})
	if result := apply("changes-requested:LEGION-208:"+mergedHead, intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42,
		State: "changes_requested", CommitID: mergedHead, HeadSHA: mergedHead, Author: testReviewApp}); result.Refusal != nil {
		t.Fatalf("the reviewer's request for changes = %+v", result.Refusal)
	}
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("the reviewer's request for changes left the issue in %s, want implementing", got)
	}
	if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != 7 || conflicts != 4 {
		t.Fatalf("rounds = %d of which %d conflict rounds, want 7 and the same 4: the reviewer's round is counted", rounds, conflicts)
	}
	want := "Issue reached review_round_cap=3: 3 rounds counted, 4 conflict rounds not counted."
	messages, blocked := capPosts(t, pool)
	if len(messages) != 1 || messages[0] != want || blocked != 1 {
		t.Fatalf("the cap posted %q and %d pr-blocked notices, want one message %q and one notice", messages, blocked, want)
	}
}

// What ends the exemption, and what does not. The mark is the head the daemon last judged
// conflicting, so a read that judges that same head mergeable clears it and the next move back
// counts; a change of the pull request's own recorded head clears it without a write, since the
// mark is read against that head; and a read of UNKNOWN between the judgement and the move clears
// nothing, which is why the fact cannot be read off pull_requests.mergeability — UNKNOWN is what
// GitHub answers for a head sitting in the merge queue, and the two-minute pass writes that answer
// over the conflict within two minutes of it.
//
// Every row makes its move back from testing, where no READY head exists at all: the fact the
// exemption reads is the pull request's own recorded head, not the head some READY awaits, so a
// tester's round at a conflicting head is excused like any other.
func TestAConflictRoundCountsAgainstNoCapUntilTheMarkIsCleared(t *testing.T) {
	for _, tc := range []struct {
		name string
		// clearing is what happens between the conflicting read and the move back; no fact at all
		// for the row that pins a tester's round at the conflicting head as excused.
		clearing intake.Fact
		// head is the pull request's recorded head when the tester's round is failed, and excused
		// whether the round is a conflict round.
		head    string
		excused bool
	}{
		{
			name:     "a read that judges the same head mergeable",
			clearing: intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", Mergeable: record.MergeabilityMergeable},
			head:     conflictedHead,
		},
		{
			name: "a change of the pull request's own recorded head",
			clearing: intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208",
				Before: conflictedHead, Sender: "implementer", HeadSHA: mergedHead, UpdatedAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)},
			head: mergedHead,
		},
		{
			name:     "a read of UNKNOWN between the judgement and the move",
			clearing: intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", Mergeable: record.MergeabilityUnknown},
			head:     conflictedHead,
			excused:  true,
		},
		{
			name:    "nothing: a tester round in testing, where no READY head exists",
			head:    conflictedHead,
			excused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			apply := applyFacts(t, pool, conflictRoundEngine())
			standsIn(t, pool, phase.AwaitingMerge, "retro")
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: conflictedHead})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim",
				HandoffCommit: conflictedHead, Summary: "READY #42 at " + conflictedHead + " for LEGION-208"})
			if result := apply("mergeability:LEGION-208:conflicting",
				intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", Mergeable: record.MergeabilityConflicting}); result.Refusal != nil {
				t.Fatalf("the conflicting read = %+v", result.Refusal)
			}
			if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != 1 || conflicts != 1 {
				t.Fatalf("after the conflicting read rounds = %d of which %d conflict rounds, want one of each", rounds, conflicts)
			}
			if tc.clearing != nil {
				if result := apply("clearing:LEGION-208", tc.clearing); result.Refusal != nil {
					t.Fatalf("%s = %+v", tc.name, result.Refusal)
				}
			}
			var head string
			if err := pool.QueryRow(context.Background(), "select head_sha from pull_requests where issue = 'LEGION-208'").Scan(&head); err != nil {
				t.Fatalf("read the recorded head: %v", err)
			}
			if head != tc.head {
				t.Fatalf("the recorded head is %q, want %q", head, tc.head)
			}

			standsIn(t, pool, phase.Testing, "testing")
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
			if result := apply("tester-failed:LEGION-208", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
				Role: claim.RoleTester, Claim: "test-claim", Summary: "the fix is not in", Verdict: "fail",
				HandoffRecord: "test-handoff", HandoffVersion: 1}); result.Refusal != nil {
				t.Fatalf("the tester's fail = %+v", result.Refusal)
			}
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("the tester's fail left the issue in %s, want implementing", got)
			}
			wantConflicts := 1
			if tc.excused {
				wantConflicts = 2
			}
			if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != 2 || conflicts != wantConflicts {
				t.Fatalf("after the tester's fail rounds = %d of which %d conflict rounds, want 2 and %d", rounds, conflicts, wantConflicts)
			}
		})
	}
}
