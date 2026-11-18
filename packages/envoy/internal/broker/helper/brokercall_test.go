// packages/envoy/internal/broker/helper/brokercall_test.go
//go:build linux

package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The broker-call ops (login, enroll-session-key, unenroll-session-key) against server_test.go's
// fake broker: which failures are known and which unknown, the bound and its timer, the stop, and
// unregister, whose revoke goes on after its answer.

// settleCause opens the cause of every unknown outcome requestReach.settle marked
// (mayHaveReachedCause). The answer brokerCall's timer gives says "did not end in" instead.
const settleCause = "the request may have reached the broker, but the helper did not get its result ("

// wantSettled fails the row unless resp, the answer to what, is code saying the outcome is unknown
// with settle's cause: an answer from brokerCall's timer, which comes at the bound plus 1 s and says
// the operation did not end, fails, though it says that the outcome is unknown as well. When the
// answer came is the row's own to prove, on the server's clock (armBrokerCall).
func wantSettled(t *testing.T, what string, resp Response, code string) {
	t.Helper()
	if resp.OK || resp.Code != code || !strings.Contains(resp.Error, "the outcome is unknown") {
		t.Fatalf("%s: %+v; want %s saying the outcome is unknown", what, resp, code)
	}
	if !strings.Contains(resp.Error, settleCause) || strings.Contains(resp.Error, "did not end in") {
		t.Errorf("%s: error %q; want settle's cause %q, not the timer's \"did not end in\"", what, resp.Error, settleCause)
	}
}

// callReply is the reply a call sent in the background got, or the error that ended it.
type callReply struct {
	resp Response
	err  error
}

// callInBackground sends req to the helper at sock on a goroutine of its own and answers a channel
// that carries its reply, so the row can move the server's clock while the call waits. The
// client's deadline is the shared wait's own, as rig.call's is.
func callInBackground(t *testing.T, sock string, req Request) <-chan callReply {
	t.Helper()
	timeout := time.Duration(math.MaxInt64)
	if deadline, bounded := testwait.Context(t).Deadline(); bounded {
		timeout = time.Until(deadline)
	}
	replies := make(chan callReply, 1)
	go func() {
		resp, err := Call(sock, req, timeout)
		replies <- callReply{resp: resp, err: err}
	}()
	return replies
}

// armBrokerCall waits for a broker-call op to arm its bound and brokerCall's timer on clock, the
// server's, and requires each where brokerCall arms it: the end of the op's context at
// BrokerCallBound from the clock's present, and the timer a second after that. It then
// requires that no reply has come yet, so whatever answers the op comes once the row moves the
// clock.
func armBrokerCall(t *testing.T, clock *clocktest.Fake, replies <-chan callReply, what string) {
	t.Helper()
	now := clock.Now()
	want := []time.Time{now.Add(BrokerCallBound), now.Add(BrokerCallBound + time.Second)}
	if armed := clock.AwaitArmed(t, what+": the op's bound and brokerCall's timer", 2); !slices.EqualFunc(armed, want, time.Time.Equal) {
		t.Fatalf("%s: the server armed %v; want the op's bound at %v and brokerCall's timer at %v", what, armed, want[0], want[1])
	}
	select {
	case reply := <-replies:
		t.Fatalf("%s: answered before the server's clock moved: %+v %v", what, reply.resp, reply.err)
	default:
	}
}

// replyOf waits for the reply on replies, and fails the row when the call ended in an error.
func replyOf(t *testing.T, replies <-chan callReply, what string) Response {
	t.Helper()
	reply := testwait.Receive(t, what, replies)
	if reply.err != nil {
		t.Fatalf("%s: %v", what, reply.err)
	}
	return reply.resp
}

