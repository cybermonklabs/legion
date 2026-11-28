// session_hidden_ids_test.go: a session's own routes that take a request id or a grant id — GET
// /v1/requests/{id}, POST /v1/requests/{id}/cancel, POST /v1/grants/{id}/values and POST
// /v1/grants/{id}/revoke — read, before their owner check, whether the request or grant touches
// a private project the calling session cannot see (requests.Machine.HideRequestFromSession): one
// its own credential does not name, or whose list no longer holds the session's person. Such an id
// answers every session but the one that holds it exactly as an id nothing has, whoever owns it,
// so 403 NOT_YOURS only ever says that an id exists in a project the caller sees. The session that
// holds it reads, cancels, releases and revokes it as before. A pod's credential names the project
// its operator's entry maps its account to, so the next pod of a claim reads and cancels a
// private project's request of the claim only when its own account maps to that project.
package api_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
)

// pubReason names an issue of PUB, a public project, so a request signed with it touches a
// project and is hidden from nobody.
const pubReason = "PUB-3: rotate the wiki key"

// privAndPubDispatch is privDispatch with PUB beside it, public and owned by testApprover.
func privAndPubDispatch(t *testing.T) *projectaccesstest.Server {
	t.Helper()
	return projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PRIV": {Visibility: "private", Owner: testApprover, Listed: []string{hiddenInsider}},
		"PUB":  {Visibility: "public", Owner: testApprover},
	}, nil)
}

// sessionAs enrolls a session of operator on a box of its own, records the person as its owner,
// as enroll.Service.Create records a person's (exchange.PersonPrincipal), so every session of one
// person shares an owner, and has its enrollment name projects, as a session started with
// AGENT_SECRETS_PROJECTS does.
func (ts *testServer) sessionAs(t *testing.T, box, operator string, projects ...string) enrolled {
	t.Helper()
	id, key := ts.newSessionEnrollment(t, "session-"+box+"-"+t.Name(), operator)
	if projects == nil {
		projects = []string{}
	}
	if _, err := ts.Store.Pool.Exec(t.Context(), `update enrollments set owner=$2, projects=$3 where id=$1`, id, exchange.PersonPrincipal(operator), projects); err != nil {
		t.Fatalf("record the owner and projects of %s: %v", box, err)
	}
	return enrolled{id: id, key: key}
}

// as is e's call of method path with no body, read as readAnswer reads an answer.
func (ts *testServer) as(t *testing.T, e enrolled, method, path string) answerRead {
	t.Helper()
	status, body := ts.session(t, e.key, e.id, method, path, nil)
	return answerOf(t, status, body)
}

// askWith has e ask for name with reason and fails t unless the request is in state want.
func (ts *testServer) askWith(t *testing.T, e enrolled, reason, name, want string) wireCreateRequestResponse {
	t.Helper()
	status, body := ts.session(t, e.key, e.id, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, e.key, ts.URL, reason, name), "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)
	if status != http.StatusOK || created.State != want {
		t.Fatalf("POST /v1/requests for %s (%q) = %d %s; want %s", name, reason, status, body, want)
	}
	return created
}

// sessionRoute is one of the four session routes that take an id, with the answer it gives an id
// nothing has.
type sessionRoute struct {
	name, method string
	path         func(id string) string
	grant        bool
	status       int
	code         string
}

var sessionRoutes = []sessionRoute{
	{"read", http.MethodGet, func(id string) string { return "/v1/requests/" + id }, false, http.StatusNotFound, "NOT_FOUND"},
	{"cancel", http.MethodPost, func(id string) string { return "/v1/requests/" + id + "/cancel" }, false, http.StatusNotFound, "NOT_FOUND"},
	{"values", http.MethodPost, func(id string) string { return "/v1/grants/" + id + "/values" }, true, http.StatusForbidden, "GRANT_NOT_LIVE"},
	{"revoke", http.MethodPost, func(id string) string { return "/v1/grants/" + id + "/revoke" }, true, http.StatusNotFound, "NOT_FOUND"},
}

