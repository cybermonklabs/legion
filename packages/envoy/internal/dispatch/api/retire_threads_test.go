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

// Review round 1 of this change: a retired document takes no new text (docs' joinLiveWrite refuses a
// live write into it), and a comment thread anchored in one stays a thread. Its resolve, its
// reopen, the edit of one of its comments and a reply to it change the thread's own rows and write
// nothing into the retired document: the thread's margin record reads the retirement under the
// document owner's lock and skips the document (docs.Service.ProjectMark, retiredUnderOwnerLock),
// so no version and no live update is written. A reword of a block ask whose block is in a retired
// document, a named version of a retired document and its rebuild are refused as an edit of that
// document is, and write nothing.

// retiredThread is one thread of a retiredThreads fixture: the document it is anchored in, the
// thread's root comment, the route a reply to it posts on, and what the document held of its
// writes once it was retired.
type retiredThread struct {
	what       string
	document   string
	root       model.Comment
	replyRoute string
	written    versionsAndUpdates
}

// versionsAndUpdates is what a document holds of its writes: how many versions, and its live
// history, each stored update's version and digest in order, which a rebuild replaces whole.
type versionsAndUpdates struct {
	versions int
	history  string
}

func readDocumentWrites(t *testing.T, database *store.Store, artifactID string) versionsAndUpdates {
	t.Helper()
	var written versionsAndUpdates
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from artifact_versions where artifact_id = $1),
			(select coalesce(string_agg(version::text || ':' || md5(update), ',' order by version), '') from doc_updates where artifact_id = $1)
	`, artifactID).Scan(&written.versions, &written.history); err != nil {
		t.Fatalf("read the writes of %s: %v", artifactID, err)
	}
	return written
}

// unwritten holds that what, a write a retired document took no part of, left the document
// artifactID the versions and the live history it held before.
func unwritten(t *testing.T, database *store.Store, artifactID, what string, before versionsAndUpdates) {
	t.Helper()
	if after := readDocumentWrites(t, database, artifactID); after != before {
		t.Errorf("%s left the document %d versions and the live history %q, want the %d versions and the history %q it held: a retired document takes no new text",
			what, after.versions, after.history, before.versions, before.history)
	}
}

// retiredThreads is a server holding three comment threads, each anchored on a quote of a document
// that was retired once the thread was written: an issue's notes.md, the same issue's decisions.md,
// and a project document. The decisions step retires a decisions.md through docs.RetireDocument,
// which retireTestDocument runs as well, as
// TestAnAnswerToAnAskWhoseRowNamesARetiredDecisionsDocumentIsTakenAndWritesItNoVersion retires one.
// Each thread's root is alice's, and resolved before the retirement where resolved says.
type retiredThreads struct {
	handler  http.Handler
	database *store.Store
	threads  []retiredThread
}

func newRetiredThreads(t *testing.T, resolved bool) retiredThreads {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	notes := uploadIssueDocument(t, handler, issue.Key, "notes.md", "Notes on the census.\n")
	decisions := seedDecisionsDocument(t, database, deps.Docs.(*docs.Service), issue.Key, "# Decisions\n\n## Transport\n\nHTTP it is.\n")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")

	issueComments := "/api/v1/issues/" + issue.Key + "/comments"
	designComments := "/api/v1/artifacts/" + design.ID + "/comments"
	fixture := retiredThreads{handler: handler, database: database}
	for _, thread := range []struct {
		what, document, route, artifact, quote string
	}{
		{"notes.md of the issue", notes.ID, issueComments, notes.ID, "census"},
		{"decisions.md of the issue", decisions, issueComments, "decisions-md", "HTTP it is"},
		{"project document Design notes", design.ID, designComments, design.Slug, "notes"},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, thread.route, map[string]any{
			"body": "Which census?", "anchor": map[string]string{"artifact": thread.artifact, "quote": thread.quote},
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("a comment on a quote of %s: status=%d body=%s", thread.what, response.Code, response.Body.String())
		}
		root := decodeBody[model.Comment](t, response)
		if resolved {
			if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+root.ID+"/resolve", nil, "alice"); response.Code != http.StatusOK {
				t.Fatalf("resolve the thread on %s before its retirement: status=%d body=%s", thread.what, response.Code, response.Body.String())
			}
		}
		retireTestDocument(t, database, thread.document)
		fixture.threads = append(fixture.threads, retiredThread{
			what: thread.what, document: thread.document, root: root, replyRoute: thread.route,
			written: readDocumentWrites(t, database, thread.document),
		})
	}
	return fixture
}

// taken holds that action answered want on thread, and that the thread's retired document holds
// the versions and the live history it held before. It answers whether the action answered want,
// so a test goes on with the other threads past one that failed.
func (f retiredThreads) taken(t *testing.T, thread retiredThread, action string, want int, response *httptest.ResponseRecorder) bool {
	t.Helper()
	if response.Code != want {
		t.Errorf("%s, on the thread in the retired %s: status=%d body=%s, want %d: the thread takes it and its margin record skips the retired document (docs.Service.ProjectMark)",
			action, thread.what, response.Code, response.Body.String(), want)
		return false
	}
	unwritten(t, f.database, thread.document, action+", on the thread in the retired "+thread.what+",", thread.written)
	return true
}

// commentRow is the comment row id as the store holds it: whether it is resolved, and its body.
func (f retiredThreads) commentRow(t *testing.T, id string) (bool, string) {
	t.Helper()
	var resolved bool
	var body string
	if err := f.database.Pool.QueryRow(context.Background(), `select resolved, body from comments where id = $1`, id).Scan(&resolved, &body); err != nil {
		t.Fatalf("read comment %s: %v", id, err)
	}
	return resolved, body
}

func TestAThreadInARetiredDocumentResolvesAndWritesTheDocumentNothing(t *testing.T) {
	t.Parallel()
	f := newRetiredThreads(t, false)
	for _, thread := range f.threads {
		if !f.taken(t, thread, "the resolve", http.StatusOK,
			dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/comments/"+thread.root.ID+"/resolve", nil, "alice")) {
			continue
		}
		if resolved, _ := f.commentRow(t, thread.root.ID); !resolved {
			t.Errorf("after the resolve the thread in the retired %s reads open, want resolved", thread.what)
		}
	}
}

func TestAThreadInARetiredDocumentReopensAndWritesTheDocumentNothing(t *testing.T) {
	t.Parallel()
	f := newRetiredThreads(t, true)
	for _, thread := range f.threads {
		if !f.taken(t, thread, "the reopen", http.StatusOK,
			dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/comments/"+thread.root.ID+"/reopen", nil, "alice")) {
			continue
		}
		if resolved, _ := f.commentRow(t, thread.root.ID); resolved {
			t.Errorf("after the reopen the thread in the retired %s reads resolved, want open", thread.what)
		}
	}
}

func TestACommentOfAThreadInARetiredDocumentTakesAnEditAndWritesTheDocumentNothing(t *testing.T) {
	t.Parallel()
	f := newRetiredThreads(t, false)
	const edited = "Which census, the corpus one?"
	for _, thread := range f.threads {
		if !f.taken(t, thread, "the edit of its root", http.StatusOK,
			dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/comments/"+thread.root.ID, map[string]any{"body": edited}, "alice")) {
			continue
		}
		if _, body := f.commentRow(t, thread.root.ID); body != edited {
			t.Errorf("after the edit the root of the thread in the retired %s reads %q, want %q", thread.what, body, edited)
		}
	}
}

// A reply posts on its thread's owner: an issue's thread on POST /api/v1/issues/{key}/comments, a
// project document's on POST /api/v1/artifacts/{id}/comments.
func TestAReplyToAThreadInARetiredDocumentIsTakenAndWritesTheDocumentNothing(t *testing.T) {
	t.Parallel()
	f := newRetiredThreads(t, false)
	for _, thread := range f.threads {
		response := dispatchRequest(t, f.handler, http.MethodPost, thread.replyRoute, map[string]any{
			"body": "The corpus one.", "reply_to": thread.root.ID,
		}, "alice")
		if !f.taken(t, thread, "a reply through POST "+thread.replyRoute, http.StatusCreated, response) {
			continue
		}
		reply := decodeBody[model.Comment](t, response)
		var replyTo string
		if err := f.database.Pool.QueryRow(context.Background(), `select coalesce(reply_to::text, '') from comments where id = $1`, reply.ID).Scan(&replyTo); err != nil || replyTo != thread.root.ID {
			t.Errorf("the reply to the thread in the retired %s is stored replying to %q (%v), want its root %s", thread.what, replyTo, err, thread.root.ID)
		}
	}
}

// A reword of a block ask writes its `:::ask` block (docs.Service.SetAskBlockText), and a block in
// a retired document takes no new text: the reword is refused as an edit of that document is
// (retiredDocumentRefusal), 409 DOCUMENT_RETIRED naming the document, or 409 DECISIONS_ARE_NODES
// naming the issue's spec for its decisions.md, and not 500. Neither the ask nor the document is
// written.
func TestARewordOfABlockAskInARetiredDocumentIsRefusedAsAnEditOfItIs(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	const notesQuestion, decisionsQuestion = "Should the census print the corpus size?", "Should the census print its run time?"
	notes := uploadIssueDocument(t, handler, issue.Key, "notes.md", "Notes.\n\n"+
		":::ask{#n1 urgency=\"low\" multiple=\"false\"}\n"+notesQuestion+"\n\n- Yes: Print it.\n- No: Leave it out.\n:::\n")
	decisions := seedDecisionsDocument(t, database, documents, issue.Key, "# Decisions\n\n"+
		":::ask{#d1 urgency=\"low\" multiple=\"false\"}\n"+decisionsQuestion+"\n\n- Yes: Print it.\n- No: Leave it out.\n:::\n")
	settleIssue(t, documents, issue.Key)

	for _, reword := range []struct {
		what, document, block, question, askID, code, names string
	}{
		{"notes.md of the issue", notes.ID, "n1", notesQuestion, blockAskID(t, handler, issue.Key, "n1"), "DOCUMENT_RETIRED", notes.ID},
		{"decisions.md of the issue", decisions, "d1", decisionsQuestion, blockAskID(t, handler, issue.Key, "d1"), codeDecisionsAreNodes, "dispatch://" + issue.Key + "/spec"},
	} {
		retireTestDocument(t, database, reword.document)
		before := readDocumentWrites(t, database, reword.document)

		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+reword.askID, map[string]any{
			"question": reword.question + " Say why.",
		}, "alice")
		body := response.Body.String()
		if response.Code != http.StatusConflict || responseCode(t, response) != reword.code || !strings.Contains(body, reword.names) {
			t.Errorf("PATCH /api/v1/asks/%s, the ask's block %s in the retired %s: status=%d body=%s, want 409 %s naming %s",
				reword.askID, reword.block, reword.what, response.Code, body, reword.code, reword.names)
		}
		if ask := readBlockAsk(t, handler, reword.askID); ask.Question != reword.question {
			t.Errorf("after the refused reword the ask of block %s reads %q, want %q", reword.block, ask.Question, reword.question)
		}
		unwritten(t, database, reword.document, "the refused reword of block "+reword.block+" in the retired "+reword.what, before)
	}
}

// retiredRefusal is one write a retired document refuses: what it is, the route it posts on with
// its body, the document, the 409 code it answers and a text that answer names, and what the test
// does just before the request (nil for nothing).
type retiredRefusal struct {
	what, route   string
	body          map[string]any
	document      string
	code, names   string
	beforeRequest func()
}

// refusedOnRetired holds that each write answered its 409 and left its retired document the
// versions and the live history it held.
func refusedOnRetired(t *testing.T, handler http.Handler, database *store.Store, writes []retiredRefusal) {
	t.Helper()
	for _, write := range writes {
		before := readDocumentWrites(t, database, write.document)
		if write.beforeRequest != nil {
			write.beforeRequest()
		}
		response := dispatchRequest(t, handler, http.MethodPost, write.route, write.body, "alice")
		body := response.Body.String()
		if response.Code != http.StatusConflict || responseCode(t, response) != write.code || !strings.Contains(body, write.names) {
			t.Errorf("%s, POST %s: status=%d body=%s, want 409 %s naming %s", write.what, write.route, response.Code, body, write.code, write.names)
		}
		unwritten(t, database, write.document, write.what, before)
	}
}

// A named version writes a version of the document (docs.Service.NamedVersion), and a retired
// document takes none: the version write reads the retirement under the owner's lock it takes and
// refuses it (docs' retiredWriteRefusal), and api answers that refusal as it answers an edit of the
// document (retiredDocumentRefusal), 409 DOCUMENT_RETIRED naming it, or 409 DECISIONS_ARE_NODES
// naming the issue's spec for its decisions.md, on each route that names a version: by the
// document's id, by an issue document's slug and by a project document's slug.
func TestANamedVersionOfARetiredDocumentIsRefusedAndWritesNoVersion(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	notes := uploadIssueDocument(t, handler, issue.Key, "notes.md", "Notes on the census.\n")
	decisions := seedDecisionsDocument(t, database, deps.Docs.(*docs.Service), issue.Key, "# Decisions\n\n## Transport\n\nHTTP it is.\n")
	design := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")
	for _, document := range []string{notes.ID, decisions, design.ID} {
		retireTestDocument(t, database, document)
	}

	named := map[string]any{"summary": "Named once the document was retired."}
	spec := "dispatch://" + issue.Key + "/spec"
	refusedOnRetired(t, handler, database, []retiredRefusal{
		{what: "a named version of the retired notes.md by its id", route: "/api/v1/artifacts/" + notes.ID + "/versions", body: named, document: notes.ID, code: "DOCUMENT_RETIRED", names: notes.ID},
		{what: "a named version of the retired notes.md by its slug", route: "/api/v1/issues/" + issue.Key + "/artifacts/" + notes.Slug + "/versions", body: named, document: notes.ID, code: "DOCUMENT_RETIRED", names: notes.ID},
		{what: "a named version of the retired decisions.md by its id", route: "/api/v1/artifacts/" + decisions + "/versions", body: named, document: decisions, code: codeDecisionsAreNodes, names: spec},
		{what: "a named version of the retired decisions.md by its slug", route: "/api/v1/issues/" + issue.Key + "/artifacts/decisions-md/versions", body: named, document: decisions, code: codeDecisionsAreNodes, names: spec},
		{what: "a named version of the retired project document by its slug", route: "/api/v1/projects/CORE/artifacts/" + design.Slug + "/versions", body: named, document: design.ID, code: "DOCUMENT_RETIRED", names: design.ID},
	})
}

// A rebuild replaces a document's history that does not load with one seed update and, from
// supplied markdown, writes that markdown's version (docs.Service.RebuildDocument), and a retired
// document takes neither: the rebuild reads the retirement under the owner's lock it takes and
// refuses it (docs' retiredWriteRefusal), and api answers 409 DOCUMENT_RETIRED naming the
// document, or 409 DECISIONS_ARE_NODES naming the issue's spec for its decisions.md. Each
// document's stored history is made to fail its next load, the one state a rebuild repairs, so the
// retirement is what refuses it, and the history stays as it was.
func TestARebuildOfARetiredDocumentIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	var broken *invalidLoadOnceStore
	handler, database, deps := newTestServer(t, testServerOptions{
		settle: time.Hour,
		persistence: func(database *store.Store) docs.VersionedStore {
			broken = &invalidLoadOnceStore{VersionedStore: docs.NewPgVersioned(database), invalid: make(map[string]bool)}
			return broken
		},
	})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	notes := uploadIssueDocument(t, handler, issue.Key, "notes.md", "Notes on the census.\n")
	decisions := seedDecisionsDocument(t, database, deps.Docs.(*docs.Service), issue.Key, "# Decisions\n\n## Transport\n\nHTTP it is.\n")
	for _, document := range []string{notes.ID, decisions} {
		retireTestDocument(t, database, document)
	}

	spec := "dispatch://" + issue.Key + "/spec"
	failLoad := func(document string) func() { return func() { broken.failNextLoad(document) } }
	supplied := map[string]any{"markdown": "Rebuilt once the document was retired.\n"}
	refusedOnRetired(t, handler, database, []retiredRefusal{
		{what: "a rebuild of the retired notes.md from its latest version", route: "/api/v1/artifacts/" + notes.ID + "/rebuild", body: map[string]any{}, document: notes.ID, code: "DOCUMENT_RETIRED", names: notes.ID, beforeRequest: failLoad(notes.ID)},
		{what: "a rebuild of the retired notes.md from supplied markdown", route: "/api/v1/artifacts/" + notes.ID + "/rebuild", body: supplied, document: notes.ID, code: "DOCUMENT_RETIRED", names: notes.ID, beforeRequest: failLoad(notes.ID)},
		{what: "a rebuild of the retired decisions.md from supplied markdown", route: "/api/v1/artifacts/" + decisions + "/rebuild", body: supplied, document: decisions, code: codeDecisionsAreNodes, names: spec, beforeRequest: failLoad(decisions)},
	})
}
