// read_only_token_test.go holds Dispatch to its read list: a read-only token — one the secrets
// broker minted from a read entry, the scope read alone beside the project scope none, a service
// and no person — reaches the routes readScopeRoutes names and nothing else. Every other row of
// the table and GET /ws/live answer it 403 READ_ONLY_TOKEN before the route's handler, before the
// sign-in check and before its Idempotency-Key is read (refuseReadOnlyToken), register refuses a
// list that names anything but a GET row it mounts, and the route the list names writes nothing
// for such a caller but its proof's replay row and the caches Dispatch rebuilds.
//
// A token that names a service and no person is already refused every write, SIGN_IN_REQUIRED or
// OAUTH_SCOPE_REQUIRED, so each refusal here is read by its code: a bare 403 would prove nothing.
package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/testwait"
)

// readOnlyProbePath is routeProbePath with every wildcard it leaves filled too - a test hook's
// {point} - so every row of the table with its test hooks in is reached at a path its own
// pattern matches rather than at a literal brace.
func readOnlyProbePath(pattern string) string {
	path := routeProbePath(pattern)
	for _, name := range patternWildcards(pattern) {
		path = strings.ReplaceAll(path, "{"+name+"}", "x")
		path = strings.ReplaceAll(path, "{"+name+"...}", "x")
	}
	return path
}

// readOnlyProbeKey is the Idempotency-Key every probe below carries: a well-formed one, so a row
// that reached its keyed layer would claim it rather than refuse it.
const readOnlyProbeKey = "read-only-probe"

// presentingProbe is the request a caller sends route when it walks the table: the probe path and
// body (readOnlyProbePath, routeProbeBody), a valid Idempotency-Key, and token under the DPoP
// scheme with a fresh proof for that method and target.
func (c *brokerCaller) presentingProbe(t *testing.T, route apiRoute, token string) *http.Request {
	t.Helper()
	request := c.presenting(t, route.Method, readOnlyProbePath(route.Pattern), token, json.RawMessage(routeProbeBody(route)))
	request.Header.Set(contracts.IdempotencyKeyHeader, readOnlyProbeKey)
	return request
}

// notTheReadOnlyRefusal says how response differs from the read-only gate's refusal, and is empty
// when it is that refusal: 403 with the code READ_ONLY_TOKEN and no WWW-Authenticate challenge,
// since no scope makes the route the token's to call.
func notTheReadOnlyRefusal(response *httptest.ResponseRecorder) string {
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	switch {
	case response.Code != http.StatusForbidden || refusal.Code != contracts.DispatchReadOnlyToken:
		return "answered " + http.StatusText(response.Code) + " " + strings.TrimSpace(response.Body.String())
	case response.Header().Get("WWW-Authenticate") != "":
		return "carries the challenge " + response.Header().Get("WWW-Authenticate")
	}
	return ""
}

// readOnlyTable is the table register mounts with its test hooks in: every row a read-only token
// could be sent to on a server that serves the hooks, which is the most rows any server mounts.
func readOnlyTable() []apiRoute {
	return (&server{deps: Deps{TestHooksEnabled: true}}).routes()
}

