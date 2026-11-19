//go:build basebuild

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/testbaseline"
)

// TestNoRouteOrFieldTheForkPointServedIsGone is the half of c-no-regression that needs the fork
// point's build: every route the table held at the fork point, and every field of every shape
// it declared, read against what this build's table answers.
//
// The fork point is the commit BASELINE_SHA names (testbaseline.Commit): `git merge-base
// origin/main HEAD` for a branch that lands on main, the parent's head for a child stacked on it.
// What it served is the OpenAPI document it committed (OpenAPIDocumentPath), which is the render
// of its route table: a document that differs from its render fails declaredShapesUnchanged at
// the commit that wrote it. This build's side is the render of its own table, taken now, so
// nothing committed beside this test can stand in for what the table answers. A route of the
// fork point that this table no longer serves fails here, and so does a schema, a field or a
// query parameter it no longer declares, a field that is no longer required, and any of them
// whose shape moved. A route, a schema or a field added since the fork point is no regression and
// is not read.
//
// Two moves are no regression. One is a field that now names the values it takes, as an `enum`
// over the type it had, where the fork point named none, in a schema this build answers and never
// takes - reachable from no request body and no parameter of its document (requestSchemas). Every
// value such a field is sent is a value of the type a reader of the fork point's document reads,
// so nothing it read is gone; it is logged, naming the schema, the field and the values
// (compareSchemas). The same enum on a schema a request takes refuses values the fork point
// took, and fails as any other move does. The other is an answer the fork point declared as JSON
// of no shape (an Opaque row, opaqueJSON) that this build's table now names a shape for: a reader
// of the fork point's document read it as whatever JSON it was sent, so it reads every answer of
// the shape now named, and it is logged, naming the operation and the shape (compareAnswer). A
// request body the fork point took as JSON of no shape is held as before, since a shape named on
// it refuses bodies the fork point took.
//
// It runs under the build tag `basebuild` because it reads the repository's history: a checkout
// that does not hold the named commit (a shallow clone, an archive) has nothing to read.
func TestNoRouteOrFieldTheForkPointServedIsGone(t *testing.T) {
	root := gitOutput(t, ".", "rev-parse", "--show-toplevel")
	commit := testbaseline.Commit(t)
	base := decodeServedDocument(t, "the fork point's document", []byte(gitOutput(t, root, "show", commit+":"+OpenAPIDocumentPath)))
	var rendered bytes.Buffer
	if err := WriteOpenAPI(&rendered); err != nil {
		t.Fatalf("render this build's route table: %v", err)
	}
	now := decodeServedDocument(t, "this build's render", rendered.Bytes())
	t.Logf("the fork point is %s: %d paths and %d schemas, against %d paths and %d schemas now",
		commit, len(base.paths), len(base.schemas), len(now.paths), len(now.schemas))
	if len(base.paths) < 100 || len(base.schemas) < 100 {
		t.Fatalf("the fork point's document holds %d paths and %d schemas, which is too few to be the document",
			len(base.paths), len(base.schemas))
	}

	table := map[string]bool{}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		table[strings.ToLower(route.Method)+" "+route.Pattern] = true
	}
	for path, operations := range base.paths {
		for method, served := range operations {
			name := strings.ToUpper(method) + " " + path
			if !table[method+" "+path] {
				t.Errorf("%s was a route of the fork point's table and is no route of this one", name)
			}
			answered, held := now.paths[path][method]
			if !held {
				t.Errorf("%s was served at the fork point and this build's document declares no such operation", name)
				continue
			}
			for parameter, shape := range served.parameters {
				switch current, declared := answered.parameters[parameter]; {
				case !declared:
					t.Errorf("%s took the parameter %s at the fork point and takes it no longer", name, parameter)
				case current != shape:
					t.Errorf("%s's parameter %s was %s at the fork point and is %s now", name, parameter, shape, current)
				}
			}
			if served.body != answered.body {
				t.Errorf("%s took the body %s at the fork point and takes %s now", name, served.body, answered.body)
			}
			for status, shape := range served.responses {
				current, declared := answered.responses[status]
				if !declared {
					t.Errorf("%s answered %s at the fork point and declares no such answer now", name, status)
					continue
				}
				switch failure, narrowing := compareAnswer(name, status, shape, current); {
				case failure != "":
					t.Error(failure)
				case narrowing != "":
					t.Log(narrowing)
				}
			}
		}
	}
	failures, narrowings := compareSchemas(t, base, now, requestSchemas(t, rendered.Bytes()))
	for _, narrowing := range narrowings {
		t.Log(narrowing)
	}
	for _, failure := range failures {
		t.Error(failure)
	}
}

