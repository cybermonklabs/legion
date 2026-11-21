package main

// edge_mode_test.go holds the edge listener's rules on a real bus: the admission, the topics a
// session follows with the GitHub rows, the stored topics a plan brings in, the roles, the sends,
// the reads, the start, and the record of the registering token's projects on each session.
//
// Every row reads the projects its listener serves from ENVOY_EDGE_PROJECTS when the test's own
// environment sets it, and serves PRIV otherwise (edgeTestProjects). So the control every row is
// run under once is the same command with ENVOY_EDGE_PROJECTS set empty: the listener is in no
// edge mode, answers as every listener does today, and each row fails naming the call it expected
// refused.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// edgeTestProjects is the projects the rows' edge listener serves: PRIV, unless the test's own
// environment sets ENVOY_EDGE_PROJECTS, read then as the listener reads it. Set empty, it is the
// edge-off control: a listener that serves no project is in no edge mode.
func edgeTestProjects(t *testing.T) []string {
	t.Helper()
	raw, set := os.LookupEnv("ENVOY_EDGE_PROJECTS")
	if !set {
		return []string{"PRIV"}
	}
	projects, err := oauthwire.ParseProjects(strings.ReplaceAll(raw, ",", " "))
	if err != nil {
		t.Fatalf("ENVOY_EDGE_PROJECTS=%q: %v", raw, err)
	}
	t.Logf("ENVOY_EDGE_PROJECTS=%q: the listener serves %v (edge mode %t)", raw, projects, len(projects) > 0)
	return projects
}

// The repository answer every GitHub row starts from, as the broker's route relays Dispatch's:
// PRIV-1 names acme/pub, PRIV-3 names none and is a child of PRIV-2, which names acme/two, so
// PRIV's issues resolve to acme/pub and acme/two; INFRA-1 names acme/priv, which is INFRA's and
// no repository of PRIV's. Which issue resolves where is Dispatch's one rule; the stand-in
// answers its result.
func edgeRepositoryFixture() map[string][]string {
	return map[string][]string{"PRIV": {"acme/pub", "acme/two"}, "INFRA": {"acme/priv"}}
}

// edgeAccountToken is the stand-in broker account the rig's repository reader asks with.
const edgeAccountToken = "listener-account-token"

// repositoryStandIn is the broker's GET /v1/project-repositories as the edge listener reads it:
// the answer of each project it serves, 403 PROJECT_NOT_LISTED for any other, and the status the
// test sets in place of every answer while it sets one.
type repositoryStandIn struct {
	server  *httptest.Server
	serves  []string
	mu      sync.Mutex
	answers map[string][]string
	status  int
	calls   atomic.Int64
	bearers []string
}

func newRepositoryStandIn(t *testing.T, serves []string, answers map[string][]string) *repositoryStandIn {
	t.Helper()
	stand := &repositoryStandIn{serves: serves, answers: answers}
	stand.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != edgeRepositoriesPath {
			http.NotFound(w, r)
			return
		}
		stand.calls.Add(1)
		stand.mu.Lock()
		stand.bearers = append(stand.bearers, r.Header.Get("Authorization"))
		status, answers := stand.status, stand.answers
		stand.mu.Unlock()
		project := r.URL.Query().Get("project")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case status != 0:
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"code":"REPOSITORIES_UNKNOWN","error":"the stand-in answers %d"}`, status)
		case !slices.Contains(stand.serves, project):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"PROJECT_NOT_LISTED","error":"the caller's edge-listener entry does not list this project"}`))
		default:
			repositories := answers[project]
			if repositories == nil {
				repositories = []string{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"project": project, "repositories": repositories})
		}
	}))
	t.Cleanup(stand.server.Close)
	return stand
}

// answer makes project's answer repositories from the next call on.
func (s *repositoryStandIn) answer(project string, repositories ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	answers := make(map[string][]string, len(s.answers))
	for key, value := range s.answers {
		answers[key] = value
	}
	answers[project] = repositories
	s.answers = answers
}

