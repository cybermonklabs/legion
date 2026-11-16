// request_wait_test.go pins GET /v1/requests/{id}?wait=N: the broker holds a session's read of its
// own pending request for up to N seconds, 1 to record.MaxRequestWaitSeconds, and answers the
// request's state as soon as it is no longer pending, whichever broker process decided it, or
// pending once N seconds pass. Any other wait is refused 400 REQUEST_WAIT_INPUT. It is what lets a
// waiting `agent-secrets NAME -- command` learn of a decision within a second of it rather than at
// its next poll.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

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

// pendingRequest is a pending request of a new session's, which a person must approve.
func (ts *testServer) pendingRequest(t *testing.T) (enrollmentID string, key *ecdsa.PrivateKey, asked approverAnswer) {
	t.Helper()
	enrollmentID, key = ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	asked = ts.ask(t, key, enrollmentID, "DEEL_API_KEY")
	if asked.State != "pending" || asked.RecordID == nil {
		t.Fatalf("POST /v1/requests = %+v, want a pending request with a record", asked)
	}
	return enrollmentID, key, asked
}

// after runs change a second from now, as another process would, and answers when it finished
// and how.
func after(change func() error) <-chan changed {
	done := make(chan changed, 1)
	go func() {
		time.Sleep(time.Second)
		err := change()
		done <- changed{at: time.Now(), err: err}
	}()
	return done
}

// changed is when after's change returned, and its error.
type changed struct {
	at  time.Time
	err error
}

// TestReadRequestWaitsForTheDecision: a session's read with ?wait=10 of its pending request is
// held, and answers the decision another broker process makes a second in (approve or deny) less
// than a second after that decision returned (R1: a waiting command learns of a decision within
// one second of it). A read that was not held would answer pending at once.
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
			enrollmentID, key, asked := ts.pendingRequest(t)
			other := ts.otherBroker(t)

			decided := after(func() error {
				_, err := other.ApplyDecision(context.Background(), *asked.RecordID, row.approve, testApprover)
				return err
			})
			status, body := ts.session(t, key, enrollmentID, http.MethodGet, "/v1/requests/"+asked.RequestID+"?wait=10", nil)
			answered := time.Now()
			decision := <-decided
			if decision.err != nil {
				t.Fatalf("the other broker's decision: %v", decision.err)
			}
			if status != http.StatusOK {
				t.Fatalf("GET /v1/requests/{id}?wait=10 = %d: %s", status, body)
			}
			read := decode[wireRequestStatus](t, body)
			if read.State != row.state || (row.state == "granted") != (read.GrantID != nil) {
				t.Fatalf("GET /v1/requests/{id}?wait=10 = %s, want state %q (a grant_id only once granted): held until the decision", body, row.state)
			}
			lag := answered.Sub(decision.at)
			if lag >= time.Second {
				t.Fatalf("GET /v1/requests/{id}?wait=10 answered %s after the decision returned, want under 1 s", lag)
			}
			t.Logf("GET /v1/requests/{id}?wait=10 answered %s %s after the decision returned", read.State, lag)
		})
	}
}

// TestReadRequestWaitEndsPending: with no decision, a read with ?wait=2 is held the two seconds
// and answers pending, before a third second has passed.
func TestReadRequestWaitEndsPending(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, key, asked := ts.pendingRequest(t)
	start := time.Now()
	status, body := ts.session(t, key, enrollmentID, http.MethodGet, "/v1/requests/"+asked.RequestID+"?wait=2", nil)
	took := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/requests/{id}?wait=2 = %d: %s", status, body)
	}
	if read := decode[wireRequestStatus](t, body); read.State != "pending" {
		t.Fatalf("GET /v1/requests/{id}?wait=2 = %s, want pending: nobody decided it", body)
	}
	if took < 2*time.Second || took >= 3*time.Second {
		t.Fatalf("GET /v1/requests/{id}?wait=2 answered after %s, want from 2 s to under 3 s", took)
	}
	t.Logf("GET /v1/requests/{id}?wait=2 answered pending after %s", took)
}

// TestReadRequestWaitRefusesBadInput: a wait that is not a whole number of seconds from 1 to
// record.MaxRequestWaitSeconds is refused 400 REQUEST_WAIT_INPUT, at once, for a request that is
// pending.
func TestReadRequestWaitRefusesBadInput(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, key, asked := ts.pendingRequest(t)
	for _, wait := range []string{strconv.Itoa(record.MaxRequestWaitSeconds + 1), "x", "0", "-1", "1.5"} {
		start := time.Now()
		status, body := ts.session(t, key, enrollmentID, http.MethodGet, "/v1/requests/"+asked.RequestID+"?wait="+wait, nil)
		took := time.Since(start)
		if status != http.StatusBadRequest || decode[wireError](t, body).Code != "REQUEST_WAIT_INPUT" {
			t.Errorf("GET /v1/requests/{id}?wait=%s = %d %s, want 400 REQUEST_WAIT_INPUT", wait, status, body)
		}
		if took >= time.Second {
			t.Errorf("GET /v1/requests/{id}?wait=%s answered after %s, want the refusal at once", wait, took)
		}
	}
}

