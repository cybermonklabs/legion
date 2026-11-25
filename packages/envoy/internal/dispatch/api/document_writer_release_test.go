package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/testwait"
)

// requireAnsweredWhileHeld requires the write under name answered 200 while the writer before it,
// which has committed and is only doing its post-commit work, is held there: the row holds that
// writer until after this answer, so a write that still waited on its documents' writer gates
// would be answered no 200 while it is held, and the answer itself, not how soon it came, is the
// proof. Its one bound is the test binary's deadline (testwait.Receive).
func requireAnsweredWhileHeld(t *testing.T, answers chan *httptest.ResponseRecorder, name string) {
	t.Helper()
	if response := testwait.Receive(t, name+"'s answer while the write before it is held past its commit", answers); response.Code != http.StatusOK {
		t.Errorf("%s: status=%d body=%s, want 200: the documents' writer gates are handed on at the commit", name, response.Code, response.Body.String())
	}
}

// A write holds its documents' writer gates for its transaction, not for what it does after the
// commit. An anchored ask's related-item check runs with its document's gate handed on already,
// so the next writer of that document waits for the ask's transaction alone and never for a
// check that has up to five seconds to answer.
func TestAnAskCreateReleasesItsDocumentsGateBeforeItsRelatedCheck(t *testing.T) {
	t.Parallel()
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	var holding atomic.Bool
	entered := make(chan struct{}, 1)
	resume := make(chan struct{})
	var released sync.Once
	releaseChecks := func() { released.Do(func() { close(resume) }) }
	defer releaseChecks()
	// Held from the moment the test arms it: the issue created below runs a check of its own,
	// and that one must run through.
	served.relatedCheckEntered = func() {
		if !holding.Load() {
			return
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-resume
	}
	issue := createInteractionIssue(t, handler, "TEST", "Ask gate release", "alpha bravo charlie")

	holding.Store(true)
	asked := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		asked <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": "Which shape should the ledger take?",
			"anchor":   map[string]string{"artifact": "spec", "quote": "bravo"},
		}, "alice")
	}()
	// The ask has committed and published by now: its related-item check is the next thing it does.
	testwait.Receive(t, "the ask's related-item check to start", entered)

	edited := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		edited <- editDocument(t, handler, issue.PrimaryArtifactID, insertAtEnd("Written while the ask's check is held."))
	}()
	requireAnsweredWhileHeld(t, edited, "an edit of the document the ask anchored to")

	releaseChecks()
	if response := testwait.Receive(t, "the anchored ask's answer", asked); response.Code != http.StatusCreated {
		t.Errorf("the anchored ask: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
}

// The same for a comment: its mentions are delivered after its commit, each a round trip to the
// listener, and the documents it marked are free for the next writer from the commit on.
func TestACommentCreateReleasesItsDocumentsGatesBeforeItsMentionDeliveries(t *testing.T) {
	t.Parallel()
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["steer"]}]`
	sending := make(chan struct{}, 1)
	resume := make(chan struct{})
	var released sync.Once
	releaseSends := func() { released.Do(func() { close(resume) }) }
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			select {
			case sending <- struct{}{}:
			default:
			}
			<-resume
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	// The sends go through before the listener closes, which waits for them.
	defer listener.Close()
	defer releaseSends()
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: pastTheRun})
	issue := createInteractionIssue(t, handler, "TEST", "Comment gate release", "alpha bravo charlie")

	posted := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		posted <- postMentionedComment(t, handler, issue.Key, map[string]any{
			"anchor":   map[string]string{"artifact": "spec", "quote": "bravo"},
			"body":     "Anchored note the planner has to read.",
			"mentions": []map[string]any{{"target": "session:s1"}},
		})
	}()
	// The comment has committed and published by now: its mention is held at the listener.
	testwait.Receive(t, "the comment's mention send to reach the listener", sending)

	edited := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		edited <- editDocument(t, handler, issue.PrimaryArtifactID, insertAtEnd("Written while the mention is held."))
	}()
	requireAnsweredWhileHeld(t, edited, "an edit of the document the comment anchored to")

	releaseSends()
	if response := testwait.Receive(t, "the anchored comment's answer", posted); response.Code != http.StatusCreated {
		t.Errorf("the anchored comment: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
}
