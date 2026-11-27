package agentloop

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// strictKeywords is every JSON Schema keyword a Final schema may carry: the keywords both
// providers' documented structured-output subsets enforce and this package's SDK sends on, and
// the two annotations that constrain nothing. Anything else is refused by name — minLength,
// maximum, multipleOf, pattern, minItems, uniqueItems, const, a keyword a later draft adds —
// because a constraint that does not reach the provider's grammar is a constraint the caller
// believes in and the model never sees. The SDK's Anthropic provider is the clearest case: it
// strips every one of those keywords out of the schema and writes it into the node's
// description, which is prose to the model and binds nothing.
//
//	https://platform.claude.com/docs/en/build-with-claude/structured-outputs#json-schema-limitations
//	https://developers.openai.com/api/docs/guides/structured-outputs#supported-schemas
var strictKeywords = []string{
	"$defs", "$ref", "additionalProperties", "definitions", "description", "enum", "format",
	"items", "properties", "required", "title", "type",
}

// strictUnions is every keyword that joins subschemas. A Final schema takes none of them:
// OpenAI's strict mode refuses allOf, not, if, then and else outright and refuses an anyOf at
// the root, Anthropic refuses allOf with $ref and counts every anyOf and every type array
// against a per-request cap of sixteen union parameters, and one Request.Final rides to
// whichever provider the request names.
var strictUnions = []string{"allOf", "anyOf", "else", "if", "not", "oneOf", "then"}

// strictTypes is every type a Final schema's node may be: the basic types, each on its own.
var strictTypes = []string{"array", "boolean", "integer", "null", "number", "object", "string"}

// strictFormats is every string format both providers enforce: Anthropic's list without uri,
// which OpenAI's does not carry.
var strictFormats = []string{
	"date", "date-time", "duration", "email", "hostname", "ipv4", "ipv6", "time", "uuid",
}

// strictFinal is Request.Final held to the structured-output subset both providers enforce,
// which is how a run finds out that a schema would ride to the model and bind nothing. The
// error names the JSON path of the node at fault and what is wrong with it, so the caller fixes
// the schema rather than the symptom; the caller of this function gives it its prefix. The walk
// starts at the root, then walks each definition it came to, in the order it came to them, until
// none is left (defs). No node is in progress when one of those walks starts, so a definition is
// not inside the node whose $defs holds it, and a definition already done is not walked again.
func strictFinal(final json.RawMessage) error {
	var doc any
	if err := json.Unmarshal(final, &doc); err != nil {
		return err
	}
	s := &strictSchema{doc: doc, states: map[string]strictState{}}
	if err := s.node(doc, "", "#"); err != nil {
		return err
	}
	for len(s.queue) > 0 {
		def := s.queue[0]
		s.queue = s.queue[1:]
		if err := s.node(def.value, def.path, def.pointer); err != nil {
			return err
		}
	}
	return nil
}

// strictSchema is one walk of a Final schema: the whole document, which a $ref points into; how
// far the walk has come with each node, keyed by the pointer a $ref names the node by (node);
// and the definitions the walk came to and has not walked yet, in the order it came to them
// (defs).
type strictSchema struct {
	doc    any
	states map[string]strictState
	queue  []strictDef
}

// strictState is how far the walk has come with one node: not come to it yet, in progress, which
// is every node on the way from where the walk started to where it is now, or done, which is a
// node whose checks passed, with those of every node it leads to.
type strictState int

const (
	strictUnseen strictState = iota
	strictInProgress
	strictDone
)

// strictDef is a definition the walk came to and walks later: the definition itself, the path an
// error names it by, and the pointer a $ref names it by.
type strictDef struct {
	value         any
	path, pointer string
}

// node is one node walked. The walk goes from a node to each of its properties, to its items,
// and from a $ref to the node it names, depth first; a node's definitions are walked later
// (defs). pointer is where the node lies in the schema, as a $ref names it, "#" at the root,
// and the node's key: a node that is done holds wherever the walk comes to it again, so its
// checks run once however many $refs name it, and a node in progress that the walk comes to
// again ends a chain of properties, items and $refs that leads from the node back to itself.
// That is a recursive schema, which Anthropic does not serve, and the error names the path the
// walk came to the node by the second time.
func (s *strictSchema) node(value any, path, pointer string) error {
	switch s.states[pointer] {
	case strictDone:
		return nil
	case strictInProgress:
		return fmt.Errorf("%s: a recursive $ref, %q", at(path), pointer)
	}
	s.states[pointer] = strictInProgress
	if err := s.check(value, path, pointer); err != nil {
		return err
	}
	s.states[pointer] = strictDone
	return nil
}

