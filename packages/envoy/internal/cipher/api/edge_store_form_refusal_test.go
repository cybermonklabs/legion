package api_test

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// TestADeviceAuthorizationFormTheStoreCannotHoldIsNeverAServerError: a device authorization whose
// form, beside a request object that verifies, holds U+0000 (in a parameter no grant reads, in a
// second value of a parameter the provider reads, or in a parameter's name) is never 500
// server_error, at the edge and on the broker's own address alike, and a request the broker
// refuses writes no row. The provider stores the whole form as JSON (oauth_device_codes.request,
// jsonb), which holds no U+0000, and it writes that row before the request object is verified: at
// the edge a caller anywhere on the internet sends the form, and a 500 there is an alert any caller
// can raise, and an answer the client cannot read as a refusal of its own request.
//
// The named revert: store the device authorization's form as the client sent it, and each row
// answers 500 server_error.
func TestADeviceAuthorizationFormTheStoreCannotHoldIsNeverAServerError(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	for i, row := range []struct{ name, key, value string }{
		{"a parameter no grant reads", "note", "a\x00b"},
		{"a second scope", "scope", oauthwire.Scope + "\x00"},
		{"a second dpop_jkt", "dpop_jkt", "a\x00b"},
		{"a second request", "request", "a\x00b"},
		{"a second client_id", "client_id", "a\x00b"},
		{"a parameter's name", "a\x00b", "c"},
	} {
		for _, base := range []string{rig.EdgeURL, rig.URL} {
			form := edgeDeviceForm(t, base, fmt.Sprintf("form-%d@outside.example", i))
			form.Add(row.key, row.value)
			var headers map[string]string
			if base == rig.EdgeURL {
				headers = map[string]string{"X-Forwarded-For": forwardedFor("", fmt.Sprintf("203.0.113.%d", 100+i))}
			}
			before := loginRows(t, rig)
			status, body := postEdge(t, base, "/v1/oauth/device_authorization", form, headers)
			if status >= http.StatusInternalServerError {
				t.Errorf("a device authorization at %s whose form holds U+0000 in %s = %d %v, want no server error", base, row.name, status, body)
			}
			if after := loginRows(t, rig); status != http.StatusOK && after != before {
				t.Errorf("the refused device authorization at %s (U+0000 in %s) wrote %d rows, want none", base, row.name, after-before)
			}
		}
	}
}

// TestADPoPProofJTITheStoreCannotHoldIsNeverAServerError: a DPoP proof made by the login's own key
// for the right method and URL, whose jti holds U+0000, is never 500 server_error on a refresh or
// on the trade of the login's access token, at the edge and on the broker's own address alike, and
// the request it carried moves nothing: the login refreshes with a proper proof afterwards. The
// provider records each proof's jti (oauth_dpop_proofs.jti, text), which holds no U+0000.
//
// The named revert: record the proof's jti as the client wrote it, and each row answers 500
// server_error.
func TestADPoPProofJTITheStoreCannotHoldIsNeverAServerError(t *testing.T) {
	rig := ciphertest.NewRig(t, ciphertest.WithEdge())
	via := throughBalancer{client: "203.0.113.120"}
	m := newEdgeMachine(t, rig, via)
	da, err := m.edgeLogin(t, outsidePerson)
	if err != nil {
		t.Fatalf("edge login: %v", err)
	}
	rig.DecideMachineLoginAs(t, outsidePerson, da.UserCode, true)
	tok := m.poll(t, da)
	for _, base := range []string{rig.EdgeURL, rig.URL} {
		headers := map[string]string{"DPoP": proofWithJTI(t, m.key, base+"/v1/oauth/token", "a\x00b")}
		if base == rig.EdgeURL {
			headers["X-Forwarded-For"] = forwardedFor("", via.client)
		}
		refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}, "client_id": {oauthwire.ClientID}}
		status, body := postEdge(t, base, "/v1/oauth/token", refresh, headers)
		if status >= http.StatusInternalServerError {
			t.Errorf("a refresh at %s with a proof whose jti holds U+0000 = %d %v, want no server error", base, status, body)
		}
		// A fix may serve such a proof rather than refuse it; the login then goes on with what it was issued.
		if rotated, _ := body["refresh_token"].(string); status == http.StatusOK && rotated != "" {
			tok.RefreshToken, tok.AccessToken = rotated, body["access_token"].(string)
		}
		headers["DPoP"] = proofWithJTI(t, m.key, base+"/v1/oauth/token", "b\x00c")
		trade := url.Values{
			"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "client_id": {oauthwire.ExchangeClientID},
			"subject_token": {tok.AccessToken}, oauth.FormSubjectTokenType: {oauthwire.SubjectTokenTypeAccessToken},
			oauthwire.FormSession: {"nul-proof"},
		}
		if status, body := postEdge(t, base, "/v1/oauth/token", trade, headers); status >= http.StatusInternalServerError {
			t.Errorf("a trade at %s with a proof whose jti holds U+0000 = %d %v, want no server error", base, status, body)
		}
	}
	if _, err := m.refresh(tok); err != nil {
		t.Fatalf("the refresh at the edge with a proper proof afterwards: %v, want a token", err)
	}
}

// proofWithJTI is a DPoP proof of key for POST htu whose jti is jti.
func proofWithJTI(t *testing.T, key *ecdsa.PrivateKey, htu, jti string) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{EmbedJWK: true}).WithType("dpop+jwt"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"jti": jti, "htm": http.MethodPost, "htu": htu, "iat": time.Now().Unix()})
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
