package workflow

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
)

// reviewerRow is LEGION-208's reviewer row as the record holds it.
func reviewerRow(t *testing.T, pool *pgxpool.Pool) record.PhaseRow {
	t.Helper()
	return roleRow(t, pool, claim.RoleReviewer)
}

// roleRow is role's phases row of LEGION-208 as the record holds it (phaseRowOf), failing the
// test where the record holds none.
func roleRow(t *testing.T, pool *pgxpool.Pool, role claim.Role) record.PhaseRow {
	t.Helper()
	row := phaseRowOf(t, pool, role)
	if row.Role != role {
		t.Fatalf("LEGION-208 holds no %s's row", role)
	}
	return row
}

// reviewTestedAt is when each review of these tests is submitted: just after the one moment the
// test engine's clock reads (testEngine), at which every entry these tests raise is raised, as a
// real review is submitted after its round's entry. No time decides whether a review moves the
// commit the reviewer read: the entry its footer names does (workflow's review). It is within
// answerSkew of a completion that clock applies, so it is no answer of the reviewer's to a round it
// completed, as a review submitted before the completion is not.
var reviewTestedAt = time.Date(2026, 9, 23, 0, 0, 5, 0, time.UTC)

// reviewAt is the review App's review of state as the reviewer posts it for the round that
// stands, its Legion footer naming head, the session the shared fixtures record on the reviewer's
// claim (seedRoundOneRows) and the entry the reviewer's row holds when it is built
// (roundReviewBody). A review of another round sets its own body (reviewBodyOfEntry).
func reviewAt(t *testing.T, pool *pgxpool.Pool, id int64, state, head string, at time.Time) intake.PullRequestReview {
	t.Helper()
	return intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: id, State: state, CommitID: head,
		Author: testReviewApp, Body: roundReviewBody(t, pool, "ses-reviewer", "the review", head), SubmittedAt: at}
}

// backToReviewing takes LEGION-208 from implementing through testing into reviewing again, as the
// implementer's and the tester's completions of round n do.
func backToReviewing(t *testing.T, apply func(string, intake.Fact) intake.Result, round int) {
	t.Helper()
	backToReviewingAs(t, apply, round, func(role claim.Role) claim.Token {
		return map[claim.Role]claim.Token{claim.RoleImplementer: "implement-claim", claim.RoleTester: "test-claim"}[role]
	})
}

// backToReviewingAs is backToReviewing with each completion from the claim as names for its role,
// read when that completion is made: on an issue with plan rows, the step's worker (workerOr),
// whose token the step's entry writes on the role's row.
func backToReviewingAs(t *testing.T, apply func(string, intake.Fact) intake.Result, round int, as func(claim.Role) claim.Token) {
	t.Helper()
	n := strconv.Itoa(round)
	apply("implementer-"+n, intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: as(claim.RoleImplementer),
		Summary: "fixed", Commit: "round-" + n, HandoffRecord: "implement-" + n, HandoffVersion: round})
	apply("tester-"+n, intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: as(claim.RoleTester),
		Summary: "tested", Verdict: "pass", HandoffRecord: "test-" + n, HandoffVersion: round})
}

// The reviewer's own deciding review of the entry that stands writes the commit it read on the
// reviewer's row beside the round's decision (record.PhaseRow.ReviewedHead), and the entry into
// the next round, which clears the decision (clearHandoff), leaves that commit where it stands.
func TestADecidingReviewRecordsTheCommitItRead(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedReviewOf(t, pool, headOne, "green")
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	apply("request-changes", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("after the request for changes the issue is in %s, want implementing", got)
	}
	if got := reviewerRow(t, pool).ReviewedHead; got != headOne {
		t.Fatalf("the reviewer's row reads the reviewed commit %q, want %s, the head its footer names", got, headOne)
	}

	backToReviewing(t, apply, 2)
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the tester passed round 2 the issue is in %s, want reviewing", got)
	}
	row := reviewerRow(t, pool)
	if row.Decision != nil {
		t.Fatalf("round 2's reviewer row holds the decision %+v, want none: the entry into reviewing ends round 1's", row.Decision)
	}
	if row.ReviewedHead != headOne {
		t.Fatalf("round 2's reviewer row reads the reviewed commit %q, want %s: no entry into a phase clears it", row.ReviewedHead, headOne)
	}
}

// anEarlierReviewRead seeds LEGION-208's reviewer row with headEarlier as the commit an earlier
// deciding review read, so a review that then records nothing still reads headEarlier and cannot
// pass for one that recorded the head its own footer names.
func anEarlierReviewRead(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	row := reviewerRow(t, pool)
	row.ReviewedHead = headEarlier
	seedPhase(t, pool, row)
}

