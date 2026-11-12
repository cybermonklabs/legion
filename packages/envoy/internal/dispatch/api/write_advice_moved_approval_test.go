package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The report a document write carries when its version took an open approval request back from
// the person it was waiting on (movedApprovalRequestAdvice): the ask, the place it was opened in,
// and every document it names with the version it asks about beside the version that document
// stands at now. The rows below are the states that decide whether it comes at all - a request
// the write moved, one the write's own carry answered, one no write moved, and a write that
// stored no version - and, where it comes, what it says.

// movedApprovalState is the state rows over a two-document request share: an issue whose spec and
// Verification document each stand at version 1, and one open approval request over both of them,
// opened by the asking agent and still with the person.
type movedApprovalState struct {
	key          string
	spec         string
	verification string
	ask          string
}

// openMovedApprovalRequest builds that state in project, which it creates, and answers it.
func openMovedApprovalRequest(t *testing.T, handler http.Handler, project, title string) movedApprovalState {
	t.Helper()
	createAdviceProject(t, handler, project)
	spec := "# Transport\n\nThe design, as it stands.\n"
	issue := createAdviceIssue(t, handler, project, title, &spec)
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", "# Verification\n\nOne.\n")
	request := requestApprovalWithVerification(t, handler, issue.PrimaryArtifactID)
	if request.Approval.Version != 1 || request.Approval.RequestedVersion != 1 {
		t.Fatalf("approval request = %#v, want one at version 1 of the spec", request.Approval)
	}
	return movedApprovalState{
		key:          issue.Key,
		spec:         issue.PrimaryArtifactID,
		verification: verification.Artifact.ID,
		ask:          request.ID,
	}
}

// unchangedEdit runs an edit whose one operation replaces find with itself, so the document is as
// it was and no version is stored, and answers the advice beside the edit's own result.
func unchangedEdit(t *testing.T, handler http.Handler, artifactID, find string) (*testWriteAdvice, editArtifactResponse) {
	t.Helper()
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"actor": adviceSessionActor("session-editor"),
		"ops":   []map[string]string{replaceOp(find, find)},
	})
	advice := adviceFromResponse(t, edited.Code, edited.Body.String())
	return advice, decodeBody[editArtifactResponse](t, edited)
}

// The whole report, over the edit route. A request names the issue's spec and its Verification
// document, each at version 1, and an edit stores version 2 of the Verification document: that
// version took the request back from the person, so the edit's own answer names the ask, the
// issue it was opened on and no project, and both documents in the request's own order - the
// spec, the issue's primary document, in no role and at the version the request asks about, and
// the Verification document in its role, one version above the version the request asks about.
func TestMovedApprovalRequestNamesTheAskTheOwnerAndEveryDocument(t *testing.T) {
	handler := newTestHandler(t)
	state := openMovedApprovalRequest(t, handler, "MOVED", "Moved onto the agent")

	advice := editAdviceDocument(t, handler, state.verification, "A line the agent added.")
	moved := advice.MovedApprovalRequest
	if moved == nil {
		t.Fatalf("advice = %#v, want the approval request the edit moved", advice)
	}
	if moved.AskID != state.ask || moved.Issue != state.key || moved.Project != "" {
		t.Fatalf("moved_approval_request = %#v, want ask %s on issue %s and no project", moved, state.ask, state.key)
	}
	want := []movedApprovalDocument{
		{ArtifactID: state.spec, Name: "spec.md", Primary: true, RequestedVersion: 1, Version: 1},
		{ArtifactID: state.verification, Name: "verification.md", Role: model.ArtifactRoleVerification, RequestedVersion: 1, Version: 2},
	}
	if !reflect.DeepEqual(moved.Documents, want) {
		t.Fatalf("moved_approval_request = %#v, want the documents %#v", moved, want)
	}
}