// TestEveryRowTheReadListDoesNotNameRefusesAReadOnlyTokenBeforeItsHandler builds every row's own
// chain as register does (routeHandler) with the row's handler replaced by a recorder, and sends
// each a read-only token with a body and a valid Idempotency-Key. Every row the list does not name
// answers 403 READ_ONLY_TOKEN and never runs its handler; the row the list names runs it once.
// GET /ws/live is walked the same way through its own chain (liveSocketHandler), with the socket
// replaced. A row is counted once its assertions have run, and the count is held to the rows the
// registered server mounts (its route index, one entry a row), so a walk that skipped a row, or
// walked none, fails.
func TestEveryRowTheReadListDoesNotNameRefusesAReadOnlyTokenBeforeItsHandler(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRigOn(t, testServerOptions{testHooks: true})
	caller := newBrokerCaller(t)
	const token = "a-read-only-token-walking-every-chain"
	rig.broker.Says(token, readOnlyToken(caller.jkt))

	table := readOnlyTable()
	replays := rig.served.keyedReplays()
	walked, listed := 0, 0
	for _, route := range table {
		name := route.Method + " " + route.Pattern
		ran := 0
		route.Handler = func(w http.ResponseWriter, _ *http.Request) {
			ran++
			w.WriteHeader(http.StatusNoContent)
		}
		response := httptest.NewRecorder()
		rig.served.routeHandler(route, replays[name]).ServeHTTP(response, caller.presentingProbe(t, route, token))
		if route.ReadScope {
			listed++
			if ran != 1 || response.Code != http.StatusNoContent {
				t.Errorf("%s is on the read list, and its handler ran %d times (status=%d body=%s), want once",
					name, ran, response.Code, response.Body.String())
			}
		} else {
			if ran != 0 {
				t.Errorf("%s ran its handler %d times for a read-only token, want none", name, ran)
			}
			if why := notTheReadOnlyRefusal(response); why != "" {
				t.Errorf("%s %s, want 403 %s", name, why, contracts.DispatchReadOnlyToken)
			}
		}
		walked++
	}

	socketOpened := 0
	live := rig.served.liveSocketHandler(func(http.ResponseWriter, *http.Request) { socketOpened++ })
	response := httptest.NewRecorder()
	live.ServeHTTP(response, caller.presenting(t, http.MethodGet, liveSocketPattern, token, nil))
	if socketOpened != 0 {
		t.Errorf("GET %s opened the socket %d times for a read-only token, want none", liveSocketPattern, socketOpened)
	}
	if why := notTheReadOnlyRefusal(response); why != "" {
		t.Errorf("GET %s %s, want 403 %s", liveSocketPattern, why, contracts.DispatchReadOnlyToken)
	}

	if mounted := len(rig.served.routeIndex); walked == 0 || walked != mounted {
		t.Fatalf("walked %d rows, want every one of the %d the server mounts", walked, mounted)
	}
	if listed != len(readScopeRoutes) {
		t.Fatalf("%d rows carry the read list's mark, want the list's %d (%v)", listed, len(readScopeRoutes), readScopeRoutes)
	}
	t.Logf("walked %d rows of the table (test hooks in) and GET %s: %d listed, %d refused, every refusal %s",
		walked, liveSocketPattern, listed, walked-listed+1, contracts.DispatchReadOnlyToken)
}

// readListAnswers is what each route of the read list answers the mounted walk below at its probe
// path, which names a project with no architecture source and an issue Dispatch does not hold: its
// handler's own answer, which a request the gate refused could not carry. A route added to the
// list adds its answer here.
var readListAnswers = map[string]struct {
	status int
	code   string
}{
	"GET /api/v1/projects/{key}/architecture": {http.StatusNotFound, "SOURCE_NOT_FOUND"},
	"GET /api/v1/issues/{key}/steps":          {http.StatusNotFound, "NOT_FOUND"},
	"GET /api/v1/projects/{key}/plans":        {http.StatusNotFound, "PROJECT_NOT_FOUND"},
}

// TestTheMountedServerServesAReadOnlyTokenOnTheReadListAlone walks the server register mounted,
// test hooks in, with a read-only token: every row the list does not name and GET /ws/live answer
// 403 READ_ONLY_TOKEN; GET /ws/doc/{room}, which reads no broker token at all, answers 401; and
// each listed route answers its handler's own refusal of its probe path (readListAnswers).
// A row is counted once its assertions have run, and the count is held to the rows the
// registered server mounts, as the walk above is.
func TestTheMountedServerServesAReadOnlyTokenOnTheReadListAlone(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRigOn(t, testServerOptions{testHooks: true})
	caller := newBrokerCaller(t)
	const token = "a-read-only-token-walking-the-mounted-server"
	rig.broker.Says(token, readOnlyToken(caller.jkt))

	table := readOnlyTable()
	walked := 0
	for _, route := range table {
		name := route.Method + " " + route.Pattern
		response := rig.serve(caller.presentingProbe(t, route, token))
		if !route.ReadScope {
			if why := notTheReadOnlyRefusal(response); why != "" {
				t.Errorf("%s %s, want 403 %s", name, why, contracts.DispatchReadOnlyToken)
			}
		} else if want, known := readListAnswers[name]; !known {
			t.Errorf("%s is on the read list and readListAnswers says nothing of what it answers", name)
		} else if refusal := decodeBody[refusalAnswer](t, response); response.Code != want.status || refusal.Code != want.code {
			t.Errorf("%s answered %d %s, want its handler's %d %s", name, response.Code, response.Body.String(), want.status, want.code)
		}
		walked++
	}

	live := rig.serve(caller.presenting(t, http.MethodGet, liveSocketPattern, token, nil))
	if why := notTheReadOnlyRefusal(live); why != "" {
		t.Errorf("GET %s %s, want 403 %s", liveSocketPattern, why, contracts.DispatchReadOnlyToken)
	}
	room := "/ws/doc/00000000-0000-0000-0000-000000000001"
	if document := rig.serve(caller.presenting(t, http.MethodGet, room, token, nil)); document.Code != http.StatusUnauthorized {
		t.Errorf("GET %s answered %d %s, want 401: the document socket takes a cookie alone", room, document.Code, document.Body.String())
	}

	if mounted := len(rig.served.routeIndex); walked == 0 || walked != mounted {
		t.Fatalf("walked %d rows, want every one of the %d the server mounts", walked, mounted)
	}
	t.Logf("walked %d rows of the mounted table (test hooks in), GET %s and GET /ws/doc/{room}", walked, liveSocketPattern)
}