// opaqueJSON is the shape of an answer the document declares as JSON and names no schema for: an
// Opaque row's (routeShape.Opaque), whose bytes the handler writes as they stand. It is not
// "none", the shape of an answer that carries no JSON at all, since an answer that moves from
// another content type to JSON is a move a reader of the fork point's document did not read.
const opaqueJSON = "JSON of no declared shape"

// compareAnswer holds one answer of the operation name, by its status, at the fork point
// (served) to this build's (current): a failure for a shape that moved, and a narrowing, logged,
// for the one move that takes nothing from a reader - an answer the fork point declared as
// opaqueJSON, which this build's table now names a shape for. A reader of the fork point's
// document read that answer as whatever JSON it was sent, and every answer of the shape named now
// is JSON.
func compareAnswer(name, status, served, current string) (failure, narrowing string) {
	switch {
	case current == served:
		return "", ""
	case served == opaqueJSON && current != "none":
		return "", fmt.Sprintf("%s's %s answer was JSON of no declared shape at the fork point and is %s now; a reader of the fork point read it as any JSON, so it reads every answer of that shape",
			name, status, current)
	}
	return fmt.Sprintf("%s's %s answer was %s at the fork point and is %s now", name, status, served, current), ""
}

// TestAnAnswerTheForkPointLeftOpaqueNarrowsAndNoOtherAnswerMoves holds compareAnswer to its one
// allowance, over this build's own render moved by hand: a fork point whose GET
// /api/v1/schema/blocks answer was JSON of no declared shape, as its Opaque row rendered it, reads
// the shape that answer names now as a narrowing, and a fork point whose GET /api/v1/limits
// answer named a shape fails against a build that declares it opaque, against one that declares
// no JSON for it, and against one whose shape moved. It reads no history, so it runs wherever the
// tag is given.
func TestAnAnswerTheForkPointLeftOpaqueNarrowsAndNoOtherAnswerMoves(t *testing.T) {
	rendered := renderOpenAPI(t)
	answerOf := func(document map[string]any, path string) map[string]any {
		operation := document["paths"].(map[string]any)[path].(map[string]any)["get"].(map[string]any)
		return operation["responses"].(map[string]any)["200"].(map[string]any)
	}
	moved := func(what string, move func(document map[string]any)) servedDocument {
		var document map[string]any
		if err := json.Unmarshal(rendered, &document); err != nil {
			t.Fatalf("decode the render: %v", err)
		}
		move(document)
		raw, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("encode %s: %v", what, err)
		}
		return decodeServedDocument(t, what, raw)
	}
	opaque := func(path string) func(map[string]any) {
		return func(document map[string]any) {
			answerOf(document, path)["content"] = map[string]any{"application/json": map[string]any{}}
		}
	}
	now := decodeServedDocument(t, "this build's render", rendered)
	compare := func(base, current servedDocument, path string) (string, string) {
		return compareAnswer("GET "+path, "200", base.paths[path]["get"].responses["200"], current.paths[path]["get"].responses["200"])
	}

	const typed = "/api/v1/schema/blocks"
	if shape := now.paths[typed]["get"].responses["200"]; shape == opaqueJSON || shape == "none" {
		t.Fatalf("GET %s answers %s in this build's render, so there is no shape to narrow to", typed, shape)
	}
	failure, narrowing := compare(moved("the fork point", opaque(typed)), now, typed)
	if failure != "" || !strings.HasPrefix(narrowing, "GET "+typed+"'s 200 answer was JSON of no declared shape") {
		t.Errorf("an answer opaque at the fork point and named now fails %q and narrows %q; want a narrowing alone", failure, narrowing)
	}

	const limits = "/api/v1/limits"
	for what, current := range map[string]servedDocument{
		"declares it opaque": moved("this build, opaque", opaque(limits)),
		"declares no JSON":   moved("this build, no JSON", func(document map[string]any) { delete(answerOf(document, limits), "content") }),
		"moves its shape": moved("this build, moved", func(document map[string]any) {
			answerOf(document, limits)["content"] = map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "string"}}}
		}),
	} {
		if failure, narrowing := compare(now, current, limits); failure == "" || narrowing != "" {
			t.Errorf("an answer named at the fork point, against a build that %s, fails %q and narrows %q; want a failure alone", what, failure, narrowing)
		}
	}
}

