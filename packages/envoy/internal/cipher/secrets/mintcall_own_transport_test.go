package secrets

import (
	"crypto/tls"
	"net/http"
	"testing"
)

// TestTheMintersOwnTransportInheritsNothingFromTheDefault: newOwnTransport builds a transport of
// its own rather than cloning http.DefaultTransport, so whatever other code set on the default
// before this package started - a callback for a negotiated protocol, a TLS configuration - does
// not reach the transport whose trace mintAttempt reads. The default is configured with both for
// the call alone and put back after.
func TestTheMintersOwnTransportInheritsNothingFromTheDefault(t *testing.T) {
	configured := &http.Transport{
		TLSClientConfig: &tls.Config{NextProtos: []string{"another-protocol"}},
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{
			"another-protocol": func(string, *tls.Conn) http.RoundTripper { return nil },
		},
	}
	original := http.DefaultTransport
	http.DefaultTransport = configured
	own := newOwnTransport()
	http.DefaultTransport = original

	if own.TLSNextProto != nil || own.TLSClientConfig != nil {
		t.Fatalf("the minters' own transport took TLSNextProto %v and TLSClientConfig %v from the default; want neither", own.TLSNextProto, own.TLSClientConfig)
	}
	if own.Proxy == nil || own.DialContext == nil || !own.ForceAttemptHTTP2 {
		t.Fatalf("the minters' own transport lacks the default's settings: proxy %t, dial %t, HTTP/2 %t", own.Proxy != nil, own.DialContext != nil, own.ForceAttemptHTTP2)
	}
	if !tracesConnections(ownClient) || ownClient.Transport != ownTransport {
		t.Fatal("the minters' own client does not send through the traced ownTransport")
	}
}
