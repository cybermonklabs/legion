package workflow

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
)

// reviewEntryPartOf is the sentence a reviewer's task in reviewing names the entry of its round
// with, " Review entry: <n>.", written out here rather than through reviewround.Sentences, so the
// tests read the sentence and not the code that writes it.
func reviewEntryPartOf(entry int64) string {
	return " " + reviewround.EntryLabel + ": " + strconv.FormatInt(entry, 10) + "."
}

// entryNamedIn is the entry the reviewer's task names in its last sentence, read as an agent reads
// it: the number after reviewround.EntryLabel, failing the test when the task does not end naming one.
func entryNamedIn(t *testing.T, task string) int64 {
	t.Helper()
	label := " " + reviewround.EntryLabel + ": "
	at := strings.LastIndex(task, label)
	if at < 0 || !strings.HasSuffix(task, ".") {
		t.Fatalf("the task %q does not end naming its review entry", task)
	}
	entry, err := strconv.ParseInt(strings.TrimSuffix(task[at+len(label):], "."), 10, 64)
	if err != nil {
		t.Fatalf("the task %q does not end naming its review entry: %v", task, err)
	}
	return entry
}

// deliveredTask is the task of the one start of LEGION-208's reviewer the outbox holds, the task
// the reviewer is DELIVERED, failing the test unless that start hands the reviewer the entry the
// reviewer's row holds (or, for a resume level with its entry, no number at all), and unless its
// row, read back from the outbox, carries the typed fact that its task holds the daemon's two
// sentences (record.SuperviseRequest.RoundNamed), so the claim's send adds none.
func deliveredTask(t *testing.T, pool *pgxpool.Pool, path string) string {
	t.Helper()
	start, row := onlyReviewerStart(t, pool), reviewerRow(t, pool)
	if start.Entry != 0 && start.Entry != row.Entry {
		t.Fatalf("%s: the start hands the reviewer entry %d, want the row's %d", path, start.Entry, row.Entry)
	}
	if !start.RoundNamed {
		t.Fatalf("%s: the reviewer's start row with the task %q carries no fact that it names its round", path, start.Task)
	}
	return start.Task
}

