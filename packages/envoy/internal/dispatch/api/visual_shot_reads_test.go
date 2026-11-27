// visual_shot_reads_test.go holds the server to what the browser suite's visual review relies on:
// the pages it draws read one corpus every row shares, and the reads write nothing a later row
// would draw. The rows themselves stop every write their pages send (packages/dispatch/e2e/
// visual-guard.ts). What a page cannot stop is a write the server makes while it answers, so this
// test sends each API read, each sign-in read and each live view those pages make
// (testdata/visual-shot-reads.json, which a visual row fails without), and reads every statement
// each one ran through read_only_token_test.go's tracer of a request's statements
// (markedStatements, writesOf). A write to any table its channel does not allow fails the test, by
// the read or view and the table. It opens and closes a document's room as a page does too, and
// holds the room to writing nothing.
package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// shotWriteChannels are the three ways a visual row's page reaches the server without writing
// itself - a read of the API, a view of the live socket, a document's room - each with every table
// the server may write while it answers that way, and what makes that write no change a later
// shot draws. Each table is one this test sees written on that channel; a write to any other
// table fails it. None of them holds a document version or an event (a settlement a room owed),
// pull_request_heads or the attribution events beside it (GET /issues/{key}/results asking GitHub
// which pull request holds a commit), or route_roster (the suite's server stores no copy of the
// listener's sessions).
var shotWriteChannels = map[string]map[string]string{
	shotReadChannel: {
		"text_trees": "the display-tree cache a read with Dispatch-Trees: 1 fills, built from what Dispatch holds",
	},
	shotLiveChannel: {
		"event_horizon":         "the event log's shared safe horizon the event feed reads up to, persisted at most once an interval: a cursor, which no page draws",
		"delivery_run_warnings": "the once-a-run record that a deploy run held no production job, which decides which process logs it: no page draws it",
	},
	shotAuthChannel: {},
	shotRoomChannel: {},
}

const (
	shotReadChannel = "read"
	shotLiveChannel = "live view"
	shotRoomChannel = "room"
	shotAuthChannel = "sign-in read"
)

// visualShotReadsFile is the list of the routes the visual review's pages read, which a visual row
// matches every read of its page against.
var visualShotReadsFile = filepath.Join("testdata", "visual-shot-reads.json")

// visualShotList is the list's three parts: the route patterns of the API reads the shot pages make,
// the kinds of the live views they subscribe on GET /ws/live, and the sign-in routes they read
// (the routes package's /auth routes, which no API route table holds).
type visualShotList struct {
	Reads []string `json:"reads"`
	Live  []string `json:"live"`
	Auth  []string `json:"auth"`
}

// visualShotReads reads the list.
func visualShotReads(t *testing.T) visualShotList {
	t.Helper()
	data, err := os.ReadFile(visualShotReadsFile)
	if err != nil {
		t.Fatalf("read %s: %v", visualShotReadsFile, err)
	}
	var list visualShotList
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("decode %s: %v", visualShotReadsFile, err)
	}
	if len(list.Reads) == 0 || len(list.Live) == 0 || len(list.Auth) == 0 {
		t.Fatalf("%s names %d reads, %d live views and %d sign-in reads: the test would hold nothing of a part it names none of", visualShotReadsFile, len(list.Reads), len(list.Live), len(list.Auth))
	}
	return list
}

// shotStatements is markedStatements, which records the statements a request the test marked runs
// and the work it detaches, with a watch beside it that records every statement the pool runs
// while it is on: a document room loads and settles on contexts of the server's own, which no
// request marks.
type shotStatements struct {
	markedStatements
	watching atomic.Bool
	mu       sync.Mutex
	watched  []string
}

func (s *shotStatements) observe(statement string) {
	if !s.watching.Load() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watched = append(s.watched, statement)
}

func (s *shotStatements) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.observe(data.SQL)
	return s.markedStatements.TraceQueryStart(ctx, conn, data)
}

func (s *shotStatements) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	s.observe(data.SQL)
	s.markedStatements.TraceBatchQuery(ctx, conn, data)
}

func (s *shotStatements) TraceCopyFromStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceCopyFromStartData) context.Context {
	s.observe("insert into " + data.TableName.Sanitize() + " (copy from)")
	return s.markedStatements.TraceCopyFromStart(ctx, conn, data)
}

