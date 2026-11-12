package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// skippedFlushLog is what the document service logs for a room flush asked while the asking
// transaction holds the document's advisory lock (docs/room_query.go, roomQueries.ask): every
// other process's append waits on that lock, so no member could answer, and the flush is skipped
// - the other process's room is never stored.
const skippedFlushLog = "the flush is skipped"

// A comment anchored by mark_id writes the comment's margin record (ProjectMark) into the same
// transaction as the version its anchor is pinned to (SnapshotVersion), whose capture takes the
// document's advisory lock and holds it to the commit. So the request joins the document's live
// write before that version (resolveAnchor's mark_id branch, docs.JoinWrite): the join is what
// asks the other processes for their rooms' flush, and asked under the lock it would be skipped.
// The request answers exactly as it did: 201, the browser's own mark, a pinned version holding
// it, and nothing orphaned - on a closed issue too, where this branch takes the version from the
// stored document.
func TestAMarkAnchoredCommentAsksItsFlushBeforeTheDocumentsLock(t *testing.T) {
	for _, test := range []struct {
		name   string
		closed bool
	}{
		{name: "on an open issue"},
		{name: "on a closed issue", closed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs := &storetest.LockedLog{}
			logtest.SetDefault(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			documentService, handler, database := browserDocumentService(t)
			runDocumentRoomSync(t, database, documentService)
			// The other envoy-dispatch task: its own pool, its own room sync, so it is a
			// room-sync member of this database and every flush is asked of it.
			second := storetest.SecondProcess(t, database)
			other := docs.New(docs.Deps{Store: second, Identity: headerIdentity(second), Settle: time.Hour})
			t.Cleanup(func() { _ = other.Shutdown(context.Background()) })
			runDocumentRoomSync(t, second, other)

			issue := createInteractionIssue(t, handler, "TEST", "Anchored to a browser's mark", "The quick brown fox")
			writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
				Kind: docs.MarkComment,
				ID:   "m-1",
				By:   model.Actor{Kind: "user", ID: "alice"},
			}, "quick")
			if test.closed {
				closeIssue(t, handler, issue.Key)
			}

			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body":   "why",
				"anchor": map[string]any{"artifact": "spec", "mark_id": "m-1"},
			}, "alice")

			if created.Code != http.StatusCreated {
				t.Fatalf("a comment anchored by mark_id: status=%d body=%s", created.Code, created.Body.String())
			}
			if strings.Contains(logs.String(), skippedFlushLog) {
				t.Fatalf("the anchor asked for a room flush while holding the document's advisory lock: %s", logs.String())
			}
			comment := decodeBody[model.Comment](t, created)
			if comment.Anchor == nil {
				t.Fatalf("the comment = %#v, want it anchored to the browser's mark", comment)
			}
			if comment.Anchor.MarkID != "m-1" || comment.Anchor.Quote != "quick" || comment.Anchor.Orphaned {
				t.Fatalf("the anchor = %#v, want the browser's mark on \"quick\", not orphaned", comment.Anchor)
			}
			if comment.Anchor.Version == 0 {
				t.Fatalf("the anchor = %#v, want it pinned to a version", comment.Anchor)
			}
			// The version the anchor names holds the mark: the document still reads it, and the
			// version's own text is the text it marks. The read is made the way the anchor made
			// it (docs.WithCommentMarks), since a closed issue's document is read for a comment's
			// mark and for nothing else.
			markCtx := docs.WithCommentMarks(context.Background())
			if anchored, err := documentService.VerifyMark(markCtx, issue.PrimaryArtifactID, docs.MarkComment, "m-1"); err != nil || anchored.Quote != "quick" {
				t.Fatalf("the mark the anchor pinned a version to = %q, %v", anchored.Quote, err)
			}
			if markdown := versionText(t, database, issue.PrimaryArtifactID, comment.Anchor.Version); !strings.Contains(markdown, "quick") {
				t.Fatalf("the pinned version reads %q, want the text the mark is in", markdown)
			}
		})
	}
}

// versionText is the stored markdown of a document's version.
func versionText(t *testing.T, database *store.Store, artifactID string, number int) string {
	t.Helper()
	var markdown string
	if err := database.Pool.QueryRow(context.Background(),
		`select markdown from artifact_versions where artifact_id = $1 and number = $2`,
		artifactID, number).Scan(&markdown); err != nil {
		t.Fatalf("read version %d's markdown: %v", number, err)
	}
	return markdown
}
