package api

// rls_world_test.go is the fixture the row-security tests of plan.md (INFRA-6168) share: a test
// server built as production builds it, the request store and the system store from testDeps,
// and a world of people, projects and content in which some projects are private, some people
// are limited members, and every kind of object exists on each side of a project's visibility.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The people of the world. Every login is one the test header identity signs in.
const (
	// rlsOwner is a full member who owns the two private projects, so she sees every project.
	rlsOwner = "alice"
	// rlsMember is a full member no access list names: he sees every public project and no
	// private one.
	rlsMember = "bob"
	// rlsGrouped is a limited member of the group rlsGroup, which rlsHush's access list names.
	rlsGrouped = "lena"
	// rlsListed is a limited member rlsVeil's and rlsList's access lists name directly.
	rlsListed = "liam"
	// rlsUnlisted is a limited member no access list names, directly or through a group: he sees
	// no project at all, a public one included (the architect's ruling on departure 6).
	rlsUnlisted = "una"
	// rlsGroup is the access group rlsHush's list names.
	rlsGroup = "crew"
)

// The projects of the world.
const (
	// rlsOpen is public and lists nobody.
	rlsOpen = "OPEN"
	// rlsHush is private, owned by rlsOwner, and lists the group rlsGroup.
	rlsHush = "HUSH"
	// rlsVeil is private, owned by rlsOwner, and lists rlsListed.
	rlsVeil = "VEIL"
	// rlsList is public and lists rlsListed.
	rlsList = "LIST"
	// rlsMissingProject and rlsMissingIssue name a project and an issue nobody holds.
	rlsMissingProject = "NONE"
	rlsMissingIssue   = "NONE-1"
)

// rlsWord is the word only one project's content holds, so a search, a list or a read that
// answers it answers a row of that project; every project's content also holds rlsCommonWord.
var rlsWord = map[string]string{rlsOpen: "pangolin", rlsHush: "quokka", rlsVeil: "narwhal", rlsList: "axolotl"}

// rlsCommonWord is the word every project's content holds.
const rlsCommonWord = "lantern"

// rlsMeaningQuery is the words a meaning search sends: the fake embedder answers them with the
// vector it answers every text it holds no vector for, so every embedded item is as close to them
// as it can be and the meaning leg finds each one the viewer may read.
const rlsMeaningQuery = "meaning probe"

// rlsContent is what the world holds of one project: its first issue and the objects on it.
type rlsContent struct {
	project, word string
	// issue is the project's first issue, "<project>-1", and spec its primary document.
	issue, spec string
	// comment is a remark of rlsOwner's; bobComment a note of rlsMember's, made while the
	// project was public, which reply answers; bobDoomed a second note of his, which a delete
	// is aimed at; sessionComment a remark of session.
	comment, bobComment, reply, bobDoomed, sessionComment string
	// session is the session that wrote sessionComment, and only in this project.
	session string
	// ask is an ask of rlsOwner's handed to rlsMember; message a message of rlsOwner's; guide a
	// project document; priority a priority the issue advances.
	ask, message, guide, priority string
	// cite is a remark of this project that cites another project's issue (HUSH's cites OPEN-1),
	// empty for a project whose content cites nothing.
	cite string
}

// ids is every object id the content holds.
func (c rlsContent) ids() []string {
	return []string{c.spec, c.comment, c.bobComment, c.reply, c.bobDoomed, c.sessionComment, c.ask, c.message, c.guide, c.cite}
}

// rlsServerConfig is what a row-security test server is built with beside its stores.
type rlsServerConfig struct {
	// embedder wires meaning search; nil is the deployment with none.
	embedder embed.Embedder
	// broker, when set, is the stub secrets broker the server asks about a broker token.
	broker *ciphertokentest.StubBroker
	// parentStatusRule turns DISPATCH_PARENT_STATUS_RULE on.
	parentStatusRule bool
	// system replaces the system store testDeps answers: a traced store on the same database.
	system *store.Store
	// asOwner builds the baseline server of TestAllPublicDeploymentUnchanged: a request store that
	// sends the viewer's settings as the owner (storetest.RequestAsOwner), meeting no policy.
	asOwner bool
	// app and githubAPIBase wire the GitHub App at a fake GitHub (architectureAppOptions); nil
	// and empty leave the client in its "no app credentials" state.
	app           *auth.AppConfig
	githubAPIBase string
}

