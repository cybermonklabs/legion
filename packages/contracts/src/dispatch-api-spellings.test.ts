import { describe, expect, test } from "bun:test";

import document from "../schemas/dispatch-openapi.json";
import * as contract from "./dispatch-api";

/**
 * A shape the Go route table holds is spelled twice, and this is what keeps the two from
 * drifting apart.
 *
 * The generated module is the one definition of such a shape: it is rendered from the OpenAPI
 * document the server writes, so `Actor`, `Comment`, `Issue` and the rest are that document's
 * reading and not a type written here. The bus needs a reading of its own all the same - a
 * `DispatchEvent` payload is parsed, not served, so a tolerant zod schema carries it - and that
 * schema is written by hand beside the generated type. The two readings are deliberately not
 * the same type: `ActorSchema` is a strict union whose `kind` is `"user" | "session" | …` while
 * the contract's `Actor.kind` is a `string`, because Go's own model says string, and eight
 * shapes declare a field the bus reading takes as optional where the contract requires it. So
 * neither type is assignable to the other, and no assertion over the two types can stand.
 *
 * What must never differ is which fields the two readings know about. A field added to the
 * contract that the schema does not declare is a field the bus drops without a word, which is
 * the drift the one-schema rule exists to refuse; a field declared here that the contract does
 * not have is a reading of something the server never sends. This file asserts that the field
 * sets agree, for every name that has both spellings.
 *
 * The list is read from the two sources rather than written down: every exported `XSchema` of
 * the split whose `X` the OpenAPI document declares with properties. A shape that grows a schema
 * here later, or a schema whose shape the route table takes over, is covered with no edit.
 */

/** The property names a zod schema declares, reading through the wrappers a schema is built
 *  with: `.readonly()` and `.optional()` hold their inner schema, and a union's fields are its
 *  members' together, since a payload may arrive as any one of them. */
function fieldsOf(schema: unknown, seen = new Set<unknown>()): Set<string> | undefined {
  if (schema === null || typeof schema !== "object" || seen.has(schema)) return undefined;
  seen.add(schema);
  const held = "_def" in schema ? schema._def : "def" in schema ? schema.def : undefined;
  if (held === null || typeof held !== "object") return undefined;
  const def: Record<string, unknown> = { ...held };
  const type = def.type ?? def.typeName;
  if (type === "object" || type === "ZodObject") {
    const shape = typeof def.shape === "function" ? def.shape() : def.shape;
    if (shape === null || typeof shape !== "object") return undefined;
    return new Set(Object.keys(shape));
  }
  if (type === "union" || type === "ZodUnion" || type === "discriminated_union") {
    const fields = new Set<string>();
    const options = Array.isArray(def.options) ? def.options : [];
    for (const option of options) {
      for (const field of fieldsOf(option, seen) ?? []) fields.add(field);
    }
    return fields;
  }
  for (const key of ["innerType", "in", "out", "element", "schema"]) {
    const inner = def[key];
    if (inner === undefined) continue;
    const fields = fieldsOf(inner, seen);
    if (fields !== undefined) return fields;
  }
  return undefined;
}

/** The document's own shapes, read once: a declared shape may carry properties or not - a map
 *  shape carries `additionalProperties` instead - so the fields are read where they are there
 *  and the name is passed over where they are not. */
const DECLARED: Record<string, Record<string, unknown>> = document.components.schemas;

/** The property names one declared shape holds, or undefined for a shape that declares none. */
function declaredFields(name: string): string[] | undefined {
  const declared = DECLARED[name];
  const properties = declared === undefined ? undefined : declared.properties;
  if (properties === null || typeof properties !== "object") return undefined;
  return Object.keys(properties).sort();
}

/** Every exported schema of the split, by the name its shape carries. */
const SCHEMAS: Record<string, unknown> = contract;

/** Every name the split carries a schema for and the document declares with properties: one
 *  shape, two spellings. */
const SPELLED_TWICE = Object.entries(SCHEMAS)
  .filter(([name]) => name.endsWith("Schema"))
  .map(([name, schema]) => ({ name: name.slice(0, -"Schema".length), schema }))
  .filter(({ name }) => declaredFields(name) !== undefined)
  .sort((left, right) => left.name.localeCompare(right.name));

/** How many shapes are spelled twice today. A derivation that found none would pass every row
 *  below in silence, so the count is pinned here and moves deliberately: a schema added beside a
 *  generated type raises it, and a shape that stops being the route table's lowers it. */
const SPELLED_TWICE_COUNT = 37;

