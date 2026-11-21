package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/busprojects"
	"github.com/sjawhar/envoy/internal/dispatch/busprojects/busprojectstest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/natsgrant"
)

const (
	// readinessAudience is the audience Dispatch's own verifier takes (DISPATCH_OIDC_AUDIENCE),
	// which the token an operator reads the go-live report with carries.
	readinessAudience = "dispatch"
	// readinessAccount is Dispatch's own service account (DISPATCH_SERVICE_ACCOUNT), the one
	// caller GET /private-readiness answers, and readinessOtherAccount another workload's.
	readinessAccount      = "system:serviceaccount:dispatch:dispatch"
	readinessOtherAccount = "system:serviceaccount:dispatch:envoy-broker"
	// emptyReadinessReport is the go-live report of a deployment with no private hub.
	emptyReadinessReport = `{"hubs":[],"listeners":[]}`
	// plainBearer is a bearer that is no service-account token: neither JWT-shaped nor one any
	// verifier reads, as a personal or shared token is.
	plainBearer = "personal-token"
)

// readinessIssuer is a service-account token issuer and Dispatch's verifier of its tokens for
// readinessAudience, discovered, and mint answers a token of subject for audience.
func readinessIssuer(t *testing.T) (*oidc.LazyVerifier, func(subject, audience string) string) {
	t.Helper()
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "signing-key")
	verifier := oidc.NewLazyVerifier(issuer.URL(), readinessAudience)
	if err := verifier.Discover(t.Context()); err != nil {
		t.Fatalf("discover the service-account token issuer: %v", err)
	}
	return verifier, func(subject, audience string) string {
		return issuer.Mint(t, key, issuer.Claims(subject, audience))
	}
}

// readinessCall sends GET /private-readiness to handler with authorization as its Authorization
// header, none when it is empty.
func readinessCall(handler http.Handler, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/private-readiness", nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// TestThePrivateReadinessRouteNeedsDispatchsCredential: GET /private-readiness answers only a
// verified projected service-account token of Dispatch's own account (DISPATCH_SERVICE_ACCOUNT):
// 401 NO_IDENTITY with no credential, the refusal every route gives a caller that carries none; 403
// DISPATCH_ACCOUNT_REQUIRED for a bearer that is no service-account token, a DPoP presentation,
// another account's verified token, and any token while the deployment names no account of its
// own; 401 OIDC_TOKEN_INVALID for a token minted for another audience; and 200 with the report for
// Dispatch's own. The router behind dispatchHandler is never reached. Control: mount the route
// without requireOwnAccount, and the anonymous call answers 200, which the first row names.
func TestThePrivateReadinessRouteNeedsDispatchsCredential(t *testing.T) {
	verifier, mint := readinessIssuer(t)
	router := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("the router served %s %s", r.Method, r.URL.Path)
	})
	handler := dispatchHandler(router, metrics.New(), nil, nil, nil, untouchedFiles{}, "", nil, issuers{serviceTokens: verifier}, nil, nil,
		newPrivateReadiness(bootConfig{ServiceAccount: readinessAccount}, nil))
	own := "Bearer " + mint(readinessAccount, readinessAudience)
	for _, row := range []struct {
		name, authorization string
		status              int
		code, naming        string
	}{
		{"no credential", "", http.StatusUnauthorized, "NO_IDENTITY", ""},
		{"a bearer that is no service-account token", "Bearer " + plainBearer, http.StatusForbidden, "DISPATCH_ACCOUNT_REQUIRED", ""},
		{"a DPoP presentation", "DPoP eyJhbGciOiJFUzI1NiJ9.e30.c2ln", http.StatusForbidden, "DISPATCH_ACCOUNT_REQUIRED", ""},
		{"another account's token", "Bearer " + mint(readinessOtherAccount, readinessAudience), http.StatusForbidden, "DISPATCH_ACCOUNT_REQUIRED", readinessOtherAccount},
		{"Dispatch's own account for another audience", "Bearer " + mint(readinessAccount, "envoy-nats"), http.StatusUnauthorized, "OIDC_TOKEN_INVALID", ""},
		{"Dispatch's own account", own, http.StatusOK, "", ""},
	} {
		response := readinessCall(handler, row.authorization)
		body := strings.TrimSpace(response.Body.String())
		if response.Code != row.status || errorCode(response) != row.code || !strings.Contains(body, row.naming) {
			t.Errorf("GET /private-readiness with %s = %d %s, want %d %s naming %q", row.name, response.Code, body, row.status, row.code, row.naming)
			continue
		}
		if row.status == http.StatusOK && body != emptyReadinessReport {
			t.Errorf("GET /private-readiness with %s = %s, want the empty report %s", row.name, body, emptyReadinessReport)
		}
	}
	// A deployment that names no account of its own answers no token, Dispatch's included.
	unnamed := dispatchHandler(router, metrics.New(), nil, nil, nil, untouchedFiles{}, "", nil, issuers{serviceTokens: verifier}, nil, nil, newPrivateReadiness(bootConfig{}, nil))
	if response := readinessCall(unnamed, own); response.Code != http.StatusForbidden || errorCode(response) != "DISPATCH_ACCOUNT_REQUIRED" {
		t.Errorf("GET /private-readiness with no DISPATCH_SERVICE_ACCOUNT = %d %s, want 403 DISPATCH_ACCOUNT_REQUIRED", response.Code, response.Body.String())
	}
	// And one with no verifier, whatever the account, answers no token either.
	unverified := dispatchHandler(router, metrics.New(), nil, nil, nil, untouchedFiles{}, "", nil, issuers{}, nil, nil, newPrivateReadiness(bootConfig{ServiceAccount: readinessAccount}, nil))
	if response := readinessCall(unverified, own); response.Code != http.StatusForbidden || errorCode(response) != "DISPATCH_ACCOUNT_REQUIRED" {
		t.Errorf("GET /private-readiness with no DISPATCH_OIDC_AUDIENCE = %d %s, want 403 DISPATCH_ACCOUNT_REQUIRED", response.Code, response.Body.String())
	}
}

