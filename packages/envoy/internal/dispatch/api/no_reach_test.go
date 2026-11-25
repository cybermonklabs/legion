package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// personNodesRoute is the one route the node and tuple store adds to the table: a person's own
// page. Everything else this child did is behind reads the table already held.
const personNodesRoute = "GET /api/v1/people/{login}/nodes"

// traversalWords are the words a caller would hand a route to walk the graph itself: a node
// kind, a filter, an edge, a direction, a depth. The person read takes none of them, and the
// three bad requests below send every one of them to every route to prove no route takes them
// from a caller either.
var traversalWords = []string{
	"depth", "direction", "edge", "edges", "filter", "kind", "kinds",
	"node", "nodes", "relation", "relations", "traverse", "via",
}

// contractsModule and contractsGenerated are the two files the dashboard's wire shapes are
// declared in, relative to the repository root: the module every consumer imports, and the
// module generated from the route table that it re-exports whole.
const (
	contractsModule    = "packages/contracts/src/dispatch-api.ts"
	contractsGenerated = "packages/contracts/src/dispatch-api.generated.ts"
)

// TestTheStoreAddsNoReachAndNoReadChangesShape is this child's negative control. The node and
// tuple store is a second way to hold what Dispatch already held, so the risk it carries is
// reach: a new read that answers more than the read it stands beside, or an old read that
// quietly answers something else. Each sub-test holds one half of that.
func TestTheStoreAddsNoReachAndNoReadChangesShape(t *testing.T) {
	t.Parallel()
	t.Run("the person read is one route and takes no kind, filter or traversal", onePersonRead)
	t.Run("the person read admits a person and nobody else", personReadReach)
	t.Run("every route the table held before this child keeps the shape the contracts declare", declaredShapesUnchanged)
	t.Run("a declared shape is not an authorization", threeBadRequests)
	t.Run("a build that cannot address the new kinds reads what it read before", olderBuildReadsTheGraph)
	t.Run("the comment read and the block read are unchanged, in answer and in plan", readsUnchanged)
	t.Run("an ask's followers who never commented read the same before and after the backfill", followersAcrossTheBackfill)
}

// onePersonRead: the store's one new read is one row of the table, with its own description and
// its own auth mode, and nothing in it lets a caller choose what the store is asked.
func onePersonRead(t *testing.T) {
	rows := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	var found []apiRoute
	for _, route := range rows {
		if strings.HasPrefix(route.Pattern, "/api/v1/people/") {
			found = append(found, route)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the table holds %d person route(s), want exactly one: %v", len(found), routeNames(found))
	}
	read := found[0]
	if name := read.Method + " " + read.Pattern; name != personNodesRoute {
		t.Fatalf("the person read is %q, want %q", name, personNodesRoute)
	}
	if strings.TrimSpace(read.Description) == "" {
		t.Fatal("the person read carries no description")
	}
	for _, other := range rows {
		if other.Pattern != read.Pattern && other.Description == read.Description {
			t.Fatalf("%s %s shares its description with the person read", other.Method, other.Pattern)
		}
	}
	if read.Auth != authAny {
		t.Fatalf("the person read is %q, want %q: a person reaches it and nobody else", read.Auth, authAny)
	}
	if want := []string{model.ScopePerson}; !slices.Equal([]string(read.OAuthScopes), want) {
		t.Fatalf("the person read needs %v, want %v, which is how gateRouteScopes refuses the shared token",
			read.OAuthScopes, want)
	}
	// What a caller may hand it: the login the path names, and the page. A node kind, a filter
	// or an edge to walk is what this route exists not to take.
	shape, shaped := routeShapes[personNodesRoute]
	if !shaped {
		t.Fatalf("%s declares no shape", personNodesRoute)
	}
	keys := queryTagsOf(shape.Params)
	sort.Strings(keys)
	if want := []string{"limit", "offset"}; !slices.Equal(keys, want) {
		t.Fatalf("the person read takes the query %v, want %v", keys, want)
	}
}

// personReadReach: who the person read answers. The deployment's shared agent bearer holds no
// scope and is refused before the handler runs; an anonymous caller is refused earlier still;
// a person's own credential reads their page; and a login the store holds no person for is a
// 404 that says nothing about whether that person exists.
func personReadReach(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A person's page", "Spec.\n")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": "Alice wrote this."}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("seed a comment: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)
	personal := personalTokenHolding(t, database, model.ScopePerson)
	const page = "/api/v1/people/alice/nodes"

	anonymous := personPageRequest(t, handler, page, "")
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("an anonymous caller: status=%d body=%s, want 401", anonymous.Code, anonymous.Body.String())
	}
	shared := personPageRequest(t, handler, page, "agent-token")
	if shared.Code != http.StatusForbidden {
		t.Fatalf("the shared agent bearer: status=%d body=%s, want 403", shared.Code, shared.Body.String())
	}
	if refusal := decodeBody[refusalAnswer](t, shared); refusal.Code != codeScopeRequired {
		t.Fatalf("the shared agent bearer is refused %+v, want %s", refusal, codeScopeRequired)
	}

	answered := personPageRequest(t, handler, page, personal)
	if answered.Code != http.StatusOK {
		t.Fatalf("a person-scoped credential: status=%d body=%s, want 200", answered.Code, answered.Body.String())
	}
	read := decodeBody[personNodes](t, answered)
	if read.Person != "alice" || read.Total != 1 || len(read.Rows) != 1 {
		t.Fatalf("the page = %+v, want alice's one authored comment", read)
	}
	row := read.Rows[0]
	if row.Kind != "comment" || row.ID != comment.ID {
		t.Fatalf("the row = %+v, want the comment %s", row, comment.ID)
	}
	if !slices.Equal(row.Relations, []string{"authored_by"}) {
		t.Fatalf("the row's relations = %v, want authored_by alone", row.Relations)
	}
	if want := "/issues/" + issue.Key + "/comments/" + comment.ID; row.Href != want {
		t.Fatalf("the row opens at %q, want %q", row.Href, want)
	}
	// The same page through the dashboard's own credential, which is the way a person reaches it.
	if cookie := dispatchRequest(t, handler, http.MethodGet, page, nil, "alice"); cookie.Code != http.StatusOK {
		t.Fatalf("alice reading her own page: status=%d body=%s, want 200", cookie.Code, cookie.Body.String())
	}

	// An unknown login, and a login that has signed in but whose person the store does not
	// hold, answer the same 404: the read says nothing about which of the two it met.
	unknown := personPageRequest(t, handler, "/api/v1/people/nobody-at-all/nodes", personal)
	signedIn := personPageRequest(t, handler, "/api/v1/people/bob/nodes", personal)
	for _, answer := range []*httptest.ResponseRecorder{unknown, signedIn} {
		if answer.Code != http.StatusNotFound {
			t.Fatalf("an unknown login: status=%d body=%s, want 404", answer.Code, answer.Body.String())
		}
	}
	if unknown.Body.String() != signedIn.Body.String() {
		t.Fatalf("the two 404s differ:\n %s\n %s", unknown.Body.String(), signedIn.Body.String())
	}
	if body := unknown.Body.String(); strings.Contains(body, "nobody-at-all") || strings.Contains(body, "person") {
		t.Fatalf("the 404 tells the caller something: %s", body)
	}

	// The caller's own login is the one login with no node that is no 404: a person who has
	// written nothing and been reached by nothing opens their own page from the signed-in login
	// and reads it empty, which tells them nothing they did not know. Another person's credential
	// still reads that same login as the 404 above.
	carol := personalTokenOwnedBy(t, database, "carol", model.ScopePerson)
	for _, own := range []*httptest.ResponseRecorder{
		personPageRequest(t, handler, "/api/v1/people/carol/nodes", carol),
		dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/Carol/nodes", nil, "carol"),
	} {
		if own.Code != http.StatusOK {
			t.Fatalf("carol reading her own page with no node: status=%d body=%s, want 200", own.Code, own.Body.String())
		}
		if read := decodeBody[personNodes](t, own); read.Person != "carol" || read.Total != 0 || len(read.Rows) != 0 {
			t.Fatalf("carol's own page with no node = %+v, want carol's page with no rows", read)
		}
	}
	if other := personPageRequest(t, handler, "/api/v1/people/carol/nodes", personal); other.Code != http.StatusNotFound ||
		other.Body.String() != unknown.Body.String() {
		t.Fatalf("alice reading carol's page with no node: status=%d body=%s, want the same 404 an unknown login reads",
			other.Code, other.Body.String())
	}
}

