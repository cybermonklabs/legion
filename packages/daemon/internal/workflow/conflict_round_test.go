package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The heads a conflict round runs between: the head the daemon judged conflicting with its base,
// the head the implementer's forward merge leaves, which answers it, and the head a second
// forward merge leaves, after the issue has reached the cap. They are the package's own heads,
// spelled as whole commit SHAs, because a review-App review decides a round only when the head
// its Legion footer names is one (intake.PullRequestReview.LegionHead).
const (
	conflictedHead = headOne
	mergedHead     = headTwo
	remergedHead   = headThree
)

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

// standsIn puts the issue back in the phase the next fact finds it in, as the work coming round
// again would: the moves below are each made from a phase the one before it left, and nothing of
// the phases in between is what these tests are about.
func standsIn(t *testing.T, pool *pgxpool.Pool, at phase.Phase, status string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: at, Generation: 1, Status: status, Rank: "U"})
}

// marksConflictingFrom is the daemon's CONFLICTING read of head while the issue stands in a phase
// after the implementer's: the one move the daemon makes on its own (TriggerConflict), which
// sends the work back for the forward merge only the implementer can make.
func marksConflictingFrom(t *testing.T, pool *pgxpool.Pool, engine *Engine, at phase.Phase, status, head string) {
	t.Helper()
	standsIn(t, pool, at, status)
	applyRefusingNothing(t, pool, engine, intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42,
		Base: "main", HeadSHA: head, Mergeable: record.MergeabilityConflicting})
}

