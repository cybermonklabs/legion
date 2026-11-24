// launcher_read_key_test.go holds the broker's answer to a launcher proof signed with a read
// token's key: a read token is never bound to a key a live launcher credential is bound to, so a
// proof that key signs authenticates as none of a person's credentials, whichever it names.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// readKeyMachine is the Google account of the box the person of these rows signs in on, listed
// for signInCredentialFleet, which is what keeps a sign-in's credential authenticating.
const readKeyMachine = "example-fleet@example-project.iam.gserviceaccount.com"

// signedDPoPProof is an RFC 9449 proof of key for a POST to htu.
func signedDPoPProof(t *testing.T, key *ecdsa.PrivateKey, htu string) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{EmbedJWK: true}).WithType("dpop+jwt"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"jti": uuid.NewString(), "htm": http.MethodPost, "htu": htu, "iat": time.Now().Unix()})
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

// keyThumbprint is key's RFC 7638 thumbprint.
func keyThumbprint(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return thumbprint
}

// launcherState is everything a launcher proof's route could change: each credential's
// revocation, and how many enrollments, grants and audit rows there are.
func (ts *testServer) launcherState(t *testing.T) string {
	t.Helper()
	var state string
	if err := ts.Store.Pool.QueryRow(context.Background(), `select coalesce((select string_agg(id::text || ' ' || coalesce(revoked_at::text, 'live'), ', ' order by id)
			from launcher_credentials), '') || ' / ' || (select count(*) from enrollments) || ' / ' || (select count(*) from grants)
		|| ' / ' || (select count(*) from audit)`).Scan(&state); err != nil {
		t.Fatalf("read the launcher state: %v", err)
	}
	return state
}

// TestALaunchersProofSignedWithAReadTokensKeyIsRefused: a person holds three live launcher
// credentials, their sign-in's on one box, their second sign-in's on another, and a machine login
// they approved, and a read token traded with a holder proof is bound to the holder's key. The
// key rule keeps that key off every launcher credential: a token login with it is answered 409
// KEY_BINDS_READ_TOKEN and leaves no credential on the key. So a launcher proof signed with
// that key and naming each credential's lid is answered 401 LAUNCHER_INVALID on POST
// /v1/enrollments, DELETE /v1/enrollments/{id}, POST /v1/operator/machines/{id}/revoke and POST
// /v1/operator/grants/{id}/revoke, and changes nothing: a launcher proof authenticates only with
// the key its credential is bound to, which the token login shows the read token's key never is.
// The same proof signed with each credential's own key is answered past 401, so what refused it
// was the key.
func TestALaunchersProofSignedWithAReadTokensKeyIsRefused(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) {
		// readKeyMachine's fleet, as BROKER_VM_SERVICES lists it: the google provider's listing,
		// which the enrollment reads through the registry.
		registry := identities(t, google.GoogleEntry{Account: readKeyMachine, Service: signInCredentialFleet})
		d.Enroll.Subjects, d.Enroll.Policy = registry, registry.Policy()
	})
	ctx := context.Background()
	tokenURL := ts.URL + "/v1/oauth/token"

	holder := newSigningKey(t)
	form := url.Values{
		"grant_type": {cipherclient.GrantTypeTokenExchange}, "client_id": {oauthwire.ExchangeClientID},
		"subject_token":            {ts.audienceToken(t, ciphertest.ExchangeAccount, "pod-uid-reader", ciphertest.ExchangeAudience, time.Hour)},
		oauth.FormSubjectTokenType: {oauthwire.SubjectTokenTypeKubernetes}, "scope": {oauthwire.ScopeRead},
	}
	traded := send(t, http.DefaultClient, request(t, http.MethodPost, tokenURL, strings.NewReader(form.Encode()), map[string]string{
		"Content-Type":             "application/x-www-form-urlencoded",
		"DPoP":                     signedDPoPProof(t, newSigningKey(t), tokenURL),
		oauthwire.HeaderDPoPHolder: signedDPoPProof(t, holder, tokenURL),
	}))
	if traded.status != http.StatusOK {
		t.Fatalf("the read trade = %d %s, want 200", traded.status, traded.body)
	}
	var bound string
	if err := ts.Store.Pool.QueryRow(ctx, `select jwk_thumbprint from oauth_narrowed_access_tokens`).Scan(&bound); err != nil {
		t.Fatalf("read the read token's row: %v", err)
	}
	if bound != keyThumbprint(t, holder) {
		t.Fatalf("the read token is bound to %s, want the holder's key %s", bound, keyThumbprint(t, holder))
	}
	if status, body := ts.tokenLogin(t, ts.accountToken(t, daemonAccount, "", time.Hour), ts.daemonRequest(t, holder)); status != http.StatusConflict ||
		decode[wireError](t, body).Code != "KEY_BINDS_READ_TOKEN" {
		t.Fatalf("a token login with the read token's key = %d %s, want 409 KEY_BINDS_READ_TOKEN", status, body)
	}
	var onHolder int
	if err := ts.Store.Pool.QueryRow(ctx, `select count(*) from launcher_credentials where key_thumbprint = $1`, keyThumbprint(t, holder)).Scan(&onHolder); err != nil {
		t.Fatalf("count the credentials on the read token's key: %v", err)
	}
	if onHolder != 0 {
		t.Fatalf("launcher_credentials holds %d rows on the read token's key, want none", onHolder)
	}

	firstBox, secondBox := newSigningKey(t), newSigningKey(t)
	approved, approvedKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	credentials := []struct {
		name string
		id   string
		key  *ecdsa.PrivateKey
	}{
		{"the sign-in's", ts.signInCredential(t, testApprover, keyThumbprint(t, firstBox), readKeyMachine), firstBox},
		{"a second sign-in's, on another box", ts.signInCredential(t, testApprover, keyThumbprint(t, secondBox), readKeyMachine), secondBox},
		{"the machine login they approved", approved, approvedKey},
	}
	routes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/v1/enrollments", map[string]any{}},
		{http.MethodDelete, "/v1/enrollments/" + uuid.NewString(), nil},
		{http.MethodPost, "/v1/operator/machines/" + uuid.NewString() + "/revoke", map[string]any{}},
		{http.MethodPost, "/v1/operator/grants/" + uuid.NewString() + "/revoke", map[string]any{}},
	}

	before := ts.launcherState(t)
	for _, credential := range credentials {
		for _, route := range routes {
			status, body := ts.launcher(t, holder, credential.id, route.method, route.path, route.body)
			if refusal := decode[wireError](t, body); status != http.StatusUnauthorized || refusal.Code != "LAUNCHER_INVALID" {
				t.Errorf("%s %s with a proof signed by the read token's key naming %s credential = %d %s, want 401 LAUNCHER_INVALID",
					route.method, route.path, credential.name, status, body)
			}
		}
	}
	if after := ts.launcherState(t); after != before {
		t.Errorf("the refused proofs changed the broker's state:\nbefore: %s\nafter:  %s", before, after)
	}

	for _, credential := range credentials {
		for _, route := range routes {
			if status, body := ts.launcher(t, credential.key, credential.id, route.method, route.path, route.body); status == http.StatusUnauthorized {
				t.Errorf("%s %s with a proof signed by %s credential's own key = %d %s, want an answer past 401",
					route.method, route.path, credential.name, status, body)
			}
		}
	}
}
