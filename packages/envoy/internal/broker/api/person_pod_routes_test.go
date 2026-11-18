// person_pod_routes_test.go drives the routes that a person's pod reaches, on the real broker
// (brokertest.NewRig with brokertest.WithPersonPod). BROKER_POD_OPERATORS maps the pod's account
// to a person. The pod logs in with its own token, enrols its own pod, and acts for the person on
// secrets alone. The pod-session route refuses its account, 403 PERSON_ACCOUNT. Each operator
// route that acts for an operator refuses its login, 403 PERSON_POD_LOGIN, live or stale, and
// serves the person's box login.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// The account that this file's Rig maps to a person, the login service the account logs in as,
// the pod that its pod-bound token names, and the person: the Rig's Operator, who also approves
// the person's box login. personPodOther is the person that a test moves the entry to.
const (
	personPodAccount = "system:serviceaccount:people:ada"
	personPodService = "person-ada"
	personPodUID     = "pod-uid-ada"
	personPodPerson  = "ada@example.com"
	personPodOther   = "grace@example.com"
)

// newPersonPodRig is a Rig whose BROKER_POD_OPERATORS maps personPodAccount to personPodPerson.
// It fails t unless that person is the Rig's Operator, so that the box login the Rig approves and
// the pod's login are the same person's.
func newPersonPodRig(t *testing.T) *brokertest.Rig {
	t.Helper()
	rig := brokertest.NewRig(t, brokertest.WithPersonPod(personPodAccount, personPodService, personPodPerson))
	if rig.Operator != personPodPerson {
		t.Fatalf("the Rig's Operator is %s; want %s, the person BROKER_POD_OPERATORS maps %s to", rig.Operator, personPodPerson, personPodAccount)
	}
	return rig
}

// launcherCall calls method path on rig under the launcher credential credentialID, with a
// launcher proof that key signs.
func launcherCall(t *testing.T, rig *brokertest.Rig, key *ecdsa.PrivateKey, credentialID, method, path string, body any) (int, []byte) {
	t.Helper()
	p, err := proof.SignLauncher(key, credentialID, method, rig.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.SignLauncher: %v", err)
	}
	return rig.Req(t, method, path, map[string]string{"Proof": p}, body)
}

// sessionCall calls method path on rig as the enrollment enrollmentID, with a session proof that
// key signs.
func sessionCall(t *testing.T, rig *brokertest.Rig, key *ecdsa.PrivateKey, enrollmentID, method, path string, body any) (int, []byte) {
	t.Helper()
	p, err := proof.Sign(key, enrollmentID, method, rig.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	return rig.Req(t, method, path, map[string]string{"Proof": p}, body)
}

// rigColumn reads one text column of every row that query selects on rig's store, as column does
// on a test server, with "<null>" for a null.
func rigColumn(t *testing.T, rig *brokertest.Rig, query string, args ...any) []string {
	t.Helper()
	rows, err := rig.Store.Pool.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v *string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if v == nil {
			out = append(out, "<null>")
			continue
		}
		out = append(out, *v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

// personPodTokenLogin logs the mapped account's pod podUID in over
// POST /v1/launcher-credentials/token. It sends a pod-bound token of the account and a request
// object that a new key signs for the account's login service. It answers the credential and its
// key. It fails t unless the credential is a person's pod login: its row carries the person, the
// account and the pod.
func personPodTokenLogin(t *testing.T, rig *brokertest.Rig, podUID string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	key := newSigningKey(t)
	status, body := rig.Req(t, http.MethodPost, "/v1/launcher-credentials/token", nil, map[string]any{
		"token":   rig.MintAccountPodToken(t, personPodAccount, podUID, 20*time.Minute),
		"request": signMachineLoginRequest(t, key, rig.URL, "", "people-ada", personPodService),
	})
	if status != http.StatusOK {
		t.Fatalf("token login of %s's pod %s = %d %s; want 200", personPodAccount, podUID, status, body)
	}
	login := decode[wireTokenLogin](t, body).CredentialID
	got := rigColumn(t, rig, `select coalesce(operator, '<none>') || ' ' || coalesce(subject, '<none>') || ' ' || coalesce(pod_uid, '<none>')
		from launcher_credentials where id=$1`, login)
	if want := []string{personPodPerson + " " + personPodAccount + " " + podUID}; !slices.Equal(got, want) {
		t.Fatalf("the pod's login %s holds %v; want %v, a person's pod login", login, got, want)
	}
	return login, key
}

// enrolPersonPod asks POST /v1/enrollments to enrol the mapped account's pod podUID under the
// pod's login, with the pod's own projected token and the thumbprint of podKey.
func enrolPersonPod(t *testing.T, rig *brokertest.Rig, key *ecdsa.PrivateKey, login, podUID string, podKey *ecdsa.PrivateKey) (int, []byte) {
	t.Helper()
	return launcherCall(t, rig, key, login, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": podUID, "thumbprint": thumbprintOf(t, podKey),
		"pod_token": rig.MintAccountPodToken(t, personPodAccount, podUID, 20*time.Minute),
	})
}

// askOnRig asks for the agent secret name as the enrollment enrollmentID. key signs the request
// object and the proof. It answers the broker's reply, and fails t unless the broker answers 200.
func askOnRig(t *testing.T, rig *brokertest.Rig, key *ecdsa.PrivateKey, enrollmentID, name string) wireCreateRequestResponse {
	t.Helper()
	status, body := sessionCall(t, rig, key, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, key, rig.URL, "need it", name), "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests %s as %s = %d %s; want 200", name, enrollmentID, status, body)
	}
	return decode[wireCreateRequestResponse](t, body)
}

