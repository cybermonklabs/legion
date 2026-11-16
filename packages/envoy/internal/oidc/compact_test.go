package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
