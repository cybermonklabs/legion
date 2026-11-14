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

/** A field of the route table's own answer that no bus payload carries: a stored text read as
 *  the tree a caller asked for with `Dispatch-Trees: 1`. The document declares those answers
 *  under a `WithTrees` name, so where it declares no plain `X` the schema is paired with
 *  `XWithTrees` instead. One rule sets the tree fields aside, on both sides and for every pair,
 *  because the bus reads the markdown and never the tree: so `body_tree` leaves `MessageWithTrees`
 *  and `question_tree` leaves `AskWithTrees` by the same reading, and a reader never has to learn
 *  why one tree field is inside a count and another outside it. */
const TREE_FIELD = /_tree$/;

/** The name the document declares one schema's shape under: `X` where it declares one, else
 *  `XWithTrees`, else nothing, which is a schema the document has no shape for. Pairing on the
 *  exact name alone compared `AskSchema`, `MessageSchema` and `AskEditSchema` against nothing at
 *  all, since this pull request's own `schemaNames` renamed those three reads. */
function declaredNameOf(name: string): string | undefined {
  if (declaredFields(name) !== undefined) return name;
  const withTrees = `${name}WithTrees`;
  return declaredFields(withTrees) === undefined ? undefined : withTrees;
}

/** One shape's fields as this comparison reads them: whatever the side declares, its tree fields
 *  set aside. */
function comparedFields(fields: Iterable<string>): string[] {
  return [...fields].filter((field) => !TREE_FIELD.test(field)).sort();
}

/** Every name the split carries a schema for and the document declares a shape with properties
 *  for: one shape, two spellings. */
const SPELLED_TWICE = Object.entries(SCHEMAS)
  .filter(([name]) => name.endsWith("Schema"))
  .map(([name, schema]) => ({ name: name.slice(0, -"Schema".length), schema }))
  .map(({ name, schema }) => ({ name, schema, declaredName: declaredNameOf(name) }))
  .filter((pair): pair is { name: string; schema: unknown; declaredName: string } => {
    return pair.declaredName !== undefined;
  })
  .sort((left, right) => left.name.localeCompare(right.name));

/** How many shapes are spelled twice today. A derivation that found none would pass every row
 *  below in silence, so the count is pinned here and moves deliberately: a schema added beside a
 *  generated type raises it, and a shape that stops being the route table's lowers it. */
const SPELLED_TWICE_COUNT = 39;

/** The fields one spelling of a pair knows and the other does not, each named by its shape and
 *  by the side that knows it: a count that mixed the two directions would pass a swap across
 *  them as readily as within one, so the printed set says which way each difference runs. */
function fieldDifferences(name: string, declaredName: string, schema: unknown): string[] {
  const written = comparedFields(fieldsOf(schema) ?? []);
  const declared = comparedFields(declaredFields(declaredName) ?? []);
  return [
    ...written.filter((field) => !declared.includes(field)).map((f) => `${name}.${f} (schema)`),
    ...declared.filter((field) => !written.includes(field)).map((f) => `${name}.${f} (document)`),
  ];
}

const FIELD_DIFFERENCES = SPELLED_TWICE.map(({ name, declaredName, schema }) =>
  fieldDifferences(name, declaredName, schema)
);

/** The one pair whose two spellings do not agree, and whose differences the counted rules carry.
 *  A row cannot both assert that a pair agrees and excuse a difference it has, and a row that
 *  stepped aside whenever it found one would never red at all, so the pair is named here and
 *  every other pair keeps a row that reds under its own name. */
const COUNTED_PAIR = "Ask";

/**
 * How many fields of a paired shape one spelling knows and the other does not. All four are
 * `Ask`'s, and they are not drift: `AskSchema` is the reading of several ask answers at once, so
 * it declares `issue`, `document` and `last_reply`, which the document carries on the inbox and
 * open-ask rows rather than on the ask row, and it leaves out `waiting_on_person`, which the
 * server resolves at read time and writes on no payload.
 *
 * It is a count rather than a list for the reason the counts below are, and it carries the same
 * edge: a number is blind to a swap, so one field excused here and another drifting in passes it,
 * and the diff that makes the swap is where a person sees it rather than this test. A failing
 * count prints the set it measured, each name with the side that knows it, so the red names the
 * shapes, the fields and the direction even though what it asserts is a number.
 */
const ONE_SIDED_FIELDS = 4;

