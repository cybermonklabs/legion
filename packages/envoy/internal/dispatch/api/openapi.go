// openapi.go emits the OpenAPI 3.1 documents of the Dispatch HTTP API from the route table and the
// Go types each of its rows takes and answers (INFRA-1406): one document per version of the API,
// /api/v1's from the v1 table and routeShapes, and /api/v2's from the query, body and answer types
// each /api/v2 row declares (dispatch://INFRA-4071, R6). It is the one generator between the
// server and its clients: the dashboard's and the CLI's TypeScript types are rendered from the
// documents this file writes, so a route the table grows reaches them without anyone hand-writing
// a type for it, and a shape that changes cannot quietly disagree with the handler that answers it.
//
// Nothing here reads the database or opens a listener, as WriteRouteIndex needs neither: the table
// with its test hooks in, routeShapes beside it and reflection over the Go types are the whole
// input. `envoy-dispatch openapi` writes both documents and `--check` holds each committed
// artifact to them.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go/openapi31"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// OpenAPIDocumentPath and OpenAPIV2DocumentPath are where the emitted documents of /api/v1 and
// /api/v2 are committed, from the repository root. The subcommand's --check and the test that
// holds each artifact to the emitter resolve them against their own directory, so each path is
// written once.
const (
	OpenAPIDocumentPath   = "packages/contracts/schemas/dispatch-openapi.json"
	OpenAPIV2DocumentPath = "packages/contracts/schemas/dispatch-openapi-v2.json"
)

// openAPISchemaPrefix is where a schema is referenced from, which is also what decides the name
// its TypeScript export carries.
const openAPISchemaPrefix = "#/components/schemas/"

// openAPISessionParameter is the one query parameter every operation carries. It is read by
// actingPerson (server.go) on every route that resolves a person rather than by any one handler,
// so it belongs to the whole API and routeShapes names it on no row.
const openAPISessionParameter = "the session the caller's credential acts for, where that " +
	"credential proved none; a credential that proved a session is refused a parameter naming " +
	"another (403 ACTOR_SESSION_MISMATCH)"

// OpenAPIFile is one document `envoy-dispatch openapi` writes: the version of the API it
// describes, where the repository commits it, and its bytes.
type OpenAPIFile struct {
	Version int
	Path    string
	Bytes   []byte
}

// RenderOpenAPI renders the OpenAPI 3.1 document of each version of the API, /api/v1's first,
// each of every route that version mounts with its test hooks in, two spaces to the level and a
// newline at its end. Two runs on one tree write identical bytes: every map it walks is sorted
// and every list it writes is the table's own order. Both are rendered in one call because the
// emitter's own tables (schemaNames, rawFieldSchemas) serve both, and a row of them is stale only
// when neither document reads it.
func RenderOpenAPI() ([]OpenAPIFile, error) {
	documents, err := openAPIDocuments()
	if err != nil {
		return nil, err
	}
	files := make([]OpenAPIFile, 0, len(documents))
	for _, version := range []apiVersion{apiV1, apiV2} {
		var rendered bytes.Buffer
		encoder := json.NewEncoder(&rendered)
		encoder.SetIndent("", "  ")
		// A description quoting `<owner>/<slug>` or an `&` is written as it stands: this document
		// is read by people as well as by generators.
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(documents[version].spec); err != nil {
			return nil, fmt.Errorf("write the /api/v%d document: %w", version, err)
		}
		files = append(files, OpenAPIFile{Version: int(version), Path: openAPIDocumentPathOf(version), Bytes: rendered.Bytes()})
	}
	return files, nil
}

// openAPIDocumentPathOf is where the document of version is committed.
func openAPIDocumentPathOf(version apiVersion) string {
	if version == apiV2 {
		return OpenAPIV2DocumentPath
	}
	return OpenAPIDocumentPath
}

// openAPIEmitter reflects the Go types of the shapes and collects what it learns: the schemas the
// document declares, the name each Go type took, and every refusal it owes the caller. Failures
// are gathered rather than returned one at a time, so one run names every name collision and every
// stale rawFieldSchemas row instead of stopping at the first.
type openAPIEmitter struct {
	// version is the version of the API the document describes. The /api/v2 document states
	// what R6 of dispatch://INFRA-4071 asks of it beside what the /api/v1 one states (each
	// operation's refusal codes, its row's declarations, its unions, the object schemas closed),
	// and the /api/v1 document stays the bytes it was.
	version   apiVersion
	reflector *openapi31.Reflector
	// schemas is the schema of each Go type already reflected. Reflect builds a fresh context
	// and walks the whole reachable type graph every call, and the routes reflect one type
	// over and over: errorBody once per route. A returned map is read, wrapped or marshalled
	// by its callers and never written, so one value serves every caller of its type.
	schemas map[reflect.Type]map[string]any
	// schemaOwners is the Go type each schema name was taken by, which is how a second type
	// wanting one name is caught: jsonschema-go would otherwise resolve the clash by suffixing
	// the second name, and a TypeScript consumer would import a type called `IssueSummaryType2`.
	schemaOwners map[string]reflect.Type
	// rawFragments is each rawFieldSchemas row resolved to the schema its field carries.
	rawFragments map[rawFieldKeyOf]map[string]any
	// rawFieldsSeen is every rawFieldSchemas row a reflected field matched.
	rawFieldsSeen map[rawFieldKeyOf]bool
	// requestBodies are the Go types the document reflects as a request body and nowhere else,
	// whose required set is the fields that tag themselves required rather than the fields Go's
	// marshaller always writes (openAPIRequestBodyTypes).
	requestBodies map[reflect.Type]bool
	// failures are the refusals gathered so far, each a whole sentence.
	failures []string
}

// newOpenAPIEmitter builds the emitter the document of version is reflected with, and answers
// beside it every Go type routes reflect as a request body and as a response both, which no one
// schema can carry (openAPIRequestBodyTypes).
func newOpenAPIEmitter(version apiVersion, routes []apiRoute) (*openAPIEmitter, []reflect.Type) {
	bodies, bothDirections := openAPIRequestBodyTypes(routes)
	emitter := &openAPIEmitter{
		version:       version,
		reflector:     openapi31.NewReflector(),
		schemaOwners:  map[string]reflect.Type{},
		rawFragments:  map[rawFieldKeyOf]map[string]any{},
		rawFieldsSeen: map[rawFieldKeyOf]bool{},
		requestBodies: bodies,
		schemas:       map[reflect.Type]map[string]any{},
	}
	emitter.reflector.DefaultOptions = append(emitter.reflector.DefaultOptions,
		jsonschema.InterceptDefName(emitter.defName),
		jsonschema.InterceptProp(emitter.interceptProp),
	)
	if version == apiV2 {
		emitter.reflector.DefaultOptions = append(emitter.reflector.DefaultOptions,
			jsonschema.InterceptSchema(emitter.closeObject))
	}
	return emitter, bothDirections
}

// openAPIEmit is one document as the emitter built it, with the emitter that learned what it
// declares (the Go type each schema name took, the rawFieldSchemas rows its fields matched).
type openAPIEmit struct {
	spec    *openapi31.Spec
	emitter *openAPIEmitter
}

