package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// TestTheAPIAdmitsAProofAtAnOriginTheVerifierLists: with the verifier's origins set here as
// cmd/dispatch's brokerTokenVerifier would list them for a served address, the canonical one's
// and the served one's, the API admits a broker token whose DPoP proof names either at any Host,
// its own or another, so an agent that calls the API at a served address is the session its token
// names, as at the canonical one; a proof naming an origin the verifier does not list is refused
// 401 BROKER_TOKEN_PROOF_INVALID at any Host. Which origins main lists, and that a former address
// is none of them, is cmd/dispatch's TestDispatchReadsItsProofOrigins'.
func TestTheAPIAdmitsAProofAtAnOriginTheVerifierLists(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t, func(o *ciphertoken.Options) { o.Origins = append(o.Origins, readersServed) })
	caller := newBrokerCaller(t)
	const token = "a-token-the-broker-issued"
	rig.broker.Says(token, liveNamedToken(caller.jkt))

	for _, row := range []struct {
		name, origin string
		admitted     bool
	}{
		{"the canonical address", testServerURL, true},
		{"a served address", readersServed, true},
		{"an address the verifier does not list", readersUnrelated, false},
	} {
		for _, host := range []string{strings.TrimPrefix(row.origin, "https://"), "a.example.test"} {
			t.Run(row.name+" at Host "+host, func(t *testing.T) {
				const path = "/api/v1/whoami"
				signed := caller.caller.Signed(t, http.MethodGet, row.origin+path, token)
				request := brokerRequest(t, http.MethodGet, path, nil, signed.Header.Get("Authorization"), signed.Header.Get("DPoP"))
				request.Host = host
				response := rig.serve(request)
				if !row.admitted {
					if refusal := decodeBody[refusalAnswer](t, response); response.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_PROOF_INVALID" {
						t.Fatalf("whoami with a proof naming %s: status=%d body=%s, want 401 BROKER_TOKEN_PROOF_INVALID", row.origin, response.Code, response.Body.String())
					}
					return
				}
				if response.Code != http.StatusOK {
					t.Fatalf("whoami with a proof naming %s: status=%d body=%s, want 200", row.origin, response.Code, response.Body.String())
				}
				if answer := decodeBody[whoamiAnswer](t, response); answer.Kind != "agent" || answer.Session == nil || *answer.Session != brokerTestSession {
					t.Fatalf("whoami with a proof naming %s = %+v, want the session the token names", row.origin, answer)
				}
			})
		}
	}
}
