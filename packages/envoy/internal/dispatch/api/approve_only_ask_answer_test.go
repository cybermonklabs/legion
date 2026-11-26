package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// approveOnlyAsks are the two asks of one issue the approve-only token's answer tests send
// answers to: a plain question, which the token may not answer, and an approval request of the
// issue's document, which it may.
type approveOnlyAsks struct {
	question, approval string
}

// openApproveOnlyAsks opens an issue under version's project, a question on it and an approval
// request of its document, each as the person's session.
func openApproveOnlyAsks(t *testing.T, rig *approveOnlyRig, version approveOnlyVersion) approveOnlyAsks {
	t.Helper()
	issue := createInteractionIssue(t, rig.handler, version.project, "Asks under the approve-only token", "A document to approve.\n")
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
	requested := sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
		"summary": "Approve this document?", "actor": sessionActor(),
	})
	if requested.Code != http.StatusCreated {
		t.Fatalf("open the approval request = %d %s; want 201", requested.Code, requested.Body.String())
	}
	approval := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID
	if question.ID == "" || approval == "" {
		t.Fatalf("the issue's asks are question %q and approval request %q: %s %s", question.ID, approval, opened.Body.String(), requested.Body.String())
	}
	return approveOnlyAsks{question: question.ID, approval: approval}
}

// answering is the request that answers ask under token with selected and text, as version takes
// it: POST /api/v1/asks/{id}/answer on /api/v1, and the state answered of PUT
// /api/v2/asks/{id}/state on /api/v2, whose one route takes every state.
func (version approveOnlyVersion) answering(t *testing.T, rig *approveOnlyRig, token, ask string, selected []string, text string) *http.Request {
	t.Helper()
	body := map[string]any{"selected": selected, "expected_edited_at": nil}
	if text != "" {
		body["text"] = text
	}
	if version.prefix == approveV1.prefix {
		return rig.caller.presenting(t, http.MethodPost, version.prefix+"/asks/"+ask+"/answer", token, body)
	}
	body["state"] = "answered"
	return rig.caller.presenting(t, http.MethodPut, version.prefix+"/asks/"+ask+"/state", token, body)
}

// readAskAsPerson is the ask as the person's own session reads it.
func readAskAsPerson(t *testing.T, rig *approveOnlyRig, ask string) *httptest.ResponseRecorder {
	t.Helper()
	return sessionRequest(t, rig.handler, http.MethodGet, "/api/v1/asks/"+ask, nil)
}

// TestAnApproveOnlyTokenAnswersAnApprovalAskAndNoOtherAsk: the approve use reaches "the answer of
// an approval ask" (spec R17's "A person's approvals through the airlock", item 3), and Dispatch
// refuses an approve-only token on any route outside that list (item 4). POST
// /api/v1/asks/{id}/answer, and PUT /api/v2/asks/{id}/state with state answered, serve every kind
// of ask, so the approve list naming the route lets the approve-only token answer a plain question
// as its person, free text and all, which is no approval. Such an answer is refused 403
// APPROVE_ONLY_TOKEN and nothing is recorded, on both versions; the same token's answer of an
// approval ask of the same issue is recorded, which is the control that the token and the route
// work.
func TestAnApproveOnlyTokenAnswersAnApprovalAskAndNoOtherAsk(t *testing.T) {
	rig := newApproveOnlyRig(t, testServerOptions{})
	for _, version := range approveOnlyVersions {
		t.Run(version.prefix, func(t *testing.T) {
			asks := openApproveOnlyAsks(t, rig, version)

			const written = "free text written by the approve-only token"
			answered := rig.serve(version.answering(t, rig, rig.approveOnly, asks.question, []string{"Yes"}, written))
			if !refusedAsApproveOnly(answered) {
				t.Errorf("the approve-only token's answer of a question ask = %d %s; want 403 %s", answered.Code, answered.Body.String(), contracts.DispatchApproveOnlyToken)
			}
			read := readAskAsPerson(t, rig, asks.question)
			if read.Code != http.StatusOK || strings.Contains(read.Body.String(), written) {
				t.Errorf("the question after the approve-only token's answer = %d %s; want 200 with no answer of that token", read.Code, read.Body.String())
			}

			approved := rig.serve(version.answering(t, rig, rig.approveOnly, asks.approval, []string{"Approve"}, ""))
			if approved.Code != http.StatusOK {
				t.Fatalf("the approve-only token's answer of the approval ask = %d %s; want 200", approved.Code, approved.Body.String())
			}
		})
	}
}

