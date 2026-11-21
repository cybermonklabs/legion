package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A person's edit of the repository of an issue the daemon has not recorded yet sends an event of
// the issue, which the daemon applies. Here the edit lands while a recovery pass holds Dispatch's
// listing, read before the edit: the issue's todo event on zeta/tools fell silent while GitHub did
// not answer for zeta/tools, which woke the pass, and the pass's listing is held with the issue at
// sequence 1. The edit to orbit/gadgets, at sequence 2, applies while GitHub does not answer for
// orbit/gadgets either, so it records nothing. Then GitHub answers for zeta/tools and the pass goes
// on. The listing is older than an event the daemon applied, so it records nothing: the issue stays
// unrecorded while GitHub does not answer for orbit/gadgets, and once it does, a later listing
// records the issue on orbit/gadgets at sequence 2. Nothing is said on the issue, since Legion
// never held another repository for it. Before, the pass admitted the issue on zeta/tools at
// sequence 1 from its listing, the edit reached no record, and no note said that Legion keeps
// zeta/tools.
func TestAnEditOfAnUnrecordedIssuesRepositoryDuringARecoveryIsApplied(t *testing.T) {
	const key = "LEGION-1800"
	h := newHeldEditRecovery(t, key)
	h.edit(t, key, "orbit/gadgets", 2)
	h.letGo(t)
	if got := recordedIssue(t, h.w, key); got != nil {
		t.Fatalf("%s is %s once the held recovery ended, want it unrecorded: the recovery's listing showed it at sequence 1, "+
			"older than the edit to orbit/gadgets at sequence 2 the daemon had applied, and GitHub does not answer for orbit/gadgets",
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
// repository. The held listing names the repository the second edit names again, so only the
// sequence tells it is old: it records nothing, and a later listing, which shows the second edit,
// records the issue on zeta/tools at sequence 3, with nothing said on it. Before, the held listing
// recorded the issue at sequence 1, behind both edits.
func TestTwoEditsOfAnUnrecordedIssuesRepositoryDuringARecoveryRecordItAtTheNewer(t *testing.T) {
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
// zeta/tools. The record keeps zeta/tools, takes the edit's sequence, and the daemon says on the
// issue that Legion keeps zeta/tools and does not apply the change to orbit/gadgets.
func TestAnEditOfARecoveredIssuesRepositoryIsNotedAndNotApplied(t *testing.T) {
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools", "implement orbit/gadgets")
	w, client, installs, _ := repositoryRigWith(t, installs)
	const key = "LEGION-1801"
	deliver(t, w, client, issue(key, "zeta/tools", "todo", true, "", 1))
	serveAndRecover(t, w, installs)
	if got := recordedIssue(t, w, key); got == nil || got.Repo != "zeta/tools" || got.LastDispatchSeq != 1 {
		t.Fatalf("%s is %s, want it recovered on zeta/tools at sequence 1", key, recordedAs(t, w, key))
	}
	deliver(t, w, client, issue(key, "orbit/gadgets", "todo", true, "", 2))
	note := repoNoteOf(t, w, key)
	const kept = "Legion keeps zeta/tools as this issue's repository; the change to orbit/gadgets in Dispatch is not applied"
	if got := recordedIssue(t, w, key); got.Repo != "zeta/tools" || got.LastDispatchSeq != 2 || note == nil ||
		note.ResolvedRepo != "orbit/gadgets" || !strings.HasPrefix(note.Note, kept) {
		t.Fatalf("%s is %s, want it on zeta/tools at sequence 2 with the note %q…", key, recordedAs(t, w, key), kept)
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
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	refresher := &heldRefresher{gate: make(chan struct{})}
	w, client, installs, _ := repositoryRigWith(t, installs, refresher.hold)
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
	quickInstallReads(t)
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools", "implement orbit/gadgets")
	w, client, installs, _ := repositoryRigWith(t, installs)
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
	if err := pgx.BeginFunc(context.Background(), w.pool, func(tx pgx.Tx) error {
		var err error
		note, err = w.records.RepoNote(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return note
}