// rlsServer is one test server and what it was built of.
type rlsServer struct {
	handler  http.Handler
	served   *server
	deps     Deps
	mux      *recordingMux
	database *store.Store
}

// recordingMux is the mux register mounts a server on, keeping the pattern of each mount, as
// routes' TestAKeyedWriteNeverRunsUnkeyed reads the mounting.
type recordingMux struct {
	*http.ServeMux
	mu       sync.Mutex
	patterns []string
}

func newRecordingMux() *recordingMux { return &recordingMux{ServeMux: http.NewServeMux()} }

func (m *recordingMux) Handle(pattern string, handler http.Handler) {
	m.record(pattern)
	m.ServeMux.Handle(pattern, handler)
}

func (m *recordingMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.record(pattern)
	m.ServeMux.HandleFunc(pattern, handler)
}

func (m *recordingMux) record(pattern string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.patterns = append(m.patterns, pattern)
}

// mounted is every pattern register mounted, in its order.
func (m *recordingMux) mounted() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.patterns)
}

// newRLSServer builds a server on database as production builds one: Store the request store and
// System database (testDeps), a document service on the system store, the header identity, the
// deployment's shared token, and what config adds.
func newRLSServer(t *testing.T, database *store.Store, config rlsServerConfig) *rlsServer {
	t.Helper()
	var input DepsInput
	if config.asOwner {
		input = DepsInput{Store: storetest.RequestAsOwner(t, database), System: database}
	} else {
		input = testDeps(t, database)
	}
	if config.system != nil {
		input.System = config.system
	}
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker, ServerURL: testServerURL, Settle: 20 * time.Millisecond, Identity: headerIdentity(database)})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the document service down: %v", err)
		}
	})
	input.Identity = headerIdentity(database)
	input.AgentTokens = sharedAgentTokens(t, "agent-token")
	input.ServerURL = testServerURL
	input.Docs = documents
	input.Events = broker
	input.SpecForm = specFormOn(t, "", testServerURL, documents)
	input.AgentPublisher = NewAgentPublishRecorder()
	input.ListenerTakeoverWindow = testTakeoverWindow
	input.HorizonStallAfter = horizontest.StallWarning
	input.HorizonWarnEvery = horizontest.StallWarning
	input.Embedder = config.embedder
	input.App, input.GitHubAPIBase = config.app, config.githubAPIBase
	input.ParentStatusRule = ParentStatusRule{on: config.parentStatusRule}
	if config.broker != nil {
		input.BrokerTokens = rlsBrokerVerifier(t, database, config.broker)
	}
	deps, err := NewDeps(input)
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := newRecordingMux()
	served := register(mux, t.Context(), deps)
	served.readWorkers = &backgroundWork{}
	t.Cleanup(func() { waitBackgroundOf(t, served) })
	return &rlsServer{handler: mux, served: served, deps: deps, mux: mux, database: database}
}

// rlsBrokerVerifier is the broker-token check newBrokerTokenRigOn builds, asking broker.
func rlsBrokerVerifier(t *testing.T, database *store.Store, broker *ciphertokentest.StubBroker) *ciphertoken.Verifier {
	t.Helper()
	verifier, err := ciphertoken.New(ciphertoken.Options{
		BrokerURL:         broker.Server.URL,
		Account:           ciphertoken.NewAccountToken("DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE", accountTokenFile(t, credentialTestAccountToken)),
		Origins:           []string{testServerURL},
		Replay:            store.NewDPoPProofs(database.Pool),
		Address:           auth.ForwardedAddress,
		ServesReadOnly:    true,
		ServesResults:     true,
		ServesApproveOnly: true,
	})
	if err != nil {
		t.Fatalf("build the broker token verifier: %v", err)
	}
	return verifier
}

// serve carries one request through the server and answers what it answered, waiting through
// the shared wait, whose one bound is the test binary's deadline.
func (s *rlsServer) serve(t *testing.T, request *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		s.handler.ServeHTTP(response, request)
		answered <- response
	}()
	return testwait.Receive(t, request.Method+" "+request.URL.String()+" to answer", answered)
}

// as is one request of login's browser: the test header identity's sign-in.
func (s *rlsServer) as(t *testing.T, login, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return s.serve(t, withPerson(brokerRequest(t, method, target, body, "", ""), login))
}

