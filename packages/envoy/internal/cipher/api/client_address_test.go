// This file is package api rather than api_test because it calls clientAddress, the broker's own
// address's reading of a caller's address, which is not exported.
package api

import (
	"net/http/httptest"
	"testing"
)

// TestTheBrokersOwnAddressReadsTheTrustedProxysLastLine holds clientAddress under
// BROKER_TRUSTED_PROXY_HEADER to the last entry of the header's last line, the one the trusted
// proxy appends however it writes the header: folded into one line, appended to the last line, or
// added as a line of its own. A line the client sent before the proxy's picks no bucket. A last
// line holding no entry, and no header, key on the connection's own address; with the setting
// unset the header is never read.
//
// The named revert: read the first line (r.Header.Get), as before this change, and a caller that
// sends a line of its own before the proxy's picks its bucket by that line's last entry.
func TestTheBrokersOwnAddressReadsTheTrustedProxysLastLine(t *testing.T) {
	const remote = "10.20.30.40"
	for _, row := range []struct {
		name, header string
		lines        []string
		want         string
	}{
		{"one line", "X-Forwarded-For", []string{"198.51.100.1, 10.0.0.9"}, "10.0.0.9"},
		{"one line of one entry", "X-Forwarded-For", []string{"10.0.0.9"}, "10.0.0.9"},
		{"a client's line before the proxy's own", "X-Forwarded-For", []string{"198.51.100.7", "10.0.0.9"}, "10.0.0.9"},
		{"a client's line before the one the proxy appended to", "X-Forwarded-For", []string{"198.51.100.7, 192.0.2.9", "203.0.113.5, 10.0.0.9"}, "10.0.0.9"},
		{"no header", "X-Forwarded-For", nil, remote},
		{"an empty last line", "X-Forwarded-For", []string{"198.51.100.7", ""}, remote},
		{"the setting unset", "", []string{"198.51.100.1, 10.0.0.9"}, remote},
	} {
		r := httptest.NewRequest("POST", "http://broker.test/v1/oauth/token", nil)
		r.RemoteAddr = remote + ":54321"
		for _, line := range row.lines {
			r.Header.Add("X-Forwarded-For", line)
		}
		if got := clientAddress(r, row.header); got != row.want {
			t.Errorf("%s: clientAddress = %q, want %q", row.name, got, row.want)
		}
	}
}
