// packages/envoy/internal/broker/helper/contract_brokercall_test.go
//go:build linux

package helper

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The broker-call ops against a real broker that is slow to answer: the helper answers each with
// the broker's outcome, or says the outcome is unknown when the op runs past its bound, and the
// broker's own rows say what really happened. The slowness is the helper's transport holding the
// request from the moment it has it in hand, while the broker gets the whole request whatever the
// op's bound does, so the broker acts however slow a loaded machine makes it.

// holdingTransport sends each request through base and, for the next holds requests whose method
// is method and whose path is path, holds the request from the moment it has it in hand, for hold:
// a broker that acted at once and is slow to say so. A request whose context has already ended
// goes to base as it came, as a transport sends nothing on an ended context. A held request goes
// to base whole, under a context the op's does not end (context.WithoutCancel, which keeps the
// helper's trace), so the broker acts however slow a loaded machine makes it, even when its
// answer comes after the op's bound. The broker's answer is handed on once it is in hand and hold
// has passed. When the request's context ends first, as a transport waiting on a slow answer
// does, the answer's body is closed and the context's error returned, but only once the request
// is written, so the helper's trace has seen it leave. held counts the requests it began to hold,
// so a row can tell that its hold happened; answered counts the broker's answers to them, a
// failure among them, and answer is the last of those, for a row that must wait for the broker
// before it reads the broker's rows.
type holdingTransport struct {
	base     http.RoundTripper
	method   string
	path     string
	hold     time.Duration
	holds    atomic.Int64
	held     atomic.Int64
	answered atomic.Int64
	answer   atomic.Pointer[string]
}

// matches reports whether req is one whose answer this transport holds: a path that ends in "/"
// matches every path under it, any other path only itself.
func (h *holdingTransport) matches(req *http.Request) bool {
	if req.Method != h.method {
		return false
	}
	if strings.HasSuffix(h.path, "/") {
		return strings.HasPrefix(req.URL.Path, h.path)
	}
	return req.URL.Path == h.path
}

func (h *holdingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !h.matches(req) || req.Context().Err() != nil || h.holds.Add(-1) < 0 {
		return h.base.RoundTrip(req)
	}
	h.held.Add(1)
	hold := time.NewTimer(h.hold)
	defer hold.Stop()
	wrote := make(chan struct{})
	var once sync.Once
	sent := httptrace.WithClientTrace(context.WithoutCancel(req.Context()), &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { once.Do(func() { close(wrote) }) },
	})
	type result struct {
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := h.base.RoundTrip(req.WithContext(sent))
		answer := fmt.Sprint(err)
		if err == nil {
			answer = resp.Status
		}
		h.answer.Store(&answer)
		h.answered.Add(1)
		got <- result{resp, err}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			return nil, r.err
		}
		select {
		case <-hold.C:
		case <-req.Context().Done():
		}
		if req.Context().Err() == nil {
			return r.resp, nil
		}
		r.resp.Body.Close()
		return nil, req.Context().Err()
	case <-req.Context().Done():
	}
	select {
	case <-wrote:
		go func() {
			if r := <-got; r.err == nil {
				r.resp.Body.Close()
			}
		}()
	case r := <-got:
		if r.err == nil {
			r.resp.Body.Close()
		}
	}
	return nil, req.Context().Err()
}

// lastAnswer is the last of the broker's answers to a held request, its status or its failure,
// or "none yet".
func (h *holdingTransport) lastAnswer() string {
	if answer := h.answer.Load(); answer != nil {
		return *answer
	}
	return "none yet"
}

// lateAnswer hands on the broker's answer to the next lates requests whose method is method and
// whose path is path only after more past the moment the broker gave it: a broker that a loaded
// machine makes slow, whose answer comes after any bound shorter than after. As any transport
// does, it watches the request's context while it waits, and returns the context's error rather
// than an answer that came after the context's deadline.
type lateAnswer struct {
	base   http.RoundTripper
	method string
	path   string
	after  time.Duration
	lates  atomic.Int64
}

func (l *lateAnswer) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := l.base.RoundTrip(req)
	if err != nil || req.Method != l.method || req.URL.Path != l.path || l.lates.Add(-1) < 0 {
		return resp, err
	}
	timer := time.NewTimer(l.after)
	defer timer.Stop()
	select {
	case <-req.Context().Done():
	case <-timer.C:
		if deadline, ok := req.Context().Deadline(); !ok || time.Now().Before(deadline) {
			return resp, nil
		}
		<-req.Context().Done()
	}
	resp.Body.Close()
	return nil, req.Context().Err()
}

// heldContractRig is a logged-in contract rig whose helper sends its broker calls through held.
func heldContractRig(t *testing.T, held *holdingTransport) *contractRig {
	t.Helper()
	cr := startContractHelper(t, brokertest.NewRig(t), filepath.Join(t.TempDir(), "agent-secrets", "machine.json"), &http.Client{Transport: held})
	cr.login(t)
	return cr
}

