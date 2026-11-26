// packages/envoy/internal/cipher/proxy/ca.go
// The proxy's certificate authority: the certificate a broker pod signs each bound host's leaf
// with, and the leaf itself, so the command's own TLS verification holds while the proxy reads
// the request. The certificate is the pod's intermediate, which the pod asks Certificate
// Authority Service for with a key it makes in memory (intermediate.go, BROKER_PROXY_CA_POOL),
// or the certificate and key the deployment mounts (BROKER_AIRLOCK_CA_CERT_FILE,
// BROKER_AIRLOCK_CA_KEY_FILE): an intermediate and its chain, or a self-signed root. A command
// trusts the root of that chain and nothing else about the proxy, which is why a certificate
// this authority could not sign a trusted leaf with is refused here rather than found out by the
// first command that dials a bound host.
package proxy

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// leafLifetime is how long one host's leaf lasts from its start to its end, leafBackdate how far
// before now it starts, and leafRenewBefore how close to its end a cached leaf may come before
// the host is signed again. A day or so: long enough that a host costs about one key and one
// signature a day whatever the load, short enough that a leaf which somehow left this process is
// worth nothing for long. The renewal is there because a pod does outlive a leaf - nothing in
// this repository rolls the broker within a day - and a pod answering a certificate that has
// expired fails every command's verification of every bound host until somebody restarts it. The
// backdate covers the seconds of clock skew between the pod and the command's own box, and the
// hour before the end covers that same skew at the other end, along with a call still running
// on the leaf it was dialled with. A leaf never ends after the certificate that signed it. Its
// start is not held to that certificate's: a command whose clock runs behind could find a new
// intermediate not yet valid, which newSignerHold covers after a renewal and the pool's 5-minute
// backdate covers always, a pod's first intermediate and the one after an ended one included.
const (
	leafLifetime    = 25 * time.Hour
	leafBackdate    = time.Minute
	leafRenewBefore = time.Hour
)

// newSignerHold is how long after a new intermediate's start the pod goes on signing with the
// one before it, while that one may still sign. A pool whose policy sets no backdate issues an
// intermediate that starts the second it is issued, so a command whose clock runs a few seconds
// behind the pool's would find every leaf under it not yet valid until its own clock caught up;
// leafBackdate covers that skew for the leaf, and this covers it for the intermediate, but only
// after a renewal: it needs a previous intermediate that may still sign, so a pod's first
// intermediate, and the one after an intermediate ended, rely on the pool's backdate alone. A
// policy that backdates (the deployment's sets backdateDuration 300s, as operate.md requires,
// and the ca stand does the same) issues one that is already older than this, so the hold never
// starts.
const newSignerHold = time.Minute

// leafCacheSize is how many hosts' leaves Authority keeps. There is a cap because the hosts
// are not the proxy's to count: a kind may bind a pattern ("*.example.com"), and a command
// that CONNECTs to a distinct resolvable subdomain each time would otherwise grow this map by
// a key and a certificate per name for as long as the pod lives. A thousand is far more hosts
// than a grant reaches and small enough to bound; past it the oldest leaf goes, and a host
// that comes back is signed again, which costs one key and one signature and changes nothing
// a client sees, since no leaf is ever held anywhere but here.
const leafCacheSize = 1024

// AuthorityOptions is what an Authority signs by beside its certificate.
type AuthorityOptions struct {
	// Clock is the time leaves are signed by and an intermediate is renewed by, and nil is the
	// system's. The local stack moves it (BROKER_PROXY_TEST_CLOCK_OFFSET_FILE, OffsetFileClock).
	Clock Clock
	// ReleaseRoots are the roots of BROKER_PROXY_RELEASE_ROOTS_FILE, which every release carries
	// beside the root of the authority's own chain.
	ReleaseRoots []*x509.Certificate
	// Hosts are the bound hosts, each an exact name or "*." before a domain. A certificate whose
	// chain does not let it sign a trusted leaf for each of them is refused, since every command
	// that dialled the host it leaves out would fail its verification.
	Hosts []string
	// Log is where each certificate the authority takes is written, one INFO line each, and
	// each failed request for an intermediate, one WARN line each; nil writes nowhere.
	Log *slog.Logger
}

