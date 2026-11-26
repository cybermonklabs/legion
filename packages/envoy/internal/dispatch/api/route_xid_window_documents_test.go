//go:build !race

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// documentRouteXidWindows is the rows of the route window table (route_xid_window_test.go) for the
// routes that write a document: its reviews, its title, its removal, its versions by upload or
// edit, under an open approval request too, the anchored ask and comment on a project document,
// and the version writes that leave the blocks of a spec past the bound to the block-index
// reconciliation.
var documentRouteXidWindows = []routeXidWindowCase{
	// The review's one write phase is the ledger's batch - the review row and artifact.approved
	// - with everything it is decided on read before it: the document under its owner's
	// advisory lock, its settled version and whether an approval request is open.
	{
		name: "the review route", route: "POST /api/v1/artifacts/{id}/reviews (createArtifactReview)",
		ready: readyArtifactReview, window: storetest.XidBatchedWindow,
	},
	// The review naming the version its caller read shares that handler (recordArtifactReview):
	// the version it names is compared in the read phase, before anything is queued, so its write
	// phase is that same batch.
	{
		name: "the version review route", route: "POST /api/v1/artifacts/{id}/versions/{number}/reviews (createArtifactVersionReview)",
		ready: readyArtifactVersionReview, window: storetest.XidBatchedWindow,
	},
	// Answering the open request adds the ask's answer, its block state, its parts' index and
	// ask.answered to that batch (closeAskTx queues them on the ledger), and nothing to the
	// window.
	{
		name: "the review route answering an approval request", route: "POST /api/v1/artifacts/{id}/reviews (createArtifactReview, closeAskTx)",
		ready: readyReviewAnsweringApprovalRequest, window: storetest.XidBatchedWindow,
	},
	{
		name: "the title correction route", route: "PATCH /api/v1/artifacts/{id} (renameArtifact)",
		ready: readyArtifactRename, window: storetest.XidWindowOf(4,
			"the title's update, the one batch reading the versions and the approval the renamed row "+
				"answers with (loadArtifactDetails), the batch appending artifact.renamed and the COMMIT ending them"),
	},
	// A project document is its own owner: the correction takes the project's upload lock and
	// then the document's owner lock (beginArtifactWrite) before the same writes.
	{
		name: "the title correction route on a project document", route: "PATCH /api/v1/artifacts/{id} on a project document (renameArtifact)",
		ready: readyProjectDocumentRename, window: storetest.XidWindowOf(4,
			"the title's update, the one batch reading the versions and the approval the renamed row "+
				"answers with (loadArtifactDetails), the batch appending artifact.renamed and the COMMIT ending them"),
	},
	{
		name: "the document removal route", route: "DELETE /api/v1/artifacts/{id} (removeArtifact)",
		ready: readyArtifactRemoval, window: storetest.XidWindowOf(16,
			"the eleven deletes of the document's rows in other tables - its versions, updates, snapshots, "+
				"checkpoints, search row, compaction debt, search-term queue, threads, "+
				"references, grammar debt and excerpt backoff - the node write stating the document's node gone, "+
				"the delete of its own row, the dispatch_doc_removed notify, the batch appending artifact.removed "+
				"and the gone statement it causes, and the COMMIT ending them"),
	},
	{
		name: "the project file upload route", route: "POST /api/v1/projects/{key}/artifacts on a file it has (storeArtifact)",
		ready: readyProjectFileUpload, window: storetest.XidBatchedWindow,
	},
	{
		name: "the project document upload route", route: "POST /api/v1/projects/{key}/artifacts on a document it has (storeArtifact)",
		ready: readyProjectDocumentUpload, window: storetest.XidBatchedWindow,
	},
	// A version written under an open approval request moves the request to it
	// (docs.MoveApprovalAsk): the rewording's reads - its references' plan, its parts' plan and
	// its edit time (docs.RewriteApprovalAsk) - run in the read phase, and its update, index,
	// parts, ask.edited and stamp join the version's batch. The upload writes its version itself
	// (storeArtifact, then MoveApprovalAsk); the edit writes it through the document service
	// (docs' writeVersionTx, then MoveApprovalAsk). The spec is a document the spec query's record
	// covers: the batch inserts the version's version_renders row with its cut and its record's
	// digest owed, and the fill after the COMMIT, a transaction of its own of main's batched shape,
	// records the digest (R15, docs' fillOwedDigests). Its approval advice is read in the read phase.
	{
		name: "the spec upload under an open approval request", route: "POST /api/v1/issues/{key}/artifacts under an open approval request (storeArtifact, docs.MoveApprovalAsk)",
		ready: readyUploadUnderApprovalRequest, window: storetest.XidBatchedWindow,
	},
	{
		name: "the spec edit under an open approval request", route: "POST /api/v1/artifacts/{id}/edits under an open approval request (editArtifact, docs' writeVersionTx, MoveApprovalAsk)",
		ready: readyEditUnderApprovalRequest, window: storetest.XidBatchedWindow,
	},
	// A version that restores the approved text under an open request moves the request and
	// then answers it in the approver's name (docs' writeCarriedReview, answerApprovalAsk): the
	// answer's reads - the transaction's time, its parts' plan - run in the read phase beside the
	// move's, and its update, parts, ask.answered and stamp, with the carried review and its
	// artifact.approved, join the version's batch, from the version's insert on behind the
	// savepoint the write phase takes for the restore's carry (docs' writeRestored), and the read of
	// the version's record before the COMMIT holds the pin to the record's digest (specRestoreWindow).
	{
		name: "the spec edit restoring the approved text under an open approval request", route: "POST /api/v1/artifacts/{id}/edits restoring the approved text (docs' carryApproval, writeCarriedReview, answerApprovalAsk)",
		ready: readyEditRestoringTheApprovedText, window: specRestoreWindow,
	},
	{
		name: "the spec upload restoring the approved text under an open approval request", route: "POST /api/v1/issues/{key}/artifacts restoring the approved text (docs.CarryUploadedApproval, writeCarriedReview, answerApprovalAsk)",
		ready: readyUploadRestoringTheApprovedText, window: specRestoreWindow,
	},
	{
		name: "the agent conversation upload route", route: "POST /api/v1/agents/{session_id}/artifacts (uploadAgentArtifact, storeArtifact)",
		ready: readyAgentArtifactUpload, window: storetest.XidBatchedWindow,
	},
	{
		name: "the document ask route", route: "POST /api/v1/artifacts/{id}/asks (createArtifactAsk, createAskFor)",
		ready: readyDocumentAsk, window: storetest.XidBatchedWindow,
	},
	{
		name: "the document comment route", route: "POST /api/v1/artifacts/{id}/comments (createArtifactComment, createCommentFor)",
		ready: readyDocumentComment, window: storetest.XidBatchedWindow,
	},
	// A Verification past the bound leaves its blocks to the reconciliation and writes its checks,
	// with their rows, their serves and their ancestors, in its own transaction (docs'
	// planTypedPasses): every read of that pass runs in the read phase, and its writes join the
	// write phase's one batch. The table's other uploads hold storetest.XidSpec's 40 KB document,
	// which cannot pass 2,000 blocks, so these two rows are the ones that run the pass. On an issue
	// that has not converted the issue is created beside a process of the release before
	// (holdIssuesUnconverted), which registers this build's owner lock protocol, so the row measures
	// this build's owner locks.
	{
		name: "the Verification upload above the bound on an issue that has not converted", route: "POST /api/v1/issues/{key}/artifacts past the bound, an issue that has not converted (storeArtifact, docs' indexDeferredVersion, planTypedPasses)",
		ready: readyVerificationUploadAboveTheBound(false), window: storetest.XidBatchedWindow,
	},
	{
		name: "the Verification upload above the bound on a converted issue", route: "POST /api/v1/issues/{key}/artifacts past the bound, a converted issue (storeArtifact, docs' indexDocumentInRoom, indexDeferredVersion, planTypedPasses)",
		ready: readyVerificationUploadAboveTheBound(true), window: storetest.XidBatchedWindow,
	},
	// The writes of a version of the deep spec (deepXidSpec), past the bound, that their routes
	// mark (docs.WithDeferredBlockIndex): each leaves the spec's blocks to the block-index
	// reconciliation and writes its typed nodes at once (docs' indexDeferredVersion,
	// planTypedPasses), every read of those passes, of the move of an open request and of the
	// anchor's mark running in the read phase, and states no record of its version, so no fill
	// follows the write phase's one batch. Each row's own check reads the spec owed at the version
	// the write wrote.
	{
		name:  "the edit of a spec of more than 2,000 blocks nested six deep under an open approval request",
		route: "POST /api/v1/artifacts/{id}/edits on a spec past the bound under an open approval request (editArtifact, docs' writeVersionTx, indexDeferredVersion, MoveApprovalAsk)",
		ready: readyDeepSpecEditUnderApprovalRequest, window: storetest.XidBatchedWindow,
	},
	{
		name:  "the anchored comment on a spec of more than 2,000 blocks nested six deep",
		route: "POST /api/v1/issues/{key}/comments anchored on a spec past the bound holding a browser's edit (createCommentFor, docs.SnapshotVersion, indexDeferredVersion)",
		ready: readyDeepSpecPin(false), window: storetest.XidBatchedWindow, unsettled: true,
	},
	{
		name:  "the anchored ask on a spec of more than 2,000 blocks nested six deep",
		route: "POST /api/v1/issues/{key}/asks anchored on a spec past the bound holding a browser's edit (createAskFor, docs.SnapshotVersion, indexDeferredVersion)",
		ready: readyDeepSpecPin(true), window: storetest.XidBatchedWindow, unsettled: true,
	},
	{
		name:  "the accept of a suggestion on a spec of more than 2,000 blocks nested six deep",
		route: "POST /api/v1/comments/{id}/accept on a spec past the bound (commentAction, docs.NamedVersion, indexDeferredVersion)",
		ready: readyDeepSpecSuggestionAccept, window: storetest.XidBatchedWindow,
	},
	{
		name:  "the named version of a spec of more than 2,000 blocks nested six deep",
		route: "POST /api/v1/artifacts/{id}/versions on a spec past the bound holding a browser's edit (createNamedVersion, docs.NamedVersion, indexDeferredVersion)",
		ready: readyDeepSpecNamedVersion, window: storetest.XidBatchedWindow, unsettled: true,
	},
	{
		name:  "the rebuild of a spec of more than 2,000 blocks nested six deep",
		route: "POST /api/v1/artifacts/{id}/rebuild with its markdown on a spec past the bound (rebuildArtifact, docs.RebuildDocument, indexDeferredVersion)",
		ready: readyDeepSpecRebuild, window: storetest.XidBatchedWindow,
	},
}