// openAPIDocuments builds the document of each version of the API, and refuses the emit, naming
// every fault of both, where either document or the emitter's tables are wrong: a row of
// schemaNames or rawFieldSchemas serves both documents, so it is stale only when neither declares
// what it names.
func openAPIDocuments() (map[apiVersion]openAPIEmit, error) {
	documents := map[apiVersion]openAPIEmit{}
	var failures []string
	for _, version := range []apiVersion{apiV1, apiV2} {
		emitter, spec, err := openAPIDocument(version)
		if err != nil {
			return nil, fmt.Errorf("emit the /api/v%d OpenAPI document: %w", version, err)
		}
		for _, failure := range emitter.failures {
			failures = append(failures, fmt.Sprintf("the /api/v%d document: %s", version, failure))
		}
		documents[version] = openAPIEmit{spec: spec, emitter: emitter}
	}
	v1, v2 := documents[apiV1].emitter, documents[apiV2].emitter
	for key := range rawFieldSchemas {
		if !v1.rawFieldsSeen[key] && !v2.rawFieldsSeen[key] {
			failures = append(failures, fmt.Sprintf("rawFieldSchemas names %s, which is no field of "+
				"any type either document describes: drop the row or name the field it meant", key))
		}
	}
	for goType, name := range schemaNames {
		if v1.schemaOwners[name] != goType && v2.schemaOwners[name] != goType {
			failures = append(failures, fmt.Sprintf("schemaNames gives %s the name %s, and neither "+
				"document declares a schema of that type: drop the row or name the type it meant",
				openAPIGoTypeName(goType), name))
		}
	}
	for goType := range itemUnions {
		if name := openAPISchemaName(goType); v1.schemaOwners[name] != goType && v2.schemaOwners[name] != goType {
			failures = append(failures, fmt.Sprintf("itemUnions names %s, and neither document declares "+
				"a schema of that type: drop the row or name the type it meant", openAPIGoTypeName(goType)))
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return nil, fmt.Errorf("emit the OpenAPI documents:\n\t%s", strings.Join(slices.Compact(failures), "\n\t"))
	}
	return documents, nil
}

// openAPIDocument builds the document of one version, and answers the emitter beside it, whose
// failures are the faults of this document alone. It walks the table with its test hooks in,
// since a hook's row is a row of the table wherever a deployment leaves it out
// (scopedRoutesNotMounted), and marks each hook row x-test-hook so a reader of the document can
// tell them apart.
func openAPIDocument(version apiVersion) (*openAPIEmitter, *openapi31.Spec, error) {
	routes := openAPIRoutes(version)
	emitter, bothDirections := newOpenAPIEmitter(version, routes)
	for _, body := range bothDirections {
		emitter.fail("%s is reflected both as a request body and as a response, and one schema "+
			"cannot answer for both: a request body requires the fields tagged required:\"true\" "+
			"and a response the fields Go's marshaller always writes, so either reading is wrong "+
			"for one of its two directions. Give each direction a Go type of its own.",
			openAPIGoTypeName(body))
	}

	spec := emitter.reflector.SpecEns()
	spec.Openapi = "3.1.0"
	spec.Info.WithTitle("Dispatch HTTP API")
	switch version {
	case apiV2:
		spec.Info.WithVersion("2")
		spec.Info.WithDescription(
			"Generated from the /api/v2 rows of the Go route table (the v2<Group>Rows functions of " +
				"internal/dispatch/api/v2_<group>.go) and the query, body and answer types each row " +
				"declares, by `envoy-dispatch openapi`. Edit those, never this document: it is " +
				"committed so a client's types can be rendered from it, and `envoy-dispatch openapi " +
				"--check` refuses a tree where it is stale. Each operation of a /api/v2 row lists " +
				"the refusal codes it can answer with their statuses (x-refusal-codes): its row's own and " +
				"those of every layer the server runs it behind, but the router's origin and host guards " +
				"in front of every route (ORIGIN_REFUSED, HOST_MISMATCH), the test hooks " +
				"(SLOW_WRITE_CUT_OFF) and a server built with no store (DATABASE_UNAVAILABLE). " +
				"components.x-refusal-codes lists every code /api/v2 can answer, once; every object a " +
				"Go struct answers declares each member it can hold and no other.")
	default:
		spec.Info.WithVersion("1")
		spec.Info.WithDescription(
			"Generated from the Go route table (internal/dispatch/api/routes_table.go) and the Go " +
				"types beside it (routes_shapes.go) by `envoy-dispatch openapi`. Edit those, never " +
				"this document: it is committed so a client's types can be rendered from it, and " +
				"`envoy-dispatch openapi --check` refuses a tree where it is stale.")
	}

	if err := emitter.resolveRawFields(routes); err != nil {
		return nil, nil, err
	}

	hooks := openAPIHookRoutes(version)
	paths := map[string]*openapi31.PathItem{}
	operationIDs := map[string]string{}
	for _, route := range routes {
		name := route.Method + " " + route.Pattern
		operation, err := emitter.operation(route, hooks[name])
		if err != nil {
			return nil, nil, err
		}
		id := *operation.ID
		if taken, clash := operationIDs[id]; clash {
			emitter.fail("operationId %s is taken by both %s and %s", id, taken, name)
		}
		operationIDs[id] = name
		item := paths[route.Pattern]
		if item == nil {
			item = &openapi31.PathItem{}
			paths[route.Pattern] = item
		}
		if err := item.SetOperation(strings.ToLower(route.Method), operation); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	for pattern, item := range paths {
		spec.PathsEns().WithMapOfPathItemValuesItem(pattern, *item)
	}
	if err := emitter.writeItemUnions(); err != nil {
		return nil, nil, err
	}
	if version == apiV2 {
		emitter.writeCodeList(spec)
	}
	return emitter, spec, nil
}

// openAPIRoutes are the routes the document of version describes: every route that version
// serves, with the test hooks in, since a hook's row is a row of the table wherever a deployment
// leaves it out (scopedRoutesNotMounted).
func openAPIRoutes(version apiVersion) []apiRoute {
	return servedAt(version, (&server{deps: Deps{TestHooksEnabled: true}}).routes())
}

// openAPIHookRoutes names every route of version the table mounts only with
// Deps.TestHooksEnabled, read as the difference between the two tables rather than listed, so a
// hook added to routes() is marked without this file being touched.
func openAPIHookRoutes(version apiVersion) map[string]bool {
	served := map[string]bool{}
	for _, route := range (&server{}).routes() {
		served[route.Method+" "+route.Pattern] = true
	}
	hooks := map[string]bool{}
	for _, route := range openAPIRoutes(version) {
		name := route.Method + " " + route.Pattern
		if !served[name] {
			hooks[name] = true
		}
	}
	return hooks
}

// fail records a refusal the whole emit carries.
func (e *openAPIEmitter) fail(format string, args ...any) {
	e.failures = append(e.failures, fmt.Sprintf(format, args...))
}

// defName decides the schema name of every Go type the document declares: the type's own name with
// its first letter upper-cased (model.Issue to Issue, api.issueCheckResults to IssueCheckResults),
// or the name schemaNames gives it. A name two distinct types want fails the emit, naming both.
func (e *openAPIEmitter) defName(t reflect.Type, defaultDefName string) string {
	name := openAPISchemaName(t)
	if name == "" {
		return defaultDefName
	}
	switch owner, taken := e.schemaOwners[name]; {
	case !taken:
		e.schemaOwners[name] = t
	case owner != t:
		e.fail("schema name %s is wanted by both %s and %s: name one of them in schemaNames",
			name, openAPIGoTypeName(owner), openAPIGoTypeName(t))
	}
	return name
}

// openAPISchemaName is the schema name of one Go type, which is the name its TypeScript export
// carries. An instance of a generic type is named for the type and its arguments, since its Go
// name is no identifier (openAPIGenericName).
func openAPISchemaName(t reflect.Type) string {
	if name, overridden := schemaNames[t]; overridden {
		return name
	}
	name := t.Name()
	if name == "" {
		return ""
	}
	if strings.Contains(name, "[") {
		return openAPIGenericName(name)
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// openAPIQualifier matches a type argument's import path and the dot after it, in the name reflect
// gives an instance of a generic type: page[github.com/sjawhar/envoy/internal/dispatch/api.askV2].
var openAPIQualifier = regexp.MustCompile(`[\w./-]*\.(\w+)`)

// openAPIGenericName names an instance of a generic type from the name reflect gives it: the
// type's own name, `Of`, and each type argument's name without its package, each word
// upper-cased and every other character dropped, so page[api.askV2] is PageOfAskV2 and
// page[[]string] PageOfString.
func openAPIGenericName(name string) string {
	base, arguments, _ := strings.Cut(name, "[")
	arguments = openAPIQualifier.ReplaceAllString(arguments, "$1")
	var named strings.Builder
	named.WriteString(openAPICamel(base))
	named.WriteString("Of")
	named.WriteString(openAPICamel(arguments))
	return named.String()
}

// openAPIGoTypeName names a Go type as its package writes it, for a refusal a reader must act on.
func openAPIGoTypeName(t reflect.Type) string {
	if path := t.PkgPath(); path != "" {
		return path[strings.LastIndexByte(path, '/')+1:] + "." + t.Name()
	}
	return t.String()
}

// openAPIRequestBodyTypes answers the Go types the document reflects as a request body and
// nowhere else, and beside them every request body a response reflects too.
//
// A request body's required set is not a response's. Go's marshaller always writes a field with
// no `,omitempty`, so a reader of a response always gets it and the document is right to require
// it. A handler is happy to receive a body without such a field - createIssueInput.Parent is a
// *string with `json:"parent"`, and every caller today sends a body with no parent in it - so a
// document that required it would render `parent: string | null` where TypeScript has
// `parent?: string`, and every call site that leaves it out would stop compiling. What a caller
// must send is the handler's own refusals, which no json tag states and `required:"true"` does.
//
// The reach is every Shape.Request and every arm of a union body, every type a rawFieldSchemas
// row names inside one, and everything those reach through the JSON they carry. A type an answer
// reaches too is left out and keeps the response rule: one schema serves both directions there,
// and the response's reading is the one its TypeScript consumers already have (model.Actor, read
// on a write and answered on every read, is required to carry kind and id in both). A request
// body that is itself an answer cannot be settled that way, since neither rule can be right for
// both of its directions, and is named here for the emit to refuse.
func openAPIRequestBodyTypes(routes []apiRoute) (map[reflect.Type]bool, []reflect.Type) {
	request, response := map[reflect.Type]bool{}, map[reflect.Type]bool{}
	var bodies []reflect.Type
	reach := func(value any, into map[reflect.Type]bool, carried *[]reflect.Type) {
		if value != nil {
			openAPIReach(reflect.TypeOf(value), into, carried)
		}
	}
	// In the table's key order, so a refusal names the same type whichever run reads it.
	routes = slices.Clone(routes)
	slices.SortFunc(routes, func(left, right apiRoute) int {
		return strings.Compare(left.Method+" "+left.Pattern, right.Method+" "+right.Pattern)
	})
	for _, route := range routes {
		shape := route.shape()
		for _, body := range slices.Concat([]any{shape.Request}, openAPIUnionArms(route)) {
			if body != nil {
				bodies = append(bodies, reflect.TypeOf(body))
				reach(body, request, &bodies)
			}
		}
		reach(shape.Response, response, nil)
		for _, advice := range slices.Concat([]any{shape.Advice}, shape.OtherAdvice) {
			reach(advice, response, nil)
		}
		for _, answer := range slices.Concat(shape.Also, shape.Refusals) {
			for _, carried := range slices.Concat([]any{answer.Body, answer.Advice}, answer.OtherAdvice) {
				reach(carried, response, nil)
			}
		}
		if route.v2Bound() {
			// The decoder's refusal of an undeclared query key, which every such route answers
			// (responses).
			openAPIReach(reflect.TypeFor[queryKeyUnknownBody](), response, nil)
		}
	}
	// The default answer every refusal richer than it rides on (responses).
	openAPIReach(reflect.TypeFor[errorBody](), response, nil)

	var bothDirections []reflect.Type
	for _, body := range bodies {
		if response[body] && !slices.Contains(bothDirections, body) {
			bothDirections = append(bothDirections, body)
		}
	}
	for answered := range response {
		delete(request, answered)
	}
	return request, bothDirections
}

// openAPIUnionArms are the arms of a /api/v2 route's union body, in the order of the values that
// pick them, and nil for every other route: apiRoute.shape leaves a union's arms out of its
// Request, since they are the v2 document's to describe (unionBody).
func openAPIUnionArms(route apiRoute) []any {
	if !route.v2Bound() || (route.Body.Kind != bodyStateUnion && route.Body.Kind != bodyKindUnion) {
		return nil
	}
	arms := make([]any, 0, len(route.Body.Arms))
	for _, value := range slices.Sorted(maps.Keys(route.Body.Arms)) {
		arms = append(arms, route.Body.Arms[value])
	}
	return arms
}

// openAPIReach gathers into every struct type the JSON of one Go type reaches: the type itself,
// what its fields carry through the pointers, slices, arrays and maps between, what a
// json.RawMessage field's bytes hold, which is rawFieldSchemas' to say rather than the field's
// own Go type, and the arms of a type itemUnions names, whose members its bytes carry too.
// carried, where it is given, collects each type rawFieldSchemas names and each such arm, since
// the document reflects those as bodies of their own.
func openAPIReach(t reflect.Type, into map[reflect.Type]bool, carried *[]reflect.Type) {
	t = openAPIStructOf(t)
	if t == nil || into[t] {
		return
	}
	into[t] = true
	openAPIReachFields(t, t, into, carried)
	if union, named := itemUnions[t]; named {
		for _, value := range slices.Sorted(maps.Keys(union.Arms)) {
			arm := reflect.TypeOf(union.Arms[value])
			if carried != nil {
				*carried = append(*carried, arm)
			}
			openAPIReach(arm, into, carried)
		}
	}
}

// openAPIReachFields walks the fields one object's JSON holds: owner's own, and those an
// embedded struct with no json name of its own carries into it. Such a struct is no object of
// its own — the reflector writes its fields where they stand, as it writes an unnamed struct's,
// and the document declares no schema for it — so it is walked here rather than reached as a
// type, and a rawFieldSchemas row naming one of its fields is keyed by the object that carries
// it, which is how the emitter reads the row back (rawFieldKey).
func openAPIReachFields(owner, t reflect.Type, into map[reflect.Type]bool, carried *[]reflect.Type) {
	for index := range t.NumField() {
		field := t.Field(index)
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if field.Anonymous && name == "" {
			if embedded := openAPIStructOf(field.Type); embedded != nil {
				openAPIReachFields(owner, embedded, into, carried)
				continue
			}
		}
		if name == "" {
			name = field.Name
		}
		if row, named := rawFieldSchemas[rawFieldKeyOf{Owner: owner, Field: name}]; named && row.GoType != nil {
			held := reflect.TypeOf(row.GoType)
			if carried != nil {
				*carried = append(*carried, held)
			}
			openAPIReach(held, into, carried)
			continue
		}
		openAPIReach(field.Type, into, carried)
	}
}

// openAPIStructOf is the struct one Go type carries its JSON object in, through the pointers,
// slices, arrays and maps around it, and nil for a type that carries no object. A reflected
// property's parent is sometimes the pointer rather than the struct behind it (an *AnchorInput
// field), and a schema's required set is the struct's whichever of the two names it.
func openAPIStructOf(t reflect.Type) reflect.Type {
	for t != nil && (t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice ||
		t.Kind() == reflect.Array || t.Kind() == reflect.Map) {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	return t
}

// interceptProp decides, for every property of every reflected struct, whether it is required,
// whether it may be null, and what a json.RawMessage field carries.
//
// Nullability is Go's own marshalling semantics throughout: a field the marshaller leaves out
// when it is empty — `,omitempty`, and `,omitzero` beside it, which omits a zero value of any
// type — is optional and never null. Required is Go's semantics for a response, where a field
// carrying neither is one the marshaller always writes and a reader therefore always
// gets. It is not for a request body (openAPIRequestBodyTypes), where what a caller must send is
// the handler's own refusals and no json tag states it: there a property is required only when
// its Go field carries `required:"true"`, which jsonschema-go has read into the parent's required
// before this runs. A field rawFieldSchemas names is left out of required by its json tag alone —
// absent is one of its three states — and carries the schema that table gives it.
//
// A name is added to required once however often it is reflected. A shaped read shadows a field
// of the row it embeds - architectureTreeRead declares `components` in front of the
// model.ArchitectureTree it embeds - and the reflector walks the embedded struct's fields into
// the object beside the shadowing one, so both reach this with the same json name. JSON Schema
// 2020-12 §6.5.3 says the entries of required are unique, and the first of the two keeps the
// place the emitter gave it.
func (e *openAPIEmitter) interceptProp(p jsonschema.InterceptPropParams) error {
	if !p.Processed {
		return nil
	}
	if named, key := e.rawFieldKey(p); named {
		e.rawFieldsSeen[key] = true
		if p.Field.Type != reflect.TypeOf(json.RawMessage{}) {
			return fmt.Errorf("rawFieldSchemas row %s names a %s field: the table is for the "+
				"json.RawMessage fields a handler holds unparsed, and a field with a Go type of "+
				"its own is reflected like any other", key, p.Field.Type)
		}
		fragment, resolved := e.rawFragments[key]
		if !resolved {
			return fmt.Errorf("rawFieldSchemas row %s was read before it was resolved", key)
		}
		if p.PropertySchema == nil {
			return fmt.Errorf("rawFieldSchemas row %s names a field with no schema to write", key)
		}
		return openAPIApplyFragment(p.PropertySchema, fragment)
	}
	// A tag states the values the field takes, and for a slice field those are the values of its
	// elements: an `enum` the reflector put on the array itself would make the document say the
	// field is one of those strings rather than a list of them.
	if err := openAPIEnumOntoItems(p.PropertySchema, p.Field); err != nil {
		return err
	}
	if !openAPIOmitsEmpty(p.Field.Tag.Get("json")) {
		parent := openAPIStructOf(p.ParentSchema.ReflectType)
		// In the /api/v2 document a field an embedded pointer carries is optional however it is
		// tagged: the marshaller writes none of that struct's fields while the pointer is nil
		// (writeAdvice's *documentBlocks). The /api/v1 document keeps the reading it was
		// published with.
		throughPointer := e.version == apiV2 && openAPIThroughPointer(parent, p.Field)
		if !e.requestBodies[parent] && !throughPointer && !slices.Contains(p.ParentSchema.Required, p.Name) {
			p.ParentSchema.Required = append(p.ParentSchema.Required, p.Name)
		}
		openAPIAllowNullRef(p.PropertySchema, p.Field.Type)
		openAPIAllowNullEnum(p.PropertySchema)
		return nil
	}
	if p.PropertySchema != nil {
		p.PropertySchema.RemoveType(jsonschema.Null)
	}
	return nil
}

// openAPIThroughPointer reports whether parent holds field only through an embedded pointer to a
// struct, whose fields the marshaller leaves out of parent's object while that pointer is nil. A
// field parent holds directly, or through embedded values alone, and one it cannot name
// unambiguously, is not.
func openAPIThroughPointer(parent reflect.Type, field reflect.StructField) bool {
	if parent == nil {
		return false
	}
	promoted, found := parent.FieldByName(field.Name)
	if !found || promoted.Type != field.Type || promoted.Tag != field.Tag {
		return false
	}
	holder := parent
	for _, index := range promoted.Index[:len(promoted.Index)-1] {
		embedded := holder.Field(index).Type
		if embedded.Kind() == reflect.Pointer {
			return true
		}
		holder = embedded
	}
	return false
}

// closeObject closes the schema of every struct the /api/v2 document answers: it declares
// additionalProperties false, so the document says each member such an object can hold and that
// it holds no other (R6), and the route tour's contract subtest refuses an answer carrying a
// member its type does not declare. A request body stays open, since the decoder ignores a
// member its type does not declare (R4), and so does a type that writes its own JSON, whose
// members are its MarshalJSON's rather than its fields'.
func (e *openAPIEmitter) closeObject(params jsonschema.InterceptSchemaParams) (bool, error) {
	if !params.Processed || !params.Value.IsValid() || params.Schema == nil || !params.Schema.HasType(jsonschema.Object) {
		return false, nil
	}
	goType := params.Value.Type()
	for goType.Kind() == reflect.Pointer {
		goType = goType.Elem()
	}
	if goType.Kind() != reflect.Struct || e.requestBodies[goType] || openAPIMarshalsItself(goType) {
		return false, nil
	}
	closed := false
	params.Schema.WithAdditionalProperties(jsonschema.SchemaOrBool{TypeBoolean: &closed})
	return false, nil
}

// openAPIMarshalsItself reports whether a type writes its own JSON, by value or through a
// pointer.
func openAPIMarshalsItself(goType reflect.Type) bool {
	marshaler := reflect.TypeFor[json.Marshaler]()
	return goType.Implements(marshaler) || reflect.PointerTo(goType).Implements(marshaler)
}

// openAPIEnumOntoItems moves a slice field's enum from the array to its items. jsonschema-go
// reads an `enum` tag onto the schema of the field it is written on, which for a []string is the
// array, and the document then says the field is one of those strings rather than a list of them
// (ProjectGatedStepsInput.steps was generated as `"decisions" | "criteria" | "spec" | null`
// before this). A field that is no slice keeps its enum where the reflector put it, which is
// where it belongs.
func openAPIEnumOntoItems(schema *jsonschema.Schema, field reflect.StructField) error {
	if schema == nil || len(schema.Enum) == 0 || !openAPIIsList(field.Type) {
		return nil
	}
	if schema.Items == nil || schema.Items.SchemaOrBool == nil || schema.Items.SchemaOrBool.TypeObject == nil {
		return fmt.Errorf("the enum tag of the slice field %s names no items schema to carry it", field.Name)
	}
	schema.Items.SchemaOrBool.TypeObject.Enum = schema.Enum
	schema.Enum = nil
	return nil
}

// openAPIIsList answers whether a type holds a list: a slice or an array, through the pointers
// around it.
func openAPIIsList(t reflect.Type) bool {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array)
}

// openAPIOmitsEmpty is whether a json tag leaves its field out of the object when the value is
// empty: `,omitempty` or `,omitzero`, which omits a zero value of a type omitempty says nothing
// about (a zero time.Time, an empty slice of a struct). Either way a reader does not always get
// the field, so the document marks it optional rather than required.
func openAPIOmitsEmpty(tag string) bool {
	return strings.Contains(tag, ",omitempty") || strings.Contains(tag, ",omitzero")
}

// openAPIAllowNullRef gives a nilable field whose schema is a bare $ref its null arm. Go
// marshals a nil pointer, interface, slice or map as `null`, and the reflector says so by adding
// "null" to a property's type — but a property that refers to a named schema carries a $ref and
// no type at all, so nothing there could say it. A *struct field with no `,omitempty` is exactly
// that case, and left alone the document would promise a reader an Ask always carries an anchor
// when the server sends null, which is the drift this whole document exists to stop. A slice or
// a map of a named type already has its own "null" beside its "array" or "object", so only the
// bare $ref is wrapped.
func openAPIAllowNullRef(schema *jsonschema.Schema, field reflect.Type) {
	if schema == nil || schema.Ref == nil {
		return
	}
	switch field.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map:
	default:
		return
	}
	null := jsonschema.Schema{}
	null.AddType(jsonschema.Null)
	referred := jsonschema.Schema{Ref: schema.Ref}
	schema.Ref = nil
	schema.WithAnyOf(referred.ToSchemaOrBool(), null.ToSchemaOrBool())
}

// openAPIAllowNullEnum puts null among the values of a nilable field's enum. The reflector reads an
// `enum` tag onto the field's schema and adds "null" to a pointer's type, but JSON Schema holds a
// value to both, so left alone the document would refuse the null the server sends for a *string
// with no `,omitempty` and an enum of its values (NearCopyCheck.reason).
func openAPIAllowNullEnum(schema *jsonschema.Schema) {
	if schema == nil || len(schema.Enum) == 0 || !schema.HasType(jsonschema.Null) || slices.Contains(schema.Enum, nil) {
		return
	}
	schema.Enum = append(schema.Enum, nil)
}

// rawFieldKey answers the rawFieldSchemas key of one property, the rawFieldKeyOf of the owning
// struct's reflect.Type and the property's json name, and whether that table names it.
func (e *openAPIEmitter) rawFieldKey(p jsonschema.InterceptPropParams) (bool, rawFieldKeyOf) {
	if p.ParentSchema == nil || p.ParentSchema.ReflectType == nil {
		return false, rawFieldKeyOf{}
	}
	owner := openAPIStructOf(p.ParentSchema.ReflectType)
	if owner == nil {
		return false, rawFieldKeyOf{}
	}
	key := rawFieldKeyOf{Owner: owner, Field: p.Name}
	_, named := rawFieldSchemas[key]
	return named, key
}

// resolveRawFields turns every rawFieldSchemas row into the schema its field carries, before any
// route is walked: a row naming a Go type is reflected here, so the schema it refers to is
// declared and nothing refers to a schema the document does not hold. The /api/v2 document
// resolves only the rows of the objects its routes reach, so it declares no schema a row of the
// /api/v1 types alone names; the /api/v1 document resolves every row, as it always has.
func (e *openAPIEmitter) resolveRawFields(routes []apiRoute) error {
	var reached map[reflect.Type]bool
	if e.version == apiV2 {
		reached = openAPIReachOf(routes)
	}
	keys := openAPISortedRawFieldKeys()
	// The written-out rows first: a row naming a Go type is reflected, and that type's own
	// properties may be rows of this table too.
	for _, pass := range []bool{false, true} {
		for _, key := range keys {
			row := rawFieldSchemas[key]
			if (row.GoType != nil) != pass || (reached != nil && !reached[key.Owner]) {
				continue
			}
			schema := row.Fragment
			if row.GoType != nil {
				reflected, err := e.schema(row.GoType)
				if err != nil {
					return fmt.Errorf("rawFieldSchemas row %s: %w", key, err)
				}
				schema = reflected
			}
			if len(schema) == 0 {
				return fmt.Errorf("rawFieldSchemas row %s names neither a Go type nor a schema", key)
			}
			if row.Nullable {
				schema = openAPINullable(schema)
			}
			e.rawFragments[key] = schema
		}
	}
	return nil
}

// openAPIReachOf is every struct the JSON of routes' requests and answers reaches
// (openAPIReach).
func openAPIReachOf(routes []apiRoute) map[reflect.Type]bool {
	reached := map[reflect.Type]bool{}
	for _, route := range routes {
		shape := route.shape()
		for _, value := range slices.Concat([]any{shape.Request, shape.Response, shape.Advice}, shape.OtherAdvice, openAPIUnionArms(route)) {
			if value != nil {
				openAPIReach(reflect.TypeOf(value), reached, nil)
			}
		}
		for _, answer := range slices.Concat(shape.Also, shape.Refusals) {
			for _, carried := range slices.Concat([]any{answer.Body, answer.Advice}, answer.OtherAdvice) {
				if carried != nil {
					openAPIReach(reflect.TypeOf(carried), reached, nil)
				}
			}
		}
	}
	return reached
}

// openAPISortedRawFieldKeys is rawFieldSchemas' keys in one order, so a failure of the resolver
// reads the same way twice. A reflect.Type has no ordering of its own, so the rows sort by the
// owner's name and then the field's, which is unique because the owner is the type itself.
func openAPISortedRawFieldKeys() []rawFieldKeyOf {
	keys := make([]rawFieldKeyOf, 0, len(rawFieldSchemas))
	for key := range rawFieldSchemas {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b rawFieldKeyOf) int {
		if owner := strings.Compare(a.Owner.String(), b.Owner.String()); owner != 0 {
			return owner
		}
		return strings.Compare(a.Field, b.Field)
	})
	return keys
}

// openAPIApplyFragment writes a schema written as JSON onto the property schema reflection built,
// replacing it whole.
func openAPIApplyFragment(target *jsonschema.Schema, fragment map[string]any) error {
	encoded, err := json.Marshal(fragment)
	if err != nil {
		return fmt.Errorf("encode a json.RawMessage field's schema: %w", err)
	}
	var written jsonschema.Schema
	if err := json.Unmarshal(encoded, &written); err != nil {
		return fmt.Errorf("read a json.RawMessage field's schema: %w", err)
	}
	*target = written
	return nil
}

// schema reflects one Go value into the schema its JSON carries, declaring every named struct it
// holds under its own name and referring to it. A named struct reflects to a reference, a slice of
// one to an array of references, and a value the reflector cannot name to the schema itself. One
// Go type is reflected once: its schema is kept and handed out again (openAPIEmitter.schemas).
func (e *openAPIEmitter) schema(value any) (map[string]any, error) {
	goType := reflect.TypeOf(value)
	if kept, ok := e.schemas[goType]; ok {
		return kept, nil
	}
	reflected, err := e.reflector.Reflect(value,
		jsonschema.RootRef,
		jsonschema.DefinitionsPrefix(openAPISchemaPrefix),
		jsonschema.CollectDefinitions(e.define),
	)
	if err != nil {
		return nil, fmt.Errorf("reflect %T: %w", value, err)
	}
	simple, err := reflected.ToSchemaOrBool().ToSimpleMap()
	if err != nil {
		return nil, fmt.Errorf("render the schema of %T: %w", value, err)
	}
	e.schemas[goType] = simple
	return simple, nil
}

// define declares one schema the reflection named.
func (e *openAPIEmitter) define(name string, schema jsonschema.Schema) {
	simple, err := schema.ToSchemaOrBool().ToSimpleMap()
	if err != nil {
		e.fail("render schema %s: %v", name, err)
		return
	}
	e.reflector.SpecEns().ComponentsEns().WithSchemasItem(name, simple)
}

// operation builds one row's operation: what it is called, what it says of itself, what it takes
// and every answer it gives.
func (e *openAPIEmitter) operation(route apiRoute, hook bool) (*openapi31.Operation, error) {
	name := route.Method + " " + route.Pattern
	shape := route.shape()
	operation := &openapi31.Operation{}
	operation.WithID(openAPIOperationID(route.Method, route.Pattern))
	operation.WithDescription(route.Description)

	extensions := map[string]any{"x-auth": string(route.Auth)}
	if route.Scope != "" {
		extensions["x-write-scope"] = string(route.Scope)
	}
	if len(route.OAuthScopes) > 0 {
		extensions["x-oauth-scopes"] = []string(route.OAuthScopes)
	}
	if hook {
		extensions["x-test-hook"] = true
	}
	if route.v2Bound() {
		maps.Copy(extensions, openAPIV2Members(route))
	}
	operation.WithMapOfAnything(extensions)

	parameters, err := e.parameters(route, shape)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	operation.Parameters = parameters

	switch {
	case len(openAPIUnionArms(route)) > 0:
		body, err := e.unionBody(route)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		operation.RequestBody = &openapi31.RequestBodyOrReference{RequestBody: body}
	case shape.hasRequestBody() || shape.OptionalRequest:
		body, err := e.requestBody(shape, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		operation.RequestBody = &openapi31.RequestBodyOrReference{RequestBody: body}
	}

	responses, err := e.responses(route, shape)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	operation.Responses = responses
	return operation, nil
}

// openAPIV2Members are the members a /api/v2 row's operation carries beside those of every
// operation, each a declaration of the row's that a client reads to call it: the refusal codes it
// can answer, each with its status (x-refusal-codes), and where the row declares them, its log's
// position field, its list's limit bounds, the fields it windows on in time, and the authority
// each value of a request needs (R1 and R6 of dispatch://INFRA-4071). The position and the bounds
// are written as GET /api/v2 writes them (routeIndexV2Entries), so the index and the document
// cannot disagree.
func openAPIV2Members(route apiRoute) map[string]any {
	members := map[string]any{"x-refusal-codes": openAPIOperationCodes(route)}
	entry := routeIndexV2Entries([]apiRoute{route})[0]
	if entry.Position != nil {
		members["x-position"] = *entry.Position
	}
	if entry.List != nil {
		members["x-list"] = *entry.List
	}
	if len(route.TimeFields) > 0 {
		members["x-time-fields"] = slices.Clone(route.TimeFields)
	}
	if len(route.Authorities) > 0 {
		authorities := make([]openAPIAuthority, 0, len(route.Authorities))
		for _, rule := range route.Authorities {
			authorities = append(authorities, openAPIAuthority{
				In: string(rule.Where.In), Name: rule.Where.Name, Value: rule.Value, Scopes: slices.Clone([]string(rule.Scopes)),
			})
		}
		members["x-authorities"] = authorities
	}
	return members
}

// openAPIAuthority is one authority rule of a row (authorityRule) as the document writes it: the
// part of the request and the member or key it reads, the value that needs the scopes (none for a
// rule on a member's presence), and the scopes, every one of which the credential must hold
// (gateV2Authorities).
type openAPIAuthority struct {
	In     string   `json:"in"`
	Name   string   `json:"name"`
	Value  string   `json:"value,omitempty"`
	Scopes []string `json:"scopes"`
}

// callerCodes are the codes the resolution of a request's caller can answer (optionalActor,
// through writeAuthenticationError): a bearer that is no token or a revoked one, a
// service-account token refused or its issuer out of reach, each refusal of a broker-issued token
// (brokerTokenActor), and the identity layer's own (identity.Refusal).
var callerCodes = []apicode.Code{
	apicode.Unauthorized, apicode.TokenRevoked, apicode.OIDCTokenInvalid, apicode.OIDCIssuerUnavailable,
	apicode.BrokerTokenNotActive, apicode.BrokerTokenKeyMismatch, apicode.BrokerTokenProofInvalid,
	apicode.BrokerTokenNotASession, apicode.BrokerTokenRateLimited, apicode.BrokerUnavailable,
	apicode.NoIdentity, apicode.SigninUnavailable, apicode.IdentityError,
}

// openAPIOperationCodes are the refusal codes one /api/v2 route can answer, by name, each with its
// status: the refusals its row declares, and every code a layer the mount runs the route behind
// (routeHandler) can write, under the condition that layer runs in:
//
//   - every route: the query decoder's (QUERY_KEY_UNKNOWN, INVALID_QUERY, and INVALID_LIMIT and
//     INVALID_OFFSET where the query pages), the parameter check's (NUL_CHARACTER, INVALID_UTF8,
//     refuseUnstorableParameters), and the server's own fault (INTERNAL);
//   - a route the read list does not name: the read-only gate's READ_ONLY_TOKEN;
//   - a route that is not public, and one whose scope gate or authority gate resolves the caller:
//     every code that resolution answers (callerCodes);
//   - a route that needs a scope, or a value of whose request needs one: OAUTH_SCOPE_REQUIRED;
//   - a route that resolves an id written short: the prefix layer's ID_AMBIGUOUS and its 404s;
//   - a write: the login check's LOGIN_CHANGED and the sign-in check's SIGN_IN_REQUIRED, the
//     project check's PROJECT_SCOPE or PROJECT_SCOPE_DEPLOYMENT by the row's class, and the
//     drain's 413 for a body past the route's bound, a write that reads no body among them;
//   - a write into one session's rows, by its path or by its body: the session check's
//     SESSION_SCOPE;
//   - a route that reads a JSON body or an upload: the body decoder's;
//   - an upload to an issue: the closed-issue check's ISSUE_CLOSED and ISSUE_NOT_FOUND;
//   - a keyed write: the keyed layer's.
//
// Three answers stand outside the list, since no layer of the mount writes them for a request a
// deployment serves: the router's guards in front of the mount (ORIGIN_REFUSED of the cookie-origin
// guard, HOST_MISMATCH of dev sign-in mode), the slow-write test hook's SLOW_WRITE_CUT_OFF, and
// DATABASE_UNAVAILABLE, which only a server built with no store answers.
func openAPIOperationCodes(route apiRoute) map[string]any {
	codes := []apicode.Code{apicode.QueryKeyUnknown, apicode.InvalidQuery, apicode.NULCharacter, apicode.InvalidUTF8, apicode.InternalError}
	for _, refusal := range route.Refusals {
		codes = append(codes, refusal.Code)
	}
	if !route.ReadScope {
		codes = append(codes, apicode.ReadOnlyToken)
	}
	gated := len(route.OAuthScopes) > 0 || len(route.Authorities) > 0
	if route.Auth != authPublic || gated {
		codes = append(codes, callerCodes...)
	}
	if query, err := compileV2Query(route.Query); err == nil {
		for _, field := range query.fields {
			switch field.kind {
			case queryLimit:
				codes = append(codes, apicode.InvalidLimit)
			case queryOffset:
				codes = append(codes, apicode.InvalidOffset)
			}
		}
	}
	if gated {
		codes = append(codes, apicode.OAuthScopeRequired)
	}
	if ids := route.IDs; ids != nil && (ids.Kind != "" || ids.Blocks) {
		codes = append(codes, apicode.IDAmbiguous)
		for _, notFound := range []apicode.Code{ids.NotFound, ids.BlockNotFound} {
			if !notFound.IsZero() {
				codes = append(codes, notFound)
			}
		}
	}
	tooLarge := route.bodyLimit().TooLarge
	if route.Method != http.MethodGet {
		codes = append(codes, apicode.LoginChanged, apicode.SignInRequired)
		switch route.Scope {
		case scopeProject:
			codes = append(codes, apicode.ProjectScope)
		case scopeDeployment:
			codes = append(codes, apicode.ProjectScopeDeployment)
		}
		if tooLarge != nil {
			// The bound a body is held to, and its refusal, are the route's own (routeRow.bodyLimit).
			codes = append(codes, tooLarge.code)
		}
	}
	if addressesOneSession(route) || addressesSessions(route.Body) {
		codes = append(codes, apicode.SessionScope)
	}
	switch route.Body.Kind {
	case bodyJSON, bodyStateUnion, bodyKindUnion:
		codes = append(codes, apicode.InvalidJSON, apicode.JSONContentType, apicode.RequestBodyUnreadable)
	case bodyUpload:
		codes = append(codes, apicode.InvalidJSON, apicode.JSONContentType, apicode.RequestBodyUnreadable,
			apicode.InvalidBody, apicode.CapExceededTooLarge)
	}
	switch route.Body.Kind {
	case bodyStateUnion:
		codes = append(codes, apicode.StateInvalid)
	case bodyKindUnion:
		codes = append(codes, apicode.InvalidKind)
	}
	if tooLarge != nil && route.Body.Kind != bodyNone {
		codes = append(codes, tooLarge.code)
	}
	if route.RefusesClosedOwner {
		codes = append(codes, apicode.IssueClosed, apicode.IssueNotFound)
	}
	if keyedMethod(route) {
		codes = append(codes, apicode.KeyReused, apicode.KeyInFlight, apicode.KeyInvalid, apicode.KeyCommitted, apicode.AnswerNotKept,
			apicode.RequestBodyUnreadable)
	}
	named := map[string]any{}
	for _, code := range codes {
		named[code.Name()] = openAPICodeStatus(code)
	}
	return named
}

// openAPICodeStatus is the status a code answers with as the document writes it: the status, or
// `*` for the one code whose status is the call's (ANSWER_NOT_KEPT), as v1-codes.txt writes it.
func openAPICodeStatus(code apicode.Code) any {
	if code.StatusFromCall() {
		return "*"
	}
	return code.Status()
}

// writeCodeList writes components.x-refusal-codes, every code /api/v2 can answer by its /api/v2
// name, once, with its status (dispatch://INFRA-4071/spec R7): a code /api/v1 alone answers
// (API_V1_ENDED) has no /api/v2 name and is left out, and two codes sharing a name
// (ISSUE_CYCLE, a loop through parents and one through dependencies) share its status, or the
// emit fails. openapi31.Components has no member for an extension, so the document's components
// are written as a map of their own, the reflected schemas beside the list.
func (e *openAPIEmitter) writeCodeList(spec *openapi31.Spec) {
	codes := map[string]any{}
	for _, code := range apicode.All() {
		if !code.InV2() {
			continue
		}
		status := openAPICodeStatus(code)
		if listed, named := codes[code.Name()]; named && listed != status {
			e.fail("the code %s is answered at %v and at %v: one name has one status", code.Name(), listed, status)
		}
		codes[code.Name()] = status
	}
	spec.WithMapOfAnythingItem("components", map[string]any{
		"schemas":         spec.ComponentsEns().Schemas,
		"x-refusal-codes": codes,
	})
	spec.Components = nil
}

// openAPIOperationID names one operation, from its method and its pattern alone: the method
// lower-cased, then every segment of the path, a literal segment camel-cased with its separators
// dropped and a {brace} segment written `By` and the parameter's name. The handler's own name will
// not do, since getArtifact serves four rows. GET /api/v1 is getApiV1, GET /api/v1/issues/{key} is
// getApiV1IssuesByKey, and POST /api/v1/agents/{session_id}/stream/_test/publish is
// postApiV1AgentsBySessionIdStreamTestPublish.
func openAPIOperationID(method, pattern string) string {
	id := strings.ToLower(method)
	for _, segment := range strings.Split(strings.TrimPrefix(pattern, "/"), "/") {
		if segment == "" {
			continue
		}
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			id += "By" + openAPICamel(segment[1:len(segment)-1])
			continue
		}
		id += openAPICamel(segment)
	}
	return id
}

// openAPICamel turns one path segment into one upper camel-cased word: every run of characters
// that are not letters or digits ends a word.
func openAPICamel(segment string) string {
	var word strings.Builder
	upper := true
	for _, letter := range segment {
		switch {
		case letter >= 'a' && letter <= 'z', letter >= 'A' && letter <= 'Z', letter >= '0' && letter <= '9':
			if upper {
				word.WriteString(strings.ToUpper(string(letter)))
				upper = false
				continue
			}
			word.WriteRune(letter)
		default:
			upper = true
		}
	}
	return word.String()
}

// parameters are the row's path parameters, in the pattern's own order, then the keys the shape's
// query struct declares, in the struct's own order, then the `session` parameter every operation
// carries. A /api/v2 row's keys are its Query type's as the decoder reads them
// (v2QueryParameters).
func (e *openAPIEmitter) parameters(
	route apiRoute,
	shape routeShape,
) ([]openapi31.ParameterOrReference, error) {
	var parameters []openapi31.ParameterOrReference
	for _, name := range openAPIPathParameters(route.Pattern) {
		parameter := openapi31.Parameter{
			Name:   name,
			In:     openapi31.ParameterInPath,
			Schema: map[string]any{"type": "string"},
		}
		parameter.WithRequired(true)
		parameters = append(parameters, openapi31.ParameterOrReference{Parameter: &parameter})
	}
	var query []openapi31.ParameterOrReference
	var err error
	switch {
	case route.v2Bound():
		query, err = v2QueryParameters(route)
	case shape.Params != nil:
		query, err = e.queryParameters(shape.Params)
	}
	if err != nil {
		return nil, err
	}
	parameters = append(parameters, query...)
	session := openapi31.Parameter{
		Name:   "session",
		In:     openapi31.ParameterInQuery,
		Schema: map[string]any{"type": "string"},
	}
	session.WithDescription(openAPISessionParameter)
	return append(parameters, openapi31.ParameterOrReference{Parameter: &session}), nil
}

// openAPIPathParameters are the names a pattern's brace segments hold, in its own order.
func openAPIPathParameters(pattern string) []string {
	var names []string
	for _, segment := range strings.Split(pattern, "/") {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			names = append(names, segment[1:len(segment)-1])
		}
	}
	return names
}

// queryParameters are the keys one query struct declares, each an optional parameter of the type
// its field carries: the handlers read every one of these with `Get` or `Has`, so none is required
// and none can be null, only absent.
func (e *openAPIEmitter) queryParameters(params any) ([]openapi31.ParameterOrReference, error) {
	structure := reflect.TypeOf(params)
	if structure.Kind() != reflect.Struct {
		return nil, fmt.Errorf("query parameters of %T are not a struct", params)
	}
	parameters := make([]openapi31.ParameterOrReference, 0, structure.NumField())
	for index := range structure.NumField() {
		field := structure.Field(index)
		key, tagged := field.Tag.Lookup("query")
		if !tagged {
			return nil, fmt.Errorf("%s.%s carries no query tag", structure.Name(), field.Name)
		}
		reflected, err := e.reflector.Reflect(reflect.New(field.Type).Interface(), jsonschema.InlineRefs)
		if err != nil {
			return nil, fmt.Errorf("reflect %s.%s: %w", structure.Name(), field.Name, err)
		}
		reflected.RemoveType(jsonschema.Null)
		// The reflector is handed the field's type alone, so the field's own tags do not reach
		// it: a parameter the handler takes one spelling of carries that spelling in an `enum`
		// tag, and this is where it is read. A repeatable parameter's values are its elements',
		// so the enum goes on the items, as it does for a body field.
		if values, tagged := field.Tag.Lookup("enum"); tagged {
			for _, value := range strings.Split(values, ",") {
				reflected.Enum = append(reflected.Enum, value)
			}
			if err := openAPIEnumOntoItems(&reflected, field); err != nil {
				return nil, fmt.Errorf("%s.%s: %w", structure.Name(), field.Name, err)
			}
		}
		schema, err := reflected.ToSchemaOrBool().ToSimpleMap()
		if err != nil {
			return nil, fmt.Errorf("render the schema of %s.%s: %w", structure.Name(), field.Name, err)
		}
		parameters = append(parameters, openapi31.ParameterOrReference{Parameter: &openapi31.Parameter{
			Name:   key,
			In:     openapi31.ParameterInQuery,
			Schema: schema,
		}})
	}
	return parameters, nil
}

// v2QueryParameters are the keys a /api/v2 row's Query type declares, as the decoder reads them
// (compileV2Query), in the type's own order, an embedded struct's keys where it embeds them: each
// with the type its values are read as and what holds them - an enum, an integer's bounds, a
// list's limit bounds (the row's List) - and the value a query that leaves the key out is read as,
// so the document says what the decoder accepts and refuses (400 INVALID_QUERY, INVALID_LIMIT,
// INVALID_OFFSET) and what it reads an absent key as. None is required and none can be null. The
// limit of a list whose include names members that render each listed row (`renders`) says in its
// description the page it takes with them (holdIncludedPage), which no schema keyword states beside
// the list's own bounds.
func v2QueryParameters(route apiRoute) ([]openapi31.ParameterOrReference, error) {
	query, err := compileV2Query(route.Query)
	if err != nil {
		return nil, err
	}
	fields := slices.Clone(query.fields)
	slices.SortFunc(fields, func(left, right queryField) int { return slices.Compare(left.index, right.index) })
	parameters := make([]openapi31.ParameterOrReference, 0, len(fields))
	for _, field := range fields {
		parameter := &openapi31.Parameter{
			Name:   field.key,
			In:     openapi31.ParameterInQuery,
			Schema: openAPIQueryFieldSchema(field, route.List),
		}
		if field.comma {
			// The key repeats, and each of its values is a comma list as well (R1.9): no style
			// OpenAPI names reads both, so the document says so beside the form style.
			parameter.WithMapOfAnythingItem("x-comma-list", true)
		}
		if field.kind == queryLimit && query.rendering != nil {
			parameter.WithDescription(includedPageText(route.List, query.rendering.renders) +
				rendersEachRowText(query.rendering.renders) + ".")
		}
		parameters = append(parameters, openapi31.ParameterOrReference{Parameter: parameter})
	}
	return parameters, nil
}

// openAPIQueryFieldSchema is the schema of one key of a /api/v2 query, as the decoder reads it
// (queryField.decode).
func openAPIQueryFieldSchema(field queryField, bounds listBounds) map[string]any {
	switch field.kind {
	case queryLimit:
		least, fallback, most := bounds.limits()
		schema := map[string]any{"type": "integer", "minimum": least}
		if !bounds.Whole {
			// A catalogue read whole takes any limit of 1 or more, and every item where the
			// query names none (decodeLimit).
			schema["maximum"], schema["default"] = most, fallback
		}
		return schema
	case queryOffset:
		return map[string]any{"type": "integer", "minimum": 0, "default": 0}
	case querySortValue:
		values := make([]string, 0, 2*len(field.enum))
		for _, sortable := range field.enum {
			values = append(values, sortable, "-"+sortable)
		}
		order := field.fallback.Interface().(querySort)
		fallback := order.Field
		if order.Descending {
			fallback = "-" + fallback
		}
		return map[string]any{"type": "string", "enum": values, "default": fallback}
	}
	scalar := openAPIQueryScalarSchema(field)
	switch field.kind {
	case queryList:
		return map[string]any{"type": "array", "items": scalar}
	case queryFilterValue:
		// `none` is the unset value of the field the filter reads (R1.6).
		return map[string]any{"anyOf": []any{scalar, map[string]any{"const": queryNone}}}
	case queryOne:
		if field.hasFallback {
			scalar["default"] = field.fallback.Interface()
		}
	}
	return scalar
}

// openAPIQueryScalarSchema is the schema of one value of a query key: an RFC3339 time, a string
// and the values its enum takes, a boolean, or an integer and the bounds its tags name.
func openAPIQueryScalarSchema(field queryField) map[string]any {
	switch {
	case field.scalar == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case field.scalar.Kind() == reflect.String:
		schema := map[string]any{"type": "string"}
		if len(field.enum) > 0 {
			schema["enum"] = slices.Clone(field.enum)
		}
		return schema
	case field.scalar.Kind() == reflect.Bool:
		return map[string]any{"type": "boolean"}
	}
	schema := map[string]any{"type": "integer"}
	if field.min != nil {
		schema["minimum"] = *field.min
	}
	if field.max != nil {
		schema["maximum"] = *field.max
	}
	return schema
}

// hasRequestBody is whether the row carries a request body at all, the one predicate that says
// so: the emitter builds a body for such a row, and routeShape.OptionalRequest, which says a
// body may be left out, means nothing on a row with none.
func (s routeShape) hasRequestBody() bool {
	return s.Request != nil || s.Multipart || s.OpaqueRequest != ""
}

// requestBody is the row's JSON body, and the multipart body beside it where the route takes one.
// It is required where the handler refuses an empty body and optional where it takes one
// (routeShape.OptionalRequest), and a row that marks its request optional and names no body is
// refused here, where the emit is the one reader of the flag. A route whose body Dispatch does
// not model carries OpaqueRequest, and the body is an object of any shape with that reason on
// it: saying nothing is honest, where naming a type that covers one arm of the body would
// promise a client a contract the route does not hold it to.
func (e *openAPIEmitter) requestBody(shape routeShape, name string) (*openapi31.RequestBody, error) {
	if !shape.hasRequestBody() {
		return nil, fmt.Errorf("%s has no body to leave out: OptionalRequest is read only for a "+
			"row with a Request, a Multipart or an OpaqueRequest", name)
	}
	body := &openapi31.RequestBody{Content: map[string]openapi31.MediaType{}}
	if shape.OpaqueRequest != "" {
		if shape.Request != nil {
			return nil, fmt.Errorf("%s names both a request type and OpaqueRequest", name)
		}
		body.WithDescription(shape.OpaqueRequest)
		// `{"type": "object"}` alone is an object that may hold no key at all, which renders as
		// Record<string, never> and refuses every body there is; additionalProperties says what
		// is meant, a JSON object of any shape, as EditOp.attributes already does.
		body.Content["application/json"] = openapi31.MediaType{
			Schema: map[string]any{"type": "object", "additionalProperties": map[string]any{}},
		}
		return body, nil
	}
	if shape.Request != nil {
		schema, err := e.schema(shape.Request)
		if err != nil {
			return nil, err
		}
		body.Content["application/json"] = openapi31.MediaType{Schema: schema}
	}
	if shape.Multipart {
		body.Content["multipart/form-data"] = openapi31.MediaType{Schema: openAPIMultipartSchema}
	}
	if !shape.OptionalRequest {
		body.WithRequired(true)
	}
	return body, nil
}

// unionBody is the body of a /api/v2 route whose body is a union (Body.Arms): a state body, keyed
// by its `state` member, and a mark, keyed by its `kind` (decode_v2.go, armOf). Each distinct arm
// type is one member of the union, holding the values of the key that pick it, so a body that
// names a value matches the one arm it is decoded into, and the discriminator maps each value to
// its arm's schema (R6: each transition body is a union keyed by state). Where the route's path
// holds the key - PATCH /api/v2/me/marks/{kind}/{id} - the body carries no member to discriminate
// by, so the union is the arms alone, and x-discriminator names the path parameter whose value
// picks the arm.
func (e *openAPIEmitter) unionBody(route apiRoute) (*openapi31.RequestBody, error) {
	key := "state"
	if route.Body.Kind == bodyKindUnion {
		key = "kind"
	}
	arms, err := e.unionArms(route.Body.Arms)
	if err != nil {
		return nil, err
	}
	members := make([]any, 0, len(arms.types))
	var union map[string]any
	if patternNames(route.Pattern, key) {
		for _, arm := range arms.types {
			members = append(members, arms.refs[arm])
		}
		union = map[string]any{
			"anyOf":           members,
			"x-discriminator": map[string]any{"in": "path", "name": key, "mapping": arms.mapping},
		}
	} else {
		union = map[string]any{
			"oneOf":         arms.keyedMembers(key),
			"discriminator": map[string]any{"propertyName": key, "mapping": arms.mapping},
		}
	}
	body := &openapi31.RequestBody{Content: map[string]openapi31.MediaType{"application/json": {Schema: union}}}
	if !route.Body.Optional {
		body.WithRequired(true)
	}
	return body, nil
}

// openAPIArms are a union's arms as the document writes them: each distinct arm type once, in
// the order of the first value that picks it, with its schema (a reference) and the values that
// pick it, and the mapping of each value to its arm's schema.
type openAPIArms struct {
	types   []reflect.Type
	picks   map[reflect.Type][]string
	refs    map[reflect.Type]map[string]any
	mapping map[string]any
}

// unionArms reflects the arms of a union by the values that pick them, in the values' order. An
// arm is a named struct, since each is a schema of its own that the union refers to.
func (e *openAPIEmitter) unionArms(byValue map[string]any) (openAPIArms, error) {
	arms := openAPIArms{
		picks: map[reflect.Type][]string{}, refs: map[reflect.Type]map[string]any{}, mapping: map[string]any{},
	}
	for _, value := range slices.Sorted(maps.Keys(byValue)) {
		arm := byValue[value]
		goType := reflect.TypeOf(arm)
		schema, err := e.schema(arm)
		if err != nil {
			return openAPIArms{}, err
		}
		reference, named := schema["$ref"].(string)
		if !named {
			return openAPIArms{}, fmt.Errorf("the %s arm %T is no named struct, and a union's arm is a schema of its own", value, arm)
		}
		arms.mapping[value] = reference
		if _, seen := arms.picks[goType]; !seen {
			arms.types = append(arms.types, goType)
			arms.refs[goType] = schema
		}
		arms.picks[goType] = append(arms.picks[goType], value)
	}
	return arms, nil
}

// keyedMembers are the members of a union whose JSON carries its key: each distinct arm's schema
// with key required beside it, holding the values that pick that arm, so a value matches the one
// arm it is decoded into.
func (arms openAPIArms) keyedMembers(key string) []any {
	members := make([]any, 0, len(arms.types))
	for _, arm := range arms.types {
		members = append(members, map[string]any{"allOf": []any{
			arms.refs[arm],
			map[string]any{
				"type":       "object",
				"required":   []any{key},
				"properties": map[string]any{key: map[string]any{"enum": arms.picks[arm]}},
			},
		}})
	}
	return members
}

// writeItemUnions adds to the schema of each type itemUnions names, where this document declares
// it, the union its JSON is beside the type's own members: oneOf its arms, each keyed by the
// values of its key that pick it (keyedMembers), so an item matches the one arm its key names. It
// names its key in x-discriminator, not in discriminator: openapi-typescript writes a
// discriminator's value into each schema the mapping names, and an arm is a body of its own
// elsewhere that carries no key (PATCH /api/v2/me/marks/{kind}/{id} takes a mark's arm with its
// kind in the path).
func (e *openAPIEmitter) writeItemUnions() error {
	schemas := e.reflector.SpecEns().ComponentsEns().Schemas
	for _, goType := range itemUnionTypes() {
		name := openAPISchemaName(goType)
		own, declared := schemas[name]
		if !declared || e.schemaOwners[name] != goType {
			continue
		}
		union := itemUnions[goType]
		if properties, _ := own["properties"].(map[string]any); properties[union.Key] == nil {
			return fmt.Errorf("itemUnions keys %s by %s, which is no member of it", openAPIGoTypeName(goType), union.Key)
		}
		arms, err := e.unionArms(union.Arms)
		if err != nil {
			return fmt.Errorf("%s: %w", openAPIGoTypeName(goType), err)
		}
		own["oneOf"] = arms.keyedMembers(union.Key)
		own["x-discriminator"] = map[string]any{"propertyName": union.Key, "mapping": arms.mapping}
	}
	return nil
}

// itemUnionTypes are the types itemUnions names, by their schema names, so an emit that fails
// names the same type whichever run reads the table.
func itemUnionTypes() []reflect.Type {
	types := slices.Collect(maps.Keys(itemUnions))
	slices.SortFunc(types, func(left, right reflect.Type) int {
		return strings.Compare(openAPISchemaName(left), openAPISchemaName(right))
	})
	return types
}

// openAPIAnswer is one status of one operation while it is being built: every schema the route
// answers at that status, and the content types its bytes are served as. anyOf marks a status
// whose shapes may overlap, a refusal's richer body beside errorBody, which the status answers as
// one of them or more rather than as exactly one.
type openAPIAnswer struct {
	schemas     []map[string]any
	description string
	opaque      []string
	anyOf       bool
}

// responses are every answer the row gives: its success with the shapes that ride it, each of the
// row's other answers, each refusal richer than errorBody, and the default answer every other
// refusal writes. A /api/v2 row's operation also answers the decoder's refusal of an undeclared
// query key at 400, and each of its refusal statuses answers errorBody beside the richer bodies it
// declares, since other refusals share that status: its schemas are closed (closeObject), so a
// status that named the richer body alone would refuse every errorBody answered at it.
func (e *openAPIEmitter) responses(route apiRoute, shape routeShape) (*openapi31.Responses, error) {
	answers := map[int]*openAPIAnswer{}
	var order []int
	answer := func(status int) *openAPIAnswer {
		if answers[status] == nil {
			answers[status] = &openAPIAnswer{}
			order = append(order, status)
		}
		return answers[status]
	}
	add := func(status int, body any) error {
		at := answer(status)
		if body == nil {
			return nil
		}
		schema, err := e.schema(body)
		if err != nil {
			return err
		}
		return at.hold(schema)
	}

	success := answer(shape.Status)
	if shape.Response != nil {
		schema, err := e.advisedAnswer(shape.Response, shape.Advice, shape.OtherAdvice)
		if err != nil {
			return nil, err
		}
		if shape.Nullable {
			schema = openAPINullable(schema)
		}
		if err := success.hold(schema); err != nil {
			return nil, err
		}
	}
	if shape.Opaque != "" {
		if len(shape.OpaqueContentTypes) == 0 {
			return nil, fmt.Errorf("%s %s answers bytes Dispatch does not model and names no "+
				"content type for them (routeShape.OpaqueContentTypes)", route.Method, route.Pattern)
		}
		success.description = shape.Opaque
		success.opaque = append(success.opaque, shape.OpaqueContentTypes...)
	}
	for _, also := range shape.Also {
		if also.Advice == nil {
			if err := add(also.Status, also.Body); err != nil {
				return nil, err
			}
			continue
		}
		schema, err := e.advisedAnswer(also.Body, also.Advice, also.OtherAdvice)
		if err != nil {
			return nil, err
		}
		if err := answer(also.Status).hold(schema); err != nil {
			return nil, err
		}
	}
	for _, refusal := range shape.Refusals {
		if err := add(refusal.Status, refusal.Body); err != nil {
			return nil, err
		}
	}
	if route.v2Bound() {
		if err := add(http.StatusBadRequest, queryKeyUnknownBody{}); err != nil {
			return nil, err
		}
	}
	refusal, err := e.schema(reflect.Zero(reflect.TypeFor[errorBody]()).Interface())
	if err != nil {
		return nil, err
	}

	responses := &openapi31.Responses{}
	sort.Ints(order)
	for _, status := range order {
		at := answers[status]
		if route.v2Bound() && status >= http.StatusBadRequest {
			if err := at.hold(refusal); err != nil {
				return nil, err
			}
			at.anyOf = true
		}
		response, err := at.response(status)
		if err != nil {
			return nil, err
		}
		responses.WithMapOfResponseOrReferenceValuesItem(
			fmt.Sprintf("%d", status), openapi31.ResponseOrReference{Response: response})
	}
	responses.WithDefault(openapi31.ResponseOrReference{Response: (&openapi31.Response{
		Content: map[string]openapi31.MediaType{"application/json": {Schema: refusal}},
	}).WithDescription("A refusal: its code and its message, which every refusal this route does " +
		"not give a richer body writes (writeError).")})
	return responses, nil
}

// advisedAnswer is the schema of an answer that carries body, and where it carries advice beside
// it (routeShape.Advice, routeAnswer.Advice), that advice: one of its shapes, the first and each
// of the others. /api/v1 writes it as openAPIAdvised's allOf, /api/v2 as a schema of its own
// (advisedSchema).
func (e *openAPIEmitter) advisedAnswer(body, advice any, otherAdvice []any) (map[string]any, error) {
	schema, err := e.schema(body)
	if err != nil || advice == nil {
		return schema, err
	}
	advices := make([]map[string]any, 0, 1+len(otherAdvice))
	for _, carried := range slices.Concat([]any{advice}, otherAdvice) {
		reflected, err := e.schema(carried)
		if err != nil {
			return nil, err
		}
		advices = append(advices, reflected)
	}
	carried := advices[0]
	if len(advices) > 1 {
		arms := make([]any, 0, len(advices))
		for _, arm := range advices {
			arms = append(arms, arm)
		}
		carried = map[string]any{"anyOf": arms}
	}
	if e.version == apiV2 {
		return e.advisedSchema(body, schema, advices, carried)
	}
	return openAPIAdvised(schema, carried), nil
}

// hold adds one schema to a status, leaving a repeat of a schema the status already answers out:
// two rows of Also can name one shape, and a status answers each shape once.
func (a *openAPIAnswer) hold(schema map[string]any) error {
	held, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("compare the schemas of one status: %w", err)
	}
	for _, existing := range a.schemas {
		encoded, err := json.Marshal(existing)
		if err != nil {
			return fmt.Errorf("compare the schemas of one status: %w", err)
		}
		if string(encoded) == string(held) {
			return nil
		}
	}
	a.schemas = append(a.schemas, schema)
	return nil
}

