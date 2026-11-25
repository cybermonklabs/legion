// release_broker_test.go drives an operator's release of a claim's names (`legion claims
// release-names`, supervise.Machine.ReleaseNames) under a daemon whose agent-secrets client reaches
// the real secrets broker (packages/envoy's ciphertest rig, which holds POST
// /v1/oauth/exchange/pod-sessions/release) through a proxy, with the daemon's store on the test
// database and the operator's route wired as the daemon wires it. Each test skips without
// BROKER_TEST_DATABASE_URL (the rig's own skip), except on GitHub Actions, where the daemon's test
// job sets it and a skip would hide that it stopped (newBrokerRig).
package daemon

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/cipherclient"
	"github.com/sjawhar/envoy/ciphertest"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/cipher"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The issue pod, the claim's issue, and the session the claim's agent registers and the operator
// then has it forget, whose names the release frees: its id, the one name of kind session, and
// its file's base name.
const (
	releasePod     = "pod-uid-released"
	releaseIssue   = "LEGION-450"
	releaseSession = "ses-released"
	releaseFile    = "/sessions/released.jsonl"
	// releaseRoute is the broker's release, as the daemon's client calls it.
	releaseRoute = "/v1/oauth/exchange/pod-sessions/release"
	// releaseWarning is the Warn line of each release attempt the broker did not answer with a 200.
	releaseWarning = "supervise: the secrets broker did not release a claim's session names; they stay held"
)

// newBrokerRig is ciphertest.NewRig with options, failing rather than skipping on GitHub Actions
// when BROKER_TEST_DATABASE_URL is unset: the daemon-go-race job of
// .github/workflows/ci-job-daemon-go.yaml, which runs internal/daemon, names its Postgres service
// there, and a silent skip would let these tests stop running unnoticed.
func newBrokerRig(t *testing.T, options ...ciphertest.Option) *ciphertest.Rig {
	t.Helper()
	if os.Getenv("BROKER_TEST_DATABASE_URL") == "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatal("BROKER_TEST_DATABASE_URL is unset on GitHub Actions: the daemon-go-race job of .github/workflows/ci-job-daemon-go.yaml names its Postgres service")
	}
	return ciphertest.NewRig(t, options...)
}

// releaseProxy stands between the daemon's agent-secrets client and the rig's broker: it passes
// every call on, but answers a release itself with 503 while down is set, as a front door with no
// broker behind it does. It records every call's method and path, and for each release the names
// it asked and the names the daemon's store held for the claim as it arrived.
type releaseProxy struct {
	t      *testing.T
	broker *httputil.ReverseProxy
	held   func() []supervise.Hold

	mu       sync.Mutex
	down     bool
	calls    []string
	releases []seenRelease
}

// seenRelease is one release the proxy received.
type seenRelease struct {
	sessions []string
	held     []supervise.Hold
}