// specRestoreFill names what a restore of the approved text reads before its COMMIT to hold the
// approval's pin to the restored version's record (docs' writeStatedRenders, R14): the record of
// the spec's query at one cut, by the walk of the issue's tree as the batch left the store - the
// cut, the five reads of the issue's conversation (its items, the asks the conversation test
// passes, their blocks, and the ask and its blocks as their loaders read them), the read of the
// items' rows, the one read of the tree, every level of it (refs.TreeSubtrees), the read of the
// issues above the issue and that of the issue a view shows. The spec query takes none of the
// fixture's blocks, so the record reads no node's values or relations.
const specRestoreFill = "the read of the spec query's record of the issue at one cut once the batch has run (R14): " +
	"the cut, the five reads of the issue's conversation - its items, the asks the conversation test passes, their blocks, and the ask and its blocks as their loaders read them - " +
	"the read of the items' rows, the one read of the tree, every level of it, the read of the issues above the issue and that of the issue a view shows"

// specRestoreWindow is the window of a version of the spec restoring the approved text under an
// open approval request (docs' writeRestored), root R29's second exception the owner approved, 17
// statements (R14, R15): the batch up to the version's insert, the savepoint taken before it, the
// batch from it on - the version's version_renders row pinned to the approval's digest, the
// carried review and its artifact.approved, the request's answer and the version's
// artifact.version - the read of the record (specRestoreFill), the update giving the pinned row its
// cut, the savepoint's release, the batch of the settlement credit's statements and the COMMIT.
var specRestoreWindow = storetest.XidWindowOf(17,
	"the write phase's batch up to the restored version's insert, the savepoint the write phase takes before that insert (writeRestored), "+
		"the batch from the insert on - the version's version_renders row pinned to the approval's digest, the carried review, its artifact.approved and the request's answer before the version's artifact.version - "+
		specRestoreFill+", the update giving the pinned version_renders row the cut (R15), "+
		"the release of the savepoint where the record is the approval's digest (a rollback to it where it is not, which writes the version again with no carry), "+
		"the batch of the settlement credit's statements, and the COMMIT ending them")

