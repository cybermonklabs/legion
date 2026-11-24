package cipheraddr_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/urljoin"
)

// TestJoinBuildsTheRequestURLAtTheAddress: Join joins a route onto the address's path and keeps its
// query, user information and port, where the route appended to Raw's text would sit inside the
// query; it drops the fragment.
func TestJoinBuildsTheRequestURLAtTheAddress(t *testing.T) {
	for _, c := range []struct{ name, addr, ref, want string }{
		{"no-path", "http://127.0.0.1:42203", "/v1/oauth/token", "http://127.0.0.1:42203/v1/oauth/token"},
		{"path-query", "https://user:pass@broker.example.com:8443/broker?sig=v", "v1/requests?wait=1", "https://user:pass@broker.example.com:8443/broker/v1/requests?sig=v&wait=1"},
		{"trailing-slash-fragment", "https://broker.example.com/broker/#frag", "/v1/requests", "https://broker.example.com/broker/v1/requests"},
	} {
		t.Run(c.name, func(t *testing.T) {
			joined, err := cipheraddr.New(c.addr).Join(c.ref)
			if err != nil {
				t.Fatalf("Join(%q): %v", c.ref, err)
			}
			if got := joined.String(); got != c.want {
				t.Errorf("Join(%q) = %q; want %q", c.ref, got, c.want)
			}
		})
	}
}

// TestJoinRefusesAnAddressInWordsThatQuoteNoneOfIt: no address, one that does not parse and one with
// no scheme are urljoin.ErrBase, whose text holds nothing of the address.
func TestJoinRefusesAnAddressInWordsThatQuoteNoneOfIt(t *testing.T) {
	const marker = "not-a-secret-token"
	for _, c := range []struct {
		name string
		addr cipheraddr.Address
	}{
		{"none", cipheraddr.Address{}},
		{"unparsed", cipheraddr.New("http://not-a-secret-user:" + marker + "@127.0.0.1:1/%zz?token=" + marker)},
		{"no-scheme", cipheraddr.New("not-a-secret-user:" + marker + "@broker.example.com")},
	} {
		t.Run(c.name, func(t *testing.T) {
			joined, err := c.addr.Join("/v1/requests")
			if !errors.Is(err, urljoin.ErrBase) || joined != nil {
				t.Fatalf("Join = %v, %v; want nil, urljoin.ErrBase", joined, err)
			}
			if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "not-a-secret-user") {
				t.Errorf("Join's error %q quotes the address", err)
			}
		})
	}
}
