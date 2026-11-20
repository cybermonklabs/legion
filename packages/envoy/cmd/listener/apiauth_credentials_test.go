package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// The audience the alert sink asks its metadata server for a Google identity token for, and
	// the accounts this file's listener lists and does not list.
	listenerGoogleAudience   = "https://listener.example"
	listenerGoogleAccount    = "example-sink@example-project.iam.gserviceaccount.com"
	listenerGoogleOtherEmail = "example-other@example-project.iam.gserviceaccount.com"

	// The projected-token subjects this file's listener lists and does not list.
	listenerListedSubject   = "system:serviceaccount:dispatch:envoy-broker"
	listenerUnlistedSubject = "system:serviceaccount:legion:legion-worker"
)

// googleStandIn is a local issuer in Google's place and the Google identities a listener takes
// from it: what ENVOY_OIDC_GOOGLE_AUDIENCE and ENVOY_OIDC_GOOGLE_ACCOUNTS build, over an issuer a
// test can mint for.
type googleStandIn struct {
	issuer     *oidctest.Issuer
	key        *oidctest.Key
	identities *googleIdentities
}

func newGoogleStandIn(t *testing.T, accounts ...string) googleStandIn {
	t.Helper()
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "google-signing-key")
	verifier, err := oidc.New(context.Background(), issuer.URL(), listenerGoogleAudience)
	if err != nil {
		t.Fatalf("oidc.New(%q, %q): %v", issuer.URL(), listenerGoogleAudience, err)
	}
	return googleStandIn{issuer: issuer, key: key, identities: &googleIdentities{audience: listenerGoogleAudience, verifier: verifier, accounts: accounts}}
}

// token mints a Google identity token as a metadata server mints one with format=full: Google's
// numeric subject, the audience asked for, and the account's address with email_verified.
func (g googleStandIn) token(t *testing.T, account string, verified any, audience string) string {
	t.Helper()
	claims := g.issuer.Claims("118000000000000000001", audience)
	if account != "" {
		claims["email"] = account
		claims["email_verified"] = verified
	}
	return g.issuer.Mint(t, g.key, claims)
}

// clusterIssuer is the cluster's issuer of projected service-account tokens and the verifier a
// listener builds for it from ENVOY_OIDC_ISSUER and ENVOY_OIDC_AUDIENCE.
type clusterIssuer struct {
	issuer   *oidctest.Issuer
	key      *oidctest.Key
	verifier *oidc.Verifier
}

func newClusterIssuer(t *testing.T) clusterIssuer {
	t.Helper()
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "listener-test-key")
	verifier, err := oidc.New(context.Background(), issuer.URL(), listenerTestAudience)
	if err != nil {
		t.Fatalf("oidc.New(%q, %q): %v", issuer.URL(), listenerTestAudience, err)
	}
	return clusterIssuer{issuer: issuer, key: key, verifier: verifier}
}

func (c clusterIssuer) token(t *testing.T, subject string) string {
	t.Helper()
	return c.issuer.Mint(t, c.key, c.issuer.Claims(subject, listenerTestAudience))
}

// bearerRequest is a GET of path as a client on this machine sends it, presenting authorize as its
// Authorization header when it is not empty.
func bearerRequest(path, authorize string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Host = "127.0.0.1:9020"
	if authorize != "" {
		request.Header.Set("Authorization", authorize)
	}
	return request
}

var served = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

// googleRejectedMessage is the one line a refused Google identity token logs (googleIdentities.admits).
const googleRejectedMessage = "listener: Google identity token rejected"

// ENVOY_OIDC_ACCOUNTS holds the projected-token branch to the subjects it lists: any pod of the
// cluster can project a token for the listener's audience, so a verified token alone says nothing
// about which caller it is. With the list unset the branch admits every subject the verifier
// verifies, as it did before the list existed.
func TestOIDCAccountsLimitTheProjectedTokenBranch(t *testing.T) {
	cluster := newClusterIssuer(t)
	listedToken := cluster.token(t, listenerListedSubject)
	unlistedToken := cluster.token(t, listenerUnlistedSubject)

	cases := []struct {
		name       string
		accounts   []string
		token      string
		wantStatus int
		// wantReason is the class the refusal logs, and wantAccount the subject it names.
		wantReason, wantAccount string
	}{
		{name: "a listed subject is admitted", accounts: []string{listenerListedSubject}, token: listedToken, wantStatus: http.StatusNoContent},
		{name: "an unlisted subject is refused", accounts: []string{listenerListedSubject}, token: unlistedToken, wantStatus: http.StatusUnauthorized, wantReason: "not_listed", wantAccount: listenerUnlistedSubject},
		{name: "the list unset admits the listed subject as today", token: listedToken, wantStatus: http.StatusNoContent},
		{name: "the list unset admits the other subject as today", token: unlistedToken, wantStatus: http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			recorder := httptest.NewRecorder()
			authorize := "Bearer " + tc.token
			apiAuth(listenerAPIAuth{verifier: cluster.verifier, accounts: tc.accounts, logger: logging.NewWithWriter("test-machine", &logged)}, served).
				ServeHTTP(recorder, bearerRequest("/v1/sessions", authorize))

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if tc.wantStatus == http.StatusUnauthorized && recorder.Body.String() != "{\"error\":\"unauthorized\"}\n" {
				t.Fatalf("body = %q, want the one unauthorized body every refused credential gets", recorder.Body.String())
			}
			assertRejectionLines(t, logged.String(), "listener: service-account token rejected", tc.wantReason, tc.wantAccount, tc.token)
		})
	}
}

