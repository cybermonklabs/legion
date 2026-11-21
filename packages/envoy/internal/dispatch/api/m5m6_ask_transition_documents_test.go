package api

import (
	"crypto/sha256"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// anchoredQuestion opens, as carol, a question on the seed's PUB issue anchored on its spec, and
// then rewrites its stored anchor to name PRIV's document, as seedAskDocuments does for its
// anchored question: a fresh one for each write below, since each write may close it.
func (s askDocumentSeed) anchoredQuestion(t *testing.T) string {
	t.Helper()
	id := createdID(t, s.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+s.anchoredIssue+"/asks", map[string]any{
		"question": "Why tonight?", "options": []map[string]string{{"label": "Yes"}, {"label": "No"}},
		"anchor": map[string]any{"artifact": "spec", "quote": "build"},
	}))
	if tag, err := s.database.Pool.Exec(t.Context(),
		`update asks set anchor = jsonb_set(anchor, '{artifact_id}', to_jsonb($2::text)) where id = $1`,
		id, s.privateDocumentID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("store the anchor of %s naming PRIV's document: %v", id, err)
	}
	return id
}

// approvalRequest opens, as the shared token for a session, an approval request on the spec of a
// new PUB issue assigned to alice, and then rewrites its stored approval to name PRIV's document,
// as seedAskDocuments does: a fresh request for each write, on an issue of its own, since a
// document holds one open request.
func (s askDocumentSeed) approvalRequest(t *testing.T) string {
	t.Helper()
	created := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, s.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": seedPublic, "title": "Work to approve", "spec": "Approve the build.", "force": true,
	}, http.StatusCreated))
	s.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"assignee": seedAlice}, http.StatusOK)
	id := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, s.mustRequest(t, callerSharedToken, http.MethodPost, "/api/v1/artifacts/"+created.PrimaryArtifactID+"/approval-requests", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s-architect"},
	}, http.StatusCreated)).Ask.ID
	if tag, err := s.database.Pool.Exec(t.Context(),
		`update asks set approval = jsonb_set(jsonb_set(approval, '{artifact_id}', to_jsonb($2::text)), '{documents,0,artifact_id}', to_jsonb($2::text)) where id = $1`,
		id, s.privateDocumentID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("store the approval of %s naming PRIV's document: %v", id, err)
	}
	return id
}

// askWrite is one write of an ask whose 200 answers the ask: its route, its body, and the ask it
// writes, made fresh for each caller.
type askWrite struct {
	name, method, route string
	body                map[string]any
	ask                 func(askDocumentSeed, *testing.T) string
	// approval says the ask is an approval request, which names its document as its approval
	// rather than as its anchor.
	approval bool
}

// The writes whose 200 answers the ask they wrote from the transition read (readAskForTransition):
// the edit, the three transitions of a question, and the resolution of an approval request - the
// one transition an approval request takes from a person who may not read its document, as its
// answer reads that document first and a decline is refused for every approval request. The
// hand-on is not among them: its 200 is loadAsk's read (ask_asked_of.go), which askRowFromSeenBy
// already binds.
var (
	askEdit = askWrite{name: "the edit", method: http.MethodPatch, body: map[string]any{"question": "Why tonight, exactly?"},
		ask: askDocumentSeed.anchoredQuestion}
	askAnswer = askWrite{name: "the answer", method: http.MethodPost, route: "/answer",
		body: map[string]any{"selected": []string{"Yes"}, "expected_edited_at": nil}, ask: askDocumentSeed.anchoredQuestion}
	askQuestionResolution = askWrite{name: "the resolution of a question", method: http.MethodPost, route: "/resolve",
		body: map[string]any{"kind": "resolved", "reason": "Settled."}, ask: askDocumentSeed.anchoredQuestion}
	askDecline = askWrite{name: "the decline", method: http.MethodPost, route: "/decline",
		body: map[string]any{"note": "Not mine to answer."}, ask: askDocumentSeed.anchoredQuestion}
	askApprovalResolution = askWrite{name: "the resolution of an approval request", method: http.MethodPost, route: "/resolve",
		body: map[string]any{"kind": "retracted", "reason": "Not needed."}, ask: askDocumentSeed.approvalRequest, approval: true}
)

