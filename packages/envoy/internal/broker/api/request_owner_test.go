package api_test

// request_owner_test.go drives who may read and cancel a request over the routes a session calls:
// any enrollment of the principal that made it — the same person, the same Legion claim within
// the launcher's service, the same machine — and, for an enrollment that records no owner, that
// enrollment alone. It also drives what the claim rule hands the next pod
// of a claim over HTTP: the value a person approved for the pod before it, and the one pending
// request an equal ask joins.

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/exchange"
)

// plannerClaim and implementerClaim are two role claims of one issue, as the Legion daemon spells
// its claim tokens: one service launches both.
const (
	plannerClaim     = "legion-acme-acme-12-planner"
	implementerClaim = "legion-acme-acme-12-implementer"
)

// launcherLogin is one launcher credential and the key its proofs are signed with.
type launcherLogin struct {
	id  string
	key *ecdsa.PrivateKey
}

// enrolled is one enrollment and the key its session proofs are signed with.
type enrolled struct {
	id  string
	key *ecdsa.PrivateKey
}

// enrolPod enrols a worker pod as the Legion daemon enrols a role process: a pod uid of its own,
// its projected token, a session of its own and the role's claim.
func (ts *testServer) enrolPod(t *testing.T, launcher launcherLogin, claim, session string) enrolled {
	t.Helper()
	return ts.enrolPodWith(t, launcher, podEnrolment{account: workerAccount, podUID: uuid.NewString(), claim: claim, session: session})
}

// podEnrolment is what a launcher states for one role process of a pod: the account the pod runs
// as, its uid, the slot of the process ("" for none), its claim ("" for none) and its session.
type podEnrolment struct {
	account, podUID, slot, claim, session string
}

// enrolPodWith enrols the role process p describes under launcher, with the pod's projected token.
func (ts *testServer) enrolPodWith(t *testing.T, launcher launcherLogin, p podEnrolment) enrolled {
	t.Helper()
	key := newSigningKey(t)
	status, body := ts.launcher(t, launcher.key, launcher.id, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": p.podUID, "slot": p.slot, "thumbprint": thumbprintOf(t, key),
		"pod_token": ts.podToken(t, p.account, p.podUID), "session_id": p.session, "claim": p.claim,
	})
	if status != http.StatusCreated {
		t.Fatalf("enrol pod %s slot %q of claim %q under session %s = %d %s, want 201", p.podUID, p.slot, p.claim, p.session, status, body)
	}
	return enrolled{id: decode[wireEnrolled](t, body).EnrollmentID, key: key}
}

// enrolSession enrols a session on a machine under launcher, naming session ("" for none).
func (ts *testServer) enrolSession(t *testing.T, launcher launcherLogin, session string) enrolled {
	t.Helper()
	key := newSigningKey(t)
	body := map[string]any{"kind": "session", "runtime_id": uuid.NewString(), "thumbprint": thumbprintOf(t, key)}
	if session != "" {
		body["session_id"] = session
	}
	status, answer := ts.launcher(t, launcher.key, launcher.id, http.MethodPost, "/v1/enrollments", body)
	if status != http.StatusCreated {
		t.Fatalf("enrol a session under launcher %s = %d %s, want 201", launcher.id, status, answer)
	}
	return enrolled{id: decode[wireEnrolled](t, answer).EnrollmentID, key: key}
}

// askAs asks for names as e and answers the broker's reply.
func (ts *testServer) askAs(t *testing.T, e enrolled, names ...string) wireCreateRequestResponse {
	t.Helper()
	status, body := ts.session(t, e.key, e.id, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, e.key, ts.URL, "checking who a request belongs to", names...), "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests as %s = %d %s", e.id, status, body)
	}
	return decode[wireCreateRequestResponse](t, body)
}

// askPending is askAs, failing t unless the broker left the request pending on a record.
func (ts *testServer) askPending(t *testing.T, e enrolled, names ...string) wireCreateRequestResponse {
	t.Helper()
	created := ts.askAs(t, e, names...)
	if created.State != "pending" || created.RecordID == nil {
		t.Fatalf("the request = %+v, want it pending on a record", created)
	}
	return created
}

