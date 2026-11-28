package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// logBuffer is a log sink the server's goroutines may write while the test reads it.
type logBuffer = logtest.Buffer

// captureLog sends the default logger's lines to a buffer for the test's lifetime, as the
// deployment's own logger writes them: a text handler, the one cmd/dispatch installs.
func captureLog(t *testing.T) *logBuffer {
	t.Helper()
	logged := &logBuffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	return logged
}

// accessLines are the access-log lines a buffer holds, in order.
func accessLines(t *testing.T, logged *logBuffer) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, `msg="`+accesslog.Message+`"`) {
			lines = append(lines, line)
		}
	}
	return lines
}

// A person's read writes one line naming the route, the person, the Host it was sent to as the
// client wrote it and the answer, and no request after it writes a second line for the same
// request.
func TestAPersonsRequestWritesOneAccessLogLineNamingThem(t *testing.T) {
	// Not parallel: it swaps the default logger (captureLog, logtest.SetDefault).
	handler, database := newTestHandlerWithStore(t)
	seedTestProject(t, database)
	logged := captureLog(t)

	response := dispatchRequest(t, handler, http.MethodGet, "http://B.Example.Test:8443/api/v1/issues?project=ACCESS", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues = %d: %s", response.Code, response.Body.String())
	}
	lines := accessLines(t, logged)
	if len(lines) != 1 {
		t.Fatalf("one request wrote %d access-log lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, want := range []string{
		`level=INFO`,
		`method=GET`,
		`route=/api/v1/issues`,
		`path=/api/v1/issues`,
		`host=B.Example.Test:8443 `,
		`caller_kind=user`,
		`caller=alice`,
		`status=200`,
		`elapsed_ms=`,
		`bytes=`,
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("access-log line %q does not carry %q", lines[0], want)
		}
	}
}

// A bearer's write is named by the session it acts as, never by the token it sent, and the line
// holds neither the token nor the body the caller wrote.
func TestABearersRequestIsNamedByItsSessionAndCarriesNoToken(t *testing.T) {
	// Not parallel: it swaps the default logger (captureLog, logtest.SetDefault).
	handler, database := newTestHandlerWithStore(t)
	seedTestProject(t, database)
	logged := captureLog(t)

	body, err := json.Marshal(map[string]any{
		"project": "ACCESS",
		"title":   "An issue a session created",
		"actor":   map[string]string{"kind": "session", "id": "session-0123456789abcdef"},
	})
	if err != nil {
		t.Fatalf("encode issue: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/issues = %d: %s", response.Code, response.Body.String())
	}
	lines := accessLines(t, logged)
	if len(lines) != 1 {
		t.Fatalf("one request wrote %d access-log lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	line := lines[0]
	for _, want := range []string{`method=POST`, `route=/api/v1/issues`, `caller_kind=session`, `caller=session-0123456789abcdef`, `credential=shared-token`, `status=201`} {
		if !strings.Contains(line, want) {
			t.Errorf("access-log line %q does not carry %q", line, want)
		}
	}
	for _, forbidden := range []string{"agent-token", "Bearer", "Authorization", "An issue a session created"} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("access-log line %q carries %q, which no line may hold", line, forbidden)
		}
	}
}

// A request no credential authenticates is logged with its refusal, its Host and no caller, so a
// flood of refused requests is visible too, at the address it was sent to.
func TestARefusedRequestIsLoggedWithNoCaller(t *testing.T) {
	// Not parallel: it swaps the default logger (captureLog, logtest.SetDefault).
	handler := newTestHandler(t)
	logged := captureLog(t)

	request := httptest.NewRequest(http.MethodGet, "http://old.example.test/api/v1/issues", nil)
	request.Header.Set("Authorization", "Bearer not-a-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/issues with an unknown bearer = %d, want 401", response.Code)
	}
	lines := accessLines(t, logged)
	if len(lines) != 1 {
		t.Fatalf("one request wrote %d access-log lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, want := range []string{`caller_kind=anonymous`, `host=old.example.test `, `status=401`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("access-log line %q does not carry %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], "not-a-token") {
		t.Fatalf("access-log line %q carries the refused bearer", lines[0])
	}
}

