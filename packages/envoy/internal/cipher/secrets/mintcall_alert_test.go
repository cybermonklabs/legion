package secrets

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/tlsalert/tlsalerttest"
)

// TestATLSAlertReadsAsTheUpstreamAnswering: a mint's call that met a real TLS alert over TCP - the
// upstream's own, or the one this side sent on a record it could not take - names the upstream as
// having answered, since Go's TLS client returns the alert as a *net.OpError a dial's failure is
// also one of. Control: a refused dial is still an upstream that could not be reached.
func TestATLSAlertReadsAsTheUpstreamAnswering(t *testing.T) {
	for _, row := range []struct{ name, addr, want string }{
		{"the upstream's TLS alert", tlsalerttest.Remote(t), "answered, but the TLS exchange failed"},
		{"this side's TLS alert on a record it could not take", tlsalerttest.Local(t), "answered, but the TLS exchange failed"},
		{"control: a refused dial", testport.Refused(t), "could not be reached"},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := tlsalerttest.Get(t, row.addr)
			if got := TransportFailure(context.Background(), err); got != row.want {
				t.Fatalf("TransportFailure(%v) = %q, want %q", err, got, row.want)
			}
		})
	}
}