// readyReviewAnsweringApprovalRequest drives POST /api/v1/artifacts/{id}/reviews on a spec with
// an approval request open, so the person's review answers that request (closeAskTx) and
// reviews the document at the version the request named.
func readyReviewAnsweringApprovalRequest(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Review answering a request")
	waitForArtifactVersion(t, fixture.handler, fixture.artifactID, 1)
	requested := sessionRequest(t, fixture.handler, http.MethodPost, "/api/v1/artifacts/"+fixture.artifactID+"/approval-requests", map[string]any{
		"summary": "The migration section is rewritten; the rest of the spec stands.", "actor": sessionActor(),
	})
	if requested.Code != http.StatusCreated {
		t.Fatalf("open the approval request: status=%d body=%s", requested.Code, requested.Body.String())
	}
	request := decodeBody[approvalRequestAsk](t, requested)
	if request.Ask == nil {
		t.Fatalf("the approval request opened no ask: %s", requested.Body.String())
	}
	return fixture.routeWrite(t, "the review answering the open request",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost,
				"/api/v1/artifacts/"+fixture.artifactID+"/reviews", map[string]any{"state": "approved"}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("review the spec: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if review := decodeBody[model.ArtifactReview](t, answer); review.AskID == nil || *review.AskID != request.Ask.ID {
				t.Fatalf("the review = %#v, want the one answering approval request %s", review, request.Ask.ID)
			}
		})
}

// readyProjectDocumentUpload drives POST /api/v1/projects/{key}/artifacts naming a document the
// project already has, the harness's large spec, which writes its next version through the
// document service (storeArtifact).
func readyProjectDocumentUpload(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Project document upload")
	document := createProjectDocument(t, fixture.handler, "LOCK", "Design notes", base.spec)
	waitForArtifactVersion(t, fixture.handler, document.ID, 1)
	revised := strings.Replace(base.spec, "Revision token r0", "Revision token r1", 1)
	// The upload replaces the document, so it names the token of the version it replaces.
	replaced := uploadBase(t, fixture.handler, document.ID)
	return fixture.routeWrite(t, "the document's next version",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/LOCK/artifacts", map[string]any{
				"name": "Design notes", "content": revised, "precondition": replaced,
			}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("upload the document again: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if uploaded := decodeBody[uploadedArtifact](t, answer); uploaded.Artifact.ID != document.ID || uploaded.Version.Number != 2 {
				t.Fatalf("the upload wrote version %d of %s, want version 2 of %s",
					uploaded.Version.Number, uploaded.Artifact.ID, document.ID)
			}
		})
}