// ENVOY_OIDC_GOOGLE_AUDIENCE and ENVOY_OIDC_GOOGLE_ACCOUNTS admit a Google identity token Google
// signed for that audience, naming a listed account whose address Google verified: the credential
// the alert sink, which runs on Cloud Run and holds no Kubernetes token, calls the listener with.
// The token's issuer picks the branch, so a projected token goes on reaching its own beside it,
// and a listener that takes Google identities requires a bearer: it is not open.
func TestAListedGoogleIdentityIsAdmitted(t *testing.T) {
	google := newGoogleStandIn(t, listenerGoogleAccount)
	cluster := newClusterIssuer(t)
	googleToken := google.token(t, listenerGoogleAccount, true, listenerGoogleAudience)
	quotedTrue := google.token(t, listenerGoogleAccount, "true", listenerGoogleAudience)

	cases := []struct {
		name       string
		auth       listenerAPIAuth
		authorize  string
		wantStatus int
	}{
		{name: "a listener taking only Google identities admits a listed one", auth: listenerAPIAuth{google: google.identities}, authorize: "Bearer " + googleToken, wantStatus: http.StatusNoContent},
		{name: "email_verified written as the string true is verified, as Google documents", auth: listenerAPIAuth{google: google.identities}, authorize: "Bearer " + quotedTrue, wantStatus: http.StatusNoContent},
		{name: "a listed Google address is compared without case", auth: listenerAPIAuth{google: google.identities}, authorize: "Bearer " + google.token(t, strings.ToUpper(listenerGoogleAccount), true, listenerGoogleAudience), wantStatus: http.StatusNoContent},
		{name: "beside a projected-token verifier the Google token reaches the Google branch", auth: listenerAPIAuth{verifier: cluster.verifier, accounts: []string{listenerListedSubject}, google: google.identities}, authorize: "Bearer " + googleToken, wantStatus: http.StatusNoContent},
		{name: "beside Google identities a listed projected token still reaches its own branch", auth: listenerAPIAuth{verifier: cluster.verifier, accounts: []string{listenerListedSubject}, google: google.identities}, authorize: "Bearer " + cluster.token(t, listenerListedSubject), wantStatus: http.StatusNoContent},
		{name: "a listener taking only Google identities is not open", auth: listenerAPIAuth{google: google.identities}, wantStatus: http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			auth := tc.auth
			auth.logger = logging.NewWithWriter("test-machine", &logged)
			recorder := httptest.NewRecorder()
			apiAuth(auth, served).ServeHTTP(recorder, bearerRequest("/v1/sessions", tc.authorize))

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s; log: %s", recorder.Code, tc.wantStatus, recorder.Body.String(), logged.String())
			}
			assertRejectionLines(t, logged.String(), googleRejectedMessage, "", "", "")
			assertRejectionLog(t, logged.String(), "", tc.authorize)
		})
	}
}

// Google's identity tokens use either spelling of its issuer. A listener that also takes
// projected tokens must send both spellings through the account list, as a Google-only listener
// does, without falling back to the projected-token verifier after a refusal.
func TestGoogleIssuerAliasesReachTheGoogleBranch(t *testing.T) {
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "google-signing-key")
	keys, err := oidc.NewKeys(context.Background(), issuer.URL())
	if err != nil {
		t.Fatalf("oidc.NewKeys: %v", err)
	}
	google := &googleIdentities{
		audience: listenerGoogleAudience,
		verifier: oidc.NewWithKeys(oidc.GoogleIssuer, listenerGoogleAudience, keys),
		accounts: []string{listenerGoogleAccount},
	}
	cluster := newClusterIssuer(t)
	for _, iss := range []string{"https://accounts.google.com", "accounts.google.com"} {
		for _, both := range []bool{false, true} {
			for _, listed := range []bool{true, false} {
				t.Run(fmt.Sprintf("issuer=%s/projected=%t/listed=%t", iss, both, listed), func(t *testing.T) {
					account := listenerGoogleAccount
					wantStatus, wantReason := http.StatusNoContent, ""
					if !listed {
						account = listenerGoogleOtherEmail
						wantStatus, wantReason = http.StatusUnauthorized, "not_listed"
					}
					claims := issuer.Claims("118000000000000000001", listenerGoogleAudience)
					claims["iss"], claims["email"], claims["email_verified"] = iss, account, true
					raw := issuer.Mint(t, key, claims)
					var logged bytes.Buffer
					auth := listenerAPIAuth{google: google, logger: logging.NewWithWriter("test-machine", &logged)}
					if both {
						auth.verifier = cluster.verifier
					}
					recorder := httptest.NewRecorder()
					apiAuth(auth, served).ServeHTTP(recorder, bearerRequest("/v1/sessions", "Bearer "+raw))
					if recorder.Code != wantStatus {
						t.Fatalf("status = %d, want %d; body = %s; log: %s", recorder.Code, wantStatus, recorder.Body.String(), logged.String())
					}
					assertRejectionLines(t, logged.String(), googleRejectedMessage, wantReason, account, raw)
					assertRejectionLog(t, logged.String(), "", raw)
				})
			}
		}
	}
}

