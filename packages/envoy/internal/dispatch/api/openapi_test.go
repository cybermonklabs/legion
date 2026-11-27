package api

//ci:reads packages/contracts/schemas/dispatch-openapi.json committedOpenAPIOf (the path built runtime-relative ../../../../.., then read: the committed /api/v1 document TestTheCommittedOpenAPIDocumentIsWhatTheEmitterWrites compares the emitter's bytes with)
//ci:reads packages/contracts/schemas/dispatch-openapi-v2.json committedOpenAPIOf, the same read of the committed /api/v2 document, which TestTheCommittedOpenAPIDocumentIsWhatTheEmitterWrites compares too and TestTheV2DocumentRequiresWhatTheStructsTag and TestTheV2DocumentListsEveryCodeItCanAnswer hold to the Go types and the code list

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// openAPIVersions are the versions of the API a document is emitted for (RenderOpenAPI).
var openAPIVersions = []apiVersion{apiV1, apiV2}

// openAPIDocumentForTest is as much of a document as these tests read: the operations by path
// and lower-cased method, the schemas it declares, and the /api/v2 document's code list.
type openAPIDocumentForTest struct {
	Paths      map[string]map[string]openAPIOperationForTest `json:"paths"`
	Components struct {
		Schemas      map[string]json.RawMessage `json:"schemas"`
		RefusalCodes map[string]any             `json:"x-refusal-codes"`
	} `json:"components"`
}

type openAPIOperationForTest struct {
	ID           string         `json:"operationId"`
	TestHook     bool           `json:"x-test-hook"`
	RefusalCodes map[string]any `json:"x-refusal-codes"`
}

// renderedOpenAPI is RenderOpenAPI's answer, rendered once for every test that reads a document
// rather than the emit itself (TestOpenAPIDocumentIsTheSameBytesEveryRun renders its own).
var renderedOpenAPI = sync.OnceValues(RenderOpenAPI)

// renderOpenAPI renders the documents and hands back the bytes of version's.
func renderOpenAPI(t testing.TB, version apiVersion) []byte {
	t.Helper()
	files, err := renderedOpenAPI()
	if err != nil {
		t.Fatalf("RenderOpenAPI: %v", err)
	}
	for _, file := range files {
		if file.Version == int(version) {
			return file.Bytes
		}
	}
	t.Fatalf("RenderOpenAPI rendered no /api/v%d document", version)
	return nil
}

// readOpenAPI renders version's document and reads back what these tests assert on.
func readOpenAPI(t testing.TB, version apiVersion) ([]byte, openAPIDocumentForTest) {
	t.Helper()
	raw := renderOpenAPI(t, version)
	var document openAPIDocumentForTest
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("read the emitted /api/v%d document: %v", version, err)
	}
	return raw, document
}

// eachOpenAPIVersion runs check once per document, as a subtest named for its version.
func eachOpenAPIVersion(t *testing.T, check func(t *testing.T, version apiVersion)) {
	for _, version := range openAPIVersions {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) { check(t, version) })
	}
}

// The documents are committed artifacts and a generator's input, so two runs on one tree must
// write the same bytes: a map walked in its own order would make every run a diff, and no reader
// could tell a real change from the walk.
func TestOpenAPIDocumentIsTheSameBytesEveryRun(t *testing.T) {
	t.Parallel()
	first, err := RenderOpenAPI()
	if err != nil {
		t.Fatalf("RenderOpenAPI: %v", err)
	}
	second, err := RenderOpenAPI()
	if err != nil {
		t.Fatalf("RenderOpenAPI: %v", err)
	}
	if len(first) != len(openAPIVersions) || len(second) != len(first) {
		t.Fatalf("two runs rendered %d and %d documents, want one per version, %d", len(first), len(second), len(openAPIVersions))
	}
	for index := range first {
		if !bytes.Equal(first[index].Bytes, second[index].Bytes) {
			t.Errorf("two runs wrote different bytes for the /api/v%d document: %d and %d bytes",
				first[index].Version, len(first[index].Bytes), len(second[index].Bytes))
		}
		if !bytes.HasSuffix(first[index].Bytes, []byte("\n")) {
			t.Errorf("the /api/v%d document does not end with a newline", first[index].Version)
		}
	}
}

