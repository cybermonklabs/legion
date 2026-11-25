// packages/envoy/internal/cipher/helper/passthrough_bound_test.go
//go:build linux

package helper

import (
	"testing"
	"time"
)

// overBound is the bound a test that runs an op past its bound gives its own Server: the test
// need not wait the real 20 s, since what it reads is the answer at the bound and not the number.
const overBound = 500 * time.Millisecond

// TestAnUnenrollPastItsBoundSaysItsOutcomeIsUnknown drives, through the REAL helper socket, the
// one case of a slow broker that contract_brokercall_test.go and brokercall_test.go leave out:
// those drive an enrollment and a revoke the broker answers after 8 s, and an enrollment and a
// login past their bound, and cmd/cipher's TestMachineLoginPrintsItsCodeAgainstASlowBroker
// drives a login the broker answers after 12 s, inside the bound.
//
// An unenroll-session-key whose DELETE the broker holds past the op's bound (the fake's
// revokeGate) must reach the client, never as an empty reply, as UNENROLL_OUTCOME_UNKNOWN with
// settle's cause, before brokerCall's timer would answer (slowTimer): the broker got the request,
// so it may have revoked the enrollment.
func TestAnUnenrollPastItsBoundSaysItsOutcomeIsUnknown(t *testing.T) {
	t.Parallel()
	r := startRig(t, "")
	enrolled := r.call(t, Request{Op: "enroll-session-key", RuntimeID: "rt-ended-box", Thumbprint: "tp-ended-box"})
	if !enrolled.OK || enrolled.EnrollmentID == "" {
		t.Fatalf("enroll the key this row ends: %+v", enrolled)
	}
	gate, _ := testGate(t)
	r.fake.mu.Lock()
	r.fake.revokeGate = gate
	r.fake.mu.Unlock()
	slowTimer(r.srv, overBound)

	started := time.Now()
	resp, err := Call(r.sock, Request{Op: "unenroll-session-key", EnrollmentID: enrolled.EnrollmentID}, BrokerCallWait)
	waited := time.Since(started)
	if err != nil {
		t.Fatalf("unenroll-session-key held past its bound: %v after %s; the client must read one JSON reply, never an empty one", err, waited)
	}
	r.fake.mu.Lock()
	attempts := r.fake.revokeAttempts
	r.fake.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("the fake received %d DELETEs, want 1, held at its gate; answer %+v after %s", attempts, resp, waited)
	}
	wantSettled(t, "unenroll-session-key past its bound", resp, CodeUnenrollUnknown, waited, overBound+answerMargin)
	if waited < overBound {
		t.Fatalf("unenroll-session-key past its bound answered after %s; want about the %s bound", waited, overBound)
	}
}