// registerRefusal is the refusal Register panics with on the table as it stands, and empty when
// it mounts the table.
func registerRefusal(t *testing.T) (refusal string) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			refusal, _ = recovered.(string)
			if refusal == "" {
				t.Fatalf("api.Register panicked with %v, want a refusal in words", recovered)
			}
		}
	}()
	Register(http.NewServeMux(), t.Context(), Deps{AgentTokens: sharedAgentTokens(t, "agent-token")})
	return ""
}

// TestRegisterRefusesAReadOnlyListOfNoMountedGETRow: a read list naming a write would open that
// write to a token minted only to read, and one naming no row reads as though it opened a route
// that is not there, so Register refuses either and names the route. It replaces the package's
// list for its length, so it is not parallel.
func TestRegisterRefusesAReadOnlyListOfNoMountedGETRow(t *testing.T) {
	kept := readScopeRoutes
	t.Cleanup(func() { readScopeRoutes = kept })
	if refusal := registerRefusal(t); refusal != "" {
		t.Fatalf("api.Register refused the read list as it stands: %s", refusal)
	}
	for why, name := range map[string]string{
		"a POST row":    "POST /api/v1/issues/{key}/comments",
		"no row at all": "GET /api/v1/projects/{key}/architecture/no-such-route",
	} {
		t.Run(why, func(t *testing.T) {
			readScopeRoutes = []string{name}
			refusal := registerRefusal(t)
			if !strings.Contains(refusal, "read list") || !strings.Contains(refusal, name) {
				t.Fatalf("api.Register with %s on the read list refused %q, want a refusal of the read list naming it", name, refusal)
			}
		})
	}
}

// TestTheReadOnlyListCheckNamesEveryRowThatIsNoMountedGET: the check register runs on the read
// list names, sorted, each name that is no row of the table and each row that is a write, and
// passes a GET row whether the deployment serves it or only a server with its test hooks in does.
func TestTheReadOnlyListCheckNamesEveryRowThatIsNoMountedGET(t *testing.T) {
	t.Parallel()
	if refused := readRoutesNotMountedAsGET(readScopeRoutes); len(refused) != 0 {
		t.Fatalf("the read list as it stands is refused: %v", refused)
	}
	got := readRoutesNotMountedAsGET([]string{
		"GET /api/v1/projects/{key}/architecture",
		"GET /api/v1/artifacts/_test/rooms",
		"POST /api/v1/issues/{key}/comments",
		"DELETE /api/v1/projects/{key}/legion/daemon",
		"PUT /api/v1/me/asks/{id}/snooze",
		"GET /api/v1/projects/{key}/architecture/no-such-route",
		"get /api/v1/projects/{key}/architecture",
	})
	want := []string{
		"DELETE /api/v1/projects/{key}/legion/daemon",
		"GET /api/v1/projects/{key}/architecture/no-such-route",
		"POST /api/v1/issues/{key}/comments",
		"PUT /api/v1/me/asks/{id}/snooze",
		"get /api/v1/projects/{key}/architecture",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("readRoutesNotMountedAsGET named %q, want %q", got, want)
	}
}