// A socket that outlives its request resolves its caller again on every heartbeat, and that
// resolution names no one on the line: a shared-token bearer's re-resolution would otherwise
// replace the session the socket was admitted as with a nameless shared token. The socket stays
// open until ten of those resolutions have returned, so the line is written after them.
func TestASocketsHeartbeatLeavesTheSessionItWasAdmittedAs(t *testing.T) {
	// Not parallel: it swaps the default logger (captureLog, logtest.SetDefault).
	_, _, deps := newTestServer(t, testServerOptions{streamHeartbeat: 10 * time.Millisecond})
	logged := captureLog(t)
	served := directServer(deps)
	var resolved atomic.Int32
	stillResolves := func(r *http.Request) error {
		defer resolved.Add(1)
		return served.callerStillResolves(r)
	}
	socket := accesslog.Handler("/ws/live", served.whileCallerResolves(stillResolves, func(w http.ResponseWriter, r *http.Request) {
		accesslog.Note(r.Context(), accesslog.KindSharedToken, "")
		accesslog.Note(r.Context(), accesslog.KindSession, "session-0123456789abcdef")
		testwait.Until(t, "ten heartbeats to resolve the socket's caller again (the resolutions returned)", func() (int32, bool) {
			if r.Context().Err() != nil {
				t.Fatalf("the socket closed after %d heartbeat resolutions: %v", resolved.Load(), context.Cause(r.Context()))
			}
			n := resolved.Load()
			return n, n >= 10
		})
	}))
	request := httptest.NewRequest(http.MethodGet, "/ws/live", nil)
	request.Header.Set("Authorization", "Bearer agent-token")
	socket.ServeHTTP(httptest.NewRecorder(), request)

	lines := accessLines(t, logged)
	if len(lines) != 1 {
		t.Fatalf("one socket wrote %d access-log lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, want := range []string{`caller_kind=session`, `caller=session-0123456789abcdef`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("access-log line %q does not carry %q", lines[0], want)
		}
	}
}

// accessTimings is the run of an access-log line's fields from status on: status, elapsed_ms and
// bytes next to each other, then the four timings.
var accessTimings = regexp.MustCompile(` status=\d+ elapsed_ms=\d+ bytes=\d+ db_ms=(\d+) db_calls=(\d+) db_wait_ms=(\d+) roster_wait_ms=(\d+)(?: |$)`)

// lineTimings is the four timings line carries where it writes them, in their order: db_ms,
// db_calls, db_wait_ms and roster_wait_ms.
func lineTimings(t *testing.T, line string) [4]int64 {
	t.Helper()
	match := accessTimings.FindStringSubmatch(line)
	if match == nil {
		t.Fatalf("access-log line %q does not carry status, elapsed_ms, bytes, db_ms, db_calls, db_wait_ms and roster_wait_ms in one run", line)
	}
	var read [4]int64
	for index := range read {
		read[index], _ = strconv.ParseInt(match[index+1], 10, 64)
	}
	return read
}