// TestAnEnumNarrowsAnAnsweredFieldAndFailsARequestedOne holds compareSchemas to its one allowance
// both ways, over this build's own render moved by hand: a fork point whose GraphNode.kind and
// GraphNodeRead.kind named no values reads the enum they name now as narrowings, logged, and the
// same enum added to a string field of a schema a request takes fails, naming that field. It
// reads no history, so it runs wherever the tag is given.
func TestAnEnumNarrowsAnAnsweredFieldAndFailsARequestedOne(t *testing.T) {
	rendered := renderOpenAPI(t)
	requested := requestSchemas(t, rendered)
	if len(requested) == 0 {
		t.Fatal("no schema is reachable from a request body or a parameter, so the walk read nothing")
	}
	answered := []string{"GraphNode", "GraphNodeRead"}
	for _, name := range answered {
		if requested[name] {
			t.Fatalf("%s is reachable from a request, so it is no answered-only schema to narrow", name)
		}
	}
	documentSchemas := func(document map[string]any) map[string]any {
		return document["components"].(map[string]any)["schemas"].(map[string]any)
	}
	var before, after map[string]any
	for _, document := range []*map[string]any{&before, &after} {
		if err := json.Unmarshal(rendered, document); err != nil {
			t.Fatalf("decode the render: %v", err)
		}
	}
	// The fork point as a build before the enum rendered it.
	for _, name := range answered {
		kind := documentSchemas(before)[name].(map[string]any)["properties"].(map[string]any)["kind"].(map[string]any)
		if _, named := kind["enum"]; !named {
			t.Fatalf("%s.kind names no enum in this build's render", name)
		}
		delete(kind, "enum")
	}
	// A string field a request takes, gaining an enum: the first, by schema and field name.
	var taken, field string
	for _, name := range slices.Sorted(maps.Keys(requested)) {
		properties, _ := documentSchemas(after)[name].(map[string]any)["properties"].(map[string]any)
		for _, candidate := range slices.Sorted(maps.Keys(properties)) {
			property, _ := properties[candidate].(map[string]any)
			if _, named := property["enum"]; property["type"] == "string" && !named {
				taken, field = name, candidate
				property["enum"] = []any{"one", "two"}
				break
			}
		}
		if taken != "" {
			break
		}
	}
	if taken == "" {
		t.Fatal("no schema a request takes has a string field to narrow")
	}
	encode := func(what string, document map[string]any) servedDocument {
		raw, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("encode %s: %v", what, err)
		}
		return decodeServedDocument(t, what, raw)
	}
	failures, narrowings := compareSchemas(t, encode("the fork point", before), encode("this build", after), requested)
	for _, narrowing := range narrowings {
		t.Log(narrowing)
	}
	for _, failure := range failures {
		t.Log(failure)
	}
	if len(failures) != 1 || !strings.HasPrefix(failures[0], taken+"."+field+" was ") {
		t.Errorf("an enum added to %s.%s, which a request takes, fails %v; want that one field named", taken, field, failures)
	}
	for _, name := range answered {
		if !slices.ContainsFunc(narrowings, func(line string) bool { return strings.HasPrefix(line, name+".kind was ") }) {
			t.Errorf("the enum %s.kind names is no narrowing: %v", name, narrowings)
		}
	}
	if len(narrowings) != len(answered) {
		t.Errorf("compareSchemas reads %d narrowings, want the %d of %v: %v", len(narrowings), len(answered), answered, narrowings)
	}
}

