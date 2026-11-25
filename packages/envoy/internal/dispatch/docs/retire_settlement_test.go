package docs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// Typing a room holds when its document is retired (RetireDocument) is kept and versions nothing:
// a browser types into the document's room, the retirement commits before any settlement saves
// it, and then the room's save runs - the settlement a room's close or the process's shutdown
// runs. The save reads the retirement beside the owner's lock it takes (lockArtifactOwner), stores
// the typed update, as every settlement stores what it read, and logs that it writes no version of
// the retired document; the document holds the versions it held before the typing, and no more.
func TestTheSaveOfARetiredDocumentsTypingKeepsItAndWritesNoVersion(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n")
	settleCurrentGeneration(t, service, artifactID)
	ctx := context.Background()
	versions := func() int {
		t.Helper()
		var count int
		if err := service.store.Pool.QueryRow(ctx, `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&count); err != nil {
			t.Fatalf("count the versions: %v", err)
		}
		return count
	}
	before := versions()

	browserReplaceText(t, service, artifactID, "First.\n\nTyped as the document is retired.\n", model.Actor{Kind: "user", ID: "alice"})
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retirement: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := RetireDocument(ctx, tx, artifactID, model.Actor{Kind: "user", ID: "alice"}, "The test retires it."); err != nil {
		t.Fatalf("retire the document: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)

	if after := versions(); after != before {
		t.Fatalf("the save of the retired document wrote version %d of it, holding %d versions, want the %d it held before the typing",
			latestVersionNumber(t, service, artifactID), after, before)
	}
	stored, err := service.storedTree(ctx, roomTarget(artifactID))
	if err != nil {
		t.Fatalf("read the stored document: %v", err)
	}
	markdown, err := renderTree(stored)
	if err != nil || !strings.Contains(markdown, "Typed as the document is retired.") {
		t.Fatalf("the stored document reads %q (%v), want the typing kept", markdown, err)
	}
	if line := logs.Lines(artifactID); !strings.Contains(line, "writes no version of it") {
		t.Fatalf("the save logged %q, want its line naming the retired document", line)
	}
}

// A live write into a retired document (an edit, an ask block's write, a comment's mark) is
// refused with ErrDocumentRetired, and logged at Info naming the document - a refusal a route
// answers 409 for in ordinary use, not a fault - where it takes the document owner's lock
// (joinLiveWrite), the lock the retirement takes: it writes no version and no update.
func TestALiveWriteIntoARetiredDocumentIsRefusedUnderItsOwnersLock(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "First.\n")
	settleCurrentGeneration(t, service, artifactID)
	ctx := context.Background()
	before := latestVersionNumber(t, service, artifactID)
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retirement: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := RetireDocument(ctx, tx, artifactID, model.Actor{Kind: "user", ID: "alice"}, "The test retires it."); err != nil {
		t.Fatalf("retire the document: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}

	_, err = joinedReplaceText(service, artifactID, "First.\n\nWritten after the retirement.\n", model.Actor{Kind: "user", ID: "alice"})
	if !errors.Is(err, ErrDocumentRetired) {
		t.Fatalf("a write into the retired document answered %v, want ErrDocumentRetired", err)
	}
	if after := latestVersionNumber(t, service, artifactID); after != before {
		t.Fatalf("the refused write into the retired document wrote version %d of it, past its version %d", after, before)
	}
	stored, err := service.storedTree(ctx, roomTarget(artifactID))
	if err != nil {
		t.Fatalf("read the stored document: %v", err)
	}
	if markdown, err := renderTree(stored); err != nil || strings.Contains(markdown, "Written after the retirement.") {
		t.Fatalf("the stored document reads %q (%v), want nothing of the refused write", markdown, err)
	}
	if line := logs.Lines(artifactID); !strings.Contains(line, "refuse a write into a retired document") ||
		!strings.Contains(line, "level=INFO") || strings.Contains(line, "level=WARN") {
		t.Fatalf("the refusal logged %q, want its line naming the retired document at Info, as an input a route answers 409 for", line)
	}
}

// A live write into a retired document of an issue room is refused as a write into a retired
// document that is its own room is: the room reloads without the retired document's fragment
// (roomLayoutSQL), so text written into that fragment would be stored where no version and no
// index reads it. The room's write reads the document's own retirement under the room's owner
// lock (joinLiveWrite), the issue's, which the retirement takes too, and refuses the write with a
// RetiredDocumentError naming the document, not the room: a replacement of its text, a comment's
// mark on a quote of it, and a mark on it in a transaction that holds the room's write already,
// from a write into the spec, which the refusal rolls back with it. None of them writes a version
// of the document or text into its fragment, and each is logged naming it. A live write into the
// spec, a document of the same room that stands, lands.
func TestALiveWriteIntoARetiredDocumentOfAnIssueRoomIsRefusedAndTheRoomsOthersLand(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := joinedReplaceText(service, room.verification, "The check the retirement keeps.\n", alice); err != nil {
		t.Fatalf("write the Verification document before its retirement: %v", err)
	}
	before := latestVersionNumber(t, service, room.verification)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retirement: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := RetireDocument(ctx, tx, room.verification, alice, "The checks moved to the plan."); err != nil {
		t.Fatalf("retire the Verification document: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}

	refused := func(write string, err error) {
		t.Helper()
		var retired *RetiredDocumentError
		if !errors.As(err, &retired) || retired.ID != room.verification || retired.Slug != "verification-md" {
			t.Errorf("%s into the retired Verification document %s of the issue room %s answered %v, want a RetiredDocumentError naming the document",
				write, room.verification, room.tree, err)
		}
	}
	_, err = joinedReplaceText(service, room.verification, "The check the retirement keeps.\n\nWritten after the retirement.\n", alice)
	refused("a replacement", err)
	comment := func(id string) MarkSpec { return MarkSpec{Kind: MarkComment, ID: id, By: alice} }
	_, err = joinedMarkQuote(service, room.verification, comment("comment-retired"), "check", nil)
	refused("a comment's mark", err)
	err = joinedWrite(service, func(ctx context.Context) error {
		if _, err := service.ReplaceText(ctx, room.spec, "Written beside the refused mark.\n", alice, nil); err != nil {
			t.Fatalf("write the spec in the transaction that then marks the retired document: %v", err)
		}
		_, err := service.MarkQuote(ctx, room.verification, comment("comment-after-the-spec"), "check", nil)
		return err
	})
	refused("a comment's mark after a write into the spec in one transaction", err)

	if after := latestVersionNumber(t, service, room.verification); after != before {
		t.Errorf("the refused writes into the retired Verification document wrote version %d of it, past its version %d", after, before)
	}
	// stored is document's fragment of the room as the room's stored history holds it, empty for a
	// fragment that holds nothing, which reads as no tree.
	stored := func(document string) string {
		t.Helper()
		target, err := service.targetOf(ctx, document)
		if err != nil {
			t.Fatalf("resolve the room of %s: %v", document, err)
		}
		tree, err := service.storedTree(ctx, target)
		if err != nil {
			t.Fatalf("read the stored fragment of %s: %v", document, err)
		}
		if tree == nil {
			return ""
		}
		markdown, err := renderTree(tree)
		if err != nil {
			t.Fatalf("render the stored fragment of %s: %v", document, err)
		}
		return markdown
	}
	if markdown := stored(room.verification); markdown != "The check the retirement keeps.\n" {
		t.Errorf("the retired Verification document's fragment of the room reads %q after the refused writes, want the text it held at its retirement", markdown)
	}
	if markdown := stored(room.spec); strings.Contains(markdown, "Written beside the refused mark.") {
		t.Errorf("the spec's fragment reads %q, want nothing of the transaction the refused mark rolled back", markdown)
	}
	if line := logs.Lines(room.verification); !strings.Contains(line, "refuse a write into a retired document") {
		t.Errorf("the refusals logged %q, want their line naming the retired Verification document", line)
	}

	if _, err := joinedReplaceText(service, room.spec, "The spec's own words.\n", alice); err != nil {
		t.Fatalf("a write into the spec, which stands in the same room: %v", err)
	}
	if _, err := joinedMarkQuote(service, room.spec, comment("comment-spec"), "own words", nil); err != nil {
		t.Fatalf("a comment's mark on the spec, which stands in the same room: %v", err)
	}
	if markdown := stored(room.spec); !strings.Contains(markdown, "The spec's own words.") {
		t.Errorf("the spec's fragment reads %q after the write into it, want its words", markdown)
	}
}

// A comment thread's margin record (ProjectMark) on a retired document of an issue room, in a
// transaction that already holds its write to that room through another document of it, is
// skipped as it is on a retired document whose room is its own: it writes nothing and refuses
// nothing. The room's write read the room's own artifact, the issue's tree, under the issue's
// owner lock (joinLiveWrite), and nothing of the retired document, which the transaction only
// resolved into the room afterwards (targetOf, the resolution every entry point of the live path
// makes before it takes the room): a lookup of the transaction's write by that document's id finds
// the room's (Ledger.liveWriteFor), which proves nothing of the document. So the record reads the
// document's retirement under the owner lock the write holds. The transaction commits its write
// into the spec; the room's stored marks hold no record of the thread, and the retired document's
// fragment keeps the text it held at its retirement.
func TestAMarginRecordOnARetiredDocumentOfAnIssueRoomIsSkippedInsideTheRoomsWrite(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	alice := model.Actor{Kind: "user", ID: "alice"}
	const kept = "The check the retirement keeps.\n"
	if _, err := joinedReplaceText(service, room.verification, kept, alice); err != nil {
		t.Fatalf("write the Verification document before its retirement: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retirement: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := RetireDocument(ctx, tx, room.verification, alice, "The checks moved to the plan."); err != nil {
		t.Fatalf("retire the Verification document: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}

	const markID, beside = "thread-on-the-retired-document", "Written beside the thread's record.\n"
	err = joinedWrite(service, func(ctx context.Context) error {
		if _, err := service.ReplaceText(ctx, room.spec, beside, alice, nil); err != nil {
			t.Fatalf("write the spec, which joins the issue room's write: %v", err)
		}
		if write := joinedLiveWrite(ctx, room.spec); write == nil || write.artifactID != room.tree {
			t.Fatalf("the transaction that wrote the spec holds the write %v, want the issue room %s's", write, room.tree)
		}
		if _, err := service.targetOf(ctx, room.verification); err != nil {
			t.Fatalf("resolve the retired Verification document into its room: %v", err)
		}
		if write := joinedLiveWrite(ctx, room.verification); write == nil || write.artifactID != room.tree {
			t.Fatalf("a lookup by the retired Verification document's id finds the write %v, want the issue room %s's", write, room.tree)
		}
		return service.ProjectMark(ctx, room.verification, markID, MarkRecord{Kind: "comment", By: "user:alice", Text: "Which check?"}, alice)
	})
	if err != nil {
		t.Fatalf("the margin record on the retired Verification document, in the transaction holding the issue room's write through the spec, answered %v, want nil: the record skips a retired document", err)
	}

	loaded, err := service.persistence.LoadDocument(ctx, room.tree)
	if err != nil || loaded.Doc == nil {
		t.Fatalf("load the issue room's stored document: %v", err)
	}
	if record, held := loaded.Doc.GetMap(marksMapName).Get(markID); held {
		t.Errorf("the issue room's stored marks hold %#v under %s, want no record of the thread on the retired Verification document", record, markID)
	}
	fragment := func(document string) string {
		t.Helper()
		tree, _, err := treeOfFragments(loaded.Doc, []string{fragmentOf(room.tree, document)})
		if err != nil {
			t.Fatalf("read the stored fragment of %s: %v", document, err)
		}
		if tree == nil {
			return ""
		}
		markdown, err := renderTree(tree)
		if err != nil {
			t.Fatalf("render the stored fragment of %s: %v", document, err)
		}
		return markdown
	}
	if markdown := fragment(room.spec); !strings.Contains(markdown, "Written beside the thread's record.") {
		t.Errorf("the spec's stored fragment reads %q, want the transaction's write into it committed", markdown)
	}
	if markdown := fragment(room.verification); markdown != kept {
		t.Errorf("the retired Verification document's stored fragment reads %q, want the text %q it held at its retirement", markdown, kept)
	}
}
