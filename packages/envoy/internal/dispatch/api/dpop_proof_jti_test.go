package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
)

// TestADPoPProofWhoseJTIHoldsNULIsTheCallersRefusal (red test by the tester of
// cybermonklabs/legion#4663 at a47015a75c): the replay store (store.DPoPProofs) writes a proof's
// jti into a text column, and PostgreSQL writes U+0000 in no text, so a proof whose jti holds a NUL
// fails the insert. The library reports that as its server_error, which the broker-token check
// answers as an unreachable broker: 503 BROKER_UNAVAILABLE ("try again in a minute") and a WARN
// line saying the secrets broker could not be reached, for a request whose only fault is the
// caller's own proof. The proof is the caller's to fix, as every other bad proof is: 401
// BROKER_TOKEN_PROOF_INVALID (store.ErrUnstorableProof). The secrets broker's own replay store
// refuses the same proof (internal/cipher/oauthstore's ErrUnstorableText), which its token
// endpoint answers 400 invalid_request REQUEST_INVALID.
//
// The proofs here are signed by hand rather than by the helper's DPoP transport, which mints its
// own jti; the first row is the control that this signer's proofs are otherwise sound.
//
// At a47015a75c the control passes and the NUL row fails: 503 BROKER_UNAVAILABLE.
func TestADPoPProofWhoseJTIHoldsNULIsTheCallersRefusal(t *testing.T) {
	rig := newBrokerTokenRig(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jkt := ciphertokentest.NewCallerWith(t, key).JKT()
	const token = "a-token-whose-proof-names-a-nul"
	rig.broker.Says(token, liveNamedToken(jkt))
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{EmbedJWK: true}).WithType("dpop+jwt"))
	if err != nil {
		t.Fatal(err)
	}
	ath := sha256.Sum256([]byte(token))
	proof := func(jti string) string {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"jti": jti, "htm": http.MethodGet, "htu": testServerURL + "/api/v1/whoami",
			"iat": time.Now().Unix(), "ath": base64.RawURLEncoding.EncodeToString(ath[:]),
		})
		if err != nil {
			t.Fatal(err)
		}
		signed, err := signer.Sign(payload)
		if err != nil {
			t.Fatal(err)
		}
		compact, err := signed.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return compact
	}

	sound := rig.serve(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "DPoP "+token, proof("an-ordinary-jti")))
	if sound.Code != http.StatusOK {
		t.Fatalf("control: a hand-signed proof with an ordinary jti answered %d %s, want 200", sound.Code, sound.Body.String())
	}
	response := rig.serve(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "DPoP "+token, proof("a-jti-with-a-\x00-in-it")))
	refusal := decodeBody[refusalAnswer](t, response)
	if response.Code != http.StatusUnauthorized || refusal.Code != "BROKER_TOKEN_PROOF_INVALID" {
		t.Fatalf("a proof whose jti holds U+0000: status=%d refusal=%+v, want 401 BROKER_TOKEN_PROOF_INVALID, the caller's refusal, never the broker's 503", response.Code, refusal)
	}
}