// TestThePrivateReadinessRouteServesTheReport: a proof over a tagged hub, whose stand-in auth
// service admits Dispatch's hub-admission token, and two listener workloads, one ready
// (busprojectstest.ReadyListener) and one whose replica answers that its read failed, is served at
// GET /private-readiness to Dispatch's own account: 200, the hub admitted, the ready replica
// answered and the other failed, each named. /healthz's status is the one a handler with no proof
// answers, so a failed proof takes no pod out of service. giveListeners, main's wiring, hands the
// proof its inventory and its client. Control: drop the route's mount, and GET /private-readiness
// falls through to the router, 404, which the test names.
func TestThePrivateReadinessRouteServesTheReport(t *testing.T) {
	verifier, mint := readinessIssuer(t)
	hub := busprojectstest.StartHub(t, true)
	admission := busprojects.NewHubAdmission([]*bus.Client{
		busprojectstest.ProofClient(t, hub.URL, busprojectstest.ProofToken(busprojectstest.DispatchAccount, natsgrant.HubAdmissionAudience)),
	})
	ready, readyClient := busprojectstest.ReadyListener(t)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != busprojects.ReadinessPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"instance":"failing-listener-0","required":true,"source":"`+busprojectstest.ReadyListenerSource+`","read":"failed"}`)
	}))
	t.Cleanup(failing.Close)
	listeners := append(ready, busprojects.ListenerTarget{
		Name: "failing-listener", Source: busprojectstest.ReadyListenerSource, Host: "failing-listener.example.invalid", Instances: failing.Listener.Addr().String(),
	})
	// One client reads both: the ready replica is its stand-in transport, the other a real server.
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == ready[0].Instances {
			return readyClient.Transport.RoundTrip(r)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	giveListeners(admission, listeners, client)
	if err := admission.Prove(t.Context()); err == nil || !strings.Contains(err.Error(), "failing-listener") {
		t.Fatalf("the proof with a failing replica answered %v, want an error naming failing-listener", err)
	}

	router := http.NotFoundHandler()
	boot := bootConfig{ServiceAccount: readinessAccount}
	handler := dispatchHandler(router, metrics.New(), nil, nil, nil, nil, "", nil, issuers{serviceTokens: verifier}, nil, nil, newPrivateReadiness(boot, admission))
	response := readinessCall(handler, "Bearer "+mint(readinessAccount, readinessAudience))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /private-readiness = %d %s, want 200 and the report", response.Code, response.Body.String())
	}
	var report busprojects.Report
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode the report %s: %v", response.Body.String(), err)
	}
	if len(report.Hubs) != 1 || report.Hubs[0].Admission != busprojects.AdmissionAdmitted {
		t.Errorf("the report's hubs = %+v, want the one tagged hub admitted", report.Hubs)
	}
	reads := map[string]string{}
	for _, replica := range report.Listeners {
		reads[replica.Workload] = replica.Read
	}
	if len(report.Listeners) != 2 || reads[ready[0].Name] != busprojects.ReadAnswered || reads["failing-listener"] != busprojects.ReadFailed {
		t.Errorf("the report's listeners = %+v, want %s answered and failing-listener failed", report.Listeners, ready[0].Name)
	}
	healthz := func(readiness privateReadiness) int {
		response := httptest.NewRecorder()
		dispatchHandler(router, metrics.New(), nil, nil, nil, nil, "", nil, issuers{serviceTokens: verifier}, nil, nil, readiness).
			ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return response.Code
	}
	if failed, none := healthz(newPrivateReadiness(boot, admission)), healthz(newPrivateReadiness(boot, nil)); failed != none {
		t.Errorf("/healthz with a failed proof = %d, with no proof %d: the report must not change the readiness probe's status", failed, none)
	}
}

// TestDispatchStartsAllPublicWithNoPrivateHub: with DISPATCH_PRIVATE_HUBS and
// DISPATCH_PRIVATE_LISTENERS unset, as in every all-public deployment and dev stack, Dispatch
// starts, /healthz answers 200, and GET /private-readiness answers Dispatch's own account 200 with
// the empty report: main hands no listener to the proof it does not run, and the route reads none.
// Control: drop giveListeners' guard, so it calls SetListeners on the nil proof; the start panics,
// and the test fails naming the process's exit.
func TestDispatchStartsAllPublicWithNoPrivateHub(t *testing.T) {
	database := storetest.Open(t)
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "pod-key")
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{Env: map[string]string{
		"DISPATCH_OIDC_ISSUER":     issuer.URL(),
		"DISPATCH_OIDC_AUDIENCE":   readinessAudience,
		"DISPATCH_SERVICE_ACCOUNT": readinessAccount,
	}})
	process.waitHealthy(t)
	if status, health := process.health(t); status != http.StatusOK || !health.OK {
		t.Fatalf("/healthz of an all-public deployment = %d %+v, want 200 ok", status, health)
	}
	request, err := http.NewRequest(http.MethodGet, process.url("/private-readiness"), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+issuer.Mint(t, key, issuer.Claims(readinessAccount, readinessAudience)))
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET /private-readiness: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != emptyReadinessReport {
		t.Fatalf("GET /private-readiness of an all-public deployment = %d %s, want 200 %s", response.StatusCode, body, emptyReadinessReport)
	}
}

// TestEachListenerReadPresentsThatListenersToken: the client each proof reads the listener replicas
// with (listenerHTTP) presents, on each read, the token of the entry whose host the read carries,
// read from that entry's token_file: each listener verifies a token for its own audience, so one
// token for every listener would be refused by all but one. A read whose Host names no entry, or
// whose entry's file cannot be read, sends nothing. Control: one token for every read; the second
// listener's read carries the first one's token, and the test names it.
func TestEachListenerReadPresentsThatListenersToken(t *testing.T) {
	seen := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Host + " " + r.Header.Get("Authorization")
	}))
	t.Cleanup(server.Close)
	file := func(name, token string) string { return writeFile(t, name, token+"\n") }
	listeners := []privateListener{
		{ListenerTarget: busprojects.ListenerTarget{Name: "one", Host: "one.example.invalid"}, TokenFile: file("one", "token-for-one")},
		{ListenerTarget: busprojects.ListenerTarget{Name: "two", Host: "two.example.invalid"}, TokenFile: file("two", "token-for-two")},
		{ListenerTarget: busprojects.ListenerTarget{Name: "gone", Host: "gone.example.invalid"}, TokenFile: filepath.Join(t.TempDir(), "missing")},
	}
	client := listenerHTTP(listeners)
	read := func(host string) error {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+busprojects.ReadinessPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = host
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
		}
		return err
	}
	for _, listener := range listeners[:2] {
		if err := read(listener.Host); err != nil {
			t.Fatalf("the read of %s: %v", listener.Name, err)
		}
		want := listener.Host + " Bearer token-for-" + listener.Name
		if got := <-seen; got != want {
			t.Errorf("the read of %s carried %q, want %q: each listener takes a token for its own audience alone", listener.Name, got, want)
		}
	}
	for _, host := range []string{"gone.example.invalid", "stranger.example.invalid"} {
		if err := read(host); err == nil {
			t.Errorf("a read with Host %s was sent, want it refused before it is sent", host)
		}
	}
	select {
	case got := <-seen:
		t.Errorf("a refused read reached the listener: %q", got)
	default:
	}
}

// roundTripFunc is a transport that answers with a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
