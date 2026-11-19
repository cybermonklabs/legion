package api

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A version whose text is the text of the version a person approved holds nothing he has not
// agreed to: an accidental edit undone, or a version written over a document that changed
// nothing. Dispatch carries the document's newest approval to that version in the transaction
// that writes it (docs.carryApproval), so the approval reads approved there, the write reports no
// approval it superseded, and a request for approval finds nothing to ask. A person's answer is
// held on the ask's node and changes no byte of the document, so text
// restored around a block answered since is still the approved text. Text one character away
// from the approved text is a change, and a version holding the approved text around a changed
// ask block is judged by the carry's cases (docs.carryVerdict), which carry no question's
// words.

// restoreSpec is the approved text the restore tests edit and restore.
const restoreSpec = "# Plan\n\nThe service ships behind a flag.\n\n- Announce the dates.\n- Pick the regions.\n"

// restoreSentence is the reason a review carried to a version restoring the approved text records.
func restoreSentence(from int) string {
	return fmt.Sprintf("Carried forward from the approval of version %d: this version's text is that version's, unchanged.", from)
}

// agentEdit applies ops through the edits route as the asking agent, in one batch that versions
// the document in the route's own transaction, waits until that version, number, reads, and
// answers the advice the write came back with.
func agentEdit(t *testing.T, handler http.Handler, artifactID string, number int, ops ...map[string]string) *testWriteAdvice {
	t.Helper()
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   ops,
	})
	advice := adviceFromResponse(t, edited.Code, edited.Body.String())
	waitForArtifactVersion(t, handler, artifactID, number)
	return advice
}

// replaceOp is one edit operation replacing find with with.
func replaceOp(find, with string) map[string]string {
	return map[string]string{"op": "replace", "find": find, "with": with}
}

// approvedDocument is a document approveThenChange approved and then changed: its issue, its id and
// the ask of each ask block it was written with, by block id.
type approvedDocument struct {
	issueKey, artifactID string
	asks                 map[string]string
}

// approveThenChange writes restoreSpec followed by extra into a new issue's spec, waits for
// settlement to index each ask block blocks names (id to question), approves version 1 from
// the header as alice, then has the agent change its paragraph at version 2, which stales that
// approval.
func approveThenChange(t *testing.T, handler http.Handler, title, extra string, blocks map[string]string) approvedDocument {
	t.Helper()
	issue := createInteractionIssue(t, handler, "TEST", title, restoreSpec+extra)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, blocks)
	if review := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", ""); review.Version != 1 {
		t.Fatalf("review = %#v, want an approval of version 1", review)
	}
	agentEdit(t, handler, issue.PrimaryArtifactID, 2, replaceOp("behind a flag.", "behind a flag and a canary."))
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
	return approvedDocument{issueKey: issue.Key, artifactID: issue.PrimaryArtifactID, asks: asks}
}

// waveBlock is the one ask block of the restore tests that hold one, waveAsk, by its id.
var waveBlock = map[string]string{"wave": "How many regions take the first wave?"}

// (a) The agent edits the approved spec by accident and undoes the edit: version 3's text is
// version 1's, byte for byte, so version 3 carries alice's approval of version 1 - one approved
// review in her name at version 3 with the restore's reason and no ask, and its artifact.approved
// before version 3's own artifact.version. The undo supersedes no approval, so its advice names
// none, and a request for approval returns the approval and opens nothing.
func TestAVersionRestoringTheApprovedTextCarriesTheApproval(t *testing.T) {
	handler := approvalCarryHandler(t)
	document := approveThenChange(t, handler, "Restore the approved text", "", nil)
	artifactID := document.artifactID

	advice := agentEdit(t, handler, artifactID, 3, replaceOp("behind a flag and a canary.", "behind a flag."))
	if restored, approved := versionMarkdown(t, handler, artifactID, 3), versionMarkdown(t, handler, artifactID, 1); restored != approved {
		t.Fatalf("version 3 = %q, want version 1's text %q", restored, approved)
	}
	requireReviewCarriedWith(t, handler, artifactID, 3, 1, restoreSentence(1), nil)
	if reviews := documentReviews(t, handler, artifactID); len(reviews) != 2 {
		t.Fatalf("reviews = %+v, want the approval and its carry", reviews)
	}
	if advice.SupersededApproval != nil {
		t.Fatalf("the undo's superseded_approval = %#v, want none: its version carries the approval", advice.SupersededApproval)
	}

	approvedSeq, versionSeq := 0, 0
	for _, event := range carryEvents(t, handler, document.issueKey) {
		switch event.Type {
		case "artifact.approved":
			payload := decodePayload[struct {
				Version int     `json:"version"`
				Reason  *string `json:"reason"`
				AskID   *string `json:"ask_id"`
			}](t, event)
			if payload.Version == 3 {
				if event.Actor != (model.Actor{Kind: "user", ID: "alice", Proven: true}) || payload.Reason == nil || *payload.Reason != restoreSentence(1) || payload.AskID != nil {
					t.Fatalf("restored artifact.approved = actor %#v payload %s", event.Actor, event.Payload)
				}
				approvedSeq = event.Seq
			}
		case "artifact.version":
			if decodePayload[struct {
				Version model.Version `json:"version"`
			}](t, event).Version.Number == 3 {
				versionSeq = event.Seq
			}
		}
	}
	if approvedSeq == 0 || versionSeq == 0 || approvedSeq >= versionSeq {
		t.Fatalf("artifact.approved seq %d, artifact.version seq %d, want both for version 3 with the approval first", approvedSeq, versionSeq)
	}

	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusOK || !strings.Contains(requested.Body.String(), `"ask":null`) || !strings.Contains(requested.Body.String(), `"state":"approved"`) {
		t.Fatalf("request after the undo: status=%d body=%s, want the approval at version 3 and no ask", requested.Code, requested.Body.String())
	}
}

