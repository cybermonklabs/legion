package supervise

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/cipher"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// holdingBroker is POST /v1/oauth/exchange/pod-sessions as a server serves it, with the pod
// and session the test registers: each write is committed, its thumbprint becoming the row's
// binding, and then answered. When holdNext is set, the next write is held open until release
// closes, and committed then — whether or not the caller is still waiting, as a server that
// received a write commits it — and committed closes once it has been.
type holdingBroker struct {
	t         *testing.T
	pod       string
	held      chan struct{}
	release   chan struct{}
	committed chan struct{}

	mu       sync.Mutex
	holdNext bool
	events   []string
	row      string
}

func (b *holdingBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session, Thumbprint string
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		b.t.Errorf("the registration's body does not decode: %v", err)
	}
	b.mu.Lock()
	hold := b.holdNext
	b.holdNext = false
	b.events = append(b.events, "received "+body.Thumbprint)
	b.mu.Unlock()
	if hold {
		close(b.held)
		<-b.release
	}
	b.mu.Lock()
	b.row = body.Thumbprint
	b.events = append(b.events, "answered "+body.Thumbprint)
	b.mu.Unlock()
	if hold {
		close(b.committed)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pod_uid": b.pod, "session": body.Session, "thumbprint": body.Thumbprint, "expires_at": epoch.Add(24 * time.Hour),
	})
}

func (b *holdingBroker) seen() ([]string, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.events), b.row
}

// renewalTimers is the clock the registrations renew by, fired by hand: the machine's own clock
// moves nothing here, so only the renewal the test fires runs.
type renewalTimers struct {
	mu      sync.Mutex
	pending []*renewalTimer
}

type renewalTimer struct {
	f    func()
	done bool
}

func (r *renewalTimers) after(_ time.Duration, f func()) func() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	timer := &renewalTimer{f: f}
	r.pending = append(r.pending, timer)
	return func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		stopped := !timer.done
		timer.done = true
		return stopped
	}
}

// fire runs the oldest timer neither fired nor stopped.
func (r *renewalTimers) fire() {
	r.mu.Lock()
	var next *renewalTimer
	for _, timer := range r.pending {
		if !timer.done {
			next = timer
			break
		}
	}
	if next != nil {
		next.done = true
	}
	r.mu.Unlock()
	if next != nil {
		next.f()
	}
}

// clientRegistrar is the daemon's Registrar over the agent-secrets client's own renewer, as the
// daemon's adapter wires it, on the test's renewal timers.
type clientRegistrar struct {
	client *cipher.Client
	timers *renewalTimers
}

func (c clientRegistrar) KeepPodSession(ctx context.Context, s PodSession) (func(), error) {
	return c.client.KeepPodSession(ctx, cipher.PodSessionRenewal{
		PodUID: s.PodUID, Session: s.Session, Thumbprint: s.Thumbprint, Generation: int64(s.Generation), Token: s.Token, After: c.timers.after, Bound: s.Bound,
	})
}

func (c clientRegistrar) ReleasePodSessions(ctx context.Context, names []string) error {
	return c.client.ReleasePodSessions(ctx, names)
}