// Each task the reviewer is given in reviewing ends with exactly two sentences of the daemon's, its
// last two: the commit its row holds, ` Last reviewed commit: <sha>.`, or ` Last reviewed commit:
// none.` for a row that holds none, then the entry of its round, ` Review entry: <n>.`, the number
// the reviewer's row holds at that moment (record.PhaseRow.Entry) while the row holds the task of
// an entry, or ` Review entry: none.` for a row that holds no entry's task. The reviewer writes the
// entry into the footer of each review it submits for that task, and only a review that names the
// entry that stands moves the commit the reviewer read (review). One function gives the reviewer's
// task those sentences (namingTheReviewRound), and the one place every task for a worker is queued
// through (queuedRow) calls it, so each way the daemon gives the reviewer a task reaches it,
// whichever composed the task: the cases here are each way there is today, a wake of each kind
// among them, each run for a row that holds a commit and for one that holds none wherever
// production reaches both. A start that raises the entry stores the text it sends (enter), so the
// row's stored task and the delivered one are one text, and the delivery of an owed entry sends
// that text again with each sentence once.
//
// The resume task made because the stored text is empty (a clear emptied it, and no entry was
// raised since) names no entry, `none`, since no review of that entry moves the commit. A task of
// any other of Legion's roles names neither (TestEachWakeOfTheReviewerInReviewingEndsNamingItsEntry).
// A review step of a role of the deployment names its commit alone (namingTheReviewRound).
func TestEachTaskOfTheReviewerInReviewingEndsNamingItsEntry(t *testing.T) {
	t.Parallel()
	const withCommit, withNone = "a row with a commit", "a row with none"
	both := []string{withCommit, withNone}
	resume := "Continue root. Issue: LEGION-208. Phase: reviewing. Working copy: branch legion/LEGION-208. Resume the existing phase work."
	for _, tc := range []struct {
		name string
		// rows is each kind of row the path is run for: one that holds a commit, one that holds
		// none, or both.
		rows []string
		// walk takes LEGION-208 to the path's start, its reviewer's row holding no commit where
		// noCommit says so, with only that start in the outbox, and returns the pool and what the
		// start's task, as stored and as delivered, must read before the daemon's two sentences.
		walk func(t *testing.T, noCommit bool) (*pgxpool.Pool, string)
		// raises is whether the path raises the entry and stores the text it sends (enter), so the
		// row's stored task is the delivered one word for word.
		raises bool
		// noEntry is whether the row holds no entry's task, so the task names the entry `none`.
		noEntry bool
	}{
		{
			name: "a transition's start of the first round", rows: []string{withNone}, raises: true,
			walk: func(t *testing.T, _ bool) (*pgxpool.Pool, string) {
				pool, _, _ := reviewRoundOne(t)
				return pool, "Continue root. Issue: LEGION-208. Phase: reviewing. Working copy: branch legion/LEGION-208. Handoff: test version 1 (dispatch handoff-read --issue LEGION-208 --phase test --version 1)." +
					" Tester's verdict: pass. Pull request: #42."
			},
		},
		{
			name: "a transition's start of a later round", rows: both, raises: true,
			walk: func(t *testing.T, noCommit bool) (*pgxpool.Pool, string) {
				pool, _, _ := reviewRoundTwoNaming(t, noCommit)
				for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
					if task := roleRow(t, pool, role).EntryTask; task == "" || strings.Contains(task, reviewround.EntryLabel) || strings.Contains(task, reviewround.CommitLabel) {
						t.Fatalf("the %s's row holds the task %q, want round 2's task with no sentence of the review round: only the reviewer and a review step of a role of the deployment are told one", role, task)
					}
				}
				return pool, "Continue root. Issue: LEGION-208. Phase: reviewing. Working copy: branch legion/LEGION-208. Handoff: test version 2 (dispatch handoff-read --issue LEGION-208 --phase test --version 2)." +
					" Tester's verdict: pass. Pull request: #42."
			},
		},
		{
			name: "the promotion's start", rows: both, raises: true,
			walk: func(t *testing.T, noCommit bool) (*pgxpool.Pool, string) {
				if noCommit {
					// A re-admission clears the generation, the commit with it.
					pool, engine, _ := reviewedChild(t)
					readmitsTheTree(t, pool, engine, engine.now().Add(time.Hour))
					outboxRan(t, pool)
					promotesTheReviewer(t, pool, engine, engine.now().Add(time.Hour))
					return pool, "Continue child. Issue: LEGION-208. Phase: reviewing. Working copy: branch legion/LEGION-208. Resume the existing phase work."
				}
				// A carried hand-over keeps its rows and the commit with them (admit's readmit
				// returns before the clear), and its promotion raises the reviewer's entry.
				pool, engine, _ := reviewRoundTwo(t)
				outboxRan(t, pool)
				carried := issueRecord(t, pool, "LEGION-208")
				carried.Generation++
				seedIssue(t, pool, *carried)
				applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
					_, err := engine.StartPromotedWorker(ctx, tx, *carried, claim.RoleReviewer, false)
					return err
				})
				return pool, resume
			},
		},
		{
			name: "the delivery of an owed entry", rows: both, raises: true,
			walk: func(t *testing.T, noCommit bool) (*pgxpool.Pool, string) {
				pool, engine, _ := reviewRoundTwoNaming(t, noCommit)
				row, issue := reviewerRow(t, pool), issueRecord(t, pool, "LEGION-208")
				outboxRan(t, pool)
				applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
					return engine.SendOwedEntry(ctx, tx, *issue, claim.RoleReviewer, reviewClaim, row)
				})
				return pool, strings.TrimSuffix(row.EntryTask, reviewedPartOf(commitNamed(row))+reviewEntryPartOf(row.Entry))
			},
		},
		{
			name: "the retry of a held reviewing phase", rows: both,
			walk: func(t *testing.T, noCommit bool) (*pgxpool.Pool, string) {
				pool, _, apply := reviewRoundTwoNaming(t, noCommit)
				held := issueRecord(t, pool, "LEGION-208")
				held.Phase, held.Hold = phase.Held, &record.Hold{From: phase.Reviewing}
				seedIssue(t, pool, *held)
				outboxRan(t, pool)
				apply("retry", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision, Reason: "the claim failed"})
				return pool, "Continue root. Issue: LEGION-208. Phase: reviewing. Working copy: branch legion/LEGION-208. Pull request: #42. Reason: the claim failed."
			},
		},
		{
			name: "a ready worker's catch-up", rows: both,
			walk: func(t *testing.T, noCommit bool) (*pgxpool.Pool, string) {
				pool, engine, _ := reviewRoundTwoNaming(t, noCommit)
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
				return pool, resume
			},
		},
		{
			name: "the silence repair", rows: both,
			walk: func(t *testing.T, noCommit bool) (*pgxpool.Pool, string) {
				pool, engine, _ := reviewRoundTwoNaming(t, noCommit)
				row, issue := reviewerRow(t, pool), issueRecord(t, pool, "LEGION-208")
				outboxRan(t, pool)
				applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
					return engine.repairSilence(ctx, tx, *issue, *issue, reviewClaim,
						record.RoleRun{Claim: &record.RoleClaim{GivenEntry: row.Entry}}, row,
						intake.WorkerSilent{Issue: "LEGION-208", Role: claim.RoleReviewer, Generation: 1, LastStep: reviewTestedAt, Bound: time.Hour},
						reviewTestedAt.Add(2*time.Hour))
				})
				return pool, resume
			},
		},
		{
			name: "the boot's resume", rows: both,
			walk: func(t *testing.T, noCommit bool) (*pgxpool.Pool, string) {
				pool, engine, _ := reviewRoundTwoNaming(t, noCommit)
				bootResumesTheReviewer(t, pool, engine)
				return pool, resume
			},
		},
		{name: "a wake on a CI settlement", rows: both, walk: wakesTheReviewerInRoundTwoNaming(pods.WakeChecks)},
		{name: "a wake on a role message", rows: both, walk: wakesTheReviewerInRoundTwoNaming(pods.WakeRoleMessage)},
		{name: "a wake on an agent message", rows: both, walk: wakesTheReviewerInRoundTwoNaming(pods.WakeAgentMessage)},
		{name: "a wake on the backstop", rows: both, walk: wakesTheReviewerInRoundTwoNaming(pods.WakeBackstop)},
		{name: "a wake of any other kind", rows: both, walk: wakesTheReviewerInRoundTwoNaming(aKindStillToCome)},
		{
			name: "the resume of an entry whose stored task is empty", rows: []string{withNone}, noEntry: true,
			walk: func(t *testing.T, _ bool) (*pgxpool.Pool, string) {
				pool, engine, _ := reviewedChild(t)
				readmitsTheTree(t, pool, engine, engine.now().Add(time.Hour))
				row, issue := reviewerRow(t, pool), issueRecord(t, pool, "LEGION-208")
				if row.EntryTask != "" {
					t.Fatalf("after the re-admission the reviewer's row stores the task %q, want none", row.EntryTask)
				}
				outboxRan(t, pool)
				applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
					return engine.SendOwedEntry(ctx, tx, *issue, claim.RoleReviewer, reviewClaim, row)
				})
				return pool, "Continue child. Issue: LEGION-208. Phase: reviewing. Working copy: branch legion/LEGION-208. Resume the existing phase work."
			},
		},
	} {
		for _, kind := range tc.rows {
			t.Run(tc.name+", "+kind, func(t *testing.T) {
				pool, before := tc.walk(t, kind == withNone)
				row := reviewerRow(t, pool)
				if (row.ReviewedHead == "") != (kind == withNone) {
					t.Fatalf("the reviewer's row holds the commit %q, want %s", row.ReviewedHead, kind)
				}
				stored := row.EntryTask
				delivered := deliveredTask(t, pool, tc.name)
				entry := reviewEntryPartOf(row.Entry)
				if tc.noEntry {
					entry = noEntryPart
				}
				if want := before + reviewedPartOf(commitNamed(row)) + entry; delivered != want {
					t.Fatalf("the reviewer is delivered the task %q, want %q: it ends with the daemon's two sentences, the row's commit and then its entry", delivered, want)
				}
				for _, label := range []string{reviewround.CommitLabel, reviewround.EntryLabel} {
					if named := strings.Count(delivered, " "+label+": "); named != 1 {
						t.Fatalf("the reviewer's task %q names %q %d times, want once: the daemon's own sentence", delivered, label, named)
					}
				}
				if tc.raises && stored != delivered {
					t.Fatalf("the reviewer's row stores the task %q, and the reviewer is delivered %q: the two must be one text", stored, delivered)
				}
				if !tc.raises && !tc.noEntry && !strings.HasSuffix(stored, reviewEntryPartOf(row.Entry)) {
					t.Fatalf("the reviewer's row stores the task %q, want the task its entry %d was raised with, which names it", stored, row.Entry)
				}
			})
		}
	}
}

