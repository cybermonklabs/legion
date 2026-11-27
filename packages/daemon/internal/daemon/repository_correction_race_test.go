package daemon

// Two reads of Dispatch at one sequence cannot be ordered, and a correction moves no sequence
// (record.Store.CorrectIssueRepository writes none), so a correction prepared before another
// correction moved the record could be applied after it and put the record back on an older
// resolution, after which nothing read the issue again before its next event or the next boot.
// Each fact carries the repository its record held at its preparation
// (intake.DispatchIssue.PreparedAgainst, intake.Handover.PreparedAgainst), and the one writer
// (workflow.Engine.ApplyRepositoryChange) writes nothing from a fact whose record has moved since:
// the issue is owed a follow at that fact's commit (workflow.Config.FollowIssue,
// requestFollowOf), which reads Dispatch and GitHub fresh. These tests run the daemon's own
// preparations, handlers and follow over a real PostgreSQL and NATS (changingRig): each issue is
// recorded on acme/widgets, strangerRepo is the resolution the late fact carries, and answeredRepo
// the one a follow reads and applies first.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// followsSettled waits until every follow asked for of w is applied.
func followsSettled(t *testing.T, w *workflowRuntime) {
	t.Helper()
	follows := w.followRequested.Load()
	testwait.Eventually(t, "every follow asked for applied", func() bool { return w.followApplied.Load() >= follows })
}

// raceBranches is the default branch changingGitHub answers for each repository a test here moves
// a record onto, by GitHub's spelling of it.
var raceBranches = map[string]string{answeredNamed: "main", "zeta/tools": "main", strangerNamed: "trunk"}

// onRepository fails unless key's record is on repo, GitHub's spelling, at the default branch
// GitHub answers for it, having left before at its last correction, with nothing waiting, and
// answers it.
func onRepository(t *testing.T, w *workflowRuntime, key, repo, before string) record.Issue {
	t.Helper()
	got := recordedIssue(t, w, key)
	if got == nil || got.Repo != repo || got.GitHubRepo != repo || got.DefaultBranch != raceBranches[repo] {
		t.Fatalf("%s = %+v, want it on %s at %s", key, got, repo, raceBranches[repo])
	}
	if got.RepoCorrection == nil || got.RepoCorrection.Before != before {
		t.Fatalf("the correction of %s = %+v, want %s left", key, got.RepoCorrection, before)
	}
	if got.RepoChange != nil {
		t.Fatalf("%s waits on %+v, want nothing waiting", key, got.RepoChange)
	}
	return *got
}

// handOverRaceRoot is the done root lingering after its sign-off, and handOverRaceChild the member
// it hands to Legion as a root of its own.
const handOverRaceRoot, handOverRaceChild = "LEGION-1151", "LEGION-1152"

// seedHandOverRace records handOverRaceRoot lingering and handOverRaceChild in its tree, both on
// acme/widgets, the child at Dispatch sequence 4, with the durable fence the linger's hand-over
// wrote for it.
func seedHandOverRace(t *testing.T, w *workflowRuntime) {
	t.Helper()
	ctx := context.Background()
	until := time.Now().Add(time.Hour)
	parent := handOverRaceRoot
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := w.records.PutIssue(ctx, tx, record.Issue{Key: handOverRaceRoot, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Tree: handOverRaceRoot, Project: "LEGION", Title: handOverRaceRoot,
			Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A", HandedOver: true, LingerUntil: &until}); err != nil {
			return err
		}
		if err := w.records.PutIssue(ctx, tx, record.Issue{Key: handOverRaceChild, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Tree: handOverRaceRoot, Project: "LEGION", Title: handOverRaceChild, Parent: &parent,
			Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "todo", Rank: "B", LastDispatchSeq: 4}); err != nil {
			return err
		}
		return w.records.PutHandover(ctx, tx, record.Handover{Issue: handOverRaceChild, Tree: handOverRaceRoot, Generation: 1})
	}); err != nil {
		t.Fatalf("seed the hand-over: %v", err)
	}
}

