package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// closedListenerOrigin is the origin the closed rig's listener is reached at: a name off loopback,
// as the clustered session listener's ENVOY_PUBLIC_URL is, and so the origin a proof's 'htu' is
// checked against and the Host its requests carry.
const closedListenerOrigin = "http://envoy-session-listener.example:9020"

// stoppableBroker is the secrets broker's introspection route, which a test can stop: it hands
// every introspection to the stub broker while it answers, and while it is down it closes each
// connection before any answer, as a broker that went away leaves its callers.
type stoppableBroker struct {
	stub   *ciphertokentest.StubBroker
	down   atomic.Bool
	server *httptest.Server
}

func newStoppableBroker(t *testing.T) *stoppableBroker {
	t.Helper()
	broker := &stoppableBroker{stub: ciphertokentest.NewStubBroker(t)}
	target, err := url.Parse(broker.stub.Server.URL)
	if err != nil {
		t.Fatalf("parse the stub broker's URL: %v", err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	broker.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broker.down.Load() {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(broker.server.Close)
	return broker
}

// closedBrokerRig is the clustered session listener after t-e-19: /v1 behind apiAuth with the
// broker settings as its only credential, on a bind off loopback, so its /v1 is closed. It is never
// newBrokerRig's open listener: it takes that rig's stores and routes, over a real bus, and mounts
// them behind an apiAuth of its own, whose spent-proof bucket is opened through the very call main
// makes, over a broker the test can stop.
type closedBrokerRig struct {
	deps    *listenerDeps
	auth    listenerAPIAuth
	handler http.Handler
	broker  *stoppableBroker
	caller  *ciphertokentest.Caller
	logs    *bytes.Buffer
}

func newClosedBrokerRig(t *testing.T) *closedBrokerRig {
	t.Helper()
	stores := newBrokerRig(t)
	broker := newStoppableBroker(t)
	accountFile := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(accountFile, []byte("listener-account-token\n"), 0o600); err != nil {
		t.Fatalf("write the listener's own account token: %v", err)
	}
	tokens := newBrokerTokens(listenerBrokerConfig{
		BrokerURL:        broker.server.URL,
		AccountTokenFile: accountFile,
		Origin:           closedListenerOrigin,
	})
	if err := tokens.open(t.Context(), stores.deps.client.Conn, 1); err != nil {
		t.Fatalf("open the spent-proof bucket: %v", err)
	}
	// The session listener's render names the Host its callers reach it by (ENVOY_ALLOWED_HOSTS),
	// so the Host fence of an open /v1 would serve these requests: what refuses a bare one is the
	// closed /v1 alone.
	auth := listenerAPIAuth{
		brokers:      tokens,
		offLoopback:  true,
		allowedHosts: []string{strings.Split(strings.TrimPrefix(closedListenerOrigin, "http://"), ":")[0]},
		logger:       logging.NewWithWriter("test-machine", stores.logs),
	}
	return &closedBrokerRig{
		deps:    stores.deps,
		auth:    auth,
		handler: apiAuth(auth, stores.routes),
		broker:  broker,
		caller:  ciphertokentest.NewCaller(t),
		logs:    stores.logs,
	}
}

// call sends one request to the closed listener at its own origin: presenting token under the DPoP
// scheme, with a proof the caller's real client signs, or nothing at all when token is empty.
func (r *closedBrokerRig) call(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Host = strings.TrimPrefix(closedListenerOrigin, "http://")
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		signed := r.caller.Signed(t, method, closedListenerOrigin+path, token)
		request.Header.Set("Authorization", signed.Header.Get("Authorization"))
		request.Header.Set("DPoP", signed.Header.Get("DPoP"))
	}
	recorder := httptest.NewRecorder()
	r.handler.ServeHTTP(recorder, request)
	return recorder
}

// register puts a live session in the registry, as its own registration would.
func (r *closedBrokerRig) register(t *testing.T, sessionID string) {
	t.Helper()
	if err := r.deps.sessions.Put(sessionID, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
}

// The test of root verification c-r9-credential-change: the session listener runs twice, in the
// required privacy mode with a privacy source that keeps answering and in the all-public mode,
// each with its open /v1 closed and its broker settings set. In each mode a session that presents
// a verified broker token performs its own operation on each of the seven routes that act as a
// session and is refused the same operation for another session; the sender limit counts the
// verified session, whatever the body names; the read routes answer as before; and a request that
// presents nothing answers 401. Then a token whose expiry is more than 5 s away is verified once
// and the broker stops answering: calls with it are served until the reuse runs out, at most
// ciphertoken.AffirmativeCacheTTL, and never past a token's own expiry; after it register,
// subscribe, send and the role claim each answer 503 with Retry-After: 1, and a session not yet
// registered receives no delivery; when the broker answers again, each call is served again.
// Last, the env of the rendered all-public session listener passes the listener's start checks.
//
// Five controls, each on its own: broker verification skipped in the all-public mode (a call with
// an unverified token is served); a cached answer kept past the token's expiry or past 5 s (a call
// is served during the outage after the reuse ran out); one session-binding check removed (a
// session acts for another on that route); the sender limit charged to the body's sender (the
// limit counts the wrong session); the outage answered 401 (the test names the status). Two more
// rows: requiresCredential as before t-e-19 (the bare request is served), and the render that
// keeps ENVOY_API_ALLOW_UNAUTHENTICATED and has no broker settings (the start check refuses it).
func TestR9CredentialChangeInBothListenerModes(t *testing.T) {
	t.Run("required mode", func(t *testing.T) {
		t.Skip("the required privacy mode (ENVOY_PROJECT_PRIVACY_REQUIRED, ENVOY_PROJECT_PRIVACY_URL, " +
			"listenerDeps.privacy) is plan C's privacy-required listener, which this branch's base does not carry; " +
			"this leg runs checkR9 on a closed rig in that mode once that listener is on the base")
	})
	t.Run("all-public mode", func(t *testing.T) {
		checkR9(t, newClosedBrokerRig(t))
	})
	t.Run("the rendered all-public session listener passes the start checks", func(t *testing.T) {
		const golden = "clustered-hub-listener-pod.golden"
		env := renderedListenerEnv(t, golden)
		// config.Load reads the process's environment; the start checks read the golden's alone, so
		// a variable this test's own environment carries cannot pass or refuse the render for it.
		for _, name := range []string{"ENVOY_LISTEN_FD", "ENVOY_ALLOWED_HOSTS"} {
			if _, rendered := env[name]; !rendered {
				t.Setenv(name, "")
				_ = os.Unsetenv(name)
			}
		}
		for name, value := range env {
			t.Setenv(name, value)
		}
		cfg, err := config.Load(9020)
		if err != nil {
			t.Fatalf("config.Load on the env %s renders: %v\nenv: %v", golden, err, env)
		}
		auth, err := listenerStartChecks(cfg, func(name string) string { return env[name] })
		if err != nil {
			t.Fatalf("the listener refuses its start on the env %s renders: %v\nenv: %v", golden, err, env)
		}
		auth.logger = logging.NewWithWriter("test-machine", &bytes.Buffer{})
		request := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
		// The Host its callers reach it by, the one its public origin names.
		request.Host = strings.TrimPrefix(env[EnvPublicURL], "http://")
		recorder := httptest.NewRecorder()
		apiAuth(auth, served).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("the listener %s renders answered a request that presents nothing %d, want 401", golden, recorder.Code)
		}
	})
}

// listenerStartChecks runs, over getenv, the checks main makes of the API auth settings before it
// binds: the settings the shared token's removal ended are refused, the broker settings are read,
// and validateListenerAPIAuth judges the auth main builds from them on cfg's bind. An env that sets
// the OIDC or the Google pair fails the test's caller instead: judging either would ask an issuer,
// and the session listener's render sets neither.
func listenerStartChecks(cfg config.Service, getenv func(string) string) (listenerAPIAuth, error) {
	if err := refuseRemovedAPISettings(getenv); err != nil {
		return listenerAPIAuth{}, err
	}
	for _, name := range []string{"ENVOY_OIDC_ISSUER", "ENVOY_OIDC_AUDIENCE", "ENVOY_OIDC_GOOGLE_AUDIENCE", "ENVOY_OIDC_GOOGLE_ACCOUNTS"} {
		if getenv(name) != "" {
			return listenerAPIAuth{}, fmt.Errorf("%s is set, which these checks do not judge", name)
		}
	}
	brokerConfig, err := resolveListenerBrokerConfig(getenv)
	if err != nil {
		return listenerAPIAuth{}, err
	}
	auth := listenerAPIAuth{brokers: newBrokerTokens(brokerConfig), offLoopback: !isLoopbackHost(cfg.ListenHost), allowedHosts: cfg.AllowedHosts}
	if err := validateListenerAPIAuth(cfg, auth); err != nil {
		return listenerAPIAuth{}, err
	}
	return auth, nil
}

// checkR9 is one mode's run of TestR9CredentialChangeInBothListenerModes on rig.
func checkR9(t *testing.T, rig *closedBrokerRig) {
	const (
		bound   = "ses_r9_bound"
		other   = "ses_r9_other"
		target  = "ses_r9_target"
		token   = "access-token-of-the-r9-session"
		unknown = "access-token-the-broker-never-issued"
	)
	rig.broker.stub.Says(token, sessionAnswer(bound, "ada@example.com", rig.caller.JKT()))
	for _, session := range []string{bound, other, target} {
		rig.register(t, session)
	}
	// The other session follows a topic under its own token, so the refused removal of its
	// interests below has something to leave standing, and the read routes something to read.
	const otherToken = "access-token-of-the-other-session"
	rig.broker.stub.Says(otherToken, sessionAnswer(other, "grace@example.com", rig.caller.JKT()))
	if answer := rig.call(t, http.MethodPost, "/v1/interests/subscribe", otherToken,
		`{"session_id":"`+other+`","topics":["notifications.github.r9"],"delivery":{"consumer":true,"epoch":"r9"}}`); answer.Code != http.StatusOK {
		t.Fatalf("%s's own subscribe under its own token: status = %d, want 200; body = %s", other, answer.Code, answer.Body.String())
	}

	t.Run("the listener's settings pass its start check off loopback", func(t *testing.T) {
		if err := validateListenerAPIAuth(config.Service{ListenHost: "0.0.0.0"}, rig.auth); err != nil {
			t.Fatalf("validateListenerAPIAuth refuses a listener off loopback whose only credential is its broker settings: %v", err)
		}
	})

	t.Run("a request that presents nothing answers 401", func(t *testing.T) {
		for _, request := range []struct{ method, path, body string }{
			{http.MethodGet, "/v1/sessions", ""},
			{http.MethodPost, "/v1/interests/subscribe", `{"session_id":"ses_r9_bare","topics":[],"delivery":{"consumer":true,"epoch":"r9"}}`},
		} {
			if answer := rig.call(t, request.method, request.path, "", request.body); answer.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s with no credential: status = %d, want 401; body = %s", request.method, request.path, answer.Code, answer.Body.String())
			}
		}
		if _, err := rig.deps.sessions.Refresh(context.Background(), "ses_r9_bare"); !errors.Is(err, natsgo.ErrKeyNotFound) {
			t.Fatalf("the bare registration left a record of ses_r9_bare (err = %v), want none", err)
		}
	})

	t.Run("a token the broker does not call live is refused", func(t *testing.T) {
		answer := rig.call(t, http.MethodPost, "/v1/interests/subscribe", unknown, `{"session_id":"ses_r9_unverified","topics":[],"delivery":{"consumer":true,"epoch":"r9"}}`)
		if answer.Code != http.StatusUnauthorized {
			t.Fatalf("POST /v1/interests/subscribe under a token the broker never issued: status = %d, want 401; body = %s", answer.Code, answer.Body.String())
		}
	})

	t.Run("each route that acts as a session acts as the verified session alone", func(t *testing.T) {
		for _, route := range []struct {
			method, field string
			path          func(session string) string
			body          func(session string) string
			want          int
			// gone reports whether the refused request left the other session's record or
			// interest standing, nil where the route removes nothing.
			gone func(session string) error
		}{
			{method: http.MethodPost, field: "session_id", want: http.StatusOK,
				path: func(string) string { return "/v1/interests/subscribe" },
				body: func(s string) string {
					return `{"session_id":"` + s + `","topics":["notifications.github.r9"],"delivery":{"consumer":true,"epoch":"r9"}}`
				}},
			{method: http.MethodPost, field: "session_id", want: http.StatusOK,
				path: func(string) string { return "/v1/interests/unsubscribe" },
				body: func(s string) string { return `{"session_id":"` + s + `","topics":["notifications.github.r9"]}` }},
			{method: http.MethodPost, field: "session_id", want: http.StatusOK,
				path: func(string) string { return "/v1/roles/set" },
				body: func(s string) string { return `{"session_id":"` + s + `","role":"r9-role"}` }},
			{method: http.MethodPost, field: "source_session", want: http.StatusOK,
				path: func(string) string { return "/v1/messages/send" },
				body: func(s string) string {
					return `{"target_session":"` + target + `","source_session":"` + s + `","message":"an r9 message"}`
				}},
			{method: http.MethodPost, field: "source_session", want: http.StatusOK,
				path: func(string) string { return "/v1/messages/publish" },
				body: func(s string) string {
					return `{"topic":"notifications.github.r9","source_session":"` + s + `","message":"an r9 message"}`
				}},
			{method: http.MethodDelete, field: "the path's session", want: http.StatusNoContent,
				path: func(s string) string { return "/v1/interests/" + s },
				body: func(string) string { return "" },
				gone: func(s string) error {
					_, err := rig.deps.registry.Get(context.Background(), s)
					return err
				}},
			{method: http.MethodDelete, field: "the path's session", want: http.StatusOK,
				path: func(s string) string {
					return "/v1/sessions/" + s + "?client_instance_id=4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
				},
				body: func(string) string { return "" },
				gone: func(s string) error {
					_, err := rig.deps.sessions.Refresh(context.Background(), s)
					return err
				}},
		} {
			name := route.method + " " + strings.SplitN(route.path("{session}"), "?", 2)[0]
			refused := rig.call(t, route.method, route.path(other), token, route.body(other))
			if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), bound) || !strings.Contains(refused.Body.String(), other) {
				t.Fatalf("%s naming %s in %s under %s's token: status = %d, want 403 naming both sessions; body = %s",
					name, other, route.field, bound, refused.Code, refused.Body.String())
			}
			if route.gone != nil {
				if err := route.gone(other); err != nil {
					t.Fatalf("%s naming %s in %s under %s's token removed what it names: %v", name, other, route.field, bound, err)
				}
			}
			if own := rig.call(t, route.method, route.path(bound), token, route.body(bound)); own.Code != route.want {
				t.Fatalf("%s naming %s in %s under its own token: status = %d, want %d; body = %s",
					name, bound, route.field, own.Code, route.want, own.Body.String())
			}
		}
	})

	t.Run("the sender limit counts the verified session, whatever the body names", func(t *testing.T) {
		const (
			limited      = "ses_r9_limited"
			another      = "ses_r9_another"
			limitedToken = "access-token-of-the-limited-session"
			anotherToken = "access-token-of-another-session"
		)
		rig.broker.stub.Says(limitedToken, sessionAnswer(limited, "ada@example.com", rig.caller.JKT()))
		rig.broker.stub.Says(anotherToken, sessionAnswer(another, "ada@example.com", rig.caller.JKT()))
		rig.register(t, limited)
		rig.register(t, another)
		frozen := time.Now()
		rig.deps.senderLimit = newSenderLimit(1, 1, func() time.Time { return frozen })
		t.Cleanup(func() { rig.deps.senderLimit = nil })
		send := func(token, source string) *httptest.ResponseRecorder {
			body := `{"target_session":"` + target + `","message":"a counted r9 message"}`
			if source != "" {
				body = `{"target_session":"` + target + `","source_session":"` + source + `","message":"a counted r9 message"}`
			}
			return rig.call(t, http.MethodPost, "/v1/messages/send", token, body)
		}
		if first := send(limitedToken, ""); first.Code != http.StatusOK {
			t.Fatalf("%s's first send, whose body names no session: status = %d, want 200; body = %s", limited, first.Code, first.Body.String())
		}
		if second := send(limitedToken, limited); second.Code != http.StatusTooManyRequests {
			t.Fatalf("%s's second send within the limit of one: status = %d, want 429: the limit counts the body's sender, not the verified session %s; body = %s",
				limited, second.Code, limited, second.Body.String())
		}
		if others := send(anotherToken, another); others.Code != http.StatusOK {
			t.Fatalf("%s's first send: status = %d, want 200: the limit charged %s's sends to it; body = %s", another, others.Code, limited, others.Body.String())
		}
	})

	t.Run("the read routes answer as before", func(t *testing.T) {
		for _, path := range []string{"/v1/registry/" + other, "/v1/interests/" + other, "/v1/sessions"} {
			if answer := rig.call(t, http.MethodGet, path, token, ""); answer.Code != http.StatusOK {
				t.Fatalf("GET %s under a token naming %s: status = %d, want 200; body = %s", path, bound, answer.Code, answer.Body.String())
			}
		}
	})

	t.Run("a broker outage", func(t *testing.T) {
		checkR9Outage(t, rig, target)
	})
}