// followersAcrossTheBackfill: an ask opened by a session that never commented, and followed by a
// second such session, answers the same followed_by edges from GET /api/v1/references and the
// same Follower rows (GET /api/v1/asks/{id}/followers) before and after the backfill. A session
// is a node of the store from the first write that records it, and an ask it opened or follows is
// such a write, so neither follower edge needs a comment to answer a node. The case is seeded
// twice, so each path is proved on its own: one ask opened and followed through this release's
// API, which proves the node the follow writes (asks.Follow), and one written as the previous
// release writes it, rows alone, which proves the backfill. The previous release built each
// follower's node out of its row's session id, so what it answered for the second ask is one
// followed_by edge per follower row, and that is what this release answers once the backfill has
// given those sessions their nodes.
func followersAcrossTheBackfill(t *testing.T) {
	ctx := context.Background()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Followers", "Spec.\n")
	asker := map[string]any{"kind": "session", "id": "019a1c0c-1111-7000-8000-000000000001"}
	follower := "019a1c0c-1111-7000-8000-000000000002"
	opened := decodeBody[model.Ask](t, created(t, "a session's ask", sessionRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+issue.Key+"/asks", map[string]any{"question": "Who follows?", "actor": asker})))
	if followed := sessionRequest(t, handler, http.MethodPut, "/api/v1/asks/"+opened.ID+"/followers/"+follower,
		map[string]any{"actor": map[string]any{"kind": "session", "id": follower}}); followed.Code != http.StatusNoContent {
		t.Fatalf("follow the ask as %s: status=%d body=%s", follower, followed.Code, followed.Body.String())
	}

	// The second ask as the previous release writes it: the ask row and its two follower rows,
	// and no node of either session.
	previousAsker, previousFollower := "019a1c0c-2222-7000-8000-000000000001", "019a1c0c-2222-7000-8000-000000000002"
	var previous string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question) values ($1, $2, 'Who followed before?') returning id::text
	`, issue.Key, `{"kind":"session","id":"`+previousAsker+`"}`).Scan(&previous); err != nil {
		t.Fatalf("write the previous release's ask: %v", err)
	}
	for _, session := range []string{previousAsker, previousFollower} {
		if _, err := database.Pool.Exec(ctx,
			`insert into ask_followers (ask_id, session_id) values ($1, $2)`, previous, session); err != nil {
			t.Fatalf("write the previous release's follower %s: %v", session, err)
		}
	}

	type answer struct{ edges, followers string }
	read := func(ask string) answer {
		t.Helper()
		edges := readGraph(t, handler, url.Values{"from": {"dispatch://" + issue.Key + "/ask/" + ask}, "kind": {"followed_by"}}).Edges
		encodedEdges, err := json.Marshal(edges)
		if err != nil {
			t.Fatalf("encode the followed_by edges of %s: %v", ask, err)
		}
		listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask+"/followers", nil, "alice")
		if listed.Code != http.StatusOK {
			t.Fatalf("list the followers of %s: status=%d body=%s", ask, listed.Code, listed.Body.String())
		}
		return answer{edges: string(encodedEdges), followers: listed.Body.String()}
	}
	followedBy := func(ask string) []string {
		t.Helper()
		var sessions []string
		for _, edge := range readGraph(t, handler, url.Values{"from": {"dispatch://" + issue.Key + "/ask/" + ask}, "kind": {"followed_by"}}).Edges {
			sessions = append(sessions, edge.Node.Kind+":"+edge.Node.ID)
		}
		sort.Strings(sessions)
		return sessions
	}
	followerRows := func(ask string) []string {
		t.Helper()
		var sessions []string
		for _, row := range decodeBody[askFollowersRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask+"/followers", nil, "alice")).Followers {
			sessions = append(sessions, "session:"+row.SessionID)
		}
		sort.Strings(sessions)
		return sessions
	}

	apiBefore, previousBefore := read(opened.ID), read(previous)
	if got, want := followedBy(opened.ID), []string{"session:" + asker["id"].(string), "session:" + follower}; !slices.Equal(got, want) {
		t.Fatalf("the ask opened and followed through the API answers followed_by %v before the backfill, want %v", got, want)
	}
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	apiAfter, previousAfter := read(opened.ID), read(previous)
	if apiAfter != apiBefore {
		t.Fatalf("the ask opened through the API reads differently after the backfill:\nbefore %+v\nafter  %+v", apiBefore, apiAfter)
	}
	if previousAfter.followers != previousBefore.followers {
		t.Fatalf("the previous release's ask lists other Follower rows after the backfill:\nbefore %s\nafter  %s",
			previousBefore.followers, previousAfter.followers)
	}
	if got, want := followedBy(previous), followerRows(previous); len(want) != 2 || !slices.Equal(got, want) {
		t.Fatalf("the previous release's ask answers followed_by %v after the backfill, want one edge per Follower row %v, as the previous release answered them",
			got, want)
	}
}

// personPageRequest reads one route with one credential: the empty one is an anonymous caller.
func personPageRequest(t *testing.T, handler http.Handler, target, credential string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func routeNames(routes []apiRoute) []string {
	names := make([]string, len(routes))
	for i, route := range routes {
		names[i] = route.Method + " " + route.Pattern
	}
	return names
}

// routesBeforeThisChild is every row of the table but the one this child added.
func routesBeforeThisChild(t *testing.T, testHooks bool) []apiRoute {
	t.Helper()
	var rows []apiRoute
	for _, route := range (&server{deps: Deps{TestHooksEnabled: testHooks}}).routes() {
		if route.Method+" "+route.Pattern == personNodesRoute {
			continue
		}
		rows = append(rows, route)
	}
	if len(rows) < 100 {
		t.Fatalf("the table holds %d rows before this child, which is too few to be the table", len(rows))
	}
	return rows
}

// declaredShapesUnchanged walks every route the table held before this child and reads the shape
// it answers out of what the contracts package declares.
//
// The chain is three links, and the test holds each: the emitter renders the table into
// `schemas/dispatch-openapi.json`, so a committed document that differs from a fresh render is a
// table whose shapes nobody published; `dispatch-api.generated.ts` is generated from that
// document, so every route and every schema of the document must be declared there, field for
// field; and `dispatch-api.ts` re-exports that module whole, so what a consumer imports is what
// the table answers. A field dropped from a Go type, or a module left stale beside a table that
// moved, breaks one of the three.
func declaredShapesUnchanged(t *testing.T) {
	root := repositoryRoot(t)
	document := committedDocument(t, root)
	module := readRepositoryFile(t, root, contractsModule)
	if !strings.Contains(module, `export * from "./dispatch-api.generated";`) {
		t.Fatalf("%s no longer re-exports the generated module, so what it declares is not what the table answers",
			contractsModule)
	}
	generated := readRepositoryFile(t, root, contractsGenerated)
	declared := schemasOfGeneratedModule(t, generated)
	owned := splitOwnedSchemas(generated)

	for _, route := range routesBeforeThisChild(t, true) {
		operations, held := document.Paths[route.Pattern]
		if !held {
			t.Errorf("%s %s is in the table and in no path of the document", route.Method, route.Pattern)
			continue
		}
		if _, served := operations[strings.ToLower(route.Method)]; !served {
			t.Errorf("%s %s is in the table and the document declares no %s for it",
				route.Method, route.Pattern, strings.ToLower(route.Method))
		}
		if !strings.Contains(generated, `  "`+route.Pattern+`": {`) {
			t.Errorf("%s %s is in the table and %s declares no path for it",
				route.Method, route.Pattern, contractsGenerated)
		}
	}
	for name, schema := range document.Components.Schemas {
		fields, held := declared[name]
		if !held {
			t.Errorf("the document declares the schema %s, which %s does not", name, contractsGenerated)
			continue
		}
		want := slices.Sorted(maps.Keys(schema.Properties))
		if !slices.Equal(fields, want) {
			t.Errorf("%s declares %s as %v, and the route table answers %v", contractsGenerated, name, fields, want)
		}
		if !owned[name] && !generatedAlias(name).MatchString(generated) {
			t.Errorf("%s declares %s, exports no alias for it and does not name it as the split's, so no consumer imports it",
				contractsGenerated, name)
		}
	}
}

// committedOpenAPIShapes is the committed document as this test reads it: what each path serves,
// and each schema as far as an answer is held to it. The name is the shapes and not the document
// because committedOpenAPI is already this package's reader of the document's own bytes
// (openapi_test.go).
type committedOpenAPIShapes struct {
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]openAPISchema `json:"schemas"`
	} `json:"components"`
}

// openAPISchema is one schema of the committed document, as far as an answer is held to it: the
// fields an object declares and the ones it always carries, the schema of an array's rows, the
// branches of a union, and the component a reference names.
type openAPISchema struct {
	Ref        string                   `json:"$ref"`
	Properties map[string]openAPISchema `json:"properties"`
	Required   []string                 `json:"required"`
	Items      *openAPISchema           `json:"items"`
	AnyOf      []openAPISchema          `json:"anyOf"`
	OneOf      []openAPISchema          `json:"oneOf"`
}

// committedDocument is the OpenAPI document the contracts package is generated from, held to a
// fresh render of the route table first: a committed document that differs from the render is a
// table whose shapes nobody published, and every comparison below would be against the wrong
// thing.
func committedDocument(t *testing.T, root string) committedOpenAPIShapes {
	t.Helper()
	var rendered bytes.Buffer
	if err := WriteOpenAPI(&rendered); err != nil {
		t.Fatalf("render the OpenAPI document: %v", err)
	}
	committedPath := filepath.Join(root, filepath.FromSlash(OpenAPIDocumentPath))
	committed, err := os.ReadFile(committedPath)
	if err != nil {
		t.Fatalf("read the committed OpenAPI document: %v", err)
	}
	if !bytes.Equal(committed, rendered.Bytes()) {
		t.Fatalf("%s is not what the table renders: run `envoy-dispatch openapi --out %s`",
			OpenAPIDocumentPath, OpenAPIDocumentPath)
	}
	var document committedOpenAPIShapes
	if err := json.Unmarshal(committed, &document); err != nil {
		t.Fatalf("decode the committed OpenAPI document: %v", err)
	}
	return document
}

// generatedAlias matches the exported alias of one schema. Biome wraps a long one onto a
// second line, so the two halves are matched across whatever whitespace stands between them.
func generatedAlias(name string) *regexp.Regexp {
	return regexp.MustCompile(`export type ` + regexp.QuoteMeta(name) + ` =\s+` +
		regexp.QuoteMeta(`components["schemas"]["`+name+`"];`))
}

// splitOwnedSchema matches one name of the generated module's SplitOwnedSchemaName union.
var splitOwnedSchema = regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_]*)"`)

// splitOwnedSchemas is the schema names the generated module says the hand-written split owns.
// The generator exports no alias for those: dispatch-api.ts re-exports the generated module and
// the split together, and one name cannot come from both, so the split's own export is what a
// consumer imports (packages/contracts/scripts/gen-api.ts). The generator renders the list into
// the module as that union, and dispatch-api-owned.ts holds the split's reading of each name to
// the document's reading at compile time, so a Go change that moves one of these shapes reds the
// typecheck by the name it moved.
//
// The list is read out of the module rather than written here, so a name the generator hands
// back goes straight back to needing an alias of its own, and a module that renders no union at
// all leaves every name needing one.
func splitOwnedSchemas(generated string) map[string]bool {
	owned := map[string]bool{}
	start := strings.Index(generated, "export type SplitOwnedSchemaName =")
	if start < 0 {
		return owned
	}
	union := generated[start:]
	if end := strings.Index(union, ";"); end >= 0 {
		union = union[:end]
	}
	for _, match := range splitOwnedSchema.FindAllStringSubmatch(union, -1) {
		owned[match[1]] = true
	}
	return owned
}

// generatedSchemaName matches a schema's own line in the generated module: the names sit at one
// indent inside `components.schemas`, and their fields one further in.
var (
	generatedSchemaName  = regexp.MustCompile(`^    ([A-Za-z_][A-Za-z0-9_]*)\??: `)
	generatedSchemaField = regexp.MustCompile(`^      "?([A-Za-z_][A-Za-z0-9_$-]*)"?\??:(\s|$)`)
)

// schemasOfGeneratedModule reads the generated module's own declarations: every schema it holds
// and the field names of each. The module is Biome-formatted, so a schema's name sits at four
// spaces and its fields at six, and a field whose value is an object nests deeper and is no
// field of the schema itself.
func schemasOfGeneratedModule(t *testing.T, source string) map[string][]string {
	t.Helper()
	lines := strings.Split(source, "\n")
	start := slices.Index(lines, "  schemas: {")
	if start < 0 {
		t.Fatalf("%s declares no components.schemas block", contractsGenerated)
	}
	schemas := map[string][]string{}
	current := ""
	for _, line := range lines[start+1:] {
		if line == "  };" {
			break
		}
		if name := generatedSchemaName.FindStringSubmatch(line); name != nil {
			current = name[1]
			schemas[current] = []string{}
			continue
		}
		if field := generatedSchemaField.FindStringSubmatch(line); field != nil && current != "" {
			schemas[current] = append(schemas[current], field[1])
		}
	}
	if len(schemas) < 100 {
		t.Fatalf("%s reads as %d schemas, which is too few to be the module", contractsGenerated, len(schemas))
	}
	for name := range schemas {
		sort.Strings(schemas[name])
	}
	return schemas
}