// noEntryPart is the sentence a reviewer's task in reviewing ends with while its row holds no
// entry's task: the daemon gives that task no entry, and no review of it moves the commit.
const noEntryPart = " " + reviewround.EntryLabel + ": none."

// commitNamed is the commit row's sentence names, the commit it holds or `none`.
func commitNamed(row record.PhaseRow) string {
	if row.ReviewedHead == "" {
		return "none"
	}
	return row.ReviewedHead
}

// reviewRoundTwoNaming is LEGION-208 in its second review round, after the reviewer requested
// changes at headOne in its first, its reviewer's row holding headOne, or no commit where noCommit
// says so: a round whose deciding review recorded none, as a person's review or the reviewer's own
// with no entry leaves it, stands in for that by taking the commit off the row before round 2
// enters.
func reviewRoundTwoNaming(t *testing.T, noCommit bool) (*pgxpool.Pool, *Engine, func(string, intake.Fact) intake.Result) {
	t.Helper()
	if !noCommit {
		return reviewRoundTwo(t)
	}
	pool, engine, apply := reviewRoundOne(t)
	apply("request-changes", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
	if _, err := pool.Exec(context.Background(), "update phases set reviewed_head = '' where issue = 'LEGION-208' and role = 'reviewer'"); err != nil {
		t.Fatalf("take the commit off the reviewer's row: %v", err)
	}
	backToReviewing(t, apply, 2)
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the tester passed round 2 the issue is in %s, want reviewing", got)
	}
	return pool, engine, apply
}

