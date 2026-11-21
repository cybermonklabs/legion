package identity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc"
)

// TestPasskeyRuleSignsInNoGoogleAccountOutsideTheWorkspace (R5, E-L1c): PasskeyRule, the rule
// boot builds with DISPATCH_SIGNIN_PASSKEY=1, signs in with Google only an account whose hd is the
// domain, as OutsideRule does (TestOutsideRuleSignsInNoGoogleAccountOutsideTheWorkspace): an
// account of no Workspace and one of another Workspace that a list names are ErrNotInDomain,
// naming the lowercased address, with no member kind and without reading the lists.
//
// Control: give PasskeyRule a Person of its own that admits a listed, verified account outside the
// domain as a restricted member, and both rows fail.
func TestPasskeyRuleSignsInNoGoogleAccountOutsideTheWorkspace(t *testing.T) {
	for _, tc := range []struct{ name, email, hd string }{
		{"an account of no Workspace", "Outsider@Other.example", ""},
		{"an account of another Workspace", "outsider@other.example", "other.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			login := strings.ToLower(tc.email)
			lister := &stubLister{listed: map[string]bool{login: true}}
			rule := PasskeyRule{OutsideRule: outsideRule(lister)}
			person, err := rule.Person(context.Background(), oidc.Claims{Subject: "g-1", Email: tc.email, EmailVerified: true, HostedDomain: tc.hd})
			if !errors.Is(err, ErrNotInDomain) || person != (SignedIn{Email: login}) || !strings.Contains(err.Error(), login) {
				t.Fatalf("PasskeyRule.Person of %s (hd %q), which a list names = %+v, %v; want ErrNotInDomain naming %s, with no member kind", tc.email, tc.hd, person, err, login)
			}
			if asked := lister.calls(); len(asked) != 0 {
				t.Fatalf("the lister was asked %q for a Google sign-in, want no call", asked)
			}
		})
	}
}
