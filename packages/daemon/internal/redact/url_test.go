package redact

import (
	"strings"
	"testing"
)

// URL prints a URL value without its query and fragment, and with its authority, when it holds an
// "@", as URLWithCredentials: a password holding a "'" (which ends a URL for URLCredentials), and
// one holding an unencoded "@" with or without a host after it, included. It prints none of a value
// that does not parse as a URL with a host, or that holds an "@" past its host, where a credential
// holding a "/", "?" or "#" written as the user lands with its head read as the host. The
// credentials are made up.
func TestURLPrintsNoCredentialOfAURLValue(t *testing.T) {
	const password, user = "madeUpUrl'Password0123", "madeUpUrlUser0123"
	for _, tc := range []struct{ name, raw, want string }{
		{"a plain URL", "http://127.0.0.1:13370", "http://127.0.0.1:13370"},
		{"a URL with a path", "https://dispatch.example/legion", "https://dispatch.example/legion"},
		{"a user name and password", "http://" + user + ":" + "secretpart0123" + "@127.0.0.1:13370", "http://" + URLWithCredentials},
		{"a password holding a quote", "http://" + user + ":" + password + "@127.0.0.1:13370", "http://" + URLWithCredentials},
		{"a password holding an @, then a host", "http://" + user + ":p@" + "madeUpTailPart0123@127.0.0.1:13370/x", "http://" + URLWithCredentials + "/x"},
		{"a password holding an @ and no host", "http://" + user + ":p@" + "madeUpTailPart0123", "http://" + URLWithCredentials},
		{"a query and a fragment", "https://dispatch.example/x?token=" + password + "#" + user, "https://dispatch.example/x"},
		{"a token holding a / as the user", "https://" + user + "/" + password + "@dispatch.example", URLNotPrinted},
		{"a token holding a ? as the user", "https://" + user + "?" + password + "@dispatch.example", URLNotPrinted},
		{"a token holding a # as the user", "https://" + user + "#" + password + "@dispatch.example", URLNotPrinted},
		{"no URL", user + password, URLNotPrinted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := URL(tc.raw)
			if got != tc.want {
				t.Fatalf("URL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			for _, secret := range []string{user, password, "madeUpTailPart0123"} {
				if strings.Contains(got, secret[:10]) {
					t.Fatalf("URL(%q) = %q, which holds %q", tc.raw, got, secret[:10])
				}
			}
		})
	}
	// The control: the rule on text keeps a password holding a "'" whole.
	if shown := URLCredentials("http://" + user + ":" + password + "@127.0.0.1:13370"); !strings.Contains(shown, password[10:]) {
		t.Fatalf("URLCredentials = %q, which this row expects to keep the password past its \"'\"", shown)
	}
}