// An approval request the agent opened on the accidental version follows the document to the
// undo's version, and the restore then answers it Approve in alice's name with the restore's
// reason, so the review names it and she is asked nothing.
func TestAnOpenApprovalRequestIsAnsweredByTheVersionRestoringTheApprovedText(t *testing.T) {
	handler := approvalCarryHandler(t)
	artifactID := approveThenChange(t, handler, "Request open at the undo", "", nil).artifactID
	request := requestApproval(t, handler, artifactID)
	if request.Approval.Version != 2 || request.Approval.Approved == nil || request.Approval.Approved.Version != 1 {
		t.Fatalf("request = %#v, want one at version 2 following the approval of version 1", request.Approval)
	}

	agentEdit(t, handler, artifactID, 3, replaceOp("behind a flag and a canary.", "behind a flag."))
	requireReviewCarriedWith(t, handler, artifactID, 3, 1, restoreSentence(1), &request.ID)
	answered := readBlockAsk(t, handler, request.ID)
	if answered.State != "answered" || answered.Answer == nil || answered.Answer.User != "alice" ||
		!reflect.DeepEqual(answered.Answer.Selected, []string{"Approve"}) || answered.Answer.Text == nil || *answered.Answer.Text != restoreSentence(1) {
		t.Fatalf("request after the undo = state %q answer %#v, want Approve by alice with the restore's reason", answered.State, answered.Answer)
	}
	if answered.Approval == nil || answered.Approval.Version != 3 {
		t.Fatalf("request after the undo names version %#v, want 3", answered.Approval)
	}
}

// A version written over an approved document that changed nothing - a named version, which the
// versions route writes whatever changed - holds the approved text, so the approval stays current.
func TestANamedVersionThatChangesNothingKeepsTheApprovalCurrent(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Name an unchanged version", restoreSpec)
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")

	named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
		map[string]string{"summary": "nothing changed"}, "alice")
	if named.Code != http.StatusCreated {
		t.Fatalf("name a version: status=%d body=%s", named.Code, named.Body.String())
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
	requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 2, 1, restoreSentence(1), nil)
}

// The upload route writes its version itself, outside the document service's version write, and
// restores an approval as the edit route does: an agent that undoes its edit by uploading the
// approved text again gets version 3 approved in alice's name, and the upload supersedes nothing.
func TestAnUploadOfTheApprovedTextCarriesTheApproval(t *testing.T) {
	handler := approvalCarryHandler(t)
	document := approveThenChange(t, handler, "Upload the approved text again", "", nil)

	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+document.issueKey+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec.md", "content": versionMarkdown(t, handler, document.artifactID, 1),
	})
	advice := adviceFromResponse(t, uploaded.Code, uploaded.Body.String())
	waitForArtifactVersion(t, handler, document.artifactID, 3)
	if restored, approved := versionMarkdown(t, handler, document.artifactID, 3), versionMarkdown(t, handler, document.artifactID, 1); restored != approved {
		t.Fatalf("version 3 = %q, want version 1's text %q", restored, approved)
	}
	requireReviewCarriedWith(t, handler, document.artifactID, 3, 1, restoreSentence(1), nil)
	if advice.SupersededApproval != nil {
		t.Fatalf("the upload's superseded_approval = %#v, want none: its version carries the approval", advice.SupersededApproval)
	}
}