// wakesTheReviewerInRoundTwoNaming is wakesTheReviewerInRoundTwo on reviewRoundTwoNaming's round.
func wakesTheReviewerInRoundTwoNaming(kind string) func(t *testing.T, noCommit bool) (*pgxpool.Pool, string) {
	return func(t *testing.T, noCommit bool) (*pgxpool.Pool, string) {
		pool, engine, _ := reviewRoundTwoNaming(t, noCommit)
		outboxRan(t, pool)
		releasesTheReviewer(t, pool, engine)
		return pool, wakes(t, pool, engine, claim.RoleReviewer, "ses-reviewer", headOne, kind)
	}
}

// The stored task of an entry (enter) is the text the reviewer was sent, word for word, and the
// delivery of that entry owed again (SendOwedEntry) sends it as stored, though the reviewer's row
// holds another commit since: each sentence of the round once, and the commit the task named when
// it was stored, a commit or `none`. The send says the task is the stored one
// (record.SuperviseRequest.RoundNamed), and the queue (queuedRow, namingTheReviewRound) adds
// nothing to it. Each case moves the row's commit by hand, to a commit the stored task does not
// name.
func TestAnOwedEntryIsSentAsStoredAfterTheRowsReviewedCommitMoved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// walk takes LEGION-208 to a round of the reviewer, its entry raised and its task stored.
		walk func(t *testing.T) (*pgxpool.Pool, *Engine)
		// names is the commit the stored task names, "" for none; moved is the commit the row is
		// moved to.
		names, moved string
	}{
		{name: "a stored task that names no commit", moved: headOne, walk: func(t *testing.T) (*pgxpool.Pool, *Engine) {
			pool, engine, _ := reviewRoundOne(t)
			return pool, engine
		}},
		{name: "a stored task that names a commit", names: headOne, moved: headTwo, walk: func(t *testing.T) (*pgxpool.Pool, *Engine) {
			pool, engine, _ := reviewRoundTwo(t)
			return pool, engine
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, engine := tc.walk(t)
			stored := reviewerRow(t, pool).EntryTask
			commit := reviewedPartOf("none")
			if tc.names != "" {
				commit = reviewedPartOf(tc.names)
			}
			if entry := reviewerRow(t, pool).Entry; !strings.HasSuffix(stored, " Pull request: #42."+commit+reviewEntryPartOf(entry)) ||
				strings.Count(stored, reviewround.CommitLabel) != 1 {
				t.Fatalf("the reviewer's row stores the task %q, want it to end with the pull request, then %q, then entry %d", stored, commit, entry)
			}
			if _, err := pool.Exec(context.Background(), "update phases set reviewed_head = $1 where issue = 'LEGION-208' and role = 'reviewer'", tc.moved); err != nil {
				t.Fatalf("move the reviewer's commit: %v", err)
			}
			row, issue := reviewerRow(t, pool), issueRecord(t, pool, "LEGION-208")
			if row.ReviewedHead != tc.moved || row.EntryTask != stored || issue.Phase != phase.Reviewing {
				t.Fatalf("the reviewer's row holds the commit %q and the task %q in %s, want %s, the task as stored, and reviewing",
					row.ReviewedHead, row.EntryTask, issue.Phase, tc.moved)
			}
			outboxRan(t, pool)
			applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
				return engine.SendOwedEntry(ctx, tx, *issue, claim.RoleReviewer, reviewClaim, row)
			})
			if delivered := deliveredTask(t, pool, tc.name); delivered != stored {
				t.Fatalf("the owed entry is delivered as %q, want the stored task %q word for word: the row's commit moved to %s since", delivered, stored, tc.moved)
			}
		})
	}
}

