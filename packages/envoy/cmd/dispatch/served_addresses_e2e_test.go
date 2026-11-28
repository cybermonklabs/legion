//go:build e2e

package main

// The live proof of INFRA-5491's checks c-l-served, c-l-record, c-l-proxy, the API half of
// c-l-refs and c-l-cli (the issue's Verification v16): each starts the real envoy-dispatch process
// on a Postgres database of its own, behind a TLS front door the test serves, signs a person in
// through packages/envoy/cmd/dev-oidc-issuer, checks broker tokens against the stub broker of
// internal/ciphertoken/ciphertokentest, and reads the process's own log. Each step of a check is
// one subtest, named after the step, so a result names the subtest it ran, and none is skipped.
//
// scripts/e2e/dispatch-served-addresses.sh builds the binaries and runs these tests; it says what
// each variable below names and how the controls run:
//
//   - e2eDispatchBinEnv, the envoy-dispatch every server but the base commit's runs (unset builds
//     this checkout's);
//   - e2eBaseBinEnv, the base commit's envoy-dispatch, which c-l-served (6) compares against;
//   - e2eIssuerBinEnv, dev-oidc-issuer (unset builds this checkout's);
//   - e2eDispatchCLIEnv, the bin/dispatch.ts c-l-cli runs (unset runs this checkout's).

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The variables the driver script hands these tests (the file's header says what each names).
const (
	e2eDispatchBinEnv = "SERVED_ADDRESSES_E2E_DISPATCH_BIN"
	e2eBaseBinEnv     = "SERVED_ADDRESSES_E2E_BASE_BIN"
	e2eIssuerBinEnv   = "SERVED_ADDRESSES_E2E_ISSUER_BIN"
	e2eDispatchCLIEnv = "SERVED_ADDRESSES_E2E_DISPATCH_CLI"
)

// The addresses of the checks' server: its canonical address (DISPATCH_SERVER_URL), its second
// served address (DISPATCH_SERVED_ORIGINS), its former address (DISPATCH_FORMER_ORIGINS), an
// address that is none of its own, and the Host under which a caller inside the cluster reaches the
// process without the front door, with the origin a proof names there (DISPATCH_PROOF_ORIGINS in
// c-l-served (4b)).
const (
	e2eCanonical     = "https://a.example.test"
	e2eServed        = "https://b.example.test"
	e2eFormer        = "https://old.example.test"
	e2eOther         = "https://other.example.test"
	e2eClusterHost   = "dispatch.test-ns.svc.cluster.local:8766"
	e2eClusterOrigin = "http://" + e2eClusterHost
)

// e2eHosts are the names the front door's one certificate holds.
var e2eHosts = []string{"a.example.test", "b.example.test", "old.example.test", "other.example.test"}

// The addresses on the path between a caller and the process: the two callers, which Linux's
// loopback holds beside 127.0.0.1, and the address that stands for the internal Application Load
// Balancer's forwarding rule, which the front door appends to X-Forwarded-For after the caller's
// address, the same for every caller.
const (
	e2eCallerSeven = "127.0.0.7"
	e2eCallerEight = "127.0.0.8"
	e2eForwardRule = "10.128.0.99"
)

// The person the issuer signs in at every authorization (--sign-in-as), a member of the sign-in
// pool group the servers admit (--sign-in-group, DISPATCH_SIGNIN_GROUP: Dispatch takes a Workspace
// domain from Google's own issuer alone, so a server signing people in at dev-oidc-issuer admits
// its group), and the user agent of the browser they sign in with.
const (
	e2ePerson  = "ada@example.test"
	e2eGroup   = "served-addresses-e2e-people"
	e2eBrowser = "Mozilla/5.0 (X11; Linux x86_64) served-addresses-e2e browser"
)

// e2eBrokerToken is the broker token the agent of every server presents: a service's, live at the
// rig's broker and bound to the agent's key. e2eProxiedToken is the token 127.0.0.8 presents in
// c-l-proxy (2), bound to that caller's key and checked by no server before that request.
const (
	e2eBrokerToken  = "served-addresses-e2e-agent"
	e2eProxiedToken = "served-addresses-e2e-proxied-agent"
)

// e2ePreviousToken is the shared agent token's previous value, which the servers of c-l-proxy take
// after the current one (DISPATCH_AGENT_TOKEN), so a request on it writes the previous-token line.
const e2ePreviousToken = "served-addresses-e2e-previous-shared"

// e2ePreviousTokenLine is the message of that line (auth's notePreviousToken).
const e2ePreviousTokenLine = "dispatch: request authenticated with a previous shared agent token"

// e2eSession is the agent session that writes the checks' issues, documents and comments on the
// shared agent token every test process takes (dispatchTestAgentToken), which a bearer's write
// names as its actor, and that c-l-cli's dispatch command runs as (DISPATCH_SESSION_ID).
const e2eSession = "0195d1a0-5491-7000-8000-00000000e2e0"

// e2eWrite is the JSON body of a bearer's write of fields, naming e2eSession as its actor.
func e2eWrite(fields map[string]any) string {
	fields["actor"] = map[string]string{"kind": "session", "id": e2eSession}
	body, _ := json.Marshal(fields)
	return string(body)
}

// e2eAccountToken is Dispatch's own account token for the broker, which the stub broker reads
// nothing of; DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE names a file holding it.
const e2eAccountToken = "dispatch-account-token\n"

// e2eClient is one client of Dispatch's at the issuer: its id, its secret and the one callback the
// issuer takes for it, as a Google OAuth client lists its redirect URIs.
type e2eClient struct {
	id, secret, callback string
}

// e2eRig is what every server of a test shares: the throwaway CA and the certificate the front
// doors serve, the secrets broker, the sign-in issuer with Dispatch's clients there, and the agent
// whose broker token the broker holds.
type e2eRig struct {
	pool         *x509.CertPool
	caPEM        []byte
	serving      tls.Certificate
	broker       *ciphertokentest.StubBroker
	accountToken string
	issuer       string
	// atServed is the client whose callback is the served address's, the one c-l-served (3)
	// and c-l-record sign in at; atCanonical's is the canonical address's, the one a server of
	// c-l-served (6) signs its session in at.
	atServed, atCanonical e2eClient
	agent                 *ciphertokentest.Caller
	dispatchBin           string
}

func newE2ERig(t *testing.T) *e2eRig {
	t.Helper()
	rig := &e2eRig{broker: ciphertokentest.NewStubBroker(t), accountToken: writeFile(t, "account-token", e2eAccountToken), agent: ciphertokentest.NewCaller(t)}
	rig.pool, rig.caPEM, rig.serving = e2eCertificates(t)
	rig.holds("served-agents", e2eBrokerToken, rig.agent)
	rig.atServed = e2eClient{id: "dispatch-at-b", secret: e2eRandom(t), callback: e2eServed + "/auth/callback"}
	rig.atCanonical = e2eClient{id: "dispatch-at-a", secret: e2eRandom(t), callback: e2eCanonical + "/auth/callback"}
	rig.issuer = e2eStartIssuer(t, rig.atServed, rig.atCanonical)
	rig.dispatchBin = os.Getenv(e2eDispatchBinEnv)
	if rig.dispatchBin == "" {
		rig.dispatchBin = cmdtest.Build(t, "envoy-dispatch")
	}
	t.Logf("envoy-dispatch: %s; sign-in issuer: %s", rig.dispatchBin, rig.issuer)
	return rig
}

// holds makes the broker answer token as a live broker token of service, bound to caller's key, as
// the broker's exchange mints a service's token.
func (r *e2eRig) holds(service, token string, caller *ciphertokentest.Caller) {
	r.broker.Says(token, ciphertoken.Introspection{
		Active: true, Session: service + "-session", Service: service, ProjectScope: ciphertoken.ProjectScopePerson,
		Scope: "dispatch.write", ExpiresAt: time.Now().Add(time.Hour).Unix(), ClientID: ciphertoken.ExchangeClientID, Thumbprint: caller.JKT(),
	})
}

// e2eRandom is 16 random bytes in hex: a client's secret, made for the run and written nowhere but
// the issuer's secret file and the server's environment.
func e2eRandom(t *testing.T) string {
	t.Helper()
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(secret)
}

// e2eCertificates makes the run's throwaway CA and the one certificate every front door serves for
// every example host, in memory. The CA's certificate is written only where the dispatch command
// reads it (c-l-cli's NODE_EXTRA_CA_CERTS).
func e2eCertificates(t *testing.T) (*x509.CertPool, []byte, tls.Certificate) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "served-addresses e2e CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(6 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: e2eHosts[0]}, DNSNames: e2eHosts,
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(6 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: key}
}

