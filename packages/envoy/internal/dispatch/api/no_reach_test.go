package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

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
	t.Run("the person read is one route and takes no kind, filter or traversal", onePersonRead)
	t.Run("the person read admits a person and nobody else", personReadReach)
	t.Run("every route the table held before this child keeps the shape the contracts declare", declaredShapesUnchanged)
	t.Run("a declared shape is not an authorization", threeBadRequests)
	t.Run("a build that cannot address the new kinds reads what it read before", olderBuildReadsTheGraph)
	t.Run("the comment read and the block read are unchanged, in answer and in plan", readsUnchanged)
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
// and the fields of each schema with the ones an answer always carries. The name is the shapes
// and not the document because committedOpenAPI is already this package's reader of the
// document's own bytes (openapi_test.go).
type committedOpenAPIShapes struct {
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"schemas"`
	} `json:"components"`
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
//   - An undeclared traversal parameter: every word a caller would walk the graph with, handed
//     to the route at once. A route that honoured one would answer something else, so the
//     answer must be the answer it gives without them, status and rows alike.
func threeBadRequests(t *testing.T) {
	handler := newTestHandler(t)
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

	owned := seedTwoProjects(t, handler)
	probed := 0
	for _, route := range rows {
		target, addressable := crossProjectPath(route, owned)
		if !addressable {
			continue
		}
		probed++
		response := dispatchRequest(t, handler, route.Method, target, routeProbeBodyValue(route), "alice")
		if response.Code < 400 {
			t.Errorf("%s answers %d for an identifier of another project: %s",
				route.Method+" "+target, response.Code, firstLine(response.Body.String()))
		}
	}
	if probed < 5 {
		t.Fatalf("only %d route(s) were probed with another project's identifier, which is too few to prove anything", probed)
	}

	traversal := "?" + strings.Join(traversalQuery(), "&")
	for _, route := range rows {
		if route.Method != http.MethodGet {
			continue
		}
		plain := dispatchRequest(t, handler, route.Method, routeProbePath(route.Pattern), nil, "alice")
		walked := dispatchRequest(t, handler, route.Method, routeProbePath(route.Pattern)+traversal, nil, "alice")
		// Two answers are right: the answer the route gives without them, word for word, and a
		// refusal. A route that answered something else walked the graph for its caller.
		if walked.Code >= 400 && plain.Code < 400 {
			continue
		}
		if plain.Code != walked.Code {
			t.Errorf("%s %s answers %d on its own and %d with a traversal parameter",
				route.Method, route.Pattern, plain.Code, walked.Code)
			continue
		}
		if before, after := answerShape(plain.Body.Bytes()), answerShape(walked.Body.Bytes()); before != after {
			t.Errorf("%s %s answers %s on its own and %s with a traversal parameter",
				route.Method, route.Pattern, before, after)
		}
	}
}

// traversalQuery is the undeclared traversal every route is sent, each word with a value a
// traversal would take.
func traversalQuery() []string {
	query := make([]string, 0, len(traversalWords))
	for _, word := range traversalWords {
		query = append(query, word+"=comment")
	}
	return query
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
// project's key.
type crossProject struct {
	otherKey, otherIssue  string
	issue, comment, ask   string
	artifact, slug, block string
}

// seedTwoProjects writes one project with an issue, a document, a comment, an ask and a block,
// and a second project the probe addresses them under.
func seedTwoProjects(t *testing.T, handler http.Handler) crossProject {
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
	owned := crossProject{
		otherKey: "OTH", otherIssue: other.Key,
		issue: issue.Key, artifact: issue.PrimaryArtifactID, slug: "design",
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": "A comment of this project."}, "alice"))
	owned.comment = comment.ID
	owned.ask = createIssueAsk(t, handler, issue.Key, "A question of this project?")
	blocks := decodeBody[[]struct {
		ID string `json:"id"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	if len(blocks) == 0 {
		t.Fatal("the seeded document holds no block")
	}
	owned.block = blocks[0].ID
	return owned
}