// Authority is the proxy's certificate authority: the certificate it signs with, and one leaf
// per host, signed on that host's first bound connection, signed again as it nears its end or
// when a new certificate has been taken, and kept until the cap above evicts it. Every method
// is safe for concurrent use, since one map is shared by every connection and the renewal
// (Intermediates) replaces the certificate while they run.
type Authority struct {
	clock        Clock
	releaseRoots []*x509.Certificate
	hosts        []string
	log          *slog.Logger

	mu     sync.Mutex
	signer *signer
	// previous is the certificate signer replaced, which signs in signer's place for
	// newSignerHold after signer's start (signerAt).
	previous *signer
	// root is the root of the first certificate's chain. Every later one must chain to it,
	// since every release this process has made names it and a command trusts no other.
	root *x509.Certificate
	// released is ReleaseRoots' answer, made again only when root is set: root and then each
	// release root, as PEM.
	released string
	leaves   map[string]heldLeaf
	// minted is the hosts of leaves in the order they were first signed, oldest first, which
	// is the order the cap evicts in. A host signed again keeps the place it had: what the cap
	// is about is how many hosts this map holds, and a renewal adds no host.
	minted []string
}

// signer is one certificate the authority signs with: its key, and what the TLS answer carries
// after each leaf - the certificate and each authority between it and its root, and the root
// itself only where the certificate is that root.
type signer struct {
	cert *x509.Certificate
	key  crypto.Signer
	sent [][]byte
	// between are the authorities of sent past the certificate, which a command verifies the
	// leaf through.
	between []*x509.Certificate
}

// heldLeaf is one host's certificate, the moment it stops being one, and the certificate that
// signed it: read on every bound connection to that host, so the expiry is held beside the
// certificate rather than parsed out of it again on each handshake.
type heldLeaf struct {
	certificate *tls.Certificate
	notAfter    time.Time
	by          *signer
}

// errNoIntermediate is what a pod that has not yet had its first certificate request answered
// signs and readies with.
var errNoIntermediate = errors.New("the proxy holds no intermediate yet: its first certificate request has not been answered")

func newAuthority(o AuthorityOptions) *Authority {
	clock := o.Clock
	if clock == nil {
		clock = systemClock{}
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Authority{clock: clock, releaseRoots: o.ReleaseRoots, released: rootsPEM(nil, o.ReleaseRoots), hosts: o.Hosts, log: log, leaves: map[string]heldLeaf{}}
}

// NewAuthority is the mounted form: the PEM certificates of certFile, an authority and then each
// authority up to and including its root, or a self-signed root alone, and the PEM private key
// of keyFile. It refuses a file it cannot read or parse, a key that is not the certificate's
// (which tls.LoadX509KeyPair compares), a certificate that is not an authority or whose key usage
// does not allow it to sign certificates, a chain that does not reach a self-signed root, and a
// certificate that would sign no trusted leaf now for one of o.Hosts. Each refusal names the
// file, because the two are mounted from one Kubernetes Secret and an operator reads which half
// is wrong.
func NewAuthority(certFile, keyFile string, o AuthorityOptions) (*Authority, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("proxy certificate authority (%s, %s): %w", certFile, keyFile, err)
	}
	// LoadX509KeyPair hands back the first certificate parsed, as Leaf, beside the DER of every
	// certificate, so only the ones after the first are parsed here.
	certs := make([]*x509.Certificate, len(pair.Certificate))
	certs[0] = pair.Leaf
	for i, der := range pair.Certificate[1:] {
		if certs[i+1], err = x509.ParseCertificate(der); err != nil {
			return nil, fmt.Errorf("proxy certificate authority %s: certificate %d: %w", certFile, i+2, err)
		}
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("proxy certificate authority %s: the key is of a kind this proxy cannot sign with (%T)", keyFile, pair.PrivateKey)
	}
	s, root, err := chainOf(certs[0], key, certs[1:])
	if err != nil {
		return nil, fmt.Errorf("proxy certificate authority %s: %w", certFile, err)
	}
	a := newAuthority(o)
	if err := a.take(s, root); err != nil {
		return nil, fmt.Errorf("proxy certificate authority %s: %w", certFile, err)
	}
	return a, nil
}

