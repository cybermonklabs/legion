package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// githubFake is GitHub as the real engine reaches it for LEGION-208's pull request #42 through the
// one call the daemon makes on the commit of a fact for it here: a read of the head's merge state
// (Config.ReadMergeState). The engine asks for it on the commit, and the fake answers once the
// fact's transaction is over, as the daemon's reader runs after it (serve), each answer applied as
// the daemon's read applies GitHub's answer (intake.PullRequestMergeability).
type githubFake struct {
	t      *testing.T
	pool   *pgxpool.Pool
	engine *Engine
	// mergeable, state, requested and checks are GitHub's answer for the head the branch stands
	// at: requested is how many review requests it holds open on the pull request, and checks is
	// the head's rollup, whether a check or a status of it still runs.
	mergeable record.Mergeability
	state     record.MergeState
	requested int
	checks    record.HeadChecks
	// reads is how many reads the engine has asked for that GitHub has not answered.
	reads int
	// answered is each state GitHub answered a read with, in order, and at the moment of the last
	// answer: each read is step after the one before (a minute unless a test sets it), so the
	// record takes every one of them (StateReadAt), and the engine's clock stands at that moment
	// as the fact is applied, as the daemon's does at a read it has just made.
	answered []record.MergeState
	at       time.Time
	step     time.Duration
}

// newGitHubFake is the fake GitHub of pool's LEGION-208, wired into engine as its reader of the
// merge state. GitHub answers MERGEABLE and state, no review requested and nothing of the head
// running, until a test says otherwise.
func newGitHubFake(t *testing.T, pool *pgxpool.Pool, engine *Engine, state record.MergeState) *githubFake {
	g := &githubFake{t: t, pool: pool, engine: engine, mergeable: record.MergeabilityMergeable, state: state,
		checks: record.HeadChecksConcluded, at: time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC), step: conflictConfirmWait}
	engine.cfg.ReadMergeState = func(string) { g.reads++ }
	return g
}

// apply applies fact through the engine, fails the test on a refusal, and then serves the reads
// the fact's commit asked GitHub for.
func (g *githubFake) apply(id string, fact intake.Fact) {
	g.t.Helper()
	if result := applyFacts(g.t, g.pool, g.engine)(id, fact); result.Refusal != nil {
		g.t.Fatalf("%s was refused: %+v", id, result.Refusal)
	}
	g.serve()
}

// serve answers each read asked for, until nothing the engine asked is left: a read that GitHub
// answers is a fact of its own, which can ask again.
func (g *githubFake) serve() {
	g.t.Helper()
	for g.reads > 0 {
		g.reads--
		g.read()
	}
}

// read is one read of #42 answered by GitHub, step after the read before it, about the head the
// branch stands at, applied as the daemon applies its read at the moment it was made.
func (g *githubFake) read() {
	g.t.Helper()
	head := recordedPR(g.t, g.pool, "LEGION-208").HeadSHA
	g.at = g.at.Add(g.step)
	clockReads(g.engine, g.at)
	g.answered = append(g.answered, g.state)
	if result := applyFacts(g.t, g.pool, g.engine)("read-"+g.at.Format(time.RFC3339), intake.PullRequestMergeability{
		Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: head, Mergeable: g.mergeable, MergeState: g.state,
		ReviewsRequested: new(g.requested), HeadChecks: g.checks, ReadAt: g.at}); result.Refusal != nil {
		g.t.Fatalf("GitHub's answer %s for %s was refused: %+v", g.state, head, result.Refusal)
	}
}

// waitsOnGitHub is whether LEGION-208's round waits on GitHub's answer for its head as the record
// holds it now: the rule the state build reads for the issue's mergeWait
// (classify.RoundWaitsOnMergeState, projection's mergeWaitView), so it is what `legion state`
// shows.
func waitsOnGitHub(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	pr := recordedPR(t, pool, "LEGION-208")
	return classify.RoundWaitsOnMergeState(*issueRecord(t, pool, "LEGION-208"), reviewerRow(t, pool), &pr)
}