// asOK is as, failing the test unless the answer's status is want.
func (s *rlsServer) asOK(t *testing.T, login, method, target string, body any, want int) *httptest.ResponseRecorder {
	t.Helper()
	response := s.as(t, login, method, target, body)
	if response.Code != want {
		t.Fatalf("%s %s as %s: status=%d body=%s, want %d", method, target, login, response.Code, response.Body.String(), want)
	}
	return response
}

// bearer is one request presenting token as a bearer: a personal token, or the shared token.
func (s *rlsServer) bearer(t *testing.T, token, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return s.serve(t, brokerRequest(t, method, target, body, "Bearer "+token, ""))
}

// awaitWork waits until no work the server's earlier requests left running is left: the boot mark,
// every read stage, the term statistics refreshes of every project and every detached run.
func (s *rlsServer) awaitWork(t *testing.T) {
	t.Helper()
	rows, err := s.database.Pool.Query(context.Background(), `select key from projects`)
	if err != nil {
		t.Fatalf("list the projects whose statistics may be refreshing: %v", err)
	}
	projects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the project keys: %v", err)
	}
	idle := make(chan struct{})
	go func() {
		defer close(idle)
		if mark := s.served.bootMark; mark != nil {
			<-mark
		}
		s.served.readWorkers.wait()
		for _, project := range projects {
			<-s.served.termStats.DoChan(project, func() (any, error) { return nil, nil })
		}
		s.served.detached.wait()
	}()
	testwait.Receive(t, "the server's background work to end", idle)
}

// rlsWorld is the world TestDispatchRLSReadsWritesAndSearch reads: the system store, the server
// on its request store, the stub broker and the caller whose key every broker token is bound to,
// the fake embedder, and what each project holds.
type rlsWorld struct {
	*rlsServer
	broker   *ciphertokentest.StubBroker
	caller   *brokerCaller
	embedder *fakeEmbedder
	content  map[string]rlsContent
	// tokens counts the broker tokens said to the stub broker, for their names.
	tokens int
}

// seedRLSPeople records the world's people with their member kinds and the group rlsGroup with
// rlsGrouped in it, written straight to the tables: a group's write still refuses a limited
// member until INFRA-6168's work step 4 (access.ErrGroupMemberRestricted).
func seedRLSPeople(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into people (email, member_kind) values
			('alice', 'full'), ('bob', 'full'), ('lena', 'restricted'), ('liam', 'restricted'), ('una', 'restricted')
		on conflict (email) do update set member_kind = excluded.member_kind;
		insert into access_groups (name, owner) values ('crew', 'alice');
		insert into access_group_members (group_name, person) values ('crew', 'lena');
		-- The session ties are filled, as the later slice's backfill leaves them: an open viewer
		-- reads a session that wrote in no project it does not see (access.SessionClause).
		insert into session_projects_filled default values;
	`); err != nil {
		t.Fatalf("seed the world's people: %v", err)
	}
}

// listOnProject writes one entry of project's access list, a person or a group, as an owner's
// write of it would.
func listOnProject(t *testing.T, database *store.Store, project, kind, principal string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into project_access (project_key, principal_kind, principal, added_by)
		values ($1, $2, $3, '{"kind":"user","id":"alice"}')
	`, project, kind, principal); err != nil {
		t.Fatalf("list %s %s on %s: %v", kind, principal, project, err)
	}
}

// makeRLSPrivate makes project private with rlsOwner its owner, in one transaction of the system
// store, as the project-owner command does.
func makeRLSPrivate(t *testing.T, database *store.Store, broker *events.Broker, project string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the visibility write: %v", err)
	}
	defer tx.Rollback(ctx)
	by := model.Actor{Kind: "user", ID: rlsOwner}
	if _, _, err := access.SetOwner(ctx, tx, broker, project, rlsOwner, by); err != nil {
		t.Fatalf("give %s to %s: %v", project, rlsOwner, err)
	}
	if _, _, err := access.SetVisibility(ctx, tx, broker, project, model.ProjectPrivate, by); err != nil {
		t.Fatalf("make %s private: %v", project, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the visibility write: %v", err)
	}
}

