package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// registrationBroker is the secrets broker's pod-session registration as the machines reach it
// (supervise.Registrar): it records each registration's first write — the pod, the session and the
// pod token that write presented — with the context its renewal runs under, which ends when the
// daemon stops renewing it.
type registrationBroker struct {
	mu      sync.Mutex
	written []registration
}

type registration struct {
	podUID, session, token string
	renewal                context.Context
}

func (b *registrationBroker) KeepPodSession(ctx context.Context, s supervise.PodSession) error {
	token, err := s.Token(ctx)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.written = append(b.written, registration{podUID: s.PodUID, session: s.Session, token: token, renewal: ctx})
	return err
}

func (b *registrationBroker) registrations() []registration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.written)
}

// noEnrollments is the broker's enrollment routes as a deployment with agent_secrets set reaches
// them: every pod enrolls. The registration needs them on, since it reads the pod token the
// shim's hello carries and the machine keeps that hello's identity only for a broker it enrolls
// with.
type noEnrollments struct{}

func (noEnrollments) Enroll(_ context.Context, e supervise.PodEnrollment) (string, error) {
	return "enr-" + e.PodUID + "-" + e.Session, nil
}

func (noEnrollments) Revoke(context.Context, string) error { return nil }

// newRegistrationHarness is newHarness under a Kubernetes deployment that enrolls its pods, and
// registers each role's session for its pod when registrar is non-nil — the daemon's wiring with
// runtime.kubernetes.agent_secrets.exchange_audience set — and registers nothing when it is nil,
// the wiring with the key unset.
func newRegistrationHarness(t *testing.T, registrar supervise.Registrar) *harness {
	t.Helper()
	h := newHarness(t)
	h.supervisor.deps.Secrets = noEnrollments{}
	h.supervisor.deps.Registrar = registrar
	return h
}

// podToken is a projected token of the pod uid, as a shim's hello carries it: a JWT whose exp
// lies far past the harness's clock, so the first write presents it as it is.
func podToken(uid string) string {
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":4102444800,"pod":%q}`, uid))
	return "eyJhbGciOiJSUzI1NiJ9." + payload + ".signature-of-" + uid
}

// inPod scripts the next launch of the fake runtime — a Spawn, or a Resume when resume is set —
// to start token's role in the issue pod uid, at generation.
func inPod(h *harness, token claim.Token, uid string, generation uint64, resume bool) runtime.Locator {
	loc := runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation(uid, generation),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-legion-840", PodUID: uid, Container: runtime.IssuePodContainer, Generation: generation},
	}
	if resume {
		h.runtime.ScriptResume(fake.SpawnResult{Locator: loc})
	} else {
		h.runtime.ScriptSpawn(fake.SpawnResult{Locator: loc})
	}
	return loc
}

// hello is token's shim connecting to the daemon at its claim's current generation, carrying the
// pod's identity: what the stream listener hands the machine.
func (h *harness) hello(token claim.Token, uid string) {
	h.t.Helper()
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.StreamHello{
		Claim: token, Generation: m.Claim().Generation,
		AgentSecrets: &supervise.AgentSecretsIdentity{Thumbprint: "tp-" + uid, PodToken: podToken(uid)},
	}); err != nil {
		h.t.Fatalf("the hello of %s: %v", token, err)
	}
}

// connected registers a connection for token's claim, as the stream listener does at the hello,
// whose shim reads uid's projected token when the daemon asks for it again.
func (h *harness) connected(token claim.Token, uid string) *fake.Conn {
	conn := fake.NewConn()
	conn.SetAgentSecretsToken(podToken(uid) + "-read-again")
	h.conns.Register(token, conn)
	return conn
}

// roleRun is what one role's run through the routes left: what its registration answered, and
// how many registrations the broker held at each step.
type roleRun struct {
	answer                              claim.RegisterResponse
	atHello, atRegistration, afterReady int
}

