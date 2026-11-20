package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Dispatch's redelivery sweep asks GitHub for a failed delivery again for up to 72 hours, so a
// webhook can reach the daemon that long after the events that followed it. A redelivery of a
// delivery the daemon applied carries that delivery's id and is dropped as a duplicate; the tests
// here apply a late event whose newer state came from another delivery, or from the daemon's own
// read of an issue's pull request - the facts that read applies (opened-read, head-read,
// review-read), each built here as the read builds it - and show that the late event changes no
// record.

// recordTables is every row of the records a GitHub fact can write - the issues, their phase rows,
// the pull requests, the production checks and the outbox - as text, to compare before and after a
// fact.
func recordTables(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	return tablesText(t, pool, "issues", "phases", "pull_requests", "production_checks", "outbox")
}

// tablesText is every row of tables as text.
func tablesText(t *testing.T, pool *pgxpool.Pool, tables ...string) string {
	t.Helper()
	var texts []string
	for _, table := range tables {
		var rows string
		if err := pool.QueryRow(t.Context(), "select coalesce(json_agg(to_jsonb(t) order by to_jsonb(t)::text), '[]')::text from "+table+" t").Scan(&rows); err != nil {
			t.Fatalf("read the %s: %v", table, err)
		}
		texts = append(texts, table+": "+rows)
	}
	return strings.Join(texts, "\n")
}

// githubFacts applies a fact under source github and the event id given, as the daemon's GitHub
// consumer and its per-issue read do, and fails t on an error.
func githubFacts(t *testing.T, pool *pgxpool.Pool, engine *Engine) func(id string, fact intake.Fact) {
	return func(id string, fact intake.Fact) {
		t.Helper()
		if _, err := intake.ApplyFact(context.Background(), pool, "github", id, fact, engine, admissionStub{}); err != nil {
			t.Fatalf("apply %s (%T): %v", id, fact, err)
		}
	}
}

// lateChangesNothing applies fact as a late webhook under an event id of its own, as a redelivery
// of a delivery the daemon never applied arrives, and fails t when it changed any record.
func lateChangesNothing(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact), id string, fact intake.Fact) {
	t.Helper()
	before := recordTables(t, pool)
	apply(id, fact)
	if after := recordTables(t, pool); after != before {
		t.Fatalf("the late %T changed the record:\nbefore %s\nafter  %s", fact, before, after)
	}
}

// seedLateIssue seeds LEGION-208 in ph with status, at generation 1, with its reviewer's, tester's
// and implementer's rows and each of rows, and pr as the pull request of its branch when pr is not
// nil.
func seedLateIssue(t *testing.T, pool *pgxpool.Pool, ph phase.Phase, status string, pr *record.PullRequest, rows ...record.PhaseRow) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: ph, Generation: 1, Status: status, Rank: "U", LastDispatchSeq: 1})
	for _, row := range append([]record.PhaseRow{
		{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"},
		{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"},
		{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"},
	}, rows...) {
		seedPhase(t, pool, row)
	}
	if pr != nil {
		seedPR(t, pool, *pr)
	}
}

// redRecord is sjawhar/legion#42 on legion/LEGION-208 in state, at head with GitHub's clock at,
// red on ci, the one check its base branch requires.
func redRecord(state record.PullRequestState, head string, at time.Time) *record.PullRequest {
	return &record.PullRequest{State: state, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208",
		HeadSHA: head, HeadUpdatedAt: at, CheckedHead: head, Failing: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, Snapshot: head + "-red"}
}

// readOpened is the opened the daemon's read of the issue applies for #42 when the open pull
// request on its branch is at head with updated_at at and the record does not hold it open (the
// daemon's issuePullRequest): a reopen when the record holds #42 closed.
func readOpened(head string, at time.Time, reopened bool) intake.Fact {
	return intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head,
		URL: "https://github.com/sjawhar/legion/pull/42", UpdatedAt: at, Reopened: reopened}
}

// readHead is the synchronize the daemon's read applies when its read of #42 finds GitHub's head
// at head, with updated_at at (the daemon's followHead).
func readHead(head string, at time.Time) intake.Fact {
	return intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head, UpdatedAt: at}
}

