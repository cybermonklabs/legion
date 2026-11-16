package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/agentsecrets"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// podSessionRetention is how long the broker honours a registration past its last write
// (oauthstore.PodSessionRetention), which the stub broker below holds registrations to.
const podSessionRetention = 24 * time.Hour

// expiringBroker is POST /v1/oauth/exchange/pod-sessions as the broker serves it, on the test's
// clock: each write registers the pod its token names for the session until
// podSessionRetention from that write, and an exchange is admitted only while a write of that pod
// and session has not lapsed (oauthstore.PodSessionRegistered, `expires_at > now()`).
type expiringBroker struct {
	t     *testing.T
	clock *waveClock

	mu     sync.Mutex
	writes []podSessionWrite
}

// podSessionWrite is one registration the broker took: the pod, the session, the moment on the
// test's clock, and the bearer it was authenticated with.
type podSessionWrite struct {
	pod, session, bearer string
	at                   time.Time
}

func (b *expiringBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/oauth/exchange/pod-sessions" {
		http.NotFound(w, r)
		return
	}
	var body struct {
		PodToken string `json:"pod_token"`
		Session  string `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		b.t.Errorf("the registration's body does not decode: %v", err)
	}
	pod, named := strings.CutPrefix(body.PodToken, "token-of-")
	if !named {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"POD_TOKEN_INVALID","error":"the pod's ServiceAccount token does not verify"}`))
		return
	}
	at := b.clock.Now()
	b.mu.Lock()
	b.writes = append(b.writes, podSessionWrite{pod: pod, session: body.Session, bearer: r.Header.Get("Authorization"), at: at})
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"pod_uid": pod, "session": body.Session, "expires_at": at.Add(podSessionRetention)})
}

// exchangeAdmitted is whether the broker's exchange, at the test clock's now, admits pod trading
// under session: some write of them has not lapsed.
func (b *expiringBroker) exchangeAdmitted(pod, session string) bool {
	now := b.clock.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, write := range b.writes {
		if write.pod == pod && write.session == session && write.at.Add(podSessionRetention).After(now) {
			return true
		}
	}
	return false
}

func (b *expiringBroker) taken() []podSessionWrite {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]podSessionWrite(nil), b.writes...)
}

// enrollsEveryPod is the broker's enrollment routes taking every enrollment.
type enrollsEveryPod struct{}

func (enrollsEveryPod) Enroll(_ context.Context, e supervise.PodEnrollment) (string, error) {
	return "enr-" + e.PodUID, nil
}

func (enrollsEveryPod) Revoke(context.Context, string) error { return nil }

// TestRegistrationsAreRenewedBeforeTheyLapse runs a role of an issue pod under the daemon's own
// registrar (newRegistrar, with exchange_audience set) for 25 hours on the test's clock, against a
// broker that holds each registration to the day past its last write. The daemon writes the
// registration when the agent registers and again every agentsecrets.RegistrationRenewal, each
// time under its own ServiceAccount token and presenting the token the pod's shim reads then, so
// at 25 hours — past the first write's day — the broker still admits the pod's exchange under its
// session. Once the claim ends, nothing more is written.
func TestRegistrationsAreRenewedBeforeTheyLapse(t *testing.T) {
	const (
		pod     = "pod-uid-renewed"
		session = "ses_renewed"
	)
	clock := newWaveClock()
	broker := &expiringBroker{t: t, clock: clock}
	server := httptest.NewServer(broker)
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("the-daemon's-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := config.AgentSecretsConfig{URL: server.URL, TokenFile: tokenFile, Audience: "agent-secrets", ExchangeAudience: "legion-broker-exchange"}
	registrar := newRegistrar(secrets, &agentsecrets.Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile, Log: quietLogger()})
	if registrar == nil {
		t.Fatal("newRegistrar answered no registrar with exchange_audience set")
	}

	project, _ := claim.ProjectToken("legion")
	token, _ := claim.NewToken(project, "LEGION-840", claim.RoleTester)
	rt, conns := fake.NewRuntime(), fake.NewConns()
	rt.ScriptSpawn(fake.SpawnResult{Locator: runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation(pod, 1),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-legion-840", PodUID: pod, Container: runtime.IssuePodContainer, Generation: 1},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := newSupervisor(ctx, nil, "legion", t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: conns, Store: &outboxClaimStore{}, Specs: outboxSpecs{}, Clock: clock, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Minute, RegistrationIntervals: 2, RPC: 5 * time.Second, Probe: time.Hour, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: api.DaemonAPIVersion,
		Secrets:        enrollsEveryPod{},
		Registrar:      registrar,
		RefreshGate:    supervise.NewRefreshGate(0),
	}
	t.Cleanup(sup.stop)
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: project, Tree: "LEGION-840", Issue: "LEGION-840",
		Role: claim.RoleTester, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create %s: %v", token, err)
	}
	conn := fake.NewConn()
	conn.SetAgentSecretsToken("token-of-" + pod)
	conns.Register(token, conn)
	for _, ev := range []supervise.Event{
		supervise.RequestSpawn{Claim: token},
		supervise.StreamHello{Claim: token, Generation: 1, AgentSecrets: &supervise.AgentSecretsIdentity{Thumbprint: "tp", PodToken: "eyJhbGciOiJSUzI1NiJ9.e30.at-the-hello"}},
		supervise.RequestRegister{Claim: token, Generation: 1, Session: session, SessionFile: "/sessions/" + session + ".jsonl", PluginContract: api.DaemonAPIVersion},
		supervise.RequestReady{Claim: token, Generation: 1, Session: session},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	started := clock.Now()
	for range 25 {
		clock.advance(time.Hour)
	}

	writes := broker.taken()
	if !broker.exchangeAdmitted(pod, session) {
		t.Fatalf("at 25 hours the broker refuses the exchange of session %s for pod %s: its registration lapsed after %d writes", session, pod, len(writes))
	}
	if len(writes) < 4 {
		t.Fatalf("the broker took %d writes of session %s in 25 hours, want at least four", len(writes), session)
	}
	for i, write := range writes {
		if write.pod != pod || write.session != session || write.bearer != "Bearer the-daemon's-token" {
			t.Errorf("write %d registered pod %s for session %s under %q, want pod %s for %s under the daemon's own token", i, write.pod, write.session, write.bearer, pod, session)
		}
		if gap := write.at.Sub(started); i > 0 && gap-writes[i-1].at.Sub(started) > agentsecrets.RegistrationRenewal {
			t.Errorf("write %d came %s after the one before it, want at most %s", i, gap-writes[i-1].at.Sub(started), agentsecrets.RegistrationRenewal)
		}
	}

	if err := machine.Handle(ctx, supervise.RequestStop{Claim: token}); err != nil {
		t.Fatalf("stop %s: %v", token, err)
	}
	for range 30 {
		clock.advance(time.Hour)
	}
	if after := broker.taken(); len(after) != len(writes) {
		t.Errorf("the broker took %d writes of session %s after the claim ended, want none", len(after)-len(writes), session)
	}
}