func (p *releaseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.calls = append(p.calls, r.Method+" "+r.URL.Path)
	down := p.down
	p.mu.Unlock()
	if r.Method == http.MethodPost && r.URL.Path == releaseRoute {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			p.t.Errorf("read the release's body: %v", err)
		}
		var body struct {
			Sessions []string `json:"sessions"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			p.t.Errorf("the release's body %q is not JSON: %v", raw, err)
		}
		seen := seenRelease{sessions: body.Sessions, held: p.held()}
		p.mu.Lock()
		p.releases = append(p.releases, seen)
		p.mu.Unlock()
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("no healthy upstream"))
			return
		}
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
	}
	p.broker.ServeHTTP(w, r)
}

func (p *releaseProxy) setDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
}

func (p *releaseProxy) seen() ([]string, []seenRelease) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls), slices.Clone(p.releases)
}

// sleepClock is stillClock recording every wait a machine asked to Sleep.
type sleepClock struct {
	stillClock
	mu    sync.Mutex
	slept []time.Duration
}

func (c *sleepClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.slept = append(c.slept, d)
	c.mu.Unlock()
	return ctx.Err()
}

func (c *sleepClock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.slept)
}

// releaseRig is the tester of an issue pod, under a daemon whose agent-secrets client reaches the
// rig's broker through proxy, after the operator suspended it and had it forget releaseSession.
type releaseRig struct {
	t           *testing.T
	ctx         context.Context
	broker      *ciphertest.Rig
	proxy       *releaseProxy
	st          *store.Store
	token       claim.Token
	key         *ecdsa.PrivateKey
	operatorAPI http.Handler
	logs        *syncBuffer
	clock       *sleepClock
}

// newReleaseRig runs the tester of an issue pod in that pod under a daemon whose agent-secrets
// block is set, with exchange_audience when exchange is true (newRegistrar), through its shim's
// hello naming the role's own key and the pod's token, its agent's registration of releaseSession
// and its ready. With exchange_audience the daemon registers the session for the pod at the broker,
// and the pod's trade under it, proved with the role's key, is admitted. The operator then suspends
// the claim, which stops the registration's renewal but leaves it live at the broker, and has the
// claim forget releaseSession, so every name of that session is a name the operator's release
// frees. The broker's carry check answers that nothing is carried (enrollsEveryPod).
func newReleaseRig(t *testing.T, exchange bool) *releaseRig {
	t.Helper()
	broker := newBrokerRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	pool := isolatedOutboxPool(t)
	putOutboxIssue(t, pool, record.NewStore(), record.Issue{Key: releaseIssue, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: "LEGION", Tree: releaseIssue, Title: "Release", Phase: phase.Testing, Generation: 1,
		Status: "testing", Rank: "U"})
	st := outboxTreeStore(t, pool, releaseIssue)
	token := mustClaimToken(t, releaseIssue, claim.RoleTester)

	target, err := url.Parse(broker.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &releaseProxy{t: t, broker: httputil.NewSingleHostReverseProxy(target), held: func() []supervise.Hold {
		holds, err := st.ClaimHolds(context.Background(), "legion", token)
		if err != nil {
			t.Errorf("read the claim's names as the release arrives: %v", err)
		}
		return holds
	}}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(broker.MintAccountToken(t, ciphertest.DaemonAccount, time.Hour)), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := config.AgentSecretsConfig{URL: server.URL, TokenFile: tokenFile, Audience: "agent-secrets"}
	if exchange {
		secrets.ExchangeAudience = "legion-broker-exchange"
	}
	registrar := newRegistrar(secrets, &cipher.Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile, Log: quietLogger()})
	if (registrar != nil) != exchange {
		t.Fatalf("newRegistrar with exchange_audience %q answered %v", secrets.ExchangeAudience, registrar)
	}

	logs, clock := &syncBuffer{}, &sleepClock{}
	rt, conns := fake.NewRuntime(), fake.NewConns()
	sup := newSupervisor(ctx, st, "legion", t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: conns, Store: st, Specs: outboxSpecs{}, Clock: clock, Log: slog.New(slog.NewTextHandler(logs, nil)),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Minute, RegistrationIntervals: 2, RPC: 5 * time.Second, Probe: time.Hour, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: api.DaemonAPIVersion(),
		Secrets:        enrollsEveryPod{},
		Registrar:      registrar,
		RefreshGate:    supervise.NewRefreshGate(0),
	}
	t.Cleanup(sup.stop)
	rt.ScriptSpawn(fake.SpawnResult{Locator: runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation(releasePod, 1),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-450", PodUID: releasePod, Container: runtime.IssuePodContainer, Generation: 1},
	}})
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: releaseIssue, Issue: releaseIssue,
		Role: claim.RoleTester, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create %s: %v", token, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	podToken := broker.MintAccountPodToken(t, ciphertest.ExchangeRolesAccount, releasePod, time.Hour)
	conn := fake.NewConn()
	conn.SetAgentSecretsToken(podToken)
	conns.Register(token, conn)
	handle := func(events ...supervise.Event) {
		t.Helper()
		for _, ev := range events {
			if err := machine.Handle(ctx, ev); err != nil {
				t.Fatalf("handle %T: %v", ev, err)
			}
		}
		machine.Wait()
	}
	handle(
		supervise.RequestSpawn{Claim: token},
		supervise.StreamHello{Claim: token, Generation: 1, AgentSecrets: &supervise.AgentSecretsIdentity{Thumbprint: keyThumbprint(t, key), PodToken: podToken}},
		supervise.RequestRegister{Claim: token, Generation: 1, Session: releaseSession, SessionFile: releaseFile, PluginContract: api.DaemonAPIVersion()},
		supervise.RequestReady{Claim: token, Generation: 1, Session: releaseSession},
	)
	r := &releaseRig{t: t, ctx: ctx, broker: broker, proxy: proxy, st: st, token: token, key: key, logs: logs, clock: clock,
		operatorAPI: api.NewServer("127.0.0.1", 0, api.Options{Supervisor: sup, OperatorToken: freshSessionOperatorToken, Log: quietLogger()}).Handler}
	if exchange {
		if err := r.trade(); err != nil {
			t.Fatalf("the pod's trade under %s while the daemon's registration of it is live = %v, want it admitted", releaseSession, err)
		}
	}
	handle(
		supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByOperator},
		supervise.RequestForgetSession{Claim: token, Session: releaseSession},
	)
	if !slices.Contains(r.holds(), sessionHold) {
		t.Fatalf("the claim holds %v after it forgot %s, want that session's id among them", r.holds(), releaseSession)
	}
	return r
}

// sessionHold is the name of kind session the release frees.
var sessionHold = supervise.Hold{Kind: supervise.HoldSessionID, Name: releaseSession}

// keyThumbprint is key's RFC 7638 thumbprint, as a role's shim hello names it.
func keyThumbprint(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	sum, err := (&jose.JSONWebKey{Key: &key.PublicKey}).Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(sum)
}

// trade is the pod's trade at the broker under releaseSession, with the pod's exchange token in
// the file a pod's broker client reads it from (cipherclient.MachineIdentity), proved with the
// role's own key.
func (r *releaseRig) trade() error {
	r.t.Helper()
	subject, err := cipherclient.MachineIdentity(getenvOf(map[string]string{
		cipherclient.IdentityVar:         cipherclient.IdentityKubernetes,
		cipherclient.MachineTokenFileVar: writeSecret(r.t, "exchange-token", r.broker.MintSubjectToken(r.t, ciphertest.ExchangeRolesAccount, releasePod)),
	}))
	if err != nil {
		r.t.Fatal(err)
	}
	client, err := cipherclient.New(cipherclient.Config{BrokerURL: cipheraddr.New(r.broker.URL), Key: r.key, Session: releaseSession, Subject: subject})
	if err != nil {
		r.t.Fatal(err)
	}
	_, err = client.Token(r.ctx)
	return err
}

// release is the operator's `legion claims release-names` of the claim, over the route it posts to.
func (r *releaseRig) release() *httptest.ResponseRecorder {
	r.t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/legion/v1/operator/claims/"+string(r.token)+"/release-names", nil)
	request.Header.Set("Authorization", "Bearer "+freshSessionOperatorToken)
	recorder := httptest.NewRecorder()
	r.operatorAPI.ServeHTTP(recorder, request)
	return recorder
}

// freed is the release's 200 answer, read; anything else fails the test.
func (r *releaseRig) freed(recorder *httptest.ResponseRecorder) api.ReleasedNames {
	r.t.Helper()
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("the release = %d %s, want 200", recorder.Code, recorder.Body)
	}
	var released api.ReleasedNames
	if err := json.Unmarshal(recorder.Body.Bytes(), &released); err != nil {
		r.t.Fatalf("decode the release's answer %s: %v", recorder.Body, err)
	}
	return released
}

// holds is every name the daemon's store holds for the claim.
func (r *releaseRig) holds() []supervise.Hold {
	r.t.Helper()
	holds, err := r.st.ClaimHolds(r.ctx, "legion", r.token)
	if err != nil {
		r.t.Fatalf("read the claim's names: %v", err)
	}
	return holds
}

// TestAReleaseTellsTheBrokerBeforeItFreesTheNames: a claim's earlier session, which a live
// registration of the daemon's still holds at the broker, so the pod's trade under it is
// admitted, is released by the operator. The daemon calls the broker's release route with that
// session's id, the one name of kind session the release frees, while its store still holds the
// name, and frees the name once the broker answered 200: the route answers it freed, the store no
// longer holds it, and the pod's trade under it afterwards is refused invalid_grant, naming it.
//
// The control (K7's, the pull request records the run): ReleaseNames without its call to the
// broker frees the name and the pod's trade under it afterwards is admitted, which fails the test
// naming the session.
func TestAReleaseTellsTheBrokerBeforeItFreesTheNames(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t, true)

	released := r.freed(r.release())

	if !slices.Contains(released.Freed, api.ReleasedName{Kind: string(supervise.HoldSessionID), Name: releaseSession}) || released.Checked != supervise.CheckedByBroker {
		t.Fatalf("the release answered %+v, want %s freed, checked %q", released, releaseSession, supervise.CheckedByBroker)
	}
	if slices.Contains(r.holds(), sessionHold) {
		t.Fatalf("after the release the store still holds %s for the claim", releaseSession)
	}
	err := r.trade()
	var refused *cipherclient.RefusedError
	if !errors.As(err, &refused) || refused.Code != "invalid_grant" || !strings.Contains(refused.Description, releaseSession) {
		t.Fatalf("after the release, the pod's trade under %s = %v, want it refused invalid_grant naming %s: the broker still honours the daemon's registration of the session it freed", releaseSession, err, releaseSession)
	}
	_, releases := r.proxy.seen()
	if len(releases) != 1 || !slices.Equal(releases[0].sessions, []string{releaseSession}) {
		t.Fatalf("the broker saw the releases %+v, want one naming %s alone, the one name of kind session the release frees", releases, releaseSession)
	}
	if !slices.Contains(releases[0].held, sessionHold) {
		t.Fatalf("as the broker saw the release of %s, the store held %v for the claim: it freed the name before the broker released it", releaseSession, releases[0].held)
	}
}

// TestAReleaseTheBrokerDoesNotAnswerKeepsTheNames: the broker does not answer the release call (a
// proxy in front of it answers 503 to that route alone, as a front door with no broker behind it
// does). The daemon asks three times, a second and then two apart, logs each failure at Warn naming
// its reason, and frees nothing: the store holds every name the claim held, and the route answers
// 503, naming the reason and saying to run it again. Once the broker answers again, the operator's
// next run frees them.
//
// The control (the pull request records the run): ReleaseNames going on to free the names after
// the broker did not release them frees them while the broker is down, which fails the test.
func TestAReleaseTheBrokerDoesNotAnswerKeepsTheNames(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t, true)
	held := r.holds()
	r.proxy.setDown(true)

	recorder := r.release()

	if got := r.holds(); !slices.Equal(got, held) {
		t.Fatalf("while the broker does not answer the release, the store holds %v for the claim, want every name it held, %v", got, held)
	}
	if body := recorder.Body.String(); recorder.Code != http.StatusServiceUnavailable || !strings.Contains(body, "no healthy upstream") || !strings.Contains(body, "run it again") {
		t.Fatalf("the release the broker does not answer = %d %s, want 503 naming the broker's reason and saying to run it again", recorder.Code, body)
	}
	_, releases := r.proxy.seen()
	if len(releases) != 3 {
		t.Fatalf("the broker was asked the release %d times, want 3", len(releases))
	}
	for i, release := range releases {
		if !slices.Equal(release.sessions, []string{releaseSession}) {
			t.Errorf("attempt %d asked the broker to release %v, want %s alone", i+1, release.sessions, releaseSession)
		}
	}
	if waits, want := r.clock.waits(), []time.Duration{time.Second, 2 * time.Second}; !slices.Equal(waits, want) {
		t.Errorf("the release waited %v between its attempts, want %v", waits, want)
	}
	var warned []string
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, releaseWarning) {
			warned = append(warned, line)
		}
	}
	if len(warned) != 3 {
		t.Fatalf("the release logged %d Warn lines %q, want one for each of its 3 attempts:\n%s", len(warned), releaseWarning, r.logs.String())
	}
	for i, line := range warned {
		for _, want := range []string{fmt.Sprintf("attempt=%d", i+1), "names=1", "503", "no healthy upstream"} {
			if !strings.Contains(line, want) {
				t.Errorf("the Warn line of attempt %d is %q, want it to name %s", i+1, line, want)
			}
		}
	}

	r.proxy.setDown(false)
	released := r.freed(r.release())
	if !slices.Contains(released.Freed, api.ReleasedName{Kind: string(supervise.HoldSessionID), Name: releaseSession}) || slices.Contains(r.holds(), sessionHold) {
		t.Fatalf("the release run again once the broker answers = %+v, want %s freed", released, releaseSession)
	}
}

// TestAReleaseWithoutTheExchangeAsksNoBroker: a daemon whose agent-secrets block sets no
// exchange_audience has no registrar (newRegistrar) and registers no pod session, so its release
// frees the claim's names once the broker's carry check answers, as before the release route, and
// makes no call to that route.
func TestAReleaseWithoutTheExchangeAsksNoBroker(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t, false)

	released := r.freed(r.release())

	if !slices.Contains(released.Freed, api.ReleasedName{Kind: string(supervise.HoldSessionID), Name: releaseSession}) || released.Checked != supervise.CheckedByBroker {
		t.Fatalf("the release answered %+v, want %s freed, checked %q", released, releaseSession, supervise.CheckedByBroker)
	}
	if slices.Contains(r.holds(), sessionHold) {
		t.Fatalf("after the release the store still holds %s for the claim", releaseSession)
	}
	if calls, _ := r.proxy.seen(); slices.Contains(calls, http.MethodPost+" "+releaseRoute) {
		t.Fatalf("the daemon with no exchange_audience called the broker %v, want no call to %s", calls, releaseRoute)
	}
}