// A Google identity token is refused unless it names a listed account whose address Google
// verified, in a token Google signed for this listener's audience, and each refusal is the one
// unauthorized body with its class in the log alone.
func TestAnUnlistedGoogleIdentityIsRefused(t *testing.T) {
	google := newGoogleStandIn(t, listenerGoogleAccount)
	cluster := newClusterIssuer(t)

	cases := []struct {
		name        string
		token       string
		wantReason  string
		wantAccount string
	}{
		{name: "an account the list does not name", token: google.token(t, listenerGoogleOtherEmail, true, listenerGoogleAudience), wantReason: "not_listed", wantAccount: listenerGoogleOtherEmail},
		{name: "a listed account whose address Google has not verified", token: google.token(t, listenerGoogleAccount, false, listenerGoogleAudience), wantReason: "email_not_verified", wantAccount: listenerGoogleAccount},
		// Addresses are compared in ASCII lower case only: Unicode's lower case of the Kelvin sign
		// U+212A is k, so a Unicode fold would read this address as the listed one.
		{name: "an address that only Unicode case folding makes the listed one", token: google.token(t, strings.Replace(listenerGoogleAccount, "k", "\u212a", 1), true, listenerGoogleAudience), wantReason: "not_listed", wantAccount: strings.Replace(listenerGoogleAccount, "k", "\u212a", 1)},
		{name: "a token minted without format=full, which names no account", token: google.token(t, "", nil, listenerGoogleAudience), wantReason: "no_email"},
		{name: "a listed account's token for another audience", token: google.token(t, listenerGoogleAccount, true, "https://other.example"), wantReason: "audience"},
		{name: "a projected token on a listener that takes only Google identities", token: cluster.token(t, listenerListedSubject), wantReason: "issuer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			recorder := httptest.NewRecorder()
			apiAuth(listenerAPIAuth{google: google.identities, logger: logging.NewWithWriter("test-machine", &logged)}, served).
				ServeHTTP(recorder, bearerRequest("/v1/sessions", "Bearer "+tc.token))

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", recorder.Code, recorder.Body.String())
			}
			if body := recorder.Body.String(); body != "{\"error\":\"unauthorized\"}\n" {
				t.Fatalf("body = %q, want the one unauthorized body every refused credential gets", body)
			}
			assertRejectionLines(t, logged.String(), googleRejectedMessage, tc.wantReason, tc.wantAccount, tc.token)
		})
	}
}

// requestSeries reads envoy_listener_requests_total from the registry's own exposition, as a
// scrape of /metrics reads it, keyed by its credential and path labels.
func requestSeries(t *testing.T, met *metrics.Registry) map[[2]string]int64 {
	t.Helper()
	recorder := httptest.NewRecorder()
	met.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	series := map[[2]string]int64{}
	scanner := bufio.NewScanner(strings.NewReader(recorder.Body.String()))
	for scanner.Scan() {
		labels, ok := strings.CutPrefix(scanner.Text(), "envoy_listener_requests_total{")
		if !ok {
			continue
		}
		labels, value, ok := strings.Cut(labels, "} ")
		if !ok {
			t.Fatalf("a sample with no value: %q", scanner.Text())
		}
		var credential, path string
		if _, err := fmt.Sscanf(strings.ReplaceAll(labels, ",", " "), "credential=%q path=%q", &credential, &path); err != nil {
			t.Fatalf("a sample whose labels are not credential and path: %q (%v)", scanner.Text(), err)
		}
		count, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			t.Fatalf("a sample whose value is not a count: %q", scanner.Text())
		}
		series[[2]string{credential, path}] = count
	}
	return series
}

