package redact

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// phraseCase is a request whose TLS handshake failed in the way one phrase of tlsErrorWords names.
type phraseCase struct {
	name string
	// fail answers the configured URL, the request made under it and net/http's error of it.
	fail func(t *testing.T) (base string, request *http.Request, err error)
	// words is the phrase, as Cause prints the failure.
	words string
	// quotes is whether x509's or crypto/tls's own text of the failure holds CertificateName.
	quotes bool
	// finds reports whether an error still finds the failure's x509 or tls type (errors.As).
	finds func(err error) bool
}

// phraseCases are one failure for each phrase of tlsErrorWords, each with the words spelled out so
// the rows also build in a tree from before them: a certificate valid for another name alone
// (hostname) and one sent with a forged authority (unknown-authority), both from a stand-in; a
// certificate crypto/tls could not verify for a reason that is no x509 type (unverified), built as
// the macOS verifier words one, which names the certificate; a record that is not TLS (record); a
// certificate a trusted authority's name constraint does not permit (name-not-permitted), from a
// stand-in; and, built, x509's CertificateInvalidError for each other reason x509 names
// (crypto/x509's InvalidReason, go1.27.3) and for a reason it does not (unknown-reason). Each
// built CertificateInvalidError holds a certificate named CertificateName, and a Detail that
// quotes that name, as x509 quotes the name a constraint does not permit; x509's text holds Detail
// for five reasons alone (quotes).
func phraseCases() []phraseCase {
	named := &x509.Certificate{Subject: pkix.Name{CommonName: redacttest.CertificateName}, DNSNames: []string{redacttest.CertificateName}}
	hasReason := func(reason x509.InvalidReason) func(error) bool {
		return func(err error) bool {
			var found x509.CertificateInvalidError
			return errors.As(err, &found) && found.Reason == reason
		}
	}
	invalid := func(name string, reason x509.InvalidReason, words string, quotes bool) phraseCase {
		return phraseCase{name, builtFailure(&tls.CertificateVerificationError{Err: x509.CertificateInvalidError{
			Cert: named, Reason: reason, Detail: `DNS name "` + redacttest.CertificateName + `"`,
		}}), "tls: failed to verify certificate: " + words, quotes, hasReason(reason)}
	}
	const detailNotPrinted = " (x509's detail is not printed: it can quote the certificate, which the answering party chose)"
	return []phraseCase{
		{"hostname", handshakeFailure(redacttest.WrongHost, true), wantHostnameWords, true,
			func(err error) bool { return errors.As(err, new(x509.HostnameError)) }},
		{"unknown-authority", handshakeFailure(redacttest.ForgedIssuer, false), wantAuthorityWords, true,
			func(err error) bool { return errors.As(err, new(x509.UnknownAuthorityError)) }},
		{"unverified", builtFailure(&tls.CertificateVerificationError{
			Err: fmt.Errorf("x509: %s", "“"+redacttest.CertificateName+"” certificate is not standards compliant"),
		}), "tls: failed to verify certificate", true,
			func(err error) bool { return errors.As(err, new(*tls.CertificateVerificationError)) }},
		{"record", builtFailure(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake", RecordHeader: [5]byte{'n', 'o', 't', '-', 'a'}}),
			wantRecordWords, false, func(err error) bool { return errors.As(err, new(tls.RecordHeaderError)) }},
		{"name-not-permitted", handshakeFailure(redacttest.NameNotPermitted, true), wantNameNotPermitWords, true,
			hasReason(x509.CANotAuthorizedForThisName)},
		invalid("not-authorized-to-sign", x509.NotAuthorizedToSign, "x509: certificate is not authorized to sign other certificates", false),
		invalid("expired", x509.Expired, "x509: certificate has expired or is not yet valid"+detailNotPrinted, true),
		invalid("too-many-intermediates", x509.TooManyIntermediates, "x509: too many intermediates for path length constraint", false),
		invalid("incompatible-usage", x509.IncompatibleUsage, "x509: certificate specifies an incompatible key usage", false),
		invalid("name-mismatch", x509.NameMismatch, "x509: issuer name does not match subject from issuing certificate", false),
		invalid("no-san", x509.NameConstraintsWithoutSANs, "x509: issuer has name constraints but leaf doesn't have a SAN extension", false),
		invalid("unconstrained-name", x509.UnconstrainedName, "x509: issuer has name constraints but leaf contains unknown or unconstrained name (the name is not printed: the answering party chose it)", true),
		invalid("too-many-constraints", x509.TooManyConstraints, "x509: checking the certificate's names against its issuers' name constraints takes more comparisons than the limit allows", false),
		invalid("ext-key-usage", x509.CANotAuthorizedForExtKeyUsage, "x509: a root or intermediate certificate is not authorized for an extended key usage"+detailNotPrinted, true),
		invalid("no-valid-chains", x509.NoValidChains, "x509: no valid chains built"+detailNotPrinted, true),
		invalid("unknown-reason", x509.InvalidReason(99), "x509: certificate is not valid, for a reason this client has no words for (x509's text is not printed: it can quote the certificate, which the answering party chose)", false),
	}
}

// TestEachCertificateFailurePrintsItsOwnPhrase: whatever answers at a configured address answers
// the TLS handshake with a certificate whose names it chose, and for most failures x509's text
// quotes them (each case's twin: the failure's own text holds the name, or for a failure whose
// text is fixed words alone, does not). Cause, beside a clean address and a request under /v1/ of
// it, and Unanswered each print the failure by its own phrase, one fixed text for each type and
// for each reason of a CertificateInvalidError, never the failure's own text, so no run of the
// name; and errors.Is finds the failure under what each prints, and errors.As its x509 or tls type
// (and its reason). Captures redact.certificate-words/<case>, Cause's text and then Unanswered's.
func TestEachCertificateFailurePrintsItsOwnPhrase(t *testing.T) {
	for _, c := range phraseCases() {
		t.Run(c.name, func(t *testing.T) {
			base, request, err := c.fail(t)
			var failed *url.Error
			if !errors.As(err, &failed) {
				t.Fatalf("the request's error holds no *url.Error: %#v", err)
			}
			if quotes := strings.Contains(failed.Err.Error(), redacttest.CertificateName); quotes != c.quotes {
				t.Fatalf("the failure's own text quotes the certificate's name: %v, want %v: %q", quotes, c.quotes, failed.Err)
			}
			cause, unanswered := Cause(err, base), Unanswered(err, request, "")
			redacttest.RequireNoPiece(t, "redact.certificate-words/"+c.name, cause.Error()+"\n"+unanswered.Error(), redacttest.CertificateName)
			if cause.Error() != c.words {
				t.Errorf("Cause = %q, want %q", cause, c.words)
			}
			if want := `Get "` + request.URL.String() + `": ` + c.words; unanswered.Error() != want {
				t.Errorf("Unanswered = %q, want %q", unanswered, want)
			}
			for _, printed := range []error{cause, unanswered} {
				if !errors.Is(printed, failed.Err) || !c.finds(printed) {
					t.Errorf("%#v no longer finds the failure under it (errors.Is) or its type (errors.As)", printed)
				}
			}
			if !errors.Is(unanswered, err) {
				t.Errorf("Unanswered = %#v, which no longer finds net/http's error", unanswered)
			}
		})
	}
}
