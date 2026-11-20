package redact

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/tlsalert/tlsalerttest"
)

// TestATLSAlertIsTheTLSClass: a request that met a real TLS alert over TCP - the server's own, or
// the one the client sent on a record it could not take - is of the class "TLS failed", which
// Go's TLS client returns as a *net.OpError and never as a tls.AlertError. Controls: QUIC's
// tls.AlertError is still that class, and a refused dial is still "connection refused".
func TestATLSAlertIsTheTLSClass(t *testing.T) {
	get := func(addr string) error {
		response, err := http.Get("https://" + addr + "/")
		if err == nil {
			_ = response.Body.Close()
			t.Fatalf("a GET at %s answered %d; want it to fail", addr, response.StatusCode)
		}
		return err
	}
	for _, row := range []struct {
		name string
		err  error
		want string
	}{
		{"the server's TLS alert", get(tlsalerttest.Remote(t)), "TLS failed"},
		{"the client's TLS alert on a record it could not take", get(tlsalerttest.Local(t)), "TLS failed"},
		{"control: QUIC's tls.AlertError", fmt.Errorf("handshake: %w", tls.AlertError(40)), "TLS failed"},
		{"control: a refused dial", get(testport.Refused(t)), "connection refused"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := Class(row.err).Error(); got != row.want {
				t.Fatalf("Class(%v) = %q, want %q", row.err, got, row.want)
			}
		})
	}
}