// Every request /v1 admits is counted under the credential that admitted it and the route it
// reached, which is what the shared token's removal waits on: shared_token and none must each
// read zero on every deployed listener. The none row is driven by a credential-less call to an
// open listener, the control that moves a series no request moved before. The path label is the
// route's pattern, so a session id in a path never becomes a series of its own, and a refused
// request counts nowhere.
func TestEveryAdmittedRequestCountsItsCredential(t *testing.T) {
	const shared = "expected-token"
	cluster := newClusterIssuer(t)
	google := newGoogleStandIn(t, listenerGoogleAccount)
	broker := brokertokentest.NewStubBroker(t)
	caller := brokertokentest.NewCaller(t)
	broker.Says("access-token-counted", sessionAnswer("ses_counted", "ada@example.com", caller.JKT()))

	met := metrics.New()
	requests := newListenerRequests(met)
	logger := logging.NewWithWriter("test-machine", io.Discard)
	gate := newStartingGate("v1", logger)
	routes := http.NewServeMux()
	for _, pattern := range []string{"/v1/sessions", "/v1/registry/", "/v1/messages/publish"} {
		routes.Handle(pattern, served)
	}
	gate.open(routes)
	closed := apiAuth(listenerAPIAuth{
		token:    shared,
		verifier: cluster.verifier,
		google:   google.identities,
		brokers:  listenerBrokerTokens(t, broker),
		logger:   logger,
		requests: requests,
		route:    gate.route,
	}, gate)
	open := apiAuth(listenerAPIAuth{logger: logger, requests: requests, route: gate.route}, gate)

	rows := []struct {
		credential string
		path       string
		handler    http.Handler
		request    func() *http.Request
	}{
		// The labels are written out rather than taken from the constants apiAuth counts with, so a
		// renamed label, which every query on the series would stop matching, fails here.
		{"shared_token", "/v1/sessions", closed, func() *http.Request { return bearerRequest("/v1/sessions", "Bearer "+shared) }},
		{"oidc", "/v1/registry/", closed, func() *http.Request {
			return bearerRequest("/v1/registry/ses_oidc", "Bearer "+cluster.token(t, listenerUnlistedSubject))
		}},
		{"google", "/v1/sessions", closed, func() *http.Request {
			return bearerRequest("/v1/sessions", "Bearer "+google.token(t, listenerGoogleAccount, true, listenerGoogleAudience))
		}},
		{"broker", "/v1/messages/publish", closed, func() *http.Request {
			return brokerTokenRequest(t, caller, http.MethodPost, "/v1/messages/publish", "access-token-counted", `{}`)
		}},
		{"none", "/v1/registry/", open, func() *http.Request { return bearerRequest("/v1/registry/ses_anyone", "") }},
	}
	for _, row := range rows {
		before := requestSeries(t, met)
		if count := before[[2]string{row.credential, row.path}]; count != 0 {
			t.Fatalf("credential %s on %s reads %d before any request of it", row.credential, row.path, count)
		}
		recorder := httptest.NewRecorder()
		row.handler.ServeHTTP(recorder, row.request())
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("credential %s: status = %d, want 204; body = %s", row.credential, recorder.Code, recorder.Body.String())
		}
		after := requestSeries(t, met)
		if count := after[[2]string{row.credential, row.path}]; count != 1 {
			t.Fatalf("credential %s on %s reads %d after one admitted request, want 1; series: %v", row.credential, row.path, count, after)
		}
		for key, count := range after {
			if key != [2]string{row.credential, row.path} && count != before[key] {
				t.Fatalf("a request admitted on %s moved the series %v from %d to %d", row.credential, key, before[key], count)
			}
		}
	}

	// Nothing apiAuth refuses, and nothing it lets past without judging (the exempt paths), is
	// counted.
	misdirected := bearerRequest("/v1/sessions", "")
	misdirected.Host = "rebind.attacker.example"
	for _, refused := range []struct {
		name    string
		handler http.Handler
		request *http.Request
	}{
		{"a wrong bearer", closed, bearerRequest("/v1/sessions", "Bearer wrong-token")},
		{"an unlisted Google identity", closed, bearerRequest("/v1/sessions", "Bearer "+google.token(t, listenerGoogleOtherEmail, true, listenerGoogleAudience))},
		{"an unverified Google identity", closed, bearerRequest("/v1/sessions", "Bearer "+google.token(t, listenerGoogleAccount, false, listenerGoogleAudience))},
		{"a broker token the broker does not know", closed, brokerTokenRequest(t, caller, http.MethodPost, "/v1/messages/publish", "access-token-unknown", `{}`)},
		{"a Host the open listener does not serve", open, misdirected},
		{"the exempt /metrics on the open listener", open, bearerRequest("/metrics", "")},
	} {
		before := requestSeries(t, met)
		refused.handler.ServeHTTP(httptest.NewRecorder(), refused.request)
		if after := requestSeries(t, met); fmt.Sprint(after) != fmt.Sprint(before) {
			t.Fatalf("%s moved the series: %v, then %v", refused.name, before, after)
		}
	}
}

