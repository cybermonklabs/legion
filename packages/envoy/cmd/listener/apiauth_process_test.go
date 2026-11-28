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
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// A listener whose ENVOY_OIDC_ACCOUNTS names no account — a list emptied by mistake — refuses to
// start, naming the setting, rather than reading it as unset and admitting every pod of the
// cluster; the same listener with the setting empty starts, as one with it unset does.
func TestTheListenerBinaryRefusesAnAccountListThatNamesNoAccount(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// A fresh listener pod whose issuer answers discovery and its key set at once accepts Dispatch's
// identity token from the moment it is ready: the real Dispatch client, holding only its projected
// token, makes its first call the moment the real binary's /healthz first answers 200, and that
// call is answered 200 on the identity token inside the client's own 5 s window, any
// identity_unchecked 503 it meets along the way tried again as the client does (Client.do). The
// row logs the time from that first 200 to the accepted call, which is what a Dispatch call
// made at a new pod's readiness waits: the listener reads its issuer in the background from its
// start (oidc.LazyVerifier.Run), and the key set at the first token.
func TestAFreshListenerAcceptsDispatchsIdentityTokenAtReadinessInsideItsWindow(t *testing.T) {
	t.Parallel()
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "listener-readiness-key")
	tokenFile := filepath.Join(t.TempDir(), "listener-account-token")
	if err := os.WriteFile(tokenFile, []byte(issuer.Mint(t, key, issuer.Claims(listenerListedSubject, listenerTestAudience))+"\n"), 0o600); err != nil {
		t.Fatalf("write the account token: %v", err)
	}
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), testnats.URL(t), "identity-at-readiness",
		"ENVOY_OIDC_ISSUER="+issuer.URL(), "ENVOY_OIDC_AUDIENCE="+listenerTestAudience)
	base := "http://127.0.0.1:" + strconv.Itoa(listener.port)

	var ready time.Time
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		response, err := http.Get(base + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				ready = time.Now()
				break
			}
		}
		select {
		case <-listener.Exited:
			t.Fatalf("the listener exited before it was ready:\n%s", listener.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener's /healthz never answered 200 within 60s:\n%s", listener.Output.String())
		}
	}
	client := dispatchenvoy.New(base, dispatchenvoy.WithAccountTokenFile(tokenFile))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := client.ListInterests(ctx)
	took := time.Since(ready)
	t.Logf("the first identity call at readiness was accepted %s after /healthz first answered 200", took.Round(time.Millisecond))
	if err != nil {
		t.Fatalf("Dispatch's first call at readiness = %v after %s, want it answered 200 on its identity token\n%s", err, took, listener.Output.String())
	}
	if window := 5 * time.Second; took >= window {
		t.Fatalf("Dispatch's first call at readiness took %s, want it inside the client's window of %s", took, window)
	}
	_, metrics := do(t, mustRequest(t, http.MethodGet, base+"/metrics"))
	if !strings.Contains(metrics, `envoy_listener_requests_total{credential="oidc",`) {
		t.Fatalf("/metrics counts no admission on the identity token:\n%s", metrics)
	}
}

