package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestARenameOfAProjectDocumentBesideACommentOnItBothSucceed meets the title correction of a
// project document (renameArtifact) with a session's comment on that document (createCommentFor).
// The correction of a document no issue holds takes only its project's upload lock
// (beginArtifactWrite, docs.LockProjectArtifacts) before it updates the document's row, and
// meets the document's owner lock (store.ArtifactOwner) first in its event's append (appendEvent,
// events.Broker's lockEventOwner). The comment takes that owner lock in its read phase
// (lockCommentOwner) and then appends comment.created, whose insert advances the document row's
// last_seq (events.Broker.QueueAppendWithLogins). A correction that updated the row before it
// took the owner lock holds the row the comment waits on while it waits on the lock the comment
// holds, and Postgres breaks the cycle by failing one of them: 500 INTERNAL, `deadlock detected`.
//
// The correction is held just after its update ran, so the comment starts while the correction's
// transaction holds whatever that update left it holding; once the comment waits on a lock, the
// correction goes on. Both must succeed, and the document must carry the corrected title and the
// comment.
func TestARenameOfAProjectDocumentBesideACommentOnItBothSucceed(t *testing.T) {
	t.Parallel()
	held := newHeldStatement(t, "update artifacts set name")
	fixture := newItemRowFixture(t, testServerOptions{tracer: held}, "Project document title", "A spec.\n")
	const quote = "The outbox ships first."
	document := createProjectDocument(t, fixture.handler, "LOCK", "Design notes", "# Design notes\n\n"+quote+"\n")
	waitForArtifactVersion(t, fixture.handler, document.ID, 1)
	const corrected = "Outbox design notes"

	held.armed.Store(true)
	renamed := make(chan *httptest.ResponseRecorder, 1)
	go func() { renamed <- correctTitle(t, fixture.handler, document.ID, corrected) }()
	testwait.Until(t, "the correction's update of the document's title to be held", func() (string, bool) {
		select {
		case <-held.reached:
			return "held", true
		case response := <-renamed:
			t.Fatalf("the correction answered before its update was held: status=%d body=%s", response.Code, response.Body.String())
		default:
		}
		return "not yet run", false
	})

	commented := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		commented <- sessionRequest(t, fixture.handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{
			"body":   "Does this still hold once the outbox is sharded?",
			"anchor": map[string]any{"artifact": document.Slug, "quote": quote}, "actor": sessionActor(),
		})
	}()
	testwait.Until(t, "the comment to wait on a lock the correction's transaction holds (the waiting sessions)", func() (int, bool) {
		waiting, err := storetest.CountLockWaits(context.Background(), fixture.database, "%")
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return waiting, true
		}
		select {
		case response := <-commented:
			t.Fatalf("the comment answered while the correction's transaction was held, waiting on nothing: status=%d body=%s",
				response.Code, response.Body.String())
		default:
		}
		return waiting, false
	})
	held.let()

	renameResponse := awaitAnswer(t, renamed, "the title correction")
	commentResponse := awaitAnswer(t, commented, "the session's comment")
	if renameResponse.Code != http.StatusOK {
		t.Fatalf("the title correction of the project document beside a comment on it: status=%d body=%s",
			renameResponse.Code, renameResponse.Body.String())
	}
	if commentResponse.Code != http.StatusCreated {
		t.Fatalf("the comment on the project document beside its title correction: status=%d body=%s",
			commentResponse.Code, commentResponse.Body.String())
	}
	if name := decodeBody[model.Artifact](t, renameResponse).Name; name != corrected {
		t.Fatalf("the corrected document is titled %q, want %q", name, corrected)
	}
	if comments := fixture.rowCount(t, `select count(*) from comments where artifact_id = $1`, document.ID); comments != 1 {
		t.Fatalf("the project document holds %d comments, want the one the session wrote", comments)
	}
}