// writeAsk makes write on a fresh ask as caller and answers the ask's id and the 200's body.
func (s askDocumentSeed) writeAsk(t *testing.T, write askWrite, caller privateCaller) (string, []byte) {
	t.Helper()
	ask := write.ask(s, t)
	return ask, s.mustRequest(t, caller, write.method, "/api/v1/asks/"+ask+write.route, write.body, http.StatusOK).Body.Bytes()
}

// wantDocumentsOf is what a writer who sees PRIV reads beside the ask write made: an approval
// request names PRIV's document as its approval, a question as its anchor.
func (s askDocumentSeed) wantDocumentsOf(write askWrite) []string {
	field := "anchor"
	if write.approval {
		field = "approval"
	}
	return []string{field + " " + s.privateDocument.Project + "/" + s.privateDocument.Slug}
}

// checkAskWrite is each write's row: alice, who does not see PRIV, makes write on an ask whose
// stored anchor or approval names PRIV's document and reads in the 200 no anchor_artifact, no
// approval_artifact, and no project, slug or name of that document; carol, who sees PRIV, making
// the same write on an ask of the same kind, reads the document beside it.
func checkAskWrite(t *testing.T, write askWrite) {
	t.Helper()
	seed := seedAskDocuments(t)
	ask, body := seed.writeAsk(t, write, callerAlice)
	if got, held := askDocuments(t, body, []string{ask})[ask]; !held {
		t.Errorf("alice: %s of %s answers %s, which holds no ask %s, so this write measures nothing", write.name, ask, body, ask)
	} else if len(got) != 0 {
		t.Errorf("alice: %s of %s answers the ask beside %v, want no document: PRIV's is a document she may not see", write.name, ask, got)
	}
	for _, named := range []string{`"project":"` + seedPrivate + `"`, `"slug":"` + seed.privateDocument.Slug + `"`, `"name":"` + seed.privateDocument.Name + `"`} {
		if strings.Contains(string(body), named) {
			t.Errorf("alice: %s of %s names %s of PRIV's document", write.name, ask, named)
		}
	}
	ask, body = seed.writeAsk(t, write, callerCarol)
	if got := askDocuments(t, body, []string{ask})[ask]; !slices.Equal(got, seed.wantDocumentsOf(write)) {
		t.Errorf("carol: %s of %s answers the ask beside %v, want %v", write.name, ask, got, seed.wantDocumentsOf(write))
	}
}

// The rows below are checkAskWrite, one per write. Control for each: readAskForTransition reading
// through the unbound askRowFrom again; every row then fails naming PRIV's document.

// TestM5M6AnAskEditNamesNoDocumentItsWriterMayNotSee: PATCH /api/v1/asks/{id} (checkAskWrite).
func TestM5M6AnAskEditNamesNoDocumentItsWriterMayNotSee(t *testing.T) { checkAskWrite(t, askEdit) }

// TestM5M6AnAnswerNamesNoDocumentItsWriterMayNotSee: POST /api/v1/asks/{id}/answer (checkAskWrite).
func TestM5M6AnAnswerNamesNoDocumentItsWriterMayNotSee(t *testing.T) { checkAskWrite(t, askAnswer) }

// TestM5M6AQuestionsResolutionNamesNoDocumentItsWriterMayNotSee: POST /api/v1/asks/{id}/resolve on
// a question (checkAskWrite).
func TestM5M6AQuestionsResolutionNamesNoDocumentItsWriterMayNotSee(t *testing.T) {
	checkAskWrite(t, askQuestionResolution)
}

// TestM5M6ADeclineNamesNoDocumentItsWriterMayNotSee: POST /api/v1/asks/{id}/decline (checkAskWrite).
func TestM5M6ADeclineNamesNoDocumentItsWriterMayNotSee(t *testing.T) { checkAskWrite(t, askDecline) }

