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
	"crypto/sha256"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
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

// decideOnThePage decides recordID on machine as testApprover does on the broker's approval page:
// it signs testApprover in to the page by its own account, under a cookie secret of its own, and
// decides under that session (requests.Machine.ApplyPageDecision), since a person decides only
// under a session the page read.
func (ts *testServer) decideOnThePage(t *testing.T, machine *requests.Machine, recordID string, approve bool) error {
	t.Helper()
	secret := "decides " + t.Name()
	ts.pageSession(t, testApprover, secret, &store.IssuerSubject{Issuer: "https://accounts.google.com", Subject: "sub-" + testApprover})
	_, err := machine.ApplyPageDecision(context.Background(), recordID, approve, store.BrowserSessionRef{Hash: sha256.Sum256([]byte(secret)), Login: testApprover})
	return err
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
				return ts.decideOnThePage(t, other, *asked.RecordID, row.approve)
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

// TestReadRequestWaitEndsAtTheJudgesHandOver: a read with ?wait=4 of a request the model judge
// holds (judge_until about a second ahead) answers once the hold runs out, still pending and no
// longer judge_deciding, well before its wait ends: the request is its person's now, and the
// agent learns so at once rather than at the end of its wait.
func TestReadRequestWaitEndsAtTheJudgesHandOver(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, key, asked := ts.pendingRequest(t)
	if _, err := ts.Store.Pool.Exec(context.Background(), `update requests set judge_until = now() + interval '1 second' where id=$1`, asked.RequestID); err != nil {
		t.Fatalf("hold the request for the judge: %v", err)
	}
	start := time.Now()
	status, body := ts.session(t, key, enrollmentID, http.MethodGet, "/v1/requests/"+asked.RequestID+"?wait=4", nil)
	took := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/requests/{id}?wait=4 = %d: %s", status, body)
	}
	if read := decode[wireRequestStatus](t, body); read.State != "pending" || strings.Contains(string(body), `"judge_deciding":true`) {
		t.Fatalf("GET /v1/requests/{id}?wait=4 = %s, want pending and no longer judge_deciding: the judge's hold ran out", body)
	}
	if took < 500*time.Millisecond || took >= 3*time.Second {
		t.Fatalf("GET /v1/requests/{id}?wait=4 answered after %s, want once the hold ran out (about 1 s), not at the wait's end (4 s)", took)
	}
	t.Logf("GET /v1/requests/{id}?wait=4 answered at the hand-over after %s", took)
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
		return ts.decideOnThePage(t, ts.machine, *asked.RecordID, true)
	})
	status, body = ts.session(t, key, enrollmentID, http.MethodGet, path, nil)
	if decision := <-decided; decision.err != nil {
		t.Fatalf("approve the request once the store is back: %v", decision.err)
	}
	if status != http.StatusOK || decode[wireRequestStatus](t, body).State != "granted" {
		t.Fatalf("GET /v1/requests/{id}?wait=10 once the store is back = %d %s, want 200 granted", status, body)
	}
}

// TestReadRequestWaitEndsWhenTheRequestMoves: a held read whose request moves to an enrollment of
// another owner while it waits (the successor here records no owner, so it is its own) is
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

// readCut sends the session's GET path?query and closes the connection after cut, as a caller that
// gives up does (answer.Recorder.NoteCallerEnded), failing t when the broker answered first. The
// proof signs the path alone.
func (ts *testServer) readCut(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, path, query string, cut time.Duration) {
	t.Helper()
	p, err := proof.Sign(key, enrollmentID, http.MethodGet, ts.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cut)
	defer cancel()
	read, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path+"?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	read.Header.Set("Proof", p)
	if resp, err := http.DefaultClient.Do(read); err == nil {
		resp.Body.Close()
		t.Fatalf("GET %s?%s answered %d before its caller went away at %s, want the read still running", path, query, resp.StatusCode, cut)
	}
}

// countedSoon is ts's broker_answers_total sample naming labels once it is above 0, or what it is
// after 5 seconds: an answer whose caller has gone is counted when its handler returns, after the
// caller stopped waiting for it.
func (ts *testServer) countedSoon(t *testing.T, labels string) int {
	t.Helper()
	testwait.Holds(5*time.Second, 20*time.Millisecond, func() bool { return ts.answersCounted(t, labels) > 0 })
	return ts.answersCounted(t, labels)
}

