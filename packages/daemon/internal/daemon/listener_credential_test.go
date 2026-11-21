package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/sjawhar/envoy/cipherclient"
	"github.com/sjawhar/envoy/ciphertest"
	"github.com/sjawhar/envoy/roleholder"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/controller"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// listenerRequest is one request as credentialListener received it.
type listenerRequest struct {
	method, path string
	header       http.Header
}

// credentialListener stands in for the Envoy listener: every route the daemon calls answers as
// the daemon reads a success — the publish 200, the controller role's lookup naming a live holder,
// a registration read naming a client instance, a delete 200 — and it keeps every request.
type credentialListener struct {
	*httptest.Server
	mu       sync.Mutex
	requests []listenerRequest
}

func newCredentialListener(t *testing.T) *credentialListener {
	t.Helper()
	l := &credentialListener{}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		l.mu.Lock()
		l.requests = append(l.requests, listenerRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone()})
		l.mu.Unlock()
		written := jsonNumber(time.Now().Add(-time.Second).UnixMilli())
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/roles/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"holder":"ses-controller","last_seen":`+written+`}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"session_id":"ses-role","state":"live","client_instance_id":"inst-1","last_seen":`+written+`,"updated_at":`+written+`}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(l.Close)
	return l
}

func jsonNumber(n int64) string {
	encoded, _ := json.Marshal(n)
	return string(encoded)
}

// received is every request the stand-in has received, in order.
func (l *credentialListener) received() []listenerRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.requests)
}

// listenerBrokerConfig is a daemon of project EXAMPLE under runtime: kubernetes that holds no Envoy
// bearer, beside the exchange audience every enrolled pod trades, as the loader takes it
// (config.Config.ListenerPresentsBrokerToken).
func listenerBrokerConfig(envoyURL string) config.Config {
	return config.Config{
		Project: "EXAMPLE", EnvoyURL: envoyURL,
		Runtime: config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{
			AgentSecrets: &config.AgentSecretsConfig{URL: "https://secrets.internal.example", ExchangeAudience: "legion-broker-exchange"},
		}},
	}
}

// callListener makes, against the listener at cfg.EnvoyURL under credential, every call the daemon
// makes of it, through the function the daemon makes each with: the outbox's publish
// (listenerCredential.publisher), the supervisor's role lookup (roleHolderLookup) and clean stop
// (cleanSessionStop), and the controller sweep's liveness probe (controllerProber).
func callListener(t *testing.T, cfg config.Config, credential listenerCredential) {
	t.Helper()
	ctx := context.Background()
	if err := credential.publisher(cfg.EnvoyURL).Publish(ctx, notify.RoleTopicPrefix+"legion-example-architect", "a notice", map[string]any{}, "dedupe-1"); err != nil {
		t.Fatalf("the outbox's publish: %v", err)
	}
	if _, _, err := roleHolderLookup(cfg.EnvoyURL, credential)(ctx, "legion-example-controller"); err != nil {
		t.Fatalf("the supervisor's role lookup: %v", err)
	}
	var inFlight sessionStopsInFlight
	cleanSessionStop(&inFlight, cfg.EnvoyURL, credential, quietLogger())("ses-role")
	inFlight.wait()
	if got := controllerProber(cfg, plan{project: "example", listener: credential}, quietLogger()).Probe(ctx, "ses-controller"); got != controller.Live {
		t.Fatalf("the controller sweep's probe = %s, want live", got)
	}
}

// The calls the boot's probe (listenerCredentialOf) and then callListener make, as the listener
// sees each: method and path.
var listenerCalls = []string{
	"GET /v1/roles/legion-example-controller",
	"POST /v1/messages/publish",
	"GET /v1/roles/legion-example-controller",
	"GET /v1/registry/ses-role",
	"DELETE /v1/sessions/ses-role",
	"GET /v1/roles/legion-example-controller",
}

