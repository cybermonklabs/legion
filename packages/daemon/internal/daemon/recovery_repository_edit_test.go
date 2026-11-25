package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A person's edit of the repository of an issue the daemon has not recorded yet sends an event of
// the issue, which the daemon applies. Here the edit lands while a recovery pass holds Dispatch's
// listing, read before the edit: the issue's todo event on zeta/tools fell silent while GitHub did
// not answer for zeta/tools, which woke the pass, and the pass's listing is held with the issue at
// sequence 1. The edit to orbit/gadgets, at sequence 2, applies while GitHub does not answer for
// orbit/gadgets either, so it records nothing. Then GitHub answers for zeta/tools and the pass goes
// on. The pass took its set while GitHub had not answered for zeta/tools, so it defers the issue
// its listing shows there and records nothing (deferUnreadIssues): the issue stays unrecorded
// while GitHub does not answer for orbit/gadgets, and once it does, a later listing records the
// issue on orbit/gadgets at sequence 2. Nothing is said on the issue, since Legion never held
// another repository for it. Before, the pass admitted the issue on zeta/tools at sequence 1 from
// its listing, and the edit, applied while the issue had no record, reached none: nothing moved
// the record onto orbit/gadgets or said anything on the issue.
func TestAnEditOfAnUnrecordedIssuesRepositoryDuringARecoveryIsApplied(t *testing.T) {
	t.Parallel()
	const key = "LEGION-1800"
	h := newHeldEditRecovery(t, key)
	h.edit(t, key, "orbit/gadgets", 2)
	h.letGo(t)
	if got := recordedIssue(t, h.w, key); got != nil {
		t.Fatalf("%s is %s once the held recovery ended, want it unrecorded: the recovery's listing showed it on zeta/tools at sequence 1, "+
			"and its pass took its set while GitHub had not answered for zeta/tools, so it deferred the issue (deferUnreadIssues)",
			key, recordedAs(t, h.w, key))
	}
	listed := h.client.listingCount()
	testwait.Eventually(t, "a recovery that listed the edit ended", func() bool { return h.client.listingCount() > listed })
	if got := recordedIssue(t, h.w, key); got != nil {
		t.Fatalf("%s is %s once a recovery that listed the edit ended, want it unrecorded while GitHub does not answer for orbit/gadgets",
			key, recordedAs(t, h.w, key))
	}
	h.installs.setFailing(nil, "implement orbit/gadgets")
	testwait.Eventually(t, key+" recorded once GitHub answers for orbit/gadgets", func() bool { return recordedIssue(t, h.w, key) != nil })
	if got := recordedIssue(t, h.w, key); got.Repo != "orbit/gadgets" || got.LastDispatchSeq != 2 || repoNoteOf(t, h.w, key) != nil {
		t.Fatalf("%s is %s, want it recorded on orbit/gadgets at sequence 2, the edit, with no repository note", key, recordedAs(t, h.w, key))
	}
}

// Two edits of the repository of an issue the daemon has not recorded yet land while a recovery
// pass holds a listing read before both (as above): the first to orbit/gadgets at sequence 2, the
// second back to zeta/tools at sequence 3, each applied while GitHub does not answer for its
// repository. The held listing names the repository the second edit names again, so its
// repository does not tell it is old, but its pass took its set while GitHub had not answered for
// zeta/tools, so the pass defers the issue and records nothing (deferUnreadIssues), and a later
// listing, which shows the second edit, records the issue on zeta/tools at sequence 3, with
// nothing said on it. Before, the held listing recorded the issue at sequence 1, behind both
// edits.
func TestTwoEditsOfAnUnrecordedIssuesRepositoryDuringARecoveryRecordItAtTheNewer(t *testing.T) {
	t.Parallel()
	const key = "LEGION-1802"
	h := newHeldEditRecovery(t, key)
	h.edit(t, key, "orbit/gadgets", 2)
	h.edit(t, key, "zeta/tools", 3)
	h.letGo(t)
	testwait.Eventually(t, key+" recorded once GitHub answers for zeta/tools", func() bool { return recordedIssue(t, h.w, key) != nil })
	if got := recordedIssue(t, h.w, key); got.Repo != "zeta/tools" || got.LastDispatchSeq != 3 || repoNoteOf(t, h.w, key) != nil {
		t.Fatalf("%s is %s, want it recorded on zeta/tools at sequence 3, the second edit, with no repository note", key, recordedAs(t, h.w, key))
	}
}

