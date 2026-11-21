package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/config"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	listenerTestAudience = "envoy"
	listenerTestSubject  = "system:serviceaccount:legion:legion-worker"
)

func TestAPIAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	issuer := oidctest.New(t)
	signing := issuer.PublishKey(t, "listener-test-key")
	verifier, err := oidc.New(context.Background(), issuer.URL(), listenerTestAudience)
	if err != nil {
		t.Fatalf("oidc.New(%q, %q): %v", issuer.URL(), listenerTestAudience, err)
	}

	serviceToken := issuer.Mint(t, signing, issuer.Claims(listenerTestSubject, listenerTestAudience))

	otherAudience := issuer.Claims(listenerTestSubject, listenerTestAudience)
	otherAudience["aud"] = []string{"dispatch"}
	otherAudienceToken := issuer.Mint(t, signing, otherAudience)

	const unauthorized = "{\"error\":\"unauthorized\"}\n"

	cases := []struct {
		name       string
		token      string
		oidc       bool
		path       string
		authorize  string
		wantStatus int
		wantBody   string
		// wantReason is the failure class the reject must log, empty when the
		// request must produce no rejection line at all.
		wantReason string
	}{
		{
			name:       "allows v1 requests when neither token nor verifier is configured",
			path:       "/v1/sessions",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "rejects v1 requests without a bearer token",
			token:      "expected-token",
			path:       "/v1/sessions",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			name:       "rejects v1 requests with a wrong bearer token",
			token:      "expected-token",
			path:       "/v1/sessions",
			authorize:  "Bearer wrong-token",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			name:       "allows v1 requests with the configured bearer token",
			token:      "expected-token",
			path:       "/v1/sessions",
			authorize:  "Bearer expected-token",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "allows health checks without the bearer token",
			token:      "expected-token",
			path:       "/healthz",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "allows metrics without the bearer token",
			token:      "expected-token",
			path:       "/metrics",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "allows webhook ingress without the bearer token",
			token:      "expected-token",
			path:       "/webhook/github",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "does not exempt the unused plural webhook path",
			token:      "expected-token",
			path:       "/webhooks/github",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			name:       "allows the shared token while a verifier is configured",
			token:      "expected-token",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer expected-token",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "allows a verified service-account token beside the shared token",
			token:      "expected-token",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer " + serviceToken,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "allows a verified service-account token when no shared token is set",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer " + serviceToken,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "rejects a service-account token minted for another audience",
			token:      "expected-token",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer " + otherAudienceToken,
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
			wantReason: "audience",
		},
		{
			name:       "rejects an unauthenticated request when only a verifier is configured",
			oidc:       true,
			path:       "/v1/sessions",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			name:       "rejects a garbage bearer when only a verifier is configured",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer not-a-service-account-token",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			name:       "rejects an empty bearer when only a verifier is configured",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer ",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			name:       "rejects a JWT-shaped bearer the verifier cannot verify",
			oidc:       true,
			path:       "/v1/sessions",
			authorize:  "Bearer not.a.jwt",
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
			wantReason: "malformed",
		},
		{
			name:       "allows health checks when only a verifier is configured",
			oidc:       true,
			path:       "/healthz",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "allows webhook ingress when only a verifier is configured",
			oidc:       true,
			path:       "/webhook/github",
			wantStatus: http.StatusNoContent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tc.path, nil)
			request.Host = "127.0.0.1:9020"
			if tc.authorize != "" {
				request.Header.Set("Authorization", tc.authorize)
			}

			var configured *oidc.Verifier
			if tc.oidc {
				configured = verifier
			}
			var logged bytes.Buffer
			apiAuth(listenerAPIAuth{token: tc.token, verifier: configured, logger: logging.NewWithWriter("test-machine", &logged)}, next).
				ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
			if body := recorder.Body.String(); body != tc.wantBody {
				t.Fatalf("body = %q, want %q", body, tc.wantBody)
			}
			// The class belongs in the operator's log and nowhere else: telling an
			// unauthenticated caller which credential was refused tells it which one
			// the listener is configured for.
			if strings.Contains(recorder.Body.String(), tc.wantReason) && tc.wantReason != "" {
				t.Fatalf("the 401 body names the failure class: %q", recorder.Body.String())
			}
			assertRejectionLog(t, logged.String(), tc.wantReason, tc.authorize)
		})
	}
}

