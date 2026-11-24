package redact

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestUnansweredPrintsACertificateFailureByItsWords: the failures of certificateCases, each a
// request that got no answer because the answering party's certificate failed, whose names x509's
// text quotes (each case's twin: net/http's own error holds the name). Unanswered names the call
// and prints the failure by the fixed words for its type, none of x509's text, so no run of the
// name; errors.Is, and errors.As for the *url.Error, the certificate failure and its x509 type,
// still find what they found. A body read that fails on a certificate (unreadable) prints the same
// words. Captures redact.certificate-names/unanswered-<case>.
func TestUnansweredPrintsACertificateFailureByItsWords(t *testing.T) {
	for _, c := range certificateCases() {
		t.Run(c.name, func(t *testing.T) {
			_, request, err := c.fail(t)
			if !strings.Contains(err.Error(), redacttest.CertificateName) {
				t.Fatalf("net/http's own text names no name the certificate holds, so the row cannot see a leak: %q", err)
			}
			printed := Unanswered(err, request, "")
			redacttest.RequireNoPiece(t, "redact.certificate-names/unanswered-"+c.name, printed.Error(), redacttest.CertificateName)
			if want := `Get "` + request.URL.String() + `": ` + c.words; printed.Error() != want {
				t.Errorf("Unanswered = %q, want %q", printed, want)
			}
			if !errors.Is(printed, err) || !errors.As(printed, new(*url.Error)) || !errors.As(printed, new(*tls.CertificateVerificationError)) || !c.finds(printed) {
				t.Errorf("Unanswered = %#v, which no longer finds the *url.Error, the certificate failure and its x509 type", printed)
			}
		})
	}
	t.Run("body-read", func(t *testing.T) {
		failure := &tls.CertificateVerificationError{Err: x509.HostnameError{Certificate: &x509.Certificate{DNSNames: []string{redacttest.CertificateName}}, Host: "localhost"}}
		if printed := unreadable(failure); printed.Error() != wantHostnameWords || !errors.Is(printed, failure) {
			t.Errorf("unreadable = %q, want %q, finding the failure", printed, wantHostnameWords)
		}
	})
}