// decidedAtHeadOne fails the test unless the reviewer's row reads headOne, the head its deciding
// review of state named in its footer, as the commit that review read; and, after a request for
// changes, unless the task of the reviewer's next round names headOne and not the headEarlier
// anEarlierReviewRead seeded.
func decidedAtHeadOne(t *testing.T, pool *pgxpool.Pool, engine *Engine, state string) {
	t.Helper()
	if got := reviewerRow(t, pool).ReviewedHead; got != headOne {
		t.Fatalf("the reviewer's row reads the reviewed commit %q, want %s, the head its %s review's footer names", got, headOne, state)
	}
	if state != "changes_requested" {
		return
	}
	backToReviewing(t, applyFacts(t, pool, engine), 2)
	task := reviewerRow(t, pool).EntryTask
	readsReviewingTask(t, "round 2", task, 2)
	if !strings.Contains(task, reviewedPartOf(headOne)) || strings.Contains(task, headEarlier) {
		t.Fatalf("round 2's task = %q, want it to name %s, the commit round 1's review read, and not %s", task, headOne, headEarlier)
	}
}

// undecidingReview is a review that decides no round: a comment, a review refused for the head
// its footer names, or a review of an issue outside reviewing. Each is the reviewer's own review
// of the round that stands, its footer naming that round's entry, so only what decides the round
// keeps it from moving the commit.
type undecidingReview struct {
	name  string
	state string
	// footerHead is the head its footer names.
	footerHead string
	// outsideReviewing puts the issue in testing before the review arrives.
	outsideReviewing bool
}

// reviewsThatDecideNothing are the three undeciding reviews of head.
func reviewsThatDecideNothing(head string) []undecidingReview {
	return []undecidingReview{
		{name: "a comment", state: "commented", footerHead: head},
		{name: "a review its footer's head refuses", state: "changes_requested", footerHead: headEarlier},
		{name: "a review outside reviewing", state: "approved", footerHead: head, outsideReviewing: true},
	}
}

// review is the review App's review id of head, submitted at at, as the reviewer posts it once the
// fixture stands where the case puts it.
func (u undecidingReview) review(t *testing.T, pool *pgxpool.Pool, id int64, head string, at time.Time) intake.PullRequestReview {
	t.Helper()
	review := reviewAt(t, pool, id, u.state, head, at)
	review.Body = roundReviewBody(t, pool, "ses-reviewer", "the review", u.footerHead)
	return review
}

// Only a review that decides the round in reviewing moves the commit: a comment decides nothing, a
// review refused for the head its footer names decides nothing, and a review of an issue outside
// reviewing decides no round.
func TestAReviewThatDecidesNothingRecordsNoCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range reviewsThatDecideNothing(headOne) {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, headOne, "green")
			if tc.outsideReviewing {
				standsIn(t, pool, phase.Testing, "testing")
			}
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			apply("review", tc.review(t, pool, 1, headOne, reviewTestedAt))
			row := reviewerRow(t, pool)
			if row.Decision != nil && row.Decision.State != "" {
				t.Fatalf("the reviewer's row holds the decision %+v, want none: the review decides nothing", row.Decision)
			}
			if row.ReviewedHead != "" {
				t.Fatalf("the reviewer's row reads the reviewed commit %q, want none: the review decided nothing", row.ReviewedHead)
			}
		})
	}
}

// A review that decides nothing leaves the commit an earlier deciding review recorded where it
// stands, as it leaves an empty one empty (TestAReviewThatDecidesNothingRecordsNoCommit): each case
// starts in round 2, after the reviewer's request for changes of round 1 recorded headOne and the
// implementer's push moved the pull request to headTwo, and the review it applies names a head
// other than headOne. A path that cleared the commit would read none, and one that moved it would
// read the head that review names.
func TestAReviewThatDecidesNothingKeepsTheCommitAnEarlierReviewRecorded(t *testing.T) {
	t.Parallel()
	for _, tc := range reviewsThatDecideNothing(headTwo) {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, headOne, "green")
			engine := testEngine(config.DesignGateRootIssues, nil)
			apply := applyFacts(t, pool, engine)
			apply("request-changes", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
			mergesForward(t, pool, engine, headOne, headTwo, reviewTestedAt.Add(time.Hour))
			backToReviewing(t, apply, 2)
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("after the tester passed round 2 the issue is in %s, want reviewing", got)
			}
			if got := reviewerRow(t, pool).ReviewedHead; got != headOne {
				t.Fatalf("round 2's reviewer row reads the reviewed commit %q, want %s, the head round 1's review read", got, headOne)
			}
			if tc.outsideReviewing {
				standsIn(t, pool, phase.Testing, "testing")
			}
			apply("review", tc.review(t, pool, 2, headTwo, reviewTestedAt.Add(2*time.Hour)))
			row := reviewerRow(t, pool)
			if row.Decision != nil && row.Decision.State != "" {
				t.Fatalf("the reviewer's row holds the decision %+v, want none: the review decides nothing", row.Decision)
			}
			if row.ReviewedHead != headOne {
				t.Fatalf("the reviewer's row reads the reviewed commit %q, want %s, the one round 1's review recorded: the review decided nothing",
					row.ReviewedHead, headOne)
			}
		})
	}
}

