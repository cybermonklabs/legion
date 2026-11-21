package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A listener whose ENVOY_OIDC_ACCOUNTS names no account — a list emptied by mistake — refuses to
// start, naming the setting, rather than reading it as unset and admitting every pod of the
// cluster; the same listener with the setting empty starts, as one with it unset does.
func TestTheListenerBinaryRefusesAnAccountListThatNamesNoAccount(t *testing.T) {
	issuer := oidctest.New(t)
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	for _, value := range []string{"   ", ", ,"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, binary)
		cmd.Env = []string{
			"PORT=0", "ENVOY_LISTEN_HOST=127.0.0.1", "ENVOY_MACHINE_ID=oidc-accounts-blank", "NATS_URLS=" + uri,
			"ENVOY_OIDC_ISSUER=" + issuer.URL(), "ENVOY_OIDC_AUDIENCE=" + listenerTestAudience, "ENVOY_OIDC_ACCOUNTS=" + value,
		}
		output, err := cmd.CombinedOutput()
		cancel()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), "ENVOY_OIDC_ACCOUNTS is set but names no account") {
			t.Fatalf("ENVOY_OIDC_ACCOUNTS=%q: the start ended with %v, want exit 1 naming the setting:\n%s", value, err, output)
		}
	}
	listener := startListenerProcess(t, binary, uri, "oidc-accounts-empty",
		"ENVOY_OIDC_ISSUER="+issuer.URL(), "ENVOY_OIDC_AUDIENCE="+listenerTestAudience, "ENVOY_OIDC_ACCOUNTS=")
	listener.waitHealthy(t)
}

// The settings reach the handler the listener binary serves, which the handler's own tests, each
// building its configuration by hand, cannot show: started with ENVOY_OIDC_ACCOUNTS beside a
// local issuer, the binary admits the listed subject's projected token and refuses another's, and
// its own /metrics counts each admission under its credential and route. Started with no list,
// the same binary admits both, as it did before the list existed.
func TestTheListenerBinaryHoldsProjectedTokensToItsListAndCountsThem(t *testing.T) {
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "listener-process-key")
	listed := issuer.Mint(t, key, issuer.Claims(listenerListedSubject, listenerTestAudience))
	unlisted := issuer.Mint(t, key, issuer.Claims(listenerUnlistedSubject, listenerTestAudience))
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	pair := []string{"ENVOY_OIDC_ISSUER=" + issuer.URL(), "ENVOY_OIDC_AUDIENCE=" + listenerTestAudience}

	for _, tc := range []struct {
		name, machineID string
		env             []string
		wantUnlisted    int
		wantOIDC        int
	}{
		{name: "held to ENVOY_OIDC_ACCOUNTS", machineID: "oidc-accounts-held", env: append([]string{"ENVOY_OIDC_ACCOUNTS=" + listenerListedSubject}, pair...),
			wantUnlisted: http.StatusUnauthorized, wantOIDC: 1},
		// Written with commas, the list holds the listed subject itself, not "<subject>,", so its
		// caller is admitted.
		{name: "held to a list written with commas", machineID: "oidc-accounts-commas",
			env:          append([]string{"ENVOY_OIDC_ACCOUNTS=" + listenerListedSubject + ", system:serviceaccount:example:other"}, pair...),
			wantUnlisted: http.StatusUnauthorized, wantOIDC: 1},
		{name: "with no list, as before", machineID: "oidc-accounts-unset", env: pair,
			wantUnlisted: http.StatusOK, wantOIDC: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := startListenerProcess(t, binary, uri, tc.machineID, tc.env...)
			listener.waitHealthy(t)
			base := "http://127.0.0.1:" + strconv.Itoa(listener.port)
			call := func(bearer string) int {
				t.Helper()
				request := mustRequest(t, http.MethodGet, base+"/v1/sessions")
				request.Header.Set("Authorization", "Bearer "+bearer)
				status, _ := do(t, request)
				return status
			}

			if status := call(listed); status != http.StatusOK {
				t.Fatalf("the listed subject's token: status %d, want 200\n%s", status, listener.Output.String())
			}
			if status := call(unlisted); status != tc.wantUnlisted {
				t.Fatalf("the unlisted subject's token: status %d, want %d\n%s", status, tc.wantUnlisted, listener.Output.String())
			}
			if status := call(listenerTestToken); status != http.StatusOK {
				t.Fatalf("the shared token: status %d, want 200", status)
			}

			_, metrics := do(t, mustRequest(t, http.MethodGet, base+"/metrics"))
			for _, want := range []string{
				`envoy_listener_requests_total{credential="oidc",path="/v1/sessions"} ` + strconv.Itoa(tc.wantOIDC),
				`envoy_listener_requests_total{credential="shared_token",path="/v1/sessions"} 1`,
			} {
				if !strings.Contains(metrics, want+"\n") {
					t.Fatalf("/metrics does not carry %s:\n%s", want, metrics)
				}
			}
		})
	}
}