func readRepositoryFile(t *testing.T, root, path string) string {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(source)
}

// threeBadRequests sends every route the table held before this child three requests a declared
// shape does not authorize.
//
//   - An unauthorized actor: an anonymous request, which every row but a public one refuses.
//   - An identifier of another project: a route addressing one project's object under another
//     project's key, which answers that object to nobody.
//   - An undeclared traversal parameter: every word a caller would walk the graph with that the
//     route does not declare, handed to it at once, at objects the route has a row of. A route
//     that honoured one would answer something else, so the answer must be the answer it gives
//     without them, status and rows alike, as the base build answered it.
func threeBadRequests(t *testing.T) {
	// A listener that holds s1 live, the session every {session_id} of the probe names
	// (routeProbePath), so a person's direct message to it is delivered and the reads of what a
	// session was sent, and of a person's conversations with it, answer that delivery.
	live, delivered := true, []map[string]any{}
	listener := sessionListener(t, &live, &delivered)
	t.Cleanup(listener.Close)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	// The production table: a test hook is mounted only where Deps.TestHooksEnabled is set, and
	// a route the server does not serve answers 404 to every caller rather than refusing one.
	rows := routesBeforeThisChild(t, false)

	for _, route := range rows {
		if route.Auth == authPublic {
			continue
		}
		response := personPageRequest(t, handler, routeProbePath(route.Pattern), "")
		if route.Method != http.MethodGet {
			response = probeWithoutCredential(t, handler, route)
		}
		if refused, _ := authRefusal(response); !refused {
			t.Errorf("%s %s answers an anonymous caller %d %s, want a refusal",
				route.Method, route.Pattern, response.Code, firstLine(response.Body.String()))
		}
	}

	owned := seedTwoProjects(t, handler, database)
	seedTheInboxReads(t, handler, owned)
	var probed []string
	for _, route := range rows {
		target, addressable := crossProjectPath(t, route, owned)
		if !addressable {
			continue
		}
		probed = append(probed, route.Method+" "+target)
		response := crossProjectRequest(t, handler, route, target, owned)
		// The source's removal and its sync find a source by the key and the repository together
		// (deleteArchitectureSource, architecture.Importer.loadSourceByRepo), so under another
		// project's key each finds none and answers 404 SOURCE_NOT_FOUND, and nothing else. One
		// that matched the repository alone would act on this project's source: the removal
		// would delete it (204), and the sync would load it and, the test server holding no
		// GitHub App, answer 409 SOURCE_ACCESS (syncArchitectureSource), which the checks below
		// would pass as a refusal.
		if strings.Contains(route.Pattern, "/architecture-sources/{owner}/{repo}") {
			if code := responseCode(t, response); response.Code != http.StatusNotFound || code != "SOURCE_NOT_FOUND" {
				t.Errorf("%s answers %d %s for another project's architecture source, want 404 SOURCE_NOT_FOUND: %s",
					route.Method+" "+target, response.Code, code, firstLine(response.Body.String()))
			}
			continue
		}
		if response.Code < 400 {
			t.Errorf("%s answers %d for an identifier of another project: %s",
				route.Method+" "+target, response.Code, firstLine(response.Body.String()))
			continue
		}
		// A server error is no refusal: a route that let another project's object past its key
		// fails further in, where it reads that object as its key's - the step write's and the
		// rating write's read of the key's issue's runs, which does not list a run of another issue,
		// answers 500 - and a 5xx from a route that refused nothing would pass the check above as a
		// refusal.
		if response.Code >= 500 {
			t.Errorf("%s fails %d for an identifier of another project rather than refusing it: %s",
				route.Method+" "+target, response.Code, firstLine(response.Body.String()))
			continue
		}
		// A refusal of the caller comes before the identifier is read, so it says nothing about
		// whether the route would answer another project's object: each route is sent as a caller
		// its own checks admit (crossProjectRequest), and one that refuses that caller all the same
		// proves nothing about the identifier. Nor does a refusal of the body: the step routes
		// refuse 400 STEP_INPUT a value outside its form before they read the identifier, and a run
		// of a kind the route does not write (the rating write's ratingOfKind) only after the
		// identifier has passed the check of its issue, so a STEP_INPUT is never that check's
		// refusal.
		if code := responseCode(t, response); code == codeStepWriter {
			t.Errorf("%s refuses the probe's caller %d %s before it reads the identifier, so the probe proves nothing about it",
				route.Method+" "+target, response.Code, code)
		} else if code == codeStepInput {
			t.Errorf("%s refuses the probe's body %d %s rather than the identifier's project, so the probe proves nothing about it: %s",
				route.Method+" "+target, response.Code, code, firstLine(response.Body.String()))
		} else if refused, why := authRefusal(response); refused {
			t.Errorf("%s refuses the probe's caller %s before it reads the identifier, so the probe proves nothing about it",
				route.Method+" "+target, why)
		}
	}
	// A removal that answered 404 but deleted the source all the same would pass the check
	// above, so the source the probe sent under another project's key must still be listed.
	if sources := listSources(t, handler, "TEST"); !slices.ContainsFunc(sources, func(source architectureSourceResponse) bool {
		return source.Repo == owned.architectureSource
	}) {
		t.Errorf("GET /api/v1/projects/TEST/architecture-sources lists %q after the probe, without the source %s it sent under OTH",
			sourceStrings(sources), owned.architectureSource)
	}
	if len(probed) < 5 {
		t.Fatalf("only %d route(s) were probed with another project's identifier, which is too few to prove anything", len(probed))
	}
	t.Logf("%d routes were probed with another project's identifier: %s", len(probed), strings.Join(probed, ", "))

	// The traversal probe reads each route at the objects seedTwoProjects wrote, with the query
	// a route requires (routeProbeQueries), so a route with a row to answer answers it, and a
	// route that honoured a traversal word would answer different rows. A route that answers no
	// row at all - a 404 for a placeholder id, or an empty list - answers the same whatever the
	// query holds, and proves nothing; so every route must answer a row here (answersARow), but
	// the ones listed in routesNoSeedAnswers, each of which needs something no seed of this test
	// holds, and each of which must then answer no row.
	//
	// Each route is sent the traversal words it does not declare. GET /api/v1/me/inbox declares
	// RefusesUndeclared: decision 7 has it refuse every key it does not declare with 400
	// INVALID_QUERY naming the key (requireTheUndeclaredKeyRefused). The other routes must answer
	// as they answered the plain read. A read that requires its query (routesRefusingTheBareRead)
	// is sent the bare read too, which must be refused naming the parameter it lacks.
	//
	// A word a route does declare - `kind` on GET /api/v1/me/replies - is that route's
	// own parameter, which it answers by its own rule, and is no traversal the caller handed
	// it. A read whose rows can grow between the two reads (growingReads) is walked under a
	// parameter it declares that holds it to the rows its plain read answered, so the two
	// answers differ only where the walk did.

	// Give each history probe a row to read: a post nobody edited has no history.
	editThreadComment(t, handler, owned.comment, "The edited comment of this project.", "alice")
	editIssueMessage(t, handler, owned.message, "The edited message of this project.", "alice")

	// The event log answers only the events at or below the server's safe horizon, which an
	// event write of the server's database holds back while it is open (events.Horizon): under
	// load its first read can answer none of the seeded events and its next read every one of
	// them. Awaiting a horizon proven after the seed puts every seeded event at or below it
	// before the log is read, and the horizon never falls, so its plain read answers them all
	// and its bound holds its walked read to them.
	if _, err := served.horizon().Await(t.Context()); err != nil {
		t.Fatalf("await an event horizon above the seeded events: %v", err)
	}
	queries := routeProbeQueries(owned)
	for _, name := range slices.Sorted(maps.Keys(rowFields)) {
		if _, known := routeShapes[name]; !known {
			t.Errorf("rowFields names %s, which is no route of the table: take it off", name)
		}
	}
	answered := 0
	for _, route := range rows {
		if route.Method != http.MethodGet {
			continue
		}
		name := route.Method + " " + route.Pattern
		target := seededProbePath(route.Pattern, owned)
		if parameter, required := routesRefusingTheBareRead[name]; required {
			requireTheBareReadRefused(t, name, parameter, target, dispatchRequest(t, handler, route.Method, target, nil, "alice"))
		}
		if query, held := queries[name]; held {
			target += "?" + query
		}
		plain := dispatchRequest(t, handler, route.Method, target, nil, "alice")
		walk := undeclaredTraversal(name)
		if bound, grows := growingReads[name]; grows && plain.Code < 400 {
			held, readRows := bound(plain.Body.Bytes())
			if !readRows {
				t.Errorf("%s answers %s at %s on its own, so no bound holds its traversal probe to the rows it read",
					name, answerShape(plain.Body.Bytes()), target)
				continue
			}
			walk = held + "&" + walk
		}
		walkedTarget := target + "?" + walk
		if _, held := queries[name]; held {
			walkedTarget = target + "&" + walk
		}
		walked := dispatchRequest(t, handler, route.Method, walkedTarget, nil, "alice")
		if reason, exempt := routesNoSeedAnswers[name]; exempt {
			if plain.Code < 400 && answersARow(name, plain.Body.Bytes()) {
				t.Errorf("%s answers a row at %s, and routesNoSeedAnswers holds it as a route no seed answers (%s): take it off that list",
					name, target, reason)
			}
		} else if plain.Code >= 400 {
			t.Errorf("%s answers %d at %s on its own, so its traversal probe reads no row: %s",
				name, plain.Code, target, firstLine(plain.Body.String()))
		} else if !answersARow(name, plain.Body.Bytes()) {
			t.Errorf("%s answers %s at %s on its own, which holds no row, so its traversal probe reads none: "+
				"seed a row it reads, or list it in routesNoSeedAnswers with what it needs", name, firstLine(plain.Body.String()), target)
		} else {
			answered++
		}
		if routeShapes[name].RefusesUndeclared {
			requireTheUndeclaredKeyRefused(t, name, walk, walked)
			continue
		}
		if plain.Code != walked.Code {
			t.Errorf("%s answers %d at %s on its own and %d with a traversal parameter: %s",
				name, plain.Code, target, walked.Code, firstLine(walked.Body.String()))
			continue
		}
		if before, after := answerShape(plain.Body.Bytes()), answerShape(walked.Body.Bytes()); before != after {
			t.Errorf("%s answers %s at %s on its own and %s with a traversal parameter",
				name, before, target, after)
		}
	}
	if answered < 60 {
		t.Fatalf("only %d read answered a row for the traversal probe, which is too few to prove anything", answered)
	}
	t.Logf("%d reads answered a row with and without a traversal parameter alike", answered)
}