// A person's review decides a round as it always has, on the commit_id its webhook carried, and
// the round's decision is the person's. The commit the reviewer's next round is told is still the
// one the reviewer's newest recorded review read: the reviewer never read the person's.
func TestAPersonsReviewDoesNotMoveTheReviewersCommit(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedReviewOf(t, pool, headOne, "green")
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	apply("reviewer-requests-changes", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
	backToReviewing(t, apply, 2)
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the tester passed round 2 the issue is in %s, want reviewing", got)
	}

	apply("person-requests-changes", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 2, State: "changes_requested", CommitID: headTwo,
		Author: "a-writer", AuthorCanWrite: true, Body: "rename it", SubmittedAt: time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)})
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("after the person's request for changes the issue is in %s, want implementing", got)
	}
	row := reviewerRow(t, pool)
	if row.Decision == nil || row.Decision.State != "changes_requested" || row.Decision.Head != headTwo || row.Decision.Body != "rename it" {
		t.Fatalf("round 2's decision = %+v, want the person's request for changes at %s", row.Decision, headTwo)
	}
	if row.ReviewedHead != headOne {
		t.Fatalf("the reviewer's row reads the reviewed commit %q, want %s: the person's review is no review the reviewer read", row.ReviewedHead, headOne)
	}

	backToReviewing(t, apply, 3)
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the tester passed round 3 the issue is in %s, want reviewing", got)
	}
	third := reviewerRow(t, pool)
	readsReviewingTask(t, "round 3", third.EntryTask, 3)
	if !strings.Contains(third.EntryTask, reviewedPartOf(headOne)) {
		t.Fatalf("round 3's task = %q, want it to name %s, the commit the reviewer's own last review read, and not the person's %s",
			third.EntryTask, headOne, headTwo)
	}
}

// A deciding review that is not of the reviewer's session moves no commit (byReviewer), whatever
// its body says: here a person with write access, whose review body holds the reviewer's own
// Legion footer in a quote, naming the reviewer's session and the entry that stands
// (lastLegionFooter reads the last footer wherever it stands in the body). The person's review
// decides the round, as a person's always does, on the commit_id its webhook carried, and the
// commit stays the one the reviewer's own review read: the session condition of the write is the
// one thing that holds it there, since the entry the footer names is the row's. The pull request
// moves to headTwo before the person's review, so a commit moved by it would differ from the
// reviewer's headOne. TestAPersonsReviewDoesNotMoveTheReviewersCommit is the person's review with
// no footer.
func TestAPersonsReviewThatQuotesTheReviewersFooterDoesNotMoveItsCommit(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	seedReviewOf(t, pool, headOne, "green")
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	apply("reviewer-requests-changes", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
	if got := reviewerRow(t, pool).ReviewedHead; got != headOne {
		t.Fatalf("after the reviewer's own review the reviewer's row reads the reviewed commit %q, want %s: its footer names the session on the reviewer's claim", got, headOne)
	}
	apply("reviewer-completes", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: reviewClaim,
		Summary: "reviewed", HandoffRecord: "review-1", HandoffVersion: 1})
	mergesForward(t, pool, engine, headOne, headTwo, reviewTestedAt.Add(time.Hour))
	backToReviewing(t, apply, 2)
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the tester passed round 2 the issue is in %s, want reviewing", got)
	}

	quoted := "Agreed: rename it.\n\n> " + strings.ReplaceAll(roundReviewBody(t, pool, "ses-reviewer", "rename it", headTwo), "\n", "\n> ")
	review := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 2, State: "changes_requested", CommitID: headTwo,
		Author: "a-writer", AuthorCanWrite: true, Body: quoted, SubmittedAt: reviewTestedAt.Add(2 * time.Hour)}
	if review.LegionSession() != "ses-reviewer" || review.LegionEntry() != reviewerRow(t, pool).Entry {
		t.Fatalf("the person's review names session %q and entry %d, want ses-reviewer and the row's entry %d: the quote must carry the reviewer's footer",
			review.LegionSession(), review.LegionEntry(), reviewerRow(t, pool).Entry)
	}
	apply("person-requests-changes", review)
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("after the person's request for changes the issue is in %s, want implementing: its author can write, so it decides the round", got)
	}
	row := reviewerRow(t, pool)
	if row.Decision == nil || row.Decision.State != "changes_requested" || row.Decision.Head != headTwo {
		t.Fatalf("round 2's decision = %+v, want the person's request for changes at %s", row.Decision, headTwo)
	}
	if row.ReviewedHead != headOne {
		t.Fatalf("the reviewer's row reads the reviewed commit %q, want %s: a person's review is no review the reviewer read, whatever footer it quotes", row.ReviewedHead, headOne)
	}
}

// reviewedPartOf is the part of a later round's task that names head as the commit the
// reviewer's newest recorded review read.
func reviewedPartOf(head string) string {
	return " " + reviewround.CommitLabel + ": " + head + "."
}