// A null `role` column reads the role its slug decides. The same state, with the Verification
// document's row left with no role - what a rolling deploy leaves on a row created before the
// column was written - and the report still names the role, because it asks documentPlays and
// not the column (documentRoleOf).
func TestMovedApprovalRequestReadsTheRoleTheSlugDecides(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	state := openMovedApprovalRequest(t, handler, "ROLELESS", "Written before the role column")
	if _, err := database.Pool.Exec(context.Background(),
		`update artifacts set role = null where id = $1`, state.verification); err != nil {
		t.Fatalf("clear the stored role: %v", err)
	}

	advice := editAdviceDocument(t, handler, state.verification, "A line the agent added.")
	moved := advice.MovedApprovalRequest
	if moved == nil || len(moved.Documents) != 2 {
		t.Fatalf("advice = %#v, want the moved request and the two documents it names", advice)
	}
	verification := moved.Documents[1]
	if verification.ArtifactID != state.verification || verification.Role != model.ArtifactRoleVerification ||
		verification.Primary || verification.RequestedVersion != 1 || verification.Version != 2 {
		t.Fatalf("moved_approval_request = %#v, want %s in the verification role at version 2, requested at 1",
			moved, state.verification)
	}
}

// The replace route reports it too. The same state, moved by an upload of a new version of the
// Verification document over POST /api/v1/issues/{key}/artifacts rather than by an edit: that
// route writes its version itself and moves the request itself (storeArtifact), so it owes its
// writer the same report.
func TestMovedApprovalRequestComesFromTheReplaceRoute(t *testing.T) {
	handler := newTestHandler(t)
	state := openMovedApprovalRequest(t, handler, "UPMOVED", "Moved by an upload")

	replaced := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+state.key+"/artifacts", map[string]any{
		"actor": adviceSessionActor("session-editor"),
		"name":  "verification.md", "content": "# Verification\n\nOne, rewritten.\n",
	})
	advice := adviceFromResponse(t, replaced.Code, replaced.Body.String())
	moved := advice.MovedApprovalRequest
	if moved == nil {
		t.Fatalf("upload advice = %#v, want the approval request the upload moved", advice)
	}
	if moved.AskID != state.ask || moved.Issue != state.key || moved.Project != "" {
		t.Fatalf("moved_approval_request = %#v, want ask %s on issue %s and no project", moved, state.ask, state.key)
	}
	want := []movedApprovalDocument{
		{ArtifactID: state.spec, Name: "spec.md", Primary: true, RequestedVersion: 1, Version: 1},
		{ArtifactID: state.verification, Name: "verification.md", Role: model.ArtifactRoleVerification, RequestedVersion: 1, Version: 2},
	}
	if !reflect.DeepEqual(moved.Documents, want) {
		t.Fatalf("moved_approval_request = %#v, want the documents %#v", moved, want)
	}
}

// The upload route reports it with no issue state beside it. The same upload, with the issue
// advice this session's write asks for failing (newFailingAdviceHandler): the document report is
// about the document alone, so it goes out on its own shape - the block count the upload read
// and the request its version took back - rather than being dropped with the issue state that
// failed, which is how the edit route answers one too
// (TestDocumentEditAdviceKeepsCountWhenIssueAdviceFails).
func TestMovedApprovalRequestRidesTheUploadAdviceWhenIssueAdviceFails(t *testing.T) {
	handler, _ := newFailingAdviceHandler(t, "session_writes_since_human", 0)
	state := openMovedApprovalRequest(t, handler, "UPFAILED", "Moved by an upload whose issue advice failed")

	replaced := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+state.key+"/artifacts", map[string]any{
		"actor": adviceSessionActor("session-editor"),
		"name":  "verification.md", "content": "# Verification\n\nOne, rewritten.\n",
	})
	var envelope struct {
		Advice map[string]json.RawMessage `json:"advice"`
	}
	if err := json.Unmarshal(replaced.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode upload %q: %v", replaced.Body.String(), err)
	}
	if _, carried := envelope.Advice["issue_status"]; carried {
		t.Fatalf("upload advice = %v, want no issue state beside the document report: its read failed", envelope.Advice)
	}
	advice := adviceFromResponse(t, replaced.Code, replaced.Body.String())
	if advice.DecisionBlocks == nil || *advice.DecisionBlocks != 0 {
		t.Fatalf("upload advice = %#v, want the document's own block count: %s", advice, replaced.Body.String())
	}
	moved := advice.MovedApprovalRequest
	if moved == nil {
		t.Fatalf("upload advice = %#v, want the approval request the upload moved", advice)
	}
	if moved.AskID != state.ask || moved.Issue != state.key || moved.Project != "" {
		t.Fatalf("moved_approval_request = %#v, want ask %s on issue %s and no project", moved, state.ask, state.key)
	}
	want := []movedApprovalDocument{
		{ArtifactID: state.spec, Name: "spec.md", Primary: true, RequestedVersion: 1, Version: 1},
		{ArtifactID: state.verification, Name: "verification.md", Role: model.ArtifactRoleVerification, RequestedVersion: 1, Version: 2},
	}
	if !reflect.DeepEqual(moved.Documents, want) {
		t.Fatalf("moved_approval_request = %#v, want the documents %#v", moved, want)
	}
	if len(envelope.Advice) != 2 {
		t.Fatalf("upload advice fields = %v, want the block count and the moved request alone", envelope.Advice)
	}
}

