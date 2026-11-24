package oidc

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAKeySetRefusalNamesItsStatusWithNoPhraseItWrote: the issuer's key-set endpoint, or whatever
// answers in its place, refuses the read under a reason phrase that quotes a credential's URL.
// Key's error names the status by its code and the standard phrase (redact.StatusLine), and none
// of the phrase the endpoint wrote. Captures oidc.status-line/key-set.
func TestAKeySetRefusalNamesItsStatusWithNoPhraseItWrote(t *testing.T) {
	keys := NewKeysAt(redacttest.StatusLineServer(t, http.StatusServiceUnavailable, "", "") + "/jwks")
	_, err := keys.Key(context.Background(), "exchange-key", testAlgorithm)
	if err == nil {
		t.Fatal("Key answered no error")
	}
	redacttest.RequireNone(t, "oidc.status-line/key-set", err.Error())
	if !strings.Contains(err.Error(), "answered 503 Service Unavailable") {
		t.Errorf("Key's error is %q, want the status in it", err)
	}
}
