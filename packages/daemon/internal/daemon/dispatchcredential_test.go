package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipherclient"
	"github.com/sjawhar/envoy/ciphertest"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
)

// daemonBrokerToken is the access token credentialBroker mints for the daemon, and
// daemonSubjectToken the projected service-account token the daemon's pod trades for it: neither
// may reach a log, and only the first may reach Dispatch.
const (
	daemonBrokerToken  = "broker-access-token-5d1c"
	daemonSubjectToken = "projected-subject-token-8b2e"
)

// credentialBroker is the secrets broker's token endpoint and nothing else: it answers the nth
// trade, counting from zero, with what answer gives.
type credentialBroker struct {
	url    string
	trades atomic.Int64
}

func newCredentialBroker(t *testing.T, answer func(n int64) (int, map[string]any)) *credentialBroker {
	t.Helper()
	broker := &credentialBroker{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/oauth/token" {
			t.Errorf("the broker was asked %s %s, want POST /v1/oauth/token", r.Method, r.URL.Path)
		}
		status, body := answer(broker.trades.Add(1) - 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	broker.url = server.URL
	return broker
}

// mintsTheDaemonsToken is a broker answer that trades every time for daemonBrokerToken.
func mintsTheDaemonsToken(int64) (int, map[string]any) {
	return http.StatusOK, map[string]any{
		"access_token": daemonBrokerToken, "token_type": "DPoP", "expires_in": 3600,
		"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
	}
}

// credentialRequest is one request as credentialDispatch received it.
type credentialRequest struct {
	method, target, body string
	header               http.Header
}

// credentialDispatch is a stand-in for Dispatch: GET /api/v1/whoami answers whoami, the issue list
// answers none, and every other route `{}`. It keeps every request.
type credentialDispatch struct {
	*httptest.Server
	mu       sync.Mutex
	whoami   map[string]any
	requests []credentialRequest
}

func newCredentialDispatch(t *testing.T, whoami map[string]any) *credentialDispatch {
	t.Helper()
	d := &credentialDispatch{whoami: whoami}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.requests = append(d.requests, credentialRequest{method: r.Method, target: r.URL.String(), body: string(body), header: r.Header.Clone()})
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/whoami":
			_ = json.NewEncoder(w).Encode(d.whoami)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues":
			_, _ = io.WriteString(w, "[]")
		default:
			_, _ = io.WriteString(w, "{}")
		}
	}))
	t.Cleanup(d.Close)
	return d
}

// received is every request the stand-in has received, in order.
func (d *credentialDispatch) received() []credentialRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]credentialRequest(nil), d.requests...)
}

// daemonWhoami is what Dispatch answers GET /api/v1/whoami for the daemon's token of project's
// daemon, which a fixed-session entry naming the operator mints.
func daemonWhoami(project string) map[string]any {
	return map[string]any{
		"kind": "agent", "owner": "ada@example.com", "project": project, "scopes": []string{},
		"service": nil, "session": dispatch.DaemonSession(project),
	}
}

// podIdentity is the environment of a daemon pod that proves its own identity at broker: the three
// variables R2 names, the projected token in a file of its own.
func podIdentity(t *testing.T, broker string) map[string]string {
	t.Helper()
	return map[string]string{
		"AGENT_SECRETS_URL":                broker,
		"AGENT_SECRETS_MACHINE_IDENTITY":   "kubernetes",
		"AGENT_SECRETS_MACHINE_TOKEN_FILE": writeSecret(t, "token", daemonSubjectToken),
	}
}

