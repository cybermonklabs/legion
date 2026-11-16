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
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
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

// readListAnswers is what each route of the read list answers the mounted walk below for a
// project with no architecture source: its handler's own answer, which a request the gate refused
// could not carry. A route added to the list adds its answer here.
var readListAnswers = map[string]struct {
	status int
	code   string
}{
	"GET /api/v1/projects/{key}/architecture": {http.StatusNotFound, "SOURCE_NOT_FOUND"},
}

// TestTheMountedServerServesAReadOnlyTokenOnTheReadListAlone walks the server register mounted,
// test hooks in, with a read-only token: every row the list does not name and GET /ws/live answer
// 403 READ_ONLY_TOKEN; GET /ws/doc/{room}, which reads no broker token at all, answers 401; and
// the listed route answers its handler's own 404 SOURCE_NOT_FOUND for a project with no source.
// A row is counted once its assertions have run, and the count is held to the rows the
// registered server mounts, as the walk above is.
func TestTheMountedServerServesAReadOnlyTokenOnTheReadListAlone(t *testing.T) {
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
	if refused := readRoutesNotMountedAsGET(readScopeRoutes); len(refused) != 0 {
		t.Fatalf("the read list as it stands is refused: %v", refused)
	}
	got := readRoutesNotMountedAsGET([]string{
		"GET /api/v1/projects/{key}/architecture",
		"GET /api/v1/artifacts/_test/rooms",
		"POST /api/v1/issues/{key}/comments",
		"DELETE /api/v1/settings/repo-projects/{owner}/{repo}",
		"PUT /api/v1/me/asks/{id}/snooze",
		"GET /api/v1/projects/{key}/architecture/no-such-route",
		"get /api/v1/projects/{key}/architecture",
	})
	want := []string{
		"DELETE /api/v1/settings/repo-projects/{owner}/{repo}",
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
	// tableWrite is one insert, update or delete and the table it writes.
	tableWrite = regexp.MustCompile(`(?i)\b(insert\s+into|update|delete\s+from)\s+"?([a-z_][a-z0-9_."]*)`)
	// writeWord is every word that makes a statement a write, so a write tableWrite cannot read
	// shows as a word left over.
	writeWord = regexp.MustCompile(`(?i)\b(insert|update|delete)\b`)
	// upsertUpdate is an insert's own conflict clause, whose table is the insert's.
	upsertUpdate = regexp.MustCompile(`(?i)\bdo\s+update\b`)
	// sourcesRead is the listed route's first read, of the project's architecture sources.
	sourcesRead = regexp.MustCompile(`(?i)\bfrom\s+architecture_sources\b`)
)

// writesOf answers each write statement makes, as "<verb> <table>", and false when it holds a
// write word no table can be read for: such a statement fails the test rather than passing it.
func writesOf(statement string) ([]string, bool) {
	read := upsertUpdate.ReplaceAllString(statement, "do upsert")
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

// TestTheReadOnlyListedRouteWritesOnlyItsProofAndCaches sends the route the read list names, with a
// read-only token, Dispatch-Trees: 1, a cold display cache and a stale route_roster, and reads
// every statement the request ran - its handler's and the tree store it detaches - through a
// tracer on the one pool the server and the verifier's replay store share. Every insert, update
// and delete is to dpop_proofs, text_trees or route_roster: the proof's replay row and the two
// caches Dispatch rebuilds from what it holds. The positive controls are the proof's insert, a
// tree's insert and the read of architecture_sources, so a tracer that saw nothing cannot pass.
func TestTheReadOnlyListedRouteWritesOnlyItsProofAndCaches(t *testing.T) {
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
	allowed := map[string]bool{"dpop_proofs": true, "text_trees": true, "route_roster": true}
	var proofInserted, treeInserted, readSources bool
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
		readSources = readSources || sourcesRead.MatchString(statement)
	}
	if !proofInserted {
		t.Errorf("the tracer saw no insert into dpop_proofs among %d statements: it did not see the request's proof spent", len(statements))
	}
	if !treeInserted {
		t.Errorf("the tracer saw no insert into text_trees among %d statements: it did not see the trees the read stored", len(statements))
	}
	if !readSources {
		t.Errorf("the tracer saw no read of architecture_sources among %d statements: it did not see the handler's reads", len(statements))
	}
	t.Logf("the request ran %d statements, whose writes were %v", len(statements), written)
}
