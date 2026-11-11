package workflow

import (
	"context"
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

// declineWords is what Dispatch writes beside a person's note when they decline to answer (its
// model.DeclineDispatchWords). The daemon holds no copy of it: the words travel as data on the
// ask.resolved event and onto the ask's mirror row, and this is the fixture's own copy, here to
// say what the architect must end up reading.
const declineWords = "The person chose not to answer this question. " +
	"Decide the next step without an answer. " +
	"Do not ask this question again unless the facts change. " +
	"Write in your work what you assumed."

// declineOf is a person's decline of ask at seq, as Dispatch's ask.resolved carries it: the
// resolution kind `declined`, with the person's own note apart from Dispatch's words.
func declineOf(ask string, seq int64, note string) intake.DispatchAskClosed {
	return intake.DispatchAskClosed{Key: "LEGION-208", Ask: ask, Kind: record.AskQuestion, State: "resolved",
		Resolution: intake.ResolutionDeclined, Reason: note, DispatchWords: declineWords, Actor: askPerson, Mark: askSeq(seq)}
}

// declinedMirror fails unless the mirror row of ask records the decline with the person's note and
// Dispatch's words as they arrived.
func declinedMirror(t *testing.T, pool *pgxpool.Pool, ask, note string) {
	t.Helper()
	row := mirroredAsk(t, pool, ask)
	if row == nil || !row.Declined || row.DeclinedNote != note || row.DeclinedWords != declineWords {
		t.Fatalf("the mirror row of %s = %+v, want it declined with the note %q and Dispatch's own words", ask, row, note)
	}
}

// resumeAnsweredRoot gives root's architect the task admission gives an answered root, from the
// wait the record holds: internal/admit's pass calls ResumeAwaitedRoot in just this way.
func resumeAnsweredRoot(t *testing.T, pool *pgxpool.Pool, root record.Issue) {
	t.Helper()
	if root.Awaiting == nil {
		t.Fatalf("%s holds no wait to resume from", root.Key)
	}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		ctx, _ := intake.WithCommitHooks(context.Background())
		return testEngine(config.DesignGateRootIssues, nil).ResumeAwaitedRoot(ctx, tx, root, *root.Awaiting)
	})
}

// architectSession is the session the architect's claim on key holds, so a resume can be held to
// the session the architect was already running in.
func architectSession(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	var session string
	if err := pool.QueryRow(context.Background(), `select session from claims where issue = $1 and role = 'architect'`, key).Scan(&session); err != nil {
		t.Fatalf("read the architect's claim of %s: %v", key, err)
	}
	return session
}

// resumeTaskOf is the one task the resume queued for key's architect, failing the test unless
// exactly one start was written to the live claim's session, generation and tree.
func resumeTaskOf(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	starts := superviseRowsOf(t, pool, key, "start")
	if len(starts) != 1 {
		t.Fatalf("%s queued %d starts, want the one resume of its architect", key, len(starts))
	}
	if starts[0].Role != claim.RoleArchitect || starts[0].Tree != key || starts[0].Generation != 1 {
		t.Fatalf("the resume = %+v, want a start of %s's architect at generation 1", starts[0], key)
	}
	if got := architectSession(t, pool, key); got != askArchitect.ID {
		t.Fatalf("the architect's claim holds session %q, want the resume to reach the session it was already running in, %q", got, askArchitect.ID)
	}
	return starts[0].Task
}

// namesTheDecline fails unless task says a person declined ask and carries Dispatch's four
// sentences and note, each as the record holds it. A decline the person wrote no note with
// (note "") must end after the sentences, with no separator left dangling.
func namesTheDecline(t *testing.T, task, key, ask, note string) {
	t.Helper()
	want := "A person declined to answer your ask " + ask + " on " + key + ". " + declineWords
	if note != "" {
		want += " Their note: " + note
	}
	if !strings.HasPrefix(task, want) {
		t.Fatalf("the resume task is %q, want it to open with %q", task, want)
	}
	if note == "" && strings.Contains(task, "Their note") {
		t.Fatalf("the resume task is %q, want no note at all for a decline the person wrote none with", task)
	}
}

// seedDeclaredRoot records LEGION-208 as a root the daemon started, holding its admission slot,
// whose architect is working the task it took and has handed a question back at seq 5: the
// standing an admission pass decides a wait from (decideAwait's rules 0, 1 and 5).
func seedDeclaredRoot(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	root := askRoot()
	root.HandedOver = true
	seedIssue(t, pool, root)
	seedArchitectWorking(t, pool, root)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: time.Date(2026, 9, 22, 22, 0, 0, 0, time.UTC)})
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().Declare(context.Background(), tx, root.Key,
			record.EventMark{Seq: 5, At: time.Date(2026, 9, 22, 23, 0, 0, 0, time.UTC)})
	})
}