// A service-account token the listener could not judge, because its issuer's key set cannot be
// read, is answered 503 with a Retry-After, so Dispatch tries the call again for up to 12 seconds
// rather than settling it failed at once, and the log names the class as it does for a refusal.
// The outage turns no verdict into a 503: a token for another audience is refused 401 during it,
// from its own claims. Once the keys read again, the same token is admitted.
func TestAPIAuthAnswersAnIssuerOutage503(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	issuer := oidctest.New(t)
	signing := issuer.PublishKey(t, "listener-test-key")
	keys, err := oidc.NewKeys(context.Background(), issuer.URL())
	if err != nil {
		t.Fatalf("oidc.NewKeys(%q): %v", issuer.URL(), err)
	}
	// A failed read holds the next one back for RetrySpacing (oidc's own tests move a clock past
	// it); here the read after the outage follows at once, so the recovery is one call.
	keys.RetrySpacing = 0
	verifier := oidc.NewWithKeys(issuer.URL(), listenerTestAudience, keys)
	token := issuer.Mint(t, signing, issuer.Claims(listenerTestSubject, listenerTestAudience))
	otherAudience := issuer.Claims(listenerTestSubject, listenerTestAudience)
	otherAudience["aud"] = []string{"dispatch"}
	otherAudienceToken := issuer.Mint(t, signing, otherAudience)
	serve := func(authorize string) (*httptest.ResponseRecorder, string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
		request.Host = "127.0.0.1:9020"
		request.Header.Set("Authorization", authorize)
		var logged bytes.Buffer
		apiAuth(listenerAPIAuth{verifier: verifier, logger: logging.NewWithWriter("test-machine", &logged)}, next).ServeHTTP(recorder, request)
		return recorder, logged.String()
	}

	// No key set has been read yet, so the outage is what answers rather than a cached key.
	issuer.FailKeys(http.StatusInternalServerError)
	recorder, logged := serve("Bearer " + token)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("while the issuer's key set answers 500: status = %d, want 503; body %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if body, want := recorder.Body.String(), "{\"error\":\"this identity token could not be checked; try again shortly\"}\n"; body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	assertRejectionLog(t, logged, "unavailable", "Bearer "+token)

	recorder, logged = serve("Bearer " + otherAudienceToken)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("a token for another audience during the outage: status = %d, want 401", recorder.Code)
	}
	assertRejectionLog(t, logged, "audience", "Bearer "+otherAudienceToken)

	issuer.FailKeys(0)
	if recorder, _ := serve("Bearer " + token); recorder.Code != http.StatusNoContent {
		t.Fatalf("once the key set reads again: status = %d, want 204", recorder.Code)
	}
}

// The listener's key-set wait stays inside the window Dispatch's client gives each call, so an
// issuer that hangs reaches Dispatch as the listener's 503, which it retries, never as its own
// timeout. It compares the two symbols, so a change to either that breaks the order fails here.
func TestListenerKeySetWaitIsInsideDispatchsCallWindow(t *testing.T) {
	if listenerKeySetWait >= dispatchenvoy.DefaultTimeout {
		t.Fatalf("listenerKeySetWait = %s, want below Dispatch's listener call window, envoy.DefaultTimeout = %s", listenerKeySetWait, dispatchenvoy.DefaultTimeout)
	}
}

// The issuer rotates its signing key while its key set cannot be read. The listener holds the old
// key, so a token signed with the new one sends it to read the key set again, and that read
// fails: no verdict on the token is reached, on the first call or on any call after it. Each call
// is therefore answered 503 with a Retry-After, as an outage at the first read is
// (TestAPIAuthAnswersAnIssuerOutage503), so Dispatch tries again rather than settling the delivery
// as failed on a live token. A 401 here tells Dispatch the token was refused when it was never
// judged: a listener that answered the first such call 503 and the second and third 401 had
// Dispatch settle the delivery as failed (internal/dispatch/api, retryableDeliveryFailure).
func TestAPIAuthAnswersARotationDuringAnIssuerOutage503(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	issuer := oidctest.New(t)
	first := issuer.PublishKey(t, "listener-key-1")
	verifier, err := oidc.New(context.Background(), issuer.URL(), listenerTestAudience)
	if err != nil {
		t.Fatalf("oidc.New(%q, %q): %v", issuer.URL(), listenerTestAudience, err)
	}
	serve := func(authorize string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
		request.Host = "127.0.0.1:9020"
		request.Header.Set("Authorization", authorize)
		apiAuth(listenerAPIAuth{verifier: verifier, logger: logging.NewWithWriter("test-machine", io.Discard)}, next).ServeHTTP(recorder, request)
		return recorder
	}

	// The listener reads the key set once and holds the first key.
	if recorder := serve("Bearer " + issuer.Mint(t, first, issuer.Claims(listenerTestSubject, listenerTestAudience))); recorder.Code != http.StatusNoContent {
		t.Fatalf("the first key with the key set up: status = %d, want 204", recorder.Code)
	}

	// The issuer rotates to a second key, and its key set cannot be read.
	issuer.FailKeys(http.StatusInternalServerError)
	rotated := oidctest.NewKey(t, "listener-key-2")
	issuer.Publish(rotated)
	token := issuer.Mint(t, rotated, issuer.Claims(listenerTestSubject, listenerTestAudience))
	for call := 1; call <= 3; call++ {
		recorder := serve("Bearer " + token)
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") == "" {
			t.Errorf("call %d with a rotated key while the key set answers 500: status = %d, Retry-After %q; want 503 with a Retry-After, since the key set could not be read and no verdict was reached",
				call, recorder.Code, recorder.Header().Get("Retry-After"))
		}
	}
}