// failWith makes every later call answer status, 0 for the answers again.
func (s *repositoryStandIn) failWith(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func (s *repositoryStandIn) lastBearer() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bearers) == 0 {
		return ""
	}
	return s.bearers[len(s.bearers)-1]
}

// edgeClock is the clock the rows' repository reader dates its answers by: the real one, moved
// on by what a row adds, so a row ages an answer past its life without waiting it out.
type edgeClock struct{ ahead atomic.Int64 }

func (c *edgeClock) now() time.Time { return time.Now().Add(time.Duration(c.ahead.Load())) }

func (c *edgeClock) advance(d time.Duration) { c.ahead.Add(int64(d)) }

// edgeRig is a listener's /v1 as an edge deployment runs it, every piece real but the broker: the
// routes and session relays over the in-process server (relayRig), the broker-token credential
// behind apiAuth with this listener's edge projects, the broker's introspection stub, and the
// stand-in of its repository route, read through the listener's own reader. Its refresh loop runs
// on ticks the row sends (tick), so a row orders a refresh against its own publishes.
type edgeRig struct {
	*relayRig
	projects     []string
	handler      http.Handler
	broker       *ciphertokentest.StubBroker
	caller       *ciphertokentest.Caller
	repositories *repositoryStandIn
	reader       *edgeRepositories
	clock        *edgeClock
	met          *metrics.Registry
	agents       *legionagents.Store
	ticks        chan time.Time
}

func newEdgeRig(t *testing.T) *edgeRig {
	t.Helper()
	return newEdgeRigServing(t, edgeTestProjects(t))
}

// newEdgeRigServing is the rig of a listener serving projects, whatever the test's environment
// says: none is a listener in no edge mode, which the rows that hold every listener use.
func newEdgeRigServing(t *testing.T, projects []string) *edgeRig {
	t.Helper()
	rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	// The publish route forwards a role message to its holder through the delivery config main
	// wires and records each confirmation, so the rows that publish to a held role need both.
	withPublishRoute(t, rig.deps)
	met := metrics.New()
	stand := newRepositoryStandIn(t, []string{"PRIV"}, edgeRepositoryFixture())
	clock := &edgeClock{}
	logger := rig.harness.config.logger
	reader := newEdgeRepositories(stand.server.URL, listenerBrokerAccount(edgeAccountToken), projects, newEdgeWithheldCounter(met), logger)
	reader.now = clock.now
	agents, err := legionagents.Open(t.Context(), rig.harness.client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	rig.deps.legionAgents = newLegionAgentRegistry(agents)
	edge := &edgeRig{relayRig: rig, projects: projects, met: met, repositories: stand, reader: reader, clock: clock,
		agents: agents, ticks: make(chan time.Time)}
	if len(projects) > 0 {
		rig.deps.repositories = reader
		rig.relays.repositories = reader
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			runEdgeRepositories(ctx, rig.deps, "test-machine", edge.ticks)
		}()
		t.Cleanup(func() {
			cancel()
			<-done
		})
	}
	rig.relays.setOwned(true)
	edge.broker = ciphertokentest.NewStubBroker(t)
	edge.caller = ciphertokentest.NewCaller(t)
	edge.handler = apiAuth(listenerAPIAuth{brokers: listenerBrokerTokens(t, edge.broker), edgeProjects: projects, logger: logger}, rig.mux)
	return edge
}

// edgeOn reports whether the rig's listener is in edge mode: false under the control.
func (r *edgeRig) edgeOn() bool { return len(r.projects) > 0 }

// tick runs one pass of the listener's repository refresh and waits for it to finish.
func (r *edgeRig) tick(t *testing.T) {
	t.Helper()
	if !r.edgeOn() {
		return
	}
	before := r.reader.passes.Load()
	select {
	case r.ticks <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("the repository refresh loop took no tick within 10 s")
	}
	testwait.Eventually(t, "the repository refresh pass to finish", func() bool { return r.reader.passes.Load() > before })
}