// checkSigns refuses a certificate that is not an authority, and one whose key usage does not
// allow it to sign certificates. A certificate that declares key usages at all must declare
// that one: a command's verification of the chain reads it, so an authority without it signs
// leaves nobody accepts. A certificate that declares none (KeyUsage 0) constrains nothing and is
// left to the verifier that reads it.
func checkSigns(cert *x509.Certificate) error {
	if !cert.IsCA {
		return errors.New("not a certificate authority (no CA basic constraint), so nothing it signed would be trusted")
	}
	if cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return errors.New("its key usage does not allow signing certificates")
	}
	return nil
}

// chainOf is cert's signer with key, and the root its chain ends at: cert itself when it is a
// self-signed root, and otherwise the last of chain, which must be one. A certificate checkSigns
// refuses makes no signer.
func chainOf(cert *x509.Certificate, key crypto.Signer, chain []*x509.Certificate) (*signer, *x509.Certificate, error) {
	if err := checkSigns(cert); err != nil {
		return nil, nil, err
	}
	if selfSigned(cert) {
		if len(chain) > 0 {
			return nil, nil, fmt.Errorf("a self-signed root followed by %d more certificate(s), which nothing it signs chains through", len(chain))
		}
		return &signer{cert: cert, key: key, sent: [][]byte{cert.Raw}}, cert, nil
	}
	if len(chain) == 0 {
		return nil, nil, errors.New("an intermediate with no chain to its root: the certificates are the intermediate, then each authority up to and including its root")
	}
	root := chain[len(chain)-1]
	if !selfSigned(root) {
		return nil, nil, fmt.Errorf("the last certificate of its chain (%s) is not a self-signed root", root.Subject)
	}
	s := &signer{cert: cert, key: key, sent: [][]byte{cert.Raw}, between: chain[:len(chain)-1]}
	for _, c := range s.between {
		s.sent = append(s.sent, c.Raw)
	}
	return s, root, nil
}

// selfSigned reports whether c is its own issuer and signature.
func selfSigned(c *x509.Certificate) bool {
	return bytes.Equal(c.RawIssuer, c.RawSubject) && c.CheckSignatureFrom(c) == nil
}

// intermediateLifeSlack is how much longer than intermediateLifetime an intermediate may last
// from its start to its end: 5 minutes, for a pool that rounds its times. So the bound is 7 days
// and 5 minutes. A pool's backdating (its issuance policy's backdate_duration) keeps the
// requested lifetime, so it adds nothing to it.
const intermediateLifeSlack = 5 * time.Minute