// requireTheUndeclaredKeyRefused fails unless walked, the read of a route that refuses every key
// it does not declare (RefusesUndeclared, the plan's decision 7), answers 400
// INVALID_QUERY naming the first of walk's keys it does not declare, and nothing else.
func requireTheUndeclaredKeyRefused(t *testing.T, name, walk string, walked *httptest.ResponseRecorder) {
	t.Helper()
	query, err := url.ParseQuery(walk)
	if err != nil {
		t.Fatalf("read the traversal query %q: %v", walk, err)
	}
	declared := queryTagsOf(routeShapes[name].Params)
	var undeclared []string
	for _, key := range slices.Sorted(maps.Keys(query)) {
		if !slices.Contains(declared, key) {
			undeclared = append(undeclared, key)
		}
	}
	if len(undeclared) == 0 {
		t.Fatalf("%s was walked with %q, which holds no key it does not declare, so its refusal proves nothing", name, walk)
	}
	var refusal struct{ Code, Error string }
	if walked.Code != http.StatusBadRequest || json.Unmarshal(walked.Body.Bytes(), &refusal) != nil ||
		refusal.Code != "INVALID_QUERY" || !strings.HasPrefix(refusal.Error, undeclared[0]+" is not a query parameter of ") {
		t.Errorf("%s answers %d %s with a traversal parameter, want 400 INVALID_QUERY naming %s, the key it does not declare (plan decision 7)",
			name, walked.Code, firstLine(walked.Body.String()), undeclared[0])
	}
}

// routesNoSeedAnswers are the reads no object seedTwoProjects writes lets answer a row, each
// with what it would need. Their traversal probe still runs, and still has to answer as the
// plain read answers, but reads nothing: each is refused or answers no row (answersARow), and
// one that answers a row is taken off the list.
var routesNoSeedAnswers = map[string]string{
	"GET /api/v1/github/app-installation/{owner}/{repo}":                 "a GitHub App private key",
	"GET /api/v1/settings/repo-projects":                                 "a repository mapped to a project",
	"GET /api/v1/settings/new-issue":                                     "the new-issue settings a person set",
	"GET /api/v1/projects/{key}/plans":                                   "an issue in one of the project's plans",
	"GET /api/v1/settings/delivery":                                      "delivery settings",
	"GET /api/v1/ci-reconcile/passes":                                    "a CI reconcile pass",
	"GET /api/v1/me/agent-tokens":                                        "a personal agent token",
	"GET /api/v1/dashboard/builds":                                       "a dashboard build a live server process serves",
	"GET /api/v1/issues/{key}/references":                                "a document the issue's content references",
	"GET /api/v1/references/questions":                                   "a question block in the comment's document",
	"GET /api/v1/block-links":                                            "a block link between two of the spec's blocks",
	"GET /api/v1/issues/{key}/results":                                   "a verification document's checks",
	"GET /api/v1/issues/{key}/coverage":                                  "a requirement the spec marks",
	"GET /api/v1/issues/{key}/role-holders":                              "a holder of one of the issue's roles",
	"GET /api/v1/issues/{key}/handoffs":                                  "a handoff",
	"GET /api/v1/me/documents/{id}/read":                                 "the person's read of the project document",
	"GET /api/v1/agents/{session_id}/artifacts":                          "a file of a session's conversation",
	"GET /api/v1/broadcasts":                                             "a broadcast",
	"GET /api/v1/artifacts/{id}/asks":                                    "an ask on the project document",
	"GET /api/v1/artifacts/{id}/comments":                                "a comment on the project document",
	"GET /api/v1/artifacts/{id}/reviews":                                 "a review of the project document",
	"GET /api/v1/artifacts/{id}/references":                              "a reference in or to the project document",
	"GET /api/v1/credential-requests":                                    "a credential request the person may decide",
	"GET /api/v1/delivery/timeline":                                      "delivery settings",
	"GET /api/v1/delivery/measures":                                      "delivery settings",
	"GET /api/v1/delivery/pipeline":                                      "delivery settings",
	"GET /api/v1/delivery/runs/{id}":                                     "a delivery run",
	"GET /api/v1/issues/{key}/subscribers":                               "a listener that answers its interests (the probe's answers sessions alone)",
	"GET /api/v1/artifacts/{id}/subscribers":                             "a listener that answers its interests (the probe's answers sessions alone)",
	"GET /api/v1/issues/{key}/handoffs/{phase}/latest":                   "a handoff",
	"GET /api/v1/handoffs/{id}":                                          "a handoff",
	"GET /api/v1/going/requests/{id}/stream":                             "a going request's answer run",
	"GET /api/v1/agents/{session_id}/artifacts/{slug}":                   "a session's own document",
	"GET /api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}": "a session's own document",
	"GET /api/v1/broadcasts/{id}":                                        "a broadcast",
	"GET /api/v1/credential-grants":                                      "the agent-secrets broker",
	"GET /api/v1/machine-logins":                                         "the agent-secrets broker",
	"GET /api/v1/signins":                                                "the agent-secrets broker",
}

// rowFields are the reads that answer an object holding their rows in fields of its own - a
// page's list, or the one value a read answers about the object it names - each with those
// fields. Such a read answers a row when one of them holds something (answersARow); the rest of
// its object, a page's count or the id it was asked about, is no row.
var rowFields = map[string][]string{
	"GET /api/v1":                             {"routes"},
	"GET /api/v1/users":                       {"users"},
	"GET /api/v1/priorities":                  {"priorities"},
	"GET /api/v1/projects/{key}/related":      {"related"},
	"GET /api/v1/projects/{key}/plans":        {"issues"},
	"GET /api/v1/dashboard/builds":            {"live"},
	"GET /api/v1/issues/{key}/references":     {"members"},
	"GET /api/v1/issues/{key}/collaborators":  {"people", "agents"},
	"GET /api/v1/references":                  {"edges"},
	"GET /api/v1/references/questions":        {"questions"},
	"GET /api/v1/reference-targets":           {"targets"},
	"GET /api/v1/block-links":                 {"links"},
	"GET /api/v1/issues/{key}/results":        {"checks"},
	"GET /api/v1/issues/{key}/coverage":       {"requirements"},
	"GET /api/v1/issues/{key}/steps":          {"tasks"},
	"GET /api/v1/issues/{key}/role-holders":   {"holders"},
	"GET /api/v1/issues/{key}/decisions":      {"entries"},
	"GET /api/v1/messages/{id}/blocks":        {"blocks"},
	"GET /api/v1/comments/{id}/blocks":        {"blocks"},
	"GET /api/v1/me/answers":                  {"rows"},
	"GET /api/v1/me/replies":                  {"rows"},
	"GET /api/v1/me/inbox":                    {"entries"},
	"GET /api/v1/me/documents/{id}/read":      {"read_at"},
	"GET /api/v1/search":                      {"results"},
	"GET /api/v1/search/results":              {"results"},
	"GET /api/v1/going/catalogue":             {"actions"},
	"GET /api/v1/near-copy-checks":            {"checks"},
	"GET /api/v1/agents/known":                {"sessions"},
	"GET /api/v1/agents/{session_id}/answers": {"items"},
	"GET /api/v1/asks/open":                   {"asks"},
	"GET /api/v1/asks/{id}/followers":         {"followers"},
	"GET /api/v1/artifacts/{id}/references":   {"outgoing", "referenced_by"},
}

// answersARow reports whether a read's answer holds a row of what route reads: an array one
// element at least; an object rowFields names the fields of, something in one of them; any other
// object - the one object the read names - something in one of its values; and an answer that is
// no JSON, any byte. Null, an empty string, an empty array and an empty object hold nothing, so
// an empty list, an object of nulls and a page of no rows answer no row.
func answersARow(route string, body []byte) bool {
	var answer any
	if json.Unmarshal(body, &answer) != nil {
		return len(bytes.TrimSpace(body)) > 0
	}
	object, isObject := answer.(map[string]any)
	if !isObject {
		return holdsSomething(answer)
	}
	fields, named := rowFields[route]
	if !named {
		fields = slices.Collect(maps.Keys(object))
	}
	for _, field := range fields {
		if holdsSomething(object[field]) {
			return true
		}
	}
	return false
}

// holdsSomething reports whether a decoded JSON value holds anything: every value but null, an
// empty string, an empty array and an empty object.
func holdsSomething(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case string:
		return value != ""
	case []any:
		return len(value) > 0
	case map[string]any:
		return len(value) > 0
	default:
		return true
	}
}

// routesRefusingTheBareRead are the reads routeProbeQueries sends a query that refuse the read
// without it, each with the parameter the refusal names. The probe sends each the bare read too,
// which must answer 400 INVALID_QUERY naming that parameter (requireTheBareReadRefused): the
// query routeProbeQueries adds is what lets the read answer a row, and a read that answered
// without it would be caught.
var routesRefusingTheBareRead = map[string]string{
	"GET /api/v1/agents/known": "id",
}

// requireTheBareReadRefused fails unless bare, the read of route name at target without the query
// it requires, answers 400 INVALID_QUERY whose words lead with the parameter it lacks.
func requireTheBareReadRefused(t *testing.T, name, parameter, target string, bare *httptest.ResponseRecorder) {
	t.Helper()
	var refusal struct{ Code, Error string }
	if bare.Code != http.StatusBadRequest || json.Unmarshal(bare.Body.Bytes(), &refusal) != nil ||
		refusal.Code != "INVALID_QUERY" || !strings.HasPrefix(refusal.Error, parameter+" ") {
		t.Errorf("%s answers %d %s at %s, without the %s it requires, want 400 INVALID_QUERY naming %s",
			name, bare.Code, firstLine(bare.Body.String()), target, parameter, parameter)
	}
}

// routeProbeQueries are the queries the reads that require one are sent, each naming the
// objects seedTwoProjects wrote, so the read answers a row rather than refusing the request
// that names nothing or answering it no row. The open-asks read needs no query to answer a row,
// and is sent the key this child adds to it, the person the project's asks wait on,
// so the read that key narrows is the one walked.
func routeProbeQueries(owned crossProject) map[string]string {
	spec := "dispatch://" + owned.issue + "/spec"
	return map[string]string{
		"GET /api/v1/search":                 url.Values{"q": {"project"}}.Encode(),
		"GET /api/v1/search/results":         url.Values{"q": {"project"}}.Encode(),
		"GET /api/v1/references":             url.Values{"to": {"dispatch://" + owned.issue}}.Encode(),
		"GET /api/v1/references/questions":   url.Values{"kind": {"comment"}, "id": {owned.comment}}.Encode(),
		"GET /api/v1/block-links":            url.Values{"from": {spec}, "to": {spec}}.Encode(),
		"GET /api/v1/issues/resolve":         url.Values{"ref": {owned.issue}}.Encode(),
		"GET /api/v1/asks/open":              url.Values{"waiting_on": {"alice"}}.Encode(),
		"GET /api/v1/credential-requests":    url.Values{"approver": {"me"}}.Encode(),
		"GET /api/v1/near-copy-checks":       url.Values{"project": {"TEST"}}.Encode(),
		"GET /api/v1/reference-targets":      url.Values{"ref": {"dispatch://" + owned.issue}}.Encode(),
		"GET /api/v1/agents/known":           url.Values{"id": {owned.session}}.Encode(),
		"GET /api/v1/projects/{key}/related": url.Values{"title": {"A project's own issue"}}.Encode(),
		"GET /api/v1/artifacts/{id}/changes": url.Values{"from": {"1"}, "to": {"2"}}.Encode(),
	}
}