// approveThenOthersAnswer writes restoreSpec and waveAsk into a new issue's spec, has alice approve
// version 1 while the block is open, and has bob answer the block, which writes no version: the
// block's row now holds bob's decision and alice's approval stands
// at version 1. The agent cannot delete the block bob answered (R6): its delete is refused and
// writes no version. Bob then deletes the block, which writes version 2 and stales the approval.
func approveThenOthersAnswer(t *testing.T, handler http.Handler, database *store.Store, title string) approvedDocument {
	t.Helper()
	issue := createInteractionIssue(t, handler, "TEST", title, restoreSpec+"\n"+waveAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveBlock)
	approveVersion(t, handler, issue.PrimaryArtifactID, "alice", 1)
	answerDecisionBlock(t, handler, asks["wave"], "bob", []string{"Two"}, "", nil)
	requireNoVersionPast(t, handler, database, issue.PrimaryArtifactID, 1)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "approved")
	deleteWave := map[string]any{"actor": sessionActor(), "ops": []map[string]string{{"op": "delete", "block": "wave"}}}
	if refused := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", deleteWave); refused.Code != http.StatusConflict ||
		decodeBody[errorBody](t, refused).Code != codeAnsweredAskKept {
		t.Fatalf("the agent deletes the block bob answered: status=%d body=%s, want 409 %s", refused.Code, refused.Body.String(), codeAnsweredAskKept)
	}
	requireNoVersionPast(t, handler, database, issue.PrimaryArtifactID, 1)
	if deleted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", deleteWave, "bob"); deleted.Code != http.StatusOK {
		t.Fatalf("bob deletes the block he answered: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
	return approvedDocument{issueKey: issue.Key, artifactID: issue.PrimaryArtifactID, asks: asks}
}

// The approved text over a block decided since is still the approved text: bob answered the block
// alice approved open, which changes no byte of the document and leaves her approval standing
// (R3: "a person's answer changes no byte of any document, and an approval of that document
// stands through it"), bob deleted the block, and the agent inserts its approved markdown again.
// Version 3 is version 1 byte for byte, and its block agrees with its row - it shows no answer
// state, and asks the wording the row was decided on - so the restore carries alice's approval to
// it, and answers the approval request the agent opened at version 2.
func TestTheApprovedTextOverABlockDecidedSinceCarriesTheApproval(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	document := approveThenOthersAnswer(t, handler, database, "Approved text over a decided block")
	request := requestApproval(t, handler, document.artifactID)

	agentEdit(t, handler, document.artifactID, 3, map[string]string{"op": "insert", "after": "end", "markdown": waveAsk})
	if restored, approved := versionMarkdown(t, handler, document.artifactID, 3), versionMarkdown(t, handler, document.artifactID, 1); restored != approved {
		t.Fatalf("version 3 = %q, want version 1's text %q", restored, approved)
	}
	requireReviewCarriedWith(t, handler, document.artifactID, 3, 1, restoreSentence(1), &request.ID)
	if ask := readBlockAsk(t, handler, request.ID); ask.State != "answered" {
		t.Fatalf("approval request after version 3 = state %q answer %#v, want it answered by the restore", ask.State, ask.Answer)
	}
	if ask := readBlockAsk(t, handler, document.asks["wave"]); ask.State != "answered" || ask.Answer == nil || ask.Answer.User != "bob" {
		t.Fatalf("the restored block's ask = state %q answer %#v, want bob's answer kept", ask.State, ask.Answer)
	}
}

// The upload route holds the blocks to their rows as the edit route does, and so carries alike: an
// upload of version 1's text over the block bob decided since restores the approved text, and
// carries alice's approval.
func TestAnUploadOfTheApprovedTextOverABlockDecidedSinceCarriesTheApproval(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	document := approveThenOthersAnswer(t, handler, database, "Uploaded approved text over a decided block")

	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+document.issueKey+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec.md", "content": versionMarkdown(t, handler, document.artifactID, 1),
	})
	if advice := adviceFromResponse(t, uploaded.Code, uploaded.Body.String()); advice.SupersededApproval != nil {
		t.Fatalf("the upload's superseded_approval = %#v, want none: the approval was already stale", advice.SupersededApproval)
	}
	waitForArtifactVersion(t, handler, document.artifactID, 3)
	if restored, approved := versionMarkdown(t, handler, document.artifactID, 3), versionMarkdown(t, handler, document.artifactID, 1); restored != approved {
		t.Fatalf("version 3 = %q, want version 1's text %q", restored, approved)
	}
	requireReviewCarriedWith(t, handler, document.artifactID, 3, 1, restoreSentence(1), nil)
}