// A list read with a routed issue reads the listener's session list, and its line says how long
// that took as roster_wait_ms. The stored copy is stale at every read (a TTL of zero), so the read
// waits on a fetch from a listener that answers 40 ms late, and its roster_wait_ms is at least
// that. A list with no routed issue reads no roster, and its line carries 0 there. Both lines
// count the statements their reads ran.
func TestAListReadsLineNamesItsWaitForTheListenersSessions(t *testing.T) {
	// Not parallel: it swaps the default logger (captureLog, logtest.SetDefault).
	const late = 40 * time.Millisecond
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			http.NotFound(w, r)
			return
		}
		testwait.Pause(late)
		_, _ = w.Write([]byte(`[{"session_id":"ses-sre","title":"worker","roles":["sre"]}]`))
	}))
	t.Cleanup(listener.Close)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: time.Minute})
	served.rosters.ttl, served.rosters.silenceTTL, served.rosters.timeout = 0, 0, time.Minute
	key := routedIssueKey(t, handler, "role:sre")
	seedTestProject(t, database)
	logged := captureLog(t)

	routed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST", nil, "alice")
	if routed.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues?project=TEST = %d: %s", routed.Code, routed.Body.String())
	}
	rows := decodeBody[[]routedIssue](t, routed)
	row := slices.IndexFunc(rows, func(issue routedIssue) bool { return issue.Key == key })
	if row < 0 || reachText(rows[row].RouteStatus, rows[row].RouteHolder) != "live ses-sre" {
		t.Fatalf("the list of TEST reads %+v, want %s live ses-sre: the read did not read the listener's answer", rows, key)
	}
	unrouted := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=ACCESS", nil, "alice")
	if unrouted.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues?project=ACCESS = %d: %s", unrouted.Code, unrouted.Body.String())
	}

	lines := accessLines(t, logged)
	if len(lines) != 2 {
		t.Fatalf("two requests wrote %d access-log lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if read := lineTimings(t, lines[0]); read[3] < late.Milliseconds() || read[1] == 0 {
		t.Errorf("the routed list's db_ms, db_calls, db_wait_ms and roster_wait_ms = %v, want a roster_wait_ms of at least the listener's %d ms and statements counted", read, late.Milliseconds())
	}
	if read := lineTimings(t, lines[1]); read[3] != 0 || read[1] == 0 {
		t.Errorf("the unrouted list's db_ms, db_calls, db_wait_ms and roster_wait_ms = %v, want a roster_wait_ms of 0 and statements counted", read)
	}
}