// A listener that takes Google identities whose issuer does not answer discovery at start stays
// up and unready, as one whose cluster issuer does not
// (TestAListenerWhoseOIDCIssuerFailsAtStartServesOnceItAnswers). Each failed read logs a WARN that
// names Google's issuer and the wait before the next read. While the listener retries, /healthz
// answers 503 "starting" and /v1 answers 503 "service starting", to the shared token and to a
// listed account's Google identity token alike. Once the issuer answers, the next read of the
// retry schedule discovers it: /healthz answers 200 and /v1 serves both credentials. The binary is
// the production one, unchanged, so what runs is main's own wiring of Google's issuer, reaching a
// stand-in the way it would reach Google (googleIssuerStandIn).
func TestAListenerWhoseGoogleIssuerFailsAtStartServesOnceItAnswers(t *testing.T) {
	google := startGoogleIssuerStandIn(t)
	googleToken := google.token(t, listenerGoogleAccount)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), testnats.URL(t), "google-issuer-start",
		append(google.env(), "ENVOY_OIDC_GOOGLE_AUDIENCE="+listenerGoogleAudience, "ENVOY_OIDC_GOOGLE_ACCOUNTS="+listenerGoogleAccount)...)
	// failedRead is the WARN of a discovery read of Google's issuer that reached the stand-in and
	// that the stand-in failed. It names the wait before the next read.
	failedRead := func(record map[string]any) bool {
		wait, _ := record["retry_in"].(string)
		reason, _ := record["error"].(string)
		return record["issuer"] == oidc.GoogleIssuer && wait != "" && strings.Contains(reason, googleStandInUnavailable)
	}
	for deadline := time.Now().Add(30 * time.Second); countRecords(listener, "listener API oidc discovery failed; retrying", failedRead) == 0; time.Sleep(20 * time.Millisecond) {
		select {
		case <-listener.Exited:
			// Exited closes only once the output is whole, so this read is final.
			output := listener.Output.String()
			t.Fatalf("the listener exited while Google's discovery failed, with %s, after the stand-in failed %d discovery reads; its last line was %q:\n%s",
				listener.Cmd.ProcessState, google.failedReads(), lastOutputLine(output), output)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener logged no failed discovery of %s with a retry_in and the stand-in's 503 within 30s; the stand-in failed %d discovery reads and the proxy was asked for %v:\n%s",
				oidc.GoogleIssuer, google.failedReads(), google.connects(), listener.Output.String())
		}
	}
	listener.WaitForOutput(t, "envoy-listener ready (NATS connected)")

	v1 := func(bearer string) (int, string) {
		t.Helper()
		request := mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/v1/sessions")
		request.Header.Set("Authorization", "Bearer "+bearer)
		return do(t, request)
	}
	bearers := []struct{ name, token string }{
		{"the shared token", listenerTestToken},
		{"a listed account's Google identity token", googleToken},
	}
	if status, body := callListener(t, listener.port, http.MethodGet, "/healthz", ""); status != http.StatusServiceUnavailable || !strings.Contains(body, `"status":"starting"`) {
		t.Fatalf("/healthz while Google's discovery fails = %d %s, want 503 starting", status, body)
	}
	for _, bearer := range bearers {
		if status, body := v1(bearer.token); status != http.StatusServiceUnavailable || !strings.Contains(body, "service starting") {
			t.Fatalf("/v1 with %s while Google's discovery fails = %d %s, want 503 service starting", bearer.name, status, body)
		}
	}
	select {
	case <-listener.Exited:
		t.Fatalf("the listener exited while Google's discovery failed:\n%s", listener.Output.String())
	default:
	}

	// The stand-in failed `failed` reads before answer, the last of them before this call, and that
	// read logged the wait before the next one: oidcDiscoveryFirstRetry after the first failure,
	// doubled after each later one up to oidcDiscoveryMaxRetry (apiVerifier.discover). So the next
	// read, which the stand-in answers, starts within that wait of this call. readSlack is for that
	// read and for the /healthz poll after it: a loopback CONNECT, a TLS handshake and one small
	// document take milliseconds on an idle machine, and this bound gives them seconds on a loaded
	// one.
	const readSlack = 10 * time.Second
	failed := google.answer()
	wait := oidcDiscoveryFirstRetry
	for range failed - 1 {
		wait = nextDiscoveryRetry(wait)
	}
	bound := wait + readSlack
	for deadline := time.Now().Add(bound); ; time.Sleep(100 * time.Millisecond) {
		status, body := callListener(t, listener.port, http.MethodGet, "/healthz", "")
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/healthz %s after the issuer answered = %d %s, want 200: the stand-in failed %d discovery reads, so the next read was due within %s:\n%s",
				bound, status, body, failed, wait, listener.Output.String())
		}
	}
	listener.WaitForOutput(t, "listener API oidc discovery succeeded")
	if succeeded := countRecords(listener, "listener API oidc discovery succeeded", func(record map[string]any) bool {
		return record["issuer"] == oidc.GoogleIssuer
	}); succeeded != 1 {
		t.Fatalf("the listener logged %d successful discoveries of %s, want 1:\n%s", succeeded, oidc.GoogleIssuer, listener.Output.String())
	}
	if warned := countRecords(listener, "listener API oidc discovery failed; retrying", failedRead); warned != failed {
		t.Fatalf("the listener logged %d failed discoveries of %s, want one for each of the %d reads the stand-in failed:\n%s",
			warned, oidc.GoogleIssuer, failed, listener.Output.String())
	}
	listener.waitHealthy(t)
	for _, bearer := range bearers {
		if status, body := v1(bearer.token); status != http.StatusOK {
			t.Fatalf("/v1 with %s once the issuer answers = %d %s, want 200:\n%s", bearer.name, status, body, listener.Output.String())
		}
	}
}

