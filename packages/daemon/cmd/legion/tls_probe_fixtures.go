package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// tlsVendoredChains are the public test sites' own chains, certificates only, which
// tls-fixtures/vendor.go fetched: the probe's server serves them, and builds its fixture chains in
// their image (newTLSFixture).
//
//go:embed tls-fixtures/vendored-*.pem
var tlsVendoredChains embed.FS

// tlsPublishedRoots are the CA-published roots the public test sites' chains reach, each in the
// file its public site names (tlsSite.root): records from repository.emsign.com/certs/emSignECCRootCAG3.crt
// (86:A1:EC:BA:08:9C:4A:8D:3B:BE:27:34:C6:12:BA:34:1D:81:3E:04:3C:F9:E8:A8:62:CD:5C:57:A3:6B:BE:6B)
// and crt.sectigo.com/AAACertificateServices.crt
// (D7:A7:A0:FB:5D:7E:27:31:D7:71:E9:48:4E:BC:DE:F7:1D:5F:0C:3E:0A:29:48:78:2B:C8:3E:E0:EA:69:9E:F4).
// They are embedded, and the probe's server writes them into its state directory
// (writeTLSPublishedRoots), so no image can lack them: a positive control that cannot read its root
// would mark the served copies and the public sites unasked and hold no client to a real chain.
//
//go:embed tls-fixtures/emsign-ecc-root-ca-g3.pem tls-fixtures/aaa-certificate-services.pem
var tlsPublishedRoots embed.FS

// writeTLSPublishedRoots writes each public site's CA-published root into stateDir, under the file
// name the site names (tlsSite.root), where the positive controls read it.
func writeTLSPublishedRoots(stateDir string) error {
	for _, site := range publicTLSSites {
		data, err := tlsPublishedRoots.ReadFile("tls-fixtures/" + site.root)
		if err != nil {
			return fmt.Errorf("the %s root: %w", site.name, err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, site.root), data, 0o644); err != nil {
			return fmt.Errorf("write the %s root: %w", site.name, err)
		}
	}
	return nil
}

// vendoredTLSChain is the vendored chain of the public site at host, leaf first.
func vendoredTLSChain(host string) ([]*x509.Certificate, error) {
	file := map[string]string{tlsEmSignHost: "emsign", tlsAAAHost: "aaa"}[host]
	data, err := tlsVendoredChains.ReadFile("tls-fixtures/vendored-" + file + "-chain.pem")
	if err != nil {
		return nil, fmt.Errorf("the vendored chain of %s: %w", host, err)
	}
	var chain []*x509.Certificate
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("the vendored chain of %s: %w", host, err)
		}
		chain = append(chain, cert)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("the vendored chain of %s holds no certificate", host)
	}
	return chain, nil
}

// tlsServedLeafWarning is how long before a served leaf's end the probe warns of it: once the
// leaf has expired, its served copy's positive control fails and every client is held to the
// public site in its place (probeVendoredTLSSites), where a run with no verdict passes, so the
// chains must be vendored again before then (tls-fixtures/vendor.go).
const tlsServedLeafWarning = 30 * 24 * time.Hour

// tlsVendorCommand vendors the public sites' chains again, from the repository's root.
const tlsVendorCommand = "cd packages/daemon/cmd/legion/tls-fixtures && go run vendor.go"

// tlsServedLeaf is the leaf of a chain the probe's server serves as a public site's own, by the
// served site's name.
type tlsServedLeaf struct {
	site string
	leaf *x509.Certificate
}

// tlsServedLeaves are the leaves of sites' vendored chains (vendoredTLSChain), by site, read from
// the chains the probe embeds; a site whose chain cannot be read has none.
func tlsServedLeaves(sites []tlsSite) []tlsServedLeaf {
	var leaves []tlsServedLeaf
	for _, site := range sites {
		if chain, err := vendoredTLSChain(tlsSiteURL(site).Hostname()); err == nil {
			leaves = append(leaves, tlsServedLeaf{site: site.name, leaf: chain[0]})
		}
	}
	return leaves
}

// tlsServedLeafWarnings are the lines that warn of each of leaves whose end is within
// tlsServedLeafWarning of now, or past: a `probe-image: WARNING:` line for the probe's log, and the
// same as a GitHub Actions warning command. The command makes an annotation only where the probe's
// stdout is a workflow step's own: inside the image build, BuildKit prefixes every line of a RUN
// step, so the runner reads it as a log line, and the worker image workflow warns of the same
// leaves in a step of its own ("Warn before a served TLS leaf expires", worker-image.yaml).
func tlsServedLeafWarnings(leaves []tlsServedLeaf, now time.Time) []string {
	var lines []string
	for _, served := range leaves {
		if served.leaf.NotAfter.Sub(now) > tlsServedLeafWarning {
			continue
		}
		text := fmt.Sprintf("the served %s chain's leaf, %s, is valid until %s, within %d days or past: vendor the chains again (%s) and commit what it writes, or from then every client is held to the public site in its place",
			served.site, served.leaf.Subject, served.leaf.NotAfter.UTC().Format(time.RFC3339), int(tlsServedLeafWarning.Hours()/24), tlsVendorCommand)
		lines = append(lines, "probe-image: WARNING: "+text, "::warning title=TLS probe::"+text)
	}
	return lines
}

// tlsFixture is a chain the probe's server serves with a key it holds: the chain, leaf first, with
// the leaf's key, and the root it reaches, PEM-encoded, which the chain does not carry but for the
// control's.
type tlsFixture struct {
	cert tls.Certificate
	root []byte
}