// mergesForward is the implementer's answer to a conflicting head: the forward merge that moves
// the pull request's own recorded head off the head the record holds conflicting, which is what
// ends the exemption. The move names the head it came from, so the arrival chains onto the
// recorded one and the head moves on this fact alone (classify.AdvancePullRequestHead).
func mergesForward(t *testing.T, pool *pgxpool.Pool, engine *Engine, from, to string, at time.Time) {
	t.Helper()
	applyRefusingNothing(t, pool, engine, intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", Before: from, Sender: "implementer", HeadSHA: to, UpdatedAt: at})
}

// recordedHead is the pull request's own head as the record holds it.
func recordedHead(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var head string
	if err := pool.QueryRow(context.Background(), "select head_sha from pull_requests where issue = 'LEGION-208'").Scan(&head); err != nil {
		t.Fatalf("read the recorded head: %v", err)
	}
	return head
}

// A backward move made while the daemon's record holds this head as conflicting with its base
// costs review_round_cap nothing, for none of the ways it is made (dispatch://INFRA-1442). The
// cap bounds a loop of rejected work; a head that stopped merging into its base is the daemon's
// own news, answered by one forward merge, and a tree reached the cap on three rounds of which
// two were conflicts and none was a rejection (dispatch://INFRA-1265).
//
// Three moves are made at the conflicting head here, one for each way this release makes one: the
// move the daemon makes on its own when its read of GitHub judges the head conflicting
// (TriggerConflict), a tester's fail at that same head, and the review App's request for changes
// at it. Each raises the implementer's pass, because a completion's key is built from that number
// (record.PhaseRow.Pass, api/handoff.go), and the review's is the only one that opens a review
// round at all - the cap counts rounds, and only a review that requested changes opens one
// (recordRound). That round is a conflict round, so with review_round_cap=3 and two counted
// rounds already behind the issue the three moves write no cap message and no pr-blocked notice.
//
// Then the implementer's forward merge moves the recorded head, which is what answers a conflict,
// and the review App's request for changes at the new head is the first counted round since: the
// mark no longer equals the recorded head, so nothing excuses it, it is the third counted round,
// and the cap's message names both numbers.
//
// The issue then stands at the cap, where a conflict round still costs nothing: it posts no
// second message and no second pr-blocked notice, since a notice is how a round counts - the
// architect is woken to decide it - and the counted number the round leaves is the one the
// message already on the issue names. The counted round after it posts nothing either, though it
// is counted: the cap's message and its notice go once for each value review_round_cap holds
// (recordRound, record.PhaseRow.CapTold), and this row told the value 3 when it reached it.
func TestConflictRoundsCostTheCapNothing(t *testing.T) {
	pool := migratedPool(t)
	engine := workBackEngine(3)
	seedWorkBackIssue(t, pool, conflictedHead)
	// Two rounds a reviewer asked for stand behind the issue already, so the cap is one counted
	// round away: anything the three moves at the conflicting head counted would reach it.
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Rounds: 2})

	// The daemon's own move: its read of GitHub judges the head conflicting, and the work goes
	// back from the phase it stood in for the implementer to merge the base forward.
	marksConflictingFrom(t, pool, engine, phase.Reviewing, "needs_review", conflictedHead)
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("the conflicting read left the issue in %s, want implementing: a head GitHub will not merge is the implementer's to answer", got)
	}
	if got := conflictingHead(t, pool); got != conflictedHead {
		t.Fatalf("the record holds %q conflicting, want %q: the mark is what every move below is read against", got, conflictedHead)
	}

	// A tester's fail at that same head, and the review App's request for changes at it. The
	// first opens no review round, the second opens one and it is a conflict round.
	testerFails(t, pool, engine, "1")
	reviewerRequestsChanges(t, pool, engine, "1", conflictedHead)

	if pass := implementerPass(t, pool); pass != 3 {
		t.Fatalf("the three moves at the conflicting head counted %d passes, want 3: every move back raises the pass, a conflict round among them", pass)
	}
	if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != 3 || conflicts != 1 {
		t.Fatalf("rounds = %d of which %d conflict rounds, want the seeded two plus the review's own round, that one excused: the daemon's move and the tester's fail open no review round at all", rounds, conflicts)
	}
	if messages, blocked := capPosts(t, pool); len(messages) != 0 || blocked != 0 {
		t.Fatalf("the moves at the conflicting head posted %q and %d pr-blocked notices, want neither: two rounds are counted, and the cap is 3", messages, blocked)
	}

	// The implementer answers the conflict with one forward merge, which moves the recorded head.
	// Nothing writes the mark here: it no longer equals the head, and that is what ends the
	// exemption.
	mergesForward(t, pool, engine, conflictedHead, mergedHead, time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC))
	if got := recordedHead(t, pool); got != mergedHead {
		t.Fatalf("after the forward merge the record holds head %q, want %q", got, mergedHead)
	}
	if got := conflictingHead(t, pool); got != conflictedHead {
		t.Fatalf("after the forward merge the record holds %q conflicting, want the old head %q kept: the head moving off it is what ends the exemption", got, conflictedHead)
	}

	reviewerRequestsChanges(t, pool, engine, "2", mergedHead)
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("the review App's request for changes left the issue in %s, want implementing", got)
	}
	if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != 4 || conflicts != 1 {
		t.Fatalf("rounds = %d of which %d conflict rounds, want 4 and the same 1: the round at the merged head is counted", rounds, conflicts)
	}
	want := "Issue reached review_round_cap=3: 3 rounds counted, 1 conflict rounds not counted."
	messages, blocked := capPosts(t, pool)
	if len(messages) != 1 || messages[0] != want || blocked != 1 {
		t.Fatalf("the cap posted %q and %d pr-blocked notices, want one message %q and one notice", messages, blocked, want)
	}

	// The issue now stands at the cap. A conflict round made there costs nothing either: it posts
	// nothing, because the counted number it leaves is the one the message above already names,
	// and a second pr-blocked notice would wake the architect to decide a round nobody spent.
	marksConflicting(t, pool, engine, mergedHead)
	reviewerRequestsChanges(t, pool, engine, "3", mergedHead)
	if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != 5 || conflicts != 2 {
		t.Fatalf("rounds = %d of which %d conflict rounds, want 5 and 2: a conflict round made at the cap is excused like the one before it", rounds, conflicts)
	}
	if messages, blocked := capPosts(t, pool); len(messages) != 1 || blocked != 1 {
		t.Fatalf("the conflict round made at the cap posted %q and %d pr-blocked notices, want only the one message and the one notice the counted round before it wrote", messages, blocked)
	}

	// The next counted round is counted and tells the architect nothing: the cap's message and
	// its pr-blocked notice go once for each value review_round_cap holds, and this row told the
	// value 3 on the round that reached it. The numbers below are what reads the round as counted.
	mergesForward(t, pool, engine, mergedHead, remergedHead, time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC))
	reviewerRequestsChanges(t, pool, engine, "4", remergedHead)
	if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != 6 || conflicts != 2 {
		t.Fatalf("rounds = %d of which %d conflict rounds, want 6 and the same 2: the round at the remerged head is counted", rounds, conflicts)
	}
	messages, blocked = capPosts(t, pool)
	if len(messages) != 1 || messages[0] != want || blocked != 1 {
		t.Fatalf("the counted round after the conflict round posted %q and %d pr-blocked notices, want still the one message %q and the one notice the round that reached the cap wrote: the cap is told once for each value it holds", messages, blocked, want)
	}
	if pass := implementerPass(t, pool); pass != 6 {
		t.Fatalf("the six moves back counted %d passes, want 6: the pass rises on every one of them, counted or excused", pass)
	}
}

