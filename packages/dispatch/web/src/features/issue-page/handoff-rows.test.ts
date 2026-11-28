import { expect, spyOn, test } from "bun:test";

import { handoffFields } from "../conversation/conversation-model";
import capturedBodies from "./fixtures/handoff-bodies.json";
import { type HandoffRow, handoffRows, jsonText } from "./handoff-rows";

// The rows the history draws of a handoff's body (`handoff-rows.ts`): the rows today's
// Conversation card reads of it (`handoffFields`), each list or object as the value parsed once
// rather than as indented text, and the JSON text of a value past the bound the history draws
// as lists and keys, written with no recursion.

/** A handoff's body whose one field `f` is `depth` lists, the innermost holding 1. */
function nestedBody(depth: number): string {
  return `{"f":${"[".repeat(depth)}1${"]".repeat(depth)}}`;
}

/** `handoffFields`' rows of `body` as `handoffRows` gives them, each but its `entry`: a list's or
 *  an object's indented text parsed back to the value it was written from. */
function conversationRows(body: string): unknown[] | undefined {
  return handoffFields(body)?.map(({ shape, text, ...row }) =>
    shape === "json" ? { ...row, shape, value: JSON.parse(text) } : { ...row, shape, text }
  );
}

/** `rows` each without its `entry`, which `handoffFields` does not give. */
function withoutEntries(rows: readonly HandoffRow[] | undefined): unknown[] | undefined {
  return rows?.map(({ entry: _entry, ...row }) => row);
}

/** Values `JSON.parse` gives, each with what it shows: every kind of string escape, numbers
 *  `JSON.stringify` writes in another form than they were read in, lists and objects empty and
 *  nested, and the order of an object's keys. */
const VALUES: readonly (readonly [string, unknown])[] = [
  ["quotes and backslashes", 'say "now" \\ and \\" again'],
  ["control characters", "a\u0000b\u0001c\u001fd\n\t\r\b\f"],
  ["U+2028 and U+2029", "line\u2028paragraph\u2029end"],
  ["non-ASCII", "naïve – 日本語 – 😀"],
  ["a lone surrogate", JSON.parse('"\\ud800 and \\udfff"')],
  ["an empty string", ""],
  ["0", 0],
  ["-0", -0],
  ["1e20", 1e20],
  ["1e21", 1e21],
  ["1.5", 1.5],
  ["-1.25e-7", -1.25e-7],
  ["null", null],
  ["true", true],
  ["false", false],
  ["an empty list", []],
  ["an empty object", {}],
  ["nested lists and objects", JSON.parse('[1, [2, [3, {}]], {"a": [], "b": {"c": null}}, []]')],
  ["a key __proto__ as JSON.parse writes it", JSON.parse('{"__proto__": {"x": [1]}, "y": 2}')],
  ["keys in their order, integer keys first", JSON.parse('{"b": 1, "2": 2, "a": 3, "1": 4}')],
  ["keys that need escapes", JSON.parse('{"a\\"b": 1, "c\\\\d": 2, "\\u2028": 3, "": 4}')],
  ["a list of objects", JSON.parse('[{"a": 1, "b": "two"}, {"c": [true, false, null]}]')],
];

test("jsonText writes what JSON.stringify writes, with no indent", () => {
  for (const [what, value] of VALUES) {
    expect(jsonText(value), what).toBe(JSON.stringify(value));
  }
});

// `JSON.stringify` recurses once a level, so it is called on a string, a number, a boolean or
// null alone; a list or an object is written by the loop, its keys through `JSON.stringify` so
// their escapes are its own.
test("jsonText gives JSON.stringify no list and no object", () => {
  const stringify = spyOn(JSON, "stringify");
  try {
    for (const [, value] of VALUES) jsonText(value);
    const given = stringify.mock.calls.map(([value]) => value);
    expect(given.length).toBeGreaterThan(0);
    expect(given.filter((value) => value !== null && typeof value === "object")).toEqual([]);
  } finally {
    stringify.mockRestore();
  }
});