// A document of a project, in the other advice shape. A project document has no issue state to
// report, so its edit's whole advice is the document report (documentWriteAdvice): the moved
// request rides it there, naming the project its document belongs to and no issue.
func TestMovedApprovalRequestRidesTheProjectDocumentAdvice(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "PROJMOVED")
	document := uploadDocument(t, handler, "/api/v1/projects/PROJMOVED", "notes.md", "# Notes\n\nOne.\n")
	request := requestApproval(t, handler, document.Artifact.ID)

	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.Artifact.ID+"/edits", map[string]any{
		"actor": adviceSessionActor("session-editor"),
		"ops":   []map[string]any{{"op": "insert", "markdown": "A line the agent added.", "after": "end"}},
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("project document edit: status=%d body=%s", edited.Code, edited.Body.String())
	}
	var envelope struct {
		Advice map[string]json.RawMessage `json:"advice"`
	}
	if err := json.Unmarshal(edited.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode project document edit %q: %v", edited.Body.String(), err)
	}
	if _, carried := envelope.Advice["issue_status"]; carried {
		t.Fatalf("project document edit advice = %v, want no issue state beside the document report", envelope.Advice)
	}
	raw, reported := envelope.Advice["moved_approval_request"]
	if !reported {
		t.Fatalf("project document edit advice = %v, want the approval request the edit moved", envelope.Advice)
	}
	var moved movedApprovalRequest
	if err := json.Unmarshal(raw, &moved); err != nil {
		t.Fatalf("decode moved_approval_request %s: %v", raw, err)
	}
	if moved.AskID != request.ID || moved.Project != "PROJMOVED" || moved.Issue != "" {
		t.Fatalf("moved_approval_request = %#v, want ask %s on project PROJMOVED and no issue", moved, request.ID)
	}
	want := []movedApprovalDocument{
		{ArtifactID: document.Artifact.ID, Name: "notes.md", RequestedVersion: 1, Version: 2},
	}
	if !reflect.DeepEqual(moved.Documents, want) {
		t.Fatalf("moved_approval_request = %#v, want the documents %#v", moved, want)
	}
}

// No open request, no report. An edit of a document nobody has asked a person to approve took
// nothing back from anybody, so its answer names no request: the field exists to tell a writer
// what their write just did, and this write did none of it.
func TestMovedApprovalRequestIsAbsentWhenNoRequestNamesTheDocument(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "UNASKED")
	spec := "# Draft\n\nNobody has been asked to approve this.\n"
	issue := createAdviceIssue(t, handler, "UNASKED", "No request at all", &spec)

	advice := editAdviceDocument(t, handler, issue.PrimaryArtifactID, "A line the agent added.")
	if advice.MovedApprovalRequest != nil {
		t.Fatalf("moved_approval_request = %#v, want none: no request names the document", advice.MovedApprovalRequest)
	}
}

// A write whose own carry answered the request reports nothing. The person approved version 2,
// the agent reworded an open decision block, and the request stands at version 3; the person's
// answer to the other block is the whole change since the version he approved, and the agent's
// edit versions it in the route's own transaction, so that version carries his approval
// (docs.carryApproval) and answers the request. The request is answered, not moved onto the
// agent's turn, so the edit's answer names it nowhere.
func TestMovedApprovalRequestIsAbsentWhenTheWritesOwnCarryAnsweredIt(t *testing.T) {
	handler, _, documents := settlingCarryDocuments(t, time.Second)
	issue := createInteractionIssue(t, handler, "TEST", "Carried, not moved", "Context\n")
	insertDecisionMarkdown(t, handler, issue.PrimaryArtifactID, waveAsk+"\n"+regionAsk, 2)
	asks := settledDecisionBlocks(t, handler, documents, issue.Key, issue.PrimaryArtifactID,
		map[string]string{"wave": "How many regions take the first wave?", "region": "Which region goes first?"})
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	reviseDocument(t, handler, issue.PrimaryArtifactID, "Which region goes first?", "Which cloud region goes first?", 3)
	request := requestApproval(t, handler, issue.PrimaryArtifactID)

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": adviceSessionActor("session-editor"),
		"ops":   []map[string]string{{"op": "replace", "find": "Which cloud region goes first?", "with": "Which cloud region ships first?"}},
	})
	advice := adviceFromResponse(t, edited.Code, edited.Body.String())
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 4, 2, &request.ID)
	if answered := readBlockAsk(t, handler, request.ID); answered.State != "answered" {
		t.Fatalf("request after the carry = %q, want answered", answered.State)
	}
	if advice.MovedApprovalRequest != nil {
		t.Fatalf("moved_approval_request = %#v, want none: the write's own carry answered the request",
			advice.MovedApprovalRequest)
	}
}