// watch starts recording every statement, and stop ends it and answers what it recorded.
func (s *shotStatements) watch() {
	s.mu.Lock()
	s.watched = nil
	s.mu.Unlock()
	s.watching.Store(true)
}

func (s *shotStatements) stop() []string {
	s.watching.Store(false)
	s.mu.Lock()
	defer s.mu.Unlock()
	watched := s.watched
	s.watched = nil
	return watched
}

// shotWriteViolations names each write of statements, run while what answered on channel, that is
// to no table shotWriteChannels allows that channel, as "<what> wrote <table>: <statement>", and
// each write it cannot read a table for. It answers too the tables it saw written.
func shotWriteViolations(channel, what string, statements []string) (violations []string, written map[string]bool) {
	allowed := shotWriteChannels[channel]
	written = map[string]bool{}
	for _, statement := range statements {
		writes, readable := writesOf(statement)
		if !readable {
			violations = append(violations, what+" ran a write this test cannot read a table for: "+statementLine(statement))
		}
		for _, write := range writes {
			_, table, _ := strings.Cut(write, " ")
			written[table] = true
			if _, cached := allowed[table]; !cached {
				violations = append(violations, what+" wrote "+table+": "+statementLine(statement))
			}
		}
	}
	return violations, written
}

func statementLine(statement string) string {
	return strings.Join(strings.Fields(statement), " ")
}

// shotCorpus is what the reads below are sent at: an issue holding one of everything its read
// answers (buildFullIssueRead), its spec, an open ask, a broadcast, a priority and a project
// document, in the shape the visual seed writes them.
type shotCorpus struct {
	project, issue, spec, ask, broadcast, priority string
	// runbook is the project document's id and runbookSlug its slug: the document reads of a
	// project document (its asks, comments and subscribers) take the id, and an issue's
	// document is refused them (ARTIFACT_LINKED), as the visual review's project-document page
	// reads them.
	runbook, runbookSlug string
}