// TestAReadOnlyTokenIsRefusedBeforeTheIdempotencyKey: a read-only POST to a keyed write is refused
// READ_ONLY_TOKEN under a well-formed Idempotency-Key and claims no key, and under a malformed one
// is still refused READ_ONLY_TOKEN rather than the key layer's 400, so the key layer never read
// the header. A named-project token's POST under the same key then runs its write. That second
// write is no proof on its own - a key belongs to its caller (requestKeyLogin), so the named
// token's key would be free whatever the first request did - which is why the key table is read
// as well.
func TestAReadOnlyTokenIsRefusedBeforeTheIdempotencyKey(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const readOnly, named = "a-read-only-token-that-tries-to-write", "a-named-token-that-writes"
	rig.broker.Says(readOnly, readOnlyToken(caller.jkt))
	rig.broker.Says(named, liveNamedToken(caller.jkt))
	own := seedProject(t, rig, brokerTestProject, "Broker")
	if held := countRequestKeys(t, rig.database); held != 0 {
		t.Fatalf("the seed left %d request keys, want none before the test's own requests", held)
	}

	path := "/api/v1/issues/" + own.issue + "/comments"
	const key, body = "read-only-comment-press-1", "Written under the key a read-only token was refused under."
	post := func(token, key string) *httptest.ResponseRecorder {
		t.Helper()
		request := caller.presenting(t, http.MethodPost, path, token, map[string]any{"body": body})
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
		return rig.serve(request)
	}

	if why := notTheReadOnlyRefusal(post(readOnly, key)); why != "" {
		t.Fatalf("a read-only comment under a key %s, want 403 %s", why, contracts.DispatchReadOnlyToken)
	}
	if held := countRequestKeys(t, rig.database); held != 0 {
		t.Fatalf("the refused request left %d request keys, want none", held)
	}
	if why := notTheReadOnlyRefusal(post(readOnly, "not a key")); why != "" {
		t.Fatalf("a read-only comment under a malformed key %s, want 403 %s and not the key layer's %s",
			why, contracts.DispatchReadOnlyToken, contracts.IdempotencyKeyInvalid)
	}
	if held := countRequestKeys(t, rig.database); held != 0 {
		t.Fatalf("the refused request left %d request keys, want none", held)
	}

	written := post(named, key)
	if written.Code != http.StatusCreated {
		t.Fatalf("the named token's comment under the same key: status=%d body=%s, want 201", written.Code, written.Body.String())
	}
	var comments int
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select count(*) from comments where issue_key = $1 and body = $2`, own.issue, body).Scan(&comments); err != nil {
		t.Fatalf("count the comment: %v", err)
	}
	if comments != 1 {
		t.Fatalf("the issue holds %d of the comment, want the named token's one", comments)
	}
	if held := countRequestKeys(t, rig.database); held != 1 {
		t.Fatalf("the named token's write left %d request keys, want its own one: the key layer ran for it", held)
	}
}

// markedStatements is sentStatements (comment_window_tick_test.go) with a copy recorded too, as
// the insert it is: the SQL of every statement a traced pool runs on a context the test marked
// (countedRead, issue_read_batch_test.go), each statement of a batch on its own. That is the one
// request the test sends and the work it detaches from that request's context, and nothing the
// server does on a context of its own. The two calls below are pgx.CopyFromTracer's, which
// sentStatements does not answer: without them a copy would be a write no tracer call names.
type markedStatements struct{ sentStatements }

func (m *markedStatements) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromStartData) context.Context {
	m.record(ctx, "insert into "+data.TableName.Sanitize()+" (copy from)")
	return ctx
}

func (m *markedStatements) TraceCopyFromEnd(context.Context, *pgx.Conn, pgx.TraceCopyFromEndData) {}

var (
	// tableWrite is one insert, update, delete or merge and the table it writes.
	tableWrite = regexp.MustCompile(`(?i)\b(insert\s+into|update|delete\s+from|merge\s+into)\s+"?([a-z_][a-z0-9_."]*)`)
	// writeWord is every word that makes a statement a write, so a write tableWrite cannot read
	// shows as a word left over.
	writeWord = regexp.MustCompile(`(?i)\b(insert|update|delete|merge)\b`)
	// upsertUpdate is an insert's own conflict clause, whose table is the insert's.
	upsertUpdate = regexp.MustCompile(`(?i)\bdo\s+update\b`)
	// mergeInto opens a merge, whose actions (mergeAction) write the merge's own table.
	mergeInto   = regexp.MustCompile(`(?i)\bmerge\s+into\b`)
	mergeAction = regexp.MustCompile(`(?i)\bthen\s+(update|insert|delete)\b`)
	// updateWord is the word update wherever it stands; updateNoWrite decides where it is none.
	updateWord = regexp.MustCompile(`(?i)\bupdate\b`)
	// sourcesRead is the architecture's first read, of the project's architecture sources.
	sourcesRead = regexp.MustCompile(`(?i)\bfrom\s+architecture_sources\b`)
	// stepsRead is the steps read's one read, of the tuples that make each run part of its issue.
	stepsRead = regexp.MustCompile(`(?i)\bfrom\s+workflow_tuples\b`)
	// plansRead is the plans read's one read of the project's finished issues, joined through
	// their tuples to their runs and plans.
	plansRead = regexp.MustCompile(`(?is)\bfrom\s+issues\s+i\b.*\bworkflow_tuples\b`)
)

// writesOf answers each write statement makes, as "<verb> <table>", and false when it holds a
// write word no table can be read for: such a statement fails the test rather than passing it. It
// reads past comments where Postgres reads them (pgmigrate.WithoutComments, on the transcription
// of scan.l: a -- comment ends at a newline or a carriage return; none starts inside a string or a
// quoted name; and a dollar quote's body, a function's or a DO block's, is read as SQL, its own
// strings and dollar quotes kept whole, so a write inside one it EXECUTEs stays visible), reads a
// merge as a write of its table, and reads the word update as no write only where updateNoWrite
// places it, so a write it cannot place is a write.
func writesOf(statement string) ([]string, bool) {
	read := pgmigrate.WithoutComments(statement)
	read = upsertUpdate.ReplaceAllString(read, "do upsert")
	if mergeInto.MatchString(read) {
		read = mergeAction.ReplaceAllString(read, "then merge_action")
	}
	var unverbed strings.Builder
	last := 0
	for _, at := range updateWord.FindAllStringIndex(read, -1) {
		unverbed.WriteString(read[last:at[0]])
		if updateNoWrite(read, at) {
			unverbed.WriteString("update_word")
		} else {
			unverbed.WriteString(read[at[0]:at[1]])
		}
		last = at[1]
	}
	unverbed.WriteString(read[last:])
	read = unverbed.String()
	matches := tableWrite.FindAllStringSubmatch(read, -1)
	writes := make([]string, 0, len(matches))
	for _, match := range matches {
		verb := strings.ToLower(strings.Fields(match[1])[0])
		table := strings.ToLower(strings.ReplaceAll(match[2], `"`, ""))
		if dot := strings.LastIndexByte(table, '.'); dot >= 0 {
			table = table[dot+1:]
		}
		writes = append(writes, verb+" "+table)
	}
	return writes, len(writeWord.FindAllString(read, -1)) == len(matches)
}

