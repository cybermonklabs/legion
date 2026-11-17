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
)

// reviewEntryPartOf is the sentence a reviewer's task in reviewing names the entry of its round
// with, " Review entry: <n>.", written out here rather than through reviewEntry.part, so the
// tests read the sentence and not the code that writes it.
func reviewEntryPartOf(entry int64) string {
	return " " + ReviewEntryLabel + ": " + strconv.FormatInt(entry, 10) + "."
}

// entryNamedIn is the entry the reviewer's task names in its last sentence, read as an agent reads
// it: the number after ReviewEntryLabel, failing the test when the task does not end naming one.
func entryNamedIn(t *testing.T, task string) int64 {
	t.Helper()
	label := " " + ReviewEntryLabel + ": "
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
// reviewer's row holds (or, for a resume level with its entry, no number at all).
func deliveredTask(t *testing.T, pool *pgxpool.Pool, path string) string {
	t.Helper()
	start, row := onlyReviewerStart(t, pool), reviewerRow(t, pool)
	if start.Entry != 0 && start.Entry != row.Entry {
		t.Fatalf("%s: the start hands the reviewer entry %d, want the row's %d", path, start.Entry, row.Entry)
	}
	return start.Task
}

// Each task the reviewer is given in reviewing ends naming the entry of its round
// (ReviewEntryLabel): the number the reviewer's row holds at that moment (record.PhaseRow.Entry),
// in each round, the first one too, while the row holds the task of an entry. The reviewer writes
// it into the footer of each review it submits for that task, and only a review that names the
// entry that stands moves the commit the reviewer read (review). One function gives the reviewer's
// task the sentences of its round (namingTheReviewRound): the commit its row holds, from the second
// round on, then the entry, its last sentence. The one place every task for a worker is queued
// through (queuedRow) calls it, so each way the daemon gives the reviewer a task reaches it,
// whichever composed the task: the cases here are each way there is today, a wake of each kind
// among them. A start that raises the entry stores the text it sends (enter), so the row's stored
// task and the delivered one are one text, and the delivery of an owed entry sends that text again
// with each sentence once.
//
// The resume task made because the stored text is empty (a clear emptied it, and no entry was
// raised since) names no entry, since no review of that entry moves the commit. A task of any
// other role names no entry either.
func TestEachTaskOfTheReviewerInReviewingEndsNamingItsEntry(t *testing.T) {
	resume := "Continue root. Issue: LEGION-208. Phase: reviewing. Resume the existing phase work." + reviewedPartOf(headOne)
	for _, tc := range []struct {
		name string
		// walk takes LEGION-208 to the path's start, with only that start in the outbox, and returns
		// the pool and the start's task as stored and as delivered must read before the sentence.
		walk func(t *testing.T) (*pgxpool.Pool, string)
		// raises is whether the path raises the entry and stores the text it sends (enter), so the
		// row's stored task is the delivered one word for word.
		raises bool
		// none is the eighth case: the task names no entry.
		none bool
	}{
		{
			name: "a transition's start of the first round", raises: true,
			walk: func(t *testing.T) (*pgxpool.Pool, string) {
				pool, _, _ := reviewRoundOne(t)
				return pool, "Continue root. Issue: LEGION-208. Phase: reviewing. Handoff: test version 1 (dispatch handoff-read --issue LEGION-208 --phase test --version 1)." +
					" Tester's verdict: pass. Pull request: #42."
			},
		},
		{
			name: "a transition's start of a later round", raises: true,
			walk: func(t *testing.T) (*pgxpool.Pool, string) {
				pool, _, apply := reviewRoundOne(t)
				outboxRan(t, pool)
				reviewedTwice(t, apply, pool)
				for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
					if task := roleRow(t, pool, role).EntryTask; task == "" || strings.Contains(task, ReviewEntryLabel) {
						t.Fatalf("the %s's row holds the task %q, want round 2's task with no review entry: only the reviewer is told one", role, task)
					}
				}
				return pool, "Continue root. Issue: LEGION-208. Phase: reviewing. Handoff: test version 2 (dispatch handoff-read --issue LEGION-208 --phase test --version 2)." +
					" Tester's verdict: pass. Pull request: #42." + reviewedPartOf(headOne)
			},
		},
		{
			name: "the promotion's start", raises: true,
			walk: func(t *testing.T) (*pgxpool.Pool, string) {
				pool, engine, _ := reviewedChild(t)
				readmitsTheTree(t, pool, engine, engine.now().Add(time.Hour))
				outboxRan(t, pool)
				promotesTheReviewer(t, pool, engine, engine.now().Add(time.Hour))
				return pool, "Continue child. Issue: LEGION-208. Phase: reviewing. Resume the existing phase work."
			},
		},
		{
			name: "the delivery of an owed entry", raises: true,
			walk: func(t *testing.T) (*pgxpool.Pool, string) {
				pool, engine, _ := reviewRoundTwo(t)
				row, issue := reviewerRow(t, pool), issueRecord(t, pool, "LEGION-208")
				outboxRan(t, pool)
				applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
					return engine.SendOwedEntry(ctx, tx, *issue, claim.RoleReviewer, row)
				})
				return pool, strings.TrimSuffix(row.EntryTask, reviewEntryPartOf(row.Entry))
			},
		},
		{
			name: "the retry of a held reviewing phase",
			walk: func(t *testing.T) (*pgxpool.Pool, string) {
				pool, _, apply := reviewRoundTwo(t)
				held := issueRecord(t, pool, "LEGION-208")
				held.Phase, held.Hold = phase.Held, &record.Hold{From: phase.Reviewing}
				seedIssue(t, pool, *held)
				outboxRan(t, pool)
				apply("retry", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision, Reason: "the claim failed"})
				return pool, "Continue root. Issue: LEGION-208. Phase: reviewing. Pull request: #42. Reason: the claim failed." + reviewedPartOf(headOne)
			},
		},
		{
			name: "a ready worker's catch-up",
			walk: func(t *testing.T) (*pgxpool.Pool, string) {
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
				return pool, resume
			},
		},
		{
			name: "the silence repair",
			walk: func(t *testing.T) (*pgxpool.Pool, string) {
				pool, engine, _ := reviewRoundTwo(t)
				row, issue := reviewerRow(t, pool), issueRecord(t, pool, "LEGION-208")
				outboxRan(t, pool)
				applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
					return engine.repairSilence(ctx, tx, *issue, *issue, record.RoleRun{Claim: &record.RoleClaim{GivenEntry: row.Entry}}, row,
						intake.WorkerSilent{Issue: "LEGION-208", Role: claim.RoleReviewer, Generation: 1, LastStep: reviewTestedAt, Bound: time.Hour},
						reviewTestedAt.Add(2*time.Hour))
				})
				return pool, resume
			},
		},
		{
			name: "the boot's resume",
			walk: func(t *testing.T) (*pgxpool.Pool, string) {
				pool, engine, _ := reviewRoundTwo(t)
				bootResumesTheReviewer(t, pool, engine)
				return pool, resume
			},
		},
		{name: "a wake on a CI settlement", walk: namingTheCommitOfRoundOne(wakesTheReviewerInRoundTwo(pods.WakeChecks))},
		{name: "a wake on a role message", walk: namingTheCommitOfRoundOne(wakesTheReviewerInRoundTwo(pods.WakeRoleMessage))},
		{name: "a wake on an agent message", walk: namingTheCommitOfRoundOne(wakesTheReviewerInRoundTwo(pods.WakeAgentMessage))},
		{name: "a wake on the backstop", walk: namingTheCommitOfRoundOne(wakesTheReviewerInRoundTwo(pods.WakeBackstop))},
		{name: "a wake of any other kind", walk: namingTheCommitOfRoundOne(wakesTheReviewerInRoundTwo(aKindStillToCome))},
		{
			name: "the resume of an entry whose stored task is empty", none: true,
			walk: func(t *testing.T) (*pgxpool.Pool, string) {
				pool, engine, _ := reviewedChild(t)
				readmitsTheTree(t, pool, engine, engine.now().Add(time.Hour))
				row, issue := reviewerRow(t, pool), issueRecord(t, pool, "LEGION-208")
				if row.EntryTask != "" {
					t.Fatalf("after the re-admission the reviewer's row stores the task %q, want none", row.EntryTask)
				}
				outboxRan(t, pool)
				applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
					return engine.SendOwedEntry(ctx, tx, *issue, claim.RoleReviewer, row)
				})
				return pool, "Continue child. Issue: LEGION-208. Phase: reviewing. Resume the existing phase work."
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, before := tc.walk(t)
			row := reviewerRow(t, pool)
			stored := row.EntryTask
			delivered := deliveredTask(t, pool, tc.name)
			want := before + reviewEntryPartOf(row.Entry)
			if tc.none {
				want = before
				if strings.Contains(delivered, ReviewEntryLabel) {
					t.Fatalf("the reviewer is delivered the task %q, want no %q in it: its row holds no entry's task", delivered, ReviewEntryLabel)
				}
			}
			if delivered != want {
				t.Fatalf("the reviewer is delivered the task %q, want %q: it ends naming entry %d, the row's", delivered, want, row.Entry)
			}
			if tc.raises && stored != delivered {
				t.Fatalf("the reviewer's row stores the task %q, and the reviewer is delivered %q: the two must be one text", stored, delivered)
			}
			if !tc.raises && !tc.none && !strings.HasSuffix(stored, reviewEntryPartOf(row.Entry)) {
				t.Fatalf("the reviewer's row stores the task %q, want the task its entry %d was raised with, which names it", stored, row.Entry)
			}
		})
	}
}