// The control order of the edit above: the edit lands after the recovery recorded the issue on
// zeta/tools. The record follows Dispatch (workflow.Engine.ApplyRepositoryChange): while GitHub
// does not answer for orbit/gadgets it keeps zeta/tools, takes the edit's sequence and says
// nothing on the issue, and once GitHub answers, the follow the edit's preparation asked for
// (requestFollowOf) moves it onto orbit/gadgets, with nothing said on the issue either.
func TestAnEditOfARecoveredIssuesRepositoryIsFollowedOnceGitHubAnswers(t *testing.T) {
	t.Parallel()
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools", "implement orbit/gadgets")
	w, client, installs, _ := repositoryRigWith(t, installs, quickInstallReads)
	const key = "LEGION-1801"
	deliver(t, w, client, issue(key, "zeta/tools", "todo", true, "", 1))
	serveAndRecover(t, w, installs)
	if got := recordedIssue(t, w, key); got == nil || got.Repo != "zeta/tools" || got.LastDispatchSeq != 1 {
		t.Fatalf("%s is %s, want it recovered on zeta/tools at sequence 1", key, recordedAs(t, w, key))
	}
	deliver(t, w, client, issue(key, "orbit/gadgets", "todo", true, "", 2))
	if got := recordedIssue(t, w, key); got.Repo != "zeta/tools" || got.LastDispatchSeq != 2 || repoNoteOf(t, w, key) != nil {
		t.Fatalf("%s is %s, want it on zeta/tools at sequence 2 with no repository note while GitHub does not answer for orbit/gadgets",
			key, recordedAs(t, w, key))
	}
	installs.setFailing(nil, "implement orbit/gadgets")
	testwait.Eventually(t, key+" on orbit/gadgets once GitHub answers for it", func() bool {
		got := recordedIssue(t, w, key)
		return got != nil && got.Repo == "orbit/gadgets"
	})
	if got := recordedIssue(t, w, key); got.LastDispatchSeq != 2 || repoNoteOf(t, w, key) != nil {
		t.Fatalf("%s is %s, want it on orbit/gadgets at sequence 2, the edit, with no repository note", key, recordedAs(t, w, key))
	}
}

// The order between the two above: the edit's event is prepared while the issue has no record,
// and a recovery's listing, read before the edit, records the issue on zeta/tools before that
// event applies. The held recovery's listing is not that one: it shows the issue at sequence 1
// while GitHub had not answered for zeta/tools as its pass took its set, so the pass defers the
// issue, records nothing and reads GitHub for zeta/tools (deferUnreadIssues). That answer wakes
// the next pass, whose listing, read after that answer and before the edit, is held too and
// shows the issue at sequence 1; the edit is prepared while it waits, and once that listing
// answers, it records the issue on zeta/tools. A preparation of an issue with no record reads
// GitHub for no correction and stamps no record (intake.DispatchIssue.PreparedAgainst), so when
// the event applies, its change onto orbit/gadgets writes nothing (workflow.Resolution.Unread),
// and the engine asks a follow of the issue at the event's commit, which reads Dispatch's listing
// and GitHub and moves the record onto orbit/gadgets, with no further event of the issue, no boot
// and nothing said on it. Before, that event asked no follow, and the record stayed on zeta/tools
// until the issue's next event or the next boot.
func TestAnEditPreparedBeforeARecoveryRecordedTheIssueIsFollowed(t *testing.T) {
	t.Parallel()
	const key = "LEGION-1803"
	h := newHeldEditRecovery(t, key)
	releaseNext := h.client.holdListing()
	t.Cleanup(releaseNext)
	h.installs.setFailing(nil, "implement zeta/tools")
	h.release()
	testwait.Eventually(t, "the next recovery's listing held", func() bool { return h.client.heldListingCount() == 2 })
	if got := recordedIssue(t, h.w, key); got != nil {
		t.Fatalf("%s is %s once the held recovery ended, want it unrecorded: its listing showed it on zeta/tools while GitHub "+
			"had not answered for zeta/tools as the pass took its set, so the pass deferred it", key, recordedAs(t, h.w, key))
	}
	h.installs.setFailing(nil, "implement orbit/gadgets")
	edit := issue(key, "orbit/gadgets", "todo", true, "", 2)
	prepared := prepareEvent(t, h.w, h.client, edit)
	if prepared.PreparedAgainst != "" || prepared.RepositoryRead != nil {
		t.Fatalf("the edit prepared while %s had no record = %+v, want no stamp of a record and no read for a correction", key, prepared)
	}
	releaseNext()
	testwait.Eventually(t, key+" recorded by the next recovery's listing", func() bool { return recordedIssue(t, h.w, key) != nil })
	if got := recordedIssue(t, h.w, key); got.Repo != "zeta/tools" || got.LastDispatchSeq != 1 {
		t.Fatalf("%s is %s, want the next recovery's listing, read before the edit, to record it on zeta/tools at sequence 1",
			key, recordedAs(t, h.w, key))
	}
	applyEvent(t, h.w, edit, prepared)
	testwait.Eventually(t, key+" on orbit/gadgets with no further event of it and no boot", func() bool {
		got := recordedIssue(t, h.w, key)
		return got != nil && got.Repo == "orbit/gadgets"
	})
	if got := recordedIssue(t, h.w, key); got.LastDispatchSeq != 2 || repoNoteOf(t, h.w, key) != nil {
		t.Fatalf("%s is %s, want it on orbit/gadgets at sequence 2, the edit, with no repository note", key, recordedAs(t, h.w, key))
	}
}