// endsNamingNoCommit says whether task ends with the daemon's sentence naming no commit,
// reviewedPartOf("none"), and then the sentence of entry, and names the commit's label nowhere
// else: the task of a round whose reviewer's row holds no reviewed commit.
func endsNamingNoCommit(task string, entry int64) bool {
	return strings.HasSuffix(task, reviewedPartOf("none")+reviewEntryPartOf(entry)) && strings.Count(task, reviewround.CommitLabel) == 1
}

// readsReviewingTask fails the test unless task is the reviewing task of the round whose tester
// handed off version version: an assertion of what a round's task lacks is only worth something
// once it has read that round's task, since a row a round never reached still holds the task of
// the round before it.
func readsReviewingTask(t *testing.T, round, task string, version int) {
	t.Helper()
	handoff := " Handoff: test version " + strconv.Itoa(version) + " ("
	if !strings.Contains(task, " Phase: reviewing.") || !strings.Contains(task, handoff) {
		t.Fatalf("%s's task = %q, want the reviewing task the tester's handoff version %d started", round, task, version)
	}
}

// reviewRoundOne seeds LEGION-208 as a root in testing, its pull request at headOne, and walks
// it into reviewing on the tester's pass, so the reviewer's first entry of the generation is a
// transition's, as in a live tree.
func reviewRoundOne(t *testing.T) (*pgxpool.Pool, *Engine, func(string, intake.Fact) intake.Result) {
	t.Helper()
	pool := migratedPool(t)
	seedReviewOf(t, pool, headOne, "green")
	standsIn(t, pool, phase.Testing, "testing")
	engine, apply := entersRoundOne(t, pool)
	return pool, engine, apply
}

// entersRoundOne is the tester's pass that takes LEGION-208, seeded in testing, into its first
// review round.
func entersRoundOne(t *testing.T, pool *pgxpool.Pool) (*Engine, func(string, intake.Fact) intake.Result) {
	t.Helper()
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := applyFacts(t, pool, engine)
	apply("round-one-tester", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim",
		Summary: "tested", Verdict: "pass", HandoffRecord: "test-1", HandoffVersion: 1})
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the tester passed round 1 the issue is in %s, want reviewing", got)
	}
	return engine, apply
}

// reviewedTwice is LEGION-208 in its second review round, after the reviewer requested changes
// at headOne in its first.
func reviewedTwice(t *testing.T, apply func(string, intake.Fact) intake.Result, pool *pgxpool.Pool) {
	t.Helper()
	apply("request-changes", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
	backToReviewing(t, apply, 2)
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the tester passed round 2 the issue is in %s, want reviewing", got)
	}
	if got := reviewerRow(t, pool).ReviewedHead; got != headOne {
		t.Fatalf("round 2's reviewer row reads the reviewed commit %q, want %s", got, headOne)
	}
}

// reviewRoundTwo is reviewRoundOne taken into its second review round (reviewedTwice).
func reviewRoundTwo(t *testing.T) (*pgxpool.Pool, *Engine, func(string, intake.Fact) intake.Result) {
	t.Helper()
	pool, engine, apply := reviewRoundOne(t)
	reviewedTwice(t, apply, pool)
	return pool, engine, apply
}

// outboxRan takes every supervise row out of the outbox, as the outbox does once it has run them,
// so the start a test makes next is the only one there.
func outboxRan(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "delete from outbox where kind = 'supervise'"); err != nil {
		t.Fatalf("run the outbox's supervise rows: %v", err)
	}
}

// onlyReviewerStart is the one start of LEGION-208's reviewer the outbox holds, failing the test
// unless it holds exactly one.
func onlyReviewerStart(t *testing.T, pool *pgxpool.Pool) record.SuperviseRequest {
	t.Helper()
	var starts []record.SuperviseRequest
	for _, start := range superviseRowsOf(t, pool, "LEGION-208", "start") {
		if start.Role == claim.RoleReviewer {
			starts = append(starts, start)
		}
	}
	if len(starts) != 1 {
		t.Fatalf("the outbox holds the reviewer starts %+v, want one", starts)
	}
	return starts[0]
}