// updateNoWrite reports whether the update read[at[0]:at[1]] is no write: a row lock, after for or
// for no key; or a column of that name (doc_updates.update), which stands after a dot, a comma, an
// open parenthesis or select, and before a comma, a close parenthesis, a cast, from, as or the
// statement's end. Every other update is the verb of a write, an update in a common table
// expression's parentheses included, since it is followed by its table.
func updateNoWrite(read string, at []int) bool {
	before := strings.TrimRight(read[:at[0]], " \t\r\n")
	after := strings.TrimLeft(read[at[1]:], " \t\r\n")
	previous := strings.ToLower(before[strings.LastIndexFunc(before, func(r rune) bool { return !isSQLWordRune(r) })+1:])
	if previous == "for" || previous == "key" {
		return true
	}
	afterColumn := strings.HasSuffix(before, ".") || strings.HasSuffix(before, ",") || strings.HasSuffix(before, "(") || previous == "select"
	next := strings.ToLower(after)
	beforeColumnEnd := after == "" || strings.HasPrefix(after, ",") || strings.HasPrefix(after, ")") || strings.HasPrefix(after, "::") ||
		startsWithSQLWord(next, "from") || startsWithSQLWord(next, "as")
	return afterColumn && beforeColumnEnd
}

func isSQLWordRune(r rune) bool {
	return r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// startsWithSQLWord reports whether s begins with the whole word word.
func startsWithSQLWord(s, word string) bool {
	return strings.HasPrefix(s, word) && (len(s) == len(word) || !isSQLWordRune(rune(s[len(word)])))
}

// writesOf reads a write by its verb and its table, and the word update by where it stands: a
// column of that name a document room reads (doc_updates.update) and a row lock are no write, an
// update statement however it names its table is one, a comment hides no write and a merge is one,
// and a write word it cannot place a table for leaves the statement unreadable.
func TestWritesOfReadsAnUpdateByItsSetClause(t *testing.T) {
	for _, tc := range []struct {
		statement string
		writes    []string
		readable  bool
	}{
		{`select update from doc_updates where artifact_id = $1 and version <= $2 order by version asc`, nil, true},
		{`select count(*), coalesce(sum(octet_length(d.update)), 0) from doc_updates d where d.artifact_id = $1`, nil, true},
		{`select id from issues where key = $1 for update`, nil, true},
		{`select id from issues where key = $1 for no key update skip locked`, nil, true},
		{`update projects set name = name where key = $1`, []string{"update projects"}, true},
		{`UPDATE "public"."projects" AS p SET name = p.name`, []string{"update projects"}, true},
		{`with moved as (update issues set rank = $2 where key = $1 returning key) select key from moved`, []string{"update issues"}, true},
		{`insert into text_trees (sum, tree) values ($1, $2) on conflict (sum) do update set tree = excluded.tree`, []string{"insert text_trees"}, true},
		{`delete from doc_updates where artifact_id = $1 and version <= $2`, []string{"delete doc_updates"}, true},
		{`insert /* a table this test cannot read */ ($1)`, []string{}, false},
		// A comment hides neither a write's verb nor its table, and a merge writes its table.
		{`update /* the project's name */ projects set name = name where key = $1`, []string{"update projects"}, true},
		{"-- the rename\nupdate projects set name = $2 where key = $1", []string{"update projects"}, true},
		{`merge into projects p using (select $1::text as key) s on p.key = s.key when matched then update set name = p.name`, []string{"merge projects"}, true},
		{`merge into projects using (select 1) s on false when not matched then insert (key, name) values ('A', 'A')`, []string{"merge projects"}, true},
		// An update whose table this test cannot place is no column: it leaves the statement unreadable.
		{`with x as (select 1) update`, []string{}, false},
		// A column named update in an insert's list is no write of its own.
		{`insert into doc_updates (artifact_id, version, update, content_changed) select $1, v, u, c from unnest($2::bigint[], $3::bytea[], $4::boolean[]) as a(v, u, c)`, []string{"insert doc_updates"}, true},
		// A comment starts only outside a quoted span: a `--` or `/*` inside a string, an escape
		// string, a quoted name or a dollar quote hides no write after it, and a parameter is no
		// dollar quote.
		{`select '--'; update projects set name = name`, []string{"update projects"}, true},
		{`select 'it''s -- no comment', '/*'; update projects set name = name`, []string{"update projects"}, true},
		{`select E'\'--'; update projects set name = name`, []string{"update projects"}, true},
		{`select "a--b" from projects; update projects set name = name`, []string{"update projects"}, true},
		{`select $$/*$$; update projects set name = name`, []string{"update projects"}, true},
		{`select $x$ -- $x$; update projects set name = name`, []string{"update projects"}, true},
		{`update projects set name = $1 -- the new name`, []string{"update projects"}, true},
		// A -- comment ends at a carriage return as at a newline, and a dollar quote's tag takes
		// bytes 0x80-0xff, as Postgres's scan.l reads them.
		{"select 1 -- c\rupdate projects set name = name", []string{"update projects"}, true},
		{`select $é$ -- $é$; update projects set name = name`, []string{"update projects"}, true},
		// A dollar quote inside a DO body is a string, kept whole, so a write in SQL the body
		// EXECUTEs stays visible, a -- before it included.
		{`do $$ begin execute $q$select 1 -- note update projects set name = name$q$; end $$`, []string{"update projects"}, true},
	} {
		writes, readable := writesOf(tc.statement)
		if readable != tc.readable || !slices.Equal(writes, tc.writes) {
			t.Errorf("writesOf(%q) = %v, %v; want %v, %v", tc.statement, writes, readable, tc.writes, tc.readable)
		}
	}
}

// readOnlyTreeFiles is the model the test below imports: two components whose prose no other test
// of this process writes, made unique to the run by nonce. pmdoc's display cache is the process's
// and a tree it holds is neither asked of the table nor stored, so prose another read had shown
// would leave the read below nothing to store.
func readOnlyTreeFiles(nonce string) map[string]string {
	return map[string]string{
		"reader.md": "---\ntitle: Reader\n---\nThe component a **read-only token** reads, run " + nonce + ".\n",
		"ledger.md": "---\ntitle: Ledger\nparent: reader\n---\nWhat the reader keeps, in prose of run " + nonce + " alone.\n",
	}
}

// TestTheReadOnlyListedRouteWritesOnlyItsProofAndCaches sends each route the read list names, with
// a read-only token, and reads every statement the request ran - its handler's and the work it
// detaches - through a tracer on the one pool the server and the verifier's replay store share.
// The architecture is read with Dispatch-Trees: 1, a cold display cache and a stale route_roster,
// the steps of an issue the daemon wrote a run of, and the plans of the project once that issue is
// done. Every insert, update and delete is to dpop_proofs, text_trees or route_roster: the proof's
// replay row and the two caches Dispatch rebuilds from what it holds. The positive controls are
// each proof's insert, a tree's insert, the read of architecture_sources, the read of
// workflow_tuples and the read of the project's finished issues, so a tracer that saw nothing
// cannot pass.
func TestTheReadOnlyListedRouteWritesOnlyItsProofAndCaches(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	marked := &markedStatements{}
	traced := storetest.TracedStore(t, database, marked)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	const project, repo = "RDR", "reader/arch"
	fake := &fakeGitHubApp{
		installations: map[string]string{repo: "read"},
		repos:         map[string]*fakeArchitectureRepo{repo: {commit: shaOne, files: readOnlyTreeFiles(rand.Text())}},
	}
	github := githubtest.Server(t, githubtest.Spec{Handle: fake.handler(t).ServeHTTP})
	rig := newBrokerTokenRigOn(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: github,
		store:         traced,
	})
	caller := newBrokerCaller(t)
	const token = "a-read-only-token-reading-the-architecture"
	rig.broker.Says(token, readOnlyToken(caller.jkt))

	createTestProject(t, rig.handler, project)
	putSource(t, rig.handler, project, repo, "main")
	if synced := syncSource(t, rig.handler, project, repo); synced.LastError != nil {
		t.Fatalf("sync rejected the model: %s", *synced.LastError)
	}

	// The import stored every component's tree (architecture's storeComponentTrees); those rows
	// go, so the read finds the table as cold as the cache and stores what it read.
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `select prose from components where project_key = $1`, project)
	if err != nil {
		t.Fatalf("read the components' prose: %v", err)
	}
	proses, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the components' prose: %v", err)
	}
	if len(proses) != len(fake.repos[repo].files) {
		t.Fatalf("the import holds %d components, want the model's %d", len(proses), len(fake.repos[repo].files))
	}
	sums := make([][]byte, len(proses))
	for index, prose := range proses {
		sum := pmdoc.TextSum(prose)
		sums[index] = sum[:]
	}
	storedTrees := func() int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from text_trees where sum = any($1)`, sums).Scan(&count); err != nil {
			t.Fatalf("count the prose's trees: %v", err)
		}
		return count
	}
	if stored := storedTrees(); stored != len(sums) {
		t.Fatalf("the import stored %d of the prose's %d trees, want every one", stored, len(sums))
	}
	if _, err := database.Pool.Exec(ctx, `delete from text_trees where sum = any($1)`, sums); err != nil {
		t.Fatalf("delete the prose's trees: %v", err)
	}
	// A roster an hour past its expiry, which any read of the roster would refresh.
	if _, err := database.Pool.Exec(ctx, `
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, '[]'::jsonb, now() - interval '2 hours', now() - interval '1 hour')
		on conflict (id) do update
		set answered = excluded.answered, sessions = excluded.sessions,
		    sent_at = excluded.sent_at, expires_at = excluded.expires_at`); err != nil {
		t.Fatalf("make the route roster stale: %v", err)
	}

	request := caller.presenting(t, http.MethodGet, "/api/v1/projects/"+project+"/architecture", token, nil)
	request = request.WithContext(context.WithValue(request.Context(), countedRead{}, true))
	request.Header.Set(displayTreesHeader, "1")
	response := rig.serve(request)
	if response.Code != http.StatusOK {
		t.Fatalf("the read-only token's read of the architecture: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	answer := decodeBody[architectureTreeRead](t, response)
	for _, component := range answer.Components {
		if len(component.ProseTree) == 0 {
			t.Fatalf("component %s came back with no prose tree: %s", component.ID, response.Body.String())
		}
	}

	// The trees are stored beside the request, on a goroutine of the server's background work, so
	// the statements are read once the rows it stores are back.
	if !testwait.Holds(10*time.Second, 20*time.Millisecond, func() bool { return storedTrees() == len(sums) }) {
		t.Fatalf("the read stored %d of the prose's %d trees within 10s, want every one", storedTrees(), len(sums))
	}

	statements := marked.take()
	if !requireOnlyProofAndCaches(t, statements, sourcesRead) {
		t.Errorf("the tracer saw no insert into text_trees among %d statements: it did not see the trees the read stored", len(statements))
	}

	// The steps of an issue: the daemon writes one run, over its own credential and outside the
	// counted read, and the read-only token reads it back.
	issue := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{"project": project, "title": "A run to read"})
	if issue.Code != http.StatusCreated {
		t.Fatalf("create an issue in %s: status=%d body=%s", project, issue.Code, issue.Body.String())
	}
	issueKey := decodeBody[struct {
		Key string `json:"key"`
	}](t, issue).Key
	putStepOK(t, rig.handler, issueKey, "6f1d2c3b-4a59-4e7d-8c6b-5a4f3e2d1c0b", implementerStart(daemonSessionOf(project)))
	marked.take()
	request = caller.presenting(t, http.MethodGet, "/api/v1/issues/"+issueKey+"/steps", token, nil)
	request = request.WithContext(context.WithValue(request.Context(), countedRead{}, true))
	response = rig.serve(request)
	if read := decodeBody[model.IssueSteps](t, response); response.Code != http.StatusOK || len(read.Tasks) != 1 {
		t.Fatalf("the read-only token's read of the steps of %s: status=%d body=%s, want 200 and its one task", issueKey, response.Code, response.Body.String())
	}
	requireOnlyProofAndCaches(t, marked.take(), stepsRead)

	// The plans of the project, once the issue is done: the person closes it and the daemon
	// writes its plan, outside the counted read, and the read-only token lists it.
	if closed := rig.asPerson(t, http.MethodPatch, "/api/v1/issues/"+issueKey, map[string]any{"status": "done"}); closed.Code != http.StatusOK {
		t.Fatalf("close %s: status=%d body=%s", issueKey, closed.Code, closed.Body.String())
	}
	putPlanRecordOK(t, rig.handler, issueKey, "0c4b2f7e-3d1a-4b8c-9e6f-1a2b3c4d5e6f", defaultPlanBody(daemonSessionOf(project), 1))
	marked.take()
	request = caller.presenting(t, http.MethodGet, "/api/v1/projects/"+project+"/plans", token, nil)
	request = request.WithContext(context.WithValue(request.Context(), countedRead{}, true))
	response = rig.serve(request)
	if read := decodeBody[model.ProjectPlans](t, response); response.Code != http.StatusOK || len(read.Issues) != 1 || read.Issues[0].Plan == nil {
		t.Fatalf("the read-only token's read of the plans of %s: status=%d body=%s, want 200 and %s with its plan", project, response.Code, response.Body.String(), issueKey)
	}
	requireOnlyProofAndCaches(t, marked.take(), plansRead)
}

// requireOnlyProofAndCaches fails unless every write of statements is to the proof's replay row or
// a cache Dispatch rebuilds, the proof's insert among them, and handlerRead, the handler's own
// read, is among them too. It answers whether a tree was stored.
func requireOnlyProofAndCaches(t *testing.T, statements []string, handlerRead *regexp.Regexp) bool {
	t.Helper()
	allowed := map[string]bool{"dpop_proofs": true, "text_trees": true, "route_roster": true}
	var proofInserted, treeInserted, readByHandler bool
	var written []string
	for _, statement := range statements {
		writes, readable := writesOf(statement)
		if !readable {
			t.Errorf("the request ran a write this test cannot read a table for: %s", statement)
		}
		for _, write := range writes {
			written = append(written, write)
			verb, table, _ := strings.Cut(write, " ")
			if !allowed[table] {
				t.Errorf("the request wrote %s, which is neither its proof's replay row nor a cache Dispatch rebuilds: %s", table, statement)
			}
			proofInserted = proofInserted || (verb == "insert" && table == "dpop_proofs")
			treeInserted = treeInserted || (verb == "insert" && table == "text_trees")
		}
		readByHandler = readByHandler || handlerRead.MatchString(statement)
	}
	if !proofInserted {
		t.Errorf("the tracer saw no insert into dpop_proofs among %d statements: it did not see the request's proof spent", len(statements))
	}
	if !readByHandler {
		t.Errorf("the tracer saw no statement matching %s among %d statements: it did not see the handler's reads", handlerRead, len(statements))
	}
	t.Logf("the request ran %d statements, whose writes were %v", len(statements), written)
	return treeInserted
}
