// request_wait_test.go pins GET /v1/requests/{id}?wait=N: the broker holds a session's read of its
// own pending request for up to N seconds, 1 to record.MaxRequestWaitSeconds, and answers the
// request's state as soon as it is no longer pending, whichever broker process decided it, or
// pending once N seconds pass. Any other wait is refused 400 REQUEST_WAIT_INPUT. It is what lets a
// waiting `agent-secrets NAME -- command` learn of a decision within a second of it rather than at
// its next poll.
//
// Every row that reads a time runs the server on a clocktest.Fake, the one clock the answer
// recorder reads a request's arrival on and the hold counts its wait on (api.Deps.Clock), and
// moves it itself: each step of a hold is a timer the read arms on it, so a row reads when the read
// is held, where its next step is armed and what it answers once the clock reaches it, and the
// time a verdict turns on is the one the row set. A stall of the store is a table lock the row
// holds, and the read queued behind it is read off the database (storetest.AwaitLockWaiters).
package api_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// newHoldServer is a test server on fake (newTestServerOn), with every other dependency as
// newTestServer wires it.
func newHoldServer(t *testing.T, fake *clocktest.Fake) *testServer {
	t.Helper()
	return newTestServerOn(t, fake, func(*api.Deps) {})
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

// heldRead is one session read of a request, sent on a goroutine of its own so the test can move
// the server's clock while the broker holds it. cut ends the read's context, as a caller that
// gives up closes its connection; answered receives the broker's answer once it writes one, or
// the call's error once its caller went away first.
type heldRead struct {
	answered <-chan heldAnswer
	cut      context.CancelFunc
}

// heldAnswer is what a heldRead's caller read: the answer's status and body, or the error the call
// ended with.
type heldAnswer struct {
	status int
	body   []byte
	err    error
}

// startRead sends the session's GET path?query (no query when it is empty) on a goroutine of its
// own. The proof signs the path alone, as agent-secrets signs it.
func (ts *testServer) startRead(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, path, query string) heldRead {
	t.Helper()
	p, err := proof.Sign(key, enrollmentID, http.MethodGet, ts.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	target := ts.URL + path
	if query != "" {
		target += "?" + query
	}
	ctx, cut := context.WithCancel(testwait.Context(t))
	t.Cleanup(cut)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Proof", p)
	answered := make(chan heldAnswer, 1)
	go func() {
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			answered <- heldAnswer{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		answered <- heldAnswer{status: resp.StatusCode, body: body, err: err}
	}()
	return heldRead{answered: answered, cut: cut}
}

// answer waits for the read's answer, failing t unless the broker wrote one.
func (r heldRead) answer(t *testing.T, what string) heldAnswer {
	t.Helper()
	got := testwait.Receive(t, what, r.answered)
	if got.err != nil {
		t.Fatalf("%s: the read ended with %v, want the broker's answer", what, got.err)
	}
	return got
}

// gaveUp ends the read as a caller that gives up does, and fails t unless the call ended with its
// caller, before the broker answered it.
func (r heldRead) gaveUp(t *testing.T, what string) {
	t.Helper()
	r.cut()
	if got := testwait.Receive(t, what+" to end with its caller", r.answered); got.err == nil {
		t.Fatalf("%s answered %d %s before its caller went away, want the read still running", what, got.status, got.body)
	}
}

// stepOrAnswer is what a held read did next: armed a step on the server's clock, or answered.
type stepOrAnswer struct {
	step   time.Time
	answer *heldAnswer
}

// nextOf waits until read has either armed its next step on fake, the server's clock, or answered.
// Nothing else of the server arms a timer on that clock, so a pending timer is the read's step,
// and the read answers nothing until it fires.
func nextOf(t *testing.T, fake *clocktest.Fake, read heldRead) stepOrAnswer {
	t.Helper()
	return testwait.Until(t, "the held read to arm its next step or answer", func() (stepOrAnswer, bool) {
		select {
		case got := <-read.answered:
			return stepOrAnswer{answer: &got}, true
		default:
		}
		if armed := fake.Armed(); len(armed) > 0 {
			return stepOrAnswer{step: armed[0]}, true
		}
		return stepOrAnswer{}, false
	})
}

// heldStep waits until read has read its pending request and armed its next step on fake, and
// answers that step's deadline. A read that answers instead was not held, which fails t.
func heldStep(t *testing.T, fake *clocktest.Fake, read heldRead) time.Time {
	t.Helper()
	next := nextOf(t, fake, read)
	if next.answer != nil {
		t.Fatalf("the read answered %d %s (%v) while its request was pending, want it held with a step armed",
			next.answer.status, next.answer.body, next.answer.err)
	}
	return next.step
}

// heldToTheEnd moves fake to each step read arms, for as long as it arms one, and answers every
// step it armed and the answer it gave once it armed no more.
func heldToTheEnd(t *testing.T, fake *clocktest.Fake, read heldRead) ([]time.Time, heldAnswer) {
	t.Helper()
	var steps []time.Time
	for {
		next := nextOf(t, fake, read)
		if next.answer != nil {
			if next.answer.err != nil {
				t.Fatalf("the held read ended with %v after steps %v, want the broker's answer", next.answer.err, steps)
			}
			return steps, *next.answer
		}
		steps = append(steps, next.step)
		fake.Set(next.step)
	}
}

// endsAt fails t unless the last of steps is end and none is past it: a hold that ends where its
// wait does.
func endsAt(t *testing.T, steps []time.Time, end time.Time, why string) {
	t.Helper()
	if len(steps) == 0 || !steps[len(steps)-1].Equal(end) || slices.ContainsFunc(steps, end.Before) {
		t.Fatalf("the held read armed steps %v, want its last at %s and none past it: %s", steps, end, why)
	}
}

// TestReadRequestWaitsForTheDecision: a session's read with ?wait=10 of its pending request is
// held, and answers the decision another broker process makes while it waits at the read's next
// step, which it armed less than a second after that decision (R1: a waiting command learns of a
// decision within one second of it). A read that was not held would answer pending at once, with
// no step armed.
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
			fake := clocktest.New(time.Now())
			ts := newHoldServer(t, fake)
			enrollmentID, key, asked := ts.pendingRequest(t)
			other := ts.otherBroker(t)

			read := ts.startRead(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait=10")
			step := heldStep(t, fake, read)
			decided := fake.Now()
			if err := ts.decideOnThePage(t, other, *asked.RecordID, row.approve); err != nil {
				t.Fatalf("the other broker's decision: %v", err)
			}
			if lag := step.Sub(decided); lag >= time.Second {
				t.Fatalf("GET /v1/requests/{id}?wait=10 reads again %s after the decision, want under 1 s", lag)
			}
			fake.Set(step)
			got := read.answer(t, "GET /v1/requests/{id}?wait=10 to answer the decision at its next step")
			if got.status != http.StatusOK {
				t.Fatalf("GET /v1/requests/{id}?wait=10 = %d: %s", got.status, got.body)
			}
			status := decode[wireRequestStatus](t, got.body)
			if status.State != row.state || (row.state == "granted") != (status.GrantID != nil) {
				t.Fatalf("GET /v1/requests/{id}?wait=10 = %s, want state %q (a grant_id only once granted): held until the decision", got.body, row.state)
			}
		})
	}
}