// e2eIssuerListening is the line dev-oidc-issuer logs once it serves, naming its URL.
var e2eIssuerListening = regexp.MustCompile(`issuer listening url=(\S+)`)

// e2eStartIssuer starts dev-oidc-issuer serving the code flow for clients, each with its one
// callback (it registers one per client id, as the run's sign-ins need), and answers its URL. Each
// authorization signs in e2ePerson as a member of e2eGroup (--sign-in-as, --sign-in-group), as the
// browser harness's DISPATCH_E2E_SIGNIN=oidc runs it (e2e/run-signin-issuer.sh).
func e2eStartIssuer(t *testing.T, clients ...e2eClient) string {
	t.Helper()
	binary := os.Getenv(e2eIssuerBinEnv)
	if binary == "" {
		binary = cmdtest.BuildCommand(t, cmdtest.Command{Name: "dev-oidc-issuer", Dir: "../dev-oidc-issuer"})
	}
	dir := t.TempDir()
	var args []string
	for _, client := range clients {
		file := filepath.Join(dir, client.id+".secret")
		if err := os.WriteFile(file, []byte(client.secret), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--code-flow", client.id+"="+file+"="+client.callback)
	}
	args = append(args, "--sign-in-as", e2ePerson, "--sign-in-group", e2eGroup)
	cmd := exec.Command(binary, args...)
	cmd.Dir, cmd.Env = dir, []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH")}
	issuer := cmdtest.Start(t, "dev-oidc-issuer", cmd)
	issuer.WaitForOutput(t, "issuer listening url=")
	match := e2eIssuerListening.FindStringSubmatch(issuer.Output.String())
	if match == nil {
		t.Fatalf("dev-oidc-issuer named no URL:\n%s", issuer.Output.String())
	}
	return match[1]
}

// settings are the server of c-l-served's settings but its lists: the canonical address, the
// sign-in through the issuer for client, admitting e2eGroup's members, cookie identity, and the
// broker. The process gets nothing else of the caller's (startDispatchProcessWith).
func (r *e2eRig) settings(client e2eClient) map[string]string {
	return map[string]string{
		"DISPATCH_IDENTITY":                         "cookie",
		"DISPATCH_SERVER_URL":                       e2eCanonical,
		"DISPATCH_SIGNIN_ISSUER":                    r.issuer,
		"DISPATCH_SIGNIN_CLIENT_ID":                 client.id,
		"DISPATCH_SIGNIN_CLIENT_SECRET":             client.secret,
		"DISPATCH_SIGNIN_GROUP":                     e2eGroup,
		"DISPATCH_AGENT_SECRETS_URL":                r.broker.Server.URL,
		"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": r.accountToken,
	}
}

// servedSettings are settings for the served address's client with both lists: b.example.test
// served, old.example.test former.
func (r *e2eRig) servedSettings() map[string]string {
	settings := r.settings(r.atServed)
	settings["DISPATCH_SERVED_ORIGINS"] = e2eServed
	settings["DISPATCH_FORMER_ORIGINS"] = e2eFormer
	return settings
}

// e2eServer is one envoy-dispatch process behind a front door of its own.
type e2eServer struct {
	rig       *e2eRig
	process   *dispatchProcess
	frontDoor string
	// caller is an agent at 127.0.0.7 that keeps no cookie; local is a caller at 127.0.0.1 that
	// sends straight to the process, past the front door.
	caller, local *http.Client
	// seeded is whether seed has made project SRV, and issues are the keys of the issues it made,
	// by title.
	seeded bool
	issues map[string]string
}

// start starts binary with settings on a database of its own and serves it behind a front door,
// once it is healthy. Header identity's acknowledgement goes: the server signs people in.
func (r *e2eRig) start(t *testing.T, binary string, settings map[string]string) *e2eServer {
	t.Helper()
	database := storetest.Open(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		Binary: binary, Env: settings, Without: []string{"DISPATCH_IDENTITY_HEADER_TRUSTED"},
	})
	process.waitHealthy(t)
	server := &e2eServer{rig: r, process: process, issues: map[string]string{}}
	backend, err := url.Parse(process.url(""))
	if err != nil {
		t.Fatal(err)
	}
	server.frontDoor = r.serveFrontDoor(t, backend)
	server.caller = r.client(t, server.frontDoor, e2eCallerSeven, nil)
	server.local = r.client(t, "", "127.0.0.1", nil)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the output of %s:\n%s", binary, process.Output.String())
		}
	})
	return server
}

// serveFrontDoor serves the TLS front door of backend and answers its address: one listener on
// 127.0.0.1 that answers every example host with the run's certificate, keeps each request's Host,
// passes on what the caller wrote, dials backend from 127.0.0.1, and writes X-Forwarded-For as
// Google's internal Application Load Balancer does, one line of what the caller wrote, then the
// caller's address, then e2eForwardRule. httputil.ReverseProxy's Rewrite drops the forwarding
// headers it is handed, so the ones a caller writes are put back: a forwarded header the caller
// chose must reach the server for c-l-served (2) to show it chooses nothing.
func (r *e2eRig) serveFrontDoor(t *testing.T, backend *url.URL) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, Timeout: 10 * time.Second}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(backend)
			request.Out.Host = request.In.Host
			for _, name := range []string{"X-Forwarded-Host", "Forwarded"} {
				if values := request.In.Header.Values(name); len(values) > 0 {
					request.Out.Header[name] = slices.Clone(values)
				}
			}
			client, _, _ := net.SplitHostPort(request.In.RemoteAddr)
			forwarded := append(slices.Clone(request.In.Header.Values("X-Forwarded-For")), client, e2eForwardRule)
			request.Out.Header.Set("X-Forwarded-For", strings.Join(forwarded, ", "))
			request.Out.Header.Set("X-Forwarded-Proto", "https")
		},
		Transport: &http.Transport{DialContext: dialer.DialContext},
	}
	server := &http.Server{
		Handler:           proxy,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{r.serving}},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String()
}

