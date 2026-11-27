// minted_test.go drives the token a broker pod mints for a grant of a capability the broker mints
// and holds (requests.MintedKind): what introspection answers of it, and that it is no subject
// token for another exchange. The fixture is exchange_test.go's.
package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// mintedGrant is a minted grant acting for person under the session ses_minted_test, holding
// scope, that ends at expires.
func mintedGrant(scope string, expires time.Time) oauthstore.MintedGrant {
	return oauthstore.MintedGrant{
		ID: uuid.NewString(), Subject: person, Session: "ses_minted_test", Principal: person, Scopes: []string{scope},
		Extra: map[string]any{
			oauth.ClaimSession: "ses_minted_test", oauth.ClaimPerson: person, oauth.ClaimProjectScope: oauthwire.ProjectScopePerson,
		},
		ExpiresAt: expires.UTC().Truncate(time.Second),
	}
}

// TestAMintedGrantsTokenIsBoundToThePodAndEndsWithItsGrant: the token a broker pod mints for a
// minted grant (MintGrantToken) is live, carries the grant's decision - its session, its person,
// its project scope and its scopes - is bound to the pod's key, and ends when the grant ends. The
// access lifetime is an hour, longer than the grant's fifteen minutes, so a token that ended with
// the access lifetime and not with its grant reads apart. Presented as the subject token of
// another exchange it mints nothing, since the library refuses a client's own token, so the
// capability the airlock holds never turns into a token of another session. Asked for with no pod
// key, nothing is minted and no access token is stored.
func TestAMintedGrantsTokenIsBoundToThePodAndEndsWithItsGrant(t *testing.T) {
	f := newExchangeFixture(t, func(c *oauth.Config) { c.AccessLifetime = time.Hour })
	grant := mintedGrant("test-scope", time.Now().Add(15*time.Minute))
	pod := newKey(t)
	minted, err := f.provider.MintGrantToken(context.Background(), grant, thumbprint(t, pod))
	if err != nil {
		t.Fatalf("MintGrantToken: %v", err)
	}
	answer := f.introspected(t, minted)
	if !answer.Active || answer.Session != grant.Session || answer.Person != person || answer.ProjectScope != oauthwire.ProjectScopePerson ||
		answer.Scope != "test-scope" || answer.Thumbprint != thumbprint(t, pod) || answer.ExpiresAt != grant.ExpiresAt.Unix() {
		t.Fatalf("the minted token introspects as %+v; want it live, session %s, person %s, project scope person, scope test-scope, bound to the pod's key, ending at %v",
			answer, grant.Session, person, grant.ExpiresAt)
	}
	before := f.count(t, "oauth_access_tokens")
	status, body := f.exchange(t, exchangeForm(minted, oauthwire.SubjectTokenTypeAccessToken, "a-session-of-its-own"), pod)
	if status != http.StatusBadRequest || body["access_token"] != nil {
		t.Fatalf("an exchange of a minted token = %d %v, want 400 and no access token", status, body)
	}
	const ownToken = "Clients are not allowed to perform a token exchange on their own tokens."
	if hint, _ := body["error_description"].(string); body["error"] != "invalid_grant" || !strings.Contains(hint, ownToken) {
		t.Errorf("the refusal reads %v %q, which is not the library's refusal of a client's own token %q", body["error"], hint, ownToken)
	}
	if _, err := f.provider.MintGrantToken(context.Background(), grant, ""); err == nil {
		t.Fatal("MintGrantToken with no pod key minted a token; want a refusal")
	}
	if after := f.count(t, "oauth_access_tokens"); after != before {
		t.Fatalf("access tokens after the refused exchange and the keyless mint = %d, want the %d there were", after, before)
	}
}

// TestEachMintedTokensHashAndBindingAreLoggedAndNeverTheToken: every token a broker pod mints for
// a minted grant writes one line naming the grant, its session, the token's SHA-256 and the RFC
// 7638 thumbprint of the pod key it is bound to, which is what an audit compares a string found
// anywhere with to say whether it is such a token. No line holds the token itself. Two mints for
// one grant write two lines, each the hash of its own token.
func TestEachMintedTokensHashAndBindingAreLoggedAndNeverTheToken(t *testing.T) {
	f := newExchangeFixture(t, func(c *oauth.Config) { c.AccessLifetime = time.Hour })
	logged := capturedLog(t)
	grant := mintedGrant(oauthwire.ScopeApprove, time.Now().Add(15*time.Minute))
	pod := newKey(t)
	for i := range 2 {
		minted, err := f.provider.MintGrantToken(context.Background(), grant, thumbprint(t, pod))
		if err != nil {
			t.Fatalf("MintGrantToken: %v", err)
		}
		sum := sha256.Sum256([]byte(minted))
		lines := logged.lines(`"msg":"broker: minted a grant's token"`, `"token_sha256":"`+hex.EncodeToString(sum[:])+`"`,
			`"jkt":"`+thumbprint(t, pod)+`"`, `"grant":"`+grant.ID+`"`, `"session":"`+grant.Session+`"`)
		if len(lines) != 1 {
			t.Fatalf("mint %d: %d lines name the grant, its session, the token's SHA-256 and the pod key's thumbprint; want 1", i+1, len(lines))
		}
		if leaked := logged.lines(minted); len(leaked) != 0 {
			t.Fatalf("mint %d: %d logged lines hold the minted token itself; want none", i+1, len(leaked))
		}
	}
}