// TestANewIncarnationsFirstWriteFollowsTheOldOnesLastWrite: a role relaunched in its pod (a new
// generation, so a new incarnation, with a new key) registers its session again with the new
// hello's thumbprint. The old incarnation's renewal may be in flight when the old process is let
// go: its write is not cancelled but waited for, so the broker answers it before the new
// incarnation's first write is sent, and the row ends bound to the new key. Were the old write
// only cancelled, the server would still commit it when it got to it, after the new write, and
// put the old key back on the row until the next renewal six hours on.
func TestANewIncarnationsFirstWriteFollowsTheOldOnesLastWrite(t *testing.T) {
	const (
		pod    = "pod-uid-9"
		oldKey = "old-incarnation-key-thumbprint-aaaaaaaaaaaa"
		newKey = "new-incarnation-key-thumbprint-bbbbbbbbbbbb"
	)
	broker := &holdingBroker{t: t, pod: pod, held: make(chan struct{}), release: make(chan struct{}), committed: make(chan struct{})}
	server := httptest.NewServer(broker)
	defer server.Close()
	released := sync.OnceFunc(func() { close(broker.release) })
	defer released()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("the-daemon's-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t)
	h.withSecrets()
	timers := &renewalTimers{}
	h.deps.Registrar = clientRegistrar{client: &cipher.Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile}, timers: timers}
	h.start(h.store.load(h.token))
	h.conn.SetAgentSecretsToken(testIdentity.PodToken)
	inPod := func(generation uint64) fake.SpawnResult {
		return fake.SpawnResult{Locator: runtime.Locator{
			Runtime: runtime.RuntimeSandbox, Claim: h.token, Incarnation: runtime.SandboxIncarnation(pod, generation),
			Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-209", PodUID: pod, Container: "roles", Generation: generation},
		}}
	}
	hello := func(thumbprint string) error {
		h.conns.Register(h.token, h.conn)
		return h.handle(StreamHello{Claim: h.token, Generation: h.generation(), AgentSecrets: &AgentSecretsIdentity{Thumbprint: thumbprint, PodToken: testIdentity.PodToken}})
	}

	h.rt.ScriptSpawn(inPod(1))
	h.launch()
	if err := hello(oldKey); err != nil {
		t.Fatal(err)
	}
	h.register()
	h.ready()
	if events, row := broker.seen(); row != oldKey {
		t.Fatalf("after the first incarnation registered, the broker saw %v and binds %q, want %q", events, row, oldKey)
	}

	broker.mu.Lock()
	broker.holdNext = true
	broker.mu.Unlock()
	go timers.fire()
	<-broker.held

	h.rt.ScriptResume(inPod(2))
	relaunched := make(chan error, 1)
	go func() {
		if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator}); err != nil {
			relaunched <- err
			return
		}
		if err := h.handle(RequestResume{Claim: h.token}); err != nil {
			relaunched <- err
			return
		}
		relaunched <- hello(newKey)
	}()
	for wait := time.Now().Add(2 * time.Second); time.Now().Before(wait); time.Sleep(10 * time.Millisecond) {
		if events, _ := broker.seen(); slices.Contains(events, "received "+newKey) {
			break
		}
	}
	released()
	if err := <-relaunched; err != nil {
		t.Fatalf("the relaunch: %v", err)
	}
	<-broker.committed

	events, row := broker.seen()
	lastOld := -1
	for i, event := range events {
		if event == "answered "+oldKey {
			lastOld = i
		}
	}
	switch first := slices.Index(events, "received "+newKey); {
	case first == -1:
		t.Fatalf("the broker saw %v: the new incarnation wrote nothing", events)
	case first < lastOld:
		t.Errorf("the broker saw %v: the new incarnation's first write arrived before the old incarnation's last write was answered", events)
	}
	if row != newKey {
		t.Fatalf("the broker's row of %s for %s ends bound to %q, want the new incarnation's key, %q (events %v)", pod, session, row, newKey, events)
	}
}

// keyRecorder is a Registrar that records the thumbprint, the generation and the claim's-current-
// generation reader of every registration a machine starts.
type keyRecorder struct {
	mu      sync.Mutex
	keys    []string
	gens    []uint64
	latests []func() uint64
}

func (r *keyRecorder) KeepPodSession(_ context.Context, s PodSession) (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, s.Thumbprint)
	r.gens = append(r.gens, s.Generation)
	r.latests = append(r.latests, s.Latest)
	return func() {}, nil
}

func (*keyRecorder) ReleasePodSessions(context.Context, []string) error { return nil }

func (r *keyRecorder) registered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.keys)
}

func (r *keyRecorder) generations() []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.gens)
}