// A later review round is told the commit the review before it read (v-review-range): the
// reviewer's own deciding review in round 1, which names round 1's entry, recorded the commit it
// read on the reviewer's row (record.PhaseRow.ReviewedHead), and the start of round 2 names it in
// its task (namingTheReviewRound), before the sentence of its entry, which ends the task. The
// first round of a generation has no earlier review, and its task names no commit; nor does a task
// of any other of Legion's roles (a review step of a role of the deployment names its own commit).
func TestALaterReviewRoundIsToldTheCommitTheReviewBeforeItRead(t *testing.T) {
	t.Parallel()
	pool, _, apply := reviewRoundOne(t)
	first := reviewerRow(t, pool)
	readsReviewingTask(t, "round 1", first.EntryTask, 1)
	if !endsNamingNoCommit(first.EntryTask, first.Entry) {
		t.Fatalf("round 1's task = %q, want it to end naming no reviewed commit, `none`, then its entry: no review came before it", first.EntryTask)
	}

	// The reviewer requests changes at headOne; the implementer completes at a new commit and the
	// tester passes it.
	reviewedTwice(t, apply, pool)
	second := reviewerRow(t, pool)
	if second.Entry <= first.Entry {
		t.Fatalf("round 2's reviewer row is on entry %d, want one above round 1's %d: round 2 started", second.Entry, first.Entry)
	}
	want := "Continue root. Issue: LEGION-208. Phase: reviewing. Working copy: branch legion/LEGION-208. Handoff: test version 2 (dispatch handoff-read --issue LEGION-208 --phase test --version 2)." +
		" Tester's verdict: pass. Pull request: #42." + reviewedPartOf(headOne) + reviewEntryPartOf(second.Entry)
	if second.EntryTask != want {
		t.Fatalf("round 2's task = %q, want %q", second.EntryTask, want)
	}

	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		row := roleRow(t, pool, role)
		if row.EntryTask == "" || !strings.Contains(row.EntryTask, " Phase: ") {
			t.Fatalf("the %s's row holds the task %q, want the task round 2 started it with", role, row.EntryTask)
		}
		if strings.Contains(row.EntryTask, reviewround.CommitLabel) {
			t.Fatalf("the %s's task = %q, want no reviewed commit: of Legion's roles only the reviewer is told one", role, row.EntryTask)
		}
	}
	for _, start := range superviseRowsOf(t, pool, "LEGION-208", "start") {
		if start.Role != claim.RoleReviewer && strings.Contains(start.Task, reviewround.CommitLabel) {
			t.Fatalf("the %s's start carries %q, want no reviewed commit", start.Role, start.Task)
		}
	}
}

// The commit a later round names is the one the reviewer's own NEWEST deciding review read, not
// its first (record.PhaseRow.ReviewedHead, "the commit that the reviewer's own newest deciding
// review read"): round 1 requests changes at headOne; the implementer's push moves the pull
// request to headTwo; round 2 names headOne and requests changes at headTwo; round 3 names headTwo
// and not headOne. A review that kept only the first commit it saw would hand round 3 a range
// that reads round 2's work again.
func TestALaterRoundNamesTheCommitOfTheReviewersNewestDecidingReview(t *testing.T) {
	t.Parallel()
	pool, engine, apply := reviewRoundOne(t)
	apply("request-changes-1", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("after round 1's request for changes the issue is in %s, want implementing", got)
	}
	mergesForward(t, pool, engine, headOne, headTwo, reviewTestedAt.Add(time.Hour))
	if got := recordedHead(t, pool); got != headTwo {
		t.Fatalf("after the implementer's push the record holds the head %s, want %s", got, headTwo)
	}
	backToReviewing(t, apply, 2)
	second := reviewerRow(t, pool).EntryTask
	readsReviewingTask(t, "round 2", second, 2)
	if !strings.Contains(second, reviewedPartOf(headOne)) {
		t.Fatalf("round 2's task = %q, want it to name %s, the commit round 1's review read", second, headOne)
	}

	apply("request-changes-2", reviewAt(t, pool, 2, "changes_requested", headTwo, reviewTestedAt.Add(2*time.Hour)))
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("after round 2's request for changes the issue is in %s, want implementing", got)
	}
	if got := reviewerRow(t, pool).ReviewedHead; got != headTwo {
		t.Fatalf("after round 2's review the reviewer's row reads the reviewed commit %q, want %s, the head that newer review read", got, headTwo)
	}
	backToReviewing(t, apply, 3)
	third := reviewerRow(t, pool).EntryTask
	readsReviewingTask(t, "round 3", third, 3)
	if !strings.Contains(third, reviewedPartOf(headTwo)) || strings.Contains(third, headOne) {
		t.Fatalf("round 3's task = %q, want it to name %s, the commit round 2's review read, and not round 1's %s", third, headTwo, headOne)
	}
}

// The commit a later round names is the head the reviewer's own review names in its Legion footer,
// never GitHub's commit_id for that review (reviewHead): GitHub moves a review's commit_id onto each
// later head of the pull request, so round 1's request for changes here reads headOne by its footer
// while its commit_id already names headTwo. A commit taken from the commit_id would hand round 2 a
// range from a head the reviewer never read.
func TestALaterRoundNamesTheHeadTheReviewsFooterNamesAndNotItsCommitID(t *testing.T) {
	t.Parallel()
	pool, _, apply := reviewRoundOne(t)
	review := reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt)
	review.CommitID = headTwo
	apply("request-changes", review)
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("after round 1's request for changes the issue is in %s, want implementing", got)
	}
	backToReviewing(t, apply, 2)
	second := reviewerRow(t, pool).EntryTask
	readsReviewingTask(t, "round 2", second, 2)
	if !strings.Contains(second, reviewedPartOf(headOne)) || strings.Contains(second, headTwo) {
		t.Fatalf("round 2's task = %q, want it to name %s, the head round 1's footer names, and not %s, its commit_id", second, headOne, headTwo)
	}
}