// client is a caller whose connections leave from, to whom every example host on port 443 resolves
// to frontDoor, and who trusts the run's CA. It follows no redirect, so each answer is read as the
// server sent it, and keeps cookies in jar, as a browser does, where jar is not nil.
func (r *e2eRig) client(t *testing.T, frontDoor, from string, jar http.CookieJar) *http.Client {
	t.Helper()
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(from)}, Timeout: 10 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: r.pool},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if host, port, err := net.SplitHostPort(address); err == nil && port == "443" && strings.HasSuffix(strings.ToLower(host), ".example.test") {
				address = frontDoor
			}
			return dialer.DialContext(ctx, network, address)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{
		Jar: jar, Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// send sends method target from client with header and, when it is not empty, body as JSON.
func (s *e2eServer) send(t *testing.T, client *http.Client, method, target string, header http.Header, body string) httpAnswer {
	t.Helper()
	return s.exchange(t, client, method, target, "", header, body)
}

// direct sends method path to the process itself from 127.0.0.1, under Host host ("" for the
// process's own address): a caller inside the cluster, or the front door's own address.
func (s *e2eServer) direct(t *testing.T, method, path, host string, header http.Header, body string) httpAnswer {
	t.Helper()
	return s.exchange(t, s.local, method, s.process.url(path), host, header, body)
}

func (s *e2eServer) exchange(t *testing.T, client *http.Client, method, target, host string, header http.Header, body string) httpAnswer {
	t.Helper()
	var content io.Reader
	if body != "" {
		content = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, target, content)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, values := range header {
		request.Header[name] = values
	}
	if host != "" {
		request.Host = host
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s %s: read the answer: %v", method, target, err)
	}
	return httpAnswer{status: response.StatusCode, header: response.Header, body: answer}
}

// e2eSummary is answer as the rows compare it: its status, then the code its JSON body names or,
// for a body that names none, its error. A body that is no JSON error is left out, and so is
// anything that differs between two runs of one build (a token, a cookie's value, a time), so
// c-l-served (6) can hold two builds' summaries equal.
func e2eSummary(answer httpAnswer) string {
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	summary := strconv.Itoa(answer.status)
	if json.Unmarshal(answer.body, &body) == nil {
		switch {
		case body.Code != "":
			return summary + " " + body.Code
		case body.Error != "":
			return summary + " " + body.Error
		}
	}
	return summary
}

// e2eObservation is one request of a check's step, what the server answered it (e2eSummary, with
// the step's own detail), and what the server of the check must answer it.
type e2eObservation struct {
	request, answer, want string
}

// requireObservations logs each observation and fails the test on each answer that is not wanted.
func requireObservations(t *testing.T, observed []e2eObservation) {
	t.Helper()
	for _, observation := range observed {
		t.Logf("%s: %s", observation.request, observation.answer)
		if observation.answer != observation.want {
			t.Errorf("%s answered %s, want %s", observation.request, observation.answer, observation.want)
		}
	}
}

// The answers the rows want: a cookie write the origin check lets through mints a personal token,
// one it refuses is the check's 403, and a proof the broker-token check refuses names its code.
const (
	e2eWritten        = "201"
	e2eOriginRefused  = "403 invalid request origin"
	e2eProofRefused   = "401 BROKER_TOKEN_PROOF_INVALID"
	e2eWhoamiAnswered = "200"
)

// e2eAt is path at host's https URL as a caller dials the front door for it: host's name in lower
// case, with no trailing dot and no port. A request sent there under Host host reaches the front
// door however host writes its name and port, and the front door hands host on as it was written.
func e2eAt(host, path string) string {
	name := strings.ToLower(host)
	if bare, _, err := net.SplitHostPort(name); err == nil {
		name = bare
	}
	return "https://" + strings.TrimSuffix(name, ".") + path
}

// cookieWrite is a browser write carrying session, POST /api/v1/me/agent-tokens, which mints a
// personal token (201) once the cookie origin check lets it through, sent to Host host, as written,
// with Origin origin ("" for none) and header's other fields.
func (s *e2eServer) cookieWrite(t *testing.T, host, origin string, header http.Header, session *http.Cookie) httpAnswer {
	t.Helper()
	sent := header.Clone()
	if sent == nil {
		sent = http.Header{}
	}
	if origin != "" {
		sent.Set("Origin", origin)
	}
	sent.Set("Cookie", session.Name+"="+session.Value)
	return s.exchange(t, s.caller, http.MethodPost, e2eAt(host, "/api/v1/me/agent-tokens"), host, sent, `{"name":"a cookie write at `+host+`"}`)
}

// servedCookieWrites are c-l-served (1)'s cookie writes: at each served address with its own
// Origin, and at the served address's Host written in capitals, with a trailing dot, and with the
// https default port, each of which names that address.
func (s *e2eServer) servedCookieWrites(t *testing.T, session *http.Cookie) []e2eObservation {
	t.Helper()
	var observed []e2eObservation
	for _, write := range []struct{ origin, host string }{
		{e2eServed, "b.example.test"}, {e2eCanonical, "a.example.test"}, {e2eServed, "B.EXAMPLE.TEST"},
		{e2eServed, "b.example.test."}, {e2eServed, "b.example.test:443"},
	} {
		observed = append(observed, e2eObservation{
			request: fmt.Sprintf("(1) a cookie POST with Origin %s to Host %s", write.origin, write.host),
			answer:  e2eSummary(s.cookieWrite(t, write.host, write.origin, nil, session)),
			want:    e2eWritten,
		})
	}
	return observed
}

// forwardedHeaderWrites are c-l-served (2)'s cookie writes: the canonical address's Origin at the
// served address, the served address's Origin at the canonical address with X-Forwarded-Host
// naming the served address, and the served address's Origin at its host name on another port,
// which is another host.
func (s *e2eServer) forwardedHeaderWrites(t *testing.T, session *http.Cookie) []e2eObservation {
	t.Helper()
	return []e2eObservation{
		{
			request: "(2) a cookie POST with Origin " + e2eCanonical + " to Host b.example.test",
			answer:  e2eSummary(s.cookieWrite(t, "b.example.test", e2eCanonical, nil, session)),
			want:    e2eOriginRefused,
		},
		{
			request: "(2) a cookie POST with Origin " + e2eServed + " and X-Forwarded-Host b.example.test to Host a.example.test",
			answer:  e2eSummary(s.cookieWrite(t, "a.example.test", e2eServed, http.Header{"X-Forwarded-Host": {"b.example.test"}}, session)),
			want:    e2eOriginRefused,
		},
		{
			request: "(2) a cookie POST with Origin " + e2eServed + " to Host b.example.test:8443",
			answer:  e2eSummary(s.cookieWrite(t, "b.example.test:8443", e2eServed, nil, session)),
			want:    e2eOriginRefused,
		},
	}
}

// e2eSignIn is one browser sign-in: the summary of each of its three hops, the session cookie the
// callback set (nil for none), and how much output the process had written before the callback.
type e2eSignIn struct {
	started, authorized, called string
	cookie                      *http.Cookie
	beforeCallback              int
}

// signIn runs the sign-in a browser starts at host: GET /auth/start there, the issuer's
// authorization of the redirect_uri the start named, which signs e2ePerson in, and the callback
// the issuer sends the browser to, the browser's cookie jar keeping the start's state cookie for
// the host that set it alone. The issuer is discovered in the background from the process's boot,
// and until then the start answers 503, so the start is asked again until it answers anything else.
func (s *e2eServer) signIn(t *testing.T, host string) e2eSignIn {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := s.rig.client(t, s.frontDoor, e2eCallerSeven, jar)
	header := http.Header{"User-Agent": {e2eBrowser}}
	var start httpAnswer
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		start = s.send(t, browser, http.MethodGet, "https://"+host+"/auth/start", header, "")
		if start.status != http.StatusServiceUnavailable || time.Now().After(deadline) {
			break
		}
	}
	var signIn e2eSignIn
	signIn.started = e2eSummary(start)
	if start.status != http.StatusFound {
		return signIn
	}
	authorization, err := url.Parse(start.header.Get("Location"))
	if err != nil {
		t.Fatalf("the start at %s sent the browser to %q: %v", host, start.header.Get("Location"), err)
	}
	signIn.started += " redirect_uri=" + authorization.Query().Get("redirect_uri")
	authorized := s.send(t, browser, http.MethodGet, authorization.String(), header, "")
	signIn.authorized = e2eSummary(authorized)
	if authorized.status != http.StatusFound {
		return signIn
	}
	callback, err := url.Parse(authorized.header.Get("Location"))
	if err != nil {
		t.Fatalf("the issuer sent the browser to %q: %v", authorized.header.Get("Location"), err)
	}
	signIn.authorized += " to " + callback.Scheme + "://" + callback.Host + callback.Path
	signIn.beforeCallback = len(s.process.Output.String())
	called := s.send(t, browser, http.MethodGet, callback.String(), header, "")
	signIn.called = e2eSummary(called) + "; no session cookie"
	for _, cookie := range (&http.Response{Header: called.header}).Cookies() {
		if (cookie.Name == "dsession" || cookie.Name == "__Host-dsession") && cookie.Value != "" && cookie.MaxAge >= 0 {
			signIn.cookie = cookie
			signIn.called = fmt.Sprintf("%s; session cookie %s with Domain %q", e2eSummary(called), cookie.Name, cookie.Domain)
		}
	}
	return signIn
}

// servedSignIn is c-l-served (3): the sign-in a browser starts at the served address.
func (s *e2eServer) servedSignIn(t *testing.T) ([]e2eObservation, *http.Cookie) {
	t.Helper()
	signIn := s.signIn(t, "b.example.test")
	return []e2eObservation{
		{"(3) GET /auth/start at Host b.example.test", signIn.started, "302 redirect_uri=" + e2eServed + "/auth/callback"},
		{"(3) the issuer's authorization of that redirect_uri", signIn.authorized, "302 to " + e2eServed + "/auth/callback"},
		{"(3) the callback the issuer sent the browser to", signIn.called, `302; session cookie dsession with Domain ""`},
	}, signIn.cookie
}

// e2eProof is the Authorization and DPoP headers of caller's request GET <origin>/api/v1/whoami
// presenting token, each call signing a fresh proof whose htu is that URL.
func e2eProof(t *testing.T, caller *ciphertokentest.Caller, origin, token string) http.Header {
	t.Helper()
	signed := caller.Signed(t, http.MethodGet, origin+"/api/v1/whoami", token)
	return http.Header{"Authorization": {signed.Header.Get("Authorization")}, "Dpop": {signed.Header.Get("DPoP")}}
}