// A listener that takes Google identities whose issuer does not answer discovery at start comes up
// ready and serves while it tries again, as one whose cluster issuer does not
// (TestAListenerWhoseOIDCIssuerFailsAtStartServesOnceItAnswers). It logs a failed discovery that
// names Google's issuer and carries the stand-in's refusal. While the issuer does not answer,
// /healthz answers 200 with "google" false and "oidc" null, /v1 serves the shared token, and a
// listed account's Google identity token is answered 503 with Retry-After: 1, never 401. Once the
// issuer answers, a read discovers it with no restart: the Google identity token is admitted,
// "google" reads true, and the background reads log the success once. The binary is the
// production one, unchanged, so what runs is main's own wiring of Google's issuer, reaching a
// stand-in the way it would reach Google (googleIssuerStandIn).
func TestAListenerWhoseGoogleIssuerFailsAtStartServesOnceItAnswers(t *testing.T) {
	t.Parallel()
	google := startGoogleIssuerStandIn(t)
	googleToken := google.token(t, listenerGoogleAccount)
	started := time.Now()
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), testnats.URL(t), "google-issuer-start",
		append(google.env(), "ENVOY_OIDC_GOOGLE_AUDIENCE="+listenerGoogleAudience, "ENVOY_OIDC_GOOGLE_ACCOUNTS="+listenerGoogleAccount)...)
	// failedReads counts the lines of a discovery read of Google's issuer that reached the stand-in
	// and that the stand-in failed: each names the issuer and carries the stand-in's body. The
	// listener logs one at most once a minute, however often it reads (oidc.LazyVerifier.Run).
	failedReads := func() int {
		return countLines(listener.Output.String(), discoveryFailedLine, "issuer="+oidc.GoogleIssuer, googleStandInUnavailable)
	}
	for deadline := time.Now().Add(30 * time.Second); failedReads() == 0; time.Sleep(20 * time.Millisecond) {
		select {
		case <-listener.Exited:
			// Exited closes only once the output is whole, so this read is final.
			output := listener.Output.String()
			t.Fatalf("the listener exited while Google's discovery failed, with %s, after the stand-in failed %d discovery reads; its last line was %q:\n%s",
				listener.Cmd.ProcessState, google.failedReads(), lastOutputLine(output), output)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener logged no failed discovery of %s carrying the stand-in's 503 within 30s; the stand-in failed %d discovery reads and the proxy was asked for %v:\n%s",
				oidc.GoogleIssuer, google.failedReads(), google.connects(), listener.Output.String())
		}
	}
	listener.waitHealthy(t)

	base := "http://127.0.0.1:" + strconv.Itoa(listener.port)
	call := func(bearer string) *http.Response {
		t.Helper()
		request := mustRequest(t, http.MethodGet, base+"/v1/sessions")
		request.Header.Set("Authorization", "Bearer "+bearer)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("GET /v1/sessions: %v", err)
		}
		_ = response.Body.Close()
		return response
	}
	wantGoogleHealth := func(read bool) {
		t.Helper()
		status, body := callListener(t, listener.port, http.MethodGet, "/healthz", "")
		var health listenerwire.Health
		if err := json.Unmarshal([]byte(body), &health); err != nil || status != http.StatusOK || health.Google == nil || *health.Google != read || !strings.Contains(body, `"oidc":null`) {
			t.Fatalf("/healthz = %d %s (%v), want 200 with google %t and oidc null", status, body, err, read)
		}
	}

	wantGoogleHealth(false)
	if response := call(listenerTestToken); response.StatusCode != http.StatusOK {
		t.Fatalf("/v1 with the shared token while Google's discovery fails = %d, want 200:\n%s", response.StatusCode, listener.Output.String())
	}
	if response := call(googleToken); response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "1" {
		t.Fatalf("/v1 with a listed account's Google identity token while Google's discovery fails = %d (Retry-After %q), want 503 with Retry-After: 1:\n%s",
			response.StatusCode, response.Header.Get("Retry-After"), listener.Output.String())
	}

	// Once the stand-in answers, the next read discovers Google's issuer: a token that finds no
	// read failed in the last few seconds starts one, and the background reads go on meanwhile.
	failed := google.answer()
	waitFor(t, "a listed account's Google identity token to be admitted once Google's issuer answers", func() bool {
		return call(googleToken).StatusCode == http.StatusOK
	})
	wantGoogleHealth(true)
	// The success line is the background reads' (oidc.LazyVerifier.Run): when a read on a token's
	// path discovered the issuer first, the background loop logs it when it next wakes, at most
	// 30 s (the loop's longest wait) after its last failed read, and then stops.
	discovered := func() int {
		return countLines(listener.Output.String(), "oidc: discovered the issuer", "issuer="+oidc.GoogleIssuer)
	}
	waitFor(t, "the listener to log that it discovered Google's issuer", func() bool { return discovered() > 0 })
	output := listener.Output.String()
	if succeeded := discovered(); succeeded != 1 {
		t.Fatalf("the listener logged %d successful discoveries of %s, want 1:\n%s", succeeded, oidc.GoogleIssuer, output)
	}
	if most := 1 + int(time.Since(started)/time.Minute); failedReads() > most {
		t.Fatalf("the listener logged %d failed discoveries of %s for the %d reads the stand-in failed in %s, want at most %d, one a minute:\n%s",
			failedReads(), oidc.GoogleIssuer, failed, time.Since(started).Round(time.Second), most, output)
	}
	select {
	case <-listener.Exited:
		t.Fatalf("the listener exited while it waited for Google's issuer:\n%s", listener.Output.String())
	default:
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
