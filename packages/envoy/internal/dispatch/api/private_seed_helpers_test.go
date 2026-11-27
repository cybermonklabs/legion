package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// The people the access tests name beside the server's own alice and bob: carol and dave are full
// members, and erin a restricted member.
const (
	accessCarol = "carol@example.com"
	accessDave  = "dave@example.com"
	accessErin  = "erin@other.example"
)

// wantAccessRefusal fails the test unless response is status with code, naming who made the call.
func wantAccessRefusal(t *testing.T, response *httptest.ResponseRecorder, status int, code, who, call string) {
	t.Helper()
	if response.Code != status || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("%s: %s = %d %s, want %d %s", who, call, response.Code, response.Body.String(), status, code)
	}
}

// The two projects every test here reads: PUB, public and alice's, and PRIV, carol's, which each
// test makes private. alice is a full member who sees PUB alone once PRIV is private; carol sees
// both.
const (
	publicProject  = "PUB"
	privateProject = "PRIV"
)

// seedCredentialProjects makes PUB as alice and PRIV as carol, both public.
func seedCredentialProjects(t *testing.T, handler http.Handler, database *store.Store) {
	t.Helper()
	seedPeople(t, database, accessCarol)
	for _, project := range []struct{ key, creator string }{{publicProject, "alice"}, {privateProject, accessCarol}} {
		if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project.key, "name": project.key,
		}, project.creator); created.Code != http.StatusCreated {
			t.Fatalf("create %s as %s: status=%d body=%s", project.key, project.creator, created.Code, created.Body.String())
		}
	}
}