// TestAFailedRequestIsKnownOrUnknownByWhetherTheBrokerGotIt: a broker request that failed after
// the broker got it (a 503, which a front door can send after the broker acted, or a 2xx the
// helper cannot read, which the broker answered and so may have acted on) has an unknown outcome,
// and one that never reached the broker (a refused dial) is a known failure. Each case answers
// one code; each unknown answer says that the outcome is unknown, and no known one does.
func TestAFailedRequestIsKnownOrUnknownByWhetherTheBrokerGotIt(t *testing.T) {
	enroll := Request{Op: "enroll-session-key", RuntimeID: "box-1", Thumbprint: "tp-1"}
	unenroll := Request{Op: "unenroll-session-key", EnrollmentID: "enr-1"}
	login := Request{Op: "login"}
	for _, tc := range []struct {
		name string
		req  Request
		// refuse points the helper at a broker URL that refuses the dial.
		refuse bool
		// fail sets the fake's failure (a 503, or a 2xx with none of what the helper needs), under
		// its lock.
		fail    func(f *fakeBroker)
		code    string
		unknown bool
	}{
		{name: "a 503 on the enrollment is unknown", req: enroll, fail: func(f *fakeBroker) { f.failFirst = 1 }, code: CodeEnrollUnknown, unknown: true},
		{name: "a 200 with no enrollment_id on the enrollment is unknown", req: enroll, fail: func(f *fakeBroker) { f.enrollEmpty2xx = true }, code: CodeEnrollUnknown, unknown: true},
		{name: "a refused dial on the enrollment is a known failure", req: enroll, refuse: true, code: CodeEnrollFailed},
		{name: "a 503 on the revoke is unknown", req: unenroll, fail: func(f *fakeBroker) { f.revokeFailFirst = 1 }, code: CodeUnenrollUnknown, unknown: true},
		{name: "a refused dial on the revoke is a known failure", req: unenroll, refuse: true, code: CodeUnenrollFailed},
		{name: "a 503 on the device authorization is unknown", req: login, fail: func(f *fakeBroker) { f.deviceAuthFail = 1 }, code: CodeLoginFailed, unknown: true},
		{name: "a 200 with no device_code or user_code on the device authorization is unknown", req: login, fail: func(f *fakeBroker) { f.deviceAuthEmpty2xx = true }, code: CodeLoginFailed, unknown: true},
		{name: "a refused dial on the device authorization is a known failure", req: login, refuse: true, code: CodeLoginFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "")
			if tc.req.Op != "login" {
				r.login(t)
			}
			if tc.refuse {
				r.srv.Broker.URL = brokeraddr.New("http://127.0.0.1:1")
			}
			if tc.fail != nil {
				r.fake.mu.Lock()
				tc.fail(r.fake)
				r.fake.mu.Unlock()
			}
			resp := r.call(t, tc.req)
			if resp.OK || resp.Code != tc.code {
				t.Fatalf("%s: %+v; want %s", tc.req.Op, resp, tc.code)
			}
			if said := strings.Contains(resp.Error, "the outcome is unknown"); said != tc.unknown {
				t.Fatalf("%s: error %q says the outcome is unknown: %v, want %v", tc.req.Op, resp.Error, said, tc.unknown)
			}
			if strings.Contains(resp.Error, "ALREADY_ENROLLED") {
				t.Fatalf("%s: error %q holds ALREADY_ENROLLED, which agentbox reads as a refusal", tc.req.Op, resp.Error)
			}
		})
	}
}

// heldSignIns is a person's sign-in whose Current waits until release is closed, whatever its
// context says: a lock that does not watch the op's context, which brokerclient.SignInAuthority's
// no longer is (ctxlock), so this row proves brokerCall's timer and not the sign-in. Then it
// answers no sign-in, so the enrolment goes on under the machine's own credential.
type heldSignIns struct{ release <-chan struct{} }

func (s *heldSignIns) Current(context.Context) (*brokerclient.Authority, error) {
	<-s.release
	return nil, nil
}

func (s *heldSignIns) Refused(context.Context, string) error { return nil }