// No version stored, no report. The request already stands above the version it asks about - one
// edit has moved it - and the next edit's operations leave the document as it was, so no version
// is stored. Nothing moved, and the writer hears nothing: the report is of the move this write
// made, not of the state the document is in.
func TestMovedApprovalRequestIsAbsentWhenTheEditStoredNoVersion(t *testing.T) {
	handler := newTestHandler(t)
	state := openMovedApprovalRequest(t, handler, "NOVERSION", "Moved, then an edit that changes nothing")
	if moved := editAdviceDocument(t, handler, state.verification, "A line the agent added.").MovedApprovalRequest; moved == nil {
		t.Fatal("the first edit reported no moved request, so the state this row needs was never reached")
	}

	advice, result := unchangedEdit(t, handler, state.verification, "One.")
	if result.Changed || result.Version != nil {
		t.Fatalf("unchanged edit = %#v, want changed=false and no version", result)
	}
	if advice.MovedApprovalRequest != nil {
		t.Fatalf("moved_approval_request = %#v, want none: the edit stored no version", advice.MovedApprovalRequest)
	}
}

// The report comes again on the next version. After a move and an edit that stored nothing, a
// second edit stores a version while the request already stands above the version it asks about:
// the report comes again, at the new version. This is where the report parts from Dispatch's own
// quiet-move rule, which wakes the person's side only on the first move - every write that takes
// the request further from the person tells its own writer.
func TestMovedApprovalRequestComesAgainOnTheNextVersion(t *testing.T) {
	handler := newTestHandler(t)
	state := openMovedApprovalRequest(t, handler, "AGAIN", "Moved twice")
	if moved := editAdviceDocument(t, handler, state.verification, "A line the agent added.").MovedApprovalRequest; moved == nil {
		t.Fatal("the first edit reported no moved request, so the state this row needs was never reached")
	}
	if _, result := unchangedEdit(t, handler, state.verification, "One."); result.Changed {
		t.Fatalf("unchanged edit = %#v, want changed=false", result)
	}

	advice := editAdviceDocument(t, handler, state.verification, "Another line the agent added.")
	moved := advice.MovedApprovalRequest
	if moved == nil || moved.AskID != state.ask || len(moved.Documents) != 2 {
		t.Fatalf("second move's advice = %#v, want ask %s and the two documents it names", advice, state.ask)
	}
	verification := moved.Documents[1]
	if verification.ArtifactID != state.verification || verification.RequestedVersion != 1 || verification.Version != 3 {
		t.Fatalf("moved_approval_request = %#v, want %s at version 3, requested at 1", moved, state.verification)
	}
}

// An open request still at the version it asks about reports nothing. The request stands at the
// version its agent handed over, and the edit's operations change nothing, so no document of it
// stands above that version: the request is still the person's to answer and the write took
// nothing back. (The row above is a write with no version while the request has already moved;
// this one is a write with no version while it has not.)
func TestMovedApprovalRequestIsAbsentWhileTheRequestStandsWhereItWasHandedOver(t *testing.T) {
	handler := newTestHandler(t)
	state := openMovedApprovalRequest(t, handler, "HANDEDOVER", "Still with the person")

	advice, result := unchangedEdit(t, handler, state.verification, "One.")
	if result.Changed || result.Version != nil {
		t.Fatalf("unchanged edit = %#v, want changed=false and no version", result)
	}
	if advice.MovedApprovalRequest != nil {
		t.Fatalf("moved_approval_request = %#v, want none: the request still waits on the person",
			advice.MovedApprovalRequest)
	}
}

