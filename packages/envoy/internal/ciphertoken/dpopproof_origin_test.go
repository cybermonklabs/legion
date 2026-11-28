// dpopproof_origin_test.go drives the verifier with a result write that reaches Dispatch at an
// address other than its public origin, as the broker's airlock proxy sends a CI run's write to
// Dispatch's in-cluster address: the proof is good when it names Dispatch's own origin, and only
// then. The rig is brokertoken_test.go's, and the token resultstoken_test.go's.
package ciphertoken_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dpopproof"
)

const (
	// inClusterResults is where the airlock sends a result write: an address of Dispatch inside
	// its cluster, which is not the origin Dispatch serves under.
	inClusterResults = "http://10.0.0.5:8766/api/v1/projects/P/results"
	// publicResults is the same route under Dispatch's own origin (DISPATCH_SERVER_URL).
	publicResults = dispatchOrigin + "/api/v1/projects/P/results"
)

// recordingTransport keeps the request a client's transport would send, and sends nothing.
type recordingTransport struct{ last *http.Request }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.last = req
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: req}, nil
}

// TestAProofForDispatchsOriginIsAcceptedWhereverTheRequestArrives: a results token's write that
// arrives at the in-cluster address verifies when its proof names Dispatch's own origin plus the
// path, and the token's hash. A proof naming the in-cluster URL is refused, and so is the proof
// cipherclient's transport signs for a request it sends to that URL, since it signs the URL it
// sends to; so is a proof whose ath names another token.
func TestAProofForDispatchsOriginIsAcceptedWhereverTheRequestArrives(t *testing.T) {
	key, err := proof.NewKey()
	if err != nil {
		t.Fatalf("mint the key: %v", err)
	}
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint the key: %v", err)
	}
	const token = "a-results-token-the-airlock-holds"
	signed := func(htu, ath string) string {
		t.Helper()
		compact, err := dpopproof.Sign(key, http.MethodPost, htu, "", ath)
		if err != nil {
			t.Fatalf("dpopproof.Sign: %v", err)
		}
		return compact
	}
	recorder := &recordingTransport{}
	clientWrite, err := http.NewRequest(http.MethodPost, inClusterResults, nil)
	if err != nil {
		t.Fatal(err)
	}
	clientWrite.Header.Set("Authorization", "DPoP "+token)
	response, err := (&http.Client{Transport: cipherclient.NewDPoPTransport(key, recorder)}).Do(clientWrite)
	if err != nil {
		t.Fatalf("sign through cipherclient's transport: %v", err)
	}
	response.Body.Close()
	clientsProof := recorder.last.Header.Get("DPoP")

	rows := []struct {
		name string
		dpop string
		want error
	}{
		{"a proof naming Dispatch's origin and the path", signed(publicResults, dpopproof.TokenHash(token)), nil},
		{"a proof naming the in-cluster URL", signed(inClusterResults, dpopproof.TokenHash(token)), ciphertoken.ErrProofInvalid},
		{"cipherclient's transport sending to the in-cluster URL", clientsProof, ciphertoken.ErrProofInvalid},
		{"a proof whose ath names another token", signed(publicResults, dpopproof.TokenHash("another-token")), ciphertoken.ErrProofInvalid},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			rig := newRig(t, func(options *ciphertoken.Options) { options.ServesResults = true })
			rig.broker.Says(token, resultsToken(jkt, time.Hour))
			write := httptest.NewRequest(http.MethodPost, inClusterResults, nil)
			write.Header.Set("Authorization", "DPoP "+token)
			write.Header.Set("DPoP", row.dpop)
			write.RemoteAddr = callerAddress
			caller, err := rig.verifier.Verify(t.Context(), write)
			if row.want == nil {
				if err != nil || !caller.ResultsOnly || caller.Session != resultsSession {
					t.Fatalf("Verify = %+v, %v; want the run's results-only caller of the token's session", caller, err)
				}
				return
			}
			if !errors.Is(err, row.want) {
				t.Fatalf("Verify = %+v, %v; want %v", caller, err, row.want)
			}
		})
	}
}
