//go:build multiprocess

// broker_moved_test.go drives two REAL compiled brokers on one Postgres and measures one thing: a
// session's pending request follows the session to a broker of another address, and the
// session's next ask there ends it and asks afresh. Two brokers of two BROKER_PUBLIC_URLs share a
// database while a deployment moves to a new broker: a session asks the old one, its person has
// not answered yet, and the session comes up again against the new one. The session's next
// enrollment there adopts the request it left waiting (enroll.adopt), so the request now hangs off
// the new broker's enrollment, but its record is a request object signed for the old broker's
// address. Nobody on the new broker can decide that record: its approval page verifies every
// record for its own address and leaves this one out (requests.PendingLeftOutMessage), and its
// judge was never handed it, since only the broker that wrote a request holds it for its judge.
// Before Create ended such a request, it also held the session's names on the new broker until
// it expired: the session's ask was coalesced onto it, or refused REQUEST_PENDING naming it.
//
// Nothing here is a unit test. Both binaries are the one cmd/broker builds, the schema is a fresh
// one on the test server, every request crosses a socket, the adoption is the broker's own, and
// the rows are read with a database handle of the test's own. Each broker derives its public
// address from the port it bound (BROKER_PUBLIC_URL of port 0, a local run's convention, which
// main resolves before it builds anything that checks an audience), so the two processes carry
// two addresses and every request object a client signs names the one broker it is sent to. That
// is what a move between brokers is, and what one broker behind one address cannot show.
//
// It carries the multiprocess tag, for the reason multiprocess_test.go gives: it builds a binary
// and starts two processes, which the unit lane's per-package deadline is not sized for.

package main

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// brokerMovedSession is the session name this test moves from broker A to broker B. Its
	// principal is the pinned fleet's Google account, the same on both brokers, since the
	// database they share is what holds the name and the enrollments that carry it.
	brokerMovedSession = "broker-moved-pending"
	// brokerMovedMessage is the INFO line a broker writes for each pending request it ends because
	// the request's record is signed for another broker address. It is the broker's own words,
	// written out rather than read from requests.RequestEndedBrokerMovedMessage: this test reads
	// the binary's log as an operator does, by its text, so a change of that text is a change this
	// test sees, and the file runs unchanged against a broker built before that constant existed,
	// which is how its negative control runs.
	brokerMovedMessage = "broker: a pending request's record is signed for another broker address; it is ended, and its session's new request replaces it"
	// movedRecordSkew is how far a record's request object may be issued from the moment its
	// record was written, when this test verifies the object against the record's own creation
	// time. The object is signed just before it is sent, so seconds would do; minutes leave room
	// for the clocks of the test and of the database server to differ.
	movedRecordSkew = 5 * time.Minute
)

// brokerMovedPhrases are what the ended request's decision_detail must say: why nobody decided
// it. Each is a phrase of requests.BrokerMovedDetail, read with strings.Contains, so the
// sentence around them may be reworded without this test failing.
var brokerMovedPhrases = []string{"broker moved", "another broker address"}

// brokerMovedLogKeys are every field the end's line carries: the three every line of the binary
// has (logging.NewCloudHandler) and the four ids the line names. The line names ids and nothing
// else, so no secret value, token or request object can reach it.
var brokerMovedLogKeys = []string{"enrollment", "message", "new_request", "record_id", "request", "severity", "time"}