// crossProjectPath is the route's path with another project's key and this project's object, or
// false for a route that addresses no object of a project under a key at all.
func crossProjectPath(route apiRoute, owned crossProject) (string, bool) {
	pattern := route.Pattern
	if !strings.Contains(pattern, "{key}") {
		return "", false
	}
	identifier := ""
	switch {
	case strings.Contains(pattern, "{slug}"):
		identifier = owned.slug
		pattern = strings.ReplaceAll(pattern, "{slug}", owned.slug)
	case strings.Contains(pattern, "{id}"):
		identifier = owned.comment
		if strings.Contains(pattern, "/asks/") {
			identifier = owned.ask
		}
		pattern = strings.ReplaceAll(pattern, "{id}", identifier)
	default:
		return "", false
	}
	if identifier == "" {
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
	references := "/api/v1/issues/" + issue.Key + "/references"
	now := dispatchRequest(t, handler, http.MethodGet, references, nil, "alice")
	if now.Code != http.StatusOK {
		t.Fatalf("read the graph: status=%d body=%s", now.Code, now.Body.String())
	}

	// A node of a kind nothing in this build can address, with an edge at the issue. A newer
	// build's kind reads exactly like this one to this build.
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
		values ('telepathy', 'a-kind-no-build-knows', 'telepathy', 'project', 'live', now(), '{}'::jsonb, 0)`); err != nil {
		t.Fatalf("write a node of an unknown kind: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
		values ('telepathy', 'a-kind-no-build-knows', 'mentions', 'issue', $1, now(), '{}'::jsonb, 0)`,
		issue.Key); err != nil {
		t.Fatalf("write a tuple of an unknown kind: %v", err)
	}
	after := dispatchRequest(t, handler, http.MethodGet, references, nil, "alice")
	if after.Code != http.StatusOK {
		t.Fatalf("read the graph beside an unknown kind: status=%d body=%s", after.Code, after.Body.String())
	}
	if after.Body.String() != now.Body.String() {
		t.Fatalf("the unknown kind changed the read:\n before %s\n after  %s", now.Body.String(), after.Body.String())
	}

	// The other side of the same thing: a tuple out of a node this build does address, whose
	// far end is a kind it cannot. An older build meets exactly this where a later child adds
	// a kind, and refs.resolveNodes drops the end rather than refusing the read.
	if _, err := database.Pool.Exec(ctx, `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
		values ('issue', $1, 'mentions', 'telepathy', 'a-kind-no-build-knows', now(), '{}'::jsonb, 0)`,
		issue.Key); err != nil {
		t.Fatalf("write a tuple at an unknown kind: %v", err)
	}
	both := dispatchRequest(t, handler, http.MethodGet, references, nil, "alice")
	if both.Code != http.StatusOK {
		t.Fatalf("read the graph at an unknown kind: status=%d body=%s", both.Code, both.Body.String())
	}
	if both.Body.String() != now.Body.String() {
		t.Fatalf("an edge at an unknown kind changed the read:\n before %s\n after  %s",
			now.Body.String(), both.Body.String())
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

// readsUnchanged reads one comment and one document block through the reads the table already
// held, and holds each to its answer and to its plan. The answer is what the contracts declare,
// field for field; the plan is the tables the statements of that read touch.
func readsUnchanged(t *testing.T) {
	recorder := &statementRecorder{}
	database := storetest.TracedStore(t, storetest.Open(t), recorder)
	handler, _, _ := newTestServer(t, testServerOptions{store: database})
	issue := createInteractionIssue(t, handler, "TEST", "An unchanged read", "Introduction.\n\nTarget passage.\n")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"anchor": map[string]string{"artifact": "spec", "quote": "Target passage"},
		"body":   "Please clarify this.",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("seed a comment: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)

	root := repositoryRoot(t)
	declared := schemasOfGeneratedModule(t, readRepositoryFile(t, root, contractsGenerated))
	document := committedDocument(t, root)
	for _, read := range []struct{ name, target, schema string }{
		{"GET /api/v1/comments/{id}", "/api/v1/comments/" + comment.ID, "CommentRead"},
		{"GET /api/v1/artifacts/{id}/blocks", "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/blocks", "ArtifactBlock"},
	} {
		t.Run(read.name, func(t *testing.T) {
			statements := recorder.record(func() {
				answer := recordedRequest(t, handler, read.target, recorder)
				if answer.Code != http.StatusOK {
					t.Fatalf("%s: status=%d body=%s", read.name, answer.Code, answer.Body.String())
				}
				holdAnswerToSchema(t, read.schema, declared,
					document.Components.Schemas[read.schema].Required, answer.Body.Bytes())
			})
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

// holdAnswerToSchema holds one answer to the fields the contracts declare for its shape: every
// declared field answered, and no field the declaration does not hold.
func holdAnswerToSchema(t *testing.T, schema string, declared map[string][]string, required []string, body []byte) {
	t.Helper()
	fields, held := declared[schema]
	if !held {
		t.Fatalf("%s declares no schema %s", contractsGenerated, schema)
	}
	var answer any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	rows, isList := answer.([]any)
	if !isList {
		rows = []any{answer}
	}
	if len(rows) == 0 {
		t.Fatalf("%s answered no row to hold to its shape", schema)
	}
	for _, row := range rows {
		object, isObject := row.(map[string]any)
		if !isObject {
			t.Fatalf("%s answered %T, want an object", schema, row)
		}
		for _, field := range slices.Sorted(maps.Keys(object)) {
			if !slices.Contains(fields, field) {
				t.Errorf("the answer carries %q, which %s does not declare for %s", field, contractsGenerated, schema)
			}
		}
		// A field the document does not require is one the answer leaves out where it has none
		// (Go's `omitempty`); a required one is what every reader of this shape is promised.
		for _, field := range required {
			if _, answered := object[field]; !answered {
				t.Errorf("%s declares %s.%s, which the answer does not carry", contractsGenerated, schema, field)
			}
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