// TestAnOpHeldOnALockAnswersUnknownAtTheBoundAndSendsNothingAfter: an enroll-session-key held on
// a lock that does not watch the op's context is answered by the timer, ENROLL_OUTCOME_UNKNOWN,
// at its bound plus 1 s on the server's clock, not left open. Once the lock is released the op
// goes on, but its context ended at the bound, so it sends the broker nothing: when the helper
// answers, no request of the op is in flight and none can start.
func TestAnOpHeldOnALockAnswersUnknownAtTheBoundAndSendsNothingAfter(t *testing.T) {
	var out syncBuffer
	clock := clocktest.New(time.Now())
	r := newLoggedRig(t, "", slog.New(slog.NewTextHandler(&out, nil)), func(s *Server) { s.Clock = clock })
	r.login(t)
	release, open := testGate(t)
	r.srv.Broker.SignIns = &heldSignIns{release: release}

	const what = "enroll-session-key held on a lock past its bound"
	replies := callInBackground(t, r.sock, Request{Op: "enroll-session-key", RuntimeID: "box-1", Thumbprint: "tp-1"})
	armBrokerCall(t, clock, replies, what)
	// The op's context ends at the bound, which the lock does not watch, so what answers is the
	// timer a second later.
	clock.Advance(BrokerCallBound + time.Second)
	resp := replyOf(t, replies, what)
	if resp.OK || resp.Code != CodeEnrollUnknown || !strings.Contains(resp.Error, "the outcome is unknown") {
		t.Fatalf("%s: %+v; want %s saying the outcome is unknown", what, resp, CodeEnrollUnknown)
	}
	if !strings.Contains(resp.Error, "did not end in") {
		t.Fatalf("%s: error %q; want the timer's answer, that the operation did not end in its bound", what, resp.Error)
	}

	open()
	// The op logs its result once it ends; by then any request it made has reached the fake.
	ended := `msg="a broker call answered as unknown has ended" op=enroll-session-key`
	waitFor(t, func() bool { return strings.Contains(out.String(), ended) })
	r.fake.mu.Lock()
	arrivals := r.fake.enrollArrivals
	r.fake.mu.Unlock()
	if arrivals != 0 {
		t.Fatalf("the fake received %d POST /v1/enrollments after the helper answered; want none (log: %s)", arrivals, out.Lines("broker call"))
	}
}

// TestALoginPastTheBoundEndsItsDeviceAuthorizationAndAnswersUnknown: the broker holds the device
// authorization until the request ends. The op's bound ends that request, which the broker sees,
// and login answers LOGIN_FAILED saying the outcome is unknown, with settle's cause, once the
// server's clock reaches the bound and while brokerCall's timer, a second later, has not fired.
func TestALoginPastTheBoundEndsItsDeviceAuthorizationAndAnswersUnknown(t *testing.T) {
	r := newRigOn(t, "")
	// A helper that left the request running would hold the fake's handler until its Close, which
	// waits for it; closing the connections first lets this row fail rather than hang.
	t.Cleanup(r.fake.srv.CloseClientConnections)
	r.fake.mu.Lock()
	r.fake.deviceAuthHold = true
	r.fake.mu.Unlock()

	const what = "login past its bound"
	replies := callInBackground(t, r.sock, Request{Op: "login"})
	armBrokerCall(t, r.clock, replies, what)
	// The device authorization reaches the broker before the bound ends it, so the transport holds
	// a connection for it when it ends, which settle reads as a request that may have reached the
	// broker.
	waitFor(t, func() bool {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.deviceAuthHeld == 1
	})
	r.clock.Advance(BrokerCallBound)
	wantSettled(t, what, replyOf(t, replies, what), CodeLoginFailed)
	ended := func() bool {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.deviceAuthEnded == 1
	}
	waitUpTo(t, ended, "the broker never saw the device authorization's request end; the bound must end it")
}

// TestAStoppingHelperAnswersItsBrokerCallBeforeServeReturns: the helper stops (the Serve context
// ends) while an enroll-session-key waits on the broker. The op's context ends with it, the client
// gets ENROLL_OUTCOME_UNKNOWN rather than an empty reply, and Serve returns only once that answer
// is written, so a helper process that exits when Serve returns never cuts the answer off.
func TestAStoppingHelperAnswersItsBrokerCallBeforeServeReturns(t *testing.T) {
	r := startRig(t, "")
	gate, open := testGate(t)
	r.fake.mu.Lock()
	r.fake.enrollGate = gate
	r.fake.mu.Unlock()

	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: r.sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	line, err := json.Marshal(Request{Op: "enroll-session-key", RuntimeID: "box-1", Thumbprint: "tp-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.enrollArrivals == 1
	})

	r.cancel()
	testwait.Receive(t, "Serve to return once its context ended", r.served)
	// Serve has returned, so the answer must already be on the connection: read what is there now,
	// without waiting for more. A Serve that returned before the answer was written leaves nothing.
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, maxLine)
	var n int
	var readErr error
	if err := raw.Read(func(fd uintptr) bool {
		n, readErr = syscall.Read(int(fd), buf)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if n <= 0 {
		t.Fatalf("Serve returned before the helper answered: nothing was on the connection (%d, %v)", n, readErr)
	}
	// The helper closed the connection before Serve returned, so the rest, if any, ends at once.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read the rest of the answer: %v", err)
	}
	answer := append(buf[:n], rest...)
	var resp Response
	if err := json.Unmarshal(bytes.TrimSpace(answer), &resp); err != nil {
		t.Fatalf("the answer %q is not one JSON object: %v", answer, err)
	}
	if resp.OK || resp.Code != CodeEnrollUnknown || !strings.Contains(resp.Error, "the outcome is unknown") {
		t.Fatalf("enroll-session-key cut off by the stop: %+v; want %s saying the outcome is unknown", resp, CodeEnrollUnknown)
	}
	open()
}

