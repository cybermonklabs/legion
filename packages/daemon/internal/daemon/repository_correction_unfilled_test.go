package daemon

// A record written before records carried their repository holds none (record.Issue.Unfilled).
// The backfill fills it (fillRepositories), with checks a correction makes none of. So no pass
// over a recorded issue's resolution corrects such a record or reads GitHub for it: a live event's
// preparation (prepareIssue), a hand-over's (prepareHandover), the follow (followListing) and the
// boot's listing, which asks for no follow of it (prepareRecorded). A move prepared against such
// a record while the refresher's backfill fills it (backfillLater) is followed by the engine where
// it is applied after the fill (workflow.Engine.ApplyRepositoryChange). These tests run the
// daemon's own preparations, backfill and one writer (workflow.Engine.ApplyRepositoryChange) over
// a real PostgreSQL and NATS, with a GitHub that names strangerRepo as strangerNamed.

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// unfilledKey is the issue whose record these tests write with no repository.
const unfilledKey = "LEGION-1121"

// recordUnfilled records key as recordCorrectionRoot does, and then empties its repository
// columns: the record store refuses to write a record with no repository, so the row is made the
// way the migration that added the columns left every earlier row.
func recordUnfilled(t *testing.T, w *workflowRuntime, key string) {
	t.Helper()
	recordCorrectionRoot(t, w.pool, w.records, key, key, 0)
	if _, err := w.pool.Exec(context.Background(), "update issues set repo = '', default_branch = '', github_repo = '' where key = $1", key); err != nil {
		t.Fatalf("empty the repository of %s: %v", key, err)
	}
}

// applyCorrection applies fact through the one writer of a recorded issue's repository, in one
// transaction with its commit hooks, as every pass over a recorded issue's resolution does.
func applyCorrection(t *testing.T, w *workflowRuntime, fact intake.DispatchIssue) {
	t.Helper()
	if err := store.Transact(context.Background(), w.pool, func(ctx context.Context, tx pgx.Tx) error {
		stored, err := w.records.Issue(ctx, tx, fact.Key)
		if err != nil {
			return err
		}
		return w.engine.ApplyRepositoryChange(ctx, tx, *stored, fact)
	}); err != nil {
		t.Fatalf("apply the correction of %s: %v", fact.Key, err)
	}
}