// closedAt is #42's close without a merge at head, with updated_at at, as its webhook carries it.
func closedAt(head string, at time.Time) intake.Fact {
	return intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42, HeadSHA: head, UpdatedAt: at}
}

// lateMerged is #42's merge at merge commit "merge", as its webhook carries it: with no clock.
var lateMerged intake.Fact = intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}

// After a pull request's lifecycle was recorded - an opened or a reopen the daemon's read of the
// issue found, a head it followed, or a close its webhook carried - a webhook of an event before
// it, delivered late, changes nothing: the recorded fact carries GitHub's updated_at as it was
// read, which no event before that is newer than, and an event older than the recorded clock is a
// late redelivery (classify.LateLifecycle). The webhook of the event already recorded, arriving
// afterwards, changes nothing either.
func TestALateLifecycleEventAfterTheRecordedOneChangesNothing(t *testing.T) {
	read := lateApplied
	for _, tc := range []struct {
		name     string
		phase    phase.Phase
		status   string
		seed     *record.PullRequest
		recorded intake.Fact
		head     string
		state    record.PullRequestState
		late     intake.Fact
	}{
		{"an opened older than the recorded opened", phase.Testing, "testing", nil,
			readOpened("head-d", read, false), "head-d", record.PullRequestOpen, lateOpened("head-a", read.Add(-time.Hour))},
		{"a synchronize older than the recorded opened", phase.Testing, "testing", nil,
			readOpened("head-d", read, false), "head-d", record.PullRequestOpen, lateSync("head-b", read.Add(-30*time.Minute))},
		{"a close older than the recorded opened", phase.Testing, "testing", nil,
			readOpened("head-d", read, false), "head-d", record.PullRequestOpen, lateClosed("head-b", read.Add(-30*time.Minute))},
		{"the reopen's own webhook after the recorded reopen", phase.Reviewing, "needs_review", redRecord(record.PullRequestClosed, "head-c", read.Add(-2*time.Hour)),
			readOpened("head-d", read, true), "head-d", record.PullRequestOpen, lateReopened("head-d", read.Add(-30*time.Minute))},
		{"a close older than the recorded reopen", phase.Reviewing, "needs_review", redRecord(record.PullRequestClosed, "head-c", read.Add(-2*time.Hour)),
			readOpened("head-d", read, true), "head-d", record.PullRequestOpen, lateClosed("head-c", read.Add(-2*time.Hour))},
		{"a synchronize older than the recorded head", phase.Reviewing, "needs_review", redRecord(record.PullRequestOpen, "head-c", read.Add(-2*time.Hour)),
			readHead("head-e", read), "head-e", record.PullRequestOpen, lateSync("head-d", read.Add(-time.Hour))},
		{"a reopen older than the recorded close", phase.Reviewing, "needs_review", redRecord(record.PullRequestOpen, "head-c", read.Add(-2*time.Hour)),
			closedAt("head-e", read), "head-e", record.PullRequestClosed, lateReopened("head-c", read.Add(-time.Hour))},
		{"a synchronize older than the recorded close", phase.Reviewing, "needs_review", redRecord(record.PullRequestOpen, "head-c", read.Add(-2*time.Hour)),
			closedAt("head-e", read), "head-e", record.PullRequestClosed, lateSync("head-e", read.Add(-30*time.Minute))},
		{"the close's own webhook after the recorded close", phase.Reviewing, "needs_review", redRecord(record.PullRequestOpen, "head-c", read.Add(-2*time.Hour)),
			closedAt("head-e", read), "head-e", record.PullRequestClosed, lateClosed("head-e", read)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedLateIssue(t, pool, tc.phase, tc.status, tc.seed)
			apply := githubFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			apply("recorded-read", tc.recorded)
			if got := pullRequestAt(t, pool); got.head != tc.head || got.state != tc.state {
				t.Fatalf("after the recorded event the pull request is at %s, %s; want %s, %s", got.head, got.state, tc.head, tc.state)
			}
			lateChangesNothing(t, pool, apply, "late-webhook", tc.late)
		})
	}
}