// growingReads are the reads the traversal probe walks whose rows can grow between its plain
// read and its walked one, each with the query, of parameters the route declares, that holds the
// walked read to the rows the plain read answered, or false when that read answered no row to
// hold it to. All three read the event log. The server's own background work appends to it after
// the request that caused the work has been answered (a document's settlement, a keyed write
// whose client left), and the whole log is read only up to the safe horizon, which moves on
// whenever an event write of the server's database that held it ends. Every other read the probe
// walks either lists rows that only a write request adds - the inbox's open asks, an issue's
// comments, messages and asks, the broadcasts - and the probe sends only reads, or answers an
// object, whose shape is its keys (answerShape), which a row added beneath it leaves as they were:
// search, the replies, the answers, a person's nodes, the related checks.
var growingReads = map[string]func(answered []byte) (string, bool){
	"GET /api/v1/events/log":            eventLogBound,
	"GET /api/v1/issues/{key}/events":   eventFeedBound,
	"GET /api/v1/artifacts/{id}/events": eventFeedBound,
}

// eventFeedBound holds a read of one issue's or one project document's events to the events
// answered holds: before= one past the newest sequence among them. An issue and a project
// document number their events under their own row, which each append locks until it commits,
// so an event the plain read did not see has a later sequence than every event it did, and
// before= leaves it out. The page keeps the route's default limit, above the rows answered, so
// a walk that reached another owner's events would still answer more rows.
func eventFeedBound(answered []byte) (string, bool) {
	var events []struct {
		Seq int `json:"seq"`
	}
	if err := json.Unmarshal(answered, &events); err != nil || len(events) == 0 {
		return "", false
	}
	newest := 0
	for _, event := range events {
		newest = max(newest, event.Seq)
	}
	return url.Values{"before": {strconv.Itoa(newest + 1)}}.Encode(), true
}

// eventLogBound holds a read of the whole event log to the events answered holds: limit=, the
// one bound the log declares besides after=, set to how many there are. The log is read oldest
// first by id, every event at or below the horizon is final, and the horizon never falls, so the
// first that many events of a later read are the events the plain read answered. The log
// already answers every event, so a traversal word it honoured as a filter would answer fewer
// rows, and one it honoured by adding rows past its limit would answer more.
func eventLogBound(answered []byte) (string, bool) {
	var events []json.RawMessage
	if err := json.Unmarshal(answered, &events); err != nil || len(events) == 0 {
		return "", false
	}
	return url.Values{"limit": {strconv.Itoa(len(events))}}.Encode(), true
}

// seededProbePath is the route's path at the objects seedTwoProjects wrote under their own
// project: each wildcard takes the seeded object its place names, and a wildcard no seeded
// object fills takes routeProbePath's placeholder. A route under /artifacts/{id} reads the
// project's own document, which every such route answers for; one of an issue's documents is
// read through the issue's routes, which is where those send a reader.
func seededProbePath(pattern string, owned crossProject) string {
	path := pattern
	for _, replacement := range []struct{ from, to string }{
		{"/issues/{key}/artifacts/{slug}", "/issues/" + owned.issue + "/artifacts/" + owned.slug},
		{"/projects/{key}/artifacts/{slug}", "/projects/TEST/artifacts/" + owned.documentSlug},
		{"/projects/{key}", "/projects/TEST"},
		{"/issues/{key}", "/issues/" + owned.issue},
		{"/comments/{id}", "/comments/" + owned.comment},
		{"/asks/{id}", "/asks/" + owned.ask},
		{"/messages/{id}", "/messages/" + owned.message},
		{"/artifacts/{id}", "/artifacts/" + owned.document},
		{"/documents/{id}", "/documents/" + owned.document},
		{"/priorities/{id}", "/priorities/" + owned.priority},
		{"/near-copy-checks/{id}", "/near-copy-checks/" + owned.nearCopyCheck},
		{"{block_id}", owned.documentBlock},
	} {
		path = strings.ReplaceAll(path, replacement.from, replacement.to)
	}
	return routeProbePath(path)
}

// undeclaredTraversal is the traversal one route is sent: every traversal word the route does
// not declare as a query parameter of its own, each with a value a traversal would take.
func undeclaredTraversal(route string) string {
	declared := queryTagsOf(routeShapes[route].Params)
	query := url.Values{}
	for _, word := range traversalWords {
		if !slices.Contains(declared, word) {
			query.Set(word, "comment")
		}
	}
	return query.Encode()
}

// probeWithoutCredential sends a mutating route its probe body and no credential.
func probeWithoutCredential(t *testing.T, handler http.Handler, route apiRoute) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), route.Method,
		routeProbePath(route.Pattern), strings.NewReader(routeProbeBody(route)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// routeProbeBodyValue is routeProbeBody as dispatchRequest takes it: nil for a read, which
// sends no body at all.
func routeProbeBodyValue(route apiRoute) any {
	if route.Method == http.MethodGet {
		return nil
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(routeProbeBody(route)), &body); err != nil {
		return nil
	}
	return body
}

// crossProject is one project's objects, for the probe that addresses them under another
// project's key and for the probe that reads each of them under its own.
type crossProject struct {
	otherKey, otherIssue  string
	issue, comment, ask   string
	artifact, slug, block string
	// message is an issue message, and document a project document, with its slug and one of
	// its blocks: the objects a route of the project's own documents and messages reads. The
	// message is also the id the cross-project probe sends a route that names a message, and the
	// document's slug the slug it sends a route of a project document (crossProjectPath), so each
	// route is sent another project's object of its own kind; slug, the issue's design.md, is
	// what it sends a route of an issue document.
	message, document, documentSlug, documentBlock string
	// step is the id of a run of a step of the issue, written as the project's Legion daemon
	// writes one: the id the cross-project probe sends the step write. retro is the id of the run
	// of the issue's retro, written the same way: the id it sends the rating write, which rates a
	// retro's run alone and refuses a run of any other kind 400 STEP_INPUT under every key alike
	// (ratingOfKind). plan is the id of the plan of a run of the issue, written the same way: the
	// id it sends the plan write.
	step, retro, plan string
	// priority is the id of a priority. A priority belongs to no project, so no project's key
	// addresses it, and only the probe that reads each object at its own address reads it.
	priority string
	// architectureSource is the repository, owner/repo, of the project's architecture source,
	// which a route of a source addresses by its project's key and that repository together: the
	// {owner}/{repo} the cross-project probe sends the source's removal and its sync
	// (crossProjectPath).
	architectureSource string
	// nearCopyCheck is the id of the near-copy gate's record of one of the project's creates.
	nearCopyCheck string
	// session is a session the store holds a record of: it follows the project's ask, so GET
	// /api/v1/agents/known, asked about it, answers it rather than no session at all.
	session string
}

// seedTwoProjects writes one project with an issue, a document of the issue (design.md), a
// comment, an ask a session follows, a message, a run of a step of the issue and a run of its
// retro, a project document at two versions and a block of each document, an architecture
// source, and the Legion daemon it names with a read of it; a second project with an issue, which
// the cross-project probe addresses them under; and one priority, which belongs to neither
// project and is read at its own id. Under the second project's key the probe sends each route the
// object of the first its path names (crossProjectPath): design.md to a route of an issue
// document, the project document to a route of a project document, the message to the message
// read, the run of a step to the step write, the retro's run to the rating write and the
// architecture source's repository to the source's removal and its sync.
func seedTwoProjects(t *testing.T, handler http.Handler, database *store.Store) crossProject {
	t.Helper()
	createDuplicateTestProject(t, handler, "OTH")
	other := requireCreatedIssue(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "OTH", "title": "Another project's issue", "spec": "Other.\n"}, "alice"))
	issue := createInteractionIssue(t, handler, "TEST", "A project's own issue", "Introduction.\n\nTarget passage.\n")
	// A document this project alone holds: `spec` is every issue's own, so addressing it under
	// another issue's key addresses that issue's spec and nothing crosses a project.
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts",
		map[string]any{"name": "design.md", "content": "A design of this project.\n"}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("seed a document: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	design := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	owned := crossProject{
		otherKey: "OTH", otherIssue: other.Key,
		issue: issue.Key, artifact: issue.PrimaryArtifactID, slug: design.Slug,
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": "A comment of this project."}, "alice"))
	owned.comment = comment.ID
	owned.ask = createIssueAsk(t, handler, issue.Key, "A question of this project?")
	// A session follows the ask through the route a session follows one by, so the store holds a
	// record of that session (agents_known.go reads ask_followers among its records) and GET
	// /api/v1/agents/known, asked about it, answers a row: asked about an id nobody ever held, it
	// answers none, and its traversal probe would compare two empty lists.
	owned.session = "019a1c0c-3333-7000-8000-000000000001"
	if followed := sessionRequest(t, handler, http.MethodPut, "/api/v1/asks/"+owned.ask+"/followers/"+owned.session,
		map[string]any{"actor": map[string]any{"kind": "session", "id": owned.session}}); followed.Code != http.StatusNoContent {
		t.Fatalf("seed a session's follow of the ask: status=%d body=%s", followed.Code, followed.Body.String())
	}
	owned.block = firstBlockOf(t, handler, issue.PrimaryArtifactID)
	message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages",
		map[string]any{"body": "A message of this project."}, "alice")
	if message.Code != http.StatusCreated {
		t.Fatalf("seed a message: status=%d body=%s", message.Code, message.Body.String())
	}
	owned.message = decodeBody[struct {
		ID string `json:"id"`
	}](t, message).ID
	// A run of a step of the issue, written over the credential the deployment configures for
	// its daemon and naming that daemon's session (putStepOK), as Legion writes one: an id the
	// step write answers for under its own issue's key, so its refusal under another project's
	// key is the refusal of an id outside that key.
	owned.step = uuid.NewString()
	putStepOK(t, handler, issue.Key, owned.step, implementerStart(daemonSessionOf("TEST")))
	// The run of the issue's retro, written the same way (as plans_test.go seeds one): the rating
	// write rates a retro's run alone, and refuses a run of a step 400 STEP_INPUT for its kind
	// under its own issue's key too, so only a retro's run makes the rating write's refusal under
	// another project's key the refusal of an id outside that key.
	owned.retro = uuid.NewString()
	putStepOK(t, handler, issue.Key, owned.retro, withFields(retroIdentity(daemonSessionOf("TEST"), 1), map[string]any{"started_at": stepStart}))
	// The plan of a run of the issue, written the same way (putPlanRecordOK).
	owned.plan = uuid.NewString()
	putPlanRecordOK(t, handler, issue.Key, owned.plan, defaultPlanBody(daemonSessionOf("TEST"), 1))
	document := createProjectDocument(t, handler, "TEST", "Notes", "# Notes\n\nA passage of the project's own document.\n")
	owned.document, owned.documentSlug = document.ID, document.Slug
	// A second version of the project document: what changed between two of its versions is a
	// read of its own (GET /api/v1/artifacts/{id}/changes, from=1&to=2 in routeProbeQueries), which
	// answers no row for a document holding one version alone. It is written before the block is
	// read, so the block is one the latest version holds.
	if revised := uploadDocument(t, handler, "/api/v1/projects/TEST", "Notes",
		"# Notes\n\nA passage of the project's own document, revised.\n"); revised.Version.Number != 2 {
		t.Fatalf("the project document's second upload = version %d, want 2", revised.Version.Number)
	}
	owned.documentBlock = firstBlockOf(t, handler, document.ID)
	// An architecture source, written into its table as the other tests of this package seed one
	// (node_facts_test.go): the route that adds one first proves the GitHub App can read the
	// repository (putArchitectureSource), and the test server holds no App. Its id is the column's
	// own identity, so it collides with no row.
	owned.architectureSource = "acme/arch"
	if _, err := database.Pool.Exec(t.Context(), `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('TEST', $1, 'main', 1, '{"kind":"system","id":"test"}')`, owned.architectureSource); err != nil {
		t.Fatalf("seed an architecture source: %v", err)
	}
	// A priority is written through its own route, as a person writes one, because its history
	// answers a row only for a priority the store holds: at a placeholder id it answers 404, and
	// its traversal probe would read nothing.
	owned.priority = createTestPriority(t, handler, map[string]any{"title": "A priority of the deployment"}, "alice").ID
	// Every create records the near-copy gate's answer (issue_create.go, recordNearCopyCheck), so
	// the project's own creates above left records; one of them is the row the read of a record
	// by its id answers, which at a placeholder id answers 404 and reads nothing.
	checks := decodeBody[model.NearCopyChecksPage](t, dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/near-copy-checks?project=TEST", nil, "alice"))
	if len(checks.Checks) == 0 {
		t.Fatalf("the project's creates left no near-copy check record, so its read by id has no row to answer")
	}
	owned.nearCopyCheck = checks.Checks[0].ID
	// The Legion board answers 404 LEGION_NOT_CONFIGURED for a project that names no Legion
	// daemon, and its cards are what the sampler recorded of the daemon a project names. So the
	// project names one, and a good read of it recorded the project's issue in a phase, written as
	// the sampler writes them, and the board answers that issue's card.
	nameLegionDaemon(t, handler, "TEST", "http://legion.invalid:13370")
	sampled := time.Now().UTC()
	seedLegionObservation(t, database, "TEST", issue.Key, legion.FieldPhase, `"implementing"`, sampled)
	seedLegionRead(t, database, "TEST", sampled, "")
	return owned
}