// getenvOf is os.Getenv over env.
func getenvOf(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

// brokerConfig is a daemon of project EXAMPLE on dispatch_token_broker, writing Dispatch at url.
func brokerConfig(url string) config.Config {
	return config.Config{Project: "EXAMPLE", DispatchURL: url, DispatchTokenBroker: true}
}

// writeSecret writes value to a file of its own and answers its path.
func writeSecret(t *testing.T, name, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTheBrokerSettingRefusesABootWithoutThePodIdentity: under dispatch_token_broker the daemon's
// Dispatch credential is its own pod identity, so a pod whose environment lacks one of the three
// variables R2 names refuses the boot naming the variable, and trades nothing.
func TestTheBrokerSettingRefusesABootWithoutThePodIdentity(t *testing.T) {
	broker := newCredentialBroker(t, mintsTheDaemonsToken)
	dispatchServer := newCredentialDispatch(t, daemonWhoami("EXAMPLE"))
	for missing, want := range map[string]string{
		"AGENT_SECRETS_MACHINE_TOKEN_FILE": "AGENT_SECRETS_MACHINE_TOKEN_FILE is required by AGENT_SECRETS_MACHINE_IDENTITY=kubernetes",
		"AGENT_SECRETS_URL":                "AGENT_SECRETS_URL names no secrets broker",
		"AGENT_SECRETS_MACHINE_IDENTITY":   "AGENT_SECRETS_MACHINE_IDENTITY names none",
	} {
		t.Run(missing, func(t *testing.T) {
			env := podIdentity(t, broker.url)
			delete(env, missing)
			transport, err := dispatchCredential(context.Background(), brokerConfig(dispatchServer.URL), "", getenvOf(env), quietLogger())
			if err == nil || !strings.Contains(err.Error(), want) || transport != nil {
				t.Fatalf("dispatchCredential without %s = %v, %v; want the boot refused naming it (%q)", missing, transport, err, want)
			}
		})
	}
	if trades := broker.trades.Load(); trades != 0 {
		t.Fatalf("the broker was asked %d trades, want none: no identity, no trade", trades)
	}
	if requests := dispatchServer.received(); len(requests) != 0 {
		t.Fatalf("Dispatch received %d requests, want none", len(requests))
	}
}

// TestAPermanentBrokerRefusalRefusesTheBoot: a broker that refuses the daemon's identity in a way
// no retry cures refuses the boot in the broker's own words, at its first trade, and Dispatch is
// never called; a broker answering 503 is logged and traded with again, at the boot gate's pacing,
// and the boot goes on.
func TestAPermanentBrokerRefusalRefusesTheBoot(t *testing.T) {
	fastReadiness(t, bootprobe.Retry{Initial: time.Millisecond, Max: 4 * time.Millisecond})
	const words = "system:serviceaccount:legion:legion-daemon is listed by no exchange entry"

	t.Run("a permanent refusal", func(t *testing.T) {
		broker := newCredentialBroker(t, func(int64) (int, map[string]any) {
			return http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": words}
		})
		dispatchServer := newCredentialDispatch(t, daemonWhoami("EXAMPLE"))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		transport, err := dispatchCredential(ctx, brokerConfig(dispatchServer.URL), "", getenvOf(podIdentity(t, broker.url)), quietLogger())
		if err == nil || !strings.Contains(err.Error(), "invalid_grant: "+words) || transport != nil {
			t.Fatalf("dispatchCredential = %v, %v; want the boot refused in the broker's words %q", transport, err, words)
		}
		if trades := broker.trades.Load(); trades != 1 {
			t.Fatalf("the broker was asked %d trades, want 1: a refusal no retry cures is not asked again", trades)
		}
		if requests := dispatchServer.received(); len(requests) != 0 {
			t.Fatalf("Dispatch received %d requests, want none", len(requests))
		}
	})

	t.Run("a 503", func(t *testing.T) {
		broker := newCredentialBroker(t, func(n int64) (int, map[string]any) {
			if n == 0 {
				return http.StatusServiceUnavailable, map[string]any{"error": "temporarily_unavailable"}
			}
			return mintsTheDaemonsToken(n)
		})
		dispatchServer := newCredentialDispatch(t, daemonWhoami("EXAMPLE"))
		var logged bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		transport, err := dispatchCredential(ctx, brokerConfig(dispatchServer.URL), "", getenvOf(podIdentity(t, broker.url)), slog.New(slog.NewTextHandler(&logged, nil)))
		if err != nil || transport == nil {
			t.Fatalf("dispatchCredential after a 503 = %v, %v; want the trade made again and the boot on\nlog:\n%s", transport, err, logged.String())
		}
		if trades := broker.trades.Load(); trades != 2 {
			t.Fatalf("the broker was asked %d trades, want 2: the 503 and the trade after it", trades)
		}
		if !strings.Contains(logged.String(), `msg="boot probe failed transiently; waiting to run it again" probe="daemon's Dispatch credential trade"`) ||
			!strings.Contains(logged.String(), "HTTP 503") {
			t.Fatalf("the 503 was not logged as a trade the boot makes again:\n%s", logged.String())
		}
	})
}

// TestTheBootRefusesAnEntryDispatchWouldRefuse: Dispatch refuses every write of a broker token whose
// session is not the daemon's, and of a machine's token that names no person, and the outbox would
// wait on each such write for ever. A token whose project is not the daemon's is a fault of the
// exchange entry that gave it. So the boot reads Dispatch's whoami of its own token and refuses
// each of the three, naming the field and the broker entry the deployment must fix. The right
// answer boots.
func TestTheBootRefusesAnEntryDispatchWouldRefuse(t *testing.T) {
	broker := newCredentialBroker(t, mintsTheDaemonsToken)
	for name, tc := range map[string]struct {
		change func(map[string]any)
		want   string
	}{
		"another session": {func(w map[string]any) { w["session"] = "example-worker" },
			`GET /api/v1/whoami answers the session "example-worker" for the daemon's broker token, not legion-daemon:EXAMPLE`},
		"no session": {func(w map[string]any) { w["session"] = nil },
			"GET /api/v1/whoami answers the session null for the daemon's broker token, not legion-daemon:EXAMPLE"},
		"another project": {func(w map[string]any) { w["project"] = "OTHER" },
			`GET /api/v1/whoami answers the project "OTHER" for the daemon's broker token, not EXAMPLE`},
		"no owner": {func(w map[string]any) { w["owner"] = nil },
			"GET /api/v1/whoami answers the owner null for the daemon's broker token"},
		"the daemon's own": {func(map[string]any) {}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			whoami := daemonWhoami("EXAMPLE")
			tc.change(whoami)
			dispatchServer := newCredentialDispatch(t, whoami)
			transport, err := dispatchCredential(context.Background(), brokerConfig(dispatchServer.URL), "", getenvOf(podIdentity(t, broker.url)), quietLogger())
			if tc.want == "" {
				if err != nil || transport == nil {
					t.Fatalf("dispatchCredential for the daemon's own whoami = %v, %v; want the boot on", transport, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "the deployment must fix the secrets broker's exchange entry for the daemon's own account") {
				t.Fatalf("dispatchCredential = %v; want the boot refused naming %q and the broker entry to fix", err, tc.want)
			}
			if transport != nil {
				t.Fatal("a refused boot answered a credential")
			}
		})
	}
}

// TestTheDaemonsClientNamesTheSessionItsTokenProves: under dispatch_token_broker the daemon's
// token proves the daemon's own session, legion-daemon:<its project>, which the boot's whoami
// check holds it to (checkDaemonCaller), and Dispatch refuses a write whose actor names any other
// session than the one the token proves (403 ACTOR_SESSION_MISMATCH). So the client the boot
// builds names that session on every write, on an issue of another project too, which the daemon
// writes when it runs a claim on that issue (legion claims spawn). With the setting off the
// daemon's bearer proves no session, and each write names the session of the daemon of the
// issue's project (dispatch.DaemonSession), as it did before the setting.
func TestTheDaemonsClientNamesTheSessionItsTokenProves(t *testing.T) {
	broker := newCredentialBroker(t, mintsTheDaemonsToken)
	for name, tc := range map[string]struct {
		broker bool
		want   string
	}{
		"dispatch_token_broker": {broker: true, want: "legion-daemon:EXAMPLE"},
		"dispatch_token_file":   {broker: false, want: "legion-daemon:OTHER"},
	} {
		t.Run(name, func(t *testing.T) {
			dispatchServer := newCredentialDispatch(t, daemonWhoami("EXAMPLE"))
			cfg, dispatchToken := brokerConfig(dispatchServer.URL), ""
			if !tc.broker {
				const fileToken = "dispatch-file-token-4e7a"
				cfg = config.Config{Project: "EXAMPLE", DispatchURL: dispatchServer.URL, DispatchTokenFile: writeSecret(t, "dispatch-token", fileToken)}
				dispatchToken = fileToken
			}
			transport, err := dispatchCredential(context.Background(), cfg, dispatchToken, getenvOf(podIdentity(t, broker.url)), quietLogger())
			if err != nil {
				t.Fatalf("dispatchCredential: %v", err)
			}
			client := daemonDispatchClient(cfg, transport)
			if err := client.PostMessage(context.Background(), "OTHER-1", "The daemon's notice on an issue of another project."); err != nil {
				t.Fatalf("post a message on OTHER-1: %v", err)
			}
			var posted []credentialRequest
			for _, request := range dispatchServer.received() {
				if request.method == http.MethodPost && request.target == "/api/v1/issues/OTHER-1/messages" {
					posted = append(posted, request)
				}
			}
			if len(posted) != 1 {
				t.Fatalf("Dispatch received %d messages on OTHER-1, want the one the client posted", len(posted))
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal([]byte(posted[0].body), &body); err != nil {
				t.Fatalf("decode the message on OTHER-1, %s: %v", posted[0].body, err)
			}
			var actor struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
			}
			if err := json.Unmarshal(body["actor"], &actor); err != nil || actor.Kind != "session" || actor.ID != tc.want {
				t.Errorf("the daemon's message on OTHER-1 names the actor %s (%v), want the session %s", body["actor"], err, tc.want)
			}
		})
	}
}

// TestTheDaemonNeverSendsTheTransitionToken: under dispatch_token_broker a transition file holds the
// shared Dispatch token a launch into a pod built before the setting gets, and the daemon's own
// client never presents it: boot plans no bearer for its own calls (plan.dispatchToken), which a
// boot reading the transition file as the daemon's bearer would set and which no request shows,
// since dispatchCredential reads no bearer under the setting; and every request the client sends
// Dispatch, the boot's whoami and the workflow's reads and writes, carries the broker's token under
// DPoP with a proof, and none carries the transition token's text, anywhere in it.
func TestTheDaemonNeverSendsTheTransitionToken(t *testing.T) {
	const transitionToken = "shared-transition-token-3a9f"
	broker := newCredentialBroker(t, mintsTheDaemonsToken)
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.EnvoyTokenFile = writeSecret(t, "envoy-token", "envoy-bearer")
	cfg.NatsNkeySeedFile = testnkeys.SeedFile(t, testnkeys.UserSeed(t))
	dispatchServer := newCredentialDispatch(t, daemonWhoami(cfg.Project))
	cfg.DispatchURL, cfg.DispatchTokenBroker = dispatchServer.URL, true
	cfg.DispatchTokenTransitionFile = writeSecret(t, "dispatch-transition-token", transitionToken)
	env := podIdentity(t, broker.url)
	var environ []string
	for name, value := range env {
		environ = append(environ, name+"="+value)
	}

	// The boot's own path: prepare reads the transition file (readBoot), and the credential is
	// settled from what prepare planned.
	p, err := prepare(cfg, quietLogger(), overrides{environ: environ})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if p.dispatchToken != "" {
		t.Fatalf("boot planned a Dispatch bearer for the daemon's own calls under dispatch_token_broker (the transition token: %t), want none",
			p.dispatchToken == transitionToken)
	}
	transport, err := dispatchCredential(context.Background(), cfg, p.dispatchToken, getenvOf(env), quietLogger())
	if err != nil {
		t.Fatalf("dispatchCredential: %v", err)
	}
	client := dispatch.New(cfg.DispatchURL, transport)
	issue := cfg.Project + "-1"
	if _, err := client.ListIssues(context.Background(), cfg.Project, nil); err != nil {
		t.Fatalf("list the project's issues: %v", err)
	}
	if _, err := client.GetIssue(context.Background(), issue); err != nil {
		t.Fatalf("read an issue: %v", err)
	}
	if err := client.PostMessage(context.Background(), issue, "a notice"); err != nil {
		t.Fatalf("post a notice: %v", err)
	}

	requests := dispatchServer.received()
	if len(requests) != 4 {
		t.Errorf("Dispatch received %d requests, want the boot's whoami and the three calls", len(requests))
	}
	for _, request := range requests {
		if request.header.Get("Authorization") != "DPoP "+daemonBrokerToken || request.header.Get("DPoP") == "" {
			t.Errorf("%s %s carried Authorization %q and DPoP %q, want the broker's token under DPoP beside a proof",
				request.method, request.target, request.header.Get("Authorization"), request.header.Get("DPoP"))
		}
		carried := request.target + "\n" + request.body
		for name, values := range request.header {
			carried += "\n" + name + ": " + strings.Join(values, ", ")
		}
		if strings.Contains(carried, transitionToken) {
			t.Errorf("%s %s carried the transition token:\n%s", request.method, request.target, carried)
		}
	}
}

// TestTheBootLogNamesTheDispatchCredential: the boot logs one line naming the credential the
// daemon's Dispatch client presents, the file without the setting and the broker and the session
// under it, the transition file beside it where one is named, and no token in either.
func TestTheBootLogNamesTheDispatchCredential(t *testing.T) {
	const fileToken = "dispatch-file-token-4e7a"
	broker := newCredentialBroker(t, mintsTheDaemonsToken)
	dispatchServer := newCredentialDispatch(t, daemonWhoami("EXAMPLE"))
	tokenFile := writeSecret(t, "dispatch-token", fileToken)
	transitionFile := writeSecret(t, "dispatch-transition-token", "shared-transition-token-3a9f")
	for name, tc := range map[string]struct {
		cfg  config.Config
		want map[string]any
	}{
		"dispatch_token_file": {
			cfg:  config.Config{Project: "EXAMPLE", DispatchURL: dispatchServer.URL, DispatchTokenFile: tokenFile},
			want: map[string]any{"credential": "file", "file": tokenFile},
		},
		"dispatch_token_broker": {
			cfg:  config.Config{Project: "EXAMPLE", DispatchURL: dispatchServer.URL, DispatchTokenBroker: true},
			want: map[string]any{"credential": "broker", "session": "legion-daemon:EXAMPLE"},
		},
		"dispatch_token_broker with a transition file": {
			cfg:  config.Config{Project: "EXAMPLE", DispatchURL: dispatchServer.URL, DispatchTokenBroker: true, DispatchTokenTransitionFile: transitionFile},
			want: map[string]any{"credential": "broker", "session": "legion-daemon:EXAMPLE", "transitionFile": transitionFile},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var logged bytes.Buffer
			dispatchToken := ""
			if !tc.cfg.DispatchTokenBroker {
				dispatchToken = fileToken
			}
			if _, err := dispatchCredential(context.Background(), tc.cfg, dispatchToken, getenvOf(podIdentity(t, broker.url)), slog.New(slog.NewJSONHandler(&logged, nil))); err != nil {
				t.Fatalf("dispatchCredential: %v", err)
			}
			var lines []map[string]any
			for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
				var entry map[string]any
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatalf("log line %q: %v", line, err)
				}
				if entry["msg"] == "legion daemon's Dispatch credential" {
					delete(entry, "time")
					delete(entry, "level")
					delete(entry, "msg")
					lines = append(lines, entry)
				}
			}
			if len(lines) != 1 {
				t.Fatalf("the boot logged %d Dispatch credential lines, want one:\n%s", len(lines), logged.String())
			}
			if got, _ := json.Marshal(lines[0]); string(got) != mustJSON(t, tc.want) {
				t.Fatalf("the Dispatch credential line carries %s, want %s", got, mustJSON(t, tc.want))
			}
			for _, token := range []string{fileToken, daemonBrokerToken, daemonSubjectToken, "shared-transition-token-3a9f"} {
				if strings.Contains(logged.String(), token) {
					t.Fatalf("the boot log carries the token %q:\n%s", token, logged.String())
				}
			}
		})
	}
}