// runRole launches role on the issue into pod uid, has its shim say hello, registers its agent
// under session through the route, and has it say it is ready, counting the registrations the
// broker holds after the hello and after the registration's answer.
func runRole(h *harness, b *registrationBroker, role claim.Role, uid, session string) (claim.Token, roleRun) {
	h.t.Helper()
	token, err := claim.NewToken(testProject, "LEGION-840", role)
	if err != nil {
		h.t.Fatal(err)
	}
	inPod(h, token, uid, 1, false)
	_, boot := h.launch("LEGION-840", role)
	h.connected(token, uid)
	h.hello(token, uid)
	var run roleRun
	count := func() int {
		if b == nil {
			return 0
		}
		return len(b.registrations())
	}
	run.atHello = count()
	run.answer = h.registered(boot, session)
	run.atRegistration = count()
	if recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: run.answer.Secret, Generation: run.answer.Generation,
	}, nil); recorder.Code != http.StatusNoContent {
		h.t.Fatalf("ready %s = %d; body %s", token, recorder.Code, recorder.Body)
	}
	run.afterReady = count()
	return token, run
}

// claudeSession is the session the Sandbox runtime chose for the Claude Code role's launch.
const claudeSession = "0b7c1d3e-5f60-4a72-8b94-c5d6e7f80912"

// TestTheDaemonRegistersEachRoleSession: under a deployment that sets exchange_audience, the
// daemon registers each role claim's session for the pod the claim's process runs in, once. An
// Oh My Pi role's session is the one its agent reports at its registration, and it is registered
// before that registration is answered, so the agent holds no claim whose pod cannot trade. A
// Claude Code role's session is the one the runtime chose for its launch, and it is registered at
// its shim's hello, before the agent registers; the agent's registration under that same session
// registers nothing again. Each first write presents the pod token the hello carried.
func TestTheDaemonRegistersEachRoleSession(t *testing.T) {
	broker := &registrationBroker{}
	h := newRegistrationHarness(t, broker)
	tester, omp := runRole(h, broker, claim.RoleTester, "pod-uid-tester", "ses_tester")
	if omp.atHello != 0 || omp.atRegistration != 1 {
		t.Errorf("the Oh My Pi role %s: %d registrations after its hello and %d once its registration was answered, want 0 and 1",
			tester, omp.atHello, omp.atRegistration)
	}

	implementer, _ := claim.NewToken(testProject, "LEGION-840", claim.RoleImplementer)
	h.runtime.ScriptLaunchSession(implementer, claudeSession)
	_, claude := runRole(h, broker, claim.RoleImplementer, "pod-uid-implementer", claudeSession)
	if claude.atHello != 2 {
		t.Errorf("the Claude Code role %s: %d registrations after its hello, want its session registered there (2 in all), before its agent registers",
			implementer, claude.atHello)
	}

	want := []registration{
		{podUID: "pod-uid-tester", session: "ses_tester", token: podToken("pod-uid-tester")},
		{podUID: "pod-uid-implementer", session: claudeSession, token: podToken("pod-uid-implementer")},
	}
	got := broker.registrations()
	if len(got) != len(want) {
		t.Fatalf("the broker holds %d registrations, want one per role claim: %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].podUID != w.podUID || got[i].session != w.session || got[i].token != w.token {
			t.Errorf("registration %d is pod %s for session %s presenting %q, want pod %s for session %s presenting the hello's token %q",
				i, got[i].podUID, got[i].session, got[i].token, w.podUID, w.session, w.token)
		}
		if got[i].renewal.Err() != nil {
			t.Errorf("the registration of session %s is no longer renewed while its process runs", w.session)
		}
	}
}

// TestARelaunchRegistersTheResumedSession: a claim relaunched into a new pod resumes its recorded
// session, and the new pod's uid is registered for that session at its shim's first hello, before
// the resumed agent registers again, presenting the new pod's token. The old pod's registration
// is renewed no longer: its process was let go.
func TestARelaunchRegistersTheResumedSession(t *testing.T) {
	broker := &registrationBroker{}
	h := newRegistrationHarness(t, broker)
	token, _ := runRole(h, broker, claim.RoleReviewer, "pod-uid-first", "ses_reviewer")

	inPod(h, token, "pod-uid-second", 2, true)
	boot := h.relaunch(token)
	if resumes := h.runtime.CallsOf("Resume"); len(resumes) != 1 || resumes[0].Spec.ResumeSessionFile != "/sessions/ses_reviewer.jsonl" {
		t.Fatalf("resumes = %+v, want the recorded session resumed once", resumes)
	}
	h.connected(token, "pod-uid-second")
	h.hello(token, "pod-uid-second")
	atHello := len(broker.registrations())
	h.registered(boot, "ses_reviewer")

	got := broker.registrations()
	if atHello != 2 || len(got) != 2 {
		t.Fatalf("the broker holds %d registrations after the relaunched pod's hello and %d after the resumed agent registered, want 2 and 2: %+v", atHello, len(got), got)
	}
	if got[1].podUID != "pod-uid-second" || got[1].session != "ses_reviewer" || got[1].token != podToken("pod-uid-second") {
		t.Errorf("the relaunch registered pod %s for session %s presenting %q, want pod-uid-second for ses_reviewer presenting its own hello's token", got[1].podUID, got[1].session, got[1].token)
	}
	if got[0].renewal.Err() == nil {
		t.Errorf("the first pod's registration of ses_reviewer is still renewed after its process was let go")
	}
	if got[1].renewal.Err() != nil {
		t.Errorf("the relaunched pod's registration of ses_reviewer is not renewed while its process runs")
	}
}