// The outbox reads a child it hands to Legion from Dispatch, which resolves it to strangerRepo,
// and prepares the hand-over against the record on acme/widgets (prepareHandover), carrying
// GitHub's read of strangerRepo. Before the hand-over applies, the project's default repository
// moves, which Dispatch sends no event of the child for, and a follow reads the child at the
// outbox read's own sequence resolved to answeredRepo and moves the record onto it: no sequence
// orders the two reads. The hand-over then applies through admission (admit's consumeHandover) and
// writes no correction, the record having moved since its preparation, and the issue is owed a
// follow at its commit, which reads Dispatch and GitHub fresh: the record ends on what Dispatch
// resolves then, answeredRepo where it still does and the newer zeta/tools where the default moved
// again, never on the hand-over's older strangerRepo. A process that stops between the
// hand-over's commit and its ask loses only the ask: the next boot's listing finds the record on
// another repository than Dispatch resolves and follows it (requestFollow). Control: with the
// correction written whatever the record held at the preparation, the hand-over moves the record
// onto strangerRepo and nothing reads the issue again.
func TestAHandOverPreparedBeforeAFollowMovedTheRecordWritesNothingAndIsFollowed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// now is what Dispatch resolves the child to once the follow has moved it onto
		// answeredRepo and before the hand-over applies; want is the repository its record ends
		// on, as GitHub spells it, and before the one its last correction left.
		now, want, before string
		// lost has the process stop between the hand-over's commit and its ask for a follow: the
		// hand-over applies through an engine that asks for none, and the next boot's listing
		// follows the issue.
		lost bool
	}{
		{name: "Dispatch still resolves the follow's repository", now: answeredRepo, want: answeredNamed, before: "acme/widgets"},
		{name: "Dispatch resolves a newer repository at the follow's fresh read", now: "zeta/tools", want: "zeta/tools", before: answeredNamed},
		{name: "the process stops before the commit's ask and the next boot follows", now: "zeta/tools", want: "zeta/tools", before: answeredNamed,
			lost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, _ := changingRig(t, &installations{})
			seedHandOverRace(t, w)
			client.show(issue(handOverRaceChild, strangerRepo, "todo", true, "", 4))
			handover, err := w.prepareHandover(ctx, intake.Handover{Issue: handOverRaceChild, Tree: handOverRaceRoot, Generation: 1,
				ResolvedRepo: strangerRepo, Seq: 4})
			if err != nil {
				t.Fatalf("prepare the hand-over: %v", err)
			}
			if handover.RepositoryRead == nil {
				t.Fatalf("the prepared hand-over = %+v, want it carrying GitHub's read of %s", handover, strangerRepo)
			}

			client.show(issue(handOverRaceChild, answeredRepo, "todo", true, "", 4))
			w.requestFollowOf(handOverRaceChild)
			followsSettled(t, w)
			onRepository(t, w, handOverRaceChild, answeredNamed, "acme/widgets")
			client.show(issue(handOverRaceChild, tc.now, "todo", true, "", 4))

			handlers := w.handlers
			if tc.lost {
				engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, NoteUnread: w.noteUnread,
					HearRepository: w.hearRecorded, Steps: config.DesignGateOff.Steps()}, quietLogger())
				handlers = []intake.Handler{engine, admit.New(w.records, engine, 3, "LEGION", quietLogger())}
			}
			asked := w.followRequested.Load()
			if _, err := intake.ApplyFact(ctx, w.pool, "outbox", "handover:"+handOverRaceChild, handover, handlers...); err != nil {
				t.Fatalf("apply the hand-over: %v", err)
			}
			got := recordedIssue(t, w, handOverRaceChild)
			if got.Tree != handOverRaceChild || got.Generation != 2 {
				t.Fatalf("the handed-over child = %+v, want it re-admitted as a tree of its own at generation 2", got)
			}
			if got.Repo == strangerNamed {
				t.Fatalf("the hand-over moved %s back onto %s, the resolution it read before the follow's", handOverRaceChild, strangerNamed)
			}
			want := asked + 1
			if tc.lost {
				want = asked
			}
			if follows := w.followRequested.Load(); follows != want {
				t.Fatalf("the hand-over asked for %d follows, want %d", follows-asked, want-asked)
			}
			if tc.lost {
				onRepository(t, w, handOverRaceChild, answeredNamed, "acme/widgets")
				if err := w.reconcile(ctx); err != nil {
					t.Fatalf("the next boot's reconcile: %v", err)
				}
				if follows := w.followRequested.Load(); follows == want {
					t.Fatal("the next boot's listing asked for no follow, want one for the record it finds on another repository than Dispatch resolves")
				}
			}

			followsSettled(t, w)
			onRepository(t, w, handOverRaceChild, tc.want, tc.before)
			if notes := repositoryNotes(t, w, handOverRaceChild); len(notes) != 0 {
				t.Fatalf("notes on %s = %q, want none", handOverRaceChild, notes)
			}
		})
	}
}