// TestM5M6AnApprovalsResolutionNamesNoDocumentItsWriterMayNotSee: POST /api/v1/asks/{id}/resolve on
// an approval request (checkAskWrite).
func TestM5M6AnApprovalsResolutionNamesNoDocumentItsWriterMayNotSee(t *testing.T) {
	checkAskWrite(t, askApprovalResolution)
}

// TestM5M6AnApprovalsAnswerNamesNoDocumentItsWriterMayNotSee: POST /api/v1/asks/{id}/answer on an
// approval request whose approval names PRIV's document answers alice, who does not see PRIV, no
// 200 at all: the answer reviews that document, which it reads as its writer first (answerAsk's
// After, loadArtifact), and alice reads 404 ARTIFACT_NOT_FOUND, naming no project, slug, name or id
// of it - at the base and here alike, so this row is the evidence that this route never leaked,
// not a row a fix turns green. carol, who sees PRIV, approves her own request and reads the
// document beside it in the 200.
func TestM5M6AnApprovalsAnswerNamesNoDocumentItsWriterMayNotSee(t *testing.T) {
	seed := seedAskDocuments(t)
	answer := func(caller privateCaller, ask string) (int, []byte) {
		t.Helper()
		edited := decodeBody[struct {
			Ask struct {
				EditedAt *string `json:"edited_at"`
			} `json:"ask"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/asks/"+ask, nil, http.StatusOK)).Ask.EditedAt
		response := seed.request(t, caller, http.MethodPost, "/api/v1/asks/"+ask+"/answer", map[string]any{
			"selected": []string{"Approve"}, "expected_edited_at": edited,
		})
		return response.Code, response.Body.Bytes()
	}
	ask := seed.approvalRequest(t)
	status, body := answer(callerAlice, ask)
	t.Logf("alice's answer of approval request %s: %d %s", ask, status, body)
	if status == http.StatusOK {
		t.Errorf("alice's answer of approval request %s answers 200, want a refusal: its answer reviews a document she may not see", ask)
	}
	for _, named := range []string{seedPrivate, seed.privateDocument.Slug, seed.privateDocument.Name, seed.privateDocumentID} {
		if strings.Contains(string(body), named) {
			t.Errorf("alice's answer of approval request %s names %s of PRIV's document: %s", ask, named, body)
		}
	}
	ask = seed.approvalRequest(t)
	status, body = answer(callerCarol, ask)
	if got := askDocuments(t, body, []string{ask})[ask]; status != http.StatusOK || !slices.Equal(got, seed.wantDocumentsOf(askApprovalResolution)) {
		t.Errorf("carol's answer of approval request %s answers %d beside %v, want 200 beside %v: %s", ask, status, got, seed.wantDocumentsOf(askApprovalResolution), body)
	}
}

// TestM5M6WithNoPrivateProjectAnAskWriteNamesItsDocumentsAsBefore (R9): with PRIV public alice, a
// full member, reads in every write's 200 the document beside the ask, as the base answered it.
// The sha256 of each 200, its times and ids blanked, is logged for the report's comparison of the
// two commits.
//
// Control: askRowFromSeenBy's anchor clause made false for every reader; alice then reads no
// anchor document in the question writes and the row fails.
func TestM5M6WithNoPrivateProjectAnAskWriteNamesItsDocumentsAsBefore(t *testing.T) {
	seed := seedAskDocuments(t)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, write := range []askWrite{askEdit, askAnswer, askQuestionResolution, askDecline, askApprovalResolution} {
		ask, body := seed.writeAsk(t, write, callerAlice)
		blanked := timestampsAndIDs.ReplaceAllString(string(body), "<varies>")
		t.Logf("R9 %s sha256 %x", write.name, sha256.Sum256([]byte(blanked)))
		if got := askDocuments(t, body, []string{ask})[ask]; !slices.Equal(got, seed.wantDocumentsOf(write)) {
			t.Errorf("PRIV public: alice's %s of %s answers the ask beside %v, want %v, as before", write.name, ask, got, seed.wantDocumentsOf(write))
		}
	}
}