// readyAgentArtifactUpload drives POST /api/v1/agents/{session_id}/artifacts naming a file the
// conversation already has, which writes its next version under the conversation's lock
// (uploadAgentArtifact, storeArtifact).
func readyAgentArtifactUpload(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	base.options.files = filestest.NewMemory()
	fixture := base.issue(t, "Agent conversation upload")
	target := "/api/v1/agents/" + agentSession + "/artifacts"
	first := multipartRequest(t, fixture.handler, target, map[string]string{"name": "shot.png"},
		"shot.png", "image/png", pngBytes("first"), "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("upload the conversation's file: status=%d body=%s", first.Code, first.Body.String())
	}
	file := decodeBody[uploadedArtifact](t, first).Artifact
	return fixture.routeWrite(t, "the file's next version",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return multipartRequest(t, handler, target, map[string]string{"name": "shot.png"},
				"shot.png", "image/png", pngBytes("second"), "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("upload the file again: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if uploaded := decodeBody[uploadedArtifact](t, answer); uploaded.Artifact.ID != file.ID || uploaded.Version.Number != 2 {
				t.Fatalf("the upload wrote version %d of %s, want version 2 of %s",
					uploaded.Version.Number, uploaded.Artifact.ID, file.ID)
			}
		})
}

// readyDocumentAsk drives POST /api/v1/artifacts/{id}/asks, a session's anchored ask on a
// project document (createArtifactAsk, createAskFor).
func readyDocumentAsk(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Document ask")
	document := projectDesignNotes(t, fixture, base.spec)
	_, quotes := xidSpec(1)
	return fixture.routeWrite(t, "the document's ask",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
				"question": "Which team signs off on this requirement?",
				"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
				"anchor":   map[string]any{"artifact": document.Slug, "quote": quotes[0]}, "actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("ask on the document: status=%d body=%s", answer.Code, answer.Body.String())
			}
		})
}

// readyDocumentComment drives POST /api/v1/artifacts/{id}/comments, a session's anchored comment
// on a project document (createArtifactComment, createCommentFor).
func readyDocumentComment(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Document comment")
	document := projectDesignNotes(t, fixture, base.spec)
	_, quotes := xidSpec(1)
	return fixture.routeWrite(t, "the document's comment",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{
				"body":   "Does this still hold once the outbox is sharded?",
				"anchor": map[string]any{"artifact": document.Slug, "quote": quotes[0]}, "actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("comment on the document: status=%d body=%s", answer.Code, answer.Body.String())
			}
		})
}

// requireApprovalRequestMovedTo2 fails t unless the approval request askID is still open, names
// version 2 of its document - the version the measured write wrote - and still names version 1 as
// the one its agent handed a person, reworded by the move (edited_at).
func requireApprovalRequestMovedTo2(t *testing.T, fixture itemRowFixture, askID string) {
	t.Helper()
	if moved := fixture.rowCount(t, `
		select count(*) from asks
		where id = $1 and state = 'open' and edited_at is not null
			and (approval->>'version')::int = 2 and (approval->>'requested_version')::int = 1
	`, askID); moved != 1 {
		t.Fatalf("approval request %s was not moved to version 2 by the write: %#v", askID, readBlockAsk(t, fixture.handler, askID))
	}
}

// readyUploadUnderApprovalRequest drives POST /api/v1/issues/{key}/artifacts replacing the spec
// while an approval request is open on it, so the version the upload writes moves the request
// (storeArtifact, docs.MoveApprovalAsk, RewriteApprovalAsk).
func readyUploadUnderApprovalRequest(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Upload under a request")
	waitForArtifactVersion(t, fixture.handler, fixture.artifactID, 1)
	request := requestApproval(t, fixture.handler, fixture.artifactID)
	revised := strings.Replace(base.spec, "Revision token r0", "Revision token r1", 1)
	// The upload replaces the spec, so it names the token of the version it replaces.
	replaced := uploadBase(t, fixture.handler, fixture.artifactID)
	return fixture.routeWrite(t, "the spec's next version and the request it moves",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/artifacts", map[string]any{
				"name": "spec.md", "content": revised, "summary": "Bump the revision", "actor": sessionActor(),
				"precondition": replaced,
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("upload the spec under the open request: status=%d body=%s", answer.Code, answer.Body.String())
			}
			requireApprovalRequestMovedTo2(t, fixture, request.ID)
		})
}

