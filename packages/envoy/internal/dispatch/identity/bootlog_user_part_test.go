package identity

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact"
)

// The line each boot logs to say whom it signs in names DISPATCH_SIGNIN_ISSUER through redact.URL,
// so an issuer with a user part prints none of it; one with none is named as written. The
// password is made for this test.
func TestBootLogNamesNoUserPartOfTheIssuer(t *testing.T) {
	const password = "not-a-secret-0c7d52"
	for _, tc := range []struct{ issuer, want string }{
		{"https://made-up-user:" + password + "@issuer.example", redact.NotPrinted},
		{"https://issuer.example", "https://issuer.example"},
	} {
		for name, rule := range map[string]interface{ BootLog(issuer, clientID string) }{
			"domain": DomainRule{Domain: "example.com"},
			"group":  GroupRule{Group: "made-up-group"},
		} {
			var logged bytes.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
			rule.BootLog(tc.issuer, "made-up-client")
			line := logged.String()
			if strings.Contains(line, password) || strings.Contains(line, "made-up-user") || !strings.Contains(line, tc.want) {
				t.Errorf("%s rule, issuer %q: logged %q, want the issuer named as %q", name, tc.issuer, line, tc.want)
			}
		}
	}
}
