package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/cipher"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// registrationBroker is the secrets broker's pod-session registration as the machines reach it
// (supervise.Registrar): it records each registration's first write — the pod, the session, the
// thumbprint and the pod token that write presented — and whether the daemon has stopped
// renewing it.
type registrationBroker struct {
	mu      sync.Mutex
	written []registration
}

type registration struct {
	podUID, session, thumbprint, token string
	stopped                            *atomic.Bool
}

func (b *registrationBroker) KeepPodSession(ctx context.Context, s supervise.PodSession) (func(), error) {
	token, err := s.Token(ctx)
	stopped := &atomic.Bool{}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.written = append(b.written, registration{podUID: s.PodUID, session: s.Session, thumbprint: s.Thumbprint, token: token, stopped: stopped})
	return func() { stopped.Store(true) }, err
}

func (*registrationBroker) ReleasePodSessions(context.Context, []string) error { return nil }

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

func (noEnrollments) Carried(context.Context, []string) ([]supervise.CarriedEnrollment, error) {
	return []supervise.CarriedEnrollment{}, nil
}

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

// podOfToken is the pod uid podToken wrote into a token, as the broker reads it from the token a
// write presents.
func podOfToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct{ Pod string }
	_ = json.Unmarshal(raw, &claims)
	return claims.Pod
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
// pod's token and the thumbprint of the role's own key: what the stream listener hands the
// machine.
func (h *harness) hello(token claim.Token, uid, thumbprint string) {
	h.t.Helper()
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.StreamHello{
		Claim: token, Generation: m.Claim().Generation,
		AgentSecrets: &supervise.AgentSecretsIdentity{Thumbprint: thumbprint, PodToken: podToken(uid)},
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

// runRole launches role on the issue into pod uid, has its shim say hello naming thumbprint,
// registers its agent under session through the route, and has it say it is ready, counting the
// registrations the broker holds after the hello and after the registration's answer.
func runRole(h *harness, b *registrationBroker, role claim.Role, uid, session, thumbprint string) (claim.Token, roleRun) {
	h.t.Helper()
	token, err := claim.NewToken(testProject, "LEGION-840", role)
	if err != nil {
		h.t.Fatal(err)
	}
	inPod(h, token, uid, 1, false)
	_, boot := h.launch("LEGION-840", role)
	h.connected(token, uid)
	h.hello(token, uid, thumbprint)
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
// daemon registers each role claim's session for the pod the claim's process runs in, once, bound
// to the thumbprint of that role's own key, which the role's shim hello names. An Oh My Pi role
// whose launch named no id (one into a kept pod that trades nothing) is registered under the
// session its agent reports at its registration, after its hello, before that registration is
// answered, so the agent holds no claim whose pod cannot trade. A Claude Code role's session is
// the one the runtime chose for its launch, and it is registered at its shim's hello, before the
// agent registers; the agent's registration under that same session registers nothing again.
// Nothing is registered for a role whose hello has not arrived, nor for one whose hello names no
// thumbprint: a registration is never made before the role's key is known. Each first write
// presents the pod token the hello carried.
func TestTheDaemonRegistersEachRoleSession(t *testing.T) {
	broker := &registrationBroker{}
	h := newRegistrationHarness(t, broker)
	tester, omp := runRole(h, broker, claim.RoleTester, "pod-uid-tester", "ses_tester", "tp-tester")
	if omp.atHello != 0 || omp.atRegistration != 1 {
		t.Errorf("the Oh My Pi role %s: %d registrations after its hello and %d once its registration was answered, want 0 and 1",
			tester, omp.atHello, omp.atRegistration)
	}

	implementer, _ := claim.NewToken(testProject, "LEGION-840", claim.RoleImplementer)
	h.runtime.ScriptLaunchSession(implementer, claudeSession)
	_, claude := runRole(h, broker, claim.RoleImplementer, "pod-uid-implementer", claudeSession, "tp-implementer")
	if claude.atHello != 2 {
		t.Errorf("the Claude Code role %s: %d registrations after its hello, want its session registered there (2 in all), before its agent registers",
			implementer, claude.atHello)
	}

	reviewer, _ := claim.NewToken(testProject, "LEGION-840", claim.RoleReviewer)
	const reviewerSession = "5d6e7f80-9a1b-4c2d-8e3f-a4b5c6d7e8f9"
	h.runtime.ScriptLaunchSession(reviewer, reviewerSession)
	inPod(h, reviewer, "pod-uid-reviewer", 1, false)
	h.launch("LEGION-840", claim.RoleReviewer)
	h.connected(reviewer, "pod-uid-reviewer")
	if n := len(broker.registrations()); n != 2 {
		t.Errorf("the Claude Code role %s, whose hello has not arrived: %d registrations in all, want still 2, none of its session", reviewer, n)
	}
	h.hello(reviewer, "pod-uid-reviewer", "")
	if n := len(broker.registrations()); n != 2 {
		t.Errorf("the Claude Code role %s, whose hello named no thumbprint: %d registrations in all, want still 2, none of its session", reviewer, n)
	}
	h.hello(reviewer, "pod-uid-reviewer", "tp-reviewer")

	want := []registration{
		{podUID: "pod-uid-tester", session: "ses_tester", thumbprint: "tp-tester", token: podToken("pod-uid-tester")},
		{podUID: "pod-uid-implementer", session: claudeSession, thumbprint: "tp-implementer", token: podToken("pod-uid-implementer")},
		{podUID: "pod-uid-reviewer", session: reviewerSession, thumbprint: "tp-reviewer", token: podToken("pod-uid-reviewer")},
	}
	got := broker.registrations()
	if len(got) != len(want) {
		t.Fatalf("the broker holds %d registrations, want one per role claim: %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].podUID != w.podUID || got[i].session != w.session || got[i].thumbprint != w.thumbprint || got[i].token != w.token {
			t.Errorf("registration %d is pod %s for session %s bound to %q presenting %q, want pod %s for session %s bound to the role's hello thumbprint %q presenting the hello's token %q",
				i, got[i].podUID, got[i].session, got[i].thumbprint, got[i].token, w.podUID, w.session, w.thumbprint, w.token)
		}
		if got[i].stopped.Load() {
			t.Errorf("the registration of session %s is no longer renewed while its process runs", w.session)
		}
	}
}

// TestTheDaemonRegistersTheIdItChose: an Oh My Pi role whose pod trades starts under a session id
// the runtime chose for its launch (runtime.LaunchSessionNamer, the Sandbox runtime's ompInputs),
// and the daemon registers that id for the role's pod at its shim's hello, bound to the thumbprint
// that hello names and presenting the pod token it carried: nothing is registered before the
// hello, and the agent's own registration under the same id registers nothing again. A relaunch
// resumes that id, the claim's recorded session (SpawnSpec.ResumeSessionID, which the runtime
// passes as `--resume <id>`), and registers it for the new pod at that pod's hello.
func TestTheDaemonRegistersTheIdItChose(t *testing.T) {
	const chosen = "019a2b3c-4d5e-7f60-8a71-92b3c4d5e6f7"
	broker := &registrationBroker{}
	h := newRegistrationHarness(t, broker)
	token, err := claim.NewToken(testProject, "LEGION-840", claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	h.runtime.ScriptLaunchSession(token, chosen)
	inPod(h, token, "pod-uid-first", 1, false)
	_, boot := h.launch("LEGION-840", claim.RoleTester)
	h.connected(token, "pod-uid-first")
	if got := broker.registrations(); len(got) != 0 {
		t.Fatalf("before the role's hello the broker holds %+v, want no registration", got)
	}
	h.hello(token, "pod-uid-first", "tp-first")
	atHello := broker.registrations()
	if len(atHello) != 1 {
		t.Fatalf("after the role's hello the broker holds %d registrations, want the chosen id's one: %+v", len(atHello), atHello)
	}
	if got := atHello[0]; got.podUID != "pod-uid-first" || got.session != chosen || got.thumbprint != "tp-first" || got.token != podToken("pod-uid-first") {
		t.Errorf("the hello registered pod %s for session %s bound to %q presenting %q, want pod-uid-first for the chosen %s bound to the hello's tp-first presenting its token",
			got.podUID, got.session, got.thumbprint, got.token, chosen)
	}
	h.registered(boot, chosen)
	if n := len(broker.registrations()); n != 1 {
		t.Errorf("after the agent registered under the chosen id the broker holds %d registrations, want still 1", n)
	}

	inPod(h, token, "pod-uid-second", 2, true)
	h.relaunch(token)
	if resumes := h.runtime.CallsOf("Resume"); len(resumes) != 1 || resumes[0].Spec.ResumeSessionID != chosen {
		t.Fatalf("resumes = %+v, want one resuming the chosen id %s", resumes, chosen)
	}
	h.connected(token, "pod-uid-second")
	h.hello(token, "pod-uid-second", "tp-second")
	got := broker.registrations()
	if len(got) != 2 || got[1].podUID != "pod-uid-second" || got[1].session != chosen || got[1].thumbprint != "tp-second" {
		t.Fatalf("after the relaunched pod's hello the broker holds %+v, want pod-uid-second registered for %s bound to tp-second", got, chosen)
	}
	if !got[0].stopped.Load() || got[1].stopped.Load() {
		t.Errorf("the first pod's registration is renewed %t and the second's %t, want only the second's renewed", !got[0].stopped.Load(), !got[1].stopped.Load())
	}
}

// TestARelaunchRegistersTheResumedSession: a claim relaunched into a new pod resumes its recorded
// session, and the new pod's uid is registered for that session at its shim's first hello, before
// the resumed agent registers again, presenting the new pod's token and bound to the thumbprint
// that hello names. The old pod's registration is renewed no longer: its process was let go.
func TestARelaunchRegistersTheResumedSession(t *testing.T) {
	broker := &registrationBroker{}
	h := newRegistrationHarness(t, broker)
	token, _ := runRole(h, broker, claim.RoleReviewer, "pod-uid-first", "ses_reviewer", "tp-first")

	inPod(h, token, "pod-uid-second", 2, true)
	boot := h.relaunch(token)
	if resumes := h.runtime.CallsOf("Resume"); len(resumes) != 1 || resumes[0].Spec.ResumeSessionFile != "/sessions/ses_reviewer.jsonl" {
		t.Fatalf("resumes = %+v, want the recorded session resumed once", resumes)
	}
	h.connected(token, "pod-uid-second")
	h.hello(token, "pod-uid-second", "tp-second")
	atHello := len(broker.registrations())
	h.registered(boot, "ses_reviewer")

	got := broker.registrations()
	if atHello != 2 || len(got) != 2 {
		t.Fatalf("the broker holds %d registrations after the relaunched pod's hello and %d after the resumed agent registered, want 2 and 2: %+v", atHello, len(got), got)
	}
	if got[1].podUID != "pod-uid-second" || got[1].session != "ses_reviewer" || got[1].thumbprint != "tp-second" || got[1].token != podToken("pod-uid-second") {
		t.Errorf("the relaunch registered pod %s for session %s bound to %q presenting %q, want pod-uid-second for ses_reviewer bound to its own hello's thumbprint tp-second presenting its own hello's token", got[1].podUID, got[1].session, got[1].thumbprint, got[1].token)
	}
	if !got[0].stopped.Load() {
		t.Errorf("the first pod's registration of ses_reviewer is still renewed after its process was let go")
	}
	if got[1].stopped.Load() {
		t.Errorf("the relaunched pod's registration of ses_reviewer is not renewed while its process runs")
	}
}

// exchangeBroker is POST /v1/oauth/exchange/pod-sessions as the broker serves it, for the
// agent-secrets client's own renewer: each write registers the pod its token names for the
// session, bound to the thumbprint the write sends, and is answered as the registration now
// stands.
type exchangeBroker struct {
	t      *testing.T
	mu     sync.Mutex
	writes []registration
}

func (b *exchangeBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PodToken   string `json:"pod_token"`
		Session    string `json:"session"`
		Thumbprint string `json:"thumbprint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		b.t.Errorf("the registration's body does not decode: %v", err)
	}
	pod := podOfToken(body.PodToken)
	b.mu.Lock()
	b.writes = append(b.writes, registration{podUID: pod, session: body.Session, thumbprint: body.Thumbprint, token: body.PodToken})
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pod_uid": pod, "session": body.Session, "thumbprint": body.Thumbprint, "expires_at": time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC),
	})
}

func (b *exchangeBroker) received() []registration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.writes)
}

// renewals is the clock the registrations renew by, fired by hand: the harness's clock never
// moves, so only the renewals the test fires run.
type renewals struct {
	mu      sync.Mutex
	pending []*renewal
}

type renewal struct {
	f    func()
	done bool
}

func (r *renewals) after(_ time.Duration, f func()) func() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := &renewal{f: f}
	r.pending = append(r.pending, next)
	return func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		stopped := !next.done
		next.done = true
		return stopped
	}
}

// fireAll runs every renewal armed now, once.
func (r *renewals) fireAll() {
	r.mu.Lock()
	var due []*renewal
	for _, next := range r.pending {
		if !next.done {
			next.done = true
			due = append(due, next)
		}
	}
	r.mu.Unlock()
	for _, next := range due {
		next.f()
	}
}

// clientRegistrar is the daemon's Registrar over the agent-secrets client's own renewer, as the
// daemon's adapter wires it (internal/daemon/registrations.go), on the test's renewals.
type clientRegistrar struct {
	client   *cipher.Client
	renewals *renewals
}

func (c clientRegistrar) KeepPodSession(ctx context.Context, s supervise.PodSession) (func(), error) {
	return c.client.KeepPodSession(ctx, cipher.PodSessionRenewal{
		PodUID: s.PodUID, Session: s.Session, Thumbprint: s.Thumbprint, Generation: int64(s.Generation), Token: s.Token, After: c.renewals.after, Bound: s.Bound,
	})
}

func (c clientRegistrar) ReleasePodSessions(ctx context.Context, names []string) error {
	return c.client.ReleasePodSessions(ctx, names)
}

// TestEachRegistrationCarriesItsRolesThumbprint: two roles of one issue pod read the same exchange
// token, and each role's session is registered bound to the thumbprint its own shim's hello named
// — never a sibling's — so the broker refuses either role a trade for the other's session. Every
// renewal of each registration sends its own thumbprint again.
func TestEachRegistrationCarriesItsRolesThumbprint(t *testing.T) {
	const (
		pod = "pod-uid-shared"
		kA  = "thumbprint-of-the-testers-own-key-aaaaaaaaa"
		kB  = "thumbprint-of-the-reviewers-own-key-bbbbbbb"
	)
	broker := &exchangeBroker{t: t}
	server := httptest.NewServer(broker)
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("the-daemon's-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	timers := &renewals{}
	h := newRegistrationHarness(t, clientRegistrar{
		client:   &cipher.Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile},
		renewals: timers,
	})
	runRole(h, nil, claim.RoleTester, pod, "ses_tester", kA)
	runRole(h, nil, claim.RoleReviewer, pod, "ses_reviewer", kB)
	for range 3 {
		timers.fireAll()
	}

	own := map[string]string{"ses_tester": kA, "ses_reviewer": kB}
	role := map[string]claim.Role{"ses_tester": claim.RoleTester, "ses_reviewer": claim.RoleReviewer}
	writes := map[string]int{}
	for _, write := range broker.received() {
		writes[write.session]++
		if write.podUID != pod || write.thumbprint != own[write.session] {
			t.Errorf("a write of the %s role's session %s registered pod %s bound to %q, want pod %s bound to its own hello's thumbprint %q",
				role[write.session], write.session, write.podUID, write.thumbprint, pod, own[write.session])
		}
	}
	for session := range own {
		if writes[session] != 4 {
			t.Errorf("the %s role's session %s was written %d times, want 4: its registration and 3 renewals", role[session], session, writes[session])
		}
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
		tester, omp := runRole(h, broker, claim.RoleTester, "pod-uid-tester", "ses_tester", "tp-tester")
		implementer, _ := claim.NewToken(testProject, "LEGION-840", claim.RoleImplementer)
		h.runtime.ScriptLaunchSession(implementer, claudeSession)
		_, claude := runRole(h, broker, claim.RoleImplementer, "pod-uid-implementer", claudeSession, "tp-implementer")
		inPod(h, tester, "pod-uid-second", 2, true)
		boot := h.relaunch(tester)
		h.connected(tester, "pod-uid-second")
		h.hello(tester, "pod-uid-second", "tp-second")
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