// personToken is a token the broker answers as session's, acting for a person, of project scope
// person and listing projects: the token an agent's own trade makes.
func (r *edgeRig) personToken(session string, projects ...string) string {
	token := "access-token-of-" + session
	answer := sessionAnswer(session, "outside@example.com", r.caller.JKT())
	answer.Projects = slices.Sorted(slices.Values(projects))
	r.broker.Says(token, answer)
	return token
}

// namedToken is a named project's token for session: an operator's entry's, its one project.
func (r *edgeRig) namedToken(session, project string) string {
	token := "named-token-of-" + session
	answer := sessionAnswer(session, "outside@example.com", r.caller.JKT())
	answer.ProjectScope = ciphertoken.ProjectScopeNamed
	answer.Projects = []string{project}
	r.broker.Says(token, answer)
	return token
}

// call sends one request through the whole stack: the credential, then the route.
func (r *edgeRig) call(t *testing.T, method, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	r.handler.ServeHTTP(recorder, brokerTokenRequest(t, r.caller, method, path, token, body))
	return recorder
}

// subscribe registers session under token for consumer delivery, following topics.
func (r *edgeRig) subscribe(t *testing.T, token, session string, topics ...string) *httptest.ResponseRecorder {
	t.Helper()
	return r.call(t, http.MethodPost, token, "/v1/interests/subscribe", consumerRegistration(session, "epoch-1", topics...))
}

// placeElsewhere writes session's entry as another listener's registration under a token listing
// projects would, and its interest row, so the edge's reads find a session it did not register.
func (r *edgeRig) placeElsewhere(t *testing.T, sessionID string, projects ...string) {
	t.Helper()
	if projects == nil {
		projects = []string{}
	}
	if err := r.deps.sessions.Put(sessionID, session.SessionEntry{MachineID: "shared-listener", SelfSubscribed: true, Title: sessionID, Projects: projects}); err != nil {
		t.Fatalf("register %s elsewhere: %v", sessionID, err)
	}
	if _, err := r.deps.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "shared-listener"}, []string{contracts.LegacyAgentSubject(sessionID)}); err != nil {
		t.Fatalf("write %s's interest row: %v", sessionID, err)
	}
}

// placeElsewhereFollowing is placeElsewhere of a session whose interest row another listener
// stored with topics beside its agent subject.
func (r *edgeRig) placeElsewhereFollowing(t *testing.T, sessionID string, projects []string, topics ...string) {
	t.Helper()
	r.placeElsewhere(t, sessionID, projects...)
	if _, err := r.deps.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "shared-listener"}, topics); err != nil {
		t.Fatalf("write %s's interest row with %v: %v", sessionID, topics, err)
	}
}

// storeGithubEvent stores a GitHub event of repository, owner/name as GitHub writes it, on the
// subject the intake publishes it on with kind after the repository, its payload naming the
// repository as every GitHub event's payload does (contracts' githubPayload), and returns it.
func (r *edgeRig) storeGithubEvent(t *testing.T, repository, kind string) contracts.Envelope {
	t.Helper()
	owner, name, _ := strings.Cut(repository, "/")
	subject := contracts.GithubRepositoryPrefix(owner, name) + "." + kind
	item := listenerTestEnvelope(subject, "edge-github-"+id.New())
	item.Source = "github"
	item.Payload = fmt.Sprintf(`{"kind":"pr","action":"opened","repo":%q}`, repository)
	if _, err := r.harness.client.JS().Publish(subject, marshalListenerEnvelope(t, item)); err != nil {
		t.Fatalf("store a GitHub event of %s on %s: %v", repository, subject, err)
	}
	return item
}

// withheld is envoy_edge_github_withheld_total{reason=...} as the listener's /metrics renders it,
// -1 for no series.
func (r *edgeRig) withheld(t *testing.T, reason string) int64 {
	t.Helper()
	recorder := httptest.NewRecorder()
	r.met.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return counterValue(t, recorder.Body.String(), `envoy_edge_github_withheld_total{reason="`+reason+`"}`)
}

