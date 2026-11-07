/**
 * Container directives nest at most 32 deep (INFRA-581). The read-only engine draws an opener
 * standing inside 32 open directive containers as the line of text it is, and the default mode
 * refuses it naming its line, as the document service refuses a 33rd nested typed block. Bodies
 * that nest `:::` thousands deep, which froze a reader's tab for about a minute, are read here
 * for what they produce and what they cost: the read-only engine's parse for its text, the
 * default mode's for the line its refusal names, and both for their cost, within
 * `parseCpuBudgetMs` of CPU time and growing no faster than the body does. A row reads each body
 * at its full size and at an eighth of it, in CPU time rather than wall time (tests/cpu-time.ts),
 * because a busy machine stretches wall time without the parse doing any more work.
 */

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type { BlockSchema } from "../src/block-schema.js";
import { createHeadlessProof } from "../src/lib-headless.js";
import {
  expectCostGrowsLinearly,
  fastestCpuTimeOf,
  growthStep,
  parseCpuBudgetMs,
  refusedCostOf,
} from "./cpu-time";

/** The document service's own block schema: its callout takes `block+`, so callouts nest. */
const serverBlockSchema: BlockSchema = JSON.parse(
  readFileSync(
    new URL("../../envoy/internal/dispatch/pmdoc/schema/blocks.json", import.meta.url),
    "utf8"
  )
);

const maxNesting = 32;

/**
 * A body that nests `:::` deeply, as a reader was sent it and as a row measures it.
 *
 * `grow` builds the body at a size of the dimension whose growth costs, which is the openers of
 * both bodies. A row builds the body at `full` and at an eighth of that size, so the two parses
 * differ by their input and by nothing else. This mirrors `HeavyShape` in
 * tests/heavy-bodies.test.ts.
 */
type DirectiveBody = {
  name: string;
  /** The body at `size` of its growing dimension. */
  grow: (size: number) => string;
  /** The size at which the body is the one the reader was sent. */
  full: number;
  /** What a parse of the full body produces in the read-only engine. */
  expectDoc: (doc: ProseMirrorNode) => void;
  /** What the default mode refuses the full body with. */
  refusedInDefaultMode: RegExp;
};

/** The long line's characters for each opener, so `grow` scales the two together. */
const lineCharsPerOpener = 21_000 / 2_000;

/** The two bodies, each at the size a reader was sent it. */
const bodies: DirectiveBody[] = [
  {
    name: "2,000 nested unclosed openers then a long line",
    grow: (size) => ":::a\n".repeat(size) + "x".repeat(Math.round(size * lineCharsPerOpener)),
    full: 2_000,
    expectDoc: (doc) => {
      // The openers past the bound, and the long line, are the text of the 32 paragraphs the
      // bound allows and of the one that holds the rest.
      expect(doc.childCount).toBe(33);
      expect(doc.textContent.length).toBe(30_968);
    },
    refusedInDefaultMode: /line 33\b/,
  },
  {
    name: "1,000 nested openers then 1,000 closing lines",
    grow: (size) => ":::a\n".repeat(size) + ":::\n".repeat(size),
    full: 1_000,
    expectDoc: (doc) => {
      expect(doc.childCount).toBe(35);
      expect(doc.textContent.length).toBe(8_965);
    },
    refusedInDefaultMode: /line 33\b/,
  },
];

/**
 * `depth` callouts nested inside one another and closed, the innermost holding a paragraph. A
 * closing line closes the outermost block whose fence it satisfies, so the fences shorten inwards:
 * level i of `depth` opens with 3 + (depth - i) colons, and the blocks close innermost first, each
 * with its own run. The level-i opener stands on line i.
 */
function nestedCallouts(depth: number): string {
  const lines: string[] = [];
  for (let level = 1; level <= depth; level++) {
    lines.push(`${":".repeat(3 + depth - level)}callout{}`);
  }
  lines.push("a");
  for (let level = depth; level >= 1; level--) lines.push(":".repeat(3 + depth - level));
  return `${lines.join("\n")}\n`;
}

/** How many callouts stand inside one another on `node`'s deepest path. */
function calloutDepth(node: ProseMirrorNode): number {
  let deepest = 0;
  node.forEach((child) => {
    deepest = Math.max(deepest, calloutDepth(child));
  });
  return node.type.name === "callout" ? deepest + 1 : deepest;
}

/** The innermost callout on `node`'s first path of callouts. */
function innermostCallout(node: ProseMirrorNode): ProseMirrorNode | undefined {
  let found: ProseMirrorNode | undefined;
  node.forEach((child) => {
    if (found === undefined && child.type.name === "callout") {
      found = innermostCallout(child) ?? child;
    }
  });
  return found;
}

async function readOnlyProof() {
  return createHeadlessProof({ blockSchema: serverBlockSchema, readOnly: true });
}

