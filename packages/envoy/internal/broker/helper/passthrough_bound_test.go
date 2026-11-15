// packages/envoy/internal/broker/helper/passthrough_bound_test.go
//go:build linux

package helper

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// overBound is the bound a row that runs an op past its bound gives its own Server: the row
// need not wait the real 20 s, since what it reads is the answer at the bound and not the number.
const overBound = 500 * time.Millisecond

// TestASlowLoginAnswersItsCodeAndAnUnenrollPastItsBoundSaysItsOutcomeIsUnknown drives, through
// the REAL helper socket, the two cases of a slow broker that contract_brokercall_test.go and
// brokercall_test.go leave out: those drive an enrollment and a revoke the broker answers after
// 8 s, and an enrollment and a login past their bound.
//
// A login the broker answers after 8 s must reach the client as a JSON reply carrying the
// broker's code. 8 s is past the 5 s deadline handle puts on every connection and inside the
// helper's 20 s bound (DefaultBrokerCallBound), so that row runs under the real bound. The
// broker answers the device authorization at once and the helper's transport holds the answer
// (holdingTransport), as contract_brokercall_test.go holds the enrollment's and the revoke's.
//
// An unenroll-session-key whose DELETE the broker holds past the op's bound (the fake's
// revokeGate) must reach the client, never as an empty reply, as UNENROLL_OUTCOME_UNKNOWN with
// settle's cause, before the bound plus 1 s where brokerCall's timer would answer: the broker got
// the request, so it may have revoked the enrollment.
//
// The named revert: put the 5 s connection deadline back over the broker-call ops (drop
// brokerCall's SetDeadline) and the login row reads an empty reply.
func TestASlowLoginAnswersItsCodeAndAnUnenrollPastItsBoundSaysItsOutcomeIsUnknown(t *testing.T) {
	const rowsToDrive = 2
	var driven atomic.Int64
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "login answers the code a broker gives after 8 s",
			run: func(t *testing.T) {
				const slowAnswer = 8 * time.Second
				r := newRig(t, "")
				client := *r.srv.Broker.HTTP
				held := &holdingTransport{base: client.Transport, method: http.MethodPost, path: "/v1/oauth/device_authorization", hold: slowAnswer}
				client.Transport = held
				r.srv.Broker.HTTP = &client

				held.holds.Store(1)
				started := time.Now()
				resp, err := Call(r.sock, Request{Op: "login"}, BrokerCallWait)
				waited := time.Since(started)
				if err != nil {
					t.Fatalf("login against a broker %s slow to answer: %v after %s; the client must read one JSON reply, never an empty one", slowAnswer, err, waited)
				}
				if n := held.held.Load(); n != 1 {
					t.Fatalf("the transport held %d answers of POST /v1/oauth/device_authorization, want 1; answer %+v after %s", n, resp, waited)
				}
				r.fake.mu.Lock()
				code, uri := r.fake.pendingCode, r.fake.srv.URL
				r.fake.mu.Unlock()
				if !resp.OK || resp.Code != code || resp.VerificationURI != uri {
					t.Fatalf("login against a broker %s slow to answer: %+v after %s; want the broker's code %q and %q", slowAnswer, resp, waited, code, uri)
				}
			},
		},
		{
			name: "unenroll-session-key past its bound says its outcome is unknown",
			run: func(t *testing.T) {
				r := startRig(t, "")
				enrolled := r.call(t, Request{Op: "enroll-session-key", RuntimeID: "rt-ended-box", Thumbprint: "tp-ended-box"})
				if !enrolled.OK || enrolled.EnrollmentID == "" {
					t.Fatalf("enroll the key this row ends: %+v", enrolled)
				}
				gate, _ := testGate(t)
				r.fake.mu.Lock()
				r.fake.revokeGate = gate
				r.fake.mu.Unlock()
				r.srv.BrokerCallBound = overBound

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
				wantSettled(t, "unenroll-session-key past its bound", resp, CodeUnenrollUnknown, waited, overBound+time.Second)
				if waited < overBound {
					t.Fatalf("unenroll-session-key past its bound answered after %s; want about the %s bound", waited, overBound)
				}
			},
		},
	}
	// The rows run side by side: each builds a rig of its own, and the login row spends its time
	// waiting on an answer the transport holds.
	t.Run("rows", func(t *testing.T) {
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				row.run(t)
				driven.Add(1)
			})
		}
	})
	if driven.Load() < rowsToDrive {
		t.Fatalf("the table drove %d rows, want %d", driven.Load(), rowsToDrive)
	}
}