// edgeAnswer is what a row reads of an edge refusal (listenerwire.EdgeRefusal).
type edgeAnswer struct {
	Reason  string   `json:"reason"`
	Error   string   `json:"error"`
	Refused []string `json:"refused"`
}

// requireEdgeRefusal fails the test unless recorder is status with reason, naming what was asked,
// and returns the refusal.
func requireEdgeRefusal(t *testing.T, recorder *httptest.ResponseRecorder, status int, reason, asked string) edgeAnswer {
	t.Helper()
	var answer edgeAnswer
	_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
	if recorder.Code != status || answer.Reason != reason {
		t.Errorf("%s: status %d reason %q (%s), want %d %s", asked, recorder.Code, answer.Reason, strings.TrimSpace(recorder.Body.String()), status, reason)
	}
	return answer
}

// requireTopicRefusal is requireEdgeRefusal of a registration, whose refusal names exactly the
// refused topics.
func requireTopicRefusal(t *testing.T, recorder *httptest.ResponseRecorder, reason string, refused ...string) {
	t.Helper()
	answer := requireEdgeRefusal(t, recorder, http.StatusForbidden, reason, fmt.Sprintf("a registration refused for %v", refused))
	if recorder.Code == http.StatusForbidden && !slices.Equal(answer.Refused, refused) {
		t.Errorf("the refusal names refused %v (%s), want exactly %v", answer.Refused, strings.TrimSpace(recorder.Body.String()), refused)
	}
}

// requireStatus fails the test unless recorder is status, naming what was asked.
func requireStatus(t *testing.T, recorder *httptest.ResponseRecorder, status int, asked string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("%s: status %d (%s), want %d", asked, recorder.Code, strings.TrimSpace(recorder.Body.String()), status)
	}
}

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

// An edge session may follow the GitHub events of each repository its project's issues
// resolve to, read fresh through the broker at the registration with the listener's own account,
// and an event of each reaches it.
func TestAnEdgeRelaysTheGitHubEventsOfItsProjectsRepositories(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_github"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	before := rig.repositories.calls.Load()
	topics := []string{"notifications.github.acme.pub.>", "notifications.github.acme.two.>"}
	answer := rig.subscribe(t, token, sessionID, topics...)
	requireStatus(t, answer, http.StatusOK, fmt.Sprintf("a registration following %v", topics))
	if rig.repositories.calls.Load() == before {
		t.Fatalf("the registration naming %v read no repository answer; the answer is read fresh at each one", topics)
	}
	if got := rig.repositories.lastBearer(); got != "Bearer "+edgeAccountToken {
		t.Fatalf("the repository read presented %q, want the listener's own account", got)
	}
	pub := rig.storeGithubEvent(t, "acme/pub", "pr.1")
	two := rig.storeGithubEvent(t, "acme/two", "issue.2")
	requireFrames(t, standIn.collect(t, 2, 10*time.Second), pub, two)
}

// A GitHub interest in a repository no issue of the project resolves to, or in no single
// repository, is refused 403 repository_not_in_project, and such a repository's event never
// reaches the session.
func TestAnEdgeRefusesAnotherRepositorysEvents(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_other_repository"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	for _, topic := range []string{"notifications.github.acme.priv.>", "notifications.github.acme.>", "notifications.github.>"} {
		requireTopicRefusal(t, rig.subscribe(t, token, sessionID, topic), "repository_not_in_project", topic)
	}
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>"), http.StatusOK, "an interest in acme/pub")
	rig.storeGithubEvent(t, "acme/priv", "pr.3")
	pub := rig.storeGithubEvent(t, "acme/pub", "pr.4")
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.Topic != pub.Topic {
		t.Fatalf("the session's first frame is %s, want acme/pub's: the event of acme/priv reached it", frames[0].item.Topic)
	}
	requireFrames(t, frames, pub)
	standIn.none(t, time.Second)
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