describe("a shape spelled twice knows the same fields in both spellings", () => {
  test(`${SPELLED_TWICE_COUNT} shapes carry both a schema here and a type the document declares`, () => {
    expect(SPELLED_TWICE.map(({ name }) => name)).toHaveLength(SPELLED_TWICE_COUNT);
  });

  for (const { name, schema } of SPELLED_TWICE) {
    test(name, () => {
      const declared = declaredFields(name) ?? [];
      const written = fieldsOf(schema);
      expect(written, `${name}Schema's fields are readable`).toBeDefined();
      expect(
        [...(written ?? [])].sort(),
        `${name}Schema and the contract's ${name} declare the same fields: a field on one side ` +
          "alone is either dropped by the bus or read from something the server never sends"
      ).toEqual(declared);
    });
  }
});

/** The object a schema's fields are read from, where it is one: a union's members each have
 *  their own optionality, so there is nothing to compare one field of a union against, and the
 *  comparison below passes such a schema over. `ActorSchema` is the one such schema today. */
function objectOf(schema: unknown, seen = new Set<unknown>()): Record<string, unknown> | undefined {
  if (schema === null || typeof schema !== "object" || seen.has(schema)) return undefined;
  seen.add(schema);
  const held = "_def" in schema ? schema._def : "def" in schema ? schema.def : undefined;
  if (held === null || typeof held !== "object") return undefined;
  const def: Record<string, unknown> = { ...held };
  const type = def.type ?? def.typeName;
  if (type === "object" || type === "ZodObject") {
    const shape = typeof def.shape === "function" ? def.shape() : def.shape;
    return shape === null || typeof shape !== "object" ? undefined : { ...shape };
  }
  for (const key of ["innerType", "in", "out", "element", "schema"]) {
    const inner = def[key];
    if (inner === undefined) continue;
    const object = objectOf(inner, seen);
    if (object !== undefined) return object;
  }
  return undefined;
}

/** Whether a zod field takes an absent value, which is what the document's `required` list
 *  answers on its side: the field is parsed with nothing, and a field that accepts nothing is
 *  optional however it was spelled (`.optional()`, `.nullish()`, a default). */
function takesNothing(field: unknown): boolean {
  if (field === null || typeof field !== "object" || !("safeParse" in field)) return false;
  const parse = field.safeParse;
  if (typeof parse !== "function") return false;
  const answer: unknown = parse.call(field, undefined);
  return answer !== null && typeof answer === "object" && "success" in answer
    ? answer.success === true
    : false;
}

/** The fields of one shape whose two spellings disagree about absence, each side's reading
 *  named. */
function optionalityDifferences(
  name: string,
  schema: unknown
): { tolerated: string[]; refused: string[] } {
  const object = objectOf(schema);
  const declared = DECLARED[name];
  const required = new Set<string>(Array.isArray(declared?.required) ? declared.required : []);
  const tolerated: string[] = [];
  const refused: string[] = [];
  for (const [field, written] of Object.entries(object ?? {})) {
    const optionalHere = takesNothing(written);
    if (optionalHere && required.has(field)) tolerated.push(`${name}.${field}`);
    if (!optionalHere && !required.has(field)) refused.push(`${name}.${field}`);
  }
  return { tolerated, refused };
}

const DIFFERENCES = SPELLED_TWICE.map(({ name, schema }) => optionalityDifferences(name, schema));

/**
 * How many fields the contract requires and the bus reading takes as optional. That direction is
 * the one the bus must tolerate: a field added after the bus began carrying a payload is absent
 * from every frame a pod of the build before it appended, and a reading that required it would
 * refuse that frame. The sixteen are `Artifact.ref_key`, `AskApproval.documents`, three of
 * `Comment`, `CommentDelivery.duplicate`, the three document ids of `Issue`, four of
 * `MessageDelivery`, `Project.open_asks` and `gated_steps`, and `RepoProject.renamed_to`.
 *
 * The number is a record of today's count rather than a budget, and it is counted rather than
 * listed so that no hand-kept list of names stands here: a seventeenth shows up in a diff, where
 * a person decides whether it is another frame an older build can send short or a schema
 * somebody loosened by accident.
 *
 * One shape is not reached by this comparison at all: `ActorSchema` is a union, so each member
 * carries its own optionality and there is no one field to compare: the count passes it over.
 */
const TOLERATED_ABSENCES = 16;

describe("a shape spelled twice agrees about which fields may be absent", () => {
  for (const [index, { name, schema }] of SPELLED_TWICE.entries()) {
    test(name, () => {
      const { refused } = DIFFERENCES[index] ?? optionalityDifferences(name, schema);
      expect(
        refused,
        `${name}Schema requires a field the contract takes as optional, so the bus would refuse ` +
          "a frame the server is allowed to send short"
      ).toEqual([]);
    });
  }

  test(`${TOLERATED_ABSENCES} fields the contract requires are optional to the bus`, () => {
    expect(DIFFERENCES.flatMap(({ tolerated }) => tolerated)).toHaveLength(TOLERATED_ABSENCES);
  });
});