// lockTable takes ACCESS EXCLUSIVE on table, in this test's own schema, on a pooled connection of
// its own, after delay, and holds it for hold, or until the test ends when hold is 0, whichever
// way the test ends: the release is a cleanup, registered after the server's, so it runs before
// the store's pool is closed, which would otherwise wait on the lock's connection for as long as
// the test binary lives. locked receives the lock's error, nil once the lock is held; a lock not
// granted within testLockWait is that error, so a test never waits on the lock without bound
// either.
func (ts *testServer) lockTable(t *testing.T, table string, delay, hold time.Duration) (locked <-chan error) {
	t.Helper()
	ctx := context.Background()
	result := make(chan error, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		time.Sleep(delay)
		tx, err := ts.Store.Pool.Begin(ctx)
		if err == nil {
			defer func() { _ = tx.Rollback(ctx) }()
			err = lockInTx(ctx, tx, table)
		}
		result <- err
		if hold == 0 {
			<-release
			return
		}
		select {
		case <-release:
		case <-time.After(hold):
		}
	}()
	return result
}

// lockNow is lockTable with no delay, failing t unless the lock is held when it returns.
func (ts *testServer) lockNow(t *testing.T, table string, hold time.Duration) {
	t.Helper()
	if err := <-ts.lockTable(t, table, 0, hold); err != nil {
		t.Fatalf("lock the %s table: %v", table, err)
	}
}

// testLockWait bounds how long a test waits for a table lock it takes (lock_timeout), so a lock
// held elsewhere fails the test naming the lock rather than holding it until go test's -timeout.
const testLockWait = "10s"

// lockInTx takes ACCESS EXCLUSIVE on table inside tx, waiting at most testLockWait for it.
func lockInTx(ctx context.Context, tx pgx.Tx, table string) error {
	if _, err := tx.Exec(ctx, "set local lock_timeout = '"+testLockWait+"'"); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "lock table "+table+" in access exclusive mode")
	return err
}

// The series of a request read's 503: a held read's caller's own end, a failure the read itself
// met, and a failure of its proof check, which counts its caller unauthenticated; and the ERROR
// lines the two failures write.
const (
	readCancelled        = `outcome="cancelled",code="HTTP_503",caller_class="session"`
	readFailed           = `outcome="failed",code="HTTP_503",caller_class="session"`
	proofCheckFailed     = `outcome="failed",code="HTTP_503",caller_class="unauthenticated"`
	readFailedLine       = "broker: read request failed"
	proofCheckFailedLine = "broker: verify proof failed"
)

// requireFailed fails t unless ts counted one answer under failed, the series of a failed 503, and
// capture holds line at ERROR. Each answer is counted once, so one under failed is also none under
// cancelled.
func requireFailed(t *testing.T, ts *testServer, capture *logtest.Recorder, failed, line, why string) {
	t.Helper()
	if n := ts.countedSoon(t, failed); n != 1 {
		t.Fatalf("broker_answers_total{%s} = %d, want 1: %s", failed, n, why)
	}
	logged := false
	for _, r := range capture.Records() {
		logged = logged || (r.Level == slog.LevelError && r.Message == line)
	}
	if !logged {
		t.Errorf("no `%s` line at ERROR; want one: %s", line, why)
	}
}