// sessionHiddenRig is a broker whose Dispatch holds PRIV, private and testApprover's with carol on
// its list, and PUB, public, and these sessions: holder, testApprover's, naming PRIV, which asked
// for SHARED_KEY with privReason (hiddenRequest, pending) and holds DEEL_API_KEY's grant asked
// with privReason too (hiddenGrant); sameNaming and sameBlind, two more sessions of testApprover,
// the first naming PRIV and the second naming nothing; other, mallory's, naming nothing; and
// plain, testApprover's, naming nothing, which asked for SHARED_KEY with pubReason
// (publicRequest, pending) and holds WORKER_TOKEN's grant asked with pubReason (publicGrant).
//
// Its token exchange maps workerAccount to PRIV and otherAccount to PUB, both for testApprover, as
// BROKER_EXCHANGE_ACCOUNTS maps a private project's agents, and the Legion daemon's login enrols
// four pods of one claim, so all four share an owner: two of workerAccount that each asked for
// DEEL_API_KEY with a reason naming no issue (podHidden, podServed: pending, and PRIV's only
// because the credential that asked names PRIV), and two that ask nothing, podMapped of
// workerAccount and podElsewhere of otherAccount.
type sessionHiddenRig struct {
	ts                            *testServer
	access                        *projectaccesstest.Swappable
	holder, sameNaming, sameBlind enrolled
	other                         enrolled
	hiddenRequest, hiddenGrant    string
	publicRequest, publicGrant    string
	missingRequest, missingGrant  string
	podMapped, podElsewhere       enrolled
	podHidden, podServed          string
}

