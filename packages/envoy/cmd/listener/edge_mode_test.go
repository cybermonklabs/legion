package main

// edge_mode_test.go holds the edge listener's request rules on a real bus: the admission, the
// topics a session follows and the stored topics a plan brings in, the roles a session claims,
// the sends and publishes, the reads, the start, and the record of the registering token's
// projects on each session. The rows of the roles a session brings to the edge and of a role read
// naming an outside holder are in edge_roles_test.go, the GitHub rows in
// edge_repositories_test.go, and the rig every one of them runs on is in edge_rig_test.go.
//
// Every row reads the projects its listener serves from ENVOY_EDGE_PROJECTS when the test's own
// environment sets it, and serves PRIV otherwise (edgeTestProjects). So the control every row is
// run under once is the same command with ENVOY_EDGE_PROJECTS set empty: the listener is in no
// edge mode, answers as every listener does today, and each row fails naming the call it expected
// refused.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// The admission: an edge listener admits a broker token alone, and only one whose projects are
// all among the ones it serves, of project scope named or person; every other caller is refused
// before any handler runs, and a refusal names what the token carries and never a project the
// edge serves that the token does not list, which may be another customer's private project. The
// exported admission (EdgeAdmits) is the very rule apiAuth asks.
func TestEdgeAdmitsOnlyTokensOfItsProjects(t *testing.T) {
	projects := edgeTestProjects(t)
	broker := ciphertokentest.NewStubBroker(t)
	caller := ciphertokentest.NewCaller(t)
	reached := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	})
	logger := logging.NewWithWriter("test-machine", io.Discard)
	handler := apiAuth(listenerAPIAuth{brokers: listenerBrokerTokens(t, broker), edgeProjects: projects, logger: logger}, next)
	answer := func(session, scope string, projects ...string) ciphertoken.Introspection {
		got := sessionAnswer(session, "outside@example.com", caller.JKT())
		got.ProjectScope, got.Projects = scope, projects
		return got
	}
	service := serviceAnswer("ses_service", "example-fleet", caller.JKT())
	for _, tc := range []struct {
		name   string
		answer *ciphertoken.Introspection
		header string
		admit  bool
		status int
	}{
		{name: "a named token of the project it serves", answer: new(answer("ses_named", ciphertoken.ProjectScopeNamed, "PRIV")), admit: true},
		{name: "a person token listing only that project", answer: new(answer("ses_person", ciphertoken.ProjectScopePerson, "PRIV")), admit: true},
		{name: "a person token listing a project it does not serve beside it", answer: new(answer("ses_two", ciphertoken.ProjectScopePerson, "INFRA", "PRIV")), status: http.StatusForbidden},
		{name: "a named token of another project", answer: new(answer("ses_other", ciphertoken.ProjectScopeNamed, "INFRA")), status: http.StatusForbidden},
		{name: "a person token listing no project", answer: new(answer("ses_none", ciphertoken.ProjectScopePerson)), status: http.StatusForbidden},
		{name: "a service's token, which lists none", answer: &service, status: http.StatusForbidden},
		{name: "a read-only token", answer: new(readOnlyAnswer("ses_reader", "example-reader", caller.JKT())), status: http.StatusUnauthorized},
		{name: "no credential at all", status: http.StatusUnauthorized},
		{name: "a shared bearer", header: "Bearer some-shared-token", status: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached = 0
			var request *http.Request
			switch {
			case tc.answer != nil:
				token := "token-" + tc.answer.Session
				broker.Says(token, *tc.answer)
				request = brokerTokenRequest(t, caller, http.MethodGet, "/v1/sessions", token, "")
			default:
				request = httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
				request.Host = "127.0.0.1:9020"
				if tc.header != "" {
					request.Header.Set("Authorization", tc.header)
				}
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if tc.admit {
				if recorder.Code != http.StatusNoContent || reached != 1 {
					t.Fatalf("GET /v1/sessions under %s: status %d, reached the handler %d times (%s), want it admitted", tc.name, recorder.Code, reached, recorder.Body.String())
				}
				return
			}
			if recorder.Code != tc.status || reached != 0 {
				t.Fatalf("GET /v1/sessions under %s: status %d, reached the handler %d times (%s), want %d before any handler", tc.name, recorder.Code, reached, recorder.Body.String(), tc.status)
			}
			if tc.answer != nil {
				requireNoServedProjectNamed(t, recorder.Body.String(), projects, tc.answer.Projects, "the refusal of "+tc.name)
			}
		})
	}
	// The exported rule, as apiAuth asks it of each request's caller.
	for _, tc := range []struct {
		caller ciphertoken.Caller
		admit  bool
	}{
		{ciphertoken.Caller{Session: "s", Person: "p", ProjectScope: ciphertoken.ProjectScopeNamed, Projects: []string{"PRIV"}}, true},
		{ciphertoken.Caller{Session: "s", Person: "p", ProjectScope: ciphertoken.ProjectScopePerson, Projects: []string{"INFRA", "PRIV"}}, false},
		{ciphertoken.Caller{Session: "s", Person: "p", ProjectScope: ciphertoken.ProjectScopePerson}, false},
		{ciphertoken.Caller{Session: "s", Service: "f", ProjectScope: ciphertoken.ProjectScopeNone, ReadOnly: true}, false},
		{ciphertoken.Caller{Session: "s", Person: "p", ProjectScope: "elsewhere", Projects: []string{"PRIV"}}, false},
	} {
		err := EdgeAdmits(projects, tc.caller)
		if tc.admit != (err == nil) {
			t.Errorf("EdgeAdmits(%v, %+v) = %v, want admitted %t", projects, tc.caller, err, tc.admit)
		}
		if err != nil && !errors.Is(err, ErrEdgeNotAdmitted) {
			t.Errorf("EdgeAdmits(%v, %+v) = %v, which is not ErrEdgeNotAdmitted", projects, tc.caller, err)
		}
		if err != nil {
			requireNoServedProjectNamed(t, err.Error(), projects, tc.caller.Projects, fmt.Sprintf("EdgeAdmits(%+v)", tc.caller))
		}
	}
}

