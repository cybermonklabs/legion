package brokertest

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// unrelatedCredential is a request credential that shares no run with the words a row quotes, so
// only the rule can take the URL's user information and query value out of them.
const unrelatedCredential = "unrelated-bearer-000000"

// TestAnErrorJSONPrintsNoCredentialItHolds: a body that is an object whose "error" is a string, as
// the broker's error JSON is, can come from whatever stands in its place and echo the request back
// or quote a URL that carries a credential. Its text holds none of them: only its error and code
// print, each with the request's credentials and every URL's redacted (the red team's error-JSON
// probes of #4368 at db7e4a0226).
func TestAnErrorJSONPrintsNoCredentialItHolds(t *testing.T) {
	for _, row := range []struct{ name, body, carried string }{
		{"echo-member", `{"error":"bad gateway","echo":{"authorization":"Bearer ` + redacttest.Token + `"}}`, redacttest.Token},
		{"error-quotes-url", `{"error":"` + redacttest.Words + `","code":"bad_gateway"}`, unrelatedCredential},
		{"error-echoes-token", `{"error":"bad gateway for Bearer ` + redacttest.Token + `"}`, redacttest.Token},
		{"code-quotes-url", `{"error":"refused","code":"` + redacttest.Code + `"}`, unrelatedCredential},
	} {
		t.Run(row.name, func(t *testing.T) {
			redacttest.RequireNone(t, "brokertest.error-json/"+row.name, answerText([]byte(row.body), row.carried))
		})
	}
}