// TestEachProcessRegistersAtItsLaunchGeneration: every registration carries the launch generation
// of the process it registers — the claim's generation, which each launch raises and stores before
// it starts the process (launchTries), and which the locator records (SandboxLocator.Generation) — so
// the broker can order two processes' writes of one (pod, session) row. A relaunch in the same pod
// registers at a higher generation than the process it replaced, and a daemon restart, which
// registers the running process again at its shim's first hello, sends that process's own
// generation again, never a lower one.
func TestEachProcessRegistersAtItsLaunchGeneration(t *testing.T) {
	const pod = "pod-uid-8"
	h := newHarness(t)
	h.withSecrets()
	recorder := &keyRecorder{}
	h.deps.Registrar = recorder
	h.start(h.store.load(h.token))
	inPod := func(generation uint64) fake.SpawnResult {
		return fake.SpawnResult{Locator: runtime.Locator{
			Runtime: runtime.RuntimeSandbox, Claim: h.token, Incarnation: runtime.SandboxIncarnation(pod, generation),
			Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-209", PodUID: pod, Container: "roles", Generation: generation},
		}}
	}
	hello := func() {
		t.Helper()
		h.conns.Register(h.token, h.conn)
		h.must(StreamHello{Claim: h.token, Generation: h.generation(), AgentSecrets: &AgentSecretsIdentity{Thumbprint: testIdentity.Thumbprint, PodToken: testIdentity.PodToken}})
	}

	h.rt.ScriptSpawn(inPod(1))
	h.launch()
	hello()
	h.register()
	h.ready()
	h.rt.ScriptResume(inPod(2))
	h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
	h.must(RequestResume{Claim: h.token})
	hello()
	h.restart()
	hello()

	if got, want := h.generation(), uint64(2); got != want || h.locator().Sandbox.Generation != want {
		t.Fatalf("the claim is at generation %d, its locator at %d, want both %d: the test drives two launches", got, h.locator().Sandbox.Generation, want)
	}
	if got, want := recorder.generations(), []uint64{1, 2, 2}; !slices.Equal(got, want) {
		t.Fatalf("the session of pod %s was registered at generations %v, want %v: the first process at 1, the relaunch at 2, and the restart registering the running process again at its own 2", pod, got, want)
	}
}

// TestARegistrationReadsTheClaimsCurrentGenerationWhenItIsAnswered: a registration reads the claim's
// current generation when an answer comes back at a later one (PodSession.Latest), to tell a later
// process of this claim from a registration this daemon never made. It reads it then, not when the
// registration started: the first process's registration, asked after the relaunch, answers the
// claim's generation 2. It reads it without the machine's lock, from the view each decision
// publishes, since a renewal's answer comes back on the clock's goroutine while a decision may hold
// that lock.
func TestARegistrationReadsTheClaimsCurrentGenerationWhenItIsAnswered(t *testing.T) {
	const pod = "pod-uid-11"
	h := newHarness(t)
	h.withSecrets()
	recorder := &keyRecorder{}
	h.deps.Registrar = recorder
	h.start(h.store.load(h.token))
	inPod := func(generation uint64) fake.SpawnResult {
		return fake.SpawnResult{Locator: runtime.Locator{
			Runtime: runtime.RuntimeSandbox, Claim: h.token, Incarnation: runtime.SandboxIncarnation(pod, generation),
			Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-209", PodUID: pod, Container: "roles", Generation: generation},
		}}
	}
	hello := func() {
		t.Helper()
		h.conns.Register(h.token, h.conn)
		h.must(StreamHello{Claim: h.token, Generation: h.generation(), AgentSecrets: &AgentSecretsIdentity{Thumbprint: testIdentity.Thumbprint, PodToken: testIdentity.PodToken}})
	}

	h.rt.ScriptSpawn(inPod(1))
	h.launch()
	hello()
	h.register()
	h.ready()
	recorder.mu.Lock()
	first := recorder.latests[0]
	recorder.mu.Unlock()
	if first == nil {
		t.Fatal("the registration carries no reader of the claim's current generation")
	}
	if got := first(); got != 1 {
		t.Fatalf("the first process's registration reads the claim's generation as %d, want 1", got)
	}
	h.rt.ScriptResume(inPod(2))
	h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
	h.must(RequestResume{Claim: h.token})
	if got := first(); got != 2 {
		t.Fatalf("after the relaunch the first process's registration reads the claim's generation as %d, want 2, the claim's current one", got)
	}

	// Read while a decision holds the machine's lock: it must not wait on it.
	h.m.mu.Lock()
	read := make(chan uint64, 1)
	go func() { read <- first() }()
	select {
	case got := <-read:
		if got != 2 {
			t.Errorf("read under a held lock, the claim's generation is %d, want 2", got)
		}
	case <-time.After(time.Second):
		t.Error("reading the claim's current generation waited on the machine's lock")
	}
	h.m.mu.Unlock()
}

