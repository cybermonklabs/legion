package reviewer

import (
	"encoding/json"
)

// The JSON schemas of the replies, built for the text's kind and the rule sets a call applies.
// Each keeps OpenAI's strict mode's rules where the call sends it strict (Model.strict): every
// object closed, and every property required, a property a reply may leave empty allowing null.

// object is a JSON schema of an object with properties, of which required must be present, and
// no other property.
func object(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

// arrayOf is a JSON schema of an array of items.
func arrayOf(items any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

var (
	stringSchema  = map[string]any{"type": "string"}
	booleanSchema = map[string]any{"type": "boolean"}
)

// textSchema is a text of kind as a reply writes it: an ask's or a decision block's question,
// options and choice, an issue's title and spec, and any other kind's body. An option is its
// label and its description, with no id: an option's id is the server's (OptionWords).
func textSchema(kind Kind) map[string]any {
	switch {
	case kind.asks():
		option := object(map[string]any{"label": stringSchema, "description": stringSchema}, "label", "description")
		return object(map[string]any{"body": stringSchema, "options": arrayOf(option), "multiple": booleanSchema}, "body", "options", "multiple")
	case kind == KindIssue:
		return object(map[string]any{"title": stringSchema, "body": stringSchema}, "title", "body")
	default:
		return object(map[string]any{"body": stringSchema}, "body")
	}
}

// reviewSchema is a review's reply: its findings, each of one of the rule sets on, and the
// rewrite, which a reply with no finding leaves out. Strict mode requires every property, so a
// strict schema requires the rewrite and takes null for none, which reads as left out.
func reviewSchema(kind Kind, on []RuleSet, strict bool) json.RawMessage {
	finding := object(map[string]any{
		"rule_set": map[string]any{"type": "string", "enum": on},
		"quote":    stringSchema,
		"why":      stringSchema,
		"fix":      stringSchema,
	}, "rule_set", "quote", "why", "fix")
	rewrite, required := textSchema(kind), []string{"findings"}
	if strict {
		rewrite, required = map[string]any{"anyOf": []any{rewrite, map[string]any{"type": "null"}}}, append(required, "rewrite")
	}
	return mustJSON(object(map[string]any{"findings": arrayOf(finding), "rewrite": rewrite}, required...))
}

// rewriteSchema is a rewrite's reply: the text.
func rewriteSchema(kind Kind) json.RawMessage {
	return mustJSON(textSchema(kind))
}

// defineSchema is a definition's reply: one definition for each item of the words.
var defineSchema = mustJSON(object(map[string]any{
	"definitions": arrayOf(object(map[string]any{
		"words":   stringSchema,
		"meaning": stringSchema,
		"links":   arrayOf(stringSchema),
	}, "words", "meaning", "links")),
}, "definitions"))

// mustJSON is v as JSON; every schema here is a map of plain values, which always encodes.
func mustJSON(v any) json.RawMessage {
	encoded, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return encoded
}
