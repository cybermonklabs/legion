// session_held_test.go pins what POST /v1/enrollments answers a launcher reaching for a session
// name another principal already holds (dispatch://INFRA-1167): 403 SESSION_HELD, the session
// named so the operator reading it knows which name is taken, and nothing of whoever holds it.
// It also pins the one principal that takes such a name rather than being refused it, an account
// BROKER_SESSION_ACCOUNT_PAIRS pairs with the holder (dispatch://INFRA-3891), and what that
// enrollment then holds.
package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// TestEnrollingUnderANameAnotherPrincipalHoldsIs403SessionHeld: one person's box enrols a session
// name, a second person's box enrols that very name, and the broker refuses the second — a grant
// follows a name under one principal, so the name belongs to one principal
// (enroll.ErrSessionHeld). The body names the session, so an operator reading it knows which name
// is taken, and names no holder, since whose the name is, is not the refused caller's to learn.
// What the refusal says beyond the session is nobody's to depend on, so none of it is pinned here.
func TestEnrollingUnderANameAnotherPrincipalHoldsIs403SessionHeld(t *testing.T) {
	ts := newTestServer(t)
	session := "role-" + t.Name()
	hers, herKey := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	status, body := ts.launcher(t, herKey, hers, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "session", "runtime_id": "box-" + t.Name(), "thumbprint": thumbprintOf(t, newSigningKey(t)),
		"session_id": session,
	})
	if status != http.StatusCreated {
		t.Fatalf("the first principal's POST /v1/enrollments = %d, want 201: %s", status, body)
	}

	theirs, theirKey := ts.mintLauncherCredential(t, "mallory@example.com", "example-host-devbox-m")
	status, body = ts.launcher(t, theirKey, theirs, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "session", "runtime_id": "box-m-" + t.Name(), "thumbprint": thumbprintOf(t, newSigningKey(t)),
		"session_id": session,
	})
	if status != http.StatusForbidden {
		t.Fatalf("the second principal's POST /v1/enrollments = %d, want 403: %s", status, body)
	}
	refusal := decode[wireError](t, body)
	if refusal.Code != "SESSION_HELD" {
		t.Fatalf("the refusal's code = %q, want SESSION_HELD: %s", refusal.Code, body)
	}
	if !strings.Contains(refusal.Error, session) {
		t.Fatalf("the refusal reads %q, want it to name the session %s so the operator knows which name is taken", refusal.Error, session)
	}
	for _, leaked := range []string{testApprover, "person:", "mallory@example.com"} {
		if strings.Contains(refusal.Error, leaked) {
			t.Fatalf("the refusal reads %q, want nothing of who holds the name in it (%q)", refusal.Error, leaked)
		}
	}
}

