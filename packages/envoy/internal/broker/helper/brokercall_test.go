// packages/envoy/internal/broker/helper/brokercall_test.go
//go:build linux

package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The broker-call ops (login, enroll-session-key, unenroll-session-key) against server_test.go's
// fake broker: which failures are known and which unknown, the bound and its timer, the stop, and
// unregister, whose revoke goes on after its answer.

// TestAFailedRequestIsKnownOrUnknownByWhetherTheBrokerGotIt: a broker request that failed after
// the broker got it (a 503, which a front door can send after the broker acted) has an unknown
// outcome, and one that never reached the broker (a refused dial) is a known failure. Each case
// answers one code; each unknown answer says that the outcome is unknown, and no known one does.
func TestAFailedRequestIsKnownOrUnknownByWhetherTheBrokerGotIt(t *testing.T) {
	enroll := Request{Op: "enroll-session-key", RuntimeID: "box-1", Thumbprint: "tp-1"}
	unenroll := Request{Op: "unenroll-session-key", EnrollmentID: "enr-1"}
	login := Request{Op: "login"}
	for _, tc := range []struct {
		name string
		req  Request
		// refuse points the helper at a broker URL that refuses the dial.
		refuse bool
		// fail sets the fake's 503, under its lock.
		fail    func(f *fakeBroker)
		code    string
		unknown bool
	}{
		{name: "a 503 on the enrollment is unknown", req: enroll, fail: func(f *fakeBroker) { f.failFirst = 1 }, code: CodeEnrollUnknown, unknown: true},
		{name: "a refused dial on the enrollment is a known failure", req: enroll, refuse: true, code: CodeEnrollFailed},
		{name: "a 503 on the revoke is unknown", req: unenroll, fail: func(f *fakeBroker) { f.revokeFailFirst = 1 }, code: CodeUnenrollUnknown, unknown: true},
		{name: "a refused dial on the revoke is a known failure", req: unenroll, refuse: true, code: CodeUnenrollFailed},
		{name: "a 503 on the device authorization is unknown", req: login, fail: func(f *fakeBroker) { f.deviceAuthFail = 1 }, code: CodeLoginFailed, unknown: true},
		{name: "a refused dial on the device authorization is a known failure", req: login, refuse: true, code: CodeLoginFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "")
			if tc.req.Op != "login" {
				r.login(t)
			}
			if tc.refuse {
				r.srv.Broker.URL = "http://127.0.0.1:1"
			}
			if tc.fail != nil {
				r.fake.mu.Lock()
				tc.fail(r.fake)
				r.fake.mu.Unlock()
			}
			resp, err := Call(r.sock, tc.req, BrokerCallWait)
			if err != nil {
				t.Fatalf("%s: %v", tc.req.Op, err)
			}
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
// context says, as a sign-in's trade does while it holds its lock across an HTTP request; then it
// answers no sign-in, so the enrolment goes on under the machine's own credential.
type heldSignIns struct{ release <-chan struct{} }

func (s *heldSignIns) Current(context.Context) (*brokerclient.Authority, error) {
	<-s.release
	return nil, nil
}

func (s *heldSignIns) Refused(string) {}

// TestAnOpHeldOnALockAnswersUnknownAtTheBoundAndSendsNothingAfter: an enroll-session-key held on
// a lock that does not watch the op's context is answered by the timer, ENROLL_OUTCOME_UNKNOWN,
// at its bound plus 1 s, not left open. Once the lock is released the op goes on, but its context
// ended at the bound, so it sends the broker nothing: when the helper answers, no request of the
// op is in flight and none can start.
func TestAnOpHeldOnALockAnswersUnknownAtTheBoundAndSendsNothingAfter(t *testing.T) {
	var out syncBuffer
	r := newLoggedRig(t, "", slog.New(slog.NewTextHandler(&out, nil)))
	r.login(t)
	release, open := testGate(t)
	r.srv.Broker.SignIns = &heldSignIns{release: release}
	const bound = time.Second
	r.srv.BrokerCallBound = bound

	start := time.Now()
	resp, err := Call(r.sock, Request{Op: "enroll-session-key", RuntimeID: "box-1", Thumbprint: "tp-1"}, BrokerCallWait)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("enroll-session-key: %v", err)
	}
	if resp.OK || resp.Code != CodeEnrollUnknown || !strings.Contains(resp.Error, "the outcome is unknown") {
		t.Fatalf("enroll-session-key held on a lock past its bound: %+v; want %s saying the outcome is unknown", resp, CodeEnrollUnknown)
	}
	if elapsed < bound || elapsed >= 4*time.Second {
		t.Fatalf("enroll-session-key held on a lock answered after %s; want the timer's answer, at the %s bound plus 1 s", elapsed, bound)
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
// and login answers LOGIN_FAILED saying the outcome is unknown, at about the bound.
func TestALoginPastTheBoundEndsItsDeviceAuthorizationAndAnswersUnknown(t *testing.T) {
	r := newRig(t, "")
	// A helper that left the request running would hold the fake's handler until its Close, which
	// waits for it; closing the connections first lets this row fail rather than hang.
	t.Cleanup(r.fake.srv.CloseClientConnections)
	r.fake.mu.Lock()
	r.fake.deviceAuthHold = true
	r.fake.mu.Unlock()
	const bound = time.Second
	r.srv.BrokerCallBound = bound

	start := time.Now()
	resp, err := Call(r.sock, Request{Op: "login"}, BrokerCallWait)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if resp.OK || resp.Code != CodeLoginFailed || !strings.Contains(resp.Error, "the outcome is unknown") {
		t.Fatalf("login past its bound: %+v; want %s saying the outcome is unknown", resp, CodeLoginFailed)
	}
	if elapsed < bound || elapsed >= 4*time.Second {
		t.Fatalf("login past its bound answered after %s; want about the %s bound", elapsed, bound)
	}
	ended := func() bool {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.deviceAuthEnded == 1
	}
	waitUpTo(t, 5*time.Second, ended, "the broker never saw the device authorization's request end; the bound must end it")
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
	select {
	case <-r.served:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return within 10 s of its context ending")
	}
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
	waitUpTo(t, 5*time.Second, held, "unregister answered, but its revoke never reached the broker")
	if _, deletes, _ := r.fake.snapshot(); len(deletes) != 0 {
		t.Fatalf("a DELETE landed while the fake held every DELETE: %v", deletes)
	}

	open()
	revoked := func() bool {
		_, deletes, _ := r.fake.snapshot()
		return len(deletes) == 1
	}
	testwait.Holds(5*time.Second, testwait.Poll, revoked)
	if _, deletes, _ := r.fake.snapshot(); len(deletes) != 1 || deletes[0] != reg.EnrollmentID {
		t.Fatalf("the revoke after unregister deleted %v once the broker let it; want %s", deletes, reg.EnrollmentID)
	}
}