// The stored task of an entry (enter) is the text the reviewer was sent, word for word, and the
// delivery of that entry owed again (SendOwedEntry) sends it as stored, though the reviewer's row
// holds another commit since: each sentence of the round once, and the commit the task named when
// it was stored, or none where it named none. The queue (queuedRow, namingTheReviewRound) finds
// the sentence of the entry that stands at the task's end and adds nothing. Each case moves the
// row's commit by hand, to a commit the stored task does not name.
func TestAnOwedEntryIsSentAsStoredAfterTheRowsReviewedCommitMoved(t *testing.T) {
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
			commit := ""
			if tc.names != "" {
				commit = reviewedPartOf(tc.names)
			}
			if entry := reviewerRow(t, pool).Entry; !strings.HasSuffix(stored, " Pull request: #42."+commit+reviewEntryPartOf(entry)) ||
				strings.Count(stored, ReviewedCommitLabel) != strings.Count(commit, ReviewedCommitLabel) {
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
			applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
				return engine.SendOwedEntry(ctx, tx, *issue, claim.RoleReviewer, row)
			})
			if delivered := deliveredTask(t, pool, tc.name); delivered != stored {
				t.Fatalf("the owed entry is delivered as %q, want the stored task %q word for word: the row's commit moved to %s since", delivered, stored, tc.moved)
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
	applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
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
// wake (wakeReleased) gives any kind it is handed.
const aKindStillToCome = "a wake of a kind still to come"

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
		return WakeTask(kind, settlementDetail(fact))
	case pods.WakeRoleMessage:
		apply("wake-role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: token, At: reviewTestedAt, Report: "report-role"})
		return WakeTask(kind, token)
	case pods.WakeAgentMessage:
		apply("wake-agent-message", intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: session, At: reviewTestedAt, Report: "report-agent"})
		return WakeTask(kind, session)
	case pods.WakeBackstop:
		id := armBackstop(t, pool, role, reviewTestedAt)
		apply("wake-backstop", intake.PodBackstopDue{ID: id, Issue: "LEGION-208", Role: role})
		return WakeTask(kind, fmt.Sprintf("the backstop armed for LEGION-208's %s came due", role))
	}
	applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		released, found, err := engine.cfg.Pods.ReleasedClaimOfRole(ctx, tx, "LEGION-208", role)
		if err != nil || !found {
			return fmt.Errorf("read the released %s claim: found %t, %v", role, found, err)
		}
		woken, err := engine.wakeReleased(ctx, tx, released, kind, "its detail", engine.now())
		if err == nil && !woken {
			err = fmt.Errorf("the %s wake of the %s took no wake", kind, role)
		}
		return err
	})
	return WakeTask(kind, "its detail")
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