// readAs is GET /v1/requests/{id} as e.
func (ts *testServer) readAs(t *testing.T, e enrolled, request string) (int, []byte) {
	t.Helper()
	return ts.session(t, e.key, e.id, http.MethodGet, "/v1/requests/"+request, nil)
}

// cancelAs is POST /v1/requests/{id}/cancel as e.
func (ts *testServer) cancelAs(t *testing.T, e enrolled, request string) (int, []byte) {
	t.Helper()
	return ts.session(t, e.key, e.id, http.MethodPost, "/v1/requests/"+request+"/cancel", nil)
}

// wantState fails t unless e reads request in state.
func (ts *testServer) wantState(t *testing.T, e enrolled, request, state string) wireRequestStatus {
	t.Helper()
	status, body := ts.readAs(t, e, request)
	read := decode[wireRequestStatus](t, body)
	if status != http.StatusOK || read.State != state {
		t.Fatalf("GET /v1/requests/%s as %s = %d %s, want 200 %s", request, e.id, status, body, state)
	}
	return read
}

// wantNotYours fails t unless e is refused both reading and cancelling request 403 NOT_YOURS.
func (ts *testServer) wantNotYours(t *testing.T, e enrolled, request string) {
	t.Helper()
	for route, call := range map[string]func(*testing.T, enrolled, string) (int, []byte){
		"GET /v1/requests/{id}":         ts.readAs,
		"POST /v1/requests/{id}/cancel": ts.cancelAs,
	} {
		if status, body := call(t, e, request); status != http.StatusForbidden || decode[wireError](t, body).Code != "NOT_YOURS" {
			t.Fatalf("%s for request %s as %s = %d %s, want 403 NOT_YOURS", route, request, e.id, status, body)
		}
	}
}

// wantCancelled fails t unless e cancels request.
func (ts *testServer) wantCancelled(t *testing.T, e enrolled, request string) {
	t.Helper()
	status, body := ts.cancelAs(t, e, request)
	if status != http.StatusOK || decode[struct {
		State string `json:"state"`
	}](t, body).State != "cancelled" {
		t.Fatalf("POST /v1/requests/%s/cancel as %s = %d %s, want 200 cancelled", request, e.id, status, body)
	}
}

// serviceLogin is the Legion daemon's own token login, under which it enrols every worker pod.
func (ts *testServer) serviceLogin(t *testing.T) launcherLogin {
	t.Helper()
	id, key := ts.mintServiceLauncherCredential(t)
	return launcherLogin{id: id, key: key}
}

// TestALaterEnrollmentOfTheSameClaimReadsAndCancels: a request a pod of a claim made is the
// claim's, so another pod of the claim, under a session of its own, reads it and cancels it while
// the first pod is still running and holds it, and the first pod reads the cancel.
func TestALaterEnrollmentOfTheSameClaimReadsAndCancels(t *testing.T) {
	ts := newTestServer(t)
	daemon := ts.serviceLogin(t)
	first := ts.enrolPod(t, daemon, plannerClaim, "planner-session-1")
	created := ts.askPending(t, first, "DEEL_API_KEY")

	next := ts.enrolPod(t, daemon, plannerClaim, "planner-session-2")
	ts.wantState(t, next, created.RequestID, "pending")
	ts.wantCancelled(t, next, created.RequestID)
	ts.wantState(t, first, created.RequestID, "cancelled")
}

// TestAnotherClaimOfTheSameServiceCannotCancel: the claim, not the service, is whose a pod's
// request is, so another role of the same issue under the same launcher is refused NOT_YOURS on
// both routes and the request is still pending for its own claim.
func TestAnotherClaimOfTheSameServiceCannotCancel(t *testing.T) {
	ts := newTestServer(t)
	daemon := ts.serviceLogin(t)
	planner := ts.enrolPod(t, daemon, plannerClaim, "planner-session-1")
	created := ts.askPending(t, planner, "DEEL_API_KEY")

	implementer := ts.enrolPod(t, daemon, implementerClaim, "implementer-session-1")
	ts.wantNotYours(t, implementer, created.RequestID)
	ts.wantState(t, planner, created.RequestID, "pending")
}