// TestARegistrationKeepsTheKeyItsProcessFirstHelloNamed: a registration's thumbprint is pinned at
// the first hello of its process (incarnation) that names one. A later hello of the same process
// naming another key — which only the holder of that claim's boot token can send, and which an
// honest shim, caching its thumbprint, never does — is logged naming both keys and changes no
// registration: one already made is not made again with the new key, and one made after it uses
// the first.
func TestARegistrationKeepsTheKeyItsProcessFirstHelloNamed(t *testing.T) {
	const (
		pod    = "pod-uid-7"
		first  = "first-hello-key-thumbprint-aaaaaaaaaaaaaaaa"
		second = "second-hello-key-thumbprint-bbbbbbbbbbbbbbb"
	)
	for name, registerBetween := range map[string]bool{
		"a later hello once registered":    true,
		"a later hello before registering": false,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.withSecrets()
			recorder := &keyRecorder{}
			h.deps.Registrar = recorder
			h.start(h.store.load(h.token))
			h.rt.ScriptSpawn(fake.SpawnResult{Locator: runtime.Locator{
				Runtime: runtime.RuntimeSandbox, Claim: h.token, Incarnation: runtime.SandboxIncarnation(pod, 1),
				Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-209", PodUID: pod, Container: "roles", Generation: 1},
			}})
			hello := func(thumbprint string) {
				t.Helper()
				h.conns.Register(h.token, h.conn)
				h.must(StreamHello{Claim: h.token, Generation: h.generation(), AgentSecrets: &AgentSecretsIdentity{Thumbprint: thumbprint, PodToken: testIdentity.PodToken}})
			}
			h.launch()
			hello(first)
			if registerBetween {
				h.register()
			}
			hello(second)
			if !registerBetween {
				h.register()
			}
			if got := recorder.registered(); !slices.Equal(got, []string{first}) {
				t.Fatalf("the session of pod %s was registered with %q, want once, with the key the process's first hello named, %s, and never with %s", pod, got, first, second)
			}
			if logged := h.logs.String(); !strings.Contains(logged, first) || !strings.Contains(logged, second) {
				t.Errorf("the later hello's other key was not logged naming both keys:\n%s", logged)
			}
		})
	}
}

// TestAHelloPinsItsKeyOnlyWhereTheMachineRegisters: the pin of a hello's key exists for the
// registration alone (ensureRegistered), so a machine with no registrar — the daemon's wiring with
// runtime.kubernetes.agent_secrets.exchange_audience unset — pins no key, and a later hello of the
// same process naming another key logs no Warn of a registration that does not exist (R9). With a
// registrar the first hello's key is pinned and the later one logged, naming both keys.
func TestAHelloPinsItsKeyOnlyWhereTheMachineRegisters(t *testing.T) {
	const (
		pod    = "pod-uid-9"
		first  = "first-hello-key-thumbprint-aaaaaaaaaaaaaaaa"
		second = "second-hello-key-thumbprint-bbbbbbbbbbbbbbb"
	)
	for name, registrar := range map[string]Registrar{
		"with no registrar": nil,
		"with a registrar":  &keyRecorder{},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.withSecrets()
			h.deps.Registrar = registrar
			h.start(h.store.load(h.token))
			incarnation := runtime.SandboxIncarnation(pod, 1)
			h.rt.ScriptSpawn(fake.SpawnResult{Locator: runtime.Locator{
				Runtime: runtime.RuntimeSandbox, Claim: h.token, Incarnation: incarnation,
				Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-209", PodUID: pod, Container: "roles", Generation: 1},
			}})
			hello := func(thumbprint string) {
				t.Helper()
				h.conns.Register(h.token, h.conn)
				h.must(StreamHello{Claim: h.token, Generation: h.generation(), AgentSecrets: &AgentSecretsIdentity{Thumbprint: thumbprint, PodToken: testIdentity.PodToken}})
			}
			h.launch()
			hello(first)
			h.register()
			hello(second)

			h.m.mu.Lock()
			pinned := h.m.helloKey
			h.m.mu.Unlock()
			warned := h.logs.lines("level=WARN", second)
			if registrar == nil {
				if pinned != (helloKey{}) {
					t.Errorf("the machine with no registrar pinned %+v, want no key: no registration exists to keep one", pinned)
				}
				if len(warned) != 0 {
					t.Errorf("the machine with no registrar logged a Warn naming the later hello's key %s, want none:\n%s", second, strings.Join(warned, "\n"))
				}
				return
			}
			if want := (helloKey{incarnation: incarnation, thumbprint: first}); pinned != want {
				t.Errorf("the machine with a registrar pinned %+v, want %+v, the key the process's first hello named", pinned, want)
			}
			if len(warned) != 1 || !strings.Contains(warned[0], first) {
				t.Errorf("the machine with a registrar logged %q, want one Warn naming both the first hello's key %s and the later one's %s", warned, first, second)
			}
		})
	}
}
