package storetest

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnAdminURLThatDoesNotParsePrintsNoneOfIt: DISPATCH_TEST_DATABASE_URL carries the server's
// user and password in its user information and can carry a query, and openAdmin's error is what
// ready prints when the admin pool cannot open. A value net/url cannot parse prints its class
// alone, beside the setting: one whose port is not a number (parse), and one that names no host
// after its user information, whose password net/url reads as the port (no-host). A value pgx
// cannot read prints pgx-config's fixed words, beside the setting. The twin shows each value holds
// the credentials. Captures storetest.admin-url/<case>.
func TestAnAdminURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	userinfo := redacttest.User + ":" + redacttest.Password
	query := "?access" + "_token=" + redacttest.Token
	notParsed := "parse " + DatabaseURLEnv + ": " + redact.ClassNotAURL
	cases := []struct{ name, raw, want string }{
		{"parse", "postgres://" + userinfo + "@127.0.0.1:not-a-port/postgres" + query, notParsed},
		{"no-host", "postgres://" + userinfo + "/postgres" + query, notParsed},
		{"pgx-config", "postgres://" + userinfo + "@127.0.0.1:1/postgres" + query + "&sslmode=not-a-mode",
			"parse the admin database URL " + DatabaseURLEnv + " names: pgx cannot read its connection settings"},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			if held := redacttest.Held(c.raw); len(held) != len(redacttest.Secrets) {
				t.Errorf("%s: the value %q holds %q; want every credential, so the row can see one printed", c.name, c.raw, held)
			}
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := openAdmin(c.raw)
			if err == nil {
				t.Fatalf("openAdmin(%q) succeeded; want the value refused", c.raw)
			}
			redacttest.RequireNoPiece(t, "storetest.admin-url/"+c.name, err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
			if err.Error() != c.want {
				t.Errorf("openAdmin = %q; want %q, the setting and the failure's class", err, c.want)
			}
		})
	}
}
