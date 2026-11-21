package oidc

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	googleTestAudience = "example-listener-audience"
	googleTestAccount  = "example-sink@example-project.iam.gserviceaccount.com"
)

// googleIdentityClaims is the claim set of a machine's Google identity token minted with
// format=full: Google's numeric subject, the audience it was asked for, and the account's
// address, which Google verified.
func googleIdentityClaims(issuer *oidctest.Issuer, audience string) map[string]any {
	claims := issuer.Claims("118000000000000000001", audience)
	claims["email"] = googleTestAccount
	claims["email_verified"] = true
	return claims
}

// A Google identity token names its caller by the address in its email claim, which Verify leaves
// unread. VerifyGoogleIdentity verifies the token as Verify does and reads that address and
// whether Google verified it.
func TestVerifyGoogleIdentityReadsTheAccount(t *testing.T) {
	issuer := oidctest.New(t)
	signing := oidctest.NewKey(t, "google-signing-key")
	issuer.Publish(signing)
	verifier, err := New(context.Background(), issuer.URL(), googleTestAudience)
	if err != nil {
		t.Fatalf("New(%s, %s): %v", issuer.URL(), googleTestAudience, err)
	}

	claims, err := verifier.VerifyGoogleIdentity(context.Background(), issuer.Mint(t, signing, googleIdentityClaims(issuer, googleTestAudience)))
	if err != nil {
		t.Fatalf("VerifyGoogleIdentity: %v", err)
	}
	if claims.Email != googleTestAccount || !claims.EmailVerified {
		t.Fatalf("claims = %+v, want the account %s with its address verified", claims, googleTestAccount)
	}

	unverified := googleIdentityClaims(issuer, googleTestAudience)
	unverified["email_verified"] = false
	claims, err = verifier.VerifyGoogleIdentity(context.Background(), issuer.Mint(t, signing, unverified))
	if err != nil || claims.Email != googleTestAccount || claims.EmailVerified {
		t.Fatalf("an unverified address: claims = %+v, err = %v; want the account read and EmailVerified false", claims, err)
	}

	_, err = verifier.VerifyGoogleIdentity(context.Background(), issuer.Mint(t, signing, googleIdentityClaims(issuer, "another-audience")))
	if !errors.Is(err, ErrAudience) {
		t.Fatalf("a token for another audience: err = %v, want %v", err, ErrAudience)
	}
}

// Google writes its issuer two ways, and go-oidc accepts the scheme-less accounts.google.com for
// a verifier of https://accounts.google.com. A token of either spelling that fails is refused
// for what is wrong with it, not as another issuer's: the class is what an operator reads, and
// what a caller that tries several verifiers takes "not mine" from (ErrIssuer). Both spellings
// also pick Google's verifier (LazyVerifier.Issues), and no other issuer does.
func TestGooglesSchemelessIssuerIsGooglesOwn(t *testing.T) {
	issuer := oidctest.New(t)
	signing := oidctest.NewKey(t, "google-signing-key")
	issuer.Publish(signing)
	keys, err := NewKeys(context.Background(), issuer.URL())
	if err != nil {
		t.Fatalf("NewKeys(%s): %v", issuer.URL(), err)
	}
	google := NewWithKeys(GoogleIssuer, googleTestAudience, keys)
	for _, spelling := range []string{GoogleIssuer, "accounts.google.com"} {
		claims := googleIdentityClaims(issuer, "another-audience")
		claims["iss"] = spelling
		raw := issuer.Mint(t, signing, claims)
		if _, err := google.VerifyGoogleIdentity(context.Background(), raw); !errors.Is(err, ErrAudience) {
			t.Errorf("iss %q for another audience: err = %v, want %v", spelling, err, ErrAudience)
		}
		if !DiscoveredVerifier(google).Issues(raw) {
			t.Errorf("iss %q does not pick Google's verifier", spelling)
		}
	}
	other := googleIdentityClaims(issuer, googleTestAudience)
	other["iss"] = "https://accounts.google.com.example"
	raw := issuer.Mint(t, signing, other)
	if _, err := google.VerifyGoogleIdentity(context.Background(), raw); !errors.Is(err, ErrIssuer) {
		t.Errorf("another issuer: err = %v, want %v", err, ErrIssuer)
	}
	if DiscoveredVerifier(google).Issues(raw) {
		t.Errorf("another issuer picks Google's verifier")
	}
}
