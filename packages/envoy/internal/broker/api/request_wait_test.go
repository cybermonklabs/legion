// request_wait_test.go pins GET /v1/requests/{id}?wait=N: the broker holds a session's read of its
// own pending request for up to N seconds, 1 to 20, and answers the request's state as soon as it
// is no longer pending, whichever broker process decided it, or pending once N seconds pass. Any
// other wait is refused 400 REQUEST_WAIT_INPUT. It is what lets a waiting `agent-secrets NAME --
// command` learn of a decision within a second of it rather than at its next poll.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/store"
)

// readWaiting reads GET /v1/requests/{id}?<query> as the session. The proof signs the path alone,
// as every session proof does: the broker checks a proof's htu against BROKER_PUBLIC_URL and the
// request's path, never its query.
func (ts *testServer) readWaiting(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, requestID, query string) (int, []byte) {
	t.Helper()
	path := "/v1/requests/" + requestID
	p, err := proof.Sign(key, enrollmentID, http.MethodGet, ts.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	return ts.req(t, http.MethodGet, path+"?"+query, map[string]string{"Proof": p}, nil)
}

// otherBroker is a second broker process on ts's database, as the broker's second replica is: a
// store of its own (its own connection pool, on the same schema) and a requests.Machine of its
// own, so nothing it decides reaches ts's server except through the database. It reads the same
// policy, as every replica reads the one rules source.
func (ts *testServer) otherBroker(t *testing.T) *requests.Machine {
	t.Helper()
	st, err := store.Open(context.Background(), ts.Store.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open a second store on the test schema: %v", err)
	}
	t.Cleanup(st.Close)
	return &requests.Machine{
		Store: st, Policy: ts.machine.Policy, Secrets: ts.machine.Secrets, PendingTTL: ts.machine.PendingTTL,
		Audience: ts.URL, Skew: time.Minute,
	}
}

// TestReadRequestWaitsForTheDecision: a session's read with ?wait=10 of its pending request is
// held, and answers the decision once another broker process approves (or denies) the request a
// second in: within 1.0 to 1.6 s of the start, the decision's own second plus at most one 500 ms
// step at which the held read reads the database again, and the round trip.
func TestReadRequestWaitsForTheDecision(t *testing.T) {
	for _, row := range []struct {
		name    string
		approve bool
		state   string
	}{
		{name: "approved", approve: true, state: "granted"},
		{name: "denied", approve: false, state: "denied"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ts := newTestServer(t)
			enrollmentID, key := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
			asked := ts.ask(t, key, enrollmentID, "DEEL_API_KEY")
			if asked.State != "pending" || asked.RecordID == nil {
				t.Fatalf("POST /v1/requests = %+v, want a pending request with a record", asked)
			}
			other := ts.otherBroker(t)

			start := time.Now()
			decided := make(chan error, 1)
			go func() {
				time.Sleep(time.Second)
				_, err := other.ApplyDecision(context.Background(), *asked.RecordID, row.approve, testApprover)
				decided <- err
			}()
			status, body := ts.readWaiting(t, key, enrollmentID, asked.RequestID, "wait=10")
			took := time.Since(start)
			if err := <-decided; err != nil {
				t.Fatalf("the other broker's decision: %v", err)
			}
			if status != http.StatusOK {
				t.Fatalf("GET /v1/requests/{id}?wait=10 = %d: %s", status, body)
			}
			read := decode[wireRequestStatus](t, body)
			if read.State != row.state || (row.state == "granted") != (read.GrantID != nil) {
				t.Fatalf("GET /v1/requests/{id}?wait=10 = %s, want state %q (a grant_id only once granted)", body, row.state)
			}
			if took < time.Second || took > 1600*time.Millisecond {
				t.Fatalf("GET /v1/requests/{id}?wait=10 answered %s after the start, want 1.0-1.6 s: held until the decision a second in, and no longer than one 500 ms step past it", took)
			}
		})
	}
}

// TestReadRequestWaitEndsPending: with no decision, a read with ?wait=2 answers pending once the
// two seconds have passed, within 2.0 to 2.6 s.
func TestReadRequestWaitEndsPending(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, key := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	asked := ts.ask(t, key, enrollmentID, "DEEL_API_KEY")
	if asked.State != "pending" {
		t.Fatalf("POST /v1/requests = %+v, want a pending request", asked)
	}
	start := time.Now()
	status, body := ts.readWaiting(t, key, enrollmentID, asked.RequestID, "wait=2")
	took := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/requests/{id}?wait=2 = %d: %s", status, body)
	}
	if read := decode[wireRequestStatus](t, body); read.State != "pending" {
		t.Fatalf("GET /v1/requests/{id}?wait=2 = %s, want pending: nobody decided it", body)
	}
	if took < 2*time.Second || took > 2600*time.Millisecond {
		t.Fatalf("GET /v1/requests/{id}?wait=2 answered after %s, want 2.0-2.6 s", took)
	}
}

// TestReadRequestWaitRefusesBadInput: a wait that is not a whole number of seconds from 1 to 20
// is refused 400 REQUEST_WAIT_INPUT, at once, for a request that is pending.
func TestReadRequestWaitRefusesBadInput(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, key := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	asked := ts.ask(t, key, enrollmentID, "DEEL_API_KEY")
	if asked.State != "pending" {
		t.Fatalf("POST /v1/requests = %+v, want a pending request", asked)
	}
	for _, wait := range []string{"21", "x", "0", "-1", "1.5"} {
		start := time.Now()
		status, body := ts.readWaiting(t, key, enrollmentID, asked.RequestID, "wait="+wait)
		took := time.Since(start)
		if status != http.StatusBadRequest || decode[wireError](t, body).Code != "REQUEST_WAIT_INPUT" {
			t.Errorf("GET /v1/requests/{id}?wait=%s = %d %s, want 400 REQUEST_WAIT_INPUT", wait, status, body)
		}
		if took >= time.Second {
			t.Errorf("GET /v1/requests/{id}?wait=%s answered after %s, want the refusal at once", wait, took)
		}
	}
}
