package main

// edge_mode_test.go holds the edge listener's request rules on a real bus: the admission, the
// topics a session follows and the stored topics a plan brings in, the roles and the claims a
// session brings, the sends and publishes, the reads, the start, and the record of the
// registering token's projects on each session. The GitHub rows are in
// edge_repositories_test.go, and the rig both files run on is in edge_rig_test.go.
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

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// The admission: an edge listener admits a broker token alone, and only one whose projects are
// all among the ones it serves, of project scope named or person; every other caller is refused
// before any handler runs. The exported admission (EdgeAdmits) is the very rule apiAuth asks.
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
		})
	}
	// The exported rule, as the relay socket calls it on each live answer.
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

// The role rule: an edge session claims and reads only a role of its token's projects, the
// project contracts.ClaimProject gives it, and its claim is keyed by contracts.RoleClaimKey.
func TestEdgeHoldsRolesToTheTokensProjects(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_roles"
	token := rig.personToken(sessionID, "PRIV")
	requireStatus(t, rig.subscribe(t, token, sessionID), http.StatusOK, "the registration")
	claim := func(role string) *httptest.ResponseRecorder {
		return rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, sessionID, role))
	}
	for _, role := range []string{"legion-infra-infra-1-tester", "INFRA.legion-infra-infra-1-tester", "lead-reviewer", "INFRA.lead-reviewer"} {
		requireEdgeRefusal(t, claim(role), http.StatusForbidden, "role_not_in_project", "a claim of "+role)
	}
	const own = "legion-priv-priv-1-tester"
	requireStatus(t, claim(own), http.StatusOK, "a claim of "+own)
	key := contracts.RoleClaimKey(mustClaimProject(t, own, ""), own)
	held, err := rig.deps.registry.RoleClaim(t.Context(), key)
	if err != nil || held.HolderSessionID != sessionID {
		t.Fatalf("the claim keyed %s holds %+v (%v), want %s", key, held, err, sessionID)
	}
	requireEdgeRefusal(t, rig.call(t, http.MethodGet, token, "/v1/roles/legion-infra-infra-1-tester", ""), http.StatusForbidden, "role_not_in_project", "a read of legion-infra-infra-1-tester")
	requireStatus(t, rig.call(t, http.MethodGet, token, "/v1/roles/"+own, ""), http.StatusOK, "a read of "+own)
}

func mustClaimProject(t *testing.T, role, requested string) string {
	t.Helper()
	project, err := contracts.ClaimProject(role, requested)
	if err != nil {
		t.Fatalf("ClaimProject(%s, %q): %v", role, requested, err)
	}
	return project
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

// On a listener in no edge mode, a free-form role claimed under a project is that
// project's own claim, keyed by contracts.RoleClaimKey (PRIV.lead-reviewer), beside the claim of
// the same name under no project (lead-reviewer), which keeps its bare key. Each is read, and
// reached by a publish and by the role lane, under its own project.
func TestAFreeFormRoleUnderAProjectIsClaimedReadAndReachedUnderThatProject(t *testing.T) {
	rig := newEdgeRigServing(t, nil)
	requireFreeFormRoleUnderItsProject(t, rig, "ses_ff_bare", "ses_ff_priv", "ses_ff_sender")
}

// At the edge, an edge session claims, reads and is reached through a free-form role of
// its token's project, and a role of the same name under no project, claimed elsewhere, keeps its
// own holder. The edge refuses the bare name, which works for no project, to claim and to publish.
func TestAnEdgeClaimsPublishesAndReadsAFreeFormRoleOfItsProject(t *testing.T) {
	rig := newEdgeRig(t)
	const bare = "ses_edge_ff_bare"
	rig.placeElsewhere(t, bare, "INFRA")
	if _, err := rig.deps.registry.SetRole(bare, "shared-listener", "lead-reviewer", false); err != nil {
		t.Fatalf("claim lead-reviewer for %s: %v", bare, err)
	}
	const holder, sender = "ses_edge_ff_holder", "ses_edge_ff_sender"
	token := rig.personToken(holder, "PRIV")
	senderToken := rig.namedToken(sender, "PRIV")
	standIn := startSessionStandIn(t, rig.url, holder, answerDone)
	requireStatus(t, rig.subscribe(t, token, holder), http.StatusOK, "the holder's registration")
	requireStatus(t, rig.subscribe(t, senderToken, sender), http.StatusOK, "the sender's registration")
	requireEdgeRefusal(t, rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":"lead-reviewer","soft":true}`, holder)),
		http.StatusForbidden, "role_not_in_project", "a claim of lead-reviewer")
	body := fmt.Sprintf(`{"topic":"notifications.role.lead-reviewer","message":"for no project's reviewer","source":"agent","source_session":%q}`, sender)
	requireEdgeRefusal(t, rig.call(t, http.MethodPost, senderToken, "/v1/messages/publish", body), http.StatusForbidden, "topic_not_published", "a publish to lead-reviewer")

	role := "PRIV.lead-reviewer"
	requireStatus(t, rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, holder, role)), http.StatusOK, "a claim of "+role)
	requireRoleHolder(t, rig, senderToken, role, holder)
	requireRoleKeys(t, rig, map[string]string{"PRIV.lead-reviewer": holder, "lead-reviewer": bare})
	published := publishToHeldRole(t, rig, senderToken, sender, contracts.RoleTopicPrefix+role)
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), published)
}