// An issuer that accepts the key-set request and never answers is an outage as much as one that
// answers 500: the listener, its verifier built as the listener builds it at boot
// (discoverListenerVerifier), answers a token it cannot judge 503 with a Retry-After, and inside
// the window Dispatch's client gives the call (envoy.DefaultTimeout), so Dispatch reads an answer
// it retries rather than its own timeout. The key-set read goes on behind the answer, under the
// verifier's FetchTimeout.
func TestAPIAuthAnswersAnIssuerThatHangs503InsideDispatchsWindow(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	issuer := oidctest.New(t)
	signing := issuer.PublishKey(t, "listener-test-key")
	verifier, err := discoverListenerVerifier(context.Background(), issuer.URL(), listenerTestAudience)
	if err != nil {
		t.Fatalf("discoverListenerVerifier(%q, %q): %v", issuer.URL(), listenerTestAudience, err)
	}
	token := issuer.Mint(t, signing, issuer.Claims(listenerTestSubject, listenerTestAudience))
	issuer.DelayKeys(time.Hour)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	request.Host = "127.0.0.1:9020"
	request.Header.Set("Authorization", "Bearer "+token)
	start := time.Now()
	apiAuth(listenerAPIAuth{verifier: verifier, logger: logging.NewWithWriter("test-machine", io.Discard)}, next).ServeHTTP(recorder, request)
	elapsed := time.Since(start)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") == "" || elapsed >= dispatchenvoy.DefaultTimeout {
		t.Fatalf("while the issuer's key set hangs: status %d, Retry-After %q after %s; want 503 with a Retry-After inside Dispatch's %s",
			recorder.Code, recorder.Header().Get("Retry-After"), elapsed.Round(time.Millisecond), dispatchenvoy.DefaultTimeout)
	}
	t.Logf("the listener answered %d after %s", recorder.Code, elapsed.Round(time.Millisecond))
}