// The two credentials the removal waits on, the shared token and none at all, each log one WARN
// a minute naming the route and the caller's address, so an operator can find the callers the
// series counts; a line says how many admissions it held back since the last. The others log
// nothing when admitted.
func TestTheSharedTokenAndTheOpenPathWarnOnceAMinute(t *testing.T) {
	const shared = "expected-token"
	cluster := newClusterIssuer(t)
	clock := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	var logged bytes.Buffer
	logger := logging.NewWithWriter("test-machine", &logged)
	gate := newStartingGate("v1", logger)
	routes := http.NewServeMux()
	routes.Handle("/v1/sessions", served)
	gate.open(routes)
	closed := apiAuth(listenerAPIAuth{token: shared, verifier: cluster.verifier, logger: logger, route: gate.route, now: now}, gate)
	open := apiAuth(listenerAPIAuth{logger: logger, route: gate.route, now: now}, gate)

	lines := func(message string) []string {
		var found []string
		for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
			if strings.Contains(line, message) {
				found = append(found, line)
			}
		}
		return found
	}
	serve := func(handler http.Handler, request *http.Request) {
		t.Helper()
		request.RemoteAddr = "192.0.2.7:41000"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", recorder.Code)
		}
	}

	for range 3 {
		serve(closed, bearerRequest("/v1/sessions", "Bearer "+shared))
		serve(open, bearerRequest("/v1/sessions", ""))
		serve(closed, bearerRequest("/v1/sessions", "Bearer "+cluster.token(t, listenerListedSubject)))
	}
	for message, credential := range map[string]string{sharedTokenAdmittedMessage: credentialSharedToken, openAdmittedMessage: credentialNone} {
		found := lines(message)
		if len(found) != 1 {
			t.Fatalf("%d lines %q within the minute, want 1; log: %s", len(found), message, logged.String())
		}
		for _, want := range []string{`"level":"WARN"`, `"credential":"` + credential + `"`, `"path":"/v1/sessions"`, `"remote":"192.0.2.7:41000"`, `"held":0`} {
			if !strings.Contains(found[0], want) {
				t.Fatalf("the first line %q does not carry %s", found[0], want)
			}
		}
	}
	if strings.Contains(logged.String(), `"credential":"oidc"`) {
		t.Fatalf("an admission on a projected token logged a warning: %s", logged.String())
	}

	// Inside the minute nothing more is logged, however close to its end.
	clock = clock.Add(time.Minute - time.Second)
	serve(closed, bearerRequest("/v1/sessions", "Bearer "+shared))
	serve(open, bearerRequest("/v1/sessions", ""))
	for _, message := range []string{sharedTokenAdmittedMessage, openAdmittedMessage} {
		if found := lines(message); len(found) != 1 {
			t.Fatalf("%d lines %q a second before the minute ends, want 1; log: %s", len(found), message, logged.String())
		}
	}
	clock = clock.Add(time.Second)
	serve(closed, bearerRequest("/v1/sessions", "Bearer "+shared))
	serve(open, bearerRequest("/v1/sessions", ""))
	for _, message := range []string{sharedTokenAdmittedMessage, openAdmittedMessage} {
		found := lines(message)
		if len(found) != 2 {
			t.Fatalf("%d lines %q once a minute had passed, want 2; log: %s", len(found), message, logged.String())
		}
		if !strings.Contains(found[1], `"held":3`) {
			t.Fatalf("the second line %q does not say it held back the three admissions since the first", found[1])
		}
	}
}

