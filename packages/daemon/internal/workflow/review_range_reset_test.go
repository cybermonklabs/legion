package workflow

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
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
)

// clockReads sets engine's clock to at for every fact it applies from here on. The test engine's
// clock reads one moment (testEngine), where each fact of a live tree comes at a moment of its own,
// and an entry is raised at the moment its start reads (record.PhaseRow.EntryRaisedAt, which enter
// writes from this clock). The review rule compares no such moment: a review moves the commit the
// reviewer read by the entry its footer names.
func clockReads(engine *Engine, at time.Time) {
	engine.cfg.Clock = func() time.Time { return at }
}

// clearsTheGeneration is a new run's clear of LEGION-208's generation at at: the engine's clock
// reads at from here on, and the issue's generation is cleared (record.Store.ClearGeneration), as
// the new run clears it (Engine.NewRun).
func clearsTheGeneration(t *testing.T, pool *pgxpool.Pool, engine *Engine, at time.Time) {
	t.Helper()
	clockReads(engine, at)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().ClearGeneration(ctx, tx, "LEGION-208", at)
	})
}

// promotesTheReviewer is the promotion's start of LEGION-208's reviewer, mid-phase, at at
// (StartPromotedWorker, the start a carried root's promotion makes), which raises the reviewer's
// entry at that moment.
func promotesTheReviewer(t *testing.T, pool *pgxpool.Pool, engine *Engine, at time.Time) {
	t.Helper()
	clockReads(engine, at)
	member := issueRecord(t, pool, "LEGION-208")
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.StartPromotedWorker(ctx, tx, *member, claim.RoleReviewer)
	})
}

// A review with no submission time moves the commit when it names the entry that stands: no time
// is compared, so nothing about it holds the commit back. It still decides the round, as an
// untimed review always has. The same untimed review naming no entry decides the round and moves
// nothing, which is the control.
func TestAReviewWithNoSubmissionTimeMovesTheCommitWhenItNamesItsEntry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// namesEntry is whether the review's footer names the entry the row holds.
		namesEntry bool
		want       string
	}{
		{name: "no submission time, the entry that stands", namesEntry: true, want: headOne},
		{name: "no submission time, no entry", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _, apply := reviewRoundOne(t)
			review := reviewAt(t, pool, 1, "changes_requested", headOne, time.Time{})
			if !tc.namesEntry {
				review.Body = reviewBody("the review", headOne)
			}
			apply("request-changes", review)
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("after the request for changes the issue is in %s, want implementing: it decides the round", got)
			}
			if got := reviewerRow(t, pool).ReviewedHead; got != tc.want {
				t.Fatalf("after the untimed request for changes the reviewer's row reads the reviewed commit %q, want %q", got, tc.want)
			}
		})
	}
}

// A carried run keeps its rows (its promotion clears nothing, workflow's StartCarriedRun), and its
// promotion raises the reviewer's entry in the middle of the round that stands. A review that
// names the entry before the promotion moves no commit, whatever time GitHub gives it: the commit
// stays where the round before left it, and the next round reads more, which is the safe side. A
// review that names the promotion's entry records its head, though GitHub times it before the
// daemon raised that entry. Two promotions in a row write the same task text before the sentence
// that names each entry, and a review of the first, applied after the second, moves nothing:
// only the number tells the two entries apart. Round 2 here stands at headTwo, and the commit
// round 1 read is headOne.
func TestACarriedReviewersReviewMovesTheCommitOnlyWhenItNamesThePromotionsEntry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// names is the entry the review's footer names: "before" the promotion, the "promoted"
		// one, or the "first" of two promotions.
		names     string
		submitted time.Duration
		want      string
	}{
		{name: "a review of the entry before the promotion, timed after it", names: "before", submitted: 4 * time.Hour, want: headOne},
		{name: "a review of the promotion's entry, timed before it", names: "promoted", submitted: 2 * time.Hour, want: headTwo},
		{name: "a review of the first of two promotions whose task text is the same", names: "first", submitted: 5 * time.Hour, want: headOne},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, engine, apply := reviewRoundOne(t)
			entered := engine.now()
			apply("request-changes-1", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
			mergesForward(t, pool, engine, headOne, headTwo, reviewTestedAt.Add(time.Hour))
			backToReviewing(t, apply, 2)
			before := reviewerRow(t, pool)
			if before.ReviewedHead != headOne {
				t.Fatalf("round 2's reviewer row reads the reviewed commit %q, want %s", before.ReviewedHead, headOne)
			}
			// The carried orphan's re-admission raises its generation and keeps its phase and its
			// rows, as the subtest "a carried hand-over's promotion" of
			// TestTheReviewedCommitSurvivesEachWayBackIntoReviewing stands it in.
			carried := issueRecord(t, pool, "LEGION-208")
			carried.Generation++
			seedIssue(t, pool, *carried)
			promotesTheReviewer(t, pool, engine, entered.Add(3*time.Hour))
			first := reviewerRow(t, pool)
			entry := map[string]int64{"before": before.Entry, "promoted": first.Entry, "first": first.Entry}[tc.names]
			if tc.names == "first" {
				promotesTheReviewer(t, pool, engine, entered.Add(4*time.Hour))
				second := reviewerRow(t, pool)
				if strings.TrimSuffix(first.EntryTask, reviewEntryPartOf(first.Entry)) != strings.TrimSuffix(second.EntryTask, reviewEntryPartOf(second.Entry)) ||
					second.Entry != first.Entry+1 {
					t.Fatalf("the two promotions wrote the tasks %q at entry %d and %q at entry %d, want one text before the sentence that names each entry",
						first.EntryTask, first.Entry, second.EntryTask, second.Entry)
				}
			}
			review := reviewAt(t, pool, 2, "changes_requested", headTwo, entered.Add(tc.submitted))
			review.Body = reviewBodyOfEntry("the review", "ses-reviewer", headTwo, entry)
			apply("request-changes-2", review)
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("after the carried reviewer's request for changes the issue is in %s, want implementing: the review decides the round", got)
			}
			if got := reviewerRow(t, pool).ReviewedHead; got != tc.want {
				t.Fatalf("after the review of entry %d the reviewer's row reads the reviewed commit %q, want %q", entry, got, tc.want)
			}
		})
	}
}

