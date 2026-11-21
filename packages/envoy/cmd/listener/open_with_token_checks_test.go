package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// openTailnetHost is the name a tailnet session reaches a private hub's tailnet-only session
// listener at, which that listener's ENVOY_ALLOWED_HOSTS holds.
const openTailnetHost = "envoy-hub.example-tailnet.ts.net"

// TestOpenWithTokenChecksServesNoneAndVerifiesWhatIsPresented: with ENVOY_V1_OPEN_WITH_TOKEN_CHECKS
// a listener that has a token check keeps /v1 open to a request that presents no credential, as a
// private hub's tailnet-only session listener does until the open /v1 is removed. Such a request,
// at a Host the listener serves, is admitted as none and reaches the handler with no principal and
// no broker caller, which the private required mode answers with no private project (the
// listener's project scope of an anonymous caller is empty). A bearer that is no JWT, an Envoy
// bearer this listener does not hold, is no credential here either, as on the open listener. A
// projected token that is presented and verifies is admitted as itself, its subject the request's
// principal, and a DPoP broker token that verifies as its session; one of either that does not
// verify is refused 401, never served as none.
//
// Control: the same listener without the setting answers the request with no credential 401
// (built in, below). Controls run by hand: the refusal of a projected token that does not verify
// replaced by a fall-through (the wrong-audience row is served, and fails); the principal dropped
// from a verified projected token (the listed row fails).
func TestOpenWithTokenChecksServesNoneAndVerifiesWhatIsPresented(t *testing.T) {
	issuer := oidctest.New(t)
	signing := issuer.PublishKey(t, "listener-test-key")
	verifier, err := oidc.New(context.Background(), issuer.URL(), listenerTestAudience)
	if err != nil {
		t.Fatalf("oidc.New(%q, %q): %v", issuer.URL(), listenerTestAudience, err)
	}
	listed := issuer.Mint(t, signing, issuer.Claims(listenerTestSubject, listenerTestAudience))
	wrongAudience := issuer.Claims(listenerTestSubject, listenerTestAudience)
	wrongAudience["aud"] = []string{"dispatch"}
	wrongAudienceToken := issuer.Mint(t, signing, wrongAudience)
	unlisted := issuer.Mint(t, signing, issuer.Claims("system:serviceaccount:elsewhere:stranger", listenerTestAudience))

	broker := ciphertokentest.NewStubBroker(t)
	session := ciphertokentest.NewCaller(t)
	const sessionToken, unknownToken = "access-token-of-a-tailnet-session", "access-token-the-broker-never-issued"
	broker.Says(sessionToken, sessionAnswer("ses_tailnet", "ada@example.com", session.JKT()))

	type seen struct {
		principal string
		session   string
	}
	var got seen
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = seen{principal: verifiedPrincipal(r)}
		if caller, ok := brokerCaller(r); ok {
			got.session = caller.Session
		}
		w.WriteHeader(http.StatusNoContent)
	})
	auth := func(open bool) http.Handler {
		return apiAuth(listenerAPIAuth{
			verifier: verifier, accounts: []string{listenerTestSubject}, brokers: listenerBrokerTokens(t, broker),
			allowedHosts: []string{openTailnetHost}, openWithTokenChecks: open,
			logger: logging.NewWithWriter("test-machine", io.Discard),
		}, next)
	}
	plain := func(host, authorization string) *http.Request {
		request := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
		request.Host = host
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		return request
	}

	for _, row := range []struct {
		name    string
		request func() *http.Request
		status  int
		want    seen
	}{
		{"no credential, at the tailnet name: served as none", func() *http.Request { return plain(openTailnetHost+":9020", "") }, http.StatusNoContent, seen{}},
		{"no credential, at a host it does not serve", func() *http.Request { return plain("attacker.example:9020", "") }, http.StatusMisdirectedRequest, seen{}},
		{"an Envoy bearer it does not hold: served as none", func() *http.Request {
			return plain(openTailnetHost+":9020", bearerPrefix+"shared-envoy-token-of-a-session")
		}, http.StatusNoContent, seen{}},
		{"a listed projected token: admitted as itself", func() *http.Request { return plain("10.0.0.7:9020", bearerPrefix+listed) }, http.StatusNoContent, seen{principal: listenerTestSubject}},
		{"a projected token for another audience: refused", func() *http.Request {
			return plain(openTailnetHost+":9020", bearerPrefix+wrongAudienceToken)
		}, http.StatusUnauthorized, seen{}},
		{"a projected token of an unlisted subject: refused", func() *http.Request { return plain(openTailnetHost+":9020", bearerPrefix+unlisted) }, http.StatusUnauthorized, seen{}},
		{"a broker token that verifies: admitted as its session", func() *http.Request {
			return brokerTokenRequest(t, session, http.MethodPost, "/v1/messages/publish", sessionToken, `{}`)
		}, http.StatusNoContent, seen{session: "ses_tailnet"}},
		{"a broker token the broker never issued: refused", func() *http.Request {
			return brokerTokenRequest(t, session, http.MethodPost, "/v1/messages/publish", unknownToken, `{}`)
		}, http.StatusUnauthorized, seen{}},
	} {
		t.Run(row.name, func(t *testing.T) {
			got = seen{}
			recorder := httptest.NewRecorder()
			auth(true).ServeHTTP(recorder, row.request())
			if recorder.Code != row.status {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, row.status, recorder.Body.String())
			}
			if got != row.want {
				t.Fatalf("the handler saw %+v, want %+v", got, row.want)
			}
		})
	}

	recorder := httptest.NewRecorder()
	auth(false).ServeHTTP(recorder, plain(openTailnetHost+":9020", ""))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("the control, the same listener without %s: a request with no credential got %d, want 401", EnvV1OpenWithTokenChecks, recorder.Code)
	}
}

