// approve_only_state_routes_test.go holds what the approve-only token does on the approve list's
// routes that take several operations by state, and on the two routes beside the review and the
// answer that settle what a person settles: it keeps a decision's block and decides a credential
// request as its person on /api/v1 and on /api/v2, and it is held to the states the list sends a
// /api/v2 route, so the dismissal of a credential request stays closed to it as it is on /api/v1.
package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAnApproveOnlyTokenKeepsADecisionOnBothVersions: the token's keep of a decision's block is
// recorded as its person's on POST /api/v1/artifacts/{id}/blocks/{block_id}/keep and on PUT
// /api/v2/issues/{key}/decisions/{block_id}/state, and the decision stops waiting for review. The
// /api/v2 route takes a keep alone (its one state), which the list names, so the token needs no
// other state of it.
func TestAnApproveOnlyTokenKeepsADecisionOnBothVersions(t *testing.T) {
	rig := newApproveOnlyRig(t, testServerOptions{})
	createReferenceAPIProject(t, rig.handler, "KEP")
	issue := createSpecIssue(t, rig.handler, "KEP", "A spec to decide about.\n")
	decisions := uploadIssueDocument(t, rig.handler, issue.Key, "decisions.md",
		"## Keep the v1 decision\n\nChosen for the first reason.\n\n## Keep the v2 decision\n\nChosen for the second reason.\n")
	for _, version := range []struct {
		approveOnlyVersion
		heading string
	}{
		{approveV1, "Keep the v1 decision"},
		{approveV2, "Keep the v2 decision"},
	} {
		t.Run(version.prefix, func(t *testing.T) {
			decision := decisionNamed(t, readIssueDecisions(t, rig.handler, issue.Key), version.heading)
			if decision.Kept != nil || !decision.WaitingReview {
				t.Fatalf("the decision before the keep = %#v; want it waiting for review and kept by nobody", decision)
			}
			var kept *http.Request
			if version.prefix == approveV1.prefix {
				kept = rig.caller.presenting(t, http.MethodPost, version.prefix+"/artifacts/"+decisions.ID+"/blocks/"+decision.BlockID+"/keep", rig.approveOnly,
					map[string]any{"text_sha256": decision.TextSHA256})
			} else {
				kept = rig.caller.presenting(t, http.MethodPut, version.prefix+"/issues/"+issue.Key+"/decisions/"+decision.BlockID+"/state", rig.approveOnly,
					map[string]any{"state": "kept", "text_sha256": decision.TextSHA256})
			}
			response := rig.serve(kept)
			if response.Code != http.StatusOK {
				t.Fatalf("the approve-only token's keep = %d %s; want 200", response.Code, response.Body.String())
			}
			if keep := decodeBody[model.DecisionKeep](t, response); keep.BlockID != decision.BlockID || keep.KeptBy != brokerTestPerson {
				t.Fatalf("the keep = %#v; want %s kept by %s", keep, decision.BlockID, brokerTestPerson)
			}
			after := decisionNamed(t, readIssueDecisions(t, rig.handler, issue.Key), version.heading)
			if after.Kept == nil || after.WaitingReview {
				t.Fatalf("the decision after the keep = %#v; want it kept and waiting for no review", after)
			}
		})
	}
}

// TestAnApproveOnlyTokenDecidesACredentialRequestAndDismissesNone: the token's approve and deny of
// a credential request reach the secrets broker as its person on POST
// /api/v1/credential-requests/{id}/approve and /deny and on PUT /api/v2/credential-requests/{id}/state
// with state approved or denied, and its dismissal, which no list route does, is refused 403
// APPROVE_ONLY_TOKEN on both versions before the broker is asked anything: on /api/v2 the one route
// takes the dismissal as a state, which the state check holds the token out of.
func TestAnApproveOnlyTokenDecidesACredentialRequestAndDismissesNone(t *testing.T) {
	broker := newFakeBrokerRig(t)
	rig := newApproveOnlyRig(t, testServerOptions{agentSecretsURL: broker.server.URL})
	const record = "rec1"
	for _, version := range approveOnlyVersions {
		t.Run(version.prefix, func(t *testing.T) {
			send := func(state string) *http.Request {
				if version.prefix == approveV1.prefix {
					action := map[string]string{"approved": "approve", "denied": "deny", "dismissed": "dismiss"}[state]
					return rig.caller.presenting(t, http.MethodPost, version.prefix+"/credential-requests/"+record+"/"+action, rig.approveOnly, map[string]any{"code": "123456"})
				}
				return rig.caller.presenting(t, http.MethodPut, version.prefix+"/credential-requests/"+record+"/state", rig.approveOnly, map[string]any{"state": state, "code": "123456"})
			}
			for _, state := range []string{"approved", "denied"} {
				before := broker.calls
				response := rig.serve(send(state))
				if refusedAsApproveOnly(response) {
					t.Errorf("the approve-only token's %s of a credential request = %d %s; want it past the approve-only checks", state, response.Code, response.Body.String())
				}
				if broker.calls == before {
					t.Errorf("the approve-only token's %s of a credential request asked the broker nothing: %d %s", state, response.Code, response.Body.String())
				}
				// /api/v1 relays the decision alone; /api/v2 reads the record the decision left (a lookup
				// by the code) after it, so its last request to the broker is that read.
				if version.prefix == approveV1.prefix && !strings.Contains(broker.lastBody, `"approver":"`+brokerTestPerson+`"`) {
					t.Errorf("the broker's last request after the %s = %s; want the token's person %s as the approver", state, broker.lastBody, brokerTestPerson)
				}
			}
			before := broker.calls
			dismissed := rig.serve(send("dismissed"))
			if !refusedAsApproveOnly(dismissed) {
				t.Errorf("the approve-only token's dismissal of a credential request = %d %s; want 403 %s", dismissed.Code, dismissed.Body.String(), contracts.DispatchApproveOnlyToken)
			}
			if broker.calls != before {
				t.Errorf("the refused dismissal asked the broker %d times; want none", broker.calls-before)
			}
		})
	}
}