// readyEditUnderApprovalRequest drives POST /api/v1/artifacts/{id}/edits on the spec while an
// approval request is open on it: the edit versions the document in its own transaction through
// the document service, whose version write moves the request (docs' writeVersionTx,
// MoveApprovalAsk, RewriteApprovalAsk).
func readyEditUnderApprovalRequest(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Edit under a request")
	waitForArtifactVersion(t, fixture.handler, fixture.artifactID, 1)
	request := requestApproval(t, fixture.handler, fixture.artifactID)
	return fixture.routeWrite(t, "the spec's next version and the request it moves",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+fixture.artifactID+"/edits", map[string]any{
				"ops": []map[string]string{replaceOp("Revision token r0", "Revision token r1")}, "actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("edit the spec under the open request: status=%d body=%s", answer.Code, answer.Body.String())
			}
			requireApprovalRequestMovedTo2(t, fixture, request.ID)
		})
}

// approvedThenRequested has alice approve the fixture's spec at version 1, the agent edit it to
// version 2, which stales that approval, and the agent open an approval request at version 2,
// which follows the approval of version 1. It answers the request's ask.
func approvedThenRequested(t *testing.T, fixture itemRowFixture) carriedAsk {
	t.Helper()
	waitForArtifactVersion(t, fixture.handler, fixture.artifactID, 1)
	if review := reviewDocument(t, fixture.handler, fixture.artifactID, "alice", "approved", ""); review.Version != 1 {
		t.Fatalf("review = %#v, want an approval of version 1", review)
	}
	reviseDocument(t, fixture.handler, fixture.artifactID, "Revision token r0", "Revision token r1", 2)
	request := requestApproval(t, fixture.handler, fixture.artifactID)
	if request.Approval.Version != 2 || request.Approval.Approved == nil || request.Approval.Approved.Version != 1 {
		t.Fatalf("request = %#v, want one at version 2 following the approval of version 1", request.Approval)
	}
	return request
}

// requireRequestAnsweredByTheCarry fails t unless the request is answered Approve in alice's name
// at version 3, the version the measured write restored, and that version carries her approval
// of version 1 against the request.
func requireRequestAnsweredByTheCarry(t *testing.T, fixture itemRowFixture, askID string) {
	t.Helper()
	waitForArtifactVersion(t, fixture.handler, fixture.artifactID, 3)
	requireReviewCarriedWith(t, fixture.handler, fixture.artifactID, 3, 1, restoreSentence(1), &askID)
	if answered := readBlockAsk(t, fixture.handler, askID); answered.State != "answered" || answered.Answer == nil ||
		answered.Answer.User != "alice" || answered.Approval == nil || answered.Approval.Version != 3 {
		t.Fatalf("the request after the restore = state %q answer %#v approval %#v, want Approve by alice at version 3",
			answered.State, answered.Answer, answered.Approval)
	}
}

// readyEditRestoringTheApprovedText drives POST /api/v1/artifacts/{id}/edits undoing the agent's
// edit under an open approval request: the version holds the approved text again, so the
// document service's version write moves the request to it and then carries alice's approval,
// which answers the request (docs' writeVersionTx, carryApproval, writeCarriedReview,
// answerApprovalAsk).
func readyEditRestoringTheApprovedText(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Edit restoring the approved text")
	request := approvedThenRequested(t, fixture)
	return fixture.routeWrite(t, "the restoring version and the request its carry answers",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+fixture.artifactID+"/edits", map[string]any{
				"ops": []map[string]string{replaceOp("Revision token r1", "Revision token r0")}, "actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("undo the edit under the open request: status=%d body=%s", answer.Code, answer.Body.String())
			}
			requireRequestAnsweredByTheCarry(t, fixture, request.ID)
		})
}

// readyUploadRestoringTheApprovedText drives POST /api/v1/issues/{key}/artifacts uploading the
// approved version's text under an open approval request: the upload moves the request to its
// version and then carries alice's approval, which answers the request
// (docs.CarryUploadedApproval, writeCarriedReview, answerApprovalAsk).
func readyUploadRestoringTheApprovedText(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Upload restoring the approved text")
	request := approvedThenRequested(t, fixture)
	approved := versionMarkdown(t, fixture.handler, fixture.artifactID, 1)
	// The upload replaces the spec, so it names the token of the version it replaces.
	replaced := uploadBase(t, fixture.handler, fixture.artifactID)
	return fixture.routeWrite(t, "the restoring version and the request its carry answers",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/artifacts", map[string]any{
				"name": "spec.md", "content": approved, "actor": sessionActor(), "precondition": replaced,
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("upload the approved text under the open request: status=%d body=%s", answer.Code, answer.Body.String())
			}
			requireRequestAnsweredByTheCarry(t, fixture, request.ID)
		})
}