// TestOpenWithTokenChecksIsOneValueBesideATokenCheck: the setting's one value is "true", unset is
// off, and any other value refuses the start naming the variable; and validateListenerAPIAuth
// refuses it on a listener with no token check, where /v1 is open anyway, while a listener with a
// projected-token check on every address passes with it and no ENVOY_API_ALLOW_UNAUTHENTICATED.
func TestOpenWithTokenChecksIsOneValueBesideATokenCheck(t *testing.T) {
	for value, want := range map[string]bool{"": false, "true": true} {
		got, err := resolveListenerV1OpenWithTokenChecks(func(string) string { return value })
		if err != nil || got != want {
			t.Fatalf("%s=%q resolves to %v, %v; want %v", EnvV1OpenWithTokenChecks, value, got, err, want)
		}
	}
	for _, value := range []string{"1", "yes", "TRUE"} {
		if _, err := resolveListenerV1OpenWithTokenChecks(func(string) string { return value }); err == nil || !strings.Contains(err.Error(), EnvV1OpenWithTokenChecks) {
			t.Fatalf("%s=%q resolves (err = %v), want a refusal naming the variable", EnvV1OpenWithTokenChecks, value, err)
		}
	}

	issuer := oidctest.New(t)
	verifier, err := oidc.New(context.Background(), issuer.URL(), listenerTestAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	everywhere := config.Service{ListenHost: "0.0.0.0"}
	if err := validateListenerAPIAuth(everywhere, listenerAPIAuth{openWithTokenChecks: true}, ""); err == nil || !strings.Contains(err.Error(), EnvV1OpenWithTokenChecks) {
		t.Fatalf("the setting with no token check validates (err = %v), want a refusal naming it", err)
	}
	if err := validateListenerAPIAuth(everywhere, listenerAPIAuth{verifier: verifier, openWithTokenChecks: true}, ""); err != nil {
		t.Fatalf("the setting beside a projected-token check: %v", err)
	}
}

// TestTheIntrospectionLimitSettings: ENVOY_AGENT_SECRETS_INTROSPECTIONS_PER_SECOND and
// ENVOY_AGENT_SECRETS_INTROSPECTION_BURST set the bucket each source address's introspections are
// bounded by, an unset one taking ciphertoken.DefaultAddressLimit's part, and neither set leaving
// the default; each is a whole number within its bounds, and either is part of the broker group, so
// one without ENVOY_AGENT_SECRETS_URL refuses the start naming it.
func TestTheIntrospectionLimitSettings(t *testing.T) {
	base := map[string]string{
		EnvBrokerURL: "https://broker.example", EnvBrokerAccountTokenFile: "/var/run/secrets/envoy-broker/token", EnvPublicURL: listenerBrokerOrigin,
	}
	with := func(extra map[string]string) func(string) string {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		return func(name string) string { return env[name] }
	}
	for _, row := range []struct {
		name  string
		extra map[string]string
		want  ratelimit.Limit
	}{
		{"neither", nil, ratelimit.Limit{}},
		{"both", map[string]string{EnvBrokerIntrospectionsPerSecond: "50", EnvBrokerIntrospectionBurst: "200"}, ratelimit.Limit{Every: 20 * time.Millisecond, Burst: 200}},
		{"the rate alone", map[string]string{EnvBrokerIntrospectionsPerSecond: "50"}, ratelimit.Limit{Every: 20 * time.Millisecond, Burst: ciphertoken.DefaultAddressLimit.Burst}},
		{"the burst alone", map[string]string{EnvBrokerIntrospectionBurst: "200"}, ratelimit.Limit{Every: ciphertoken.DefaultAddressLimit.Every, Burst: 200}},
	} {
		t.Run(row.name, func(t *testing.T) {
			config, err := resolveListenerBrokerConfig(with(row.extra))
			if err != nil || config.AddressLimit != row.want {
				t.Fatalf("resolves to %+v, %v; want the limit %+v", config.AddressLimit, err, row.want)
			}
		})
	}
	for _, row := range []struct{ name, variable, value string }{
		{"a rate of 0", EnvBrokerIntrospectionsPerSecond, "0"},
		{"a rate past its bound", EnvBrokerIntrospectionsPerSecond, "1001"},
		{"a burst that is no number", EnvBrokerIntrospectionBurst, "lots"},
		{"a burst past its bound", EnvBrokerIntrospectionBurst, "100001"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := resolveListenerBrokerConfig(with(map[string]string{row.variable: row.value})); err == nil || !strings.Contains(err.Error(), row.variable) {
				t.Fatalf("%s=%q resolves (err = %v), want a refusal naming it", row.variable, row.value, err)
			}
		})
	}
	alone := func(name string) string {
		if name == EnvBrokerIntrospectionBurst {
			return "200"
		}
		return ""
	}
	if _, err := resolveListenerBrokerConfig(alone); err == nil || !strings.Contains(err.Error(), EnvBrokerURL+" is required") {
		t.Fatalf("the limit with no broker resolves (err = %v), want a refusal naming %s", err, EnvBrokerURL)
	}
}
