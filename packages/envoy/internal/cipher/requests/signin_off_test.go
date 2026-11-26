// signin_off_test.go drives a request machine whose broker signs nobody in (BROKER_SIGNIN=off,
// Machine.SignInOff): a name whose answer would be a person's approval is denied the moment it
// is asked for, by name and naming the setting, because nobody could ever approve it, every name
// the policy grants at once is granted as on any broker, and the sweep denies what a run with
// sign-in on left pending.
package requests

import (
	"context"
	"crypto/ecdsa"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
)

// otherPersonsSecret is an agent-tier secret another person than the fixture's operator owns, so
// the operator's session would have to have that person approve it.
const otherPersonsSecret = "OTHER_PERSON_KEY"

// TestASignInOffBrokerDeniesAtOnceEveryNameThatWouldWait: each way a name comes to need a
// person's approval — the secret's own human tier, another person's agent-tier secret, a shared
// human-tier secret, a name the session's operator withheld from it — is denied at once on a
// broker that signs nobody in, with a reason or without one: no record for anyone's page, no
// grant, no hand-over to the judge, and a detail naming the secret and the setting, so the
// session is not told to ask again with a reason that would change nothing. The same request on
// a broker that signs people in waits for its approver, which is what makes the denial the
// switch's doing and not the name's.
func TestASignInOffBrokerDeniesAtOnceEveryNameThatWouldWait(t *testing.T) {
	for _, tc := range []struct {
		name, secret, reason string
		withheld             bool
	}{
		{name: "a human-tier credential", secret: "DEEL_API_KEY", reason: "rotating the key"},
		{name: "a human-tier credential asked for with no reason", secret: "DEEL_API_KEY"},
		{name: "another person's agent-tier credential", secret: otherPersonsSecret, reason: "reading the other person's key"},
		{name: "a shared human-tier credential", secret: "SHARED_KEY", reason: "reading the shared key"},
		{name: "a name the session's operator withheld from it", secret: "AUTO_TOKEN", reason: "taking it back", withheld: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, enr, key, _ := newFixture(t)
			retag(t, m, policytest.Secret(otherPersonsSecret, otherPerson, policy.TierAgent, "other-person-v1"))
			if tc.withheld {
				withholdFromSession(t, m, enr, key, tc.secret)
			}
			var judged []PendingRequest
			m.Judge = func(p PendingRequest) { judged = append(judged, p) }
			m.SignInOff = true

			req, err := m.Create(ctx, enr, signRequest(t, m, key, tc.reason, tc.secret), "")
			if err != nil {
				t.Fatalf("Create(%s): %v", tc.secret, err)
			}
			want := tc.secret + ": " + policy.ReasonSignInOff
			if req.State != "denied" || req.GrantID != nil || req.RecordID != nil || req.Detail == nil || *req.Detail != want {
				t.Fatalf("Create(%s) = %+v (detail %v); want it denied with no grant and no record, reading %q", tc.secret, req, req.Detail, want)
			}
			if len(req.Secrets) != 1 || req.Secrets[0].Decision != policy.Deny {
				t.Fatalf("decisions = %+v; want %s decided %s", req.Secrets, tc.secret, policy.Deny)
			}
			if rows := recordRows(t, m); rows != 0 {
				t.Fatalf("credential_requests rows = %d, want 0: a broker nobody signs in to opens no card", rows)
			}
			if len(judged) != 0 {
				t.Fatalf("the judge was handed %+v; want nothing, since nothing waits", judged)
			}
			if state, _, _ := createdAudit(t, m, req.ID); state != "denied" {
				t.Fatalf("request.created audit state = %q, want denied", state)
			}

			m.SignInOff = false
			reason := tc.reason
			if reason == "" {
				reason = "with a reason this time"
			}
			waiting, err := m.Create(ctx, enr, signRequest(t, m, key, reason, tc.secret), "")
			if err != nil || waiting.State != "pending" || waiting.RecordID == nil {
				t.Fatalf("Create(%s) with sign-in on = %+v, %v; want it to wait for its approver", tc.secret, waiting, err)
			}
		})
	}
}