// TestAPendingRequestFollowsItsSessionToTheNewBroker is the live measurement of the end. Broker A
// and broker B share one database and one Google issuer and differ in their address alone. The
// fleet's machine trades its Google identity at A, enrolls the session, and asks for the secret
// its fleet's person owns: pending, on a record signed for A's address. The machine then trades
// again at B under a key of its own, enrolls the same session name there, which adopts the pending
// request onto B's enrollment, and asks again. B must end the old request (cancelled, decided by
// the broker, its detail saying the broker moved) and answer a new pending request on a record
// signed for B's address, log the end once with the four ids, and coalesce the session's next ask
// onto the new request without logging the end again. A, which ended nothing, logs nothing of it.
//
// At a broker without the end, the ask on B is coalesced onto the old request or refused
// REQUEST_PENDING naming it, which is where this test then fails.
//
// It skips when BROKER_TEST_DATABASE_URL is unset, as every Postgres-backed broker test does.
func TestAPendingRequestFollowsItsSessionToTheNewBroker(t *testing.T) {
	databaseURL := storetest.URL(t)
	// The issuers TestASessionsGrantFollowsItToANewBox starts its broker with, one set shared by
	// both brokers, as two brokers of one deployment trust the same issuers: the cluster's, which
	// serves the approval page's provider too, GitHub's, and the one standing in for Google,
	// whose identity tokens the machine trades at each broker.
	cluster, github, google := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	github.PublishKey(t, "github-key")
	googleKey := google.PublishKey(t, "google-key")
	brokertest.SignIn(cluster, cluster.PublishKey(t, "signin-key"))

	// One configuration for both brokers. Sign-in is on, as in production, because a broker with
	// sign-in off denies every pending request it cannot decide (requests' denyUndecidable), and
	// the request this test moves must still be pending when it reaches B. No judge is set, so
	// nobody decides the request on either broker while the test moves it.
	env := append(googleExchangeEnv(t, databaseURL, cluster, github, google, longAccessSeconds),
		"BROKER_SIGNIN_ISSUER="+cluster.URL())
	binary := buildBroker(t)
	start := func() *brokerProcess {
		broker := startBroker(t, brokerOptions{Binary: binary, Env: env})
		requireHealthy(t, broker.Addr)
		return broker
	}
	brokerA, brokerB := start(), start()
	if brokerA.Addr == brokerB.Addr {
		t.Fatalf("both brokers bound %s, want an address each: the move this test drives is between two broker addresses", brokerA.Addr)
	}

	ctx := context.Background()
	db, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the brokers' database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(ctx) })

	// A rig per broker, each naming its own broker's address as the address it dials and the
	// base every proof and request object is signed for, as TestASessionsGrantFollowsItToANewBox
	// sets its one rig: a request a client signs through a rig is signed for that rig's broker.
	rigFor := func(broker *brokerProcess) carryRig {
		return carryRig{t: t, ctx: ctx, addr: broker.Addr, base: "http://" + broker.Addr, db: db,
			google: google, googleKey: googleKey, signIn: cluster}
	}
	rigA, rigB := rigFor(brokerA), rigFor(brokerB)

	// On A: the machine's trade, the session's enrollment under its name, and its ask, which
	// waits on the fleet's person.
	keyA := proofKey(t)
	credentialA := rigA.machineCredential(keyA, brokerMovedSession)
	onA := rigA.enrol(keyA, credentialA, brokerMovedSession+"-a", brokerMovedSession)
	asked := requirePendingAsk(rigA, onA, personSecret,
		"the session's first ask, on broker A, waits on a person")
	requireSignedFor(rigA, *asked.RecordID, rigA.base,
		"the record broker A wrote for the session's first ask is the request object signed for A's address")

	// On B: the machine's trade under a key of its own, which mints a launcher credential of B's,
	// and the session's enrollment under the same name, which adopts the request left pending.
	keyB := proofKey(t)
	credentialB := rigB.machineCredential(keyB, brokerMovedSession)
	if credentialB == credentialA {
		t.Fatalf("the machine holds the credential %s on both brokers, want one each: its trade at B presents a key of its own", credentialA)
	}
	onB := rigB.enrol(keyB, credentialB, brokerMovedSession+"-b", brokerMovedSession)
	if onB.enrollmentID == onA.enrollmentID {
		t.Fatalf("broker B answered the session the enrollment %s it holds on A, so this test measures nothing: the move must make an enrollment of its own", onA.enrollmentID)
	}
	if held := rigB.held(brokerMovedSession); held.Requests != 1 || held.RequestStates != "pending" || held.RequestEnrollment != onB.enrollmentID {
		t.Fatalf("after the session enrolled on broker B the brokers hold %+v for session %s, want its one request still pending and adopted by B's enrollment %s: the request has to follow its session before B's ask can end it",
			held, brokerMovedSession, onB.enrollmentID)
	}

	// The ask on B: a new request, not the old one. A coalesce onto the old request, or a
	// REQUEST_PENDING refusal naming it, is the session held to a request nobody on B can decide.
	again := requirePendingAsk(rigB, onB, personSecret,
		"a request signed for another broker address does not hold the session's names on broker B")
	if again.RequestID == asked.RequestID || again.Coalesced {
		t.Fatalf("the session's ask on broker B was answered the request %s (coalesced %t), and its ask on A was answered %s; want a new request: the old one is signed for A's address, where B can neither decide it nor let it hold the names",
			again.RequestID, again.Coalesced, asked.RequestID)
	}
	if *again.RecordID == *asked.RecordID {
		t.Fatalf("the new request %s names the old request's record %s, want a record of its own, signed for broker B's address", again.RequestID, *asked.RecordID)
	}

	// The rows, as the test's own handle reads them.
	old := readRequestRow(rigB, asked.RequestID)
	if old.State != "cancelled" || old.DecidedBy != "broker" {
		t.Fatalf("the old request %s is %+v, want cancelled and decided by broker: B's ask ended it", asked.RequestID, old)
	}
	for _, phrase := range brokerMovedPhrases {
		if !strings.Contains(old.Detail, phrase) {
			t.Fatalf("the old request %s's decision_detail is %q, want it to say %q: the session reads it back as why nobody decided its request", asked.RequestID, old.Detail, phrase)
		}
	}
	fresh := readRequestRow(rigB, again.RequestID)
	if fresh.State != "pending" || fresh.EnrollmentID != onB.enrollmentID || fresh.RecordID != *again.RecordID {
		t.Fatalf("the new request %s is %+v, want it pending on broker B's enrollment %s with the record %s it was answered", again.RequestID, fresh, onB.enrollmentID, *again.RecordID)
	}
	requireSignedFor(rigB, *again.RecordID, rigB.base,
		"the new request's record is the request object signed for B's address, which B's own approval page can verify")

	// The end's line: one, on B, naming the four ids; none on A, which ended nothing.
	awaitLogCaughtUp(rigB, brokerB)
	awaitLogCaughtUp(rigA, brokerA)
	lines := recordsWith(brokerB.Log(), brokerMovedMessage, nil)
	if len(lines) != 1 {
		t.Fatalf("broker B logged %d %q lines, want 1, for the one request it ended:\n%s", len(lines), brokerMovedMessage, brokerB.Log())
	}
	t.Logf("broker B's line for the end: %v", lines[0])
	want := map[string]any{
		"severity": "INFO", "request": asked.RequestID, "record_id": *asked.RecordID,
		"new_request": again.RequestID, "enrollment": onB.enrollmentID,
	}
	if line := lines[0]; !carries(line, want) || !slices.Equal(slices.Sorted(maps.Keys(line)), brokerMovedLogKeys) {
		t.Fatalf("broker B's %q line is %v, want %v and no field but %v", brokerMovedMessage, line, want, brokerMovedLogKeys)
	}
	if lines := recordsWith(brokerA.Log(), brokerMovedMessage, nil); len(lines) != 0 {
		t.Fatalf("broker A logged %v, want no %q line: the end is B's, where the session asked again", lines, brokerMovedMessage)
	}

	// The session asks B again: it joins the request it was answered, and nothing more is ended.
	joined := requirePendingAsk(rigB, onB, personSecret,
		"the session's next ask on broker B joins the new request, which B can decide")
	if joined.RequestID != again.RequestID || !joined.Coalesced {
		t.Fatalf("the session's next ask on broker B was answered the request %s (coalesced %t), want it coalesced onto %s", joined.RequestID, joined.Coalesced, again.RequestID)
	}
	if held := rigB.held(brokerMovedSession); held.Requests != 2 || held.RequestStates != "cancelled,pending" ||
		held.RequestEnrollment != onB.enrollmentID+","+onB.enrollmentID {
		t.Fatalf("after the session's next ask the brokers hold %+v for session %s, want the ended request and the one pending request, both on B's enrollment %s",
			held, brokerMovedSession, onB.enrollmentID)
	}
	awaitLogCaughtUp(rigB, brokerB)
	if lines := recordsWith(brokerB.Log(), brokerMovedMessage, nil); len(lines) != 1 {
		t.Fatalf("after the session's next ask broker B has logged %d %q lines, want still 1: a request signed for this broker is no end", len(lines), brokerMovedMessage)
	}
}