// personsBox is the person's dev box on a Rig: the machine login that the person approved, its
// key, a session that it enrolled, and a live grant of TEST_AUTO_SECRET that the session holds.
type personsBox struct {
	login    string
	loginKey *ecdsa.PrivateKey
	session  string
	grant    string
}

// newPersonsBox logs the person's box in and has the person approve the login. It enrols a
// session under the login, and the session asks for TEST_AUTO_SECRET, the person's own agent-tier
// secret, which the policy grants at once. It fails t unless the login, the session and the grant
// are live, because they are what a call of the pod's login would take.
func newPersonsBox(t *testing.T, rig *brokertest.Rig) personsBox {
	t.Helper()
	m, _, login := approvedLogin(t, rig)
	sessionKey := newSigningKey(t)
	status, body := launcherCall(t, rig, m.key, login, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "session", "runtime_id": "ada-devbox-" + uuid.NewString(), "operator": rig.Operator, "thumbprint": thumbprintOf(t, sessionKey),
	})
	if status != http.StatusCreated {
		t.Fatalf("enrol a session on ada's box = %d %s; want 201", status, body)
	}
	session := decode[wireEnrolled](t, body).EnrollmentID
	asked := askOnRig(t, rig, sessionKey, session, "TEST_AUTO_SECRET")
	if asked.State != "granted" || asked.GrantID == nil {
		t.Fatalf("ada's box session asks for TEST_AUTO_SECRET = %+v; want it granted at once", asked)
	}
	box := personsBox{login: login, loginKey: m.key, session: session, grant: *asked.GrantID}
	box.requireLive(t, rig, "ada's box before any call of the pod's login")
	return box
}

// requireLive fails t unless the box's login, its session and the session's grant are all live.
func (b personsBox) requireLive(t *testing.T, rig *brokertest.Rig, when string) {
	t.Helper()
	got := rigColumn(t, rig, `select (c.revoked_at is null)::text || ' ' || (e.revoked_at is null)::text || ' '
			|| (g.revoked_at is null and (g.expires_at is null or g.expires_at > now()))::text
		from launcher_credentials c, enrollments e, grants g
		where c.id=$1 and e.id=$2 and e.launcher_credential_id = c.id and g.id=$3 and g.enrollment_id = e.id`, b.login, b.session, b.grant)
	if !slices.Equal(got, []string{"true true true"}) {
		t.Fatalf("%s: ada's box login %s, its session %s and the session's grant %s are live %v; want all three live",
			when, b.login, b.session, b.grant, got)
	}
}

// operatorCall is one call of a route that acts for its caller's operator, aimed at what the
// person's box holds, and what the call does when the route admits the caller.
type operatorCall struct {
	method, path, does string
}

// operatorCalls are the four routes that read their caller's operator (operatorOf), each aimed at
// box.
func operatorCalls(box personsBox) []operatorCall {
	return []operatorCall{
		{http.MethodGet, "/v1/operator/machines", "lists ada's machine logins, her box login " + box.login + " among them"},
		{http.MethodPost, "/v1/operator/machines/" + box.login + "/revoke", "ends ada's box login " + box.login + " and her box session " + box.session},
		{http.MethodGet, "/v1/operator/grants", "lists ada's grants, grant " + box.grant + " of her box session among them"},
		{http.MethodPost, "/v1/operator/grants/" + box.grant + "/revoke", "ends grant " + box.grant + " of ada's box session " + box.session},
	}
}

