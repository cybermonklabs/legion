// Package forwardedtest gives the shapes in which a request can say it comes from an address of
// ours, for the walks that hold every route to answering such a request as it answers a bare one
// (api's and routes' TestNoRouteGrantsAccessByNetworkAddress, c-no-route-trusts-the-network).
package forwardedtest

import (
	"net/http"
	"strings"
)

// Internal are addresses of ours: loopback in both families, a private range, and the tailnet's
// IPv4 and IPv6 ranges.
var Internal = []string{"127.0.0.1", "::1", "10.0.0.1", "100.64.0.1", "fd7a:115c:a1e0::1"}

// Peers are the connections such a request arrives on: a loopback peer, and a peer in Google's
// external load balancer's range, from which auth.ForwardedAddress reads the second-to-last
// X-Forwarded-For entry rather than the last.
var Peers = []string{"127.0.0.1:40000", "35.191.4.5:41234"}

// headers are the other headers a proxy or a tailnet writes to say where a request came from, each
// naming an address or a person of ours.
var headers = map[string]string{
	"X-Real-IP":              "100.64.0.1",
	"Forwarded":              `for=127.0.0.1;proto=https, for="[fd7a:115c:a1e0::1]"`,
	"Via":                    "1.1 10.0.0.1",
	"Tailscale-User-Login":   "alice@example.com",
	"Tailscale-User-Name":    "alice@example.com",
	"Tailscale-Headers-Info": "https://tailscale.com/s/serve-headers",
}

// Variant is one shape of such a request: its name, which a failure quotes, and what it sets.
type Variant struct {
	Name  string
	Apply func(*http.Request)
}

// Variants are every shape, on each of Peers: X-Forwarded-For with every Internal address in every
// position a rule might read - each rotation of Internal as one line, so each address is once the
// leftmost entry, once the rightmost and once the second-to-last; each address alone; and each
// address as the first and as the last of two lines, the rest of Internal the other line - and
// with it each of the other headers, as an internal request carries them all.
func Variants() []Variant {
	var lines []struct {
		name  string
		lines []string
	}
	add := func(name string, value ...string) {
		lines = append(lines, struct {
			name  string
			lines []string
		}{name, value})
	}
	for i := range Internal {
		rotated := append(append([]string{}, Internal[i:]...), Internal[:i]...)
		add("one line from "+Internal[i], strings.Join(rotated, ", "))
	}
	for _, address := range Internal {
		add(address+" alone", address)
	}
	for i, address := range Internal {
		rest := strings.Join(append(append([]string{}, Internal[:i]...), Internal[i+1:]...), ", ")
		add(address+" as the first of two lines", address, rest)
		add(address+" as the last of two lines", rest, address)
	}
	var variants []Variant
	for _, peer := range Peers {
		for _, shape := range lines {
			variants = append(variants, Variant{
				Name: "X-Forwarded-For " + shape.name + " from " + peer,
				Apply: func(r *http.Request) {
					r.RemoteAddr = peer
					r.Header.Del("X-Forwarded-For")
					for _, line := range shape.lines {
						r.Header.Add("X-Forwarded-For", line)
					}
					for name, value := range headers {
						r.Header.Set(name, value)
					}
				},
			})
		}
	}
	return variants
}