// response renders one status: the shape it answers, or one of the shapes it answers, beside every
// content type its bytes are served as.
func (a *openAPIAnswer) response(status int) (*openapi31.Response, error) {
	response := &openapi31.Response{Description: http.StatusText(status)}
	if a.description != "" {
		response.Description = a.description
	}
	content := map[string]openapi31.MediaType{}
	switch {
	case len(a.schemas) == 1:
		content["application/json"] = openapi31.MediaType{Schema: a.schemas[0]}
	case len(a.schemas) > 1:
		shapes := make([]any, 0, len(a.schemas))
		for _, schema := range a.schemas {
			shapes = append(shapes, schema)
		}
		combinator := "oneOf"
		if a.anyOf {
			combinator = "anyOf"
		}
		content["application/json"] = openapi31.MediaType{Schema: map[string]any{combinator: shapes}}
	}
	for _, contentType := range a.opaque {
		if _, modelled := content[contentType]; modelled {
			return nil, fmt.Errorf("%d answers %s both as a schema and as bytes", status, contentType)
		}
		content[contentType] = openapi31.MediaType{}
	}
	if len(content) > 0 {
		response.Content = content
	}
	return response, nil
}

// openAPIAdvised is the success shape of a route that carries write advice: the payload with an
// `advice` sibling, since advisedResponse writes the payload's own fields and `advice` beside them
// rather than nesting the payload (write_advice.go). An allOf of the two renders as a TypeScript
// intersection.
func openAPIAdvised(payload, advice map[string]any) map[string]any {
	return map[string]any{"allOf": []any{
		payload,
		map[string]any{
			"type":       "object",
			"properties": map[string]any{"advice": advice},
		},
	}}
}

