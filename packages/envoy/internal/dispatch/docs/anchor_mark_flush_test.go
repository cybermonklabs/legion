package docs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A comment anchored to a mark the document already holds (api/anchors.go, resolveAnchor's
// mark_id branch, and comment_create.go behind it) writes nothing into the document's tree: in
// one transaction it joins the document's write, verifies the mark, pins the version its anchor
// names and writes the comment's margin record. The join comes first because the version's
// capture takes the document's advisory lock and holds it to the commit (lockedHead), and the
// join is what has every other process store its room of the document (flushOtherRooms): asked
// under that lock, which every other process's append waits on, the flush is skipped and the
// other process's room is never stored (roomQueries.ask).
//
// The tests here run that order with a second envoy-dispatch process on the same database
// (processPair), open issue and closed, and hold the other process's append at a gate, so the
// flush is answered only once that append is durable.

// anchorToAMark is the order a mark_id anchor runs in the request's one transaction
// (api/anchors.go, resolveAnchor's mark_id branch; comment_create.go, the margin record after
// the comment's row): the document's live write, the mark, the version the anchor pins, and the
// comment's margin record, all under WithCommentMarks.
func anchorToAMark(service *Service, artifactID, markID string, actor model.Actor) (Anchored, VersionResult, error) {
	var anchored Anchored
	var snapshot VersionResult
	err := joinedWrite(service, func(ctx context.Context) (err error) {
		ctx = WithCommentMarks(ctx)
		if err := service.JoinWrite(ctx, artifactID); err != nil {
			return err
		}
		if anchored, err = service.VerifyMark(ctx, artifactID, MarkComment, markID); err != nil {
			return err
		}
		if snapshot, err = service.SnapshotVersion(ctx, artifactID, actor); err != nil {
			return err
		}
		return service.ProjectMark(ctx, artifactID, markID, MarkRecord{
			Kind:      "comment",
			By:        ActorRef(actor),
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
			Text:      "About this.",
		}, actor)
	})
	return anchored, snapshot, err
}

// skippedFlush is what roomQueries.ask logs for a flush asked while the asking transaction holds
// the document's advisory lock - the mistake in this package's lock order the anchor's join
// keeps the mark_id branch out of.
const skippedFlush = "the flush is skipped"

// markedPair is the pair the tests below anchor on: the lost-edit document with a comment mark on
// its first paragraph, written while no other room held the document, both processes' room sync
// running, and task B's room resident and holding that mark. Task A holds no room.
func markedPair(t *testing.T, markID string) processPair {
	t.Helper()
	pair := newSeededProcessPair(t, lostEditDocument)
	if _, err := commentAnchor(pair.taskA, pair.artifactID, markID, "Alpha", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("the mark the later comment anchors to: %v", err)
	}
	pair.runRoomSync(t)
	if err := pair.taskB.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task B's room: %v", err)
	}
	return pair
}

// On an open issue the anchor waits for the other process's room: its join asks for the flush
// before it takes any lock of the document's, so task B's held append is durable before the
// version is captured, and the version the anchor pins holds both the mark and task B's text.
func TestAMarkAnchorFlushesTheOtherProcessesRoomBeforeTheDocumentsLock(t *testing.T) {
	logs := captureWarnings(t)
	pair := markedPair(t, "c1")
	bob := model.Actor{Kind: "user", ID: "bob"}
	pair.heldEditOnB(t, 1, bob, replaceRun("Bravo paragraph.", "Bravo paragraph, by bob."))
	if stored := storedMarkdown(t, pair.database, pair.artifactID); strings.Contains(stored, "by bob") {
		t.Fatalf("the store holds bob's edit %q while its append is held, so nothing here is tested", stored)
	}

	const held = 200 * time.Millisecond
	released := time.AfterFunc(held, pair.gateB.release)
	defer released.Stop()
	started := time.Now()
	anchored, snapshot, err := anchorToAMark(pair.taskA, pair.artifactID, "c1", model.Actor{Kind: "user", ID: "carol"})
	waited := time.Since(started)

	if err != nil {
		t.Fatalf("a comment anchored to a mark the document already holds: %v", err)
	}
	if strings.Contains(logs.String(), skippedFlush) {
		t.Fatalf("the anchor asked for a flush while holding the document's advisory lock: %s", logs.String())
	}
	if waited < held-20*time.Millisecond {
		t.Fatalf("the anchor returned after %v, before task B's held append was let go at %v: it never waited for the other process's room", waited, held)
	}
	if stored := storedMarkdown(t, pair.database, pair.artifactID); !strings.Contains(stored, "by bob") {
		t.Fatalf("the store reads %q once the anchor's flush was answered, without bob's edit", stored)
	}
	if anchored.Quote != "Alpha" {
		t.Fatalf("the verified mark anchored = %#v, want the quote it marks", anchored)
	}
	if snapshot.Version.Number == 0 {
		t.Fatalf("the anchor pinned version %#v, want the version holding its mark", snapshot.Version)
	}
	markdown := versionMarkdown(t, pair.database, pair.artifactID, snapshot.Version.Number)
	if !strings.Contains(markdown, "Alpha") || !strings.Contains(markdown, "by bob") {
		t.Fatalf("the pinned version reads %q, want the text the mark is in and the other process's edit the flush stored", markdown)
	}
	if err := joinedWrite(pair.taskA, func(ctx context.Context) error {
		_, err := pair.taskA.VerifyMark(WithCommentMarks(ctx), pair.artifactID, MarkComment, "c1")
		return err
	}); err != nil {
		t.Fatalf("the mark the pinned version names, read again: %v", err)
	}
}

// A closed issue takes the same comment (WithCommentMarks), and the same order holds: its version
// is taken from the stored document (closedIssueCommentAnchor), which stores none of the room's
// updates, but the write it joins still asks the other process for its room's flush - and asks it
// before the lock, not under it.
func TestAMarkAnchorOnAClosedIssueAsksItsFlushBeforeTheDocumentsLock(t *testing.T) {
	logs := captureWarnings(t)
	pair := markedPair(t, "c1")
	closeIssueAgo(t, pair.database, "DOC-1", closeMargin+30*time.Second)

	anchored, snapshot, err := anchorToAMark(pair.taskA, pair.artifactID, "c1", model.Actor{Kind: "user", ID: "carol"})

	if err != nil {
		t.Fatalf("a comment anchored to a mark already in the closed issue's document: %v", err)
	}
	if strings.Contains(logs.String(), skippedFlush) {
		t.Fatalf("the closed issue's anchor asked for a flush while holding the document's advisory lock: %s", logs.String())
	}
	if anchored.Quote != "Alpha" {
		t.Fatalf("the verified mark anchored = %#v, want the quote it marks", anchored)
	}
	if snapshot.Version.Number == 0 {
		t.Fatalf("the anchor pinned version %#v, want the version holding its mark", snapshot.Version)
	}
	if markdown := versionMarkdown(t, pair.database, pair.artifactID, snapshot.Version.Number); !strings.Contains(markdown, "Alpha") {
		t.Fatalf("the pinned version reads %q, want the stored document the mark is in", markdown)
	}
	if err := joinedWrite(pair.taskA, func(ctx context.Context) error {
		_, err := pair.taskA.VerifyMark(WithCommentMarks(ctx), pair.artifactID, MarkComment, "c1")
		return err
	}); err != nil {
		t.Fatalf("the mark the pinned version names, read again: %v", err)
	}
}
