package oidc

// Regression test from the final review of cybermonklabs/legion#4160. At 17da83d280 it failed
// with EXPLOIT: VerifyGoogleIdentity read email and email_verified from the token's text, between
// its first and last dot, while the signature check (go-jose's ParseSigned) also verified the JWS
// JSON serialization, whose text can carry a forged claim set beside the signed members. It
// passes now that every claim is read from the verified payload and only the compact
// serialization verifies.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

func TestRedTeamGoogleIdentityJSONSerialization(t *testing.T) {
	t.Parallel()
	const attacker = "attacker@attacker-project.iam.gserviceaccount.com"
	issuer := oidctest.New(t)
	signing := oidctest.NewKey(t, "google-signing-key")
	issuer.Publish(signing)
	verifier, err := New(context.Background(), issuer.URL(), googleTestAudience)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A genuine token the issuer signed for the listener's audience, naming the attacker's own account.
	genuine := googleIdentityClaims(issuer, googleTestAudience)
	genuine["email"] = attacker
	compact := issuer.Mint(t, signing, genuine)

	// Control: the compact form names the attacker.
	claims, err := verifier.VerifyGoogleIdentity(context.Background(), compact)
	if err != nil || claims.Email != attacker || !claims.EmailVerified {
		t.Fatalf("control: compact token: claims = %+v, err = %v; want the attacker's own account", claims, err)
	}

	// The same signed parts as a flattened JWS JSON object, plus one unknown member whose two dots
	// frame a forged claim set. go-jose (ParseSigned) verifies the real payload; tokenPayload reads
	// the text between the first and last dot.
	parts := strings.Split(compact, ".")
	forgedClaims, _ := json.Marshal(map[string]any{
		"iss":            issuer.URL(),
		"email":          googleTestAccount,
		"email_verified": true,
	})
	envelope, _ := json.Marshal(map[string]string{
		"protected": parts[0],
		"payload":   parts[1],
		"signature": parts[2],
		"x":         "." + base64.RawURLEncoding.EncodeToString(forgedClaims) + ".",
	})
	raw := string(envelope)
	if strings.Count(raw, ".") != 2 || !LooksLikeJWT(raw) {
		t.Fatalf("test construction: %q is not three dot-separated segments", raw)
	}

	claims, err = verifier.VerifyGoogleIdentity(context.Background(), raw)
	if err == nil && claims.Email != attacker {
		t.Fatalf("EXPLOIT: a token signed for %s, re-serialized as JWS JSON, verified as %q (email_verified %t)", attacker, claims.Email, claims.EmailVerified)
	}
	t.Logf("JSON-serialized token: claims = %+v, err = %v", claims, err)
}
