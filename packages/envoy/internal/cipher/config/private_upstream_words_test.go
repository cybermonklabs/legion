package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAPrivateUpstreamHostRefusalQuotesNoneOfTheEntry: the host of a BROKER_PROXY_PRIVATE_UPSTREAM
// entry is the text before its first =, so an entry written as a URL alone whose query holds an =
// puts the URL's scheme, user information, host and query key there. rulehost.Refusal refuses that
// for its scheme, and the refusal names the setting and says what is wrong in rulehost's words,
// quoting none of the entry, so it prints no run of the probe's user or password. The probe's
// token follows the query's =, so no host holds it; the twin shows the entry as written holds
// every marker. Captures config.private-upstream-host.
func TestAPrivateUpstreamHostRefusalQuotesNoneOfTheEntry(t *testing.T) {
	const entry = "https://" + redacttest.User + ":" + redacttest.Password + "@dispatch.test/?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		for _, marker := range redacttest.Secrets {
			if !strings.Contains(entry, marker) {
				t.Errorf("the entry as written, %q, holds no %q, so the row cannot see it printed", entry, marker)
			}
		}
	})
	t.Run("host", func(t *testing.T) {
		_, err := ParsePrivateUpstreams(entry)
		if err == nil {
			t.Fatalf("ParsePrivateUpstreams(%q) read it; want its host refused", entry)
		}
		text := err.Error()
		redacttest.RequireNoPiece(t, "config.private-upstream-host", text, redacttest.User, redacttest.Password, redacttest.Token)
		if !strings.HasPrefix(text, privateUpstreamSetting+" ") || !strings.Contains(text, "holds a scheme") {
			t.Errorf("the refusal %q does not name %s and say its host holds a scheme", text, privateUpstreamSetting)
		}
	})
}