// GitHub sends nothing of a pull request after its merge: it never synchronizes, closes or reopens
// a merged pull request, and sends the merge once. So a synchronize, a close or a reopen observed
// after the merge is older than it, and so is a second copy of the merge, whether the merge came
// from its webhook or from the daemon's read of the pull request; the merge carries no clock to
// fence them by (classify.LateLifecycle), and each changes nothing. A synchronize newer than the
// last lifecycle event the record applied, which the clock does not fence, would otherwise move
// the merged pull request's head and queue a disarm of its merge.
func TestALateEventOfAMergedPullRequestChangesNothing(t *testing.T) {
	merged := lateApplied
	setups := []struct {
		name  string
		merge func(t *testing.T) (*pgxpool.Pool, func(string, intake.Fact))
	}{
		{"merged early in reviewing, by the merge's webhook", func(t *testing.T) (*pgxpool.Pool, func(string, intake.Fact)) {
			pool := migratedPool(t)
			seedLateIssue(t, pool, phase.Reviewing, "needs_review", redRecord(record.PullRequestOpen, "head-c", merged.Add(-2*time.Hour)))
			apply := githubFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			apply("merged-webhook", lateMerged)
			if got, notices := pullRequestAt(t, pool), architectNotices(t, pool); got.state != record.PullRequestMerged || len(notices) != 1 || notices[0].Kind != "pr-merged" {
				t.Fatalf("after the merge the pull request is %+v with architect notices %+v, want it merged and one pr-merged", got, notices)
			}
			return pool, apply
		}},
		{"merged in merging, by the daemon's read of the merge", func(t *testing.T) (*pgxpool.Pool, func(string, intake.Fact)) {
			pool := migratedPool(t)
			now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
			seedLateIssue(t, pool, phase.Merging, "retro", redRecord(record.PullRequestOpen, "head-c", merged.Add(-2*time.Hour)),
				record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head-c"})
			seedTreeLifecycle(t, pool, "LEGION-208")
			apply := githubFacts(t, pool, New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ProductionCheckBound: 48 * time.Hour, ProductionCheckRole: claim.RoleTester, Clock: func() time.Time { return now }}, nil))
			apply("merged-read", lateMerged)
			assertPhase(t, pool, phase.Done)
			if check := productionCheck(t, pool, "LEGION-208"); check == nil || !check.Open() || check.MergeSHA != "merge" {
				t.Fatalf("after the merge the production check is %+v, want it open at merge", check)
			}
			return pool, apply
		}},
	}
	for _, setup := range setups {
		for _, late := range []struct {
			name string
			fact intake.Fact
		}{
			{"the merge's own webhook", lateMerged},
			{"a synchronize newer than the record's clock", lateSync("head-d", merged.Add(-time.Hour))},
			{"a close", lateClosed("head-d", merged.Add(-time.Hour))},
			{"a reopen", lateReopened("head-d", merged.Add(-time.Hour))},
		} {
			t.Run(setup.name+", then "+late.name, func(t *testing.T) {
				pool, apply := setup.merge(t)
				lateChangesNothing(t, pool, apply, "late-webhook", late.fact)
			})
		}
	}
}

// GitHub orders a pull request's deciding reviews by when they were submitted, then by id, and
// the pull request keeps the newest it has had (record.PullRequest.ReviewSeen), whether that one
// came by its webhook or by the daemon's read of the reviews (the daemon's readReviews, which applies
// a review its webhook did not bring as the webhook would). A deciding review submitted before it,
// delivered late, records nothing: the round keeps the newer decision.
func TestALateReviewOlderThanTheNewestChangesNothing(t *testing.T) {
	submitted := lateApplied
	webhook := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 12, SubmittedAt: submitted, State: "approved",
		CommitID: headOne, HeadSHA: headOne, Author: testReviewApp, Body: reviewBody("ship it", headOne)}
	read := webhook
	read.HeadSHA = ""
	for _, tc := range []struct {
		name   string
		newest intake.PullRequestReview
	}{
		{"after the newest review's webhook", webhook},
		{"after the daemon's read of the newest review", read},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, headOne, "green")
			apply := githubFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			apply("newest-review", tc.newest)
			if got := pullRequestAt(t, pool); got.decision != "approved" {
				t.Fatalf("after the newest review the round's decision is %q, want approved", got.decision)
			}
			lateChangesNothing(t, pool, apply, "late-review", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, SubmittedAt: submitted.Add(-time.Hour),
				State: "changes_requested", CommitID: headOne, HeadSHA: headOne, Author: testReviewApp, Body: reviewBody("rename the widget", headOne)})
		})
	}
}

