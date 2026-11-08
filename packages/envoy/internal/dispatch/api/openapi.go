// openapi.go emits the OpenAPI 3.1 document of the Dispatch HTTP API from the route table and the
// Go types routeShapes gives each of its rows (INFRA-1406). It is the one generator between the
// server and its clients: the dashboard's and the CLI's TypeScript types are rendered from the
// document this file writes, so a route the table grows reaches them without anyone hand-writing a
// type for it, and a shape that changes cannot quietly disagree with the handler that answers it.
//
// Nothing here reads the database or opens a listener, as WriteRouteIndex needs neither: the table
// with its test hooks in, routeShapes beside it and reflection over the Go types are the whole
// input. `envoy-dispatch openapi` prints it and `--check` holds the committed artifact to it.
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go/openapi31"
)

// OpenAPIDocumentPath is where the emitted document is committed, from the repository root. The
// subcommand's --check and the test that holds the artifact to the emitter each resolve it against
// their own directory, so the path is written once.
const OpenAPIDocumentPath = "packages/contracts/schemas/dispatch-openapi.json"

// openAPISchemaPrefix is where a schema is referenced from, which is also what decides the name
// its TypeScript export carries.
const openAPISchemaPrefix = "#/components/schemas/"

// openAPISessionParameter is the one query parameter every operation carries. It is read by
// actingPerson (server.go) on every route that resolves a person rather than by any one handler,
// so it belongs to the whole API and routeShapes names it on no row.
const openAPISessionParameter = "the session the caller's credential acts for, where that " +
	"credential proved none; a credential that proved a session is refused a parameter naming " +
	"another (403 ACTOR_SESSION_MISMATCH)"

// WriteOpenAPI writes the OpenAPI 3.1 document of every route the table mounts with its test hooks
// in, two spaces to the level and a newline at its end. Two runs on one tree write identical
// bytes: every map it walks is sorted and every list it writes is the table's own order.
func WriteOpenAPI(w io.Writer) error {
	document, err := openAPIDocument()
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	// A description quoting `<owner>/<slug>` or an `&` is written as it stands: this document is
	// read by people as well as by generators.
	encoder.SetEscapeHTML(false)
	return encoder.Encode(document)
}