// requireNoServedProjectNamed fails the test when text, an admission refusal's, names a project
// of served that listed, the refused token's own projects, does not hold: the edge tells a token
// it refuses nothing of the projects it serves.
func requireNoServedProjectNamed(t *testing.T, text string, served, listed []string, what string) {
	t.Helper()
	for _, project := range served {
		if !slices.Contains(listed, project) && strings.Contains(text, project) {
			t.Errorf("%s reads %q: it names %s, a project the edge serves and the token does not list", what, text, project)
		}
	}
}

// Every subject of no project but GitHub's is refused 403 topic_not_in_project: the
// intakes, the shared wake, the listener's own lanes.
func TestAnEdgeRefusesEverySubjectOfNoProjectButGitHub(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_no_project"
	token := rig.personToken(sessionID, "PRIV")
	for _, topic := range []string{
		"notifications.slack.>",
		"notifications.whatsapp.>",
		"notifications.ghostwispr.>",
		"notifications.team.>",
		"notifications.legion.wake",
		"notifications.envoy.kept.role.x",
	} {
		requireTopicRefusal(t, rig.subscribe(t, token, sessionID, topic), "topic_not_in_project", topic)
	}
}

// The publish rule: an edge session publishes only a role subject of its project in the form
// that names the project, for a role that works for that project (contracts.ClaimProject): a
// Legion role of the project, or a free-form role under it. Anything else is 403
// topic_not_published, a Legion role of another project named under this one among it.
func TestEdgePublishesOnlyRoleSubjectsOfItsProjects(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_publisher"
	token := rig.personToken(sessionID, "PRIV")
	for _, topic := range []string{
		"notifications.dispatch.issue.PRIV.PRIV-1.comment.created",
		"notifications.team.priv",
		"notifications.role.legion-priv-priv-1-tester",
		"notifications.role.INFRA.legion-infra-infra-1-tester",
		"notifications.role.PRIV.legion-infra-infra-1-tester",
		"notifications.role._.lead-reviewer",
		"notifications.role.INFRA.lead-reviewer",
		"notifications.github.acme.pub.pr.1",
	} {
		body := fmt.Sprintf(`{"topic":%q,"message":"from the edge","source":"agent","source_session":%q}`, topic, sessionID)
		requireEdgeRefusal(t, rig.call(t, http.MethodPost, token, "/v1/messages/publish", body), http.StatusForbidden, "topic_not_published", "a publish to "+topic)
	}
	for _, role := range []string{contracts.RoleSubject("PRIV", "legion-priv-priv-1-tester"), contracts.RoleSubject("PRIV", "lead-reviewer")} {
		body := fmt.Sprintf(`{"topic":%q,"message":"for the role's holder","source":"agent","source_session":%q}`, role, sessionID)
		recorder := rig.call(t, http.MethodPost, token, "/v1/messages/publish", body)
		if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
			t.Fatalf("a publish to %s: status %d (%s), want it taken", role, recorder.Code, recorder.Body.String())
		}
	}
}

// The send rule: an edge session sends only to a session whose token lists one of its token's
// projects; a send to any other is answered as to a missing session, and a publish to a role
// subject names one of its projects.
func TestEdgeSendsOnlyToSessionsOfTheTokensProjects(t *testing.T) {
	rig := newEdgeRig(t)
	const sender, peer = "ses_edge_sender", "ses_edge_peer"
	token := rig.personToken(sender, "PRIV")
	requireStatus(t, rig.subscribe(t, token, sender), http.StatusOK, "the sender's registration")
	requireStatus(t, rig.subscribe(t, rig.namedToken(peer, "PRIV"), peer), http.StatusOK, "the peer's registration")
	rig.placeElsewhere(t, "ses_infra_agent", "INFRA")
	rig.placeElsewhere(t, "ses_no_project")
	send := func(target string) *httptest.ResponseRecorder {
		return rig.call(t, http.MethodPost, token, "/v1/messages/send",
			fmt.Sprintf(`{"target_session":%q,"source_session":%q,"message":"from the edge","source":"agent"}`, target, sender))
	}
	requireStatus(t, send(peer), http.StatusOK, "a send to "+peer)
	for _, target := range []string{"ses_infra_agent", "ses_no_project"} {
		recorder := send(target)
		var refusal listenerwire.NoLiveSessionRefusal
		_ = json.Unmarshal(recorder.Body.Bytes(), &refusal)
		if recorder.Code != http.StatusNotFound || !strings.HasPrefix(refusal.Error, "no live session "+target) || refusal.LastSeen != 0 {
			t.Errorf("a send to %s: status %d (%s), want the 404 a missing session gets, naming no last registration", target, recorder.Code, recorder.Body.String())
		}
	}
	body := fmt.Sprintf(`{"topic":%q,"message":"from the edge","source":"agent","source_session":%q}`, contracts.RoleSubject("INFRA", "legion-infra-infra-1-tester"), sender)
	requireEdgeRefusal(t, rig.call(t, http.MethodPost, token, "/v1/messages/publish", body), http.StatusForbidden, "topic_not_published", "a publish to INFRA's role")
}