// A document describes its version of the API, so it holds that version's routes and they hold
// it: a route with no operation is a route a client is given no type for, and an operation that
// is no route is a route nothing serves (openAPIRoutes).
func TestOpenAPIDocumentHasOneOperationPerMountedRoute(t *testing.T) {
	t.Parallel()
	eachOpenAPIVersion(t, func(t *testing.T, version apiVersion) {
		_, document := readOpenAPI(t, version)
		described := map[string]bool{}
		for pattern, item := range document.Paths {
			for method := range item {
				described[strings.ToUpper(method)+" "+pattern] = true
			}
		}
		mounted := map[string]bool{}
		for _, route := range openAPIRoutes(version) {
			mounted[route.Method+" "+route.Pattern] = true
		}
		for name := range mounted {
			if !described[name] {
				t.Errorf("%s is mounted and the document describes no operation for it", name)
			}
		}
		for name := range described {
			if !mounted[name] {
				t.Errorf("the document describes %s, which the table does not mount", name)
			}
		}
		if len(described) != len(mounted) {
			t.Fatalf("the document holds %d operations and the table %d routes", len(described), len(mounted))
		}
	})
}

// Every reference resolves to a schema the document declares. An unresolved one is a type a
// generator renders as `unknown` or refuses outright, and nothing else in the pipeline would say
// which reference it was.
func TestOpenAPIDocumentDeclaresEverySchemaItReferences(t *testing.T) {
	t.Parallel()
	eachOpenAPIVersion(t, func(t *testing.T, version apiVersion) {
		tree, document := readOpenAPITreeOf(t, version)
		references := map[string]bool{}
		walkOpenAPI(tree, "", func(_ string, node map[string]any) {
			if reference, isString := node["$ref"].(string); isString {
				references[reference] = true
			}
		})
		if len(references) == 0 {
			t.Fatal("the document holds no reference at all, so this test would pass on an empty document")
		}
		var unresolved []string
		for reference := range references {
			name, inComponents := strings.CutPrefix(reference, "#/components/schemas/")
			if !inComponents {
				unresolved = append(unresolved, reference+": not a component schema reference")
				continue
			}
			if _, declared := document.Components.Schemas[name]; !declared {
				unresolved = append(unresolved, reference+": no such schema")
			}
		}
		sort.Strings(unresolved)
		if len(unresolved) > 0 {
			t.Fatalf("references the document does not declare:\n\t%s", strings.Join(unresolved, "\n\t"))
		}
	})
}

// readOpenAPITree renders the /api/v1 document and hands back every node of it beside the few
// fields openAPIDocumentForTest names, for the readers that walk the whole tree.
func readOpenAPITree(t testing.TB) (any, openAPIDocumentForTest) {
	t.Helper()
	return readOpenAPITreeOf(t, apiV1)
}

// readOpenAPITreeOf is readOpenAPITree for the document of version.
func readOpenAPITreeOf(t testing.TB, version apiVersion) (any, openAPIDocumentForTest) {
	t.Helper()
	raw, document := readOpenAPI(t, version)
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("read the emitted /api/v%d document: %v", version, err)
	}
	return tree, document
}

// walkOpenAPI calls visit for every object the document holds, wherever it holds it - a component
// schema, a property of a nested object, a query parameter's schema - with the JSON path the
// object sits at. Its readers say what they look for and nothing of the recursion, and a node one
// of them reaches is a node they all reach.
func walkOpenAPI(node any, path string, visit func(path string, m map[string]any)) {
	switch value := node.(type) {
	case map[string]any:
		visit(path, value)
		for key, child := range value {
			walkOpenAPI(child, path+"/"+key, visit)
		}
	case []any:
		for index, child := range value {
			walkOpenAPI(child, fmt.Sprintf("%s/%d", path, index), visit)
		}
	}
}

// A test hook is a route a deployment may leave out, so each document marks each one and nothing
// else: a client generated from it keeps the hooks out of the API it offers, and a hook that lost
// its mark would be offered as an ordinary route. Each version serves the 15 hooks, /api/v2 at
// /api/v2/_test/....
func TestOpenAPIDocumentMarksEveryTestHookAndNothingElse(t *testing.T) {
	t.Parallel()
	eachOpenAPIVersion(t, func(t *testing.T, version apiVersion) {
		_, document := readOpenAPI(t, version)
		marked := map[string]bool{}
		for pattern, item := range document.Paths {
			for method, operation := range item {
				if operation.TestHook {
					marked[strings.ToUpper(method)+" "+pattern] = true
				}
			}
		}
		hooks := openAPIHookRoutes(version)
		if len(hooks) != 15 {
			t.Fatalf("the table mounts %d test hooks with Deps.TestHooksEnabled, want 15", len(hooks))
		}
		for name := range hooks {
			if !marked[name] {
				t.Errorf("%s is a test hook and its operation carries no x-test-hook", name)
			}
		}
		for name := range marked {
			if !hooks[name] {
				t.Errorf("%s carries x-test-hook and is no test hook", name)
			}
		}
	})
}

