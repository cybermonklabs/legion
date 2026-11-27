// post_corrections_party_references_test.go holds an edit and a delete of a post to the rule that
// a mention is a reference (R4 of this change's spec). Who a post names is read out of its
// body by the shared reader, so an edit changes who it names and sends nothing: the delivery
// ledger (comment_mentions and comment_deliveries) keeps what the create delivered, and the edit
// writes no row in it. The post's reference edges are planned again from the edited body
// (planEditedItem, which reads the body with the shared reader and counts what the edit moves by
// refs.EditedTargets), and a delete plans them from the empty body its clear leaves
// (clearDeletedPostBody), so the post leaves the graph and every Referenced-by read.
package api

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// partiesNamedBy is the read of whom a post's body names: the delivering parties the shared
// reader finds in it, as a comment's create reads them (commentPartiesOf over text.ExtractAt).
func partiesNamedBy(t *testing.T, body string) []string {
	t.Helper()
	parties, err := commentPartiesOf(commentInput{}, text.ExtractAt(body, text.NewOrigins(testServerURL)))
	if err != nil {
		t.Fatalf("read the parties %q names: %v", body, err)
	}
	return parties.targets
}

// mentionLedger is a comment's delivery ledger as the store holds it: each comment_mentions row,
// as its target and its mode, and every attempt of comment_deliveries.
type mentionLedger struct {
	mentions []string
	attempts []model.CommentDelivery
}