// compareSchemas holds every schema the fork point declared to this build's (now), field by
// field: a failure for each schema, field or requirement that is gone and each shape that moved,
// and apart from them each narrowing, a field of a schema no request takes (requested names
// those a request does) whose shape is the fork point's with an enum added (narrowedToEnum).
func compareSchemas(t *testing.T, base, now servedDocument, requested map[string]bool) (failures, narrowings []string) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(base.schemas)) {
		declared := base.schemas[name]
		answered, held := now.schemas[name]
		if !held {
			failures = append(failures, fmt.Sprintf("the schema %s was declared at the fork point and is declared no longer", name))
			continue
		}
		if len(declared.properties) == 0 && declared.whole != answered.whole {
			failures = append(failures, fmt.Sprintf("the schema %s was %s at the fork point and is %s now", name, declared.whole, answered.whole))
		}
		for _, field := range slices.Sorted(maps.Keys(declared.properties)) {
			shape := declared.properties[field]
			current, kept := answered.properties[field]
			switch {
			case !kept:
				failures = append(failures, fmt.Sprintf("%s.%s was a field at the fork point and has disappeared", name, field))
			case current == shape:
			default:
				if enum, narrowed := narrowedToEnum(t, shape, current); narrowed && !requested[name] {
					narrowings = append(narrowings, fmt.Sprintf("%s.%s was %s at the fork point and names its values now, %s; no request takes %s, so a reader of the fork point reads every value it is sent",
						name, field, shape, enum, name))
					continue
				}
				failures = append(failures, fmt.Sprintf("%s.%s was %s at the fork point and is %s now", name, field, shape, current))
			}
		}
		for _, field := range declared.required {
			if !slices.Contains(answered.required, field) {
				failures = append(failures, fmt.Sprintf("%s.%s was required at the fork point and is required no longer", name, field))
			}
		}
	}
	return failures, narrowings
}

// narrowedToEnum reports whether current, a field's shape now (schemaShape), is base, its shape at
// the fork point, with an `enum` added and nothing else: base names no enum, and current without
// its enum is base. It answers the enum current names, as the document spells it.
func narrowedToEnum(t *testing.T, base, current string) (string, bool) {
	t.Helper()
	var was, is map[string]any
	if err := json.Unmarshal([]byte(base), &was); err != nil {
		return "", false
	}
	if err := json.Unmarshal([]byte(current), &is); err != nil {
		return "", false
	}
	enum, named := is["enum"]
	if _, before := was["enum"]; before || !named {
		return "", false
	}
	delete(is, "enum")
	without, err := json.Marshal(is)
	if err != nil {
		t.Fatalf("encode a shape without its enum: %v", err)
	}
	if string(without) != base {
		return "", false
	}
	values, err := json.Marshal(enum)
	if err != nil {
		t.Fatalf("encode an enum: %v", err)
	}
	return string(values), true
}

// requestSchemas is every component schema a request of document may carry: each one a `$ref`
// of an operation's parameters or request body reaches, through the `$ref`s of every component
// it reaches in turn. A schema outside it is one this build only answers.
func requestSchemas(t *testing.T, document []byte) map[string]bool {
	t.Helper()
	var raw struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components map[string]map[string]json.RawMessage `json:"components"`
	}
	if err := json.Unmarshal(document, &raw); err != nil {
		t.Fatalf("decode the document for the schemas a request takes: %v", err)
	}
	var pending []json.RawMessage
	for _, item := range raw.Paths {
		for key, value := range item {
			if key == "parameters" {
				// Parameters a path item declares for every operation under it.
				pending = append(pending, value)
				continue
			}
			var operation struct {
				Parameters  json.RawMessage `json:"parameters"`
				RequestBody json.RawMessage `json:"requestBody"`
			}
			if err := json.Unmarshal(value, &operation); err != nil {
				continue
			}
			pending = append(pending, operation.Parameters, operation.RequestBody)
		}
	}
	reached := map[string]bool{}
	seen := map[string]bool{}
	for len(pending) > 0 {
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if len(next) == 0 {
			continue
		}
		var value any
		if err := json.Unmarshal(next, &value); err != nil {
			t.Fatalf("decode a part of the document a request carries: %v", err)
		}
		walkOpenAPI(value, "", func(_ string, object map[string]any) {
			ref, isRef := object["$ref"].(string)
			if !isRef {
				return
			}
			section, name, found := strings.Cut(strings.TrimPrefix(ref, "#/components/"), "/")
			if !found || seen[ref] {
				return
			}
			seen[ref] = true
			if section == "schemas" {
				reached[name] = true
			}
			pending = append(pending, raw.Components[section][name])
		})
	}
	return reached
}

