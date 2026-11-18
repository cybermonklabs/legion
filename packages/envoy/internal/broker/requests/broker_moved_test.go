// broker_moved_test.go holds what Create does with a pending request whose record is signed for
// another broker address. A session asks a broker of another BROKER_PUBLIC_URL that shares this
// database, moves to this broker, and asks again here. No person and no judge here can decide the
// request it left waiting, since this broker lists that record for nobody, so Create ends it, as
// the broker that moved, and then applies its one-pending-request rule to what is left.
//
// A request of that other broker is planted as that broker writes one: m.Audience is set to
// anotherBroker around its Create, as TestPendingForApproverLeavesOutARecordThatDoesNotVerify
// does, so its request object and its record name the other address. This file names only what
// the package's tests named before Create ended such a request, and reads the ended request's
// detail for its two phrases rather than its whole text, so the same file compiles and runs
// against the code from before the change: there every case but the one of a request signed for
// this broker fails.
package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// askOfAnotherBroker is a pending request for names that enrollment enr's session made of
// anotherBroker: its request object and its record are signed for that broker's address. m answers
// for testAudience again before it returns, whatever the Create answered.
func askOfAnotherBroker(t *testing.T, m *Machine, enr string, key *ecdsa.PrivateKey, names ...string) Request {
	t.Helper()
	m.Audience = anotherBroker
	req, err := m.Create(context.Background(), enr, signRequest(t, m, key, "asked of the other broker", names...), "", nil)
	m.Audience = testAudience
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create(%v) on %s = %+v, %v; want a pending request with a record", names, anotherBroker, req, err)
	}
	return req
}

// stripeKey is a second human-tier secret of the fixture's operator, which retagStripeKey puts
// into the policy, so an ask for it beside DEEL_API_KEY waits on the one approver both have.
const stripeKey = "STRIPE_KEY"

// retagStripeKey puts stripeKey into the fixture's policy, its value stripe-v1.
func retagStripeKey(t *testing.T, m *Machine) {
	t.Helper()
	retag(t, m, policytest.Secret(stripeKey, fixtureOperator, policy.TierHuman, "stripe-v1"))
}

// saysTheBrokerMoved reports whether detail says what an end of a request signed for another
// broker address says: that the broker moved, and that the request was signed for another
// broker address.
func saysTheBrokerMoved(detail string) bool {
	return strings.Contains(detail, "broker moved") && strings.Contains(detail, "another broker address")
}

// requireEndedAsBrokerMoved fails t unless request req, which was pending, was ended as a request
// signed for another broker address: cancelled and decided by the broker, its detail saying so
// (saysTheBrokerMoved), with one request.cancelled audit row whose actor is the broker and whose
// reason is that detail, and a cancelled event of its record by the broker carrying the same
// detail.
func requireEndedAsBrokerMoved(t *testing.T, m *Machine, req Request) {
	t.Helper()
	ctx := context.Background()
	var state string
	var decidedBy, detail *string
	if err := m.Store.Pool.QueryRow(ctx, `select state, decided_by, decision_detail from requests where id=$1`, req.ID).
		Scan(&state, &decidedBy, &detail); err != nil {
		t.Fatalf("read request %s: %v", req.ID, err)
	}
	if state != "cancelled" || deref(decidedBy) != "broker" || !saysTheBrokerMoved(deref(detail)) {
		t.Fatalf("request %s is %s, decided by %q, with the detail %q; want it cancelled by the broker, saying the broker moved and it was signed for another broker address",
			req.ID, state, deref(decidedBy), deref(detail))
	}
	var audits int
	var actor, reason string
	if err := m.Store.Pool.QueryRow(ctx, `select count(*), coalesce(min(actor), ''), coalesce(min(detail->>'reason'), '')
		from audit where request_id=$1 and kind='request.cancelled'`, req.ID).Scan(&audits, &actor, &reason); err != nil {
		t.Fatalf("read request %s's request.cancelled audit row: %v", req.ID, err)
	}
	if audits != 1 || actor != "broker" || reason != *detail {
		t.Fatalf("request %s has %d request.cancelled audit rows, by %q, with the reason %q; want one, by the broker, whose reason is the request's detail %q",
			req.ID, audits, actor, reason, *detail)
	}
	var events int
	var eventActor, eventDetail string
	if err := m.Store.Pool.QueryRow(ctx, `select count(*), coalesce(min(actor), ''), coalesce(min(detail), '')
		from credential_request_events where record_id=$1 and event='cancelled'`, *req.RecordID).Scan(&events, &eventActor, &eventDetail); err != nil {
		t.Fatalf("read record %s's cancelled event: %v", *req.RecordID, err)
	}
	if events != 1 || eventActor != "broker" || eventDetail != *detail {
		t.Fatalf("record %s has %d cancelled events, by %q, with the detail %q; want one, by the broker, carrying the request's detail %q",
			*req.RecordID, events, eventActor, eventDetail, *detail)
	}
}

