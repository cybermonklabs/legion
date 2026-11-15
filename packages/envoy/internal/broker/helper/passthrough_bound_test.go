// packages/envoy/internal/broker/helper/passthrough_bound_test.go
//go:build linux

package helper

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// SetBrokerCallBound, replyWriteSlack, overBound and clientCallWait adapt the tester's red test
// of this issue (passthrough_signin_test.go, which is kept as it was written) to main's bound
// (#4037, Server.brokerCall), and this file's table and passthrough_refresh_test.go read them
// too. They are test code only: production has main's bound alone, Server.BrokerCallBound,
// DefaultBrokerCallBound and BrokerCallWait.

// SetBrokerCallBound sets main's bound, Server.BrokerCallBound, and nothing else.
func (s *Server) SetBrokerCallBound(d time.Duration) { s.BrokerCallBound = d }

// replyWriteSlack is how long past the bound the connection of a broker-call op stays open:
// brokerCall sets its deadline to the bound plus 5 s, and answers by the bound plus 1 s.
const replyWriteSlack = 5 * time.Second

// clientCallWait is the wait every client of a broker-call op gives one call, BrokerCallWait;
// every row reads its reply with it, as those clients do.
const clientCallWait = BrokerCallWait

// overBound is the bound the rows that run out of time give their own Server, and
// pastOverBound how long their broker then takes: a row that proves the bound answers need not
// wait the real 20 s to prove it, since what it reads is the answer at the bound and not the
// number. Six times the bound is far enough past it that a loaded box still answers at the
// bound first, and short enough that the fake's own call — which goes on until its delay is
// up, since a cancelled request reaches the handler only when the connection breaks — does not
// hold the test up at the end of the row.
const (
	overBound     = 500 * time.Millisecond
	pastOverBound = 3 * time.Second
)

// slowAnswer is how long the broker takes to answer the rows that must still be answered: 8 s,
// well past the 5 s deadline handle puts on every connection, and well inside
// DefaultBrokerCallBound. These rows run against the REAL bound, because the deadline they
// prove gone is the one those 8 s are measured against.
const slowAnswer = 8 * time.Second