// callBroker asks cr's helper req and waits for the answer as long as a client of a broker-call
// op does.
func callBroker(t *testing.T, cr *contractRig, req Request) Response {
	t.Helper()
	resp, err := Call(cr.sock, req, BrokerCallWait)
	if err != nil {
		t.Fatalf("%s: %v", req.Op, err)
	}
	return resp
}

// brokerEnrollment is one row of the broker's enrollments table: its id, and whether it is live
// (not revoked and its lease not passed).
type brokerEnrollment struct {
	ID   string
	Live bool
}

// enrollmentsOf reads the broker's enrollments of runtimeID, oldest first.
func enrollmentsOf(t *testing.T, cr *contractRig, runtimeID string) []brokerEnrollment {
	t.Helper()
	// Query's error comes back from the rows, and CollectRows returns it (pgx's Query doc allows this).
	rows, _ := cr.broker.Store.Pool.Query(context.Background(),
		`select id::text, revoked_at is null and lease_expires_at > now() from enrollments where runtime_id = $1 order by created_at`, runtimeID)
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[brokerEnrollment])
	if err != nil {
		t.Fatalf("read the broker's enrollments of %s: %v", runtimeID, err)
	}
	return got
}

// liveIDs are the ids of the live rows among rows.
func liveIDs(rows []brokerEnrollment) []string {
	var ids []string
	for _, e := range rows {
		if e.Live {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// TestContractASlowBrokersEnrollmentAnswersWithItsID: the broker makes the enrollment at once and
// its answer reaches the helper 8 s later, past the 5 s a helper once gave every op. The client
// that waits BrokerCallWait gets OK with the enrollment's id, and the broker holds exactly that
// one live enrollment for the runtime id.
func TestContractASlowBrokersEnrollmentAnswersWithItsID(t *testing.T) {
	held := &holdingTransport{base: http.DefaultTransport, method: http.MethodPost, path: "/v1/enrollments", hold: 8 * time.Second}
	cr := heldContractRig(t, held)
	runtimeID := "slow-box-" + t.Name()

	held.holds.Store(1)
	resp := callBroker(t, cr, Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: sessionThumbprint(t)})
	rows := enrollmentsOf(t, cr, runtimeID)
	if n := held.held.Load(); n != 1 {
		t.Fatalf("the transport held %d requests of POST /v1/enrollments, want 1; answer %+v, the broker's rows %+v", n, resp, rows)
	}
	if !resp.OK || resp.EnrollmentID == "" {
		t.Fatalf("enroll-session-key against a broker 8 s slow to answer: %+v; want OK with an id (the broker's rows %+v)", resp, rows)
	}
	if live := liveIDs(rows); len(live) != 1 || live[0] != resp.EnrollmentID {
		t.Fatalf("the broker's rows for %s: %+v; want exactly one live row, %s", runtimeID, rows, resp.EnrollmentID)
	}
}

// TestContractASlowBrokersRevokeAnswersOK: the broker revokes the enrollment at once and its
// answer reaches the helper 8 s later. unenroll-session-key answers OK, and the broker's row is
// revoked.
func TestContractASlowBrokersRevokeAnswersOK(t *testing.T) {
	held := &holdingTransport{base: http.DefaultTransport, method: http.MethodDelete, path: "/v1/enrollments/", hold: 8 * time.Second}
	cr := heldContractRig(t, held)
	runtimeID := "slow-revoke-box-" + t.Name()
	enrolled := callBroker(t, cr, Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: sessionThumbprint(t)})
	if !enrolled.OK || enrolled.EnrollmentID == "" {
		t.Fatalf("enroll-session-key: %+v", enrolled)
	}

	held.holds.Store(1)
	resp := callBroker(t, cr, Request{Op: "unenroll-session-key", EnrollmentID: enrolled.EnrollmentID})
	rows := enrollmentsOf(t, cr, runtimeID)
	if n := held.held.Load(); n != 1 {
		t.Fatalf("the transport held %d requests of DELETE /v1/enrollments/{id}, want 1; answer %+v, the broker's rows %+v", n, resp, rows)
	}
	if !resp.OK {
		t.Fatalf("unenroll-session-key against a broker 8 s slow to answer: %+v; want OK (the broker's rows %+v)", resp, rows)
	}
	if len(rows) != 1 || rows[0].ID != enrolled.EnrollmentID || rows[0].Live {
		t.Fatalf("the broker's rows for %s: %+v; want only %s, revoked", runtimeID, rows, enrolled.EnrollmentID)
	}
}

// TestContractAnEnrollPastTheBoundIsUnknownAndTheSameEnrollAgainAnswersItsRow: the broker makes
// the enrollment, and its answer would reach the helper 5 s after the transport has the request in
// hand, past the op's 2 s bound. The broker answers at once, or, as a loaded machine can make it,
// only after the bound and brokerCall's timer. The bound ends the op, which answers
// ENROLL_OUTCOME_UNKNOWN with settle's cause, before the bound plus 1 s where brokerCall's timer
// would answer, while the broker holds the row. The bound ends the helper's request and not the
// broker's (holdingTransport), so the broker's rows are read once the broker has answered, however
// late. The same enroll again (the same key and runtime id), answered at once, gets that row: the
// broker answers the same key under the same credential and runtime id with its live enrollment,
// and makes no second one.
func TestContractAnEnrollPastTheBoundIsUnknownAndTheSameEnrollAgainAnswersItsRow(t *testing.T) {
	const bound = 2 * time.Second
	for _, tc := range []struct {
		name string
		// late is how long after it made the enrollment the broker answers, through lateAnswer:
		// none, or past the bound and brokerCall's timer.
		late time.Duration
	}{
		{name: "the broker answers at once"},
		{name: "the broker answers after the bound", late: bound + time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			late := &lateAnswer{base: http.DefaultTransport, method: http.MethodPost, path: "/v1/enrollments", after: tc.late}
			held := &holdingTransport{base: late, method: http.MethodPost, path: "/v1/enrollments", hold: 5 * time.Second}
			cr := heldContractRig(t, held)
			cr.srv.BrokerCallBound = bound
			runtimeID := "past-bound-box-" + t.Name()
			req := Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: sessionThumbprint(t)}

			late.lates.Store(1)
			held.holds.Store(1)
			start := time.Now()
			first := callBroker(t, cr, req)
			elapsed := time.Since(start)
			if n := held.held.Load(); n != 1 {
				t.Fatalf("the transport held %d requests of POST /v1/enrollments, want 1; answer %+v, the broker's answer %s, the broker's rows %+v", n, first, held.lastAnswer(), enrollmentsOf(t, cr, runtimeID))
			}
			if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), testwait.Poll, func() bool { return held.answered.Load() == 1 }) {
				t.Fatalf("the broker never answered the held POST /v1/enrollments; answer %+v, the broker's rows %+v", first, enrollmentsOf(t, cr, runtimeID))
			}
			rows := enrollmentsOf(t, cr, runtimeID)
			wantSettled(t, fmt.Sprintf("enroll-session-key past its bound (the broker's answer %s, its rows %+v)", held.lastAnswer(), rows), first, CodeEnrollUnknown, elapsed, bound+time.Second)
			live := liveIDs(rows)
			if len(live) != 1 {
				t.Fatalf("the broker's rows for %s after the enroll past the bound: %+v; want the one live row it made (the broker's answer %s)", runtimeID, rows, held.lastAnswer())
			}

			again := callBroker(t, cr, req)
			rows = enrollmentsOf(t, cr, runtimeID)
			if !again.OK || again.EnrollmentID != live[0] {
				t.Fatalf("the same enroll again: %+v; want OK with %s, the row the first made (the broker's rows %+v)", again, live[0], rows)
			}
			if now := liveIDs(rows); len(now) != 1 || now[0] != live[0] {
				t.Fatalf("the broker's rows for %s after the same enroll again: %+v; want exactly one live row, %s", runtimeID, rows, live[0])
			}
		})
	}
}