// newRLSWorld builds the world: the people, the server, the four projects and their content,
// written while every project is public, then the access lists, then HUSH and VEIL made private.
func newRLSWorld(t *testing.T) *rlsWorld {
	t.Helper()
	database := storetest.Open(t)
	seedRLSPeople(t, database)
	world := &rlsWorld{
		broker:   ciphertokentest.NewStubBroker(t),
		caller:   newBrokerCaller(t),
		embedder: &fakeEmbedder{vectors: map[string][]float32{rlsMeaningQuery: defaultVector()}},
		content:  map[string]rlsContent{},
	}
	world.rlsServer = newRLSServer(t, database, rlsServerConfig{embedder: world.embedder, broker: world.broker})
	for _, project := range []string{rlsOpen, rlsHush, rlsVeil, rlsList} {
		world.asOK(t, rlsOwner, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": "The " + project + " project"}, http.StatusCreated)
	}
	for _, project := range []string{rlsOpen, rlsHush} {
		world.content[project] = world.seedContent(t, project)
	}
	for _, project := range []string{rlsVeil, rlsList} {
		word := rlsWord[project]
		issue := createdIssue(t, "file "+project+"'s issue", world.as(t, rlsOwner, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": fmt.Sprintf("The %s %s issue", word, rlsCommonWord), "spec": fmt.Sprintf("# Spec\n\nThe %s %s spec.\n", word, rlsCommonWord), "force": true,
		}))
		world.content[project] = rlsContent{project: project, word: word, issue: issue.Key, spec: issue.PrimaryArtifactID}
	}
	// HUSH's comment cites OPEN-1: a source in a project bob does not see, pointing at an issue he
	// does.
	cite := createThreadComment(t, world.handler, world.content[rlsHush].issue, map[string]any{
		"body": "The " + rlsWord[rlsHush] + " " + rlsCommonWord + " cites dispatch://" + world.content[rlsOpen].issue + ".",
	}, rlsOwner)
	hush := world.content[rlsHush]
	hush.cite = cite.ID
	world.content[rlsHush] = hush
	listOnProject(t, database, rlsHush, "group", rlsGroup)
	listOnProject(t, database, rlsVeil, "person", rlsListed)
	listOnProject(t, database, rlsList, "person", rlsListed)
	makeRLSPrivate(t, database, world.deps.Events, rlsHush)
	makeRLSPrivate(t, database, world.deps.Events, rlsVeil)
	awaitSettledDocuments(t, database)
	processAllPending(t, database, world.embedder)
	world.awaitWork(t)
	return world
}

// seedContent writes project's issue and every object on it, as rlsOwner, rlsMember and a session
// of the project's own write them.
func (w *rlsWorld) seedContent(t *testing.T, project string) rlsContent {
	t.Helper()
	word := rlsWord[project]
	content := rlsContent{project: project, word: word, session: "ses_" + word + "0001"}
	issue := createdIssue(t, "file "+project+"'s issue", w.as(t, rlsOwner, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": project, "title": fmt.Sprintf("The %s %s issue", word, rlsCommonWord), "spec": fmt.Sprintf("# Spec\n\nThe %s %s spec.\n", word, rlsCommonWord), "force": true,
	}))
	content.issue, content.spec = issue.Key, issue.PrimaryArtifactID
	content.comment = createThreadComment(t, w.handler, issue.Key, map[string]any{"body": fmt.Sprintf("A %s %s remark.", word, rlsCommonWord)}, rlsOwner).ID
	content.bobComment = createThreadComment(t, w.handler, issue.Key, map[string]any{"body": fmt.Sprintf("Bob's %s %s note.", word, rlsCommonWord)}, rlsMember).ID
	content.reply = createThreadComment(t, w.handler, issue.Key, map[string]any{
		"body": fmt.Sprintf("Alice's %s %s reply.", word, rlsCommonWord), "reply_to": content.bobComment,
	}, rlsOwner).ID
	content.bobDoomed = createThreadComment(t, w.handler, issue.Key, map[string]any{"body": fmt.Sprintf("Bob's %s %s aside.", word, rlsCommonWord)}, rlsMember).ID
	sessionComment := sessionRequest(t, w.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": fmt.Sprintf("The session's %s %s remark.", word, rlsCommonWord), "actor": map[string]any{"kind": "session", "id": content.session},
	})
	if sessionComment.Code != http.StatusCreated {
		t.Fatalf("comment on %s as %s: status=%d body=%s", issue.Key, content.session, sessionComment.Code, sessionComment.Body.String())
	}
	content.sessionComment = decodeBody[model.Comment](t, sessionComment).ID
	content.ask = decodeBody[model.Ask](t, w.asOK(t, rlsOwner, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": fmt.Sprintf("Is the %s %s lit?", word, rlsCommonWord), "options": []map[string]string{{"label": "Yes"}, {"label": "No"}},
	}, http.StatusCreated)).ID
	w.asOK(t, rlsOwner, http.MethodPut, "/api/v1/asks/"+content.ask+"/asked-of", map[string]any{"asked_of": rlsMember}, http.StatusOK)
	content.message = decodeBody[model.Message](t, w.asOK(t, rlsOwner, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": fmt.Sprintf("A %s %s message.", word, rlsCommonWord),
	}, http.StatusCreated)).ID
	content.guide = uploadDocument(t, w.handler, "/api/v1/projects/"+project, "guide.md", fmt.Sprintf("# Guide\n\nThe %s %s guide.\n", word, rlsCommonWord)).Artifact.ID
	content.priority = createTestPriority(t, w.handler, map[string]any{
		"id": word + "-" + rlsCommonWord, "title": fmt.Sprintf("The %s %s priority", word, rlsCommonWord), "description": "What the issue advances.", "owner": rlsOwner,
	}, rlsOwner).ID
	w.asOK(t, rlsOwner, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"advances": []string{content.priority}}, http.StatusOK)
	// Bob's own state of the session's conversation, which an upsert of his reaches later.
	w.asOK(t, rlsMember, http.MethodPut, "/api/v1/me/agents/"+content.session+"/state", map[string]any{
		"cleared_before": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
	}, http.StatusOK)
	return content
}