// newTLSFixture builds, for this one run of the probe, a chain in the image of vendored, a public
// site's chain leaf first, with keys of its own: each certificate has the subject, the subject key
// identifier, the names, the usages and the key type and size of the one it stands for, and the root
// above it the subject and the key identifier vendored's last certificate names as its issuer's,
// the CA-published root's that the image's trust stores distrust. So a store looks the fixture's
// issuers up by the names and key identifiers it looks the public site's up by: one that holds no
// root of that name refuses the fixture for an unknown issuer, as it refuses the public site, and
// one that holds the distrusted root finds it and fails the fixture's signature
// (tlsSignatureFailure). The keys are made at each run and never written, so no certificate can be
// issued under these roots once the probe has exited; every certificate is valid from an hour
// before the run to a day after it.
func newTLSFixture(vendored []*x509.Certificate) (tlsFixture, error) {
	now := time.Now()
	top := vendored[len(vendored)-1]
	rootKey, err := tlsKeyFor(top.SignatureAlgorithm, nil)
	if err != nil {
		return tlsFixture{}, err
	}
	issuer, err := tlsIssue(&x509.Certificate{
		RawSubject:            top.RawIssuer,
		SubjectKeyId:          top.AuthorityKeyId,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}, rootKey, nil, nil, now)
	if err != nil {
		return tlsFixture{}, err
	}
	fixture := tlsFixture{root: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Raw})}
	issuerKey := rootKey
	var chain [][]byte
	for index := len(vendored) - 1; index >= 0; index-- {
		model := vendored[index]
		key, err := tlsKeyFor(0, model.PublicKey)
		if err != nil {
			return tlsFixture{}, err
		}
		cert, err := tlsIssue(&x509.Certificate{
			RawSubject:            model.RawSubject,
			SubjectKeyId:          model.SubjectKeyId,
			DNSNames:              model.DNSNames,
			IsCA:                  model.IsCA,
			BasicConstraintsValid: model.BasicConstraintsValid,
			MaxPathLen:            model.MaxPathLen,
			MaxPathLenZero:        model.MaxPathLenZero,
			KeyUsage:              model.KeyUsage,
			ExtKeyUsage:           model.ExtKeyUsage,
		}, key, issuer, issuerKey, now)
		if err != nil {
			return tlsFixture{}, err
		}
		chain = append([][]byte{cert.Raw}, chain...)
		issuer, issuerKey = cert, key
		if index == 0 {
			fixture.cert = tls.Certificate{Certificate: chain, PrivateKey: key}
		}
	}
	return fixture, nil
}

// newTLSControlFixture builds the control's chain for one run: the leaf control.legion-tls-probe.test
// and the root "Legion TLS probe control root" that issued it, which no trust store holds, and which
// the chain carries because Chromium's --ignore-certificate-errors-spki-list, how the probe gives
// Chromium that root, matches only a certificate the server sends.
func newTLSControlFixture() (tlsFixture, error) {
	now := time.Now()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsFixture{}, err
	}
	root, err := tlsIssue(&x509.Certificate{
		Subject:               pkix.Name{Organization: []string{"Legion"}, CommonName: "Legion TLS probe control root"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}, rootKey, nil, nil, now)
	if err != nil {
		return tlsFixture{}, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsFixture{}, err
	}
	leaf, err := tlsIssue(&x509.Certificate{
		Subject:               pkix.Name{CommonName: tlsControlHost},
		DNSNames:              []string{tlsControlHost},
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, leafKey, root, rootKey, now)
	if err != nil {
		return tlsFixture{}, err
	}
	return tlsFixture{
		cert: tls.Certificate{Certificate: [][]byte{leaf.Raw, root.Raw}, PrivateKey: leafKey},
		root: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}),
	}, nil
}

// tlsKeyFor makes a key of public's type and size, or, where public is nil, of the type a
// certificate signed with algorithm was signed by: P-384 for ECDSA with SHA-384, P-256 for other
// ECDSA, and RSA 2048 for RSA, the sizes of the two CA-published roots.
func tlsKeyFor(algorithm x509.SignatureAlgorithm, public crypto.PublicKey) (crypto.Signer, error) {
	switch key := public.(type) {
	case *ecdsa.PublicKey:
		return ecdsa.GenerateKey(key.Curve, rand.Reader)
	case *rsa.PublicKey:
		return rsa.GenerateKey(rand.Reader, key.N.BitLen())
	case nil:
	default:
		return nil, fmt.Errorf("a key of type %T", key)
	}
	switch algorithm {
	case x509.ECDSAWithSHA384, x509.ECDSAWithSHA512:
		return ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case x509.ECDSAWithSHA256, x509.ECDSAWithSHA1:
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	default:
		return rsa.GenerateKey(rand.Reader, 2048)
	}
}

// tlsIssue signs template with key's public half under parent and parentKey, or self-signs it where
// parent is nil, valid from an hour before now to a day after it. The authority key identifier is
// the parent's subject key identifier (x509.CreateCertificate).
func tlsIssue(template *x509.Certificate, key crypto.Signer, parent *x509.Certificate, parentKey crypto.Signer, now time.Time) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	template.SerialNumber = serial
	template.NotBefore, template.NotAfter = now.Add(-time.Hour), now.Add(24*time.Hour)
	if parent == nil {
		parent, parentKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, key.Public(), parentKey)
	if err != nil {
		return nil, fmt.Errorf("issue the TLS probe's certificate %s: %w", template.Subject.CommonName, err)
	}
	return x509.ParseCertificate(der)
}