// TestAnApproveOnlyTokenIsRefusedARepeatOfTheAnswerThePersonGaveOnV2: PUT /api/v2/asks/{id}/state
// answers a body equal to the ask's present answer, from the caller who gave it, 200 and writes
// nothing: a genuine no-op. The person-scoped token of the person who answered a question gets
// that answer, which is the control that the no-op is live; the approve-only token sends the same
// body and is refused 403 APPROVE_ONLY_TOKEN instead of being told the answer stands, since the
// token may not answer a question at all. /api/v1 has no such no-op: its second answer of an
// answered ask is the engine's own conflict, which is no proof of the token's refusal and is not
// asserted here.
func TestAnApproveOnlyTokenIsRefusedARepeatOfTheAnswerThePersonGaveOnV2(t *testing.T) {
	rig := newApproveOnlyRig(t, testServerOptions{})
	asks := openApproveOnlyAsks(t, rig, approveV2)
	answer := func(token string) *httptest.ResponseRecorder {
		return rig.serve(approveV2.answering(t, rig, token, asks.question, []string{"No"}, ""))
	}
	if own := answer(rig.person); own.Code != http.StatusOK {
		t.Fatalf("the person-scoped token's answer of the question = %d %s; want 200", own.Code, own.Body.String())
	}
	if again := answer(rig.person); again.Code != http.StatusOK {
		t.Fatalf("the person-scoped token's repeat of its answer = %d %s; want 200, the answer as it stands", again.Code, again.Body.String())
	}
	if repeat := answer(rig.approveOnly); !refusedAsApproveOnly(repeat) {
		t.Errorf("the approve-only token's repeat of the person's answer of a question ask = %d %s; want 403 %s", repeat.Code, repeat.Body.String(), contracts.DispatchApproveOnlyToken)
	}
}

// TestAnApproveOnlyTokenSendsNoStateOfAnAskButAnswered: PUT /api/v2/asks/{id}/state takes the
// states resolved, retracted and declined as well as answered, and the approve list gives the
// token the state answered alone. Each of the other three, sent to the approval request and to the
// question, is refused 403 APPROVE_ONLY_TOKEN, and neither ask moves.
func TestAnApproveOnlyTokenSendsNoStateOfAnAskButAnswered(t *testing.T) {
	rig := newApproveOnlyRig(t, testServerOptions{})
	asks := openApproveOnlyAsks(t, rig, approveV2)
	for _, ask := range []string{asks.approval, asks.question} {
		for _, body := range []map[string]any{
			{"state": "resolved", "reason": "closed by the approve-only token", "actor": sessionActor()},
			{"state": "retracted", "reason": "closed by the approve-only token", "actor": sessionActor()},
			{"state": "declined", "reason": "declined by the approve-only token"},
		} {
			refused := rig.send(t, rig.approveOnly, http.MethodPut, approveV2.prefix+"/asks/"+ask+"/state", body)
			if !refusedAsApproveOnly(refused) {
				t.Errorf("the approve-only token's state %v of ask %s = %d %s; want 403 %s", body["state"], ask, refused.Code, refused.Body.String(), contracts.DispatchApproveOnlyToken)
			}
		}
	}
	for _, ask := range []string{asks.approval, asks.question} {
		read := readAskAsPerson(t, rig, ask)
		if strings.Contains(read.Body.String(), `"state":"resolved"`) || strings.Contains(read.Body.String(), "by the approve-only token") {
			t.Errorf("ask %s after the approve-only token's refused states = %s; want it unmoved", ask, read.Body.String())
		}
	}
}