// readyVerificationUploadAboveTheBound readies POST /api/v1/issues/{key}/artifacts uploading the
// issue's first Verification document, a list past the bound and 103 checks serving the ten
// requirements of its spec, on an issue born converted or, where not converted, created beside a
// process of the release before (holdIssuesUnconverted): the upload leaves its blocks to the
// block-index reconciliation and writes its checks at once (docs' planTypedPasses).
func readyVerificationUploadAboveTheBound(converted bool) func(*testing.T, routeFixture) lockedItemWrite {
	return func(t *testing.T, base routeFixture) lockedItemWrite {
		t.Helper()
		handler, database, deps := newTestServer(t, base.options)
		if !converted {
			holdIssuesUnconverted(t, database)
		}
		requirements := make([]acceptedBlock, 10)
		for index := range requirements {
			requirements[index] = acceptedRequirement(fmt.Sprintf("r%d", index+1))
		}
		issue := createInteractionIssue(t, handler, "LOCK", "Verification upload above the bound", base.spec+"\n"+typedDocument("", false, requirements...))
		if tree, err := issueTreeOf(context.Background(), database.Pool, issue.Key); err != nil || (tree != "") != converted {
			t.Fatalf("issue %s holds the tree %q (%v), want it converted %t", issue.Key, tree, err, converted)
		}
		checks := make([]acceptedBlock, 103)
		for index := range checks {
			checks[index] = acceptedCheck(fmt.Sprintf("c%d", index+1), fmt.Sprintf("r%d", index%10+1))
		}
		content := typedDocument(issue.Key, true, checks...)
		fixture := itemRowFixture{handler: handler, database: database, deps: deps, issueKey: issue.Key, artifactID: issue.PrimaryArtifactID}
		return fixture.routeWrite(t, "the Verification's first version and its checks",
			func(handler http.Handler) *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts",
					map[string]any{"name": "verification.md", "content": content}, "alice")
			},
			func(t *testing.T, answer *httptest.ResponseRecorder) {
				if answer.Code != http.StatusCreated {
					t.Fatalf("upload the Verification: status=%d body=%s", answer.Code, answer.Body.String())
				}
				verification := decodeBody[uploadedArtifact](t, answer).Artifact.ID
				if indexed := blockIndexVersion(t, database, verification); indexed != nil {
					t.Fatalf("the upload indexed the Verification at %d in its own request, want its blocks left to the reconciliation", *indexed)
				}
				if held := readHeldTypedBlock(t, database, blockKeyOf(t, database, verification)+"#c103"); held.state != graphstore.StateLive || held.rows != 1 || held.serves != 1 {
					t.Fatalf("the upload left its check c103 as %+v, want it written at once with its row and its serves", held)
				}
			})
	}
}

// The deep spec's shape: deepXidChains chains, each a paragraph and a list nested deepXidLevels
// levels below it, a list and its one item's paragraph at each level, under the spec's heading and
// its revision token line - 2 + 360 × 13 = 4,682 blocks, past the bound (docs' deferBlockIndexBlocks).
const (
	deepXidChains = 360
	deepXidLevels = 6
	deepXidBlocks = 2 + deepXidChains*(1+2*deepXidLevels)
)

// deepXidSpec is the spec the table's deferred rows measure on, a spec of more than 2,000 blocks
// nested six deep, about 104 KB: each chain's paragraph opens with a line an anchor can quote, and
// the revision token line is one an edit can bump. It answers the markdown and each chain's quote,
// in document order.
func deepXidSpec() (string, []string) {
	var spec strings.Builder
	quotes := make([]string, 0, deepXidChains)
	spec.WriteString("# Deep spec for " + xidProject + "\n\nRevision token r0\n\n")
	for chain := 1; chain <= deepXidChains; chain++ {
		quote := fmt.Sprintf("Chain %d holds", chain)
		quotes = append(quotes, quote)
		spec.WriteString(quote + " the outbox.\n\n")
		for level := range deepXidLevels {
			fmt.Fprintf(&spec, "%s- Level %d of chain %d reads the bus.\n", strings.Repeat("  ", level), level+1, chain)
		}
		spec.WriteString("\n")
	}
	return spec.String(), quotes
}

// deepXidFixture stands up a row's server on base with deepXidSpec as its issue's spec and runs the
// block-index reconciliation until the spec is indexed - the create left its blocks to it - so the
// row measures a write of an indexed document. Where browser, alice's browser then writes a
// paragraph into the spec that no settlement versions (connectBrowserPeer, under a settle delay of
// an hour), which the measured write's version takes up.
func deepXidFixture(t *testing.T, base routeFixture, title string, browser bool) (itemRowFixture, []string) {
	t.Helper()
	spec, quotes := deepXidSpec()
	tree, err := pmdoc.Parse(spec)
	if err != nil {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the deep spec does not parse: %v", err)
	}
	pmdoc.EnsureBlockIDs(tree)
	if _, offsets, err := pmdoc.RenderWithBlockOffsets(tree); err != nil || len(offsets) != deepXidBlocks {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the deep spec holds %d blocks (%v), want %d", len(offsets), err, deepXidBlocks)
	}
	if browser {
		base.options.browserPeers, base.options.settle = true, time.Hour
	}
	fixture := newItemRowFixture(t, base.options, title, spec)
	t.Logf("deep spec: %d bytes, %d blocks", len(spec), deepXidBlocks)
	waitForArtifactVersion(t, fixture.handler, fixture.artifactID, 1)
	reconcileUntilNothingOwed(t, fixture.handler, fixture.deps, fixture.issueKey)
	requireIndexedAt(t, fixture.database, fixture.artifactID, 1)
	if browser {
		peer := connectBrowserPeer(t, fixture.handler, fixture.deps.Docs.(*docs.Service), fixture.artifactID)
		t.Cleanup(peer.close)
		peer.appendParagraph(t, "A browser's words no settlement has versioned.\n")
		peer.barrier(t)
	}
	return fixture, quotes
}

