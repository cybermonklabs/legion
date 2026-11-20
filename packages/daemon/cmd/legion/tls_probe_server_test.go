package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// startTLSServer starts the probe's own server in a state directory of the test's, stopped when
// the test ends.
func startTLSServer(t *testing.T) (*tlsServer, string) {
	t.Helper()
	stateDir := t.TempDir()
	server, err := newTLSServer(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return server, stateDir
}

// tlsCertPool is the certificates of the PEM file at path.
func tlsCertPool(t *testing.T, path string) *x509.CertPool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		t.Fatalf("%s holds no certificate", path)
	}
	return pool
}

// tlsPEMCertificates parses every certificate of the PEM bytes data.
func tlsPEMCertificates(t *testing.T, data []byte) []*x509.Certificate {
	t.Helper()
	var certs []*x509.Certificate
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		certs = append(certs, cert)
	}
	return certs
}

// The server's fixture listener serves its three sites on 127.0.0.1 at a port the kernel chose,
// each under its own host name, as a proxy's CONNECT target: each chain verifies with its fixture
// root and the site's host, and the answer is the page that host's public site gave (the
// control's: a page at / and 404 elsewhere; the compromised sites': 200 text for GET, 405 for
// POST). Each handshake counts for its site. A client that trusts only the system's roots refuses
// every chain.
func TestTLSServerServesEachChainUnderItsHostThroughAProxyConnect(t *testing.T) {
	server, _ := startTLSServer(t)
	sites := server.sites()[:len(servedTLSSiteNames)]
	if names := []string{sites[0].name, sites[1].name, sites[2].name}; !slices.Equal(names, servedTLSSiteNames) {
		t.Fatalf("server sites = %q, want %q", names, servedTLSSiteNames)
	}
	for _, site := range sites {
		if host, port, err := net.SplitHostPort(site.via); err != nil || host != "127.0.0.1" || port == "0" {
			t.Fatalf("%s is served at %q, want 127.0.0.1 at the port the kernel chose", site.name, site.via)
		}
		if site.control != (site.name == "control") || (site.trust != "") != site.control || (site.control && site.trust != site.root) {
			t.Errorf("%s = %+v, want the control alone with its own root as its trust file", site.name, site)
		}
	}
	proxy, _ := url.Parse("http://" + sites[0].via)
	for _, site := range sites {
		client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxy),
			TLSClientConfig: &tls.Config{RootCAs: tlsCertPool(t, site.root)},
		}}
		before := site.seen.Load()
		response, err := client.Get(site.url)
		if err != nil {
			t.Fatalf("GET %s through the server with its fixture root: %v", site.url, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		want := "Your client successfully connected to the site. That's bad!\n"
		if site.control {
			want = "<!doctype html><html><head><title>Legion TLS probe control</title></head><body><p>Legion TLS probe control.</p></body></html>\n"
		}
		if response.StatusCode != http.StatusOK || string(body) != want {
			t.Errorf("GET %s = %d %q, want 200 %q", site.url, response.StatusCode, body, want)
		}
		if got := site.seen.Load(); got <= before {
			t.Errorf("%s handshakes counted %d after a GET, want more than %d", site.name, got, before)
		}
		path, method, status := "/legion-tls-probe?go-get=1", http.MethodGet, http.StatusNotFound
		if !site.control {
			path, method, status = "/", http.MethodPost, http.StatusMethodNotAllowed
		}
		request, _ := http.NewRequest(method, strings.TrimSuffix(site.url, "/")+path, nil)
		if response, err := client.Do(request); err != nil || response.StatusCode != status {
			t.Errorf("%s %s%s = %v, %v; want %d", method, site.url, path, response, err, status)
		} else {
			response.Body.Close()
		}

		systemOnly := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
		var unknown x509.UnknownAuthorityError
		if _, err := systemOnly.Get(site.url); !errors.As(err, &unknown) {
			t.Errorf("GET %s trusting the system's roots = %v, want an unknown authority", site.url, err)
		}
	}
}

