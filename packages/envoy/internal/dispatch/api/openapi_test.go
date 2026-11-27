package api

//ci:reads packages/contracts/schemas/dispatch-openapi.json TestARefusalStatusAlsoAnswersThePlainErrorBody (the path built runtime-relative ../../../../.., then read: the committed document TestTheCommittedOpenAPIDocumentIsWhatTheEmitterWrites compares the emitter's bytes with)

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// openAPIDocumentForTest is as much of the document as these tests read: the operations by path
// and lower-cased method, and the schemas it declares.
type openAPIDocumentForTest struct {
	Paths      map[string]map[string]openAPIOperationForTest `json:"paths"`
	Components struct {
		Schemas map[string]json.RawMessage `json:"schemas"`
	} `json:"components"`
}

type openAPIOperationForTest struct {
	ID       string `json:"operationId"`
	TestHook bool   `json:"x-test-hook"`
}

// renderOpenAPI writes the document and hands back its bytes.
func renderOpenAPI(t testing.TB) []byte {
	t.Helper()
	var document bytes.Buffer
	if err := WriteOpenAPI(&document); err != nil {
		t.Fatalf("WriteOpenAPI: %v", err)
	}
	return document.Bytes()
}

// readOpenAPI writes the document and reads back what these tests assert on.
func readOpenAPI(t testing.TB) ([]byte, openAPIDocumentForTest) {
	t.Helper()
	raw := renderOpenAPI(t)
	var document openAPIDocumentForTest
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("read the emitted document: %v", err)
	}
	return raw, document
}

// The document is a committed artifact and a generator's input, so two runs on one tree must write
// the same bytes: a map walked in its own order would make every run a diff, and no reader could
// tell a real change from the walk.
func TestOpenAPIDocumentIsTheSameBytesEveryRun(t *testing.T) {
	t.Parallel()
	first, second := renderOpenAPI(t), renderOpenAPI(t)
	if !bytes.Equal(first, second) {
		t.Fatalf("two runs wrote different bytes: %d and %d bytes", len(first), len(second))
	}
	if !bytes.HasSuffix(first, []byte("\n")) {
		t.Error("the document does not end with a newline")
	}
}

// The document describes the API, so it holds the table and the table holds it: a row with no
// operation is a route a client is given no type for, and an operation that is no row is a route
// nothing serves.
func TestOpenAPIDocumentHasOneOperationPerMountedRoute(t *testing.T) {
	t.Parallel()
	_, document := readOpenAPI(t)
	described := map[string]bool{}
	for pattern, item := range document.Paths {
		for method := range item {
			described[strings.ToUpper(method)+" "+pattern] = true
		}
	}
	mounted := map[string]bool{}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
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
		t.Fatalf("the document holds %d operations and the table %d rows", len(described), len(mounted))
	}
}

// Every reference resolves to a schema the document declares. An unresolved one is a type a
// generator renders as `unknown` or refuses outright, and nothing else in the pipeline would say
// which reference it was.
func TestOpenAPIDocumentDeclaresEverySchemaItReferences(t *testing.T) {
	t.Parallel()
	tree, document := readOpenAPITree(t)
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
}

// readOpenAPITree writes the document and hands back every node of it beside the few fields
// openAPIDocumentForTest names, for the readers that walk the whole tree.
func readOpenAPITree(t testing.TB) (any, openAPIDocumentForTest) {
	t.Helper()
	raw, document := readOpenAPI(t)
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("read the emitted document: %v", err)
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

// A test hook is a row a deployment may leave out, so the document marks each one and nothing
// else: a client generated from it keeps the hooks out of the API it offers, and a hook that lost
// its mark would be offered as an ordinary route.
func TestOpenAPIDocumentMarksEveryTestHookAndNothingElse(t *testing.T) {
	t.Parallel()
	_, document := readOpenAPI(t)
	marked := map[string]bool{}
	for pattern, item := range document.Paths {
		for method, operation := range item {
			if operation.TestHook {
				marked[strings.ToUpper(method)+" "+pattern] = true
			}
		}
	}
	// The hooks are read as the rows a server with its test hooks mounts and one without does not,
	// rather than off the declarations' TestHook, which is what the emitter marks by.
	served := map[string]bool{}
	for _, route := range (&server{}).routes() {
		served[route.Method+" "+route.Pattern] = true
	}
	hooks := map[string]bool{}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		if name := route.Method + " " + route.Pattern; !served[name] {
			hooks[name] = true
		}
	}
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
}

