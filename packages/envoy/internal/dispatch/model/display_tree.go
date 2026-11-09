package model

import "encoding/json"

// DisplayTree is a stored text read as the ProseMirror document a reader is shown, held as the
// bytes pmdoc.ReadForDisplay wrote: nothing between the read that built it and the response
// parses or reshapes it, so raw bytes are what every `*_tree` field of every read carries.
//
// It is a type of its own rather than a json.RawMessage so that the bytes say what they are.
// Raw bytes reflect to a schema saying only that the field is JSON, which a client generated
// from the OpenAPI document reads as `unknown` and no reader could draw. The description below
// travels with the type, so a read that gains a tree field describes it by declaring it, where
// a table of what each field's bytes hold describes only the fields somebody remembered to
// write a row for.
type DisplayTree json.RawMessage

// MarshalJSON writes the tree's bytes where they stand, as a json.RawMessage does: they are
// already the JSON the read built.
func (t DisplayTree) MarshalJSON() ([]byte, error) {
	return json.RawMessage(t).MarshalJSON()
}

// UnmarshalJSON keeps the bytes as they came, so a Go client of this package reads the tree the
// server wrote rather than a re-encoding of it.
func (t *DisplayTree) UnmarshalJSON(data []byte) error {
	return (*json.RawMessage)(t).UnmarshalJSON(data)
}

// JSONSchemaBytes is the schema the OpenAPI emitter writes for every field of this type, which
// jsonschema-go asks a type that describes itself for (its RawExposer). The bytes are built per
// call rather than shared, since each property takes the schema as its own and the emitter
// writes on it.
func (DisplayTree) JSONSchemaBytes() ([]byte, error) {
	return json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"type": map[string]any{"type": "string"}},
		"required":             []any{"type"},
		"additionalProperties": true,
		"description": "a stored text read as the ProseMirror document a reader is shown " +
			"(pmdoc.ReadForDisplay): a node named by its own `type`, with whatever that node " +
			"type carries beside it. Present only for a caller that sent `Dispatch-Trees: 1`",
	})
}

// InlineJSONSchema marks the schema to be written where each tree field stands rather than
// declared under a name of its own (jsonschema-go's SchemaInliner). The name a client imports
// for a tree is the hand-written `DisplayTree` of @legion/contracts (dispatch-api-core.ts),
// which this schema matches; a declaration here would export a second name for the same shape.
func (DisplayTree) InlineJSONSchema() {}