// requireDeferredDeepVersion fails t unless what left version 2 of the deep spec to the block-index
// reconciliation: the spec's document_block_versions version null, the coverage read and the tree
// read naming it owed at version 2, which holds no stated record. A version indexed in the request
// is the finding the row exists for, not a refusal, so it fails t without stopping it, and the
// window's verdict still names the statements that index ran inside the window.
func requireDeferredDeepVersion(t *testing.T, fixture itemRowFixture, what string) {
	t.Helper()
	if indexed := blockIndexVersion(t, fixture.database, fixture.artifactID); indexed != nil {
		t.Errorf("%s indexed the deep spec at %d in its own request, want its blocks left to the reconciliation", what, *indexed)
		return
	}
	requireOwedNamed(t, fixture.handler, fixture.issueKey, fixture.artifactID, 2, what)
	requireUnstatedVersion(t, fixture.database, fixture.artifactID, 2)
}

// requireNamedDeepVersion fails t unless version 2 of the deep spec is a named version holding
// written and the artifact.version event recording it carries a diff that adds it.
func requireNamedDeepVersion(t *testing.T, fixture itemRowFixture, written, what string) {
	t.Helper()
	var named bool
	var markdown, diff string
	if err := fixture.database.Pool.QueryRow(context.Background(), `
		select v.named, v.markdown, coalesce((select e.payload->>'diff' from events e
			where e.type = 'artifact.version' and e.payload->>'artifact_id' = $1 order by e.id desc limit 1), '')
		from artifact_versions v where v.artifact_id = $1::uuid and v.number = 2`, fixture.artifactID).Scan(&named, &markdown, &diff); err != nil {
		t.Fatalf("read version 2 of the deep spec after %s: %v", what, err)
	}
	if !named || !strings.Contains(markdown, written) || !strings.Contains(diff, "+"+written) {
		t.Fatalf("%s wrote version 2 of the deep spec named %t, holding %q %t, its event's diff adding it %t, want a named version holding it with its diff",
			what, named, written, strings.Contains(markdown, written), strings.Contains(diff, "+"+written))
	}
}

// readyDeepSpecEditUnderApprovalRequest drives POST /api/v1/artifacts/{id}/edits on the deep spec
// while an approval request is open on it: the edit bumps the revision token and adds a
// requirement, and its version moves the request and writes the requirement's node at once while
// leaving the spec's blocks to the reconciliation (docs' writeVersionTx, indexDeferredVersion,
// planTypedPasses, MoveApprovalAsk).
func readyDeepSpecEditUnderApprovalRequest(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture, _ := deepXidFixture(t, base, "Deep edit under a request", false)
	request := requestApproval(t, fixture.handler, fixture.artifactID)
	return fixture.routeWrite(t, "the deep spec's next version, its requirement and the request it moves",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+fixture.artifactID+"/edits", map[string]any{
				"ops": []map[string]any{
					{"op": "replace", "find": "Revision token r0", "with": "Revision token r1"},
					{"op": "insert", "after": "end", "markdown": ":::requirement{#r1}\nThe deep spec holds its requirement.\n:::\n"},
				},
				"actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("edit the deep spec under the open request: status=%d body=%s", answer.Code, answer.Body.String())
			}
			requireApprovalRequestMovedTo2(t, fixture, request.ID)
			requireDeferredDeepVersion(t, fixture, "the deep spec's edit")
			if held := readHeldTypedBlock(t, fixture.database, blockKeyOf(t, fixture.database, fixture.artifactID)+"#r1"); held.state != graphstore.StateLive || held.rows != 1 {
				t.Fatalf("the deep spec's edit left its requirement r1 as %+v, want its node and its row written at once", held)
			}
		})
}

