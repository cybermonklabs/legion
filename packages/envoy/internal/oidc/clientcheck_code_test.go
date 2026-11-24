package oidc

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/redact"
)

// TestACheckNamesTheOAuthErrorFromTheClosedSet pins which words a client check's error names an
// OAuth error with: the closed set's reading of the code as it came (redact.RefusalCode), the one
// every reader of a token endpoint's answer reads a code by (oauthrefusal), and never the
// ErrorCode of the answer. A code the set names prints as the wire spells it; a code it does not,
// and an answer that names no code at all, print the set's own words for that case, so the issuer
// chooses none of the words.
func TestACheckNamesTheOAuthErrorFromTheClosedSet(t *testing.T) {
	// unnamedCode is a code of the issuer's own that the closed set does not name, spelled as a
	// named code is - plain lowercase letters and underscores - so the rule would print it as it
	// came and only the closed set keeps it out of the error.
	const unnamedCode = "tenant_locked_out"
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), checkClientID, checkClientSecret)
	for _, c := range []struct {
		name   string
		status int
		code   string
		want   string
		absent string
		check  ClientCheck
	}{
		{"unnamed-code", http.StatusUnauthorized, unnamedCode, redact.OAuthOther.String(), unnamedCode, ClientCheckRejected},
		{"named-code", http.StatusUnauthorized, "invalid_client", redact.InvalidClient.String(), "", ClientCheckRejected},
		{"no-code", http.StatusForbidden, "", redact.OAuthNone.String(), "", ClientCheckRejected},
	} {
		t.Run(c.name, func(t *testing.T) {
			issuer.FailToken(c.status, c.code)
			t.Cleanup(func() { issuer.FailToken(0, "") })
			check, err := CheckClient(context.Background(), issuer.URL(), checkClientID, checkClientSecret)
			if check != c.check || err == nil {
				t.Fatalf("CheckClient against a token endpoint answering %d %q = (%q, %v), want %q with an error", c.status, c.code, check, err, c.check)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the check's error for a %d %q does not name the closed set's %q: %v", c.status, c.code, c.want, err)
			}
			if c.absent != "" && strings.Contains(err.Error(), c.absent) {
				t.Errorf("the check's error names the code the issuer wrote, %q: %v", c.absent, err)
			}
		})
	}
}