// seedTheInboxReads writes, through the routes a person and a session write them by, what the
// reads of a person's Inbox and of a session's conversation answer a row of: alice's direct
// message to s1, which the probe's listener delivers, and s1's reply to it (the session's
// conversation, which GET /api/v1/agents/{session_id}/conversation and .../messages both answer,
// and what it was sent); alice's pin of that conversation and of the project's issue (her state
// of each); an ask s1 opens on the issue and alice's reply in its thread (her answers); s1's reply
// to alice's comment, a thread she wrote in that somebody else wrote in since, so unread: the
// state the read of her threads answers by default.
func seedTheInboxReads(t *testing.T, handler http.Handler, owned crossProject) {
	t.Helper()
	sent := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "Is the probe's conversation here?", "delivery": "aside",
	}, "alice")
	root := decodeBody[model.Message](t, created(t, "seed alice's direct message to s1", sent))
	created(t, "seed s1's reply to alice's direct message", replyTo(t, handler, root.ID, "It is."))
	putAgentState(t, handler, "alice", "s1", map[string]any{"pinned": true})
	if pinned := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+owned.issue+"/state",
		map[string]any{"pinned": true}, "alice"); pinned.Code != http.StatusOK {
		t.Fatalf("seed alice's pin of %s: status=%d body=%s", owned.issue, pinned.Code, pinned.Body.String())
	}
	ask := decodeBody[model.Ask](t, created(t, "seed s1's ask", sessionRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+owned.issue+"/asks", map[string]any{
			"question": "Which read does the probe walk first?", "actor": map[string]any{"kind": "session", "id": "s1"},
		})))
	created(t, "seed alice's reply in s1's ask", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+owned.issue+"/comments", map[string]any{"body": "The person read.", "ask_id": ask.ID}, "alice"))
	created(t, "seed s1's reply to alice's comment", sessionRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+owned.issue+"/comments", map[string]any{
			"body": "Seen.", "reply_to": owned.comment, "actor": map[string]any{"kind": "session", "id": "s1"},
		}))
}