// The read rule: the edge's reads answer only the sessions and roles of the token's projects, and
// the interest list, another session's interests and every delete naming another session are 403.
func TestEdgeReadsListOnlyTheTokensProjects(t *testing.T) {
	rig := newEdgeRig(t)
	const reader, peer = "ses_edge_reader", "ses_edge_peer_read"
	token := rig.personToken(reader, "PRIV")
	requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
	requireStatus(t, rig.subscribe(t, rig.namedToken(peer, "PRIV"), peer), http.StatusOK, "the peer's registration")
	rig.placeElsewhere(t, "ses_infra_agent", "INFRA")
	rig.placeElsewhere(t, "ses_no_project")
	for role, holder := range map[string]string{"legion-priv-priv-2-tester": peer, "legion-infra-infra-2-tester": peer} {
		if _, err := rig.deps.registry.SetRole(holder, "test-machine", role, false); err != nil {
			t.Fatalf("claim %s for %s: %v", role, holder, err)
		}
	}
	testwait.Eventually(t, "the role claim copy to name both claims", func() bool {
		held, err := rig.deps.registry.RolesHeldBy(peer)
		return err == nil && len(held) == 2
	})
	ctx := t.Context()
	agents := rig.agents
	for _, entry := range []legionagents.Entry{
		{SessionID: peer, LegionProject: "priv", Issue: "PRIV-2", Role: "tester", RoleToken: "legion-priv-priv-2-tester", Title: "Legion tester · PRIV-2", Harness: "omp", StartedAt: "2026-11-20T00:00:00Z"},
		{SessionID: "ses_infra_agent", LegionProject: "infra", Issue: "INFRA-2", Role: "tester", RoleToken: "legion-infra-infra-2-tester", Title: "Legion tester · INFRA-2", Harness: "omp", StartedAt: "2026-11-20T00:00:00Z"},
	} {
		if _, err := agents.Put(ctx, entry); err != nil {
			t.Fatalf("record the legion agent %s: %v", entry.SessionID, err)
		}
	}

	listed := rig.call(t, http.MethodGet, token, "/v1/sessions", "")
	requireStatus(t, listed, http.StatusOK, "GET /v1/sessions")
	var rows []listenerwire.SessionInfo
	if err := json.Unmarshal(listed.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode %s: %v", listed.Body.String(), err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.SessionID)
		if row.SessionID == peer && !slices.Equal(row.Roles, []string{"legion-priv-priv-2-tester"}) {
			t.Errorf("GET /v1/sessions lists %s with roles %v, want only its PRIV role", peer, row.Roles)
		}
	}
	slices.Sort(ids)
	if want := []string{peer, reader}; !slices.Equal(ids, want) {
		t.Errorf("GET /v1/sessions lists %v, want only the sessions of PRIV %v", ids, want)
	}
	for _, other := range []string{"ses_infra_agent", "ses_no_project"} {
		recorder := rig.call(t, http.MethodGet, token, "/v1/registry/"+other, "")
		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET /v1/registry/%s: status %d (%s), want the 404 of a missing session", other, recorder.Code, recorder.Body.String())
		}
	}
	requireStatus(t, rig.call(t, http.MethodGet, token, "/v1/registry/"+peer, ""), http.StatusOK, "GET /v1/registry/"+peer)

	legion := func(path string) []string {
		t.Helper()
		recorder := rig.call(t, http.MethodGet, token, path, "")
		requireStatus(t, recorder, http.StatusOK, "GET "+path)
		var list listenerwire.LegionAgentList
		if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode %s: %v", recorder.Body.String(), err)
		}
		sessions := make([]string, 0, len(list.Entries))
		for _, entry := range list.Entries {
			sessions = append(sessions, entry.SessionID)
		}
		return sessions
	}
	if got := legion("/v1/legion/agents?issue=INFRA-2"); len(got) != 0 {
		t.Errorf("GET /v1/legion/agents?issue=INFRA-2 answers %v, want none", got)
	}
	if got := legion("/v1/legion/agents?issue=PRIV-2"); !slices.Equal(got, []string{peer}) {
		t.Errorf("GET /v1/legion/agents?issue=PRIV-2 answers %v, want %s", got, peer)
	}
	if got := legion("/v1/legion/agents/roles/legion-infra-infra-2-tester"); len(got) != 0 {
		t.Errorf("GET /v1/legion/agents/roles/legion-infra-infra-2-tester answers %v, want none", got)
	}
	if recorder := rig.call(t, http.MethodGet, token, "/v1/legion/agents/sessions/ses_infra_agent", ""); recorder.Code != http.StatusNotFound {
		t.Errorf("GET /v1/legion/agents/sessions/ses_infra_agent: status %d (%s), want 404", recorder.Code, recorder.Body.String())
	}
	requireStatus(t, rig.call(t, http.MethodGet, token, "/v1/legion/agents/sessions/"+peer, ""), http.StatusOK, "GET /v1/legion/agents/sessions/"+peer)
	requireEdgeRefusal(t, rig.call(t, http.MethodPut, token, "/v1/legion/agents", `{"entries":[]}`), http.StatusForbidden, "legion_registry_not_written", "PUT /v1/legion/agents")

	requireEdgeRefusal(t, rig.call(t, http.MethodGet, token, "/v1/interests/", ""), http.StatusForbidden, "session_not_in_project", "GET /v1/interests/")
	requireEdgeRefusal(t, rig.call(t, http.MethodGet, token, "/v1/interests/ses_infra_agent", ""), http.StatusForbidden, "session_not_in_project", "GET /v1/interests/ses_infra_agent")
	requireStatus(t, rig.call(t, http.MethodGet, token, "/v1/interests/"+reader, ""), http.StatusOK, "GET /v1/interests/"+reader)
	for _, path := range []string{"/v1/interests/ses_infra_agent", "/v1/sessions/ses_infra_agent"} {
		if recorder := rig.call(t, http.MethodDelete, token, path, ""); recorder.Code != http.StatusForbidden {
			t.Errorf("DELETE %s: status %d (%s), want 403", path, recorder.Code, recorder.Body.String())
		}
	}
}

