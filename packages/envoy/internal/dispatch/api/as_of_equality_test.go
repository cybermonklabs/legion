// as_of_equality_test.go holds every route that answers as_of (readEntry.AsOf, asOfAnswers) to
// what it answered at that time. One world is written through the routes (seedAsOfWorld), so its
// events are the ones the writers write. Every answers route is read once for each caller, the cut
// is taken from the database's clock after those reads, the world is written again after the cut
// (writeAfterTheCut), and every route is read again with as_of at the cut. The second answer must
// be the first one, but for the exceptions the plan's decisions 5, 12 and 14 and the Verification
// document's negative control name (asOfComparison): the fields the answer names absent, the
// posts deleted after the cut, which read as deleted, the document removed after the cut, which is
// absent, the clocks of a window, which count from the cut, and a search that broadened at the
// cut, which answers no results at it.
//
// The rows are one per route of kind answers (asOfEqualityRows), and
// TestEveryAnswersRouteHasAnEqualityRow holds them to the routes' declarations (routeDecl.Read),
// so a route that joins the kind later fails there until it has a row here.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const (
	// asOfProject is the world's project, and asOfOtherProject the second one, whose issue blocks
	// the world's child.
	asOfProject      = "ASOF"
	asOfOtherProject = "ASOG"
	// asOfListenerSession is the session the fake Envoy listener holds live: the world's targeted
	// messages are delivered to it, and it replies. Its answers (GET
	// /api/v1/agents/{session_id}/answers) are the targeted messages the listener took for it.
	asOfListenerSession = "asof-listener"
	// asOfExternalRef is the GitHub issue the parent links at the cut, and the child after it.
	asOfExternalRef = "example/widgets#7"
	asOfExternalURL = "https://github.com/example/widgets/issues/7"
	// asOfKeptPriority is the priority the parent advances at the cut and stops advancing after
	// it; asOfAddedPriority is the one it starts to advance after the cut.
	asOfKeptPriority  = "asof-keep-the-record"
	asOfAddedPriority = "asof-answer-the-past"
	// asOfSearchWord is a word the world's text holds at the cut. asOfBroadenedQuery is two words
	// that no item holds together at the cut, the first held at the cut and the second only by
	// text written after it: at the cut the search broadens to the first word.
	// asOfEditedMessageWord is a word only the text the edited message had at the cut holds: its
	// first body and its body after the cut hold others.
	asOfSearchWord        = "lighthouse"
	asOfBroadenedQuery    = "lighthouse marmalade"
	asOfEditedMessageWord = "plover"
	// asOfOtherPerson is the second person among the callers (asOfRig.callers), the other one
	// newTestServer seeds beside brokerTestPerson, and asOfOtherSession the session their machine
	// login's token acts for. The comment brokerTestPerson keeps to herself (asOfWorld.hidden) is
	// hidden from them.
	asOfOtherPerson  = "bob"
	asOfOtherSession = "ses_f6e5d4c3b2a1"
)

// asOfAgentSession is the session that asks the world's questions and requests its approval, the
// one sessionActor names, and whose conversation holds the agent upload.
var asOfAgentSession = sessionActor()["id"].(string)

// asOfWorld is what seedAsOfWorld wrote: the keys and ids the rows read, and the posts deleted
// after the cut, each with a word only its own body holds.
type asOfWorld struct {
	parent, child, other string
	// The artifacts: the parent's spec, an issue document with versions, the Verification
	// document, the empty document removed after the cut and the project document, and the
	// slugs the rows read them by.
	spec, notes, verification, removed, handbook string
	notesSlug, handbookSlug, uploadSlug          string
	threadRoot, longComment                      string
	delivered, editedMessage                     string
	approval                                     string
	// since is the start of the answers row's own window: a time before the world, within the
	// week the route reads back.
	since string
	// deleted is each post its author deletes after the cut, by id, with the word only its body
	// holds.
	deleted map[string]string
	// What writeAfterTheCut writes to: the asks it answers and closes, and the posts it deletes.
	openQuestion, handbookAsk, deletedComment, deletedMessage string
	// hidden is the comment brokerTestPerson keeps to herself (seedHiddenComment): the world's
	// search word matches it at the cut, and its node is reserved for her, its author.
	hidden string
}

// asOfWrite sends one write of the world as login, by the test identity's header, and fails the
// test unless it is answered 2xx.
func asOfWrite(t *testing.T, handler http.Handler, method, target string, body any, login string) *httptest.ResponseRecorder {
	t.Helper()
	response := dispatchRequest(t, handler, method, target, body, login)
	if response.Code/100 != 2 {
		t.Fatalf("%s %s as %s: status=%d body=%s", method, target, login, response.Code, response.Body.String())
	}
	return response
}

// asOfSessionWrite is asOfWrite as the world's agent session, under the shared agent token with
// the session named in the body.
func asOfSessionWrite(t *testing.T, handler http.Handler, method, target string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body["actor"] = sessionActor()
	response := sessionRequest(t, handler, method, target, body)
	if response.Code/100 != 2 {
		t.Fatalf("%s %s as the agent session: status=%d body=%s", method, target, response.Code, response.Body.String())
	}
	return response
}

// asOfIssue creates an issue and answers its key and its spec's id.
func asOfIssue(t *testing.T, handler http.Handler, body map[string]any) model.Issue {
	t.Helper()
	body["force"] = true
	return decodeBody[model.Issue](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/issues", body, "alice"))
}

// asOfUpload writes a document of owner (an issue's or a project's path) with content, a first
// version for a new name and the next version for a name it holds, which names the token of the
// version it replaces, as every upload that replaces a document must (uploadBaseNamed).
func asOfUpload(t *testing.T, handler http.Handler, owner, name, content string) model.Artifact {
	t.Helper()
	body := map[string]any{"name": name}
	if content != "" {
		body["content"] = content
	}
	if base := uploadBaseNamed(t, handler, owner+"/artifacts", name); base != nil {
		body["precondition"] = base
	}
	return decodeBody[uploadedArtifact](t, asOfWrite(t, handler, http.MethodPost, owner+"/artifacts", body, "alice")).Artifact
}

// seedAsOfWorld writes the world the rows read at the cut, through the routes: two projects whose
// default assignees a settings event names, issues with a parent and a child, labels, a claim, a
// blocked_by link, an advance and an external link; a comment thread with a reply, a comment
// edited before the cut, a comment longer than 32,000 UTF-16 units and one deleted after the cut;
// an issue message delivered to a live session that replies, one edited before the cut and again
// after it, and one deleted after the cut whose reply froze a preview of it; a question answered
// and one left open and snoozed, and an approval request answered with a reply and handed back;
// the spec with two versions, an issue document with two versions, a Verification document with
// a check and a result, and an empty document removed after the cut; a project document with a
// comment and an ask; and an agent upload.
func seedAsOfWorld(t *testing.T, handler http.Handler) *asOfWorld {
	t.Helper()
	w := &asOfWorld{deleted: map[string]string{}, since: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}

	for key, name := range map[string]string{asOfProject: "The as-of world", asOfOtherProject: "The other project"} {
		asOfWrite(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": key, "name": name}, "alice")
	}
	asOfWrite(t, handler, http.MethodPut, "/api/v1/projects/"+asOfProject+"/default-assignee", map[string]any{"assignee": "alice"}, "alice")
	asOfWrite(t, handler, http.MethodPut, "/api/v1/projects/"+asOfOtherProject+"/default-assignee", map[string]any{"assignee": "bob"}, "alice")
	asOfWrite(t, handler, http.MethodPost, "/api/v1/priorities", map[string]any{"id": asOfKeptPriority, "title": "Keep the record"}, "alice")
	asOfWrite(t, handler, http.MethodPost, "/api/v1/priorities", map[string]any{"id": asOfAddedPriority, "title": "Answer the past"}, "alice")

	w.other = asOfIssue(t, handler, map[string]any{
		"project": asOfOtherProject, "title": "The other project's blocker", "spec": "The blocker's spec.\n",
	}).Key
	parent := asOfIssue(t, handler, map[string]any{
		"project": asOfProject, "title": "Read the past as it was", "labels": []string{"history"},
		"spec": "# Spec\n\nThe parent's spec, as first written.\n", "advances": []string{asOfKeptPriority},
	})
	w.parent, w.spec = parent.Key, parent.PrimaryArtifactID
	w.child = asOfIssue(t, handler, map[string]any{
		"project": asOfProject, "title": "A child that waits on the blocker", "parent": w.parent,
		"blocked_by": []string{w.other}, "labels": []string{"child"},
	}).Key
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/issues/"+w.parent,
		map[string]any{"external_links": []map[string]string{{"url": asOfExternalURL}}}, "alice")
	asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.child+"/claim", map[string]any{})

	// The spec's second version, and an issue document with two.
	asOfWrite(t, handler, http.MethodPost, "/api/v1/artifacts/"+w.spec+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "insert", "after": "end", "markdown": "The spec's second paragraph.\n"}},
		"summary": "The second version of the spec",
	}, "alice")
	notes := asOfUpload(t, handler, "/api/v1/issues/"+w.parent, "notes.md", "# Notes\n\nThe notes as first written.\n")
	asOfUpload(t, handler, "/api/v1/issues/"+w.parent, "notes.md", "# Notes\n\nThe notes as written again before the cut.\n")
	w.notes, w.notesSlug = notes.ID, notes.Slug
	w.verification = asOfUpload(t, handler, "/api/v1/issues/"+w.parent, "verification.md", twoChecks).ID
	recordedResult := recordResult(t, handler, w.parent, resultBody("k1", "pass", headCommit, map[string]any{
		"surface": "cli", "run": "go test ./...", "observed": "ok",
	}))
	if recordedResult.Code != http.StatusCreated {
		t.Fatalf("record the check's result: status=%d body=%s", recordedResult.Code, recordedResult.Body.String())
	}
	w.removed = asOfUpload(t, handler, "/api/v1/issues/"+w.parent, "Scratch", "").ID

	// The parent's comments: a thread with a reply, an edit, a long body and a deleted one.
	w.threadRoot = decodeBody[model.Comment](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/comments",
		map[string]any{"body": "The " + asOfSearchWord + " keeps its first log."}, "alice")).ID
	asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/comments",
		map[string]any{"body": "Bob answers the first log.", "reply_to": w.threadRoot}, "bob")
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/comments/"+w.threadRoot,
		map[string]any{"body": "The " + asOfSearchWord + " keeps its first log, corrected before the cut."}, "alice")
	paragraph := strings.Repeat("ledger ", 3000)
	w.longComment = decodeBody[model.Comment](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/comments",
		map[string]any{"body": "A long comment.\n\n" + paragraph + "\n\n" + paragraph}, "alice")).ID
	w.deletedComment = decodeBody[model.Comment](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/comments",
		map[string]any{"body": "A note its author withdraws later: zebrafinch."}, "alice")).ID
	w.deleted[w.deletedComment] = "zebrafinch"

	// The parent's messages: one delivered to a live session that replies, one edited before the
	// cut, and one deleted after the cut that a reply quoted first.
	delivered := decodeBody[model.Message](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/messages", map[string]any{
		"body": "Please check the past reads.", "target": "session:" + asOfListenerSession, "delivery": "btw",
	}, "alice"))
	if len(delivered.Deliveries) != 1 || delivered.Deliveries[0].State != "sent" {
		t.Fatalf("the targeted message's deliveries = %+v, want one sent attempt", delivered.Deliveries)
	}
	w.delivered = delivered.ID
	if reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+w.delivered+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": asOfListenerSession}, "attempt": 1, "body": "Checked: the past reads hold.",
	}); reply.Code != http.StatusCreated {
		t.Fatalf("the session's reply: status=%d body=%s", reply.Code, reply.Body.String())
	}
	w.editedMessage = decodeBody[model.Message](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/messages",
		map[string]any{"body": "The draft as first written: a sandpiper."}, "alice")).ID
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/messages/"+w.editedMessage,
		map[string]any{"body": "The draft as edited before the cut: a " + asOfEditedMessageWord + "."}, "alice")
	w.deletedMessage = decodeBody[model.Message](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/messages",
		map[string]any{"body": "A message its author withdraws later: kingfisher."}, "alice")).ID
	w.deleted[w.deletedMessage] = "kingfisher"
	asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/messages",
		map[string]any{"body": "Bob quotes it before it goes.", "in_reply_to": w.deletedMessage}, "bob")

	// The asks: a question answered, one left open and snoozed, and an approval request the person
	// replies to and the session hands back.
	options := []map[string]string{{"label": "Yes"}, {"label": "No"}}
	question := decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/asks",
		map[string]any{"question": "Read the issue first?", "options": options})).ID
	asOfWrite(t, handler, http.MethodPost, "/api/v1/asks/"+question+"/answer", map[string]any{"selected": []string{"Yes"}}, "alice")
	w.openQuestion = decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/asks",
		map[string]any{"question": "Which cut do the reads test against?"})).ID
	asOfWrite(t, handler, http.MethodPut, "/api/v1/me/asks/"+w.openQuestion+"/snooze",
		map[string]any{"snoozed_until": time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)}, "alice")
	summary := map[string]any{"summary": "Reads the past as it was."}
	requested := decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/artifacts/"+w.spec+"/approval-requests", maps.Clone(summary)))
	w.approval = requested.Ask.ID
	asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/comments",
		map[string]any{"body": "Say which cut the reads answer.", "ask_id": w.approval}, "alice")
	asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/artifacts/"+w.spec+"/approval-requests", maps.Clone(summary))

	// The project document, its comment and its ask, and the agent upload.
	handbook := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/projects/"+asOfProject+"/artifacts", map[string]string{
		"name": "handbook.md", "content": "# Handbook\n\nThe " + asOfSearchWord + " handbook.\n",
	}, "alice")).Artifact
	w.handbook, w.handbookSlug = handbook.ID, handbook.Slug
	asOfWrite(t, handler, http.MethodPost, "/api/v1/artifacts/"+w.handbook+"/comments",
		map[string]any{"body": "The handbook's first remark."}, "alice")
	w.handbookAsk = decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/artifacts/"+w.handbook+"/asks",
		map[string]any{"question": "Does the handbook name the cut?"})).ID
	upload := multipartRequest(t, handler, "/api/v1/agents/"+asOfAgentSession+"/artifacts", map[string]string{"name": "brief.txt"},
		"brief.txt", "text/plain", []byte("The brief as first sent."), "alice")
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload the agent's brief: status=%d body=%s", upload.Code, upload.Body.String())
	}
	w.uploadSlug = decodeBody[uploadedArtifact](t, upload).Artifact.Slug
	return w
}

