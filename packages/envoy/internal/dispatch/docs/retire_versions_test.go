package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A retired document is given no version (retiredWriteRefusal): typing its room held when it was
// retired is stored by the room's save and versioned by nothing (TestTheSaveOfARetiredDocumentsTypingKeepsItAndWritesNoVersion),
// and a snapshot taken after it (SnapshotVersion), which would version that typing, reads the
// retirement under the owner's lock its version write takes and is refused with ErrDocumentRetired.
// No route of api snapshots a retired document today - a comment's anchor joins the document's
// live write first, which refuses it, and an edit tests the row - so this holds the rule where the
// version is written.
func TestASnapshotOfARetiredDocumentIsRefusedAndWritesNoVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n")
	settleCurrentGeneration(t, service, artifactID)
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	browserReplaceText(t, service, artifactID, "First.\n\nTyped as the document is retired.\n", alice)
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retirement: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := RetireDocument(ctx, tx, artifactID, alice, "The test retires it."); err != nil {
		t.Fatalf("retire the document: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	before := latestVersionNumber(t, service, artifactID)

	err = joinedWrite(service, func(ctx context.Context) error {
		_, err := service.SnapshotVersion(ctx, artifactID, alice)
		return err
	})
	if !errors.Is(err, ErrDocumentRetired) {
		t.Errorf("a snapshot of the retired document, its typing stored and versioned by nothing, answered %v, want ErrDocumentRetired", err)
	}
	if after := latestVersionNumber(t, service, artifactID); after != before {
		t.Errorf("the snapshot of the retired document wrote version %d of it, past its version %d", after, before)
	}
}