func readMentionLedger(t *testing.T, database *store.Store, commentID string) mentionLedger {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select target || ' ' || delivery from comment_mentions where comment_id = $1 order by target
	`, commentID)
	if err != nil {
		t.Fatalf("read comment %s mentions: %v", commentID, err)
	}
	mentions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read comment %s mentions: %v", commentID, err)
	}
	attempts := waitForCommentDeliveries(t, database, commentID, "any attempts",
		func([]model.CommentDelivery) bool { return true })
	return mentionLedger{mentions: mentions, attempts: attempts}
}

// projectDocumentRef is the dispatch:// reference a text writes for a project document.
func projectDocumentRef(document model.Artifact) string {
	return "dispatch://TEST/artifact/" + document.Slug
}

// citedDocuments is what the post ref names cites, as GET /api/v1/references?from= lists its
// mentions edges: each edge's other end, by its reference, sorted.
func citedDocuments(t *testing.T, handler http.Handler, ref string) []string {
	t.Helper()
	var cited []string
	for _, edge := range graphEdges(t, handler, url.Values{"from": {ref}, "kind": {"mentions"}}).Edges {
		cited = append(cited, edge.Node.Ref)
	}
	slices.Sort(cited)
	return cited
}

// citedFrom reports whether GET /api/v1/references?to= of the document documentRef names lists
// an edge from post kind/id: the post itself or a block of it.
func citedFrom(t *testing.T, handler http.Handler, documentRef, kind, id string) bool {
	t.Helper()
	for _, edge := range graphEdges(t, handler, url.Values{"to": {documentRef}, "kind": {"mentions"}}).Edges {
		if edge.Node.Kind == kind && edge.Node.ID == id ||
			edge.Node.Parent != nil && edge.Node.Parent.Kind == kind && edge.Node.Parent.ID == id {
			return true
		}
	}
	return false
}

// referencedBy reports whether the Referenced-by read of document documentID
// (GET /api/v1/artifacts/{id}/references) lists post kind/id.
func referencedBy(t *testing.T, handler http.Handler, documentID, kind, id string) bool {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+documentID+"/references", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("get artifact %s references: status=%d body=%s", documentID, response.Code, response.Body.String())
	}
	for _, row := range decodeBody[model.ArtifactReferences](t, response).ReferencedBy {
		if row.Kind == kind && row.ID == id {
			return true
		}
	}
	return false
}

// editMentionedComment edits the comment as alice, its author, and answers the comment the edit
// answers with, its ledger as a read of it lists it.
func editMentionedComment(t *testing.T, handler http.Handler, id, body string) mentionedCommentRead {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+id, map[string]any{"body": body}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("edit comment %s: status=%d body=%s", id, response.Code, response.Body.String())
	}
	return decodeBody[mentionedCommentRead](t, response)
}

// An edit that keeps the party reference its body wrote, and changes the words and the record
// around it: the party is still named by the edited body, the delivery the create made stays the
// one delivery, the edit sends nothing and writes no ledger row, and the record edge is planned
// again from the edited body - the document the edit stopped citing loses its edge, the one it
// cites now gains one.
func TestACommentEditThatKeepsAPartyReferenceStillNamesItAndSendsNothing(t *testing.T) {
	t.Parallel()
	listener, sends := partyListener(t)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "An edit keeps the party", "before")
	design := createProjectDocument(t, handler, "TEST", "Design notes", "# Design\n")
	runbook := createProjectDocument(t, handler, "TEST", "Runbook", "# Runbook\n")
	designRef, runbookRef := projectDocumentRef(design), projectDocumentRef(runbook)

	body := "[planner](dispatch://agent/s1) please read " + designRef + "."
	if named := partiesNamedBy(t, body); !slices.Equal(named, []string{"session:s1"}) {
		t.Fatalf("the body names %v, want session:s1", named)
	}
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{"body": body}))
	commentRef := "dispatch://" + issue.Key + "/comment/" + created.ID
	if attempts := settledCommentDeliveries(t, database, created.ID); len(attempts) != 1 ||
		attempts[0].Target != "session:s1" || attempts[0].State != "sent" {
		t.Fatalf("the create's deliveries = %#v, want one sent attempt of session:s1", attempts)
	}
	if len(sends()) != 1 {
		t.Fatalf("sends after the create = %#v, want one", sends())
	}
	before := readMentionLedger(t, database, created.ID)
	if cited := citedDocuments(t, handler, commentRef); !slices.Equal(cited, []string{designRef}) {
		t.Fatalf("the comment cites %v before the edit, want %s", cited, designRef)
	}

	edited := editMentionedComment(t, handler, created.ID,
		"Still for [planner](dispatch://agent/s1): read "+runbookRef+" instead of the design notes.")

	if named := partiesNamedBy(t, edited.Body); !slices.Equal(named, []string{"session:s1"}) {
		t.Fatalf("the edited body names %v, want session:s1 still", named)
	}
	if len(edited.Mentions) != 1 || edited.Mentions[0].Target != "session:s1" ||
		len(edited.Deliveries) != 1 || edited.Deliveries[0].State != "sent" {
		t.Fatalf("the edit's ledger = %#v / %#v, want the create's one sent mention of session:s1",
			edited.Mentions, edited.Deliveries)
	}
	if after := readMentionLedger(t, database, created.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("the ledger after the edit = %#v, want it as the create left it: %#v", after, before)
	}
	if cited := citedDocuments(t, handler, commentRef); !slices.Equal(cited, []string{runbookRef}) {
		t.Fatalf("the comment cites %v after the edit, want %s alone", cited, runbookRef)
	}
	if citedFrom(t, handler, designRef, "comment", created.ID) || !citedFrom(t, handler, runbookRef, "comment", created.ID) {
		t.Fatalf("the documents' edges after the edit: %s from the comment = %v, %s = %v; want only the runbook's",
			designRef, citedFrom(t, handler, designRef, "comment", created.ID),
			runbookRef, citedFrom(t, handler, runbookRef, "comment", created.ID))
	}
	if len(sends()) != 1 {
		t.Fatalf("sends after the edit = %#v, want the create's one: an edit sends nothing", sends())
	}
}

// An edit that takes the party reference out of the body, and the record reference beside it:
// the edited body names nobody, the edit sends nothing new, the ledger stands as the create left
// it, and the record's edge is gone from both ends of the graph and from the document's
// Referenced-by read. An issue message's edit drops its record edge the same way.
func TestACommentEditThatRemovesAPartyReferenceNamesNobodyAndDropsTheRecordEdge(t *testing.T) {
	t.Parallel()
	listener, sends := partyListener(t)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "An edit removes the party", "before")
	design := createProjectDocument(t, handler, "TEST", "Design notes", "# Design\n")
	designRef := projectDocumentRef(design)

	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "[planner](dispatch://agent/s1) please read " + designRef + ".",
	}))
	commentRef := "dispatch://" + issue.Key + "/comment/" + created.ID
	settledCommentDeliveries(t, database, created.ID)
	if len(sends()) != 1 {
		t.Fatalf("sends after the create = %#v, want one", sends())
	}
	before := readMentionLedger(t, database, created.ID)
	message := createSessionIssueMessage(t, handler, issue.Key, "Please read "+designRef+".")
	messageRef := "dispatch://" + issue.Key + "/message/" + message.ID
	// The control: before the edits each post cites the document at both ends of the graph and
	// in its Referenced-by read, so an absence below is the edit's doing.
	for _, post := range []struct{ kind, id, ref string }{{"comment", created.ID, commentRef}, {"message", message.ID, messageRef}} {
		if cited := citedDocuments(t, handler, post.ref); !slices.Equal(cited, []string{designRef}) ||
			!citedFrom(t, handler, designRef, post.kind, post.id) || !referencedBy(t, handler, design.ID, post.kind, post.id) {
			t.Fatalf("the %s's edges before the edit: cites %v, want %s at both ends and in Referenced-by", post.kind, cited, designRef)
		}
	}

	edited := editMentionedComment(t, handler, created.ID, "Never mind: nobody needs to read anything.")
	editIssueMessage(t, handler, message.ID, "Never mind.", "")

	if named := partiesNamedBy(t, edited.Body); len(named) != 0 {
		t.Fatalf("the edited body names %v, want nobody", named)
	}
	if after := readMentionLedger(t, database, created.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("the ledger after the edit = %#v, want it as the create left it: %#v", after, before)
	}
	for _, post := range []struct{ kind, id, ref string }{{"comment", created.ID, commentRef}, {"message", message.ID, messageRef}} {
		if cited := citedDocuments(t, handler, post.ref); len(cited) != 0 {
			t.Fatalf("the %s cites %v after the edit, want nothing", post.kind, cited)
		}
		if citedFrom(t, handler, designRef, post.kind, post.id) {
			t.Fatalf("%s still has an edge from the %s after the edit took the reference out", designRef, post.kind)
		}
		if referencedBy(t, handler, design.ID, post.kind, post.id) {
			t.Fatalf("%s's Referenced-by still lists the %s after the edit took the reference out", designRef, post.kind)
		}
	}
	if len(sends()) != 1 {
		t.Fatalf("sends after the edit = %#v, want the create's one: an edit sends nothing", sends())
	}
}

// A delete of a comment, and of an issue message, takes each out of the reference graph and out
// of the Referenced-by read of the document it cited: the delete indexes the empty body its clear
// leaves, which holds no reference.
func TestADeletedPostLeavesTheGraphAndTheReferencedByRead(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "A delete leaves the graph", "before")
	design := createProjectDocument(t, handler, "TEST", "Design notes", "# Design\n")
	designRef := projectDocumentRef(design)
	comment := createReferenceAPIComment(t, handler, issue.Key, "Please read "+designRef+".")
	message := createSessionIssueMessage(t, handler, issue.Key, "Please read "+designRef+".")
	posts := []struct{ kind, id, ref string }{
		{"comment", comment.ID, "dispatch://" + issue.Key + "/comment/" + comment.ID},
		{"message", message.ID, "dispatch://" + issue.Key + "/message/" + message.ID},
	}
	for _, post := range posts {
		if cited := citedDocuments(t, handler, post.ref); !slices.Equal(cited, []string{designRef}) ||
			!citedFrom(t, handler, designRef, post.kind, post.id) || !referencedBy(t, handler, design.ID, post.kind, post.id) {
			t.Fatalf("the %s's edges before the delete: cites %v, want %s at both ends and in Referenced-by", post.kind, cited, designRef)
		}
	}

	deleteThreadComment(t, handler, comment.ID, "alice")
	deleteIssueMessage(t, handler, message.ID, "")

	for _, post := range posts {
		if cited := citedDocuments(t, handler, post.ref); len(cited) != 0 {
			t.Fatalf("the deleted %s cites %v, want nothing", post.kind, cited)
		}
		if citedFrom(t, handler, designRef, post.kind, post.id) {
			t.Fatalf("%s still has an edge from the deleted %s", designRef, post.kind)
		}
		if referencedBy(t, handler, design.ID, post.kind, post.id) {
			t.Fatalf("%s's Referenced-by still lists the deleted %s", designRef, post.kind)
		}
	}
}