// TestAPersonReadsAndCancelsTheirRequestFromAnotherBox: a request a person's session made is the
// person's, so their session on another box, under a machine login of its own, reads and cancels
// it, and another person's session is refused.
func TestAPersonReadsAndCancelsTheirRequestFromAnotherBox(t *testing.T) {
	ts := newTestServer(t)
	boxOne, keyOne := ts.mintLauncherCredential(t, testApprover, "example-host-one")
	boxTwo, keyTwo := ts.mintLauncherCredential(t, testApprover, "example-host-two")
	elsewhere, keyElsewhere := ts.mintLauncherCredential(t, "bob@example.com", "example-host-bob")
	asker := ts.enrolSession(t, launcherLogin{id: boxOne, key: keyOne}, "person-session-1")
	created := ts.askPending(t, asker, "DEEL_API_KEY")

	ts.wantNotYours(t, ts.enrolSession(t, launcherLogin{id: elsewhere, key: keyElsewhere}, "bob-session-1"), created.RequestID)
	other := ts.enrolSession(t, launcherLogin{id: boxTwo, key: keyTwo}, "person-session-2")
	ts.wantState(t, other, created.RequestID, "pending")
	ts.wantCancelled(t, other, created.RequestID)
	ts.wantState(t, asker, created.RequestID, "cancelled")
}

// vmAccount is the fleet account the VM test lists for vmFleet.
const (
	vmAccount = "example-fleet@example-project.iam.gserviceaccount.com"
	vmFleet   = "example-fleet"
)

// TestAnotherVMOfTheSameServiceCannotReadTheRequest: a machine's own request is that machine's,
// by the subject its credential proved, so another machine of the same fleet is refused on both
// routes while a second session of the same machine reads it.
func TestAnotherVMOfTheSameServiceCannotReadTheRequest(t *testing.T) {
	var enrollments *enroll.Service
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.Enroll.VMServicesByAccount = map[string]string{vmAccount: vmFleet}
		enrollments = d.Enroll
	})
	machine := func(instance string) launcherLogin {
		t.Helper()
		key := newSigningKey(t)
		thumbprint := thumbprintOf(t, key)
		expires := time.Now().Add(time.Hour)
		id, err := enrollments.IssueTokenLogin(context.Background(), enroll.TokenLogin{
			Subject: exchange.MachineSubject(exchange.GoogleIdentity{Account: vmAccount, Compute: exchange.ComputeEngine{
				Project: "example-project", Zone: "example-zone-a", Instance: instance, InstanceID: instance,
			}}, thumbprint),
			Service: vmFleet, Host: "example-project/example-zone-a/" + instance,
			TokenExpires: expires, Thumbprint: thumbprint, JWK: json.RawMessage(`{"kty":"EC"}`), Expires: expires,
		})
		if err != nil {
			t.Fatalf("IssueTokenLogin(%s): %v", instance, err)
		}
		return launcherLogin{id: id.String(), key: key}
	}
	first, second := machine("example-vm-1"), machine("example-vm-2")
	asker := ts.enrolSession(t, first, "")
	created := ts.askPending(t, asker, "DEEL_API_KEY")

	ts.wantNotYours(t, ts.enrolSession(t, second, ""), created.RequestID)
	ts.wantState(t, ts.enrolSession(t, first, ""), created.RequestID, "pending")
}