// A task whose own text holds the words of a round's sentence is still given the round's sentences
// by the daemon, after its text, so the daemon's two sentences are always the task's last two:
// whether a task already holds them is the fact a start carries
// (record.SuperviseRequest.RoundNamed), never read from the task's text, and the commit's sentence
// is written for a row that holds no commit too, naming `none`. The text here is a held reviewing
// phase's retry whose reason, as an architect writes it, quotes the entry the reviewer's row stands
// at, so the task ends with the words ` Review entry: <n>.`, or ends quoting another commit than
// the row's. Round 2's row holds headOne, the commit round 1's review read; round 1's holds none,
// so a reason that ends with a commit's sentence there stands just before the daemon's `none`.
func TestATaskWhoseTextQuotesTheRoundsSentencesIsStillGivenThem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// firstRound is whether the retry is of the reviewer's first round, whose row holds no
		// commit; else it is of round 2, whose row holds headOne.
		firstRound bool
		// reason is the retry's reason, given the entry the reviewer's row stands at.
		reason func(entry int64) string
	}{
		{name: "a reason that ends quoting the row's entry", reason: func(entry int64) string {
			return fmt.Sprintf("Retry %s: %d", reviewround.EntryLabel, entry)
		}},
		{name: "a reason that quotes another commit", reason: func(int64) string {
			return "the reviewer read " + reviewround.CommitLabel + ": " + headTwo + ". Read it again"
		}},
		{name: "a first round's reason that ends quoting a commit", firstRound: true, reason: func(int64) string {
			return "the reviewer read " + reviewround.CommitLabel + ": " + headTwo
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			walk, commit, holds := reviewRoundTwo, headOne, headOne
			if tc.firstRound {
				walk, commit, holds = reviewRoundOne, "none", ""
			}
			pool, _, apply := walk(t)
			row := reviewerRow(t, pool)
			if row.ReviewedHead != holds || row.EntryTask == "" {
				t.Fatalf("the reviewer's row holds the commit %q and the task %q, want %q and its entry's task", row.ReviewedHead, row.EntryTask, holds)
			}
			held := issueRecord(t, pool, "LEGION-208")
			held.Phase, held.Hold = phase.Held, &record.Hold{From: phase.Reviewing}
			seedIssue(t, pool, *held)
			outboxRan(t, pool)
			reason := tc.reason(row.Entry)
			apply("retry", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision, Reason: reason})
			before := "Continue root. Issue: LEGION-208. Phase: reviewing. Working copy: branch legion/LEGION-208. Pull request: #42. Reason: " + reason + "."
			want := before + reviewedPartOf(commit) + reviewEntryPartOf(row.Entry)
			if delivered := deliveredTask(t, pool, tc.name); delivered != want {
				t.Fatalf("the retry is delivered as %q, want %q: the task's text, then the daemon's sentence naming %s, then its sentence naming entry %d",
					delivered, want, commit, row.Entry)
			}
		})
	}
}

// bootResumesTheReviewer is the boot's resume of LEGION-208's reviewer, a claim the daemon
// suspended level with the entry its row holds (startSuspendedOwner), with only that start in the
// outbox: the reviewer relaunched in the middle of its round.
func bootResumesTheReviewer(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	row, issue := reviewerRow(t, pool), issueRecord(t, pool, "LEGION-208")
	outboxRan(t, pool)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		_, err := engine.startSuspendedOwner(ctx, tx, suspendedOwner{issue: *issue, root: *issue, role: claim.RoleReviewer,
			run: record.RoleRun{Claim: &record.RoleClaim{GivenEntry: row.Entry}}}, row)
		return err
	})
}

