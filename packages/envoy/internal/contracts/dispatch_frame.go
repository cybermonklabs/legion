package contracts

// dispatch_frame.go reads a Dispatch envelope's payload by the shapes a host reads it by
// (readDispatchPayload in packages/contracts/src/dispatch-api-targeted.ts): the JSON Schema
// packages/contracts/scripts/gen-go-dispatch-frame.ts renders from those zod schemas (carried here
// as dispatchFrameSchemaJSON), checked by a reader of the JSON Schema keywords that schema uses. A
// keyword it does not know stops the program at its start, so a schema change the reader cannot
// follow fails every test rather than passing a payload it did not check.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
)

// jsonShape is one JSON Schema node of the Dispatch frame schema.
type jsonShape struct {
	types            []string
	properties       map[string]*jsonShape
	required         []string
	additional       *jsonShape
	noAdditional     bool
	items            *jsonShape
	anyOf            []*jsonShape
	enum             []any
	pattern          *regexp.Regexp
	minimum          *float64
	maximum          *float64
	exclusiveMinimum *float64
}

// dispatchShapes is the schema's $defs: event, messageDelivery, commentDelivery, messagePayload and
// commentPayload.
var dispatchShapes = mustDispatchShapes(dispatchFrameSchemaJSON)

func mustDispatchShapes(text string) map[string]*jsonShape {
	var document struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal([]byte(text), &document); err != nil {
		panic(fmt.Sprintf("contracts: the Dispatch frame schema does not read: %v", err))
	}
	shapes := map[string]*jsonShape{}
	for _, name := range []string{"event", "messageDelivery", "commentDelivery", "messagePayload", "commentPayload"} {
		raw, ok := document.Defs[name]
		if !ok {
			panic("contracts: the Dispatch frame schema has no $defs." + name)
		}
		shape, err := readShape(raw)
		if err != nil {
			panic(fmt.Sprintf("contracts: the Dispatch frame schema's %s: %v", name, err))
		}
		shapes[name] = shape
	}
	return shapes
}

// readShape reads one schema node, refusing a keyword this reader does not check.
func readShape(raw json.RawMessage) (*jsonShape, error) {
	var node map[string]json.RawMessage
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	shape := &jsonShape{}
	number := func(raw json.RawMessage) (*float64, error) {
		var value float64
		err := json.Unmarshal(raw, &value)
		return &value, err
	}
	for keyword, value := range node {
		var err error
		switch keyword {
		case "$schema", "$comment", "description", "title", "readOnly", "format":
			// Annotations, which JSON Schema validates nothing by (readOnly is an annotation: the
			// schema marks a deleted item's origin with it); format is checked by the pattern zod
			// writes beside it, and one without a pattern is refused below.
		case "type":
			var one string
			if json.Unmarshal(value, &one) == nil {
				shape.types = []string{one}
			} else {
				err = json.Unmarshal(value, &shape.types)
			}
		case "properties":
			var properties map[string]json.RawMessage
			if err = json.Unmarshal(value, &properties); err == nil {
				shape.properties = map[string]*jsonShape{}
				for name, property := range properties {
					if shape.properties[name], err = readShape(property); err != nil {
						return nil, fmt.Errorf("%s: %w", name, err)
					}
				}
			}
		case "required":
			err = json.Unmarshal(value, &shape.required)
		case "additionalProperties":
			var allowed bool
			if json.Unmarshal(value, &allowed) == nil {
				shape.noAdditional = !allowed
			} else {
				shape.additional, err = readShape(value)
			}
		case "items":
			shape.items, err = readShape(value)
		case "anyOf":
			var branches []json.RawMessage
			if err = json.Unmarshal(value, &branches); err == nil {
				for _, branch := range branches {
					read, branchErr := readShape(branch)
					if branchErr != nil {
						return nil, branchErr
					}
					shape.anyOf = append(shape.anyOf, read)
				}
			}
		case "enum":
			err = json.Unmarshal(value, &shape.enum)
		case "pattern":
			var expression string
			if err = json.Unmarshal(value, &expression); err == nil {
				shape.pattern, err = regexp.Compile(expression)
			}
		case "minimum":
			shape.minimum, err = number(value)
		case "maximum":
			shape.maximum, err = number(value)
		case "exclusiveMinimum":
			shape.exclusiveMinimum, err = number(value)
		default:
			return nil, fmt.Errorf("the keyword %q, which this reader does not check", keyword)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", keyword, err)
		}
	}
	if _, formatted := node["format"]; formatted && shape.pattern == nil {
		return nil, fmt.Errorf("a format with no pattern, which this reader does not check")
	}
	return shape, nil
}