// firstBlockOf is the id of the first block GET /api/v1/artifacts/{id}/blocks lists.
func firstBlockOf(t *testing.T, handler http.Handler, artifactID string) string {
	t.Helper()
	blocks := decodeBody[[]struct {
		ID string `json:"id"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/blocks", nil, "alice"))
	if len(blocks) == 0 {
		t.Fatalf("the seeded document %s holds no block", artifactID)
	}
	return blocks[0].ID
}

// crossProjectPath is the route's path under another project's key, each other place of it
// filled with this project's object of the kind that place names, or false for a route that
// addresses no object of a project under a key at all. A route under /issues/{key} takes the
// other project's issue as its key, and every other route the other project's key. What each
// place takes:
//
//   - {slug} of /issues/{key}/artifacts/{slug}: the issue's design.md, a document of the issue,
//     where the issue's spec would be every issue's own slug and cross nothing;
//   - {slug} of /projects/{key}/artifacts/{slug}: the project's own document, so a read that
//     served a project document under whatever project's key it was sent answers a row;
//   - {number} beside either: 1, the first version, which each of the two documents holds;
//   - {id} of /issues/{key}/messages/{id}: the issue's message;
//   - {id} of /issues/{key}/steps/{id}: the run of a step of the issue its project's daemon
//     wrote, which the experiment write's path, /issues/{key}/steps/{id}/experiment, takes too;
//   - {id} of /issues/{key}/steps/{id}/rating: the run of the issue's retro its project's daemon
//     wrote, the one kind of run the rating write rates;
//   - {id} of /issues/{key}/plans/{id}: the plan of a run of the issue its project's daemon
//     wrote;
//   - {owner}/{repo} of /projects/{key}/architecture-sources/{owner}/{repo}: the repository of
//     the project's architecture source, which the source's removal and its sync address by the
//     key and the repository together. The source write (PUT) takes none and answers false: its
//     {owner}/{repo} names the repository it adds as a source of the key's own project, an
//     upsert on the key and the repository, and no object of another project.
//
// A route under a key whose {slug}, {id} or {owner}/{repo} is of a kind no line above names fails
// the test: an identifier of another kind is refused under every key alike, and would prove
// nothing about this one.
func crossProjectPath(t *testing.T, route apiRoute, owned crossProject) (string, bool) {
	t.Helper()
	pattern := route.Pattern
	// addressesObject reports whether a path holds a place an object of a project sits in:
	// {slug}, {id} or {owner}/{repo}. A route under a key whose path holds none addresses no
	// object of a project, and one whose path still holds one once every place below is filled
	// addresses an object of a kind this function sends none of.
	addressesObject := func(path string) bool {
		return slices.ContainsFunc([]string{"{slug}", "{id}", "{owner}/{repo}"}, func(place string) bool {
			return strings.Contains(path, place)
		})
	}
	if !strings.Contains(pattern, "{key}") || !addressesObject(pattern) {
		return "", false
	}
	// The source write's {owner}/{repo} is the repository it adds as a source of the key's own
	// project (putArchitectureSource, an upsert on the key and the repository), so under another
	// project's key it writes a source of that project and addresses no object of this one.
	if route.Method+" "+route.Pattern == "PUT /api/v1/projects/{key}/architecture-sources/{owner}/{repo}" {
		return "", false
	}
	for _, place := range []struct{ named, wildcard, object string }{
		{"/issues/{key}/artifacts/{slug}", "{slug}", owned.slug},
		{"/projects/{key}/artifacts/{slug}", "{slug}", owned.documentSlug},
		{"/artifacts/{slug}/versions/{number}", "{number}", "1"},
		{"/issues/{key}/messages/{id}", "{id}", owned.message},
		// The rating's place comes before the step write's, whose path its own path holds: the
		// first replaces its {id} with the retro's run, and the second finds none left.
		{"/issues/{key}/steps/{id}/rating", "{id}", owned.retro},
		{"/issues/{key}/steps/{id}", "{id}", owned.step},
		{"/issues/{key}/plans/{id}", "{id}", owned.plan},
		// The source's removal and its sync, whose path holds the removal's.
		{"/projects/{key}/architecture-sources/{owner}/{repo}", "{owner}/{repo}", owned.architectureSource},
	} {
		if strings.Contains(route.Pattern, place.named) {
			pattern = strings.ReplaceAll(pattern, place.wildcard, place.object)
		}
	}
	if addressesObject(pattern) {
		t.Errorf("%s %s addresses an object under a key of a kind crossProjectPath sends none of: seed one (seedTwoProjects) and name its place there",
			route.Method, route.Pattern)
		return "", false
	}
	switch {
	case strings.Contains(pattern, "/issues/{key}"), strings.Contains(pattern, "/me/issues/{key}"):
		pattern = strings.ReplaceAll(pattern, "{key}", owned.otherIssue)
	default:
		pattern = strings.ReplaceAll(pattern, "{key}", owned.otherKey)
	}
	if strings.Contains(pattern, "{") {
		return "", false
	}
	return pattern, true
}

// crossProjectRequest sends one route its cross-project path (crossProjectPath) as a caller the
// route's own checks admit, with a body that passes what the route checks before it reads the
// object, so what refuses it is the object and not the caller or the body. Every route is sent
// as alice, a person, with its probe body (routeProbeBody), but these:
//
//   - the step write, its rating, its experiment and the plan write each record only for the
//     session of the Legion daemon of the key's project and refuse any other writer 403
//     STEP_WRITER before they read the id (putIssueStep, the rating's, the experiment's and
//     putIssuePlan's requireIssueDaemon), so each is sent over the deployment's shared agent
//     credential, which proves no session, naming the session of the other project's daemon, with
//     a body each accepts: the rating carries one, the experiment write an experiment and its
//     variant, and the plan write the default plan of a run (defaultPlanBody);
//   - a named version requires a summary and refuses a body without one 400 INVALID_VERSION before
//     it reads the slug (createNamedVersion), so its body carries one.
func crossProjectRequest(t *testing.T, handler http.Handler, route apiRoute, target string, owned crossProject) *httptest.ResponseRecorder {
	t.Helper()
	switch route.Method + " " + route.Pattern {
	case "PUT /api/v1/issues/{key}/steps/{id}":
		return sessionRequest(t, handler, route.Method, target, map[string]any{"actor": daemonSessionOf(owned.otherKey)})
	case "PUT /api/v1/issues/{key}/steps/{id}/rating":
		return sessionRequest(t, handler, route.Method, target, map[string]any{
			"actor": daemonSessionOf(owned.otherKey), "rating": 3, "rating_reason": "A rating under another project's key.",
		})
	case "PUT /api/v1/issues/{key}/steps/{id}/experiment":
		return sessionRequest(t, handler, route.Method, target, map[string]any{
			"actor": daemonSessionOf(owned.otherKey), "experiment": "planning", "variant": "b",
		})
	case "PUT /api/v1/issues/{key}/plans/{id}":
		return sessionRequest(t, handler, route.Method, target, defaultPlanBody(daemonSessionOf(owned.otherKey), 1))
	case "POST /api/v1/issues/{key}/artifacts/{slug}/versions", "POST /api/v1/projects/{key}/artifacts/{slug}/versions":
		return dispatchRequest(t, handler, route.Method, target, map[string]any{"summary": "A version under another project's key."}, "alice")
	}
	return dispatchRequest(t, handler, route.Method, target, routeProbeBodyValue(route), "alice")
}

// answerShape renders an answer as what it answered rather than what it said: an object as its
// keys, an array as how many rows it holds. A route that honoured a traversal parameter answers
// a different number of rows or a different set of keys; a route that read the clock answers the
// same shape twice.
func answerShape(body []byte) string {
	var answer any
	if err := json.Unmarshal(body, &answer); err != nil {
		return "not JSON"
	}
	switch value := answer.(type) {
	case map[string]any:
		return "{" + strings.Join(slices.Sorted(maps.Keys(value)), ",") + "}"
	case []any:
		return fmt.Sprintf("[%d rows]", len(value))
	default:
		return fmt.Sprintf("%T", answer)
	}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// olderBuildReadsTheGraph: a build that cannot address a node kind skips it rather than refusing
// the read. The store's own kinds - a person, a session, a role and a page - are the kinds this
// child added, and a node of a kind no build knows stands for the kinds the children after it
// will add.
func olderBuildReadsTheGraph(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A graph read", "Introduction.\n\nTarget passage.\n")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"anchor": map[string]string{"artifact": "spec", "quote": "Target passage"},
		"body":   "Please clarify this.",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("seed a comment: status=%d body=%s", created.Code, created.Body.String())
	}
	// The read a `dispatch://` address is read by, in both directions at the issue. It reads
	// graph_edges, whose first arm is the refs table, so an edge written there is one this read
	// lists, or, for a kind this build cannot address, skips (refs.resolveNodes).
	address := "dispatch://" + issue.Key
	read := func(when string) [2]string {
		t.Helper()
		var answers [2]string
		for index, direction := range []string{"to", "from"} {
			response := dispatchRequest(t, handler, http.MethodGet,
				"/api/v1/references?"+url.Values{direction: {address}}.Encode(), nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("read the issue's edges (%s) %s: status=%d body=%s", direction, when, response.Code, response.Body.String())
			}
			answers[index] = response.Body.String()
		}
		return answers
	}
	now := read("before any unknown kind")

	// A node of a kind nothing in this build can address, an edge from it at the issue and an
	// edge from the issue at it, written where this read's edges come from. A newer build's kind
	// reads exactly like this one to this build, and an older build meets the edge at it where a
	// later child adds a kind: refs.resolveNodes drops that end rather than refusing the read.
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
		values ('telepathy', 'a-kind-no-build-knows', 'telepathy', 'project', 'live', now(), '{}'::jsonb, 0)`); err != nil {
		t.Fatalf("write a node of an unknown kind: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, kind, to_kind, to_id) values
			('telepathy', 'a-kind-no-build-knows', 'mentions', 'issue', $1),
			('issue', $1, 'mentions', 'telepathy', 'a-kind-no-build-knows')`, issue.Key); err != nil {
		t.Fatalf("write the edges of an unknown kind: %v", err)
	}
	// The view answers both edges, so the read below meets them and has to skip them.
	var listed int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_edges
		where (from_kind = 'telepathy' and to_kind = 'issue' and to_id = $1)
		   or (from_kind = 'issue' and from_id = $1 and to_kind = 'telepathy')`, issue.Key).Scan(&listed); err != nil {
		t.Fatalf("read the unknown kind's edges out of graph_edges: %v", err)
	}
	if listed != 2 {
		t.Fatalf("graph_edges holds %d of the two edges of the unknown kind, so the read below never meets them", listed)
	}
	if after := read("beside an unknown kind"); after != now {
		t.Fatalf("the unknown kind changed the read:\n before %v\n after  %v", now, after)
	}

	// The same edge from a kind this build does address changes the answer, so the comparison
	// above is of a read that lists what the refs table holds, and not of one that reads past it.
	other := requireCreatedIssue(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "TEST", "title": "An issue this build addresses", "spec": "Other.\n"}, "alice"))
	if _, err := database.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, kind, to_kind, to_id) values ('issue', $1, 'mentions', 'issue', $2)`,
		other.Key, issue.Key); err != nil {
		t.Fatalf("write an edge of a known kind: %v", err)
	}
	if known := read("beside a known kind"); known[0] == now[0] {
		t.Fatalf("an edge from an issue this build addresses left the read unchanged, so the comparison above read nothing the refs table holds:\n %s", known[0])
	}
	// Every kind this child's reads resolve answers a visibility predicate, which is what makes
	// an unknown kind the one case the reads skip rather than one they never meet.
	for _, kind := range []string{"person", "session", "role", "page"} {
		if !slices.Contains(refs.VisibilityKinds(), kind) {
			t.Errorf("the kind %s answers no visibility predicate", kind)
		}
	}
	if slices.Contains(refs.VisibilityKinds(), "telepathy") {
		t.Error("the registry holds a predicate for the kind this test invented")
	}
}

// readRelations are the tables each of the two reads touches, which is its plan as this test
// holds it: a read that gained a join, lost an index or started reading the store would name a
// different set. They are pinned rather than derived, because a derivation from the same
// statement would move with it.
var readRelations = map[string][]string{
	"GET /api/v1/comments/{id}":         {"comment_deliveries", "comment_mentions", "comments", "doc_updates"},
	"GET /api/v1/artifacts/{id}/blocks": {"artifacts", "asks", "comments", "doc_updates", "embeddings"},
}

// readAnswerPaths are the fields each of the two reads answers for the seed readsUnchanged
// writes, at every depth, as the fork-point build answered them for that seed. A field's path is
// the path of the object holding it, a dot and its name; the rows of an array sit under the
// array's path and `[]`, so the blocks the block read lists are `[]` and a cell of a table block
// is `[].table.rows[].cells[]`. They are pinned rather than read off the answer, as readRelations
// is, so a field the answer drops fails naming its path however deep it sits, where a check of
// the top level alone, or of the fields the document requires, passes a nested field and one Go
// leaves out where it is empty (`omitempty`).
//
// Four paths are not the fork point's, and each is named where it stands: deleted_at and
// deleted_by of the comment and of its replies, which pull request #3512 (INFRA-1523) adds to
// every comment by its R3 and not this child. Every other path is the fork point's, so a field
// the store adds to either read still fails here as one the read did not answer.
var readAnswerPaths = map[string][]string{
	"GET /api/v1/comments/{id}": {
		"comment",
		"comment.anchor", "comment.anchor.artifact_id", "comment.anchor.block_id", "comment.anchor.mark_id",
		"comment.anchor.orphaned", "comment.anchor.quote", "comment.anchor.version",
		"comment.anchor_block", "comment.anchor_block.id", "comment.anchor_block.type",
		"comment.anchor_block.path", "comment.anchor_block.path[].id", "comment.anchor_block.path[].index",
		"comment.anchor_block.path[].type",
		"comment.anchor_block.table", "comment.anchor_block.table.cells", "comment.anchor_block.table.column",
		"comment.anchor_block.table.header", "comment.anchor_block.table.id", "comment.anchor_block.table.row",
		"comment.answers", "comment.artifact_id", "comment.ask_id",
		"comment.author", "comment.author.id", "comment.author.kind", "comment.author.proven",
		"comment.body", "comment.created_at",
		// INFRA-1523's R3, not #3178: the delete mark every read of a comment carries
		// (model.Comment's DeletedAt and DeletedBy, no omitempty). A comment that stands, as this
		// seed's does, answers each as null, and answerWalk.object counts a field it carries as
		// null.
		"comment.deleted_at", "comment.deleted_by",
		"comment.deliveries", "comment.deliveries[].attempt", "comment.deliveries[].comment_id",
		"comment.deliveries[].created_at", "comment.deliveries[].delivery", "comment.deliveries[].duplicate",
		"comment.deliveries[].envelope_id", "comment.deliveries[].error", "comment.deliveries[].reply_id",
		"comment.deliveries[].resolve_error", "comment.deliveries[].session_id", "comment.deliveries[].state",
		"comment.deliveries[].target",
		"comment.edited_at", "comment.id", "comment.issue_key",
		"comment.mentions", "comment.mentions[].delivery", "comment.mentions[].session_id", "comment.mentions[].target",
		"comment.reply_block", "comment.reply_to", "comment.resolved", "comment.resolved_at",
		"comment.resolved_by", "comment.resolved_by.id", "comment.resolved_by.kind", "comment.resolved_by.proven",
		"comment.suggestion", "comment.suggestion.accepted", "comment.suggestion.replace_with",
		"comment.turn",
		"replies",
		"replies[].anchor", "replies[].answers", "replies[].artifact_id", "replies[].ask_id",
		"replies[].author", "replies[].author.id", "replies[].author.kind", "replies[].author.proven",
		"replies[].body", "replies[].created_at", "replies[].deliveries", "replies[].edited_at", "replies[].id",
		// INFRA-1523's R3, not #3178: the reply's delete mark, null on this seed's reply, as
		// comment.deleted_at and comment.deleted_by above.
		"replies[].deleted_at", "replies[].deleted_by",
		"replies[].issue_key", "replies[].mentions", "replies[].reply_block", "replies[].reply_to",
		"replies[].resolved", "replies[].resolved_at", "replies[].resolved_by", "replies[].suggestion",
		"replies[].turn",
	},
	"GET /api/v1/artifacts/{id}/blocks": {
		"[].id", "[].type", "[].from", "[].to", "[].token", "[].embedding",
		"[].attributes", "[].attributes[].kind", "[].attributes[].name", "[].attributes[].value",
		"[].references", "[].references.asks", "[].references.comments",
		"[].table", "[].table.rows", "[].table.rows[].id", "[].table.rows[].index", "[].table.rows[].cells",
		"[].table.rows[].cells[].column", "[].table.rows[].cells[].header", "[].table.rows[].cells[].id",
		"[].table.rows[].cells[].row", "[].table.rows[].cells[].text",
	},
}