// movedAsk is the part of the broker's answer to POST /v1/requests this test reads. podBroker's
// requestReply carries no request id, and the id is what tells the new request from the old one.
type movedAsk struct {
	RequestID string  `json:"request_id"`
	State     string  `json:"state"`
	RecordID  *string `json:"record_id"`
	GrantID   *string `json:"grant_id"`
	Coalesced bool    `json:"coalesced"`
}

// requirePendingAsk has p ask the broker rig drives for secret and answers the request it was
// given, failing unless the broker answered 200 with a pending request on a record and no grant.
// The failure quotes the broker's own answer, so a 409 REQUEST_PENDING reads as one, and proves,
// the sentence the step makes.
func requirePendingAsk(rig carryRig, p pod, secret, proves string) movedAsk {
	rig.t.Helper()
	pods := rig.pods()
	status, body := pods.askRaw(p, secret)
	if status != http.StatusOK {
		rig.t.Fatalf("the session (runtime %s, enrollment %s) asking %s for %s = %d %s, want 200 with a pending request: %s",
			p.runtimeID, p.enrollmentID, rig.base, secret, status, strings.TrimSpace(string(body)), proves)
	}
	var asked movedAsk
	pods.decode(body, &asked)
	if asked.State != "pending" || asked.RequestID == "" || asked.RecordID == nil || asked.GrantID != nil {
		rig.t.Fatalf("the session (runtime %s, enrollment %s) asking %s for %s = %+v, want a pending request on a record and no grant: %s",
			p.runtimeID, p.enrollmentID, rig.base, secret, asked, proves)
	}
	return asked
}