// servedProofs are c-l-served (4)'s broker-token requests, each GET /api/v1/whoami with the
// agent's token.
func (s *e2eServer) servedProofs(t *testing.T) []e2eObservation {
	t.Helper()
	agent := s.rig.agent
	atServed := e2eProof(t, agent, e2eServed, e2eBrokerToken)
	first := e2eSummary(s.send(t, s.caller, http.MethodGet, e2eServed+"/api/v1/whoami", atServed, ""))
	again := e2eSummary(s.send(t, s.caller, http.MethodGet, e2eServed+"/api/v1/whoami", atServed, ""))
	cluster := e2eSummary(s.direct(t, http.MethodGet, "/api/v1/whoami", e2eClusterHost, e2eProof(t, agent, e2eServed, e2eBrokerToken), ""))
	other := e2eSummary(s.send(t, s.caller, http.MethodGet, e2eServed+"/api/v1/whoami", e2eProof(t, agent, e2eOther, e2eBrokerToken), ""))
	return []e2eObservation{
		{"(4) a broker-token request to Host b.example.test with a proof whose htu is " + e2eServed + "/api/v1/whoami", first, e2eWhoamiAnswered},
		{"(4) the same proof again", again, e2eProofRefused},
		{"(4) a request to Host " + e2eClusterHost + " with a proof whose htu is " + e2eServed + "/api/v1/whoami", cluster, e2eWhoamiAnswered},
		{"(4) a request to Host b.example.test with a proof whose htu is " + e2eOther + "/api/v1/whoami", other, e2eProofRefused},
	}
}

// listedProofs are c-l-served (4b)'s broker-token requests to a server that also lists
// e2eClusterOrigin in DISPATCH_PROOF_ORIGINS, each GET /api/v1/whoami with the agent's token to
// Host e2eClusterHost, past the front door: a proof whose htu is at that origin, and one whose htu
// is at the served address.
func (s *e2eServer) listedProofs(t *testing.T) []e2eObservation {
	t.Helper()
	agent := s.rig.agent
	listed := e2eSummary(s.direct(t, http.MethodGet, "/api/v1/whoami", e2eClusterHost, e2eProof(t, agent, e2eClusterOrigin, e2eBrokerToken), ""))
	served := e2eSummary(s.direct(t, http.MethodGet, "/api/v1/whoami", e2eClusterHost, e2eProof(t, agent, e2eServed, e2eBrokerToken), ""))
	return []e2eObservation{
		{"(4b) a request to Host " + e2eClusterHost + " with a proof whose htu is " + e2eClusterOrigin + "/api/v1/whoami", listed, e2eWhoamiAnswered},
		{"(4b) a request to Host " + e2eClusterHost + " with a proof whose htu is " + e2eServed + "/api/v1/whoami", served, e2eWhoamiAnswered},
	}
}

// requireRefusedStart starts the rig's envoy-dispatch with settings on databaseURL and fails the
// test unless the process exits non-zero before it serves, having written a refusal that names
// naming. what names the start in the log.
func (r *e2eRig) requireRefusedStart(t *testing.T, databaseURL, what string, settings map[string]string, naming string) {
	t.Helper()
	process := startDispatchProcessWith(t, databaseURL, dispatchOptions{
		Binary: r.dispatchBin, Env: settings, Without: []string{"DISPATCH_IDENTITY_HEADER_TRUSTED"},
	})
	process.WaitExit(t, "its start")
	output := process.Output.String()
	code := process.Cmd.ProcessState.ExitCode()
	refusal := "none"
	for line := range strings.Lines(output) {
		if strings.Contains(line, naming) {
			refusal = strings.TrimSpace(line)
		}
	}
	t.Logf("a start with %s: exit %d; %s", what, code, refusal)
	if code == 0 || !strings.Contains(output, naming) || strings.Contains(output, `msg="dispatch: listening"`) {
		t.Errorf("a start with %s exited %d, want a refusal naming %q before it serves:\n%s", what, code, naming, output)
	}
}

// e2eStatus is answer's status alone, for a row whose answer is a path no route serves, whatever
// body the router writes for it.
func e2eStatus(answer httpAnswer) string { return strconv.Itoa(answer.status) }

// e2eLogFields reads one line of the process's log as slog's text handler writes it: key=value
// pairs separated by spaces, a value quoted (strconv.Quote) where it holds a space, a quote, an
// equals sign or a byte it cannot print.
func e2eLogFields(line string) map[string]string {
	fields := map[string]string{}
	for rest := strings.TrimSpace(line); rest != ""; rest = strings.TrimLeft(rest, " ") {
		equals := strings.IndexByte(rest, '=')
		if equals <= 0 {
			break
		}
		key := rest[:equals]
		rest = rest[equals+1:]
		if strings.HasPrefix(rest, `"`) {
			quoted, err := strconv.QuotedPrefix(rest)
			if err != nil {
				break
			}
			fields[key], _ = strconv.Unquote(quoted)
			rest = rest[len(quoted):]
			continue
		}
		end := strings.IndexByte(rest, ' ')
		if end < 0 {
			end = len(rest)
		}
		fields[key] = rest[:end]
		rest = rest[end:]
	}
	return fields
}

// logLines are the lines the process wrote past since bytes of its output whose msg is message.
func (s *e2eServer) logLines(since int, message string) []map[string]string {
	output := s.process.Output.String()
	var lines []map[string]string
	for line := range strings.Lines(output[min(since, len(output)):]) {
		if fields := e2eLogFields(line); fields["msg"] == message {
			lines = append(lines, fields)
		}
	}
	return lines
}

// waitForLine waits up to 10 s for a line of message that match takes, written past since bytes
// of the output, and answers the first. A request's line is written as its handler returns, which
// may be after the caller has read the answer.
func (s *e2eServer) waitForLine(t *testing.T, since int, message, what string, match func(map[string]string) bool) map[string]string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		for _, line := range s.logLines(since, message) {
			if match(line) {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the process wrote no %q line for %s within 10 s", message, what)
		}
	}
}

// requestLineOf is the dispatch: request line of the one request that carried userAgent.
func (s *e2eServer) requestLineOf(t *testing.T, userAgent string) map[string]string {
	t.Helper()
	return s.waitForLine(t, 0, accesslog.Message, "the request whose user agent is "+userAgent, func(line map[string]string) bool {
		return line["user_agent"] == userAgent
	})
}

// seed makes project SRV, once per server, and an issue of it for each title not made yet, as the
// agent session. The shared agent token acts for no person, so the project names its default
// assignee, which must be a person who has signed in: e2ePerson, whom an earlier subtest signed in.
func (s *e2eServer) seed(t *testing.T, titles ...string) {
	t.Helper()
	if !s.seeded {
		if created := s.api(t, http.MethodPost, "/api/v1/projects", e2eWrite(map[string]any{"key": "SRV", "name": "Served addresses", "default_assignee": e2ePerson})); created.status != http.StatusCreated {
			t.Fatalf("create project SRV: %s", created)
		}
		s.seeded = true
	}
	for _, title := range titles {
		if s.issues[title] != "" {
			continue
		}
		created := s.api(t, http.MethodPost, "/api/v1/issues", e2eWrite(map[string]any{"project": "SRV", "title": title, "force": true}))
		var issue struct {
			Key string `json:"key"`
		}
		if created.status != http.StatusCreated || json.Unmarshal(created.body, &issue) != nil || issue.Key == "" {
			t.Fatalf("create issue %q: %s", title, created)
		}
		s.issues[title] = issue.Key
	}
}

// api sends method path to the canonical address as the agent session, on the shared agent token;
// a write names its actor (e2eWrite).
func (s *e2eServer) api(t *testing.T, method, path, body string) httpAnswer {
	t.Helper()
	return s.send(t, s.caller, method, e2eCanonical+path, http.Header{"Authorization": {"Bearer " + dispatchTestAgentToken}}, body)
}

// citedFrom is, for each key of keys, whether GET /api/v1/references?from=ref names an edge out to
// that issue. It reads until every key of wanted is cited or 30 s have passed, since the index of a
// write may be written after its answer, and answers the last reading.
func (s *e2eServer) citedFrom(t *testing.T, ref string, keys, wanted []string) map[string]bool {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		answer := s.api(t, http.MethodGet, "/api/v1/references?"+url.Values{"from": {ref}}.Encode(), "")
		if answer.status != http.StatusOK {
			t.Fatalf("GET /api/v1/references?from=%s: %s", ref, answer)
		}
		var graph struct {
			Edges []struct {
				Direction string `json:"direction"`
				Node      struct {
					Kind     string  `json:"kind"`
					ID       string  `json:"id"`
					Ref      string  `json:"ref"`
					IssueKey *string `json:"issue_key"`
				} `json:"node"`
			} `json:"edges"`
		}
		if err := json.Unmarshal(answer.body, &graph); err != nil {
			t.Fatalf("GET /api/v1/references?from=%s: %v: %s", ref, err, answer.body)
		}
		cited := map[string]bool{}
		for _, edge := range graph.Edges {
			for _, key := range keys {
				if edge.Direction == "out" && (edge.Node.Ref == "dispatch://"+key || edge.Node.Kind == "issue" && edge.Node.ID == key) {
					cited[key] = true
				}
			}
		}
		all := true
		for _, key := range wanted {
			all = all && cited[key]
		}
		if all || time.Now().After(deadline) {
			return cited
		}
	}
}