// check is one node's checks: its keywords, its type, then what that type brings with it, each
// node inside it walked through node, so no part of a schema is left unread. The pointers of the
// nodes inside it are its own with their place after it.
func (s *strictSchema) check(value any, path, pointer string) error {
	m, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: not a JSON object", at(path))
	}
	for _, key := range slices.Sorted(maps.Keys(m)) {
		switch {
		case slices.Contains(strictUnions, key):
			return fmt.Errorf("%s: an %s union (a node of a Final schema takes one type)", at(path), key)
		case !slices.Contains(strictKeywords, key):
			return fmt.Errorf("%s: unsupported keyword %q", at(path), key)
		}
	}
	if _, ok := m["$ref"]; ok {
		// The shape a schema generator writes when the type it reflects is named:
		// a root that is one $ref into $defs. Neither provider takes it.
		if path == "" {
			return fmt.Errorf("%s: the schema itself is a $ref, not an object", at(path))
		}
		return s.ref(m, path)
	}

	kind := ""
	if raw, ok := m["type"]; ok {
		switch t := raw.(type) {
		case string:
			kind = t
		case []any:
			return fmt.Errorf("%s: a type union (type is an array)", at(path))
		default:
			return fmt.Errorf("%s: type is not a string", at(path))
		}
		if !slices.Contains(strictTypes, kind) {
			return fmt.Errorf("%s: unsupported type %q", at(path), kind)
		}
	}
	if path == "" && kind != "object" {
		return fmt.Errorf("%s: the schema itself is not an object", at(path))
	}
	if kind == "" {
		return fmt.Errorf("%s: no type (a node of a Final schema takes one type)", at(path))
	}
	if err := s.scalar(m, path); err != nil {
		return err
	}

	_, properties := m["properties"]
	_, required := m["required"]
	_, additional := m["additionalProperties"]
	if kind == "object" || properties || required || additional {
		if err := s.object(m, path, pointer); err != nil {
			return err
		}
	}
	if _, items := m["items"]; kind == "array" || items {
		if err := s.array(m, path, pointer); err != nil {
			return err
		}
	}
	return s.defs(m, path, pointer)
}

// ref is a $ref node: it names a place in this schema and nothing else, and the walk goes on at
// what it names, at the path of the $ref and under the pointer of the place it names however
// the $ref writes it (resolve), so one node has one key.
func (s *strictSchema) ref(m map[string]any, path string) error {
	pointer, ok := m["$ref"].(string)
	if !ok {
		return fmt.Errorf("%s: $ref is not a string", at(path))
	}
	for _, key := range slices.Sorted(maps.Keys(m)) {
		if key != "$ref" && key != "description" && key != "title" {
			return fmt.Errorf("%s: a $ref that carries %q as well", at(path), key)
		}
	}
	target, place, err := s.resolve(pointer, path)
	if err != nil {
		return err
	}
	return s.node(target, path, place)
}

// resolve is the node a pointer names, and the pointer of that place written again from its
// names as within writes the pointers of the nodes the walk comes to, so two ways of writing
// one place are one key. An external $ref is a document neither provider loads, and a pointer
// that names nothing is a schema that cannot be compiled at all.
func (s *strictSchema) resolve(pointer, path string) (any, string, error) {
	if pointer == "#" {
		return s.doc, pointer, nil
	}
	if !strings.HasPrefix(pointer, "#/") {
		return nil, "", fmt.Errorf("%s: $ref %q points outside this schema", at(path), pointer)
	}
	node := s.doc
	var names []string
	for segment := range jsontext.Pointer(strings.TrimPrefix(pointer, "#")).Tokens() {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("%s: $ref %q names nothing in this schema", at(path), pointer)
		}
		if node, ok = m[segment]; !ok {
			return nil, "", fmt.Errorf("%s: $ref %q names nothing in this schema", at(path), pointer)
		}
		names = append(names, segment)
	}
	return node, within("#", names...), nil
}