// requireStillPending fails t unless request req is still pending, with no audit row and no
// record event that ended it.
func requireStillPending(t *testing.T, m *Machine, req Request) {
	t.Helper()
	ctx := context.Background()
	var state string
	var ends, events int
	if err := m.Store.Pool.QueryRow(ctx, `select state,
		(select count(*) from audit where request_id=$1 and kind <> 'request.created'),
		(select count(*) from credential_request_events where record_id=$2)
		from requests where id=$1`, req.ID, *req.RecordID).Scan(&state, &ends, &events); err != nil {
		t.Fatalf("read request %s: %v", req.ID, err)
	}
	if state != "pending" || ends != 0 || events != 0 {
		t.Fatalf("request %s is %s, with %d audit rows past its creation and %d record events; want it pending and untouched", req.ID, state, ends, events)
	}
}

// requireJudgedAlone fails t unless the judge was handed request id, once, and nothing else.
func requireJudgedAlone(t *testing.T, watched *judged, id string) {
	t.Helper()
	if len(watched.handed) != 1 || watched.handed[0].RequestID != id {
		t.Fatalf("the judge was handed %+v; want request %s alone, once", watched.handed, id)
	}
}

// pendingOf is the ids of the requests enrollment enr holds pending now.
func pendingOf(t *testing.T, m *Machine, enr string) []string {
	t.Helper()
	var ids []string
	if err := m.Store.Pool.QueryRow(context.Background(), `select coalesce(array_agg(id::text order by created_at), '{}')
		from requests where enrollment_id=$1 and state='pending'`, enr).Scan(&ids); err != nil {
		t.Fatalf("read the requests %s holds pending: %v", enr, err)
	}
	return ids
}

// TestAskingAgainWithTheSameNamesEndsARequestSignedForAnotherBroker: the session asks this broker
// for the very names and lifetime of the request it left waiting on another broker. That request
// is ended, with the broker's reason on its row, its audit row and its record's event, and the
// session gets a new request of this broker, which is not coalesced onto the old one, is handed to
// the judge, and is the one its approver's list shows here.
func TestAskingAgainWithTheSameNamesEndsARequestSignedForAnotherBroker(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	stale := askOfAnotherBroker(t, m, enr, key, "DEEL_API_KEY")
	watched := watchJudge(m)

	again, err := m.Create(ctx, enr, signRequest(t, m, key, "asked again on this broker", "DEEL_API_KEY"), "", nil)
	if err != nil || again.ID == stale.ID || again.Coalesced || again.State != "pending" || again.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) again = %+v, %v; want a new pending request, not coalesced onto %s", again, err, stale.ID)
	}
	requireEndedAsBrokerMoved(t, m, stale)
	requireJudgedAlone(t, watched, again.ID)
	listed, err := m.PendingForApprover(ctx, approver)
	if err != nil || len(listed) != 1 || listed[0].RecordID != *again.RecordID {
		t.Fatalf("PendingForApprover(%s) = %+v, %v; want the new request's record %s alone", approver, listed, err, *again.RecordID)
	}
}

// TestAskingAgainWithOverlappingNamesEndsARequestSignedForAnotherBroker: an ask that overlaps the
// request left waiting on another broker without matching it, by another lifetime or by more
// names, was refused REQUEST_PENDING before; it ends that request and gets a new one all the same.
func TestAskingAgainWithOverlappingNamesEndsARequestSignedForAnotherBroker(t *testing.T) {
	for _, c := range []struct {
		name  string
		names []string
		ask   func(t *testing.T, m *Machine, key *ecdsa.PrivateKey) string
	}{
		{"another lifetime", []string{"DEEL_API_KEY"}, func(t *testing.T, m *Machine, key *ecdsa.PrivateKey) string {
			return signRequestFor(t, m, key, 600, "DEEL_API_KEY")
		}},
		{"more names", []string{"DEEL_API_KEY", stripeKey}, func(t *testing.T, m *Machine, key *ecdsa.PrivateKey) string {
			return signRequest(t, m, key, "asked again for more", "DEEL_API_KEY", stripeKey)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key, _ := newFixture(t)
			retagStripeKey(t, m)
			stale := askOfAnotherBroker(t, m, enr, key, "DEEL_API_KEY")
			watched := watchJudge(m)

			again, err := m.Create(context.Background(), enr, c.ask(t, m, key), "", nil)
			if err != nil || again.ID == stale.ID || again.Coalesced || again.State != "pending" || again.RecordID == nil {
				t.Fatalf("Create(%v) with %s = %+v, %v; want a new pending request, not refused for %s", c.names, c.name, again, err, stale.ID)
			}
			if len(again.Secrets) != len(c.names) {
				t.Fatalf("the new request names %+v; want %v", again.Secrets, c.names)
			}
			requireEndedAsBrokerMoved(t, m, stale)
			requireJudgedAlone(t, watched, again.ID)
		})
	}
}

