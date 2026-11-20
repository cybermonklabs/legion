import { z } from "zod";

/**
 * How the Dispatch contract tests read a zod schema: the one reading of zod's definitions that
 * `dispatch-api-spellings.test.ts` (which field names, which absences and which value types a
 * schema declares) and `example-walk.testing.ts` (which keys, values and forms each object of a
 * shared example must hold) share. It is used by tests alone, and nothing of the package exports
 * it.
 *
 * A schema is read as one node of these kinds, each with the schemas it holds:
 *
 * - `object`: its shape, by key;
 * - `array`: its element, and `record`: its value type;
 * - `union`: its options, a discriminated union's included;
 * - `intersection`: its two parts, each of which a value must satisfy;
 * - `wrapper`: the one schema it reads a value through - the inner type of `optional`,
 *   `nullable`, `readonly`, `default`, `prefault`, `nonoptional`, `catch`, `success` and
 *   `promise`, the schema a `lazy` answers, and of a `pipe` the input it parses (`in`), or the
 *   output when the input is a transform, as `z.preprocess` builds;
 * - `leaf`: a value of no inner schema, with the JSON kinds it takes (`json`: `string`,
 *   `number`, `boolean`, `null`, or `any` for a schema that takes every value; none for `never`,
 *   `undefined` and `void`), `closed` when it takes a closed set of values (an `enum` or a
 *   `literal`, whose kinds are their values'), and that set's `values` (none for an open one).
 *
 * On that reading, `valueSet` says which set of values (an enum's, a literal's, a boolean's, a
 * union of them) a schema reads a value as, if any. Which forms of value a schema takes at a place
 * is never read from its definition: `takenForms` asks zod's own parse of each probe value
 * (`FORM_PROBES`), so a coercion, a preprocess, a fallback or a refinement is judged by what it
 * takes, however it is spelled.
 *
 * Any other kind (`tuple`, `map`, `set`, a bare `transform`, ...) is an error naming it and where
 * it is: a schema these tests cannot read is reported, never taken as read.
 */
export type SchemaNode =
  | { readonly kind: "object"; readonly shape: Readonly<Record<string, z.core.$ZodType>> }
  | { readonly kind: "array"; readonly element: z.core.$ZodType }
  | { readonly kind: "record"; readonly value: z.core.$ZodType }
  | { readonly kind: "union"; readonly options: readonly z.core.$ZodType[] }
  | { readonly kind: "intersection"; readonly parts: readonly z.core.$ZodType[] }
  | { readonly kind: "wrapper"; readonly inner: z.core.$ZodType }
  | {
      readonly kind: "leaf";
      readonly closed: boolean;
      readonly json: readonly JsonKind[];
      readonly values: readonly unknown[];
    };

/** A kind of JSON value, or `any` for a schema that takes every value. */
export type JsonKind = "string" | "number" | "boolean" | "null" | "object" | "array" | "any";

const WRAPPERS: Readonly<Record<string, true>> = {
  optional: true,
  nullable: true,
  readonly: true,
  default: true,
  prefault: true,
  nonoptional: true,
  catch: true,
  success: true,
  promise: true,
};

/** Each leaf kind of an open set of values, and the JSON kinds it takes: a `bigint`, a `date` and
 *  a `symbol` take no JSON value at all, and `custom` is a check whose value zod does not type. */
const LEAVES: Readonly<Record<string, readonly JsonKind[]>> = {
  any: ["any"],
  bigint: [],
  boolean: ["boolean"],
  custom: ["any"],
  date: [],
  nan: ["number"],
  never: [],
  null: ["null"],
  number: ["number"],
  string: ["string"],
  symbol: [],
  template_literal: ["string"],
  undefined: [],
  unknown: ["any"],
  void: [],
};

/** The JSON kind of one value an `enum` or a `literal` takes. */
function jsonKindOf(value: unknown): JsonKind {
  if (value === null) return "null";
  if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") {
    return typeof value as JsonKind;
  }
  return "any";
}