func TestAPIAuthValidatesV1BodyContentType(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	cases := []struct {
		name          string
		path          string
		contentType   string
		unknownLength bool
		wantStatus    int
	}{
		{
			name:        "allows JSON parameters",
			path:        "/v1/messages/publish",
			contentType: "application/json; charset=utf-8",
			wantStatus:  http.StatusNoContent,
		},
		{
			name:       "refuses missing content type",
			path:       "/v1/messages/publish",
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name:        "refuses non JSON content type",
			path:        "/v1/messages/publish",
			contentType: "text/plain",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			// A chunked body reaches the handler with ContentLength -1, so the gate must not
			// read an unknown length as an empty body.
			name:          "refuses non JSON body of unknown length",
			path:          "/v1/messages/publish",
			contentType:   "text/plain",
			unknownLength: true,
			wantStatus:    http.StatusUnsupportedMediaType,
		},
		{
			// ParseMediaType still returns "application/json" beside its error here, so the
			// gate must refuse on the error rather than on the type alone.
			name:        "refuses malformed JSON content type",
			path:        "/v1/messages/publish",
			contentType: "application/json; charset",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			name:        "leaves webhooks to their signature handler",
			path:        "/webhook/github",
			contentType: "text/plain",
			wantStatus:  http.StatusNoContent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requestBody io.Reader = strings.NewReader(`{}`)
			if tc.unknownLength {
				requestBody = io.NopCloser(requestBody)
			}
			request := httptest.NewRequest(http.MethodPost, tc.path, requestBody)
			request.Host = "127.0.0.1:9020"
			if tc.unknownLength && request.ContentLength != -1 {
				t.Fatalf("ContentLength = %d, want -1 for a body of unknown length", request.ContentLength)
			}
			if tc.contentType != "" {
				request.Header.Set("Content-Type", tc.contentType)
			}
			recorder := httptest.NewRecorder()

			apiAuth(listenerAPIAuth{logger: logging.NewWithWriter("test-machine", io.Discard)}, next).ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if tc.wantStatus == http.StatusUnsupportedMediaType {
				if got := recorder.Header().Get("Content-Type"); got != "application/json" {
					t.Fatalf("Content-Type = %q, want application/json", got)
				}
				if body := recorder.Body.String(); body != "{\"error\":\"Content-Type must be application/json\"}\n" {
					t.Fatalf("body = %q, want JSON Content-Type refusal", body)
				}
			}
		})
	}
}

// assertRejectionLog holds the listener to one rejection line per refused
// service-account token, naming the class and never repeating the bearer.
func assertRejectionLog(t *testing.T, logged, wantReason, authorize string) {
	t.Helper()
	raw, bearer := strings.CutPrefix(authorize, "Bearer ")
	if !bearer {
		raw = ""
	}
	assertRejectionLines(t, logged, "listener: service-account token rejected", wantReason, "", raw)
}

// assertRejectionLines holds the listener to one line carrying message per refused token, naming
// the class wantReason, naming the account wantAccount when one is expected, and never repeating
// raw, the token it refused. An empty wantReason expects no line.
func assertRejectionLines(t *testing.T, logged, message, wantReason, wantAccount, raw string) {
	t.Helper()
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if !strings.Contains(line, message) {
			continue
		}
		lines++
		if !strings.Contains(line, `"reason":"`+wantReason+`"`) {
			t.Fatalf("rejection line does not name reason %q: %s", wantReason, line)
		}
		if wantAccount != "" && !strings.Contains(line, `"account":"`+wantAccount+`"`) {
			t.Fatalf("rejection line does not name the account %q: %s", wantAccount, line)
		}
	}
	want := 0
	if wantReason != "" {
		want = 1
	}
	if lines != want {
		t.Fatalf("logged %d rejection lines %q, want %d; log: %s", lines, message, want, logged)
	}
	if raw != "" && strings.Contains(logged, raw) {
		t.Fatalf("the log repeats the token it refused: %s", logged)
	}
}