// A deciding review submitted before the entry the reviewer's row stands at was raised
// (record.PhaseRow.EntryRaisedAt) is a review of an earlier round, and decides nothing of the round
// that stands, however late it is applied - a webhook redelivered late, or a message the intake
// gave up on and replayed: a person's by that time alone, and the review App's when its footer also
// names an entry other than the row's (intake.PullRequestReview.LegionEntry). It is dropped before
// anything is written, so the round records no refusal of it either, even where its footer names
// a head that is no longer the pull request's. Here round 1 (entry 1) ended undecided at the head
// it reviewed and round 2 (entry 2) stands at that same head, raised at raised.
//
// What the rule leaves deciding: the review App's review of the earlier entry whose submission
// time is after the raise, which is the reviewer's review of the round that stands after a carried
// hand-over's promotion raised its entry in the middle of that round, and is also a late review
// whose GitHub time runs ahead of the daemon's clock (decided only when its footer names the
// current head, refusesRound); a footer that names no entry; a person's review with no submission
// time; and any review submitted after the raise.
func TestAReviewOfAnEarlierRoundDecidesNoLaterRound(t *testing.T) {
	raised := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	before, after := raised.Add(-time.Hour), raised.Add(time.Hour)
	app := func(state, body string, at time.Time) intake.PullRequestReview {
		return intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, SubmittedAt: at, State: state, CommitID: headOne,
			HeadSHA: headOne, Author: testReviewApp, Body: body}
	}
	person := func(at time.Time) intake.PullRequestReview {
		return intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 12, SubmittedAt: at, State: "changes_requested", CommitID: headOne,
			Author: "a-writer", AuthorCanWrite: true, Body: "rename the widget"}
	}
	for _, tc := range []struct {
		name   string
		review intake.PullRequestReview
		// decides is the state the review decides round 2 with, "" for none.
		decides string
	}{
		{name: "a review-App approval whose footer names the earlier entry",
			review: app("approved", reviewBodyOfEntry("ship it", "ses-reviewer", headOne, 1), before)},
		{name: "a person's changes request submitted before the entry was raised", review: person(before)},
		{name: "a review-App changes request of the earlier entry whose footer names another head",
			review: app("changes_requested", reviewBodyOfEntry("rename the widget", "ses-reviewer", headTwo, 1), before)},
		{name: "a review-App approval of the earlier entry submitted after the raise",
			review: app("approved", reviewBodyOfEntry("ship it", "ses-reviewer", headOne, 1), after), decides: "approved"},
		{name: "a review-App approval whose footer names no entry",
			review: app("approved", reviewBody("ship it", headOne), before), decides: "approved"},
		{name: "a person's changes request with no submission time", review: person(time.Time{}), decides: "changes_requested"},
		{name: "a person's changes request submitted after the raise", review: person(after), decides: "changes_requested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, headOne, "green")
			// Round 2's entry, raised when the issue came back into reviewing at the same head.
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Entry: 2,
				EntryTask: "Continue root. Issue: LEGION-208. Phase: reviewing. Pull request: #42.", EntryRaisedAt: raised})
			apply := githubFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			if tc.decides == "" {
				lateChangesNothing(t, pool, apply, "late-review", tc.review)
				if row := reviewerRow(t, pool); row.Decision != nil {
					t.Fatalf("round 2's decision after the review of round 1 = %+v, want none", *row.Decision)
				}
				return
			}
			apply("review", tc.review)
			if row := reviewerRow(t, pool); row.Decision == nil || row.Decision.State != tc.decides {
				t.Fatalf("round 2's decision = %+v, want the review's %s", row.Decision, tc.decides)
			}
		})
	}
}