// A repository that leaves the project's answer stops reaching the session at the first
// refresh that reads the new answer, its event acknowledged undelivered and counted, and the
// session's interest in it is gone at the refresh after. The production refresh runs every 30 s
// and an answer lives 60 s, so both happen within 60 s of the change.
func TestARepositoryThatLeavesTheProjectStopsReachingTheEdge(t *testing.T) {
	if edgeRepositoryRefresh != 30*time.Second || edgeRepositoryAnswerLife != 60*time.Second {
		t.Fatalf("the refresh runs every %s and an answer lives %s, want 30 s and 60 s", edgeRepositoryRefresh, edgeRepositoryAnswerLife)
	}
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_leaving"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>", "notifications.github.acme.two.>"), http.StatusOK, "the registration")
	first := rig.storeGithubEvent(t, "acme/two", "pr.1")
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), first)

	rig.repositories.answer("PRIV", "acme/pub")
	rig.tick(t)
	left := rig.storeGithubEvent(t, "acme/two", "pr.2")
	stays := rig.storeGithubEvent(t, "acme/pub", "pr.3")
	// The frames go in stream order, so the event after it arriving first is the proof the one
	// before it was not relayed.
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID == left.EventID {
		t.Fatalf("the acme/two event %s stored after acme/two left the answer reached the session", left.EventID)
	}
	requireFrames(t, frames, stays)
	if got := rig.withheld(t, "not_in_project"); got != 1 {
		t.Fatalf(`envoy_edge_github_withheld_total{reason="not_in_project"} = %d, want 1: the acme/two event withheld`, got)
	}

	rig.tick(t)
	recorder := rig.call(t, http.MethodGet, token, "/v1/registry/"+sessionID, "")
	requireStatus(t, recorder, http.StatusOK, "GET /v1/registry/"+sessionID)
	var info listenerwire.SessionInfo
	if err := json.Unmarshal(recorder.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	for _, topic := range info.Topics {
		if strings.HasPrefix(topic, "notifications.github.acme.two") {
			t.Fatalf("the session still follows %s after acme/two left the answer: %v", topic, info.Topics)
		}
	}
	if !slices.Contains(info.Topics, "notifications.github.acme.pub.>") {
		t.Fatalf("the session no longer follows acme/pub, which stayed: %v", info.Topics)
	}
}

// While no repository answer younger than 60 s stands, a registration naming a GitHub topic
// is 503 repositories_unknown, every GitHub frame is acknowledged undelivered with the gap mark
// on the next frame, named Envoy-Gap-Reason: repository_withheld so a client does not read it as
// the stream's retention gap, and every frame that is not GitHub's flows as before.
func TestAnUnknownRepositoryAnswerWithholdsOnlyGitHubFrames(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_unknown"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>"), http.StatusOK, "the registration")

	rig.repositories.failWith(http.StatusServiceUnavailable)
	requireEdgeRefusal(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>"), http.StatusServiceUnavailable, "repositories_unknown",
		"a registration naming acme/pub while the answer is 503")
	rig.clock.advance(edgeRepositoryAnswerLife + time.Second)
	rig.storeGithubEvent(t, "acme/pub", "pr.5")
	agent, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID != agent.EventID {
		t.Fatalf("the session's next frame is %s on %s, want the agent message: the acme/pub event was delivered with an answer %s old",
			frames[0].item.EventID, frames[0].item.Topic, edgeRepositoryAnswerLife+time.Second)
	}
	if got := frames[0].msg.Header.Get(relayHeaderGap); got != "true" {
		t.Fatalf("the frame after the withheld GitHub event carries %s %q, want true", relayHeaderGap, got)
	}
	if got := frames[0].msg.Header.Get("Envoy-Gap-Reason"); got != "repository_withheld" {
		t.Fatalf("the frame after the withheld GitHub event carries Envoy-Gap-Reason %q, want repository_withheld: a gap with no reason reads as the stream's retention gap", got)
	}
	if got := rig.withheld(t, "answer_unknown"); got != 1 {
		t.Fatalf(`envoy_edge_github_withheld_total{reason="answer_unknown"} = %d, want 1`, got)
	}
}