func seedTestProject(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), `insert into projects (key, name, default_assignee) values ('ACCESS', 'Access', 'alice')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// searchKeyShape is the shape the search_key a request line names must have: 16 hex characters.
var searchKeyShape = regexp.MustCompile(`^[0-9a-f]{16}$`)

// sendSearch sends a search on path with query as alice, with the display-trees header when
// trees, and answers the one request line it wrote and the search_key that line names, which
// must be 16 hex characters. A search that is not answered 200, that writes no line or more than
// one, or whose line names no key fails the test, and so does a search with as_of that is not
// answered at a cut (Dispatch-As-Of).
func sendSearch(t *testing.T, handler http.Handler, logged *logBuffer, path string, query url.Values, trees bool) (line, key string) {
	t.Helper()
	before := len(accessLines(t, logged))
	request := aliceSearch(context.Background(), path, query.Encode())
	if trees {
		request.Header.Set(displayTreesHeader, "1")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s?%s = %d: %s", path, query.Encode(), response.Code, response.Body.String())
	}
	if query.Has(asOfParameter) && response.Header().Get(contracts.DispatchAsOfHeader) == "" {
		t.Fatalf("GET %s?%s was answered with no %s, so not at a cut", path, query.Encode(), contracts.DispatchAsOfHeader)
	}
	lines := accessLines(t, logged)[before:]
	if len(lines) != 1 {
		t.Fatalf("GET %s?%s wrote %d request lines, want one:\n%s", path, query.Encode(), len(lines), strings.Join(lines, "\n"))
	}
	key, found := horizontest.Field(lines[0], "search_key")
	if !found {
		t.Fatalf("the request line of GET %s?%s names no search_key: %s", path, query.Encode(), lines[0])
	}
	if !searchKeyShape.MatchString(key) {
		t.Fatalf("the request line of GET %s?%s names search_key=%q, want 16 hex characters: %s", path, query.Encode(), key, lines[0])
	}
	return lines[0], key
}

// unkeyedHashes are the hashes of the search query names that a reader of the log could make
// without the server's key, each cut to the 16 hex characters the line keeps: SHA-256 of q,
// SHA-256 of the normalized request searchRequestKey hashes, and HMAC-SHA256 of that request under
// the empty key. The normalized request is written here from the parameters as D5 lists them,
// not by the server's own code, for a search that names no updated_since, kind, limit or offset and
// asks for no display trees; its as_of is the cut, in RFC 3339 UTC to the microsecond as a cut
// holds it, or empty for a search of the present.
func unkeyedHashes(t *testing.T, query url.Values) []string {
	t.Helper()
	asOf := ""
	if query.Has(asOfParameter) {
		cut, err := time.Parse(time.RFC3339Nano, query.Get(asOfParameter))
		if err != nil {
			t.Fatalf("read the search's as_of %q: %v", query.Get(asOfParameter), err)
		}
		asOf = cut.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	}
	q := strings.TrimSpace(query.Get("q"))
	normalized, _ := json.Marshal([]any{q, query.Get("project"), query.Get("number_project"), query.Get("status"),
		query.Get("author"), "", 0, searchDefaultLimit, 0, query.Get("keywords_only") == "true", false, asOf})
	textSum := sha256.Sum256([]byte(q))
	requestSum := sha256.Sum256(normalized)
	emptyKey := hmac.New(sha256.New, nil)
	emptyKey.Write(normalized)
	return []string{
		hex.EncodeToString(textSum[:])[:16],
		hex.EncodeToString(requestSum[:])[:16],
		hex.EncodeToString(emptyKey.Sum(nil))[:16],
	}
}

// A search's request line names the keyed hash of its request as search_key, 16 hex characters,
// and the statement it chose as search_shape, in each shape a search takes: keywords alone,
// keywords because no embedder answered, each of the three meaning statements, a broadened run
// of a keyword and of a meaning search, and none for a query of stop words alone, which runs no
// statement. The line holds no query text and no hash of it a reader could make without the key
// (R5b): not the SHA-256 of q, not the SHA-256 of the normalized request, and not its HMAC under
// the empty key. The fixture is TestEverySearchStatementRunsInsideItsBoundedTransaction's, which
// reaches each shape.
func TestASearchsRequestLineNamesItsKeyAndShape(t *testing.T) {
	// Not parallel: it swaps the default logger (captureLog, logtest.SetDefault).
	const degradedQuery, broadenedQuery = "lighthouse keeper", "lighthouse zzzabsent"
	embedder := refusingQueryEmbedder{
		fakeEmbedder: &fakeEmbedder{vectors: map[string][]float32{broadenedQuery: queryVector()}},
		refused:      degradedQuery,
	}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "SHAPE", "Lighthouse keeper roster", "The keeper logs the tide at dawn.")
	searchComment(t, handler, issue.Key, "alice", "The lighthouse keeper logs the tide at dawn.")
	processAllPending(t, database, embedder)
	RefreshEverySearchTermStats(context.Background(), database.Pool)
	recordEmbeddingProjectKeyFill(t, database, true)
	logged := captureLog(t)

	for _, shape := range []struct {
		name, path string
		query      url.Values
		want       string
	}{
		{"keywords only", "/api/v1/search/results",
			url.Values{"q": {"lighthouse"}, "keywords_only": {"true"}}, "keyword"},
		{"keywords because no embedder answered", "/api/v1/search",
			url.Values{"q": {degradedQuery}}, "keyword-degraded"},
		{"meaning over every project", "/api/v1/search",
			url.Values{"q": {"lighthouse"}}, "meaning-every"},
		{"meaning over one project once the fill is done", "/api/v1/search",
			url.Values{"q": {"lighthouse"}, "project": {"SHAPE"}}, "meaning-scoped"},
		{"meaning joined for a filtered search", "/api/v1/search/results",
			url.Values{"q": {"lighthouse"}, "author": {"alice"}}, "meaning-joined"},
		{"keywords only, broadened", "/api/v1/search/results",
			url.Values{"q": {broadenedQuery}, "keywords_only": {"true"}}, "keyword+broadened"},
		{"meaning, broadened", "/api/v1/search",
			url.Values{"q": {broadenedQuery}}, "meaning-every+broadened"},
		{"stop words alone", "/api/v1/search",
			url.Values{"q": {"the of"}}, "none"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			line, _ := sendSearch(t, handler, logged, shape.path, shape.query, false)
			if got, _ := horizontest.Field(line, "search_shape"); got != shape.want {
				t.Errorf("the request line names no search_shape=%s: %s", shape.want, line)
			}
			requireNoQueryOnTheLine(t, line, shape.query)
		})
	}
}

// requireNoQueryOnTheLine fails the test when line, a search's request line, carries the query
// query names, escaped or not, a word of it longer than three characters, or a hash of it a reader
// could make without the server's key (unkeyedHashes).
func requireNoQueryOnTheLine(t *testing.T, line string, query url.Values) {
	t.Helper()
	q := query.Get("q")
	forbidden := []string{q, url.QueryEscape(q)}
	for word := range strings.FieldsSeq(q) {
		if len(word) > 3 {
			forbidden = append(forbidden, word)
		}
	}
	forbidden = append(forbidden, unkeyedHashes(t, query)...)
	for _, text := range forbidden {
		if strings.Contains(line, text) {
			t.Errorf("the request line carries %q, which is the query or a hash of it made without the server's key: %s", text, line)
		}
	}
}

// A search at a cut names its key and its shape on its request line as a search of the present
// does (TestASearchsRequestLineNamesItsKeyAndShape): keyword and keyword-degraded for the keyword
// statement at the cut, meaning-as-of for the one meaning statement a search at a cut runs, whatever
// project or filter it names, and none for a query of stop words alone. The line holds no query
// text and no hash of it a reader could make without the key, the cut among the request's members
// (unkeyedHashes). Each search is answered at the cut (sendSearch reads Dispatch-As-Of).
func TestASearchAtACutNamesItsKeyAndShape(t *testing.T) {
	// Not parallel: it swaps the default logger (captureLog, logtest.SetDefault).
	const degradedQuery = "lighthouse keeper"
	embedder := refusingQueryEmbedder{fakeEmbedder: &fakeEmbedder{vectors: map[string][]float32{}}, refused: degradedQuery}
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "CUTSHAPE", "Lighthouse keeper roster", "The keeper logs the tide at dawn.")
	searchComment(t, handler, issue.Key, "alice", "The lighthouse keeper logs the tide at dawn.")
	processAllPending(t, database, embedder)
	awaitBackgroundWrites(t, served)
	safeHorizon(t, served)
	cut := databaseCut(t, database)
	fillTextVersionsPast(t, database, cut)
	logged := captureLog(t)

	at := func(query url.Values) url.Values {
		query.Set(asOfParameter, cut.String())
		return query
	}
	for _, shape := range []struct {
		name, path string
		query      url.Values
		want       string
	}{
		{"keywords only at a cut", "/api/v1/search/results",
			at(url.Values{"q": {"lighthouse"}, "keywords_only": {"true"}}), "keyword"},
		{"keywords because no embedder answered, at a cut", "/api/v1/search",
			at(url.Values{"q": {degradedQuery}}), "keyword-degraded"},
		{"meaning at a cut", "/api/v1/search",
			at(url.Values{"q": {"lighthouse"}}), "meaning-as-of"},
		{"meaning at a cut over one project", "/api/v1/search",
			at(url.Values{"q": {"lighthouse"}, "project": {"CUTSHAPE"}}), "meaning-as-of"},
		{"meaning at a cut narrowed by an author", "/api/v1/search/results",
			at(url.Values{"q": {"lighthouse"}, "author": {"alice"}}), "meaning-as-of"},
		{"stop words alone at a cut", "/api/v1/search",
			at(url.Values{"q": {"the of"}}), "none"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			line, _ := sendSearch(t, handler, logged, shape.path, shape.query, false)
			if got, _ := horizontest.Field(line, "search_shape"); got != shape.want {
				t.Errorf("the request line names no search_shape=%s: %s", shape.want, line)
			}
			requireNoQueryOnTheLine(t, line, shape.query)
		})
	}
}

// Each member of the normalized request moves the key: a search that differs from another in
// one of them alone is named by another search_key, one row for each member, and the same search
// sent again is named by the same key, so the difference is the member's and not a key drawn
// afresh. as_of has two rows, a cut and an earlier one, so a search at a cut is named apart from
// the same search of the present and from the same search at another cut. Two spellings the
// handler reads as one request - a q with blanks around it, an updated_since or an as_of in
// another zone at the same instant, or equivalent kind sets - are named by one key, and so is
// one as_of sent twice.
//
// A search at a cut is answered only once the keyword table's fill has reached the cut
// (store.TextVersionsAt), so the test takes its two cuts, the later 5 ms after the earlier, and
// fills the table past the later before it searches.
func TestEachSearchParameterMovesTheKey(t *testing.T) {
	// Not parallel: it swaps the default logger (captureLog, logtest.SetDefault).
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createInteractionIssue(t, handler, "MOVE", "Lighthouse keeper roster", "The keeper logs the tide at dawn.")
	awaitBackgroundWrites(t, served)
	safeHorizon(t, served)
	earlier := databaseCut(t, database)
	time.Sleep(5 * time.Millisecond)
	later := databaseCut(t, database)
	fillTextVersionsPast(t, database, later)
	logged := captureLog(t)
	base := func() url.Values { return url.Values{"q": {"lighthouse"}} }
	_, baseKey := sendSearch(t, handler, logged, "/api/v1/search", base(), false)
	if _, again := sendSearch(t, handler, logged, "/api/v1/search", base(), false); again != baseKey {
		t.Fatalf("the same search sent twice was named %s and %s, want one key", baseKey, again)
	}

	keys := map[string]string{baseKey: "the base search"}
	for _, row := range []struct {
		member string
		set    func(url.Values)
		trees  bool
	}{
		{member: "q", set: func(query url.Values) { query.Set("q", "lighthouses") }},
		{member: "project", set: func(query url.Values) { query.Set("project", "MOVE") }},
		{member: "number_project", set: func(query url.Values) { query.Set("number_project", "MOVE") }},
		{member: "status", set: func(query url.Values) { query.Set("status", model.IssueStatuses[0]) }},
		{member: "author", set: func(query url.Values) { query.Set("author", "alice") }},
		{member: "updated_since", set: func(query url.Values) { query.Set("updated_since", "2026-01-02T03:04:05Z") }},
		{member: "kind", set: func(query url.Values) { query.Set("kind", "issue") }},
		{member: "limit", set: func(query url.Values) { query.Set("limit", "5") }},
		{member: "offset", set: func(query url.Values) { query.Set("offset", "1") }},
		{member: "keywords_only", set: func(query url.Values) { query.Set("keywords_only", "true") }},
		{member: "trees", set: func(url.Values) {}, trees: true},
		{member: "as_of", set: func(query url.Values) { query.Set(asOfParameter, later.String()) }},
		{member: "as_of, at an earlier cut", set: func(query url.Values) { query.Set(asOfParameter, earlier.String()) }},
	} {
		t.Run(row.member, func(t *testing.T) {
			query := base()
			row.set(query)
			_, key := sendSearch(t, handler, logged, "/api/v1/search", query, row.trees)
			if earlier, taken := keys[key]; taken {
				t.Fatalf("a search that differs from the base in %s alone was named %s, the key of %s", row.member, key, earlier)
			}
			keys[key] = "the search that differs in " + row.member
		})
	}

	for _, same := range []struct {
		name          string
		first, second url.Values
	}{
		{"q with blanks around it", base(), url.Values{"q": {"  lighthouse  "}}},
		{"updated_since in another zone at the same instant",
			url.Values{"q": {"lighthouse"}, "updated_since": {"2026-01-02T03:04:05Z"}},
			url.Values{"q": {"lighthouse"}, "updated_since": {"2026-01-02T05:04:05+02:00"}}},
		{"reordered and repeated kind parameters",
			url.Values{"q": {"lighthouse"}, "kind": {"issue", "document"}},
			url.Values{"q": {"lighthouse"}, "kind": {"document", "issue", "issue"}}},
		{"every kind named explicitly",
			base(),
			url.Values{"q": {"lighthouse"}, "kind": searchIndexedKinds[:]}},
		{"as_of sent twice",
			url.Values{"q": {"lighthouse"}, asOfParameter: {later.String()}},
			url.Values{"q": {"lighthouse"}, asOfParameter: {later.String()}}},
		{"as_of in another zone at the same instant",
			url.Values{"q": {"lighthouse"}, asOfParameter: {later.String()}},
			url.Values{"q": {"lighthouse"}, asOfParameter: {later.Time().In(time.FixedZone("", 2*60*60)).Format(time.RFC3339Nano)}}},
	} {
		_, first := sendSearch(t, handler, logged, "/api/v1/search", same.first, false)
		_, second := sendSearch(t, handler, logged, "/api/v1/search", same.second, false)
		if first != second {
			t.Errorf("%s: the two spellings were named %s and %s, want one key", same.name, first, second)
		}
	}
}

// The key is made when a server is registered and never written: two servers registered in one
// test answer the same search under two keys, each server holds a key of 32 bytes, and no line
// either server wrote, from its registration to its answer, holds either key in hex.
func TestTwoServersKeyTheSameSearchDifferently(t *testing.T) {
	// Not parallel: it swaps the default logger (captureLog, logtest.SetDefault).
	logged := captureLog(t)
	first, _, _, firstServer := newRegisteredTestServer(t, testServerOptions{})
	second, _, _, secondServer := newRegisteredTestServer(t, testServerOptions{})
	query := url.Values{"q": {"lighthouse"}}
	_, firstKey := sendSearch(t, first, logged, "/api/v1/search", query, false)
	_, secondKey := sendSearch(t, second, logged, "/api/v1/search", query, false)
	if firstKey == secondKey {
		t.Fatalf("two servers named the same search %s, want a key of each server's own", firstKey)
	}

	for name, key := range map[string][]byte{"the first server's": firstServer.searchLogKey, "the second server's": secondServer.searchLogKey} {
		if len(key) != 32 || bytes.Equal(key, make([]byte, 32)) {
			t.Fatalf("%s key is %x, want 32 bytes from crypto/rand", name, key)
		}
		written := hex.EncodeToString(key)
		for _, line := range strings.Split(logged.String(), "\n") {
			if strings.Contains(strings.ToLower(line), written) {
				t.Errorf("a log line holds %s key in hex: %s", name, line)
			}
		}
	}
	if bytes.Equal(firstServer.searchLogKey, secondServer.searchLogKey) {
		t.Fatalf("two servers hold one key %x", firstServer.searchLogKey)
	}
}

// The answerer's own search (goingSearch) runs on a context of the going request's, which still
// carries that request's line, and names no search on it: it holds no key, so it notes no
// shape either, and the going line carries neither field. The search finds the issue, so a
// statement ran and would have been noted.
func TestTheAnswerersSearchNamesNoSearchOnItsRequestLine(t *testing.T) {
	// Not parallel: it swaps the default logger (captureLog, logtest.SetDefault).
	const goingRoute = "/api/v1/going/requests"
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "GOING", "Lighthouse keeper roster", "The keeper logs the tide at dawn.")
	logged := captureLog(t)
	found := 0
	accesslog.HandlerFunc(goingRoute, func(w http.ResponseWriter, r *http.Request) {
		hits, err := served.goingSearch("GOING", model.Actor{})(r.Context(), "lighthouse")
		if err != nil {
			t.Errorf("the answerer's search: %v", err)
		}
		for _, hit := range hits {
			if hit.ID == issue.Key {
				found++
			}
		}
	}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, goingRoute, nil))
	if found == 0 {
		t.Fatalf("the answerer's search did not find %s, so no statement ran", issue.Key)
	}
	lines := accessLines(t, logged)
	if len(lines) != 1 {
		t.Fatalf("the going request wrote %d request lines, want one:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if strings.Contains(lines[0], "search_") {
		t.Fatalf("the going request's line names a search: %s", lines[0])
	}
}