// Every listener that verifies broker tokens records the registering token's projects on the
// session's entry, sorted, and [] for a registration that lists none, so the edge's send and read
// rules can read the target's projects. It holds with edge mode off too: the field is every
// listener's, and the API's answers do not carry it.
func TestEdgeSessionEntryRecordsTheRegisteringTokensProjects(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_k6"
	requireStatus(t, rig.subscribe(t, rig.personToken(sessionID, "PRIV"), sessionID), http.StatusOK, "the registration")
	entry, err := rig.deps.sessions.Refresh(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("read %s's entry: %v", sessionID, err)
	}
	if !slices.Equal(entry.Projects, []string{"PRIV"}) {
		t.Fatalf("%s's entry records projects %v, want [PRIV]", sessionID, entry.Projects)
	}
	open := httptest.NewRecorder()
	rig.mux.ServeHTTP(open, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(consumerRegistration("ses_edge_k6_open", "epoch-1"))))
	requireStatus(t, open, http.StatusOK, "a registration with no broker token")
	stored, err := rig.deps.sessions.Refresh(t.Context(), "ses_edge_k6_open")
	if err != nil {
		t.Fatalf("read ses_edge_k6_open's entry: %v", err)
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Projects == nil || len(stored.Projects) != 0 || !strings.Contains(string(raw), `"projects":[]`) {
		t.Fatalf("a registration with no token records %s, want projects []", raw)
	}
	listed := rig.call(t, http.MethodGet, rig.personToken(sessionID, "PRIV"), "/v1/registry/"+sessionID, "")
	if strings.Contains(listed.Body.String(), `"projects"`) {
		t.Fatalf("GET /v1/registry/%s answers %s, which carries the entry's projects: the API's answers are unchanged", sessionID, listed.Body.String())
	}
}

// The start rule: an edge listener refuses to start beside any credential that admits a caller by
// something other than a broker token, beside webhooks, and without the whole broker-token group;
// each refusal names the variable. The last row starts the built binary that way.
func TestEdgeRefusesToStartBesideSharedCredentials(t *testing.T) {
	projects := edgeTestProjects(t)
	broker := []string{
		"ENVOY_AGENT_SECRETS_URL=http://broker.example",
		"ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE=/var/run/secrets/broker/token",
		"ENVOY_PUBLIC_URL=https://edge.example",
	}
	for _, tc := range []struct {
		extra []string
		drop  string
		names string
	}{
		{extra: []string{"ENVOY_API_TOKEN=shared"}, names: "ENVOY_API_TOKEN"},
		{extra: []string{"ENVOY_OIDC_ISSUER=https://issuer.example", "ENVOY_OIDC_AUDIENCE=envoy"}, names: "ENVOY_OIDC_ISSUER"},
		{extra: []string{"ENVOY_OIDC_ACCOUNTS=system:serviceaccount:a:b"}, names: "ENVOY_OIDC_ACCOUNTS"},
		{extra: []string{"ENVOY_OIDC_GOOGLE_AUDIENCE=envoy"}, names: "ENVOY_OIDC_GOOGLE_AUDIENCE"},
		{extra: []string{"ENVOY_WEBHOOKS=github"}, names: "ENVOY_WEBHOOKS"},
		{extra: []string{"ENVOY_GITHUB_WEBHOOK_SECRET=s"}, names: "ENVOY_GITHUB_WEBHOOK_SECRET"},
		{extra: []string{"ENVOY_SLACK_SIGNING_SECRET=s"}, names: "ENVOY_SLACK_SIGNING_SECRET"},
		{extra: []string{"ENVOY_GHOSTWISPR_SIGNING_SECRET=s"}, names: "ENVOY_GHOSTWISPR_SIGNING_SECRET"},
		{drop: "ENVOY_AGENT_SECRETS_URL", names: "ENVOY_AGENT_SECRETS_URL"},
		{drop: "ENVOY_PUBLIC_URL", names: "ENVOY_PUBLIC_URL"},
	} {
		env := slices.DeleteFunc(slices.Clone(broker), func(entry string) bool { return tc.drop != "" && strings.HasPrefix(entry, tc.drop+"=") })
		env = append(env, tc.extra...)
		err := validateEdgeMode(projects, env)
		if err == nil || !strings.Contains(err.Error(), tc.names) {
			t.Errorf("an edge listener of %v with %v: %v, want a refusal naming %s", projects, env, err, tc.names)
		}
	}
	if err := validateEdgeMode(projects, broker); err != nil {
		t.Fatalf("an edge listener with the broker-token group alone: %v, want it to start", err)
	}

	binary := cmdtest.Build(t, "envoy-listener")
	listener := startListenerProcess(t, binary, testnats.URL(t), "edge-refused",
		"ENVOY_EDGE_PROJECTS="+strings.Join(projects, ","))
	select {
	case <-listener.Exited:
	case <-time.After(30 * time.Second):
		t.Fatalf("the listener with ENVOY_EDGE_PROJECTS=%s beside ENVOY_API_TOKEN is still running after 30 s, want a refusal to start:\n%s",
			strings.Join(projects, ","), listener.Output.String())
	}
	if out := listener.Output.String(); listener.Cmd.ProcessState.Success() || !strings.Contains(out, "ENVOY_API_TOKEN") || !strings.Contains(out, "ENVOY_EDGE_PROJECTS") {
		t.Fatalf("the listener with ENVOY_EDGE_PROJECTS beside ENVOY_API_TOKEN exited %v, want a refusal naming both:\n%s", listener.Cmd.ProcessState, out)
	}
}