// Dispatch answers a repository in lower case and the intake keeps GitHub's case, so the
// edge compares owner and name ignoring case: an interest in Acme/Pub is admitted through an
// answer of acme/pub, and the event the intake publishes for Acme/Pub reaches the session.
func TestAnEdgeComparesRepositoriesIgnoringCase(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_case"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	before := rig.repositories.calls.Load()
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.Acme.Pub.>"), http.StatusOK, "an interest in Acme/Pub")
	if rig.repositories.calls.Load() == before {
		t.Fatal("the registration naming Acme/Pub read no repository answer; the answer is read fresh at each one")
	}
	event := rig.storeGithubEvent(t, "Acme/Pub", "pr.9")
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), event)
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

// A registration the edge refuses 503 repositories_unknown is told the listener's own sentence:
// nothing of what the broker answered reaches the client, its error code and its text among it.
// It holds both for a GitHub topic the registration names and for one the session's row holds,
// which the edge reads the answer for as well.
func TestAnEdgeRepositoriesRefusalCopiesNoBrokerText(t *testing.T) {
	rig := newEdgeRig(t)
	const named, stored = "ses_edge_unknown_named", "ses_edge_unknown_stored"
	rig.placeElsewhereFollowing(t, stored, []string{}, "notifications.github.acme.pub.>")
	rig.repositories.failWith(http.StatusServiceUnavailable)
	for _, call := range []struct {
		session string
		topics  []string
	}{
		{named, []string{"notifications.github.acme.pub.>"}},
		{stored, nil},
	} {
		recorder := rig.subscribe(t, rig.personToken(call.session, "PRIV"), call.session, call.topics...)
		requireEdgeRefusal(t, recorder, http.StatusServiceUnavailable, "repositories_unknown", fmt.Sprintf("%s's registration naming %v while the broker answers 503", call.session, call.topics))
		for _, broker := range []string{"REPOSITORIES_UNKNOWN", "the stand-in answers"} {
			if strings.Contains(recorder.Body.String(), broker) {
				t.Errorf("%s's 503 copies the broker's answer (%q): %s", call.session, broker, recorder.Body.String())
			}
		}
	}
}

// A subject writes a dot in a repository's name as an underscore, so acme/a.b and acme/a_b
// publish on one subject (notifications.github.acme.a_b): the session follows it through an
// answer that names acme/a.b, and the event of acme/a_b on that subject is withheld
// not_in_project, while the event of acme/a.b reaches it. The relay holds each event to the
// repository its own payload names.
func TestAnEdgeRelaysOnlyTheEventsOfTheRepositoryItsAnswerNames(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_dotted"
	rig.repositories.answer("PRIV", "acme/a.b")
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.a_b.>"), http.StatusOK, "an interest in acme/a.b, which its subject writes acme.a_b")
	other := rig.storeGithubEvent(t, "acme/a_b", "pr.1")
	own := rig.storeGithubEvent(t, "acme/a.b", "pr.2")
	if otherPrefix, ownPrefix := contracts.GithubRepositoryPrefix("acme", "a_b"), contracts.GithubRepositoryPrefix("acme", "a.b"); otherPrefix != ownPrefix {
		t.Fatalf("UNKNOWN: acme/a_b's events are under %s and acme/a.b's under %s, not one subject", otherPrefix, ownPrefix)
	}
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID == other.EventID {
		t.Fatalf("the event of acme/a_b reached a session whose answer names acme/a.b alone")
	}
	requireFrames(t, frames, own)
	if got := rig.withheld(t, "not_in_project"); got != 1 {
		t.Fatalf(`envoy_edge_github_withheld_total{reason="not_in_project"} = %d, want 1: the acme/a_b event withheld`, got)
	}
}