// A reviewer relaunched in the middle of its round is given the boot's resume task, which names
// the entry its row holds, and its review that names that entry records the head it read: the
// entry a resume names is the entry that stands. Round 2 here stands at headTwo, and round 1's
// review read headOne. The number is read from the task the reviewer was delivered, as the agent
// reads it.
func TestAReviewOfTheEntryTheBootsResumeNamesRecordsItsHead(t *testing.T) {
	t.Parallel()
	pool, engine, apply := reviewRoundOne(t)
	apply("request-changes-1", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
	mergesForward(t, pool, engine, headOne, headTwo, reviewTestedAt.Add(time.Hour))
	backToReviewing(t, apply, 2)
	if got := reviewerRow(t, pool).ReviewedHead; got != headOne {
		t.Fatalf("round 2's reviewer row reads the reviewed commit %q, want %s", got, headOne)
	}
	bootResumesTheReviewer(t, pool, engine)
	entry := entryNamedIn(t, deliveredTask(t, pool, "the boot's resume"))
	review := reviewAt(t, pool, 2, "changes_requested", headTwo, reviewTestedAt.Add(2*time.Hour))
	review.Body = reviewBodyOfEntry("the review", "ses-reviewer", headTwo, entry)
	apply("request-changes-2", review)
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("after the resumed reviewer's request for changes the issue is in %s, want implementing", got)
	}
	if got := reviewerRow(t, pool).ReviewedHead; got != headTwo {
		t.Fatalf("after the resumed reviewer's review of entry %d the reviewer's row reads the reviewed commit %q, want %s, the head that review read", entry, got, headTwo)
	}
}

// aKindStillToCome is a wake no fact reports today: WakeTask's default, which the one act of every
// wake (wakeReleased) gives any kind it is handed. A start names only a kind of the pods wake kinds
// (record.SuperviseRequest's WakeKind), so the delivery kind, which no workflow wake takes and
// WakeTask names no sentence of its own for, stands in for a kind still to come.
const aKindStillToCome = pods.WakeDelivery

// wakeKinds is each kind of wake that brings a released role back (wake.go): the CI settlement,
// the role message, the agent message, the backstop, and any other.
var wakeKinds = []string{pods.WakeChecks, pods.WakeRoleMessage, pods.WakeAgentMessage, pods.WakeBackstop, aKindStillToCome}

// releasesTheReviewer leaves LEGION-208's reviewer claim as an idle release leaves a pod's claims
// (seedReleasedClaim): suspended with the source `released`, serving the issue's generation, under
// the project as a claim token spells it, and it hands engine the project's pod records, so a wake
// of the reviewer is a wake of the claim the fixtures record the reviewer's session on.
func releasesTheReviewer(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	engine.cfg.Pods = wakePods(t, pool)
	tag, err := pool.Exec(context.Background(), `update claims set project = $2, state = 'suspended', suspended_by = 'released',
		serving_generation = $3 where token = $1`, string(reviewClaim), wakeProjectToken(t), int64(issueRecord(t, pool, "LEGION-208").Generation))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("release the reviewer's claim: %d rows, %v", tag.RowsAffected(), err)
	}
}

// wakes brings LEGION-208's released role back by a wake of kind and answers the task that wake
// gives it (WakeTask): a CI settlement of the pull request at head, a message published to the
// role's topic, a message sent to session, the backstop running out, and any other kind through
// the one act every wake performs (wakeReleased).
func wakes(t *testing.T, pool *pgxpool.Pool, engine *Engine, role claim.Role, session, head, kind string) string {
	t.Helper()
	apply, token := applyFacts(t, pool, engine), string(wakeToken(t, role))
	switch kind {
	case pods.WakeChecks:
		fact := settlement(head)
		apply("wake-checks", fact)
		return WakeTask(kind, settlementDetail(fact), StoppedByRelease)
	case pods.WakeRoleMessage:
		apply("wake-role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: token, At: reviewTestedAt, Report: "report-role"})
		return WakeTask(kind, token, StoppedByRelease)
	case pods.WakeAgentMessage:
		apply("wake-agent-message", intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: session, At: reviewTestedAt, Report: "report-agent"})
		return WakeTask(kind, session, StoppedByRelease)
	case pods.WakeBackstop:
		apply("wake-backstop", armBackstop(t, pool, wakeToken(t, role), role, reviewTestedAt))
		return WakeTask(kind, fmt.Sprintf("the backstop armed for LEGION-208's %s came due", role), StoppedByRelease)
	}
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		released, found, err := engine.cfg.Pods.ClaimByToken(ctx, tx, token)
		if err != nil || !found || !released.Released() {
			return fmt.Errorf("read the released %s claim: found %t, %v", role, found, err)
		}
		woken, err := engine.wakeReleased(ctx, tx, released, kind, "its detail", engine.now())
		if err == nil && !woken {
			err = fmt.Errorf("the %s wake of the %s took no wake", kind, role)
		}
		return err
	})
	return WakeTask(kind, "its detail", StoppedByRelease)
}

