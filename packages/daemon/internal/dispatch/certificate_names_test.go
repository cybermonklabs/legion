package dispatch

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestACertificateFailureOfADispatchReadPrintsNoNameTheCertificateHolds: whatever answers at
// Dispatch's address answers the TLS handshake with a certificate whose names it chose - one valid
// for oauth2calltest.CertificateName alone, read by the daemon's own client, which does not trust
// it (wrong-host-untrusted), and by a client that does (wrong-host-trusted), since x509 checks the
// name before the chain; and one for localhost sent with a forged authority of that name
// (forged-issuer). x509's text of the failure quotes the name (each case's twin: the certificate
// failure net/http's error holds names it). The read's error holds no run of the name, and is
// still a refusal of the transport no wait fixes (TransportRefusal, which reads the failure's type
// through errors.As), never a TransientError. Captures dispatch.certificate-names/<case>.
func TestACertificateFailureOfADispatchReadPrintsNoNameTheCertificateHolds(t *testing.T) {
	for _, c := range []struct {
		name    string
		fault   oauth2calltest.CertificateFault
		trusted bool
	}{
		{"wrong-host-untrusted", oauth2calltest.WrongHost, false},
		{"wrong-host-trusted", oauth2calltest.WrongHost, true},
		{"forged-issuer", oauth2calltest.ForgedIssuer, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			base, trusting := oauth2calltest.CertificateServer(t, c.fault)
			client := New(base, BearerTransport("test-token", nil))
			if c.trusted {
				client.httpClient = trusting
			}
			_, err := client.EventsAfter(context.Background(), "LEGION-9", 0)
			if err == nil {
				t.Fatal("EventsAfter answered no error")
			}
			var verification *tls.CertificateVerificationError
			if !errors.As(err, &verification) || !strings.Contains(verification.Error(), oauth2calltest.CertificateName) {
				t.Fatalf("the read's error holds no certificate failure that names the certificate's name, so the row cannot see a leak: %#v", err)
			}
			oauth2calltest.RequireNoPiece(t, "dispatch.certificate-names/"+c.name, err.Error(), oauth2calltest.CertificateName)
			if !TransportRefusal(err) || errors.As(err, new(*TransientError)) {
				t.Errorf("the read's error %q is not a refusal of the transport, or is a TransientError", err)
			}
		})
	}
}
