package going

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

// strictKeywords are the only keywords the Final schema may use: the subset of JSON Schema that
// every service serving structured outputs takes, OpenAI's strict mode the narrowest of them.
// A keyword outside it (a union, a format, a pattern, a default, a bound) is one some service
// refuses with a 400, which costs every answer its block.
var strictKeywords = []string{"type", "properties", "required", "additionalProperties", "enum", "items", "description"}

// The Final schema keeps to the strict subset written out here, so a service that serves
// structured outputs serves it whether or not it checks the schema strictly: one object at the
// root, each node of a single type and no union, only the keywords above, and on every object
// every property required and additionalProperties false.
func TestTheFinalSchemaKeepsToTheStrictSubset(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(finalSchema, &root); err != nil {
		t.Fatalf("the Final schema is no JSON object: %v", err)
	}
	if root["type"] != "object" {
		t.Fatalf("the Final schema's root is a %v, want one object", root["type"])
	}
	checkStrict(t, "#", root)

	properties := root["properties"].(map[string]any)
	if got := slices.Sorted(maps.Keys(properties)); !slices.Equal(got, []string{"answer", "arguments", "block"}) {
		t.Errorf("the Final schema's properties are %v, want answer, block and arguments", got)
	}
	block := properties["block"].(map[string]any)["properties"].(map[string]any)
	kinds := block["kind"].(map[string]any)["enum"].([]any)
	want := []any{"chart", "progress", "table", "issue_list", "agent_status", "metric", "timeline", BlockNone}
	if !slices.Equal(kinds, want) {
		t.Errorf("the block's kinds are %v, want the seven kinds and none, %v", kinds, want)
	}
}

// The answer is the first property the schema names, so a model that writes the object in the
// schema's order writes the answer first and the reader is given it while the block and the
// arguments are still being written.
func TestTheFinalSchemaNamesTheAnswerFirst(t *testing.T) {
	decoder := json.NewDecoder(bytes.NewReader(finalSchema))
	depth := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			t.Fatalf("the Final schema names no properties: %v", err)
		}
		switch token {
		case json.Delim('{'), json.Delim('['):
			depth++
			continue
		case json.Delim('}'), json.Delim(']'):
			depth--
			continue
		}
		if depth != 1 || token != "properties" {
			continue
		}
		if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
			t.Fatalf("the root's properties are %v (%v), want an object", open, err)
		}
		first, err := decoder.Token()
		if err != nil || first != "answer" {
			t.Fatalf("the root's first property is %v (%v), want answer", first, err)
		}
		return
	}
}

// checkStrict walks one schema node and every node beneath it.
func checkStrict(t *testing.T, at string, node map[string]any) {
	t.Helper()
	for keyword := range node {
		if !slices.Contains(strictKeywords, keyword) {
			t.Errorf("%s uses the keyword %q, outside the strict subset %v", at, keyword, strictKeywords)
		}
	}
	kind, single := node["type"].(string)
	if !single {
		t.Errorf("%s has type %v, want a single type and no union", at, node["type"])
		return
	}
	if enum, ok := node["enum"].([]any); ok {
		for _, value := range enum {
			if _, ok := value.(string); !ok || kind != "string" {
				t.Errorf("%s enumerates %v of a %s, want strings of a string", at, value, kind)
			}
		}
	}
	switch kind {
	case "object":
		properties, ok := node["properties"].(map[string]any)
		if !ok || len(properties) == 0 {
			t.Errorf("%s is an object with no properties", at)
			return
		}
		if node["additionalProperties"] != false {
			t.Errorf("%s has additionalProperties %v, want false", at, node["additionalProperties"])
		}
		var required []string
		if raw, ok := node["required"].([]any); ok {
			for _, name := range raw {
				name, _ := name.(string)
				required = append(required, name)
			}
		}
		slices.Sort(required)
		if names := slices.Sorted(maps.Keys(properties)); !slices.Equal(required, names) {
			t.Errorf("%s requires %v, want every property it names, %v", at, required, names)
		}
		for name, property := range properties {
			child, ok := property.(map[string]any)
			if !ok {
				t.Errorf("%s/properties/%s is no schema object", at, name)
				continue
			}
			checkStrict(t, at+"/properties/"+name, child)
		}
	case "array":
		items, ok := node["items"].(map[string]any)
		if !ok {
			t.Errorf("%s is an array with no items schema", at)
			return
		}
		checkStrict(t, at+"/items", items)
	case "string", "number", "integer", "boolean":
		for _, keyword := range []string{"properties", "required", "additionalProperties", "items"} {
			if _, ok := node[keyword]; ok {
				t.Errorf("%s is a %s carrying %s", at, kind, keyword)
			}
		}
	default:
		t.Errorf("%s has type %q, want one of object, array, string, number, integer or boolean", at, kind)
	}
}