// A role read at the edge names a holder whose registering token listed none of the reader's
// projects without its place: a PRIV role held by a session another listener registered with no
// broker token is answered with its holder and claim, and with no directory, machine, host or
// title and no capabilities. A holder of the reader's project is answered whole.
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
	for role, holder := range map[string]string{internalRole: internal, peerRole: peer} {
		if _, err := rig.deps.registry.SetRole(holder, "test-machine", role, false); err != nil {
			t.Fatalf("claim %s for %s: %v", role, holder, err)
		}
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
	held := read(internalRole)
	if held.Holder != internal || held.Dir != "" || held.Host != "" || held.MachineID != "" || held.Title != "" || len(held.Capabilities) != 0 {
		t.Errorf("GET /v1/roles/%s answers %+v, want holder %s with no directory, host, machine, title or capabilities", internalRole, held, internal)
	}
	if whole := read(peerRole); whole.Holder != peer || whole.MachineID == "" || whole.Host == "" {
		t.Errorf("GET /v1/roles/%s answers %+v, want holder %s with its machine and host: it is a session of PRIV", peerRole, whole, peer)
	}
}

// edgeBrokerTransport answers each request the edge's repository reader sends with answer, on the
// reader's own goroutine, as the broker would over the network.
type edgeBrokerTransport func(*http.Request) (*http.Response, error)

func (f edgeBrokerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// One project whose broker read hangs holds back no other project's read: a refresh reads each
// served project on its own, so PRIV's answer is read while SLOW's read is still waiting, and
// PRIV's answer is fresh when the pass ends. Read one after the other, PRIV's would wait for
// SLOW's to time out, and with enough slow projects ahead of it would age past its life.
func TestAnEdgeRefreshReadsEachProjectOnItsOwn(t *testing.T) {
	var slowWaiting atomic.Int32
	slowEntered := make(chan struct{})
	release := make(chan struct{})
	var readWhileSlowWaited atomic.Bool
	privRead := make(chan struct{})
	transport := edgeBrokerTransport(func(request *http.Request) (*http.Response, error) {
		project := request.URL.Query().Get("project")
		if project == "SLOW" {
			slowWaiting.Add(1)
			close(slowEntered)
			defer slowWaiting.Add(-1)
			select {
			case <-release:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("{}")), Request: request}, nil
		}
		// This read waits for SLOW's to have begun, whichever order the reads start in, and then
		// sees whether that one is still waiting. A read that ends before SLOW's begins, its
		// time spent while the refresh runs one read at a time, saw no read beside it.
		defer close(privRead)
		select {
		case <-slowEntered:
			readWhileSlowWaited.Store(slowWaiting.Load() == 1)
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		body := fmt.Sprintf(`{"project":%q,"repositories":["acme/pub"]}`, project)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	reader := newEdgeRepositories("http://broker.example", listenerBrokerAccount(edgeAccountToken), []string{"SLOW", "PRIV"},
		newEdgeWithheldCounter(metrics.New()), logging.New("test"))
	reader.client = &http.Client{Transport: transport}
	done := make(chan struct{})
	go func() {
		defer close(done)
		reader.refresh(context.Background())
	}()
	select {
	case <-privRead:
	case <-time.After(30 * time.Second):
		t.Fatal("the refresh read no answer of PRIV within 30 s")
	}
	close(release)
	<-done
	if !readWhileSlowWaited.Load() {
		t.Fatal("the refresh read PRIV's answer only once SLOW's read had ended: a project whose read hangs holds back every project after it")
	}
	if repositories, complete := reader.standing([]string{"PRIV"}); !complete || !slices.Equal(repositories, []string{"acme/pub"}) {
		t.Fatalf("after the refresh PRIV stands %v (complete %t), want [acme/pub]", repositories, complete)
	}
}