// A status that answers a richer refusal (routeShape.Refusals) is answered with writeError's plain
// body too, by the same handler: an issue PATCH answers 409 ISSUE_CHANGED and ISSUE_CLOSED beside
// 409 FOREIGN_CHILD_OPEN. The default answer covers only the statuses the document does not name,
// so such a status must name the plain body itself, beside the rich one, and must not require a
// body to match exactly one of them, since the rich body carries code and error as well.
func TestARefusalStatusAlsoAnswersThePlainErrorBody(t *testing.T) {
	t.Parallel()
	// Each version's routes are read in its own document.
	documents := map[apiVersion]map[string]any{}
	for _, version := range []apiVersion{apiV1, apiV2} {
		tree, _ := readOpenAPITreeOf(t, version)
		documents[version], _ = tree.(map[string]any)["paths"].(map[string]any)
	}
	plain := openAPISchemaPrefix + "ErrorBody"
	checked := 0
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		paths := documents[route.Version]
		name, method, pattern, shape := route.Method+" "+route.Pattern, route.Method, route.Pattern, route.Shape
		for _, refusal := range shape.Refusals {
			status := fmt.Sprint(refusal.Status)
			operation, _ := paths[pattern].(map[string]any)[strings.ToLower(method)].(map[string]any)
			response, _ := operation["responses"].(map[string]any)[status].(map[string]any)
			schema, _ := response["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
			alternatives, isAnyOf := schema["anyOf"].([]any)
			if !isAnyOf {
				t.Errorf("%s answers %s with %v, want an anyOf that holds %s beside its rich refusal", name, status, schema, plain)
				continue
			}
			refs := make([]string, 0, len(alternatives))
			for _, alternative := range alternatives {
				if ref, isRef := alternative.(map[string]any)["$ref"].(string); isRef {
					refs = append(refs, ref)
				}
			}
			if !slices.Contains(refs, plain) || len(refs) < 2 {
				t.Errorf("%s answers %s with %v, want %s beside its rich refusal", name, status, refs, plain)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no route declares a refusal, so this test read nothing")
	}
	// The issue PATCH's 409 is the case that showed it: its rich body and the plain one.
	patch, _ := documents[apiV1]["/api/v1/issues/{key}"].(map[string]any)["patch"].(map[string]any)
	conflict, _ := patch["responses"].(map[string]any)["409"].(map[string]any)
	encoded, err := json.Marshal(conflict)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{openAPISchemaPrefix + "ForeignChildOpenError", plain} {
		if !strings.Contains(string(encoded), `"`+want+`"`) {
			t.Errorf("PATCH /api/v1/issues/{key} answers 409 with %s, which does not name %s", encoded, want)
		}
	}
}

// The committed artifacts are what the clients' types are rendered from, so a tree whose document
// has moved on fails `go test` as well as `envoy-dispatch openapi --check`: nobody has to remember
// to run the command.
func TestTheCommittedOpenAPIDocumentIsWhatTheEmitterWrites(t *testing.T) {
	t.Parallel()
	eachOpenAPIVersion(t, func(t *testing.T, version apiVersion) {
		if !bytes.Equal(committedOpenAPIOf(t, version), renderOpenAPI(t, version)) {
			t.Fatalf("%s is stale: run `go run ./cmd/dispatch openapi` in packages/envoy and commit the result",
				openAPIDocumentPathOf(version))
		}
	})
}

// committedOpenAPI reads the /api/v1 document this repository holds, which is what every
// generated client of /api/v1 is written from and so what a test about the contract a client is
// given reads.
func committedOpenAPI(t testing.TB) []byte {
	t.Helper()
	return committedOpenAPIOf(t, apiV1)
}

// committedOpenAPIOf reads the committed document of version.
func committedOpenAPIOf(t testing.TB, version apiVersion) []byte {
	t.Helper()
	// From this package to the repository root, which openAPIDocumentPathOf is written from.
	path := filepath.Join("..", "..", "..", "..", "..", filepath.FromSlash(openAPIDocumentPathOf(version)))
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the committed /api/v%d document: %v", version, err)
	}
	return committed
}

// A request body's required set is what its own Go fields tag `required:"true"`, and nothing
// else. The "no omitempty" rule the other schemas take is Go's marshalling, which says what a
// reader of a response always gets and nothing at all about what a caller must send: held to it,
// createIssueInput.Parent would render `parent: string | null` where TypeScript has
// `parent?: string`, and every call site that leaves it out would stop compiling. So the two
// sets are read apart here - one from the document the emitter writes, one from the Go fields by
// reflection - and held equal, and neither can drift from the other without this failing.
func TestOpenAPIRequestBodySchemasRequireWhatTheirFieldsTag(t *testing.T) {
	t.Parallel()
	eachOpenAPIVersion(t, func(t *testing.T, version apiVersion) {
		_, document := readOpenAPI(t, version)
		bodies, bothDirections := openAPIRequestBodyTypes(openAPIRoutes(version))
		if len(bodies) == 0 {
			t.Fatal("the table reflects no request body at all, so this test would pass on anything")
		}
		held := 0
		for _, body := range sortedOpenAPITypes(bodies) {
			name := openAPISchemaName(body)
			if name == "" {
				// An unnamed struct is written where it stands, and is held against its tags by
				// the walk of the named body that holds it.
				continue
			}
			declared, inDocument := document.Components.Schemas[name]
			if !inDocument {
				t.Errorf("%s is a request body and the document declares no schema %s for it",
					openAPIGoTypeName(body), name)
				continue
			}
			var schema map[string]any
			if err := json.Unmarshal(declared, &schema); err != nil {
				t.Errorf("read schema %s: %v", name, err)
				continue
			}
			held += holdOpenAPIRequiredToTags(t, body, schema)
		}
		if held == 0 {
			t.Fatal("no request body was held to its tags, so this test proves nothing")
		}
		for _, body := range bothDirections {
			t.Errorf("%s is reflected both as a request body and as a response, and one schema "+
				"cannot answer for both directions", openAPIGoTypeName(body))
		}
	})
}

// An `enum` tag states the values a field takes, and for a slice field those are the values of
// its elements. jsonschema-go puts the tag's enum on the schema of the field it is written on,
// which for a []string is the array, and a document that said so would promise a client one of
// those strings where the route takes a list of them: ProjectGatedStepsInput.steps was generated
// as `"decisions" | "criteria" | "spec" | null` until openAPIEnumOntoItems moved it. So every
// enum of the document sits where its Go field says, and a reader of the schema can tell which.
// The whole document is walked rather than the top-level properties of its component schemas,
// since the emitter moves a query parameter's enum (queryParameters) and a nested object's by
// the same call, and an enum left on an array there reads the same way to a client.
func TestEveryEnumSitsWhereItsGoFieldPutsIt(t *testing.T) {
	t.Parallel()
	eachOpenAPIVersion(t, func(t *testing.T, version apiVersion) {
		tree, _ := readOpenAPITreeOf(t, version)
		read := 0
		var misplaced []string
		walkOpenAPI(tree, "", func(path string, node map[string]any) {
			if enum, carried := node["enum"].([]any); carried {
				read++
				if openAPITypeHolds(node["type"], "array") {
					misplaced = append(misplaced, fmt.Sprintf("%s is an array and carries the enum "+
						"%v on the array itself: a list's values are its items', so the document says "+
						"the field is one of them rather than a list of them", path, enum))
				}
			}
			if items, nested := node["items"].(map[string]any); nested {
				if _, carried := items["enum"]; carried && !openAPITypeHolds(node["type"], "array") {
					misplaced = append(misplaced, fmt.Sprintf("%s carries an enum on its items and is no array", path))
				}
			}
		})
		sort.Strings(misplaced)
		for _, found := range misplaced {
			t.Error(found)
		}
		if read == 0 {
			t.Fatal("the document declares no enum at all, so this test proves nothing")
		}
	})
}

// JSON Schema 2020-12 §6.5.3 says the entries of an object's `required` are unique, so a
// document that lists one twice is invalid however kindly a generator reads it: the one in hand
// (openapi-typescript 7.13.0) accepts it, and a stricter consumer need not. The emitter would
// repeat a name wherever a shaped read shadows a field of the row it embeds, since the
// reflector walks the embedded struct's fields into the object beside the shadowing one and
// both reach interceptProp under the same json name. The emitted tree is walked whole rather
// than its component schemas alone: a request body's nested objects are written where they
// stand and carry a required of their own.
func TestNoSchemaRequiresOneNameTwice(t *testing.T) {
	t.Parallel()
	eachOpenAPIVersion(t, func(t *testing.T, version apiVersion) {
		tree, _ := readOpenAPITreeOf(t, version)
		read := 0
		var repeated []string
		walkOpenAPI(tree, "", func(path string, node map[string]any) {
			required, isList := node["required"].([]any)
			if !isList {
				return
			}
			read++
			seen := map[string]bool{}
			for _, entry := range required {
				name, isString := entry.(string)
				if !isString {
					continue
				}
				if seen[name] {
					repeated = append(repeated, fmt.Sprintf("%s requires %s twice", path, name))
				}
				seen[name] = true
			}
		})
		sort.Strings(repeated)
		for _, found := range repeated {
			t.Error(found)
		}
		if read == 0 {
			t.Fatal("the document declares no required at all, so this test proves nothing")
		}
	})
}

// openAPITypeHolds answers whether a schema's `type`, which is one name or a list of them, holds
// the one named.
func openAPITypeHolds(declared any, want string) bool {
	switch value := declared.(type) {
	case string:
		return value == want
	case []any:
		return slices.Contains(value, any(want))
	}
	return false
}

// holdOpenAPIRequiredToTags holds one request body's schema to its Go fields' tags, and every
// unnamed struct it holds to theirs: those are written where they stand rather than declared, so
// nothing else reaches them. It answers how many schemas it read, so a walk that silently found
// nothing cannot pass for a walk that found everything right.
func holdOpenAPIRequiredToTags(t *testing.T, body reflect.Type, schema map[string]any) int {
	t.Helper()
	name := openAPIGoTypeName(body)
	got, want := openAPIRequiredOf(t, name, schema), openAPITaggedRequired(body)
	if !slices.Equal(got, want) {
		t.Errorf("%s requires %v and its fields tag %v: what a caller must send is the "+
			"handler's own refusals, which only `required:\"true\"` states", name, got, want)
	}
	read := 1
	properties, _ := schema["properties"].(map[string]any)
	for index := range body.NumField() {
		field := body.Field(index)
		held := openAPIStructOf(field.Type)
		if held == nil || held.Name() != "" {
			continue
		}
		property, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if property == "" || property == "-" {
			continue
		}
		inner, written := properties[property].(map[string]any)
		if !written {
			t.Errorf("%s holds an unnamed struct in %s and the schema writes no such property",
				name, property)
			continue
		}
		// A slice of unnamed structs is an array of them, and the struct is under `items`.
		for element := field.Type; written && element.Kind() != reflect.Struct; element = element.Elem() {
			if element.Kind() != reflect.Slice && element.Kind() != reflect.Array {
				continue
			}
			if inner, written = inner["items"].(map[string]any); !written {
				t.Errorf("%s writes %s as no array of objects", name, property)
			}
		}
		if written {
			read += holdOpenAPIRequiredToTags(t, held, inner)
		}
	}
	return read
}

// sortedOpenAPITypes orders a set of Go types by the name their package writes, so a failure
// names the same type whichever run reads it.
func sortedOpenAPITypes(types map[reflect.Type]bool) []reflect.Type {
	ordered := make([]reflect.Type, 0, len(types))
	for goType := range types {
		ordered = append(ordered, goType)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return openAPIGoTypeName(ordered[i]) < openAPIGoTypeName(ordered[j])
	})
	return ordered
}