// TestASignInOffBrokerGrantsWhatThePolicyGrantsAtOnce: the operator's own agent-tier secret for
// the operator's session, and a service's own secret for a pod of that service, are granted and
// released on a broker that signs nobody in exactly as on one that does, and a request naming
// one of them beside a name that would wait is denied whole, naming the waiting one alone.
func TestASignInOffBrokerGrantsWhatThePolicyGrantsAtOnce(t *testing.T) {
	ctx := context.Background()
	m, enr, key, _ := newFixture(t)
	m.SignInOff = true
	pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-"+t.Name())

	for name, ask := range map[string]struct {
		enrollment    string
		key           *ecdsa.PrivateKey
		secret, value string
	}{
		"the operator's own agent-tier secret": {enr, key, "AUTO_TOKEN", "auto-v1"},
		"a service's own secret for its pod":   {pod, podKey, "SERVICE_KEY", "service-v1"},
	} {
		granted, err := m.Create(ctx, ask.enrollment, signRequest(t, m, ask.key, "", ask.secret), "")
		if err != nil || granted.State != "granted" || granted.GrantID == nil || granted.Detail != nil {
			t.Fatalf("%s: Create(%s) = %+v, %v; want it granted at once", name, ask.secret, granted, err)
		}
		if released, err := m.Values(ctx, *granted.GrantID, ask.enrollment, ReleaseCall{}); err != nil || released.Values[ask.secret] != ask.value {
			t.Fatalf("%s: Values = %v, %v; want %s=%s", name, released.Values, err, ask.secret, ask.value)
		}
	}

	both, err := m.Create(ctx, enr, signRequest(t, m, key, "both of them", "AUTO_TOKEN", "DEEL_API_KEY"), "")
	if err != nil {
		t.Fatalf("Create(AUTO_TOKEN, DEEL_API_KEY): %v", err)
	}
	want := "DEEL_API_KEY: " + policy.ReasonSignInOff
	if both.State != "denied" || both.GrantID != nil || both.Detail == nil || *both.Detail != want {
		t.Fatalf("Create(AUTO_TOKEN, DEEL_API_KEY) = %+v (detail %v); want it denied whole, reading %q", both, both.Detail, want)
	}
	if len(both.Secrets) != 2 || both.Secrets[0].Decision != policy.Automatic || both.Secrets[1].Decision != policy.Deny {
		t.Fatalf("decisions = %+v; want AUTO_TOKEN %s and DEEL_API_KEY %s", both.Secrets, policy.Automatic, policy.Deny)
	}
}

// anotherBroker is the public URL of a second broker sharing the fixture's database.
const anotherBroker = "https://another-broker.internal.example"