// readsUnchangedSpec is the document readsUnchanged seeds. Each part of it gives a field of the
// two reads a value: the table gives the block read a table block's rows and cells, and the
// comment anchored in one of its cells that cell's place in the table; the callout gives a typed
// block's attributes.
const readsUnchangedSpec = "Introduction.\n\n" +
	"| Step | Due |\n| --- | --- |\n| Target passage | Today |\n\n" +
	":::callout{kind=\"note\" title=\"Heads up\"}\nA note.\n:::\n"

// readsUnchanged reads one comment and one document block through the reads the table already
// held, and holds each to its answer and to its plan. The answer is held to what the contracts
// declare at every depth, and to the fields the fork-point build answered for the same seed
// (readAnswerPaths); the plan is the tables the statements of that read touch.
func readsUnchanged(t *testing.T) {
	recorder := &statementRecorder{}
	database := storetest.TracedStore(t, storetest.Open(t), recorder)
	// A listener that holds no session: the comment's mention is stored and its attempt settles
	// failed, so the comment answers a mention and a delivery, each with every field it has.
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(listener.Close)
	handler, _, _ := newTestServer(t, testServerOptions{store: database, envoyURL: listener.URL})
	issue := createInteractionIssue(t, handler, "TEST", "An unchanged read", readsUnchangedSpec)
	// A field whose value is empty is one an answer may leave out, so the comment gives each of
	// its fields a value: an anchor in a table cell, a suggestion, a mention and a reply, and the
	// rejection of the suggestion, which resolves it.
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"anchor":     map[string]string{"artifact": "spec", "quote": "Target passage"},
		"body":       "Please clarify this.",
		"suggestion": map[string]string{"replace_with": "Clearer passage"},
		"mentions":   []map[string]string{{"target": "session:gone"}},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("seed a comment: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)
	if reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "It reads clearly to me.", "reply_to": comment.ID,
	}, "bob"); reply.Code != http.StatusCreated {
		t.Fatalf("seed a reply: status=%d body=%s", reply.Code, reply.Body.String())
	}
	if rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reject", nil, "alice"); rejected.Code != http.StatusOK {
		t.Fatalf("reject the suggestion: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	settledCommentDeliveries(t, database, comment.ID)

	root := repositoryRoot(t)
	declared := schemasOfGeneratedModule(t, readRepositoryFile(t, root, contractsGenerated))
	document := committedDocument(t, root)
	for _, read := range []struct{ name, target string }{
		{"GET /api/v1/comments/{id}", "/api/v1/comments/" + comment.ID},
		{"GET /api/v1/artifacts/{id}/blocks", "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/blocks"},
	} {
		t.Run(read.name, func(t *testing.T) {
			var answered []string
			statements := recorder.record(func() {
				answer := recordedRequest(t, handler, read.target, recorder)
				if answer.Code != http.StatusOK {
					t.Fatalf("%s: status=%d body=%s", read.name, answer.Code, answer.Body.String())
				}
				answered = answerFieldPaths(t, document, declared, read.name, answer.Body.Bytes())
			})
			want := readAnswerPaths[read.name]
			for _, path := range want {
				if !slices.Contains(answered, path) {
					t.Errorf("%s no longer carries %s, which it answered for this seed before this child", read.name, path)
				}
			}
			for _, path := range answered {
				if !slices.Contains(want, path) {
					t.Errorf("%s carries %s, which it did not answer for this seed before this child", read.name, path)
				}
			}
			if len(statements) == 0 {
				t.Fatalf("%s issued no statement", read.name)
			}
			relations := relationsOf(t, database, statements)
			if want := readRelations[read.name]; !slices.Equal(relations, want) {
				t.Fatalf("%s reads %v, and read %v before this child", read.name, relations, want)
			}
		})
	}
}

// componentSchemaRef is how the document refers to one of its own component schemas.
const componentSchemaRef = "#/components/schemas/"

// answerFieldPaths holds one answer of a GET route to the schema the committed document declares
// for its 200, at every depth, and answers the path of every field the answer carries
// (readAnswerPaths). Each object is held to the schema its field declares, through a reference
// or a union, as the top-level one is: a field that schema does not declare fails, and so does a
// field it requires and the answer leaves out.
func answerFieldPaths(t *testing.T, document committedOpenAPIShapes, declared map[string][]string, route string, body []byte) []string {
	t.Helper()
	method, pattern, _ := strings.Cut(route, " ")
	operation, served := document.Paths[pattern][strings.ToLower(method)]
	if !served {
		t.Fatalf("the document declares no %s", route)
	}
	var declaration struct {
		Responses map[string]struct {
			Content map[string]struct {
				Schema openAPISchema `json:"schema"`
			} `json:"content"`
		} `json:"responses"`
	}
	if err := json.Unmarshal(operation, &declaration); err != nil {
		t.Fatalf("decode the document's %s: %v", route, err)
	}
	success, declaresSuccess := declaration.Responses["200"].Content["application/json"]
	if !declaresSuccess {
		t.Fatalf("the document declares no JSON answer of %s for 200", route)
	}
	var answer any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode the answer of %s: %v", route, err)
	}
	walk := answerWalk{t: t, document: document, declared: declared, paths: map[string]bool{}}
	walk.value("", answer, success.Schema)
	return slices.Sorted(maps.Keys(walk.paths))
}

// answerWalk is one answer held to its schema: the document and the contracts module it is held
// to, and the path of every field it has met.
type answerWalk struct {
	t        *testing.T
	document committedOpenAPIShapes
	declared map[string][]string
	paths    map[string]bool
}

// value holds the value at path to schema. A null holds no field; an array holds each row to the
// schema of its rows; an object holds its fields to the ones schema declares.
func (w *answerWalk) value(path string, value any, schema openAPISchema) {
	if value == nil {
		return
	}
	schema, name := w.resolve(path, schema, value)
	switch typed := value.(type) {
	case map[string]any:
		w.object(path, typed, schema, name)
	case []any:
		if schema.Items == nil {
			w.t.Errorf("the answer carries an array at %s, where the contracts declare none", path)
			return
		}
		for _, row := range typed {
			w.value(path+"[]", row, *schema.Items)
		}
	}
}

// resolve follows a reference to the component schema it names, and a union to its branch that
// declares what value is, an object's fields or an array's rows. It answers the schema and the
// name of the component it is, "" for a schema declared in place. A union with no such branch
// answers itself, which declares no field, so every field of the value fails rather than passing
// unread.
func (w *answerWalk) resolve(path string, schema openAPISchema, value any) (openAPISchema, string) {
	if schema.Ref != "" {
		name, local := strings.CutPrefix(schema.Ref, componentSchemaRef)
		component, held := w.document.Components.Schemas[name]
		if !local || !held {
			w.t.Fatalf("the schema at %s refers to %s, which the document does not declare", path, schema.Ref)
		}
		resolved, inner := w.resolve(path, component, value)
		if inner != "" {
			return resolved, inner
		}
		return resolved, name
	}
	_, isList := value.([]any)
	for _, branch := range slices.Concat(schema.AnyOf, schema.OneOf) {
		resolved, name := w.resolve(path, branch, value)
		if (isList && resolved.Items != nil) || (!isList && resolved.Properties != nil) {
			return resolved, name
		}
	}
	return schema, ""
}

// object holds one object to its schema. A field of a component schema is held to the fields
// the contracts module declares for that component as well as to the document's, so what a
// consumer imports is what the answer carries.
func (w *answerWalk) object(path string, object map[string]any, schema openAPISchema, name string) {
	holder := name
	if holder == "" {
		holder = "the object at " + cmp.Or(path, "the top")
	}
	for _, field := range slices.Sorted(maps.Keys(object)) {
		fieldPath := field
		if path != "" {
			fieldPath = path + "." + field
		}
		fieldSchema, held := schema.Properties[field]
		if !held || (name != "" && !slices.Contains(w.declared[name], field)) {
			w.t.Errorf("the answer carries %s, which %s does not declare for %s", fieldPath, contractsGenerated, holder)
			continue
		}
		w.paths[fieldPath] = true
		w.value(fieldPath, object[field], fieldSchema)
	}
	for _, field := range schema.Required {
		if _, answered := object[field]; !answered {
			w.t.Errorf("%s declares %s.%s, which the answer does not carry at %s", contractsGenerated, holder, field, cmp.Or(path, "the top"))
		}
	}
}

// statementRecorder keeps the statements one request issued. It records on the request's own
// context alone, so the background work of a running server is no part of the record.
type statementRecorder struct {
	mu         sync.Mutex
	statements []recordedStatement
}

type recordedStatement struct {
	SQL  string
	Args []any
}

// recordingKey marks the one request whose statements are recorded.
type recordingKey struct{}

func (r *statementRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if ctx.Value(recordingKey{}) == nil {
		return ctx
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, recordedStatement{SQL: data.SQL, Args: data.Args})
	return ctx
}

func (r *statementRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// record runs read and answers the statements it issued.
func (r *statementRecorder) record(read func()) []recordedStatement {
	r.mu.Lock()
	r.statements = nil
	r.mu.Unlock()
	read()
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.statements)
}

// recordedRequest reads one route as alice, on a context the recorder watches.
func recordedRequest(t *testing.T, handler http.Handler, target string, _ *statementRecorder) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.WithValue(t.Context(), recordingKey{}, true)
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// relationsOf is every table the statements of one read touch, sorted and without repeats: the
// plan of that read, as this test holds it.
func relationsOf(t *testing.T, database *store.Store, statements []recordedStatement) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, statement := range statements {
		opening := strings.ToLower(strings.TrimSpace(statement.SQL))
		if !strings.HasPrefix(opening, "select") && !strings.HasPrefix(opening, "with") {
			continue
		}
		for _, relation := range planRelations(planWithoutSeqScans(t, database, statement.SQL, statement.Args...)) {
			seen[relation] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// planRelations reads every relation a plan names, however deep the plan nests.
func planRelations(plan json.RawMessage) []string {
	var node any
	if err := json.Unmarshal(plan, &node); err != nil {
		return nil
	}
	var found []string
	var walk func(value any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			if name, named := typed["Relation Name"].(string); named {
				found = append(found, name)
			}
			for _, key := range slices.Sorted(maps.Keys(typed)) {
				walk(typed[key])
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(node)
	return found
}