// A CI settlement whose runs are older, check by check, than the settlement the record holds - the
// listener's earlier settlement of the head, delivered late - is stale and changes nothing, whether
// the head stays green and the issue in testing, though the late settlement is red.
func TestALateSettlementWithLowerRunsChangesNothing(t *testing.T) {
	settlement := func(id, generation int64, snapshot string, failing ...string) intake.PullRequestChecks {
		return intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: id}},
			Generation: generation, Snapshot: snapshot, Failing: append([]string{}, failing...), Cancelled: []string{}}
	}
	for _, tc := range []struct {
		name         string
		newest, late intake.PullRequestChecks
	}{
		{"after the listener's newer settlement", settlement(5, 2, "head-2"), settlement(4, 1, "head-1", "ci")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedLateIssue(t, pool, phase.Testing, "testing", &record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", HeadSHA: "head", HeadUpdatedAt: lateApplied, Failing: []string{}})
			apply := githubFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			apply("newest-settlement", tc.newest)
			lateChangesNothing(t, pool, apply, "late-settlement", tc.late)
			assertPhase(t, pool, phase.Testing)
		})
	}
}

// A settlement of a head the fence no longer stands at starts that head's fence afresh
// (classify.SettlementFor), and the record remembers the runs of the heads it settled
// (record.PullRequest.SettledRuns): so an earlier settlement of a head, delivered late or replayed
// from a message the intake gave up on, however late, is stale against them and changes nothing
// once the fence has left that head. Here head settled (ci, run 5), and then its fence was reset
// in each of the three ways one is: the branch moved to head-2, which settled (ci, run 7), and
// came back to head; the pull request was closed and reopened at head; or a new generation of the
// issue cleared it. The settlement of head before run 5 then arrives.
func TestALateSettlementOfAHeadWhoseFenceWasResetChangesNothing(t *testing.T) {
	const dev = "legion-implementer[bot]"
	settled := func(head string, id, generation int64, failing ...string) intake.PullRequestChecks {
		return intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head, CheckRuns: []record.AttemptRun{{Name: "ci", ID: id}},
			Generation: generation, Snapshot: fmt.Sprintf("%s-%d", head, generation), Failing: append([]string{}, failing...), Cancelled: []string{}}
	}
	for _, tc := range []struct {
		name  string
		reset func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact))
	}{
		{"the branch left the head and came back", func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact)) {
			apply("pushed-head-2", sentPush("head", "head-2", dev))
			apply("moved-to-head-2", namedSync("head", "head-2", dev, lateApplied.Add(time.Minute)))
			apply("head-2-settled", settled("head-2", 7, 1))
			apply("pushed-back", sentPush("head-2", "head", dev))
			apply("moved-back", namedSync("head-2", "head", dev, lateApplied.Add(2*time.Minute)))
		}},
		{"the pull request was closed and reopened", func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact)) {
			apply("closed", closedAt("head", lateApplied.Add(time.Minute)))
			apply("reopened", readOpened("head", lateApplied.Add(2*time.Minute), true))
		}},
		{"a new generation cleared it", func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact)) {
			seedRecord(t, pool, func(tx pgx.Tx) error {
				return record.NewStore().ClearGeneration(t.Context(), tx, "LEGION-208", lateApplied.Add(time.Hour))
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedLateIssue(t, pool, phase.Testing, "testing", &record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", HeadSHA: "head", HeadUpdatedAt: lateApplied, Failing: []string{}})
			apply := githubFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			apply("head-settled", settled("head", 5, 2))
			tc.reset(t, pool, apply)
			if pr := latePullRequest(t, pool); pr.State != record.PullRequestOpen || pr.HeadSHA != "head" || pr.CheckedHead == "head" || pr.Run != nil {
				t.Fatalf("after the reset the pull request is %s at %s, its fence at %q, a head run open %v; want it open at head, its fence elsewhere and no run",
					pr.State, pr.HeadSHA, pr.CheckedHead, pr.Run != nil)
			}
			lateChangesNothing(t, pool, apply, "late-settlement", settled("head", 4, 1, "ci"))
			assertPhase(t, pool, phase.Testing)
		})
	}
}

