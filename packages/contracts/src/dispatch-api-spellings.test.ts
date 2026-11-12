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