// TestNoRegistrationWithoutTheKey: with exchange_audience unset the daemon wires no registrar, and
// the same runs — an Oh My Pi role, a Claude Code role whose session the runtime chose, and a
// relaunch — answer every registration and leave every stored claim, its enrollment included, and
// every runtime call exactly as they are with the key set. The keyed run is the control: it
// registers.
func TestNoRegistrationWithoutTheKey(t *testing.T) {
	type outcome struct {
		answers []claim.RegisterResponse
		claims  []supervise.Claim
		calls   []string
	}
	run := func(registrar supervise.Registrar, broker *registrationBroker) outcome {
		h := newRegistrationHarness(t, registrar)
		var o outcome
		tester, omp := runRole(h, broker, claim.RoleTester, "pod-uid-tester", "ses_tester")
		implementer, _ := claim.NewToken(testProject, "LEGION-840", claim.RoleImplementer)
		h.runtime.ScriptLaunchSession(implementer, claudeSession)
		_, claude := runRole(h, broker, claim.RoleImplementer, "pod-uid-implementer", claudeSession)
		inPod(h, tester, "pod-uid-second", 2, true)
		boot := h.relaunch(tester)
		h.connected(tester, "pod-uid-second")
		h.hello(tester, "pod-uid-second")
		resumed := h.registered(boot, "ses_tester")
		for _, answer := range []claim.RegisterResponse{omp.answer, claude.answer, resumed} {
			answer.Secret = ""
			o.answers = append(o.answers, answer)
		}
		for _, token := range []claim.Token{tester, implementer} {
			c := h.stored(token)
			// Both are random per launch and per registration, in either run.
			c.CapabilityHash, c.BootTokenHash = nil, nil
			o.claims = append(o.claims, c)
		}
		o.calls = h.runtime.Methods()
		return o
	}
	broker := &registrationBroker{}
	keyed := run(broker, broker)
	if len(broker.registrations()) == 0 {
		t.Fatal("the keyed run registered nothing, so the comparison proves nothing")
	}
	unkeyed := run(nil, nil)
	if !reflect.DeepEqual(unkeyed.answers, keyed.answers) {
		t.Errorf("without the key the registrations answered %s, want %s", asJSON(t, unkeyed.answers), asJSON(t, keyed.answers))
	}
	for i := range keyed.claims {
		if differ := differingFields(t, unkeyed.claims[i], keyed.claims[i]); len(differ) > 0 {
			t.Errorf("without the key the store holds %s differently in %v: %s, want %s", keyed.claims[i].Token, differ, asJSON(t, unkeyed.claims[i]), asJSON(t, keyed.claims[i]))
		}
	}
	if !slices.Equal(unkeyed.calls, keyed.calls) {
		t.Errorf("without the key the runtime was called %v, want %v", unkeyed.calls, keyed.calls)
	}
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// differingFields is the top-level JSON fields a and b hold differently.
func differingFields(t *testing.T, a, b any) []string {
	t.Helper()
	var left, right map[string]any
	if json.Unmarshal([]byte(asJSON(t, a)), &left) != nil || json.Unmarshal([]byte(asJSON(t, b)), &right) != nil {
		t.Fatal("not a JSON object")
	}
	var differ []string
	for key := range left {
		if !reflect.DeepEqual(left[key], right[key]) {
			differ = append(differ, key)
		}
	}
	slices.Sort(differ)
	return differ
}
