package record

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/proof"
)

// TestANewRequestObjectRefusesAnUnstorableRuneInEveryStoredField: VerifyRequestObject, which
// every new device login, launcher login and secret request is read through, refuses with
// ErrRequestInvalid an object whose login_hint, jti, or a detail's type, identifier, service or
// action holds a rune of Unicode category Cc, Cf, Co, Zl or Zp, naming the field, before anything
// is written. A space (Zs) is no refused rune, and an object with one in every field is accepted:
// the control that the refusals are the runes' and not the object's. A lone surrogate (Cs) cannot
// reach the check: JSON carries it as an escape, which encoding/json reads as U+FFFD, or as its
// raw UTF-8-style bytes (ED A0 80), which are no UTF-8 and which encoding/json reads as three
// U+FFFD, so the object is accepted and stores no surrogate.
//
// A detail's type is refused by the details check first (validateDetails takes agent_secret and
// launcher_credential alone), so its row names that reason; every other row names its own field.
//
// The named reverts: leave a field out of validateStoredText, and its row is accepted; drop a
// category from ReasonRefuses, and its row is accepted.
func TestANewRequestObjectRefusesAnUnstorableRuneInEveryStoredField(t *testing.T) {
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	const audience = "https://secrets.test"
	now := time.Now()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, &jose.SignerOptions{
		EmbedJWK: true, ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: RequestTyp},
	})
	if err != nil {
		t.Fatal(err)
	}
	signPayload := func(t *testing.T, payload []byte) string {
		t.Helper()
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
	// claimsWith is a request for one agent secret with every stored field filled, text set into
	// the field set chooses.
	claimsWith := func(set func(*requestClaims, string), text string) requestClaims {
		c := requestClaims{
			Issuer: thumbprint, Audience: audience, JTI: uuid.NewString(), IssuedAt: now.Unix(),
			Expires:              now.Unix() + int64(maxRequestLifetime.Seconds()),
			AuthorizationDetails: []AuthorizationDetail{{Type: KindAgentSecret, Identifier: "A_SECRET", Service: "a-service", Actions: []string{"read"}}},
			LoginHint:            "outside@outside.example",
		}
		if set != nil {
			set(&c, text)
		}
		return c
	}
	verify := func(t *testing.T, c requestClaims) (RequestObject, error) {
		t.Helper()
		payload, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return VerifyRequestObject(signPayload(t, payload), audience, time.Minute, now)
	}

	fields := []struct {
		name string
		set  func(*requestClaims, string)
		// refusal is what the error names: the field, or for a detail's type the details check.
		refusal string
	}{
		{"login_hint", func(c *requestClaims, s string) { c.LoginHint = s }, "login_hint contains a refused rune category"},
		{"jti", func(c *requestClaims, s string) { c.JTI = s }, "jti contains a refused rune category"},
		{"a detail's type", func(c *requestClaims, s string) { c.AuthorizationDetails[0].Type = s }, "authorization_details mixes types"},
		{"a detail's identifier", func(c *requestClaims, s string) { c.AuthorizationDetails[0].Identifier = s }, "an authorization detail's identifier contains a refused rune category"},
		{"a detail's service", func(c *requestClaims, s string) { c.AuthorizationDetails[0].Service = s }, "an authorization detail's service contains a refused rune category"},
		{"a detail's action", func(c *requestClaims, s string) { c.AuthorizationDetails[0].Actions[0] = s }, "an authorization detail's action contains a refused rune category"},
	}

	// The control: the object every row edits verifies as it is.
	if _, err := verify(t, claimsWith(nil, "")); err != nil {
		t.Fatalf("the object the rows edit is refused before any edit: %v", err)
	}

	t.Run("a refused rune in each field", func(t *testing.T) {
		for _, f := range fields {
			_, err := verify(t, claimsWith(f.set, "a\u0000b"))
			if !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), f.refusal) {
				t.Errorf("a NUL in %s = %v, want ErrRequestInvalid naming %q", f.name, err, f.refusal)
			}
		}
	})

	t.Run("each refused category", func(t *testing.T) {
		for _, row := range []struct {
			category string
			r        rune
		}{
			{"Cc", '\u0007'},
			{"Cf", '\u202e'},
			{"Co", '\ue000'},
			{"Zl", '\u2028'},
			{"Zp", '\u2029'},
		} {
			for _, f := range fields {
				if f.name == "a detail's type" {
					continue // refused by the details check whatever it holds
				}
				_, err := verify(t, claimsWith(f.set, "a"+string(row.r)+"b"))
				if !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), f.refusal) {
					t.Errorf("%U (%s) in %s = %v, want ErrRequestInvalid naming %q", row.r, row.category, f.name, err, f.refusal)
				}
			}
		}
	})

	t.Run("a space is no refused rune", func(t *testing.T) {
		for _, space := range []rune{' ', '\u00a0', '\u3000'} {
			for _, f := range fields {
				if f.name == "a detail's type" {
					continue
				}
				if _, err := verify(t, claimsWith(f.set, "a"+string(space)+"b")); err != nil {
					t.Errorf("%U (Zs) in %s = %v, want it accepted", space, f.name, err)
				}
			}
		}
	})

	t.Run("a lone surrogate arrives as U+FFFD", func(t *testing.T) {
		if !ReasonRefuses(0xD800) {
			t.Fatal("ReasonRefuses(U+D800) = false, want the Cs category refused")
		}
		payload, err := json.Marshal(claimsWith(nil, ""))
		if err != nil {
			t.Fatal(err)
		}
		raw := strings.Replace(string(payload), `"login_hint":"outside@outside.example"`, `"login_hint":"a\ud800b@outside.example"`, 1)
		if raw == string(payload) {
			t.Fatal("the payload holds no login_hint to edit")
		}
		obj, err := VerifyRequestObject(signPayload(t, []byte(raw)), audience, time.Minute, now)
		if err != nil || obj.LoginHint != "a\ufffdb@outside.example" {
			t.Fatalf("a login_hint with the escape \\ud800 = (%q, %v), want it read as U+FFFD and accepted", obj.LoginHint, err)
		}
	})
}