// An edge listener refuses to start beside ENVOY_DISPATCH_PRINCIPAL, in either of its shapes,
// naming the variable: Dispatch sends its role-marked messages through a shared listener, and no
// session of the edge sends as Dispatch, so the edge names no principal whose send may carry a
// role mark. A value of white space alone names no principal, and the edge starts.
func TestAnEdgeRefusesToStartBesideDispatchsPrincipal(t *testing.T) {
	projects := edgeTestProjects(t)
	broker := []string{
		"ENVOY_AGENT_SECRETS_URL=http://broker.example",
		"ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE=/var/run/secrets/broker/token",
		"ENVOY_PUBLIC_URL=https://edge.example",
	}
	for _, principal := range []string{"system:serviceaccount:dispatch:example-dispatch", "dispatch@example.iam.gserviceaccount.com"} {
		env := append(slices.Clone(broker), "ENVOY_DISPATCH_PRINCIPAL="+principal)
		err := validateEdgeMode(projects, env)
		if err == nil || !strings.Contains(err.Error(), "ENVOY_DISPATCH_PRINCIPAL") || !strings.Contains(err.Error(), "ENVOY_EDGE_PROJECTS") {
			t.Errorf("an edge listener of %v with ENVOY_DISPATCH_PRINCIPAL=%s: %v, want a refusal naming ENVOY_DISPATCH_PRINCIPAL and ENVOY_EDGE_PROJECTS", projects, principal, err)
		}
	}
	if err := validateEdgeMode(projects, append(slices.Clone(broker), "ENVOY_DISPATCH_PRINCIPAL= ")); err != nil {
		t.Fatalf("an edge listener whose ENVOY_DISPATCH_PRINCIPAL is white space alone: %v, want it to start", err)
	}
}

// A registration the edge refuses names exactly the topics it refuses, as the
// registration carried them, so the client drops those and registers the rest again; the allowed
// topic beside them is named nowhere, and nothing of the registration is stored.
func TestAnEdgeRefusalNamesOnlyTheRefusedTopics(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_mixed"
	token := rig.personToken(sessionID, "PRIV")
	const refused, allowed = "notifications.github.acme.priv.>", "notifications.github.acme.pub.>"
	requireTopicRefusal(t, rig.subscribe(t, token, sessionID, refused, allowed), "repository_not_in_project", refused)
	if _, err := rig.deps.registry.Get(t.Context(), sessionID); err == nil {
		t.Fatalf("a refused registration stored %s's interest record", sessionID)
	}
	// A refusal of both kinds names every topic it refuses, in the request's order, under the
	// reason of the kind that is not GitHub's.
	requireTopicRefusal(t, rig.subscribe(t, token, sessionID, "notifications.slack.>", allowed, refused), "topic_not_in_project",
		"notifications.slack.>", refused)
	requireStatus(t, rig.subscribe(t, token, sessionID, allowed), http.StatusOK, "the registration of the allowed topic alone")
}

// The listener links no package of Dispatch's store and no PostgreSQL driver: the edge reads a
// broker token's projects as the list the token carries (projectHeld) and asks nothing of
// internal/dispatch/access. A listing that names no package, the listener among them, has read
// nothing and fails as UNKNOWN.
func TestEdgeListenerLinksNoDispatchStore(t *testing.T) {
	const self = "github.com/sjawhar/envoy/cmd/listener"
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	listed, found := 0, false
	for line := range strings.Lines(string(out)) {
		path := strings.TrimSpace(line)
		listed++
		switch {
		case path == self:
			found = true
		case path == "github.com/sjawhar/envoy/internal/dispatch/store", strings.HasPrefix(path, "github.com/jackc/pgx"):
			t.Errorf("the listener links %s", path)
		}
	}
	if listed == 0 || !found {
		t.Fatalf("UNKNOWN: go list -deps listed %d packages and %s among them: %t", listed, self, found)
	}
}

