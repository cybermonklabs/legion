import { readFileSync } from "node:fs";
import { join, resolve } from "node:path";

import * as checkout from "../src/dispatch-api";

/**
 * Whether what a Dispatch of this checkout answers still parses under the strict schemas of `main`,
 * the release a deployed client was built from: a client of `main` reads a comment with
 * `CommentSchema` and keeps an event under `WrittenCommentEventPayloadSchema`, and both refuse a
 * member they do not declare.
 *
 *     bun packages/contracts/scripts/wire-against-main.ts <main's packages/contracts/src> <wire file>
 *
 * The wire file holds one JSON value a line, `{schema, label, value}`, as a test of the server
 * writes it (`NODE_ANCHOR_WIRE` in `internal/dispatch/api/node_anchor_check_test.go`): the name of
 * the schema `value` is parsed with, what the test calls the value, and the value as the server
 * answered it. The schema is read from `dispatch-api.ts` under the first argument, a checkout of
 * `main` whose dependencies are installed. A name `main` does not export is a schema this checkout
 * adds, such as `CommentWithNodeAnchorSchema`, and is read from this checkout's `src/dispatch-api.ts`
 * instead; a name neither exports refuses its line.
 *
 * Every line parsed prints `PASS: wire-against-main <n> ...` and exits 0. Otherwise each refused
 * line is printed with its label, the schema and whose it is, and each issue the schema raised by
 * its path (`anchor.artifact_id`, or `(the value)` for an unrecognized key of the value itself),
 * then a `FAIL:` line, and the run exits 1. A file with no line fails too, since it proves nothing.
 */

/** The part of a zod schema this script reads, so a schema of `main`'s zod and one of this
 *  checkout's are read alike: each package resolves its own copy of the library. */
interface WireSchema {
  safeParse(value: unknown):
    | { readonly success: true }
    | {
        readonly success: false;
        readonly error: {
          readonly issues: readonly {
            readonly path: readonly PropertyKey[];
            readonly message: string;
          }[];
        };
      };
}

/** One line of the wire file, as the test of the server writes it. */
interface WireLine {
  readonly schema: string;
  readonly label: string;
  readonly value: unknown;
}

function isSchema(value: unknown): value is WireSchema {
  return (
    value !== null &&
    typeof value === "object" &&
    "safeParse" in value &&
    typeof value.safeParse === "function"
  );
}

function wireLine(text: string): WireLine | string {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch (error) {
    return `it is no JSON value: ${error instanceof Error ? error.message : String(error)}`;
  }
  if (parsed === null || typeof parsed !== "object") return "it is no JSON object";
  const schema = "schema" in parsed ? parsed.schema : undefined;
  const label = "label" in parsed ? parsed.label : undefined;
  if (typeof schema !== "string" || schema === "") return "it names no schema";
  if (typeof label !== "string") return "it carries no label";
  if (!("value" in parsed)) return "it carries no value";
  return { schema, label, value: parsed.value };
}

const [mainSource, wireFile] = process.argv.slice(2);
if (mainSource === undefined || wireFile === undefined) {
  console.error(
    "usage: bun packages/contracts/scripts/wire-against-main.ts <main's packages/contracts/src> <wire file>"
  );
  process.exit(2);
}

// main's checkout is the first argument, so its module is known only at run time.
const main: Record<string, unknown> = await import(join(resolve(mainSource), "dispatch-api.ts"));
const own: Record<string, unknown> = checkout;

const lines = readFileSync(wireFile, "utf8")
  .split("\n")
  .map((text, index) => ({ number: index + 1, text }))
  .filter(({ text }) => text.trim() !== "");

let refused = 0;
let parsedByMain = 0;
let parsedByCheckout = 0;
const checkoutSchemas = new Set<string>();
for (const { number, text } of lines) {
  const line = wireLine(text);
  if (typeof line === "string") {
    refused++;
    console.log(`REFUSED: line ${number} of ${wireFile}: ${line}`);
    continue;
  }
  const mainSchema = main[line.schema];
  const checkoutSchema = own[line.schema];
  const fromMain = isSchema(mainSchema);
  const schema = fromMain ? mainSchema : isSchema(checkoutSchema) ? checkoutSchema : undefined;
  if (schema === undefined) {
    refused++;
    console.log(
      `REFUSED: line ${number} (${line.label}): neither main nor this checkout exports a schema ${line.schema}`
    );
    continue;
  }
  const answer = schema.safeParse(line.value);
  if (answer.success) {
    if (fromMain) {
      parsedByMain++;
    } else {
      parsedByCheckout++;
      checkoutSchemas.add(line.schema);
    }
    continue;
  }
  refused++;
  console.log(
    `REFUSED: line ${number} (${line.label}) by ${fromMain ? "main's" : "this checkout's"} ${line.schema}:`
  );
  for (const issue of answer.error.issues) {
    const path = issue.path.length === 0 ? "(the value)" : issue.path.map(String).join(".");
    console.log(`  ${path}: ${issue.message}`);
  }
  console.log(`  value: ${JSON.stringify(line.value)}`);
}

if (lines.length === 0) {
  console.log(`FAIL: wire-against-main read no line from ${wireFile}`);
  process.exit(1);
}
if (refused > 0) {
  console.log(`FAIL: wire-against-main refused ${refused} of ${lines.length} lines`);
  process.exit(1);
}
const checkoutPart =
  parsedByCheckout === 0
    ? ""
    : `, ${parsedByCheckout} with this checkout's ${[...checkoutSchemas].sort().join(", ")}`;
console.log(
  `PASS: wire-against-main ${lines.length} lines: ${parsedByMain} with main's schemas${checkoutPart}`
);