// brokerToken says answer to the stub broker for a new token and answers the token.
func (w *rlsWorld) brokerToken(answer ciphertoken.Introspection) string {
	w.tokens++
	token := fmt.Sprintf("an-rls-broker-token-%d", w.tokens)
	w.broker.Says(token, answer)
	return token
}

// personToken is a broker token of person's own (the project scope person) bound to the world's
// caller, carrying projects as its list.
func (w *rlsWorld) personToken(person string, projects ...string) string {
	answer := personScopedTokenAnswer(w.caller.jkt)
	answer.Person, answer.Projects = person, projects
	return w.brokerToken(answer)
}

// presenting is one request of the world's caller under the broker token token.
func (w *rlsWorld) presenting(t *testing.T, token, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return w.serve(t, w.caller.presenting(t, method, target, token, body))
}

// personalToken mints a personal token of owner's holding the person scope, with projects as its
// list (agent_tokens.projects, which only INFRA-6170 will let a person write).
func (w *rlsWorld) personalToken(t *testing.T, owner string, projects ...string) string {
	t.Helper()
	token := personalTokenOwnedBy(t, w.database, owner, model.ScopePerson)
	w.setTokenList(t, token, projects...)
	return token
}

// setTokenList writes the personal token's list.
func (w *rlsWorld) setTokenList(t *testing.T, token string, projects ...string) {
	t.Helper()
	if projects == nil {
		projects = []string{}
	}
	if _, err := w.database.Pool.Exec(context.Background(), `update agent_tokens set projects = $2 where id = $1::uuid`,
		personalTokenID(t, w.database, token), projects); err != nil {
		t.Fatalf("write the personal token's list %v: %v", projects, err)
	}
}

// rlsSend is one caller's request.
type rlsSend func(t *testing.T, method, target string, body any) *httptest.ResponseRecorder

// browser is login's browser.
func (w *rlsWorld) browser(login string) rlsSend {
	return func(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return w.as(t, login, method, target, body)
	}
}

// broker is the world's caller under the broker token token.
func (w *rlsWorld) brokered(token string) rlsSend {
	return func(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return w.presenting(t, token, method, target, body)
	}
}

// personal is an agent presenting the personal token token.
func (w *rlsWorld) personal(token string) rlsSend {
	return func(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return w.bearer(t, token, method, target, body)
	}
}

