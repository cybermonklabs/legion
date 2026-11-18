// packages/envoy/internal/broker/helper/passthrough_bound_test.go
//go:build linux

package helper

import "testing"

// TestAnUnenrollPastItsBoundSaysItsOutcomeIsUnknown drives, through the REAL helper socket, the
// one case of a slow broker that contract_brokercall_test.go and brokercall_test.go leave out:
// those drive an enrollment and a revoke the broker answers after 8 s, and an enrollment and a
// login past their bound, and cmd/agent-secrets' TestMachineLoginPrintsItsCodeAgainstASlowBroker
// drives a login the broker answers after 12 s, inside the bound.
//
// An unenroll-session-key whose DELETE the broker holds past the op's bound (the fake's
// revokeGate) must reach the client, never as an empty reply, as UNENROLL_OUTCOME_UNKNOWN with
// settle's cause, once the server's clock reaches the bound and while brokerCall's timer, a second
// later, has not fired: the broker got the request, so it may have revoked the enrollment. The
// bound and the timer run on the server's fake clock (newRigOn), which the row moves to the bound
// alone, so the answer can be no one's but the op's.
func TestAnUnenrollPastItsBoundSaysItsOutcomeIsUnknown(t *testing.T) {
	r := newRigOn(t, "")
	r.login(t)
	enrolled := r.call(t, Request{Op: "enroll-session-key", RuntimeID: "rt-ended-box", Thumbprint: "tp-ended-box"})
	if !enrolled.OK || enrolled.EnrollmentID == "" {
		t.Fatalf("enroll the key this row ends: %+v", enrolled)
	}
	gate, _ := testGate(t)
	r.fake.mu.Lock()
	r.fake.revokeGate = gate
	r.fake.mu.Unlock()

	const what = "unenroll-session-key past its bound"
	replies := callInBackground(t, r.sock, Request{Op: "unenroll-session-key", EnrollmentID: enrolled.EnrollmentID})
	armBrokerCall(t, r.clock, replies, what)
	// The DELETE reaches the broker before the bound ends it, so the transport holds a connection
	// for it when it ends, which settle reads as a request that may have reached the broker.
	waitFor(t, func() bool {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.revokeAttempts == 1
	})
	r.clock.Advance(BrokerCallBound)
	resp := replyOf(t, replies, what)
	r.fake.mu.Lock()
	attempts := r.fake.revokeAttempts
	r.fake.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("the fake received %d DELETEs, want 1, held at its gate; answer %+v", attempts, resp)
	}
	wantSettled(t, what, resp, CodeUnenrollUnknown)
}
