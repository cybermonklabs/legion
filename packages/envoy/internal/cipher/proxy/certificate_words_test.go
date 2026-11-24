package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/logmask"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// wantHostnameWords is what the error of a call to a service whose certificate is valid for other
// hosts than the one asked for reads as (upstreamReason): redact's words for x509.HostnameError
// after crypto/tls's, written out so the rows also build in a tree from before them.
const wantHostnameWords = "tls: failed to verify certificate: x509: certificate is not valid for the host asked (the names it is valid for are not printed: the answering party chose them)"

// hostnameWordsHead is the start of wantHostnameWords that a line still holds where the words
// come after an endpoint's URL in a field answer.ClipFields cuts to its first 256 bytes, such as
// the reason of a refused Azure exchange.
const hostnameWordsHead = "tls: failed to verify certificate: x509: certificate is not valid for the host asked"

// TestACertificateFailureInMacOSWordsWritesNoNameTheCertificateHolds: on macOS, x509 hands on the
// system verifier's words for a certificate it refuses (crypto/x509/root_darwin.go of go1.27.3:
// for an expired certificate as a CertificateInvalidError's Detail, line 68, and for any other as
// an error of its own, line 74), and those words name the certificate's subject between curly
// quotation marks, which logmask.HidePeerBytes does not cut (each case's twin: the failure's text,
// its peer bytes hidden, still holds the name). The line and the 502 of a call the proxy could not
// carry (badGateway), and the error of an Azure exchange whose endpoint failed so (exchangeAt),
// print the failure by redact's fixed words for it (upstreamReason, redact.TLSWords), so no run of
// the name. Captures proxy.certificate-words/<case>: the line, the answer and the exchange's
// error.
func TestACertificateFailureInMacOSWordsWritesNoNameTheCertificateHolds(t *testing.T) {
	named := &x509.Certificate{Subject: pkix.Name{CommonName: redacttest.CertificateName}, DNSNames: []string{redacttest.CertificateName}}
	subject := "“" + redacttest.CertificateName + "”"
	for _, c := range []struct {
		name    string
		failure error
		words   string
	}{
		{"macos-untrusted", &tls.CertificateVerificationError{Err: fmt.Errorf("x509: %s", subject+" certificate is not trusted")},
			"tls: failed to verify certificate"},
		{"macos-expired", &tls.CertificateVerificationError{Err: x509.CertificateInvalidError{Cert: named, Reason: x509.Expired, Detail: subject + " certificate is expired"}},
			"tls: failed to verify certificate: x509: certificate has expired or is not yet valid (x509's detail is not printed: it can quote the certificate, which the answering party chose)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if hidden := logmask.HidePeerBytes(c.failure.Error()); !strings.Contains(hidden, redacttest.CertificateName) {
				t.Fatalf("the failure's text, its peer bytes hidden, names no name the certificate holds, so the row cannot see a leak: %q", hidden)
			}
			var lines bytes.Buffer
			s := &Server{
				log: slog.New(slog.NewJSONHandler(&lines, nil)),
				upstream: &http.Transport{DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
					return nil, c.failure
				}},
			}
			answer := httptest.NewRecorder()
			s.badGateway(boundHost, "/v1/things")(answer, httptest.NewRequest(http.MethodGet, "https://"+boundHost+"/v1/things", nil), c.failure)
			_, _, exchanged := s.exchangeAt(context.Background(), "https://login.example.net/token", url.Values{})
			if exchanged == nil {
				t.Fatal("the exchange answered no error")
			}
			redacttest.RequireNoPiece(t, "proxy.certificate-words/"+c.name, lines.String()+answer.Body.String()+exchanged.Error(), redacttest.CertificateName)
			var line map[string]any
			if err := json.Unmarshal(lines.Bytes(), &line); err != nil || line["msg"] != upstreamMessage || line["error"] != c.words {
				t.Errorf("the upstream line = %s (%v), want its error %q", lines.Bytes(), err, c.words)
			}
			if want := "airlock: " + boundHost + ": " + c.words + "\n"; answer.Code != http.StatusBadGateway || answer.Body.String() != want {
				t.Errorf("the answer = %d %q, want 502 %q", answer.Code, answer.Body.String(), want)
			}
			if exchanged.Error() != c.words {
				t.Errorf("the exchange's error = %q, want %q", exchanged, c.words)
			}
		})
	}
}