// latePullRequest reads LEGION-208's pull request back.
func latePullRequest(t *testing.T, pool *pgxpool.Pool) record.PullRequest {
	t.Helper()
	var got *record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		got, err = record.NewStore().PullRequest(t.Context(), tx, "LEGION-208")
		return err
	})
	if got == nil {
		t.Fatal("LEGION-208 has no pull request")
	}
	return *got
}

// latePush is the push of legion/LEGION-208 from before to after by pusher, as the push webhook
// carries it and as the daemon's read rebuilds it from the branch's activity (the daemon's
// lostPushes).
func latePush(before, after, pusher string) intake.Push {
	return intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: before, After: after, Pusher: pusher}
}

// latePushDecidesNothing applies push as a late webhook under an event id of its own and fails t
// when it changed the issues, their phase rows, the production checks, the outbox, or the pull
// request's judgement, pushes or head run. The record's moves may change: a push of a move the
// record made is kept as that move's push or as a repeat of it (classify.ApplyPush), which only a
// head's next arrival by that move reads.
func latePushDecidesNothing(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact), id string, push intake.Push) {
	t.Helper()
	others := []string{"issues", "phases", "production_checks", "outbox"}
	before, prBefore := tablesText(t, pool, others...), latePullRequest(t, pool)
	apply(id, push)
	after, prAfter := tablesText(t, pool, others...), latePullRequest(t, pool)
	if after != before {
		t.Fatalf("the late push %s..%s changed the records:\nbefore %s\nafter  %s", push.Before, push.After, before, after)
	}
	if classify.JudgementChanged(prBefore, prAfter) || !slices.Equal(prAfter.Pushes, prBefore.Pushes) || (prAfter.Run == nil) != (prBefore.Run == nil) {
		t.Fatalf("the late push %s..%s changed the pull request: judgement or pushes %+v, run open %t; was %+v, run open %t",
			push.Before, push.After, prAfter.Pushes, prAfter.Run != nil, prBefore.Pushes, prBefore.Run != nil)
	}
}

// The review App pushes the tester's red tests twice onto the implementer's red code, and the
// daemon's read rebuilds both pushes from the branch's activity and applies the head it read, so
// the head is judged by its chain of pushes. The push webhooks arriving late decide nothing - the
// push the chain began from, whose move the record made before the read, and a copy of each push
// the read rebuilt: the judgement, the pushes waiting for a link, the phase and the outbox stand,
// and the record keeps each as its move's push or a repeat of it.
func TestALatePushAfterTheReadRebuiltItsChainDecidesNothing(t *testing.T) {
	const implementer = "legion-implementer[bot]"
	pr := redRecord(record.PullRequestOpen, "code", lateApplied.Add(-2*time.Hour))
	pr.Moves = []record.Move{{Before: "base", SHA: "code"}}
	pool := migratedPool(t)
	seedLateIssue(t, pool, phase.Testing, "testing", pr)
	apply := githubFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	tests := latePush("code", "red-tests", testReviewApp)
	more := latePush("red-tests", "more-red-tests", testReviewApp)
	apply("push-read:code...red-tests", tests)
	apply("push-read:red-tests...more-red-tests", more)
	apply("head-read:more-red-tests", readHead("more-red-tests", lateApplied))
	if got := latePullRequest(t, pool); got.HeadSHA != "more-red-tests" || got.Run != nil {
		t.Fatalf("after the read the record is at %q, run %+v; want more-red-tests judged by its pushes, with no head run open", got.HeadSHA, got.Run)
	}
	for _, late := range []struct {
		id   string
		push intake.Push
	}{
		{"the push the chain began from", latePush("base", "code", implementer)},
		{"the red tests' webhook", tests},
		{"the second push of red tests' webhook", more},
	} {
		latePushDecidesNothing(t, pool, apply, late.id, late.push)
	}
	assertPhase(t, pool, phase.Testing)
}
