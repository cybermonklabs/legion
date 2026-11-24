// project_access_unavailable_test.go pins what the routes answer while the broker cannot ask
// Dispatch about a project (requests.ErrProjectAccessUnavailable): a session reads one fixed
// sentence naming no project, with the detail in the broker's log, and an approver's pending list
// leaves out only the request Dispatch could not answer for.
package api_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// downDispatch is a Dispatch stand-in that fails every question, as one the broker cannot reach.
func downDispatch(t *testing.T) *projectaccesstest.Server {
	t.Helper()
	down := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{}, nil)
	down.SetDown(true)
	return down
}

// projectsDownDispatch is a Dispatch stand-in that fails every question about a project while its
// person route answers, every person a full member's "none": the outage of a project read for a
// session whose person's member kind is known. While the person route fails too, every request
// and release of a person's session is refused 503 MEMBER_KIND_UNCHECKABLE before any project is
// read (requests.Machine.restrictedOperator).
func projectsDownDispatch(t *testing.T) *projectaccesstest.Server {
	t.Helper()
	down := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{}, nil)
	down.SetProjectsDown(true)
	return down
}

// TestAnUnansweredProjectNamesNoProjectToTheSession: while Dispatch cannot answer for a project,
// a request for PRIV_KEY, tagged with the private project PRIV, and a release of a grant of
// PUB_KEY, tagged with the public project PUB, each answer 503 PROJECT_ACCESS_UNAVAILABLE with the
// same fixed sentence, which names neither project; what Dispatch answered goes to the broker's
// log.
func TestAnUnansweredProjectNamesNoProjectToTheSession(t *testing.T) {
	tagged := func(name, project string) awstest.LocalSecret {
		s := policytest.Secret(name, policy.OwnerShared, policy.TierAgent, strings.ToLower(name)+"-v1")
		s.Tags[policy.TagProject] = project
		return s
	}
	access := &projectaccesstest.Swappable{}
	access.Serve(projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PRIV": {Visibility: "private", Owner: testApprover},
		"PUB":  {Visibility: "public", Owner: testApprover},
	}, nil))
	ts := newTestServerWith(t, func(d *api.Deps) {
		local := awstest.NewLocal(tagged("PRIV_KEY", "PRIV"), tagged("PUB_KEY", "PUB"))
		loader := policytest.Loader(local)
		loader.ProjectChecks = true
		current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
		if err != nil {
			t.Fatalf("policy.NewCurrent: %v", err)
		}
		d.Machine.Policy, d.Machine.Secrets, d.Machine.Projects = current, policytest.Store(local), access
	})
	enrollmentID, key := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	status, body := ts.session(t, key, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, key, ts.URL, "read it", "PUB_KEY"), "session_id": nil})
	granted := decode[wireCreateRequestResponse](t, body)
	if status != http.StatusOK || granted.GrantID == nil {
		t.Fatalf("POST /v1/requests for PUB_KEY while Dispatch answers = %d %s; want a grant at once", status, body)
	}

	down := projectsDownDispatch(t)
	access.Serve(down)
	logged := policytest.CaptureLog(t)
	var sentences []string
	for _, c := range []struct {
		what, path, project string
		body                any
	}{
		{"a request for PRIV_KEY", "/v1/requests", "PRIV", map[string]any{"request": signAgentSecretRequest(t, key, ts.URL, "read it", "PRIV_KEY"), "session_id": nil}},
		{"a release of the PUB_KEY grant", "/v1/grants/" + *granted.GrantID + "/values", "PUB", nil},
	} {
		status, body := ts.session(t, key, enrollmentID, http.MethodPost, c.path, c.body)
		answer := decode[wireError](t, body)
		if status != http.StatusServiceUnavailable || answer.Code != "PROJECT_ACCESS_UNAVAILABLE" {
			t.Fatalf("%s while Dispatch is down = %d %s; want 503 PROJECT_ACCESS_UNAVAILABLE", c.what, status, body)
		}
		if strings.Contains(answer.Error, c.project) || strings.Contains(answer.Error, "/api/v1/access") {
			t.Errorf("%s while Dispatch is down tells the session %q, which names its project or Dispatch's route; want one fixed sentence", c.what, answer.Error)
		}
		if want := "/api/v1/access/projects/" + c.project + " 503"; !strings.Contains(logged.String(), want) {
			t.Errorf("the broker's log does not carry Dispatch's answer for %s (%q):\n%s", c.what, want, logged)
		}
		sentences = append(sentences, answer.Error)
	}
	if sentences[0] != sentences[1] || !strings.Contains(sentences[0], "Dispatch") {
		t.Errorf("the two answers tell the session %q; want one fixed sentence saying Dispatch could not be asked", sentences)
	}
	if down.Calls() == 0 {
		t.Fatal("the down Dispatch was never asked: the test is blind")
	}
}

// TestThePendingListLeavesOutOnlyTheRequestDispatchCannotAnswerFor: nothing is tagged and
// Dispatch cannot answer for a project. GET /v1/pending answers 200 and lists the shared secret's
// request whose reason names no issue; the one whose reason names INFRA-12, whose project Dispatch
// could not be asked about, is left out of that list alone rather than failing it.
func TestThePendingListLeavesOutOnlyTheRequestDispatchCannotAnswerFor(t *testing.T) {
	down := projectsDownDispatch(t)
	access := &projectaccesstest.Swappable{}
	access.Serve(down)
	ts := newTestServerWith(t, func(d *api.Deps) { d.Machine.Projects = access })
	ask := func(runtimeID, reason string) string {
		t.Helper()
		enrollmentID, key := ts.newSessionEnrollment(t, runtimeID, testApprover)
		status, body := ts.session(t, key, enrollmentID, http.MethodPost, "/v1/requests",
			map[string]any{"request": signAgentSecretRequest(t, key, ts.URL, reason, "SHARED_KEY"), "session_id": nil})
		created := decode[wireCreateRequestResponse](t, body)
		if status != http.StatusOK || created.RecordID == nil {
			t.Fatalf("POST /v1/requests for SHARED_KEY (%q) = %d %s; want pending", reason, status, body)
		}
		return *created.RecordID
	}
	plain := ask("session-plain-"+t.Name(), "rotate the wiki key")
	keyed := ask("session-keyed-"+t.Name(), "INFRA-12: rotate the deploy key")

	status, body := ts.ui(t, http.MethodGet, "/v1/pending?approver="+testApprover, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/pending while Dispatch is down = %d %s; want 200 with the requests Dispatch need not answer for", status, body)
	}
	var listed []string
	for _, p := range decode[struct {
		Pending []wirePendingEntry `json:"pending"`
	}](t, body).Pending {
		listed = append(listed, p.RecordID)
	}
	if !slices.Contains(listed, plain) || slices.Contains(listed, keyed) {
		t.Fatalf("GET /v1/pending while Dispatch is down lists %v; want %s (no issue named) and not %s (INFRA-12)", listed, plain, keyed)
	}
	if down.Calls() == 0 {
		t.Fatal("the down Dispatch was never asked: the test is blind")
	}
}
