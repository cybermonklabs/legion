package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// TestTheAPIAdmitsAProofAtAServedAddressAndRefusesOneAtAFormerAddress: the API admits a broker
// token whose DPoP proof names the canonical address or a served address, with the verifier's
// origins as cmd/dispatch's brokerTokenVerifier lists them: DISPATCH_SERVER_URL's, then each
// address of DISPATCH_SERVED_ORIGINS, and no address of DISPATCH_FORMER_ORIGINS. So an agent that
// calls the API at a served address is the session its token names, as at the canonical one, and
// a proof naming a former address is refused 401 BROKER_TOKEN_PROOF_INVALID, as a proof naming an
// address that is none of the server's is: a former address is one a link is still read at, and no
// address a request is proven at. Each request arrives at the Host of the address its proof names.
func TestTheAPIAdmitsAProofAtAServedAddressAndRefusesOneAtAFormerAddress(t *testing.T) {
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
		{"a former address", readersFormer, false},
		{"an unrelated address", readersUnrelated, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			const path = "/api/v1/whoami"
			signed := caller.caller.Signed(t, http.MethodGet, row.origin+path, token)
			request := brokerRequest(t, http.MethodGet, path, nil, signed.Header.Get("Authorization"), signed.Header.Get("DPoP"))
			request.Host = strings.TrimPrefix(row.origin, "https://")
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