const (
	// googleIssuerName is the host of Google's issuer (oidc.GoogleIssuer), and googleIssuerHost the
	// address a listener's HTTPS client asks a proxy to reach it at.
	googleIssuerName = "accounts.google.com"
	googleIssuerHost = googleIssuerName + ":443"
	// googleStandInUnavailable is the body the stand-in fails a discovery read with. The listener's
	// WARN for that read carries it in its error.
	googleStandInUnavailable = "the stand-in for Google's issuer is not answering discovery"
)

// googleIssuerStandIn is Google's issuer as a listener binary under test reaches it. The binary
// holds Google's issuer as a constant (oidc.GoogleIssuer) and has no setting that points it at
// another address, so the test reaches the stand-in through the two settings the network uses.
// The listener's HTTPS client is Go's default transport, which sends a request for
// https://accounts.google.com through the proxy that HTTPS_PROXY names, and verifies the answer's
// certificate against the roots that SSL_CERT_FILE names. This proxy tunnels accounts.google.com:443
// and refuses every other host. It tunnels to a TLS server whose certificate for that name an
// authority of this test signed, the authority that SSL_CERT_FILE names. The server answers Google's
// discovery document, which names Google as its issuer and a key set on the same host: the keys of
// issuer, which mints the Google identity tokens. Until answer is called, the server fails every
// discovery read with 503.
type googleIssuerStandIn struct {
	issuer   *oidctest.Issuer
	key      *oidctest.Key
	proxyURL string
	caFile   string

	mu sync.Mutex
	// answering is whether discovery answers, failed is how many reads it failed before that, and
	// asked is every host the proxy was asked to reach, oldest first.
	answering bool
	failed    int
	asked     []string
}