// TestReadRequestWaitAnswersAFailedStoreUnavailable: a store that fails while a read is held is
// answered 503 DATABASE_UNAVAILABLE, which agent-secrets reads as a failed read and waits on
// through (retryable), and not 500 INTERNAL, which ends its wait with nothing run. The fault is
// the requests table renamed away a second into the hold, in this test's own schema
// (storetest). Once the table is back, the request is still pending, and a new held read
// answers its decision.
func TestReadRequestWaitAnswersAFailedStoreUnavailable(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, key, asked := ts.pendingRequest(t)
	ctx := context.Background()
	path := "/v1/requests/" + asked.RequestID + "?wait=10"

	broken := after(func() error {
		_, err := ts.Store.Pool.Exec(ctx, `alter table requests rename to requests_away`)
		return err
	})
	status, body := ts.session(t, key, enrollmentID, http.MethodGet, path, nil)
	if fault := <-broken; fault.err != nil {
		t.Fatalf("rename the requests table away: %v", fault.err)
	}
	if status != http.StatusServiceUnavailable || decode[wireError](t, body).Code != "DATABASE_UNAVAILABLE" {
		t.Fatalf("GET /v1/requests/{id}?wait=10 while the store fails = %d %s, want 503 DATABASE_UNAVAILABLE", status, body)
	}

	if _, err := ts.Store.Pool.Exec(ctx, `alter table requests_away rename to requests`); err != nil {
		t.Fatalf("rename the requests table back: %v", err)
	}
	decided := after(func() error {
		_, err := ts.machine.ApplyDecision(ctx, *asked.RecordID, true, testApprover)
		return err
	})
	status, body = ts.session(t, key, enrollmentID, http.MethodGet, path, nil)
	if decision := <-decided; decision.err != nil {
		t.Fatalf("approve the request once the store is back: %v", decision.err)
	}
	if status != http.StatusOK || decode[wireRequestStatus](t, body).State != "granted" {
		t.Fatalf("GET /v1/requests/{id}?wait=10 once the store is back = %d %s, want 200 granted", status, body)
	}
}

// TestReadRequestWaitEndsWhenTheRequestMoves: a held read whose request moves to another
// enrollment while it waits, as adopt moves a predecessor's requests to its successor, is
// answered 403 NOT_YOURS at the next step, as a new read of it is, and not the request's state
// for the rest of the wait.
func TestReadRequestWaitEndsWhenTheRequestMoves(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, key, asked := ts.pendingRequest(t)
	successor, _ := ts.newSessionEnrollment(t, "successor-"+t.Name(), testApprover)

	moved := after(func() error {
		_, err := ts.Store.Pool.Exec(context.Background(), `update requests set enrollment_id=$1 where id=$2`, successor, asked.RequestID)
		return err
	})
	start := time.Now()
	status, body := ts.session(t, key, enrollmentID, http.MethodGet, "/v1/requests/"+asked.RequestID+"?wait=5", nil)
	took := time.Since(start)
	if move := <-moved; move.err != nil {
		t.Fatalf("move the request to the successor: %v", move.err)
	}
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_YOURS" {
		t.Fatalf("GET /v1/requests/{id}?wait=5 of a request moved away = %d %s, want 403 NOT_YOURS", status, body)
	}
	if took >= 5*time.Second {
		t.Fatalf("GET /v1/requests/{id}?wait=5 answered after %s, want the refusal at the step after the move, not at the wait's end", took)
	}
}

// TestReadRequestWaitEndedByItsCallerIsNoFailure: a held read whose caller goes away a second in
// (a Ctrl-C, a tool's timeout that killed the command, a front door that cut the call) is counted
// outcome="cancelled" and writes no line, since the broker failed at nothing and nobody reads the
// answer. It is counted neither as the failed 503 a store fault is nor with the ERROR line one
// writes.
func TestReadRequestWaitEndedByItsCallerIsNoFailure(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	ts := newTestServer(t)
	enrollmentID, key, asked := ts.pendingRequest(t)
	const (
		cancelled = `outcome="cancelled",code="HTTP_503",caller_class="session"`
		failed    = `outcome="failed",code="HTTP_503",caller_class="session"`
	)

	path := "/v1/requests/" + asked.RequestID
	p, err := proof.Sign(key, enrollmentID, http.MethodGet, ts.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	read, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path+"?wait=10", nil)
	if err != nil {
		t.Fatal(err)
	}
	read.Header.Set("Proof", p)
	if resp, err := http.DefaultClient.Do(read); err == nil {
		resp.Body.Close()
		t.Fatalf("GET /v1/requests/{id}?wait=10 answered %d before its caller went away, want the read held", resp.StatusCode)
	}
	// The handler ends once the closed connection cancels its request, and its answer is counted
	// when it returns, after the caller has already stopped waiting for it.
	for wait := time.Now().Add(5 * time.Second); ts.answersCounted(t, cancelled) == 0 && time.Now().Before(wait); {
		time.Sleep(20 * time.Millisecond)
	}
	if n := ts.answersCounted(t, cancelled); n != 1 {
		t.Fatalf("broker_answers_total{%s} = %d, want 1: the read its caller ended", cancelled, n)
	}
	if n := ts.answersCounted(t, failed); n != 0 {
		t.Errorf("broker_answers_total{%s} = %d, want 0: a caller that went away is no failure", failed, n)
	}
	for _, r := range capture.Records() {
		if strings.Contains(r.Message, "read request") {
			t.Errorf("the read its caller ended logged %q at %s, want no line", r.Message, r.Level)
		}
	}
}