// Only the reviewer's own deciding review that names the entry that stands moves the commit, in
// either deciding state, and each review of the reviewer's own that decides the round and moves
// nothing says why in one log line: the row holds no entry's task, the footer names no entry, or
// it names another entry than the row's. Round 2 here stands at headTwo, the commit round 1 read
// is headOne, and each review is at headTwo. Round 1's entry is an older entry of the same issue
// generation. Another review-App session's review of the entry that stands decides nothing and
// moves nothing, and logs no such line, since it is not the reviewer's. Each review is the review
// App's as production delivers it: the daemon never reads write access for the review App's
// login (daemon's reviewerCanWrite), so AuthorCanWrite is false.
func TestOnlyAReviewOfTheEntryThatStandsMovesTheCommit(t *testing.T) {
	t.Parallel()
	const why = "workflow: the reviewer's own review moves no reviewed commit: "
	for _, tc := range []struct {
		name  string
		state string
		// session is the session the footer names; entry is "standing", "first", or "none".
		session, entry string
		// taskless takes the entry's task off the reviewer's row before the review.
		taskless bool
		// decides is whether the review decides round 2.
		decides bool
		want    string
		logged  func(first, standing int64) string
	}{
		{name: "an approval of the entry that stands", state: "approved", session: "ses-reviewer", entry: "standing", decides: true, want: headTwo},
		{name: "a request for changes of the entry that stands", state: "changes_requested", session: "ses-reviewer", entry: "standing", decides: true, want: headTwo},
		{name: "a footer that names no entry", state: "changes_requested", session: "ses-reviewer", entry: "none", decides: true, want: headOne,
			logged: func(int64, int64) string { return why + "its footer names no entry" }},
		{name: "an older entry of the same generation", state: "changes_requested", session: "ses-reviewer", entry: "first", decides: true, want: headOne,
			logged: func(first, standing int64) string {
				return fmt.Sprintf(why+"its footer names entry %d and the reviewer's row stands at entry %d", first, standing)
			}},
		{name: "a row that holds no entry's task", state: "changes_requested", session: "ses-reviewer", entry: "standing", taskless: true, decides: true, want: headOne,
			logged: func(int64, int64) string { return why + "the reviewer's row holds no entry's task" }},
		{name: "another session's review of the entry that stands", state: "changes_requested", session: "ses-controller", entry: "standing", want: headOne},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, headOne, "green")
			standsIn(t, pool, phase.Testing, "testing")
			var logs bytes.Buffer
			engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logs, nil)))
			apply := applyFacts(t, pool, engine)
			apply("round-one-tester", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim",
				Summary: "tested", Verdict: "pass", HandoffRecord: "test-1", HandoffVersion: 1})
			first := reviewerRow(t, pool).Entry
			apply("request-changes-1", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
			mergesForward(t, pool, engine, headOne, headTwo, reviewTestedAt.Add(time.Hour))
			backToReviewing(t, apply, 2)
			standing := reviewerRow(t, pool)
			if standing.ReviewedHead != headOne || standing.Entry <= first {
				t.Fatalf("round 2's reviewer row reads the reviewed commit %q on entry %d, want %s on an entry above round 1's %d", standing.ReviewedHead, standing.Entry, headOne, first)
			}
			if tc.taskless {
				if _, err := pool.Exec(context.Background(), "update phases set entry_task = '' where issue = 'LEGION-208' and role = 'reviewer'"); err != nil {
					t.Fatalf("take the task off the reviewer's row: %v", err)
				}
			}
			body := roundReviewBody(t, pool, tc.session, "the review", headTwo)
			switch tc.entry {
			case "none":
				body = reviewBody("the review", headTwo)
			case "first":
				body = reviewBodyOfEntry("the review", tc.session, headTwo, first)
			}
			logs.Reset()
			apply("review-2", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 2, State: tc.state, CommitID: headTwo,
				Author: testReviewApp, Body: body, SubmittedAt: reviewTestedAt.Add(2 * time.Hour)})
			row := reviewerRow(t, pool)
			switch {
			case tc.decides && (row.Decision == nil || row.Decision.State != tc.state || row.Decision.Head != headTwo):
				t.Fatalf("round 2's decision = %+v, want the review's %s at %s: the entry it names does not change who decides the round", row.Decision, tc.state, headTwo)
			case !tc.decides && (row.Decision != nil || issuePhase(t, pool) != phase.Reviewing):
				t.Fatalf("round 2's decision = %+v in %s, want none in reviewing: a review-App review of another session decides nothing", row.Decision, issuePhase(t, pool))
			}
			if got := reviewerRow(t, pool).ReviewedHead; got != tc.want {
				t.Fatalf("the reviewer's row reads the reviewed commit %q, want %q", got, tc.want)
			}
			lines := strings.Count(logs.String(), why)
			if tc.logged == nil {
				if lines != 0 {
					t.Fatalf("logs = %s, want no line that the review moves no reviewed commit", logs.String())
				}
				return
			}
			if want := tc.logged(first, standing.Entry); lines != 1 || !strings.Contains(logs.String(), want) {
				t.Fatalf("logs = %s, want the one line %q", logs.String(), want)
			}
		})
	}
}