// wakesTheReviewerInRoundTwo is the walk of a wake of kind: LEGION-208's reviewer in its second
// round, released while it waits and brought back by that wake, with only the wake's start in the
// outbox.
func wakesTheReviewerInRoundTwo(kind string) func(t *testing.T) (*pgxpool.Pool, string) {
	return func(t *testing.T) (*pgxpool.Pool, string) {
		pool, engine, _ := reviewRoundTwo(t)
		outboxRan(t, pool)
		releasesTheReviewer(t, pool, engine)
		return pool, wakes(t, pool, engine, claim.RoleReviewer, "ses-reviewer", headOne, kind)
	}
}

// Each wake of the reviewer of an issue in reviewing, while the reviewer's row holds the task of
// an entry, gives it a task that ends naming the commit the row holds (reviewround.CommitLabel)
// and then that entry (reviewround.EntryLabel), each once: a reviewer that waits for CI is released after
// the idle period, and the wake is then its newest task. The wake's task is never the phase's
// work, and it still names the round's commit and entry, as each task of the reviewer in reviewing
// does: the reviewer writes the number of the newest entry its session was given into its review's
// footer, and the commit moves only for a review that names the entry that stands. The two
// sentences are the ones the queue of every worker's task appends (queuedRow, namingTheReviewRound),
// on each kind of wake.
//
// namingTheReviewRound gives each task of the reviewer those two sentences, in each phase, `none`
// where its row holds no commit or no entry's task and `none` for both outside reviewing, and
// gives them to no other of Legion's roles. The controls are its three other cases: a woken role that is not
// the reviewer, given the wake's task alone; a woken reviewer whose row the generation's clear
// left with no task and no commit (record.Store.ClearGeneration); and a woken reviewer whose issue
// has left reviewing, whose row still holds both, whose wake is delivered once the issue is back
// in reviewing. The last two end with ` Last reviewed commit: none. Review entry: none.`, and the
// start row of each of the three carries the fact the claim's send reads, so a wake queued outside
// reviewing is sent as it stands, ending with the daemon's sentences.
func TestEachWakeOfTheReviewerInReviewingEndsNamingItsEntry(t *testing.T) {
	t.Parallel()
	for _, kind := range wakeKinds {
		t.Run(kind, func(t *testing.T) {
			pool, wake := wakesTheReviewerInRoundTwo(kind)(t)
			row := reviewerRow(t, pool)
			if row.EntryTask == "" || row.Entry < 2 || row.ReviewedHead != headOne {
				t.Fatalf("the reviewer's row holds entry %d with the task %q and the commit %q, want round 2's entry, its task and %s",
					row.Entry, row.EntryTask, row.ReviewedHead, headOne)
			}
			task := onlyReviewerStart(t, pool).Task
			if want := wake + reviewedPartOf(headOne) + reviewEntryPartOf(row.Entry); task != want {
				t.Fatalf("the woken reviewer is delivered the task %q, want %q: it ends naming %s, the row's commit, and then entry %d, the row's",
					task, want, headOne, row.Entry)
			}
			for _, label := range []string{reviewround.CommitLabel, reviewround.EntryLabel} {
				if named := strings.Count(task, label); named != 1 {
					t.Fatalf("the woken reviewer's task %q names %q %d times, want once", task, label, named)
				}
			}
		})
	}
	for _, tc := range []struct {
		name string
		// role is the role the wake brings back.
		role claim.Role
		// walk takes LEGION-208 to the state the wake finds, with no supervise row in the outbox.
		walk func(t *testing.T) (*pgxpool.Pool, *Engine)
		// after is what the delivered task holds after the wake's task: the daemon's two sentences
		// naming none, or nothing.
		after string
	}{
		{
			name: "a woken role that is not the reviewer", role: claim.RoleImplementer,
			walk: func(t *testing.T) (*pgxpool.Pool, *Engine) {
				pool, engine, _ := reviewRoundTwo(t)
				if task := roleRow(t, pool, claim.RoleImplementer).EntryTask; task == "" {
					t.Fatalf("the implementer's row holds no entry's task: the control would prove nothing")
				}
				outboxRan(t, pool)
				seedReleasedClaim(t, pool, claim.RoleImplementer, "session-implementer")
				engine.cfg.Pods = wakePods(t, pool)
				return pool, engine
			},
		},
		{
			name: "a woken reviewer whose row the clear left with no task", role: claim.RoleReviewer, after: reviewedPartOf("none") + noEntryPart,
			walk: func(t *testing.T) (*pgxpool.Pool, *Engine) {
				pool, engine, _ := reviewRoundTwo(t)
				applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
					return record.NewStore().ClearGeneration(ctx, tx, "LEGION-208", engine.now())
				})
				if row := reviewerRow(t, pool); row.EntryTask != "" || row.ReviewedHead != "" || row.Entry < 2 {
					t.Fatalf("after the clear the reviewer's row holds entry %d, the task %q and the commit %q, want an entry and neither", row.Entry, row.EntryTask, row.ReviewedHead)
				}
				outboxRan(t, pool)
				releasesTheReviewer(t, pool, engine)
				return pool, engine
			},
		},
		{
			name: "a woken reviewer whose issue has left reviewing", role: claim.RoleReviewer, after: reviewedPartOf("none") + noEntryPart,
			walk: func(t *testing.T) (*pgxpool.Pool, *Engine) {
				pool, engine, apply := reviewRoundOne(t)
				apply("request-changes", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
				if row := reviewerRow(t, pool); issuePhase(t, pool) != phase.Implementing || row.EntryTask == "" || row.ReviewedHead != headOne {
					t.Fatalf("after the request for changes the issue is in %s and the reviewer's row holds the task %q and the commit %q, want implementing, round 1's task and %s",
						issuePhase(t, pool), row.EntryTask, row.ReviewedHead, headOne)
				}
				outboxRan(t, pool)
				releasesTheReviewer(t, pool, engine)
				return pool, engine
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, engine := tc.walk(t)
			session := map[claim.Role]string{claim.RoleReviewer: "ses-reviewer", claim.RoleImplementer: "session-implementer"}[tc.role]
			wake := wakes(t, pool, engine, tc.role, session, headOne, pods.WakeRoleMessage)
			starts := wakeStarts(t, pool)
			if len(starts) != 1 || starts[0].Role != tc.role {
				t.Fatalf("supervise start rows = %+v, want the one wake of the %s", starts, tc.role)
			}
			if want := wake + tc.after; starts[0].Task != want {
				t.Fatalf("the woken %s is delivered the task %q, want %q", tc.role, starts[0].Task, want)
			}
			if named := tc.role == claim.RoleReviewer; starts[0].RoundNamed != named {
				t.Fatalf("the woken %s's start row carries the fact that it names its round = %v, want %v", tc.role, starts[0].RoundNamed, named)
			}
			if tc.role == claim.RoleReviewer {
				back := issueRecord(t, pool, "LEGION-208")
				back.Phase = phase.Reviewing
				seedIssue(t, pool, *back)
				if again := wakeStarts(t, pool); len(again) != 1 || again[0].Task != wake+tc.after || !again[0].RoundNamed {
					t.Fatalf("with the issue back in reviewing the wake's start row = %+v, want it as queued: the wake's task, then the daemon's two sentences, and the fact", again)
				}
			}
		})
	}
}

