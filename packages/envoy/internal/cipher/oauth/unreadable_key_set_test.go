// unreadable_key_set_test.go drives the token exchange while an issuer's key set cannot be read:
// the library reached no verdict on the subject or actor token, so the trade is answered 503
// temporarily_unavailable with Retry-After, never refused as the token's own invalid_request.
package oauth_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestATokenExchangeThroughAnUnreadableKeySetIsTemporarilyUnavailable: "subject unreadable", a
// pod's trade while the cluster's key set fails, and "actor unreadable", a person's trade on a
// machine while the Google key set fails, are each answered 503 temporarily_unavailable with code
// ISSUER_UNAVAILABLE and Retry-After 5, the key set's default FailureSpacing. The library checks
// the actor token before the subject, so the actor's check is the one that fails in the person's
// trade, and the subject is never read. "spacing": the cluster's key set read with a FailureSpacing
// of 2.5 s answers Retry-After 3. "refused": a subject token signed by a key the issuer never
// published, the set readable, is refused 400 invalid_request with no Retry-After, as today.
func TestATokenExchangeThroughAnUnreadableKeySetIsTemporarilyUnavailable(t *testing.T) {
	t.Run("subject unreadable", func(t *testing.T) {
		f := newExchangeFixture(t, func(*oauth.Config) {})
		f.cluster.FailKeys(http.StatusServiceUnavailable)
		status, header, body := f.post(t, "/token", exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), newKey(t))
		answeredTemporarilyUnavailable(t, "a pod's trade", status, header, body, "5")
	})
	t.Run("actor unreadable", func(t *testing.T) {
		f := newSignInFixture(t)
		f.google.FailKeys(http.StatusServiceUnavailable)
		status, header, body := f.post(t, "/token", f.personTrade(t, "ada-box-1"), newKey(t))
		answeredTemporarilyUnavailable(t, "a person's trade on a machine", status, header, body, "5")
	})
	t.Run("spacing", func(t *testing.T) {
		f := newExchangeFixture(t, func(*oauth.Config) {})
		keys, ok := f.config.Kubernetes.Keys.(*oidc.Keys)
		if !ok {
			t.Fatalf("the cluster's key source is a %T, want the *oidc.Keys a broker reads keys through", f.config.Kubernetes.Keys)
		}
		keys.FailureSpacing = 2500 * time.Millisecond
		f.cluster.FailKeys(http.StatusServiceUnavailable)
		status, header, body := f.post(t, "/token", exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), newKey(t))
		answeredTemporarilyUnavailable(t, "a pod's trade, the key set's FailureSpacing 2.5 s", status, header, body, "3")
	})
	t.Run("refused", func(t *testing.T) {
		f := newExchangeFixture(t, func(*oauth.Config) {})
		claims := f.cluster.Claims(account, podAudience)
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": "pod-uid-1"}}
		forged := f.cluster.Mint(t, oidctest.NewKey(t, "never-published"), claims)
		status, header, body := f.post(t, "/token", exchangeForm(forged, oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), newKey(t))
		if status != http.StatusBadRequest || body["error"] != "invalid_request" || header.Get("Retry-After") != "" {
			t.Fatalf("a pod's trade with a token signed by a key the issuer never published = %d (Retry-After %q) %v, want 400 invalid_request with no Retry-After",
				status, header.Get("Retry-After"), body)
		}
	})
}

// answeredTemporarilyUnavailable fails t unless the token endpoint answered what, a trade whose
// token check reached no verdict, 503 with Retry-After retryAfter and OAuth's error object
// {"error":"temporarily_unavailable","error_description":...,"code":"ISSUER_UNAVAILABLE"}.
func answeredTemporarilyUnavailable(t *testing.T, what string, status int, header http.Header, body map[string]any, retryAfter string) {
	t.Helper()
	description, _ := body["error_description"].(string)
	if status != http.StatusServiceUnavailable || header.Get("Retry-After") != retryAfter ||
		body["error"] != "temporarily_unavailable" || body["code"] != "ISSUER_UNAVAILABLE" || description == "" {
		t.Fatalf("%s while the issuer's key set cannot be read = %d (Retry-After %q) %v, want 503 with Retry-After %s, error temporarily_unavailable and code ISSUER_UNAVAILABLE naming the outage: the library reached no verdict on the token, so the answer is no refusal of it",
			what, status, header.Get("Retry-After"), body, retryAfter)
	}
}