// openAPIRequiredOf is one schema's `required`, sorted.
func openAPIRequiredOf(t *testing.T, name string, schema map[string]any) []string {
	t.Helper()
	held, carried := schema["required"].([]any)
	if !carried {
		return nil
	}
	required := make([]string, 0, len(held))
	for _, property := range held {
		text, isText := property.(string)
		if !isText {
			t.Errorf("%s requires %v, which is no property name", name, property)
			continue
		}
		required = append(required, text)
	}
	sort.Strings(required)
	return required
}

// openAPITaggedRequired is the json names of one Go type's fields tagged `required:"true"`, the
// tag jsonschema-go reads (reflect.go, ReadBoolTag), sorted. An embedded struct carries its own
// fields into the object, so its tags are read as the outer type's.
func openAPITaggedRequired(goType reflect.Type) []string {
	var required []string
	for index := range goType.NumField() {
		field := goType.Field(index)
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" && field.Anonymous && field.Type.Kind() == reflect.Struct {
			required = append(required, openAPITaggedRequired(field.Type)...)
			continue
		}
		if name == "" {
			name = field.Name
		}
		if field.Tag.Get("required") == "true" {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	return required
}

// R6 of dispatch://INFRA-4071: the /api/v2 document states what the server answers. A field of
// an answer is required exactly when its json tag carries neither omitempty nor omitzero, a
// pointer among them required and nullable and a value that is no pointer, slice, map or interface
// never null; every other field is optional; and the object holds no member its type does not
// declare. So every schema the committed document declares for an answer's Go type is walked
// against that type's fields as encoding/json reads them, and a document that marks one required
// field optional, or that drifts from its types any other way, fails here naming the schema and
// the field. In the same document opened_event_id is required on every ask shape, and each
// transition body (PUT …/state) is a union keyed by state.
func TestTheV2DocumentRequiresWhatTheStructsTag(t *testing.T) {
	documents, err := openAPIDocuments()
	if err != nil {
		t.Fatalf("emit the documents: %v", err)
	}
	emitter := documents[apiV2].emitter
	committed := committedOpenAPIOf(t, apiV2)
	var document openAPIDocumentForTest
	if err := json.Unmarshal(committed, &document); err != nil {
		t.Fatalf("read the committed /api/v2 document: %v", err)
	}
	schemaOf := func(name string) map[string]any {
		var schema map[string]any
		if err := json.Unmarshal(document.Components.Schemas[name], &schema); err != nil {
			t.Fatalf("read schema %s: %v", name, err)
		}
		return schema
	}

	held := 0
	for _, name := range slices.Sorted(maps.Keys(emitter.schemaOwners)) {
		goType := emitter.schemaOwners[name]
		if goType.Kind() != reflect.Struct || emitter.requestBodies[goType] || openAPIMarshalsItself(goType) {
			continue
		}
		if _, declared := document.Components.Schemas[name]; !declared {
			t.Errorf("%s answers an object and the document declares no schema %s for it", openAPIGoTypeName(goType), name)
			continue
		}
		holdV2SchemaToFields(t, name, goType, schemaOf(name))
		held++
	}
	if held < 100 {
		t.Fatalf("held %d answer schemas to their types, and the /api/v2 document declares hundreds: the walk found next to nothing", held)
	}

	// An ask shape is a schema whose type holds model.Ask, embedded at any depth (askV2 through
	// shapedAsk), or one that holds the member: each must require opened_event_id, so a shape that
	// drops the member fails as one that leaves it optional does.
	asks := 0
	for _, name := range slices.Sorted(maps.Keys(document.Components.Schemas)) {
		schema := schemaOf(name)
		properties, _ := schema["properties"].(map[string]any)
		property, holds := properties["opened_event_id"]
		goType, owned := emitter.schemaOwners[name]
		if !holds && (!owned || !embedsAsk(goType)) {
			continue
		}
		asks++
		required, _ := schema["required"].([]any)
		if !holds || !slices.Contains(required, any("opened_event_id")) || openAPIAllowsNull(property) {
			t.Errorf("%s is an ask shape whose opened_event_id is missing, optional or nullable: every ask of /api/v2 has one (R6)", name)
		}
	}
	if asks == 0 {
		t.Error("no schema of the /api/v2 document is an ask shape, so none was held to opened_event_id")
	}

	var tree map[string]any
	if err := json.Unmarshal(committed, &tree); err != nil {
		t.Fatalf("read the committed /api/v2 document: %v", err)
	}
	transitions := 0
	for _, route := range openAPIRoutes(apiV2) {
		if !route.v2Bound() || route.Kind != kindState {
			continue
		}
		transitions++
		body := openAPIAt(tree, "paths", route.Pattern, strings.ToLower(route.Method), "requestBody", "content", "application/json", "schema")
		members, _ := body["oneOf"].([]any)
		discriminator, _ := body["discriminator"].(map[string]any)
		mapping, _ := discriminator["mapping"].(map[string]any)
		if len(members) == 0 || discriminator["propertyName"] != "state" ||
			!slices.Equal(slices.Sorted(maps.Keys(mapping)), slices.Sorted(maps.Keys(route.Body.Arms))) {
			t.Errorf("%s %s is a transition, and its body is no union keyed by state over the states %v: %v",
				route.Method, route.Pattern, slices.Sorted(maps.Keys(route.Body.Arms)), body)
		}
	}
	if transitions == 0 {
		t.Error("the /api/v2 table holds no transition (a row of kind state), so no union was held to it")
	}
}

// embedsAsk reports whether goType, or what it points to, embeds model.Ask at any depth: the
// members of an ask are promoted into it, so its schema is an ask shape.
func embedsAsk(goType reflect.Type) bool {
	for goType.Kind() == reflect.Pointer {
		goType = goType.Elem()
	}
	if goType.Kind() != reflect.Struct {
		return false
	}
	for index := range goType.NumField() {
		field := goType.Field(index)
		if field.Anonymous && (field.Type == reflect.TypeFor[model.Ask]() || embedsAsk(field.Type)) {
			return true
		}
	}
	return false
}

// holdV2SchemaToFields holds one answer schema of the /api/v2 document to its Go type's fields as
// encoding/json writes them (openAPIJSONFields): each a property, required exactly when its tag
// carries neither omitempty nor omitzero, nullable where it is a pointer the marshaller always
// writes and never null where it is a value; and no other property, the schema closed.
func holdV2SchemaToFields(t *testing.T, name string, goType reflect.Type, schema map[string]any) {
	t.Helper()
	if open, isBool := schema["additionalProperties"].(bool); !isBool || open {
		t.Errorf("%s (%s) is not closed: an answer holds no member its type does not declare, so its schema "+
			"says additionalProperties false", name, openAPIGoTypeName(goType))
	}
	properties, _ := schema["properties"].(map[string]any)
	required, _ := schema["required"].([]any)
	fields := openAPIJSONFields(goType)
	declared := map[string]bool{}
	for _, field := range fields {
		declared[field.name] = true
		property, holds := properties[field.name]
		if !holds {
			t.Errorf("%s declares no property %s, which %s.%s writes", name, field.name, openAPIGoTypeName(goType), field.field.Name)
			continue
		}
		// A field an embedded pointer carries is left out while that pointer is nil, so it is
		// optional however it is tagged.
		always := !openAPIOmitsEmpty(field.field.Tag.Get("json")) && !field.throughPointer
		if always != slices.Contains(required, any(field.name)) {
			t.Errorf("%s: %s.%s is tagged %q, so %s is %s, and the document says otherwise", name,
				openAPIGoTypeName(goType), field.field.Name, field.field.Tag.Get("json"), field.name,
				map[bool]string{true: "required (the marshaller always writes it)", false: "optional (the marshaller leaves it out when empty)"}[always])
		}
		if !always {
			continue
		}
		switch nullable := openAPIAllowsNull(property); field.field.Type.Kind() {
		case reflect.Pointer:
			if !nullable {
				t.Errorf("%s: %s is a pointer the marshaller always writes, and the document does not let it be null", name, field.name)
			}
		case reflect.Slice, reflect.Map, reflect.Interface:
		default:
			if nullable {
				t.Errorf("%s: %s is a %s, which is never null, and the document lets it be null", name, field.name, field.field.Type)
			}
		}
	}
	for _, property := range slices.Sorted(maps.Keys(properties)) {
		if !declared[property] {
			t.Errorf("%s declares %s, which no field of %s writes", name, property, openAPIGoTypeName(goType))
		}
	}
}

// openAPIJSONField is one member encoding/json writes for a struct: its name, the field that
// holds it, how deep in embedded structs that field stands, whether its tag names it, and whether
// an embedded pointer carries it, which leaves it out while the pointer is nil.
type openAPIJSONField struct {
	name           string
	field          reflect.StructField
	depth          int
	tagged         bool
	throughPointer bool
}

// openAPIJSONFields are the members encoding/json writes for a struct, in field order: its own
// exported fields, and those an embedded struct with no json name carries into it, a field of one
// name the shallowest wins, a tagged one among the shallowest, and none where two untagged share
// the shallowest depth (encoding/json's typeFields).
func openAPIJSONFields(goType reflect.Type) []openAPIJSONField {
	var candidates []openAPIJSONField
	var walk func(structure reflect.Type, depth int, throughPointer bool)
	walk = func(structure reflect.Type, depth int, throughPointer bool) {
		for index := range structure.NumField() {
			field := structure.Field(index)
			tag := field.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if field.Anonymous && name == "" && embedded.Kind() == reflect.Struct {
				walk(embedded, depth+1, throughPointer || field.Type.Kind() == reflect.Pointer)
				continue
			}
			if !field.IsExported() {
				continue
			}
			candidates = append(candidates, openAPIJSONField{
				name: cmp.Or(name, field.Name), field: field, depth: depth, tagged: name != "", throughPointer: throughPointer,
			})
		}
	}
	walk(goType, 0, false)
	var fields []openAPIJSONField
	decided := map[string]bool{}
	for index, candidate := range candidates {
		if decided[candidate.name] {
			continue
		}
		decided[candidate.name] = true
		var rivals []openAPIJSONField
		for _, other := range candidates[index:] {
			if other.name == candidate.name {
				rivals = append(rivals, other)
			}
		}
		shallowest := slices.MinFunc(rivals, func(left, right openAPIJSONField) int { return left.depth - right.depth }).depth
		rivals = slices.DeleteFunc(rivals, func(rival openAPIJSONField) bool { return rival.depth != shallowest })
		if tagged := slices.DeleteFunc(slices.Clone(rivals), func(rival openAPIJSONField) bool { return !rival.tagged }); len(tagged) > 0 {
			rivals = tagged
		}
		if len(rivals) == 1 {
			fields = append(fields, rivals[0])
		}
	}
	return fields
}

// openAPIAllowsNull reports whether a schema takes the JSON literal null: a type that names it, a
// const of it, or an arm of an anyOf or oneOf that does.
func openAPIAllowsNull(node any) bool {
	schema, isObject := node.(map[string]any)
	if !isObject {
		return false
	}
	if value, constant := schema["const"]; openAPITypeHolds(schema["type"], "null") || (constant && value == nil) {
		return true
	}
	for _, combinator := range []string{"anyOf", "oneOf"} {
		arms, _ := schema[combinator].([]any)
		if slices.ContainsFunc(arms, openAPIAllowsNull) {
			return true
		}
	}
	return false
}

// openAPIAt is the object at path in a decoded document, and nil where path names none.
func openAPIAt(node map[string]any, path ...string) map[string]any {
	for _, key := range path {
		node, _ = node[key].(map[string]any)
	}
	return node
}

// Decision 3 of the plan of dispatch://INFRA-4071: the /api/v2 document lists every code /api/v2
// can answer, once, with its status, and it is the declared list of code constants, so a code
// added to apicode reaches the document and the clients generated from it, and one the document
// names cannot be a code nothing declares. ANSWER_NOT_KEPT's status is `*`, its call's; a code
// only /api/v1 answers (API_V1_ENDED) is no code of the list. Each operation's own list is drawn
// from it, at the same statuses.
func TestTheV2DocumentListsEveryCodeItCanAnswer(t *testing.T) {
	var document openAPIDocumentForTest
	if err := json.Unmarshal(committedOpenAPIOf(t, apiV2), &document); err != nil {
		t.Fatalf("read the committed /api/v2 document: %v", err)
	}
	want := map[string]any{}
	for _, code := range apicode.All() {
		if !code.InV2() {
			continue
		}
		status := any(float64(code.Status()))
		if code.StatusFromCall() {
			status = "*"
		}
		if listed, named := want[code.Name()]; named && listed != status {
			t.Errorf("apicode declares %s at %v and at %v: a /api/v2 name has one status", code.Name(), listed, status)
		}
		want[code.Name()] = status
	}
	got := document.Components.RefusalCodes
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if listed, named := got[name]; !named || listed != want[name] {
			t.Errorf("components.x-refusal-codes gives %s %v, want %v", name, listed, want[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(got)) {
		if _, declared := want[name]; !declared {
			t.Errorf("components.x-refusal-codes names %s, which is no /api/v2 code of apicode", name)
		}
	}
	if _, named := got[apicode.APIV1Ended.V1Name()]; named || len(got) != len(want) {
		t.Errorf("components.x-refusal-codes holds %d codes, want the %d /api/v2 names of apicode without %s",
			len(got), len(want), apicode.APIV1Ended.V1Name())
	}
	if got["ANSWER_NOT_KEPT"] != "*" {
		t.Errorf("components.x-refusal-codes gives ANSWER_NOT_KEPT %v, want *: its status is the call's", got["ANSWER_NOT_KEPT"])
	}

	operations := 0
	for pattern, item := range document.Paths {
		for method, operation := range item {
			if operation.RefusalCodes == nil {
				continue
			}
			operations++
			for code, status := range operation.RefusalCodes {
				if listed, named := want[code]; !named || listed != status {
					t.Errorf("%s %s lists %s at %v, which the document's list gives %v", strings.ToUpper(method), pattern, code, status, listed)
				}
			}
		}
	}
	if operations == 0 {
		t.Error("no operation of the /api/v2 document lists its refusal codes")
	}
}

// R1 of dispatch://INFRA-4071/spec: one item has one shape. The 201 answer the /api/v2 document
// gives POST /api/v2/projects holds exactly the members of an item of GET /api/v2/projects's page,
// each $ref followed to the schema it names, default_assignee among them, the default the create
// stored. A create that answers a project under another name of a member (default_repo, where every
// other /api/v2 answer of a project says default_repository), or with a member the list's item
// does not hold, fails here.
func TestTheV2CreateOfAProjectAnswersTheItemItsListPages(t *testing.T) {
	var document map[string]any
	if err := json.Unmarshal(committedOpenAPIOf(t, apiV2), &document); err != nil {
		t.Fatalf("read the committed /api/v2 document: %v", err)
	}
	resolve := func(schema map[string]any) map[string]any {
		for {
			reference, names := schema["$ref"].(string)
			if !names {
				return schema
			}
			schema = openAPIAt(document, "components", "schemas", strings.TrimPrefix(reference, openAPISchemaPrefix))
		}
	}
	answer := func(method, status string) map[string]any {
		return resolve(openAPIAt(document, "paths", "/api/v2/projects", method, "responses", status, "content", "application/json", "schema"))
	}
	membersOf := func(schema map[string]any) []string {
		properties, _ := schema["properties"].(map[string]any)
		return slices.Sorted(maps.Keys(properties))
	}
	created := membersOf(answer("post", "201"))
	listed := membersOf(resolve(openAPIAt(answer("get", "200"), "properties", "items", "items")))
	if len(listed) == 0 {
		t.Fatalf("the /api/v2 document declares no members of an item of GET /api/v2/projects's page")
	}
	if !slices.Equal(created, listed) {
		t.Errorf("POST /api/v2/projects answers 201 the members %v, and an item of GET /api/v2/projects's page "+
			"holds %v: one project has one shape at /api/v2", created, listed)
	}
	if !slices.Contains(created, "default_assignee") {
		t.Errorf("POST /api/v2/projects answers 201 the members %v, without default_assignee, the default the create stored", created)
	}
}
