package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
)

// outsideBrokerOrigin is the outside origin (DISPATCH_OUTSIDE_ORIGIN) a broker-token test's
// Dispatch answers at beside brokerOrigin, its canonical one.
const outsideBrokerOrigin = "https://outside.example"

// outsideAgentToken is the broker token an agent whose Dispatch URL is the outside origin
// presents, so it signs each proof for that origin. The fixture is a service's token: the origin a
// proof is checked against is chosen for every live token before the caller's kind is read, so the
// token's kind does not matter to these tests.
const outsideAgentToken = "token-of-an-agent-outside"

// presentedAt answers GET /api/v1/whoami from handler sent to origin, carrying signed's
// Authorization and DPoP headers: the proof signed names whatever origin it was signed for.
func presentedAt(handler http.Handler, signed *http.Request, origin string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, origin+"/api/v1/whoami", nil)
	request.Header.Set("Authorization", signed.Header.Get("Authorization"))
	request.Header.Set("DPoP", signed.Header.Get("DPoP"))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// requireProofInvalid requires recorder to be the 401 BROKER_TOKEN_PROOF_INVALID a proof that
// does not meet RFC 9449 Section 4.3 is answered: one naming another target URI, or one spent.
func requireProofInvalid(t *testing.T, what string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusUnauthorized || errorCode(recorder) != "BROKER_TOKEN_PROOF_INVALID" {
		t.Fatalf("%s answered %d %s, want 401 BROKER_TOKEN_PROOF_INVALID", what, recorder.Code, recorder.Body.String())
	}
}

// An agent outside signs its proof for DISPATCH_OUTSIDE_ORIGIN and sends the request there, and
// Dispatch answers it: the router answers a request whose Host is that origin's host as that
// origin, and brokerTokenVerifier checks the proof against it. The same proof sent to the
// canonical origin is refused 401 BROKER_TOKEN_PROOF_INVALID, which spends nothing; once the
// outside origin has accepted it, it is refused at either origin. An agent signing for the
// canonical origin is still answered there.
func TestOutsideOriginAnswersABrokerTokensProofNamingIt(t *testing.T) {
	s := newOutsideSignIn(t, nil)
	s.boot.OutsideOrigin = outsideBrokerOrigin
	broker := newFakeBroker(t)
	handler, _ := s.brokerRouter(t, broker)
	agent := brokertokentest.NewCaller(t)
	broker.holds(&fakeCredential{id: "login-outside", person: privOwner, service: "outside-agents", tokens: []string{outsideAgentToken}}, agent)
	signed := agent.Signed(t, http.MethodGet, outsideBrokerOrigin+"/api/v1/whoami", outsideAgentToken)

	requireProofInvalid(t, "the proof naming the outside origin, sent to the canonical one", presentedAt(handler, signed, brokerOrigin))
	requireTokenActs(t, "the proof naming the outside origin, sent there", presentedAt(handler, signed, outsideBrokerOrigin))
	requireProofInvalid(t, "the same proof sent to the outside origin again", presentedAt(handler, signed, outsideBrokerOrigin))
	requireProofInvalid(t, "the spent proof sent to the canonical origin", presentedAt(handler, signed, brokerOrigin))
	requireTokenActs(t, "a proof naming the canonical origin, sent there",
		brokerTokenRequest(t, handler, agent, http.MethodGet, "/api/v1/whoami", "", outsideAgentToken))
}

// The control: a Dispatch with no DISPATCH_OUTSIDE_ORIGIN checks every proof against its canonical
// origin, so the proof naming the outside origin is refused 401 BROKER_TOKEN_PROOF_INVALID at that
// origin's host too, and the outside origin is what admits it.
func TestNoOutsideOriginRefusesABrokerTokensProofNamingIt(t *testing.T) {
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, _ := s.brokerRouter(t, broker)
	agent := brokertokentest.NewCaller(t)
	broker.holds(&fakeCredential{id: "login-outside", person: privOwner, service: "outside-agents", tokens: []string{outsideAgentToken}}, agent)
	signed := agent.Signed(t, http.MethodGet, outsideBrokerOrigin+"/api/v1/whoami", outsideAgentToken)

	requireProofInvalid(t, "the proof naming an origin Dispatch does not answer at, sent to its host", presentedAt(handler, signed, outsideBrokerOrigin))
	requireTokenActs(t, "a proof naming the canonical origin, sent there",
		brokerTokenRequest(t, handler, agent, http.MethodGet, "/api/v1/whoami", "", outsideAgentToken))
}