// A status that answers a richer refusal (routeShape.Refusals) is answered with writeError's plain
// body too, by the same handler: an issue PATCH answers 409 ISSUE_CHANGED and ISSUE_CLOSED beside
// 409 FOREIGN_CHILD_OPEN. The default answer covers only the statuses the document does not name,
// so such a status must name the plain body itself, beside the rich one, and must not require a
// body to match exactly one of them, since the rich body carries code and error as well.
func TestARefusalStatusAlsoAnswersThePlainErrorBody(t *testing.T) {
	t.Parallel()
	tree, _ := readOpenAPITree(t)
	paths, _ := tree.(map[string]any)["paths"].(map[string]any)
	plain := openAPISchemaPrefix + "ErrorBody"
	checked := 0
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
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
	patch, _ := paths["/api/v1/issues/{key}"].(map[string]any)["patch"].(map[string]any)
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

// The committed artifact is what the clients' types are rendered from, so a tree whose document
// has moved on fails `go test` as well as `envoy-dispatch openapi --check`: nobody has to remember
// to run the command.
func TestTheCommittedOpenAPIDocumentIsWhatTheEmitterWrites(t *testing.T) {
	t.Parallel()
	if !bytes.Equal(committedOpenAPI(t), renderOpenAPI(t)) {
		t.Fatalf("%s is stale: run `go run ./cmd/dispatch openapi --out ../../%s` in packages/envoy and commit the result",
			OpenAPIDocumentPath, OpenAPIDocumentPath)
	}
}

// committedOpenAPI reads the document this repository holds, which is what every generated
// client is written from and so what a test about the contract a client is given reads.
func committedOpenAPI(t testing.TB) []byte {
	t.Helper()
	// From this package to the repository root, which OpenAPIDocumentPath is written from.
	path := filepath.Join("..", "..", "..", "..", "..", filepath.FromSlash(OpenAPIDocumentPath))
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the committed document: %v", err)
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
	_, document := readOpenAPI(t)
	bodies, bothDirections := openAPIRequestBodyTypes()
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
	tree, _ := readOpenAPITree(t)
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
	tree, _ := readOpenAPITree(t)
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
}

// The 409 ID_AMBIGUOUS body is written in front of a route's handler, by the prefix layer, and by
// the two reads that resolve an id written short in a query value, so no handler's own shape
// would name it. The document names it all the same, at 409, on every route that can answer it
// and on no other: a client generated from the document then reads that refusal as the type
// the Go one is reflected into, and a route whose declared ID resolves an id answers it there
// unasked.
func TestEveryRouteThatCanAnswerIDAmbiguousNamesItsBody(t *testing.T) {
	tree, _ := readOpenAPITree(t)
	want := map[string]bool{
		// The two reads that resolve a prefix of their own (writeIDPrefixError).
		"GET /api/v1/references":           true,
		"GET /api/v1/references/questions": true,
	}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		if route.ID != nil && route.ID.resolves() {
			want[route.Method+" "+route.Pattern] = true
		}
	}
	named := map[string]bool{}
	for name, responses := range openAPIResponsesByRoute(tree) {
		if slices.Contains(openAPIAnswerSchemas(responses["409"]), "IDAmbiguousError") {
			named[name] = true
		}
	}
	var wrong []string
	for name := range want {
		if !named[name] {
			wrong = append(wrong, name+" can answer 409 ID_AMBIGUOUS and the document does not name IDAmbiguousError there")
		}
	}
	for name := range named {
		if !want[name] {
			wrong = append(wrong, name+" resolves no id written short and the document names IDAmbiguousError there")
		}
	}
	sort.Strings(wrong)
	for _, found := range wrong {
		t.Error(found)
	}
	if len(named) == 0 {
		t.Fatal("no route names IDAmbiguousError, so this test proves nothing")
	}
}

// Every refusal a route gives without a richer body of its own is the common {error, code}
// (writeError), so every operation's default answer is that one schema, and a client reads the
// refusal of any route as the type errorBody is reflected into.
func TestEveryRouteAnswersTheCommonRefusalByDefault(t *testing.T) {
	tree, _ := readOpenAPITree(t)
	routes := openAPIResponsesByRoute(tree)
	var wrong []string
	for name, responses := range routes {
		if got := openAPIAnswerSchemas(responses["default"]); !slices.Equal(got, []string{"ErrorBody"}) {
			wrong = append(wrong, fmt.Sprintf("%s answers %v by default, not ErrorBody", name, got))
		}
	}
	sort.Strings(wrong)
	for _, found := range wrong {
		t.Error(found)
	}
	if len(routes) == 0 {
		t.Fatal("the document holds no operation, so this test proves nothing")
	}
}