// A person may decline to answer an ask rather than answer it, and the root whose architect asked
// must hear so and start again, as a person's answer starts it. The task names the question they
// would not answer, and carries the four sentences Dispatch writes for the asking agent and the
// person's own note, each as the record holds it: the daemon writes neither itself.
//
// It holds for any ask of the root, the one the wait names and any other, and by either path that
// resumes a root — the live one, and the admission pass that reads the daemon's own mirror of the
// root's asks because the decline reached the record before the pass could put the root in the
// wait. Each root below runs on a record of its own.
func TestAPersonsDeclineStartsTheWaitingRootAndNamesTheDecline(t *testing.T) {
	const note = "The vendor owns that date; I do not."

	// The first root waits on the question a person then declines: the live path answers its wait
	// the moment the decline arrives, and the resume says what the person did, in Dispatch's own
	// words and theirs.
	t.Run("a root waiting on the question a person declines", func(t *testing.T) {
		pool, apply := askRig(t)
		root := awaitingRoot(3)
		root.HandedOver = true
		seedIssue(t, pool, root)
		seedArchitectWorking(t, pool, root)
		seedQuestion(t, pool, "ask-q", 4)

		apply("declined", declineOf("ask-q", 8, note))

		answered := recordedRoot(t, pool)
		awaiting := answered.Awaiting
		if awaiting == nil || awaiting.AnsweredAt == nil || awaiting.AnsweredBy != record.AnsweredByDecline || awaiting.AnsweredDetail != "ask-q" {
			t.Fatalf("awaiting = %+v, want the wait answered by the person's decline of ask-q", awaiting)
		}
		// The daemon's state serves the record's own word for the act (api.WaitingOnView's
		// answeredBy, which projection copies as it stands).
		if got := string(awaiting.AnsweredBy); got != "declined" {
			t.Fatalf("the state would read answeredBy %q, want declined", got)
		}
		declinedMirror(t, pool, "ask-q", note)

		resumeAnsweredRoot(t, pool, answered)
		namesTheDecline(t, resumeTaskOf(t, pool, "LEGION-208"), "LEGION-208", "ask-q", note)
	})

	// The second root takes that same decline between its architect's hand-back and the pass that
	// would have put it in the wait, so no wait is standing for the live path to answer: the pass
	// reads the decline from the daemon's own mirror and tells the architect where it stands,
	// writing the answer on no issue row.
	t.Run("a root whose decline arrives before the pass that would start its wait", func(t *testing.T) {
		pool, apply := askRig(t)
		seedDeclaredRoot(t, pool)
		seedQuestion(t, pool, "ask-q", 4)
		seedQuestion(t, pool, "ask-second", 5)

		apply("declined", declineOf("ask-q", 8, note))
		if awaiting := recordedRoot(t, pool).Awaiting; awaiting != nil {
			t.Fatalf("awaiting = %+v, want no wait: the decline arrived while the root was still admitted", awaiting)
		}
		declinedMirror(t, pool, "ask-q", note)

		if entered := reconcileRoots(t, pool, "LEGION-208"); len(entered) != 0 {
			t.Fatalf("the pass put %v into the wait, want the architect told where it stands instead", entered)
		}
		told := recordedRoot(t, pool)
		if told.Phase != phase.Admitted || told.Awaiting != nil {
			t.Fatalf("the root = %s with awaiting %+v, want it left admitted with the answer written on no issue row", told.Phase, told.Awaiting)
		}
		namesTheDecline(t, resumeTaskOf(t, pool, "LEGION-208"), "LEGION-208", "ask-q", note)
	})

	// The third root waits on one question while a person declines another of its own. The resume
	// names the declined question and carries that question's note, by either path, and this
	// person wrote no note at all, so the task ends after Dispatch's sentences.
	t.Run("a root waiting on one question while a person declines another", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			drive func(*testing.T, *pgxpool.Pool, func(string, intake.Fact))
		}{
			{name: "with the event in hand", drive: func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact)) {
				root := awaitingRoot(3)
				root.HandedOver = true
				seedIssue(t, pool, root)
				seedArchitectWorking(t, pool, root)
				apply("declined", declineOf("ask-other", 8, ""))
				awaiting := recordedRoot(t, pool).Awaiting
				if awaiting == nil || awaiting.AnsweredBy != record.AnsweredByDecline || awaiting.AnsweredDetail != "ask-other" ||
					awaiting.Ask != "ask-q" {
					t.Fatalf("awaiting = %+v, want the wait on ask-q answered by the decline of ask-other", awaiting)
				}
				resumeAnsweredRoot(t, pool, recordedRoot(t, pool))
			}},
			{name: "through the mirror", drive: func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact)) {
				seedDeclaredRoot(t, pool)
				apply("declined", declineOf("ask-other", 8, ""))
				if entered := reconcileRoots(t, pool, "LEGION-208"); len(entered) != 0 {
					t.Fatalf("the pass put %v into the wait, want the architect told where it stands instead", entered)
				}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pool, apply := askRig(t)
				seedQuestion(t, pool, "ask-q", 4)
				seedQuestion(t, pool, "ask-other", 5)

				tc.drive(t, pool, apply)

				declinedMirror(t, pool, "ask-other", "")
				namesTheDecline(t, resumeTaskOf(t, pool, "LEGION-208"), "LEGION-208", "ask-other", "")
			})
		}
	})
}