// Every way back into reviewing that keeps the generation tells the reviewer the commit its last
// deciding review read: a transition's start whichever move came before it (enter stores that
// task, so a claim behind its entry is sent the same text), the retry of a held phase, each start
// of a worker level with its entry that builds the phase's resume task — a ready worker's
// catch-up, the boot's resume, the silence repair, and the owed entry of a row that names no
// task — and the promotion a carried hand-over makes, which keeps its rows (admit's readmit
// returns before the clear for an orphan) and so keeps the commit.
func TestTheReviewedCommitSurvivesEachWayBackIntoReviewing(t *testing.T) {
	t.Parallel()
	resumeWant := "Continue root. Issue: LEGION-208. Phase: reviewing. Working copy: branch legion/LEGION-208. Pull request: #42. Resume the existing phase work." + reviewedPartOf(headOne)
	t.Run("the architect's move back after an approval", func(t *testing.T) {
		pool, engine, apply := reviewRoundOne(t)
		apply("approve", reviewAt(t, pool, 1, "approved", headOne, reviewTestedAt))
		if got := issuePhase(t, pool); got != phase.Accepting {
			t.Fatalf("after the approval the issue is in %s, want accepting", got)
		}
		if result := apply("move-back", rootsMoveBack(t, pool, engine, "LEGION-208", phase.Implementing, "the API needs another shape")); result.Refusal != nil {
			t.Fatalf("the architect's move back was refused: %+v", result.Refusal)
		}
		backToReviewing(t, apply, 2)
		task := reviewerRow(t, pool).EntryTask
		readsReviewingTask(t, "round 2", task, 2)
		if !strings.Contains(task, reviewedPartOf(headOne)) {
			t.Fatalf("round 2's task = %q, want it to name %s, the commit the approval read", task, headOne)
		}
	})
	t.Run("the reviewer's own move back from a round that decided nothing", func(t *testing.T) {
		pool, engine, apply := reviewRoundTwo(t)
		if result := apply("move-back", intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleReviewer, To: phase.Implementing,
			Reason: "the change needs the migration first"}); result.Refusal != nil {
			t.Fatalf("the reviewer's move back was refused: %+v", result.Refusal)
		}
		if got := issuePhase(t, pool); got != phase.Implementing {
			t.Fatalf("after the reviewer's move back the issue is in %s, want implementing", got)
		}
		mergesForward(t, pool, engine, headOne, headTwo, reviewTestedAt.Add(2*time.Hour))
		backToReviewing(t, apply, 3)
		task := reviewerRow(t, pool).EntryTask
		readsReviewingTask(t, "round 3", task, 3)
		if !strings.Contains(task, reviewedPartOf(headOne)) {
			t.Fatalf("round 3's task = %q, want it to name %s: round 2 decided nothing, so round 1's review is the last", task, headOne)
		}
	})
	t.Run("the retry of a held reviewing phase", func(t *testing.T) {
		pool, _, apply := reviewRoundTwo(t)
		held := issueRecord(t, pool, "LEGION-208")
		held.Phase, held.Hold = phase.Held, &record.Hold{From: phase.Reviewing}
		seedIssue(t, pool, *held)
		outboxRan(t, pool)
		apply("retry", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision, Reason: "the claim failed"})
		task := onlyReviewerStart(t, pool).Task
		if !strings.Contains(task, " Phase: reviewing.") || !strings.HasSuffix(task, " Pull request: #42. Reason: the claim failed."+reviewedPartOf(headOne)+reviewEntryPartOf(reviewerRow(t, pool).Entry)) {
			t.Fatalf("the retry's task = %q, want the reviewing task naming %s after the reason and before the entry", task, headOne)
		}
	})
	t.Run("a ready worker's catch-up", func(t *testing.T) {
		pool, engine, _ := reviewRoundTwo(t)
		row := reviewerRow(t, pool)
		outboxRan(t, pool)
		token := seedPhaseWorker(t, pool, "LEGION-208", claim.RoleReviewer, "")
		if _, err := pool.Exec(context.Background(), "update claims set given_entry = $1 where token = $2", row.Entry, string(token)); err != nil {
			t.Fatalf("give the reviewer's claim its entry: %v", err)
		}
		if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "supervise:reviewer:2:ready",
			intake.ClaimReady{Issue: "LEGION-208", Role: claim.RoleReviewer, Launch: 2}, engine, admissionStub{}); err != nil {
			t.Fatalf("apply the ready: %v", err)
		}
		if task, want := onlyReviewerStart(t, pool).Task, resumeWant+reviewEntryPartOf(row.Entry); task != want {
			t.Fatalf("the catch-up's task = %q, want %q", task, want)
		}
	})
	t.Run("the boot's resume", func(t *testing.T) {
		pool, engine, _ := reviewRoundTwo(t)
		row, issue := reviewerRow(t, pool), issueRecord(t, pool, "LEGION-208")
		outboxRan(t, pool)
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			s, err := firstScope(ctx, tx, engine, *issue)
			if err != nil {
				return err
			}
			_, err = engine.startSuspendedOwner(ctx, tx, suspendedOwner{scope: s, root: *issue, role: claim.RoleReviewer,
				run: record.RoleRun{Claim: &record.RoleClaim{GivenEntry: row.Entry}}}, row)
			return err
		})
		if task, want := onlyReviewerStart(t, pool).Task, resumeWant+reviewEntryPartOf(row.Entry); task != want {
			t.Fatalf("the resume's task = %q, want %q", task, want)
		}
	})
	t.Run("the silence repair", func(t *testing.T) {
		pool, engine, _ := reviewRoundTwo(t)
		row, issue := reviewerRow(t, pool), issueRecord(t, pool, "LEGION-208")
		outboxRan(t, pool)
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			s, err := firstScope(ctx, tx, engine, *issue)
			if err != nil {
				return err
			}
			return engine.repairSilence(ctx, tx, s, *issue, record.RoleRun{Claim: &record.RoleClaim{GivenEntry: row.Entry}}, row,
				intake.WorkerSilent{Issue: "LEGION-208", Role: claim.RoleReviewer, Generation: 1, LastStep: reviewTestedAt, Bound: time.Hour},
				reviewTestedAt.Add(2*time.Hour))
		})
		if task, want := onlyReviewerStart(t, pool).Task, resumeWant+reviewEntryPartOf(row.Entry); task != want {
			t.Fatalf("the silence repair's task = %q, want %q", task, want)
		}
	})
	t.Run("a carried hand-over's promotion", func(t *testing.T) {
		pool, engine, _ := reviewRoundTwo(t)
		before := reviewerRow(t, pool)
		// The carried orphan's re-admission raises its generation and keeps its phase and its rows.
		// This stands in for admission's re-admission, which this package cannot drive; admit's
		// TestAHandedOverChildCarriesItsPhaseGateAndPullRequest drives the real hand-over, its
		// reviewing case holding a reviewed commit.
		carried := issueRecord(t, pool, "LEGION-208")
		carried.Generation++
		seedIssue(t, pool, *carried)
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			s, err := firstScope(ctx, tx, engine, *carried)
			if err != nil {
				return err
			}
			return engine.StartPromotedWorker(ctx, tx, s, claim.RoleReviewer)
		})
		after := reviewerRow(t, pool)
		if want := resumeWant + reviewEntryPartOf(before.Entry+1); after.Entry != before.Entry+1 || after.EntryTask != want {
			t.Fatalf("the promoted reviewer is on entry %d with the task %q, want entry %d with %q", after.Entry, after.EntryTask, before.Entry+1, want)
		}
	})
}

