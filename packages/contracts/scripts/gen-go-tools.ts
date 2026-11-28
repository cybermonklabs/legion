import {
  type DispatchToolRoute,
  type DispatchToolSpec,
  dispatchToolSpecs,
  personToolSpecs,
} from "../src/dispatch-tools";
import type { SchemaApi, SchemaNode } from "../src/tool-schema";

// The slice of a schema the Go side needs to check a proposed call: what each argument holds,
// whether it is required, the names an enum allows, and the bounds it states. gen-go runs every
// dispatch tool's arguments through a SchemaApi that records these calls instead of building zod,
// so the Go the server validates a call with is the TypeScript declaration itself rather than a
// second copy of it. A bound (min, max) is a text's length in UTF-16 units, as a JavaScript string
// counts it, a list's count of items or a number's value. Most are a route's own: a constant of
// dispatch-api-limits.ts or dispatch-tools.ts that gen-go writes into Go for the handler that
// enforces it and names it in its refusal, or the route's refusal of an empty value. A few are the
// tool's alone, which no route reads: dispatch_issues' priority list of at most 5 buckets (0 to 3,
// and null for none), which the route reads as repeated values, and the image files of `images`,
// which the command uploads before it sends anything. The catalogue serves a write tool's bounds
// beside its fields, and the command bar's check of a call holds each one too.
// dispatch-tools.test.ts holds the generated Go to every bound a spec states, and the fields its
// rows name to the constants their routes enforce; the server's test of every write tool's schema
// against its route sends a call at each edge.
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
// `refineObject` records the object alone: a tool's own cross-field rule (`DispatchToolSpec.
// validation`) is a function no recording can carry, so the generator writes the rule's words
// (`DispatchTool.Validation`) and the server holds each write tool marked so to a Go port of it,
// which its test holds to the marks; an object argument's own rule (`componentsArgument`) is the
// route handler's to enforce.
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

// One route binding as a composite literal with its type elided, inside a []ToolRoute. Every
// method but GET sends a JSON body, `{}` where the binding lists no argument for it, so an empty
// list is left out as an absent one is.
function renderToolRoute(route: DispatchToolRoute): string {
  const parts = [`Method: ${goString(route.method)}`, `Pattern: ${goString(route.path)}`];
  const params = Object.entries(route.params ?? {});
  if (params.length > 0) {
    const bound = params.map(([key, value]) => `${goString(key)}: ${goString(value)}`).join(", ");
    parts.push(`Path: map[string]string{${bound}}`);
  }
  if ((route.query ?? []).length > 0) {
    parts.push(`Query: []string{${(route.query ?? []).map(goString).join(", ")}}`);
  }
  if ((route.body ?? []).length > 0) {
    parts.push(`Body: []string{${(route.body ?? []).map(goString).join(", ")}}`);
  }
  return `{${parts.join(", ")}}`;
}

// A tool's label as a composite literal, its count noun left out where the spec states none.
function renderToolLabel(label: DispatchToolSpec["label"]): string {
  const parts = [
    `Kind: ${goString(label.kind)}`,
    `Verb: ${goString(label.verb)}`,
    `Present: ${goString(label.present)}`,
    `Past: ${goString(label.past)}`,
  ];
  if (label.count !== undefined) {
    parts.push(`Count: ${goString(label.count)}`);
  }
  return `ToolLabel{${parts.join(", ")}}`;
}

// The Go the action catalogue is built from: every dispatch tool with its own description, its
// own arguments, the words of its cross-field rule where its spec states one, and a call those
// arguments accept, then the person tools the command bar alone calls (personToolSpecs). Which of
// them the catalogue holds is the server's to decide from its own route table, so nothing here
// says which tool writes: this is what the TypeScript knows and the Go cannot read.
export function renderDispatchTools() {
  const specs: readonly DispatchToolSpec[] = [...dispatchToolSpecs, ...personToolSpecs];
  const tools = specs
    .map((spec) => {
      const fields = recordedFields(spec.arguments(recordingApi) as Record<string, RecordedNode>)
        .map(([name, field]) => `${renderToolField(name, field)},`)
        .join("\n");
      const parts = [
        `Name: ${goString(spec.name)}`,
        `Label: ${renderToolLabel(spec.label)}`,
        `Description: ${goString(spec.description)}`,
        `Arguments: []ToolField{\n${fields}\n}`,
        `Example: json.RawMessage(${goString(JSON.stringify(spec.example))})`,
      ];
      if (spec.validation !== undefined) {
        parts.push(`Validation: ${goString(spec.validation.message)}`);
      }
      const routes: readonly DispatchToolRoute[] = spec.routes ?? [];
      if (routes.length > 0) {
        parts.push(
          `Routes: []ToolRoute{\n${routes.map((route) => `${renderToolRoute(route)},`).join("\n")}\n}`
        );
      }
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
// by, the words the command bar shows for a call of it (Label), the whole description a model
// chooses it on, the arguments it takes, a call those arguments accept, the whole words of the
// cross-field rule its spec states beside its arguments (Validation, DispatchToolSpec.validation's
// message), empty for a spec that states none, and, for a tool that calls one route of the table
// as it stands, that route's binding (Routes), empty for a tool whose command is written by hand.
type DispatchTool struct {
	Name        string          \`json:"name"\`
	Label       ToolLabel       \`json:"label"\`
	Description string          \`json:"description"\`
	Arguments   []ToolField     \`json:"arguments"\`
	Example     json.RawMessage \`json:"example"\`
	Validation  string          \`json:"validation,omitempty"\`
	Routes      []ToolRoute     \`json:"routes,omitempty"\`
}

// ToolLabel is the words the command bar shows for one call of a tool, as dispatch-tools.ts's
// DispatchToolLabel states them: the kind of step it is (read, search, look, chart, plan, review
// or write), its base verb, its present form while the call runs and its past form once it ended,
// each naming an argument as {name} and an argument a call may leave out as {name|words shown in
// its place}, and the noun a tool that answers a list counts its results by, empty for another.
type ToolLabel struct {
	Kind    string \`json:"kind"\`
	Verb    string \`json:"verb"\`
	Present string \`json:"present"\`
	Past    string \`json:"past"\`
	Count   string \`json:"count,omitempty"\`
}

// ToolRoute is one route of the table a tool calls, as dispatch-tools.ts binds it: the method,
// the path pattern, the argument that fills each placeholder of the pattern (Path, by
// placeholder), the arguments sent as query parameters under their own names, and the arguments
// sent as the members of a JSON body under their own names. Every method but GET sends a JSON
// body. A tool's routes are tried in order, and a call takes the first whose placeholders its
// arguments all fill.
type ToolRoute struct {
	Method  string            \`json:"method"\`
	Pattern string            \`json:"pattern"\`
	Path    map[string]string \`json:"path,omitempty"\`
	Query   []string          \`json:"query,omitempty"\`
	Body    []string          \`json:"body,omitempty"\`
}

// DispatchTools is every dispatch tool, in the order dispatch-tools.ts states them, then the
// person tools the command bar alone calls (personToolSpecs), which no dispatch command offers.
var DispatchTools = []DispatchTool{
${tools}
}
`;
}