// (b) The undo misses by one character: version 3 is version 1 with its period an exclamation
// mark, which is a change, so the approval stays stale, the undo supersedes nothing (the approval
// was already stale) and a request for approval opens one.
func TestAVersionOneCharacterFromTheApprovedTextLeavesTheApprovalStale(t *testing.T) {
	handler := approvalCarryHandler(t)
	artifactID := approveThenChange(t, handler, "Nearly restore the approved text", "", nil).artifactID

	advice := agentEdit(t, handler, artifactID, 3, replaceOp("behind a flag and a canary.", "behind a flag!"))
	approved, nearly := versionMarkdown(t, handler, artifactID, 1), versionMarkdown(t, handler, artifactID, 3)
	if want := strings.Replace(approved, "behind a flag.", "behind a flag!", 1); nearly != want || len(nearly) != len(approved) {
		t.Fatalf("version 3 = %q, want version 1 with one character changed, %q", nearly, want)
	}
	requireNoCarry(t, handler, artifactID, 1, "stale")
	if advice.SupersededApproval != nil {
		t.Fatalf("the near undo's superseded_approval = %#v, want none: the approval was already stale", advice.SupersededApproval)
	}
	request := requestApproval(t, handler, artifactID)
	if request.Approval.Version != 3 || request.Approval.Approved == nil || request.Approval.Approved.Version != 1 {
		t.Fatalf("request = %#v, want one at version 3 following the approval of version 1", request.Approval)
	}
}

// (c) Version 3 holds version 1's text outside its ask block, restored, and an ask block
// changed since. Such a version is judged by the carry's cases (docs.carryVerdict), which carry
// the server-owned attributes of an ask block and no word of its question: an agent's reword of
// the open block is a change, so the approval stays stale.
func TestARestoredTextAroundADecisionBlockTheAgentRewordedLeavesTheApprovalStale(t *testing.T) {
	handler := approvalCarryHandler(t)
	artifactID := approveThenChange(t, handler, "Restore around a reworded block", "\n"+waveAsk, waveBlock).artifactID

	agentEdit(t, handler, artifactID, 3,
		replaceOp("behind a flag and a canary.", "behind a flag."),
		replaceOp("How many regions take the first wave?", "How many regions go in the first wave?"))
	requireRestoredAroundAChangedBlock(t, handler, artifactID, 3)
	requireNoCarry(t, handler, artifactID, 1, "stale")
}

// (c) Text restored around the approver's own answer: alice answers the open block while the
// paragraph is still changed, which writes no version (R3), and the agent then restores the
// paragraph. Her answer is in no byte of the document, so version 3 is version 1's text byte for
// byte, and the restore carries her approval to it with the restore's reason.
func TestARestoredTextAroundTheApproversOwnAnswerIsTheApprovedText(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	document := approveThenChange(t, handler, "Restore around her answer", "\n"+waveAsk, waveBlock)
	artifactID := document.artifactID

	answerDecisionBlock(t, handler, document.asks["wave"], "alice", []string{"Two"}, "", nil)
	requireNoVersionPast(t, handler, database, artifactID, 2)
	requireNoCarry(t, handler, artifactID, 1, "stale")

	agentEdit(t, handler, artifactID, 3, replaceOp("behind a flag and a canary.", "behind a flag."))
	if restored, approved := versionMarkdown(t, handler, artifactID, 3), versionMarkdown(t, handler, artifactID, 1); restored != approved {
		t.Fatalf("version 3 = %q, want version 1's text %q", restored, approved)
	}
	requireReviewCarriedWith(t, handler, artifactID, 3, 1, restoreSentence(1), nil)
	if ask := readBlockAsk(t, handler, document.asks["wave"]); ask.State != "answered" || ask.Answer == nil || ask.Answer.User != "alice" {
		t.Fatalf("the block's ask = state %q answer %#v, want alice's answer on it", ask.State, ask.Answer)
	}
}

// requireRestoredAroundAChangedBlock fails t unless version number of the document holds version
// 1's text outside its ask blocks and is not version 1's text.
func requireRestoredAroundAChangedBlock(t *testing.T, handler http.Handler, artifactID string, number int) {
	t.Helper()
	approved, restored := versionMarkdown(t, handler, artifactID, 1), versionMarkdown(t, handler, artifactID, number)
	if cutAskBlocks(t, restored) != cutAskBlocks(t, approved) || restored == approved {
		t.Fatalf("version %d = %q, want version 1's text %q outside a changed ask block", number, restored, approved)
	}
}

// cutAskBlocks is markdown rendered again with every top-level ask block left out.
func cutAskBlocks(t *testing.T, markdown string) string {
	t.Helper()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	kept := tree.Children[:0]
	for _, child := range tree.Children {
		if child.Type != "ask" {
			kept = append(kept, child)
		}
	}
	tree.Children = kept
	cut, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render %q without its ask blocks: %v", markdown, err)
	}
	return cut
}