// Every pass over a recorded issue's resolution leaves a record with no repository as it is:
// it reads GitHub for none of it, writes no repository, no repository it left and no change
// waiting, says nothing on the issue, and the boot's listing asks for no follow of it. The
// backfill then fills the record from Dispatch's resolution, at GitHub's spelling and default
// branch. Control: with the passes correcting such a record, each pass's preparation reads
// GitHub for stranger/tools, and the correction fills the record onto Stranger/Tools.
func TestNoPassCorrectsARecordWithNoRepositoryAndTheBackfillFillsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	implement := "implement " + strangerRepo
	for _, tc := range []struct {
		name string
		pass func(t *testing.T, w *workflowRuntime)
	}{
		{name: "a live event", pass: func(t *testing.T, w *workflowRuntime) {
			prepared, err := prepareAsDelivered(t, w, intake.DispatchIssue{Key: unfilledKey, Seq: 2, Type: "issue.updated", Status: "in_progress",
				Title: unfilledKey, Rank: "A", HandedOver: true, ResolvedRepo: strangerRepo})
			if err != nil {
				t.Fatalf("prepare the event: %v", err)
			}
			applyCorrection(t, w, prepared)
		}},
		{name: "a hand-over the outbox prepares", pass: func(t *testing.T, w *workflowRuntime) {
			handover, err := w.prepareHandover(ctx, intake.Handover{Issue: unfilledKey, ResolvedRepo: strangerRepo})
			if err != nil {
				t.Fatalf("prepare the hand-over: %v", err)
			}
			// The fact admission's consumeHandover builds from the hand-over.
			applyCorrection(t, w, intake.DispatchIssue{Key: unfilledKey, ResolvedRepo: handover.ResolvedRepo,
				RepositoryRead: handover.RepositoryRead, RepositoryRefusal: handover.RepositoryRefusal,
				RepositoryRefusalKind: handover.RepositoryRefusalKind, PreparedAgainst: handover.PreparedAgainst})
		}},
		{name: "the follow", pass: func(t *testing.T, w *workflowRuntime) {
			if left, err := w.followListing(ctx, map[string]bool{unfilledKey: true}); err != nil || len(left) != 0 {
				t.Fatalf("the follow left %v (%v), want nothing left and no error", left, err)
			}
		}},
		{name: "the boot's listing", pass: func(t *testing.T, w *workflowRuntime) {
			asked := w.followRequested.Load()
			recorded := recordedIssue(t, w, unfilledKey)
			w.prepareRecorded(ctx, intake.DispatchIssue{Key: unfilledKey, Seq: 1, Status: "in_progress", Title: unfilledKey,
				ResolvedRepo: strangerRepo}, recorded, true, nil)
			if w.followRequested.Load() != asked {
				t.Fatal("the boot's listing asked for a follow of a record with no repository, want none: the backfill fills it")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installs := &installations{}
			w, client, github := changingRig(t, installs)
			recordUnfilled(t, w, unfilledKey)
			client.show(dispatch.Issue{Key: unfilledKey, ResolvedRepo: strangerRepo, Title: unfilledKey, Status: "in_progress", Rank: "A",
				HandedOver: true, LastSeq: 1})

			tc.pass(t, w)

			if reads, named := installs.reads(implement), github.reads.Load(); reads != 0 || named != 0 {
				t.Fatalf("the pass read %s's implement App %d times and its name %d times, want neither: a record with no repository is the backfill's",
					strangerRepo, reads, named)
			}
			got := recordedIssue(t, w, unfilledKey)
			if got == nil || got.Repo != "" || got.DefaultBranch != "" || got.GitHubRepo != "" || got.RepoChange != nil {
				t.Fatalf("%s = %+v, want it still with no repository and nothing waiting", unfilledKey, got)
			}
			var corrected bool
			if err := w.pool.QueryRow(ctx, "select repo_corrected_at is not null from issues where key = $1", unfilledKey).Scan(&corrected); err != nil {
				t.Fatalf("read whether %s records a correction: %v", unfilledKey, err)
			}
			if corrected {
				t.Fatalf("%s records the moment of a correction, want none: nothing corrected it", unfilledKey)
			}
			if notes := repositoryNotes(t, w, unfilledKey); len(notes) != 0 {
				t.Fatalf("notes on %s = %q, want none", unfilledKey, notes)
			}

			if left, err := backfillRepositories(ctx, w.pool, w.records, client, "LEGION", t.TempDir(), w.defaultBranches, ghrepo.Renames{}, w.reads(), quietLogger()); err != nil || left {
				t.Fatalf("the backfill left a record = %t (%v), want every record filled", left, err)
			}
			got = recordedIssue(t, w, unfilledKey)
			if got == nil || got.Repo != strangerNamed || got.DefaultBranch != "trunk" || got.GitHubRepo != strangerNamed {
				t.Fatalf("%s after the backfill = %+v, want it filled onto %s at trunk, GitHub's spelling and default branch", unfilledKey, got, strangerNamed)
			}
		})
	}
}

// hookedListing is repositoryDispatch whose project listing, once onList is set, runs onList the
// next time it is listed, after the listing is read and before it is answered: a change Dispatch
// makes after a pass has read its listing.
type hookedListing struct {
	*repositoryDispatch
	onList atomic.Pointer[func()]
}

func (d *hookedListing) ListIssues(ctx context.Context, project string, statuses []string) ([]dispatch.IssueSummary, error) {
	listed, err := d.repositoryDispatch.ListIssues(ctx, project, statuses)
	if hook := d.onList.Swap(nil); hook != nil {
		(*hook)()
	}
	return listed, err
}

