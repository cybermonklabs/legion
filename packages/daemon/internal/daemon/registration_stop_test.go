package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/cipher"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// answeringBroker is POST /v1/oauth/exchange/pod-sessions answering every write at once, as the
// registration stands after it. atWrite, when set, is called with the write's number (1 for the
// first) as the write arrives, before it is answered.
type answeringBroker struct {
	pod     string
	atWrite func(n int)

	mu     sync.Mutex
	writes int
}

func (b *answeringBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct{ Session, Thumbprint string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	b.mu.Lock()
	b.writes++
	n := b.writes
	b.mu.Unlock()
	if b.atWrite != nil {
		b.atWrite(n)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pod_uid": b.pod, "session": body.Session, "thumbprint": body.Thumbprint, "expires_at": time.Now().Add(24 * time.Hour),
	})
}

func (b *answeringBroker) taken() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.writes
}

// firedRenewals is the clock a registration renews by, fired by the test alone.
type firedRenewals struct {
	mu      sync.Mutex
	pending []func()
}

func (r *firedRenewals) after(_ time.Duration, f func()) func() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, f)
	n := len(r.pending) - 1
	return func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		stopped := r.pending[n] != nil
		r.pending[n] = nil
		return stopped
	}
}

// fire runs the latest renewal still armed.
func (r *firedRenewals) fire() {
	r.mu.Lock()
	var f func()
	for i := len(r.pending) - 1; i >= 0; i-- {
		if r.pending[i] != nil {
			f, r.pending[i] = r.pending[i], nil
			break
		}
	}
	r.mu.Unlock()
	if f != nil {
		f()
	}
}

// renewingRegistrar is the daemon's Registrar over the agent-secrets client's own renewer, as
// newRegistrar wires it, renewing on the test's fired renewals.
type renewingRegistrar struct {
	client   *cipher.Client
	renewals *firedRenewals
}

func (r renewingRegistrar) KeepPodSession(ctx context.Context, s supervise.PodSession) (func(), error) {
	return r.client.KeepPodSession(ctx, cipher.PodSessionRenewal{
		PodUID: s.PodUID, Session: s.Session, Thumbprint: s.Thumbprint, Generation: int64(s.Generation), Token: s.Token, After: r.renewals.after, Bound: s.Bound,
	})
}

