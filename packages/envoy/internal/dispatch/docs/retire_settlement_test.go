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
	stored, err := service.storedTree(ctx, artifactID)
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
	stored, err := service.storedTree(ctx, artifactID)
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