// What ends the exemption, and what does not. The mark is the head the daemon last judged
// conflicting, so a read that judges that same head mergeable clears it and the next round
// counts; a change of the pull request's own recorded head clears it without a write, since the
// mark is read against that head; and a read of UNKNOWN between the judgement and the move clears
// nothing, which is why the fact cannot be read off pull_requests.mergeability - UNKNOWN is what
// GitHub answers while it computes one, and a read on the ladder behind it writes that answer over
// the conflict.
//
// Every row makes the mark from implementing, the one phase with no row for the conflict trigger,
// so the read records its judgement and moves nothing (workflow's mergeability): what the rounds
// below are read against is the mark the record holds, not the move some other phase would have
// made. The round itself is the review App's request for changes, because that is what opens a
// review round at all in this release (recordRound).
func TestAConflictRoundCountsAgainstNoCapUntilTheMarkIsCleared(t *testing.T) {
	for _, tc := range []struct {
		name string
		// clearing is what happens between the mark and the round; no fact at all for the row
		// that pins a round at the conflicting head itself as excused.
		clearing intake.Fact
		// head is the pull request's recorded head when the review requests changes, and excused
		// whether the round it opens is a conflict round.
		head    string
		excused bool
	}{
		{
			name:     "a read that judges the same head mergeable",
			clearing: intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: conflictedHead, Mergeable: record.MergeabilityMergeable},
			head:     conflictedHead,
		},
		{
			name: "a change of the pull request's own recorded head",
			clearing: intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208",
				Before: conflictedHead, Sender: "implementer", HeadSHA: mergedHead, UpdatedAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)},
			head: mergedHead,
		},
		{
			name:     "a read of UNKNOWN between the judgement and the round",
			clearing: intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: conflictedHead, Mergeable: record.MergeabilityUnknown},
			head:     conflictedHead,
			excused:  true,
		},
		{
			name:    "nothing: the round is opened at the conflicting head itself",
			head:    conflictedHead,
			excused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := workBackEngine(3)
			seedWorkBackIssue(t, pool, conflictedHead)

			marksConflicting(t, pool, engine, conflictedHead)
			if got := conflictingHead(t, pool); got != conflictedHead {
				t.Fatalf("the record holds %q conflicting, want %q", got, conflictedHead)
			}
			if rounds := implementerRounds(t, pool); rounds != 0 {
				t.Fatalf("the mark made in implementing counted %d rounds, want none: the phase the work is already in has no row for the conflict trigger, so the read moves nothing", rounds)
			}
			if tc.clearing != nil {
				applyRefusingNothing(t, pool, engine, tc.clearing)
			}
			if got := recordedHead(t, pool); got != tc.head {
				t.Fatalf("the recorded head is %q, want %q", got, tc.head)
			}

			reviewerRequestsChanges(t, pool, engine, "1", tc.head)
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("the review App's request for changes left the issue in %s, want implementing", got)
			}
			wantConflicts := 0
			if tc.excused {
				wantConflicts = 1
			}
			if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != 1 || conflicts != wantConflicts {
				t.Fatalf("after the review App's request for changes rounds = %d of which %d conflict rounds, want 1 and %d", rounds, conflicts, wantConflicts)
			}
		})
	}
}
