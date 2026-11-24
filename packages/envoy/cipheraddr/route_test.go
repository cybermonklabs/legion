package cipheraddr_test

import (
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/redact"
)

// TestRoutePrintsTheRouteWhereTheCallWent: Route names a route whose id is spelled as words at the
// address the way Join builds the call's URL - the route joined onto the address's path, a trailing
// slash and a "." element of the address cleaned as Join cleans them - with the words as written,
// not escaped; beside an address that is not clean (user information, a query, one that does not
// parse) it prints redact.NotPrinted.
func TestRoutePrintsTheRouteWhereTheCallWent(t *testing.T) {
	const route = "/v1/enrollments/(the enrollment id, not printed)"
	for _, c := range []struct{ name, addr, want string }{
		{"no-path", "http://127.0.0.1:42203", "http://127.0.0.1:42203" + route},
		{"path", "https://broker.example.com/broker", "https://broker.example.com/broker" + route},
		{"trailing-slash", "https://broker.example.com/broker/", "https://broker.example.com/broker" + route},
		{"dot-element", "https://broker.example.com/a/./broker", "https://broker.example.com/a/broker" + route},
		{"query", "https://broker.example.com/broker?sig=not-a-secret-token", redact.NotPrinted},
		{"user-information", "https://not-a-secret-user:not-a-secret-password@broker.example.com", redact.NotPrinted},
		{"unparsed", "http://127.0.0.1:1/%zz", redact.NotPrinted},
		{"none", "", redact.NotPrinted},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := cipheraddr.New(c.addr).Route(route); got != c.want {
				t.Errorf("Route(%q) at %q = %q; want %q", route, c.addr, got, c.want)
			}
		})
	}
}