// TestReadRequestWaitEndsPending: with no decision, a read with ?wait=2 is held, reading the
// request again at each step, until two seconds after it arrived, and then answers pending: its
// last step is armed for that moment and none for later.
func TestReadRequestWaitEndsPending(t *testing.T) {
	fake := clocktest.New(time.Now())
	ts := newHoldServer(t, fake)
	enrollmentID, key, asked := ts.pendingRequest(t)
	arrived := fake.Now()
	read := ts.startRead(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait=2")
	steps, got := heldToTheEnd(t, fake, read)
	if got.status != http.StatusOK {
		t.Fatalf("GET /v1/requests/{id}?wait=2 = %d: %s", got.status, got.body)
	}
	if status := decode[wireRequestStatus](t, got.body); status.State != "pending" {
		t.Fatalf("GET /v1/requests/{id}?wait=2 = %s, want pending: nobody decided it", got.body)
	}
	endsAt(t, steps, arrived.Add(2*time.Second), "a read that asked two seconds is held two seconds from its arrival")
}

// TestReadRequestWaitEndsAtTheJudgesHandOver: a read with ?wait=4 of a request the model judge
// holds answers at the step after the hold runs out, still pending and no longer judge_deciding,
// before its wait ends: the request is its person's now, and the agent learns so at once rather
// than at the end of its wait. The hold is judge_until, which PostgreSQL reads on its own clock
// (now()), so the row ends it in SQL while the read waits for its next step.
func TestReadRequestWaitEndsAtTheJudgesHandOver(t *testing.T) {
	fake := clocktest.New(time.Now())
	ts := newHoldServer(t, fake)
	enrollmentID, key, asked := ts.pendingRequest(t)
	if _, err := ts.Store.Pool.Exec(context.Background(), `update requests set judge_until = now() + interval '1 hour' where id=$1`, asked.RequestID); err != nil {
		t.Fatalf("hold the request for the judge: %v", err)
	}
	arrived := fake.Now()
	read := ts.startRead(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait=4")
	step := heldStep(t, fake, read)
	if _, err := ts.Store.Pool.Exec(context.Background(), `update requests set judge_until = now() - interval '1 second' where id=$1`, asked.RequestID); err != nil {
		t.Fatalf("end the judge's hold: %v", err)
	}
	fake.Set(step)
	got := read.answer(t, "GET /v1/requests/{id}?wait=4 to answer at the step after the judge's hold ran out")
	if got.status != http.StatusOK {
		t.Fatalf("GET /v1/requests/{id}?wait=4 = %d: %s", got.status, got.body)
	}
	if status := decode[wireRequestStatus](t, got.body); status.State != "pending" || strings.Contains(string(got.body), `"judge_deciding":true`) {
		t.Fatalf("GET /v1/requests/{id}?wait=4 = %s, want pending and no longer judge_deciding: the judge's hold ran out", got.body)
	}
	if end := arrived.Add(4 * time.Second); !step.Before(end) {
		t.Fatalf("GET /v1/requests/{id}?wait=4 read again at %s, want the step after the hand-over, before the wait's end at %s", step, end)
	}
}

// TestReadRequestWaitRefusesBadInput: a wait that is not a whole number of seconds from 1 to
// record.MaxRequestWaitSeconds is refused 400 REQUEST_WAIT_INPUT, at once, for a request that is
// pending: answered with the server's clock never moved and no step armed on it.
func TestReadRequestWaitRefusesBadInput(t *testing.T) {
	fake := clocktest.New(time.Now())
	ts := newHoldServer(t, fake)
	enrollmentID, key, asked := ts.pendingRequest(t)
	for _, wait := range []string{strconv.Itoa(record.MaxRequestWaitSeconds + 1), "x", "0", "-1", "1.5"} {
		read := ts.startRead(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait="+wait)
		got := read.answer(t, "GET /v1/requests/{id}?wait="+wait+" to be refused")
		if got.status != http.StatusBadRequest || decode[wireError](t, got.body).Code != "REQUEST_WAIT_INPUT" {
			t.Errorf("GET /v1/requests/{id}?wait=%s = %d %s, want 400 REQUEST_WAIT_INPUT", wait, got.status, got.body)
		}
		if armed := fake.Armed(); len(armed) != 0 {
			t.Errorf("GET /v1/requests/{id}?wait=%s left steps %v armed, want the refusal before any read", wait, armed)
		}
	}
}

// TestReadRequestWaitAnswersAFailedStoreUnavailable: a store that fails while a read is held is
// answered 503 DATABASE_UNAVAILABLE, which agent-secrets reads as a failed read and waits on
// through (retryable), and not 500 INTERNAL, which ends its wait with nothing run. The fault is
// the requests table renamed away while the read waits for its next step, in this test's own
// schema (storetest). Once the table is back, the request is still pending, and a new held read
// answers its decision at its next step.
func TestReadRequestWaitAnswersAFailedStoreUnavailable(t *testing.T) {
	fake := clocktest.New(time.Now())
	ts := newHoldServer(t, fake)
	enrollmentID, key, asked := ts.pendingRequest(t)
	ctx := context.Background()
	path := "/v1/requests/" + asked.RequestID

	read := ts.startRead(t, key, enrollmentID, path, "wait=10")
	step := heldStep(t, fake, read)
	if _, err := ts.Store.Pool.Exec(ctx, `alter table requests rename to requests_away`); err != nil {
		t.Fatalf("rename the requests table away: %v", err)
	}
	fake.Set(step)
	got := read.answer(t, "GET /v1/requests/{id}?wait=10 to answer the store's failure at its next step")
	if got.status != http.StatusServiceUnavailable || decode[wireError](t, got.body).Code != "DATABASE_UNAVAILABLE" {
		t.Fatalf("GET /v1/requests/{id}?wait=10 while the store fails = %d %s, want 503 DATABASE_UNAVAILABLE", got.status, got.body)
	}

	if _, err := ts.Store.Pool.Exec(ctx, `alter table requests_away rename to requests`); err != nil {
		t.Fatalf("rename the requests table back: %v", err)
	}
	read = ts.startRead(t, key, enrollmentID, path, "wait=10")
	step = heldStep(t, fake, read)
	if err := ts.decideOnThePage(t, ts.machine, *asked.RecordID, true); err != nil {
		t.Fatalf("approve the request once the store is back: %v", err)
	}
	fake.Set(step)
	got = read.answer(t, "GET /v1/requests/{id}?wait=10 to answer the approval once the store is back")
	if got.status != http.StatusOK || decode[wireRequestStatus](t, got.body).State != "granted" {
		t.Fatalf("GET /v1/requests/{id}?wait=10 once the store is back = %d %s, want 200 granted", got.status, got.body)
	}
}

// TestReadRequestWaitEndsWhenTheRequestMoves: a held read whose request moves to an enrollment of
// another owner while it waits (the successor here records no owner, so it is its own) is
// answered 403 NOT_YOURS at the next step, as a new read of it is, and not the request's state at
// the end of its wait.
func TestReadRequestWaitEndsWhenTheRequestMoves(t *testing.T) {
	fake := clocktest.New(time.Now())
	ts := newHoldServer(t, fake)
	enrollmentID, key, asked := ts.pendingRequest(t)
	successor, _ := ts.newSessionEnrollment(t, "successor-"+t.Name(), testApprover)

	arrived := fake.Now()
	read := ts.startRead(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait=5")
	step := heldStep(t, fake, read)
	if _, err := ts.Store.Pool.Exec(context.Background(), `update requests set enrollment_id=$1 where id=$2`, successor, asked.RequestID); err != nil {
		t.Fatalf("move the request to the successor: %v", err)
	}
	fake.Set(step)
	got := read.answer(t, "GET /v1/requests/{id}?wait=5 to answer at the step after the move")
	if got.status != http.StatusForbidden || decode[wireError](t, got.body).Code != "NOT_YOURS" {
		t.Fatalf("GET /v1/requests/{id}?wait=5 of a request moved away = %d %s, want 403 NOT_YOURS", got.status, got.body)
	}
	if end := arrived.Add(5 * time.Second); !step.Before(end) {
		t.Fatalf("GET /v1/requests/{id}?wait=5 read again at %s, want the step after the move, before the wait's end at %s", step, end)
	}
}

// counted waits until ts's broker_answers_total sample naming labels is above 0 and answers it: an
// answer whose caller has gone is counted when its handler returns, after the caller stopped
// waiting for it.
func (ts *testServer) counted(t *testing.T, labels string) int {
	t.Helper()
	return testwait.Until(t, "broker_answers_total{"+labels+"} to count the answer", func() (int, bool) {
		n := ts.answersCounted(t, labels)
		return n, n > 0
	})
}

// lockHeld takes ACCESS EXCLUSIVE on table, in this test's own schema, in a transaction on a pooled
// connection of its own, and holds it until release or the test's end, whichever comes first. It
// answers the transaction, which storetest.AwaitLockWaiters reads the backends queued behind, and
// release. The release is also a cleanup, registered after the server's, so it runs before the
// server closes, which waits for the handlers the lock holds, and before the store's pool is
// closed, which would otherwise wait on the lock's connection for as long as the test binary lives.
func (ts *testServer) lockHeld(t *testing.T, table string) (holder pgx.Tx, release func()) {
	t.Helper()
	tx, err := ts.Store.Pool.Begin(testwait.Context(t))
	if err != nil {
		t.Fatalf("begin the lock of the %s table: %v", table, err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback(context.Background()) }) }
	t.Cleanup(release)
	if _, err := tx.Exec(testwait.Context(t), "lock table "+table+" in access exclusive mode"); err != nil {
		t.Fatalf("lock the %s table: %v", table, err)
	}
	return tx, release
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
	if n := ts.counted(t, failed); n != 1 {
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
	fake := clocktest.New(time.Now())
	ts := newHoldServer(t, fake)
	enrollmentID, key, asked := ts.pendingRequest(t)

	arrived := fake.Now()
	read := ts.startRead(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait=10")
	// The read's steps up to a second into its wait, which is where its caller goes away, the read
	// holding for its next step.
	for step := heldStep(t, fake, read); !step.After(arrived.Add(time.Second)); step = heldStep(t, fake, read) {
		fake.Set(step)
	}
	read.gaveUp(t, "GET /v1/requests/{id}?wait=10")
	if n := ts.counted(t, readCancelled); n != 1 {
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
	fake := clocktest.New(time.Now())
	ts := newHoldServer(t, fake)
	enrollmentID, key, asked := ts.pendingRequest(t)

	arrived := fake.Now()
	read := ts.startRead(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait=1")
	step := heldStep(t, fake, read)
	holder, _ := ts.lockHeld(t, "requests")
	fake.Set(step)
	storetest.AwaitLockWaiters(t, ts.Store.Pool, holder, 1)
	fake.Set(arrived.Add(3 * time.Second))
	read.gaveUp(t, "GET /v1/requests/{id}?wait=1")
	requireFailed(t, ts, capture, readFailed, readFailedLine, "a store that stalled past the read's wait failed")
}

// TestReadRequestWithNoWaitEndedByItsCallerIsAFailure: a read with no wait owes its answer at once,
// so a caller that leaves while the store stalls it left an answer the broker was already late
// with: a store failure, logged and counted failed, as it was before ?wait= existed. Only a held
// read's caller leaves early.
func TestReadRequestWithNoWaitEndedByItsCallerIsAFailure(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	fake := clocktest.New(time.Now())
	ts := newHoldServer(t, fake)
	enrollmentID, key, asked := ts.pendingRequest(t)

	holder, _ := ts.lockHeld(t, "requests")
	read := ts.startRead(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "")
	storetest.AwaitLockWaiters(t, ts.Store.Pool, holder, 1)
	read.gaveUp(t, "GET /v1/requests/{id}")
	requireFailed(t, ts, capture, readFailed, readFailedLine, "a read with no wait owed its answer at once")
}

// TestReadRequestWaitCountsFromItsArrival: a held read's wait counts from when the request reached
// the broker, not from when its proof was checked. A ?wait=2 read whose proof check the store
// stalls for a second, a lock on the enrollments table, is held to two seconds after it arrived,
// as it asked, and not to three.
func TestReadRequestWaitCountsFromItsArrival(t *testing.T) {
	fake := clocktest.New(time.Now())
	ts := newHoldServer(t, fake)
	enrollmentID, key, asked := ts.pendingRequest(t)

	holder, release := ts.lockHeld(t, "enrollments")
	arrived := fake.Now()
	read := ts.startRead(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait=2")
	storetest.AwaitLockWaiters(t, ts.Store.Pool, holder, 1)
	fake.Advance(time.Second)
	release()
	steps, got := heldToTheEnd(t, fake, read)
	if got.status != http.StatusOK || decode[wireRequestStatus](t, got.body).State != "pending" {
		t.Fatalf("GET /v1/requests/{id}?wait=2 = %d %s, want 200 pending", got.status, got.body)
	}
	endsAt(t, steps, arrived.Add(2*time.Second), "with its proof check stalled a second, the wait still counts from its arrival")
}

// TestReadRequestOwedTimeCountsFromItsArrival: the time a held read owes its answer by counts from
// the request's arrival too, on the clock its hold counts on. A ?wait=1 read whose proof check the
// store stalls for a second (enrollments), and whose read the store then stalls (requests), owes
// its answer 1.5 s after it arrived; its caller, giving up at 2 s, left a broker already late, so
// the read is a store failure, logged and counted failed. Counted from after the proof check, the
// window would run to 2.5 s and count it cancelled.
func TestReadRequestOwedTimeCountsFromItsArrival(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	fake := clocktest.New(time.Now())
	ts := newHoldServer(t, fake)
	enrollmentID, key, asked := ts.pendingRequest(t)

	requestsHolder, _ := ts.lockHeld(t, "requests")
	enrollmentsHolder, releaseEnrollments := ts.lockHeld(t, "enrollments")
	arrived := fake.Now()
	read := ts.startRead(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait=1")
	storetest.AwaitLockWaiters(t, ts.Store.Pool, enrollmentsHolder, 1)
	fake.Advance(time.Second)
	releaseEnrollments()
	storetest.AwaitLockWaiters(t, ts.Store.Pool, requestsHolder, 1)
	fake.Set(arrived.Add(2 * time.Second))
	read.gaveUp(t, "GET /v1/requests/{id}?wait=1")
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
	fake := clocktest.New(time.Now())
	ts := newHoldServer(t, fake)
	enrollmentID, key, asked := ts.pendingRequest(t)

	holder, _ := ts.lockHeld(t, "enrollments")
	read := ts.startRead(t, key, enrollmentID, "/v1/requests/"+asked.RequestID, "wait=5")
	storetest.AwaitLockWaiters(t, ts.Store.Pool, holder, 1)
	read.gaveUp(t, "GET /v1/requests/{id}?wait=5")
	requireFailed(t, ts, capture, proofCheckFailed, proofCheckFailedLine, "the caller left while its proof was being checked, inside its wait")
	for _, r := range capture.Records() {
		if r.Message == readFailedLine {
			t.Errorf("logged %q; want the proof check's line alone: the request did not reach the read", r.Message)
		}
	}
}