// advisedSchema is, in the /api/v2 document, the success shape of a route that carries write
// advice: a schema of its own, <Payload>With<Advice>, and Or<Advice> for each other advice shape
// the route answers (routeShape.OtherAdvice), declaring the payload's members and `advice` beside
// them, adviceSchema. openAPIAdvised's allOf cannot serve there: the payload's schema is closed
// (closeObject), so it would refuse the `advice` member its sibling declares.
func (e *openAPIEmitter) advisedSchema(payload any, payloadSchema map[string]any, advices []map[string]any, adviceSchema map[string]any) (map[string]any, error) {
	payloadReference, _ := payloadSchema["$ref"].(string)
	payloadName, named := strings.CutPrefix(payloadReference, openAPISchemaPrefix)
	if !named {
		return nil, fmt.Errorf("%T is advised and no named struct: an advised answer's payload is a schema of its own", payload)
	}
	name := payloadName
	for index, advice := range advices {
		adviceReference, _ := advice["$ref"].(string)
		adviceName, named := strings.CutPrefix(adviceReference, openAPISchemaPrefix)
		if !named {
			return nil, fmt.Errorf("%T's advice %v is no named struct: each advice shape is a schema of its own", payload, advice)
		}
		name += map[bool]string{true: "With", false: "Or"}[index == 0] + adviceName
	}
	if owner, taken := e.schemaOwners[name]; taken {
		return nil, fmt.Errorf("the advised answer %s is a name %s already takes", name, openAPIGoTypeName(owner))
	}
	components := e.reflector.SpecEns().ComponentsEns()
	if _, declared := components.Schemas[name]; !declared {
		encoded, err := json.Marshal(components.Schemas[payloadName])
		if err != nil {
			return nil, fmt.Errorf("copy the schema %s: %w", payloadName, err)
		}
		var advised map[string]any
		if err := json.Unmarshal(encoded, &advised); err != nil {
			return nil, fmt.Errorf("copy the schema %s: %w", payloadName, err)
		}
		properties, isObject := advised["properties"].(map[string]any)
		if _, clash := properties["advice"]; !isObject || clash {
			return nil, fmt.Errorf("%s is no object with room for an advice member beside its own", payloadName)
		}
		properties["advice"] = adviceSchema
		components.WithSchemasItem(name, advised)
	}
	return map[string]any{"$ref": openAPISchemaPrefix + name}, nil
}

// openAPINullable is a shape a route answers, or a field holds, as the shape or the JSON literal
// null.
func openAPINullable(schema map[string]any) map[string]any {
	return map[string]any{"anyOf": []any{schema, map[string]any{"type": "null"}}}
}