func TestValidateListenerAPIAuth(t *testing.T) {
	issuer := oidctest.New(t)
	verifier, err := oidc.New(context.Background(), issuer.URL(), listenerTestAudience)
	if err != nil {
		t.Fatalf("oidc.New(%q, %q): %v", issuer.URL(), listenerTestAudience, err)
	}
	google := newGoogleStandIn(t, listenerGoogleAccount)

	cases := []struct {
		name                 string
		listenHost           string
		apiToken             string
		allowUnauthenticated string
		oidc                 bool
		google               bool
		wantError            bool
	}{
		{
			name:       "allows loopback IPv4 without a token",
			listenHost: "127.0.0.1",
		},
		{
			name:       "allows loopback IPv6 without a token",
			listenHost: "::1",
		},
		{
			name:       "allows localhost without a token",
			listenHost: "localhost",
		},
		{
			name:       "rejects wildcard bind without a token or a verifier",
			listenHost: "0.0.0.0",
			wantError:  true,
		},
		{
			name:       "allows non-loopback bind with a token",
			listenHost: "0.0.0.0",
			apiToken:   "expected-token",
		},
		{
			name:       "allows non-loopback bind with only a configured verifier",
			listenHost: "0.0.0.0",
			oidc:       true,
		},
		{
			name:       "allows non-loopback bind with only Google identities",
			listenHost: "0.0.0.0",
			google:     true,
		},
		{
			name:                 "allows explicit unauthenticated non-loopback bind",
			listenHost:           "0.0.0.0",
			allowUnauthenticated: "1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := listenerAPIAuth{token: tc.apiToken}
			if tc.oidc {
				auth.verifier = verifier
			}
			if tc.google {
				auth.google = google.identities
			}
			err := validateListenerAPIAuth(config.Service{ListenHost: tc.listenHost}, auth, tc.allowUnauthenticated)
			if (err != nil) != tc.wantError {
				t.Fatalf("validateListenerAPIAuth(%q, token set: %t, allow unauthenticated: %q, verifier set: %t, google set: %t) error = %v, want error: %t", tc.listenHost, tc.apiToken != "", tc.allowUnauthenticated, tc.oidc, tc.google, err, tc.wantError)
			}
		})
	}
}

func TestResolveListenerOIDCConfig(t *testing.T) {
	cases := []struct {
		name         string
		env          map[string]string
		wantIssuer   string
		wantAudience string
		wantMissing  string
	}{
		{
			name: "neither variable set leaves the verifier unconfigured",
			env:  map[string]string{},
		},
		{
			name: "both variables set configure the verifier",
			env: map[string]string{
				"ENVOY_OIDC_ISSUER":   "https://oidc.eks.example/id/CLUSTER",
				"ENVOY_OIDC_AUDIENCE": "envoy",
			},
			wantIssuer:   "https://oidc.eks.example/id/CLUSTER",
			wantAudience: "envoy",
		},
		{
			name:        "issuer without audience names the missing audience",
			env:         map[string]string{"ENVOY_OIDC_ISSUER": "https://oidc.eks.example/id/CLUSTER"},
			wantMissing: "ENVOY_OIDC_AUDIENCE",
		},
		{
			name:        "audience without issuer names the missing issuer",
			env:         map[string]string{"ENVOY_OIDC_AUDIENCE": "envoy"},
			wantMissing: "ENVOY_OIDC_ISSUER",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issuer, audience, err := resolveListenerOIDCConfig(func(key string) string { return tc.env[key] })
			if tc.wantMissing != "" {
				if err == nil {
					t.Fatalf("resolveListenerOIDCConfig(%v) error = nil, want one naming %s", tc.env, tc.wantMissing)
				}
				if !strings.Contains(err.Error(), tc.wantMissing) {
					t.Fatalf("resolveListenerOIDCConfig(%v) error = %q, want it to name %s", tc.env, err, tc.wantMissing)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveListenerOIDCConfig(%v) error = %v, want nil", tc.env, err)
			}
			if issuer != tc.wantIssuer || audience != tc.wantAudience {
				t.Fatalf("resolveListenerOIDCConfig(%v) = (%q, %q), want (%q, %q)", tc.env, issuer, audience, tc.wantIssuer, tc.wantAudience)
			}
		})
	}
}