// holds reports whether value, decoded with json.Decoder.UseNumber, has the shape.
func (shape *jsonShape) holds(value any) bool {
	if len(shape.types) > 0 && !slices.ContainsFunc(shape.types, func(kind string) bool { return jsonKind(value, kind) }) {
		return false
	}
	if len(shape.anyOf) > 0 && !slices.ContainsFunc(shape.anyOf, func(branch *jsonShape) bool { return branch.holds(value) }) {
		return false
	}
	if shape.enum != nil && !slices.ContainsFunc(shape.enum, func(allowed any) bool { return jsonEqual(value, allowed) }) {
		return false
	}
	switch typed := value.(type) {
	case string:
		if shape.pattern != nil && !shape.pattern.MatchString(typed) {
			return false
		}
	case json.Number:
		number, err := strconv.ParseFloat(typed.String(), 64)
		if err != nil || math.IsInf(number, 0) ||
			(shape.minimum != nil && number < *shape.minimum) || (shape.maximum != nil && number > *shape.maximum) ||
			(shape.exclusiveMinimum != nil && number <= *shape.exclusiveMinimum) {
			return false
		}
	case []any:
		if shape.items != nil {
			for _, item := range typed {
				if !shape.items.holds(item) {
					return false
				}
			}
		}
	case map[string]any:
		for _, name := range shape.required {
			if _, ok := typed[name]; !ok {
				return false
			}
		}
		for name, field := range typed {
			if property, ok := shape.properties[name]; ok {
				if !property.holds(field) {
					return false
				}
				continue
			}
			if shape.noAdditional || (shape.additional != nil && !shape.additional.holds(field)) {
				return false
			}
		}
	}
	return true
}

// jsonKind reports whether value is of the JSON Schema type kind, an integer being a number with no
// fraction as JavaScript reads one (1.0 is the integer 1).
func jsonKind(value any, kind string) bool {
	switch typed := value.(type) {
	case nil:
		return kind == "null"
	case bool:
		return kind == "boolean"
	case string:
		return kind == "string"
	case []any:
		return kind == "array"
	case map[string]any:
		return kind == "object"
	case json.Number:
		if kind == "number" {
			return true
		}
		number, err := strconv.ParseFloat(typed.String(), 64)
		return kind == "integer" && err == nil && !math.IsInf(number, 0) && number == math.Trunc(number)
	}
	return false
}

// jsonEqual reports whether value equals allowed, an enum member, as JavaScript's === does for the
// strings, numbers, booleans and null an enum holds.
func jsonEqual(value, allowed any) bool {
	switch typed := allowed.(type) {
	case float64:
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		read, err := strconv.ParseFloat(number.String(), 64)
		return err == nil && read == typed
	case nil, bool, string:
		return value == allowed
	}
	return false
}

// DispatchPayloadIsEvent reports whether payload, a Dispatch envelope's payload, is one a host reads
// as an event and shows (readDispatchPayload's kind "event"): a targeted frame, `{event, delivery}`,
// whose event header holds, whose delivery line is a comment's or a message's, and whose payload is
// a complete comment for a comment.created event with a comment's line or a complete message for a
// message.created event with a message's - DispatchTargetedFrameSchema's refinement, which JSON
// Schema cannot say, made here by hand - or, for a payload with no "delivery" key, an event whose
// header holds. The union of the two delivery lines is read as zod reads it: a comment's line
// first, then a message's, whose reading drops the comment's fields. A payload that is no JSON
// encoding/json reads, or that holds a "__proto__" key at any depth, which JSON.parse reads as an
// own key and zod's passthrough copies onto a prototype, is none.
func DispatchPayloadIsEvent(payload []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.More() || holdsPrototypeKey(value) {
		return false
	}
	frame, isObject := value.(map[string]any)
	if !isObject {
		return false
	}
	if _, targeted := frame["delivery"]; !targeted {
		return dispatchShapes["event"].holds(value)
	}
	event, eventIsObject := frame["event"].(map[string]any)
	if !eventIsObject || !dispatchShapes["event"].holds(event) {
		return false
	}
	comment := dispatchShapes["commentDelivery"].holds(frame["delivery"])
	message := !comment && dispatchShapes["messageDelivery"].holds(frame["delivery"])
	switch event["type"] {
	case "message.created":
		return message && dispatchShapes["messagePayload"].holds(event["payload"])
	case "comment.created":
		return comment && dispatchShapes["commentPayload"].holds(event["payload"])
	}
	return false
}

// holdsPrototypeKey reports whether value holds an object with a "__proto__" key at any depth.
func holdsPrototypeKey(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for name, field := range typed {
			if name == "__proto__" || holdsPrototypeKey(field) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, holdsPrototypeKey)
	}
	return false
}