// The topics a session's interest row already holds, which another listener stored for it under
// a token of other projects, are held to the edge's rule as the topics a registration names are:
// the session registers at the edge naming a topic of PRIV alone, and its row's subject of no
// project (notifications.slack.>), its INFRA issue topic and its GitHub topic of INFRA's
// repository are dropped from the row and from its consumer's filter, each named in the answer's
// warnings, while its GitHub topic of PRIV's repository stays. No event of a dropped topic reaches
// the session, and another PRIV session's GET /v1/sessions and GET /v1/registry/<id> show none of
// them.
func TestAnEdgeDropsTheStoredTopicsOfOtherProjects(t *testing.T) {
	rig := newEdgeRig(t)
	const moved, reader = "ses_edge_moved", "ses_edge_moved_reader"
	const (
		slack      = "notifications.slack.>"
		infra      = "notifications.dispatch.issue.INFRA.INFRA-7.>"
		infraRepo  = "notifications.github.acme.priv.>"
		privRepo   = "notifications.github.acme.pub.>"
		privIssues = "notifications.dispatch.issue.PRIV.>"
	)
	rig.placeElsewhereFollowing(t, moved, []string{}, slack, infra, infraRepo, privRepo)
	standIn := startSessionStandIn(t, rig.url, moved, answerDone)
	answer := rig.subscribe(t, rig.personToken(moved, "PRIV"), moved, privIssues)
	requireStatus(t, answer, http.StatusOK, "the moved session's registration at the edge")
	warnings := registrationWarnings(t, answer)
	for _, dropped := range []string{slack, infra, infraRepo} {
		requireWarning(t, warnings, dropped+" was removed")
	}
	row := bucketInterestRow(t, rig.harness.client, moved)
	for _, dropped := range []string{slack, infra, infraRepo} {
		if slices.Contains(row.Topics, dropped) {
			t.Errorf("after the registration at the edge %s's row still holds %s: %v", moved, dropped, row.Topics)
		}
	}
	if !slices.Contains(row.Topics, privRepo) || !slices.Contains(row.Topics, privIssues) {
		t.Errorf("after the registration at the edge %s's row is %v, want it to keep %s and %s", moved, row.Topics, privRepo, privIssues)
	}

	rig.storeEvent(t, "notifications.slack.T1.C1.message")
	rig.storeEvent(t, "notifications.dispatch.issue.INFRA.INFRA-7.comment.created")
	rig.storeGithubEvent(t, "acme/priv", "pr.8")
	priv, _ := rig.storeEvent(t, "notifications.dispatch.issue.PRIV.PRIV-1.comment.created")
	// The frames go in stream order, so PRIV's event arriving first is the proof that none of
	// the three before it was relayed.
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID != priv.EventID {
		t.Fatalf("the moved session's first frame is %s on %s, want PRIV's event: an event of a stored topic of another project reached it",
			frames[0].item.EventID, frames[0].item.Topic)
	}
	requireFrames(t, frames, priv)

	token := rig.namedToken(reader, "PRIV")
	requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
	shown := requireTopicsHidden(t, rig, token, moved, slack, infra, infraRepo)
	if !slices.Contains(shown, privIssues) {
		t.Errorf("GET /v1/registry/%s shows %v, want it to show %s, a topic of PRIV", moved, shown, privIssues)
	}
}

// A registration's plan made again after a conflict is held as its first plan is: another
// listener stores the session's row with a subject of no project and an INFRA issue topic while
// this listener's copy of the row still lacks them, so the registration's first plan, made from
// the copy, has its write refused, and its second plan, made from the bucket's row, merges them
// in. The edge drops both from that plan, so the row stored holds neither, and the answer names
// each.
func TestAnEdgeHoldsThePlanItMakesAgainAfterAConflict(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_replanned"
	const slack, infra, privIssues = "notifications.slack.>", "notifications.dispatch.issue.INFRA.INFRA-7.>", "notifications.dispatch.issue.PRIV.>"
	token := rig.personToken(sessionID, "PRIV")
	requireStatus(t, rig.subscribe(t, token, sessionID, privIssues), http.StatusOK, "the first registration")
	holdInterestCache(t, rig.deps.registry)
	other, err := store.Open(t.Context(), rig.harness.client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the other listener's registry: %v", err)
	}
	t.Cleanup(other.StopWatch)
	waitForRegistry(t, other)
	if _, err := other.Upsert(store.Interest{SessionID: sessionID, MachineID: "shared-listener"}, []string{slack, infra}); err != nil {
		t.Fatalf("store %s's row on the other listener: %v", sessionID, err)
	}
	if cached, _ := rig.deps.registry.Cached(sessionID); slices.Contains(cached.Topics, slack) || slices.Contains(cached.Topics, infra) {
		t.Fatalf("UNKNOWN: this listener's copy of %s's row holds %v already, so its first plan merges the other listener's topics and no plan is made again", sessionID, cached.Topics)
	}
	if stored := bucketInterestRow(t, rig.harness.client, sessionID); !slices.Contains(stored.Topics, slack) || !slices.Contains(stored.Topics, infra) {
		t.Fatalf("UNKNOWN: the bucket holds %s's row %v, without the other listener's topics", sessionID, stored.Topics)
	}

	answer := rig.subscribe(t, token, sessionID, privIssues)
	requireStatus(t, answer, http.StatusOK, "the registration after the other listener's write")
	row := bucketInterestRow(t, rig.harness.client, sessionID)
	if slices.Contains(row.Topics, slack) || slices.Contains(row.Topics, infra) || !slices.Contains(row.Topics, privIssues) {
		t.Fatalf("after a plan made again over the other listener's row, %s's row is %v, want %s and neither %s nor %s", sessionID, row.Topics, privIssues, slack, infra)
	}
	warnings := registrationWarnings(t, answer)
	for _, dropped := range []string{slack, infra} {
		requireWarning(t, warnings, dropped+" was removed")
	}
}