// requirePersonPodLoginRefused calls each operator route under the pod's login. It fails t
// unless each call is 403 PERSON_POD_LOGIN and ada's box is live after the calls.
func requirePersonPodLoginRefused(t *testing.T, rig *brokertest.Rig, key *ecdsa.PrivateKey, login string, box personsBox, when string) {
	t.Helper()
	for _, c := range operatorCalls(box) {
		status, body := launcherCall(t, rig, key, login, c.method, c.path, nil)
		if status != http.StatusForbidden || decode[wireError](t, body).Code != "PERSON_POD_LOGIN" {
			t.Errorf("%s: the pod's login %s calls %s %s = %d %s; want 403 PERSON_POD_LOGIN, because the call %s",
				when, login, c.method, c.path, status, body, c.does)
		}
	}
	box.requireLive(t, rig, when+", after the pod's login called each operator route")
}

// TestAPersonPodAccountIsRefusedThePodSessionRoute: the pod-session route is a service's route,
// and an account that BROKER_POD_OPERATORS maps is a person's account. So the route refuses a
// token of that account, 403 PERSON_ACCOUNT, and the refusal names the account. It refuses a token
// bound to the account's pod and a token bound to no pod, and neither writes a registration. The
// daemon's account then registers the same pod under the same name. That shows the body is sound
// and the route refused the caller alone.
func TestAPersonPodAccountIsRefusedThePodSessionRoute(t *testing.T) {
	rig := newPersonPodRig(t)
	const session, pod = "implementer-g1", "pod-uid-roles"
	podToken := rig.MintAccountPodToken(t, brokertest.ExchangeRolesAccount, pod, 20*time.Minute)
	register := func(bearer string) (int, []byte) {
		t.Helper()
		return rig.Req(t, http.MethodPost, "/v1/oauth/exchange/pod-sessions", map[string]string{"Authorization": "Bearer " + bearer},
			map[string]any{"pod_token": podToken, "session": session})
	}
	for _, c := range []struct{ name, bearer string }{
		{"a token bound to the mapped account's pod", rig.MintAccountPodToken(t, personPodAccount, personPodUID, 20*time.Minute)},
		{"a token of the mapped account bound to no pod", rig.MintAccountToken(t, personPodAccount, 20*time.Minute)},
	} {
		status, body := register(c.bearer)
		if status != http.StatusForbidden || decode[wireError](t, body).Code != "PERSON_ACCOUNT" || !strings.Contains(string(body), personPodAccount) {
			t.Errorf("%s registers %s under %s = %d %s; want 403 PERSON_ACCOUNT naming %s: admitted, a person's account chooses the session that pod %s writes under",
				c.name, pod, session, status, body, personPodAccount, pod)
		}
		if rows := rigColumn(t, rig, `select pod_uid || ' ' || session from oauth_pod_sessions`); len(rows) != 0 {
			t.Fatalf("%s wrote the registrations %v; want none", c.name, rows)
		}
	}
	status, body := register(rig.MintAccountToken(t, brokertest.DaemonAccount, 20*time.Minute))
	if status != http.StatusOK {
		t.Fatalf("the daemon registers %s under %s = %d %s; want 200", pod, session, status, body)
	}
	if got := decode[wirePodSession](t, body); got.PodUID != pod || got.Session != session {
		t.Fatalf("the daemon's registration = %+v; want %s under %s", got, pod, session)
	}
	if rows := rigColumn(t, rig, `select pod_uid || ' ' || session from oauth_pod_sessions`); !slices.Equal(rows, []string{pod + " " + session}) {
		t.Fatalf("registrations after the daemon's = %v; want only %s under %s", rows, pod, session)
	}
}