// shotReadPaths is the path this test sends each route of the list at, in its corpus, with the
// query the dashboard sends it with where the route reads one.
func shotReadPaths(c shotCorpus) map[string]string {
	// The person page of the signed-in person, as the visual seed's person row opens it.
	return map[string]string{
		"/api/v1/agents":                              "/api/v1/agents",
		"/api/v1/agents/{session_id}/messages":        "/api/v1/agents/planner/messages",
		"/api/v1/artifacts/{id}":                      "/api/v1/artifacts/" + c.spec,
		"/api/v1/artifacts/{id}/asks":                 "/api/v1/artifacts/" + c.runbook + "/asks",
		"/api/v1/artifacts/{id}/blocks":               "/api/v1/artifacts/" + c.spec + "/blocks",
		"/api/v1/artifacts/{id}/comments":             "/api/v1/artifacts/" + c.runbook + "/comments",
		"/api/v1/artifacts/{id}/subscribers":          "/api/v1/artifacts/" + c.runbook + "/subscribers",
		"/api/v1/artifacts/{id}/text":                 "/api/v1/artifacts/" + c.spec + "/text",
		"/api/v1/asks/{id}":                           "/api/v1/asks/" + c.ask,
		"/api/v1/broadcasts":                          "/api/v1/broadcasts",
		"/api/v1/broadcasts/{id}":                     "/api/v1/broadcasts/" + c.broadcast,
		"/api/v1/credential-grants":                   "/api/v1/credential-grants?approver=me",
		"/api/v1/credential-requests":                 "/api/v1/credential-requests?approver=me",
		"/api/v1/inbox":                               "/api/v1/inbox",
		"/api/v1/issues":                              "/api/v1/issues",
		"/api/v1/issues/{key}":                        "/api/v1/issues/" + c.issue,
		"/api/v1/issues/{key}/artifacts":              "/api/v1/issues/" + c.issue + "/artifacts",
		"/api/v1/issues/{key}/asks":                   "/api/v1/issues/" + c.issue + "/asks?thread=1",
		"/api/v1/issues/{key}/comments":               "/api/v1/issues/" + c.issue + "/comments",
		"/api/v1/issues/{key}/coverage":               "/api/v1/issues/" + c.issue + "/coverage",
		"/api/v1/issues/{key}/events":                 "/api/v1/issues/" + c.issue + "/events",
		"/api/v1/issues/{key}/references":             "/api/v1/issues/" + c.issue + "/references?versions=latest",
		"/api/v1/issues/{key}/results":                "/api/v1/issues/" + c.issue + "/results",
		"/api/v1/issues/{key}/subscribers":            "/api/v1/issues/" + c.issue + "/subscribers",
		"/api/v1/issues/{key}/summary":                "/api/v1/issues/" + c.issue + "/summary",
		"/api/v1/issues/{key}/tree":                   "/api/v1/issues/" + c.issue + "/tree?view=decisions",
		"/api/v1/me/agent-tokens":                     "/api/v1/me/agent-tokens",
		"/api/v1/me/agents/state":                     "/api/v1/me/agents/state",
		"/api/v1/me/answers":                          "/api/v1/me/answers",
		"/api/v1/me/documents/{id}/read":              "/api/v1/me/documents/" + c.spec + "/read",
		"/api/v1/me/replies":                          "/api/v1/me/replies",
		"/api/v1/me/state":                            "/api/v1/me/state",
		"/api/v1/people/{login}/nodes":                "/api/v1/people/" + shotPerson + "/nodes?limit=50&offset=0",
		"/api/v1/priorities":                          "/api/v1/priorities",
		"/api/v1/priorities/{id}/history":             "/api/v1/priorities/" + c.priority + "/history",
		"/api/v1/projects":                            "/api/v1/projects",
		"/api/v1/projects/{key}/architecture-sources": "/api/v1/projects/" + c.project + "/architecture-sources",
		"/api/v1/projects/{key}/artifacts":            "/api/v1/projects/" + c.project + "/artifacts",
		"/api/v1/projects/{key}/artifacts/{slug}":     "/api/v1/projects/" + c.project + "/artifacts/" + c.runbookSlug,
		"/api/v1/projects/{key}/legion/daemon":        "/api/v1/projects/" + c.project + "/legion/daemon",
		"/api/v1/references":                          "/api/v1/references?to=dispatch://" + c.issue,
		"/api/v1/settings/architecture-sources":       "/api/v1/settings/architecture-sources",
		"/api/v1/settings/delivery":                   "/api/v1/settings/delivery",
		"/api/v1/settings/project-default-assignees":  "/api/v1/settings/project-default-assignees",
		"/api/v1/signins":                             "/api/v1/signins",
		"/api/v1/whoami":                              "/api/v1/whoami",
	}
}

// shotRead reads target as the dashboard does, the person signed in with a session cookie as the
// suite's dev sign-in signs in and the display trees asked for, on a request the tracer records, and waits out the work it detached before it answers, so
// the statements it took are this read's alone. It waits for the detached work alone: the server's
// whole background wait (waitBackground) is its shutdown's, after which it closes every live socket
// that opens with 1012.
func shotRead(t *testing.T, handler http.Handler, served *server, statements *shotStatements, cookie *http.Cookie, target string) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	statements.take()
	ctx := context.WithValue(context.Background(), countedRead{}, true)
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	request.AddCookie(cookie)
	request.Header.Set(displayTreesHeader, "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	served.detached.wait()
	return response, statements.take()
}

// nothingOwed waits until the document service owes no document a settlement, as the visual seed
// waits before any row draws, and fails naming what it still owes.
func nothingOwed(t *testing.T, database *store.Store) {
	t.Helper()
	var owed []string
	read := func() bool {
		rows, err := database.Pool.Query(context.Background(), `select artifact_id::text from doc_settlements_pending order by 1`)
		if err != nil {
			t.Fatalf("read the settlements owed: %v", err)
		}
		owed, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the settlements owed: %v", err)
		}
		return len(owed) == 0
	}
	if !testwait.Holds(10*time.Second, 20*time.Millisecond, read) {
		t.Fatalf("the document service still owes %s a settlement after 10s", strings.Join(owed, ", "))
	}
}

// shotPerson is the person the shot pages are drawn for, signed in with a session cookie.
const shotPerson = "alice@d.example"

