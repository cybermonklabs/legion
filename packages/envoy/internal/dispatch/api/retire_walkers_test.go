package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Test round 6 of this change: no writer that walks an issue's documents writes into a retired one.
// Each reads the document's retirement under the document owner's lock, the lock
// docs.RetireDocument takes (docs' retiredUnderOwnerLock), and skips a document it reads retired:
// it writes nothing into it, refuses nothing, and goes on with the issue's other documents.

// A reopen follows the versions a settlement wrote while the issue was closed
// (docs.Service.FollowClosedIssueVersions): it moves the open approval request that names a
// document to the document's latest version, as that version's own write moves it on an open
// issue. A retired document takes no new text, and the reopen moves no request onto it. The
// approval request of notes.md waits on alice at version 1. The issue closes, and a browser edit
// the store admits inside the close margin is settled on the closed issue into version 2. notes.md
// is then retired, and the reopen through PATCH is taken: it restores the request still naming
// version 1 of the retired document, and appends no ask.edited.
func TestAReopenMovesNoApprovalRequestOntoTheClosedVersionOfARetiredDocument(t *testing.T) {
	t.Parallel()
	handler, database, documentService := newClosedSettlementHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "An approval request on a document retired while closed", "Context\n")
	notes := uploadIssueDocument(t, handler, issue.Key, "notes.md", "Context\n")
	f := closedEditFixture{handler: handler, database: database, documents: documentService, spec: notes.ID}
	awaitSettled(t, database, issue.PrimaryArtifactID, notes.ID)
	request := requestApproval(t, handler, notes.ID)
	named := request.Approval.Version

	f.peer = connectBrowserPeer(t, handler, documentService, notes.ID)
	closeIssueThroughPatch(t, handler, database, issue.Key)
	awaitSettled(t, database, notes.ID)
	closed := settleClosedEdit(t, f, replaceFirstParagraph("Context", "Context, edited in the margin"))
	if closed <= named {
		t.Fatalf("the closed issue's settlement left notes.md at version %d, want a version above the %d the request names", closed, named)
	}
	retireTestDocument(t, database, notes.ID)

	seq := latestIssueSeq(t, database, issue.Key)
	reopenAndSettle(t, f, issue.Key)
	if restored := readCarriedAsk(t, handler, request.ID); restored.Approval.Version != named {
		t.Fatalf("after the reopen the restored request names version %d of the retired notes.md, want version %d, the one it named before the close: FollowClosedIssueVersions moved it onto the retired document",
			restored.Approval.Version, named)
	}
	if ask := readBlockAsk(t, handler, request.ID); ask.State != "open" {
		t.Fatalf("after the reopen the approval request reads %s, want open", ask.State)
	}
	for _, event := range issueEventsAfter(t, database, issue.Key, seq) {
		if event.kind == "ask.edited" {
			t.Fatalf("the reopen appended %+v, want no ask.edited: no request moves onto a retired document", event)
		}
	}
}