// The issue is recorded on acme/widgets at Dispatch sequence 1. Its event at sequence 2 resolves it
// to strangerRepo, the project's default then, and is prepared while the record is on acme/widgets,
// carrying GitHub's read of strangerRepo; it is late, and applies only after the default moves to
// answeredRepo, which Dispatch sends no event of the issue for, and a follow reads the issue at
// sequence 2 resolved there and moves the record onto it. The follow moves no sequence, so the
// event is still above the record's (record.Issue.Outdates lets it through), but the record has
// moved since its preparation: it writes no correction, it is applied all the same (the record
// takes its sequence), and the issue is owed a follow at its commit, whose fresh read finds
// Dispatch resolving answeredRepo and leaves the record there. An event no preparation stamped
// with its record's repository (intake.DispatchIssue.PreparedAgainst empty) is applied whatever
// repository the record holds: its correction is written from its read, and nothing is followed.
// Control: with the correction written whatever the record held at the preparation, the stamped
// event moves the record back onto strangerRepo, the older resolution.
func TestALateEventPreparedBeforeAFollowMovedTheRecordWritesNothingAndIsFollowed(t *testing.T) {
	t.Parallel()
	const key = "LEGION-1161"
	for _, tc := range []struct {
		name string
		// unstamped clears the repository the event's preparation found on the record.
		unstamped bool
		// want is the repository the record ends on, as GitHub spells it, and before the one its
		// last correction left; asks is how many follows the event asks for.
		want, before string
		asks         uint64
	}{
		{name: "an event its preparation stamped", want: answeredNamed, before: "acme/widgets", asks: 1},
		{name: "an event no preparation stamped", unstamped: true, want: strangerNamed, before: answeredNamed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, _ := changingRig(t, &installations{})
			recordCorrectionRoot(t, w.pool, w.records, key, key, 0)
			late := issue(key, strangerRepo, "in_progress", true, "", 2)
			client.show(late)
			prepared, err := prepareAsDelivered(t, w, intake.DispatchIssue{Key: key, Seq: late.LastSeq, Type: "issue.updated",
				Status: late.Status, Title: late.Title, Rank: late.Rank, HandedOver: true, ResolvedRepo: strangerRepo})
			if err != nil {
				t.Fatal(err)
			}
			if prepared.RepositoryRead == nil {
				t.Fatalf("the prepared event = %+v, want it carrying GitHub's read of %s", prepared, strangerRepo)
			}
			if tc.unstamped {
				prepared.PreparedAgainst = ""
			}

			client.show(issue(key, answeredRepo, "in_progress", true, "", 2))
			w.requestFollowOf(key)
			followsSettled(t, w)
			followed := onRepository(t, w, key, answeredNamed, "acme/widgets")
			if followed.LastDispatchSeq != 1 {
				t.Fatalf("after the follow %s is at Dispatch sequence %d, want 1: a correction moves no sequence", key, followed.LastDispatchSeq)
			}

			asked := w.followRequested.Load()
			if _, err := intake.ApplyFact(ctx, w.pool, "dispatch", key+"-late", prepared, w.handlers...); err != nil {
				t.Fatalf("apply the late event: %v", err)
			}
			if got := recordedIssue(t, w, key); got.Repo != tc.want {
				t.Fatalf("the late event left %s on %s, want %s", key, got.Repo, tc.want)
			}
			if follows := w.followRequested.Load() - asked; follows != tc.asks {
				t.Fatalf("the late event asked for %d follows, want %d", follows, tc.asks)
			}
			followsSettled(t, w)
			got := onRepository(t, w, key, tc.want, tc.before)
			if got.LastDispatchSeq != 2 {
				t.Fatalf("%s is at Dispatch sequence %d, want the late event's 2: it applies though its correction writes nothing", key, got.LastDispatchSeq)
			}
			if !tc.unstamped && !got.RepoCorrection.At.Equal(followed.RepoCorrection.At) {
				t.Fatalf("the correction of %s = %+v, want the follow's %+v: no later correction", key, got.RepoCorrection, followed.RepoCorrection)
			}
			if notes := repositoryNotes(t, w, key); len(notes) != 0 {
				t.Fatalf("notes on %s = %q, want none", key, notes)
			}
		})
	}
}