// rebuildRefs runs envoy-dispatch rebuild-refs with the process's own settings, beside it, and
// answers what it printed.
func (s *e2eServer) rebuildRefs(t *testing.T) string {
	t.Helper()
	var env []string
	for _, entry := range s.process.Cmd.Env {
		if !strings.HasPrefix(entry, "DISPATCH_LISTEN_FD=") {
			env = append(env, entry)
		}
	}
	cmd := exec.Command(s.process.Cmd.Path, "rebuild-refs")
	cmd.Dir, cmd.Env = s.process.Cmd.Dir, env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("envoy-dispatch rebuild-refs: %v:\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

// TestServedAddressesLive runs c-l-served, c-l-record, the API half of c-l-refs and c-l-cli on the
// real process: the server of c-l-served answers at https://a.example.test, its canonical address,
// serves https://b.example.test beside it and served https://old.example.test before, and signs in
// through the issuer for the client whose callback is the served address's.
func TestServedAddressesLive(t *testing.T) {
	rig := newE2ERig(t)
	server := rig.start(t, rig.dispatchBin, rig.servedSettings())
	// session is the person's session cookie from the last sign-in at the served address; the
	// writes of the later steps carry it.
	var session *http.Cookie
	requireSession := func(t *testing.T) {
		t.Helper()
		if session == nil {
			t.Fatal("no earlier sign-in at the served address set a session cookie")
		}
	}

	t.Run("c-l-record one request to each Host, then a browser sign-in at b.example.test", func(t *testing.T) {
		for index, host := range []string{
			"a.example.test", "b.example.test", "B.EXAMPLE.TEST", "b.example.test.", "b.example.test:443", "b.example.test:8443",
			"old.example.test", "other.example.test", e2eClusterHost,
		} {
			userAgent := fmt.Sprintf("served-addresses-e2e c-l-record request %d", index)
			header := http.Header{"User-Agent": {userAgent}}
			if host == e2eClusterHost {
				server.direct(t, http.MethodGet, "/auth/whoami", host, header, "")
			} else {
				server.exchange(t, server.caller, http.MethodGet, e2eAt(host, "/auth/whoami"), host, header, "")
			}
			line := server.requestLineOf(t, userAgent)
			t.Logf("GET /auth/whoami to Host %s: host=%q route=%s status=%s address=%s", host, line["host"], line["route"], line["status"], line["address"])
			if got, present := line["host"]; !present || got != host {
				t.Errorf("the dispatch: request line of a request to Host %s names host %q (present: %t), want the Host as the request wrote it", host, got, present)
			}
		}

		signIn := server.signIn(t, "b.example.test")
		if signIn.cookie == nil {
			t.Fatalf("the sign-in at b.example.test set no session cookie: start %s, authorization %s, callback %s", signIn.started, signIn.authorized, signIn.called)
		}
		session = signIn.cookie
		callback := server.waitForLine(t, signIn.beforeCallback, accesslog.Message, "the sign-in's callback", func(line map[string]string) bool {
			return line["path"] == "/auth/callback"
		})
		t.Logf("the callback's line: caller_kind=%s caller=%s host=%s user_agent=%q status=%s", callback["caller_kind"], callback["caller"], callback["host"], callback["user_agent"], callback["status"])
		for field, want := range map[string]string{"caller_kind": "user", "caller": e2ePerson, "host": "b.example.test", "user_agent": e2eBrowser, "status": "302"} {
			if callback[field] != want {
				t.Errorf("the sign-in callback's line names %s %q, want %q", field, callback[field], want)
			}
		}
		// A line that would record the sign-in a second time is written while the callback is
		// served, so the window is the callback's own, with a second for a line written as it
		// ends.
		time.Sleep(time.Second)
		output := server.process.Output.String()
		var naming []string
		for line := range strings.Lines(output[signIn.beforeCallback:]) {
			if strings.Contains(line, e2ePerson) {
				naming = append(naming, strings.TrimSpace(line))
			}
		}
		t.Logf("the lines written while the callback was served that name %s: %d:\n%s", e2ePerson, len(naming), strings.Join(naming, "\n"))
		if len(naming) != 1 {
			t.Errorf("the sign-in wrote %d lines naming %s, want its callback's request line alone:\n%s", len(naming), e2ePerson, strings.Join(naming, "\n"))
		}
	})

	t.Run("c-l-served (3) GET /auth/start at b.example.test and the sign-in it starts", func(t *testing.T) {
		observed, cookie := server.servedSignIn(t)
		requireObservations(t, observed)
		if cookie != nil {
			session = cookie
		}
	})

	t.Run("the runtime read: GET /auth/whoami at b.example.test names the origins the page reads", func(t *testing.T) {
		requireSession(t)
		answer := server.send(t, server.caller, http.MethodGet, e2eServed+"/auth/whoami", http.Header{"Cookie": {session.Name + "=" + session.Value}}, "")
		var whoami struct {
			Origins []string `json:"origins"`
		}
		if answer.status != http.StatusOK || json.Unmarshal(answer.body, &whoami) != nil {
			t.Fatalf("GET /auth/whoami at b.example.test with the session cookie: %s", answer)
		}
		t.Logf("GET /auth/whoami at Host b.example.test: origins %q", whoami.Origins)
		if want := []string{e2eCanonical, e2eServed, e2eFormer}; strings.Join(whoami.Origins, " ") != strings.Join(want, " ") {
			t.Errorf("GET /auth/whoami at b.example.test names origins %q, want %q: the canonical, the served and the former address", whoami.Origins, want)
		}
	})

	t.Run("c-l-served (1) a cookie write at each served address, by its Host alone", func(t *testing.T) {
		requireSession(t)
		requireObservations(t, server.servedCookieWrites(t, session))
	})

	t.Run("c-l-served (2) a cookie write an Origin or a forwarded header would choose another address for", func(t *testing.T) {
		requireSession(t)
		requireObservations(t, server.forwardedHeaderWrites(t, session))
	})

	t.Run("c-l-served (4) broker-token proofs naming the served address and another", func(t *testing.T) {
		requireObservations(t, server.servedProofs(t))
	})

	t.Run("c-l-served (4b) proofs at a DISPATCH_PROOF_ORIGINS origin", func(t *testing.T) {
		settings := rig.servedSettings()
		settings["DISPATCH_PROOF_ORIGINS"] = e2eClusterOrigin
		listed := rig.start(t, rig.dispatchBin, settings)
		requireObservations(t, listed.listedProofs(t))
		listed.process.Terminate(t)
		listed.process.WaitExit(t, "SIGTERM")
	})

	t.Run("c-l-served (7) the former address is not served", func(t *testing.T) {
		requireSession(t)
		get := func(path string) string {
			return e2eStatus(server.send(t, server.caller, http.MethodGet, e2eFormer+path, nil, ""))
		}
		requireObservations(t, []e2eObservation{
			{"(7) GET /auth/start at Host old.example.test", get("/auth/start"), "404"},
			{"(7) GET /auth/callback at Host old.example.test", get("/auth/callback?code=a-code&state=a-state"), "404"},
			{"(7) a cookie POST to Host old.example.test with Origin " + e2eFormer, e2eSummary(server.cookieWrite(t, "old.example.test", e2eFormer, nil, session)), e2eOriginRefused},
			{"(7) a cookie POST to Host old.example.test with Origin " + e2eCanonical, e2eSummary(server.cookieWrite(t, "old.example.test", e2eCanonical, nil, session)), e2eOriginRefused},
			{"(7) a cookie POST to Host old.example.test with no Origin and Sec-Fetch-Site: same-origin", e2eSummary(server.cookieWrite(t, "old.example.test", "", http.Header{"Sec-Fetch-Site": {"same-origin"}}, session)), e2eOriginRefused},
		})
	})

	// A document socket carries a person's edits on the session cookie alone, so at a former
	// address it is a cookie write too: R1 refuses every cookie write there, and a page at a former
	// address writes as no one. The socket at the served address is the control: the same dial,
	// cookie and document there are admitted read-write, so a refusal at the former address is the
	// fence and not a broken dial. The socket reads no Authorization header: one beside the cookie
	// leaves it the cookie's write, as a write that carries the header is not (an HTTP write with a
	// header the server cannot read is 401), and the former address refuses it too.
	t.Run("c-l-served (7) a document socket at the former address writes as no one", func(t *testing.T) {
		requireSession(t)
		const title = "Edited through a document socket"
		server.seed(t, title)
		written := server.api(t, http.MethodPost, "/api/v1/issues/"+server.issues[title]+"/artifacts", e2eWrite(map[string]any{"name": "socket.md", "content": "Before.\n"}))
		var document struct {
			Artifact struct {
				ID string `json:"id"`
			} `json:"artifact"`
		}
		if written.status != http.StatusCreated || json.Unmarshal(written.body, &document) != nil || document.Artifact.ID == "" {
			t.Fatalf("write the document: %s", written)
		}
		version := strconv.Itoa(pmdoc.SchemaVersion())
		// scopeAt dials the document's room at host as the page there does, with that host's
		// Origin, the session cookie and authorization as its Authorization header where it is not
		// empty, and answers the scope the room grants, or why it refused.
		scopeAt := func(host, authorization string) string {
			t.Helper()
			header := http.Header{"Host": {host}, "Origin": {"https://" + host}, "Cookie": {session.Name + "=" + session.Value}}
			if authorization != "" {
				header.Set("Authorization", authorization)
			}
			address := "ws" + strings.TrimPrefix(server.process.url("/ws/doc/"+document.Artifact.ID), "http") + "?schema_version=" + version
			peer, err := docstest.DialContext(context.Background(), address, header, document.Artifact.ID, crdt.New())
			if err != nil {
				return "refused: " + err.Error()
			}
			defer peer.Close()
			scope, err := peer.Authenticate(version, 10*time.Second)
			if err != nil {
				return "refused: " + err.Error()
			}
			return scope
		}
		const basic = "Basic eDp5"
		served, former, formerWithBasic := scopeAt("b.example.test", ""), scopeAt("old.example.test", ""), scopeAt("old.example.test", basic)
		t.Logf("(7) a document socket with the session cookie at Host b.example.test: %s", served)
		t.Logf("(7) a document socket with the session cookie at Host old.example.test: %s", former)
		t.Logf("(7) a document socket with the session cookie and Authorization: %s at Host old.example.test: %s", basic, formerWithBasic)
		if served != "read-write" {
			t.Fatalf("the document socket at the served address answered %q, want read-write: the control's dial does not reach the room", served)
		}
		if former == "read-write" {
			t.Errorf("the document socket at the former address old.example.test was admitted read-write on the session cookie, so a page there writes as the person; want it refused or read-only")
		}
		if formerWithBasic == "read-write" {
			t.Errorf("the document socket at the former address old.example.test, with Authorization: %s beside the session cookie, was admitted read-write on the cookie; want it refused or read-only", basic)
		}
	})

	t.Run("c-l-refs the API half: links at the served and former addresses are references", func(t *testing.T) {
		links := []struct {
			address, origin, title string
			reference              bool
		}{
			{"the canonical address", e2eCanonical, "Linked at the canonical address", true},
			{"the served address", e2eServed, "Linked at the served address", true},
			{"the former address", e2eFormer, "Linked at the former address", true},
			{"another address", e2eOther, "Linked at another address", false},
		}
		const owner = "The issue whose document and comment hold the links"
		titles := []string{owner}
		for _, link := range links {
			titles = append(titles, link.title)
		}
		requireSession(t)
		server.seed(t, titles...)
		var keys, wanted, pages []string
		for _, link := range links {
			key := server.issues[link.title]
			keys = append(keys, key)
			if link.reference {
				wanted = append(wanted, key)
			}
			pages = append(pages, link.origin+"/issues/"+key)
		}
		text := "The plan is at " + strings.Join(pages, " and ") + " today.\n"
		requireCited := func(t *testing.T, what string, cited map[string]bool) {
			t.Helper()
			for index, link := range links {
				t.Logf("%s: the link at %s (%s) is a reference: %t", what, link.address, pages[index], cited[keys[index]])
				if cited[keys[index]] != link.reference {
					t.Errorf("%s: the link at %s (%s) is a reference: %t, want %t", what, link.address, pages[index], cited[keys[index]], link.reference)
				}
			}
		}

		upload := e2eWrite(map[string]any{"name": "served-links.md", "content": text})
		written := server.api(t, http.MethodPost, "/api/v1/issues/"+server.issues[owner]+"/artifacts", upload)
		var document struct {
			Artifact struct {
				Slug string `json:"slug"`
			} `json:"artifact"`
		}
		if written.status != http.StatusCreated || json.Unmarshal(written.body, &document) != nil || document.Artifact.Slug == "" {
			t.Fatalf("write the document: %s", written)
		}
		documentRef := "dispatch://" + server.issues[owner] + "/artifact/" + document.Artifact.Slug
		requireCited(t, "the document's references", server.citedFrom(t, documentRef, keys, wanted))

		t.Logf("rebuild-refs: %s", server.rebuildRefs(t))
		requireCited(t, "the document's references after rebuild-refs", server.citedFrom(t, documentRef, keys, wanted))

		for index, link := range links {
			answer := server.api(t, http.MethodGet, "/api/v1/references?"+url.Values{"to": {pages[index]}}.Encode(), "")
			var graph struct {
				Node struct {
					Ref string `json:"ref"`
				} `json:"node"`
			}
			read := answer.status == http.StatusOK && json.Unmarshal(answer.body, &graph) == nil && graph.Node.Ref == "dispatch://"+keys[index]
			t.Logf("GET /api/v1/references?to=%s: %s, node %q", pages[index], e2eSummary(answer), graph.Node.Ref)
			switch {
			case link.reference && !read:
				t.Errorf("GET /api/v1/references?to=%s answered %s, want 200 naming dispatch://%s", pages[index], answer, keys[index])
			case !link.reference && e2eSummary(answer) != "400 INVALID_REFERENCE":
				t.Errorf("GET /api/v1/references?to=%s answered %s, want 400 INVALID_REFERENCE", pages[index], answer)
			}
		}

		body := e2eWrite(map[string]any{"body": text})
		posted := server.api(t, http.MethodPost, "/api/v1/issues/"+server.issues[owner]+"/comments", body)
		var comment struct {
			ID string `json:"id"`
		}
		if posted.status != http.StatusCreated || json.Unmarshal(posted.body, &comment) != nil || comment.ID == "" {
			t.Fatalf("post the comment: %s", posted)
		}
		requireCited(t, "the comment's references", server.citedFrom(t, "dispatch://"+server.issues[owner]+"/comment/"+comment.ID, keys, wanted))

		requireObservations(t, []e2eObservation{
			{"a broker-token request to Host old.example.test with a proof whose htu is at " + e2eFormer, e2eSummary(server.send(t, server.caller, http.MethodGet, e2eFormer+"/api/v1/whoami", e2eProof(t, rig.agent, e2eFormer, e2eBrokerToken), "")), e2eProofRefused},
			{"a broker-token request to Host b.example.test with a proof whose htu is at " + e2eFormer, e2eSummary(server.send(t, server.caller, http.MethodGet, e2eServed+"/api/v1/whoami", e2eProof(t, rig.agent, e2eFormer, e2eBrokerToken), "")), e2eProofRefused},
		})
		requireSession(t)
		requireObservations(t, []e2eObservation{
			{"a cookie POST with Origin " + e2eFormer + " to Host old.example.test", e2eSummary(server.cookieWrite(t, "old.example.test", e2eFormer, nil, session)), e2eOriginRefused},
			{"a cookie POST with Origin " + e2eFormer + " to Host b.example.test", e2eSummary(server.cookieWrite(t, "b.example.test", e2eFormer, nil, session)), e2eOriginRefused},
		})
	})

	t.Run("c-l-cli dispatch read --ref with a link at the former, the served and another address", func(t *testing.T) {
		const title = "Read by the dispatch command"
		requireSession(t)
		server.seed(t, title)
		key := server.issues[title]
		cli := os.Getenv(e2eDispatchCLIEnv)
		if cli == "" {
			var err error
			if cli, err = filepath.Abs("../../../envoy-client/bin/dispatch.ts"); err != nil {
				t.Fatal(err)
			}
		}
		bun, err := exec.LookPath("bun")
		if err != nil {
			t.Fatalf("bun runs the dispatch command: %v", err)
		}
		// The pod's /etc/hosts is not writable, so the command reaches each example host through
		// an HTTP CONNECT proxy (HTTPS_PROXY, which Bun's fetch honours) that takes every
		// *.example.test:443 to the front door, and trusts the run's CA (NODE_EXTRA_CA_CERTS).
		authority := filepath.Join(t.TempDir(), "front-door-ca.pem")
		if err := os.WriteFile(authority, rig.caPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		connect := server.serveConnect(t)
		t.Logf("the dispatch command: %s; HTTPS_PROXY=http://%s; NODE_EXTRA_CA_CERTS=%s", cli, connect, authority)
		for _, configured := range []string{e2eCanonical, e2eServed} {
			for _, link := range []struct {
				origin    string
				reference bool
			}{{e2eFormer, true}, {e2eServed, true}, {e2eOther, false}} {
				page := link.origin + "/issues/" + key
				home := t.TempDir()
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				cmd := exec.CommandContext(ctx, bun, cli, "read", "--ref", page)
				cmd.Dir = home
				cmd.Env = []string{
					"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CONFIG_HOME=" + home, "TMPDIR=" + home,
					"DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=" + e2eSession,
					"DISPATCH_URL=" + configured, "DISPATCH_TOKEN=" + dispatchTestAgentToken,
					"HTTPS_PROXY=http://" + connect, "NODE_EXTRA_CA_CERTS=" + authority,
				}
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				runErr := cmd.Run()
				cancel()
				if cmd.ProcessState == nil {
					t.Fatalf("DISPATCH_URL=%s dispatch read --ref %s did not run: %v", configured, page, runErr)
				}
				code := cmd.ProcessState.ExitCode()
				said := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
				read := code == 0 && strings.Contains(stdout.String(), key)
				refused := code == 2 && strings.Contains(said, "ref must be a valid dispatch:// reference")
				verdict := "neither read nor refused"
				switch {
				case read:
					verdict = "read as " + key
				case refused:
					verdict = "refused: not a dispatch:// reference"
				}
				t.Logf("DISPATCH_URL=%s dispatch read --ref %s: exit %d, %s", configured, page, code, verdict)
				want := "refused"
				if link.reference {
					want = "read as " + key
				}
				if link.reference && !read || !link.reference && !refused {
					t.Errorf("DISPATCH_URL=%s dispatch read --ref %s: exit %d, %s; want it %s:\n%s", configured, page, code, verdict, want, said)
				}
			}
		}
	})

	t.Run("c-l-served (5) a start the lists cannot be answered under is refused", func(t *testing.T) {
		database := storetest.Open(t)
		for _, row := range []struct {
			name     string
			settings map[string]string
			naming   string
		}{
			{"two served addresses that share one host", map[string]string{"DISPATCH_SERVED_ORIGINS": e2eServed + ",https://b.example.test:8443"},
				"DISPATCH_SERVED_ORIGINS entry 2 of 2: its host name b.example.test is DISPATCH_SERVED_ORIGINS entry 1's"},
			{"a served address that has a path", map[string]string{"DISPATCH_SERVED_ORIGINS": e2eServed + "/dispatch"},
				"DISPATCH_SERVED_ORIGINS entry 1 of 1: must be an absolute http or https URL with no path"},
			{"a former address on b.example.test's host", map[string]string{"DISPATCH_FORMER_ORIGINS": "http://b.example.test:8080"},
				"DISPATCH_FORMER_ORIGINS entry 1 of 1: its host name b.example.test is DISPATCH_SERVED_ORIGINS entry 1's"},
			{"a former address DISPATCH_PROOF_ORIGINS also lists", map[string]string{"DISPATCH_PROOF_ORIGINS": e2eFormer},
				"DISPATCH_FORMER_ORIGINS entry 1 of 1: its host name old.example.test is that of the proof origin " + e2eFormer + "; a former address passes no proof check"},
		} {
			t.Run(row.name, func(t *testing.T) {
				settings := rig.servedSettings()
				maps.Copy(settings, row.settings)
				rig.requireRefusedStart(t, database.Pool.Config().ConnString(), row.name, settings, row.naming)
			})
		}
	})

	t.Run("c-l-served (6) a server with no list answers (1) to (4) as the base commit's build does", func(t *testing.T) {
		base := os.Getenv(e2eBaseBinEnv)
		if base == "" {
			t.Fatalf("%s names no build of the base commit, which this step compares with", e2eBaseBinEnv)
		}
		var answered [][]e2eObservation
		for _, build := range []struct{ name, binary string }{
			{"this build with no served or former list", rig.dispatchBin},
			{"the base commit's build", base},
		} {
			// Its own session is signed in at the canonical address, the one address both builds
			// sign in at, through the client whose callback is that address's.
			listless := rig.start(t, build.binary, rig.settings(rig.atCanonical))
			signIn := listless.signIn(t, "a.example.test")
			if signIn.cookie == nil {
				t.Fatalf("%s: the sign-in at a.example.test set no session cookie: start %s, authorization %s, callback %s", build.name, signIn.started, signIn.authorized, signIn.called)
			}
			observed := listless.servedCookieWrites(t, signIn.cookie)
			observed = append(observed, listless.forwardedHeaderWrites(t, signIn.cookie)...)
			steps, _ := listless.servedSignIn(t)
			observed = append(observed, steps...)
			observed = append(observed, listless.servedProofs(t)...)
			for _, observation := range observed {
				t.Logf("%s: %s: %s", build.name, observation.request, observation.answer)
			}
			answered = append(answered, observed)
			listless.process.Terminate(t)
			listless.process.WaitExit(t, "SIGTERM")
		}
		for index, observation := range answered[0] {
			if baseAnswer := answered[1][index]; observation.answer != baseAnswer.answer {
				t.Errorf("%s: this build with no list answered %s, the base commit's build %s", observation.request, observation.answer, baseAnswer.answer)
			}
		}
	})
}

// serveConnect serves the HTTP CONNECT proxy the dispatch command reaches the example hosts
// through and answers its address: a CONNECT to any *.example.test:443 is a tunnel to the front
// door, dialled from 127.0.0.7, and any other request is refused.
func (s *e2eServer) serveConnect(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(e2eCallerSeven)}, Timeout: 10 * time.Second}
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, port, err := net.SplitHostPort(r.Host)
		if r.Method != http.MethodConnect || err != nil || port != "443" || !strings.HasSuffix(strings.ToLower(host), ".example.test") {
			http.Error(w, "this proxy reaches the example hosts on port 443 alone", http.StatusForbidden)
			return
		}
		upstream, err := dialer.Dial("tcp", s.frontDoor)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		conn, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
			return
		}
		go func() {
			_, _ = io.Copy(upstream, buffered)
			_ = upstream.(*net.TCPConn).CloseWrite()
		}()
		_, _ = io.Copy(conn, upstream)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String()
}

