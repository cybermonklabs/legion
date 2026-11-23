// visual_shot_reads_test.go holds the server to what the browser suite's visual review relies on:
// the pages it draws read one corpus every row shares, and the reads write nothing a later row
// would draw. The rows themselves stop every write their pages send (packages/dispatch/e2e/
// visual-guard.ts). What a page cannot stop is a write the server makes while it answers a read,
// so this test sends each route those pages read (testdata/visual-shot-reads.json, which a visual
// row fails without) and reads every statement each one ran through the tracer
// TestTheReadOnlyListedRouteWritesOnlyItsProofAndCaches reads with (markedStatements, writesOf). A
// write to any table but the caches below fails the test, by the read and the table. It opens and
// closes a document's room as a page does too, and holds the room to writing nothing.
package api

import (
	"context"
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

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// shotReadCaches is every table a read of the visual review's pages may write while it answers,
// each with what makes it no write a page draws. A read writes nothing else: not a document
// version or an event (a settlement a room owed), not pull_request_heads or the attribution events
// beside it (GET /issues/{key}/results asking GitHub which pull request holds a commit), and not
// route_roster (the suite's server stores no copy of the listener's sessions).
var shotReadCaches = map[string]string{
	"text_trees":            "the display-tree cache a read with Dispatch-Trees: 1 fills, built from what Dispatch holds",
	"people":                "the signed-in person's own row, written once (identity.HeaderIdentity, the suite's dev sign-in)",
	"user_sessions":         "the sign-in's session row (the suite's dev sign-in)",
	"event_horizon":         "the event log's shared safe horizon, persisted at most once an interval",
	"search_term_totals":    "the related-item check's word statistics, rebuilt from what Dispatch holds",
	"search_term_stats":     "the related-item check's word statistics, rebuilt from what Dispatch holds",
	"search_term_queue":     "the related-item check's word statistics' queue, drained by the rebuild",
	"delivery_run_warnings": "the once-a-run log bookkeeping of a delivery read, which no page draws",
}

// visualShotReadsFile is the list of the routes the visual review's pages read, which a visual row
// matches every read of its page against.
var visualShotReadsFile = filepath.Join("testdata", "visual-shot-reads.json")

// visualShotReads reads the route patterns the list names.
func visualShotReads(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(visualShotReadsFile)
	if err != nil {
		t.Fatalf("read %s: %v", visualShotReadsFile, err)
	}
	var list struct {
		Reads []string `json:"reads"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("decode %s: %v", visualShotReadsFile, err)
	}
	if len(list.Reads) == 0 {
		t.Fatalf("%s names no read: the test would hold nothing", visualShotReadsFile)
	}
	return list.Reads
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

// shotWriteViolations names each write of statements, run while what answered, that is to no
// table of shotReadCaches, as "<what> wrote <table>: <statement>", and each write it cannot read
// a table for. It answers too whether a statement stored a display tree.
func shotWriteViolations(what string, statements []string) (violations []string, treeStored bool) {
	for _, statement := range statements {
		writes, readable := writesOf(statement)
		if !readable {
			violations = append(violations, what+" ran a write this test cannot read a table for: "+statementLine(statement))
		}
		for _, write := range writes {
			verb, table, _ := strings.Cut(write, " ")
			if _, cached := shotReadCaches[table]; !cached {
				violations = append(violations, what+" wrote "+table+": "+statementLine(statement))
			}
			treeStored = treeStored || (verb == "insert" && table == "text_trees")
		}
	}
	return violations, treeStored
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
		"/api/v1/issues/{key}/decisions":              "/api/v1/issues/" + c.issue + "/decisions",
		"/api/v1/issues/{key}/events":                 "/api/v1/issues/" + c.issue + "/events",
		"/api/v1/issues/{key}/references":             "/api/v1/issues/" + c.issue + "/references?versions=latest",
		"/api/v1/issues/{key}/results":                "/api/v1/issues/" + c.issue + "/results",
		"/api/v1/issues/{key}/subscribers":            "/api/v1/issues/" + c.issue + "/subscribers",
		"/api/v1/issues/{key}/summary":                "/api/v1/issues/" + c.issue + "/summary",
		"/api/v1/me/agent-tokens":                     "/api/v1/me/agent-tokens",
		"/api/v1/me/agents/state":                     "/api/v1/me/agents/state",
		"/api/v1/me/answers":                          "/api/v1/me/answers",
		"/api/v1/me/documents/{id}/read":              "/api/v1/me/documents/" + c.spec + "/read",
		"/api/v1/me/replies":                          "/api/v1/me/replies",
		"/api/v1/me/state":                            "/api/v1/me/state",
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
	}
}

// shotRead reads target as the dashboard does, the person signed in and the display trees asked
// for, on a request the tracer records, and waits out the work it detached before it answers, so
// the statements it took are this read's alone.
func shotRead(t *testing.T, handler http.Handler, served *server, statements *shotStatements, target string) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	statements.take()
	ctx := context.WithValue(context.Background(), countedRead{}, true)
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set(displayTreesHeader, "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	served.waitBackground()
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

// buildShotCorpus writes, through the API alone, the corpus the reads are sent at.
func buildShotCorpus(t *testing.T, handler http.Handler) shotCorpus {
	t.Helper()
	full := buildFullIssueRead(t, handler)
	corpus := shotCorpus{project: "BATCH", issue: full.key, ask: full.openAsk}
	corpus.spec = decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+full.key, nil, "alice")).PrimaryArtifactID
	if corpus.spec == "" {
		t.Fatalf("%s holds no spec", full.key)
	}

	sent := postBroadcast(t, handler, map[string]any{
		"body": "Hold the **deploy** and report.", "delivery": "steer", "session_ids": []string{"planner"},
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

	runbook := createProjectDocument(t, handler, corpus.project, "Runbook.md", "# Runbook\n\nRestart the **listener** first.\n")
	corpus.runbook, corpus.runbookSlug = runbook.ID, runbook.Slug
	return corpus
}

// TestTheVisualShotReadsWriteOnlyTheirCaches sends every route the visual review's pages read
// (testdata/visual-shot-reads.json) at a corpus of the shape the visual seed writes, as the
// dashboard sends it - signed in, with Dispatch-Trees: 1, on a server whose listener roster is
// read uncached as the suite's is - and holds every write each read makes, and every write the
// work it detaches makes, to shotReadCaches. It then opens the spec's room as a page does and
// closes it, once the service owes no settlement, and holds the room to writing nothing.
//
// Its positive controls: every read answers 200, the reads store a display tree between them,
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
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{
		store:                  traced,
		envoyURL:               listener.URL,
		agentSecretsURL:        broker.server.URL,
		listenerRosterUncached: true,
	})
	corpus := buildShotCorpus(t, handler)
	paths := shotReadPaths(corpus)

	listed := visualShotReads(t)
	for _, pattern := range listed {
		if _, held := paths[pattern]; !held {
			t.Errorf("%s names %s, which this test sends no read to: add its path to shotReadPaths", visualShotReadsFile, pattern)
		}
	}
	for pattern := range paths {
		if !slices.Contains(listed, pattern) {
			t.Errorf("shotReadPaths sends a read to %s, which %s does not name", pattern, visualShotReadsFile)
		}
	}
	nothingOwed(t, database)
	// Every display tree the corpus's writes stored goes, so the reads find the cache cold, as a
	// seed written by psql leaves it, and each read stores the trees it builds.
	if _, err := database.Pool.Exec(context.Background(), `delete from text_trees`); err != nil {
		t.Fatalf("empty the display-tree cache: %v", err)
	}

	var treeStored bool
	for _, pattern := range listed {
		target, held := paths[pattern]
		if !held {
			continue
		}
		response, ran := shotRead(t, handler, served, statements, target)
		if response.Code != http.StatusOK {
			t.Errorf("GET %s: status=%d body=%s, want 200: the read must answer for its statements to be its handler's", target, response.Code, response.Body.String())
			continue
		}
		violations, stored := shotWriteViolations("GET "+pattern, ran)
		for _, violation := range violations {
			t.Error(violation)
		}
		treeStored = treeStored || stored
	}
	if !treeStored {
		t.Error("no read stored a display tree: the tracer saw none of the detached writes it exists to see")
	}

	// The planted read: a write inside a listed read is named by the read and the table.
	planted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := traced.Pool.Exec(r.Context(), `update projects set name = name where key = $1`, corpus.project); err != nil {
			t.Errorf("plant a write in the read: %v", err)
		}
		handler.ServeHTTP(w, r)
	})
	if _, ran := shotRead(t, planted, served, statements, "/api/v1/projects"); !slices.ContainsFunc(
		violationsOnly(shotWriteViolations("GET /api/v1/projects", ran)),
		func(violation string) bool {
			return strings.HasPrefix(violation, "GET /api/v1/projects wrote projects: ")
		},
	) {
		t.Errorf("a write planted in GET /api/v1/projects was not named by the read and its table among %d statements: %v", len(ran), ran)
	}

	// The room: a page opens the spec and leaves it, and the room writes nothing.
	// The room is served as a page's socket reaches it, signed in, on the traced pool, by a
	// document service of the test's own: the test server's names no identity for its sockets.
	documents := docs.New(docs.Deps{Store: traced, Identity: headerIdentity(traced), Settle: 20 * time.Millisecond})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	statements.watch()
	page := connectBrowserPeer(t, documents, corpus.spec)
	page.closeAndWait(t)
	served.waitBackground()
	nothingOwed(t, database)
	watched := statements.stop()
	if len(watched) == 0 {
		t.Error("the tracer saw no statement while the room loaded: it did not see the room")
	}
	violations, _ := shotWriteViolations("the spec's room", watched)
	for _, violation := range violations {
		t.Error(violation)
	}

	// The planted edit: a person's keystroke in the room is named by the room and its table.
	statements.watch()
	page.connect(t)
	page.appendParagraph(t, "A keystroke the visual guard would have stopped.")
	page.closeAndWait(t)
	served.waitBackground()
	nothingOwed(t, database)
	edited := statements.stop()
	if !slices.ContainsFunc(violationsOnly(shotWriteViolations("the spec's room", edited)), func(violation string) bool {
		return strings.HasPrefix(violation, "the spec's room wrote doc_updates: ")
	}) {
		t.Errorf("a person's edit in the room was not named by the room and doc_updates among %d statements", len(edited))
	}
}

// violationsOnly answers a check's violations, for a control that reads only them.
func violationsOnly(violations []string, _ bool) []string {
	return violations
}
