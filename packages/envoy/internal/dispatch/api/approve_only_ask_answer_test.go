package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// TestAnApproveOnlyTokenAnswersAnApprovalAskAndNoOtherAsk: the approve use reaches "the answer of
// an approval ask" (spec R17's "A person's approvals through the airlock", item 3), and Dispatch
// refuses an approve-only token on any route outside that list (item 4). POST
// /api/v1/asks/{id}/answer serves every kind of ask, so the approve list naming the route lets the
// approve-only token answer a plain question as its person, free text and all, which is no
// approval. Such an answer is refused 403 APPROVE_ONLY_TOKEN and nothing is recorded; the same
// token's answer of an approval ask of the same issue is recorded, which is the control that the
// token and the route work.
func TestAnApproveOnlyTokenAnswersAnApprovalAskAndNoOtherAsk(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const approveOnly = "an-approve-only-token-answering-asks"
	rig.broker.Says(approveOnly, approveOnlyAnswer(caller.jkt))
	issue := createInteractionIssue(t, rig.handler, "APQ", "Asks under the approve-only token", "A document to approve.\n")

	opened := sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "A plain question for the person?",
		"options":  []map[string]any{{"label": "Yes"}, {"label": "No"}},
		"actor":    sessionActor(),
	})
	if opened.Code != http.StatusCreated {
		t.Fatalf("open the question = %d %s; want 201", opened.Code, opened.Body.String())
	}
	question := decodeBody[struct {
		ID string `json:"id"`
	}](t, opened)

	const written = "free text written by the approve-only token"
	answered := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/asks/"+question.ID+"/answer", approveOnly, map[string]any{
		"selected": []string{"Yes"}, "text": written, "expected_edited_at": nil,
	}))
	if refusal := decodeBody[refusalAnswer](t, answered); answered.Code != http.StatusForbidden || refusal.Code != contracts.DispatchApproveOnlyToken {
		t.Errorf("the approve-only token's answer of a question ask = %d %s; want 403 %s", answered.Code, answered.Body.String(), contracts.DispatchApproveOnlyToken)
	}
	read := sessionRequest(t, rig.handler, http.MethodGet, "/api/v1/asks/"+question.ID, nil)
	if read.Code != http.StatusOK || strings.Contains(read.Body.String(), written) {
		t.Errorf("the question after the approve-only token's answer = %d %s; want 200 with no answer of that token", read.Code, read.Body.String())
	}

	// The control: the same token answers an approval ask of the same issue.
	requested := sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
		"summary": "Approve this document?", "actor": sessionActor(),
	})
	if requested.Code != http.StatusCreated {
		t.Fatalf("open the approval request = %d %s; want 201", requested.Code, requested.Body.String())
	}
	approvalID := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID
	if approvalID == "" {
		t.Fatalf("the approval request opened no ask: %s", requested.Body.String())
	}
	approved := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/asks/"+approvalID+"/answer", approveOnly, map[string]any{
		"selected": []string{"Approve"}, "expected_edited_at": nil,
	}))
	if approved.Code != http.StatusOK {
		t.Fatalf("the approve-only token's answer of the approval ask = %d %s; want 200", approved.Code, approved.Body.String())
	}
}
