package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// TestAnApproveOnlyTokenAnswersAnApprovalAskAndNoOtherAsk: the approve use reaches "the answer of
// an approval ask" (spec R17's "A person's approvals through the airlock", item 3), and Dispatch
// refuses an approve-only token on any route outside that list (item 4). POST
// /api/v1/asks/{id}/answer, and PUT /api/v2/asks/{id}/state with state answered, serve every kind
// of ask, so the approve list naming the route lets the approve-only token answer a plain question
// as its person, free text and all, which is no approval. Such an answer is refused 403
// APPROVE_ONLY_TOKEN and nothing is recorded, on both versions, and so is the same answer sent
// again by the person who gave it, which /api/v2 would otherwise answer as it stands; the same
// token's answer of an approval ask of the same issue is recorded, which is the control that the
// token and the route work. The /api/v2 route's other states are no answer, and the token is held
// to state answered on it: a resolve, a retraction and a decline of either ask are refused 403
// APPROVE_ONLY_TOKEN, and neither ask moves.
func TestAnApproveOnlyTokenAnswersAnApprovalAskAndNoOtherAsk(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const approveOnly, person = "an-approve-only-token-answering-asks", "the-boxs-person-scoped-token-answering-asks"
	rig.broker.Says(approveOnly, approveOnlyAnswer(caller.jkt))
	rig.broker.Says(person, personScopedTokenAnswer(caller.jkt))
	for index, prefix := range approveOnlyVersions {
		t.Run(prefix, func(t *testing.T) {
			// answer sends the answer each version takes, which /api/v2 takes as a state of its one
			// route.
			answer := func(id, token string, body map[string]any) *httptest.ResponseRecorder {
				if prefix == "/api/v1" {
					return rig.serve(caller.presenting(t, http.MethodPost, prefix+"/asks/"+id+"/answer", token, body))
				}
				return rig.serve(caller.presenting(t, http.MethodPut, prefix+"/asks/"+id+"/state", token, body))
			}
			answerBody := func(selected []string, text string) map[string]any {
				body := map[string]any{"selected": selected, "expected_edited_at": nil}
				if text != "" {
					body["text"] = text
				}
				if prefix == "/api/v2" {
					body["state"] = "answered"
				}
				return body
			}
			issue := createInteractionIssue(t, rig.handler, []string{"APQ", "APX"}[index], "Asks under the approve-only token", "A document to approve.\n")

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
			answered := answer(question.ID, approveOnly, answerBody([]string{"Yes"}, written))
			if refusal := decodeBody[refusalAnswer](t, answered); answered.Code != http.StatusForbidden || refusal.Code != contracts.DispatchApproveOnlyToken {
				t.Errorf("the approve-only token's answer of a question ask = %d %s; want 403 %s", answered.Code, answered.Body.String(), contracts.DispatchApproveOnlyToken)
			}
			read := sessionRequest(t, rig.handler, http.MethodGet, "/api/v1/asks/"+question.ID, nil)
			if read.Code != http.StatusOK || strings.Contains(read.Body.String(), written) {
				t.Errorf("the question after the approve-only token's answer = %d %s; want 200 with no answer of that token", read.Code, read.Body.String())
			}

			// The person's own answer of the question, then the same answer sent again under the
			// approve-only token: /api/v2 answers such a repeat as it stands, no change at all, and an
			// approve-only token's answer of a question is refused before that compare. On /api/v1 a
			// second answer is a conflict of the engine's own, which says nothing of the token.
			own := answer(question.ID, person, answerBody([]string{"No"}, ""))
			if own.Code != http.StatusOK {
				t.Fatalf("the person-scoped token's answer of the question = %d %s; want 200", own.Code, own.Body.String())
			}
			if prefix == "/api/v2" {
				again := answer(question.ID, approveOnly, answerBody([]string{"No"}, ""))
				if refusal := decodeBody[refusalAnswer](t, again); again.Code != http.StatusForbidden || refusal.Code != contracts.DispatchApproveOnlyToken {
					t.Errorf("the approve-only token's repeat of the person's answer of a question ask = %d %s; want 403 %s", again.Code, again.Body.String(), contracts.DispatchApproveOnlyToken)
				}
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
			if prefix == "/api/v2" {
				// Before the answer, so each refusal shows the ask moved nowhere: the states that are
				// no answer, on the approval request and on the question.
				for _, ask := range []string{approvalID, question.ID} {
					for _, body := range []map[string]any{
						{"state": "resolved", "reason": "closed by the approve-only token", "actor": sessionActor()},
						{"state": "retracted", "reason": "closed by the approve-only token", "actor": sessionActor()},
						{"state": "declined", "reason": "declined by the approve-only token"},
					} {
						refused := rig.serve(caller.presenting(t, http.MethodPut, prefix+"/asks/"+ask+"/state", approveOnly, body))
						if refusal := decodeBody[refusalAnswer](t, refused); refused.Code != http.StatusForbidden || refusal.Code != contracts.DispatchApproveOnlyToken {
							t.Errorf("the approve-only token's state %v of ask %s = %d %s; want 403 %s", body["state"], ask, refused.Code, refused.Body.String(), contracts.DispatchApproveOnlyToken)
						}
					}
				}
				for _, ask := range []string{approvalID, question.ID} {
					read := sessionRequest(t, rig.handler, http.MethodGet, "/api/v1/asks/"+ask, nil)
					if strings.Contains(read.Body.String(), `"state":"resolved"`) || strings.Contains(read.Body.String(), "by the approve-only token") {
						t.Errorf("ask %s after the approve-only token's refused states = %s; want it unmoved", ask, read.Body.String())
					}
				}
			}
			approved := answer(approvalID, approveOnly, answerBody([]string{"Approve"}, ""))
			if approved.Code != http.StatusOK {
				t.Fatalf("the approve-only token's answer of the approval ask = %d %s; want 200", approved.Code, approved.Body.String())
			}
		})
	}
}
