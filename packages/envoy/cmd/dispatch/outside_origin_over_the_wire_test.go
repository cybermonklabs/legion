package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
)

// overTheWire sends GET /api/v1/whoami to server under host, carrying signed's Authorization and
// DPoP headers, as a deployment's edge forwards a request to Dispatch: over a socket, in origin
// form, so the router sees the request's Host and an empty r.URL.Host, as it sees every request in
// a deployment. It answers the status and the refusal's code, "" for none.
func overTheWire(t *testing.T, server *httptest.Server, host string, signed *http.Request) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = host
	request.Header.Set("Authorization", signed.Header.Get("Authorization"))
	request.Header.Set("DPoP", signed.Header.Get("DPoP"))
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("send to %s under Host %s: %v", server.URL, host, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var refusal struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &refusal)
	return response.StatusCode, refusal.Code
}

// hostOf is origin's Host, what a client sending to it writes in its request's Host.
func hostOf(t *testing.T, origin string) string {
	t.Helper()
	parsed, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Host
}

// An agent outside signs its proof for DISPATCH_OUTSIDE_ORIGIN and sends the request there, and
// the request reaches Dispatch as a deployment's edge forwards it: over a socket, with the outside
// origin's host as its Host and nothing in r.URL.Host. Dispatch answers it, and refuses the same
// proof at the canonical origin's Host, then as a replay; an agent signing for the canonical origin
// is still answered there. A request built in memory (httptest.NewRequest with an absolute URL)
// carries the host in r.URL.Host as well, so only a request read off a socket shows that the
// origin is chosen from the Host a deployment forwards.
func TestOutsideOriginAnswersABrokerTokensProofOverTheWire(t *testing.T) {
	s := newOutsideSignIn(t, nil)
	s.boot.OutsideOrigin = outsideBrokerOrigin
	broker := newFakeBroker(t)
	handler, _ := s.brokerRouter(t, broker)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	agent := ciphertokentest.NewCaller(t)
	broker.holds(&fakeCredential{id: "login-outside", person: privOwner, service: "outside-agents", tokens: []string{outsideAgentToken}}, agent)
	outsideHost, canonicalHost := hostOf(t, outsideBrokerOrigin), hostOf(t, brokerOrigin)

	outside := agent.Signed(t, http.MethodGet, outsideBrokerOrigin+"/api/v1/whoami", outsideAgentToken)
	if status, code := overTheWire(t, server, canonicalHost, outside); status != http.StatusUnauthorized || code != "BROKER_TOKEN_PROOF_INVALID" {
		t.Fatalf("the proof naming the outside origin, sent to the canonical one, answered %d %s, want 401 BROKER_TOKEN_PROOF_INVALID", status, code)
	}
	if status, code := overTheWire(t, server, outsideHost, outside); status != http.StatusOK {
		t.Fatalf("the proof naming the outside origin, sent there over a socket, answered %d %s, want 200", status, code)
	}
	if status, code := overTheWire(t, server, outsideHost, outside); status != http.StatusUnauthorized || code != "BROKER_TOKEN_PROOF_INVALID" {
		t.Fatalf("the same proof sent to the outside origin again answered %d %s, want 401 BROKER_TOKEN_PROOF_INVALID", status, code)
	}
	canonical := agent.Signed(t, http.MethodGet, brokerOrigin+"/api/v1/whoami", outsideAgentToken)
	if status, code := overTheWire(t, server, canonicalHost, canonical); status != http.StatusOK {
		t.Fatalf("a proof naming the canonical origin, sent there over a socket, answered %d %s, want 200", status, code)
	}
}