// TestARegisterWaitingForItsEnrollmentDoesNotHoldTheStop: the stop waits for the broker-call
// ops alone. A register whose wait_seconds is still running when the Serve context ends gives its
// place in that wait back once its request is read (handle's release), because its wait does not
// watch the context: the broker holds the session's enrollment, so the register would otherwise
// hold Serve until its whole wait ran out. That wait runs on the server's clock, which the row
// never moves, so it ends only at the row's cleanup: a Serve that waited for it would not return.
func TestARegisterWaitingForItsEnrollmentDoesNotHoldTheStop(t *testing.T) {
	const wait = 6 * time.Second
	r := newRigOn(t, "")
	r.login(t)
	gate, _ := testGate(t) // its cleanup opens the gate; the rig's cleanup ends the register's wait
	r.fake.mu.Lock()
	r.fake.enrollGate = gate
	r.fake.mu.Unlock()
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))

	callInBackground(t, r.sock, Request{Op: "register", WaitSeconds: int(wait / time.Second)})
	// The broker holds the session's enrollment, so the register is in its wait.
	waitFor(t, func() bool {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.enrollArrivals >= 1
	})

	stopped := time.Now()
	r.cancel()
	testwait.Receive(t, fmt.Sprintf("Serve to return with a register still in its %s wait", wait), r.served)
	t.Logf("Serve returned %s after its context ended, with a register still in its %s wait", time.Since(stopped), wait)
}

// TestUnregisterAnswersBeforeItsRevokeEnds: unregister answers with what it decides, the session
// retired, while the broker still holds the revoke of the session's enrollment; the revoke goes
// on, and the DELETE of that enrollment lands once the broker lets it.
func TestUnregisterAnswersBeforeItsRevokeEnds(t *testing.T) {
	r := startRig(t, "")
	gate, open := testGate(t)
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	reg := r.call(t, Request{Op: "register", WaitSeconds: 5})
	if !reg.OK || reg.State != "enrolled" || reg.EnrollmentID == "" {
		t.Fatalf("register: %+v", reg)
	}
	r.fake.mu.Lock()
	r.fake.revokeGate = gate
	r.fake.mu.Unlock()

	// The DELETE is held until open, so an answer here is an answer before the revoke ends.
	un := r.call(t, Request{Op: "unregister"})
	if !un.OK {
		t.Fatalf("unregister with its revoke held: %+v", un)
	}
	// The revoke unregister fired off reaches the broker and waits there, after the answer.
	held := func() bool {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.revokeAttempts == 1
	}
	waitUpTo(t, held, "unregister answered, but its revoke never reached the broker")
	if _, deletes, _ := r.fake.snapshot(); len(deletes) != 0 {
		t.Fatalf("a DELETE landed while the fake held every DELETE: %v", deletes)
	}

	open()
	revoked := func() bool {
		_, deletes, _ := r.fake.snapshot()
		return len(deletes) == 1
	}
	testwait.Eventually(t, "the revoke unregister fired off to land once the broker let it", revoked)
	if _, deletes, _ := r.fake.snapshot(); len(deletes) != 1 || deletes[0] != reg.EnrollmentID {
		t.Fatalf("the revoke after unregister deleted %v once the broker let it; want %s", deletes, reg.EnrollmentID)
	}
}