// openAPIResponsesByRoute is each operation's responses, by "<method> <pattern>" as the table
// names its rows.
func openAPIResponsesByRoute(tree any) map[string]map[string]any {
	document, _ := tree.(map[string]any)
	paths, _ := document["paths"].(map[string]any)
	routes := map[string]map[string]any{}
	for pattern, item := range paths {
		operations, _ := item.(map[string]any)
		for method, operation := range operations {
			fields, _ := operation.(map[string]any)
			responses, _ := fields["responses"].(map[string]any)
			routes[strings.ToUpper(method)+" "+pattern] = responses
		}
	}
	return routes
}

// openAPIAnswerSchemas names the component schemas one response answers as JSON: its schema's
// reference, or each reference of its oneOf or its anyOf. The emitter writes anyOf at a status
// that answers a rich refusal beside ErrorBody (openapi.go, the combinator of a refusal status).
func openAPIAnswerSchemas(response any) []string {
	answer, _ := response.(map[string]any)
	content, _ := answer["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	schema, _ := media["schema"].(map[string]any)
	shapes := []any{schema}
	if oneOf, many := schema["oneOf"].([]any); many {
		shapes = oneOf
	} else if anyOf, many := schema["anyOf"].([]any); many {
		shapes = anyOf
	}
	var names []string
	for _, shape := range shapes {
		node, _ := shape.(map[string]any)
		if reference, isString := node["$ref"].(string); isString {
			names = append(names, strings.TrimPrefix(reference, openAPISchemaPrefix))
		}
	}
	return names
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

// openAPIPromoted is the struct TestAFieldAnEmbeddedPointerPromotesIsNotRequired embeds: one field
// with no omitempty, which the marshaller writes wherever the struct is there to write it from.
type openAPIPromoted struct {
	Count int `json:"count"`
}

// openAPIPromotedByPointer carries openAPIPromoted's field through an embedded pointer, as
// writeAdvice carries documentBlocks'.
type openAPIPromotedByPointer struct {
	Kind string `json:"kind"`
	*openAPIPromoted
}

// openAPIPromotedByValue carries the same field embedded by value.
type openAPIPromotedByValue struct {
	Kind string `json:"kind"`
	openAPIPromoted
}

// openAPIPromotedThroughValue carries it a level further down: embedded by value, through a
// struct that embeds the pointer itself.
type openAPIPromotedThroughValue struct {
	Kind string `json:"kind"`
	openAPIPromotedPointerHolder
}

type openAPIPromotedPointerHolder struct {
	*openAPIPromoted
}

// A field an embedded pointer promotes is in the object only while the pointer is set:
// encoding/json leaves out every field of a nil embedded pointer, so the server answers an issue
// write with no decision_blocks in its advice whatever that field's tag says, and a document that
// required it refused the server's own answer. The same struct embedded by value is always
// written, and its field stays required. Each row's zero value is marshalled beside it, so what the
// row expects is Go's own reading and not the emitter's, and `kind`, the owner's own field, is
// held required too, so a schema with no required at all cannot pass for one that left `count` out.
func TestAFieldAnEmbeddedPointerPromotesIsNotRequired(t *testing.T) {
	emitter, _ := newOpenAPIEmitter()
	for _, row := range []struct {
		value    any
		required bool
	}{
		{value: openAPIPromotedByPointer{}, required: false},
		{value: openAPIPromotedThroughValue{}, required: false},
		{value: openAPIPromotedByValue{}, required: true},
	} {
		goType := reflect.TypeOf(row.value)
		name := openAPISchemaName(goType)
		marshalled, err := json.Marshal(row.value)
		if err != nil {
			t.Fatalf("marshal %s: %v", openAPIGoTypeName(goType), err)
		}
		var object map[string]any
		if err := json.Unmarshal(marshalled, &object); err != nil {
			t.Fatalf("read back %s: %v", openAPIGoTypeName(goType), err)
		}
		if _, written := object["count"]; written != row.required {
			t.Fatalf("the row for %s expects count required=%v, and its zero value marshals to %s",
				openAPIGoTypeName(goType), row.required, marshalled)
		}
		if _, err := emitter.schema(row.value); err != nil {
			t.Fatal(err)
		}
		schema, declared := emitter.reflector.SpecEns().ComponentsEns().Schemas[name]
		if !declared {
			t.Fatalf("reflecting %s declared no schema %s", openAPIGoTypeName(goType), name)
		}
		required := openAPIRequiredOf(t, name, schema)
		if !slices.Contains(required, "kind") {
			t.Errorf("%s requires %v, without kind, the field it declares itself", name, required)
		}
		if slices.Contains(required, "count") != row.required {
			t.Errorf("%s requires %v, and the marshaller writes count on its zero value: %v",
				name, required, row.required)
		}
	}
	if len(emitter.failures) > 0 {
		t.Fatalf("the emit failed: %v", emitter.failures)
	}
}