func TestDescribeListenerAPIAuth(t *testing.T) {
	cases := []struct {
		name     string
		token    string
		issuer   string
		accounts []string
		google   *googleIdentities
		brokers  *brokerTokens
		want     string
	}{
		{
			name: "neither is disabled",
			want: "disabled",
		},
		{
			name:  "only the shared token",
			token: "expected-token",
			want:  "shared token",
		},
		{
			name:   "only the verifier names its issuer",
			issuer: "https://oidc.eks.example/id/CLUSTER",
			want:   "oidc https://oidc.eks.example/id/CLUSTER",
		},
		{
			name:   "both name both",
			token:  "expected-token",
			issuer: "https://oidc.eks.example/id/CLUSTER",
			want:   "shared token and oidc https://oidc.eks.example/id/CLUSTER",
		},
		{
			name:     "the verifier names the subjects ENVOY_OIDC_ACCOUNTS holds it to",
			issuer:   "https://oidc.eks.example/id/CLUSTER",
			accounts: []string{"system:serviceaccount:dispatch:envoy-broker", "system:serviceaccount:dispatch:dispatch"},
			want:     "oidc https://oidc.eks.example/id/CLUSTER for system:serviceaccount:dispatch:envoy-broker, system:serviceaccount:dispatch:dispatch",
		},
		{
			name:   "Google identities name their audience and accounts",
			google: &googleIdentities{audience: "https://listener.example", accounts: []string{"example-sink@example-project.iam.gserviceaccount.com"}},
			want:   "google https://listener.example for example-sink@example-project.iam.gserviceaccount.com",
		},
		{
			name: "only broker tokens name the broker and the projected token it calls as",
			brokers: newBrokerTokens(listenerBrokerConfig{
				BrokerURL:        "https://broker.example",
				AccountTokenFile: "/var/run/secrets/broker/token",
				Origin:           "https://listener.example",
			}),
			want: "broker tokens at https://broker.example as projected token /var/run/secrets/broker/token",
		},
		{
			name: "a listener on a VM names the audience it mints its machine identity for",
			brokers: newBrokerTokens(listenerBrokerConfig{
				BrokerURL:      "https://broker.example",
				GoogleAudience: "legion-broker-pod",
				Origin:         "https://listener.example",
			}),
			want: "broker tokens at https://broker.example as google audience legion-broker-pod",
		},
		{
			name:   "all three name all three",
			token:  "expected-token",
			issuer: "https://oidc.eks.example/id/CLUSTER",
			brokers: newBrokerTokens(listenerBrokerConfig{
				BrokerURL:        "https://broker.example",
				AccountTokenFile: "/var/run/secrets/broker/token",
				Origin:           "https://listener.example",
			}),
			want: "shared token and oidc https://oidc.eks.example/id/CLUSTER and broker tokens at https://broker.example as projected token /var/run/secrets/broker/token",
		},
		{
			name:   "every credential names itself, the bearers first",
			token:  "expected-token",
			issuer: "https://oidc.eks.example/id/CLUSTER",
			google: &googleIdentities{audience: "https://listener.example", accounts: []string{"example-sink@example-project.iam.gserviceaccount.com"}},
			brokers: newBrokerTokens(listenerBrokerConfig{
				BrokerURL:      "https://broker.example",
				GoogleAudience: "legion-broker-pod",
				Origin:         "https://listener.example",
			}),
			want: "shared token and oidc https://oidc.eks.example/id/CLUSTER and google https://listener.example for example-sink@example-project.iam.gserviceaccount.com and broker tokens at https://broker.example as google audience legion-broker-pod",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := listenerAPIAuth{token: tc.token, accounts: tc.accounts, google: tc.google, brokers: tc.brokers}
			if got := describeListenerAPIAuth(auth, tc.issuer); got != tc.want {
				t.Fatalf("describeListenerAPIAuth(token set: %t, %q, accounts %q, google set: %t, brokers set: %t) = %q, want %q", tc.token != "", tc.issuer, tc.accounts, tc.google != nil, tc.brokers != nil, got, tc.want)
			}
		})
	}
}

