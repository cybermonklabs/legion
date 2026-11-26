package docs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// A live write into a retired document of an issue room is refused as one into a retired document
// that is its own room is (retiredWriteRefusal): an input a route answers 409 DOCUMENT_RETIRED for
// in ordinary use, and no fault. So refuseRetiredFragment logs it at Info naming the document, as
// retiredWriteRefusal logs that one, and logs no warning.
func TestARefusedWriteIntoARetiredDocumentOfAnIssueRoomLogsAtInfo(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	alice := model.Actor{Kind: "user", ID: "alice"}
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

	_, err = joinedReplaceText(service, room.verification, "Written after the retirement.\n", alice)
	var retired *RetiredDocumentError
	if !errors.As(err, &retired) || retired.ID != room.verification {
		t.Fatalf("a write into the retired Verification document %s of the issue room answered %v, want a RetiredDocumentError naming it", room.verification, err)
	}
	lines := logs.Lines("refuse a write into a retired document")
	if !strings.Contains(lines, room.verification) {
		t.Fatalf("the refusal logged %q, want its line naming the retired Verification document %s", lines, room.verification)
	}
	for line := range strings.Lines(lines) {
		if !strings.Contains(line, "level=INFO") {
			t.Errorf("the refusal logged %q, want it at Info, as an input a route answers 409 for", line)
		}
	}
}