// standsUntold fails the test unless LEGION-208 stands in want and no review-stuck notice has gone
// out.
func standsUntold(t *testing.T, pool *pgxpool.Pool, want phase.Phase, why string) {
	t.Helper()
	if got := issuePhase(t, pool); got != want {
		t.Fatalf("LEGION-208 is in %s, want %s: %s", got, want, why)
	}
	if told := reviewStuckNotices(t, pool); len(told) != 0 {
		t.Fatalf("the architect was told the round was stuck %+v, want nothing: %s", told, why)
	}
}

// waits fails the test unless LEGION-208's round waits in reviewing on GitHub, untold, and the
// issue's view would carry the wait.
func waits(t *testing.T, pool *pgxpool.Pool, why string) {
	t.Helper()
	standsUntold(t, pool, phase.Reviewing, why)
	if !waitsOnGitHub(t, pool) {
		t.Fatalf("the round waits on no answer of GitHub's, want the wait the state shows: %s", why)
	}
}

// GitHub's merge state is an answer to the reader, not GitHub's verdict on the merge, and `blocked`
// is the one state the daemon cannot read past: it covers a rule about the reader (a token without
// the workflows permission reads `blocked` for every pull request that changes
// .github/workflows/, dispatch://INFRA-4192/comment/1c89885e-5e89-42f7-8122-9cdc697d0ccd), a check
// suite of another pull request on the same commit that holds a required check `expected`, and
// rules the daemon reads nothing of. The merger's enqueue is the call GitHub answers about the
// merge itself, accepted or refused with its reason. So an approval that stands on the head ends
// the round on `blocked` as classify.MergeOutcomeOf says - the head MERGEABLE, the read's rollup
// saying nothing of it runs, every check run GitHub reported on that head concluded, whatever each
// concluded to, and no review request open - and the merger answers the state at merging. The
// round waits quietly while GitHub has given no whole answer, while a check of the head runs, and
// while GitHub has not computed whether the head merges; it waits and is told once while GitHub
// holds a review request open; and `behind` and `draft` keep the stuck round and its notice, told
// once.
func TestAnApprovalOfABlockedHeadWhoseChecksConcludedMovesToAccepting(t *testing.T) {
	t.Parallel()
	// rig is LEGION-208 in reviewing, its pull request #42 at headOne with CI settled there and no
	// merge state read (seedMergeStateRound), applied through the real engine and the fake GitHub.
	rig := func(t *testing.T, state record.MergeState) (*pgxpool.Pool, *githubFake) {
		t.Helper()
		pool := migratedPool(t)
		seedMergeStateRound(t, pool, record.MergeStateUnread)
		return pool, newGitHubFake(t, pool, testEngine(config.DesignGateRootIssues, nil), state)
	}
	approve := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: headOne,
		HeadSHA: headOne, Author: testReviewApp, Body: reviewBody("ship it", headOne)}

	t.Run("GitHub answers blocked once every check run on the head concluded", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateBlocked)
		github.apply("approve", approve)
		if len(github.answered) != 1 || github.answered[0] != record.MergeStateBlocked {
			t.Fatalf("GitHub answered %v, want the one read the approval asked for, answered blocked", github.answered)
		}
		standsUntold(t, pool, phase.Accepting, "the approval stands on a blocked, mergeable head whose checks concluded")
	})

	// A head approved at the head, MERGEABLE, with no review request, whose every check run
	// concluded, a cancelled run and a failed one of another pull request's check suite on the
	// same commit among them: GitHub answers `blocked` to every writer and refuses the enqueue with
	// `Required status check "gate" is expected`, though the head's own required check passed.
	// Its round ends, and the merger meets that refusal.
	t.Run("a blocked head whose runs concluded, cancelled and failed among them, with no review asked for", func(t *testing.T) {
		pool := migratedPool(t)
		seedMergeStateRound(t, pool, record.MergeStateUnread)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: headOne, Failing: []string{"required"}, Cancelled: []string{"tests"},
			CheckedHead: headOne, CheckRuns: []record.AttemptRun{
				{Name: "gate", ID: 1, Head: headOne, Conclusion: record.RunSucceeded},
				{Name: "required", ID: 2, Head: headOne, Conclusion: record.RunFailed},
				{Name: "tests", ID: 3, Head: headOne, Conclusion: record.RunCancelled},
				{Name: "workflow-size", ID: 4, Head: headOne, Conclusion: record.RunSucceeded}}})
		if _, err := pool.Exec(t.Context(), "update pull_requests set mergeability = '', merge_state = '' where issue = 'LEGION-208'"); err != nil {
			t.Fatalf("clear GitHub's answer on the seeded pull request: %v", err)
		}
		github := newGitHubFake(t, pool, testEngine(config.DesignGateRootIssues, nil), record.MergeStateBlocked)
		github.apply("approve", approve)
		if len(github.answered) != 1 || github.answered[0] != record.MergeStateBlocked {
			t.Fatalf("GitHub answered %v, want the one read the approval asked for, answered blocked", github.answered)
		}
		standsUntold(t, pool, phase.Accepting, "every check run of the head concluded, a failed and a cancelled one among them, and no review is asked for")
		if waitsOnGitHub(t, pool) {
			t.Fatalf("the round still waits on GitHub after it ended, want no wait on the issue's view")
		}
	})

	t.Run("GitHub holds a review request open on a blocked head whose runs concluded", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateBlocked)
		github.requested = 1
		github.apply("approve", approve)
		if got := issuePhase(t, pool); got != phase.Reviewing {
			t.Fatalf("LEGION-208 is in %s, want reviewing: a review GitHub asked for is not given yet", got)
		}
		if !waitsOnGitHub(t, pool) {
			t.Fatalf("the round waits on no answer of GitHub's, want the wait the state shows while a review is asked for")
		}
		told := reviewStuckNotices(t, pool)
		if len(told) != 1 || !strings.Contains(told[0].Reason, "GitHub reports that head blocked rather than one it would merge") ||
			!strings.Contains(told[0].Reason, "GitHub holds 1 review request(s) open on the pull request") {
			t.Fatalf("review-stuck notices %+v, want one naming blocked and the open review request", told)
		}
		if stuck := stuckRoleMessages(t, pool); len(stuck) != 1 {
			t.Fatalf("the reviewer was told %d times %+v, want once", len(stuck), stuck)
		}
		// A second read with the request still open tells nobody again.
		github.read()
		if again := reviewStuckNotices(t, pool); len(again) != 1 || issuePhase(t, pool) != phase.Reviewing {
			t.Fatalf("after a second read the notices are %+v in %s, want the one notice and reviewing", again, issuePhase(t, pool))
		}
		// The requested review is given and GitHub holds no request open: the next read ends the
		// round on the same `blocked`.
		github.requested = 0
		github.read()
		if got := issuePhase(t, pool); got != phase.Accepting {
			t.Fatalf("LEGION-208 is in %s, want accepting: no review is asked for any more and every check run concluded", got)
		}
		if again := reviewStuckNotices(t, pool); len(again) != 1 {
			t.Fatalf("the architect was told %d times (%+v), want the one notice of the review request", len(again), again)
		}
	})

	t.Run("GitHub's answer unread", func(t *testing.T) {
		pool := migratedPool(t)
		seedMergeStateRound(t, pool, record.MergeStateUnread)
		engine := testEngine(config.DesignGateRootIssues, nil)
		github := newGitHubFake(t, pool, engine, record.MergeStateBlocked)
		if result := applyFacts(t, pool, engine)("approve", approve); result.Refusal != nil {
			t.Fatalf("the approval was refused: %+v", result.Refusal)
		}
		if github.reads != 1 {
			t.Fatalf("the approval asked GitHub for %d reads, want one: the round waits on GitHub's answer", github.reads)
		}
		waits(t, pool, "GitHub has answered nothing for the head")
		github.serve()
		standsUntold(t, pool, phase.Accepting, "GitHub answered blocked for a mergeable head whose checks concluded")
	})

	t.Run("GitHub answers unknown", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateUnknown)
		github.apply("approve", approve)
		waits(t, pool, "GitHub is still computing the merge state")
		github.state = record.MergeStateBlocked
		github.read()
		standsUntold(t, pool, phase.Accepting, "the read after unknown answered blocked")
	})

	// A check re-run on the same head leaves the head's settlement standing, every run of it
	// concluded, while GitHub holds the head `blocked` for the run again under way. The read's own
	// rollup says so (record.HeadChecksRunning), and the round waits on it quietly rather than
	// ending on the settlement it has outlived; the read after the re-run ends it.
	t.Run("a check re-run on the head the settlement already concluded", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateBlocked)
		github.checks = record.HeadChecksRunning
		github.apply("approve", approve)
		waits(t, pool, "GitHub's rollup says a check of the head runs again")
		github.checks = record.HeadChecksConcluded
		github.read()
		standsUntold(t, pool, phase.Accepting, "the read after the re-run found nothing of the head running")
	})

	// GitHub answers `blocked` with `mergeable` null while a background job computes whether the
	// head merges cleanly, routinely on the first read after a push. A second null read sooner than
	// classify.NullMergeableFloor after the first tells nobody; one at the floor tells the architect
	// and the reviewer once, naming the reads; and the read that finds the head MERGEABLE ends the
	// round.
	t.Run("GitHub answers blocked before it computed whether the head merges", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateBlocked)
		github.mergeable, github.checks = record.MergeabilityUnknown, record.HeadChecksUnread
		github.apply("approve", approve)
		waits(t, pool, "GitHub has not said the head merges, on a first read")
		github.read()
		waits(t, pool, "a second null read a minute after the first is below the floor")
		github.step = classify.NullMergeableFloor
		github.read()
		if got := issuePhase(t, pool); got != phase.Reviewing {
			t.Fatalf("LEGION-208 is in %s, want reviewing: GitHub still has not said the head merges", got)
		}
		told := reviewStuckNotices(t, pool)
		if len(told) != 1 || !strings.Contains(told[0].Reason, "GitHub has answered `mergeable` null for it on 3 read(s) in a row") {
			t.Fatalf("review-stuck notices %+v, want one naming three null reads in a row", told)
		}
		github.read()
		if again := reviewStuckNotices(t, pool); len(again) != 1 {
			t.Fatalf("after a fourth null read the notices are %+v, want the one", again)
		}
		github.mergeable, github.checks = record.MergeabilityMergeable, record.HeadChecksConcluded
		github.read()
		if got := issuePhase(t, pool); got != phase.Accepting {
			t.Fatalf("LEGION-208 is in %s, want accepting: the read found the head MERGEABLE", got)
		}
	})

	// After a push to the base every waiting round's climb reads GitHub every 30 seconds, and
	// GitHub answers `unknown`/null until it has computed the head again, within 15 s as measured
	// (classify.NullMergeableFloor). Null reads of the climb below the floor - at 0, 30 and 60 s -
	// are that routine recompute, not a head stuck: they tell nobody, and the read that finds the
	// head MERGEABLE ends the round. A head still null at the climb's fourth read, 90 s on, is told
	// (the classify rows at the floor).
	t.Run("a climb's null reads 30 seconds apart after a push to the base", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateUnknown)
		github.mergeable, github.checks, github.step = record.MergeabilityUnknown, record.HeadChecksUnread, 30*time.Second
		github.apply("approve", approve)
		github.read()
		github.read()
		waits(t, pool, "three null reads within a minute are GitHub's recompute")
		github.state, github.mergeable, github.checks = record.MergeStateBlocked, record.MergeabilityMergeable, record.HeadChecksConcluded
		github.read()
		standsUntold(t, pool, phase.Accepting, "the recompute answered MERGEABLE")
	})

	// The bound is time, not a count: a first null read whose next reads all fail is told once
	// classify.NullMergeableTellAfter has passed since it, by the fact the reader applies after a
	// failed read (intake.PullRequestMergeStateUnread), and not a second before.
	t.Run("a null mergeable whose next reads fail is told at the time bound", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateBlocked)
		github.mergeable, github.checks = record.MergeabilityUnknown, record.HeadChecksUnread
		github.apply("approve", approve)
		first := github.at
		unread := intake.PullRequestMergeStateUnread{Repo: "sjawhar/legion", Number: 42, HeadSHA: headOne}
		clockReads(github.engine, first.Add(classify.NullMergeableTellAfter-time.Second))
		github.apply("unread-inside", unread)
		waits(t, pool, "a failed read inside the bound tells nobody")
		clockReads(github.engine, first.Add(classify.NullMergeableTellAfter))
		github.apply("unread-at-bound", unread)
		told := reviewStuckNotices(t, pool)
		if len(told) != 1 || !strings.Contains(told[0].Reason, "GitHub has answered `mergeable` null for it on 1 read(s) in a row since") {
			t.Fatalf("review-stuck notices %+v, want one naming the one null read the bound has outlived", told)
		}
		if got := issuePhase(t, pool); got != phase.Reviewing {
			t.Fatalf("LEGION-208 is in %s, want reviewing", got)
		}
	})

	// Evidence the round must not decide on: a row an earlier release wrote holds `blocked` and
	// MERGEABLE beside no count of review requests and no rollup (migration 20261124130001); a row a
	// rollback left holds the count and rollup of an older read beside the read time an earlier
	// release wrote when it read the same head again (record.PullRequest.EvidenceReadAt). The
	// approval ends the round on neither: the round waits quietly for the read the approval asks,
	// which writes both from one read.
	for _, tc := range []struct{ name, set string }{
		{"a row recorded blocked before the count and the rollup were read",
			"reviews_requested = null, head_checks = '', evidence_read_at = '0001-01-01 00:00:00+00', state_read_at = '2026-09-22 00:00:00+00'"},
		{"a row an earlier release read again at the same head",
			"reviews_requested = 0, head_checks = 'concluded', state_read_at = '2026-09-22 00:00:00+00', evidence_read_at = '2026-09-21 23:59:00+00'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedMergeStateRound(t, pool, record.MergeStateBlocked)
			if _, err := pool.Exec(t.Context(), "update pull_requests set "+tc.set+" where issue = 'LEGION-208'"); err != nil {
				t.Fatalf("leave the row as an earlier release wrote it: %v", err)
			}
			engine := testEngine(config.DesignGateRootIssues, nil)
			github := newGitHubFake(t, pool, engine, record.MergeStateBlocked)
			if result := applyFacts(t, pool, engine)("approve", approve); result.Refusal != nil {
				t.Fatalf("the approval was refused: %+v", result.Refusal)
			}
			waits(t, pool, "no read of this release has counted the review requests or read the rollup")
			github.serve()
			standsUntold(t, pool, phase.Accepting, "the read wrote a count of none and a rollup with nothing running")
		})
	}

	// GitHub answers `mergeable` null beside `blocked` on one read and beside `unknown` on the next
	// while it computes. Every null read counts, whatever the state beside it, so the second read
	// tells: a count that the `unknown` read set back would let the alternation walk past both of
	// the round's conditions and never tell. The finding is the null `mergeable`, whatever state
	// is beside it, so the reads that go on alternating tell nothing more.
	t.Run("null reads alternating blocked and unknown", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateBlocked)
		github.mergeable, github.checks, github.step = record.MergeabilityUnknown, record.HeadChecksUnread, classify.NullMergeableFloor
		github.apply("approve", approve)
		waits(t, pool, "one null read, beside blocked")
		github.state = record.MergeStateUnknown
		github.read()
		told := reviewStuckNotices(t, pool)
		if len(told) != 1 || !strings.Contains(told[0].Reason, "GitHub has answered `mergeable` null for it on 2 read(s) in a row") {
			t.Fatalf("review-stuck notices %+v, want one naming two null reads in a row after a blocked and an unknown read", told)
		}
		github.state = record.MergeStateBlocked
		github.read()
		github.state = record.MergeStateUnknown
		github.read()
		if again := reviewStuckNotices(t, pool); len(again) != 1 {
			t.Fatalf("after more alternating null reads the notices are %+v, want the one", again)
		}
	})

	// A head GitHub answers only `unknown` with `mergeable` null is bounded as `blocked` is: told on
	// the second null read in a row at the floor. Found while fixing the alternation hole: `unknown`
	// was waited on quietly before the count was read at all.
	t.Run("null reads beside unknown alone", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateUnknown)
		github.mergeable, github.checks, github.step = record.MergeabilityUnknown, record.HeadChecksUnread, classify.NullMergeableFloor
		github.apply("approve", approve)
		waits(t, pool, "one null read, beside unknown")
		github.read()
		told := reviewStuckNotices(t, pool)
		if len(told) != 1 || !strings.Contains(told[0].Reason, "GitHub reports that head unknown") ||
			!strings.Contains(told[0].Reason, "on 2 read(s) in a row") {
			t.Fatalf("review-stuck notices %+v, want one naming unknown and two null reads in a row", told)
		}
	})

	// What holds a `blocked` head is part of the notice's key, and the mark keeps every finding told
	// for the head: told once for a null `mergeable`, the same head is told again when a review
	// request holds it next, and findings that then take turns - a push to the base holding it on a
	// null `mergeable` past the floor, then the review request again - tell nothing more.
	t.Run("findings of one head that take turns are each told once", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateBlocked)
		github.mergeable, github.checks, github.step = record.MergeabilityUnknown, record.HeadChecksUnread, classify.NullMergeableFloor
		github.apply("approve", approve)
		github.read()
		if told := reviewStuckNotices(t, pool); len(told) != 1 {
			t.Fatalf("review-stuck notices %+v, want the one of the null mergeable", told)
		}
		github.mergeable, github.checks, github.requested = record.MergeabilityMergeable, record.HeadChecksConcluded, 1
		github.read()
		told := reviewStuckNotices(t, pool)
		if len(told) != 2 || !strings.Contains(told[1].Reason, "GitHub holds 1 review request(s) open") {
			t.Fatalf("review-stuck notices %+v, want a second naming the open review request", told)
		}
		for turn := range 2 {
			github.state, github.mergeable, github.checks = record.MergeStateUnknown, record.MergeabilityUnknown, record.HeadChecksUnread
			github.read()
			github.read()
			github.state, github.mergeable, github.checks = record.MergeStateBlocked, record.MergeabilityMergeable, record.HeadChecksConcluded
			github.read()
			if again := reviewStuckNotices(t, pool); len(again) != 2 {
				t.Fatalf("after turn %d of a null mergeable and the review request again the notices are %+v, want the two", turn+1, again)
			}
		}
	})

	// A head GitHub answers `behind`, then `unknown` with `mergeable` null past the floor while it
	// recomputes, then `behind` again: `behind` is told once, the null `mergeable` is a finding of
	// its own told once, and `behind` again tells nothing.
	t.Run("behind, a null mergeable, then behind again", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateBehind)
		github.step = classify.NullMergeableFloor
		github.apply("approve", approve)
		if told := reviewStuckNotices(t, pool); len(told) != 1 {
			t.Fatalf("review-stuck notices %+v, want the one of behind", told)
		}
		github.state, github.mergeable, github.checks = record.MergeStateUnknown, record.MergeabilityUnknown, record.HeadChecksUnread
		github.read()
		github.read()
		if told := reviewStuckNotices(t, pool); len(told) != 2 {
			t.Fatalf("review-stuck notices %+v, want a second of the null mergeable", told)
		}
		github.state, github.mergeable, github.checks = record.MergeStateBehind, record.MergeabilityMergeable, record.HeadChecksConcluded
		github.read()
		if again := reviewStuckNotices(t, pool); len(again) != 2 {
			t.Fatalf("after behind again the notices are %+v, want the two", again)
		}
	})

	// A decision an earlier release marked holds the finding it told as ToldStuckState and no list
	// (record.ReviewDecision.ToldStuck). Its finding is kept when the next one is told: `behind` told
	// by that release, a null `mergeable` past the floor told now, and `behind` again tells nothing.
	// A writer that appended to the empty list would forget `behind` and tell it a second time.
	t.Run("a decision an earlier release marked keeps its finding", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateBehind)
		github.step = classify.NullMergeableFloor
		github.apply("approve", approve)
		if told := reviewStuckNotices(t, pool); len(told) != 1 {
			t.Fatalf("review-stuck notices %+v, want the one of behind", told)
		}
		// The mark as an earlier release wrote it: the head and the state, no list.
		if _, err := pool.Exec(t.Context(), `update phases set decision = decision - 'toldStuck'
			where issue = 'LEGION-208' and role = 'reviewer'`); err != nil {
			t.Fatalf("leave the mark as an earlier release wrote it: %v", err)
		}
		github.read()
		if told := reviewStuckNotices(t, pool); len(told) != 1 {
			t.Fatalf("after behind read again the notices are %+v, want the one: the earlier release told behind", told)
		}
		github.state, github.mergeable, github.checks = record.MergeStateUnknown, record.MergeabilityUnknown, record.HeadChecksUnread
		github.read()
		github.read()
		if told := reviewStuckNotices(t, pool); len(told) != 2 {
			t.Fatalf("review-stuck notices %+v, want a second of the null mergeable", told)
		}
		github.state, github.mergeable, github.checks = record.MergeStateBehind, record.MergeabilityMergeable, record.HeadChecksConcluded
		github.read()
		if again := reviewStuckNotices(t, pool); len(again) != 2 {
			t.Fatalf("after behind again the notices are %+v, want the two: the earlier release's finding must be kept", again)
		}
	})

	// GitHub sends `review_request_removed` when a requested review is withdrawn, and no other
	// event says so. The fact asks a read, which counts the requests again, and the read that finds
	// none open ends the round.
	t.Run("a review request withdrawn", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateBlocked)
		github.requested = 1
		github.apply("approve", approve)
		if got := issuePhase(t, pool); got != phase.Reviewing {
			t.Fatalf("LEGION-208 is in %s, want reviewing while a review is asked for", got)
		}
		github.requested = 0
		reads := len(github.answered)
		github.apply("withdrawn", intake.PullRequestReviewRequests{Repo: "sjawhar/legion", Number: 42, HeadSHA: headOne})
		if len(github.answered) != reads+1 {
			t.Fatalf("the withdrawn request asked GitHub for %d reads, want one", len(github.answered)-reads)
		}
		if got := issuePhase(t, pool); got != phase.Accepting {
			t.Fatalf("LEGION-208 is in %s, want accepting: the read after the withdrawal found no request open", got)
		}
	})

	t.Run("a check run of the head is still running", func(t *testing.T) {
		pool := migratedPool(t)
		seedMergeStateRound(t, pool, record.MergeStateUnread)
		// CI settled the head before the reviewer's push; the head the approval stands on has no
		// settlement of its own, which is what a CI run still going there leaves.
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: headOne, Failing: []string{}, Cancelled: []string{},
			Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateBlocked,
			CheckedHead: headEarlier, CheckRuns: []record.AttemptRun{{Name: "unit tests", ID: 1, Head: headEarlier, Conclusion: record.RunSucceeded}}})
		github := newGitHubFake(t, pool, testEngine(config.DesignGateRootIssues, nil), record.MergeStateBlocked)
		github.apply("approve", approve)
		if len(github.answered) != 1 {
			t.Fatalf("GitHub answered %v, want the one read the approval asked for", github.answered)
		}
		waits(t, pool, "GitHub answers blocked while a check run of the head runs")
		// The last check concludes: the settlement asks for a read, and GitHub still answers blocked.
		github.apply("settled", intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: headOne, Generation: 1, Snapshot: "settled",
			CheckRuns: []record.AttemptRun{{Name: "unit tests", ID: 2}}, Failing: []string{}, Cancelled: []string{}})
		if len(github.answered) != 2 || github.answered[1] != record.MergeStateBlocked {
			t.Fatalf("GitHub answered %v, want a second read, asked by the settlement, answered blocked", github.answered)
		}
		standsUntold(t, pool, phase.Accepting, "every check run of the head has concluded and GitHub answers blocked")
	})

	for _, state := range []record.MergeState{record.MergeStateBehind, record.MergeStateDraft} {
		t.Run("GitHub answers "+string(state), func(t *testing.T) {
			pool, github := rig(t, state)
			github.apply("approve", approve)
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("LEGION-208 is in %s, want reviewing: GitHub will not merge a head it answers %s", got, state)
			}
			told := reviewStuckNotices(t, pool)
			if len(told) != 1 || !strings.Contains(told[0].Reason, "GitHub reports that head "+string(state)+" rather than one it would merge") {
				t.Fatalf("review-stuck notices %+v, want one naming %s (stuckMergeState)", told, state)
			}
			if stuck := stuckRoleMessages(t, pool); len(stuck) != 1 {
				t.Fatalf("the reviewer was told %d times %+v, want once", len(stuck), stuck)
			}
			if !waitsOnGitHub(t, pool) {
				t.Fatalf("the stuck round waits on no answer of GitHub's, want the wait the state shows")
			}
			github.read()
			if again := reviewStuckNotices(t, pool); len(again) != 1 || issuePhase(t, pool) != phase.Reviewing {
				t.Fatalf("after a second %s read the notices are %+v in %s, want the one notice and reviewing", state, again, issuePhase(t, pool))
			}
		})
	}
}