/** The node a schema is, read at `where` (the place an error names). */
export function schemaNode(schema: unknown, where: string): SchemaNode {
  if (!(schema instanceof z.core.$ZodType)) {
    throw new Error(`${where} is not a zod schema`);
  }
  const def = schema._zod.def;
  if (def.type === "object" && schema instanceof z.core.$ZodObject) {
    return { kind: "object", shape: schema._zod.def.shape };
  }
  if (def.type === "array" && schema instanceof z.core.$ZodArray) {
    return { kind: "array", element: schema._zod.def.element };
  }
  if (def.type === "record" && schema instanceof z.core.$ZodRecord) {
    return { kind: "record", value: schema._zod.def.valueType };
  }
  if (def.type === "union" && schema instanceof z.core.$ZodUnion) {
    return { kind: "union", options: schema._zod.def.options };
  }
  if (def.type === "intersection" && schema instanceof z.core.$ZodIntersection) {
    return { kind: "intersection", parts: [schema._zod.def.left, schema._zod.def.right] };
  }
  if (def.type === "lazy" && schema instanceof z.core.$ZodLazy) {
    return { kind: "wrapper", inner: schema._zod.def.getter() };
  }
  if (def.type === "pipe" && schema instanceof z.core.$ZodPipe) {
    const input = schema._zod.def.in;
    return {
      kind: "wrapper",
      inner: input._zod.def.type === "transform" ? schema._zod.def.out : input,
    };
  }
  if (
    WRAPPERS[def.type] === true &&
    "innerType" in def &&
    def.innerType instanceof z.core.$ZodType
  ) {
    return { kind: "wrapper", inner: def.innerType };
  }
  if (def.type === "enum" && schema instanceof z.core.$ZodEnum) {
    const values = Object.values(schema._zod.def.entries);
    return { kind: "leaf", closed: true, json: [...new Set(values.map(jsonKindOf))], values };
  }
  if (def.type === "literal" && schema instanceof z.core.$ZodLiteral) {
    const values: readonly unknown[] = schema._zod.def.values;
    return { kind: "leaf", closed: true, json: [...new Set(values.map(jsonKindOf))], values };
  }
  const json = LEAVES[def.type];
  if (json !== undefined) return { kind: "leaf", closed: false, json, values: [] };
  throw new Error(`${where} is a zod ${def.type} schema, which the contract tests do not read`);
}

/** The shape of the object a schema reads a value as, through its wrappers; undefined for a
 *  schema that is no object there (a union, an intersection, a list, a map or a leaf). */
export function objectShape(
  schema: unknown,
  where: string
): Readonly<Record<string, z.core.$ZodType>> | undefined {
  const node = schemaNode(schema, where);
  if (node.kind === "object") return node.shape;
  return node.kind === "wrapper" ? objectShape(node.inner, where) : undefined;
}

/** The object schema a schema reads a value as, through its wrappers: the schema itself where it
 *  is an object, else the object one of its wrappers holds, else undefined. */
export function objectSchema(schema: unknown, where: string): z.core.$ZodType | undefined {
  const node = schemaNode(schema, where);
  if (node.kind === "object" && schema instanceof z.core.$ZodType) return schema;
  return node.kind === "wrapper" ? objectSchema(node.inner, where) : undefined;
}

/** Whether a schema takes a value: zod's own parse of it succeeds. */
export function takes(schema: unknown, value: unknown): boolean {
  if (!(schema instanceof z.core.$ZodType)) return false;
  return z.safeParse(schema, value).success;
}

/** A form a value takes at a place, by which the contract tests compare what a schema takes with
 *  what its writer writes and with what the Legion daemon's decoder takes: the key left out
 *  (`absent`), a JSON null, a string (the `empty string` apart), a number - a whole one above
 *  zero (`number`), `zero`, a `negative` whole one, a `fraction`, or a whole one `past range` of
 *  the integers a JavaScript number holds exactly (and so of Go's int64) - a boolean, an object or
 *  a list. `any` is a schema's taking every probe of every form but `absent`. */
export type Form =
  | "absent"
  | "null"
  | "string"
  | "empty string"
  | "number"
  | "zero"
  | "negative"
  | "fraction"
  | "past range"
  | "boolean"
  | "object"
  | "array"
  | "any";

/** A form a value holds: every form but `absent` and `any`. */
export type ValueForm = Exclude<Form, "absent" | "any">;