func TestResolveListenerOIDCAccounts(t *testing.T) {
	const issuer = "https://oidc.eks.example/id/CLUSTER"
	cases := []struct {
		name   string
		value  string
		issuer string
		want   []string
		// wantError is text the refusal must carry, empty when the value is accepted.
		wantError string
	}{
		{name: "unset admits every subject the verifier verifies", issuer: issuer},
		{name: "unset with no verifier", issuer: ""},
		// An empty value is how a deployment writes the setting unset, so it admits as unset does;
		// a value of only blanks or commas is a list emptied by mistake, which must not admit
		// every pod of the cluster.
		{name: "an empty value is unset", value: "", issuer: issuer},
		{name: "a value of only spaces", value: "   ", issuer: issuer, wantError: "ENVOY_OIDC_ACCOUNTS is set but names no account"},
		{name: "a value of only white space", value: " \n\t ", issuer: issuer, wantError: "ENVOY_OIDC_ACCOUNTS is set but names no account"},
		{name: "a value of only commas", value: ", ,,", issuer: issuer, wantError: "ENVOY_OIDC_ACCOUNTS is set but names no account"},
		{name: "a value of only a no-break space", value: "\u00a0", issuer: issuer, wantError: "ENVOY_OIDC_ACCOUNTS is set but names no account"},
		{name: "a value of only spaces with no verifier", value: "  ", wantError: "ENVOY_OIDC_ACCOUNTS is set but names no account"},
		{name: "whitespace-separated subjects", value: " " + listenerListedSubject + "\n\t" + listenerUnlistedSubject + " ", issuer: issuer, want: []string{listenerListedSubject, listenerUnlistedSubject}},
		// Commas separate entries as white space does, as in ENVOY_ALLOWED_HOSTS beside it, and an
		// entry a comma leaves empty is refused, naming where it is.
		{name: "comma-separated subjects", value: listenerListedSubject + "," + listenerUnlistedSubject, issuer: issuer, want: []string{listenerListedSubject, listenerUnlistedSubject}},
		{name: "subjects separated by a comma and a space", value: listenerListedSubject + ", " + listenerUnlistedSubject, issuer: issuer, want: []string{listenerListedSubject, listenerUnlistedSubject}},
		{name: "a trailing comma leaves an empty entry", value: listenerListedSubject + ",", issuer: issuer, wantError: "ENVOY_OIDC_ACCOUNTS holds an empty entry"},
		{name: "two commas leave an empty entry", value: listenerListedSubject + ", ," + listenerUnlistedSubject, issuer: issuer, wantError: "ENVOY_OIDC_ACCOUNTS holds an empty entry"},
		{name: "a leading comma leaves an empty entry", value: "," + listenerListedSubject, issuer: issuer, wantError: "ENVOY_OIDC_ACCOUNTS holds an empty entry"},
		{name: "a list with no verifier to hold to it", value: listenerListedSubject, wantError: "ENVOY_OIDC_ACCOUNTS needs ENVOY_OIDC_ISSUER"},
		{name: "an entry that is not a ServiceAccount subject", value: listenerListedSubject + " dispatch:envoy-broker", issuer: issuer, wantError: `"dispatch:envoy-broker"`},
		{name: "a subject with no name", value: "system:serviceaccount:dispatch:", issuer: issuer, wantError: `"system:serviceaccount:dispatch:"`},
		{name: "a subject with a third part", value: "system:serviceaccount:dispatch:envoy:broker", issuer: issuer, wantError: `"system:serviceaccount:dispatch:envoy:broker"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveListenerOIDCAccounts(func(key string) string {
				if key == "ENVOY_OIDC_ACCOUNTS" {
					return tc.value
				}
				return ""
			}, tc.issuer)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want one carrying %s", err, tc.wantError)
				}
				return
			}
			if err != nil || fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("resolveListenerOIDCAccounts(%q) = %q, %v; want %q", tc.value, got, err, tc.want)
			}
		})
	}
}

func TestResolveListenerGoogleConfig(t *testing.T) {
	cases := []struct {
		name         string
		env          map[string]string
		wantAudience string
		wantAccounts []string
		wantError    string
	}{
		{name: "neither set takes no Google identity", env: map[string]string{}},
		{
			name:         "both set, each account compared in lower case",
			env:          map[string]string{"ENVOY_OIDC_GOOGLE_AUDIENCE": " " + listenerGoogleAudience + " ", "ENVOY_OIDC_GOOGLE_ACCOUNTS": "Example-Sink@Example-Project.iam.gserviceaccount.com\n" + listenerGoogleOtherEmail},
			wantAudience: listenerGoogleAudience,
			wantAccounts: []string{listenerGoogleAccount, listenerGoogleOtherEmail},
		},
		{name: "an audience no account is listed for", env: map[string]string{"ENVOY_OIDC_GOOGLE_AUDIENCE": listenerGoogleAudience}, wantError: "ENVOY_OIDC_GOOGLE_ACCOUNTS is required when ENVOY_OIDC_GOOGLE_AUDIENCE is set"},
		{name: "accounts with no audience", env: map[string]string{"ENVOY_OIDC_GOOGLE_ACCOUNTS": listenerGoogleAccount}, wantError: "ENVOY_OIDC_GOOGLE_AUDIENCE is required when ENVOY_OIDC_GOOGLE_ACCOUNTS is set"},
		{name: "an entry that is not an address", env: map[string]string{"ENVOY_OIDC_GOOGLE_AUDIENCE": listenerGoogleAudience, "ENVOY_OIDC_GOOGLE_ACCOUNTS": "example-sink"}, wantError: `"example-sink"`},
		{
			name:         "accounts separated by a comma and a space",
			env:          map[string]string{"ENVOY_OIDC_GOOGLE_AUDIENCE": listenerGoogleAudience, "ENVOY_OIDC_GOOGLE_ACCOUNTS": listenerGoogleAccount + ", " + listenerGoogleOtherEmail},
			wantAudience: listenerGoogleAudience,
			wantAccounts: []string{listenerGoogleAccount, listenerGoogleOtherEmail},
		},
		{name: "a trailing comma leaves an empty entry", env: map[string]string{"ENVOY_OIDC_GOOGLE_AUDIENCE": listenerGoogleAudience, "ENVOY_OIDC_GOOGLE_ACCOUNTS": listenerGoogleAccount + ","}, wantError: "ENVOY_OIDC_GOOGLE_ACCOUNTS holds an empty entry"},
		{name: "only commas", env: map[string]string{"ENVOY_OIDC_GOOGLE_AUDIENCE": listenerGoogleAudience, "ENVOY_OIDC_GOOGLE_ACCOUNTS": ", ,"}, wantError: "ENVOY_OIDC_GOOGLE_ACCOUNTS holds an empty entry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			audience, accounts, err := resolveListenerGoogleConfig(func(key string) string { return tc.env[key] })
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want one carrying %s", err, tc.wantError)
				}
				return
			}
			if err != nil || audience != tc.wantAudience || fmt.Sprint(accounts) != fmt.Sprint(tc.wantAccounts) {
				t.Fatalf("resolveListenerGoogleConfig(%v) = %q, %q, %v; want %q, %q", tc.env, audience, accounts, err, tc.wantAudience, tc.wantAccounts)
			}
		})
	}
}

// A listener that takes Google identities requires a bearer, so the boot log's hosts line says
// /v1 is served to any Host, as it says for the other two bearers.
func TestDescribeListenerAPIHostsCountsEveryBearer(t *testing.T) {
	google := &googleIdentities{audience: listenerGoogleAudience, accounts: []string{listenerGoogleAccount}}
	for _, tc := range []struct {
		name string
		auth listenerAPIAuth
		want string
	}{
		{name: "open", auth: listenerAPIAuth{}, want: "loopback"},
		{name: "open with allowed hosts", auth: listenerAPIAuth{allowedHosts: []string{"hub.internal.example"}}, want: "loopback, hub.internal.example"},
		{name: "the shared token", auth: listenerAPIAuth{token: "expected-token"}, want: "any (a bearer is required)"},
		{name: "Google identities alone", auth: listenerAPIAuth{google: google}, want: "any (a bearer is required)"},
		{name: "Google identities beside allowed hosts", auth: listenerAPIAuth{google: google, allowedHosts: []string{"hub.internal.example"}}, want: "any (a bearer is required, so ENVOY_ALLOWED_HOSTS is unused)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeListenerAPIHosts(tc.auth); got != tc.want {
				t.Fatalf("describeListenerAPIHosts = %q, want %q", got, tc.want)
			}
		})
	}
}

// The path label is a route's pattern and never the path a caller wrote, so the series stay
// bounded whatever a caller sends: through the outer mux as main mounts it, a request before the
// gate opens is counted under the pattern that routed it to the gate, one after under the route
// that serves it, and a path no route matches under the gate's pattern again.
func TestTheRouteLabelIsAPatternNeverAPath(t *testing.T) {
	met := metrics.New()
	logger := logging.NewWithWriter("test-machine", io.Discard)
	gate := newStartingGate("v1", logger)
	handler := apiAuth(listenerAPIAuth{logger: logger, requests: newListenerRequests(met), route: gate.route}, gate)
	outer := http.NewServeMux()
	outer.Handle("/v1", handler)
	outer.Handle("/v1/", handler)
	call := func(path string) {
		t.Helper()
		outer.ServeHTTP(httptest.NewRecorder(), bearerRequest(path, ""))
	}

	call("/v1/registry/ses_before_the_gate_opens")
	routes := http.NewServeMux()
	routes.Handle("/v1/registry/", served)
	gate.open(routes)
	call("/v1/registry/ses_one")
	call("/v1/registry/ses_two")
	call("/v1/no-route/of/any/kind")

	want := map[[2]string]int64{
		{credentialNone, "/v1/"}:          2,
		{credentialNone, "/v1/registry/"}: 2,
	}
	if got := requestSeries(t, met); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("series = %v, want %v", got, want)
	}
}

// A Google issuer claim must not select a branch the listener has not configured. In particular,
// the projected-only listener retains its verifier's admission rule when the new lists are unset.
func TestGoogleIssuerOnAProjectedOnlyListener(t *testing.T) {
	cluster := newClusterIssuer(t)
	keys, err := oidc.NewKeys(context.Background(), cluster.issuer.URL())
	if err != nil {
		t.Fatalf("oidc.NewKeys: %v", err)
	}
	claims := cluster.issuer.Claims("118000000000000000001", listenerTestAudience)
	claims["iss"] = "https://accounts.google.com"
	raw := cluster.issuer.Mint(t, cluster.key, claims)
	for _, tc := range []struct {
		name       string
		verifier   *oidc.Verifier
		wantStatus int
		wantReason string
	}{
		{name: "the cluster verifier refuses Google's issuer", verifier: cluster.verifier, wantStatus: http.StatusUnauthorized, wantReason: "issuer"},
		{name: "a verifier configured for Google still admits it without a Google account list", verifier: oidc.NewWithKeys(oidc.GoogleIssuer, listenerTestAudience, keys), wantStatus: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			recorder := httptest.NewRecorder()
			apiAuth(listenerAPIAuth{verifier: tc.verifier, logger: logging.NewWithWriter("test-machine", &logged)}, served).
				ServeHTTP(recorder, bearerRequest("/v1/sessions", "Bearer "+raw))
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; log: %s", recorder.Code, tc.wantStatus, logged.String())
			}
			assertRejectionLog(t, logged.String(), tc.wantReason, "Bearer "+raw)
			assertRejectionLines(t, logged.String(), googleRejectedMessage, "", "", raw)
		})
	}
}

// Concurrent admissions share one warning window and retain every suppressed admission in the
// next warning's held count. Both credentials have independent windows.
func TestAdmissionWarningsCountConcurrentAdmissions(t *testing.T) {
	clock := time.Unix(0, 0)
	warnings := newAdmissionWarnings(func() time.Time { return clock })
	var logged bytes.Buffer
	logger := logging.NewWithWriter("test-machine", &logged)
	request := bearerRequest("/v1/sessions", "")
	var workers sync.WaitGroup
	for range 64 {
		for _, credential := range []string{credentialSharedToken, credentialNone} {
			workers.Add(1)
			go func() {
				defer workers.Done()
				warnings.admitted(logger, credential, "/v1/sessions", request)
			}()
		}
	}
	workers.Wait()
	clock = clock.Add(time.Minute)
	for _, credential := range []string{credentialSharedToken, credentialNone} {
		warnings.admitted(logger, credential, "/v1/sessions", request)
	}
	decoder := json.NewDecoder(&logged)
	held := map[string][]int{}
	for decoder.More() {
		var line struct {
			Credential string `json:"credential"`
			Held       int    `json:"held"`
		}
		if err := decoder.Decode(&line); err != nil {
			t.Fatalf("warning is not JSON: %v", err)
		}
		held[line.Credential] = append(held[line.Credential], line.Held)
	}
	if len(held) != 2 {
		t.Fatalf("warning credentials = %v, want shared_token and none", held)
	}
	for _, credential := range []string{credentialSharedToken, credentialNone} {
		if got := held[credential]; len(got) != 2 || got[0] != 0 || got[1] != 63 {
			t.Fatalf("%s held counts = %v, want [0 63]", credential, got)
		}
	}
}

// The outer mux handles path cleaning before authentication. CONNECT skips that cleaning, but
// every admitted request still uses one of the registered patterns, including slash redirects
// and refusals while the gate is closed. An empty Host keeps the open listener's Host refusal.
func TestTheRouteLabelStaysBoundedForUnusualRequests(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, host, token string
		closed                          bool
		wantStatus                      int
		wantPath                        string
	}{
		{name: "a session path", method: http.MethodGet, path: "/v1/registry/ses_one", host: "localhost", wantStatus: http.StatusNoContent, wantPath: "/v1/registry/"},
		{name: "a repeated slash redirects before admission", method: http.MethodGet, path: "/v1//registry/ses_one", host: "localhost", wantStatus: http.StatusTemporaryRedirect},
		{name: "a dot segment redirects before admission", method: http.MethodGet, path: "/v1/registry/../unknown", host: "localhost", wantStatus: http.StatusTemporaryRedirect},
		{name: "an escaped slash stays in its route", method: http.MethodGet, path: "/v1/registry/ses%2fone", host: "localhost", wantStatus: http.StatusNoContent, wantPath: "/v1/registry/"},
		{name: "an inner slash redirect", method: http.MethodGet, path: "/v1/registry", host: "localhost", wantStatus: http.StatusTemporaryRedirect, wantPath: "/v1/registry/"},
		{name: "CONNECT with an inner slash redirect", method: http.MethodConnect, path: "/v1/registry", host: "localhost", wantStatus: http.StatusTemporaryRedirect, wantPath: "/v1/registry/"},
		{name: "CONNECT leaves a repeated slash alone", method: http.MethodConnect, path: "/v1//registry/ses_one", host: "localhost", wantStatus: http.StatusNotFound, wantPath: "/v1/"},
		{name: "CONNECT leaves a dot segment alone", method: http.MethodConnect, path: "/v1/registry/../ses_one", host: "localhost", wantStatus: http.StatusNoContent, wantPath: "/v1/registry/"},
		{name: "an empty Host is refused when open", method: http.MethodGet, path: "/v1/registry/ses_one", wantStatus: http.StatusMisdirectedRequest},
		{name: "an empty Host is unused beside a bearer", method: http.MethodGet, path: "/v1/registry/ses_one", token: "shared-example", wantStatus: http.StatusNoContent, wantPath: "/v1/registry/"},
		{name: "a closed gate counts under the outer pattern", method: http.MethodGet, path: "/v1/registry/ses_one", host: "localhost", closed: true, wantStatus: http.StatusServiceUnavailable, wantPath: "/v1/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			met := metrics.New()
			logger := logging.NewWithWriter("test-machine", io.Discard)
			gate := newStartingGate("v1", logger)
			if !tc.closed {
				routes := http.NewServeMux()
				routes.Handle("/v1/registry/", served)
				gate.open(routes)
			}
			handler := apiAuth(listenerAPIAuth{token: tc.token, logger: logger, requests: newListenerRequests(met), route: gate.route}, gate)
			outer := http.NewServeMux()
			outer.Handle("/v1", handler)
			outer.Handle("/v1/", handler)
			request := httptest.NewRequest(http.MethodGet, tc.path, nil)
			request.Method, request.Host = tc.method, tc.host
			if tc.token != "" {
				request.Header.Set("Authorization", "Bearer "+tc.token)
			}
			recorder := httptest.NewRecorder()
			outer.ServeHTTP(recorder, request)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			series := requestSeries(t, met)
			if tc.wantPath == "" {
				if len(series) != 0 {
					t.Fatalf("a request refused before admission counted: %v", series)
				}
				return
			}
			credential := credentialNone
			if tc.token != "" {
				credential = credentialSharedToken
			}
			if len(series) != 1 || series[[2]string{credential, tc.wantPath}] != 1 {
				t.Fatalf("series = %v, want one request under %s and %s", series, credential, tc.wantPath)
			}
		})
	}
}

// An auth apiAuth cannot serve is refused when the handler is built, not at the first request
// that would reach the missing part: one with no logger, which every refusal and every warned
// admission writes to, and one whose Google identities carry no verifier to judge a token with.
func TestAPIAuthRefusesAnAuthItCannotServe(t *testing.T) {
	logger := logging.NewWithWriter("test-machine", io.Discard)
	for _, tc := range []struct {
		name string
		auth listenerAPIAuth
	}{
		{name: "an open listener with no logger", auth: listenerAPIAuth{}},
		{name: "the shared token with no logger", auth: listenerAPIAuth{token: "expected-token"}},
		{name: "Google identities with no verifier", auth: listenerAPIAuth{google: &googleIdentities{audience: listenerGoogleAudience, accounts: []string{listenerGoogleAccount}}, logger: logger}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("apiAuth built a handler from an auth it cannot serve")
				}
			}()
			apiAuth(tc.auth, served)
		})
	}
}