// startGoogleIssuerStandIn starts the stand-in's TLS server, failing discovery, and the proxy in
// front of it, and stops both when t ends.
func startGoogleIssuerStandIn(t *testing.T) *googleIssuerStandIn {
	t.Helper()
	g := &googleIssuerStandIn{issuer: oidctest.New(t)}
	g.key = g.issuer.PublishKey(t, "google-signing-key")
	keys, err := url.Parse(g.issuer.URL())
	if err != nil {
		t.Fatalf("parse the key issuer's URL %q: %v", g.issuer.URL(), err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", g.serveDiscovery)
	// The key set is issuer's own: this host serves it at /keys, as issuer serves it.
	mux.Handle("GET /keys", httputil.NewSingleHostReverseProxy(keys))
	authority, leaf := googleIssuerCertificate(t)
	g.caFile = filepath.Join(t.TempDir(), "google-issuer-authority.pem")
	if err := os.WriteFile(g.caFile, authority, 0o600); err != nil {
		t.Fatalf("write the test authority's certificate: %v", err)
	}
	server := httptest.NewUnstartedServer(mux)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	server.StartTLS()
	t.Cleanup(server.Close)
	g.proxyURL = "http://" + g.startProxy(t, server.Listener.Addr().String())
	return g
}

// env is what a listener is started with to reach the stand-in in Google's place.
func (g *googleIssuerStandIn) env() []string {
	return []string{"HTTPS_PROXY=" + g.proxyURL, "SSL_CERT_FILE=" + g.caFile}
}

// token mints a Google identity token as a metadata server mints one with format=full for
// listenerGoogleAudience: Google's issuer, a numeric subject, and account with email_verified.
func (g *googleIssuerStandIn) token(t *testing.T, account string) string {
	t.Helper()
	claims := g.issuer.Claims("118000000000000000001", listenerGoogleAudience)
	claims["iss"], claims["email"], claims["email_verified"] = oidc.GoogleIssuer, account, true
	return g.issuer.Mint(t, g.key, claims)
}

// serveDiscovery fails the read with 503 until answer is called, and counts each read it fails.
// After that, it answers Google's discovery document.
func (g *googleIssuerStandIn) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	answering := g.answering
	if !answering {
		g.failed++
	}
	g.mu.Unlock()
	if !answering {
		http.Error(w, googleStandInUnavailable, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                oidc.GoogleIssuer,
		"jwks_uri":                              oidc.GoogleIssuer + "/keys",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

// answer makes discovery answer from now on, and returns how many reads it failed before. One
// lock holds both, so every read is either counted here or answered.
func (g *googleIssuerStandIn) answer() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answering = true
	return g.failed
}

// failedReads is how many discovery reads the stand-in has failed.
func (g *googleIssuerStandIn) failedReads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failed
}

// connects is every host the proxy was asked to reach, oldest first.
func (g *googleIssuerStandIn) connects() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.asked...)
}

// startProxy starts the proxy that HTTPS_PROXY names, which tunnels each CONNECT to Google's issuer
// to upstream, the stand-in's TLS server, and returns its address. It stops when t ends.
func (g *googleIssuerStandIn) startProxy(t *testing.T, upstream string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the stand-in's proxy: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go g.tunnel(client, upstream)
		}
	}()
	return listener.Addr().String()
}

// tunnel answers one CONNECT. For Google's issuer, it copies the bytes both ways until either side
// closes. It refuses a CONNECT to any other host, and closes a request that is not a CONNECT
// without an answer, so nothing the listener sends through the proxy leaves this machine.
func (g *googleIssuerStandIn) tunnel(client net.Conn, upstream string) {
	defer client.Close()
	buffered := bufio.NewReader(client)
	request, err := http.ReadRequest(buffered)
	if err != nil || request.Method != http.MethodConnect {
		return
	}
	g.mu.Lock()
	g.asked = append(g.asked, request.Host)
	g.mu.Unlock()
	if request.Host != googleIssuerHost {
		_, _ = io.WriteString(client, "HTTP/1.1 403 Forbidden\r\n\r\n")
		return
	}
	server, err := net.Dial("tcp", upstream)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer server.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// The buffered reader, not the connection: TLS bytes that the client sent with the CONNECT are
	// in the buffer, and a read of the connection would lose them.
	go func() {
		_, _ = io.Copy(server, buffered)
		if half, ok := server.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		}
	}()
	_, _ = io.Copy(client, server)
}

// googleIssuerCertificate makes a test authority and a certificate for accounts.google.com that it
// signs. It returns the authority's certificate as PEM, for the file SSL_CERT_FILE names, and the
// signed certificate with its key, for the stand-in's TLS server. So the listener's own TLS check
// of Google's name passes against the stand-in, and the listener skips no part of that check.
func googleIssuerCertificate(t *testing.T) (authorityPEM []byte, leaf tls.Certificate) {
	t.Helper()
	now := time.Now()
	authorityKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("the test authority's key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "listener test authority for Google's issuer"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	authorityDER, err := x509.CreateCertificate(rand.Reader, template, template, &authorityKey.PublicKey, authorityKey)
	if err != nil {
		t.Fatalf("the test authority's certificate: %v", err)
	}
	authority, err := x509.ParseCertificate(authorityDER)
	if err != nil {
		t.Fatalf("parse the test authority's certificate: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("the stand-in's key: %v", err)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: googleIssuerName},
		DNSNames:     []string{googleIssuerName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, authority, &leafKey.PublicKey, authorityKey)
	if err != nil {
		t.Fatalf("the stand-in's certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authorityDER}), tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}