// requireDPoP fails unless request presents token under DPoP beside a proof of its own: an ES256
// JWS carrying its public key, signed by that key, naming the request's method and the listener's
// URL with the request's path, and carrying ath, the hash of token, which is what the listener's
// verifier holds a presentation to (packages/envoy/internal/ciphertoken).
func requireDPoP(t *testing.T, listenerURL string, request listenerRequest, token string) {
	t.Helper()
	what := request.method + " " + request.path
	if got := request.header.Get("Authorization"); got != "DPoP "+token {
		t.Fatalf("%s carried Authorization %q, want the daemon's broker token under DPoP", what, got)
	}
	proof := request.header.Get("DPoP")
	signed, err := jose.ParseSigned(proof, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(signed.Signatures) != 1 {
		t.Fatalf("%s carried DPoP %q, want one ES256 proof: %v", what, proof, err)
	}
	header := signed.Signatures[0].Protected
	if header.JSONWebKey == nil || header.ExtraHeaders["typ"] != "dpop+jwt" {
		t.Fatalf("%s carried a proof with header %+v, want typ dpop+jwt and the key it is signed with", what, header)
	}
	payload, err := signed.Verify(header.JSONWebKey)
	if err != nil {
		t.Fatalf("%s carried a proof its own key does not verify: %v", what, err)
	}
	var claims struct {
		HTM string `json:"htm"`
		HTU string `json:"htu"`
		ATH string `json:"ath"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("%s carried a proof whose claims do not decode: %v", what, err)
	}
	hash := sha256.Sum256([]byte(token))
	if claims.HTM != request.method || claims.HTU != listenerURL+request.path || claims.ATH != base64.RawURLEncoding.EncodeToString(hash[:]) {
		t.Fatalf("%s carried a proof naming %s %s with ath %q, want %s %s%s and the hash of the token beside it",
			what, claims.HTM, claims.HTU, claims.ATH, request.method, listenerURL, request.path)
	}
}

// TestPublishPresentsTheDaemonsBrokerToken: a daemon under runtime: kubernetes that holds no Envoy
// bearer, beside runtime.kubernetes.agent_secrets.exchange_audience, trades its own pod identity at
// the secrets broker once at boot and presents the token it gets, `Authorization: DPoP` beside a
// proof of each request's own URL, on every call it makes of the listener: the outbox's publish,
// the supervisor's role lookup and clean stop, and the controller sweep's probe. The control is a
// daemon that holds the bearer: each of the same calls carries `Bearer <the file's token>` and no
// proof, as at the merge base, and the broker is asked nothing.
func TestPublishPresentsTheDaemonsBrokerToken(t *testing.T) {
	t.Run("no envoy_token_file, under exchange_audience", func(t *testing.T) {
		broker := newCredentialBroker(t, mintsTheDaemonsToken)
		listener := newCredentialListener(t)
		cfg := listenerBrokerConfig(listener.URL)
		var logged bytes.Buffer
		credentials, err := settleCredentials(t.Context(), cfg, "", "", getenvOf(podIdentity(t, broker.url)), slog.New(slog.NewTextHandler(&logged, nil)))
		if err != nil {
			t.Fatalf("settleCredentials: %v", err)
		}
		if line := `msg="legion daemon's Envoy listener credential" credential=broker session=legion-daemon:EXAMPLE`; !strings.Contains(logged.String(), line) || strings.Contains(logged.String(), daemonBrokerToken) {
			t.Fatalf("the boot logged\n%s\nwant the line %s, and no token", logged.String(), line)
		}
		callListener(t, cfg, credentials.listener)
		requests := listener.received()
		var seen []string
		for _, request := range requests {
			seen = append(seen, request.method+" "+request.path)
			requireDPoP(t, listener.URL, request, daemonBrokerToken)
		}
		if !slices.Equal(seen, listenerCalls) {
			t.Fatalf("the listener received %v, want %v", seen, listenerCalls)
		}
		if trades := broker.trades.Load(); trades != 1 {
			t.Fatalf("the broker was asked %d trades, want the one boot made: every call presents the token in hand", trades)
		}
	})

	t.Run("control: envoy_token_file's bearer", func(t *testing.T) {
		broker := newCredentialBroker(t, mintsTheDaemonsToken)
		listener := newCredentialListener(t)
		cfg := listenerBrokerConfig(listener.URL)
		cfg.EnvoyTokenFile = "/var/run/legion/ENVOY_TOKEN"
		var logged bytes.Buffer
		credentials, err := settleCredentials(t.Context(), cfg, "", "envoy-bearer", getenvOf(podIdentity(t, broker.url)), slog.New(slog.NewTextHandler(&logged, nil)))
		if err != nil {
			t.Fatalf("settleCredentials: %v", err)
		}
		if strings.Contains(logged.String(), "Envoy listener credential") {
			t.Fatalf("a daemon that holds the bearer logged %s, want no listener credential line, as it never logged one", logged.String())
		}
		callListener(t, cfg, credentials.listener)
		for _, request := range listener.received() {
			if got, proof := request.header.Get("Authorization"), request.header.Get("DPoP"); got != "Bearer envoy-bearer" || proof != "" {
				t.Fatalf("%s %s carried Authorization %q and DPoP %q, want the file's bearer and no proof", request.method, request.path, got, proof)
			}
		}
		if trades := broker.trades.Load(); trades != 0 {
			t.Fatalf("the broker was asked %d trades, want none: a daemon that holds the bearer trades nothing", trades)
		}
	})
}

// TestTheDaemonTradesAgainWhenTheListenerSaysItsTokenIsNotActive: a listener answers a broker
// token the broker ended early (a revoke) 401 with the code BROKER_TOKEN_NOT_ACTIVE, as Dispatch
// does, and the daemon's listener credential then drops that token, so its next call trades again
// and presents the new one at once, rather than present the dead token until its renewal comes
// due. The control is a listener that answers the plain 401 of every other refusal: the daemon's
// next call presents the same token again.
func TestTheDaemonTradesAgainWhenTheListenerSaysItsTokenIsNotActive(t *testing.T) {
	for name, tc := range map[string]struct {
		refusal  string
		retrades bool
	}{
		"the listener's not-active code": {`{"code":"BROKER_TOKEN_NOT_ACTIVE","error":"unauthorized"}`, true},
		"control: a plain 401":           {`{"error":"unauthorized"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			broker := newCredentialBroker(t, func(n int64) (int, map[string]any) {
				minted := "daemon-token-" + strconv.FormatInt(n, 10)
				return http.StatusOK, map[string]any{
					"access_token": minted, "token_type": "DPoP", "expires_in": 3600,
					"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
				}
			})
			var mu sync.Mutex
			var presented []string
			// The boot's own role lookup is answered unclaimed; the first publish after it is the
			// one refused, and every publish is kept with the token it carried.
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, `{"error":"no claim","reason":"unclaimed"}`)
					return
				}
				mu.Lock()
				presented = append(presented, strings.TrimPrefix(r.Header.Get("Authorization"), "DPoP "))
				first := len(presented) == 1
				mu.Unlock()
				if first {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, tc.refusal)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(listener.Close)
			cfg := listenerBrokerConfig(listener.URL)
			credentials, err := settleCredentials(t.Context(), cfg, "", "", getenvOf(podIdentity(t, broker.url)), quietLogger())
			if err != nil {
				t.Fatalf("settleCredentials: %v", err)
			}
			publisher := credentials.listener.publisher(listener.URL)
			publish := func() error {
				return publisher.Publish(t.Context(), notify.RoleTopicPrefix+"legion-example-architect", "a notice", map[string]any{}, "dedupe-1")
			}
			if err := publish(); err == nil {
				t.Fatal("the publish the listener answered 401 succeeded")
			}
			if err := publish(); err != nil {
				t.Fatalf("the publish after the 401: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := []string{"daemon-token-0", "daemon-token-0"}
			if tc.retrades {
				want[1] = "daemon-token-1"
			}
			if !slices.Equal(presented, want) {
				t.Fatalf("the listener was presented %v, want %v", presented, want)
			}
		})
	}
}

// TestOneBrokerTokenServesDispatchAndTheListener: a daemon under dispatch_token_broker that also
// holds no Envoy bearer trades its pod identity once, for the one session legion-daemon:EXAMPLE,
// and presents that token to Dispatch and to the listener alike: one client, one trade, one
// renewal (settleCredentials), not a second key and token of the same session for each server.
func TestOneBrokerTokenServesDispatchAndTheListener(t *testing.T) {
	broker := newCredentialBroker(t, mintsTheDaemonsToken)
	dispatchServer := newCredentialDispatch(t, daemonWhoami("EXAMPLE"))
	listener := newCredentialListener(t)
	cfg := listenerBrokerConfig(listener.URL)
	cfg.DispatchURL, cfg.DispatchTokenBroker = dispatchServer.URL, true
	credentials, err := settleCredentials(t.Context(), cfg, "", "", getenvOf(podIdentity(t, broker.url)), quietLogger())
	if err != nil {
		t.Fatalf("settleCredentials: %v", err)
	}
	callListener(t, cfg, credentials.listener)
	for _, request := range listener.received() {
		requireDPoP(t, listener.URL, request, daemonBrokerToken)
	}
	whoami := dispatchServer.received()
	if len(whoami) != 1 || whoami[0].header.Get("Authorization") != "DPoP "+daemonBrokerToken {
		t.Fatalf("Dispatch received %+v, want boot's one whoami carrying the same broker token", whoami)
	}
	if trades := broker.trades.Load(); trades != 1 {
		t.Fatalf("the broker was asked %d trades, want one for Dispatch and the listener together", trades)
	}
}

// TestTheDaemonsListenerTokenIsItsOwnDaemonToken: at the real broker, the token the daemon's pod
// identity trades for (ciphertest.DaemonExchangeAccount, the account whose exchange row fixes the
// session legion-daemon:EXAMPLE and the project EXAMPLE) introspects as exactly what the listener
// takes for a Legion daemon's own token (packages/envoy/cmd/listener's daemonCaller): that session,
// the named project scope, and the one project EXAMPLE. A listener that recognised a daemon by
// another shape would refuse this daemon's clean stop and count its notices.
func TestTheDaemonsListenerTokenIsItsOwnDaemonToken(t *testing.T) {
	if os.Getenv("BROKER_TEST_DATABASE_URL") == "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatal("BROKER_TEST_DATABASE_URL is unset on GitHub Actions: ci.yaml's daemon-go job names its Postgres service")
	}
	rig := ciphertest.NewRig(t)
	listener := newCredentialListener(t)
	env := map[string]string{
		cipherclient.BrokerURLVar: rig.URL, cipherclient.IdentityVar: cipherclient.IdentityKubernetes,
		cipherclient.MachineTokenFileVar: writeSecret(t, "daemon-exchange-token", rig.MintSubjectToken(t, ciphertest.DaemonExchangeAccount, "daemon-pod-uid")),
	}
	cfg := listenerBrokerConfig(listener.URL)
	credentials, err := settleCredentials(t.Context(), cfg, "", "", getenvOf(env), quietLogger())
	if err != nil {
		t.Fatalf("settleCredentials: %v", err)
	}
	if err := credentials.listener.publisher(listener.URL).Publish(t.Context(), notify.RoleTopicPrefix+"legion-example-architect", "a notice", map[string]any{}, "dedupe-1"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// The boot reads the controller role first under the same token (listenerCredentialOf); the
	// token read here is the publish's.
	var publishes []listenerRequest
	for _, request := range listener.received() {
		if request.method == http.MethodPost {
			publishes = append(publishes, request)
		}
	}
	if len(publishes) != 1 {
		t.Fatalf("the listener received %d publishes, want the one", len(publishes))
	}
	token, found := strings.CutPrefix(publishes[0].header.Get("Authorization"), "DPoP ")
	if !found || token == "" {
		t.Fatalf("the publish carried Authorization %q, want the broker's token under DPoP", publishes[0].header.Get("Authorization"))
	}
	requireDPoP(t, listener.URL, publishes[0], token)

	request, err := http.NewRequest(http.MethodPost, rig.URL+"/v1/oauth/introspect", strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer "+rig.ListenerToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("introspect the daemon's token: %v", err)
	}
	defer response.Body.Close()
	var read struct {
		Active       bool     `json:"active"`
		Session      string   `json:"session"`
		ProjectScope string   `json:"project_scope"`
		Projects     []string `json:"projects"`
	}
	if err := json.NewDecoder(response.Body).Decode(&read); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("introspect the daemon's token: %d (%v)", response.StatusCode, err)
	}
	if !read.Active || read.Session != "legion-daemon:EXAMPLE" || read.ProjectScope != "named" || !slices.Equal(read.Projects, []string{"EXAMPLE"}) {
		t.Fatalf("the broker reads the daemon's token as %+v, want an active token of the session legion-daemon:EXAMPLE, the named scope and the one project EXAMPLE", read)
	}
}

// TestNoEnvoyBearerIsHandedOutWithoutTheKey: a daemon configuration under runtime: kubernetes with
// no envoy_token_file, beside agent_secrets.exchange_audience, loads, and names no Envoy bearer
// among the secrets every launch carries, so no pod's Secret holds ENVOY_TOKEN and no container is
// told ENVOY_TOKEN_FILE: each pod's sessions trade their own broker token with the exchange identity
// the pod carries. The control is the same file with envoy_token_file: every launch carries the
// bearer, as before.
func TestNoEnvoyBearerIsHandedOutWithoutTheKey(t *testing.T) {
	for name, tc := range map[string]struct {
		line   string
		bearer bool
	}{
		"no envoy_token_file":           {"", false},
		"control: envoy_token_file set": {"envoy_token_file: ./envoy-token\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for _, file := range []string{"envoy-token", "operator-token", "dispatch-token"} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte("placeholder-"+file+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, "legion.yaml")
			if err := os.WriteFile(path, []byte(exchangeIdentityFile+tc.line), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadForValidation(path, nil)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			lookup := environLookup([]string{})
			names := launchSecretNames(cfg, lookup)
			secrets, err := readLaunchSecrets(cfg, lookup)
			if err != nil {
				t.Fatalf("read the launch secrets: %v", err)
			}
			_, held := secrets["ENVOY_TOKEN"]
			if slices.Contains(names, "ENVOY_TOKEN") != tc.bearer || held != tc.bearer {
				t.Fatalf("every launch carries the launch secrets %v (read %d), want ENVOY_TOKEN among them: %v", names, len(secrets), tc.bearer)
			}
			if err := checkOperatorConfig(cfg, lookup); err != nil {
				t.Fatalf("checkOperatorConfig: %v", err)
			}
			// The runtime is built from the same configuration: it hands no launch the bearer, and
			// holds a launch into a kept pod that carries no exchange identity to that pod's round.
			opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, "example", sandbox.DispatchCredential{Bearer: "placeholder-dispatch-token"}, lookup, quietLogger())
			if err != nil {
				t.Fatalf("sandboxOptions: %v", err)
			}
			if opts.ListenerBroker == tc.bearer || slices.Contains(opts.LaunchSecrets, "ENVOY_TOKEN") != tc.bearer {
				t.Fatalf("the runtime's options carry ListenerBroker %v and launch secrets %v, want ListenerBroker %v and ENVOY_TOKEN among them: %v",
					opts.ListenerBroker, opts.LaunchSecrets, !tc.bearer, tc.bearer)
			}
			if got := cfg.ListenerPresentsBrokerToken(); got == tc.bearer {
				t.Fatalf("ListenerPresentsBrokerToken() = %v, want %v: the daemon presents its broker token exactly where it holds no bearer", got, !tc.bearer)
			}
		})
	}
}

// exchangeIdentityFile is a configuration under runtime: kubernetes whose agent_secrets block sets
// exchange_audience, and which names no envoy_token_file: TestNoEnvoyBearerIsHandedOutWithoutTheKey
// appends one for its control. Its token files sit beside it.
const exchangeIdentityFile = `project: EXAMPLE
postgres_dsn: postgres://legion@127.0.0.1:1/legion
state_dir: ./state
bind: 192.0.2.10
daemon_url: http://192.0.2.10:13370
envoy_url: http://envoy-listener.internal.example:9020
operator_token_file: ./operator-token
dispatch_url: https://dispatch.internal.example
dispatch_token_file: ./dispatch-token
nats_urls: [nats://nats.internal.example:4222]
projects:
  EXAMPLE: {}
github_apps:
  implement: { app_id: "1", private_key_command: "exit 1" }
  review: { app_id: "2", private_key_command: "exit 1" }
runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/cybermonklabs/legion-worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    storage_class: standard
    session_store: postgres
    session_dsn_secret: SESSION_DSN
    agent_secrets:
      url: https://secrets.internal.example
      token_file: /var/run/legion/agent-secrets-token/token
      exchange_audience: legion-broker-exchange
`

// TestTheBootRefusesAListenerThatRefusesTheDaemonsToken: the boot goes on only on an answer the
// listener gives behind its credential check, and refuses on every other, naming envoy_url and
// ENVOY_PUBLIC_URL, rather than leave every listener call failing with no line saying why. A 401
// or 403 (a listener that takes no broker token, or whose ENVOY_PUBLIC_URL is not envoy_url), a
// redirect, and a 404 whose body is not the listener's (a server at envoy_url that is not the
// listener) each refuse it; a 503 before a refusal is waited out first. The controls: a 503, as a
// starting listener answers, is asked again, and the boot goes on once the listener answers that
// nobody holds the controller role; a 500, which only the listener's route handlers write, after
// the check, lets the boot go on after that one lookup. Each row counts the lookups the boot made.
func TestTheBootRefusesAListenerThatRefusesTheDaemonsToken(t *testing.T) {
	fastReadiness(t, bootprobe.Retry{Initial: time.Millisecond, Max: 4 * time.Millisecond})
	const (
		unclaimed = `{"error":"no claim","reason":"` + roleholder.Unclaimed + `"}`
		foreign   = `<html>404 page not found</html>`
	)
	type answer struct {
		status int
		body   string
	}
	for name, tc := range map[string]struct {
		answers []answer
		refused bool
		asked   int
	}{
		"a 401":                                {[]answer{{http.StatusUnauthorized, `{"error":"unauthorized"}`}}, true, 1},
		"a 403":                                {[]answer{{http.StatusForbidden, `{"error":"forbidden"}`}}, true, 1},
		"a redirect":                           {[]answer{{http.StatusTemporaryRedirect, ""}}, true, 1},
		"a 404 that is not the listener's":     {[]answer{{http.StatusNotFound, foreign}}, true, 1},
		"a 503, then a 401":                    {[]answer{{http.StatusServiceUnavailable, ""}, {http.StatusUnauthorized, `{"error":"unauthorized"}`}}, true, 2},
		"control: a 503, then no holder":       {[]answer{{http.StatusServiceUnavailable, ""}, {http.StatusNotFound, unclaimed}}, false, 2},
		"control: a 500 from behind the check": {[]answer{{http.StatusInternalServerError, `{"error":"the role claim does not decode"}`}}, false, 1},
	} {
		t.Run(name, func(t *testing.T) {
			broker := newCredentialBroker(t, mintsTheDaemonsToken)
			var mu sync.Mutex
			asked := 0
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				given := tc.answers[min(asked, len(tc.answers)-1)]
				asked++
				mu.Unlock()
				if given.status == http.StatusTemporaryRedirect {
					w.Header().Set("Location", "http://elsewhere.invalid"+r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(given.status)
				_, _ = io.WriteString(w, given.body)
			}))
			t.Cleanup(listener.Close)
			_, err := settleCredentials(t.Context(), listenerBrokerConfig(listener.URL), "", "", getenvOf(podIdentity(t, broker.url)), quietLogger())
			mu.Lock()
			made := asked
			mu.Unlock()
			if made != tc.asked {
				t.Fatalf("the boot made %d role lookups, want %d", made, tc.asked)
			}
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "envoy_url "+listener.URL) || !strings.Contains(err.Error(), "ENVOY_PUBLIC_URL") {
					t.Fatalf("settleCredentials = %v, want the boot refused naming envoy_url %s and ENVOY_PUBLIC_URL", err, listener.URL)
				}
				return
			}
			if err != nil {
				t.Fatalf("settleCredentials = %v, want the boot on", err)
			}
		})
	}
}