// A reviewer that a CI settlement brings back submits its review of the round that stands with the
// entry the wake's task names, read from that task as the agent reads it, and the review moves the
// commit to the head it read. The same review with no entry decides the round and moves nothing,
// which is what a woken reviewer that followed a task naming no entry got. Round 2 here stands at
// headTwo, and round 1's review read headOne.
func TestAWokenReviewersReviewOfTheEntryItsWakeNamesMovesTheCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// namesEntry is whether the review's footer names the entry the wake's task names.
		namesEntry bool
		want       string
	}{
		{name: "the entry the wake names", namesEntry: true, want: headTwo},
		{name: "no entry", want: headOne},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, engine, apply := reviewRoundOne(t)
			apply("request-changes-1", reviewAt(t, pool, 1, "changes_requested", headOne, reviewTestedAt))
			mergesForward(t, pool, engine, headOne, headTwo, reviewTestedAt.Add(time.Hour))
			backToReviewing(t, apply, 2)
			outboxRan(t, pool)
			releasesTheReviewer(t, pool, engine)
			wakes(t, pool, engine, claim.RoleReviewer, "ses-reviewer", headTwo, pods.WakeChecks)
			entry := entryNamedIn(t, deliveredTask(t, pool, "the wake on a CI settlement"))
			review := reviewAt(t, pool, 2, "changes_requested", headTwo, reviewTestedAt.Add(2*time.Hour))
			review.Body = reviewBody("the review", headTwo)
			if tc.namesEntry {
				review.Body = reviewBodyOfEntry("the review", "ses-reviewer", headTwo, entry)
			}
			apply("request-changes-2", review)
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("after the woken reviewer's request for changes the issue is in %s, want implementing: the review decides the round", got)
			}
			if got := reviewerRow(t, pool).ReviewedHead; got != tc.want {
				t.Fatalf("after the woken reviewer's review the reviewer's row reads the reviewed commit %q, want %q", got, tc.want)
			}
		})
	}
}