func newSessionHiddenRig(t *testing.T) *sessionHiddenRig {
	t.Helper()
	pods, err := exchange.NewSet([]exchange.AccountEntry{
		{Account: workerAccount, Project: "PRIV", Person: testApprover},
		{Account: otherAccount, Project: "PUB", Person: testApprover},
	}, nil, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	ts, access := hiddenServer(t, func(d *api.Deps) { d.Machine.Exchange = pods })
	access.Serve(privAndPubDispatch(t))
	r := &sessionHiddenRig{ts: ts, access: access, missingRequest: uuid.NewString(), missingGrant: uuid.NewString()}
	r.holder = ts.sessionAs(t, "holder", testApprover, "PRIV")
	r.sameNaming = ts.sessionAs(t, "same-naming", testApprover, "PRIV")
	r.sameBlind = ts.sessionAs(t, "same-blind", testApprover)
	r.other = ts.sessionAs(t, "other", hiddenOutsider)
	plain := ts.sessionAs(t, "plain", testApprover)
	r.hiddenRequest = ts.askWith(t, r.holder, privReason, "SHARED_KEY", "pending").RequestID
	r.hiddenGrant = ts.approveAs(t, ts.askWith(t, r.holder, privReason, "DEEL_API_KEY", "pending"), testApprover)
	r.publicRequest = ts.askWith(t, plain, pubReason, "SHARED_KEY", "pending").RequestID
	r.publicGrant = *ts.askWith(t, plain, pubReason, "WORKER_TOKEN", "granted").GrantID

	daemon := ts.serviceLogin(t)
	pod := func(account, session string) enrolled {
		t.Helper()
		return ts.enrolPodWith(t, daemon, podEnrolment{account: account, podUID: uuid.NewString(), claim: plannerClaim, session: session})
	}
	r.podHidden = ts.askPending(t, pod(workerAccount, "planner-session-hidden"), "DEEL_API_KEY").RequestID
	r.podServed = ts.askPending(t, pod(workerAccount, "planner-session-served"), "DEEL_API_KEY").RequestID
	r.podMapped = pod(workerAccount, "planner-session-mapped")
	r.podElsewhere = pod(otherAccount, "planner-session-elsewhere")
	return r
}

// ids is the hidden id and the missing id route takes, by what each names.
func (r *sessionHiddenRig) ids(route sessionRoute) map[string]string {
	if route.grant {
		return map[string]string{"the hidden grant": r.hiddenGrant, "a grant id no grant has": r.missingGrant}
	}
	return map[string]string{"the hidden request": r.hiddenRequest, "a request id no request has": r.missingRequest}
}

// requireUntouched fails t unless the hidden request is still pending and the hidden grant live.
func (r *sessionHiddenRig) requireUntouched(t *testing.T) {
	t.Helper()
	if got := r.ts.column(t, `select state from requests where id=$1`, r.hiddenRequest); len(got) != 1 || got[0] != "pending" {
		t.Errorf("the hidden request's state = %v; want it pending", got)
	}
	if got := r.ts.column(t, `select (revoked_at is null)::text from grants where id=$1`, r.hiddenGrant); len(got) != 1 || got[0] != "true" {
		t.Errorf("the hidden grant live = %v; want it live", got)
	}
}

// requirePending fails t unless request id is still pending.
func (r *sessionHiddenRig) requirePending(t *testing.T, id, what string) {
	t.Helper()
	if got := r.ts.column(t, `select state from requests where id=$1`, id); len(got) != 1 || got[0] != "pending" {
		t.Errorf("%s's state = %v; want it pending", what, got)
	}
}

// requireHiddenFrom fails t unless every session route answers s the hidden id exactly as it
// answers an id nothing has, in status and body.
func (r *sessionHiddenRig) requireHiddenFrom(t *testing.T, s enrolled, who string) {
	t.Helper()
	for _, route := range sessionRoutes {
		t.Run(route.name, func(t *testing.T) {
			answers := map[string]answerRead{}
			for what, id := range r.ids(route) {
				answers[what] = r.ts.as(t, s, route.method, route.path(id))
			}
			requireSameAnswers(t, route.name+" by "+who, answers)
			for what, got := range answers {
				requireAnswered(t, route.name+" by "+who+" of "+what, got, route.status, route.code)
			}
		})
	}
}

// TestASessionsRoutesAnswerAnIDOfAProjectItCannotSeeAsAnIDNothingHas holds every row of the
// session routes' project check, in one broker so each row reads the same ids. Its subtests run in
// order: the rows that must leave the hidden requests and grant as they are come first, and the
// rows that cancel and release them last.
func TestASessionsRoutesAnswerAnIDOfAProjectItCannotSeeAsAnIDNothingHas(t *testing.T) {
	r := newSessionHiddenRig(t)
	ts := r.ts

	t.Run("another owner's id of a project it cannot see answers as an id nothing has", func(t *testing.T) {
		r.requireHiddenFrom(t, r.other, hiddenOutsider)
		r.requireUntouched(t)
	})

	t.Run("another owner's id of no private project is still NOT_YOURS", func(t *testing.T) {
		for _, route := range sessionRoutes {
			id, what := r.publicRequest, "request"
			if route.grant {
				id, what = r.publicGrant, "grant"
			}
			requireAnswered(t, route.name+" by "+hiddenOutsider+" of testApprover's PUB "+what,
				ts.as(t, r.other, route.method, route.path(id)), http.StatusForbidden, "NOT_YOURS")
		}
	})

	t.Run("a session of the same owner that does not name the project reads its id as an id nothing has", func(t *testing.T) {
		r.requireHiddenFrom(t, r.sameBlind, "testApprover's session naming no project")
		r.requireUntouched(t)
	})

	t.Run("a pod of the same claim whose account maps elsewhere reads its sibling's request of PRIV as an id nothing has", func(t *testing.T) {
		for _, route := range sessionRoutes {
			if route.grant {
				continue
			}
			answers := map[string]answerRead{
				"the sibling's request of PRIV": ts.as(t, r.podElsewhere, route.method, route.path(r.podHidden)),
				"a request id no request has":   ts.as(t, r.podElsewhere, route.method, route.path(r.missingRequest)),
			}
			requireSameAnswers(t, route.name+" by the pod mapped to PUB", answers)
			requireAnswered(t, route.name+" by the pod mapped to PUB of the sibling's request", answers["the sibling's request of PRIV"], route.status, route.code)
		}
		r.requirePending(t, r.podHidden, "the sibling's request of PRIV")
	})

	down := downDispatch(t)
	r.access.Serve(down)
	t.Run("while Dispatch cannot answer, an id that touches a project is 503 and nothing changes", func(t *testing.T) {
		for _, caller := range []struct {
			who string
			s   enrolled
		}{{hiddenOutsider, r.other}, {"testApprover's session naming no project", r.sameBlind}} {
			for _, route := range sessionRoutes {
				id := r.hiddenRequest
				if route.grant {
					id = r.hiddenGrant
				}
				requireAnswered(t, route.name+" by "+caller.who+" while Dispatch is down", ts.as(t, caller.s, route.method, route.path(id)),
					http.StatusServiceUnavailable, "PROJECT_ACCESS_UNAVAILABLE")
			}
		}
		r.requireUntouched(t)
		if down.Calls() == 0 {
			t.Fatal("the down Dispatch was never asked: the rows above are blind")
		}
	})
	t.Run("while Dispatch cannot answer, a pod of the same claim is 503 for its sibling's request of PRIV and cancels nothing", func(t *testing.T) {
		before := down.Calls()
		for _, caller := range []struct {
			who string
			s   enrolled
		}{{"the pod mapped to PRIV", r.podMapped}, {"the pod mapped to PUB", r.podElsewhere}} {
			for _, route := range sessionRoutes {
				if route.grant {
					continue
				}
				requireAnswered(t, route.name+" by "+caller.who+" while Dispatch is down", ts.as(t, caller.s, route.method, route.path(r.podHidden)),
					http.StatusServiceUnavailable, "PROJECT_ACCESS_UNAVAILABLE")
			}
		}
		r.requirePending(t, r.podHidden, "the sibling's request of PRIV")
		if down.Calls() == before {
			t.Fatal("the down Dispatch was never asked for the pods: the rows above are blind")
		}
	})
	r.access.Serve(privAndPubDispatch(t))

	t.Run("the holder whose reason cites a private project it does not name still reads and releases", func(t *testing.T) {
		blindHolder := ts.sessionAs(t, "blind-holder", testApprover)
		own := ts.askWith(t, blindHolder, privReason, "SHARED_KEY", "pending")
		for _, path := range []string{"/v1/requests/" + own.RequestID + "?wait=1", "/v1/requests/" + own.RequestID} {
			status, body := ts.session(t, blindHolder.key, blindHolder.id, http.MethodGet, path, nil)
			if got := decode[wireRequestStatus](t, body); status != http.StatusOK || got.State != "pending" {
				t.Fatalf("GET %s by the session that asked = %d %s; want 200 pending", path, status, body)
			}
		}
		ts.decide(t, testApprover, *own.RecordID, true)
		grant := ts.grantOf(t, own.RequestID)
		status, body := ts.session(t, blindHolder.key, blindHolder.id, http.MethodPost, "/v1/grants/"+grant+"/values", nil)
		if got := decode[struct {
			Values map[string]string `json:"values"`
		}](t, body); status != http.StatusOK || got.Values["SHARED_KEY"] != "shared-v1" {
			t.Fatalf("the release of its own grant = %d %s; want 200 with SHARED_KEY", status, body)
		}
	})

	t.Run("a session of the same owner that names the project still reads and cancels", func(t *testing.T) {
		status, body := ts.session(t, r.sameNaming.key, r.sameNaming.id, http.MethodGet, "/v1/requests/"+r.hiddenRequest, nil)
		if got := decode[wireRequestStatus](t, body); status != http.StatusOK || got.RecordID == nil {
			t.Fatalf("the read by testApprover's other session naming PRIV = %d %s; want 200 naming its record", status, body)
		}
		for _, route := range sessionRoutes {
			if !route.grant {
				continue
			}
			requireAnswered(t, route.name+" of the holder's grant by testApprover's other session naming PRIV",
				ts.as(t, r.sameNaming, route.method, route.path(r.hiddenGrant)), http.StatusForbidden, "NOT_YOURS")
		}
		// A request of its own, so the cancel leaves the ids the other rows read as they are.
		asker := ts.sessionAs(t, "asker", testApprover, "PRIV")
		pending := ts.askWith(t, asker, privReason, "SHARED_KEY", "pending")
		status, body = ts.session(t, r.sameNaming.key, r.sameNaming.id, http.MethodPost, "/v1/requests/"+pending.RequestID+"/cancel", nil)
		if status != http.StatusOK || decode[stateBody](t, body).State != "cancelled" {
			t.Fatalf("the cancel by testApprover's other session naming PRIV = %d %s; want 200 cancelled", status, body)
		}
	})

	t.Run("a pod of the same claim whose account maps to PRIV reads and cancels its sibling's request of PRIV", func(t *testing.T) {
		ts.wantState(t, r.podMapped, r.podServed, "pending")
		ts.wantCancelled(t, r.podMapped, r.podServed)
	})
}