// The refresher's backfill (backfillLater) runs while intake is live, and it lists Dispatch and
// reads GitHub before it writes. Here the listing resolves the issue to strangerRepo, and before
// the fill writes, a person moves the issue onto answeredRepo: the event is prepared against a
// record that holds no repository, so its preparation reads nothing, stamps no repository and
// owes no follow (correctionMoves). The fill then writes strangerRepo, and the event applies after
// it with no read of GitHub, so its correction writes nothing
// (workflow.Engine.ApplyRepositoryChange). That apply asks for one follow of the issue at its
// commit, as every apply of a fact no preparation stamped against a record's repository does, and
// with no further event and no boot the follow reads Dispatch again and corrects the record onto
// answeredRepo, off the strangerRepo the fill wrote. The follows are counted across the apply
// alone, so no follow asked anywhere else stands in for the one the apply owes. Control: with no
// follow asked at the commit of an apply of a fact no preparation stamped
// (workflow.Engine.followOnCommit taken out of the unread branch), the apply asks for none and the
// record stays on strangerRepo while Dispatch resolves answeredRepo, and the issue's next pod is
// built there until its next event or the next boot.
func TestAMoveDuringTheRefreshersBackfillIsFollowedWhereItApplies(t *testing.T) {
	t.Parallel()
	const key = "LEGION-1122"
	var hooked *hookedListing
	w, client, _ := changingRig(t, &installations{}, func(w *workflowRuntime) {
		hooked = &hookedListing{repositoryDispatch: w.dispatch.(*repositoryDispatch)}
		w.dispatch, w.stateDir = hooked, t.TempDir()
	})
	recordUnfilled(t, w, key)
	client.show(issue(key, strangerRepo, "in_progress", true, "", 1))

	moved := issue(key, answeredRepo, "in_progress", true, "", 2)
	var prepared *intake.DispatchIssue
	move := func() {
		client.show(moved)
		asked := w.followRequested.Load()
		fact, err := prepareAsDelivered(t, w, intake.DispatchIssue{Key: key, Seq: moved.LastSeq, Type: "issue.updated", Status: moved.Status,
			Title: moved.Title, Rank: moved.Rank, HandedOver: true, ResolvedRepo: answeredRepo})
		if err != nil {
			t.Fatalf("prepare the move: %v", err)
		}
		if follows := w.followRequested.Load() - asked; fact.RepositoryRead != nil || fact.PreparedAgainst != "" || follows != 0 {
			t.Fatalf("the prepared move = %+v, asking %d follows; want no read of GitHub, no repository stamped and no follow: its record holds none",
				fact, follows)
		}
		prepared = &fact
	}
	hooked.onList.Store(&move)
	w.backfillLater(context.Background())
	if prepared == nil {
		t.Fatal("the backfill listed no Dispatch issue, want the move prepared after its listing")
	}
	if got := recordedIssue(t, w, key); got == nil || got.Repo != strangerNamed {
		t.Fatalf("%s after the backfill = %+v, want it filled onto %s, which the listing resolved", key, got, strangerNamed)
	}

	asked := w.followRequested.Load()
	if _, err := intake.ApplyFact(context.Background(), w.pool, "dispatch", key+"-moved", *prepared, w.handlers...); err != nil {
		t.Fatalf("apply the move: %v", err)
	}
	if follows := w.followRequested.Load() - asked; follows != 1 {
		t.Fatalf("the move's apply asked for %d follows, want one: its correction wrote nothing, and no preparation stamped it", follows)
	}
	followsSettled(t, w)
	if got := onRepository(t, w, key, answeredNamed, strangerNamed); got.LastDispatchSeq != 2 {
		t.Fatalf("%s is at Dispatch sequence %d, want the move's 2: the move applies though its correction writes nothing", key, got.LastDispatchSeq)
	}
}
