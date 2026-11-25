// result_write_test.go drives the REAL compiled broker's airlock with a CI run's result write: the
// run trades its token for a placeholder (result_grant_test.go), and a write carrying the
// placeholder through the broker's proxy port reaches a TLS stand-in for Dispatch with the run's
// results token, which the broker pod minted for itself, and a DPoP proof of that pod's own key.
// The stand-in is the broker's private destination (resultsUpstream, whose URL names the bound host
// itself): BROKER_AIRLOCK_TEST_RESOLVE names where that host is, BROKER_AIRLOCK_TEST_DIAL_ALLOW lets
// the proxy dial that loopback address, and BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE trusts its
// certificate.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// resultWritePath is the results route a run's write is sent to.
const resultWritePath = "/api/v1/projects/" + project + "/results"

// resultsDispatch is the TLS stand-in for Dispatch's public host, which records every request
// the proxy sends it and answers each 201.
type resultsDispatch struct {
	service *proxyrulestest.Service
	caFile  string
}

func startResultsDispatch(t *testing.T) *resultsDispatch {
	t.Helper()
	service, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Hosts: []string{resultsBoundHost}})
	if err != nil {
		t.Fatalf("start the stand-in for Dispatch: %v", err)
	}
	t.Cleanup(service.Close)
	service.AnswerWith(func(w http.ResponseWriter, _ *http.Request, _ proxyrulestest.Request) bool {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"stored":true}`)
		return true
	})
	caFile := filepath.Join(t.TempDir(), "dispatch-ca.pem")
	if err := os.WriteFile(caFile, service.Authority().CertificatePEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	return &resultsDispatch{service: service, caFile: caFile}
}

// env is what sends the private destination's requests to this stand-in: its loopback address and
// port, the one dial the proxy may make there, and the authority its certificate is signed by.
func (d *resultsDispatch) env() []string {
	return []string{
		"BROKER_AIRLOCK_TEST_RESOLVE=" + resultsBoundHost + "=" + d.service.Addr(),
		"BROKER_AIRLOCK_TEST_DIAL_ALLOW=" + resultsBoundHost + ":443",
		"BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE=" + d.caFile,
	}
}

// tokens is the token each request the stand-in received carried in Authorization, in order.
func (d *resultsDispatch) tokens(t *testing.T) []string {
	t.Helper()
	var tokens []string
	for _, seen := range d.service.Requests() {
		values := seen.Header.Values("Authorization")
		token, ok := strings.CutPrefix(strings.Join(values, ","), "DPoP ")
		if len(values) != 1 || !ok || token == "" {
			t.Fatalf("the stand-in for Dispatch received Authorization %q, want one DPoP token", values)
		}
		tokens = append(tokens, token)
	}
	return tokens
}

// resultsEnvironment is a broker whose result entry's writes go through its airlock to the
// stand-in: its database, the two issuers and the GitHub key a run's token is signed with, and
// the API account's token an introspection presents.
type resultsEnvironment struct {
	databaseURL string
	env         []string
	github      *oidctest.Issuer
	githubKey   *oidctest.Key
	apiToken    string
	dispatch    *resultsDispatch
}

func newResultsEnvironment(t *testing.T, extra ...string) *resultsEnvironment {
	t.Helper()
	databaseURL := storetest.URL(t)
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	dispatch := startResultsDispatch(t)
	env := append(resultsExchangeEnv(t, databaseURL, cluster, github, longAccessSeconds), dispatch.env()...)
	return &resultsEnvironment{
		databaseURL: databaseURL, env: append(env, extra...), github: github,
		githubKey: github.PublishKey(t, "github-key"), apiToken: accountToken(t, cluster, clusterKey, apiAccount), dispatch: dispatch,
	}
}

// resultsBroker is one broker process of a resultsEnvironment: its proxy port and the thumbprint
// of the pod key it logged at start.
type resultsBroker struct {
	*brokerProcess
	proxyAddr, thumbprint string
}

func (e *resultsEnvironment) start(t *testing.T) *resultsBroker {
	t.Helper()
	broker := startBroker(t, brokerOptions{Env: e.env})
	broker.AwaitRecord(t, "token exchange", map[string]any{"state": "on", "github": true, "result_workflows": 1})
	thumbprint, _ := broker.AwaitRecord(t, dpopKeyLine, nil)["thumbprint"].(string)
	if thumbprint == "" {
		t.Fatalf("the broker's %q line names no thumbprint", dpopKeyLine)
	}
	return &resultsBroker{brokerProcess: broker, proxyAddr: airlockProxyAddr(t, broker), thumbprint: thumbprint}
}

// resultGrant is what a run holds after its exchange: the placeholder, its grant's id, and the
// airlock authority's certificate its client trusts the proxy by.
type resultGrant struct {
	placeholder, grantID, caPEM string
}

// grant is run's exchange of its token for a placeholder at b.
func (e *resultsEnvironment) grant(t *testing.T, b *resultsBroker, run string) resultGrant {
	t.Helper()
	status, body := postTokenAt(t, b.Addr, "http://"+b.Addr, askedForThePlaceholder(runForm(mintRunToken(t, e.github, e.githubKey, resultsWorkflow, resultsBranch, run), "")))
	held, _ := body["access_token"].(string)
	grantID, _ := body[oauthwire.ExtraGrantID].(string)
	airlock, _ := body[oauthwire.ExtraAirlock].(map[string]any)
	caPEM, _ := airlock["ca_certificate"].(string)
	if status != http.StatusOK || !strings.HasPrefix(held, placeholder.MintedPrefix(oauthwire.AirlockResultsWord)) || grantID == "" || caPEM == "" {
		t.Fatalf("run %s's placeholder exchange = %d %v, want 200 with a result placeholder, its grant and the airlock's certificate", run, status, body)
	}
	return resultGrant{placeholder: held, grantID: grantID, caPEM: caPEM}
}

// client is a command's own client of g through b's proxy: the proxy as its HTTPS_PROXY with the
// placeholder as the password, and the airlock's authority as the one thing it trusts.
func (g resultGrant) client(t *testing.T, b *resultsBroker) *http.Client {
	t.Helper()
	anchor := x509.NewCertPool()
	if !anchor.AppendCertsFromPEM([]byte(g.caPEM)) {
		t.Fatal("the airlock authority's certificate did not parse")
	}
	proxyURL, err := url.Parse("http://agent:" + g.placeholder + "@" + b.proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: anchor, MinVersion: tls.VersionTLS12},
	}}
}

// write is one result write of g through client, with the placeholder as its one Authorization
// header, answering the answer and its body.
func (g resultGrant) write(t *testing.T, client *http.Client) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "https://"+resultsBoundHost+resultWritePath, strings.NewReader(`{"verdict":"pass"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "DPoP "+g.placeholder)
	answer, err := client.Do(request)
	if err != nil {
		t.Fatalf("the result write through the airlock's proxy: %v", err)
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer, string(body)
}

// stored fails t unless the write reached the stand-in and its answer came back as Dispatch's.
func stored(t *testing.T, what string, answer *http.Response, body string) {
	t.Helper()
	if answer.StatusCode != http.StatusCreated || answer.Header.Get("Cipher-Proxy-Forwarded") != "true" {
		t.Fatalf("%s = %d, Cipher-Proxy-Forwarded %q, Cipher-Proxy-Code %q (%s); want the stand-in's 201, forwarded", what, answer.StatusCode, answer.Header.Get("Cipher-Proxy-Forwarded"), answer.Header.Get("Cipher-Proxy-Code"), body)
	}
}

// introspect is the API account's reading of token at b.
func (e *resultsEnvironment) introspect(t *testing.T, b *resultsBroker, token string) oauthwire.Introspection {
	t.Helper()
	return introspected(t, newPlainBroker(b.Addr), token, e.apiToken)
}

// staticAccount is the API account's token, presented as a Verifier's bearer.
type staticAccount string

func (a staticAccount) Read() (string, error) { return string(a), nil }

// spentProofs is a Verifier's replay store in memory.
type spentProofs struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (s *spentProofs) CheckAndSetDPoPProofUsed(_ context.Context, jti, _, _, _, htu string, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	key := jti + "\x00" + htu
	used := s.seen[key]
	s.seen[key] = true
	return used, nil
}

// TestAResultWriteLeavesTheProxyWithTheRunsTokenAndAProofOfThePodsKey: a run's write carrying
// its placeholder reaches Dispatch with a token that is not the placeholder, which the broker
// reads back as a live results token of the run, bound to the key whose thumbprint the broker
// logged at start; and Dispatch's own verifier, with Dispatch's public origin, accepts the write
// as it arrived.
func TestAResultWriteLeavesTheProxyWithTheRunsTokenAndAProofOfThePodsKey(t *testing.T) {
	e := newResultsEnvironment(t)
	b := e.start(t)
	g := e.grant(t, b, "7")
	answer, body := g.write(t, g.client(t, b))
	stored(t, "the write", answer, body)
	tokens := e.dispatch.tokens(t)
	if len(tokens) != 1 || tokens[0] == g.placeholder || strings.HasPrefix(tokens[0], placeholder.Prefix) {
		t.Fatalf("the stand-in for Dispatch received tokens %q, want one that is no placeholder", tokens)
	}
	got := e.introspect(t, b, tokens[0])
	if !got.Active || got.Scope != oauthwire.ScopeResults || got.CI == nil || got.CI.CheckRunID != "97" || got.Thumbprint != b.thumbprint {
		t.Fatalf("the token Dispatch received introspects as %+v (run %+v); want a live results token of check run 97 bound to the pod key %s", got, got.CI, b.thumbprint)
	}
	verifier, err := ciphertoken.New(ciphertoken.Options{
		BrokerURL: "http://" + b.Addr, Account: staticAccount(e.apiToken), Origin: "https://" + resultsBoundHost,
		Replay: &spentProofs{}, Address: func(*http.Request) string { return "127.0.0.1" }, ServesResults: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := e.dispatch.service.Requests()[0]
	arrived, err := http.NewRequest(seen.Method, "http://10.0.0.5:8766"+seen.Path, strings.NewReader(seen.Body))
	if err != nil {
		t.Fatal(err)
	}
	arrived.Header = seen.Header.Clone()
	caller, err := verifier.Verify(t.Context(), arrived)
	if err != nil || !caller.ResultsOnly {
		t.Fatalf("Dispatch's verifier with origin https://%s = %+v, %v; want the write accepted as a results token's", resultsBoundHost, caller, err)
	}
}

// TestTwoBrokersMintTheirOwnTokensForOneGrant: a grant made at broker A serves writes through
// broker B and then through A, the two processes on one database: each pod mints its own token,
// bound to its own key, and neither token's text is kept in any table.
func TestTwoBrokersMintTheirOwnTokensForOneGrant(t *testing.T) {
	e := newResultsEnvironment(t)
	a, b := e.start(t), e.start(t)
	if a.thumbprint == b.thumbprint {
		t.Fatalf("the two brokers logged one pod key %s, want one each", a.thumbprint)
	}
	g := e.grant(t, a, "7")
	for _, through := range []*resultsBroker{b, a} {
		answer, body := g.write(t, g.client(t, through))
		stored(t, "a write of A's grant", answer, body)
	}
	tokens := e.dispatch.tokens(t)
	if len(tokens) != 2 || tokens[0] == tokens[1] {
		t.Fatalf("the stand-in for Dispatch received tokens %q, want two that differ", tokens)
	}
	for i, through := range []*resultsBroker{b, a} {
		if got := e.introspect(t, a, tokens[i]); !got.Active || got.Thumbprint != through.thumbprint {
			t.Errorf("write %d's token introspects as %+v; want it live and bound to its own broker's key %s", i+1, got, through.thumbprint)
		}
	}
	conn, err := pgx.Connect(t.Context(), e.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	for _, token := range tokens {
		for _, table := range []string{"result_grants", "oauth_access_tokens"} {
			var held int
			if err := conn.QueryRow(t.Context(), `select count(*) from `+table+` t where strpos(row_to_json(t)::text, $1) > 0`, token).Scan(&held); err != nil {
				t.Fatal(err)
			}
			if held != 0 {
				t.Errorf("%d rows of %s hold a minted token's text", held, table)
			}
		}
	}
}

// TestAResultGrantOutlivesTheBrokerThatMadeIt: broker A makes the grant and stops; broker B,
// started afterwards on the same database, serves the grant's write.
func TestAResultGrantOutlivesTheBrokerThatMadeIt(t *testing.T) {
	e := newResultsEnvironment(t)
	a := e.start(t)
	g := e.grant(t, a, "7")
	a.Terminate(t)
	a.WaitExit(t, "SIGTERM")
	b := e.start(t)
	answer, body := g.write(t, g.client(t, b))
	stored(t, "the write through B after A stopped", answer, body)
}

// TestTwoRunsNeverShareACapability: two runs' grants through one broker: the token each write
// reaches Dispatch with is its own run's.
func TestTwoRunsNeverShareACapability(t *testing.T) {
	e := newResultsEnvironment(t)
	b := e.start(t)
	first, second := e.grant(t, b, "1"), e.grant(t, b, "2")
	for _, g := range []resultGrant{first, second, first} {
		answer, body := g.write(t, g.client(t, b))
		stored(t, "a write of "+g.grantID, answer, body)
	}
	for i, want := range []string{"91", "92", "91"} {
		got := e.introspect(t, b, e.dispatch.tokens(t)[i])
		if !got.Active || got.CI == nil || got.CI.CheckRunID != want {
			t.Errorf("write %d's token introspects as %+v (run %+v); want check run %s's", i+1, got, got.CI, want)
		}
	}
}

// rawConnect is one CONNECT to the bound host through b's proxy, with password as the proxy
// password, answering the proxy's answer.
func rawConnect(t *testing.T, b *resultsBroker, password string) *http.Response {
	t.Helper()
	conn, err := net.Dial("tcp", b.proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	target := resultsBoundHost + ":443"
	credential := base64.StdEncoding.EncodeToString([]byte("agent:" + password))
	if _, err := conn.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\nProxy-Authorization: Basic " + credential + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	answer, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read the answer to CONNECT %s: %v", target, err)
	}
	return answer
}

// TestAnExpiredResultGrantEndsTheProxysUse: with a grant of two seconds, a write passes; once the
// grant has ended, a CONNECT on a new connection and a write on the connection already open are
// each answered 407 GRANT_NOT_LIVE by the proxy itself, and the token the pod minted for the
// grant introspects inactive.
func TestAnExpiredResultGrantEndsTheProxysUse(t *testing.T) {
	e := newResultsEnvironment(t, "BROKER_RESULT_GRANT_TEST_SECONDS=2")
	b := e.start(t)
	g := e.grant(t, b, "7")
	client := g.client(t, b)
	answer, body := g.write(t, client)
	stored(t, "the write inside the grant", answer, body)
	token := e.dispatch.tokens(t)[0]
	time.Sleep(3 * time.Second)
	connect := rawConnect(t, b, g.placeholder)
	if connect.StatusCode != http.StatusProxyAuthRequired || connect.Header.Get("Cipher-Proxy-Forwarded") != "false" || connect.Header.Get("Cipher-Proxy-Code") != "GRANT_NOT_LIVE" {
		t.Errorf("a CONNECT after the grant ended = %d, Cipher-Proxy-Forwarded %q, Cipher-Proxy-Code %q; want 407, false, GRANT_NOT_LIVE", connect.StatusCode, connect.Header.Get("Cipher-Proxy-Forwarded"), connect.Header.Get("Cipher-Proxy-Code"))
	}
	answer, body = g.write(t, client)
	if answer.StatusCode != http.StatusProxyAuthRequired || answer.Header.Get("Cipher-Proxy-Forwarded") != "false" || answer.Header.Get("Cipher-Proxy-Code") != "GRANT_NOT_LIVE" {
		t.Errorf("a write on the open connection after the grant ended = %d, Cipher-Proxy-Forwarded %q, Cipher-Proxy-Code %q (%s); want 407, false, GRANT_NOT_LIVE", answer.StatusCode, answer.Header.Get("Cipher-Proxy-Forwarded"), answer.Header.Get("Cipher-Proxy-Code"), body)
	}
	if received := len(e.dispatch.service.Requests()); received != 1 {
		t.Errorf("the stand-in for Dispatch received %d writes, want the one inside the grant", received)
	}
	if got := e.introspect(t, b, token); got.Active {
		t.Errorf("the token minted for the ended grant introspects as %+v, want inactive", got)
	}
}

// TestTheResultsTokenIntrospectsAsResultsAlone: the token a pod minted for a run's grant, which
// Dispatch receives, reads back as the results token of the run and nothing more: the scope
// results and the project scope results, the entry's person and project, the workflow's session,
// the run whole, and the pod's key.
func TestTheResultsTokenIntrospectsAsResultsAlone(t *testing.T) {
	e := newResultsEnvironment(t)
	b := e.start(t)
	g := e.grant(t, b, "7")
	answer, body := g.write(t, g.client(t, b))
	stored(t, "the write", answer, body)
	got := e.introspect(t, b, e.dispatch.tokens(t)[0])
	run := oauthwire.CIRun{
		Repository: exchangeRepository, WorkflowRef: exchangeRepository + "/" + resultsWorkflow + "@" + resultsBranch, Ref: resultsBranch,
		SHA: commitSHA, EventName: "push", RunID: "7", RunAttempt: "1", RunnerEnvironment: "github-hosted", CheckRunID: "97",
	}
	if got.CI == nil || *got.CI != run {
		t.Fatalf("introspection answered the run %+v, want %+v", got.CI, run)
	}
	got.CI = nil
	want := oauthwire.Introspection{Active: true, Scope: "results", ClientID: oauthwire.ExchangeClientID, Thumbprint: b.thumbprint, Session: resultsSession, Person: person, ProjectScope: "results", Projects: []string{project}, ExpiresAt: got.ExpiresAt}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("introspection answered %+v, want %+v: a minted results token holds the scope results and the project scope results, names the entry's person, its project and the workflow's session, and is bound to the pod's key", got, want)
	}
	if got.ExpiresAt <= time.Now().Unix() {
		t.Errorf("introspection answered exp %d, which is not in the future", got.ExpiresAt)
	}
}