// TestAskingAgainLeavesARequestSignedForThisBrokerAsItIs: the control. A request this broker
// wrote, whose record verifies here, is the one-pending-request rule's as before: the same ask
// coalesces onto it and is handed to no judge, an overlapping one is refused REQUEST_PENDING
// naming it, and it stays pending, ended by nobody.
func TestAskingAgainLeavesARequestSignedForThisBrokerAsItIs(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	retagStripeKey(t, m)
	watched := watchJudge(m)
	own, err := m.Create(ctx, enr, signRequest(t, m, key, "asked of this broker", "DEEL_API_KEY"), "", nil)
	if err != nil || own.State != "pending" || own.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) = %+v, %v; want a pending request with a record", own, err)
	}

	same, err := m.Create(ctx, enr, signRequest(t, m, key, "asked again", "DEEL_API_KEY"), "", nil)
	if err != nil || !same.Coalesced || same.ID != own.ID {
		t.Fatalf("Create(DEEL_API_KEY) again = %+v, %v; want it coalesced onto %s", same, err, own.ID)
	}
	for name, overlapping := range map[string]string{
		"another lifetime": signRequestFor(t, m, key, 600, "DEEL_API_KEY"),
		"more names":       signRequest(t, m, key, "asked again for more", "DEEL_API_KEY", stripeKey),
	} {
		if req, err := m.Create(ctx, enr, overlapping, "", nil); !errors.Is(err, ErrRequestPending) || !strings.Contains(err.Error(), own.ID) {
			t.Fatalf("Create with %s = %+v, %v; want ErrRequestPending naming %s", name, req, err, own.ID)
		}
	}
	requireStillPending(t, m, own)
	requireJudgedAlone(t, watched, own.ID)
}

// TestAskingAgainEndsEveryStaleRequestItOverlaps: one enrollment can hold two pending requests of
// another broker for different names, when the session's box adopts what two earlier boxes held.
// One ask that overlaps both ends both and makes one new request, the only one left pending.
func TestAskingAgainEndsEveryStaleRequestItOverlaps(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	retagStripeKey(t, m)
	first := askOfAnotherBroker(t, m, enr, key, "DEEL_API_KEY")
	earlier, earlierKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
	second := askOfAnotherBroker(t, m, earlier, earlierKey, stripeKey)
	moveSession(t, m, earlier, enr)
	watched := watchJudge(m)

	again, err := m.Create(context.Background(), enr, signRequest(t, m, key, "asked again for both", "DEEL_API_KEY", stripeKey), "", nil)
	if err != nil || again.Coalesced || again.State != "pending" || again.ID == first.ID || again.ID == second.ID {
		t.Fatalf("Create(DEEL_API_KEY, %s) = %+v, %v; want one new pending request, not refused for %s or %s", stripeKey, again, err, first.ID, second.ID)
	}
	requireEndedAsBrokerMoved(t, m, first)
	requireEndedAsBrokerMoved(t, m, second)
	if pending := pendingOf(t, m, enr); len(pending) != 1 || pending[0] != again.ID {
		t.Fatalf("the enrollment holds %v pending; want the new request %s alone", pending, again.ID)
	}
	requireJudgedAlone(t, watched, again.ID)
}

