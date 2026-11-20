package record

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/proof"
)

// signRawRequest signs, as Sign does, a machine login's request object whose claims are the ones
// Sign writes for audience followed by members, raw JSON text that may repeat a key or write one
// in another case, which a claims struct cannot.
func signRawRequest(t *testing.T, key *ecdsa.PrivateKey, audience, members string, now time.Time) string {
	t.Helper()
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	base, err := json.Marshal(requestClaims{
		Issuer: thumbprint, Audience: audience, JTI: uuid.NewString(), IssuedAt: now.Unix(), Expires: now.Unix() + int64(maxRequestLifetime.Seconds()),
		AuthorizationDetails: []AuthorizationDetail{{Type: KindLauncherCredential, Identifier: "devbox.example"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := append(base[:len(base)-1:len(base)-1], ","+members+"}"...)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, &jose.SignerOptions{
		EmbedJWK:     true,
		ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: RequestTyp},
	})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return compact
}

// TestTheGatesLoginHintIsTheOneTheVerifierReads holds ClaimedLoginHint, which the broker's edge
// reads a device authorization's person by before any signature check, to the login_hint the
// verifier reads once the signature verifies: for a key written in another case, for two keys
// that name the field in either order, and for a payload naming none, both read the same person,
// so no payload passes the edge's Workspace gate as one person and is served as another.
//
// The named revert: decode the gate's hint with a reader of its own that matches the key's case
// exactly, and the rows whose last key is written in capitals read another person.
func TestTheGatesLoginHintIsTheOneTheVerifierReads(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const audience = "https://secrets.test"
	now := time.Now()
	for _, row := range []struct {
		members, want string
	}{
		{`"login_hint":"ada@example.com"`, "ada@example.com"},
		{`"LOGIN_HINT":"ada@example.com"`, "ada@example.com"},
		{`"login_hint":"x@outside.example","LOGIN_HINT":"ada@example.com"`, "ada@example.com"},
		{`"LOGIN_HINT":"x@outside.example","login_hint":"ada@example.com"`, "ada@example.com"},
		{`"login_hint":"ada@example.com","Login_Hint":"x@outside.example"`, "x@outside.example"},
		{`"login_hint":"ada@example.com","login_hint":"x@outside.example"`, "x@outside.example"},
		{`"reason":"no person named"`, ""},
	} {
		compact := signRawRequest(t, key, audience, row.members, now)
		verified, err := VerifyRequestObject(compact, audience, time.Minute, now)
		if err != nil {
			t.Fatalf("VerifyRequestObject of {%s}: %v", row.members, err)
		}
		if got := ClaimedLoginHint(compact); got != verified.LoginHint || got != row.want {
			t.Errorf("for {%s} the gate reads %q and the verifier %q, want both %q", row.members, got, verified.LoginHint, row.want)
		}
	}
	if got := ClaimedLoginHint("not.a.jws"); got != "" {
		t.Errorf("ClaimedLoginHint of no JWS = %q, want none", got)
	}
}

// TestARequestObjectHoldsNoRuneTheStoreCannotKeep: a request object whose login_hint, jti, or an
// authorization detail's identifier, service or action carries a rune ReasonRefuses, U+0000 among
// them, which Postgres cannot write in text at all, is refused ErrRequestInvalid by the verifier,
// before any caller stores it, naming the field and never quoting it; the same object with plain
// text verifies.
//
// The named revert: drop validateStoredText from verifyRequestObject, and each refused row
// verifies.
func TestARequestObjectHoldsNoRuneTheStoreCannotKeep(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const audience = "https://secrets.test"
	now := time.Now()
	secret := func(identifier, action, service string) []AuthorizationDetail {
		return []AuthorizationDetail{{Type: KindAgentSecret, Identifier: identifier, Actions: []string{action}, Service: service}}
	}
	sign := func(t *testing.T, details []AuthorizationDetail, loginHint string) string {
		t.Helper()
		compact, err := Sign(key, audience, details, "", loginHint, nil, now)
		if err != nil {
			t.Fatal(err)
		}
		return compact
	}
	launcher := []AuthorizationDetail{{Type: KindLauncherCredential, Identifier: "devbox.example"}}
	for _, bad := range []string{"\x00", "\a", "\n", "\u200b", "\u202e"} {
		for name, compact := range map[string]string{
			"login_hint":                           sign(t, launcher, "ada"+bad+"@outside.example"),
			"jti":                                  signRawRequest(t, key, audience, `"jti":"j`+jsonEscape(bad)+`"`, now),
			"an authorization detail's identifier": sign(t, secret("DEEL"+bad+"KEY", "inject", ""), ""),
			"an authorization detail's action":     sign(t, secret("DEEL_API_KEY", "inject"+bad, ""), ""),
			"an authorization detail's service":    sign(t, secret("DEEL_API_KEY", "inject", "svc"+bad), ""),
		} {
			_, err := VerifyRequestObject(compact, audience, time.Minute, now)
			if !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), bad) {
				t.Errorf("a request object whose %s carries %q = %v, want ErrRequestInvalid naming %s and not quoting it", name, bad, err, name)
			}
		}
	}
	for _, compact := range []string{sign(t, launcher, "ada@outside.example"), sign(t, secret("DEEL_API_KEY", "inject", ""), "")} {
		if _, err := VerifyRequestObject(compact, audience, time.Minute, now); err != nil {
			t.Errorf("a plain request object = %v, want it verified", err)
		}
	}
}

// jsonEscape is s as the text of a JSON string, without its quotes.
func jsonEscape(s string) string {
	text, _ := json.Marshal(s)
	return string(text[1 : len(text)-1])
}