// The answer route keeps an ask's own block in step with its answer (docs.KeepAskBlockInStep). An
// ask whose row names its block in the issue's decisions.md, which has since been retired, is
// answered through POST /api/v1/asks/{id}/answer: the answer is taken, the ask reads answered, and
// the retired document, which takes no new text, gets no version, before or after the issue's
// documents settle.
func TestAnAnswerToAnAskWhoseRowNamesARetiredDecisionsDocumentIsTakenAndWritesItNoVersion(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	decisions := seedDecisionsDocument(t, database, documents, issue.Key, "# Decisions\n\n"+
		":::ask{#d1 urgency=\"low\" multiple=\"false\"}\nShould the census print the corpus size?\n\n- Yes: Print it.\n- No: Leave it out.\n:::\n")
	settleIssue(t, documents, issue.Key)
	askID := blockAskID(t, handler, issue.Key, "d1")
	var named string
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(block_artifact_id::text, '') from asks where id = $1`, askID).Scan(&named); err != nil || named != decisions {
		t.Fatalf("the ask of block d1 names block document %q (%v), want the decisions document %s", named, err, decisions)
	}
	before := latestVersion(t, database, decisions).Number
	retireTestDocument(t, database, decisions)

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
		map[string]any{"selected": []string{"Yes"}, "expected_edited_at": nil}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/asks/%s/answer, the ask's block in the retired decisions document %s: status=%d body=%s, want 200: KeepAskBlockInStep writes nothing into a retired document",
			askID, decisions, response.Code, response.Body.String())
	}
	if ask := readBlockAsk(t, handler, askID); ask.State != "answered" {
		t.Fatalf("after the answer the ask reads %s, want answered", ask.State)
	}
	if after := latestVersion(t, database, decisions).Number; after != before {
		t.Fatalf("the answer wrote version %d of the retired decisions document, past its version %d", after, before)
	}
	settleIssue(t, documents, issue.Key)
	if after := latestVersion(t, database, decisions).Number; after != before {
		t.Fatalf("the settlement after the answer wrote version %d of the retired decisions document, past its version %d", after, before)
	}
}

// A live write into an issue's decisions document once it is retired - a comment's mark on one of
// its quotes, which api does not test the row for itself - is refused by the document service under
// the owner's lock (docs.RetiredDocumentError), and api answers it as it answers an edit of that
// document (retiredDocumentRefusal): 409 DECISIONS_ARE_NODES, saying to write each decision as a
// `decision` block in the issue's spec, and not DOCUMENT_RETIRED, whose advice to write a document
// of another name would lead its caller to decisions-2.md. No version is written.
func TestACommentsMarkOnARetiredDecisionsDocumentIsRefusedAsDecisionsAreNodes(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	decisions := seedDecisionsDocument(t, database, deps.Docs.(*docs.Service), issue.Key, "# Decisions\n\n## Transport\n\nHTTP it is.\n")
	retireTestDocument(t, database, decisions)

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Use gRPC.", "anchor": map[string]string{"artifact": "decisions-md", "quote": "HTTP it is"},
	}, "alice")
	body := response.Body.String()
	if response.Code != http.StatusConflict || responseCode(t, response) != codeDecisionsAreNodes || !strings.Contains(body, "dispatch://"+issue.Key+"/spec") {
		t.Fatalf("a comment's mark on the retired decisions document %s: status=%d body=%s, want 409 %s naming dispatch://%s/spec",
			decisions, response.Code, body, codeDecisionsAreNodes, issue.Key)
	}
	if versions := latestVersion(t, database, decisions); versions.Number != 1 {
		t.Fatalf("the retired decisions document reads version %d after the refused mark, want 1", versions.Number)
	}
}

// The close's ask-state write (docs.WriteIssueAskStates) reads each document's retirement under
// the document owner's lock, the lock docs.RetireDocument takes, and not only in the list it walks:
// a retirement that commits after the list is read, while the write waits on that lock, is read
// there, and the retired document is skipped. Every route that runs the write takes the issue's
// owner lock before the list (lockPatchOwnersAndParent, sweepClosedIssueAsksTx,
// RunIssueAskRepair), so no retirement commits between the two on a route; here the write runs in
// a transaction that took no lock before it. The retirement of notes.md holds the issue's owner
// lock, uncommitted, while the write reads its list, which finds notes.md standing, and commits
// once the write waits on that lock. The write answers no error and gives notes.md no version.
func TestAnAskStateWriteSkipsADocumentRetiredAfterItsListWasRead(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	notes := uploadIssueDocument(t, handler, issue.Key, "notes.md", "Notes.\n")
	settleIssue(t, documents, issue.Key)
	before := latestVersion(t, database, notes.ID).Number
	alice := model.Actor{Kind: "user", ID: "alice"}

	ctx := context.Background()
	retirement, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retirement: %v", err)
	}
	defer retirement.Rollback(ctx)
	if _, err := docs.RetireDocument(ctx, retirement, notes.ID, alice, "The test retires it."); err != nil {
		t.Fatalf("retire %s: %v", notes.ID, err)
	}
	written := make(chan error, 1)
	go func() {
		written <- func() error {
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			joined, ledger := documents.Join(ctx, tx)
			defer ledger.Discard()
			if err := documents.WriteIssueAskStates(joined, issue.Key, nil, alice, true); err != nil {
				return err
			}
			return ledger.Commit(ctx)
		}()
	}()
	waitForDatabaseLocks(t, retirement, 1)
	if err := retirement.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("the ask-state write whose list read notes.md before its retirement committed answered %v, want no error: WriteIssueAskStates reads the retirement under the owner's lock and skips the retired document", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the ask-state write never answered once the retirement committed")
	}
	if after := latestVersion(t, database, notes.ID).Number; after != before {
		t.Fatalf("the ask-state write gave the retired notes.md version %d, past its version %d", after, before)
	}
}

// SettleIssueDocuments, which the close runs before its transaction (settleIssueBeforeClose),
// settles every document of the issue that owes a settlement, a retired one too: the retired
// document's settlement stores the updates it read and writes no version (docs' commitVersion,
// artifactOwner.Retired), and the call answers no error for it. notes.md takes a browser's edit,
// which leaves it owing a settlement, and is retired before that settlement runs.
func TestSettlingAnIssuesDocumentsSettlesARetiredOneWithNoErrorAndNoVersion(t *testing.T) {
	t.Parallel()
	handler, database, documentService := closedSettlementHandlerSettlingAfter(t, time.Hour)
	issue := createInteractionIssue(t, handler, "TEST", "A retired document owing a settlement", "Context\n")
	notes := uploadIssueDocument(t, handler, issue.Key, "notes.md", "Context\n")
	settleIssue(t, documentService, issue.Key)
	before := latestVersion(t, database, notes.ID).Number

	peer := connectBrowserPeer(t, handler, documentService, notes.ID)
	stored := storedUpdateCount(t, database, notes.ID)
	peer.edit(t, replaceFirstParagraph("Context", "Context, typed before the retirement"))
	peer.barrier(t)
	awaitStoredUpdates(t, database, notes.ID, stored+1)
	retireTestDocument(t, database, notes.ID)

	if err := documentService.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
		t.Fatalf("settle the documents of %s, notes.md retired and owing a settlement: %v, want no error", issue.Key, err)
	}
	awaitSettled(t, database, notes.ID)
	if after := latestVersion(t, database, notes.ID).Number; after != before {
		t.Fatalf("the settlement of the retired notes.md wrote version %d, past its version %d", after, before)
	}
}
