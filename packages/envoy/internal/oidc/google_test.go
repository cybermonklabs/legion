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