// checkR9Outage is the outage half of checkR9: what a token verified once before the broker
// stopped answering is served, and when it stops being served.
func checkR9Outage(t *testing.T, rig *closedBrokerRig, target string) {
	const (
		live          = "ses_r9_live"
		short         = "ses_r9_short"
		unregistered  = "ses_r9_unregistered"
		liveToken     = "access-token-living-an-hour"
		shortToken    = "access-token-ending-in-seconds"
		newcomerToken = "access-token-of-a-session-not-yet-registered"
	)
	hour := time.Now().Add(time.Hour).Unix()
	shortAnswer := sessionAnswer(short, "ada@example.com", rig.caller.JKT())
	shortAnswer.ExpiresAt = time.Now().Add(3 * time.Second).Unix()
	shortEnds := time.Unix(shortAnswer.ExpiresAt, 0)
	liveAnswer := sessionAnswer(live, "ada@example.com", rig.caller.JKT())
	liveAnswer.ExpiresAt = hour
	newcomerAnswer := sessionAnswer(unregistered, "ada@example.com", rig.caller.JKT())
	newcomerAnswer.ExpiresAt = hour
	rig.broker.stub.Says(liveToken, liveAnswer)
	rig.broker.stub.Says(shortToken, shortAnswer)
	rig.broker.stub.Says(newcomerToken, newcomerAnswer)
	rig.register(t, short)

	registration := `{"session_id":"` + live + `","topics":[],"delivery":{"consumer":true,"epoch":"r9-outage"}}`
	calls := []struct{ name, method, path, body string }{
		{"register", http.MethodPost, "/v1/interests/subscribe", registration},
		{"subscribe", http.MethodPost, "/v1/interests/subscribe",
			`{"session_id":"` + live + `","topics":["notifications.github.r9-outage"],"delivery":{"consumer":true,"epoch":"r9-outage"}}`},
		{"send", http.MethodPost, "/v1/messages/send",
			`{"target_session":"` + target + `","source_session":"` + live + `","message":"an r9 outage message"}`},
		{"the role claim", http.MethodPost, "/v1/roles/set", `{"session_id":"` + live + `","role":"r9-outage-role"}`},
	}
	eachServed := func(t *testing.T, when string) {
		t.Helper()
		for _, call := range calls {
			if answer := rig.call(t, call.method, call.path, liveToken, call.body); answer.Code != http.StatusOK {
				t.Fatalf("%s %s: %s with the live token: status = %d, want 200; body = %s", when, call.name, call.path, answer.Code, answer.Body.String())
			}
		}
	}
	unavailable := func(t *testing.T, when, name string, answer *httptest.ResponseRecorder) {
		t.Helper()
		if answer.Code != http.StatusServiceUnavailable || answer.Header().Get("Retry-After") != "1" {
			t.Fatalf("%s %s: status = %d with Retry-After %q, want 503 with Retry-After: 1; body = %s",
				when, name, answer.Code, answer.Header().Get("Retry-After"), answer.Body.String())
		}
	}

	// Each token is verified once while the broker answers. The reuse of the live token's answer
	// starts when that answer arrives, which is between verifying and verified.
	if answer := rig.call(t, http.MethodGet, "/v1/sessions", shortToken, ""); answer.Code != http.StatusOK {
		t.Fatalf("the short-lived token while the broker answers: status = %d, want 200; body = %s", answer.Code, answer.Body.String())
	}
	verifying := time.Now()
	if answer := rig.call(t, http.MethodPost, "/v1/interests/subscribe", liveToken, registration); answer.Code != http.StatusOK {
		t.Fatalf("the live token's registration while the broker answers: status = %d, want 200; body = %s", answer.Code, answer.Body.String())
	}
	verified := time.Now()

	rig.broker.down.Store(true)
	asked := rig.broker.stub.Count()
	eachServed(t, "as the broker stops answering")

	// The short-lived token's answer ends with the token, inside the reuse's window.
	time.Sleep(time.Until(shortEnds.Add(300 * time.Millisecond)))
	if reuseEnds := verifying.Add(ciphertoken.AffirmativeCacheTTL); !time.Now().Before(reuseEnds) {
		t.Fatalf("the short-lived token ended %s after the live token was verified, past the %s reuse; the run cannot tell the token's expiry from the reuse's end",
			time.Since(verifying), ciphertoken.AffirmativeCacheTTL)
	}
	unavailable(t, "past the short-lived token's expiry, inside the reuse's window,", "a call with that token",
		rig.call(t, http.MethodGet, "/v1/sessions", shortToken, ""))
	eachServed(t, "inside the live token's reuse")

	// Past the reuse, every call the live token makes asks the broker, which does not answer.
	time.Sleep(time.Until(verified.Add(ciphertoken.AffirmativeCacheTTL + 300*time.Millisecond)))
	for _, call := range calls {
		unavailable(t, "past the reuse with the broker down,", call.name, rig.call(t, call.method, call.path, liveToken, call.body))
	}
	newcomer := `{"session_id":"` + unregistered + `","topics":["notifications.github.r9-outage"],"delivery":{"consumer":true,"epoch":"r9-outage"}}`
	unavailable(t, "with the broker down,", "the registration of a session not yet registered",
		rig.call(t, http.MethodPost, "/v1/interests/subscribe", newcomerToken, newcomer))
	if _, err := rig.deps.sessions.Refresh(context.Background(), unregistered); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("%s, whose registration the outage refused, has a record (err = %v); want none, so nothing is delivered to it", unregistered, err)
	}
	if _, err := rig.deps.registry.Get(context.Background(), unregistered); err == nil {
		t.Fatalf("%s, whose registration the outage refused, has an interest; want none, so nothing is delivered to it", unregistered)
	}
	if reached := rig.broker.stub.Count() - asked; reached != 0 {
		t.Fatalf("%d introspections reached the broker while it was down, want none", reached)
	}

	rig.broker.down.Store(false)
	eachServed(t, "once the broker answers again")
	if answer := rig.call(t, http.MethodPost, "/v1/interests/subscribe", newcomerToken, newcomer); answer.Code != http.StatusOK {
		t.Fatalf("once the broker answers again, the registration of %s: status = %d, want 200; body = %s", unregistered, answer.Code, answer.Body.String())
	}
}