// shotSignIn makes shotPerson somebody who has signed in, as the suite's dev sign-in does - a
// session and a people row, written on database, untraced, since they are the sign-in's and no
// read's - and answers the session cookie, which every read, socket and room of the test presents,
// and the identity that resolves it. The identity reads its sessions on traced, the pool the
// tracer watches, so what a cookie's resolution runs on any read is that read's. The suite's
// server resolves a cookie (identity.CookieIdentity), which writes nothing on a request; the
// package's header identity records the person it names on every one, which would be no write of
// a read's.
func shotSignIn(t *testing.T, database, traced *store.Store) (identity.CookieIdentity, *http.Cookie) {
	t.Helper()
	generation, err := store.NewPgSessionStore(database.Pool).EnsureSession(context.Background(), shotPerson)
	if err != nil {
		t.Fatalf("establish %s's session: %v", shotPerson, err)
	}
	seedPeople(t, database, shotPerson)
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie(shotPerson, generation, testSigningKey, true))
	if err != nil {
		t.Fatalf("parse the session cookie: %v", err)
	}
	return identity.CookieIdentity{SigningKey: testSigningKey, Sessions: store.NewPgSessionStore(traced.Pool)}, cookie
}

// asShotPerson is handler for the corpus's own writes, which the package's helpers send as the
// person "alice" names (X-Dispatch-User): each is sent with the session cookie instead, since the
// server resolves cookies alone. A request naming anybody else fails the test.
func asShotPerson(t *testing.T, handler http.Handler, cookie *http.Cookie) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch login := r.Header.Get("X-Dispatch-User"); login {
		case "":
		case "alice":
			r.Header.Del("X-Dispatch-User")
			r.AddCookie(cookie)
		default:
			t.Errorf("the corpus wrote as %s, whom the test signs in no session for", login)
		}
		handler.ServeHTTP(w, r)
	})
}

// buildShotCorpus writes, through the API alone, the corpus the reads are sent at. The broadcast's
// body and the runbook's prose carry a nonce of the run, as the read drive's prose document does
// (read_class_drive_seed_test.go): pmdoc's display cache is the process's, and a tree it already
// holds is neither asked of the table nor stored, so under -count above 1 a corpus of fixed texts
// would leave the reads no tree to store.
func buildShotCorpus(t *testing.T, handler http.Handler) shotCorpus {
	t.Helper()
	nonce := rand.Text()
	full := buildFullIssueRead(t, handler)
	corpus := shotCorpus{project: "BATCH", issue: full.key, ask: full.openAsk}
	corpus.spec = decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+full.key, nil, "alice")).PrimaryArtifactID
	if corpus.spec == "" {
		t.Fatalf("%s holds no spec", full.key)
	}

	sent := postBroadcast(t, handler, map[string]any{
		"body": "Hold the **deploy** and report, run " + nonce + ".", "delivery": "steer", "session_ids": []string{"planner"},
	}, "alice")
	if sent.Code != http.StatusCreated {
		t.Fatalf("send the broadcast: status=%d body=%s", sent.Code, sent.Body.String())
	}
	corpus.broadcast = decodeBody[broadcastResponse](t, sent).ID

	priority := dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities", map[string]any{
		"title": "Local testing is one command",
	}, "alice")
	if priority.Code != http.StatusCreated && priority.Code != http.StatusOK {
		t.Fatalf("write the priority: status=%d body=%s", priority.Code, priority.Body.String())
	}
	corpus.priority = decodeBody[struct {
		ID string `json:"id"`
	}](t, priority).ID

	runbook := createProjectDocument(t, handler, corpus.project, "Runbook.md", "# Runbook\n\nRestart the **listener** first, run "+nonce+".\n")
	corpus.runbook, corpus.runbookSlug = runbook.ID, runbook.Slug
	return corpus
}

// shotLiveViews is the view this test subscribes for each live view kind the list names, over
// its pipeline fixture, in the shape the shot pages send it: the Delivery page's timeline and
// measures over the custom window its URL names and no facet, the Pipeline page pinned to an at,
// and the event feed every page of the shell subscribes, from its start.
func shotLiveViews() map[string]LiveView {
	from, to := pipelineFixtureTime(26, 0, 0), pipelineFixtureTime(33, 0, 0)
	at := pipelineFixtureTime(32, 12, 0)
	return map[string]LiveView{
		deliveryLiveTimeline: {DeliveryLiveView: timelineRange(from, to, "")},
		deliveryLiveMeasures: {DeliveryLiveView: measuresRange(from, to, "")},
		deliveryLivePipeline: {DeliveryLiveView: livePipelineView(&at)},
		liveEventsView:       eventsView(),
	}
}