// TestAnApproveOnlyTokenIsRefusedAKeyedAnswerOfAnAskThatIsNoApprovalRequest: the keyed layer keeps
// one answer per login, and the approve-only token acts as its person, so a saved answer is the
// person's own credential's to replay to the token unless the answer's route judges the ask first.
// The person-scoped token answers a question under a key, and the same request under the same key
// is replayed to it, 200 with Idempotency-Replayed: that is the saved answer. The approve-only
// token sending that very request, key and body, is refused 403 APPROVE_ONLY_TOKEN and is not
// replayed the saved answer. Its keyed answer of the question under a key nobody used is refused
// too, and claims no key. Its keyed answer of the approval ask is the control that the token and
// its keys work: answered 200 under its key, replayed to its repeat. Each holds on /api/v1 and on
// /api/v2.
func TestAnApproveOnlyTokenIsRefusedAKeyedAnswerOfAnAskThatIsNoApprovalRequest(t *testing.T) {
	rig := newApproveOnlyRig(t, testServerOptions{})
	for _, version := range approveOnlyVersions {
		t.Run(version.prefix, func(t *testing.T) {
			asks := openApproveOnlyAsks(t, rig, version)
			keyed := func(token, ask, key string, selected []string) *httptest.ResponseRecorder {
				request := version.answering(t, rig, token, ask, selected, "")
				request.Header.Set(contracts.IdempotencyKeyHeader, key)
				return rig.serve(request)
			}
			claims := func(key string) int {
				var held int
				if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from request_keys where idempotency_key = $1`, key).Scan(&held); err != nil {
					t.Fatal(err)
				}
				return held
			}
			saved, fresh, control := "saved-answer-"+version.project, "unused-key-"+version.project, "approval-answer-"+version.project

			if own := keyed(rig.person, asks.question, saved, []string{"No"}); own.Code != http.StatusOK || replayed(own) {
				t.Fatalf("the person-scoped token's keyed answer of the question = %d %s (replayed %v); want 200, run", own.Code, own.Body.String(), replayed(own))
			}
			if again := keyed(rig.person, asks.question, saved, []string{"No"}); again.Code != http.StatusOK || !replayed(again) {
				t.Fatalf("the person-scoped token's repeat under its key = %d %s (replayed %v); want 200, replayed", again.Code, again.Body.String(), replayed(again))
			}

			stolen := keyed(rig.approveOnly, asks.question, saved, []string{"No"})
			if !refusedAsApproveOnly(stolen) || replayed(stolen) {
				t.Errorf("the approve-only token's request under the person's saved key = %d %s (replayed %v); want 403 %s, not replayed",
					stolen.Code, stolen.Body.String(), replayed(stolen), contracts.DispatchApproveOnlyToken)
			}
			claimed := keyed(rig.approveOnly, asks.question, fresh, []string{"Yes"})
			if !refusedAsApproveOnly(claimed) || replayed(claimed) {
				t.Errorf("the approve-only token's keyed answer of the question = %d %s (replayed %v); want 403 %s, not replayed",
					claimed.Code, claimed.Body.String(), replayed(claimed), contracts.DispatchApproveOnlyToken)
			}
			if held := claims(fresh); held != 0 {
				t.Errorf("the refused keyed answer claimed %d keys; want none", held)
			}

			if first := keyed(rig.approveOnly, asks.approval, control, []string{"Approve"}); first.Code != http.StatusOK || replayed(first) {
				t.Fatalf("the approve-only token's keyed answer of the approval ask = %d %s (replayed %v); want 200, run", first.Code, first.Body.String(), replayed(first))
			}
			if again := keyed(rig.approveOnly, asks.approval, control, []string{"Approve"}); again.Code != http.StatusOK || !replayed(again) {
				t.Fatalf("the approve-only token's repeat under its key = %d %s (replayed %v); want 200, replayed", again.Code, again.Body.String(), replayed(again))
			}
			if held := claims(control); held != 1 {
				t.Errorf("the approval ask's keyed answer holds %d keys; want one", held)
			}
		})
	}
}