// seedHiddenComment writes the comment brokerTestPerson keeps to herself (asOfWorld.hidden): a
// comment of hers on the parent that holds the world's search word, written through the route,
// whose node is then reserved for her, as a later release's author-private write marks one. No
// route writes such a node yet, so the store's own writer (graphstore.Write) moves the comment's
// node to the visibility graphstoretest.VisibilityFor answers for author on the credential that
// wrote it (graphstore.ProvesAuthor) and states that attribute beside it, as
// TestAPersonPageDecidesAndListsInOneSnapshot reserves one. Written through the route, the comment
// has the event the keyword table's fill reads, so a search at the cut matches it as a search at
// the present does. Reserved, it is seen by its author alone: the read-only token that holds
// person names her and sees it, and the one that names asOfOtherPerson does not.
//
// It is a comment, and no block of a document, because a keyword search answers a comment as a
// hit of its own, and binds the visibility predicate over it: the search's kinds
// (searchKindsCTE, refs.VisibleEnd) at the present and at the cut, and each post kind's boundary
// at the cut (searchAsOfBoundedPosts, refs.VisibleKind). So the comment is in an answer whole or
// not at all. A block reserved for its author in a document every caller may see would change
// only which block the document's hit names (searchPageAndTailReading).
func (w *asOfWorld) seedHiddenComment(t *testing.T, handler http.Handler, database *store.Store) {
	t.Helper()
	w.hidden = decodeBody[model.Comment](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/comments",
		map[string]any{"body": "The " + asOfSearchWord + " notes its author keeps to herself."}, brokerTestPerson)).ID
	ctx := t.Context()
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		node, found, err := graphstore.ReadNode(ctx, tx, graphstore.KindComment, w.hidden)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("the store holds no node of the comment")
		}
		visibility, err := graphstoretest.VisibilityFor(graphstore.VisibilityAuthor, graphstore.ProvesAuthor(node.MadeBy))
		if err != nil {
			return err
		}
		node.Visibility = visibility
		return graphstore.Write(ctx, tx, []graphstore.Node{node}, nil)
	}); err != nil {
		t.Fatalf("reserve the comment %s for its author, %s: %v", w.hidden, brokerTestPerson, err)
	}
}

// writeAfterTheCut makes every later write of the check t-routes' run text and the two the plan
// adds: edits, new comments, messages, asks and answers, a closed ask, an issue status change, a
// new document version, a new check result, a document removed, a comment and an issue message
// deleted by their author, the issue message edited before the cut edited again, text that
// matches the search queries, an advance asserted and one retracted, and a project's default
// assignee moved.
func (w *asOfWorld) writeAfterTheCut(t *testing.T, handler http.Handler) {
	t.Helper()
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/comments/"+w.threadRoot,
		map[string]any{"body": "The " + asOfSearchWord + " keeps its log, corrected after the cut."}, "alice")
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/comments/"+w.longComment, map[string]any{"body": "The long comment, cut short after the cut."}, "alice")
	asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/comments",
		map[string]any{"body": "A " + asOfBroadenedQuery + " note written after the cut."}, "alice")
	asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/comments",
		map[string]any{"body": "Bob adds to the thread after the cut.", "reply_to": w.threadRoot}, "bob")
	asOfWrite(t, handler, http.MethodDelete, "/api/v1/comments/"+w.deletedComment, nil, "alice")

	asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/messages",
		map[string]any{"body": "A message written after the cut."}, "alice")
	asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/messages", map[string]any{
		"body": "Please check the reads again after the cut.", "target": "session:" + asOfListenerSession, "delivery": "btw",
	}, "alice")
	asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/messages",
		map[string]any{"body": "Bob follows the checked reads up after the cut.", "in_reply_to": w.delivered}, "bob")
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/messages/"+w.editedMessage, map[string]any{"body": "The draft as edited after the cut: a ptarmigan."}, "alice")
	asOfWrite(t, handler, http.MethodDelete, "/api/v1/messages/"+w.deletedMessage, nil, "alice")

	later := decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/issues/"+w.parent+"/asks", map[string]any{
		"question": "Is an ask made after the cut seen at it?", "options": []map[string]string{{"label": "Yes"}, {"label": "No"}},
	})).ID
	asOfWrite(t, handler, http.MethodPost, "/api/v1/asks/"+later+"/answer", map[string]any{"selected": []string{"No"}}, "alice")
	asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/asks/"+w.openQuestion+"/resolve",
		map[string]any{"kind": "resolved", "reason": "Settled after the cut."})
	asOfWrite(t, handler, http.MethodPost, "/api/v1/asks/"+w.approval+"/answer", map[string]any{"selected": []string{"Approve"}}, "alice")
	asOfWrite(t, handler, http.MethodPost, "/api/v1/asks/"+w.handbookAsk+"/answer", map[string]any{"text": "It does now."}, "alice")

	asOfWrite(t, handler, http.MethodPatch, "/api/v1/issues/"+w.child, map[string]any{"status": "in_progress"}, "alice")
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/issues/"+w.parent, map[string]any{"title": "Read the past as it was, retitled after the cut"}, "alice")
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/issues/"+w.parent, map[string]any{"advances": []string{asOfKeptPriority, asOfAddedPriority}}, "alice")
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/issues/"+w.parent, map[string]any{"advances": []string{asOfAddedPriority}}, "alice")
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/issues/"+w.parent, map[string]any{"external_links": []map[string]string{}}, "alice")
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/issues/"+w.child,
		map[string]any{"external_links": []map[string]string{{"url": asOfExternalURL}}}, "alice")
	asOfWrite(t, handler, http.MethodPut, "/api/v1/projects/"+asOfProject+"/default-assignee", map[string]any{"assignee": "bob"}, "alice")

	asOfWrite(t, handler, http.MethodPost, "/api/v1/artifacts/"+w.spec+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "insert", "after": "end", "markdown": "A paragraph the spec gained after the cut.\n"}},
		"summary": "The spec after the cut",
	}, "alice")
	asOfUpload(t, handler, "/api/v1/issues/"+w.parent, "notes.md", "# Notes\n\nThe notes as written after the cut.\n")
	asOfWrite(t, handler, http.MethodPost, "/api/v1/artifacts/"+w.handbook+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "insert", "after": "end", "markdown": "The handbook after the cut.\n"}},
		"summary": "The handbook after the cut",
	}, "alice")
	asOfWrite(t, handler, http.MethodPost, "/api/v1/artifacts/"+w.handbook+"/comments", map[string]any{"body": "A remark after the cut."}, "alice")
	if again := multipartRequest(t, handler, "/api/v1/agents/"+asOfAgentSession+"/artifacts", map[string]string{"name": "brief.txt"},
		"brief.txt", "text/plain", []byte("The brief as sent again after the cut."), "alice"); again.Code != http.StatusCreated {
		t.Fatalf("upload the agent's brief again: status=%d body=%s", again.Code, again.Body.String())
	}
	if recorded := recordResult(t, handler, w.parent, resultBody("k1", "fail", newHeadCommit, map[string]any{"failed": 1})); recorded.Code != http.StatusCreated {
		t.Fatalf("record a result after the cut: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	if removed := removeDocument(t, handler, w.removed); removed.Code != http.StatusNoContent {
		t.Fatalf("remove the empty document: status=%d body=%s", removed.Code, removed.Body.String())
	}
}

