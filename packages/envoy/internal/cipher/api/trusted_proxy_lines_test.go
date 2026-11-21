package api

import (
	"net/http"
	"testing"
)

// TestTheTrustedProxyHeaderKeysOnTheLastEntryOfItsLastLine: on the broker's own address behind
// BROKER_TRUSTED_PROXY_HEADER, a request whose header comes as two lines, the first the client's
// own text and the last the one the proxy wrote, keys its buckets on the last entry of the last
// line. The proxy's entry is there whether it merges every line into one, appends to the last line
// it was sent, or adds a line of its own after the client's, so reading the first line keys the
// bucket on an address the client chose (the edge reads its header the same way: edgeClientAddress).
//
// The named revert: read the header's first line (r.Header.Get), and the client's own last entry
// keys the bucket.
func TestTheTrustedProxyHeaderKeysOnTheLastEntryOfItsLastLine(t *testing.T) {
	r, err := http.NewRequest(http.MethodPost, "http://broker.example/v1/oauth/device_authorization", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.RemoteAddr = "10.0.0.9:4321"
	r.Header.Add("X-Forwarded-For", "6.6.6.6, 192.0.2.250")
	r.Header.Add("X-Forwarded-For", "10.9.0.1")
	if got := clientAddress(r, "X-Forwarded-For"); got != "10.9.0.1" {
		t.Fatalf("clientAddress of the lines %q = %q, want 10.9.0.1, the last entry of the last line", r.Header.Values("X-Forwarded-For"), got)
	}
}
