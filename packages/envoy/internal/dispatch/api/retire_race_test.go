package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// retireDuring retires artifactID through docs.RetireDocument in a transaction that holds the
// document owner's lock the retirement takes (lockArtifactOwner) until write, started on a
// goroutine of its own, waits on that lock, and then commits it: write's reads before the lock
// find the document standing, and every read after it finds it retired. It answers write's
// response.
func retireDuring(t *testing.T, database *store.Store, artifactID string, write func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retirement: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := docs.RetireDocument(ctx, tx, artifactID, model.Actor{Kind: "user", ID: "alice"}, "The test retires it."); err != nil {
		t.Fatalf("retire %s: %v", artifactID, err)
	}
	request := startRequest(write)
	waitForDatabaseLocks(t, tx, 1)
	if request.finished() {
		t.Fatalf("the write answered before the retirement committed, holding no lock of the document's owner: %s", request)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}
	select {
	case <-request.done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the write never answered once the retirement committed")
	}
	return request.response
}

// A write of new text whose first read of its document found it standing, and whose document's
// retirement (the decisions step's, docs.RetireDocument) commits before the write takes the lock of
// the document's owner, is refused under that lock as a write to a document retired before it came
// is: 409 DOCUMENT_RETIRED, 409 DECISIONS_ARE_NODES for an issue's decisions document, and no
// version written. The edit route tests the row before its transaction (editArtifact) and a
// project's upload finds its document before it takes that lock (uploadArtifactFor), so each reads
// the row again under it; an issue's upload finds its document under its issue's lock, the lock the
// retirement takes, and reads the retirement there.
func TestAWriteRacingItsDocumentsRetirementIsRefusedUnderTheOwnersLock(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "RACE", "Retirement races", "A spec\n")
	ops := map[string]any{"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": "More."}}, "summary": "edit"}

	for _, race := range []struct {
		what     string
		document func(t *testing.T) model.Artifact
		write    func(document model.Artifact) *httptest.ResponseRecorder
		code     string
	}{{
		what: "an edit of an issue document by its id",
		document: func(t *testing.T) model.Artifact {
			return uploadIssueDocument(t, handler, issue.Key, "notes.md", "Notes.\n")
		},
		write: func(document model.Artifact) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/edits", ops, "alice")
		},
		code: "DOCUMENT_RETIRED",
	}, {
		what: "an edit of the issue's decisions document by its slug",
		document: func(t *testing.T) model.Artifact {
			id := seedDecisionsDocument(t, database, deps.Docs.(*docs.Service), issue.Key, "# Decisions\n\n## Transport\n\nHTTP it is.\n")
			return model.Artifact{ID: id, Name: "decisions.md", Slug: "decisions-md"}
		},
		write: func(document model.Artifact) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts/"+document.Slug+"/edits", ops, "alice")
		},
		code: codeDecisionsAreNodes,
	}, {
		what: "an upload of a project document",
		document: func(t *testing.T) model.Artifact {
			return createProjectDocument(t, handler, "RACE", "Roadmap", "# Roadmap\n")
		},
		write: func(document model.Artifact) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/RACE/artifacts",
				map[string]string{"name": document.Name, "content": "# Roadmap\n\nNew text.\n"}, "alice")
		},
		code: "DOCUMENT_RETIRED",
	}, {
		what: "an upload of an issue document",
		document: func(t *testing.T) model.Artifact {
			return uploadIssueDocument(t, handler, issue.Key, "log.md", "Log.\n")
		},
		write: func(document model.Artifact) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts",
				map[string]string{"name": document.Name, "content": "Log, rewritten.\n"}, "alice")
		},
		code: "DOCUMENT_RETIRED",
	}} {
		t.Run(race.what, func(t *testing.T) {
			document := race.document(t)
			before := latestVersion(t, database, document.ID).Number
			response := retireDuring(t, database, document.ID, func() *httptest.ResponseRecorder { return race.write(document) })
			body := strings.TrimSpace(response.Body.String())
			if after := latestVersion(t, database, document.ID).Number; after != before {
				t.Fatalf("%s racing its retirement wrote version %d into the retired document %s, past its version %d: status=%d body=%s",
					race.what, after, document.ID, before, response.Code, body)
			}
			if response.Code != http.StatusConflict || responseCode(t, response) != race.code {
				t.Fatalf("%s racing its retirement: status=%d body=%s, want 409 %s", race.what, response.Code, body, race.code)
			}
		})
	}
}