// TestAnEqualRequestFromTheNextPodOfTheClaimJoinsThePendingOne: a pod's box ends while its
// request is pending, and the next pod of its claim, under a session of its own, takes the request
// with it. Asking for the same names again joins that request: one pending request, and one card
// in the approver's Inbox.
func TestAnEqualRequestFromTheNextPodOfTheClaimJoinsThePendingOne(t *testing.T) {
	ts := newTestServer(t)
	daemon := ts.serviceLogin(t)
	first := ts.enrolPod(t, daemon, plannerClaim, "planner-session-1")
	created := ts.askPending(t, first, "DEEL_API_KEY")
	if status, body := ts.launcher(t, daemon.key, daemon.id, http.MethodDelete, "/v1/enrollments/"+first.id, nil); status != http.StatusNoContent {
		t.Fatalf("the daemon revoking pod A = %d %s, want 204", status, body)
	}

	next := ts.enrolPod(t, daemon, plannerClaim, "planner-session-2")
	again := ts.askAs(t, next, "DEEL_API_KEY")
	if !again.Coalesced || again.RequestID != created.RequestID {
		t.Fatalf("pod B asking for DEEL_API_KEY again = %+v, want it joined to pod A's pending request %s", again, created.RequestID)
	}
	if pending := ts.column(t, `select id::text from requests where state='pending'`); len(pending) != 1 {
		t.Fatalf("pending requests %v, want one", pending)
	}
	status, body := ts.ui(t, http.MethodGet, "/v1/pending?approver="+testApprover, nil)
	cards := decode[struct {
		Pending []wirePendingEntry `json:"pending"`
	}](t, body).Pending
	if status != http.StatusOK || len(cards) != 1 || cards[0].RecordID != *created.RecordID {
		t.Fatalf("GET /v1/pending = %d %s, want the one card of record %s", status, body, *created.RecordID)
	}
}

// TestTheNextPodOfAClaimReceivesTheValueAPersonApprovedForTheLastOne is this change end to end: a
// pod asks, its box ends, the approver approves on the broker's page while no pod of the claim is
// up, and the claim's next pod, under a session of its own, reads the request granted and
// releases the value.
func TestTheNextPodOfAClaimReceivesTheValueAPersonApprovedForTheLastOne(t *testing.T) {
	ts := newTestServer(t)
	daemon := ts.serviceLogin(t)
	first := ts.enrolPod(t, daemon, plannerClaim, "planner-session-1")
	created := ts.askPending(t, first, "DEEL_API_KEY")
	if status, body := ts.launcher(t, daemon.key, daemon.id, http.MethodDelete, "/v1/enrollments/"+first.id, nil); status != http.StatusNoContent {
		t.Fatalf("the daemon revoking pod A = %d %s, want 204", status, body)
	}
	ts.decide(t, testApprover, *created.RecordID, true)

	next := ts.enrolPod(t, daemon, plannerClaim, "planner-session-2")
	read := ts.wantState(t, next, created.RequestID, "granted")
	if read.GrantID == nil {
		t.Fatalf("pod B reads request %s granted with no grant", created.RequestID)
	}
	status, body := ts.session(t, next.key, next.id, http.MethodPost, "/v1/grants/"+*read.GrantID+"/values", nil)
	values := decode[struct {
		Values map[string]string `json:"values"`
	}](t, body).Values
	if status != http.StatusOK || values["DEEL_API_KEY"] != "deel-v1" {
		t.Fatalf("pod B releasing grant %s = %d, %d values; want 200 and DEEL_API_KEY's value", *read.GrantID, status, len(values))
	}
}

// TestARequestOfAnEnrollmentWithNoOwnerIsItsAlone: every enrollment live when migration 0035
// applies records no owner, and a request of such an enrollment is read and cancelled by that
// enrollment alone, as every request was before. With a null owner on either side, even the same
// person's other enrollment is refused.
func TestARequestOfAnEnrollmentWithNoOwnerIsItsAlone(t *testing.T) {
	ts := newTestServer(t)
	ownerless := func(name string) enrolled {
		id, key := ts.newSessionEnrollment(t, "session-"+name, testApprover)
		return enrolled{id: id, key: key}
	}
	box, key := ts.mintLauncherCredential(t, testApprover, "example-host-one")
	owned := ts.enrolSession(t, launcherLogin{id: box, key: key}, "person-session-1")
	asker, sibling := ownerless("asker"), ownerless("sibling")

	created := ts.askPending(t, asker, "DEEL_API_KEY")
	ts.wantNotYours(t, sibling, created.RequestID)
	ts.wantNotYours(t, owned, created.RequestID)
	ts.wantState(t, asker, created.RequestID, "pending")

	ownersRequest := ts.askPending(t, owned, "SHARED_KEY")
	ts.wantNotYours(t, asker, ownersRequest.RequestID)
	ts.wantCancelled(t, asker, created.RequestID)
}

