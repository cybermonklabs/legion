package machine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestAServiceTokenThroughAnUnreadableKeySetIsUnavailableNotInvalid: VerifyServiceToken passes a
// verifier's oidc.ErrIssuerUnavailable through as it came, with the wait of the key set whose read
// failed (oidc.RetryAfter), never as ErrTokenInvalid: the check reached no verdict on the token.
// The verifier's other answers keep their refusals: oidc.ErrSignature is ErrTokenInvalid and
// oidc.ErrExpired is ErrTokenExpired. The unreadable row's error is the one a Verifier answers
// (classify wraps the key source's error), from a real key set that fails, its FailureSpacing set
// to 2.5 s so the wait it carries is that set's own.
func TestAServiceTokenThroughAnUnreadableKeySetIsUnavailableNotInvalid(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.FailKeys(http.StatusServiceUnavailable)
	keys, err := oidc.NewKeys(context.Background(), issuer.URL())
	if err != nil {
		t.Fatalf("oidc.NewKeys: %v", err)
	}
	keys.FailureSpacing = 2500 * time.Millisecond
	_, keySetErr := keys.Key(context.Background(), "signing-key", "RS256")
	if !errors.Is(keySetErr, oidc.ErrKeySet) {
		t.Fatalf("Key while the key set answers 503 = %v, want oidc.ErrKeySet", keySetErr)
	}
	unreadable := fmt.Errorf("%w: %w", oidc.ErrIssuerUnavailable, keySetErr)

	svc := &Service{Enroll: &enroll.Service{}}
	for _, row := range []struct {
		name     string
		verifier error
		want     error
		not      error
	}{
		{"unreadable", unreadable, oidc.ErrIssuerUnavailable, ErrTokenInvalid},
		{"signature", oidc.ErrSignature, ErrTokenInvalid, oidc.ErrIssuerUnavailable},
		{"expired", fmt.Errorf("%w: the token expired a minute ago", oidc.ErrExpired), ErrTokenExpired, ErrTokenInvalid},
	} {
		t.Run(row.name, func(t *testing.T) {
			svc.Enroll.Pod = fakeTokens{err: row.verifier}
			_, err := svc.VerifyServiceToken(context.Background(), "a-service-token")
			if !errors.Is(err, row.want) || errors.Is(err, row.not) {
				t.Fatalf("VerifyServiceToken whose verifier answered %v = %v, want %v and not %v", row.verifier, err, row.want, row.not)
			}
			wait, carried := oidc.RetryAfter(err)
			if row.want == oidc.ErrIssuerUnavailable && (!carried || wait != keys.FailureSpacing) {
				t.Fatalf("oidc.RetryAfter(VerifyServiceToken's error) = %s, %t; want the key set's FailureSpacing, %s", wait, carried, keys.FailureSpacing)
			}
			if row.want != oidc.ErrIssuerUnavailable && carried {
				t.Fatalf("oidc.RetryAfter(VerifyServiceToken's refusal %v) = %s; want no wait on a refusal", err, wait)
			}
		})
	}
}