// requestRow is one requests row as the test's own handle reads it: what was decided, by whom,
// why, which enrollment it hangs off, and its record. A column that is null, as decided_by and
// decision_detail are while a request is pending, reads as "".
type requestRow struct {
	State        string
	EnrollmentID string
	DecidedBy    string
	Detail       string
	RecordID     string
}

// readRequestRow reads the request of the id id from the database the two brokers share, which
// is the one place both brokers' writes to it can be read together.
func readRequestRow(rig carryRig, id string) requestRow {
	rig.t.Helper()
	var row requestRow
	if err := rig.db.QueryRow(rig.ctx, `select state, enrollment_id::text, coalesce(decided_by, ''), coalesce(decision_detail, ''), coalesce(record_id, '') from requests where id = $1`, id).
		Scan(&row.State, &row.EnrollmentID, &row.DecidedBy, &row.Detail, &row.RecordID); err != nil {
		rig.t.Fatalf("read request %s: %v", id, err)
	}
	rig.t.Logf("request %s is %+v", id, row)
	return row
}

// requireSignedFor fails unless the request object inside record recordID's canonical body
// verifies for audience, which is the broker address it was signed for. The object is verified
// at the moment the record was written, so its short life is no part of the answer, and in full,
// signature included, so the audience read is the one the session signed.
func requireSignedFor(rig carryRig, recordID, audience, proves string) {
	rig.t.Helper()
	var body string
	var created time.Time
	if err := rig.db.QueryRow(rig.ctx, `select body, created_at from credential_requests where id = $1`, recordID).Scan(&body, &created); err != nil {
		rig.t.Fatalf("read record %s: %v", recordID, err)
	}
	parsed, err := record.ParseBody(body)
	if err != nil {
		rig.t.Fatalf("parse record %s's body: %v", recordID, err)
	}
	if _, err := record.VerifyRequestObject(parsed.Request, audience, movedRecordSkew, created); err != nil {
		rig.t.Fatalf("record %s's request object does not verify for %s: %v; want it to: %s", recordID, audience, err, proves)
	}
}

// awaitLogCaughtUp waits until broker's log holds every line it wrote while answering what this
// test has sent it so far, so that a count of a line read next counts all of them. A line is in
// the log only once the process's stderr has been copied into it, which can come after the answer
// it was written for has reached the test. So this sends broker one call it refuses, a request
// with no proof, and waits for that refusal's own line, which the broker writes after every line
// of the calls answered before it, under the answer id the refusal's body names.
func awaitLogCaughtUp(rig carryRig, broker *brokerProcess) {
	rig.t.Helper()
	pods := rig.pods()
	status, body := pods.post("/v1/requests", nil, map[string]any{})
	var refused struct {
		AnswerID string `json:"answer_id"`
	}
	pods.decode(body, &refused)
	if status != http.StatusUnauthorized || refused.AnswerID == "" {
		rig.t.Fatalf("a request with no proof to %s = %d %s, want 401 naming its answer_id", rig.base, status, strings.TrimSpace(string(body)))
	}
	broker.AwaitRecord(rig.t, answer.RefusalMessage, map[string]any{"answer_id": refused.AnswerID})
}