// liveRootOf seeds key as a root whose tree runs, implementing, holding the admission slot of
// index, which a child of these tests stands under.
func liveRootOf(t *testing.T, pool *pgxpool.Pool, key string, index int) record.Issue {
	t.Helper()
	root := record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: key, Project: "LEGION",
		Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "T", LastDispatchSeq: 1}
	seedIssue(t, pool, root)
	seedSlot(t, pool, record.Slot{Issue: key, Index: index})
	return root
}

// reviewedChild is LEGION-208 as a child of the live root LEGION-207, in its second review round
// after the reviewer requested changes at headOne in its first.
func reviewedChild(t *testing.T) (*pgxpool.Pool, *Engine, func(string, intake.Fact) intake.Result) {
	t.Helper()
	pool := migratedPool(t)
	seedReviewOf(t, pool, headOne, "green")
	liveRootOf(t, pool, "LEGION-207", 0)
	parent := "LEGION-207"
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: parent,
		Parent: &parent, Project: "LEGION", Title: "child", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", LastDispatchSeq: 1})
	engine, apply := entersRoundOne(t, pool)
	reviewedTwice(t, apply, pool)
	return pool, engine, apply
}

// testsIntoReviewing walks LEGION-208, admitted at a new generation, into its first review round
// of that generation on the tester's pass at handoff version 9. The phases before it are seeded:
// what is under test is the reviewer's start, which reads the rows the generation's clear left.
func testsIntoReviewing(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result) {
	t.Helper()
	issue := issueRecord(t, pool, "LEGION-208")
	if issue.Generation != 2 || issue.Phase != phase.Admitted {
		t.Fatalf("LEGION-208 is at generation %d in %s, want admitted at generation 2", issue.Generation, issue.Phase)
	}
	issue.Phase, issue.Status = phase.Testing, "testing"
	seedIssue(t, pool, *issue)
	apply("generation-2-tester", intake.HandoffComplete{Generation: 2, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim",
		Summary: "tested", Verdict: "pass", HandoffRecord: "test-9", HandoffVersion: 9})
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the tester passed generation 2 the issue is in %s, want reviewing", got)
	}
}