// TestTheSweepOfASignInOffBrokerDeniesWhatAnEarlierRunLeftPending: a database a broker with
// sign-in on wrote can hold requests still waiting for a person when the broker restarts with
// sign-in off, or while one replica of a rolling change still has it on. Nobody can sign in to
// decide them on this pod, so the sweep denies each of them at once, naming the setting, closes
// its record with a denied event, writes its request.denied audit row and wakes its session,
// rather than leaving a link to a page that is gone in the approver's Inbox until the deadline.
// It does so whatever the database's clock says the request was written at, and for one a pod
// with sign-in on writes while this pod runs too. A grant a person approved before is left as it
// was, and so is a request signed for another broker sharing the database, which that broker
// still lists. The control is the same sweep with sign-in on, which leaves every request pending.
func TestTheSweepOfASignInOffBrokerDeniesWhatAnEarlierRunLeftPending(t *testing.T) {
	ctx := context.Background()
	m, enr, key, approver := newFixture(t)
	other, otherKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
	owned, err := m.Create(ctx, enr, signRequest(t, m, key, "rotating the key", "DEEL_API_KEY"), "")
	if err != nil || owned.State != "pending" || owned.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) = %+v, %v; want it pending", owned, err)
	}
	shared, err := m.Create(ctx, other, signRequest(t, m, otherKey, "reading the shared key", "SHARED_KEY"), "")
	if err != nil || shared.State != "pending" || shared.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v; want it pending", shared, err)
	}
	approved, err := m.Create(ctx, other, signRequest(t, m, otherKey, "approved before", "DEEL_API_KEY"), "")
	if err != nil || approved.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY for the other box) = %+v, %v; want it pending", approved, err)
	}
	decided, err := m.ApplyDecision(ctx, *approved.RecordID, true, approver)
	if err != nil || decided.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v; want a grant", decided, err)
	}
	// A request another broker sharing this database wrote, signed for its own public URL: that
	// broker still serves its approval page, so this one's sweep must leave it alone.
	third, thirdKey := newEnrollment(t, m.Store, "session", "box-c-"+t.Name(), new(fixtureOperator), nil)
	m.Audience = anotherBroker
	foreign, err := m.Create(ctx, third, signRequest(t, m, thirdKey, "asked of the other broker", "DEEL_API_KEY"), "")
	m.Audience = testAudience
	if err != nil || foreign.State != "pending" || foreign.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) for another broker = %+v, %v; want it pending", foreign, err)
	}

	var woken []wakeCall
	sweeper := &Sweeper{Enrollments: &enroll.Service{Store: m.Store}, Machine: m, MachineLogins: &machine.Service{Store: m.Store}, OAuth: oauthstore.New(m.Store.Pool), Interval: time.Hour,
		Wake: func(_ context.Context, enrollmentID, requestID, state string) {
			woken = append(woken, wakeCall{enrollmentID, requestID, state})
		}}
	sweeper.Tick(ctx)
	for _, req := range []Request{owned, shared, foreign} {
		if got, err := m.Get(ctx, req.ID); err != nil || got.State != "pending" {
			t.Fatalf("with sign-in on, request %s after a sweep = %+v, %v; want still pending", req.ID, got, err)
		}
	}
	if len(woken) != 0 {
		t.Fatalf("with sign-in on, the sweep woke %+v; want no one", woken)
	}

	// The broker's clock and the database's need not agree: a request row whose created_at, the
	// database's own time, is an hour ahead of the sweep's clock is denied all the same.
	if _, err := m.Store.Pool.Exec(ctx, `update requests set created_at = now() + interval '1 hour' where id = $1`, owned.ID); err != nil {
		t.Fatalf("move %s's created_at ahead: %v", owned.ID, err)
	}
	m.SignInOff = true
	sweeper.Tick(ctx)
	for _, req := range []Request{owned, shared} {
		got, err := m.Get(ctx, req.ID)
		if err != nil || got.State != "denied" || got.GrantID != nil || got.Detail == nil || *got.Detail != policy.ReasonSignInOff {
			t.Fatalf("with sign-in off, request %s after a sweep = %+v (detail %v), %v; want denied, reading %q", req.ID, got, got.Detail, err, policy.ReasonSignInOff)
		}
		var event string
		if err := m.Store.Pool.QueryRow(ctx, `select event from credential_request_events where record_id=$1`, *req.RecordID).Scan(&event); err != nil || event != "denied" {
			t.Fatalf("record %s's event = %q, %v; want denied", *req.RecordID, event, err)
		}
		if state, _, _ := createdAudit(t, m, req.ID); state != "pending" {
			t.Fatalf("request.created audit state = %q, want pending: the sweep adds a row, it rewrites none", state)
		}
		var audits int
		if err := m.Store.Pool.QueryRow(ctx, `select count(*) from audit where request_id=$1 and kind='request.denied'`, req.ID).Scan(&audits); err != nil || audits != 1 {
			t.Fatalf("request %s's request.denied audit rows = %d, %v; want 1", req.ID, audits, err)
		}
	}
	wantWoken := map[string]string{owned.ID: enr, shared.ID: other}
	for _, w := range woken {
		if want, ok := wantWoken[w.requestID]; !ok || w.state != "denied" || w.enrollmentID != want {
			t.Fatalf("the sweep woke %+v; want each of %v once, denied, and nothing else", woken, map[string]string{owned.ID: enr, shared.ID: other})
		}
		delete(wantWoken, w.requestID)
	}
	if len(wantWoken) != 0 {
		t.Fatalf("the sweep woke %+v; it never woke %v", woken, wantWoken)
	}
	if got, err := m.Get(ctx, foreign.ID); err != nil || got.State != "pending" {
		t.Fatalf("the request signed for another broker on this database = %+v, %v; want it left pending for that broker", got, err)
	}
	m.Audience = anotherBroker
	listed, err := m.PendingForApprover(ctx, approver, PendingStates{})
	m.Audience = testAudience
	if err != nil || len(listed) != 1 || listed[0].RecordID != *foreign.RecordID {
		t.Fatalf("the other broker's list for %s = %+v, %v; want its own record %s still there to decide", approver, listed, err, *foreign.RecordID)
	}
	if pending, err := m.PendingForApprover(ctx, approver, PendingStates{}); err != nil || len(pending) != 0 {
		t.Fatalf("PendingForApprover(%s) = %+v, %v; want nothing left to decide", approver, pending, err)
	}
	if released, err := m.Values(ctx, decided.GrantID, other, ReleaseCall{}); err != nil || released.Values["DEEL_API_KEY"] != "deel-v1" {
		t.Fatalf("Values of the grant approved before = %v, %v; want DEEL_API_KEY=deel-v1", released.Values, err)
	}
	woken = nil
	sweeper.Tick(ctx)
	if len(woken) != 0 {
		t.Fatalf("a second sweep woke %+v; want no one, nothing being pending", woken)
	}

	// A roll between the two settings: a broker pod with sign-in on, sharing the database and the
	// public URL, writes a request while this pod runs. This pod's next sweep denies it too, as
	// operate.md says, since no bound a process takes can tell the overlap apart from a restart.
	m.SignInOff = false
	fresh, err := m.Create(ctx, enr, signRequest(t, m, key, "asked of a pod with sign-in on", "DEEL_API_KEY"), "")
	m.SignInOff = true
	if err != nil || fresh.State != "pending" || fresh.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) on a pod with sign-in on = %+v, %v; want it pending", fresh, err)
	}
	sweeper.Tick(ctx)
	if got, err := m.Get(ctx, fresh.ID); err != nil || got.State != "denied" || len(woken) != 1 || woken[0].requestID != fresh.ID || woken[0].state != "denied" {
		t.Fatalf("the request a pod with sign-in on wrote while this pod runs = %+v, %v (woken %+v); want it denied at the next sweep, its session woken denied", got, err, woken)
	}
}
