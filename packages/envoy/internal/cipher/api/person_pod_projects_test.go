// person_pod_projects_test.go drives POST /v1/enrollments for a person's pod that names
// projects, on the real handlers api_test.go mounts. BROKER_POD_OPERATORS maps the pod's account
// to bob, a full member. The pod logs in with its own token and names its projects as bob's own
// session does, and the broker takes them. A restricted member's pod enrolls nothing at all
// (enroll/restricted_member_test.go); the refusals of a pod's list are
// enroll.Service.Create's, which enroll/person_pod_test.go drives for a person's pod.
package api_test

import (
	"crypto/ecdsa"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
)

// bob's account, the login service BROKER_SERVICE_LOGINS gives it, and bob, whom
// BROKER_POD_OPERATORS maps it to.
const (
	projectsPodAccount = "system:serviceaccount:people:bob"
	projectsPodService = "person-bob"
	projectsPodPerson  = "bob@example.com"
)

// newPersonPodProjectsServer is newTestServer with bob's account mapped to bob and a client of a
// Dispatch stand-in in which bob is a member of kind kind, and PRIV is private and its access list
// names bob.
func newPersonPodProjectsServer(t *testing.T, kind string) *testServer {
	t.Helper()
	dispatch := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PRIV": {Visibility: "private", Owner: testApprover, Listed: []string{projectsPodPerson}},
	}, map[string]string{projectsPodPerson: kind})
	return newTestServerWith(t, func(d *api.Deps) {
		d.Enroll.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
		d.Enroll.ServiceLogins[projectsPodAccount] = projectsPodService
		d.Enroll.PodOperators = map[string]string{projectsPodAccount: projectsPodPerson}
	})
}

// bobsPod is one pod of bob's account: its pod UID, the login its own token made and that
// login's key, and the key the pod enrols.
type bobsPod struct {
	uid           string
	login         string
	loginKey, key *ecdsa.PrivateKey
}

// logInBobsPod logs pod uid of bob's account in over POST /v1/launcher-credentials/token, with a
// token bound to that pod, and fails t unless the broker answers 200.
func logInBobsPod(t *testing.T, ts *testServer, uid string) bobsPod {
	t.Helper()
	loginKey := newSigningKey(t)
	status, body := ts.tokenLogin(t, ts.accountToken(t, projectsPodAccount, uid, 20*time.Minute),
		signMachineLoginRequest(t, loginKey, ts.URL, "", "people-bob", projectsPodService))
	if status != http.StatusOK {
		t.Fatalf("token login of bob's pod %s = %d %s; want 200", uid, status, body)
	}
	return bobsPod{uid: uid, login: decode[wireTokenLogin](t, body).CredentialID, loginKey: loginKey, key: newSigningKey(t)}
}

// enrol asks POST /v1/enrollments to enrol the pod under its own login, naming projects (no
// projects field at all for nil).
func (p bobsPod) enrol(t *testing.T, ts *testServer, projects []string) (int, []byte) {
	t.Helper()
	body := map[string]any{
		"kind": "pod", "runtime_id": p.uid, "thumbprint": thumbprintOf(t, p.key),
		"pod_token": ts.podToken(t, projectsPodAccount, p.uid),
	}
	if projects != nil {
		body["projects"] = projects
	}
	return ts.launcher(t, p.loginKey, p.login, http.MethodPost, "/v1/enrollments", body)
}

// TestAPersonsPodEnrollsNamingItsProjects: bob, a full member, has a pod naming PRIV, whose access
// list names him, and it is enrolled: 201, and the same call again answers 200 with the same
// enrollment, as the route answers a key already enrolled. GET /v1/enrollments/self answers bob as
// its operator and PRIV as its projects. So the route takes a pod's projects and gives them to the
// rule that checks a person's pod against its person.
func TestAPersonsPodEnrollsNamingItsProjects(t *testing.T) {
	ts := newPersonPodProjectsServer(t, projectaccess.MemberFull)

	pod := logInBobsPod(t, ts, "pod-uid-bob-priv")
	status, body := pod.enrol(t, ts, []string{"PRIV"})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/enrollments for bob's pod naming PRIV = %d %s; want 201", status, body)
	}
	enrollmentID := decode[wireEnrolled](t, body).EnrollmentID
	status, body = pod.enrol(t, ts, []string{"PRIV"})
	if status != http.StatusOK || decode[wireEnrolled](t, body).EnrollmentID != enrollmentID {
		t.Fatalf("POST /v1/enrollments again for bob's pod naming PRIV = %d %s; want 200 with enrollment %s", status, body, enrollmentID)
	}
	status, body = ts.session(t, pod.key, enrollmentID, http.MethodGet, "/v1/enrollments/self", nil)
	self := decode[struct {
		Kind     string   `json:"kind"`
		Operator *string  `json:"operator"`
		Projects []string `json:"projects"`
	}](t, body)
	if status != http.StatusOK || self.Kind != "pod" || self.Operator == nil || *self.Operator != projectsPodPerson || !slices.Equal(self.Projects, []string{"PRIV"}) {
		t.Fatalf("GET /v1/enrollments/self of bob's pod = %d %s; want a pod of %s naming [PRIV]", status, body, projectsPodPerson)
	}
}