// asOfRead is one request of a row: its target, path and query; whether it names the document
// removed after the cut, which answers 404 as of the cut; whether it names what the world holds
// only after the cut, which answers 404 at the cut and so as of it; and whether it is a search
// whose query matches the comment reserved for its author (asOfWorld.hidden) at the cut, which
// must show that comment to its author and hide it from asOfOtherPerson (hiddenShown).
type asOfRead struct {
	target  string
	removed bool
	missing bool
	hidden  bool
}

// asOfRow is one route of kind answers and the reads the harness makes of it. window, where it
// is set, holds the members of the answer that count from the time of the read: it checks them
// against the cut in the as_of answer and takes them out of both answers before they are
// compared.
type asOfRow struct {
	route  string
	reads  func(w *asOfWorld) []asOfRead
	window func(cut asof.Cut, then, asOf any) []string
}

// asOfOne is a row's one read of target.
func asOfOne(target func(w *asOfWorld) string) func(w *asOfWorld) []asOfRead {
	return func(w *asOfWorld) []asOfRead { return []asOfRead{{target: target(w)}} }
}

// asOfSearchReads is a search route's three reads: a query the world's text matched at the cut,
// the comment reserved for its author among it (asOfWorld.hidden); two words that matched nothing
// together at the cut, which the read at the cut broadened; and a word only the edited message's
// text at the cut holds, which the fill reads from its edit (message.edited).
func asOfSearchReads(path string) func(w *asOfWorld) []asOfRead {
	return func(*asOfWorld) []asOfRead {
		return []asOfRead{
			{target: path + "?" + url.Values{"q": {asOfSearchWord}, "keywords_only": {"true"}}.Encode(), hidden: true},
			{target: path + "?" + url.Values{"q": {asOfBroadenedQuery}, "keywords_only": {"true"}}.Encode()},
			{target: path + "?" + url.Values{"q": {asOfEditedMessageWord}, "keywords_only": {"true"}}.Encode()},
		}
	}
}