// openAPIEmitter reflects the Go types of the shapes and collects what it learns: the schemas the
// document declares, the name each Go type took, and every refusal it owes the caller. Failures
// are gathered rather than returned one at a time, so one run names every name collision and every
// stale rawFieldSchemas row instead of stopping at the first.
type openAPIEmitter struct {
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

// newOpenAPIEmitter builds the emitter the document is reflected with, and answers beside it
// every Go type the table reflects as a request body and as a response both, which no one schema
// can carry (openAPIRequestBodyTypes).
func newOpenAPIEmitter() (*openAPIEmitter, []reflect.Type) {
	bodies, bothDirections := openAPIRequestBodyTypes()
	emitter := &openAPIEmitter{
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
	return emitter, bothDirections
}

// openAPIDocument builds the document. It walks the table with its test hooks in, since a hook's
// row is a row of the table wherever a deployment leaves it out (scopedRoutesNotMounted), and
// marks each hook row x-test-hook so a reader of the document can tell the twelve apart.
func openAPIDocument() (*openapi31.Spec, error) {
	emitter, bothDirections := newOpenAPIEmitter()
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
	spec.Info.WithVersion("1")
	spec.Info.WithDescription(
		"Generated from the Go route table (internal/dispatch/api/routes_table.go) and the Go " +
			"types beside it (routes_shapes.go) by `envoy-dispatch openapi`. Edit those, never " +
			"this document: it is committed so a client's types can be rendered from it, and " +
			"`envoy-dispatch openapi --check` refuses a tree where it is stale.")

	if err := emitter.resolveRawFields(); err != nil {
		return nil, err
	}

	hooks := openAPIHookRoutes()
	paths := map[string]*openapi31.PathItem{}
	operationIDs := map[string]string{}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		name := route.Method + " " + route.Pattern
		operation, err := emitter.operation(route, hooks[name])
		if err != nil {
			return nil, err
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
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	for pattern, item := range paths {
		spec.PathsEns().WithMapOfPathItemValuesItem(pattern, *item)
	}

	for key := range rawFieldSchemas {
		if !emitter.rawFieldsSeen[key] {
			emitter.fail("rawFieldSchemas names %s, which is no field of any type the document "+
				"describes: drop the row or name the field it meant", key)
		}
	}
	for goType, name := range schemaNames {
		if emitter.schemaOwners[name] != goType {
			emitter.fail("schemaNames gives %s the name %s, and the document declares no schema "+
				"of that type: drop the row or name the type it meant", openAPIGoTypeName(goType), name)
		}
	}
	if len(emitter.failures) > 0 {
		sort.Strings(emitter.failures)
		return nil, fmt.Errorf("emit the OpenAPI document:\n\t%s",
			strings.Join(slices.Compact(emitter.failures), "\n\t"))
	}
	return spec, nil
}

// openAPIHookRoutes names every row the table mounts only with Deps.TestHooksEnabled, read as the
// difference between the two tables rather than listed, so a hook added to routes() is marked
// without this file being touched.
func openAPIHookRoutes() map[string]bool {
	served := map[string]bool{}
	for _, route := range (&server{}).routes() {
		served[route.Method+" "+route.Pattern] = true
	}
	hooks := map[string]bool{}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
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
// carries.
func openAPISchemaName(t reflect.Type) string {
	if name, overridden := schemaNames[t]; overridden {
		return name
	}
	name := t.Name()
	if name == "" {
		return ""
	}
	return strings.ToUpper(name[:1]) + name[1:]
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
// The reach is every Shape.Request, every type a rawFieldSchemas row names inside one, and
// everything those reach through the JSON they carry. A type an answer reaches too is left out
// and keeps the response rule: one schema serves both directions there, and the response's
// reading is the one its TypeScript consumers already have (model.Actor, read on a write and
// answered on every read, is required to carry kind and id in both). A request body that is
// itself an answer cannot be settled that way, since neither rule can be right for both of its
// directions, and is named here for the emit to refuse.
func openAPIRequestBodyTypes() (map[reflect.Type]bool, []reflect.Type) {
	request, response := map[reflect.Type]bool{}, map[reflect.Type]bool{}
	var bodies []reflect.Type
	reach := func(value any, into map[reflect.Type]bool, carried *[]reflect.Type) {
		if value != nil {
			openAPIReach(reflect.TypeOf(value), into, carried)
		}
	}
	// In the table's key order, so a refusal names the same type whichever run reads it.
	for _, name := range slices.Sorted(maps.Keys(routeShapes)) {
		shape := routeShapes[name]
		if shape.Request != nil {
			bodies = append(bodies, reflect.TypeOf(shape.Request))
			reach(shape.Request, request, &bodies)
		}
		reach(shape.Response, response, nil)
		reach(shape.Advice, response, nil)
		for _, answer := range slices.Concat(shape.Also, shape.Refusals) {
			reach(answer.Body, response, nil)
		}
	}
	// The default answer every refusal richer than it rides on (responses).
	openAPIReach(reflect.TypeOf(errorBody{}), response, nil)

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

// openAPIReach gathers into every struct type the JSON of one Go type reaches: the type itself,
// what its fields carry through the pointers, slices, arrays and maps between, and what a
// json.RawMessage field's bytes hold, which is rawFieldSchemas' to say rather than the field's
// own Go type. carried, where it is given, collects each type that table names, since the
// document reflects those bytes as a body of their own.
func openAPIReach(t reflect.Type, into map[reflect.Type]bool, carried *[]reflect.Type) {
	t = openAPIStructOf(t)
	if t == nil || into[t] {
		return
	}
	into[t] = true
	openAPIReachFields(t, t, into, carried)
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
		if !e.requestBodies[openAPIStructOf(p.ParentSchema.ReflectType)] {
			p.ParentSchema.Required = append(p.ParentSchema.Required, p.Name)
		}
		openAPIAllowNullRef(p.PropertySchema, p.Field.Type)
		return nil
	}
	if p.PropertySchema != nil {
		p.PropertySchema.RemoveType(jsonschema.Null)
	}
	return nil
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
// declared and nothing refers to a schema the document does not hold.
func (e *openAPIEmitter) resolveRawFields() error {
	keys := openAPISortedRawFieldKeys()
	// The written-out rows first: a row naming a Go type is reflected, and that type's own
	// properties may be rows of this table too.
	for _, pass := range []bool{false, true} {
		for _, key := range keys {
			row := rawFieldSchemas[key]
			if (row.GoType != nil) != pass {
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
	shape := routeShapes[name]
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
	operation.WithMapOfAnything(extensions)

	parameters, err := e.parameters(route, shape)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	operation.Parameters = parameters

	if shape.hasRequestBody() || shape.OptionalRequest {
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
// carries.
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
	if shape.Params != nil {
		query, err := e.queryParameters(shape.Params)
		if err != nil {
			return nil, err
		}
		parameters = append(parameters, query...)
	}
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

// openAPIAnswer is one status of one operation while it is being built: every schema the route
// answers at that status, and the content types its bytes are served as.
type openAPIAnswer struct {
	schemas     []map[string]any
	description string
	opaque      []string
}

// responses are every answer the row gives: its success with the shapes that ride it, each of the
// row's other answers, each refusal richer than errorBody, and the default answer every other
// refusal writes.
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
		schema, err := e.schema(shape.Response)
		if err != nil {
			return nil, err
		}
		if shape.Advice != nil {
			advice, err := e.schema(shape.Advice)
			if err != nil {
				return nil, err
			}
			schema = openAPIAdvised(schema, advice)
		}
		if shape.Nullable {
			schema = openAPINullable(schema)
		}
		if err := success.hold(schema); err != nil {
			return nil, err
		}
	}
	if shape.Opaque != "" {
		if shape.OpaqueContentType == "" {
			return nil, fmt.Errorf("%s %s answers bytes Dispatch does not model and names no "+
				"content type for them (routeShape.OpaqueContentType)", route.Method, route.Pattern)
		}
		success.description = shape.Opaque
		success.opaque = append(success.opaque, shape.OpaqueContentType)
	}
	for _, also := range shape.Also {
		if err := add(also.Status, also.Body); err != nil {
			return nil, err
		}
	}
	for _, refusal := range shape.Refusals {
		if err := add(refusal.Status, refusal.Body); err != nil {
			return nil, err
		}
	}

	responses := &openapi31.Responses{}
	sort.Ints(order)
	for _, status := range order {
		response, err := answers[status].response(status)
		if err != nil {
			return nil, err
		}
		responses.WithMapOfResponseOrReferenceValuesItem(
			fmt.Sprintf("%d", status), openapi31.ResponseOrReference{Response: response})
	}
	refusal, err := e.schema(errorBody{})
	if err != nil {
		return nil, err
	}
	responses.WithDefault(openapi31.ResponseOrReference{Response: (&openapi31.Response{
		Content: map[string]openapi31.MediaType{"application/json": {Schema: refusal}},
	}).WithDescription("A refusal: its code and its message, which every refusal this route does " +
		"not give a richer body writes (writeError).")})
	return responses, nil
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
		content["application/json"] = openapi31.MediaType{Schema: map[string]any{"oneOf": shapes}}
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

// openAPINullable is a shape a route answers, or a field holds, as the shape or the JSON literal
// null.
func openAPINullable(schema map[string]any) map[string]any {
	return map[string]any{"anyOf": []any{schema, map[string]any{"type": "null"}}}
}
