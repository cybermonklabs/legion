package redacttest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// CertificateName is the name the certificates of a CertificateServer hold, as a DNS name and as
// the common name: words the answering party chose, which crypto/x509 quotes in its error. It
// holds Token, so a printed text that holds it fails RequireNone.
const CertificateName = Token + "-0123456789.example"

// A CertificateFault is the certificate a CertificateServer answers every TLS handshake with.
type CertificateFault int

const (
	// WrongHost is a self-signed certificate valid for CertificateName alone. x509 checks the
	// name before the chain, so it refuses the certificate for localhost whether the client
	// trusts it or not, and its error (x509.HostnameError) names every name the certificate is
	// valid for.
	WrongHost CertificateFault = iota
	// ForgedIssuer is a certificate valid for localhost that names CertificateName as its issuer,
	// sent with an authority certificate of that name whose key did not sign it. x509 refuses it as
	// signed by an unknown authority (x509.UnknownAuthorityError) and names the candidate authority
	// it tried.
	ForgedIssuer
	// NameNotPermitted is a certificate valid for CertificateName and for localhost, issued by an
	// authority named CertificateName whose name constraint permits neither. x509 finds localhost
	// among the names, builds the chain, then refuses the first name the authority does not
	// permit, CertificateName, and quotes it (x509.CertificateInvalidError, reason
	// CANotAuthorizedForThisName). Only a client that trusts the authority (trusting) gets that
	// far; one that trusts nothing of the stand-in fails on an unknown authority.
	NameNotPermitted
)

// CertificateServer is a TLS stand-in that answers every handshake with fault's certificates. It
// answers base, its address as https://localhost:<port>, and trusting, a client whose roots hold
// the stand-in's certificate named CertificateName. A client that trusts nothing of the stand-in
// (http.Client's default roots) fails the handshake the same way, but for NameNotPermitted.
func CertificateServer(t testing.TB, fault CertificateFault) (base string, trusting *http.Client) {
	t.Helper()
	authorityKey := certificateKey(t)
	authority := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: CertificateName},
		DNSNames:              []string{CertificateName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	if fault == NameNotPermitted {
		authority.PermittedDNSDomains = []string{"permitted.example"}
	}
	authorityDER := signed(t, authority, authority, authorityKey, authorityKey)
	answered := tls.Certificate{Certificate: [][]byte{authorityDER}, PrivateKey: authorityKey}
	if fault != WrongHost {
		leafKey := certificateKey(t)
		leaf := &x509.Certificate{
			SerialNumber: big.NewInt(2),
			Subject:      pkix.Name{CommonName: "localhost"},
			DNSNames:     []string{"localhost"},
			IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
			NotBefore:    authority.NotBefore,
			NotAfter:     authority.NotAfter,
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		// A forged issuer's leaf names the authority as its issuer, and its own key signs it. The
		// authority's own key signs a leaf that holds a name it does not permit, that name first.
		signer := leafKey
		if fault == NameNotPermitted {
			leaf.DNSNames = []string{CertificateName, "localhost"}
			signer = authorityKey
		}
		answered = tls.Certificate{Certificate: [][]byte{signed(t, leaf, authority, leafKey, signer), authorityDER}, PrivateKey: leafKey}
	}
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{answered}}
	server.StartTLS()
	t.Cleanup(server.Close)
	parsed, err := x509.ParseCertificate(authorityDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	trusting = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	t.Cleanup(trusting.CloseIdleConnections)
	_, port, _ := strings.Cut(strings.TrimPrefix(server.URL, "https://"), ":")
	return "https://localhost:" + port, trusting
}

// certificateKey is a new P-256 key.
func certificateKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// signed is template's certificate, issued in parent's name and signed by signer, in DER.
func signed(t testing.TB, template, parent *x509.Certificate, key, signer *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	return der
}
