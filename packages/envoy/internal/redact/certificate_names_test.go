package redact

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The words each certificate failure below prints as (tlsErrorWords), written out here so the rows
// also build in a tree from before those words, where they fail on the name x509 quotes.
const (
	wantHostnameWords  = "tls: failed to verify certificate: x509: certificate is not valid for the host asked (the names it is valid for are not printed: the answering party chose them)"
	wantAuthorityWords = "tls: failed to verify certificate: x509: certificate signed by unknown authority"
	wantInvalidWords   = "tls: failed to verify certificate: x509: certificate is not valid (x509's reason is not printed: it can quote the certificate, which the answering party chose)"
	wantRecordWords    = "tls: a record the server sent is not TLS this client can read (most often: what listens at the address does not speak TLS)"
)

// certificateCase is a request whose TLS handshake failed on the certificate the answering party
// sent, made against a stand-in or built as crypto/tls and crypto/x509 build that failure.
type certificateCase struct {
	name string
	// fail answers the configured URL, the request made under it and the request's failure.
	fail func(t *testing.T) (base string, request *http.Request, err error)
	// words is what the failure prints as.
	words string
	// finds reports whether an error still finds the failure's x509 type (errors.As).
	finds func(err error) bool
}

// certificateCases are the failures of a request whose certificate names redacttest's
// CertificateName, each of which crypto/x509 quotes in its own text: a self-signed certificate
// valid for that name alone, asked for as localhost by a client that does not trust it
// (wrong-host-untrusted) and by one that does (wrong-host-trusted), since x509 checks the name
// before the chain; a certificate for localhost sent with a forged authority of that name
// (forged-issuer); and, built, an authority's constraint that excludes the name (constraint) and
// a certificate failure under a *net.OpError (under-a-net-error), whose text quotes what it wraps.
func certificateCases() []certificateCase {
	handshake := func(fault redacttest.CertificateFault, trusted bool) func(t *testing.T) (string, *http.Request, error) {
		return func(t *testing.T) (string, *http.Request, error) {
			base, trusting := redacttest.CertificateServer(t, fault)
			client := &http.Client{}
			if trusted {
				client = trusting
			}
			request, err := http.NewRequest(http.MethodGet, base+"/v1/enrollments/self", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err == nil {
				_ = response.Body.Close()
				t.Fatal("the stand-in's certificate was accepted")
			}
			return base, request, err
		}
	}
	built := func(failure error) func(t *testing.T) (string, *http.Request, error) {
		return func(t *testing.T) (string, *http.Request, error) {
			const base = "https://localhost:8443"
			request, err := http.NewRequest(http.MethodGet, base+"/v1/enrollments/self", nil)
			if err != nil {
				t.Fatal(err)
			}
			return base, request, &url.Error{Op: "Get", URL: request.URL.String(), Err: failure}
		}
	}
	named := &x509.Certificate{Subject: pkix.Name{CommonName: redacttest.CertificateName}, DNSNames: []string{redacttest.CertificateName}}
	hostname := func(err error) bool { return errors.As(err, new(x509.HostnameError)) }
	return []certificateCase{
		{"wrong-host-untrusted", handshake(redacttest.WrongHost, false), wantHostnameWords, hostname},
		{"wrong-host-trusted", handshake(redacttest.WrongHost, true), wantHostnameWords, hostname},
		{"forged-issuer", handshake(redacttest.ForgedIssuer, false), wantAuthorityWords,
			func(err error) bool { return errors.As(err, new(x509.UnknownAuthorityError)) }},
		{"constraint", built(&tls.CertificateVerificationError{Err: x509.CertificateInvalidError{
			Cert: named, Reason: x509.CANotAuthorizedForThisName, Detail: `DNS name "` + redacttest.CertificateName + `" is not permitted by any constraint`,
		}}), wantInvalidWords, func(err error) bool { return errors.As(err, new(x509.CertificateInvalidError)) }},
		{"under-a-net-error", built(&net.OpError{Op: "read", Net: "tcp", Err: &tls.CertificateVerificationError{
			Err: x509.HostnameError{Certificate: named, Host: "localhost"},
		}}), wantHostnameWords, hostname},
	}
}

// TestCausePrintsACertificateFailureByItsWords: whatever answers at a configured address answers
// the TLS handshake with a certificate whose names it chose, and x509's text of the failure
// quotes them (each case's twin: net/http's own error holds the name): the names the certificate
// is valid for, the common name of a candidate authority sent with it, a name an authority's
// constraint excludes. Cause, beside a clean address and a request under /v1/ of it, prints the
// failure by the fixed words for its type, none of x509's text, so no run of the name; Error
// prints the call's line over the same words; and errors.As still finds the certificate failure
// and its x509 type, which a caller decides on. A TLS record failure prints its words too. Captures
// redact.certificate-names/cause-<case>.
func TestCausePrintsACertificateFailureByItsWords(t *testing.T) {
	for _, c := range certificateCases() {
		t.Run(c.name, func(t *testing.T) {
			base, request, err := c.fail(t)
			if !strings.Contains(err.Error(), redacttest.CertificateName) {
				t.Fatalf("net/http's own text names no name the certificate holds, so the row cannot see a leak: %q", err)
			}
			printed := Cause(err, base)
			redacttest.RequireNoPiece(t, "redact.certificate-names/cause-"+c.name, printed.Error(), redacttest.CertificateName)
			if printed.Error() != c.words {
				t.Errorf("Cause = %q, want %q", printed, c.words)
			}
			if line, want := Error(err, base).Error(), `Get "`+request.URL.String()+`": `+c.words; line != want {
				t.Errorf("Error = %q, want %q", line, want)
			}
			if !errors.As(printed, new(*tls.CertificateVerificationError)) || !c.finds(printed) {
				t.Errorf("Cause = %#v, which no longer finds the certificate failure and its x509 type", printed)
			}
		})
	}
	t.Run("record", func(t *testing.T) {
		failure := &url.Error{Op: "Get", URL: "https://localhost:8443/v1/enrollments/self", Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}}
		printed := Cause(failure, "https://localhost:8443")
		if printed.Error() != wantRecordWords || !errors.As(printed, new(tls.RecordHeaderError)) {
			t.Errorf("Cause = %q, want %q, finding the record failure", printed, wantRecordWords)
		}
	})
}