// A child's own event that the daemon applies while the child's parent is unrecorded records
// nothing, and nothing but a listing records the child after its parent is recorded
// (applySummary). Here GitHub has answered for zeta/tools again and the recovery pass that
// follows holds a listing that shows the child at sequence 1. Meanwhile the child's event at 2
// applies with its parent unrecorded, and the parent's own event records the parent under a live
// tree. The pass's listing is older than the child's applied event, so it records nothing of the
// child; the recovery keeps the child in the scope of its next pass and zeta/tools among the
// repositories to recover, and that next pass's listing, at 2, records the child under its
// parent. Before, the pass let zeta/tools go with the child unrecorded, and no later pass listed
// it.
func TestAChildARecoveryFoundNewerThanItsListingIsRecordedByTheNextPass(t *testing.T) {
	t.Parallel()
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	refresher := &heldRefresher{gate: make(chan struct{})}
	w, client, installs, _ := repositoryRigWith(t, installs, quickInstallReads, refresher.hold)
	const root, parent, child = "LEGION-1809", "LEGION-1810", "LEGION-1811"
	liveRoot(t, w, client, root)
	deliver(t, w, client, issue(child, "zeta/tools", "todo", false, parent, 1))
	if got := recordedIssue(t, w, child); got != nil {
		t.Fatalf("%s is %s while its parent is unrecorded and GitHub does not answer for zeta/tools, want it unrecorded", child, recordedAs(t, w, child))
	}
	// GitHub answers for zeta/tools while the refresher is parked; its next turn reads zeta/tools
	// again, which moves it among the repositories to recover, and the pass that recovers it holds
	// the next listing, read before the two events below.
	refresher.park(t, w)
	installs.setFailing(nil, "implement zeta/tools")
	release := client.holdListing()
	t.Cleanup(release)
	refresher.release(t, w)
	testwait.Eventually(t, "the recovery's listing held", func() bool { return client.heldListingCount() == 1 })
	listed := client.listingCount()
	deliver(t, w, client, issue(child, "zeta/tools", "todo", false, parent, 2))
	deliver(t, w, client, issue(parent, "acme/widgets", "todo", false, root, 1))
	if got := recordedIssue(t, w, parent); got == nil || got.Tree != root {
		t.Fatalf("%s is %s, want it recorded in %s's tree by its own event", parent, recordedAs(t, w, parent), root)
	}
	if got := recordedIssue(t, w, child); got != nil {
		t.Fatalf("%s is %s after its event at 2, applied while its parent was unrecorded, want it unrecorded", child, recordedAs(t, w, child))
	}
	release()
	testwait.Eventually(t, "the held recovery ended", func() bool {
		return len(w.recoveringReasons()) == 0 || client.listingCount() > listed
	})
	if recordedIssue(t, w, child) == nil && len(w.recoveringReasons()) == 0 {
		t.Fatalf("the held recovery let zeta/tools go with %s unrecorded: its listing, at 1, was older than the child's event at 2, and no later pass lists it", child)
	}
	testwait.Eventually(t, child+" recorded by a later recovery", func() bool { return recordedIssue(t, w, child) != nil })
	if got := recordedIssue(t, w, child); got.Repo != "zeta/tools" || got.Tree != root || got.LastDispatchSeq != 2 {
		t.Fatalf("%s is %s in tree %s, want it recorded on zeta/tools in %s's tree at sequence 2", child, recordedAs(t, w, child), got.Tree, root)
	}
}