// TestAPairedAccountsEnrollmentTakesTheNameAndWhatItCarries is BROKER_SESSION_ACCOUNT_PAIRS at the
// enrollment route: an operator moves a deployment's pods from workerAccount to otherAccount, and
// a role's next pod, running as otherAccount, enrols under the session name the role's replaced
// pod held as workerAccount. The replaced pod holds a grant a person approved, a request still
// waiting on a person, and a secret withheld from it, one it got automatically until an operator
// revoked that grant; then its launcher revoked its enrollment, as the Legion daemon does when it
// lets a role's process go, which leaves the row dormant holding all of it for the session.
//
// Unpaired, the name is another principal's, and the next pod is refused 403 SESSION_HELD.
// Paired, it is enrolled, and it holds what the replaced pod held, as the same account's next pod
// would: it reads the approved secret's value, reads and cancels the waiting request, and is not
// served the withheld secret without a person, which a pod of its account under a name of its own
// is served at once. Neither pod states a claim, since the claim rule would move the grant and
// the request to the claim's next pod whatever its account: what moves here moves by the session
// rule, which the pair widens. Each arm is a server of its own, so the refusal cannot change what
// the paired arm reads.
//
// No route withholds from a service's pod: only the revoke of a grant's enrollment's operator
// withholds (requests.Machine.RevokeByApprover), and a service's pod enrollment has no operator. So
// the test writes what that revoke writes for the automatic grant, the grant ended and its name
// withheld from the enrollment, as enroll's own carry tests write the withheld row.
func TestAPairedAccountsEnrollmentTakesTheNameAndWhatItCarries(t *testing.T) {
	const session = "implementer-session-1"
	// replaced is one arm's server and what its replaced pod left dormant for the session: the
	// daemon's login, which enrols both accounts' pods, the grant testApprover approved for
	// DEEL_API_KEY, and the request for SHARED_KEY still waiting on a person. WORKER_TOKEN, which
	// every pod gets at once, is withheld from it.
	type replaced struct {
		ts      *testServer
		daemon  launcherLogin
		grant   string
		waiting string
	}
	start := func(t *testing.T, paired bool) replaced {
		t.Helper()
		ts := newTestServer(t)
		if paired {
			ts.sessions.SetSessionPairs([][2]string{{exchange.AccountPrincipal(workerAccount), exchange.AccountPrincipal(otherAccount)}})
		}
		daemon := ts.serviceLogin(t)
		old := ts.enrolPodWith(t, daemon, podEnrolment{account: workerAccount, podUID: uuid.NewString(), session: session})
		grant := ts.approveAs(t, ts.askPending(t, old, "DEEL_API_KEY"), testApprover)
		waiting := ts.askPending(t, old, "SHARED_KEY").RequestID
		automatic := ts.askAs(t, old, "WORKER_TOKEN")
		if automatic.State != "granted" || automatic.GrantID == nil {
			t.Fatalf("the replaced pod's WORKER_TOKEN request = %+v, want it granted at once", automatic)
		}
		if _, err := ts.Store.Pool.Exec(context.Background(), `with withheld as (
				insert into withheld_secrets (enrollment_id, name) values ($1, 'WORKER_TOKEN')
			)
			update grants set revoked_at = now(), revoked_by = $3 where id = $2`,
			old.id, *automatic.GrantID, record.HumanActor(testApprover)); err != nil {
			t.Fatalf("the operator's revoke of the replaced pod's WORKER_TOKEN grant: %v", err)
		}
		if status, body := ts.launcher(t, daemon.key, daemon.id, http.MethodDelete, "/v1/enrollments/"+old.id, nil); status != http.StatusNoContent {
			t.Fatalf("the daemon revoking the replaced pod = %d %s, want 204", status, body)
		}
		if dormant := ts.column(t, `select (dormant_since is not null)::text from enrollments where id=$1`, old.id); len(dormant) != 1 || dormant[0] != "true" {
			t.Fatalf("the replaced pod's enrollment is dormant: %v, want true: this test measures what its successor takes from it", dormant)
		}
		return replaced{ts: ts, daemon: daemon, grant: grant, waiting: waiting}
	}
	// enrolNext has the daemon enrol the role's next pod, a pod of otherAccount, under the session
	// name, and answers the broker's status and body beside the enrollment (postPodEnrolment).
	enrolNext := func(t *testing.T, r replaced) (int, []byte, enrolled) {
		t.Helper()
		return r.ts.postPodEnrolment(t, r.daemon, podEnrolment{account: otherAccount, podUID: uuid.NewString(), session: session})
	}

	t.Run("unpaired, the next pod is refused the name", func(t *testing.T) {
		r := start(t, false)
		status, body, _ := enrolNext(t, r)
		if status != http.StatusForbidden || decode[wireError](t, body).Code != "SESSION_HELD" {
			t.Fatalf("a pod of %s enrolling %s with no pair = %d %s, want 403 SESSION_HELD: %s holds the name", otherAccount, session, status, body, workerAccount)
		}
	})

	t.Run("paired, the next pod takes the name and what the replaced pod held", func(t *testing.T) {
		r := start(t, true)
		status, body, next := enrolNext(t, r)
		if status != http.StatusCreated {
			t.Fatalf("a pod of %s enrolling %s paired with %s = %d %s, want 201: a paired account takes the name its pair holds", otherAccount, session, workerAccount, status, body)
		}

		t.Run("it reads the approved secret's value", func(t *testing.T) {
			status, body := r.ts.session(t, next.key, next.id, http.MethodPost, "/v1/grants/"+r.grant+"/values", nil)
			if status != http.StatusOK {
				t.Fatalf("the next pod releasing grant %s = %d %s, want 200: the grant follows the session to the paired account's pod", r.grant, status, body)
			}
			if values := decode[struct {
				Values map[string]string `json:"values"`
			}](t, body).Values; values["DEEL_API_KEY"] != "deel-v1" {
				t.Fatalf("the next pod released %d values from grant %s, want DEEL_API_KEY's value among them", len(values), r.grant)
			}
		})

		t.Run("the waiting request is its own to read and cancel", func(t *testing.T) {
			r.ts.wantState(t, next, r.waiting, "pending")
			r.ts.wantCancelled(t, next, r.waiting)
		})

		t.Run("the withheld secret is not served without a person", func(t *testing.T) {
			fresh := r.ts.enrolPodWith(t, r.daemon, podEnrolment{account: otherAccount, podUID: uuid.NewString(), session: "implementer-session-2"})
			if served := r.ts.askAs(t, fresh, "WORKER_TOKEN"); served.State != "granted" || served.GrantID == nil {
				t.Fatalf("a pod of %s under a name of its own asking for WORKER_TOKEN is %s (with a grant: %t), want it granted at once: nothing is withheld from that session",
					otherAccount, served.State, served.GrantID != nil)
			}
			if asked := r.ts.askAs(t, next, "WORKER_TOKEN"); asked.State != "pending" || asked.GrantID != nil || asked.RecordID == nil {
				t.Fatalf("the next pod asking for WORKER_TOKEN is %s (with a grant: %t, on a record: %t), want it pending on a record: an operator took that secret back from the session, and a move to the paired account does not give it back",
					asked.State, asked.GrantID != nil, asked.RecordID != nil)
			}
		})
	})
}
