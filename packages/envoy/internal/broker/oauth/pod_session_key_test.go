package oauth_test

// The roles of one issue pod read one exchange token and are registered for one pod uid, so a
// registration names the key its own role proves: the exchange refuses a trade for a session
// proved with a sibling's key, and a registration that names none stays the pod's.

import (
	"context"
	"crypto/ecdsa"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// The pod both roles run in, and the two roles' sessions.
const (
	issuePod = "pod-uid-issue"
	roleA    = "implementer-g1"
	roleB    = "reviewer-g1"
)

// registersBound records that the pod uid of the account of may exchange under session with a
// DPoP proof of the key whose thumbprint is jkt, as the Legion daemon registers each role.
func (f *exchangeFixture) registersBound(t *testing.T, of, uid, session, jkt string) {
	t.Helper()
	if _, _, err := f.provider.RegisterPodSession(context.Background(), of, uid, session, jkt); err != nil {
		t.Fatalf("register %s's pod %s for %s bound to %s: %v", of, uid, session, jkt, err)
	}
}

// keyThumbprint is key's RFC 7638 thumbprint, what a registration names.
func keyThumbprint(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return jkt
}

// podTrade is the issue pod's trade for session, presenting a fresh projected token.
func (f *exchangeFixture) podTrade(t *testing.T, session string) url.Values {
	t.Helper()
	return exchangeForm(f.podToken(t, issuePod, nil), oauthwire.SubjectTokenTypeKubernetes, session)
}

// TestASiblingRoleCannotTradeForAnotherRolesSession: pod P is registered for A with A's key and
// for B with B's. A trade presenting P's token for B, proved with A's key, is refused
// invalid_grant naming B; the same trade proved with B's key is admitted, as is A's own with A's.
func TestASiblingRoleCannotTradeForAnotherRolesSession(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	kA, kB := newKey(t), newKey(t)
	f.registersBound(t, account, issuePod, roleA, keyThumbprint(t, kA))
	f.registersBound(t, account, issuePod, roleB, keyThumbprint(t, kB))

	status, body := f.exchange(t, f.podTrade(t, roleB), kA)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" || body["access_token"] != nil {
		t.Fatalf("role A's trade for %s's session = %d %v, want 400 invalid_grant and no token: a sibling role trades only for its own session", roleB, status, body)
	}
	want := "this pod's registration for session '" + roleB + "' is bound to another key; a role trades only for its own session"
	if description, _ := body["error_description"].(string); !strings.Contains(description, want) {
		t.Errorf("the refusal reads %q, want it to say %q", description, want)
	}

	f.introspectsAs(t, f.exchanged(t, f.podTrade(t, roleB), kB), roleB, person, project)
	f.introspectsAs(t, f.exchanged(t, f.podTrade(t, roleA), kA), roleA, person, project)
}

// TestARegistrationWithNoThumbprintIsPodBound: a registration that names no key, as a session
// watch registers a session pod's one session, admits that pod's trade for that session proved
// with any key, as every registration did before keys were named.
func TestARegistrationWithNoThumbprintIsPodBound(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	f.registers(t, account, issuePod, roleA)
	for _, key := range []*ecdsa.PrivateKey{newKey(t), newKey(t)} {
		f.introspectsAs(t, f.exchanged(t, f.podTrade(t, roleA), key), roleA, person, project)
	}
}