// On every listener, a role message on one project's lane that names a Legion role of another
// project reaches no holder: the claim of a Legion role is keyed on its bare name, so without the
// rule a message on notifications.role.PRIV.legion-infra-reviewer would be forwarded to the holder
// of INFRA's legion-infra-reviewer. The role lane drops it before it resolves a holder, and the
// publish route refuses it 400, as POST /v1/roles/set refuses the claim of that role under PRIV.
// Control: a message on the role's own lane, before and after, reaches its holder.
func TestARoleLaneOfAnotherProjectReachesNoHolderOfItsLegionRole(t *testing.T) {
	rig := newEdgeRigServing(t, nil)
	const holder, sender, role = "ses_lane_infra_holder", "ses_lane_sender", "legion-infra-reviewer"
	holderToken, senderToken := rig.personToken(holder, "INFRA"), rig.personToken(sender, "INFRA")
	standIn := startSessionStandIn(t, rig.url, holder, answerDone)
	requireStatus(t, rig.subscribe(t, holderToken, holder), http.StatusOK, "the holder's registration")
	requireStatus(t, rig.subscribe(t, senderToken, sender), http.StatusOK, "the sender's registration")
	requireStatus(t, rig.call(t, http.MethodPost, holderToken, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q}`, holder, role)),
		http.StatusOK, "the claim of "+role)
	cfg := wireTestRoleForwarder(t, rig.harness.config)
	lane := func(project, key string) contracts.Envelope {
		t.Helper()
		item := listenerTestEnvelope(contracts.RoleSubject(project, role), key+"-"+id.New())
		coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Subject: item.Topic, Data: marshalListenerEnvelope(t, item)})
		return item
	}

	own := lane("INFRA", "lane-own")
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), own)

	other := lane("PRIV", "lane-other")
	// The lane records its forward before it hands it on, in the call that took the message, so
	// a forward to the holder is seen here as soon as that call returns.
	if cfg.attemptCache.Seen(other.DedupeKey, holder) || cfg.dedupeCache.Seen(other.DedupeKey, holder) {
		t.Fatalf("the role lane forwarded the message on %s to %s, the holder of INFRA's %s", other.Topic, holder, role)
	}
	after := lane("INFRA", "lane-after")
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID == other.EventID {
		t.Fatalf("the message on %s reached %s, the holder of INFRA's %s", other.Topic, holder, role)
	}
	requireFrames(t, frames, after)

	body := fmt.Sprintf(`{"topic":%q,"message":"for INFRA's reviewer, under PRIV","source":"agent","source_session":%q}`, contracts.RoleSubject("PRIV", role), sender)
	recorder := rig.call(t, http.MethodPost, senderToken, "/v1/messages/publish", body)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "works for project INFRA") {
		t.Fatalf("a publish to %s: status %d (%s), want 400 naming INFRA, the project the role works for",
			contracts.RoleSubject("PRIV", role), recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
}

// requireFreeFormRoleUnderItsProject is the free-form role row on rig: bare claims lead-reviewer,
// priv claims PRIV.lead-reviewer, both soft, and neither takes the other's role; a read, a publish
// through the route and a message on the role lane each reach the holder of their own name's
// claim.
func requireFreeFormRoleUnderItsProject(t *testing.T, rig *edgeRig, bare, priv, sender string) {
	t.Helper()
	tokens := map[string]string{}
	standIns := map[string]*sessionStandIn{}
	for _, sessionID := range []string{bare, priv, sender} {
		tokens[sessionID] = rig.personToken(sessionID, "PRIV")
		standIns[sessionID] = startSessionStandIn(t, rig.url, sessionID, answerDone)
		requireStatus(t, rig.subscribe(t, tokens[sessionID], sessionID), http.StatusOK, "the registration of "+sessionID)
	}
	for sessionID, role := range map[string]string{bare: "lead-reviewer", priv: "PRIV.lead-reviewer"} {
		recorder := rig.call(t, http.MethodPost, tokens[sessionID], "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, sessionID, role))
		requireStatus(t, recorder, http.StatusOK, sessionID+"'s soft claim of "+role)
	}
	requireRoleKeys(t, rig, map[string]string{"PRIV.lead-reviewer": priv, "lead-reviewer": bare})
	requireRoleHolder(t, rig, tokens[sender], "PRIV.lead-reviewer", priv)
	requireRoleHolder(t, rig, tokens[sender], "lead-reviewer", bare)

	toPriv := publishToHeldRole(t, rig, tokens[sender], sender, contracts.RoleSubject("PRIV", "lead-reviewer"))
	requireFrames(t, standIns[priv].collect(t, 1, 10*time.Second), toPriv)
	toBare := publishToHeldRole(t, rig, tokens[sender], sender, contracts.RoleTopicPrefix+"lead-reviewer")
	requireFrames(t, standIns[bare].collect(t, 1, 10*time.Second), toBare)

	// The role lane, which takes every role message published on the bus itself (Dispatch's
	// among them), resolves the claim the same way.
	cfg := wireTestRoleForwarder(t, rig.harness.config)
	item := listenerTestEnvelope(contracts.RoleSubject("PRIV", "lead-reviewer"), "free-form-lane-"+priv)
	coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Subject: item.Topic, Data: marshalListenerEnvelope(t, item)})
	frames := standIns[priv].collect(t, 1, 10*time.Second)
	if frames[0].item.EventID != item.EventID {
		t.Fatalf("the role lane's message reached %s as %s, want %s", priv, frames[0].item.EventID, item.EventID)
	}
	standIns[bare].none(t, time.Second)
}

// requireRoleKeys fails the test unless each claim key of want is held by its session.
func requireRoleKeys(t *testing.T, rig *edgeRig, want map[string]string) {
	t.Helper()
	for key, holder := range want {
		claim, err := rig.deps.registry.RoleClaim(t.Context(), key)
		if err != nil || claim.HolderSessionID != holder {
			t.Errorf("the claim keyed %s names %q (%v), want %s", key, claim.HolderSessionID, err, holder)
		}
	}
}

// requireRoleHolder fails the test unless GET /v1/roles/<role> answers holder.
func requireRoleHolder(t *testing.T, rig *edgeRig, token, role, holder string) {
	t.Helper()
	recorder := rig.call(t, http.MethodGet, token, "/v1/roles/"+role, "")
	requireStatus(t, recorder, http.StatusOK, "GET /v1/roles/"+role)
	var answer listenerwire.RoleHolder
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || answer.Holder != holder {
		t.Fatalf("GET /v1/roles/%s answers %s (%v), want holder %s", role, recorder.Body.String(), err, holder)
	}
}

// publishToHeldRole publishes one message to topic, a role's lane, as sender and requires the
// route to answer that the holder took it; it returns the envelope the answer names.
func publishToHeldRole(t *testing.T, rig *edgeRig, token, sender, topic string) contracts.Envelope {
	t.Helper()
	body := fmt.Sprintf(`{"topic":%q,"message":"for the role's holder","source":"agent","source_session":%q}`, topic, sender)
	recorder := rig.call(t, http.MethodPost, token, "/v1/messages/publish", body)
	requireStatus(t, recorder, http.StatusOK, "a publish to "+topic)
	var answer listenerwire.PublishResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the publish answer %s: %v", recorder.Body.String(), err)
	}
	return answer.Envelope
}

// The listener links no package of Dispatch's store and no PostgreSQL driver: the bus's rule for
// a broker token's projects, which the edge reads, is internal/projectscope's, a package that
// imports neither. A listing that names no package, the listener among them, has read nothing and
// fails as UNKNOWN.
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
	var registered listenerwire.SubscribeResponse
	if err := json.Unmarshal(answer.Body.Bytes(), &registered); err != nil {
		t.Fatalf("decode the registration's answer %s: %v", answer.Body.String(), err)
	}
	for _, dropped := range []string{slack, infra, infraRepo} {
		if !slices.ContainsFunc(registered.Warnings, func(warning string) bool { return strings.HasPrefix(warning, dropped+" was removed") }) {
			t.Errorf("the registration's warnings %q name no removal of the stored topic %s", registered.Warnings, dropped)
		}
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
	var registered listenerwire.SubscribeResponse
	if err := json.Unmarshal(answer.Body.Bytes(), &registered); err != nil {
		t.Fatalf("decode the registration's answer %s: %v", answer.Body.String(), err)
	}
	for _, dropped := range []string{slack, infra} {
		if !slices.ContainsFunc(registered.Warnings, func(warning string) bool { return strings.HasPrefix(warning, dropped+" was removed") }) {
			t.Errorf("the registration's warnings %q name no removal of %s", registered.Warnings, dropped)
		}
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

// requireTopicsHidden fails the test unless GET /v1/sessions and GET /v1/registry/<sessionID>,
// each asked with token, show sessionID and none of hidden among its topics; it returns the
// topics the registry read shows.
func requireTopicsHidden(t *testing.T, rig *edgeRig, token, sessionID string, hidden ...string) []string {
	t.Helper()
	listed := rig.call(t, http.MethodGet, token, "/v1/sessions", "")
	requireStatus(t, listed, http.StatusOK, "GET /v1/sessions")
	var rows []listenerwire.SessionInfo
	if err := json.Unmarshal(listed.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode %s: %v", listed.Body.String(), err)
	}
	found := false
	for _, row := range rows {
		if row.SessionID != sessionID {
			continue
		}
		found = true
		for _, topic := range row.Topics {
			if slices.Contains(hidden, topic) {
				t.Errorf("GET /v1/sessions shows %s following %s, a topic of no project of the reader's", sessionID, topic)
			}
		}
	}
	if !found {
		t.Errorf("GET /v1/sessions lists no %s: %s", sessionID, listed.Body.String())
	}
	one := rig.call(t, http.MethodGet, token, "/v1/registry/"+sessionID, "")
	requireStatus(t, one, http.StatusOK, "GET /v1/registry/"+sessionID)
	var info listenerwire.SessionInfo
	if err := json.Unmarshal(one.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode %s: %v", one.Body.String(), err)
	}
	for _, topic := range info.Topics {
		if slices.Contains(hidden, topic) {
			t.Errorf("GET /v1/registry/%s shows the topic %s, of no project of the reader's", sessionID, topic)
		}
	}
	return info.Topics
}

// A role read at the edge names no holder whose registering token listed none of the reader's
// projects: a PRIV role held by a session another listener registered with no broker token is
// answered as a role no claim holds, the 404 unclaimed with no holder, place or claim revision. A
// holder of the reader's project is answered whole. TestAnEdgeRoleAnswerNamesNoForeignHolder
// holds the same of a lapsed holder and of a role send's answers.
func TestAnEdgeRoleReadNamesNoPlaceOfAHolderOutsideItsProjects(t *testing.T) {
	rig := newEdgeRig(t)
	const reader, internal, peer = "ses_edge_role_reader", "ses_edge_role_internal", "ses_edge_role_peer"
	const internalRole, peerRole = "legion-priv-priv-4-tester", "legion-priv-priv-5-tester"
	token := rig.personToken(reader, "PRIV")
	requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
	requireStatus(t, rig.subscribe(t, rig.namedToken(peer, "PRIV"), peer), http.StatusOK, "the peer's registration")
	if err := rig.deps.sessions.Put(internal, session.SessionEntry{MachineID: "internal-machine", Host: "internal-host", SelfSubscribed: true,
		Title: "an internal agent", Dir: "/home/agent/internal-checkout", Capabilities: []string{"tmux"}, Projects: []string{}}); err != nil {
		t.Fatalf("register %s elsewhere: %v", internal, err)
	}
	if _, err := rig.deps.registry.SetRoleWithPrevious(t.Context(), store.RoleClaimRequest{
		SessionID: internal, MachineID: "test-machine", ClientInstanceID: "internal-process", Role: internalRole,
	}); err != nil {
		t.Fatalf("claim %s for %s: %v", internalRole, internal, err)
	}
	if _, err := rig.deps.registry.SetRole(peer, "test-machine", peerRole, false); err != nil {
		t.Fatalf("claim %s for %s: %v", peerRole, peer, err)
	}
	internalRead := rig.call(t, http.MethodGet, token, "/v1/roles/"+internalRole, "")
	var unheld listenerwire.RoleRefusal
	_ = json.Unmarshal(internalRead.Body.Bytes(), &unheld)
	if internalRead.Code != http.StatusNotFound || unheld.Reason != roles.Unclaimed || unheld.Holder != "" || unheld.ClaimRevision != 0 ||
		strings.Contains(internalRead.Body.String(), internal) {
		t.Errorf("GET /v1/roles/%s: status %d %s, want the 404 of a role no claim holds, naming no holder", internalRole, internalRead.Code, strings.TrimSpace(internalRead.Body.String()))
	}
	read := func(role string) listenerwire.RoleHolder {
		t.Helper()
		recorder := rig.call(t, http.MethodGet, token, "/v1/roles/"+role, "")
		requireStatus(t, recorder, http.StatusOK, "GET /v1/roles/"+role)
		var answer listenerwire.RoleHolder
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode %s: %v", recorder.Body.String(), err)
		}
		return answer
	}
	if whole := read(peerRole); whole.Holder != peer || whole.MachineID == "" || whole.Host == "" {
		t.Errorf("GET /v1/roles/%s answers %+v, want holder %s with its machine and host: it is a session of PRIV", peerRole, whole, peer)
	}
}

// A session that holds a role of another project when it registers at the edge, which another
// listener claimed for it, loses that role at the registration: the claim is released rather than
// renewed, and the answer's warnings name it, as a claim of that role at the edge is refused. A
// message on that role's lane after the registration then reaches no holder, and the session's
// role of the token's project stays and is renewed. A role message the lane forwarded to the
// session's agent subject before the registration, or after a later claim another writer makes,
// is not this row's: the relay's check of each frame's topic holds those frames. Controls: with
// edge mode off, and with the edge's hold of the renewal off, the other project's claim is renewed
// and the row fails.
func TestAnEdgeReleasesTheRoleClaimsOfOtherProjects(t *testing.T) {
	rig := newEdgeRig(t)
	const moved, privIssues = "ses_edge_role_moved", "notifications.dispatch.issue.PRIV.>"
	const infraRole, privRole = "legion-infra-infra-9-reviewer", "legion-priv-priv-9-tester"
	rig.placeElsewhere(t, moved)
	for _, role := range []string{infraRole, privRole} {
		if _, err := rig.deps.registry.SetRole(moved, "shared-listener", role, false); err != nil {
			t.Fatalf("claim %s for %s on the other listener: %v", role, moved, err)
		}
	}
	testwait.Eventually(t, "the role claim copy to name both claims", func() bool {
		held, err := rig.deps.registry.RolesHeldBy(moved)
		return err == nil && len(held) == 2
	})
	ctx := t.Context()
	before, err := rig.deps.registry.RoleClaim(ctx, privRole)
	if err != nil || before.HolderSessionID != moved {
		t.Fatalf("UNKNOWN: %s's claim before the registration reads %+v (%v), not %s's", privRole, before, err, moved)
	}

	standIn := startSessionStandIn(t, rig.url, moved, answerDone)
	answer := rig.subscribe(t, rig.personToken(moved, "PRIV"), moved, privIssues)
	requireStatus(t, answer, http.StatusOK, "the moved session's registration at the edge")
	var registered listenerwire.SubscribeResponse
	if err := json.Unmarshal(answer.Body.Bytes(), &registered); err != nil {
		t.Fatalf("decode the registration's answer %s: %v", answer.Body.String(), err)
	}
	if !slices.ContainsFunc(registered.Warnings, func(warning string) bool { return strings.HasPrefix(warning, "role "+infraRole+" was released") }) {
		t.Errorf("the registration's warnings %q name no release of %s", registered.Warnings, infraRole)
	}
	if slices.ContainsFunc(registered.Warnings, func(warning string) bool { return strings.Contains(warning, privRole) }) {
		t.Errorf("the registration's warnings %q name %s, a role of PRIV", registered.Warnings, privRole)
	}
	if infra, err := rig.deps.registry.RoleClaim(ctx, infraRole); err != nil || (infra.Revision != 0 && infra.HolderSessionID == moved) {
		t.Fatalf("after the registration at the edge %s's claim reads %+v (%v): %s still holds a role of INFRA", infraRole, infra, err, moved)
	}
	if priv, err := rig.deps.registry.RoleClaim(ctx, privRole); err != nil || priv.HolderSessionID != moved || priv.Revision <= before.Revision {
		t.Fatalf("after the registration at the edge %s's claim reads %+v (%v), want %s's, renewed past revision %d", privRole, priv, err, moved, before.Revision)
	}

	// A message on the INFRA role's lane reaches no holder now: the lane records a forward before
	// it hands it on, in the call that took the message, and the PRIV event stored after it is
	// the session's first frame, since the relay sends in stream order.
	cfg := wireTestRoleForwarder(t, rig.harness.config)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+infraRole, "edge-infra-role-"+id.New())
	coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Subject: item.Topic, Data: marshalListenerEnvelope(t, item)})
	if cfg.attemptCache.Seen(item.DedupeKey, moved) || cfg.dedupeCache.Seen(item.DedupeKey, moved) {
		t.Fatalf("the role lane forwarded INFRA's %s message to %s, a session whose token lists only PRIV", infraRole, moved)
	}
	sentinel, _ := rig.storeEvent(t, "notifications.dispatch.issue.PRIV.PRIV-1.comment.created")
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID != sentinel.EventID {
		t.Fatalf("the moved session's first frame is %s on %s, want PRIV's event: a message of INFRA's role reached it", frames[0].item.EventID, frames[0].item.Topic)
	}
}

// A registration at the edge releases a claim of another project's role also while the role claim
// copy is refilling and names no role (store.Registry.RolesHeldBy refuses): the role's lane topic,
// which the hold took out of the session's row, names the role then. Control: with the lane topics
// the hold dropped left out of the renewal's weighing, the claim is neither released nor renewed,
// and the row fails.
func TestAnEdgeReleasesAnotherProjectsClaimWhileTheClaimViewRefills(t *testing.T) {
	rig := newEdgeRig(t)
	const moved, infraRole = "ses_edge_role_refill", "legion-infra-infra-7-reviewer"
	rig.placeElsewhere(t, moved)
	if _, err := rig.deps.registry.SetRole(moved, "shared-listener", infraRole, false); err != nil {
		t.Fatalf("claim %s for %s on the other listener: %v", infraRole, moved, err)
	}
	testwait.Eventually(t, "the role claim copy to name the claim", func() bool {
		held, err := rig.deps.registry.RolesHeldBy(moved)
		return err == nil && len(held) == 1
	})
	refilled := refuseRoleClaimView(t, rig.deps.registry)
	defer refilled()
	if _, err := rig.deps.registry.RolesHeldBy(moved); err == nil {
		t.Fatal("UNKNOWN: the role claim copy still answers, so the registration would read the claim from it")
	}

	answer := rig.subscribe(t, rig.personToken(moved, "PRIV"), moved)
	requireStatus(t, answer, http.StatusOK, "the moved session's registration at the edge while the claim copy refills")
	var registered listenerwire.SubscribeResponse
	if err := json.Unmarshal(answer.Body.Bytes(), &registered); err != nil {
		t.Fatalf("decode the registration's answer %s: %v", answer.Body.String(), err)
	}
	if !slices.ContainsFunc(registered.Warnings, func(warning string) bool { return strings.HasPrefix(warning, "role "+infraRole+" was released") }) {
		t.Errorf("the registration's warnings %q name no release of %s", registered.Warnings, infraRole)
	}
	if claim, err := rig.deps.registry.RoleClaim(t.Context(), infraRole); err != nil || (claim.Revision != 0 && claim.HolderSessionID == moved) {
		t.Fatalf("after the registration at the edge %s's claim reads %+v (%v): %s still holds a role of INFRA", infraRole, claim, err, moved)
	}
}

// A claim of another project's role that the edge cannot release refuses the registration with
// the retryable 503, and nothing of the registration is stored: the claim stands, and nothing else
// frees it while the session lives. Here the claim's record cannot be read, so the release cannot
// tell that it is not the session's. Control: with edge mode off the registration is taken.
func TestAnEdgeRegistrationWhoseRoleReleaseFailsIsRefused(t *testing.T) {
	rig := newEdgeRig(t)
	const moved, infraRole = "ses_edge_release_fails", "legion-infra-infra-6-reviewer"
	rig.placeElsewhereFollowing(t, moved, []string{}, contracts.RoleTopicPrefix+infraRole)
	// JSON that is not a claim is a record no read can decode (store.ErrMalformedRoleClaim); a
	// string that is not JSON would read as an earlier build's bare holder id.
	if _, err := registryRoleKV(t, rig.deps.registry).Put(infraRole, []byte(`{"holder_session_id":7}`)); err != nil {
		t.Fatalf("store an unreadable claim of %s: %v", infraRole, err)
	}
	before := storedEdgeSessionEntry(t, rig, moved)

	answer := rig.subscribe(t, rig.personToken(moved, "PRIV"), moved)
	if answer.Code != http.StatusServiceUnavailable || answer.Header().Get("Retry-After") == "" {
		t.Fatalf("the registration whose release of %s fails: status %d, Retry-After %q (%s), want the retryable 503",
			infraRole, answer.Code, answer.Header().Get("Retry-After"), strings.TrimSpace(answer.Body.String()))
	}
	if after := storedEdgeSessionEntry(t, rig, moved); after.MachineID != before.MachineID || after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("the refused registration wrote %s's entry: %+v, want it left at %+v", moved, after, before)
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