// TestTheDaemonRenewsItsDispatchTokenOffTheRequestPath: once boot has settled the daemon's
// Dispatch credential at the real broker, the client's own renewal loop trades for the next token
// when boot's comes due, with no Dispatch call made, so the first call after that presents a new
// token that the broker issued before the call was sent: no call traded for it. The broker's
// tokens live 40 s, so boot's comes due 20 s in, and the transport would still present it until
// 10 s before it lapses (cipherclient's liveMargin); the call goes between the two.
func TestTheDaemonRenewsItsDispatchTokenOffTheRequestPath(t *testing.T) {
	const lifetime = 40 * time.Second
	rig := newBrokerRig(t, ciphertest.WithAccessLifetime(lifetime))
	dispatchServer := newCredentialDispatch(t, daemonWhoami("EXAMPLE"))
	env := map[string]string{
		cipherclient.BrokerURLVar: rig.URL, cipherclient.IdentityVar: cipherclient.IdentityKubernetes,
		cipherclient.MachineTokenFileVar: writeSecret(t, "daemon-exchange-token", rig.MintSubjectToken(t, ciphertest.DaemonExchangeAccount, "daemon-pod-uid")),
	}
	booted := time.Now()
	transport, err := dispatchCredential(t.Context(), brokerConfig(dispatchServer.URL), "", getenvOf(env), quietLogger())
	if err != nil {
		t.Fatalf("dispatchCredential: %v", err)
	}
	client := dispatch.New(dispatchServer.URL, transport)
	// presented makes one Dispatch call, answering the token it carried and when it was sent.
	presented := func() (string, time.Time) {
		t.Helper()
		sent := time.Now()
		if _, err := client.Whoami(t.Context()); err != nil {
			t.Fatalf("a Dispatch call: %v", err)
		}
		requests := dispatchServer.received()
		authorization := requests[len(requests)-1].header.Get("Authorization")
		token, found := strings.CutPrefix(authorization, "DPoP ")
		if !found || token == "" {
			t.Fatalf("the call carried Authorization %q, want the broker's token under DPoP", authorization)
		}
		return token, sent
	}

	boot, _ := presented()
	time.Sleep(time.Until(booted.Add(lifetime/2 + 5*time.Second)))
	renewed, sent := presented()
	if renewed == boot {
		t.Fatalf("the call %s after boot presented boot's token, want the one the renewal loop traded for when boot's came due",
			sent.Sub(booted).Round(time.Second))
	}

	// When the broker issued the renewed token: its expiry less its life, read back at the
	// broker's introspection as the listener reads it.
	request, err := http.NewRequest(http.MethodPost, rig.URL+"/v1/oauth/introspect", strings.NewReader(url.Values{"token": {renewed}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer "+rig.ListenerToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("introspect the renewed token: %v", err)
	}
	defer response.Body.Close()
	var read struct {
		Active bool  `json:"active"`
		Exp    int64 `json:"exp"`
	}
	if err := json.NewDecoder(response.Body).Decode(&read); err != nil || response.StatusCode != http.StatusOK || !read.Active || read.Exp == 0 {
		t.Fatalf("introspect the renewed token: %d %+v (%v), want a live token with its expiry", response.StatusCode, read, err)
	}
	// exp is whole seconds, so a token issued in the second the call was sent could read as up to
	// a second earlier: before that second is what proves the trade came first.
	if issued := time.Unix(read.Exp, 0).Add(-lifetime); !issued.Before(sent.Add(-time.Second)) {
		t.Fatalf("the broker issued the renewed token at %s, want it issued before the call that presented it was sent, at %s: that call traded for it",
			issued.Format(time.RFC3339), sent.Format(time.RFC3339Nano))
	}
}

// mustJSON is value as JSON, with its keys sorted.
func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