// TestEachBrokerCallOpAnswersASlowBrokerOrSaysItsOutcomeIsUnknown drives main's bound (#4037,
// Server.brokerCall) on each broker-call op — enroll-session-key, unenroll-session-key and
// login — through the REAL helper socket against a broker that answers after a delay the row
// sets.
//
// Two rows for each op. The first is answered after 8 s and must reach the client as a JSON
// reply carrying the broker's own result: that is what the defect cost, a helper that made the
// enrollment and could not write its reply, leaving `agent-secrets enroll --helper` reading an
// empty one and exiting 1 (dispatch://INFRA-49/comment/9bf092b6). The second is answered later
// than the bound and must reach the client, never as an empty reply either, with that op's
// answer for an outcome the helper does not know: ENROLL_OUTCOME_UNKNOWN,
// UNENROLL_OUTCOME_UNKNOWN, or LOGIN_FAILED saying the outcome is unknown, since the broker may
// have acted on a request it was still answering.
//
// The named revert: put the 5 s connection deadline back over the broker-call ops (drop
// brokerCall's SetDeadline) and every 8 s row fails with its reply unread.
func TestEachBrokerCallOpAnswersASlowBrokerOrSaysItsOutcomeIsUnknown(t *testing.T) {
	const rowsToDrive = 6
	var driven atomic.Int64
	rows := []struct {
		op   string
		code string
		// request is this row's request, built against a rig whose broker still answers at
		// once: the unenroll row enrolls the key it then ends.
		request func(t *testing.T, r *rig) Request
		// answered reads the reply of the call the broker answered inside the bound, and holds
		// it to the result the broker gave.
		answered func(t *testing.T, r *rig, req Request, resp Response)
	}{
		{
			op:   "enroll-session-key",
			code: CodeEnrollUnknown,
			request: func(*testing.T, *rig) Request {
				return Request{Op: "enroll-session-key", RuntimeID: "rt-slow-box", Thumbprint: "tp-slow-box"}
			},
			answered: func(t *testing.T, r *rig, _ Request, resp Response) {
				t.Helper()
				if !resp.OK || resp.EnrollmentID == "" || resp.LeaseExpires == "" {
					t.Fatalf("enroll-session-key against an %s broker: %+v; want the broker's enrollment", slowAnswer, resp)
				}
				if _, err := time.Parse(time.RFC3339Nano, resp.LeaseExpires); err != nil {
					t.Fatalf("lease_expires %q: %v", resp.LeaseExpires, err)
				}
				posts, _, _ := r.fake.snapshot()
				if len(posts) != 1 || posts[0]["runtime_id"] != "rt-slow-box" {
					t.Fatalf("the broker was asked %v; want one enroll of rt-slow-box", posts)
				}
			},
		},
		{
			op:   "unenroll-session-key",
			code: CodeUnenrollUnknown,
			request: func(t *testing.T, r *rig) Request {
				t.Helper()
				enrolled := r.call(t, Request{Op: "enroll-session-key", RuntimeID: "rt-ended-box", Thumbprint: "tp-ended-box"})
				if !enrolled.OK || enrolled.EnrollmentID == "" {
					t.Fatalf("enroll the key this row ends: %+v", enrolled)
				}
				return Request{Op: "unenroll-session-key", EnrollmentID: enrolled.EnrollmentID}
			},
			answered: func(t *testing.T, r *rig, req Request, resp Response) {
				t.Helper()
				if !resp.OK {
					t.Fatalf("unenroll-session-key against an %s broker: %+v; want the broker's 204", slowAnswer, resp)
				}
				_, deletes, _ := r.fake.snapshot()
				if len(deletes) != 1 || deletes[0] != req.EnrollmentID {
					t.Fatalf("the broker was asked to delete %v; want %q", deletes, req.EnrollmentID)
				}
			},
		},
		{
			op:   "login",
			code: CodeLoginFailed,
			request: func(*testing.T, *rig) Request {
				return Request{Op: "login"}
			},
			answered: func(t *testing.T, r *rig, _ Request, resp Response) {
				t.Helper()
				r.fake.mu.Lock()
				code, uri := r.fake.pendingCode, r.fake.srv.URL
				r.fake.mu.Unlock()
				if !resp.OK || resp.Code != code || resp.VerificationURI != uri {
					t.Fatalf("login against an %s broker: %+v; want the broker's code %q and %q", slowAnswer, resp, code, uri)
				}
			},
		},
	}
	// The rows run side by side: each builds a rig of its own, and nearly all of their time is
	// spent waiting on a broker that is deliberately slow.
	t.Run("rows", func(t *testing.T) {
		for _, row := range rows {
			t.Run(row.op+" answers what a broker says after "+slowAnswer.String(), func(t *testing.T) {
				t.Parallel()
				r := startRig(t, "")
				req := row.request(t, r)
				r.fake.slowAnswers(slowAnswer)
				started := time.Now()
				resp, err := Call(r.sock, req, clientCallWait)
				waited := time.Since(started)
				if err != nil {
					t.Fatalf("%s against an %s broker: %v after %s; the client must read one JSON reply, never an empty one", row.op, slowAnswer, err, waited)
				}
				if waited < slowAnswer {
					t.Fatalf("%s answered after %s, before the broker did: the row proves nothing", row.op, waited)
				}
				row.answered(t, r, req, resp)
				driven.Add(1)
			})
			t.Run(row.op+" says its outcome is unknown past its bound", func(t *testing.T) {
				t.Parallel()
				r := startRig(t, "")
				r.srv.BrokerCallBound = overBound
				req := row.request(t, r)
				r.fake.slowAnswers(pastOverBound)
				started := time.Now()
				resp, err := Call(r.sock, req, clientCallWait)
				waited := time.Since(started)
				if err != nil {
					t.Fatalf("%s against a broker slower than its bound: %v after %s; the client must read one JSON reply, never an empty one", row.op, err, waited)
				}
				if resp.OK || resp.Code != row.code || !strings.Contains(resp.Error, "the outcome is unknown") {
					t.Fatalf("%s past its bound: %+v; want %s saying the outcome is unknown", row.op, resp, row.code)
				}
				if waited >= pastOverBound {
					t.Fatalf("%s past its bound answered after %s: it waited out the broker rather than its bound", row.op, waited)
				}
				driven.Add(1)
			})
		}
	})
	if driven.Load() < rowsToDrive {
		t.Fatalf("the table drove %d rows, want %d", driven.Load(), rowsToDrive)
	}
}
