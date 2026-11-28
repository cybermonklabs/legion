package daemon

// Two reads of Dispatch at one sequence cannot be ordered, and a correction moves no sequence
// (record.Store.CorrectIssueRepository writes none), so a correction prepared before another
// correction moved the record could be applied after it and put the record back on an older
// resolution, after which nothing read the issue again before its next event or the next boot.
// Each fact carries the repository its record held at its preparation
// (intake.DispatchIssue.PreparedAgainst), and the one writer
// (workflow.Engine.ApplyRepositoryChange) writes nothing from a fact whose record has moved since:
// the issue is owed a follow at that fact's commit (workflow.Config.FollowIssue,
// requestFollowOf), which reads Dispatch and GitHub fresh. These tests run the daemon's own
// preparations, handlers and follow over a real PostgreSQL and NATS (changingRig): each issue is
// recorded on acme/widgets, strangerRepo is the resolution the late fact carries, and answeredRepo
// the one a follow reads and applies first.

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
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