// shotAuthReads is, for each sign-in route the list names, what the server runs when a page reads
// it: GET /auth/whoami (routes.authWhoami, internal/dispatch/routes/router.go) resolves the caller
// through the deployment's identity, a session cookie here as in the suite, and answers the event
// feed's head (Background.EventHead, which is eventFeedHead). The routes package mounts the route
// over this package's server and is no import of it, so the test runs those two calls themselves.
func shotAuthReads(served *server, cookies identity.CookieIdentity, cookie *http.Cookie) map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		"/auth/whoami": func(ctx context.Context) error {
			request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/auth/whoami", nil)
			request.AddCookie(cookie)
			if _, err := cookies.Login(request); err != nil {
				return err
			}
			_, err := served.eventFeedHead(ctx)
			return err
		},
	}
}

// liveAnswer reads client's frames until sub's first answer - a delivery view's snapshot, the event
// feed's start - and fails on a refusal of sub. Each frame is waited for until the test binary's
// deadline less testwait's margin (socketClient.next), so an answer a loaded machine sends late is
// still read, and one that never comes fails the test by name.
func liveAnswer(t *testing.T, client *socketClient, sub string) {
	t.Helper()
	for {
		switch frame := client.next(t, "the first answer of "+sub).(type) {
		case DeliveryLiveSnapshot:
			if frame.Sub == sub {
				return
			}
		case LiveEventsStart:
			if frame.Sub == sub {
				return
			}
		case LiveError:
			if frame.Sub == sub || frame.Sub == "" {
				t.Fatalf("the live view %s was refused: %s %s", sub, frame.Code, frame.Message)
			}
		}
	}
}