// TestContractAnEnrollTheBrokerRefusesIsAKnownFailureThatAddsNoRow: a second, different key under
// a runtime id that already has a live enrollment is refused by the broker (409), a refusal it
// decides before it writes anything. The helper answers ENROLL_FAILED, not unknown, and the
// broker holds no new row.
func TestContractAnEnrollTheBrokerRefusesIsAKnownFailureThatAddsNoRow(t *testing.T) {
	cr := newContractRig(t)
	cr.login(t)
	runtimeID := "refused-box-" + t.Name()
	first := callBroker(t, cr, Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: sessionThumbprint(t)})
	if !first.OK || first.EnrollmentID == "" {
		t.Fatalf("enroll-session-key: %+v", first)
	}
	before := enrollmentsOf(t, cr, runtimeID)

	refused := callBroker(t, cr, Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: sessionThumbprint(t)})
	after := enrollmentsOf(t, cr, runtimeID)
	if refused.OK || refused.Code != CodeEnrollFailed {
		t.Fatalf("a second key under a live runtime id: %+v; want %s (the broker's rows %+v)", refused, CodeEnrollFailed, after)
	}
	if len(after) != len(before) {
		t.Fatalf("the broker's rows for %s: %+v before the refused enroll, %+v after; want no new row", runtimeID, before, after)
	}
	if live := liveIDs(after); len(live) != 1 || live[0] != first.EnrollmentID {
		t.Fatalf("the broker's rows for %s after the refused enroll: %+v; want only %s live", runtimeID, after, first.EnrollmentID)
	}
}