// TestReadRequestWaitEndedByItsCallerIsNoFailure: a held read whose caller goes away a second into
// its ten-second wait, before the broker owed its answer, is counted outcome="cancelled" and writes
// no line. It is counted neither as the failed 503 a store fault is nor with the ERROR line one
// writes.
func TestReadRequestWaitEndedByItsCallerIsNoFailure(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	ts := newTestServer(t)
	enrollmentID, key, asked := ts.pendingRequest(t)

	ts.readCut(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait=10", time.Second)
	if n := ts.countedSoon(t, readCancelled); n != 1 {
		t.Fatalf("broker_answers_total{%s} = %d, want 1: the read its caller ended", readCancelled, n)
	}
	if n := ts.answersCounted(t, readFailed); n != 0 {
		t.Errorf("broker_answers_total{%s} = %d, want 0: a caller that went away is no failure", readFailed, n)
	}
	for _, r := range capture.Records() {
		if strings.Contains(r.Message, "read request") {
			t.Errorf("the read its caller ended logged %q at %s, want no line", r.Message, r.Level)
		}
	}
}

// TestReadRequestWaitStalledPastItsWaitIsAFailure: a held read whose store stalls, a lock on the
// requests table taken after its first step, until its caller gives up at 3 s, past the 1 s it
// asked for and the half second after it, is a store failure: counted outcome="failed" with a
// `broker: read request failed` line at ERROR, as the same stall in the proof check is. The pool
// sets no statement or acquire timeout, so such a read runs until its caller gives up or the
// request deadline ends it, and counting it cancelled would hide the stall.
func TestReadRequestWaitStalledPastItsWaitIsAFailure(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	ts := newTestServer(t)
	enrollmentID, key, asked := ts.pendingRequest(t)

	locked := ts.lockTable(t, "requests", 200*time.Millisecond, 0)
	ts.readCut(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait=1", 3*time.Second)
	if err := <-locked; err != nil {
		t.Fatalf("lock the requests table: %v", err)
	}
	requireFailed(t, ts, capture, readFailed, readFailedLine, "a store that stalled past the read's wait failed")
}

// TestReadRequestWithNoWaitEndedByItsCallerIsAFailure: a read with no wait owes its answer at once,
// so a caller that leaves while the store stalls it, 300 ms in, left an answer the broker was
// already late with: a store failure, logged and counted failed, as it was before ?wait= existed.
// Only a held read's caller leaves early.
func TestReadRequestWithNoWaitEndedByItsCallerIsAFailure(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	ts := newTestServer(t)
	enrollmentID, key, asked := ts.pendingRequest(t)

	ts.lockNow(t, "requests", 0)
	ts.readCut(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "", 300*time.Millisecond)
	requireFailed(t, ts, capture, readFailed, readFailedLine, "a read with no wait owed its answer at once")
}

// TestReadRequestWaitCountsFromItsArrival: a held read's wait counts from when the request reached
// the broker, not from when its proof was checked. A ?wait=2 read whose proof check the store
// stalls for a second, a lock on the enrollments table, answers pending 2 s after it was sent, as
// it asked, and not 3 s.
func TestReadRequestWaitCountsFromItsArrival(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, key, asked := ts.pendingRequest(t)

	ts.lockNow(t, "enrollments", time.Second)
	start := time.Now()
	status, body := ts.session(t, key, enrollmentID, http.MethodGet, "/v1/requests/"+asked.RequestID+"?wait=2", nil)
	took := time.Since(start)
	if status != http.StatusOK || decode[wireRequestStatus](t, body).State != "pending" {
		t.Fatalf("GET /v1/requests/{id}?wait=2 = %d %s, want 200 pending", status, body)
	}
	if took < 2*time.Second || took >= 2500*time.Millisecond {
		t.Fatalf("GET /v1/requests/{id}?wait=2 with its proof check stalled a second answered after %s, want from 2 s to under 2.5 s: the wait counts from its arrival", took)
	}
	t.Logf("GET /v1/requests/{id}?wait=2 with its proof check stalled a second answered pending after %s", took)
}

// TestReadRequestOwedTimeCountsFromItsArrival: the time a held read owes its answer by counts from
// the request's arrival too, on the clock its hold counts on. A ?wait=1 read whose proof check the
// store stalls for a second (enrollments), and whose read the store then stalls (requests), owes
// its answer 1.5 s after it arrived; its caller, giving up at 2 s, left a broker already late, so
// the read is a store failure, logged and counted failed. Counted from after the proof check, the
// window would run to 2.5 s and count it cancelled. The cut at 2 s sits half a second from each
// end, so a request a few hundred ms late to arrive still lands on the right side.
func TestReadRequestOwedTimeCountsFromItsArrival(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	ts := newTestServer(t)
	enrollmentID, key, asked := ts.pendingRequest(t)

	ts.lockNow(t, "requests", 0)
	ts.lockNow(t, "enrollments", time.Second)
	ts.readCut(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait=1", 2*time.Second)
	requireFailed(t, ts, capture, readFailed, readFailedLine, "the caller gave up at 2 s, past the 1.5 s its read owed its answer by, counted from its arrival")
}

// TestReadRequestWaitEndedDuringItsProofCheckIsAFailure: the wait counts from arrival, so a caller
// that goes away while the store stalls its proof check, here while the broker looks its
// enrollment up, leaves inside its wait; its request does not reach the read, and the proof check
// it ended is a store failure: `broker: verify proof failed` at ERROR, a 503 counted failed for an
// unauthenticated caller.
func TestReadRequestWaitEndedDuringItsProofCheckIsAFailure(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	ts := newTestServer(t)
	enrollmentID, key, asked := ts.pendingRequest(t)

	ts.lockNow(t, "enrollments", 0)
	ts.readCut(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait=5", time.Second)
	requireFailed(t, ts, capture, proofCheckFailed, proofCheckFailedLine, "the caller left while its proof was being checked, inside its wait")
	for _, r := range capture.Records() {
		if r.Message == readFailedLine {
			t.Errorf("logged %q; want the proof check's line alone: the request did not reach the read", r.Message)
		}
	}
}