// A person's write gets the report. A person's write runs none of the advice queries
// (computeWriteAdvice answers only a session), and this report is read outside them, beside the
// version the write stored: so the person who moved the request onto the agent's turn is told so
// too.
func TestMovedApprovalRequestReachesAPersonsWrite(t *testing.T) {
	handler := newTestHandler(t)
	state := openMovedApprovalRequest(t, handler, "BYPERSON", "Moved by a person")

	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+state.verification+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "markdown": "A line alice added.", "after": "end"}},
	}, "alice")
	advice := adviceFromResponse(t, edited.Code, edited.Body.String())
	moved := advice.MovedApprovalRequest
	if moved == nil || moved.AskID != state.ask || len(moved.Documents) != 2 {
		t.Fatalf("a person's edit advice = %#v, want ask %s and the two documents it names", advice, state.ask)
	}
	verification := moved.Documents[1]
	if verification.ArtifactID != state.verification || verification.RequestedVersion != 1 || verification.Version != 2 {
		t.Fatalf("moved_approval_request = %#v, want %s at version 2, requested at 1", moved, state.verification)
	}
}

// A carry that answers only one document of a two-document request. The person's answer to a
// decision block of the spec carried his approval to the version it settled into, which answers
// the spec's half alone: nobody has approved the Verification document, so the request stays
// open. The edit that then versions the Verification document takes it further from him, and the
// report names that document as the one standing above the version the request asks about.
func TestMovedApprovalRequestNamesTheDocumentACarryLeftBehind(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Carried one half", "Context\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", "# Verification\n\nOne.\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2,
		map[string]string{"wave": "How many regions take the first wave?"})
	request := requestApprovalWithVerification(t, handler, issue.PrimaryArtifactID)

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 3, 1, &request.ID)
	if still := readBlockAsk(t, handler, request.ID); still.State != "open" {
		t.Fatalf("request after the carry = %q, want it still open for the Verification document", still.State)
	}

	advice := editAdviceDocument(t, handler, verification.Artifact.ID, "A line the agent added.")
	moved := advice.MovedApprovalRequest
	if moved == nil || moved.AskID != request.ID || moved.Issue != issue.Key {
		t.Fatalf("advice = %#v, want ask %s on issue %s", advice, request.ID, issue.Key)
	}
	want := []movedApprovalDocument{
		{ArtifactID: issue.PrimaryArtifactID, Name: "spec.md", Primary: true, RequestedVersion: 2, Version: 3},
		{ArtifactID: verification.Artifact.ID, Name: "verification.md", Role: model.ArtifactRoleVerification, RequestedVersion: 1, Version: 2},
	}
	if !reflect.DeepEqual(moved.Documents, want) {
		t.Fatalf("moved_approval_request = %#v, want the documents %#v", moved, want)
	}
}

// The --include-verification pair as the server reports it. A request over a spec and its
// issue's Verification document, moved by a write of the spec: the first document the report
// names is the issue's primary document in no role, and another document of the request carries
// the verification role. That pair is what the command-line client reads to decide the
// --include-verification flag of the hand-back it prints, so both halves are asserted where it
// reads them.
func TestMovedApprovalRequestNamesThePrimaryDocumentAndTheVerificationRole(t *testing.T) {
	handler := newTestHandler(t)
	state := openMovedApprovalRequest(t, handler, "INCLUDEVER", "The pair the flag is read from")

	advice := editAdviceDocument(t, handler, state.spec, "A line the agent added.")
	moved := advice.MovedApprovalRequest
	if moved == nil || len(moved.Documents) != 2 {
		t.Fatalf("advice = %#v, want the moved request over the spec and the Verification document", advice)
	}
	first := moved.Documents[0]
	if first.ArtifactID != state.spec || !first.Primary || first.Role != "" ||
		first.RequestedVersion != 1 || first.Version != 2 {
		t.Fatalf("moved_approval_request = %#v, want %s first, the issue's primary document in no role at version 2, requested at 1",
			moved, state.spec)
	}
	second := moved.Documents[1]
	if second.ArtifactID != state.verification || second.Primary || second.Role != model.ArtifactRoleVerification ||
		second.RequestedVersion != 1 || second.Version != 1 {
		t.Fatalf("moved_approval_request = %#v, want %s second, in the verification role and still at version 1",
			moved, state.verification)
	}
}