// A walk that recursed once a level would overflow V8's call stack from about 4,500 levels; the
// loop's own stack is a list on the heap. The server stores no body nested past 10,000 levels
// (its JSON check refuses one), so 65,000 is far past any value the page reads, and this value's
// text, 260,001 characters, stays under the server's 262,144-byte cap on a body.
test("jsonText writes a value nested 65,000 deep", () => {
  const depth = 65_000;
  let value: unknown = 1;
  for (let level = 0; level < depth; level += 1) value = level % 2 === 0 ? [value] : { k: value };
  const expected = `${'{"k":['.repeat(depth / 2)}1${"]}".repeat(depth / 2)}`;
  const text = jsonText(value);
  expect(text.length).toBe(expected.length);
  expect(text === expected, "the value nested 65,000 deep is not written as its JSON").toBe(true);
});

test("handoffRows gives the rows handoffFields gives for each captured body", () => {
  expect(capturedBodies.length, "the fixture holds the captured shapes").toBeGreaterThan(5);
  for (const captured of capturedBodies) {
    expect(withoutEntries(handoffRows(captured.body)), `handoff ${captured.id}`).toEqual(
      conversationRows(captured.body)
    );
  }
});

// A `proof` list, one row for each entry of whatever kind, or none for an empty one; a `proof`
// that is no list; a key with `/` and `~`, whose pointer takes the escapes `~1` and `~0`; a
// field named `proof.0` beside the first entry of `proof`; a key `__proto__`; integer keys; and
// bodies that are no JSON object, which have no rows.
const EDGE_BODIES: readonly string[] = [
  '{"summary": "s", "proof": ["a", {"command": "bun test", "exit": 0}, [2], 3, null, true]}',
  '{"proof": "see CI"}',
  '{"proof": [], "summary": "nothing proved"}',
  '{"proof": {"first": "a"}}',
  '{"a/b~c": "slashed", "proof.0": "x", "proof": ["y"]}',
  '{"__proto__": {"x": 1}, "y": [1]}',
  '{"b": "two", "2": [2], "a": {}, "1": null}',
  "{}",
  nestedBody(20),
  "[1, 2]",
  '"a string"',
  "null",
  "3",
  "{not json",
  "",
];

test("handoffRows gives the rows handoffFields gives for each edge of a body", () => {
  for (const body of EDGE_BODIES) {
    expect(withoutEntries(handoffRows(body)), body).toEqual(conversationRows(body));
  }
});

// Each entry of a `proof` list is numbered from 1, and no other row is: not a field, and not a
// `proof` that is no list.
test("handoffRows numbers each entry of a proof list", () => {
  expect(
    handoffRows('{"summary": "s", "proof": ["a", {"b": 1}], "after": 1}')?.map(
      ({ entry, label }) => [label, entry]
    )
  ).toEqual([
    ["summary", undefined],
    ["proof 1", 1],
    ["proof 2", 2],
    ["after", undefined],
  ]);
  expect(handoffRows('{"proof": "see CI"}')?.map(({ entry }) => entry)).toEqual([undefined]);
});

// A list or an object is the value parsed once, and carries no text: the history draws it, and
// its levels past the bound, from that value.
test("handoffRows gives a list or an object as its value, with no text", () => {
  const rows = handoffRows('{"files": ["a.ts"], "ci": {"status": "green"}}');
  expect(rows).toEqual([
    {
      entry: undefined,
      field: "files",
      label: "files",
      path: "/files",
      shape: "json",
      value: ["a.ts"],
    },
    {
      entry: undefined,
      field: "ci",
      label: "ci",
      path: "/ci",
      shape: "json",
      value: { status: "green" },
    },
  ]);
  expect(rows?.some((row) => Object.hasOwn(row, "text"))).toBe(false);
});

// The most the server takes, 10,000 levels counting the body's own object: the body is parsed
// once, and no list or object of it is written as text, where `JSON.stringify` of the field would
// throw the call stack's size in Chrome's engine.
test("handoffRows reads a body nested as deep as the server takes, and writes none of it as text", () => {
  const stringify = spyOn(JSON, "stringify");
  try {
    const rows = handoffRows(nestedBody(9_999));
    expect(rows?.map(({ field, shape }) => [field, shape])).toEqual([["f", "json"]]);
    const given = stringify.mock.calls.map(([value]) => value);
    expect(given.filter((value) => value !== null && typeof value === "object")).toEqual([]);
  } finally {
    stringify.mockRestore();
  }
});