// TestAStaleRequestBeforeOneThatVerifies: one enrollment holds a request of another broker and,
// after it, an overlapping request of this broker, which an adoption of two earlier boxes' requests
// can make. The ask ends the stale one whichever way it then goes, and the rule applies to the one
// that verifies: the same names and lifetime coalesce onto it, with no judge run, and another
// lifetime is refused REQUEST_PENDING naming it, the stale request's end committed all the same.
func TestAStaleRequestBeforeOneThatVerifies(t *testing.T) {
	for _, c := range []struct {
		name      string
		coalesces bool
		ask       func(t *testing.T, m *Machine, key *ecdsa.PrivateKey) string
	}{
		{"the same names and lifetime as the request that verifies", true, func(t *testing.T, m *Machine, key *ecdsa.PrivateKey) string {
			return signRequest(t, m, key, "asked again", "DEEL_API_KEY")
		}},
		{"another lifetime", false, func(t *testing.T, m *Machine, key *ecdsa.PrivateKey) string {
			return signRequestFor(t, m, key, 600, "DEEL_API_KEY")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key, _ := newFixture(t)
			ctx := context.Background()
			stale := askOfAnotherBroker(t, m, enr, key, "DEEL_API_KEY")
			earlier, earlierKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
			verifies, err := m.Create(ctx, earlier, signRequest(t, m, earlierKey, "asked of this broker", "DEEL_API_KEY"), "", nil)
			if err != nil || verifies.State != "pending" || verifies.RecordID == nil {
				t.Fatalf("Create(DEEL_API_KEY) on the earlier box = %+v, %v; want a pending request with a record", verifies, err)
			}
			moveSession(t, m, earlier, enr)
			watched := watchJudge(m)

			got, err := m.Create(ctx, enr, c.ask(t, m, key), "", nil)
			if c.coalesces {
				if err != nil || !got.Coalesced || got.ID != verifies.ID {
					t.Fatalf("Create = %+v, %v; want it coalesced onto %s, the request that verifies, and not onto %s", got, err, verifies.ID, stale.ID)
				}
			} else if !errors.Is(err, ErrRequestPending) || !strings.Contains(err.Error(), verifies.ID) {
				t.Fatalf("Create = %+v, %v; want ErrRequestPending naming %s, the request that verifies, and not %s", got, err, verifies.ID, stale.ID)
			}
			requireEndedAsBrokerMoved(t, m, stale)
			requireStillPending(t, m, verifies)
			if len(watched.handed) != 0 {
				t.Fatalf("the judge was handed %+v; want nothing, no request having been written", watched.handed)
			}
		})
	}
}

// TestAStaleRequestDecidedOnTheOtherBrokerMeanwhile: the broker the request was signed for decides
// it while Create is about to end it. That decision holds the request's row until it commits, as
// applyDecisionAttempt does, so Create's end waits for it, then finds the request no longer pending
// and ends nothing: the decision stands, and the session gets a new request of this broker.
func TestAStaleRequestDecidedOnTheOtherBrokerMeanwhile(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	stale := askOfAnotherBroker(t, m, enr, key, "DEEL_API_KEY")
	watched := watchJudge(m)

	decision, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer decision.Rollback(ctx)
	if _, err := decision.Exec(ctx, `select 1 from requests where id=$1 for update`, stale.ID); err != nil {
		t.Fatalf("hold the request's row as the other broker's decision does: %v", err)
	}
	type result struct {
		req Request
		err error
	}
	done := make(chan result, 1)
	again := signRequest(t, m, key, "asked again on this broker", "DEEL_API_KEY")
	go func() {
		req, err := m.Create(ctx, enr, again, "", nil)
		done <- result{req, err}
	}()
	storetest.AwaitLockWaiters(t, m.Store.Pool, decision, 1)
	if _, err := decision.Exec(ctx, `update requests set state='granted', decided_at=now(), decided_by=$2 where id=$1`, stale.ID, approver); err != nil {
		t.Fatalf("decide the request as the other broker: %v", err)
	}
	if err := decision.Commit(ctx); err != nil {
		t.Fatalf("commit the other broker's decision: %v", err)
	}

	got := <-done
	if got.err != nil || got.req.ID == stale.ID || got.req.Coalesced || got.req.State != "pending" {
		t.Fatalf("Create(DEEL_API_KEY) again = %+v, %v; want a new pending request", got.req, got.err)
	}
	var state string
	var ends, events int
	if err := m.Store.Pool.QueryRow(ctx, `select state,
		(select count(*) from audit where request_id=$1 and kind='request.cancelled'),
		(select count(*) from credential_request_events where record_id=$2)
		from requests where id=$1`, stale.ID, *stale.RecordID).Scan(&state, &ends, &events); err != nil {
		t.Fatalf("read request %s: %v", stale.ID, err)
	}
	if state != "granted" || ends != 0 || events != 0 {
		t.Fatalf("request %s is %s, with %d request.cancelled audit rows and %d record events; want it granted by the other broker and ended by nobody", stale.ID, state, ends, events)
	}
	requireJudgedAlone(t, watched, got.req.ID)
}