// A new generation that clears its rows tells its first reviewer no commit: the re-admission of
// the root (record.Store.ClearTreeGeneration, whose promotion starts the member it finds
// mid-phase), the rerun of a child (ReenterChild) and an adoption (moveIntoTree) each clear the
// reviewer's row with the rest of the generation, so the review of that generation reads the
// whole change.
func TestAGenerationThatClearsItsRowsNamesNoCommit(t *testing.T) {
	t.Parallel()
	t.Run("the re-admission of the root", func(t *testing.T) {
		pool, engine, _ := reviewedChild(t)
		before := reviewerRow(t, pool)
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			return record.NewStore().ClearTreeGeneration(ctx, tx, "LEGION-207", time.Now().UTC())
		})
		member := issueRecord(t, pool, "LEGION-208")
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			s, err := firstScope(ctx, tx, engine, *member)
			if err != nil {
				return err
			}
			return engine.StartPromotedWorker(ctx, tx, s, claim.RoleReviewer)
		})
		after := reviewerRow(t, pool)
		want := "Continue child. Issue: LEGION-208. Phase: reviewing. Working copy: branch legion/LEGION-208. Pull request: #42. Resume the existing phase work." + reviewedPartOf("none") + reviewEntryPartOf(before.Entry+1)
		if after.Entry != before.Entry+1 || after.EntryTask != want {
			t.Fatalf("the promoted reviewer is on entry %d with the task %q, want entry %d with %q", after.Entry, after.EntryTask, before.Entry+1, want)
		}
	})
	t.Run("the rerun of the child", func(t *testing.T) {
		pool, engine, apply := reviewedChild(t)
		before := reviewerRow(t, pool)
		child := issueRecord(t, pool, "LEGION-208")
		fact := RecordedFact(*child)
		fact.Status = "todo"
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			return engine.ReenterChild(ctx, tx, *child, fact)
		})
		testsIntoReviewing(t, pool, apply)
		after := reviewerRow(t, pool)
		if after.Entry <= before.Entry {
			t.Fatalf("the rerun's reviewer is on entry %d, want one above %d: its first round started", after.Entry, before.Entry)
		}
		readsReviewingTask(t, "the rerun's first round", after.EntryTask, 9)
		if !endsNamingNoCommit(after.EntryTask, after.Entry) {
			t.Fatalf("the rerun's first review task = %q, want it to end naming no reviewed commit, `none`: the rerun cleared the generation", after.EntryTask)
		}
	})
	t.Run("an adoption into another tree", func(t *testing.T) {
		pool, engine, apply := reviewedChild(t)
		before := reviewerRow(t, pool)
		root := liveRootOf(t, pool, "LEGION-300", 1)
		member := issueRecord(t, pool, "LEGION-208")
		member.Status = "todo"
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			return engine.AdoptMember(ctx, tx, *member, root, intake.AdoptedChild{Issue: "LEGION-208", Parent: "LEGION-300", Status: "todo"})
		})
		if got := issueRecord(t, pool, "LEGION-208").Tree; got != "LEGION-300" {
			t.Fatalf("the adopted issue stands in tree %s, want LEGION-300", got)
		}
		testsIntoReviewing(t, pool, apply)
		after := reviewerRow(t, pool)
		if after.Entry <= before.Entry {
			t.Fatalf("the adopted reviewer is on entry %d, want one above %d: its first round started", after.Entry, before.Entry)
		}
		readsReviewingTask(t, "the adoption's first round", after.EntryTask, 9)
		if !endsNamingNoCommit(after.EntryTask, after.Entry) {
			t.Fatalf("the adoption's first review task = %q, want it to end naming no reviewed commit, `none`: the adoption cleared the generation", after.EntryTask)
		}
	})
}

// The negative control of the commit a later round is told: the commit round 2 names comes from
// the record of the first review and from nothing else. With the stored commit taken off the
// reviewer's row after round 1's review, round 2's task names none; with it left, the same lines
// name it, so the control can fail.
func TestALaterReviewRoundWithItsRecordRemovedIsToldNoCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		removed bool
	}{
		{name: "the record kept", removed: false},
		{name: "the record removed", removed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, apply := reviewRoundOne(t)
			first := reviewerRow(t, pool)
			apply("request-changes", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
			if tc.removed {
				if _, err := pool.Exec(context.Background(), "update phases set reviewed_head = '' where issue = 'LEGION-208' and role = 'reviewer'"); err != nil {
					t.Fatalf("remove the reviewed commit: %v", err)
				}
			}
			backToReviewing(t, apply, 2)
			second := reviewerRow(t, pool)
			if second.Entry <= first.Entry {
				t.Fatalf("round 2's reviewer row is on entry %d, want one above round 1's %d: round 2 started", second.Entry, first.Entry)
			}
			readsReviewingTask(t, "round 2", second.EntryTask, 2)
			if tc.removed && !endsNamingNoCommit(second.EntryTask, second.Entry) {
				t.Fatalf("round 2's task = %q, want it to end naming no reviewed commit, `none`: the record of round 1's review was removed", second.EntryTask)
			}
			if !tc.removed && !strings.Contains(second.EntryTask, reviewedPartOf(headOne)) {
				t.Fatalf("round 2's task = %q, want it to name %s, the commit round 1's review read", second.EntryTask, headOne)
			}
		})
	}
}
