import { dispatchToolSpecs } from "../src/dispatch-tools";
import type { SchemaApi, SchemaNode } from "../src/tool-schema";

// The slice of a schema the Go side needs to check a proposed call: what each argument holds,
// whether it is required, the names an enum allows, and the bounds it states. gen-go runs every
// dispatch tool's arguments through a SchemaApi that records these calls instead of building zod,
// so the Go the server validates a call with is the TypeScript declaration itself rather than a
// second copy of it. A bound (min, max) is a text's length in UTF-16 units, as a JavaScript string
// counts it, a list's count of items or a number's value. Most are a route's own: a constant of
// dispatch-api-constants.ts or dispatch-tools-constants.ts that gen-go writes into Go for the
// handler that enforces it and names it in its refusal, or the route's refusal of an empty value.
// A few are the tool's alone, which no route reads: dispatch_issues' priority list of at most 5
// buckets (0 to 3, and null for none), which the route reads as repeated values, and the image
// files of `images`, which the command uploads before it sends anything. The catalogue serves a
// write tool's bounds beside its fields. dispatch-tools.test.ts holds the generated Go to every
// bound a spec states, and the fields its rows name to the constants their routes enforce.
type RecordedField = {
  kind: "string" | "number" | "boolean" | "enum" | "array" | "object" | "any";
  optional: boolean;
  nullable: boolean;
  description: string;
  values?: readonly string[];
  min?: number;
  max?: number;
  item?: RecordedField;
  fields?: [string, RecordedField][];
};

// One recorded node. Every modifier answers a new node, as zod's do, so a shared helper's
// `.optional()` never reaches the node another route built from it.
class RecordedNode implements SchemaNode<RecordedNode> {
  constructor(readonly field: RecordedField) {}

  optional() {
    return new RecordedNode({ ...this.field, optional: true });
  }

  nullable() {
    return new RecordedNode({ ...this.field, nullable: true });
  }

  describe(text: string) {
    return new RecordedNode({ ...this.field, description: text });
  }
}

function recordedNode(field: Omit<RecordedField, "optional" | "nullable" | "description">) {
  return new RecordedNode({ optional: false, nullable: false, description: "", ...field });
}

function recordedFields(shape: Record<string, RecordedNode>): [string, RecordedField][] {
  return Object.entries(shape).map(([name, node]) => [name, node.field]);
}

// The facade the tool specs' arguments are written against, recording what each call declares.
// `refineObject` records the object alone: a cross-field rule is the tool's and the route
// handler's to enforce, and the catalogue's check is of the arguments an action names, not of
// how they agree.
const recordingApi: SchemaApi<RecordedNode> = {
  string: (opts = {}) => recordedNode({ kind: "string", min: opts.min, max: opts.max }),
  number: (opts = {}) => recordedNode({ kind: "number", min: opts.min, max: opts.max }),
  boolean: () => recordedNode({ kind: "boolean" }),
  enum: (values) => recordedNode({ kind: "enum", values }),
  array: (item, opts = {}) =>
    recordedNode({ kind: "array", item: item.field, min: opts.min, max: opts.max }),
  unknown: () => recordedNode({ kind: "any" }),
  object: (shape) => recordedNode({ kind: "object", fields: recordedFields(shape) }),
  refineObject: (shape) => recordedNode({ kind: "object", fields: recordedFields(shape) }),
};

const toolFieldKinds = {
  string: "ToolFieldString",
  number: "ToolFieldNumber",
  boolean: "ToolFieldBoolean",
  enum: "ToolFieldEnum",
  array: "ToolFieldArray",
  object: "ToolFieldObject",
  any: "ToolFieldAny",
} satisfies Record<RecordedField["kind"], string>;

// A Go string literal of text: JSON's escapes are Go's for every character JSON escapes.
function goString(text: string) {
  return JSON.stringify(text);
}