// TestAPersonPodLoginIsRefusedTheOperatorRoutesThatServeItsPersonsBox: a person's pod login acts
// for the person on secrets alone. Each of the four operator routes refuses it, 403
// PERSON_POD_LOGIN, and the person's box login, its session and the session's grant stay live.
// The same four routes then serve the person's box login. Its machine list names the box, a
// second machine of the person and the pod's login. Its grant list names the box session's
// grant. It revokes that grant and the second machine's login.
func TestAPersonPodLoginIsRefusedTheOperatorRoutesThatServeItsPersonsBox(t *testing.T) {
	rig := newPersonPodRig(t)
	box := newPersonsBox(t, rig)
	_, _, laptop := approvedLogin(t, rig)
	login, key := personPodTokenLogin(t, rig, personPodUID)
	requirePersonPodLoginRefused(t, rig, key, login, box, "the live pod login")

	status, body := launcherCall(t, rig, box.loginKey, box.login, http.MethodGet, "/v1/operator/machines", nil)
	if status != http.StatusOK {
		t.Fatalf("ada's box lists her machines = %d %s; want 200", status, body)
	}
	var machines []string
	for _, c := range decode[struct {
		Credentials []wireLauncherCredential `json:"credentials"`
	}](t, body).Credentials {
		machines = append(machines, c.CredentialID)
	}
	for _, want := range []string{box.login, laptop, login} {
		if !slices.Contains(machines, want) {
			t.Fatalf("ada's box lists the logins %v; want her box %s, her second machine %s and her pod's login %s among them", machines, box.login, laptop, login)
		}
	}

	status, body = launcherCall(t, rig, box.loginKey, box.login, http.MethodGet, "/v1/operator/grants", nil)
	if status != http.StatusOK {
		t.Fatalf("ada's box lists her grants = %d %s; want 200", status, body)
	}
	var grants []string
	for _, g := range decode[struct {
		Grants []wireApproverGrant `json:"grants"`
	}](t, body).Grants {
		grants = append(grants, g.GrantID)
	}
	if !slices.Contains(grants, box.grant) {
		t.Fatalf("ada's box lists the grants %v; want her box session's grant %s among them", grants, box.grant)
	}

	if status, body := launcherCall(t, rig, box.loginKey, box.login, http.MethodPost, "/v1/operator/grants/"+box.grant+"/revoke", nil); status != http.StatusOK || decode[stateBody](t, body).State != "revoked" {
		t.Fatalf("ada's box revokes grant %s = %d %s; want 200 revoked", box.grant, status, body)
	}
	if got := rigColumn(t, rig, `select (revoked_at is not null)::text from grants where id=$1`, box.grant); !slices.Equal(got, []string{"true"}) {
		t.Fatalf("grant %s revoked after ada's box revoked it = %v; want true", box.grant, got)
	}
	if status, body := launcherCall(t, rig, box.loginKey, box.login, http.MethodPost, "/v1/operator/machines/"+laptop+"/revoke", nil); status != http.StatusOK || decode[stateBody](t, body).State != "revoked" {
		t.Fatalf("ada's box revokes her second machine's login %s = %d %s; want 200 revoked", laptop, status, body)
	}
	if got := rigColumn(t, rig, `select (revoked_at is not null)::text from launcher_credentials where id=$1`, laptop); !slices.Equal(got, []string{"true"}) {
		t.Fatalf("login %s revoked after ada's box revoked it = %v; want true", laptop, got)
	}
}

// TestAPersonPodLoginEnrolsItsOwnPodForItsPersonOverTheWire: a pod of the mapped account logs in
// with its own pod-bound token over POST /v1/launcher-credentials/token, and its credential
// carries the person. The login enrols its own pod over POST /v1/enrollments, 201, and that
// answer names no operator. GET /v1/enrollments/self, under the new enrollment's own proof, names
// the pod's enrollment and the person as its operator.
func TestAPersonPodLoginEnrolsItsOwnPodForItsPersonOverTheWire(t *testing.T) {
	rig := newPersonPodRig(t)
	login, key := personPodTokenLogin(t, rig, personPodUID)
	podKey := newSigningKey(t)
	status, body := enrolPersonPod(t, rig, key, login, personPodUID, podKey)
	if status != http.StatusCreated {
		t.Fatalf("the pod's login enrols its own pod = %d %s; want 201", status, body)
	}
	if _, named := decode[map[string]any](t, body)["operator"]; named {
		t.Fatalf("POST /v1/enrollments answered %s; want no operator in the answer", body)
	}
	enrollment := decode[wireEnrolled](t, body).EnrollmentID

	status, body = sessionCall(t, rig, podKey, enrollment, http.MethodGet, "/v1/enrollments/self", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/enrollments/self as the pod = %d %s; want 200", status, body)
	}
	self := decode[struct {
		EnrollmentID string  `json:"enrollment_id"`
		Kind         string  `json:"kind"`
		Operator     *string `json:"operator"`
	}](t, body)
	if self.EnrollmentID != enrollment || self.Kind != "pod" || self.Operator == nil || *self.Operator != personPodPerson {
		t.Fatalf("GET /v1/enrollments/self as the pod = %s; want the pod enrollment %s with the operator %s", body, enrollment, personPodPerson)
	}
}