// proxySettings are the server of c-l-served's settings with ranges as DISPATCH_TRUSTED_PROXY_RANGES
// ("" leaves the setting unset: its default, the empty list) and the shared agent token's current
// value then its previous one.
func (r *e2eRig) proxySettings(ranges string) map[string]string {
	settings := r.servedSettings()
	settings["DISPATCH_AGENT_TOKEN"] = dispatchTestAgentToken + "\n" + e2ePreviousToken
	if ranges != "" {
		settings["DISPATCH_TRUSTED_PROXY_RANGES"] = ranges
	}
	return settings
}

// TestTrustedProxyLive runs c-l-proxy on the real process: the server of c-l-served with
// DISPATCH_TRUSTED_PROXY_RANGES=127.0.0.1/32, the front door's own address, behind which callers
// connect from 127.0.0.7 and 127.0.0.8.
func TestTrustedProxyLive(t *testing.T) {
	rig := newE2ERig(t)
	server := rig.start(t, rig.dispatchBin, rig.proxySettings("127.0.0.1/32"))

	t.Run("c-l-proxy (1) a caller's own X-Forwarded-For entry is not its address", func(t *testing.T) {
		const userAgent = "served-addresses-e2e c-l-proxy (1)"
		answer := server.send(t, server.caller, http.MethodGet, e2eServed+"/auth/whoami", http.Header{"X-Forwarded-For": {"198.51.100.66"}, "User-Agent": {userAgent}}, "")
		line := server.requestLineOf(t, userAgent)
		t.Logf("GET /auth/whoami from %s through the front door, the caller writing X-Forwarded-For: 198.51.100.66: %s; its line's address %s", e2eCallerSeven, e2eSummary(answer), line["address"])
		if line["address"] != e2eCallerSeven {
			t.Errorf("the request line's address is %s, want %s, the caller the front door appended", line["address"], e2eCallerSeven)
		}
	})

	t.Run("c-l-proxy (2) each caller behind the front door has its own broker-token bucket", func(t *testing.T) {
		seven := ciphertokentest.NewCaller(t)
		eight := ciphertokentest.NewCaller(t)
		rig.holds("proxied-agents", e2eProxiedToken, eight)
		const sevenAgent, eightAgent = "served-addresses-e2e c-l-proxy (2) at 127.0.0.7", "served-addresses-e2e c-l-proxy (2) at 127.0.0.8"
		began := time.Now()
		sent, limited := 0, false
		for sent < 200 && !limited {
			header := e2eProof(t, seven, e2eServed, fmt.Sprintf("served-addresses-e2e-never-issued-%d", sent))
			header.Set("User-Agent", sevenAgent)
			answer := server.send(t, server.caller, http.MethodGet, e2eServed+"/api/v1/whoami", header, "")
			sent++
			switch summary := e2eSummary(answer); summary {
			case "429 BROKER_TOKEN_RATE_LIMITED":
				limited = true
			case "401 BROKER_TOKEN_NOT_ACTIVE":
			default:
				t.Fatalf("token %d from %s answered %s, want 401 BROKER_TOKEN_NOT_ACTIVE until its bucket runs out", sent, e2eCallerSeven, summary)
			}
		}
		elapsed := time.Since(began)
		t.Logf("%d distinct tokens the broker never issued, from %s through the front door in %v (%.0f a second): limited %t", sent, e2eCallerSeven, elapsed.Round(time.Millisecond), float64(sent)/elapsed.Seconds(), limited)
		if !limited {
			t.Fatalf("%d tokens from %s never ran out its bucket", sent, e2eCallerSeven)
		}
		header := e2eProof(t, eight, e2eServed, e2eProxiedToken)
		header.Set("User-Agent", eightAgent)
		answer := server.send(t, rig.client(t, server.frontDoor, e2eCallerEight, nil), http.MethodGet, e2eServed+"/api/v1/whoami", header, "")
		t.Logf("at once, a valid token no request had presented, from %s: %s", e2eCallerEight, e2eSummary(answer))
		if e2eSummary(answer) != e2eWhoamiAnswered {
			t.Errorf("the valid token from %s answered %s, want 200 from a bucket of its own", e2eCallerEight, answer)
		}
		limitedLine := server.waitForLine(t, 0, accesslog.Message, "the 429", func(line map[string]string) bool {
			return line["user_agent"] == sevenAgent && line["status"] == "429"
		})
		eightLine := server.requestLineOf(t, eightAgent)
		t.Logf("the 429's line names address %s; the valid token's names %s", limitedLine["address"], eightLine["address"])
		if limitedLine["address"] != e2eCallerSeven || eightLine["address"] != e2eCallerEight {
			t.Errorf("the lines name the addresses %s and %s, want %s and %s", limitedLine["address"], eightLine["address"], e2eCallerSeven, e2eCallerEight)
		}
	})

	t.Run("c-l-proxy (3) a peer in no configured range keeps the rightmost entry", func(t *testing.T) {
		for _, ranges := range []string{"", "192.0.2.0/24"} {
			name := "DISPATCH_TRUSTED_PROXY_RANGES=" + ranges
			if ranges == "" {
				name = "DISPATCH_TRUSTED_PROXY_RANGES unset"
			}
			t.Run(name, func(t *testing.T) {
				other := rig.start(t, rig.dispatchBin, rig.proxySettings(ranges))
				headerAgent := "served-addresses-e2e c-l-proxy (3) header, " + name
				answer := other.direct(t, http.MethodGet, "/auth/whoami", "", http.Header{"X-Forwarded-For": {"198.51.100.66"}, "User-Agent": {headerAgent}}, "")
				headerLine := other.requestLineOf(t, headerAgent)
				t.Logf("from 127.0.0.1 with X-Forwarded-For: 198.51.100.66: %s; its line's address %s", e2eSummary(answer), headerLine["address"])

				previousAgent := "served-addresses-e2e c-l-proxy (3) previous token, " + name
				answer = other.direct(t, http.MethodGet, "/api/v1/whoami", "", http.Header{
					"Authorization": {"Bearer " + e2ePreviousToken}, "X-Forwarded-For": {"198.51.100.8, 198.51.100.66"}, "User-Agent": {previousAgent},
				}, "")
				previousLine := other.requestLineOf(t, previousAgent)
				noted := other.waitForLine(t, 0, e2ePreviousTokenLine, "the request on the previous token", func(line map[string]string) bool {
					return line["user_agent"] == previousAgent
				})
				t.Logf("from 127.0.0.1 on the previous shared token with X-Forwarded-For: 198.51.100.8, 198.51.100.66: %s; its request line's address %s, its previous-token line's %s",
					e2eSummary(answer), previousLine["address"], noted["address"])
				if answer.status != http.StatusOK {
					t.Errorf("the request on the previous shared token answered %s, want 200", answer)
				}
				for _, line := range []struct {
					what, address string
				}{{"the header request's line", headerLine["address"]}, {"the previous-token request's line", previousLine["address"]}, {"the previous-token line", noted["address"]}} {
					if line.address != "198.51.100.66" {
						t.Errorf("%s names address %s, want 198.51.100.66, the rightmost entry, as a peer in no range is read", line.what, line.address)
					}
				}
			})
		}
	})

	t.Run("c-l-proxy (4) a header the front door could not have written is refused", func(t *testing.T) {
		asked := rig.broker.Count()
		for _, row := range []struct {
			name      string
			forwarded []string
		}{
			{"two X-Forwarded-For lines, 198.51.100.8 then 198.51.100.66", []string{"198.51.100.8", "198.51.100.66"}},
			{"the single entry 198.51.100.8", []string{"198.51.100.8"}},
		} {
			// Once more than a bucket holds, each with a token the broker never issued: had one
			// reached the broker token check, the bucket of the address it keyed on would spend.
			userAgent := "served-addresses-e2e c-l-proxy (4) " + row.name
			for attempt := range ciphertoken.DefaultAddressLimit.Burst + 1 {
				answer := server.direct(t, http.MethodGet, "/api/v1/whoami", "b.example.test", http.Header{
					"X-Forwarded-For": row.forwarded, "Authorization": {fmt.Sprintf("DPoP served-addresses-e2e-refused-%d", attempt)}, "User-Agent": {userAgent},
				}, "")
				if summary := e2eSummary(answer); summary != "400 FORWARDED_FOR_INVALID" {
					t.Fatalf("request %d from 127.0.0.1 with %s answered %s, want 400 FORWARDED_FOR_INVALID", attempt, row.name, summary)
				}
			}
			line := server.requestLineOf(t, userAgent)
			t.Logf("from 127.0.0.1 with %s: 400 FORWARDED_FOR_INVALID, %d times; the line: route=%s address=%s status=%s",
				row.name, ciphertoken.DefaultAddressLimit.Burst+1, line["route"], line["address"], line["status"])
			if line["route"] != accesslog.RefusedRoute || line["address"] != "127.0.0.1" || line["status"] != "400" {
				t.Errorf("the refused request's line names route %s, address %s, status %s; want %s, 127.0.0.1 and 400", line["route"], line["address"], line["status"], accesslog.RefusedRoute)
			}
		}
		if got := rig.broker.Count() - asked; got != 0 {
			t.Errorf("the refused requests asked the broker %d times, want none", got)
		}
		answer := server.direct(t, http.MethodGet, "/api/v1/whoami", "b.example.test", http.Header{
			"X-Forwarded-For": {"198.51.100.8, " + e2eForwardRule}, "Authorization": {"DPoP served-addresses-e2e-keyed-on-198.51.100.8"},
		}, "")
		t.Logf("then a token keyed on 198.51.100.8 (X-Forwarded-For: 198.51.100.8, %s from 127.0.0.1): %s", e2eForwardRule, e2eSummary(answer))
		if e2eSummary(answer) != "401 BROKER_TOKEN_NOT_ACTIVE" {
			t.Errorf("a token keyed on 198.51.100.8 after the refusals answered %s, want 401 BROKER_TOKEN_NOT_ACTIVE from a bucket no refusal spent", answer)
		}
	})

	t.Run("c-l-proxy (5) a start with a range that is no network, or that trusts every peer, is refused", func(t *testing.T) {
		database := storetest.Open(t)
		for _, row := range []struct{ ranges, naming string }{
			{"10.0.0.1/8", "DISPATCH_TRUSTED_PROXY_RANGES entry 1 of 1 must be written as the network it names, 10.0.0.0/8"},
			{"0.0.0.0/0", "DISPATCH_TRUSTED_PROXY_RANGES entry 1 of 1 trusts every peer's X-Forwarded-For"},
			{"::/0", "DISPATCH_TRUSTED_PROXY_RANGES entry 1 of 1 trusts every peer's X-Forwarded-For"},
			{"not-a-cidr", "DISPATCH_TRUSTED_PROXY_RANGES entry 1 of 1 must be a CIDR"},
		} {
			t.Run("DISPATCH_TRUSTED_PROXY_RANGES="+row.ranges, func(t *testing.T) {
				rig.requireRefusedStart(t, database.Pool.Config().ConnString(), "DISPATCH_TRUSTED_PROXY_RANGES="+row.ranges, rig.proxySettings(row.ranges), row.naming)
			})
		}
	})
}