/** The fixed values that probe each form a value holds. The Legion daemon's verdicts were made
 *  with the same values (`daemon-verdicts.json`'s `probes`, which the issue events test holds
 *  equal to these). */
export const FORM_PROBES: Readonly<Record<ValueForm, readonly unknown[]>> = {
  null: [null],
  string: ["x", "1", "true"],
  "empty string": [""],
  number: [1, 7],
  zero: [0],
  negative: [-1],
  fraction: [1.5],
  "past range": [1e20],
  boolean: [true, false],
  object: [{}],
  array: [[]],
};

/** The forms a value holds, in the order the problems and the daemon's verdicts list them. */
export const VALUE_FORMS = Object.keys(FORM_PROBES) as readonly ValueForm[];

/** The form a JSON value holds. */
export function formOf(value: unknown): ValueForm {
  if (value === null) return "null";
  if (Array.isArray(value)) return "array";
  switch (typeof value) {
    case "string":
      return value === "" ? "empty string" : "string";
    case "boolean":
      return "boolean";
    case "number":
      if (!Number.isInteger(value)) return "fraction";
      if (!Number.isSafeInteger(value)) return "past range";
      if (value === 0) return "zero";
      return value < 0 ? "negative" : "number";
    default:
      return "object";
  }
}

/** The text of a JSON number, which a string probe of a number place spells. */
const NUMBER_TEXT = /^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$/;

/** The probes a value held at a place adds to the fixed ones: the value itself, its text where it
 *  is a number or a boolean, the number a string spells where it spells one, and a list holding
 *  it. So a coercion or a preprocess that takes the held value in another form is asked of that
 *  form with a value its refinements take (a positive timestamp's text, say), not only with the
 *  fixed probes. The daemon's verdicts add the same probes (`heldProbes` in its
 *  `daemon_verdicts_test.go`). */
export function heldProbes(value: unknown): readonly unknown[] {
  const probes: unknown[] = [value];
  if (typeof value === "number" || typeof value === "boolean") probes.push(String(value));
  if (typeof value === "string" && NUMBER_TEXT.test(value)) probes.push(Number(value));
  probes.push([value]);
  return probes;
}

/** The forms `take` - zod's parse of the place's schema with the probe there - takes, of the
 *  fixed probes and of the probes the values the place holds add; `any` alone when it takes every
 *  fixed probe of every form. */
export function takenForms(take: (probe: unknown) => boolean, held: readonly unknown[]): Set<Form> {
  const taken = new Set<Form>();
  let every = true;
  for (const form of VALUE_FORMS) {
    const took = FORM_PROBES[form].filter(take);
    if (took.length > 0) taken.add(form);
    if (took.length < FORM_PROBES[form].length) every = false;
  }
  if (every) return new Set<Form>(["any"]);
  for (const probe of held.flatMap(heldProbes)) {
    if (take(probe)) taken.add(formOf(probe));
  }
  return taken;
}

/** The set of values a schema reads a value as, through its wrappers, and the schema that
 *  declares it: an enum's or a literal's values, `true` and `false` for a boolean, and every
 *  member's for a union of such sets. `closed` is whether no part of it is a boolean, so that the
 *  set is one the schema's writer closed (an enum, a literal or a union of them). Undefined for any
 *  other schema. It is the one reading of a schema's values both the examples' value coverage and
 *  the closed-set rule read (`example-walk.testing.ts`). */
export function valueSet(
  schema: unknown,
  where: string
):
  | { readonly schema: unknown; readonly values: readonly unknown[]; readonly closed: boolean }
  | undefined {
  const node = schemaNode(schema, where);
  switch (node.kind) {
    case "wrapper":
      return valueSet(node.inner, where);
    case "leaf":
      if (node.closed) return { schema, values: node.values, closed: true };
      return node.json.length === 1 && node.json[0] === "boolean"
        ? { schema, values: [true, false], closed: false }
        : undefined;
    case "union": {
      if (node.options.length === 0) return undefined;
      const values: unknown[] = [];
      let closed = true;
      for (const option of node.options) {
        const set = valueSet(option, where);
        if (set === undefined) return undefined;
        values.push(...set.values);
        closed &&= set.closed;
      }
      return { schema, values, closed };
    }
    default:
      return undefined;
  }
}
