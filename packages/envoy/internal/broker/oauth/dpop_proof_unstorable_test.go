package oauth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// A token request whose DPoP proof's jti holds U+0000 is the caller's fault: the replay store
// refuses the proof before its write (oauthstore.ErrUnstorableText), and the provider answers it
// 400 invalid_request REQUEST_INVALID and records no proof. Before, the store's insert failed,
// since PostgreSQL stores no U+0000 in text, and the provider answered 500 server_error, which a
// client retries. Each poll asks for the tokens of an approved device login of its own, since the
// replay store reads a proof only when the endpoint issues tokens; the control, a proof with an
// ordinary jti of the same signer, is answered 200 with them.
//
// Control in provider.go: writeError without its ErrUnstorableText arm (500 server_error).
func TestATokenRequestWhoseProofJTIHoldsNULIsTheCallersRefusal(t *testing.T) {
	f := newFixture(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	approved := func() string {
		t.Helper()
		deviceCode, userCode := f.authorize(t, key)
		if err := f.provider.ApproveDevice(context.Background(), userCode, true, map[string]any{"credential_id": f.newCredential(t)}); err != nil {
			t.Fatalf("ApproveDevice: %v", err)
		}
		return deviceCode
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{EmbedJWK: true}).WithType("dpop+jwt"))
	if err != nil {
		t.Fatal(err)
	}
	poll := func(deviceCode, jti string) (int, map[string]any) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"jti": jti, "htm": http.MethodPost, "htu": f.url + "/token", "iat": time.Now().Unix()})
		if err != nil {
			t.Fatal(err)
		}
		signed, err := signer.Sign(payload)
		if err != nil {
			t.Fatal(err)
		}
		proof, err := signed.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		form := url.Values{"grant_type": {deviceGrant}, "device_code": {deviceCode}, "client_id": {oauthwire.ClientID}}
		req, err := http.NewRequest(http.MethodPost, f.url+"/token", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", proof)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode the token answer: %v", err)
		}
		return resp.StatusCode, body
	}

	if status, body := poll(approved(), "an-ordinary-jti"); status != http.StatusOK || body["access_token"] == nil {
		t.Fatalf("control: an approved login's poll with an ordinary jti answered %d %v, want 200 with its tokens", status, body)
	}
	refused := approved()
	before := f.count(t, "oauth_dpop_proofs")
	if status, body := poll(refused, "a-jti-with-a-\x00-in-it"); status != http.StatusBadRequest || body["code"] != "REQUEST_INVALID" {
		t.Fatalf("an approved login's poll whose proof's jti holds U+0000 answered %d %v, want 400 REQUEST_INVALID, the caller's refusal, never 500", status, body)
	}
	if after := f.count(t, "oauth_dpop_proofs"); after != before {
		t.Fatalf("the refused proof moved the replay table from %d to %d rows; want none written", before, after)
	}
}
