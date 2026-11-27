package main

// edge_rig_test.go is the edge listener's test rig: the listener as an edge deployment runs it on
// a real bus, with a stand-in for the broker's repository answer, and the helpers the edge rows
// share (edge_mode_test.go, edge_roles_test.go, edge_repositories_test.go,
// edge_repository_outage_test.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// edgeTestProjects is the projects the rows' edge listener serves: PRIV, unless the test's own
// environment sets ENVOY_EDGE_PROJECTS, read then as the listener reads it. Set empty, it is the
// edge-off control, the verification check's control run of every edge row: a listener that
// serves no project is in no edge mode. A set value that names no project, such as a lone comma,
// is refused, as the listener's start refuses it (internal/config's parseEdgeProjects), so it is
// never taken for the control.
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
	if raw != "" && len(projects) == 0 {
		t.Fatalf("ENVOY_EDGE_PROJECTS=%q names no project, and the listener refuses to start on it; set it empty for the edge-off control", raw)
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
// behind apiAuth with this listener's edge projects, the edge's grant on the delivery config the
// routes and the relays read (edgeGrant, as main sets it on every edge listener), the broker's
// introspection stub, and the stand-in of its repository route, read through the listener's own
// reader. Its refresh loop runs on ticks the row sends (tick), so a row orders a refresh against
// its own publishes.
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
		// Every edge listener runs with its grant (main.go): it keeps no role message for a next
		// holder, so a publish to a role no session holds is the 404, and its relays take over no
		// frame (takeOverFrame). The rig's role lane, rig.harness.config, stays the shared
		// listener's, which is where the role lane runs.
		grant, err := newEdgeGrant("test-machine", projects, met)
		if err != nil {
			t.Fatalf("the edge's grant for %v: %v", projects, err)
		}
		rig.deps.delivery.edge = grant
		rig.relays.cfg.edge = grant
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
	case <-time.After(listenerWaitBound(t)):
		t.Fatal("the repository refresh loop took no tick in time")
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

// storedEdgeSessionEntry is sessionID's entry as the session bucket holds it, read through a handle
// of the rig client's own, so the read caches nothing in any registry.
func storedEdgeSessionEntry(t *testing.T, rig *edgeRig, sessionID string) session.SessionEntry {
	t.Helper()
	entry, _ := storedEdgeSessionRecord(t, rig, sessionID)
	return entry
}

// storedEdgeSessionRecord is storedEdgeSessionEntry with the bucket revision the entry was read at,
// which every write of the entry moves on, so a row tells a write from none by the revision and
// not by a clock.
func storedEdgeSessionRecord(t *testing.T, rig *edgeRig, sessionID string) (session.SessionEntry, uint64) {
	t.Helper()
	sessions, err := rig.harness.client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	stored, err := sessions.Get(sessionID)
	if err != nil {
		t.Fatalf("read %s's session entry: %v", sessionID, err)
	}
	var entry session.SessionEntry
	if err := json.Unmarshal(stored.Value(), &entry); err != nil {
		t.Fatalf("decode %s's session entry %s: %v", sessionID, stored.Value(), err)
	}
	return entry, stored.Revision()
}

// registrationWarnings is the warnings of answer, a registration's 200
// (listenerwire.SubscribeResponse).
func registrationWarnings(t *testing.T, answer *httptest.ResponseRecorder) []string {
	t.Helper()
	var registered listenerwire.SubscribeResponse
	if err := json.Unmarshal(answer.Body.Bytes(), &registered); err != nil {
		t.Fatalf("decode the registration's answer %s: %v", answer.Body.String(), err)
	}
	return registered.Warnings
}

// requireWarning fails the test unless one of warnings, a registration's, begins with prefix.
func requireWarning(t *testing.T, warnings []string, prefix string) {
	t.Helper()
	if !slices.ContainsFunc(warnings, func(warning string) bool { return strings.HasPrefix(warning, prefix) }) {
		t.Errorf("the registration's warnings %q name nothing beginning %q", warnings, prefix)
	}
}

// requireUnknownSession fails the test unless recorder is the 404 of a session no listener has
// heard of (unknownSession): no last registration and no state of the session's own.
func requireUnknownSession(t *testing.T, recorder *httptest.ResponseRecorder, asked string) {
	t.Helper()
	var refusal listenerwire.NoLiveSessionRefusal
	_ = json.Unmarshal(recorder.Body.Bytes(), &refusal)
	if recorder.Code != http.StatusNotFound || refusal.LastSeen != 0 || refusal.State != session.Gone || !strings.Contains(refusal.Error, "does not know when it last registered") {
		t.Errorf("%s: status %d (%s), want the 404 of a session no listener has heard of", asked, recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
}

// edgeLogLines is each line of output, a listener's JSON log (logging.NewWithWriter), at level
// whose message begins with prefix, decoded. A line that is no JSON is passed over.
func edgeLogLines(output, level, prefix string) []map[string]any {
	var lines []map[string]any
	for line := range strings.SplitSeq(output, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if message, _ := record["msg"].(string); record["level"] == level && strings.HasPrefix(message, prefix) {
			lines = append(lines, record)
		}
	}
	return lines
}

// edgeSessionLogLines is each line of edgeLogLines(output, level, prefix) whose session_id is
// sessionID.
func edgeSessionLogLines(output, level, prefix, sessionID string) []map[string]any {
	return slices.DeleteFunc(edgeLogLines(output, level, prefix), func(line map[string]any) bool {
		return line["session_id"] != sessionID
	})
}

// requireLoggedTime fails the test unless the attribute key of line, a decoded log line
// (edgeLogLines), is the time want.
func requireLoggedTime(t *testing.T, line map[string]any, key string, want time.Time) {
	t.Helper()
	text, _ := line[key].(string)
	got, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || !got.Equal(want) {
		t.Errorf("the line %q has %s %q, want %s", line["msg"], key, text, want.Format(time.RFC3339Nano))
	}
}