// redis-cli and socat have no proxy: they connect to the server itself and name the site's host
// in their TLS server name, which picks its chain, the same chain a CONNECT to that host gets.
func TestTLSServerServesEachChainByItsServerName(t *testing.T) {
	server, _ := startTLSServer(t)
	for _, site := range server.sites()[:len(servedTLSSiteNames)] {
		host := tlsSiteHosts[site.name]
		before := site.seen.Load()
		conn, err := tls.Dial("tcp", site.via, &tls.Config{ServerName: host, RootCAs: tlsCertPool(t, site.root)})
		if err != nil {
			t.Fatalf("TLS to %s naming %s with its fixture root: %v", site.via, host, err)
		}
		leaf := conn.ConnectionState().PeerCertificates[0]
		conn.Close()
		if !slices.Equal(leaf.DNSNames, []string{host}) || leaf.Subject.CommonName != host {
			t.Errorf("%s's leaf names %q (%s), want %s alone", site.name, leaf.DNSNames, leaf.Subject.CommonName, host)
		}
		if got := site.seen.Load(); got != before+1 {
			t.Errorf("%s handshakes counted %d after one, want %d", site.name, got, before+1)
		}
	}
	if _, err := tls.Dial("tcp", server.sites()[0].via, &tls.Config{ServerName: "example.com", InsecureSkipVerify: true}); err == nil {
		t.Error("TLS naming a host the server does not answer for succeeded, want a refused handshake")
	}
}

// The vendored listener serves each public site's own chain, byte for byte as vendored, under the
// site's host name, at a port of its own, and fails every handshake at the server's signature,
// since its key is the server's own: a client that gets past the chain (here by skipping its check,
// since Go 1.27.3 refuses these chains even against their roots) gets no further, and one that
// trusts no root refuses the chain itself. Each handshake counts for its site.
func TestTLSServerServesTheVendoredChainsItCannotSignFor(t *testing.T) {
	server, _ := startTLSServer(t)
	sites := server.sites()
	fixtures, vendored := sites[0], sites[3:]
	for index, site := range vendored {
		public := publicTLSSites[index]
		if site.name != "vendored "+strings.TrimPrefix(public.name, "public ") || !site.vendored || site.url != public.url || site.root != public.root || site.via == fixtures.via {
			t.Fatalf("vendored site %d = %+v, want the public site %s's URL and root at a listener of its own", index, site, public.name)
		}
		file := map[string]string{"vendored emSign": "emsign", "vendored AAA": "aaa"}[site.name]
		vendoredPEM, err := tlsVendoredChains.ReadFile("tls-fixtures/vendored-" + file + "-chain.pem")
		if err != nil {
			t.Fatal(err)
		}
		want := tlsPEMCertificates(t, vendoredPEM)
		host := tlsSiteHosts[site.name]
		var served []*x509.Certificate
		before := site.seen.Load()
		// The chain is read, not trusted: the check here is the server's signature.
		_, err = tls.Dial("tcp", site.via, &tls.Config{ServerName: host, InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error {
			served = state.PeerCertificates
			return nil
		}})
		if err == nil || !strings.Contains(err.Error(), "invalid signature by the server certificate") {
			t.Errorf("TLS to the vendored %s past its chain = %v, want the server's signature refused", host, err)
		}
		if len(served) != len(want) {
			t.Fatalf("the vendored %s served %d certificates, want the %d vendored", host, len(served), len(want))
		}
		for position := range want {
			if !served[position].Equal(want[position]) {
				t.Errorf("the vendored %s served %q at %d, want the vendored %q", host, served[position].Subject, position, want[position].Subject)
			}
		}
		var unknown x509.UnknownAuthorityError
		if _, err := tls.Dial("tcp", site.via, &tls.Config{ServerName: host, RootCAs: x509.NewCertPool()}); !errors.As(err, &unknown) {
			t.Errorf("TLS to the vendored %s trusting no root = %v, want an unknown authority", host, err)
		}
		if got := site.seen.Load(); got != before+2 {
			t.Errorf("%s handshakes counted %d after two, want %d", site.name, got, before+2)
		}
	}
}

