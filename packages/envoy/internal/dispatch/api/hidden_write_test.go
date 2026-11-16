package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestNoWriteReachesAHiddenObject walks every write row whose pattern names an object ({key}, {id}
// or {session_id}), whatever its write class, on verification v16's seed (seedPrivateProject). Each
// row is called twice as one caller: once naming PRIV's object of each kind its Probe names, and
// once naming an object nobody holds. The two answers must be the same status and the same bytes
// (the second's ids written as the first's), and neither call may change a row of any table: the
// event log, the graph's tuples and the object itself among them. The call naming nothing must be
// answered as the row answers a missing object, so a body the row refuses before its read cannot
// pass the row.
//
// Two callers walk it. alice, at a browser, is an open viewer who sees PUB and no object of PRIV.
// alice's token that lists PRIV sees no project at all, since alice does not see PRIV, and is not
// open, so it walks the deployment's rows too, which it is refused whatever they name.
//
// A session in a path is never the hidden object: it is the other party of the caller's own
// conversation, a subscriber or follower of an object the path names, or the caller's own pin, and
// no such write reads anything of the session (which sessions a credential reaches is the
// listener's to hold, K6). So the rows that name a session and nothing else are walked by no caller,
// and sessionOnlyRows names them.
func TestNoWriteReachesAHiddenObject(t *testing.T) {
	walk := seedHiddenWrites(t)
	rows := objectWriteRows()
	if problems := writeProbeProblems(rows); len(problems) > 0 {
		t.Fatalf("writeProbes does not describe the write rows that name an object:\n%s", strings.Join(problems, "\n"))
	}
	walked := map[string]bool{}
	for _, caller := range []privateCaller{callerAlice, callerAlicePrivToken} {
		open := caller == callerAlice
		for _, name := range rows {
			if !writeProbes[name].hides(open) {
				continue
			}
			walked[name] = true
			walk.compare(t, caller, open, name)
		}
	}
	for _, name := range rows {
		if walked[name] == sessionOnlyRows[name] {
			t.Errorf("%s: walked %t, named in sessionOnlyRows %t: a row is walked exactly when it names an object a caller of the walk does not see",
				name, walked[name], sessionOnlyRows[name])
		}
	}
	t.Logf("walked %d of the %d write rows that name an object", len(walked), len(rows))

	// What makes the subscriber rows able to fail: the walk's listener holds s1 subscribed to
	// PRIV's issue and document, so carol, who sees PRIV, removes it. A listener that held no such
	// interest would answer every removal "not subscribed", PRIV's and a missing one's alike.
	for _, path := range []string{
		"/api/v1/issues/" + walk.fills[probeIssue].hidden + "/subscribers/s1",
		"/api/v1/artifacts/" + walk.fills[probeDocument].hidden + "/subscribers/s1",
	} {
		if removed := walk.request(t, callerCarol, http.MethodDelete, path, nil); removed.Code != http.StatusNoContent {
			t.Errorf("carol: DELETE %s = %d %s, want 204: the walk's listener does not hold s1 subscribed to PRIV, so its subscriber rows prove nothing",
				path, removed.Code, strings.TrimSpace(removed.Body.String()))
		}
	}
}

// sessionOnlyRows are the write rows whose only object is a session (the test's doc comment).
var sessionOnlyRows = map[string]bool{
	"POST /api/v1/agents/{session_id}/messages":             true,
	"POST /api/v1/agents/{session_id}/artifacts":            true,
	"POST /api/v1/agents/{session_id}/stream/_test/publish": true,
	"PUT /api/v1/me/agents/{session_id}/state":              true,
}

// missingAnswers is what a row answers for an object nobody holds where that is no 404, and why.
var missingAnswers = map[string]struct {
	status int
	code   string
	why    string
}{
	"DELETE /api/v1/me/asks/{id}/snooze": {http.StatusNoContent, "",
		"un-snoozing an ask the caller never snoozed leaves the state asked for, whatever the id names (idRoutes: notFoundNone)"},
	"PUT /api/v1/me/threads/{id}/read": {http.StatusNoContent, "",
		"the caller's own read mark, kept under any thread id and reading no message (idRoutes)"},
	"POST /api/v1/issues/{key}/results": {http.StatusBadRequest, "UNKNOWN_CHECK",
		"a result names a check of its issue's Verification document, and an issue the caller cannot read holds none"},
}

// refusedAtABrowser is the refusal a row makes a person at a browser before it reads anything: the
// row is a session's own, so only a bearer reaches its read.
var refusedAtABrowser = map[string]string{
	"POST /api/v1/comments/{id}/reply":                       "REPLY_FORBIDDEN",
	"POST /api/v1/messages/{id}/reply":                       "REPLY_FORBIDDEN",
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept": "ACCEPT_FORBIDDEN",
	"POST /api/v1/issues/{key}/handoffs/{phase}":             "HANDOFF_FORBIDDEN",
}

// objectWriteRows is every write row of writeClasses whose pattern names an object, sorted.
func objectWriteRows() []string {
	var rows []string
	for name := range writeClasses {
		if strings.Contains(name, "{key}") || strings.Contains(name, "{id}") || strings.Contains(name, "{session_id}") {
			rows = append(rows, name)
		}
	}
	slices.Sort(rows)
	return rows
}

// writeProbeProblems names every row writeProbes leaves out, every entry that is no such row, and
// every Probe that does not fill exactly its pattern's wildcards with known objects or whose body
// is not JSON.
func writeProbeProblems(rows []string) []string {
	var problems []string
	for _, name := range rows {
		if _, ok := writeProbes[name]; !ok {
			problems = append(problems, name+": no entry in writeProbes")
		}
	}
	for name, probe := range writeProbes {
		if !slices.Contains(rows, name) {
			problems = append(problems, name+": an entry of writeProbes for no write row that names an object")
			continue
		}
		_, pattern, _ := strings.Cut(name, " ")
		wildcards := map[string]bool{}
		for _, match := range patternWildcard.FindAllStringSubmatch(pattern, -1) {
			wildcards[match[1]] = true
			if _, filled := probe.Probe[match[1]]; !filled {
				problems = append(problems, fmt.Sprintf("%s: a Probe that does not fill {%s}", name, match[1]))
			}
		}
		for wildcard, object := range probe.Probe {
			if !wildcards[wildcard] {
				problems = append(problems, fmt.Sprintf("%s: a Probe that fills {%s}, which the pattern does not hold", name, wildcard))
			}
			if !slices.Contains(probeObjects, object) {
				problems = append(problems, fmt.Sprintf("%s: a Probe that fills {%s} with %q, none of probeObjects", name, wildcard, object))
			}
		}
		if probe.Body != "" && !json.Valid([]byte(probe.Body)) {
			problems = append(problems, name+": a body that is not JSON")
		}
	}
	slices.Sort(problems)
	return problems
}

// hiddenFrom reports whether the walk names PRIV's object of kind in place of one nobody holds, for
// an open caller or not: an object of a project is hidden from both callers, a personal token is
// someone else's, and a row of the deployment is hidden from a caller that is not open. Every other
// kind is part of an object the path already names - a document's slug, a version, a block, an
// attempt, a phase, a person or group on a list, a repository, a pause point - or a session, and is
// PRIV's own in both calls.
func hiddenFrom(object probeObject, open bool) bool {
	switch object {
	case probeProject, probeIssue, probeDocument, probeAsk, probeComment, probeMessage, probeThread,
		probeResult, probeAgentToken:
		return true
	case probePriority, probeSessionGroup, probeCredentialRequest, probeCredentialGrant, probeMachineLogin,
		probeSignin:
		return !open
	}
	return false
}

// hides reports whether the probe names an object hidden from the caller.
func (p writeProbe) hides(open bool) bool {
	for _, object := range p.Probe {
		if hiddenFrom(object, open) {
			return true
		}
	}
	return false
}

// probeFill is one kind's two values: PRIV's object, and one of that kind nobody holds.
type probeFill struct{ hidden, missing string }

// hiddenWriteSeed is the seed the walk calls, with the value of every probe kind.
type hiddenWriteSeed struct {
	privateSeed
	fills  map[probeObject]probeFill
	tables []string
}

// volatileColumns are the columns a call stamps whatever it names, left out of a table's digest: a
// personal token's use stamps its own row.
var volatileColumns = map[string]string{"public.agent_tokens": "last_used_at"}

// walkListener is a fake Envoy listener holding session s1 live and subscribed to PRIV's issue and
// project document, so a write that removes a subscriber reaches the removal.
func walkListener(t *testing.T, topics *[]string) *httptest.Server {
	t.Helper()
	const s1 = `{"session_id":"s1","title":"planner","dir":"/w/legion","machine_id":"m1","roles":[],"capabilities":["aside","btw","steer"],"last_seen":42}`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			_, _ = w.Write([]byte(s1))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[` + s1 + `]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/interests/s1":
			encoded, _ := json.Marshal(map[string]any{"session_id": "s1", "topics": *topics, "updated_at": 1})
			_, _ = w.Write(encoded)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/interests/unsubscribe":
			var request struct {
				Topics []string `json:"topics"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			encoded, _ := json.Marshal(map[string]any{"removed": request.Topics})
			_, _ = w.Write(encoded)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
}

// seedHiddenWrites stands up verification v16's seed and writes, as carol, PRIV's object of every
// kind a write row names, through the routes.
func seedHiddenWrites(t *testing.T) hiddenWriteSeed {
	t.Helper()
	topics := []string{}
	listener := walkListener(t, &topics)
	t.Cleanup(listener.Close)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	issue := seed.issue[seedPrivate]
	asCarol := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		response := seed.request(t, callerCarol, method, path, body)
		if response.Code < 200 || response.Code > 299 {
			t.Fatalf("carol: %s %s = %d %s", method, path, response.Code, response.Body.String())
		}
		return response
	}

	asCarol(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "verification.md", "content": twoChecks})
	issueDocument := decodeBody[uploadedArtifact](t, asCarol(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts",
		map[string]string{"name": "walked-notes.md", "content": "# Notes\n\nA passage of PRIV.\n"}))
	projectDocument := decodeBody[uploadedArtifact](t, asCarol(http.MethodPost, "/api/v1/projects/"+seedPrivate+"/artifacts",
		map[string]string{"name": "walked-notes.md", "content": "# Notes\n\nA passage of PRIV.\n"}))
	if issueDocument.Artifact.Slug != projectDocument.Artifact.Slug {
		t.Fatalf("the issue's document is %q and the project's %q: one slug fills both owners' rows", issueDocument.Artifact.Slug, projectDocument.Artifact.Slug)
	}
	topics = []string{issueSubscriberBase(issue) + ".>", documentSubscriberBase(seedPrivate, projectDocument.Artifact.Slug) + ".>"}
	blocks := decodeBody[[]struct {
		ID string `json:"id"`
	}](t, asCarol(http.MethodGet, "/api/v1/artifacts/"+projectDocument.Artifact.ID+"/blocks", nil))
	if len(blocks) == 0 {
		t.Fatalf("PRIV's project document holds no block")
	}
	ask := createdID(t, asCarol(http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{"question": "Which way for PRIV?"}))
	comment := createdID(t, asCarol(http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{"body": "A remark on PRIV."}))
	message := createdID(t, asCarol(http.MethodPost, "/api/v1/issues/"+issue+"/messages", map[string]any{
		"body": "A note on PRIV.", "target": "session:s1", "delivery": "btw",
	}))
	result := decodeBody[recordedResult](t, asCarol(http.MethodPost, "/api/v1/issues/"+issue+"/results",
		resultBody("k1", "pass", headCommit, nil))).Result.ID
	const group = "priv-readers"
	asCarol(http.MethodPut, "/api/v1/access-groups/"+group, map[string]any{"members": []string{seedCarol}})
	asCarol(http.MethodPut, "/api/v1/projects/"+seedPrivate+"/access/groups/"+group, nil)
	priority := createdID(t, asCarol(http.MethodPost, "/api/v1/priorities", map[string]any{"title": "A priority PRIV's people steer"}))
	sessionGroup := createdID(t, asCarol(http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": "PRIV's sessions", "sessions": []string{"s1"},
	}))
	var carolsToken string
	if err := seed.database.Pool.QueryRow(context.Background(), `select id::text from agent_tokens where name = $1`,
		string(callerCarolPrivToken)).Scan(&carolsToken); err != nil {
		t.Fatalf("read the id of %s: %v", callerCarolPrivToken, err)
	}

	same := func(value string) probeFill { return probeFill{value, value} }
	nobody := func(value string) probeFill { return probeFill{value, uuid.NewString()} }
	walk := hiddenWriteSeed{privateSeed: seed, fills: map[probeObject]probeFill{
		probeProject:         {seedPrivate, "NOPE"},
		probeIssue:           {issue, "NOPE-1"},
		probeDocument:        nobody(projectDocument.Artifact.ID),
		probeDocumentSlug:    same(projectDocument.Artifact.Slug),
		probeDocumentVersion: same("1"),
		probeBlock:           same(blocks[0].ID),
		probeAsk:             nobody(ask),
		probeComment:         nobody(comment),
		probeMessage:         nobody(message),
		probeThread:          nobody(message),
		probeResult:          nobody(result),
		probeAttempt:         same("1"),
		probeHandoffPhase:    same("build"),
		probeSession:         same("s1"),
		probeEmail:           same(seedBob),
		probeGroup:           same(group),
		probeRepositoryOwner: same("acme"),
		probeRepositoryName:  same("walked"),
		probeLivePause:       same("walked"),
		probeAgentToken:      nobody(carolsToken),
		probePriority:        {priority, "no-such-priority"},
		probeSessionGroup:    nobody(sessionGroup),
		// The secrets broker holds these, and the test server has none: both fills name nothing, and
		// the walk holds the deployment's refusal of a caller that is not open.
		probeCredentialRequest: nobody(uuid.NewString()),
		probeCredentialGrant:   nobody(uuid.NewString()),
		probeMachineLogin:      nobody(uuid.NewString()),
		probeSignin:            nobody(uuid.NewString()),
	}}
	rows, err := seed.database.Pool.Query(context.Background(), `
		select format('%I.%I', schemaname, tablename) from pg_tables
		where schemaname not in ('pg_catalog', 'information_schema') order by 1`)
	if err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	if walk.tables, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(walk.tables) == 0 {
		t.Fatalf("list the tables: %v (%d)", err, len(walk.tables))
	}
	return walk
}

// fill is pattern with each wildcard filled for the caller: PRIV's object where hidden, and
// otherwise one nobody holds of each kind hidden from it. It answers too what the second fill wrote
// in place of the first, so an answer that names what the caller sent can be compared.
func (w hiddenWriteSeed) fill(t *testing.T, pattern string, probe writeProbe, open, hidden bool) (string, map[string]string) {
	t.Helper()
	written := map[string]string{}
	path := patternWildcard.ReplaceAllStringFunc(pattern, func(match string) string {
		object := probe.Probe[patternWildcard.FindStringSubmatch(match)[1]]
		value, ok := w.fills[object]
		if !ok {
			t.Fatalf("%s: the walk holds no value of %q", pattern, object)
		}
		if hidden || !hiddenFrom(object, open) {
			return value.hidden
		}
		written[value.missing] = value.hidden
		return value.missing
	})
	return path, written
}

// tableDigests is a digest of every row of every table, by table, less volatileColumns.
func (w hiddenWriteSeed) tableDigests(t *testing.T) map[string]string {
	t.Helper()
	parts := make([]string, len(w.tables))
	for index, table := range w.tables {
		row := "to_jsonb(t)"
		if column, volatile := volatileColumns[table]; volatile {
			row += " - " + sqlString(column)
		}
		parts[index] = fmt.Sprintf(`select %s, md5(coalesce(string_agg((%s)::text, '|' order by (%s)::text), '')) from %s t`,
			sqlString(table), row, row, table)
	}
	rows, err := w.database.Pool.Query(context.Background(), strings.Join(parts, " union all "))
	if err != nil {
		t.Fatalf("digest the tables: %v", err)
	}
	digests, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var pair [2]string
		return pair, row.Scan(&pair[0], &pair[1])
	})
	if err != nil {
		t.Fatalf("digest the tables: %v", err)
	}
	byTable := make(map[string]string, len(digests))
	for _, pair := range digests {
		byTable[pair[0]] = pair[1]
	}
	return byTable
}

// sqlString is value as a SQL string literal.
func sqlString(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

// changedTables names every table whose rows differ between two digests.
func changedTables(before, after map[string]string) []string {
	var changed []string
	for table, digest := range after {
		if before[table] != digest {
			changed = append(changed, table)
		}
	}
	sort.Strings(changed)
	return changed
}

// compare calls one row as caller on PRIV's objects and on objects nobody holds, and holds the two
// to one answer and no write.
func (w hiddenWriteSeed) compare(t *testing.T, caller privateCaller, open bool, name string) {
	t.Helper()
	probe := writeProbes[name]
	method, pattern, _ := strings.Cut(name, " ")
	var body any
	if probe.Body != "" {
		body = json.RawMessage(probe.Body)
	}
	hiddenPath, _ := w.fill(t, pattern, probe, open, true)
	missingPath, written := w.fill(t, pattern, probe, open, false)

	before := w.tableDigests(t)
	hidden := w.request(t, caller, method, hiddenPath, body)
	afterHidden := w.tableDigests(t)
	if changed := changedTables(before, afterHidden); len(changed) > 0 {
		t.Errorf("%s as %s: the call naming PRIV's object (%s) answered %d %s and changed %v",
			name, caller, hiddenPath, hidden.Code, strings.TrimSpace(hidden.Body.String()), changed)
	}
	missing := w.request(t, caller, method, missingPath, body)
	if changed := changedTables(afterHidden, w.tableDigests(t)); len(changed) > 0 {
		t.Errorf("%s as %s: the call naming nothing (%s) answered %d %s and changed %v",
			name, caller, missingPath, missing.Code, strings.TrimSpace(missing.Body.String()), changed)
	}

	want := missing.Body.String()
	keys := make([]string, 0, len(written))
	for key := range written {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool { return len(keys[left]) > len(keys[right]) })
	for _, key := range keys {
		want = strings.ReplaceAll(want, key, written[key])
	}
	if hidden.Code != missing.Code || hidden.Body.String() != want {
		t.Errorf("%s as %s: PRIV's object answers %d %s, and an object nobody holds %d %s",
			name, caller, hidden.Code, strings.TrimSpace(hidden.Body.String()), missing.Code, strings.TrimSpace(missing.Body.String()))
	}
	if !answersMissing(name, open, missing) {
		t.Errorf("%s as %s: the call naming nothing (%s) answered %d %s, which is no answer for a missing object: the row's probe never reaches its read of the object",
			name, caller, missingPath, missing.Code, strings.TrimSpace(missing.Body.String()))
	}
}

// answersMissing reports whether a call naming no object was answered as the row answers a missing
// one: 404, the row's own answer (missingAnswers), the refusal of a person at a browser that comes
// before any read (refusedAtABrowser), or the deployment's refusal of a caller that is not open.
func answersMissing(name string, open bool, answer *httptest.ResponseRecorder) bool {
	var refusal refusalAnswer
	_ = json.Unmarshal(answer.Body.Bytes(), &refusal)
	if declared, ok := missingAnswers[name]; ok {
		return answer.Code == declared.status && refusal.Code == declared.code
	}
	switch {
	case answer.Code == http.StatusNotFound:
		return true
	case answer.Code != http.StatusForbidden:
		return false
	case open:
		return refusedAtABrowser[name] != "" && refusal.Code == refusedAtABrowser[name]
	default:
		return writeClasses[name] == scopeDeployment && refusal.Code == codeProjectScopeDeployment
	}
}