// TestTheVisualShotReadsWriteOnlyTheirCaches holds what the server writes while it answers the
// visual review's pages, on each way they reach it, to the tables shotWriteChannels allows that
// way. It sends every route the list names (testdata/visual-shot-reads.json) at a corpus of the
// shape the visual seed writes, as the dashboard sends it - signed in, with Dispatch-Trees: 1, on a
// server whose listener roster is read uncached as the suite's is - and reads every statement each
// read runs and every statement the work it detaches runs. It subscribes each live view kind the
// list names on GET /ws/live, one at a time over the pipeline fixture, and reads every statement
// the pool runs from the subscribe to the view's first answer. It opens the spec's room as a page
// does and closes it, once the service owes no settlement, and reads every statement the pool runs
// meanwhile.
//
// Its positive controls: every read answers 200 and the reads store a display tree between them;
// every live view answers, and the delivery views read the deploy runs; the room's load is seen;
// and two planted writes are named - a write run inside one listed read, by that read and its
// table, and a person's edit in the room, by the room and the table it appends to - so a tracer
// that saw nothing, or a check that names nothing, cannot pass.
func TestTheVisualShotReadsWriteOnlyTheirCaches(t *testing.T) {
	database := storetest.Open(t)
	statements := &shotStatements{}
	traced := storetest.TracedStore(t, database, statements)
	// The listener answers the subscriber reads too: no session holds an interest.
	listener, _ := newBroadcastListener(t, broadcastSessions, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/interests/" {
			return false
		}
		WriteJSON(w, http.StatusOK, []map[string]any{})
		return true
	})
	broker := newFakeBrokerRig(t)
	cookies, cookie := shotSignIn(t, database, traced)
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{
		store:                  traced,
		envoyURL:               listener.URL,
		agentSecretsURL:        broker.server.URL,
		listenerRosterUncached: true,
		identity:               cookies,
	})
	corpus := buildShotCorpus(t, asShotPerson(t, handler, cookie))
	paths := shotReadPaths(corpus)
	views := shotLiveViews()

	listed := visualShotReads(t)
	for _, pattern := range listed.Reads {
		if _, held := paths[pattern]; !held {
			t.Errorf("%s names the read %s, which this test sends no read to: add its path to shotReadPaths", visualShotReadsFile, pattern)
		}
	}
	for pattern := range paths {
		if !slices.Contains(listed.Reads, pattern) {
			t.Errorf("shotReadPaths sends a read to %s, which %s does not name", pattern, visualShotReadsFile)
		}
	}
	for _, kind := range listed.Live {
		if _, held := views[kind]; !held {
			t.Errorf("%s names the live view %s, which this test subscribes no view of: add it to shotLiveViews", visualShotReadsFile, kind)
		}
	}
	for kind := range views {
		if !slices.Contains(listed.Live, kind) {
			t.Errorf("shotLiveViews subscribes a %s view, which %s does not name", kind, visualShotReadsFile)
		}
	}
	authReads := shotAuthReads(served, cookies, cookie)
	for _, route := range listed.Auth {
		if _, held := authReads[route]; !held {
			t.Errorf("%s names the sign-in read %s, which this test runs nothing of: add it to shotAuthReads", visualShotReadsFile, route)
		}
	}
	for route := range authReads {
		if !slices.Contains(listed.Auth, route) {
			t.Errorf("shotAuthReads runs the sign-in read %s, which %s does not name", route, visualShotReadsFile)
		}
	}
	nothingOwed(t, database)
	// Every display tree the corpus's writes stored goes, so the reads find the cache cold, as a
	// seed written by psql leaves it, and each read stores the trees it builds.
	if _, err := database.Pool.Exec(context.Background(), `delete from text_trees`); err != nil {
		t.Fatalf("empty the display-tree cache: %v", err)
	}

	written := map[string]map[string]bool{}
	record := func(channel string, seen map[string]bool) {
		if written[channel] == nil {
			written[channel] = map[string]bool{}
		}
		for table := range seen {
			written[channel][table] = true
		}
	}
	// The sign-in reads first, as a page reads GET /auth/whoami before anything else, on a server
	// whose event horizon nothing has built yet; each with every statement the pool runs while it
	// answers.
	for _, route := range listed.Auth {
		read, held := authReads[route]
		if !held {
			continue
		}
		statements.watch()
		if err := read(context.Background()); err != nil {
			t.Errorf("the sign-in read %s: %v", route, err)
		}
		served.detached.wait()
		ran := statements.stop()
		// Both halves of the read are seen: the cookie's resolution reads the person's session
		// generation, and the feed's head marks the event log (events.Horizon.Mark), so a write by
		// either is caught too.
		if !slices.ContainsFunc(ran, func(statement string) bool { return strings.Contains(statement, "from user_sessions") }) {
			t.Errorf("the tracer saw no read of user_sessions among the %d statements %s ran: the cookie's resolution ran on a pool it does not watch", len(ran), route)
		}
		if !slices.ContainsFunc(ran, func(statement string) bool { return strings.Contains(statement, "pg_current_xact_id()") }) {
			t.Errorf("the tracer saw no mark of the event log among the %d statements %s ran: it did not see the feed's head", len(ran), route)
		}
		violations, seen := shotWriteViolations(shotAuthChannel, "GET "+route, ran)
		for _, violation := range violations {
			t.Error(violation)
		}
		record(shotAuthChannel, seen)
	}

	for _, pattern := range listed.Reads {
		target, held := paths[pattern]
		if !held {
			continue
		}
		response, ran := shotRead(t, handler, served, statements, cookie, target)
		if response.Code != http.StatusOK {
			t.Errorf("GET %s: status=%d body=%s, want 200: the read must answer for its statements to be its handler's", target, response.Code, response.Body.String())
			continue
		}
		violations, seen := shotWriteViolations(shotReadChannel, "GET "+pattern, ran)
		for _, violation := range violations {
			t.Error(violation)
		}
		record(shotReadChannel, seen)
	}
	if !written[shotReadChannel]["text_trees"] {
		t.Error("no read stored a display tree: the tracer saw none of the detached writes it exists to see")
	}

	// The planted read: a write inside a listed read is named by the read and the table.
	planted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := traced.Pool.Exec(r.Context(), `update projects set name = name where key = $1`, corpus.project); err != nil {
			t.Errorf("plant a write in the read: %v", err)
		}
		handler.ServeHTTP(w, r)
	})
	if _, ran := shotRead(t, planted, served, statements, cookie, "/api/v1/projects"); !slices.ContainsFunc(
		violationsOnly(shotWriteViolations(shotReadChannel, "GET /api/v1/projects", ran)),
		func(violation string) bool {
			return strings.HasPrefix(violation, "GET /api/v1/projects wrote projects: ")
		},
	) {
		t.Errorf("a write planted in GET /api/v1/projects was not named by the read and its table among %d statements: %v", len(ran), ran)
	}

	// The live views, each on the socket a page opens, over the deploy runs and the merged pull
	// request the Delivery and Pipeline pages draw, written beside the tracer so the fixture's own
	// writes are no view's.
	seedPipelineFixture(t, database, pipelineSettings())
	upsertPR(t, database, hubPR(1, pipelineFixtureTime(32, 10, 0)))
	socketServer := httptest.NewServer(handler)
	t.Cleanup(socketServer.Close)
	client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(socketServer.URL, "http")+"/ws/live", http.Header{"Cookie": {cookie.String()}}).servedBy(database).read()
	runsRead := false
	for _, kind := range listed.Live {
		view, held := views[kind]
		if !held {
			continue
		}
		statements.watch()
		client.send(t, LiveSubscribe{Type: "subscribe", Sub: kind, View: view})
		liveAnswer(t, client, kind)
		served.detached.wait()
		ran := statements.stop()
		violations, seen := shotWriteViolations(shotLiveChannel, "the live view "+kind, ran)
		for _, violation := range violations {
			t.Error(violation)
		}
		record(shotLiveChannel, seen)
		runsRead = runsRead || slices.ContainsFunc(ran, func(statement string) bool {
			return strings.Contains(statement, "delivery_runs")
		})
		client.send(t, LiveUnsubscribe{Type: "unsubscribe", Sub: kind})
	}
	if !runsRead {
		t.Error("no live view read delivery_runs: the tracer saw none of the delivery views' work")
	}

	// The room: a page opens the spec and leaves it, and the room writes nothing.
	// The room is served as a page's socket reaches it, signed in with the cookie, on the traced
	// pool, by a document service of the test's own: the test server's names no identity for its
	// sockets.
	documents := docs.New(docs.Deps{Store: traced, Identity: cookies, Settle: 20 * time.Millisecond})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	sockets := &servedSockets{finished: make(map[string]chan struct{})}
	roomServer := httptest.NewServer(sockets.serve(documents.ServeHTTP))
	t.Cleanup(roomServer.Close)
	// The page binds the fragment the artifact read answers for the spec, as the page does: the
	// spec's own fragment of its issue's room, since an issue is born converted (connectBrowserPeer).
	fragment := artifactFragment(t, asShotPerson(t, handler, cookie), corpus.spec)
	statements.watch()
	page := &syncedPeer{
		wsURL: "ws" + strings.TrimPrefix(roomServer.URL, "http") + "/ws/doc/" + corpus.spec, sockets: sockets,
		headers: http.Header{"Cookie": {cookie.String()}}, artifactID: corpus.spec, fragment: fragment, doc: crdt.New(),
	}
	page.connect(t)
	page.closeAndWait(t)
	served.detached.wait()
	nothingOwed(t, database)
	watched := statements.stop()
	if len(watched) == 0 {
		t.Error("the tracer saw no statement while the room loaded: it did not see the room")
	}
	violations, seen := shotWriteViolations(shotRoomChannel, "the spec's room", watched)
	for _, violation := range violations {
		t.Error(violation)
	}
	record(shotRoomChannel, seen)

	// The planted edit: a person's keystroke in the room is named by the room and its table.
	statements.watch()
	page.connect(t)
	page.appendParagraph(t, "A keystroke the visual guard would have stopped.")
	page.closeAndWait(t)
	served.detached.wait()
	nothingOwed(t, database)
	edited := statements.stop()
	if !slices.ContainsFunc(violationsOnly(shotWriteViolations(shotRoomChannel, "the spec's room", edited)), func(violation string) bool {
		return strings.HasPrefix(violation, "the spec's room wrote doc_updates: ")
	}) {
		t.Errorf("a person's edit in the room was not named by the room and doc_updates among %d statements", len(edited))
	}

	// Each allowance is one this test sees written on its channel: an allowance nothing here
	// writes is one nothing holds, and goes.
	for channel, allowed := range shotWriteChannels {
		for table := range allowed {
			if !written[channel][table] {
				t.Errorf("shotWriteChannels allows the %s channel %s, which no %s of this test wrote: drop the allowance", channel, table, channel)
			}
		}
	}
	t.Logf("tables written, by channel: %v", written)
}

// violationsOnly answers a check's violations, for a control that reads only them.
func violationsOnly(violations []string, _ map[string]bool) []string {
	return violations
}