// The server proxies nothing but a CONNECT to one of its own hosts at port 443: a CONNECT to any
// other host or port, and a plain request through it (as Chromium's fetch of an issuer's AIA URL
// is), is answered 403 at once, so nothing a client fetches through it reaches the network.
func TestTLSServerRefusesEveryOtherProxyRequest(t *testing.T) {
	server, _ := startTLSServer(t)
	via := server.sites()[0].via
	for _, request := range []string{
		"CONNECT repository.emsign.com:443 HTTP/1.1\r\nHost: repository.emsign.com:443\r\n\r\n",
		"CONNECT " + tlsEmSignHost + ":8443 HTTP/1.1\r\nHost: " + tlsEmSignHost + ":8443\r\n\r\n",
		"GET http://repository.emsign.com/certs/emSignECCRootCAG3.crt HTTP/1.1\r\nHost: repository.emsign.com\r\n\r\n",
	} {
		conn, err := net.DialTimeout("tcp", via, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		if _, err := io.WriteString(conn, request); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		conn.Close()
		if err != nil || response.StatusCode != http.StatusForbidden {
			t.Errorf("%q through the server = %v, %v; want 403", strings.SplitN(request, "\r\n", 2)[0], response, err)
		}
	}
}

// Close stops the server: its port takes no connection after it.
func TestTLSServerCloseStopsIt(t *testing.T) {
	server, err := newTLSServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	via := server.sites()[0].via
	server.Close()
	if conn, err := net.DialTimeout("tcp", via, 5*time.Second); err == nil {
		conn.Close()
		t.Fatalf("a connection to %s after Close succeeded, want it refused", via)
	}
	server.Close()
}

// Each fixture root is named as the CA-published root it stands in for, byte for byte, with that
// root's subject key identifier and a key of its own; each chain verifies with its fixture root
// for the public site's host and fails with the CA-published root, as a store that holds that root
// fails it; the chains carry the public sites' certificate names and depths. The control's chain
// verifies with the control's root alone. Every certificate is made for this run, and is valid for
// a day at most.
func TestTLSFixturesAreNamedAsTheDistrustedRoots(t *testing.T) {
	server, _ := startTLSServer(t)
	sites := server.sites()
	for index, fixture := range []struct {
		published string
		names     []string
	}{
		{"emsign-ecc-root-ca-g3.pem", []string{tlsEmSignHost, "EM DV TLS CA - G3B", "emSign Root TLS CA - G3"}},
		{"aaa-certificate-services.pem", []string{tlsAAAHost, "EM DV TLS CA - G3B"}},
	} {
		site := sites[index]
		publishedPEM, err := os.ReadFile(filepath.Join("..", "..", "docker", "tls-roots", fixture.published))
		if err != nil {
			t.Fatal(err)
		}
		published := tlsPEMCertificates(t, publishedPEM)[0]
		rootPEM, err := os.ReadFile(site.root)
		if err != nil {
			t.Fatal(err)
		}
		root := tlsPEMCertificates(t, rootPEM)[0]
		if string(root.RawSubject) != string(published.RawSubject) || string(root.SubjectKeyId) != string(published.SubjectKeyId) {
			t.Errorf("%s fixture root is %q with key identifier %X, want the CA-published root's %q and %X", site.name, root.Subject, root.SubjectKeyId, published.Subject, published.SubjectKeyId)
		}
		if string(root.RawSubjectPublicKeyInfo) == string(published.RawSubjectPublicKeyInfo) {
			t.Errorf("%s fixture root has the CA-published root's key, want a key of its own", site.name)
		}
		var chain []*x509.Certificate
		for _, der := range server.fixtures.hosts[tlsSiteHosts[site.name]].cert.Certificate {
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				t.Fatal(err)
			}
			chain = append(chain, cert)
		}
		var names []string
		intermediates := x509.NewCertPool()
		for position, cert := range chain {
			names = append(names, cert.Subject.CommonName)
			if position > 0 {
				intermediates.AddCert(cert)
			}
			if life := time.Until(cert.NotAfter); life <= 0 || life > 25*time.Hour {
				t.Errorf("%s fixture %q is valid until %s, want within a day of the run", site.name, cert.Subject.CommonName, cert.NotAfter)
			}
		}
		if !slices.Equal(names, fixture.names) {
			t.Errorf("%s fixture chain is %q, want the public site's %q", site.name, names, fixture.names)
		}
		if string(chain[len(chain)-1].RawIssuer) != string(published.RawSubject) || string(chain[len(chain)-1].AuthorityKeyId) != string(published.SubjectKeyId) {
			t.Errorf("%s fixture chain's last certificate names issuer %q, key %X, want the CA-published root's", site.name, chain[len(chain)-1].Issuer, chain[len(chain)-1].AuthorityKeyId)
		}
		host := tlsSiteHosts[site.name]
		for _, roots := range []struct {
			name  string
			pool  *x509.CertPool
			valid bool
		}{{"fixture root", tlsCertPool(t, site.root), true}, {"CA-published root", tlsCertPool(t, filepath.Join("..", "..", "docker", "tls-roots", fixture.published)), false}} {
			_, err := chain[0].Verify(x509.VerifyOptions{DNSName: host, Roots: roots.pool, Intermediates: intermediates})
			if (err == nil) != roots.valid {
				t.Errorf("%s fixture chain verified with the %s: %v, want valid %v", site.name, roots.name, err, roots.valid)
			}
		}
	}
	control := sites[2]
	leaf, err := x509.ParseCertificate(server.fixtures.hosts[tlsControlHost].cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: tlsControlHost, Roots: tlsCertPool(t, control.trust)}); err != nil {
		t.Errorf("the control's chain does not verify with the control's root: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: tlsControlHost, Roots: tlsCertPool(t, sites[0].root)}); err == nil {
		t.Error("the control's chain verifies with the emSign fixture root, want it refused")
	}
}