describe("a shape spelled twice knows the same fields in both spellings", () => {
  test(`${SPELLED_TWICE_COUNT} shapes carry both a schema here and a shape the document declares`, () => {
    // The other counts print their sets to say what moved; this one prints to say which failure
    // it is. A red here is either a schema leaving the split or the pairing ceasing to match a
    // name, and only the list of what it found separates the two.
    const paired = SPELLED_TWICE.map(({ name }) => name);
    expect(paired.length, `the shapes paired: ${paired.join(", ")}`).toBe(SPELLED_TWICE_COUNT);
  });

  for (const { name, declaredName, schema } of SPELLED_TWICE) {
    if (name === COUNTED_PAIR) continue;
    test(name, () => {
      const written = fieldsOf(schema);
      expect(written, `${name}Schema's fields are readable`).toBeDefined();
      expect(
        comparedFields(written ?? []),
        `${name}Schema and the contract's ${declaredName} declare the same fields: a field on ` +
          "one side alone is either dropped by the bus or read from something the server never sends"
      ).toEqual(comparedFields(declaredFields(declaredName) ?? []));
    });
  }

  test(`${ONE_SIDED_FIELDS} fields are known to one spelling alone`, () => {
    const oneSided = FIELD_DIFFERENCES.flat();
    expect(oneSided.length, `the fields measured on one side alone: ${oneSided.join(", ")}`).toBe(
      ONE_SIDED_FIELDS
    );
  });
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
 *  named. The document's reading is the shape the pair is declared under, so a `WithTrees`
 *  answer's `required` list is the one read here. */
function optionalityDifferences(
  name: string,
  declaredName: string,
  schema: unknown
): { tolerated: string[]; refused: string[] } {
  const object = objectOf(schema);
  const declared = DECLARED[declaredName];
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

const DIFFERENCES = SPELLED_TWICE.map(({ name, declaredName, schema }) =>
  optionalityDifferences(name, declaredName, schema)
);

/**
 * How many fields the contract requires and the bus reading takes as optional. That direction is
 * the one the bus must tolerate: a field added after the bus began carrying a payload is absent
 * from every frame a pod of the build before it appended, and a reading that required it would
 * refuse that frame.
 *
 * The number is a record of today's count rather than a budget, and it is counted rather than
 * listed so that no hand-kept list of names stands here, which is a list nobody re-reads: a
 * field added to it shows up in a diff, where a person decides whether it is another frame an
 * older build can send short or a schema somebody loosened by accident. The count is a count,
 * though, so a swap of one pair for another passes it, and the diff that makes the swap is where
 * that is seen rather than here. A failing count prints the set it measured, so the red names
 * the fields it found.
 *
 * Two limits of these comparisons, which belong beside the ones above, each true of the half it
 * names rather than of the file.
 *
 * `comparedFields` drops every field matching `/_tree$/` from both sides of the **field-name**
 * comparison, so a tree field one spelling declares and the other does not, or one typed
 * differently on the two, goes unseen there. The optionality rules read the zod schema's own
 * fields and drop nothing, so a tree field the bus requires where the document declares it
 * optional, or takes as absent where the document requires it, still reds them. Measured both
 * ways: `ExternalLink.url_tree` added as optional leaves the guard green, and the same field
 * added as required reds the optionality rules.
 *
 * A union is reached by the field half no more than by the optionality half: `fieldsOf` merges a
 * union's members into one set of names, so a field moving from one member of `ActorSchema` to
 * another leaves that merged set unchanged and the field comparison sees nothing, while the
 * optionality half passes the shape over for the reason below. `ActorSchema` is the one union
 * today. This half is read from the two functions rather than driven, unlike the tree-field
 * limit above.
 *
 * One shape is not reached by this comparison at all: `ActorSchema` is a union, so each member
 * carries its own optionality and there is no one field to compare: the count passes it over.
 */
const TOLERATED_ABSENCES = 22;

/**
 * How many fields the bus reading requires and the document declares optional. That is the
 * direction a reading must never take on its own, since it refuses a frame the server may send
 * short, so every shape below carries a per-shape red for it and only a measured exception is
 * counted here.
 *
 * The one today is `Ask.opened_event_id`, and the document is the side that understates it:
 * `model.Ask.OpenedEventID` is a `*int64` with `omitempty`, which is what renders the field
 * optional in the document, while no answer the server builds omits it - the read fails rather
 * than answering an ask with no identity event (`api/asks.go`, `applyOpenedEventIDs`), and the
 * payload builder dereferences it unconditionally (`api/events.go`). So a second field of this
 * class is a Go pointer rendered optional and not a schema somebody tightened, and it shows up in
 * a diff beside this one, where a person reads which of the two it is.
 *
 * dispatch://INFRA-3237 owns that cause and deletes this rule in the same change: it makes the
 * four shapes that declare the field - `AskWithTrees`, `InboxRow`, `IssueAskRow`, `IssueOpenAsk`
 * - require it and proves no response omits it. So this count is a record of a known departure
 * with an owner, not a place to add a second field to.
 */
const REQUIRED_OF_AN_OPTIONAL = 1;

describe("a shape spelled twice agrees about which fields may be absent", () => {
  for (const [index, { name, declaredName, schema }] of SPELLED_TWICE.entries()) {
    if (name === COUNTED_PAIR) continue;
    test(name, () => {
      const { refused } = DIFFERENCES[index] ?? optionalityDifferences(name, declaredName, schema);
      expect(
        refused,
        `${name}Schema requires a field the contract takes as optional, so the bus would refuse ` +
          "a frame the server is allowed to send short"
      ).toEqual([]);
    });
  }

  test(`${TOLERATED_ABSENCES} fields the contract requires are optional to the bus`, () => {
    const tolerated = DIFFERENCES.flatMap((difference) => difference.tolerated);
    expect(tolerated.length, `the fields measured: ${tolerated.join(", ")}`).toBe(
      TOLERATED_ABSENCES
    );
  });

  test(`${REQUIRED_OF_AN_OPTIONAL} fields the bus requires are optional to the contract`, () => {
    const refused = DIFFERENCES.flatMap((difference) => difference.refused);
    expect(refused.length, `the fields measured: ${refused.join(", ")}`).toBe(
      REQUIRED_OF_AN_OPTIONAL
    );
  });
});

/** The two names of the split that are neither the document's alias nor `z.output` of a schema:
 *  the route row written out by hand, with the bus's tolerance taken off it. */
const HAND_COMPOSITIONS = [
  { name: "Ask", declaredName: "AskWithTrees", schema: contract.AskSchema },
  { name: "Message", declaredName: "MessageWithTrees", schema: contract.MessageSchema },
];

/** Whether a zod field reads a closed set of strings where the document declares a plain string:
 *  the enum, the literal and the union of literals a schema writes for a value this release
 *  knows the spellings of. */
function readsAClosedSet(field: unknown, seen = new Set<unknown>()): boolean {
  if (field === null || typeof field !== "object" || seen.has(field)) return false;
  seen.add(field);
  const held = "_def" in field ? field._def : "def" in field ? field.def : undefined;
  if (held === null || typeof held !== "object") return false;
  const def: Record<string, unknown> = { ...held };
  const type = def.type ?? def.typeName;
  if (type === "enum" || type === "ZodEnum" || type === "literal" || type === "ZodLiteral") {
    return true;
  }
  if (type === "union" || type === "ZodUnion") {
    const options = Array.isArray(def.options) ? def.options : [];
    return options.length > 0 && options.every((option) => readsAClosedSet(option, seen));
  }
  for (const key of ["innerType", "in", "out", "element", "schema"]) {
    if (def[key] !== undefined && readsAClosedSet(def[key], seen)) return true;
  }
  return false;
}

/** The fields of one hand composition the bus reads differently from the document, each named by
 *  the mechanism that makes it differ. */
function handCompositionDifferences(name: string, declaredName: string, schema: unknown): string[] {
  const object = objectOf(schema) ?? {};
  const declared = DECLARED[declaredName];
  const properties: Record<string, unknown> =
    declared !== undefined &&
    typeof declared.properties === "object" &&
    declared.properties !== null
      ? (declared.properties as Record<string, unknown>)
      : {};
  const required = new Set<string>(Array.isArray(declared?.required) ? declared.required : []);
  const differences: string[] = [];
  for (const [field, written] of Object.entries(object)) {
    if (takesNothing(written) && required.has(field)) {
      differences.push(`${name}.${field} (the bus tolerates its absence)`);
      continue;
    }
    const property = properties[field];
    const declaresAnySpelling =
      property !== null &&
      typeof property === "object" &&
      (property as Record<string, unknown>).type === "string" &&
      (property as Record<string, unknown>).enum === undefined;
    if (declaresAnySpelling && readsAClosedSet(written)) {
      differences.push(`${name}.${field} (the bus reads a closed set)`);
    }
  }
  return differences.sort();
}

const HAND_COMPOSITION_DIFFERENCES = HAND_COMPOSITIONS.flatMap(({ name, declaredName, schema }) =>
  handCompositionDifferences(name, declaredName, schema)
);

/**
 * How many fields of `Ask` and `Message` the bus reads differently from the shape the document
 * declares. Those two are the only names of this split written out by hand - `Omit<XWithTrees,
 * …> & {…}` beside an exact `XSchema` - and nothing else here watches them, since `Ask` is the
 * pair the counted rules carry and has no per-shape row.
 *
 * They are not a careless second spelling. Each is the route row with the bus's tolerance taken
 * off it, and the fields below are that tolerance: a field the bus reads as absent where the
 * document requires it, and a field whose spellings the bus closes to the ones this release
 * knows where the document says a plain string. Deriving either type from its schema would push
 * both into the type the routes answer with, so a consumer would handle an absence no route
 * sends and a value narrowed to today's literals, which is what the generated contract exists to
 * refuse. Measured with `tsc` over both directions, field by field: `z.output` of the schema is
 * not assignable to `Ask` in four fields and `Ask` not assignable to it in twelve, and `Message`
 * differs in two each way. The count below is of the fields themselves rather than of those two
 * directions, so it is nine where the type comparison reads four and twelve.
 *
 * The number is today's count rather than a budget, and a failing count prints the set it
 * measured with the mechanism beside each name, so a field joining or leaving that set is read
 * in a diff by a person rather than asserted here.
 */
const HAND_COMPOSITION_FIELDS = 9;

describe("a row written out by hand carries the tolerance it was written for", () => {
  test(`${HAND_COMPOSITION_FIELDS} fields of Ask and Message are read differently by the bus`, () => {
    expect(
      HAND_COMPOSITION_DIFFERENCES.length,
      `the fields measured: ${HAND_COMPOSITION_DIFFERENCES.join(", ")}`
    ).toBe(HAND_COMPOSITION_FIELDS);
  });
});