async function defaultProof() {
  return createHeadlessProof({ blockSchema: serverBlockSchema });
}

// The `60_000` the four rows below carry replaces bun's own default of 5,000 ms, which is real
// elapsed time and so is decided by the machine's load rather than by the parse. It is a hang
// guard, not a bound on work: on bun 1.4.4 a per-test limit cannot interrupt a synchronous body,
// and is reported once that body returns, so what ends a hang is the `dispatch-ui` job's
// `timeout-minutes: 15` in .github/workflows/ci.yaml. Each of the four costs 9 to 33 ms here
// with the machine quiet, over seven readings of each, so the limit is 1,800 times the dearest.
test("read-only draws the 33rd nested opener and every line after it as text", async () => {
  // Each level's opener is followed by a line of its own, so every line drawn as text can be
  // found by its words; the openers past the bound are left unclosed, as the reported body's were.
  const lines: string[] = [];
  for (let level = 1; level <= maxNesting + 1; level++) {
    lines.push(":::callout{}", `level ${level} words`, "");
  }
  lines.push("after the openers", "", "the last line");
  const doc = (await readOnlyProof()).parseMarkdown(lines.join("\n"));

  expect(calloutDepth(doc)).toBe(maxNesting);
  const innermost = innermostCallout(doc);
  expect(innermost).toBeDefined();
  innermost?.forEach((child) => {
    expect(child.type.name).toBe("paragraph");
  });
  // The 33rd opener, its line, and every line after it stand in the innermost callout as text.
  for (const line of lines.slice(maxNesting * 3).filter((line) => line !== "")) {
    expect(innermost?.textContent).toContain(line);
  }
  for (let level = 1; level <= maxNesting + 1; level++) {
    expect(doc.textContent).toContain(`level ${level} words`);
  }
}, 60_000);

test("read-only nests 32 callouts as 32 typed blocks", async () => {
  const doc = (await readOnlyProof()).parseMarkdown(nestedCallouts(maxNesting));
  expect(calloutDepth(doc)).toBe(maxNesting);
  expect(innermostCallout(doc)?.textContent).toBe("a");
  expect(doc.textContent).not.toContain(":::");
}, 60_000);

test("the default mode nests 32 callouts as 32 typed blocks", async () => {
  const doc = (await defaultProof()).parseMarkdown(nestedCallouts(maxNesting));
  expect(calloutDepth(doc)).toBe(maxNesting);
  expect(innermostCallout(doc)?.textContent).toBe("a");
  expect(doc.textContent).not.toContain(":::");
}, 60_000);

test("the default mode refuses a 33rd nested callout, naming its line", async () => {
  const proof = await defaultProof();
  const markdown = nestedCallouts(maxNesting + 1);
  const parse = () => {
    proof.parseMarkdown(markdown);
  };
  expect(parse).toThrow(/line 33\b/);
  expect(parse).toThrow(/32/);
}, 60_000);

// The `300_000` each row below carries is a hang guard, not the bound on work: on bun 1.4.4 a
// per-test limit cannot interrupt a synchronous body, and is reported once that body returns, so
// what ends a hang is the `dispatch-ui` job's `timeout-minutes: 15` in .github/workflows/ci.yaml.
// `parseCpuBudgetMs` is the bound on work.
for (const body of bodies) {
  const smallInput = body.grow(Math.floor(body.full / growthStep));
  const fullInput = body.grow(body.full);

  test(`read-only parses ${body.name} at a cost proportional to its length`, async () => {
    const proof = await readOnlyProof();
    const smallCost = fastestCpuTimeOf(() => proof.parseMarkdown(smallInput));
    let doc: ProseMirrorNode | undefined;
    const fullCost = fastestCpuTimeOf(() => {
      doc = proof.parseMarkdown(fullInput);
    });
    expect(fullCost, `the full size cost ${Math.round(fullCost)} ms of CPU time`).toBeLessThan(
      parseCpuBudgetMs
    );
    expectCostGrowsLinearly(`the read-only engine's parse of ${body.name}`, smallCost, fullCost);
    if (doc === undefined) throw new Error("the measured parse produced no document");
    body.expectDoc(doc);
  }, 300_000);

  test(`the default mode refuses ${body.name} at a cost proportional to its length`, async () => {
    const proof = await defaultProof();
    const small = refusedCostOf(() => proof.parseMarkdown(smallInput));
    const full = refusedCostOf(() => proof.parseMarkdown(fullInput));
    expect(full.cpuMs, `the full size cost ${Math.round(full.cpuMs)} ms of CPU time`).toBeLessThan(
      parseCpuBudgetMs
    );
    expectCostGrowsLinearly(`the default mode's refusal of ${body.name}`, small.cpuMs, full.cpuMs);
    const refusal = full.cause;
    if (!(refusal instanceof Error)) throw refusal;
    expect(refusal.message).toMatch(body.refusedInDefaultMode);
  }, 300_000);
}