// TestTwoSlotsOfAPodThatStatesNoClaimCannotReadOrCancelEachOther: every role process of an issue
// pod enrols under the pod's one uid, each in a slot of its own (<role>-g<generation>). A pod
// whose launcher states no claim, which is every pod while the broker runs this code and the
// daemon does not yet send one, is its own pod's, and each slot of it is its own slot's, so one
// role of the pod is refused NOT_YOURS on the other's request on both routes, as each slot of a
// pod holds its own requests (packages/envoy/AGENTS.md). A second enrollment of the same slot,
// the role process enrolled again, reads its slot's request.
func TestTwoSlotsOfAPodThatStatesNoClaimCannotReadOrCancelEachOther(t *testing.T) {
	ts := newTestServer(t)
	daemon := ts.serviceLogin(t)
	podUID := uuid.NewString()
	slot := func(name, session string) enrolled {
		return ts.enrolPodWith(t, daemon, podEnrolment{account: workerAccount, podUID: podUID, slot: name, session: session})
	}
	planner := slot("planner-g1", "planner-session-1")
	implementer := slot("implementer-g1", "implementer-session-1")
	created := ts.askPending(t, planner, "DEEL_API_KEY")

	ts.wantNotYours(t, implementer, created.RequestID)
	ts.wantState(t, planner, created.RequestID, "pending")
	theirs := ts.askPending(t, implementer, "SHARED_KEY")
	ts.wantNotYours(t, planner, theirs.RequestID)

	if status, body := ts.launcher(t, daemon.key, daemon.id, http.MethodDelete, "/v1/enrollments/"+planner.id, nil); status != http.StatusNoContent {
		t.Fatalf("the daemon revoking the planner's slot = %d %s, want 204", status, body)
	}
	again := slot("planner-g1", "planner-session-1")
	ts.wantState(t, again, created.RequestID, "pending")
	ts.wantNotYours(t, implementer, created.RequestID)
}

// TestAClaimTheBrokerRefusesIsInvalidClaim: POST /v1/enrollments answers a claim the broker
// refuses (enroll.ErrInvalidClaim) 400 INVALID_CLAIM, a refusal the daemon does not retry without
// the field, and enrols nothing: a claim in a shape no claim token takes, and a claim on a session.
func TestAClaimTheBrokerRefusesIsInvalidClaim(t *testing.T) {
	ts := newTestServer(t)
	daemon := ts.serviceLogin(t)
	box, boxKey := ts.mintLauncherCredential(t, testApprover, "example-host-one")
	podUID := uuid.NewString()
	for _, c := range []struct {
		name       string
		credential launcherLogin
		body       map[string]any
	}{
		{"a malformed claim on a pod", daemon, map[string]any{
			"kind": "pod", "runtime_id": podUID, "thumbprint": thumbprintOf(t, newSigningKey(t)),
			"pod_token": ts.podToken(t, workerAccount, podUID), "session_id": "planner-session-1", "claim": "Legion/planner",
		}},
		{"a claim on a session", launcherLogin{id: box, key: boxKey}, map[string]any{
			"kind": "session", "runtime_id": uuid.NewString(), "thumbprint": thumbprintOf(t, newSigningKey(t)), "claim": plannerClaim,
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := ts.column(t, `select id::text from enrollments`)
			status, body := ts.launcher(t, c.credential.key, c.credential.id, http.MethodPost, "/v1/enrollments", c.body)
			if status != http.StatusBadRequest || decode[wireError](t, body).Code != "INVALID_CLAIM" {
				t.Fatalf("POST /v1/enrollments = %d %s, want 400 INVALID_CLAIM", status, body)
			}
			if after := ts.column(t, `select id::text from enrollments`); len(after) != len(before) {
				t.Fatalf("the refusal left enrollments %v, want %v: it enrols nothing", after, before)
			}
		})
	}
}