// asOfEqualityRows is one row per route of kind answers, each with the request's path and query.
var asOfEqualityRows = []asOfRow{
	{route: "GET /api/v1/issues/{key}", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/issues/" + w.parent })},
	{route: "GET /api/v1/issues", reads: asOfOne(func(*asOfWorld) string { return "/api/v1/issues?project=" + asOfProject })},
	{route: "GET /api/v1/issues/resolve", reads: asOfOne(func(*asOfWorld) string {
		return "/api/v1/issues/resolve?" + url.Values{"ref": {asOfExternalRef}}.Encode()
	})},
	{route: "GET /api/v1/issues/{key}/events", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/issues/" + w.parent + "/events" })},
	{route: "GET /api/v1/artifacts/{id}/events", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/artifacts/" + w.handbook + "/events" })},
	{route: "GET /api/v1/events/log", reads: asOfOne(func(*asOfWorld) string { return "/api/v1/events/log?after=0" })},
	{route: "GET /api/v1/issues/{key}/comments", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/issues/" + w.parent + "/comments" })},
	{route: "GET /api/v1/artifacts/{id}/comments", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/artifacts/" + w.handbook + "/comments" })},
	{route: "GET /api/v1/comments/{id}", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/comments/" + w.threadRoot })},
	{route: "GET /api/v1/issues/{key}/messages", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/issues/" + w.parent + "/messages" })},
	{route: "GET /api/v1/comments/{id}/history", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/comments/" + w.threadRoot + "/history" })},
	{route: "GET /api/v1/messages/{id}/history", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/messages/" + w.editedMessage + "/history" })},
	{route: "GET /api/v1/issues/{key}/messages/{id}", reads: asOfOne(func(w *asOfWorld) string {
		return "/api/v1/issues/" + w.parent + "/messages/" + w.editedMessage
	})},
	{route: "GET /api/v1/messages/{id}", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/messages/" + w.delivered })},
	{route: "GET /api/v1/issues/{key}/asks", reads: asOfOne(func(w *asOfWorld) string {
		return "/api/v1/issues/" + w.parent + "/asks?state=all&thread=1"
	})},
	{route: "GET /api/v1/artifacts/{id}/asks", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/artifacts/" + w.handbook + "/asks?state=all" })},
	{route: "GET /api/v1/asks/{id}", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/asks/" + w.approval })},
	{route: "GET /api/v1/issues/{key}/artifacts", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/issues/" + w.parent + "/artifacts" })},
	{route: "GET /api/v1/issues/{key}/artifacts/{slug}", reads: asOfOne(func(w *asOfWorld) string {
		return "/api/v1/issues/" + w.parent + "/artifacts/" + w.notesSlug
	})},
	{route: "GET /api/v1/projects/{key}/artifacts", reads: asOfOne(func(*asOfWorld) string { return "/api/v1/projects/" + asOfProject + "/artifacts" })},
	{route: "GET /api/v1/projects/{key}/artifacts/{slug}", reads: asOfOne(func(w *asOfWorld) string {
		return "/api/v1/projects/" + asOfProject + "/artifacts/" + w.handbookSlug
	})},
	{route: "GET /api/v1/agents/{session_id}/artifacts/{slug}", reads: asOfOne(func(w *asOfWorld) string {
		return "/api/v1/agents/" + asOfAgentSession + "/artifacts/" + w.uploadSlug
	})},
	{route: "GET /api/v1/artifacts/{id}", reads: func(w *asOfWorld) []asOfRead {
		return []asOfRead{{target: "/api/v1/artifacts/" + w.notes}, {target: "/api/v1/artifacts/" + w.removed, removed: true}}
	}},
	{route: "GET /api/v1/artifacts/{id}/text", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/artifacts/" + w.spec + "/text?blocks=true" })},
	{route: "GET /api/v1/artifacts/{id}/versions/{number}", reads: func(w *asOfWorld) []asOfRead {
		return []asOfRead{{target: "/api/v1/artifacts/" + w.notes + "/versions/2"}, {target: "/api/v1/artifacts/" + w.notes + "/versions/3", missing: true}}
	}},
	{route: "GET /api/v1/issues/{key}/artifacts/{slug}/blocks", reads: asOfOne(func(w *asOfWorld) string {
		return "/api/v1/issues/" + w.parent + "/artifacts/" + w.notesSlug + "/blocks"
	})},
	{route: "GET /api/v1/projects/{key}/artifacts/{slug}/blocks", reads: asOfOne(func(w *asOfWorld) string {
		return "/api/v1/projects/" + asOfProject + "/artifacts/" + w.handbookSlug + "/blocks"
	})},
	{route: "GET /api/v1/issues/{key}/results", reads: asOfOne(func(w *asOfWorld) string { return "/api/v1/issues/" + w.parent + "/results" })},
	{route: "GET /api/v1/search", reads: asOfSearchReads("/api/v1/search")},
	{route: "GET /api/v1/search/results", reads: asOfSearchReads("/api/v1/search/results")},
	{route: "GET /api/v1/agents/{session_id}/answers", reads: asOfOne(func(w *asOfWorld) string {
		return "/api/v1/agents/" + asOfListenerSession + "/answers?" + url.Values{"since": {w.since}}.Encode()
	})},
	{route: "GET /api/v1/asks/open", reads: asOfOne(func(*asOfWorld) string { return "/api/v1/asks/open?project=" + asOfProject }), window: openAsksWindow},
	{route: "GET /api/v1/inbox", reads: asOfOne(func(*asOfWorld) string { return "/api/v1/inbox?assignee=me" })},
}

// asOfReader is one credential the rows are read with: what a failure calls it, the token the
// stub broker answers for, whether its credential holds the person scope, and the person it
// names, empty for a credential that names none.
type asOfReader struct {
	name   string
	token  string
	person bool
	login  string
}

// asOfRig is the server under test with the broker that answers its callers' tokens, the key
// that proves them, and, on the harness's own rig (newAsOfRig), the statements each as_of read
// sends (markedStatements: every statement of a request whose context the harness marked,
// countedRead).
type asOfRig struct {
	*brokerTokenRig
	key        *brokerCaller
	statements *markedStatements
}

// newAsOfListener is the Envoy listener the world's targeted message reaches: it holds one live
// session, asOfListenerSession, and takes every send to it.
func newAsOfListener(t *testing.T) string {
	t.Helper()
	const sessions = `[{"session_id":"` + asOfListenerSession + `","title":"reader","capabilities":["aside","btw","steer"]}]`
	var mu sync.Mutex
	sent := 0
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/"+asOfListenerSession:
			writeSessionRecord(t, w, sessions, asOfListenerSession)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			mu.Lock()
			sent++
			envelope := sent
			mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"event_id":"asof-envelope-%d","recipient":%q}`, envelope, asOfListenerSession)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(listener.Close)
	return listener.URL
}

// newAsOfRig is the package's broker-token rig on a traced store (newAsOfRigOn), whose tracer
// records the statements of each read the harness marks.
func newAsOfRig(t *testing.T) *asOfRig {
	t.Helper()
	statements := &markedStatements{}
	rig := newAsOfRigOn(t, storetest.TracedStore(t, storetest.Open(t), statements))
	rig.statements = statements
	return rig
}

// newAsOfRigOn is the package's broker-token rig on database, with the test hooks in (the harness
// settles every document before it reads) and the listener above.
func newAsOfRigOn(t *testing.T, database *store.Store) *asOfRig {
	t.Helper()
	listener := newAsOfListener(t)
	rig := newBrokerTokenRigOn(t, testServerOptions{
		store:     database,
		envoyURL:  listener,
		testHooks: true,
	})
	return &asOfRig{brokerTokenRig: rig, key: newBrokerCaller(t)}
}

// callers are the four credentials every row is read with, each said to the stub broker: a
// read-only token, INFRA-4619's read entry's token (readOnlyToken); a read-only token that holds
// person, the token narrowed from a machine login's (ciphertokentest.NarrowedAnswers), here
// alice's (brokerTestPerson), who wrote the comment the world reserves for its author
// (asOfWorld.hidden); the same token of a second person's machine login, asOfOtherPerson's, from
// whom INFRA-1149's visibility hides that comment; and a caller the read filter at this head
// holds, a session token that names a service and no person, so the person scope refuses it on
// each person route. Each is held to be the credential it is said to be, the person it names
// included, so a row cannot pass for want of one.
func (r *asOfRig) callers(t *testing.T) []asOfReader {
	t.Helper()
	expires := time.Now().Add(time.Hour).Unix()
	person := ciphertokentest.NarrowedAnswers(t, r.key.jkt, brokerTestSession, expires)["machine-login"]
	person.Person = brokerTestPerson
	other := ciphertokentest.NarrowedAnswers(t, r.key.jkt, asOfOtherSession, expires)["machine-login"]
	other.Person = asOfOtherPerson
	answers := []struct {
		caller   asOfReader
		answer   ciphertoken.Introspection
		readOnly bool
	}{
		{asOfReader{name: "a read-only token", token: "asof-read-only"}, readOnlyToken(r.key.jkt), true},
		{asOfReader{name: "a read-only token that holds person", token: "asof-read-only-person", person: true, login: brokerTestPerson}, person, true},
		{asOfReader{name: "a token without person", token: "asof-no-person"}, serviceOnlyToken(r.key.jkt), false},
		{asOfReader{name: "a read-only token that holds person and names " + asOfOtherPerson, token: "asof-read-only-other-person",
			person: true, login: asOfOtherPerson}, other, true},
	}
	callers := make([]asOfReader, 0, len(answers))
	for _, said := range answers {
		r.broker.Says(said.caller.token, said.answer)
		actor, _, err := r.served.brokerTokenActor(r.key.presenting(t, http.MethodGet, "/api/v1/whoami", said.caller.token, nil))
		if err != nil || actor.Credential == nil || actor.Credential.ReadOnly != said.readOnly ||
			actor.Credential.Holds(model.ScopePerson) != said.caller.person || deref(actor.Owner) != said.caller.login || !actor.Credential.Cut.IsZero() {
			t.Fatalf("%s: Dispatch's actor is %+v (%v), want a credential read-only %t, holding person %t, naming the person %q, with no cut",
				said.caller.name, actor, err, said.readOnly, said.caller.person, said.caller.login)
		}
		callers = append(callers, said.caller)
	}
	return callers
}

// cutCaller is a read-only token that holds person and carries cut, said to the stub broker: the
// token narrowed from a machine login's trade with a cut (ciphertokentest.NarrowedAnswers,
// machine-login-with-a-cut), cut at cut and here alice's, so it is alice's read-only token that
// holds person among callers but for its cut (cutTokenAnswer). Dispatch's credential of it is held
// to be read-only, to hold person and to carry cut (sayCutToken).
func (r *asOfRig) cutCaller(t *testing.T, cut asof.Cut) asOfReader {
	t.Helper()
	caller := asOfReader{name: "a read-only token that holds person, cut at the cut", token: "asof-cut", person: true, login: brokerTestPerson}
	sayCutToken(t, r.brokerTokenRig, r.key, caller.token, cut)
	return caller
}

// read sends target as caller, with a fresh proof, and keeps the answer. A counted read's
// statements are the ones markedStatements records.
func (r *asOfRig) read(t *testing.T, caller asOfReader, target string, counted bool) asOfAnswer {
	t.Helper()
	request := r.key.presenting(t, http.MethodGet, target, caller.token, nil)
	if counted {
		request = request.WithContext(context.WithValue(request.Context(), countedRead{}, true))
	}
	response := r.serve(request)
	return asOfAnswer{status: response.Code, header: response.Header().Clone(), body: bytes.Clone(response.Body.Bytes())}
}

// settle finishes everything the writes so far left running before a read: the documents'
// settlements (the quiesce hook), the server's detached work and the term statistics' refresh
// (awaitBackgroundWrites), and the event horizon, at or below which every event is final.
func (r *asOfRig) settle(t *testing.T) {
	t.Helper()
	asOfWrite(t, r.handler, http.MethodPost, "/api/v1/artifacts/_test/quiesce", nil, "alice")
	awaitBackgroundWrites(t, r.served)
	safeHorizon(t, r.served)
}

// cut is the database's clock_timestamp() as a cut.
func (r *asOfRig) cut(t *testing.T) asof.Cut {
	t.Helper()
	return databaseCut(t, r.database)
}

// databaseCut is the cut at the database's clock_timestamp(), rounded down to the microsecond as
// the gate rounds an as_of (asof.CutAt).
func databaseCut(t *testing.T, database *store.Store) asof.Cut {
	t.Helper()
	var now time.Time
	if err := database.Pool.QueryRow(t.Context(), `select clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	return asof.CutAt(now)
}

// withAsOf is target with as_of set to cut.
func withAsOf(t *testing.T, target string, cut asof.Cut) string {
	t.Helper()
	return withAsOfValue(t, target, cut.String())
}

// withAsOfValue is target with as_of set to value as it is written, a time or not.
func withAsOfValue(t *testing.T, target, value string) string {
	t.Helper()
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse %s: %v", target, err)
	}
	query := parsed.Query()
	query.Set(asOfParameter, value)
	parsed.RawQuery = query.Encode()
	return parsed.RequestURI()
}

// TestEveryAnswersRouteAnswersAsOfTheCutWhatItAnsweredThen is the check t-routes: each route of
// kind answers, read with as_of at the cut, answers exactly what it answered at the cut, and
// nothing written after it, for each of four callers. Every read at the cut is made first, once
// the world has settled and the event horizon has reached it; the cut is the database's clock
// after the last of them; 5 ms later the world is written again; and once that has settled, and
// the keyword table's fill has read every event past the cut (fillTextVersionsPast), each route
// is read with as_of at the cut. The as_of read is held to the read at the cut by
// asOfComparison, and every statement it sends is held to the writes a read may make: the DPoP
// proof's replay row, a personal token's last_used_at and the caches Dispatch rebuilds.
//
// Each route is then read once more, with no as_of, by a fifth caller: a read-only token whose
// cut is the harness's cut and that is otherwise alice's read-only token that holds person
// (asOfRig.cutCaller). Its answer is held to that token's read at the cut, as an as_of read is.
//
// Each read at the cut must reach the row's own route (the mux names its pattern) and be served:
// 200 for a caller its scopes admit, 403 OAUTH_SCOPE_REQUIRED for a caller without person on a
// person route, which the as_of read must then be refused alike, and 404 for a read of what only
// the writes after the cut made. And the writes after the cut must change what each row reads at
// the present, for one caller at least, so no row passes because nothing it reads moved.
//
// Before the cut, the world gains a comment of alice's that its search word matches and whose
// node is reserved for her (seedHiddenComment), so INFRA-1149's visibility hides it from one of
// the callers, bob's (asOfOtherPerson). Each search read whose query matches it (asOfRead.hidden)
// must show it to alice's caller and not to bob's, at the cut and as of it (hiddenShown), so a
// search row cannot pass because the comment matched nothing or because no caller had it hidden.
func TestEveryAnswersRouteAnswersAsOfTheCutWhatItAnsweredThen(t *testing.T) {
	rig := newAsOfRig(t)
	world := seedAsOfWorld(t, rig.handler)
	world.seedHiddenComment(t, rig.handler, rig.database)
	callers := rig.callers(t)
	author := slices.IndexFunc(callers, func(caller asOfReader) bool { return caller.login == brokerTestPerson })
	if author < 0 || !slices.ContainsFunc(callers, func(caller asOfReader) bool { return caller.login == asOfOtherPerson }) {
		t.Fatalf("the callers name no %s or no %s, so the comment %s keeps to herself is shown to none of them or hidden from none, and the cut token's reads have no read at the cut to be held to",
			brokerTestPerson, asOfOtherPerson, brokerTestPerson)
	}
	mux, ok := rig.handler.(*http.ServeMux)
	if !ok {
		t.Fatalf("the rig's handler is %T, want the *http.ServeMux register mounted", rig.handler)
	}
	rig.settle(t)

	type readKey struct{ row, read, caller int }
	then := map[readKey]asOfAnswer{}
	for rowIndex, row := range asOfEqualityRows {
		for readIndex, read := range row.reads(world) {
			if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, read.target, nil)); pattern != row.route {
				t.Fatalf("%s: the read %s reaches %q, not the row's route", row.route, read.target, pattern)
			}
			for callerIndex, caller := range callers {
				answer := rig.read(t, caller, read.target, false)
				if problem := servedAtTheCut(row.route, read, caller, answer); problem != "" {
					t.Fatalf("%s as %s, %s: %s", row.route, caller.name, read.target, problem)
				}
				if problem := world.hiddenShown(read, caller, answer); problem != "" {
					t.Fatalf("%s as %s, %s: at the cut, %s", row.route, caller.name, read.target, problem)
				}
				then[readKey{rowIndex, readIndex, callerIndex}] = answer
			}
		}
	}
	cut := rig.cut(t)
	time.Sleep(5 * time.Millisecond)
	world.writeAfterTheCut(t, rig.handler)
	rig.settle(t)
	fillTextVersionsPast(t, rig.database, cut)
	cutCaller := rig.cutCaller(t, cut)
	t.Logf("the cut is %s; %d rows, %d callers and the token cut at the cut; the comment %s reserved for %s",
		cut, len(asOfEqualityRows), len(callers), world.hidden, brokerTestPerson)

	for rowIndex, row := range asOfEqualityRows {
		t.Run(row.route, func(t *testing.T) {
			moved := false
			for readIndex, read := range row.reads(world) {
				for callerIndex, caller := range callers {
					atTheCut := then[readKey{rowIndex, readIndex, callerIndex}]
					moved = moved || movedSinceTheCut(atTheCut, rig.read(t, caller, read.target, false))
					rig.statements.take()
					answer := rig.read(t, caller, withAsOf(t, read.target, cut), true)
					comparison := asOfComparison{route: row.route, cut: cut, world: world, window: row.window, removed: read.removed}
					problems := comparison.problems(atTheCut, answer)
					problems = append(problems, asOfReadWrites(rig.statements.take())...)
					if problem := world.hiddenShown(read, caller, answer); problem != "" {
						problems = append(problems, "as of the cut, "+problem)
					}
					if len(problems) > 0 {
						t.Errorf("%s as %s, %s:\n  %s", row.route, caller.name, read.target, strings.Join(problems, "\n  "))
					}
				}
				rig.statements.take()
				answer := rig.read(t, cutCaller, read.target, true)
				comparison := asOfComparison{route: row.route, cut: cut, world: world, window: row.window, removed: read.removed}
				problems := comparison.problems(then[readKey{rowIndex, readIndex, author}], answer)
				problems = append(problems, asOfReadWrites(rig.statements.take())...)
				if len(problems) > 0 {
					t.Errorf("%s as %s, %s:\n  %s", row.route, cutCaller.name, read.target, strings.Join(problems, "\n  "))
				}
			}
			if !moved {
				t.Errorf("%s: the writes after the cut changed nothing this row reads at the present, so it cannot tell an answer at the cut from the present's", row.route)
			}
		})
	}
}

// hiddenShown says how answer, caller's answer to read at the cut or as of it, fails to show the
// comment reserved for its author (asOfWorld.hidden) to that author, brokerTestPerson, or fails to
// hide it from asOfOtherPerson, and is empty when it does both. It holds nothing of a read whose
// query does not match the comment (asOfRead.hidden) or of a caller that names neither person.
// The comment is shown when the search's page holds a comment hit of its id.
func (w *asOfWorld) hiddenShown(read asOfRead, caller asOfReader, answer asOfAnswer) string {
	if !read.hidden || (caller.login != brokerTestPerson && caller.login != asOfOtherPerson) {
		return ""
	}
	var page model.SearchResponse
	if err := json.Unmarshal(answer.body, &page); answer.status != http.StatusOK || err != nil {
		return fmt.Sprintf("the search answered %d %s (%v), want 200 with a page of hits", answer.status, abbreviateAnswer(answer.body), err)
	}
	shown := slices.ContainsFunc(page.Results, func(hit model.SearchResult) bool { return hit.Kind == "comment" && hit.ID == w.hidden })
	switch author := caller.login == brokerTestPerson; {
	case author && !shown:
		return fmt.Sprintf("the answer lacks the comment %s, which %s keeps to herself and this caller, %s, may see: %s",
			w.hidden, brokerTestPerson, caller.login, abbreviateAnswer(answer.body))
	case !author && shown:
		return fmt.Sprintf("the answer holds the comment %s, which %s keeps to herself and this caller, %s, may not see",
			w.hidden, brokerTestPerson, caller.login)
	}
	return ""
}

// movedSinceTheCut reports whether present, a read after the writes that followed the cut, is not
// then, the same read at the cut, but for the request's own time (took_ms).
func movedSinceTheCut(then, present asOfAnswer) bool {
	if then.status != present.status {
		return true
	}
	thenDoc, thenErr := decodeAsOfBody(then.body)
	presentDoc, presentErr := decodeAsOfBody(present.body)
	if thenErr != nil || presentErr != nil {
		return !bytes.Equal(then.body, present.body)
	}
	dropMember(thenDoc, []string{"took_ms"})
	dropMember(presentDoc, []string{"took_ms"})
	return !reflect.DeepEqual(thenDoc, presentDoc)
}

// servedAtTheCut says how a read at the cut fails to be one a row can compare against, and is
// empty when it is one: 200 for a caller the route's scopes admit, 403 OAUTH_SCOPE_REQUIRED for a
// caller without person on a route that needs it, and 404 for a read of what only the writes
// after the cut made.
func servedAtTheCut(route string, read asOfRead, caller asOfReader, answer asOfAnswer) string {
	needsPerson := slices.Contains(declared(route).OAuthScopes, model.ScopePerson)
	if needsPerson && !caller.person {
		var refusal refusalAnswer
		_ = json.Unmarshal(answer.body, &refusal)
		if answer.status != http.StatusForbidden || refusal.Code != codeScopeRequired {
			return fmt.Sprintf("a caller without person answered %d %s, want 403 %s", answer.status, abbreviateAnswer(answer.body), codeScopeRequired)
		}
		return ""
	}
	want := http.StatusOK
	if read.missing {
		want = http.StatusNotFound
	}
	if answer.status != want {
		return fmt.Sprintf("answered %d %s at the cut, want %d", answer.status, abbreviateAnswer(answer.body), want)
	}
	return ""
}

// asOfAnswer is one answer as the harness keeps it.
type asOfAnswer struct {
	status int
	header http.Header
	body   []byte
}

var (
	// asOfWriteTable is one write of a statement and the table it writes: an insert (the copy
	// markedStatements records as one among them), a delete, a merge, a truncate, or an update,
	// which is read only with its set clause, so that a column named update, as doc_updates
	// holds, is no write, and an upsert's do update is its insert's.
	asOfWriteTable = regexp.MustCompile(`(?is)\b(?:insert\s+into|delete\s+from|merge\s+into|truncate(?:\s+table)?)\s+"?([a-z_][a-z0-9_."]*)` +
		`|\bupdate\s+(?:only\s+)?"?([a-z_][a-z0-9_."]*)"?(?:\s+(?:as\s+)?[a-z_][a-z0-9_]*)?\s+set\b`)
	// lastUsedAtOnly is personalTokenActor's one write, the token's last_used_at.
	lastUsedAtOnly = regexp.MustCompile(`(?is)^\s*update\s+agent_tokens\s+set\s+last_used_at\s*=\s*now\(\)\s+where\b`)
)

// asOfReadWrites names each write of statements that an as_of read may not make: every write but
// the DPoP proof's replay row, a personal token's last_used_at and the caches Dispatch rebuilds
// (text_trees, route_roster). Every read spends a proof, so a trace that holds no insert into
// dpop_proofs did not see the read, and says so.
func asOfReadWrites(statements []string) []string {
	allowed := map[string]bool{"dpop_proofs": true, "text_trees": true, "route_roster": true}
	var problems []string
	proofSpent := false
	for _, statement := range statements {
		for _, match := range asOfWriteTable.FindAllStringSubmatch(statement, -1) {
			table := strings.ToLower(strings.ReplaceAll(match[1]+match[2], `"`, ""))
			if dot := strings.LastIndexByte(table, '.'); dot >= 0 {
				table = table[dot+1:]
			}
			proofSpent = proofSpent || table == "dpop_proofs"
			if allowed[table] || (table == "agent_tokens" && lastUsedAtOnly.MatchString(statement)) {
				continue
			}
			problems = append(problems, "the read wrote "+table+", which a read at a past time may not: "+abbreviateAnswer([]byte(statement)))
		}
	}
	if !proofSpent {
		problems = append(problems, fmt.Sprintf("the trace holds no insert into dpop_proofs among the read's %d statements: it did not see the read", len(statements)))
	}
	return problems
}

// asOfComparison is what one as_of answer of route is held to beside the answer read at the cut.
type asOfComparison struct {
	route   string
	cut     asof.Cut
	world   *asOfWorld
	window  func(cut asof.Cut, then, asOf any) []string
	removed bool
}

// problems says each way asOf, the answer read with as_of at the cut, is not then, the answer
// read at the cut, and is empty when it is. The answer must carry Dispatch-As-Of naming the cut,
// and may name in Dispatch-As-Of-Absent only pointers asof.AbsentFields holds for the route, each
// a member it leaves out. Then the two bodies are compared, once each has been brought to what
// the plan says the as_of answer gives in their place:
//
//   - took_ms, the request's own time, is in neither;
//   - a search that broadened at the cut answers no results, since broadening reads word counts
//     that keep no history (D2);
//   - the document removed after the cut is in neither list, and the as_of answer must not hold
//     it (its removal deleted its versions);
//   - a post deleted after the cut must read as deleted, with its deleted_at and no body, and is
//     compared without its body and its marks; its own events in the event logs carry no body;
//   - the row's window, where it has one, counts from the cut;
//   - each member the answer names absent, where it leaves it out, is in neither.
//
// Everything else must be equal, and the first member that differs is named. No string of the
// as_of answer may hold the word of a post deleted after the cut, but for the previews frozen in
// another post's events (reply_body, reply_block.text).
func (c asOfComparison) problems(then, asOf asOfAnswer) []string {
	if c.removed {
		var refusal refusalAnswer
		_ = json.Unmarshal(asOf.body, &refusal)
		if asOf.status != http.StatusNotFound || refusal.Code != "ARTIFACT_NOT_FOUND" {
			return []string{fmt.Sprintf("the document removed after the cut answers %d %s as of the cut, want 404 ARTIFACT_NOT_FOUND: its removal deleted its versions",
				asOf.status, abbreviateAnswer(asOf.body))}
		}
		return nil
	}
	if asOf.status != then.status {
		return []string{fmt.Sprintf("answers %d %s as of the cut, and answered %d at the cut", asOf.status, abbreviateAnswer(asOf.body), then.status)}
	}
	if then.status != http.StatusOK {
		var thenRefusal, asOfRefusal refusalAnswer
		_ = json.Unmarshal(then.body, &thenRefusal)
		_ = json.Unmarshal(asOf.body, &asOfRefusal)
		if asOfRefusal.Code != thenRefusal.Code {
			return []string{fmt.Sprintf("refuses %s as of the cut, and refused %s at the cut", abbreviateAnswer(asOf.body), abbreviateAnswer(then.body))}
		}
		return nil
	}
	var problems []string
	switch named := asOf.header.Get(contracts.DispatchAsOfHeader); named {
	case c.cut.String():
	case "":
		problems = append(problems, "route answers the present: its answer carries no "+contracts.DispatchAsOfHeader+" header")
	default:
		problems = append(problems, fmt.Sprintf("its %s header is %q, want the cut %s", contracts.DispatchAsOfHeader, named, c.cut))
	}
	absent := absentPointers(asOf.header.Get(contracts.DispatchAsOfAbsentHeader))
	if err := asof.ValidAbsent(c.route, absent); err != nil {
		problems = append(problems, "it names a field absent that the plan's list does not hold: "+err.Error())
	}
	thenDoc, thenErr := decodeAsOfBody(then.body)
	asOfDoc, asOfErr := decodeAsOfBody(asOf.body)
	if thenErr != nil || asOfErr != nil {
		if !bytes.Equal(then.body, asOf.body) {
			problems = append(problems, fmt.Sprintf("its body is %s as of the cut, and was %s at the cut", abbreviateAnswer(asOf.body), abbreviateAnswer(then.body)))
		}
		return problems
	}
	problems = append(problems, c.deletedBodies(asOfDoc)...)
	problems = append(problems, absentButGiven(asOfDoc, absent)...)

	dropMember(thenDoc, []string{"took_ms"})
	dropMember(asOfDoc, []string{"took_ms"})
	broadenedAtTheCut(thenDoc)
	if c.world.removed != "" {
		var held int
		thenDoc, _ = withoutElement(thenDoc, c.world.removed)
		if asOfDoc, held = withoutElement(asOfDoc, c.world.removed); held > 0 {
			problems = append(problems, fmt.Sprintf("it lists the document %s, removed after the cut, %d times: a document removed after the cut is absent", c.world.removed, held))
		}
	}
	problems = append(problems, c.deletedPosts(thenDoc, asOfDoc, absent)...)
	if c.window != nil {
		problems = append(problems, c.window(c.cut, thenDoc, asOfDoc)...)
	}
	dropAbsent(thenDoc, asOfDoc, absent)
	if difference, found := firstDifference("", thenDoc, asOfDoc); found {
		problems = append(problems, difference)
	}
	return problems
}

// decodeAsOfBody is body as JSON, its numbers kept as written.
func decodeAsOfBody(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, fmt.Errorf("more than one JSON value")
	}
	return value, nil
}

// absentPointers is a Dispatch-As-Of-Absent header's pointers, none for an empty header.
func absentPointers(header string) []string {
	var pointers []string
	for _, pointer := range strings.Split(header, ",") {
		if pointer = strings.TrimSpace(pointer); pointer != "" {
			pointers = append(pointers, pointer)
		}
	}
	return pointers
}

// pointerTokens is an RFC 6901 pointer's reference tokens, unescaped.
func pointerTokens(pointer string) []string {
	if pointer == "" {
		return nil
	}
	tokens := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for index, token := range tokens {
		tokens[index] = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
	}
	return tokens
}

// pointerJoin is path with key appended as one escaped reference token.
func pointerJoin(path, key string) string {
	return path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

// eachParent calls visit with each object of value the tokens reach, and the concrete pointer to
// it. A token * reaches every member of an array; every other token reaches the member of an
// object it names, or the element of an array its number names.
func eachParent(value any, tokens []string, path string, visit func(path string, parent map[string]any)) {
	if len(tokens) == 0 {
		if object, ok := value.(map[string]any); ok {
			visit(path, object)
		}
		return
	}
	token, rest := tokens[0], tokens[1:]
	switch node := value.(type) {
	case map[string]any:
		if member, ok := node[token]; ok {
			eachParent(member, rest, pointerJoin(path, token), visit)
		}
	case []any:
		if token == "*" {
			for index, element := range node {
				eachParent(element, rest, pointerJoin(path, strconv.Itoa(index)), visit)
			}
			return
		}
		if index, err := strconv.Atoi(token); err == nil && index >= 0 && index < len(node) {
			eachParent(node[index], rest, pointerJoin(path, token), visit)
		}
	}
}

// pointerMatches reports whether pointer, whose * stands for every member of an array, names the
// concrete pointer path.
func pointerMatches(pointer, path string) bool {
	want, got := pointerTokens(pointer), pointerTokens(path)
	if len(want) != len(got) {
		return false
	}
	for index := range want {
		if want[index] == "*" {
			if _, err := strconv.Atoi(got[index]); err == nil {
				continue
			}
			return false
		}
		if want[index] != got[index] {
			return false
		}
	}
	return true
}

// absentButGiven names each pointer the answer names absent and gives at every place it reaches:
// an answer names a field absent only where it leaves it out (decision 5).
func absentButGiven(doc any, pointers []string) []string {
	var problems []string
	for _, pointer := range pointers {
		tokens := pointerTokens(pointer)
		if len(tokens) == 0 {
			continue
		}
		reached, given := 0, 0
		eachParent(doc, tokens[:len(tokens)-1], "", func(_ string, parent map[string]any) {
			reached++
			if _, ok := parent[tokens[len(tokens)-1]]; ok {
				given++
			}
		})
		if reached > 0 && given == reached {
			problems = append(problems, fmt.Sprintf("it names %s absent, and gives it at each of the %d places it reaches", pointer, reached))
		}
	}
	return problems
}

// dropAbsent takes each member a pointer of pointers reaches out of then at each place where asOf
// leaves it out. A member the as_of answer gives at a place a pointer reaches is compared there as
// every other member is.
func dropAbsent(then, asOf any, pointers []string) {
	for _, pointer := range pointers {
		tokens := pointerTokens(pointer)
		if len(tokens) == 0 {
			continue
		}
		key := tokens[len(tokens)-1]
		given := map[string]bool{}
		eachParent(asOf, tokens[:len(tokens)-1], "", func(path string, parent map[string]any) {
			_, given[path] = parent[key]
		})
		eachParent(then, tokens[:len(tokens)-1], "", func(path string, parent map[string]any) {
			if !given[path] {
				delete(parent, key)
			}
		})
	}
}

// broadenedAtTheCut makes a search answer that broadened at the cut the answer the plan gives as
// of it: a query that matched nothing at the cut answers no results, since broadening picks words
// by counts of the present text (search_term_stats), which keep no history (D2). query.broadened
// itself is named absent, which dropAbsent reads.
func broadenedAtTheCut(doc any) {
	root, ok := doc.(map[string]any)
	if !ok {
		return
	}
	query, _ := root["query"].(map[string]any)
	if broadened, _ := query["broadened"].(bool); !broadened {
		return
	}
	root["results"] = []any{}
	root["total"] = json.Number("0")
	root["reachable"] = json.Number("0")
}

// withoutElement is value with every element of every array that is an object whose id is id
// left out, and how many it left out.
func withoutElement(value any, id string) (any, int) {
	switch node := value.(type) {
	case map[string]any:
		count := 0
		for key, member := range node {
			var dropped int
			node[key], dropped = withoutElement(member, id)
			count += dropped
		}
		return node, count
	case []any:
		kept := make([]any, 0, len(node))
		count := 0
		for _, element := range node {
			if object, ok := element.(map[string]any); ok && object["id"] == id {
				count++
				continue
			}
			element, dropped := withoutElement(element, id)
			count += dropped
			kept = append(kept, element)
		}
		return kept, count
	}
	return value, 0
}

// eachPost calls post with each comment or message object of value, and event with each event of
// a kind that carries a post's body (postBodyEventKinds) with its payload and the post it names,
// each with its concrete pointer. An event's payload is the event's copy of the post as it was
// written: it is read as the event, never as a post.
func eachPost(value any, path string, post func(path string, object map[string]any), event func(path string, payload map[string]any, id string)) {
	switch node := value.(type) {
	case map[string]any:
		if payload, id := eventPayload(node); payload != nil {
			event(path, payload, id)
			for _, key := range slices.Sorted(maps.Keys(payload)) {
				eachPost(payload[key], pointerJoin(pointerJoin(path, "payload"), key), post, event)
			}
			return
		}
		if isPostObject(node) {
			post(path, node)
		}
		for _, key := range slices.Sorted(maps.Keys(node)) {
			eachPost(node[key], pointerJoin(path, key), post, event)
		}
	case []any:
		for index, element := range node {
			eachPost(element, pointerJoin(path, strconv.Itoa(index)), post, event)
		}
	}
}

// eventPayload is object's payload and the post it names when object is an event of a kind that
// carries a post's body (postBodyEventKinds), and nil otherwise.
func eventPayload(object map[string]any) (map[string]any, string) {
	kind, _ := object["type"].(string)
	if _, carries := postBodyEventKinds[kind]; !carries {
		return nil, ""
	}
	payload, _ := object["payload"].(map[string]any)
	id, _ := payload["id"].(string)
	return payload, id
}

// deletedPosts holds each post deleted after the cut to reading as deleted in asOf, and takes its
// body and its marks out of both answers, which compares the rest of it. As of the cut the post
// must carry a deleted_at and no body but an empty one; so must each event of the post's own (its
// creation, an edit), whose payload carries no body at all. A body the answer leaves out is named
// absent (decision 14, the reason T of the plan's list). Another post's event is no event of the
// post, and keeps the preview it froze.
func (c asOfComparison) deletedPosts(then, asOf any, absent []string) []string {
	var problems []string
	named := func(path string) bool {
		return slices.ContainsFunc(absent, func(pointer string) bool { return pointerMatches(pointer, pointerJoin(path, "body")) })
	}
	eachPost(asOf, "", func(path string, object map[string]any) {
		id, _ := object["id"].(string)
		if c.world.deleted[id] == "" {
			return
		}
		body, holdsBody := object["body"]
		switch {
		case object["deleted_at"] == nil:
			problems = append(problems, fmt.Sprintf("%s is post %s, deleted after the cut, and reads as standing as of the cut (no deleted_at)", orRoot(path), id))
		case holdsBody && body != "":
			problems = append(problems, fmt.Sprintf("%s is post %s, deleted after the cut, and gives a body as of the cut", orRoot(path), id))
		case !holdsBody && !named(path):
			problems = append(problems, fmt.Sprintf("%s is post %s, deleted after the cut, and leaves out its body without naming %s in %s",
				orRoot(path), id, pointerJoin(path, "body"), contracts.DispatchAsOfAbsentHeader))
		}
	}, func(path string, payload map[string]any, id string) {
		if c.world.deleted[id] == "" {
			return
		}
		_, holdsBody := payload["body"]
		switch {
		case payload["deleted_at"] == nil || holdsBody:
			problems = append(problems, fmt.Sprintf("%s is an event of post %s, deleted after the cut, and reads as of the cut with its body or without its deleted_at", orRoot(path), id))
		case !named(pointerJoin(path, "payload")):
			problems = append(problems, fmt.Sprintf("%s is an event of post %s, deleted after the cut, and leaves out its body without naming %s in %s",
				orRoot(path), id, pointerJoin(pointerJoin(path, "payload"), "body"), contracts.DispatchAsOfAbsentHeader))
		}
	})
	for _, doc := range []any{then, asOf} {
		eachPost(doc, "", func(_ string, object map[string]any) {
			if id, _ := object["id"].(string); c.world.deleted[id] != "" {
				for _, key := range []string{"body", "deleted_at", "deleted_by"} {
					delete(object, key)
				}
			}
		}, func(_ string, payload map[string]any, id string) {
			if c.world.deleted[id] != "" {
				for _, key := range []string{"body", "body_truncated", "suggestion", "deleted_at", "deleted_by"} {
					delete(payload, key)
				}
			}
		})
	}
	return problems
}

// isPostObject reports whether object is a comment or a message as a read gives it: it names its
// author and carries a body or a deleted_at.
func isPostObject(object map[string]any) bool {
	_, author := object["author"]
	_, body := object["body"]
	_, deletedAt := object["deleted_at"]
	return author && (body || deletedAt)
}

// deletedBodies names each string of doc that holds the word of a post deleted after the cut,
// but for a preview frozen in another post's event: the reply_body and the reply_block.text of
// its payload, which the delete leaves as written.
func (c asOfComparison) deletedBodies(doc any) []string {
	var problems []string
	var walk func(value any, path string, event map[string]any, relative []string)
	walk = func(value any, path string, event map[string]any, relative []string) {
		switch node := value.(type) {
		case map[string]any:
			if _, ok := node["payload"].(map[string]any); ok {
				if _, ok := node["type"].(string); ok {
					event, relative = node, nil
				}
			}
			for _, key := range slices.Sorted(maps.Keys(node)) {
				walk(node[key], pointerJoin(path, key), event, append(slices.Clone(relative), key))
			}
		case []any:
			for index, element := range node {
				walk(element, pointerJoin(path, strconv.Itoa(index)), event, append(slices.Clone(relative), strconv.Itoa(index)))
			}
		case string:
			for id, word := range c.world.deleted {
				if !strings.Contains(node, word) {
					continue
				}
				if event != nil && frozenPreview(relative) {
					if payload, owner := event["payload"].(map[string]any); owner && payload["id"] != id {
						continue
					}
				}
				problems = append(problems, fmt.Sprintf("%s gives part of the body of post %s, deleted after the cut", orRoot(path), id))
			}
		}
	}
	walk(doc, "", nil, nil)
	return problems
}

// frozenPreview reports whether relative, a path inside an event, is a preview of another post
// that the event froze.
func frozenPreview(relative []string) bool {
	return slices.Equal(relative, []string{"payload", "reply_body"}) || slices.Equal(relative, []string{"payload", "reply_block", "text"})
}

// openAsksWindow holds GET /api/v1/asks/open to its clocks at the cut: its as_of is the cut, and
// each ask's age_seconds counts from the cut (decision 12). Both are then taken out of both
// answers, whose reads at the cut counted from their own time.
func openAsksWindow(cut asof.Cut, then, asOf any) []string {
	var problems []string
	root, _ := asOf.(map[string]any)
	if named, _ := root["as_of"].(string); !namesInstant(named, cut.Time()) {
		problems = append(problems, fmt.Sprintf("/as_of is %v as of the cut, want the cut %s: open asks count from the cut", root["as_of"], cut))
	}
	asks, _ := root["asks"].([]any)
	for index, element := range asks {
		ask, _ := element.(map[string]any)
		created, err := time.Parse(time.RFC3339Nano, fmt.Sprint(ask["created_at"]))
		if err != nil {
			problems = append(problems, fmt.Sprintf("/asks/%d/created_at is %v, which is no time", index, ask["created_at"]))
			continue
		}
		want := max(0, (cut.Time().UnixMicro()-created.UnixMicro())/int64(time.Second/time.Microsecond))
		if fmt.Sprint(ask["age_seconds"]) != strconv.FormatInt(want, 10) {
			problems = append(problems, fmt.Sprintf("/asks/%d/age_seconds is %v as of the cut, want %d: an ask's age counts from the cut", index, ask["age_seconds"], want))
		}
	}
	for _, doc := range []any{then, asOf} {
		dropMember(doc, []string{asOfParameter})
		eachParent(doc, []string{"asks", "*"}, "", func(_ string, ask map[string]any) { delete(ask, "age_seconds") })
	}
	return problems
}

// namesInstant reports whether raw is an RFC 3339 time at instant.
func namesInstant(raw string, instant time.Time) bool {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	return err == nil && parsed.Equal(instant)
}

// firstDifference names the first member of got, the as_of answer, that is not want's, the answer
// at the cut, in key order, and whether there is one.
func firstDifference(path string, want, got any) (string, bool) {
	switch wanted := want.(type) {
	case map[string]any:
		gotObject, ok := got.(map[string]any)
		if !ok {
			break
		}
		keys := slices.Sorted(maps.Keys(wanted))
		for key := range gotObject {
			if _, ok := wanted[key]; !ok {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		for _, key := range slices.Compact(keys) {
			wantMember, wantHeld := wanted[key]
			gotMember, gotHeld := gotObject[key]
			member := pointerJoin(path, key)
			switch {
			case !gotHeld:
				return fmt.Sprintf("%s is not in the answer as of the cut, and was %s at the cut", member, abbreviateValue(wantMember)), true
			case !wantHeld:
				return fmt.Sprintf("%s is %s as of the cut, and was not in the answer at the cut", member, abbreviateValue(gotMember)), true
			}
			if difference, found := firstDifference(member, wantMember, gotMember); found {
				return difference, true
			}
		}
		return "", false
	case []any:
		gotArray, ok := got.([]any)
		if !ok {
			break
		}
		for index := range min(len(wanted), len(gotArray)) {
			if difference, found := firstDifference(pointerJoin(path, strconv.Itoa(index)), wanted[index], gotArray[index]); found {
				return difference, true
			}
		}
		if len(wanted) != len(gotArray) {
			return fmt.Sprintf("%s holds %d elements as of the cut, and held %d at the cut", orRoot(path), len(gotArray), len(wanted)), true
		}
		return "", false
	}
	if reflect.DeepEqual(want, got) {
		return "", false
	}
	return fmt.Sprintf("%s is %s as of the cut, and was %s at the cut", orRoot(path), abbreviateValue(got), abbreviateValue(want)), true
}

// orRoot is path, or "the answer" for the pointer to the whole answer.
func orRoot(path string) string {
	if path == "" {
		return "the answer"
	}
	return path
}

// abbreviateValue is value as JSON, cut to a length a failure line can hold.
func abbreviateValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return abbreviateAnswer(encoded)
}

// abbreviateAnswer is body cut to a length a failure line can hold.
func abbreviateAnswer(body []byte) string {
	const most = 240
	text := strings.TrimSpace(string(body))
	if len(text) <= most {
		return text
	}
	return text[:most] + fmt.Sprintf("… (%d bytes)", len(text))
}

// TestEveryAnswersRouteHasAnEqualityRow holds the harness's rows to the routes of kind answers
// their declarations state (declaredReads), both ways and each once: a route that answers as_of
// with no row, and a row whose route does not answer as_of, each fail by name.
func TestEveryAnswersRouteHasAnEqualityRow(t *testing.T) {
	rows := map[string]bool{}
	var problems []string
	for _, row := range asOfEqualityRows {
		if rows[row.route] {
			problems = append(problems, row.route+": the harness holds two equality rows for it")
		}
		rows[row.route] = true
	}
	answers := map[string]bool{}
	for name, entry := range declaredReads() {
		if entry.AsOf == asOfAnswers {
			answers[name] = true
		}
	}
	if len(answers) == 0 {
		t.Fatal("no route's declaration states kind asOfAnswers, so the rows are held to nothing")
	}
	for name := range answers {
		if !rows[name] {
			problems = append(problems, name+": it answers as_of, and the harness holds no equality row for it")
		}
	}
	for name := range rows {
		if !answers[name] {
			problems = append(problems, name+": the harness holds an equality row for it, and its read class's AsOf is "+asOfKindName(declared(name).Read.AsOf))
		}
	}
	slices.Sort(problems)
	t.Logf("%d equality rows; the declarations state %d routes of kind asOfAnswers", len(rows), len(answers))
	if len(problems) > 0 {
		t.Errorf("the equality rows differ from the routes of kind answers in %d ways:\n%s", len(problems), strings.Join(problems, "\n"))
	}
}

// TestAWindowCountsFromTheCut: a window a route counts back from now counts back from the cut
// (decision 12). The cut is taken after an open ask and a message the listener took for its
// session, and the reads run more than a second later, after a second such message, so a window
// counted from now differs from one counted from the cut. GET /api/v1/asks/open answers as_of the
// cut and each ask's age_seconds from the cut. GET /api/v1/agents/{session_id}/answers takes
// since as the cut less 72 hours when the request names none and lists the message sent before
// the cut and not the one after; it takes a since a second less than seven days before the cut,
// which is more than seven days before now, and refuses one a second more than seven days before
// the cut 400; and it answers no items for a since after the cut, where the present read lists the
// later message, since nothing after the cut exists at it.
func TestAWindowCountsFromTheCut(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: newAsOfListener(t)})
	const project = "ASOW"
	asOfWrite(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": "The window"}, "alice")
	issue := asOfIssue(t, handler, map[string]any{"project": project, "title": "Windows count from the cut"}).Key
	send := func(body string) string {
		t.Helper()
		sent := decodeBody[model.Message](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/messages", map[string]any{
			"body": body, "target": "session:" + asOfListenerSession, "delivery": "btw",
		}, "alice"))
		if len(sent.Deliveries) != 1 || sent.Deliveries[0].State != "sent" {
			t.Fatalf("the message %q's deliveries = %+v, want one sent attempt", body, sent.Deliveries)
		}
		return sent.ID
	}
	open := decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/asks",
		map[string]any{"question": "Is the window counted from the cut?"})).ID
	before := send("Sent before the cut.")
	awaitBackgroundWrites(t, served)
	safeHorizon(t, served)
	cut := databaseCut(t, database)
	time.Sleep(2 * time.Second)
	after := send("Sent after the cut.")
	awaitBackgroundWrites(t, served)
	safeHorizon(t, served)

	read := func(t *testing.T, target string) *httptest.ResponseRecorder {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, withAsOf(t, target, cut), nil, "alice")
		if response.Code == http.StatusOK && response.Header().Get(contracts.DispatchAsOfHeader) != cut.String() {
			t.Errorf("GET %s answered 200 with %s %q, want the cut %s: the route answers the present",
				target, contracts.DispatchAsOfHeader, response.Header().Get(contracts.DispatchAsOfHeader), cut)
		}
		return response
	}
	answers := "/api/v1/agents/" + asOfListenerSession + "/answers"
	listed := func(t *testing.T, response *httptest.ResponseRecorder) (time.Time, []string) {
		t.Helper()
		answer := decodeBody[struct {
			Since time.Time `json:"since"`
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}](t, response)
		ids := []string{}
		for _, item := range answer.Items {
			ids = append(ids, item.ID)
		}
		return answer.Since, ids
	}

	t.Run("open asks", func(t *testing.T) {
		response := read(t, "/api/v1/asks/open?project="+project)
		if response.Code != http.StatusOK {
			t.Fatalf("open asks as of the cut: status=%d body=%s", response.Code, response.Body.String())
		}
		answer := decodeBody[struct {
			AsOf string `json:"as_of"`
			Asks []struct {
				ID         string    `json:"id"`
				CreatedAt  time.Time `json:"created_at"`
				AgeSeconds int64     `json:"age_seconds"`
			} `json:"asks"`
		}](t, response)
		if !namesInstant(answer.AsOf, cut.Time()) {
			t.Errorf("as_of = %s, want the cut %s", answer.AsOf, cut)
		}
		var ids []string
		for _, ask := range answer.Asks {
			want := max(0, (cut.Time().UnixMicro()-ask.CreatedAt.UnixMicro())/int64(time.Second/time.Microsecond))
			if ask.AgeSeconds != want {
				t.Errorf("ask %s: age_seconds = %d, want %d, its age at the cut", ask.ID, ask.AgeSeconds, want)
			}
			ids = append(ids, ask.ID)
		}
		if !slices.Equal(ids, []string{open}) {
			t.Errorf("open asks as of the cut = %v, want the one ask open then, %s", ids, open)
		}
	})
	t.Run("the answers' default since", func(t *testing.T) {
		response := read(t, answers)
		if response.Code != http.StatusOK {
			t.Fatalf("answers as of the cut: status=%d body=%s", response.Code, response.Body.String())
		}
		since, ids := listed(t, response)
		if want := cut.Time().Add(-sessionAnswersWindow); !since.Equal(want) {
			t.Errorf("since = %s, want the cut less 72 hours, %s", since.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
		}
		if !slices.Equal(ids, []string{before}) {
			t.Errorf("answers as of the cut = %v, want the one message sent before it, %s", ids, before)
		}
	})
	t.Run("a since a week before the cut", func(t *testing.T) {
		week := cut.Time().Add(-sessionAnswersOldest)
		taken := read(t, answers+"?"+url.Values{"since": {week.Add(time.Second).Format(time.RFC3339Nano)}}.Encode())
		if taken.Code != http.StatusOK {
			t.Errorf("a since a second inside the week before the cut: status=%d body=%s, want 200: the week counts back from the cut",
				taken.Code, taken.Body.String())
		}
		refused := read(t, answers+"?"+url.Values{"since": {week.Add(-time.Second).Format(time.RFC3339Nano)}}.Encode())
		if refused.Code != http.StatusBadRequest {
			t.Errorf("a since a second before the week before the cut: status=%d body=%s, want 400", refused.Code, refused.Body.String())
		}
	})
	t.Run("a since after the cut answers no items", func(t *testing.T) {
		target := answers + "?" + url.Values{"since": {cut.Time().Add(time.Second).Format(time.RFC3339Nano)}}.Encode()
		present := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if present.Code != http.StatusOK {
			t.Fatalf("a since after the cut at the present: status=%d body=%s", present.Code, present.Body.String())
		}
		if _, ids := listed(t, present); !slices.Equal(ids, []string{after}) {
			t.Fatalf("a since after the cut lists %v at the present, want the message sent after the cut, %s", ids, after)
		}
		response := read(t, target)
		if response.Code != http.StatusOK {
			t.Fatalf("a since after the cut: status=%d body=%s, want 200 with no items", response.Code, response.Body.String())
		}
		if _, ids := listed(t, response); len(ids) != 0 {
			t.Errorf("a since after the cut answers %v as of the cut, want no items: nothing after the cut exists at it", ids)
		}
	})
}

// TestThePastTimeIssueListingRefusesAFilterWithNoHistory: the issue listing serves route_status
// and pinned at the present, and refuses each with a cut 400 AS_OF_NOT_SERVED naming the
// parameter, since the route roster and a person's pins keep no history (decision 5).
func TestThePastTimeIssueListingRefusesAFilterWithNoHistory(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	const project = "ASOL"
	asOfWrite(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": "The listing"}, "alice")
	asOfIssue(t, handler, map[string]any{"project": project, "title": "An issue the listing lists"})
	safeHorizon(t, served)
	cut := databaseCut(t, database)
	for _, filter := range []struct{ parameter, value string }{
		{"route_status", model.RouteNoHolder},
		{"pinned", "true"},
	} {
		t.Run(filter.parameter, func(t *testing.T) {
			target := "/api/v1/issues?" + url.Values{"project": {project}, filter.parameter: {filter.value}}.Encode()
			if present := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"); present.Code != http.StatusOK {
				t.Fatalf("%s at the present: status=%d body=%s, want 200: the listing serves the filter", target, present.Code, present.Body.String())
			}
			past := dispatchRequest(t, handler, http.MethodGet, withAsOf(t, target, cut), nil, "alice")
			var refusal refusalAnswer
			_ = json.Unmarshal(past.Body.Bytes(), &refusal)
			if past.Code != http.StatusBadRequest || refusal.Code != contracts.DispatchAsOfNotServed || !strings.Contains(refusal.Error, filter.parameter) {
				t.Errorf("%s as of the cut: status=%d body=%s, want 400 %s naming %s",
					target, past.Code, past.Body.String(), contracts.DispatchAsOfNotServed, filter.parameter)
			}
		})
	}
}

// TestAPastTimeThreadListRefusesAPageOfThreads: the three issue thread lists answer a page of
// threads (?order=activity) at the present, and refuse it at a cut 400 AS_OF_NOT_SERVED naming
// order (refuseThreadPageAtCut), before the list parses the page or reads anything. A page whose
// limit the present refuses 400 INVALID_QUERY is refused at the cut for its order all the same,
// and neither request at the cut runs a statement but the as_of gate's read of the database's
// clock, so the list's handler read nothing. The same list at the cut with no order answers 200.
func TestAPastTimeThreadListRefusesAPageOfThreads(t *testing.T) {
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{
		store: traced,
		// The caller's identity is resolved on the untraced pool, so the count is the request's
		// own statements and not the sign-in lookup every request makes.
		identity: headerIdentity(database),
		settle:   time.Hour,
	})
	const project = "ASOP"
	asOfWrite(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": "The thread pages"}, "alice")
	issue := asOfIssue(t, handler, map[string]any{"project": project, "title": "An issue whose threads are paged"})
	safeHorizon(t, served)
	cut := databaseCut(t, database)
	for _, list := range []string{"asks", "comments", "messages"} {
		t.Run(list, func(t *testing.T) {
			base := "/api/v1/issues/" + issue.Key + "/" + list
			page := base + "?" + url.Values{"order": {"activity"}}.Encode()
			if present := listRead(t, handler, page, listReadOptions{}); present.Code != http.StatusOK || !strings.Contains(present.Body.String(), `"threads"`) {
				t.Fatalf("%s at the present: status=%d body=%s, want 200 with a page of threads", page, present.Code, present.Body.String())
			}
			unparsed := base + "?" + url.Values{"order": {"activity"}, "limit": {"0"}}.Encode()
			if present := listRead(t, handler, unparsed, listReadOptions{}); present.Code != http.StatusBadRequest {
				t.Fatalf("%s at the present: status=%d body=%s, want 400: the page's limit is out of range", unparsed, present.Code, present.Body.String())
			}
			for _, target := range []string{page, unparsed} {
				counter.take()
				past := listRead(t, handler, withAsOf(t, target, cut), listReadOptions{counted: true})
				counted := counter.take()
				var refusal refusalAnswer
				_ = json.Unmarshal(past.Body.Bytes(), &refusal)
				if past.Code != http.StatusBadRequest || refusal.Code != contracts.DispatchAsOfNotServed || !strings.HasPrefix(refusal.Error, "order: ") {
					t.Errorf("%s as of the cut: status=%d body=%s, want 400 %s naming order",
						target, past.Code, past.Body.String(), contracts.DispatchAsOfNotServed)
				}
				ran := slices.DeleteFunc(counted, func(statement string) bool { return strings.TrimSpace(statement) == "select now()" })
				if len(ran) > 0 {
					t.Errorf("%s as of the cut ran %d statements besides the as_of gate's clock read, want none: the list read before it refused the page:\n%s",
						target, len(ran), strings.Join(roundTripLines(ran), "\n"))
				}
			}
			if past := listRead(t, handler, withAsOf(t, base, cut), listReadOptions{}); past.Code != http.StatusOK {
				t.Errorf("%s as of the cut: status=%d body=%s, want 200: the list with no order answers at a cut", base, past.Code, past.Body.String())
			}
		})
	}
}