// answeredAsMissing fails t unless hidden, the request naming an object of a project the caller
// does not see, answers exactly what missing, the same request naming an object nobody holds,
// answers, once missingName is read as hiddenName in missing's body.
func answeredAsMissing(t *testing.T, what string, hidden, missing *httptest.ResponseRecorder, hiddenName, missingName string) {
	t.Helper()
	want := strings.ReplaceAll(missing.Body.String(), missingName, hiddenName)
	if hidden.Code != missing.Code || hidden.Body.String() != want {
		t.Errorf("%s: the hidden object's answer is %d %s, and the missing object's %d %s, want one answer",
			what, hidden.Code, cutBody(hidden.Body.String()), missing.Code, cutBody(missing.Body.String()))
	}
}

// sees reports whether send's caller reads key: 200 is true, and the answer an issue nobody holds
// gets (key read as rlsMissingIssue) is false; any other answer fails t.
func sees(t *testing.T, send rlsSend, key string) bool {
	t.Helper()
	read := send(t, http.MethodGet, "/api/v1/issues/"+key, nil)
	if read.Code == http.StatusOK {
		return true
	}
	missing := send(t, http.MethodGet, "/api/v1/issues/"+rlsMissingIssue, nil)
	if want := strings.ReplaceAll(missing.Body.String(), rlsMissingIssue, key); read.Code != missing.Code || read.Body.String() != want {
		t.Fatalf("GET %s answered %d %s, which is neither the issue nor the answer %s gets (%d %s)",
			key, read.Code, cutBody(read.Body.String()), rlsMissingIssue, missing.Code, cutBody(missing.Body.String()))
	}
	return false
}

// requireSight fails t unless send's caller sees exactly the issues of want among the first issues
// of the world's four projects.
func (w *rlsWorld) requireSight(t *testing.T, who string, send rlsSend, want ...string) {
	t.Helper()
	for _, project := range []string{rlsOpen, rlsHush, rlsVeil, rlsList} {
		key := w.content[project].issue
		if got, expected := sees(t, send, key), slices.Contains(want, project); got != expected {
			t.Errorf("%s reads %s: %t, want %t (the projects it sees are %v)", who, key, got, expected, want)
		}
	}
}

// listedProjects is the projects GET /api/v1/projects lists to send's caller.
func listedProjects(t *testing.T, send rlsSend) []string {
	t.Helper()
	response := send(t, http.MethodGet, "/api/v1/projects", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/projects: status=%d body=%s", response.Code, response.Body.String())
	}
	keys := []string{}
	for _, project := range decodeBody[[]model.Project](t, response) {
		keys = append(keys, project.Key)
	}
	slices.Sort(keys)
	return keys
}

// hiddenMarks is what an answer naming a row of one of projects carries: the row's key and
// project in the members that name them, its project's own word, and the id of each of its
// objects. A key written in public text (a blocked_by list, a body) is not one of them: plan D13
// accepts it.
func (w *rlsWorld) hiddenMarks(projects ...string) []string {
	var marks []string
	for _, project := range projects {
		content := w.content[project]
		marks = append(marks,
			`"key":"`+project+`-`, `"issue_key":"`+project+`-`, `"project_key":"`+project+`"`, `"project":"`+project+`"`, `"key":"`+project+`"`,
			content.word)
		for _, id := range content.ids() {
			if id != "" {
				marks = append(marks, id)
			}
		}
	}
	return marks
}

// requireNothingHidden fails t unless body carries none of marks.
func requireNothingHidden(t *testing.T, what, body string, marks []string) {
	t.Helper()
	for _, mark := range marks {
		if strings.Contains(body, mark) {
			t.Errorf("%s names a row the caller does not see (%q): %s", what, mark, cutBody(body))
			return
		}
	}
}

// requireHolds fails t unless body carries mark, the row of a visible project the answer must
// hold, so a check that it holds no hidden row is not passed by an empty answer.
func requireHolds(t *testing.T, what, body, mark string) {
	t.Helper()
	if !strings.Contains(body, mark) {
		t.Errorf("%s does not name the visible row %q: %s", what, mark, cutBody(body))
	}
}

// cutAtNow is the database's now() as RFC 3339: the latest cut a read at a past time takes, and a
// moment no clock of the test's runs ahead of.
func cutAtNow(t *testing.T, database *store.Store) string {
	t.Helper()
	return databaseNow(t, database.Pool).UTC().Format(time.RFC3339Nano)
}

// missingUUID is an id no object holds.
func missingUUID() string { return uuid.NewString() }
