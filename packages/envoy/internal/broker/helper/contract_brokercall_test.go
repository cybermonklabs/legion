// packages/envoy/internal/broker/helper/contract_brokercall_test.go
//go:build linux

package helper

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// The broker-call ops against a real broker that is slow to answer: the helper answers each with
// the broker's outcome, or says the outcome is unknown when the op runs past its bound, and the
// broker's own rows say what really happened. The slowness is the helper's transport holding the
// broker's answer after the broker gave it, so the broker has acted when the hold begins.

// holdingTransport sends each request through base and, for the next holds requests whose method
// is method and whose path is path, holds the broker's answer for hold before it hands it on: a
// broker that acted at once and is slow to say so. It watches the request's context while it
// holds, as a transport waiting on a slow answer does, so the op's bound ends the hold: the
// answer's body is closed and the context's error returned. held counts the answers it began to
// hold, so a row can tell that its hold happened.
type holdingTransport struct {
	base   http.RoundTripper
	method string
	path   string
	hold   time.Duration
	holds  atomic.Int64
	held   atomic.Int64
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
	resp, err := h.base.RoundTrip(req)
	if err != nil || !h.matches(req) || h.holds.Add(-1) < 0 {
		return resp, err
	}
	h.held.Add(1)
	timer := time.NewTimer(h.hold)
	defer timer.Stop()
	select {
	case <-timer.C:
		return resp, nil
	case <-req.Context().Done():
		resp.Body.Close()
		return nil, req.Context().Err()
	}
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

// freshThumbprint is the thumbprint of a new session key.
func freshThumbprint(t *testing.T) string {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return thumbprint
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
	rows, err := cr.broker.Store.Pool.Query(context.Background(),
		`select id::text, revoked_at is null and lease_expires_at > now() from enrollments where runtime_id = $1 order by created_at`, runtimeID)
	if err != nil {
		t.Fatalf("read the broker's enrollments of %s: %v", runtimeID, err)
	}
	defer rows.Close()
	var got []brokerEnrollment
	for rows.Next() {
		var e brokerEnrollment
		if err := rows.Scan(&e.ID, &e.Live); err != nil {
			t.Fatalf("read the broker's enrollments of %s: %v", runtimeID, err)
		}
		got = append(got, e)
	}
	if err := rows.Err(); err != nil {
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
	resp := callBroker(t, cr, Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: freshThumbprint(t)})
	rows := enrollmentsOf(t, cr, runtimeID)
	if n := held.held.Load(); n != 1 {
		t.Fatalf("the transport held %d answers of POST /v1/enrollments, want 1; answer %+v, the broker's rows %+v", n, resp, rows)
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
	enrolled := callBroker(t, cr, Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: freshThumbprint(t)})
	if !enrolled.OK || enrolled.EnrollmentID == "" {
		t.Fatalf("enroll-session-key: %+v", enrolled)
	}

	held.holds.Store(1)
	resp := callBroker(t, cr, Request{Op: "unenroll-session-key", EnrollmentID: enrolled.EnrollmentID})
	rows := enrollmentsOf(t, cr, runtimeID)
	if n := held.held.Load(); n != 1 {
		t.Fatalf("the transport held %d answers of DELETE /v1/enrollments/{id}, want 1; answer %+v, the broker's rows %+v", n, resp, rows)
	}
	if !resp.OK {
		t.Fatalf("unenroll-session-key against a broker 8 s slow to answer: %+v; want OK (the broker's rows %+v)", resp, rows)
	}
	if len(rows) != 1 || rows[0].ID != enrolled.EnrollmentID || rows[0].Live {
		t.Fatalf("the broker's rows for %s: %+v; want only %s, revoked", runtimeID, rows, enrolled.EnrollmentID)
	}
}

// TestContractAnEnrollPastTheBoundIsUnknownAndTheSameEnrollAgainAnswersItsRow: the broker makes
// the enrollment, and its answer would reach the helper 5 s later, past the op's 2 s bound. The
// bound ends the op, which answers ENROLL_OUTCOME_UNKNOWN, while the broker holds the row. The
// same enroll again (the same key and runtime id), answered at once, gets that row: the broker
// answers the same key under the same credential and runtime id with its live enrollment, and
// makes no second one.
func TestContractAnEnrollPastTheBoundIsUnknownAndTheSameEnrollAgainAnswersItsRow(t *testing.T) {
	held := &holdingTransport{base: http.DefaultTransport, method: http.MethodPost, path: "/v1/enrollments", hold: 5 * time.Second}
	cr := heldContractRig(t, held)
	cr.srv.BrokerCallBound = 2 * time.Second
	runtimeID := "past-bound-box-" + t.Name()
	req := Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: freshThumbprint(t)}

	held.holds.Store(1)
	first := callBroker(t, cr, req)
	rows := enrollmentsOf(t, cr, runtimeID)
	if n := held.held.Load(); n != 1 {
		t.Fatalf("the transport held %d answers of POST /v1/enrollments, want 1; answer %+v, the broker's rows %+v", n, first, rows)
	}
	if first.OK || first.Code != CodeEnrollUnknown || !strings.Contains(first.Error, "the outcome is unknown") {
		t.Fatalf("enroll-session-key past its bound: %+v; want %s saying the outcome is unknown (the broker's rows %+v)", first, CodeEnrollUnknown, rows)
	}
	live := liveIDs(rows)
	if len(live) != 1 {
		t.Fatalf("the broker's rows for %s after the enroll past the bound: %+v; want the one live row it made", runtimeID, rows)
	}

	again := callBroker(t, cr, req)
	rows = enrollmentsOf(t, cr, runtimeID)
	if !again.OK || again.EnrollmentID != live[0] {
		t.Fatalf("the same enroll again: %+v; want OK with %s, the row the first made (the broker's rows %+v)", again, live[0], rows)
	}
	if now := liveIDs(rows); len(now) != 1 || now[0] != live[0] {
		t.Fatalf("the broker's rows for %s after the same enroll again: %+v; want exactly one live row, %s", runtimeID, rows, live[0])
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
	first := callBroker(t, cr, Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: freshThumbprint(t)})
	if !first.OK || first.EnrollmentID == "" {
		t.Fatalf("enroll-session-key: %+v", first)
	}
	before := enrollmentsOf(t, cr, runtimeID)

	refused := callBroker(t, cr, Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: freshThumbprint(t)})
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