// TestAStalePersonPodLoginIsRefusedTheOperatorRoutesAndItsOwnPod: the live pod login enrols its
// own pod. Then BROKER_POD_OPERATORS drops the account, or moves it to another person. The
// login's row keeps the first person until the pod logs in again. Each of the four operator
// routes refuses that stale login, 403 PERSON_POD_LOGIN, as it refuses a live one, and the
// person's box stays live. No route reads the stale login as the person's machine login.
// POST /v1/enrollments refuses the same enrolment of its own pod, 403 OPERATOR_MISMATCH.
func TestAStalePersonPodLoginIsRefusedTheOperatorRoutesAndItsOwnPod(t *testing.T) {
	for _, change := range []struct{ name, person string }{
		{"the setting drops the account", ""},
		{"the setting moves the account to another person", personPodOther},
	} {
		t.Run(change.name, func(t *testing.T) {
			rig := newPersonPodRig(t)
			box := newPersonsBox(t, rig)
			login, key := personPodTokenLogin(t, rig, personPodUID)
			podKey := newSigningKey(t)
			if status, body := enrolPersonPod(t, rig, key, login, personPodUID, podKey); status != http.StatusCreated {
				t.Fatalf("the live pod login enrols its own pod = %d %s; want 201", status, body)
			}

			rig.SetPodOperator(personPodAccount, change.person)
			if got := rigColumn(t, rig, `select coalesce(operator, '<none>') from launcher_credentials where id=$1 and revoked_at is null`, login); !slices.Equal(got, []string{personPodPerson}) {
				t.Fatalf("after %s, the pod's live login %s holds the operator %v; want %s until the pod logs in again", change.name, login, got, personPodPerson)
			}

			requirePersonPodLoginRefused(t, rig, key, login, box, "after "+change.name)
			status, body := enrolPersonPod(t, rig, key, login, personPodUID, podKey)
			if status != http.StatusForbidden || decode[wireError](t, body).Code != "OPERATOR_MISMATCH" {
				t.Errorf("after %s, the stale login enrols its own pod = %d %s; want 403 OPERATOR_MISMATCH: admitted, the pod acts as %s, whom the setting no longer names for %s",
					change.name, status, body, personPodPerson, personPodAccount)
			}
		})
	}
}

// TestAPersonPodsGrantIsListedToItsPersonOnTheGrantsRoute: the mapped pod asks for
// TEST_AUTO_SECRET, its person's own agent-tier secret, and the policy grants it at once.
// GET /v1/grants, which Dispatch's Live grants page calls (listGrantsForApprover), lists that
// grant to the person as the pod's automatic grant with the person as its operator. It does not
// list the grant to another person.
func TestAPersonPodsGrantIsListedToItsPersonOnTheGrantsRoute(t *testing.T) {
	rig := newPersonPodRig(t)
	login, key := personPodTokenLogin(t, rig, personPodUID)
	podKey := newSigningKey(t)
	status, body := enrolPersonPod(t, rig, key, login, personPodUID, podKey)
	if status != http.StatusCreated {
		t.Fatalf("the pod's login enrols its own pod = %d %s; want 201", status, body)
	}
	enrollment := decode[wireEnrolled](t, body).EnrollmentID
	asked := askOnRig(t, rig, podKey, enrollment, "TEST_AUTO_SECRET")
	if asked.State != "granted" || asked.GrantID == nil {
		t.Fatalf("the pod asks for TEST_AUTO_SECRET = %+v; want it granted at once, as to ada's own box", asked)
	}
	grant := *asked.GrantID
	if got := rigColumn(t, rig, `select (revoked_at is null)::text from grants where id=$1 and enrollment_id=$2`, grant, enrollment); !slices.Equal(got, []string{"true"}) {
		t.Fatalf("grant %s of the pod's enrollment %s is live %v; want true", grant, enrollment, got)
	}

	for _, c := range []struct {
		approver string
		listed   bool
	}{{personPodPerson, true}, {personPodOther, false}} {
		status, body := rig.UI(t, http.MethodGet, "/v1/grants?approver="+url.QueryEscape(c.approver), nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/grants?approver=%s = %d %s; want 200", c.approver, status, body)
		}
		var found *wireApproverGrant
		for _, g := range decode[struct {
			Grants []wireApproverGrant `json:"grants"`
		}](t, body).Grants {
			if g.GrantID == grant {
				found = &g
			}
		}
		switch {
		case c.listed && (found == nil || found.Granted != "automatic" || found.Enrollment.Kind != "pod" ||
			found.Enrollment.RuntimeID != personPodUID || found.Enrollment.Operator != personPodPerson):
			t.Errorf("GET /v1/grants?approver=%s = %s; want the pod's automatic grant %s on pod %s with the operator %s",
				c.approver, body, grant, personPodUID, personPodPerson)
		case !c.listed && found != nil:
			t.Errorf("GET /v1/grants?approver=%s = %s; want no grant %s, which is %s's pod's", c.approver, body, grant, personPodPerson)
		}
	}
}