func TestAPIAuthRefusesHostsAnUnauthenticatedListenerDoesNotServe(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	allowed := []string{"hub.internal.example", "192.0.2.10"}
	cases := []struct {
		name       string
		host       string
		path       string
		token      string
		wantStatus int
	}{
		{name: "refuses a rebinding host with its port", host: "rebind.attacker.example:9020", path: "/v1/messages/publish", wantStatus: http.StatusMisdirectedRequest},
		{name: "refuses a rebinding host without a port", host: "rebind.attacker.example", path: "/v1/sessions", wantStatus: http.StatusMisdirectedRequest},
		{name: "refuses a host that only ends in an allowed name", host: "evil-hub.internal.example:9020", path: "/v1/sessions", wantStatus: http.StatusMisdirectedRequest},
		{name: "refuses a non-ASCII letter that lower-cases to an allowed one", host: "HUB.\u0130NTERNAL.EXAMPLE", path: "/v1/sessions", wantStatus: http.StatusMisdirectedRequest},
		{name: "refuses an address that only starts like an allowed one", host: "192.0.2.100:9020", path: "/v1/sessions", wantStatus: http.StatusMisdirectedRequest},
		{name: "refuses an empty host", host: "", path: "/v1/sessions", wantStatus: http.StatusMisdirectedRequest},
		{name: "refuses a non-loopback address", host: "192.0.2.7:9020", path: "/v1/sessions", wantStatus: http.StatusMisdirectedRequest},
		{name: "allows 127.0.0.1 with its port", host: "127.0.0.1:9020", path: "/v1/messages/publish", wantStatus: http.StatusNoContent},
		{name: "allows bracketed ::1 with its port", host: "[::1]:9020", path: "/v1/sessions", wantStatus: http.StatusNoContent},
		{name: "allows bracketed ::1 without a port", host: "[::1]", path: "/v1/sessions", wantStatus: http.StatusNoContent},
		{name: "allows localhost in any case", host: "LocalHost:9020", path: "/v1/sessions", wantStatus: http.StatusNoContent},
		{name: "allows localhost with a trailing dot", host: "localhost.:9020", path: "/v1/sessions", wantStatus: http.StatusNoContent},
		{name: "allows a configured address", host: "192.0.2.10:9020", path: "/v1/sessions", wantStatus: http.StatusNoContent},
		{name: "allows a configured host in any case", host: "HUB.Internal.Example:9020", path: "/v1/messages/publish", wantStatus: http.StatusNoContent},
		{name: "allows a configured host with a trailing dot", host: "hub.internal.example.", path: "/v1/sessions", wantStatus: http.StatusNoContent},
		{name: "leaves health checks to any host", host: "rebind.attacker.example:9020", path: "/healthz", wantStatus: http.StatusNoContent},
		{name: "leaves webhooks to any host", host: "rebind.attacker.example:9020", path: "/webhook/github", wantStatus: http.StatusNoContent},
		{
			// A bearer a rebinding page cannot hold already guards an authenticated listener, which
			// clients reach by names it does not list.
			name: "leaves an authenticated listener to its bearer", host: "listener.internal.example:9020",
			path: "/v1/sessions", token: "expected-token", wantStatus: http.StatusNoContent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{}`))
			request.Header.Set("Content-Type", "application/json")
			request.Host = tc.host
			if tc.token != "" {
				request.Header.Set("Authorization", "Bearer "+tc.token)
			}
			recorder := httptest.NewRecorder()
			var logged bytes.Buffer

			apiAuth(listenerAPIAuth{token: tc.token, allowedHosts: allowed, logger: logging.NewWithWriter("test-machine", &logged)}, next).ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("Host %q: status = %d, want %d; body = %s", tc.host, recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if tc.wantStatus == http.StatusMisdirectedRequest {
				if got := recorder.Header().Get("Content-Type"); got != "application/json" {
					t.Fatalf("Content-Type = %q, want application/json", got)
				}
				var body struct{ Error string }
				if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || !strings.Contains(body.Error, "ENVOY_ALLOWED_HOSTS") {
					t.Fatalf("body = %q (%v), want a JSON error naming ENVOY_ALLOWED_HOSTS", recorder.Body.String(), err)
				}
				if !strings.Contains(logged.String(), "listener: request for a host it does not serve") {
					t.Fatalf("log = %q, want the refusal line", logged.String())
				}
			} else if strings.Contains(logged.String(), "does not serve") {
				t.Fatalf("log = %q, want no refusal line for a served request", logged.String())
			}
		})
	}
}

// TestAnAllPublicListenerAcceptsADaemonsDPoPToken: a listener in the all-public mode, no privacy
// variable set, whose environment carries the four broker settings in their token-file form, as
// the webhook listener envoy-listener renders them wherever the deployment names its broker,
// answers a Legion daemon's POST /v1/messages/publish presented under the DPoP scheme, with its
// proof for the listener's own URL, 2xx. That listener requires a bearer of every other caller, as
// envoy-listener does, and it introspects the daemon's token as its own account: the broker is
// presented the projected token the token file holds, which the broker resolves to its caller
// listener. The daemon's request is the wire shape the Go client's DPoP transport produces
// (ciphertokentest.Caller: Authorization: DPoP <token> and a DPoP proof for the method and the
// listener's URL); the daemon's own sender is on no branch yet.
//
// Control: the same listener without the broker settings, the all-public render before them; it
// answers the daemon 401, and the test fails naming the status.
func TestAnAllPublicListenerAcceptsADaemonsDPoPToken(t *testing.T) {
	const (
		shared       = "shared-bearer-of-the-webhook-listener"
		daemonToken  = "access-token-of-a-legion-daemon"
		accountToken = "projected-token-of-the-listeners-own-account"
	)
	broker := ciphertokentest.NewStubBroker(t)
	daemon := ciphertokentest.NewCaller(t)
	broker.Says(daemonToken, serviceAnswer("legion-go-example", "legion-daemon", daemon.JKT()))
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(accountToken), 0o600); err != nil {
		t.Fatal(err)
	}
	withBroker := map[string]string{
		EnvBrokerURL:              broker.Server.URL,
		EnvBrokerAccountTokenFile: tokenFile,
		EnvPublicURL:              listenerBrokerOrigin,
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if caller, ok := brokerCaller(r); !ok || caller.Session != "legion-go-example" {
			t.Errorf("the publish reached its handler as %+v (bound %v), want the daemon's session", caller, ok)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	publish := func(env map[string]string) int {
		t.Helper()
		for _, name := range []string{"ENVOY_PROJECT_PRIVACY_URL", "ENVOY_PROJECT_PRIVACY_REQUIRED"} {
			if env[name] != "" {
				t.Fatalf("%s is set: this listener is in the all-public mode", name)
			}
		}
		brokers, err := resolveListenerBrokerConfig(func(name string) string { return env[name] })
		if err != nil {
			t.Fatalf("resolveListenerBrokerConfig: %v", err)
		}
		tokens := newBrokerTokens(brokers)
		if tokens != nil {
			verifier, err := ciphertoken.New(ciphertoken.Options{
				BrokerURL: brokers.BrokerURL, Account: brokers.account(), Origin: brokers.Origin,
				Replay: &spentProofsInMemory{}, Address: listenerSourceAddress,
			})
			if err != nil {
				t.Fatalf("build the broker-token verifier: %v", err)
			}
			tokens.verifier.Store(verifier)
		}
		auth := listenerAPIAuth{token: shared, brokers: tokens, logger: logging.NewWithWriter("test-machine", io.Discard)}
		recorder := httptest.NewRecorder()
		apiAuth(auth, next).ServeHTTP(recorder, brokerTokenRequest(t, daemon, http.MethodPost, "/v1/messages/publish", daemonToken, `{}`))
		return recorder.Code
	}

	if status := publish(withBroker); status < 200 || status > 299 {
		t.Fatalf("the daemon's DPoP publish to an all-public listener with the broker settings: status = %d, want 2xx", status)
	}
	if bearers := broker.Bearers(); len(bearers) != 1 || bearers[0] != "Bearer "+accountToken {
		t.Fatalf("the broker was presented %q, want the listener's own account token from %s, once", bearers, EnvBrokerAccountTokenFile)
	}
	if status := publish(map[string]string{}); status != http.StatusUnauthorized {
		t.Fatalf("the control, the same listener without the broker settings: status = %d, want 401", status)
	}
}