// A read at the edge shows a session's topics of the reader's projects alone, also before the
// session registers at the edge: a session another listener registered under a PRIV token, whose
// row holds a subject of no project, an INFRA issue topic and the GitHub topics of an INFRA and a
// PRIV repository beside its PRIV topic, is listed to a PRIV reader without the first three, by
// GET /v1/sessions and by GET /v1/registry/<id>, and the session's own GET /v1/interests/<id> at
// the edge shows its own record the same way.
func TestAnEdgeReadShowsOnlyTheTopicsOfTheTokensProjects(t *testing.T) {
	rig := newEdgeRig(t)
	const elsewhere, reader = "ses_edge_shown", "ses_edge_shown_reader"
	const (
		slack      = "notifications.slack.>"
		infra      = "notifications.dispatch.issue.INFRA.INFRA-7.>"
		infraRepo  = "notifications.github.acme.priv.>"
		privRepo   = "notifications.github.acme.pub.>"
		privIssues = "notifications.dispatch.issue.PRIV.>"
	)
	rig.placeElsewhereFollowing(t, elsewhere, []string{"PRIV"}, slack, infra, infraRepo, privRepo, privIssues)
	rig.tick(t)
	token := rig.namedToken(reader, "PRIV")
	requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
	shown := requireTopicsHidden(t, rig, token, elsewhere, slack, infra, infraRepo)
	for _, kept := range []string{privRepo, privIssues, contracts.LegacyAgentSubject(elsewhere)} {
		if !slices.Contains(shown, kept) {
			t.Errorf("GET /v1/registry/%s shows %v, want it to show %s", elsewhere, shown, kept)
		}
	}
	own := rig.call(t, http.MethodGet, rig.personToken(elsewhere, "PRIV"), "/v1/interests/"+elsewhere, "")
	requireStatus(t, own, http.StatusOK, "the session's own GET /v1/interests/"+elsewhere)
	var record listenerwire.InterestRow
	if err := json.Unmarshal(own.Body.Bytes(), &record); err != nil {
		t.Fatalf("decode %s: %v", own.Body.String(), err)
	}
	for _, hidden := range []string{slack, infra, infraRepo} {
		if slices.Contains(record.Topics, hidden) {
			t.Errorf("the session's own GET /v1/interests/%s shows %s: %v", elsewhere, hidden, record.Topics)
		}
	}
	if !slices.Contains(record.Topics, privIssues) {
		t.Errorf("the session's own GET /v1/interests/%s shows %v, want %s", elsewhere, record.Topics, privIssues)
	}
}

// A send at the edge is the session's own: one under another source (dispatch, human), and one
// that names no source session, is refused 403 sender_not_session, and the peer receives none of
// them; a send as agent naming the token's session reaches the peer with that session as its
// sender. Control: with edge mode off, the first refused send is taken.
func TestAnEdgeSendNamesItsOwnSession(t *testing.T) {
	rig := newEdgeRig(t)
	const sender, peer = "ses_edge_send_sender", "ses_edge_send_peer"
	senderToken := rig.personToken(sender, "PRIV")
	requireStatus(t, rig.subscribe(t, senderToken, sender), http.StatusOK, "the sender's registration")
	standIn := startSessionStandIn(t, rig.url, peer, answerDone)
	requireStatus(t, rig.subscribe(t, rig.namedToken(peer, "PRIV"), peer), http.StatusOK, "the peer's registration")
	payload := `{"event":{"type":"comment.created","actor":{"kind":"person","id":"owner@example.com"},"issue":"PRIV-1"}}`
	for _, refused := range []struct{ source, sourceSession string }{
		{"dispatch", ""}, {"human", ""}, {"dispatch", sender}, {"agent", ""}, {"", ""},
	} {
		body := fmt.Sprintf(`{"target_session":%q,"source_session":%q,"source":%q,"message":"approved, go ahead","payload":%q}`,
			peer, refused.sourceSession, refused.source, payload)
		requireEdgeRefusal(t, rig.call(t, http.MethodPost, senderToken, "/v1/messages/send", body), http.StatusForbidden, "sender_not_session",
			fmt.Sprintf("a send as source %q with source_session %q", refused.source, refused.sourceSession))
	}
	body := fmt.Sprintf(`{"target_session":%q,"source_session":%q,"source":"agent","message":"from the sender"}`, peer, sender)
	requireStatus(t, rig.call(t, http.MethodPost, senderToken, "/v1/messages/send", body), http.StatusOK, "a send as agent from "+sender)
	got := standIn.collect(t, 1, 10*time.Second)[0].item
	if got.Source != "agent" || got.SourceSession != sender {
		t.Fatalf("the peer's first frame is from source %q, source_session %q, want agent and %s: a refused send reached it", got.Source, got.SourceSession, sender)
	}
}

