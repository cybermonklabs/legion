//go:build basebuild

package api

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestNoRouteOrFieldTheForkPointServedIsGone is the half of c-no-regression that needs the build
// before this child: every route the table held at the fork point, and every field of every shape
// it declared, read against what this build's table answers.
//
// The fork point is `git merge-base origin/main HEAD`, and what it served is the OpenAPI document
// it committed (OpenAPIDocumentPath), which is the render of its route table: a document that
// differs from its render fails declaredShapesUnchanged at the commit that wrote it. This build's
// side is the render of its own table, taken now, so nothing committed beside this test can stand
// in for what the table answers. A route of the fork point that this table no longer serves fails
// here, and so does a schema, a field or a query parameter it no longer declares, a field that is
// no longer required, and any of them whose shape moved. A route, a schema or a field this child
// added is no regression and is not read.
//
// It runs under the build tag `basebuild` because it reads the repository's history: a checkout
// with no `origin/main` (a shallow clone, an archive) has no fork point to read, which is the
// reason cmd/dispatch's older-build test reads its base the same way.
func TestNoRouteOrFieldTheForkPointServedIsGone(t *testing.T) {
	root := gitOutput(t, ".", "rev-parse", "--show-toplevel")
	commit := gitOutput(t, root, "merge-base", "origin/main", "HEAD")
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
				switch current, declared := answered.responses[status]; {
				case !declared:
					t.Errorf("%s answered %s at the fork point and declares no such answer now", name, status)
				case current != shape:
					t.Errorf("%s's %s answer was %s at the fork point and is %s now", name, status, shape, current)
				}
			}
		}
	}
	for name, declared := range base.schemas {
		answered, held := now.schemas[name]
		if !held {
			t.Errorf("the schema %s was declared at the fork point and is declared no longer", name)
			continue
		}
		if len(declared.properties) == 0 && declared.whole != answered.whole {
			t.Errorf("the schema %s was %s at the fork point and is %s now", name, declared.whole, answered.whole)
		}
		for field, shape := range declared.properties {
			switch current, kept := answered.properties[field]; {
			case !kept:
				t.Errorf("%s.%s was a field at the fork point and has disappeared", name, field)
			case current != shape:
				t.Errorf("%s.%s was %s at the fork point and is %s now", name, field, shape, current)
			}
		}
		for _, field := range declared.required {
			if !slices.Contains(answered.required, field) {
				t.Errorf("%s.%s was required at the fork point and is required no longer", name, field)
			}
		}
	}
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
				read.responses[status] = schemaShape(t, response.Content["application/json"].Schema)
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