// readyDeepSpecPin drives POST /api/v1/issues/{key}/comments, or /asks where ask, anchored on the
// deep spec while alice's browser holds an edit of it no settlement has versioned, so the pin
// writes the spec's next version (resolveAnchor, docs.SnapshotVersion).
func readyDeepSpecPin(ask bool) func(*testing.T, routeFixture) lockedItemWrite {
	return func(t *testing.T, base routeFixture) lockedItemWrite {
		t.Helper()
		fixture, quotes := deepXidFixture(t, base, "Deep pin", true)
		route, what := "/comments", "the anchored comment"
		body := map[string]any{"body": "Does this chain still hold?"}
		if ask {
			route, what = "/asks", "the anchored ask"
			body = map[string]any{"question": "Which team owns this chain?", "options": []map[string]string{{"label": "Platform"}, {"label": "Product"}}}
		}
		body["anchor"], body["actor"] = map[string]any{"artifact": "spec", "quote": quotes[0]}, sessionActor()
		return fixture.routeWrite(t, what+" and the deep spec's version its pin writes",
			func(handler http.Handler) *httptest.ResponseRecorder {
				return sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+route, body)
			},
			func(t *testing.T, answer *httptest.ResponseRecorder) {
				if answer.Code != http.StatusCreated {
					t.Fatalf("%s on the deep spec: status=%d body=%s", what, answer.Code, answer.Body.String())
				}
				var anchor *model.Anchor
				if ask {
					anchor = decodeBody[model.Ask](t, answer).Anchor
				} else {
					anchor = decodeBody[model.Comment](t, answer).Anchor
				}
				if anchor == nil || anchor.Version != 2 {
					t.Fatalf("%s is pinned to %+v, want version 2 of the deep spec, which its pin wrote", what, anchor)
				}
				requireDeferredDeepVersion(t, fixture, what+"'s pin")
			})
	}
}

// readyDeepSpecSuggestionAccept drives POST /api/v1/comments/{id}/accept on a session's suggestion
// anchored on the deep spec, which applies it and names the version it produced (commentAction,
// docs.NamedVersion).
func readyDeepSpecSuggestionAccept(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture, quotes := deepXidFixture(t, base, "Deep suggestion accept", false)
	replaceWith := strings.Replace(quotes[1], "holds", "stands", 1)
	suggested := sessionRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/comments", map[string]any{
		"body": "This chain no longer holds as worded.", "anchor": map[string]any{"artifact": "spec", "quote": quotes[1]},
		"suggestion": map[string]string{"replace_with": replaceWith}, "actor": sessionActor(),
	})
	if suggested.Code != http.StatusCreated {
		t.Fatalf("open the suggestion on the deep spec: status=%d body=%s", suggested.Code, suggested.Body.String())
	}
	suggestion := decodeBody[model.Comment](t, suggested)
	return fixture.routeWrite(t, "the suggestion applied and the named version it produced",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+suggestion.ID+"/accept", map[string]any{}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("accept the suggestion on the deep spec: status=%d body=%s", answer.Code, answer.Body.String())
			}
			requireNamedDeepVersion(t, fixture, replaceWith, "the accept")
			requireDeferredDeepVersion(t, fixture, "the accept")
		})
}

// readyDeepSpecNamedVersion drives POST /api/v1/artifacts/{id}/versions on the deep spec while
// alice's browser holds an edit of it no settlement has versioned, which the named version takes up
// (createNamedVersion, docs.NamedVersion).
func readyDeepSpecNamedVersion(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture, _ := deepXidFixture(t, base, "Deep named version", true)
	return fixture.routeWrite(t, "the deep spec's named version",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+fixture.artifactID+"/versions", map[string]any{
				"summary": "Revision one", "actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("name a version of the deep spec: status=%d body=%s", answer.Code, answer.Body.String())
			}
			requireNamedDeepVersion(t, fixture, "A browser's words no settlement has versioned.", "the named-version route")
			requireDeferredDeepVersion(t, fixture, "the named-version route")
		})
}

// readyDeepSpecRebuild drives POST /api/v1/artifacts/{id}/rebuild with the deep spec's markdown,
// its revision token bumped, on a document store whose next load of the spec's room is a history
// that does not decode (invalidLoadOnceStore), so the rebuild writes the supplied markdown as the
// spec's next version (rebuildArtifact, docs.RebuildDocument).
func readyDeepSpecRebuild(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	var broken *invalidLoadOnceStore
	base.options.persistence = func(database *store.Store) docs.VersionedStore {
		broken = newInvalidLoadOnceStore(database)
		return broken
	}
	fixture, _ := deepXidFixture(t, base, "Deep rebuild", false)
	if err := fixture.deps.Docs.(*docs.Service).Evict(context.Background(), artifactRoom(t, fixture.handler, fixture.artifactID)); err != nil {
		t.Fatalf("evict the deep spec's room: %v", err)
	}
	broken.failNextLoad(t, fixture.artifactID)
	spec, _ := deepXidSpec()
	revised := strings.Replace(spec, "Revision token r0", "Revision token r1", 1)
	return fixture.routeWrite(t, "the deep spec's rebuilt version",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+fixture.artifactID+"/rebuild", map[string]any{"markdown": revised}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("rebuild the deep spec: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if markdown := versionMarkdownAt(t, fixture.database, fixture.artifactID, 2); !strings.Contains(markdown, "Revision token r1") {
				t.Fatalf("the rebuild wrote version 2 of the deep spec without the supplied markdown's token:\n%.200s", markdown)
			}
			requireDeferredDeepVersion(t, fixture, "the rebuild")
		})
}