// A publish at the edge is the session's own, as a send is: a publish to a role subject of PRIV
// under another source, or naming no source session, is refused 403 sender_not_session, and one as
// agent naming the token's session is taken. Control: with edge mode off, the first refused
// publish is taken.
func TestAnEdgePublishNamesItsOwnSession(t *testing.T) {
	rig := newEdgeRig(t)
	const sender = "ses_edge_publish_sender"
	token := rig.personToken(sender, "PRIV")
	requireStatus(t, rig.subscribe(t, token, sender), http.StatusOK, "the sender's registration")
	topic := contracts.RoleSubject("PRIV", "legion-priv-priv-1-tester")
	for _, refused := range []struct{ source, sourceSession string }{
		{"dispatch", ""}, {"human", sender}, {"agent", ""},
	} {
		body := fmt.Sprintf(`{"topic":%q,"message":"for the role's holder","source":%q,"source_session":%q}`, topic, refused.source, refused.sourceSession)
		requireEdgeRefusal(t, rig.call(t, http.MethodPost, token, "/v1/messages/publish", body), http.StatusForbidden, "sender_not_session",
			fmt.Sprintf("a publish as source %q with source_session %q", refused.source, refused.sourceSession))
	}
	body := fmt.Sprintf(`{"topic":%q,"message":"for the role's holder","source":"agent","source_session":%q}`, topic, sender)
	if recorder := rig.call(t, http.MethodPost, token, "/v1/messages/publish", body); recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
		t.Fatalf("a publish as agent from %s: status %d (%s), want it taken", sender, recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
}

// The registration view of GET /v1/registry/<session_id> (view=registration) at the edge answers a
// session of the token's projects, and answers a session of another project, and one with no
// record, as a session no listener has heard of (unknownSession), naming no state of its own and
// no last registration. Control: with edge mode off, the other project's session is answered 200.
func TestAnEdgeRegistrationViewShowsOnlySessionsOfItsProjects(t *testing.T) {
	rig := newEdgeRig(t)
	const reader, other = "ses_edge_view_reader", "ses_edge_view_other"
	token := rig.personToken(reader, "PRIV")
	requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
	rig.placeElsewhere(t, other, "INFRA")
	requireStatus(t, rig.call(t, http.MethodGet, token, "/v1/registry/"+reader+"?view=registration", ""), http.StatusOK, "the reader's own registration view")
	for _, sessionID := range []string{other, "ses_edge_view_never"} {
		requireUnknownSession(t, rig.call(t, http.MethodGet, token, "/v1/registry/"+sessionID+"?view=registration", ""), "the registration view of "+sessionID)
	}
}

// GET /v1/registry/<session_id> at the edge names nothing of a session of another project that
// has no live record: one whose record is gone, which every other listener answers with its last
// registration (last_seen), and a stopped one the known-session store holds, which every other
// listener answers 200 with include=stopped (listenerwire.StoppedSessionInfo). The edge answers
// both as a session no listener has heard of. Control: with edge mode off, the gone session's 404
// carries its last registration and the stopped one is answered 200.
func TestAnEdgeRegistryReadNamesNothingOfAGoneOrStoppedSessionOfAnotherProject(t *testing.T) {
	rig := newEdgeRig(t)
	const reader, gone, stopped = "ses_edge_read_reader", "ses_edge_read_gone", "ses_edge_read_stopped"
	token := rig.personToken(reader, "PRIV")
	requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
	known, err := session.OpenKnownSessions(t.Context(), rig.harness.client.Conn)
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	rig.deps.known = known

	rig.placeElsewhere(t, gone, "INFRA")
	if err := rig.deps.sessions.Delete(t.Context(), gone); err != nil {
		t.Fatalf("delete %s's record: %v", gone, err)
	}
	if seen := rig.deps.sessions.LastSeen(gone); seen == 0 {
		t.Fatalf("UNKNOWN: the session registry keeps no last registration of %s, so no answer could name one", gone)
	}
	requireUnknownSession(t, rig.call(t, http.MethodGet, token, "/v1/registry/"+gone, ""), "GET /v1/registry/"+gone)

	if err := known.RecordWithin(t.Context(), session.KnownSession{SessionID: stopped, Title: "elsewhere", LastSeen: time.Now().UnixMilli()}); err != nil {
		t.Fatalf("record %s as a known session: %v", stopped, err)
	}
	if _, ok := known.Get(stopped); !ok {
		t.Fatalf("UNKNOWN: the known-session store does not answer %s, so no answer could name it stopped", stopped)
	}
	requireUnknownSession(t, rig.call(t, http.MethodGet, token, "/v1/registry/"+stopped+"?include="+sessionStateStopped, ""), "GET /v1/registry/"+stopped+"?include=stopped")
}

// A write of one Legion agent registry entry (PUT /v1/legion/agents/sessions/<session_id>) is
// refused at the edge 403 legion_registry_not_written before its body is read, as the batch write
// is. Control: with edge mode off, the body is read and refused 400 for what it lacks.
func TestAnEdgeWritesNoLegionAgentEntry(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_legion_writer"
	token := rig.personToken(sessionID, "PRIV")
	requireStatus(t, rig.subscribe(t, token, sessionID), http.StatusOK, "the registration")
	requireEdgeRefusal(t, rig.call(t, http.MethodPut, token, "/v1/legion/agents/sessions/"+sessionID, fmt.Sprintf(`{"session_id":%q}`, sessionID)),
		http.StatusForbidden, "legion_registry_not_written", "a write of "+sessionID+"'s Legion agent entry")
}

// A registration at the edge follows the session's own agent subject in the form that names a
// project under one of its token's projects or under every project (the wildcard
// contracts.CanonicalTopic writes), and refuses it 403 topic_not_in_project under another project,
// as it refuses another session's agent subject. Control: with edge mode off, each refused topic is
// taken.
func TestAnEdgeFollowsOnlyTheSessionsOwnAgentSubject(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID, other = "ses_edge_agent_subject", "ses_edge_agent_other"
	token := rig.personToken(sessionID, "PRIV")
	requireStatus(t, rig.subscribe(t, token, sessionID, contracts.AgentSubject("PRIV", sessionID), contracts.AgentSubject("*", sessionID)),
		http.StatusOK, "the registration of its own agent subject under PRIV and under every project")
	for _, refused := range []string{contracts.AgentSubject("INFRA", sessionID), contracts.AgentSubject("PRIV", other), contracts.LegacyAgentSubject(other)} {
		requireTopicRefusal(t, rig.subscribe(t, token, sessionID, refused), "topic_not_in_project", refused)
	}
}