// One field as a composite literal with its type elided, the form gofmt leaves alone inside a
// []ToolField. An array's element is rendered with no name, since it is the array's shape
// rather than an argument a call names.
function renderToolField(name: string, field: RecordedField): string {
  const parts = name === "" ? [] : [`Name: ${goString(name)}`];
  parts.push(`Kind: ${toolFieldKinds[field.kind]}`);
  if (!field.optional) parts.push("Required: true");
  if (field.nullable) parts.push("Nullable: true");
  if (field.description !== "") parts.push(`Description: ${goString(field.description)}`);
  if (field.values) parts.push(`Values: []string{${field.values.map(goString).join(", ")}}`);
  if (field.min !== undefined) parts.push(`Min: new(float64(${field.min}))`);
  if (field.max !== undefined) parts.push(`Max: new(float64(${field.max}))`);
  if (field.item) parts.push(`Item: &ToolField${renderToolField("", field.item)}`);
  if (field.fields) {
    const nested = field.fields.map(([key, value]) => `${renderToolField(key, value)},`).join("\n");
    parts.push(`Fields: []ToolField{\n${nested}\n}`);
  }
  return `{${parts.join(", ")}}`;
}

// The Go the action catalogue is built from: every dispatch tool with its own description, its
// own arguments and a call those arguments accept. Which of them the catalogue holds is the
// server's to decide from its own route table, so nothing here says which tool writes: this is
// what the TypeScript knows and the Go cannot read.
export function renderDispatchTools() {
  const tools = dispatchToolSpecs
    .map((spec) => {
      const fields = recordedFields(spec.arguments(recordingApi) as Record<string, RecordedNode>)
        .map(([name, field]) => `${renderToolField(name, field)},`)
        .join("\n");
      const parts = [
        `Name: ${goString(spec.name)}`,
        `Description: ${goString(spec.description)}`,
        `Arguments: []ToolField{\n${fields}\n}`,
        `Example: json.RawMessage(${goString(JSON.stringify(spec.example))})`,
      ];
      return `{\n${parts.join(",\n")},\n},`;
    })
    .join("\n");

  return `// Code generated by packages/contracts/scripts/gen-go.ts from
// packages/contracts/src/dispatch-tools.ts. DO NOT EDIT.

package contracts

import "encoding/json"

// ToolFieldKind is what one argument of a dispatch tool holds.
type ToolFieldKind string

const (
	ToolFieldString  ToolFieldKind = "string"
	ToolFieldNumber  ToolFieldKind = "number"
	ToolFieldBoolean ToolFieldKind = "boolean"
	ToolFieldEnum    ToolFieldKind = "enum"
	ToolFieldArray   ToolFieldKind = "array"
	ToolFieldObject  ToolFieldKind = "object"
	// ToolFieldAny is an argument whose shape the route reads for itself, such as a typed
	// block's attributes.
	ToolFieldAny ToolFieldKind = "any"
)

// ToolField is one argument a tool takes: an element of an array carries no name, and an
// object's own arguments hang from Fields. Min and Max are the bounds the spec states, nil for
// none: a text's length in UTF-16 units, a list's count of items, or a number's value.
type ToolField struct {
	Name        string        \`json:"name,omitempty"\`
	Kind        ToolFieldKind \`json:"kind"\`
	Required    bool          \`json:"required,omitempty"\`
	Nullable    bool          \`json:"nullable,omitempty"\`
	Description string        \`json:"description,omitempty"\`
	Values      []string      \`json:"values,omitempty"\`
	Min         *float64      \`json:"min,omitempty"\`
	Max         *float64      \`json:"max,omitempty"\`
	Item        *ToolField    \`json:"item,omitempty"\`
	Fields      []ToolField   \`json:"fields,omitempty"\`
}

// DispatchTool is one tool of the \`dispatch\` CLI as its spec states it: the name it is called
// by, the whole description a model chooses it on, the arguments it takes and a call those
// arguments accept.
type DispatchTool struct {
	Name        string          \`json:"name"\`
	Description string          \`json:"description"\`
	Arguments   []ToolField     \`json:"arguments"\`
	Example     json.RawMessage \`json:"example"\`
}

// DispatchTools is every dispatch tool, in the order dispatch-tools.ts states them.
var DispatchTools = []DispatchTool{
${tools}
}
`;
}
