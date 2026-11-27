package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestARefusedURLPrintsNoneOfItsCredentials: a -url that is no http(s) URL with a host, with
// redacttest's credentials in its user information and its query, is refused naming the value as
// redact.URL reads it, where the refusal quoted it as given: one of another scheme (scheme), one
// with no host (no-host) and one url.Parse refuses (unparsed). The value as given holds the
// credentials (twin). The control: the refusal still says what -url must be. Captures
// loadtest.browsertype-url/<case>.
func TestARefusedURLPrintsNoneOfItsCredentials(t *testing.T) {
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	query := "?access" + "_token=" + redacttest.Token
	cases := []struct{ name, value string }{
		{"scheme", "ftp://" + userinfo + "127.0.0.1:1/docs" + query},
		{"no-host", "http://" + userinfo + "/docs" + query},
		{"unparsed", "http://" + userinfo + "127.0.0.1:1/%zz" + query},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			if len(redacttest.Held(c.value)) != len(redacttest.Secrets) {
				t.Fatalf("%s: the value %q holds fewer than every credential, so the row cannot see them printed", c.name, c.value)
			}
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseFlags([]string{"-url", c.value, "-artifact", "a"})
			if err == nil {
				t.Fatalf("parseFlags took -url %q", c.value)
			}
			redacttest.RequireNoPiece(t, "loadtest.browsertype-url/"+c.name, err.Error())
			if !strings.Contains(err.Error(), "-url must be http(s)://host:port") {
				t.Errorf("parseFlags = %q; want the refusal to say what -url must be", err)
			}
		})
	}
}