// namingTheCommitOfRoundOne is walk with the commit round 1's review read named after the task it
// answers, as the queue names it before the entry (namingTheReviewRound): the task a wake of the
// reviewer in its second round must read before the sentence of its entry.
func namingTheCommitOfRoundOne(walk func(t *testing.T) (*pgxpool.Pool, string)) func(t *testing.T) (*pgxpool.Pool, string) {
	return func(t *testing.T) (*pgxpool.Pool, string) {
		pool, before := walk(t)
		return pool, before + reviewedPartOf(headOne)
	}
}

// Each wake of the reviewer of an issue in reviewing, while the reviewer's row holds the task of
// an entry, gives it a task that ends naming the commit the row holds (ReviewedCommitLabel) and
// then that entry (ReviewEntryLabel), each once: a reviewer that waits for CI is released after
// the idle period, and the wake is then its newest task. The wake's task is never the phase's
// work, and it still names the round's commit and entry, as each task of the reviewer in reviewing
// does: the reviewer writes the number of the newest entry its session was given into its review's
// footer, and the commit moves only for a review that names the entry that stands. The two
// sentences are the ones the queue of every worker's task appends (queuedRow, namingTheReviewRound),
// on each kind of wake.
//
// namingTheReviewRound names a commit and an entry for the reviewer, in reviewing, while its row
// holds them, and for nothing else. The controls are its three other cases, each given the wake's
// task alone: a woken role that is not the reviewer, a woken reviewer whose row the generation's
// clear left with no task and no commit (record.Store.ClearGeneration), and a woken reviewer
// whose issue has left reviewing, whose row still holds both.
func TestEachWakeOfTheReviewerInReviewingEndsNamingItsEntry(t *testing.T) {
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
			for _, label := range []string{ReviewedCommitLabel, ReviewEntryLabel} {
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
			name: "a woken reviewer whose row the clear left with no task", role: claim.RoleReviewer,
			walk: func(t *testing.T) (*pgxpool.Pool, *Engine) {
				pool, engine, _ := reviewRoundTwo(t)
				applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
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
			name: "a woken reviewer whose issue has left reviewing", role: claim.RoleReviewer,
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
			if starts[0].Task != wake {
				t.Fatalf("the woken %s is delivered the task %q, want the wake's task %q alone: namingTheReviewRound names neither a commit nor an entry here", tc.role, starts[0].Task, wake)
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