// makePrivate makes project private, naming channels when any are given. This release's route
// refuses the setting (409 PRIVATE_PROJECTS_OFF) until the deployment's bus can hold it, so the row
// is written as that write will write it, with the fill marker the switch requires first: a
// project the route made private implies session_projects_filled, since the switch refuses until
// it is written.
func makePrivate(t *testing.T, database *store.Store, project string, channels ...string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'private' where key = $1`, project); err != nil {
		t.Fatalf("make %s private: %v", project, err)
	}
	if len(channels) > 0 {
		if _, err := database.Pool.Exec(context.Background(), `update projects set slack_channels = $2 where key = $1`, project, channels); err != nil {
			t.Fatalf("name %s's Slack channels %v: %v", project, channels, err)
		}
	}
	if _, err := database.Pool.Exec(context.Background(), `insert into session_projects_filled select now() where not exists (select 1 from session_projects_filled)`); err != nil {
		t.Fatalf("write the fill marker beside %s: %v", project, err)
	}
}

// issueIn files an issue in project as login and answers its key.
func issueIn(t *testing.T, handler http.Handler, login, project string) string {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": "An issue of " + project,
	}, login)
	if created.Code != http.StatusCreated {
		t.Fatalf("file an issue in %s as %s: status=%d body=%s", project, login, created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
}

// mintBody is the body of a mint of a token named name, naming projects unless it is nil.
func mintBody(name string, projects []string) map[string]any {
	body := map[string]any{"name": name}
	if projects != nil {
		body["projects"] = projects
	}
	return body
}

// The people of the reached-project rows: each a full member who sees PRIV in one way, and loses it.
const (
	reachedXavier = "xavier@example.com" // on PRIV's list
	reachedGina   = "gina@example.com"   // in a group PRIV's list names
	reachedOwen   = "owen@example.com"   // PRIV's owner for a while
	reachedGroup  = "priv-team"
)

// reachedReplay is the replay each session holds: the text of PRIV's issue, which it read while
// its person saw PRIV.
func reachedReplay(session string) agentstream.Frame {
	return agentstream.Frame(`{"v":1,"kind":"replay","session":"` + session + `","text":"The issue of PRIV"}`)
}

// newReachedRigOf is a private seed whose listener holds roster, the listener's records, each of
// sessions holding a replay of PRIV's issue's text.
func newReachedRigOf(t *testing.T, roster string, sessions ...string) contentAgentRig {
	t.Helper()
	return newReachedRigWith(t, testServerOptions{}, roster, sessions...)
}

// newReachedRigWith is newReachedRigOf on a test server built from options.
func newReachedRigWith(t *testing.T, options testServerOptions, roster string, sessions ...string) contentAgentRig {
	t.Helper()
	listener, _ := newBroadcastListener(t, roster)
	stream := agentstream.NewMemory()
	for _, session := range sessions {
		stream.SetReplay(session, reachedReplay(session))
	}
	options.envoyURL, options.agentStream = listener.URL, stream
	seed := seedPrivateProjectWith(t, options)
	seedPeople(t, seed.database, reachedXavier, reachedGina, reachedOwen)
	if _, err := seed.database.Pool.Exec(context.Background(), `
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, $1::jsonb, now(), now() + interval '1 hour')
		on conflict (id) do update set sessions = excluded.sessions, sent_at = excluded.sent_at, expires_at = excluded.expires_at`,
		roster); err != nil {
		t.Fatalf("store the listener's sessions: %v", err)
	}
	return contentAgentRig{seed: seed, url: liveURLOf(t, seed.handler), stream: stream}
}

// hideFromBrokerPerson makes project key, which the broker rig's person made and so owns, private
// and owned by carol instead, with no access list: a project the rig's tokens' person, alice, does
// not see.
func hideFromBrokerPerson(t *testing.T, rig *brokerTokenRig, key string) {
	t.Helper()
	for _, statement := range []string{
		`update projects set owner = 'carol@example.com' where key = $1`,
		`delete from project_access where project_key = $1`,
	} {
		if _, err := rig.database.Pool.Exec(t.Context(), statement, key); err != nil {
			t.Fatalf("give %s to carol: %v", key, err)
		}
	}
	makePrivate(t, rig.database, key)
}

// addPerson names person on project key's access list as its owner carol, through the write the
// access route makes (access.AddPrincipal), as setVisibility makes a project private: this
// release has no route that writes an access list (PUT /api/v1/projects/{key}/access/people/{email}
// comes with a later slice).
func (s privateSeed) addPerson(t *testing.T, key, person string) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin naming %s on %s's list: %v", person, key, err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := access.AddPrincipal(ctx, tx, s.deps.Events, key, access.PrincipalPerson, person, model.Actor{Kind: "user", ID: seedCarol}); err != nil {
		t.Fatalf("name %s on %s's list: %v", person, key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit naming %s on %s's list: %v", person, key, err)
	}
}

// The helpers below are H's, from the S3c test files this stand-in leaves out because their own
// tests need a slice that is not here (S2's access routes, S7's credentials, or main's newer
// routes and tables that the real S3c slices port), kept because the tests that remain call them:
// live_access_test.go, server_reads_test.go, people_reads_access_test.go,
// m3_hidden_key_lock_test.go, every_create_stores_its_node_test.go, document_reads_access_test.go
// and read_fragment_census_test.go.

// liveURLOf serves handler behind a real listener until the test ends and answers its GET /ws/live
// address, so a caller of verification v16's seed opens the live socket as production serves it.
func liveURLOf(t *testing.T, handler http.Handler) string {
	t.Helper()
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/live"
}

// liveHeader is what caller presents on the live socket's upgrade: a person at a browser by the
// test identity's header, a token by its bearer.
func (s privateSeed) liveHeader(t *testing.T, caller privateCaller) http.Header {
	t.Helper()
	if login := caller.login(); login != "" {
		return asUser(login)
	}
	return withBearer(s.bearer(t, caller))
}

// dialLive opens the live socket at url, a server of the seed's handler (liveURLOf), as header
// presents and reads it. The socket names the seed's store (servedBy), the one its server reads,
// so a wait behind the event horizon reads that horizon's own account of itself.
func (s privateSeed) dialLive(t *testing.T, url string, header http.Header) *socketClient {
	t.Helper()
	return openLiveSocketAt(t, gws.DefaultDialer, url, header).servedBy(s.database).read()
}

// liveEvent is what these tests read of an event a live socket sent.
type liveEvent struct {
	ID      int64           `json:"id"`
	Type    string          `json:"type"`
	Project string          `json:"project"`
	Payload json.RawMessage `json:"payload"`
}

// liveEventsThrough reads sub's events on client until marker holds for one, and answers each event
// it read, the marker's included. Events go out in id order, so an event of a lower id than the
// marker's that is not among them was not sent.
func liveEventsThrough(t *testing.T, client *socketClient, sub string, marker func(liveEvent) bool) []liveEvent {
	t.Helper()
	var events []liveEvent
	for {
		frame := client.event(t, sub)
		var event liveEvent
		if err := json.Unmarshal(frame.Event, &event); err != nil {
			t.Fatalf("decode the event of %s's frame %+v: %v", sub, frame, err)
		}
		events = append(events, event)
		if marker(event) {
			return events
		}
	}
}

// commentCreated is the marker of the comment.created event of the comment id.
func commentCreated(id string) func(liveEvent) bool {
	return func(event liveEvent) bool {
		var payload struct {
			ID string `json:"id"`
		}
		return event.Type == "comment.created" && json.Unmarshal(event.Payload, &payload) == nil && payload.ID == id
	}
}

// agentStateOf is the marker of a user_agent_state.updated event of login's: the one ownerless
// event about a person's own thread, which reaches that person's credentials, and an open viewer,
// that may see the session it names (eventReadableSQL).
func agentStateOf(login string) func(liveEvent) bool {
	return func(event liveEvent) bool {
		var payload struct {
			Login string `json:"login"`
		}
		return event.Type == "user_agent_state.updated" && json.Unmarshal(event.Payload, &payload) == nil && payload.Login == login
	}
}

// markAgentState writes login's pin on session, as that person at the browser, which appends one
// user_agent_state.updated of theirs (agentStateOf): a marker that ends a read. The session must be
// one login sees (access.SessionClause): one that acts in no project, for an open viewer, or one
// that acts in a project login sees. The listener is first made to list it with no project, as
// Dispatch stores the listener's answer (the route_roster row), since while a project is private a
// session that acts in no project reaches an open viewer only through that record.
func (s privateSeed) markAgentState(t *testing.T, login, session string) {
	t.Helper()
	if _, err := s.database.Pool.Exec(t.Context(), `
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, jsonb_build_array(jsonb_build_object('session_id', $1::text, 'projects', '[]'::jsonb)), now(), now())
		on conflict (id) do update
		set answered = true, sessions = case when route_roster.sessions @> excluded.sessions then route_roster.sessions
			else route_roster.sessions || excluded.sessions end`, session); err != nil {
		t.Fatalf("list %s as the listener's stored answer: %v", session, err)
	}
	if response := dispatchRequest(t, s.handler, http.MethodPut, "/api/v1/me/agents/"+session+"/state", map[string]any{"pinned": true}, login); response.Code != http.StatusOK {
		t.Fatalf("%s pins %s: status=%d body=%s", login, session, response.Code, response.Body.String())
	}
}

// requireResumeComplete fails unless caller's resume from since says replay_complete want, naming
// why the caller's page does or does not owe a refresh.
func requireResumeComplete(t *testing.T, seed privateSeed, url string, caller privateCaller, since int64, want bool, why string) {
	t.Helper()
	client := seed.dialLive(t, url, seed.liveHeader(t, caller))
	if got := subscribeEventsStart(t, client, "feed", &since); got.ReplayComplete != want {
		t.Errorf("%s's resume from %d says replay_complete %t, want %t: %s", caller, since, got.ReplayComplete, want, why)
	}
}

// readerRequest is a request as caller, as request sends one, for a test that calls a server
// method on it directly.
func (s privateSeed) readerRequest(t *testing.T, caller privateCaller) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/issues", nil)
	if login := caller.login(); login != "" {
		request.Header.Set("X-Dispatch-User", login)
	} else {
		request.Header.Set("Authorization", "Bearer "+s.bearer(t, caller))
	}
	return request
}

// The sessions the stand-in listener lists, by what each does in the seed: session-pub comments on
// PUB's issue, session-priv on PRIV's, session-both opens an ask on each, and session-idle does
// nothing, so it has no history in any project.
const (
	sessionOfPublic  = "session-pub"
	sessionOfPrivate = "session-priv"
	sessionOfBoth    = "session-both"
	sessionIdle      = "session-idle"
)

// privateRoster is the stand-in listener's sessions, newest first by last_seen, so the agents list
// answers them in this order. session-idle has acted in no project, and the listener lists it with
// a credential that lists none ("projects":[]): a row with no projects key is a listener that does
// not know the session's projects, and reaches no open viewer while a project is private.
const privateRoster = `[
	{"session_id":"session-both","title":"Both","capabilities":["btw","steer"],"last_seen":400},
	{"session_id":"session-priv","title":"Private","capabilities":["btw","steer"],"last_seen":300},
	{"session_id":"session-pub","title":"Public","capabilities":["btw","steer"],"last_seen":200},
	{"session_id":"session-idle","title":"Idle","capabilities":["btw","steer"],"projects":[],"last_seen":100}
]`

// privateRosterListed is privateRoster with the listener listing the projects of session-pub's
// and session-priv's credentials, as a listener that knows them does: each is carol's, its broker
// token scoped to one project (asSession). A session's conversations and files are its content
// (access.SessionContentClause), and carol sees PRIV, so with no list a caller that does not see
// PRIV reads no conversation of either; the list bounds what session-pub's credential reads to PUB.
// A read stores the list where the content rule reads it only once Dispatch has read the
// listener's sessions, as a list of the agents does.
const privateRosterListed = `[
	{"session_id":"session-both","title":"Both","capabilities":["btw","steer"],"last_seen":400},
	{"session_id":"session-priv","title":"Private","capabilities":["btw","steer"],"projects":["PRIV"],"last_seen":300},
	{"session_id":"session-pub","title":"Public","capabilities":["btw","steer"],"projects":["PUB"],"last_seen":200},
	{"session_id":"session-idle","title":"Idle","capabilities":["btw","steer"],"projects":[],"last_seen":100}
]`

// seedSessionWork has the roster's sessions act in the seed's projects, each under a credential
// that proves the session (asSession): what a proven session writes in a project is what makes it
// one of the project's agents (session_projects).
func seedSessionWork(t *testing.T, seed privateSeed) {
	t.Helper()
	act := func(project, route, session string, body map[string]any) {
		t.Helper()
		seed.asSession(t, session, project, http.MethodPost, "/api/v1/issues/"+seed.issue[project]+"/"+route, body, http.StatusCreated)
	}
	act(seedPublic, "comments", sessionOfPublic, map[string]any{"body": "Done on PUB."})
	act(seedPrivate, "comments", sessionOfPrivate, map[string]any{"body": "Done on PRIV."})
	act(seedPublic, "asks", sessionOfBoth, map[string]any{"question": "Ship PUB?"})
	act(seedPrivate, "asks", sessionOfBoth, map[string]any{"question": "Ship PRIV?"})
}

// heldLock is a transaction of the test's own that holds a lock a write takes, and its backend.
type heldLock struct {
	tx  pgx.Tx
	pid int32
}

// awaitAnswerOrWait waits, up to storeStepWait, for the answer on answered while held holds its
// lock. It answers the response when the write answered first, and otherwise the statement a
// backend queued behind the holder is waiting at: a write that waits there is the one under test,
// since no other writer of the seed runs while a row does (lockSeed). The bound is far above what
// any of these writes takes, so passing it is a write that neither answers nor waits on the holder.
func awaitAnswerOrWait(t *testing.T, seed privateSeed, held heldLock, answered <-chan *httptest.ResponseRecorder) (*httptest.ResponseRecorder, string) {
	t.Helper()
	deadline := time.NewTimer(storeStepWait)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case response := <-answered:
			return response, ""
		case <-deadline.C:
			t.Fatalf("the write neither answered nor queued behind the holder within %s", storeStepWait)
		case <-tick.C:
		}
		var waiting *string
		err := seed.database.Pool.QueryRow(context.Background(), `
			select query from pg_stat_activity where $1::int = any(pg_blocking_pids(pid)) limit 1
		`, held.pid).Scan(&waiting)
		if err == nil {
			return nil, deref(waiting)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read who waits behind the holder: %v", err)
		}
	}
}

// letGo lets the holder go and answers the write's answer, which must come within storeStepWait.
func (held heldLock) letGo(t *testing.T, answered <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	if err := held.tx.Rollback(context.Background()); err != nil {
		t.Fatalf("let the holder go: %v", err)
	}
	select {
	case response := <-answered:
		return response
	case <-time.After(storeStepWait):
		t.Fatalf("the write did not answer within %s of the holder letting go", storeStepWait)
		return nil
	}
}

// documentObjects are the objects of one project a document read names: the project, its issue,
// a document of the project and one of the issue, each document with its slug. The missing
// objects are names nobody holds, each spelled as a real one is, so the answer for a hidden
// object and the answer for a missing one differ by the name alone.
type documentObjects struct {
	project, issue                   string
	document, documentSlug           string
	issueDocument, issueDocumentSlug string
}

// seedDocumentObjects writes, as carol, who owns both projects, what project's document reads
// read: a document of the project and one of its issue, and an architecture source, each through
// its route, and a repository mapping as the last release left one (storeRepoMapping), which no
// route writes now.
func seedDocumentObjects(t *testing.T, seed privateSeed, project string) documentObjects {
	t.Helper()
	objects := documentObjects{project: project, issue: seed.issue[project]}
	upload := func(target string) (id, slug string) {
		uploaded := decodeBody[artifactUploadResponse](t, seed.mustRequest(t, callerCarol, http.MethodPost, target,
			map[string]any{"name": "Runbook.md", "content": "# Runbook\n\nThe runbook steps.\n"}, http.StatusCreated))
		return uploaded.Artifact.ID, uploaded.Artifact.Slug
	}
	objects.document, objects.documentSlug = upload("/api/v1/projects/" + project + "/artifacts")
	objects.issueDocument, objects.issueDocumentSlug = upload("/api/v1/issues/" + objects.issue + "/artifacts")
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+project+"/architecture-sources/"+architectureRepo(project),
		map[string]string{"branch": "main"}, http.StatusOK)
	storeRepoMapping(t, seed.database, "acme/"+strings.ToLower(project)+"-app", project, false)
	return objects
}

// architectureRepo is the repository project's architecture source is read from, which the fake
// GitHub App the test server reads holds.
func architectureRepo(project string) string {
	return "acme/" + strings.ToLower(project) + "-arch"
}

// fakeGitHubAppConfig is a GitHub App credential with a key of its own, which a test server whose
// GitHub is a fake (githubtest.Server) signs its App requests with.
func fakeGitHubAppConfig(t *testing.T) *auth.AppConfig {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the App's key: %v", err)
	}
	return &auth.AppConfig{ClientID: "Iv1.test", PEM: string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))}
}

// readsProjectRows reports whether a GET row of class answers rows a project, the graph, a person
// or a session owns: the reads the census walks.
func readsProjectRows(class readclass.Class) bool {
	switch class {
	case readclass.Project, readclass.Graph, readclass.People, readclass.Agents:
		return true
	}
	return false
}

// censusQueries are the queries the walked reads that read nothing without one are sent, each
// naming objects of the project the walk reads at.
var censusQueries = map[string]func(censusObjects) url.Values{
	"GET /api/v1/issues/resolve": func(o censusObjects) url.Values { return url.Values{"ref": {o.issue}} },
	"GET /api/v1/references":     func(o censusObjects) url.Values { return url.Values{"to": {"dispatch://" + o.issue}} },
	"GET /api/v1/references/questions": func(o censusObjects) url.Values {
		return url.Values{"kind": {"comment"}, "id": {o.comment}}
	},
	"GET /api/v1/block-links": func(o censusObjects) url.Values {
		spec := "dispatch://" + o.issue + "/spec"
		return url.Values{"from": {spec}, "to": {spec}}
	},
	"GET /api/v1/asks/open":              func(o censusObjects) url.Values { return url.Values{"project": {o.project}} },
	"GET /api/v1/search":                 func(o censusObjects) url.Values { return url.Values{"q": {"runbook"}} },
	"GET /api/v1/search/results":         func(o censusObjects) url.Values { return url.Values{"q": {"runbook"}} },
	"GET /api/v1/projects/{key}/related": func(o censusObjects) url.Values { return url.Values{"title": {"Runbook steps"}} },
	"GET /api/v1/related-checks":         func(o censusObjects) url.Values { return url.Values{"project": {o.project}} },
	"GET /api/v1/near-copy-checks":       func(o censusObjects) url.Values { return url.Values{"project": {o.project}} },
	"GET /api/v1/reference-targets": func(o censusObjects) url.Values {
		return url.Values{"ref": {"dispatch://" + o.issue}}
	},
	// Both versions are required (400 INVALID_VERSION without them); the seed names the project
	// document's second version.
	"GET /api/v1/artifacts/{id}/changes": func(o censusObjects) url.Values { return url.Values{"from": {"1"}, "to": {"2"}} },
}

// censusObjects are the objects of one project a walked read is filled with: its documents, an
// ask, a comment and a message of its issue, a handoff, a session that acted in it with a file of
// that session's conversation, a person, a broadcast to the session; and the priority and alice's
// going run, which belong to no project and are the same on both walks.
type censusObjects struct {
	documentObjects
	block                          string
	ask, comment, message, handoff string
	session, conversationSlug      string
	person, broadcast              string
	priority, goingRun             string
	// nearCopyCheck is the near-copy gate's row of the project's own create (advancing the
	// priority), which GET /api/v1/near-copy-checks/{id} reads under the project's rule.
	nearCopyCheck string
	// deliveryRun is a deploy run of the deploy repository, one for each walk, which GET
	// /api/v1/delivery/runs/{id} reads under the timeline's rule.
	deliveryRun string
}

// wildcard matches one wildcard of a route pattern.
var wildcard = regexp.MustCompile(`\{([a-z_]+)\}`)

// fill is pattern with each wildcard filled by the object probe names for it. A document's slug
// is that of the document the path's owner holds: the conversation's, the issue's or the
// project's.
func (o censusObjects) fill(t *testing.T, pattern string, probe readProbe) string {
	t.Helper()
	slug := o.documentSlug
	switch {
	case strings.HasPrefix(pattern, "/api/v1/agents/"):
		slug = o.conversationSlug
	case strings.HasPrefix(pattern, "/api/v1/issues/"):
		slug = o.issueDocumentSlug
	}
	return wildcard.ReplaceAllStringFunc(pattern, func(match string) string {
		object := probe[wildcard.FindStringSubmatch(match)[1]]
		value := map[probeObject]string{
			probeProject: o.project, probeIssue: o.issue, probeDocument: o.document, probeDocumentSlug: slug,
			probeDocumentVersion: "1", probeBlock: o.block, probeAsk: o.ask, probeComment: o.comment,
			probeMessage: o.message, probeHandoff: o.handoff, probeHandoffPhase: "plan", probeSession: o.session,
			probePerson: o.person, probePriority: o.priority, probeBroadcast: o.broadcast, probeGoingRun: o.goingRun,
			probeNearCopyCheck: o.nearCopyCheck, probeDeliveryRun: o.deliveryRun,
			// The repository seedDocumentObjects maps to the project.
			probeRepositoryOwner: "acme", probeRepositoryName: strings.ToLower(o.project) + "-app",
		}[object]
		if value == "" {
			t.Fatalf("%s: the census seeds no %q for %s", pattern, object, match)
		}
		return value
	})
}

// fragmentCensus records the SQL of every statement a request made on a recording context
// (recordingKey), each statement of a batch on its own, so the background work of the server is
// no part of the record.
type fragmentCensus struct {
	mu         sync.Mutex
	statements []string
}

func (c *fragmentCensus) add(ctx context.Context, statement string) {
	if ctx.Value(recordingKey{}) == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statements = append(c.statements, statement)
}

func (c *fragmentCensus) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.add(ctx, data.SQL)
	return ctx
}

func (c *fragmentCensus) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *fragmentCensus) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (c *fragmentCensus) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	c.add(ctx, data.SQL)
}

func (c *fragmentCensus) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// read makes one GET of target as alice on a recording context and answers the statements it made.
func (c *fragmentCensus) read(t *testing.T, handler http.Handler, target string) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	c.mu.Lock()
	c.statements = nil
	c.mu.Unlock()
	request := httptest.NewRequestWithContext(context.WithValue(t.Context(), recordingKey{}, true), http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", seedAlice)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	c.mu.Lock()
	defer c.mu.Unlock()
	return response, slices.Clone(c.statements)
}

// fragmentCensusFixture is verification v16's seed on a server whose pool census traces, holding
// the objects of each project the walk reads at.
type fragmentCensusFixture struct {
	privateSeed
	public, private censusObjects
}

// seedFragmentCensus writes, through the routes and as the person who would write each, what every
// walked read answers: each project's documents, threads, handoff, session work, conversation file
// and broadcast; alice's comment, answer, direct message and going run on PUB; a priority each
// project's work advances; and the delivery settings, pull requests and CI pass the deployment
// reads count. The delivery rows are written as the code that stores them writes them, because no
// route writes those.
func seedFragmentCensus(t *testing.T, census *fragmentCensus) fragmentCensusFixture {
	t.Helper()
	interests := func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/v1/interests/":
			_, _ = w.Write([]byte(`[{"session_id":"` + sessionOfPublic + `","topics":["notifications.dispatch.>"],"updated_at":1}]`))
		case "/v1/legion/agents":
			_, _ = w.Write([]byte("[]"))
		default:
			return false
		}
		return true
	}
	// The listener lists session-pub's and session-priv's credentials' projects, so alice reads
	// PUB's session's conversation file and her direct message to it, which are its content
	// (access.SessionContentClause), and no conversation of PRIV's session.
	listener, _ := newBroadcastListener(t, privateRosterListed, interests)
	github := &fakeGitHubApp{installations: map[string]string{}, repos: map[string]*fakeArchitectureRepo{}}
	for _, project := range []string{seedPublic, seedPrivate} {
		github.installations[architectureRepo(project)] = "read"
		github.installations["acme/"+strings.ToLower(project)+"-app"] = "read"
		github.repos[architectureRepo(project)] = &fakeArchitectureRepo{commit: shaOne, files: archFiles()}
	}
	embedder := &fakeEmbedder{vectors: map[string][]float32{}}
	seed := seedPrivateProjectWith(t, testServerOptions{
		store: storetest.TracedStore(t, storetest.Open(t), census), envoyURL: listener.URL, embedder: embedder,
		app: fakeGitHubAppConfig(t), githubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: github.handler(t).ServeHTTP}),
	})
	seedSessionWork(t, seed)
	// A list of the agents stores the listener's list, whose projects the content rule reads.
	seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)
	fixture := fragmentCensusFixture{privateSeed: seed}
	// Each project names a Legion daemon, so its board and its daemon setting read their rows.
	for _, project := range []string{seedPublic, seedPrivate} {
		seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+project+"/legion/daemon",
			map[string]any{"state_url": "http://legion-" + strings.ToLower(project) + ".internal:8080"}, http.StatusOK)
	}

	priority := decodeBody[model.Priority](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/priorities",
		map[string]any{"title": "Ship the census"}, http.StatusCreated)).ID
	goingRun := createdID(t, seed.mustRequest(t, callerAlice, http.MethodPost, "/api/v1/going/requests",
		map[string]any{"words": "What do the runbook steps say?", "project": seedPublic}, http.StatusCreated))
	// Each project's objects name a session that acted in that project alone (seedSessionWork), so
	// alice, who sees PUB, sees PUB's session and not PRIV's (access.SessionClause).
	for _, owned := range []struct{ project, session string }{{seedPublic, sessionOfPublic}, {seedPrivate, sessionOfPrivate}} {
		project, session := owned.project, owned.session
		objects := censusObjects{
			documentObjects: seedDocumentObjects(t, seed, project), session: session, priority: priority, goingRun: goingRun,
		}
		path := "/api/v1/issues/" + objects.issue
		objects.block = decodeBody[[]struct {
			ID string `json:"id"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/artifacts/"+objects.document+"/blocks", nil, http.StatusOK))[0].ID
		// The project document's second version, an edit named as a version, which writes it at
		// once: the change set of versions 1 and 2 (censusQueries) then reads both.
		if revised := decodeBody[struct {
			Version *model.Version `json:"version"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/artifacts/"+objects.document+"/edits", map[string]any{
			"ops":     []map[string]string{{"op": "insert", "after": "end", "markdown": "The revised runbook steps of " + project + "."}},
			"summary": "Revise the runbook",
		}, http.StatusOK)).Version; revised == nil || revised.Number != 2 {
			t.Fatalf("carol's named edit of %s's runbook wrote %+v, want its version 2", project, revised)
		}
		objects.comment = createdID(t, seed.request(t, callerCarol, http.MethodPost, path+"/comments",
			map[string]any{"body": "The runbook comment of " + project + "."}))
		objects.ask = createdID(t, seed.request(t, callerCarol, http.MethodPost, path+"/asks",
			map[string]any{"question": "Which runbook does " + project + " keep?"}))
		objects.message = createdID(t, seed.request(t, callerCarol, http.MethodPost, path+"/messages",
			map[string]any{"body": "The runbook message of " + project + "."}))
		writer := map[string]privateCaller{seedPublic: callerCarolToken, seedPrivate: callerCarolPrivToken}[project]
		objects.handoff = createdID(t, seed.request(t, writer, http.MethodPost, path+"/handoffs/plan", map[string]any{
			"body": `{"plan":"the runbook of ` + project + `"}`, "actor": map[string]string{"kind": "session", "id": session},
		}))
		uploaded := multipartRequest(t, seed.handler, "/api/v1/agents/"+session+"/artifacts", map[string]string{"name": "shot.png"},
			"shot.png", "image/png", pngBytes("the runbook shot of "+project), seedCarol)
		if uploaded.Code != http.StatusCreated {
			t.Fatalf("carol uploads to %s's conversation: %d %s", session, uploaded.Code, uploaded.Body.String())
		}
		objects.conversationSlug = decodeBody[artifactUploadResponse](t, uploaded).Artifact.Slug
		broadcast := keyedRequest(t, seed.handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
			"body": "Status of " + project + "?", "delivery": "btw", "session_ids": []string{session},
		}, seedCarol, uuid.NewString())
		objects.broadcast = createdID(t, broadcast)
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": "Advance " + project + "'s census", "advances": []string{priority}, "force": true,
		}, http.StatusCreated)
		if err := seed.database.Pool.QueryRow(context.Background(), `
			select id::text from near_copy_checks where project_key = $1 order by created_at desc, id desc limit 1
		`, project).Scan(&objects.nearCopyCheck); err != nil {
			t.Fatalf("read the near-copy check of %s's create: %v", project, err)
		}
		objects.person = seedCarol
		if project == seedPublic {
			objects.person = seedAlice
			fixture.public = objects
		} else {
			fixture.private = objects
		}
	}

	// alice's own rows on PUB: a comment, an answer to its ask, and a direct message to its session.
	public := fixture.public
	createdID(t, seed.request(t, callerAlice, http.MethodPost, "/api/v1/issues/"+public.issue+"/comments",
		map[string]any{"body": "alice read the runbook."}))
	seed.mustRequest(t, callerAlice, http.MethodPost, "/api/v1/asks/"+public.ask+"/answer",
		map[string]any{"text": "The first runbook.", "expected_edited_at": nil}, http.StatusOK)
	createdID(t, postAgentMessage(t, seed.handler, public.session,
		map[string]any{"body": "alice asks about the runbook.", "delivery": "btw"}, seedAlice))

	ctx := context.Background()
	if _, err := delivery.PutSettings(ctx, seed.database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/deploy", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"release / release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("write the delivery settings: %v", err)
	}
	merged := time.Now().UTC().Add(-time.Hour)
	opened := merged.Add(-time.Hour)
	for number, project := range []string{seedPublic, seedPrivate} {
		repo := "acme/" + strings.ToLower(project) + "-app"
		if err := delivery.UpsertPullRequest(ctx, seed.database.Pool, delivery.DeliveryPullRequest{
			Repo: repo, Number: number + 1, Title: "runbook change of " + project, Author: "octocat",
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number+1), CreatedAt: &opened, MergedAt: &merged,
		}); err != nil {
			t.Fatalf("store %s#%d: %v", repo, number+1, err)
		}
	}
	// A deploy run of the deploy repository for each walk, which ships none of the pull requests
	// above: they are of other repositories.
	for index, objects := range []*censusObjects{&fixture.public, &fixture.private} {
		started := merged.Add(time.Duration(index+1) * 10 * time.Minute)
		run := delivery.DeliveryRun{
			Repo: "acme/deploy", RunID: int64(900 + index), Kind: delivery.DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("census%d", index),
			HeadCommitAt: started, StartedAt: started, CompletedAt: new(started.Add(5 * time.Minute)),
			Conclusion: new(delivery.DeliveryRunConclusionSuccess), URL: fmt.Sprintf("https://github.com/acme/deploy/actions/runs/%d", 900+index),
			HeadBranch: new("main"), Event: new("push"),
		}
		if err := delivery.UpsertRun(ctx, seed.database.Pool, run); err != nil {
			t.Fatalf("store deploy run %d: %v", run.RunID, err)
		}
		if err := delivery.UpsertRunJobs(ctx, seed.database.Pool, run.Repo, run.RunID, []delivery.DeliveryRunJob{{
			Repo: run.Repo, RunID: run.RunID, Name: "release / release", StartedAt: new(started), CompletedAt: new(started.Add(4 * time.Minute)),
			Conclusion: new(delivery.DeliveryJobConclusionSuccess),
		}}, true); err != nil {
			t.Fatalf("store the jobs of deploy run %d: %v", run.RunID, err)
		}
		objects.deliveryRun = strconv.FormatInt(run.RunID, 10)
	}
	endedCIPass(t, seed.database, seedServerProcess(t, seed.database, 0), time.Hour, "complete")

	awaitBackgroundWrites(t, seed.server)
	windowOwedOwners(t, seed.deps)
	processAllPending(t, seed.database, embedder)
	return fixture
}