// object is an object node: every object a provider generates holds the properties the schema
// names and no others, so additionalProperties is false and every property is required. A
// property left out of required is the defect this check exists for: the schema rides to the
// provider, the provider does not hold the model to it, and the reply is whatever the model
// writes.
func (s *strictSchema) object(m map[string]any, path, pointer string) error {
	additional, ok := m["additionalProperties"]
	switch {
	case !ok:
		return fmt.Errorf("%s: no additionalProperties: false", at(path))
	case additional != false:
		return fmt.Errorf("%s: additionalProperties is %v, want false", at(path), additional)
	}
	properties := map[string]any{}
	if raw, ok := m["properties"]; ok {
		if properties, ok = raw.(map[string]any); !ok {
			return fmt.Errorf("%s: properties is not an object", at(path))
		}
	}
	required := map[string]bool{}
	if raw, ok := m["required"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("%s: required is not an array", at(path))
		}
		for _, value := range list {
			name, ok := value.(string)
			if !ok {
				return fmt.Errorf("%s: required holds %v, which is no property name", at(path), value)
			}
			if _, ok := properties[name]; !ok {
				return fmt.Errorf("%s: required names %q, which is no property of this object", at(path), name)
			}
			required[name] = true
		}
	}
	names := slices.Sorted(maps.Keys(properties))
	for _, name := range names {
		if !required[name] {
			return fmt.Errorf("%s: optional property (not in required)", at(under(path, "properties", name)))
		}
	}
	for _, name := range names {
		if err := s.node(properties[name], under(path, "properties", name), within(pointer, "properties", name)); err != nil {
			return err
		}
	}
	return nil
}

// array is an array node: it says what its items are, since a provider constrains what it
// generates and can constrain no item it has no schema for. How many items there are is not a
// Final schema's to say: Anthropic enforces no array constraint but a minItems of 0 or 1, and
// the SDK takes minItems out of the schema before the call either way.
func (s *strictSchema) array(m map[string]any, path, pointer string) error {
	items, ok := m["items"]
	if !ok {
		return fmt.Errorf("%s: an array with no items schema", at(path))
	}
	return s.node(items, under(path, "items"), within(pointer, "items"))
}

// scalar is what a node says about the value itself: an enum of plain values, since neither
// provider takes an object or an array in one, and a format both providers know.
func (s *strictSchema) scalar(m map[string]any, path string) error {
	if raw, ok := m["enum"]; ok {
		values, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("%s: enum is not an array", at(path))
		}
		for _, value := range values {
			switch value.(type) {
			case nil, bool, float64, string:
			default:
				return fmt.Errorf("%s: enum holds %v, which is no plain value", at(path), value)
			}
		}
	}
	if raw, ok := m["format"]; ok {
		format, ok := raw.(string)
		if !ok || !slices.Contains(strictFormats, format) {
			return fmt.Errorf("%s: unsupported format %v", at(path), raw)
		}
	}
	return nil
}

// defs is the node's own definitions put in the walk's queue, each with the path an error names
// it by and the pointer a $ref names it by, the node's own with the keyword and the definition's
// name after it. strictFinal walks them once the walk that came to them is over, so a definition
// the schema never uses is held to the same subset, and a definition that names the node holding
// it is no recursive schema unless that node leads back to the definition.
func (s *strictSchema) defs(m map[string]any, path, pointer string) error {
	for _, keyword := range []string{"$defs", "definitions"} {
		raw, ok := m[keyword]
		if !ok {
			continue
		}
		defs, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: %s is not an object", at(path), keyword)
		}
		for _, name := range slices.Sorted(maps.Keys(defs)) {
			s.queue = append(s.queue, strictDef{
				value:   defs[name],
				path:    under(path, keyword, name),
				pointer: within(pointer, keyword, name),
			})
		}
	}
	return nil
}

// under is the path of a node inside path, which is empty at the schema's root.
func under(path string, segments ...string) string {
	if path != "" {
		segments = append([]string{path}, segments...)
	}
	return strings.Join(segments, ".")
}

// within is the pointer of a node inside the node at pointer: pointer with each name after it,
// escaped as RFC 6901 escapes a name.
func within(pointer string, names ...string) string {
	p := jsontext.Pointer(pointer)
	for _, name := range names {
		p = p.AppendToken(name)
	}
	return string(p)
}

// at is a path as an error names it.
func at(path string) string {
	if path == "" {
		return "the root"
	}
	return path
}
