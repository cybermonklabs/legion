package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestAGoogleSignInOfAListedOutsiderIsRefusedWithPasskeysOn (R5, E-L1c): with
// DISPATCH_SIGNIN_PASSKEY=1, the setting production runs, a Google account outside example.com
// that PRIV's list names directly gets the 403 page at the canonical origin, no session cookie and
// no people row: an account of no Workspace and an account of another Workspace (hd other.example)
// alike. A person from outside signs in with a passkey or not at all.
//
// Control: give identity.PasskeyRule a Person that signs a listed, verified account outside the
// domain in as a restricted member (the Google outside rule's Person before E-L1c), and both
// sign-ins answer 302 with a session, people records the address as restricted, and the row fails.
func TestAGoogleSignInOfAListedOutsiderIsRefusedWithPasskeysOn(t *testing.T) {
	r := newPasskeyRig(t, nil)
	for _, claims := range []map[string]any{
		{"sub": "google-outsider", "email": outsiderEmail, "email_verified": true},
		{"sub": "google-outsider-ws", "email": outsiderEmail, "email_verified": true, "hd": "other.example"},
	} {
		refused := codeFlowSignInAt(t, r.handler, r.pool, "dispatch.test", claims)
		if refused.Code != http.StatusForbidden || sessionCookieSet(refused) ||
			!strings.Contains(refused.Body.String(), outsiderEmail+" is not in the domain that may use Dispatch") {
			t.Errorf("with passkey sign-in on, the Google sign-in %v at the canonical origin answered %d %s, want the 403 page saying it is not in the domain, with no session", claims, refused.Code, refused.Body.String())
		}
	}
	if kind := r.memberKind(t, outsiderEmail); kind != "" {
		t.Errorf("with passkey sign-in on, the refused Google sign-ins of %s were recorded in people as %q, want no row", outsiderEmail, kind)
	}
}