// checkBounds holds the certificate a pod signs leaves with, unless it is the root itself, to what
// a pod's CSR asks for: path length 0, certificate-signing key usage, serverAuth as its one
// extended key usage, and a lifetime no longer than the requested week and the issuer's time
// slack. Its critical name constraints permit exactly the bound DNS names, permit no IP range,
// and exclude every IPv4 and IPv6 address.
//
// Name constraints apply to each name type separately, so permitted DNS names alone would not
// constrain IP addresses. A certificate wider than these bounds would make a leaked pod key an
// authority for other authorities, for longer, for names no profile binds, or for any IP address,
// under a root commands trust. `docs/site/src/content/docs/broker/concepts.md`, "The proxy's
// chain", records the client measurements and their common-name limits; this check enforces the
// bounds those measurements leave in place.
func checkBounds(cert *x509.Certificate, hosts []string) error {
	if !cert.BasicConstraintsValid || cert.MaxPathLen != 0 || !cert.MaxPathLenZero {
		return fmt.Errorf("its path length is %s, not 0, so it could sign other authorities", pathLength(cert))
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return errors.New("its key usage does not name signing certificates")
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || len(cert.UnknownExtKeyUsage) > 0 {
		return fmt.Errorf("its extended key usage is %s, not serverAuth alone, so it could sign certificates for other uses than a server's", extKeyUsages(cert))
	}
	if life := cert.NotAfter.Sub(cert.NotBefore); life > intermediateLifetime+intermediateLifeSlack {
		return fmt.Errorf("its life is %s, past the %s a pod asks for and %s of slack", life, intermediateLifetime, intermediateLifeSlack)
	}
	want, got := lowerNames(requestedNames(hosts)), lowerNames(cert.PermittedDNSDomains)
	if len(got) == 0 || !slices.Equal(got, want) {
		return fmt.Errorf("its name constraints permit [%s]; want exactly the bound hosts' names [%s]", strings.Join(got, " "), strings.Join(want, " "))
	}
	if len(cert.PermittedIPRanges) > 0 {
		return fmt.Errorf("its name constraints permit IP ranges %v; want none, so it signs for no address", cert.PermittedIPRanges)
	}
	switch v4, v6 := excludesEveryAddress(cert.ExcludedIPRanges); {
	case !v4 && !v6:
		return fmt.Errorf("its name constraints exclude the IP ranges %v; want them to say what a pod's CSR does, that excludes 0.0.0.0/0 and ::/0, so it signs for no address", cert.ExcludedIPRanges)
	case !v4:
		return errors.New("its name constraints do not exclude 0.0.0.0/0, so it could sign for any IPv4 address")
	case !v6:
		return errors.New("its name constraints do not exclude ::/0, so it could sign for any IPv6 address")
	}
	if !cert.PermittedDNSDomainsCritical {
		return errors.New("its name constraints are not marked critical, so a verifier that does not implement them would ignore them")
	}
	return nil
}

// excludesEveryAddress reports whether ranges hold all of IPv4 (0.0.0.0/0) and all of IPv6 (::/0).
// A constraint is matched only against an address of its own length, so each family needs its
// own range.
func excludesEveryAddress(ranges []*net.IPNet) (v4, v6 bool) {
	for _, r := range ranges {
		ones, bits := r.Mask.Size()
		if ones != 0 {
			continue
		}
		switch {
		case bits == 32 && len(r.IP) == net.IPv4len:
			v4 = true
		case bits == 128 && len(r.IP) == net.IPv6len:
			v6 = true
		}
	}
	return v4, v6
}

// extKeyUsages is cert's extended key usages as a sentence reads them, the ones the standard
// library does not name by their object identifiers.
func extKeyUsages(cert *x509.Certificate) string {
	names := make([]string, 0, len(cert.ExtKeyUsage)+len(cert.UnknownExtKeyUsage))
	for _, usage := range cert.ExtKeyUsage {
		names = append(names, usage.String())
	}
	for _, oid := range cert.UnknownExtKeyUsage {
		names = append(names, oid.String())
	}
	return "[" + strings.Join(names, " ") + "]"
}

// everyAddress is the exclusion a pod asks for and holds every intermediate to.
func everyAddress() []*net.IPNet {
	return []*net.IPNet{
		{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
		{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
	}
}

// pathLength is a certificate's path length constraint as a sentence reads it.
func pathLength(cert *x509.Certificate) string {
	switch {
	case !cert.BasicConstraintsValid:
		return "unset, with no basic constraints"
	case cert.MaxPathLen < 0 || cert.MaxPathLen == 0 && !cert.MaxPathLenZero:
		return "unconstrained"
	}
	return fmt.Sprint(cert.MaxPathLen)
}

// lowerNames is names in lower case, sorted, each once: DNS names compare without case.
func lowerNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, strings.ToLower(name))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// take makes s the certificate the authority signs with, once it holds that s chains to root
// now, that root is the root of every certificate taken before it, and that a leaf s signs for
// each bound host verifies under root alone, name constraints and all. Then it logs the line an
// operator reads each intermediate by: its serial number, its names and its end.
func (a *Authority) take(s *signer, root *x509.Certificate) error {
	now := a.clock.Now()
	a.mu.Lock()
	pinned := a.root
	a.mu.Unlock()
	if pinned != nil && !pinned.Equal(root) {
		return fmt.Errorf("it chains to another root (%s) than the one every release of this process names (%s)", root.Subject, pinned.Subject)
	}
	if !now.Before(s.cert.NotAfter) {
		return fmt.Errorf("its life ended at %s", s.cert.NotAfter.UTC().Format(time.RFC3339))
	}
	// What a command verifies a leaf by: the root alone, trusted, and every certificate the TLS
	// answer carries after the leaf, s's own first, as its intermediates.
	roots, between, sent := x509.NewCertPool(), x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(root)
	sent.AddCert(s.cert)
	for _, c := range s.between {
		between.AddCert(c)
		sent.AddCert(c)
	}
	if _, err := s.cert.Verify(x509.VerifyOptions{Roots: roots, Intermediates: between, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return fmt.Errorf("it does not chain to its root: %w", err)
	}
	if s.cert != root {
		if err := checkBounds(s.cert, a.hosts); err != nil {
			return err
		}
	}
	for _, host := range a.hosts {
		name := host
		if suffix, pattern := strings.CutPrefix(host, "*."); pattern {
			// A pattern is signed for each name under it, and one of them stands for all.
			name = "a." + suffix
		}
		leaf, _, err := s.sign(name, now)
		if err != nil {
			return err
		}
		parsed, err := x509.ParseCertificate(leaf.Certificate[0])
		if err != nil {
			return err
		}
		if _, err := parsed.Verify(x509.VerifyOptions{DNSName: name, Roots: roots, Intermediates: sent, CurrentTime: now}); err != nil {
			return fmt.Errorf("it signs no leaf a command trusts for %s: %w", host, err)
		}
	}
	a.mu.Lock()
	if a.root == nil {
		a.root = root
		a.released = rootsPEM(root, a.releaseRoots)
	}
	a.previous, a.signer = a.signer, s
	a.mu.Unlock()
	a.log.Info("proxy intermediate", "serial", fmt.Sprintf("%x", s.cert.SerialNumber),
		"names", strings.Join(s.cert.PermittedDNSDomains, " "),
		"not_before", s.cert.NotBefore.UTC().Format(time.RFC3339), "not_after", s.cert.NotAfter.UTC().Format(time.RFC3339),
		"self_signed", s.cert == root)
	return nil
}

// sign makes one leaf for host, from now, ending a leaf's life on or with s's own end.
func (s *signer) sign(host string, now time.Time) (*tls.Certificate, time.Time, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, time.Time{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, time.Time{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-leafBackdate),
		NotAfter:     s.leafEnd(now),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, s.cert, &key.PublicKey, s.key)
	if err != nil {
		return nil, time.Time{}, err
	}
	return &tls.Certificate{Certificate: append([][]byte{der}, s.sent...), PrivateKey: key}, template.NotAfter, nil
}

// leafEnd is when a leaf signed now ends: a leaf's life after its start, or s's own end if that
// comes first.
func (s *signer) leafEnd(now time.Time) time.Time {
	end := now.Add(-leafBackdate).Add(leafLifetime)
	if s.cert.NotAfter.Before(end) {
		return s.cert.NotAfter
	}
	return end
}

// Certificate is the certificate the authority signs with now, and nil before it has one.
func (a *Authority) Certificate() *x509.Certificate {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.signer == nil {
		return nil
	}
	return a.signer.cert
}

// ended says why s may no longer sign at now, and is nil while its life is not over.
func (s *signer) ended(now time.Time) error {
	if !now.Before(s.cert.NotAfter) {
		return fmt.Errorf("the proxy's intermediate ended at %s and no new one has arrived", s.cert.NotAfter.UTC().Format(time.RFC3339))
	}
	return nil
}

// signerAt is the certificate the authority signs with at now: the newest, unless it is less
// than newSignerHold old and the one before it may still sign. a.mu is held.
func (a *Authority) signerAt(now time.Time) *signer {
	s := a.signer
	if p := a.previous; p != nil && s != nil && now.Before(s.cert.NotBefore.Add(newSignerHold)) && p.ended(now) == nil {
		return p
	}
	return s
}

// Ready is nil while the authority holds a certificate whose life is not over, and says why not
// otherwise: a pod with nothing to sign with fails every bound host's handshake, so its
// readiness answer keeps traffic away from it, and a roll keeps the old pods until a new pod
// gets its intermediate.
func (a *Authority) Ready() error {
	now := a.clock.Now()
	a.mu.Lock()
	s := a.signerAt(now)
	a.mu.Unlock()
	if s == nil {
		return errNoIntermediate
	}
	return s.ended(now)
}

// ReleaseRoots is the PEM every release hands a command to trust the proxy by: the root of the
// authority's own chain, then each root of BROKER_PROXY_RELEASE_ROOTS_FILE, each once. A command
// gets the certificates and nothing more, whatever else the mounted file held - a PEM holding the
// certificate and its private key together is a file tls.LoadX509KeyPair accepts, and a release
// that answered the file's bytes would hand every command the key that signs for any host. Before
// a pool's first answer the pod knows no chain and carries the release roots alone, and a nil
// authority, a deployment with no proxy, carries nothing.
func (a *Authority) ReleaseRoots() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.released
}

// rootsPEM is root and then each of extra, each once, as PEM blocks one after another; a nil
// root is left out.
func rootsPEM(root *x509.Certificate, extra []*x509.Certificate) string {
	var out []byte
	var held []*x509.Certificate
	for _, c := range append([]*x509.Certificate{root}, extra...) {
		if c == nil || slices.ContainsFunc(held, c.Equal) {
			continue
		}
		held = append(held, c)
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return string(out)
}

// leaf is host's certificate, signed once and then answered from the map until it nears its end:
// a host the agent calls a thousand times costs one key and one signature a day. The chain it
// answers holds the signing certificate and each authority up to its root, so a command that
// trusts the root alone - which is every command the broker releases to - completes the chain
// without fetching anything. The map holds leafCacheSize hosts and the oldest goes to make room,
// which costs the host that comes back one key and one signature again.
//
// A cached leaf within leafRenewBefore of its NotAfter is signed again here rather than handed
// out, unless the leaf it would sign ends no later, as in the last hour of an intermediate: a
// pod that has served one host for longer than a leaf lasts would otherwise answer every
// handshake for that host with an expired certificate. A leaf an older certificate signed is
// signed again under the new one, so every handshake after a renewal shows the new chain, once
// newSignerHold has passed since the new one's start (signerAt). Once the certificate's own life
// is over there is nothing to sign with, and the handshake fails.
func (a *Authority) leaf(host string) (*tls.Certificate, error) {
	// Read before the lock every bound handshake shares: the local stack's clock reads a file.
	now := a.clock.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.signerAt(now)
	if s == nil {
		return nil, errNoIntermediate
	}
	if err := s.ended(now); err != nil {
		return nil, err
	}
	held, kept := a.leaves[host]
	if kept && held.by == s && (now.Add(leafRenewBefore).Before(held.notAfter) || !s.leafEnd(now).After(held.notAfter)) {
		return held.certificate, nil
	}
	c, notAfter, err := s.sign(host, now)
	if err != nil {
		return nil, err
	}
	if !kept {
		// One more host in the map, so the cap applies; a host signed again is the host that
		// was already there, and the order the cap evicts in does not change for it.
		if len(a.minted) >= leafCacheSize {
			delete(a.leaves, a.minted[0])
			a.minted = a.minted[1:]
		}
		a.minted = append(a.minted, host)
	}
	a.leaves[host] = heldLeaf{certificate: c, notAfter: notAfter, by: s}
	return c, nil
}