// servedDocument is an OpenAPI document as this test compares two of them: each operation's
// parameters, body and answers, and each schema's fields, every one as its shape.
type servedDocument struct {
	paths   map[string]map[string]servedOperation
	schemas map[string]servedSchema
}

// servedOperation is one method of one path: its parameters by "<in> <name>", its request body's
// shape, and its answers' shapes by status.
type servedOperation struct {
	parameters map[string]string
	body       string
	responses  map[string]string
}

// servedSchema is one component schema: the shape of each field, the fields it requires, and
// the shape of the whole schema, which is what a schema of no fields is compared by.
type servedSchema struct {
	properties map[string]string
	required   []string
	whole      string
}

// decodeServedDocument reads one OpenAPI document into the shapes it declares.
func decodeServedDocument(t *testing.T, what string, document []byte) servedDocument {
	t.Helper()
	var raw struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(document, &raw); err != nil {
		t.Fatalf("decode %s: %v", what, err)
	}
	served := servedDocument{paths: map[string]map[string]servedOperation{}, schemas: map[string]servedSchema{}}
	for path, operations := range raw.Paths {
		served.paths[path] = map[string]servedOperation{}
		for method, body := range operations {
			var operation struct {
				Parameters []struct {
					Name   string          `json:"name"`
					In     string          `json:"in"`
					Schema json.RawMessage `json:"schema"`
				} `json:"parameters"`
				RequestBody *struct {
					Content map[string]struct {
						Schema json.RawMessage `json:"schema"`
					} `json:"content"`
				} `json:"requestBody"`
				Responses map[string]struct {
					Content map[string]struct {
						Schema json.RawMessage `json:"schema"`
					} `json:"content"`
				} `json:"responses"`
			}
			if err := json.Unmarshal(body, &operation); err != nil {
				t.Fatalf("decode %s %s of %s: %v", method, path, what, err)
			}
			read := servedOperation{parameters: map[string]string{}, responses: map[string]string{}}
			for _, parameter := range operation.Parameters {
				read.parameters[parameter.In+" "+parameter.Name] = schemaShape(t, parameter.Schema)
			}
			if operation.RequestBody != nil {
				read.body = schemaShape(t, operation.RequestBody.Content["application/json"].Schema)
			}
			for status, response := range operation.Responses {
				content, carried := response.Content["application/json"]
				if carried && len(content.Schema) == 0 {
					read.responses[status] = opaqueJSON
					continue
				}
				read.responses[status] = schemaShape(t, content.Schema)
			}
			served.paths[path][method] = read
		}
	}
	for name, body := range raw.Components.Schemas {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(body, &schema); err != nil {
			t.Fatalf("decode the schema %s of %s: %v", name, what, err)
		}
		read := servedSchema{properties: map[string]string{}, required: schema.Required, whole: schemaShape(t, body)}
		for field, shape := range schema.Properties {
			read.properties[field] = schemaShape(t, shape)
		}
		served.schemas[name] = read
	}
	return served
}

// schemaShape is a JSON schema as its shape: the schema with every description taken out, in
// the one spelling json.Marshal gives it (object keys sorted). A description is prose about a
// field and no part of what a reader of it decodes, so rewording one is no change of shape.
func schemaShape(t *testing.T, schema json.RawMessage) string {
	t.Helper()
	if len(schema) == 0 {
		return "none"
	}
	var value any
	if err := json.Unmarshal(schema, &value); err != nil {
		t.Fatalf("decode a schema: %v", err)
	}
	var strip func(any) any
	strip = func(value any) any {
		switch typed := value.(type) {
		case map[string]any:
			delete(typed, "description")
			for key, inner := range typed {
				typed[key] = strip(inner)
			}
		case []any:
			for index, inner := range typed {
				typed[index] = strip(inner)
			}
		}
		return value
	}
	shape, err := json.Marshal(strip(value))
	if err != nil {
		t.Fatalf("encode a schema's shape: %v", err)
	}
	return string(shape)
}

// gitOutput runs one git command in dir and answers what it printed, trimmed, as
// cmd/dispatch's older-build test reads the commit it builds.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}