// TestAStopDoesNotWaitOutTheRPCTimeoutWhileTheShimIsAskedForItsToken drives the stop of a
// registration (stopRegistration, from the decision that lets the process go) while the
// registration's renewal is reading the pod's token over the real worker stream (stream.Conn's
// AgentSecretsToken), with the stream's events handed to the machine by a pump that calls Handle
// itself, so it waits on the lock the stopping decision holds. The shim sends a tool step, waits
// until the pump has taken it and called Handle — parked on that lock — then sends a second step
// behind it, and only then the token's answer.
//
// The stop waits for the write in flight, by design (TestANewIncarnationsFirstWriteFollowsThe
// OldOnesLastWrite). What it must not do is wait out the RPC timeout because the answer it waits
// for is stuck behind the lock it holds. It cannot: a connection's reader hands its events to an
// unbounded queue (stream.eventQueue) and never waits on whoever consumes them, so it reads past
// the step the parked pump has not taken and hands the token's answer to the write; the write is
// sent, and the stop returns once the broker answers, a small fraction of the RPC timeout. A
// reader that waited on its consumer would hold the answer until the write's own deadline, the
// RPC timeout, ended the write unsent.
func TestAStopDoesNotWaitOutTheRPCTimeoutWhileTheShimIsAskedForItsToken(t *testing.T) {
	const (
		pod        = "pod-uid-stopwait"
		session    = "ses_stopwait"
		thumbprint = "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"
		bootToken  = "boot-stopwait"
		rpc        = 5 * time.Second
		// hold is how long the shim sits on the renewal's token request once the stopping decision
		// holds the lock, before it answers: long enough that the pump is parked on the lock.
		hold = 300 * time.Millisecond
	)
	// The pump's state, which the broker reads as the renewal's write (the second) arrives:
	// waitingOnStep is when the pump called Handle with the first tool step, and leftStep when
	// that call returned; later steps change neither. parkedAtRenewal is whether, as the
	// renewal's write reached the broker, the pump had called Handle and not yet left it — so the
	// write landed while the stop held the lock the pump waited on. The stop waits for that
	// write's answer while it holds the lock, so the pump cannot have left; unlike the moment the
	// test reads after the stop returns, which Handle's deferred unlock precedes.
	var waitingOnStep, leftStep atomic.Int64
	var renewalArrived, parkedAtRenewal atomic.Bool
	broker := &answeringBroker{pod: pod, atWrite: func(n int) {
		if n == 2 {
			parkedAtRenewal.Store(waitingOnStep.Load() != 0 && leftStep.Load() == 0)
			renewalArrived.Store(true)
		}
	}}
	server := httptest.NewServer(broker)
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("the-daemon's-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	renewals := &firedRenewals{}

	project, _ := claim.ProjectToken("legion")
	token, _ := claim.NewToken(project, "LEGION-840", claim.RoleTester)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resolve := func(boot string) (claim.Token, uint64, bool, bool, error) {
		if boot != bootToken {
			return "", 0, false, false, nil
		}
		return token, 1, false, true, nil
	}
	listener, err := stream.Listen(ctx, "unix://"+filepath.Join(shortTempDir(t), "worker-stream.sock"), resolve,
		stream.Options{RPCTimeout: rpc, Log: quietLogger()})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	rt := fake.NewRuntime()
	rt.ScriptSpawn(fake.SpawnResult{Locator: runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation(pod, 1),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-legion-840", PodUID: pod, Container: runtime.IssuePodContainer, Generation: 1},
	}})
	sup := newSupervisor(ctx, nil, "legion", t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: listener, Store: &outboxClaimStore{}, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Minute, RegistrationIntervals: 2, RPC: rpc, Probe: time.Hour, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: api.DaemonAPIVersion,
		Secrets:        enrollsEveryPod{},
		Registrar: renewingRegistrar{
			client:   &cipher.Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile, Log: quietLogger()},
			renewals: renewals,
		},
		RefreshGate: supervise.NewRefreshGate(0),
	}
	t.Cleanup(sup.stop)
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: project, Tree: "LEGION-840", Issue: "LEGION-840",
		Role: claim.RoleTester, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create %s: %v", token, err)
	}
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: token}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// The pump: every stream event to the machine, through Handle itself, so a decision holding
	// the machine's lock holds the pump too. It records waitingOnStep and leftStep (above).
	// helloHandled closes once the pump has handed the machine the shim's hello, which the agent's
	// registration follows, as it does on a pod.
	helloHandled := make(chan struct{})
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		for ev := range listener.Events() {
			mapped, err := superviseEvent(ev)
			if err != nil {
				continue
			}
			_, step := mapped.(supervise.StreamToolStep)
			first := step && waitingOnStep.CompareAndSwap(0, time.Now().UnixNano())
			_ = machine.Handle(ctx, mapped)
			if _, hello := mapped.(supervise.StreamHello); hello {
				close(helloHandled)
			}
			if first {
				leftStep.Store(time.Now().UnixNano())
			}
		}
	}()

	// The shim, over the real socket: its hello names the pod's identity; it answers the enrollment
	// hand-off, and every token read at once until holdNext is set. The next token read it reports
	// on held and answers only once answer closes, and only after the tool step frame, which the
	// reader must hand on before it reads the answer.
	conn, err := net.Dial("unix", strings.TrimPrefix(listener.Addr(), "unix://"))
	if err != nil {
		t.Fatalf("dial the worker stream: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	writer, lines := shimwire.NewWriter(conn), bufio.NewReader(conn)
	// send is called from the shim's goroutine, which may outlive a failed test, so it never
	// touches t: the first write error is kept for the test goroutine to report.
	var writeMu sync.Mutex
	var sendErr error
	send := func(frame shimwire.Frame) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := writer.WriteFrame(frame); err != nil && sendErr == nil {
			sendErr = fmt.Errorf("the shim's %s: %w", frame.FrameType(), err)
		}
	}
	shimFailed := func() error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return sendErr
	}
	send(shimwire.Hello2{BootToken: bootToken, AgentSecrets: &shimwire.AgentSecretsHello{Thumbprint: thumbprint, PodToken: "eyJhbGciOiJSUzI1NiJ9.e30.at-the-hello"}})
	line, err := lines.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read the hello's answer: %v", err)
	}
	if frame, err := shimwire.Decode(line); err != nil || frame.FrameType() != shimwire.TypeHelloAck {
		t.Fatalf("the daemon answered the hello with %q (%v), want hello_ack", line, err)
	}
	var holdNext atomic.Bool
	// answeredAt is when the shim wrote the held token's answer, which it does only once the pump
	// has called Handle with the tool step it sent first, and so waits on the lock the stop holds.
	var answeredAt atomic.Int64
	held, answer := make(chan string, 1), make(chan struct{})
	go func() {
		for {
			line, err := lines.ReadBytes('\n')
			if err != nil {
				return
			}
			frame, err := shimwire.Decode(line)
			if err != nil {
				continue
			}
			switch frame := frame.(type) {
			case shimwire.AgentSecretsEnrollment:
				send(shimwire.AgentSecretsEnrollmentResult{ID: frame.ID, OK: true})
			case shimwire.AgentSecretsToken:
				if holdNext.Swap(false) {
					held <- frame.ID
					<-answer
					send(shimwire.ToolExecutionStart{ToolCallID: "call-1", ToolName: "bash"})
					for deadline := time.Now().Add(rpc); waitingOnStep.Load() == 0 && time.Now().Before(deadline); {
						time.Sleep(5 * time.Millisecond)
					}
					// A second frame behind the one the pump holds: the reader has it to hand on
					// while the pump is parked, before it reads the token's answer.
					send(shimwire.ToolExecutionEnd{ToolCallID: "call-1", ToolName: "bash"})
					answeredAt.Store(time.Now().UnixNano())
				}
				send(shimwire.AgentSecretsTokenResult{ID: frame.ID, OK: true, PodToken: "eyJhbGciOiJSUzI1NiJ9.e30.read-again"})
			}
		}
	}()

	select {
	case <-helloHandled:
	case <-time.After(rpc):
		t.Fatal("the shim's hello never reached the machine")
	}
	for _, ev := range []supervise.Event{
		supervise.RequestRegister{Claim: token, Generation: 1, Session: session, SessionFile: "/sessions/" + session + ".jsonl", PluginContract: api.DaemonAPIVersion},
		supervise.RequestReady{Claim: token, Generation: 1, Session: session},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	if n := broker.taken(); n != 1 {
		t.Fatalf("the broker took %d writes of %s once the agent registered, want its first", n, session)
	}

	// The renewal: its write asks the shim for the pod's token, and the shim holds the answer.
	holdNext.Store(true)
	go renewals.fire()
	select {
	case <-held:
	case <-time.After(rpc):
		t.Fatalf("the renewal of %s never asked the shim for the pod's token", session)
	}

	// The stop: a decision that lets the process go, holding the machine's lock while it waits for
	// the write in flight. The shim then sends a frame the pump must hand the machine — which
	// waits on that lock — before the token's answer.
	stopped := make(chan time.Duration, 1)
	stopStart := time.Now()
	go func() {
		_ = machine.Handle(ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByOperator})
		stopped <- time.Since(stopStart)
	}()
	time.Sleep(hold)
	close(answer)
	var waited time.Duration
	select {
	case waited = <-stopped:
	case <-time.After(2 * rpc):
		t.Fatalf("the stop of %s had not returned %s after it started", session, 2*rpc)
	}
	stopReturned := time.Now()
	sinceAnswer := stopReturned.Sub(time.Unix(0, answeredAt.Load()))
	t.Logf("the stop of %s waited %s in all, %s of it after the shim answered the token (RPC timeout %s)", session, waited, sinceAnswer, rpc)
	if err := shimFailed(); err != nil {
		t.Errorf("%v", err)
	}

	entered := waitingOnStep.Load()
	if entered == 0 || entered > stopReturned.UnixNano() {
		t.Fatalf("the pump never waited on the machine's lock with the tool step while the stop held it (entered %d, the stop returned %d): the test proves nothing", entered, stopReturned.UnixNano())
	}
	// Not leftStep against stopReturned: Handle's deferred unlock frees the lock before the stop's
	// Handle returns, so the parked pump may legally leave first. Whether the write in flight
	// landed while the pump was parked is read at the write itself.
	if renewalArrived.Load() && !parkedAtRenewal.Load() {
		t.Errorf("the renewal's write of %s reached the broker when the pump was not parked on the lock the stop held (it had not called Handle, or had left it): the stop did not hold the lock while its write was in flight", session)
	}
	if sinceAnswer > rpc/5 {
		t.Errorf("the stop of %s waited %s after the shim answered the token, against an RPC timeout of %s: the answer was stuck behind the lock the stop held", session, sinceAnswer, rpc)
	}
	if n := broker.taken(); n != 2 {
		t.Errorf("the broker took %d writes of %s, want 2: the renewal in flight was not sent before the stop returned", n, session)
	}
	_ = pumped
}