// heldEditRecovery is the repository rig with a recovery pass held at its listing: key's todo
// event on zeta/tools applied while GitHub answers for neither zeta/tools nor orbit/gadgets, and
// the recovery that event's silence woke holding Dispatch's listing, which shows key on zeta/tools
// at sequence 1. release lets that pass answer.
type heldEditRecovery struct {
	w        *workflowRuntime
	client   *repositoryDispatch
	installs *installations
	release  func()
}

func newHeldEditRecovery(t *testing.T, key string) heldEditRecovery {
	t.Helper()
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools", "implement orbit/gadgets")
	w, client, installs, _ := repositoryRigWith(t, installs, quickInstallReads)
	// No repository is unread before the event's preparation leaves zeta/tools so, which is what
	// wakes the recovery, and deliver shows the issue before it prepares the event: the held listing
	// is the first, and shows the issue at sequence 1.
	release := client.holdListing()
	t.Cleanup(release)
	deliver(t, w, client, issue(key, "zeta/tools", "todo", true, "", 1))
	if got := recordedIssue(t, w, key); got != nil {
		t.Fatalf("%s is %s while GitHub does not answer for zeta/tools, want it unrecorded", key, recordedAs(t, w, key))
	}
	testwait.Eventually(t, "the recovery's listing held", func() bool { return client.heldListingCount() == 1 })
	return heldEditRecovery{w: w, client: client, installs: installs, release: release}
}

// edit delivers a person's edit of key's own repository to repo, at seq: Dispatch sends an event
// of it, which the daemon applies. GitHub does not answer for repo, so the event records nothing.
func (h heldEditRecovery) edit(t *testing.T, key, repo string, seq int64) {
	t.Helper()
	deliver(t, h.w, h.client, issue(key, repo, "todo", true, "", seq))
	if got := recordedIssue(t, h.w, key); got != nil {
		t.Fatalf("%s is %s after the edit to %s while GitHub does not answer for it, want it unrecorded", key, recordedAs(t, h.w, key), repo)
	}
}

// letGo lets GitHub answer for zeta/tools, then lets the held pass answer, and returns once that
// pass has ended: the refresher runs one pass at a time, and a later one has listed.
func (h heldEditRecovery) letGo(t *testing.T) {
	t.Helper()
	h.installs.setFailing(nil, "implement zeta/tools")
	listed := h.client.listingCount()
	h.release()
	testwait.Eventually(t, "the held recovery ended", func() bool { return h.client.listingCount() > listed })
}

// recordedAs is what w holds of key, for a failure to name: unrecorded, or recorded on a
// repository at a sequence, with the repository note the daemon last said on it.
func recordedAs(t *testing.T, w *workflowRuntime, key string) string {
	t.Helper()
	got := recordedIssue(t, w, key)
	if got == nil {
		return "unrecorded"
	}
	said := "no repository note"
	if note := repoNoteOf(t, w, key); note != nil {
		said = fmt.Sprintf("the repository note at %s %q", note.ResolvedRepo, note.Note)
	}
	return fmt.Sprintf("recorded on %s at sequence %d, with %s", got.Repo, got.LastDispatchSeq, said)
}

// repoNoteOf is the last repository note the daemon said on key (record.RepoNote), nil for none.
func repoNoteOf(t *testing.T, w *workflowRuntime, key string) *record.RepoNote {
	t.Helper()
	var note *record.RepoNote
	if err := store.TransactWith(context.Background(), w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		note, err = w.records.RepoNote(ctx, tx, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return note
}
