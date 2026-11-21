package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// jwsJSON is compact re-serialized as a flattened JWS JSON object: the same signed header, payload
// and signature, which go-jose's ParseSigned verifies as it verifies the compact form.
func jwsJSON(t *testing.T, compact string) string {
	t.Helper()
	parts := strings.Split(compact, ".")
	envelope, err := json.Marshal(map[string]string{"protected": parts[0], "payload": parts[1], "signature": parts[2]})
	if err != nil {
		t.Fatal(err)
	}
	return string(envelope)
}

// A token is read in one serialization only, the compact one every issuer mints, so the payload a
// signature covers can never differ from the token's middle segment: the key set refuses the JSON
// serialization of a token whose compact form it verifies, and so does Verify, as malformed.
func TestOnlyTheCompactSerializationVerifies(t *testing.T) {
	issuer := oidctest.New(t)
	signing := oidctest.NewKey(t, "signing-key")
	issuer.Publish(signing)
	keys, err := NewKeys(context.Background(), issuer.URL())
	if err != nil {
		t.Fatalf("NewKeys(%s): %v", issuer.URL(), err)
	}
	compact := issuer.Mint(t, signing, issuer.Claims(testSubject, testAudience))
	asJSON := jwsJSON(t, compact)

	// The control: the key set verifies the compact form and answers its payload.
	payload, err := keySet{keys: keys}.VerifySignature(context.Background(), compact)
	if err != nil || !strings.Contains(string(payload), testSubject) {
		t.Fatalf("the compact form: payload %q, err %v; want the verified payload naming %s", payload, err, testSubject)
	}
	if _, err := (keySet{keys: keys}).VerifySignature(context.Background(), asJSON); err == nil {
		t.Fatalf("the key set verified the JWS JSON serialization of a token: %s", asJSON)
	}

	verifier := NewWithKeys(issuer.URL(), testAudience, keys)
	if _, err := verifier.Verify(context.Background(), compact); err != nil {
		t.Fatalf("Verify of the compact form: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), asJSON); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Verify of the JWS JSON serialization: err = %v, want %v", err, ErrMalformed)
	}
}

// VerifyClaims unmarshals the payload the verified signature covers into each target, so a caller
// reading private claims never re-reads the token's text.
func TestVerifyClaimsReadsTheVerifiedPayload(t *testing.T) {
	issuer := oidctest.New(t)
	signing := oidctest.NewKey(t, "signing-key")
	issuer.Publish(signing)
	verifier, err := New(context.Background(), issuer.URL(), testAudience)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	claims := issuer.Claims(testSubject, testAudience)
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": "example-pod-uid"}}
	var private struct {
		Kubernetes struct {
			Pod struct {
				UID string `json:"uid"`
			} `json:"pod"`
		} `json:"kubernetes.io"`
	}
	var registered struct {
		IssuedAt int64 `json:"iat"`
	}
	got, err := verifier.VerifyClaims(context.Background(), issuer.Mint(t, signing, claims), &private, &registered)
	if err != nil {
		t.Fatalf("VerifyClaims: %v", err)
	}
	if got.Subject != testSubject || private.Kubernetes.Pod.UID != "example-pod-uid" || registered.IssuedAt == 0 {
		t.Fatalf("VerifyClaims = %+v, pod uid %q, iat %d; want the subject, the pod uid and the issue time", got, private.Kubernetes.Pod.UID, registered.IssuedAt)
	}
}

// whiteSpaced is compact carrying white space in each of the places a token has been seen to
// carry it or could: a trailing line feed, a trailing CRLF, a space inside the payload segment, a
// tab inside the signature and a trailing no-break space. go-jose's ParseSigned stripped every
// space before it parsed a compact token, so each verified before only the compact serialization
// was taken, and each must verify still: the change refuses the JSON serialization and nothing
// else.
func whiteSpaced(compact string) map[string]string {
	dot := strings.IndexByte(compact, '.') + 5
	last := strings.LastIndexByte(compact, '.') + 5
	return map[string]string{
		"a trailing LF":                   compact + "\n",
		"a trailing CRLF":                 compact + "\r\n",
		"a space inside the payload":      compact[:dot] + " " + compact[dot:],
		"a tab inside the signature":      compact[:last] + "\t" + compact[last:],
		"a no-break space before the end": compact + "\u00a0",
	}
}

func TestATokenCarryingWhiteSpaceVerifiesAsBefore(t *testing.T) {
	issuer := oidctest.New(t)
	signing := oidctest.NewKey(t, "signing-key")
	issuer.Publish(signing)
	verifier, err := New(context.Background(), issuer.URL(), testAudience)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	compact := issuer.Mint(t, signing, issuer.Claims(testSubject, testAudience))
	for name, raw := range whiteSpaced(compact) {
		claims, err := verifier.Verify(context.Background(), raw)
		if err != nil || claims.Subject != testSubject {
			t.Errorf("%s: claims %+v, err %v; want it verified as the compact token is", name, claims, err)
		}
	}
}

// An issuer set reads a token's iss as a Verifier reads the token, every space removed, so each
// white-spaced form of a token of the set's second issuer goes to that issuer alone and verifies,
// as it verifies through a Verifier.
func TestATokenCarryingWhiteSpaceVerifiesThroughAnIssuerSet(t *testing.T) {
	set, first, second, _, secondKey := twoIssuers(t)
	compact := second.Mint(t, secondKey, second.Claims(testSubject, testAudience))
	want := NamedIssuer{Name: "second", URL: second.URL()}
	for name, raw := range whiteSpaced(compact) {
		claims, issuer, err := set.VerifyClaims(context.Background(), raw)
		if err != nil || claims.Subject != testSubject || issuer != want {
			t.Errorf("%s: claims %+v, issuer %+v, err %v; want it verified by the second issuer as the compact token is", name, claims, issuer, err)
		}
	}
	if got := reads(first); got != [2]int{} {
		t.Fatalf("the second issuer's tokens read the first issuer's discovery document and key set %v times, want none", got)
	}
}

// The sign-in's verifier (newVerifier) checks signatures with go-oidc's own RemoteKeySet, which
// verifies the JWS JSON serialization too, so verify's own refusal is the one compact check that
// verifier has: a JSON-form ID token is refused as malformed there, and a compact one, white
// space and all, verifies.
func TestTheSignInVerifierTakesOnlyTheCompactSerialization(t *testing.T) {
	issuer := oidctest.New(t)
	signing := oidctest.NewKey(t, "signing-key")
	issuer.Publish(signing)
	provider, err := gooidc.NewProvider(context.Background(), issuer.URL())
	if err != nil {
		t.Fatalf("gooidc.NewProvider(%s): %v", issuer.URL(), err)
	}
	verifier := newVerifier(provider, issuer.URL(), testAudience)
	compact := issuer.Mint(t, signing, issuer.Claims(testSubject, testAudience))
	if claims, err := verifier.Verify(context.Background(), compact); err != nil || claims.Subject != testSubject {
		t.Fatalf("the compact form: claims %+v, err %v; want it verified", claims, err)
	}
	if _, err := verifier.Verify(context.Background(), jwsJSON(t, compact)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("the JWS JSON serialization: err = %v, want %v", err, ErrMalformed)
	}
	for name, raw := range whiteSpaced(compact) {
		if claims, err := verifier.Verify(context.Background(), raw); err != nil || claims.Subject != testSubject {
			t.Errorf("%s: claims %+v, err %v; want it verified as the compact token is", name, claims, err)
		}
	}
}
