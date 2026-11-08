import { afterEach, describe, expect, spyOn, test } from "bun:test";
import * as headlessModule from "@legion/proof-editor/headless";

import serverBlockSchema from "../../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import { fixture, type PmdocFixture } from "../../__tests__/pmdoc-fixtures";
import type { BlockSchema } from "../../api/types";
import * as schemaModule from "../doc/schema";
import {
  type MarkdownRendering,
  parseAheadOfTheRender,
  parsesInTheWorker,
  type RawHtml,
  renderWithEngine,
  warmMarkdownRenderer,
} from "./markdown-engine";
import { markdownWorkerThreshold } from "./markdown-worker-threshold";
import { flushTasks, installFakeParseWorker, restoreParseWorker } from "./parse-worker-fixture";

const blockSchema = serverBlockSchema as unknown as BlockSchema;

/** Renders `markdown` through the shared engine as a surface would, under `rawHtml` (`"refuse"`,
 *  as a document takes it, by default), and answers the parsed document's plain text, or
 *  `undefined` where the engine answered nothing for it. */
function parseThroughSharedEngine(
  markdown: string,
  rawHtml: RawHtml = "refuse"
): Promise<string | undefined> {
  const { promise, resolve } = Promise.withResolvers<string | undefined>();
  renderWithEngine(markdown, "space", rawHtml, ({ engine }) => {
    const doc = engine?.parse(markdown, "space", rawHtml);
    resolve(doc === undefined ? undefined : doc.textBetween(0, doc.content.size, " ", " "));
  });
  return promise;
}

/**
 * What `parseThroughSharedEngine` must answer for `entry`, derived from the table's own reading
 * rather than written out again here. The helper joins the document's blocks with a space and
 * writes a space for every leaf; a paragraph's stored reading is that paragraph's own text with
 * runs of whitespace collapsed. For a document whose every block is a paragraph - a list item's
 * and a table cell's paragraph among them - and which holds no inline leaf, the two are the same
 * string, which is why the shapes below are the families' paragraph-only documents. The shapes
 * that hold a heading, a code block or a leaf are read against the table in
 * `fixture-table-readings.test.ts`, where a DOM says what each paragraph reads as.
 */
function wholeText(entry: PmdocFixture): string {
  return entry.reading.paragraphs.map((paragraph) => paragraph.text).join(" ");
}

/** The bodies the parses after a refused one carry: eight documents of the fixture table, one
 *  for each markdown shape this engine must keep reading, and two for the addresses - a list
 *  (`lists`), the tilde-beside-emphasis shapes (`emphasis-beside-a-tilde`), the spelling a
 *  writer's backslash forces (`writer-spells-a-flanked-letter-after-a-backslash`), the
 *  addresses two linkifiers argue over (`email-autolinks-in-a-spec`,
 *  `bare-addresses-each-with-its-own-host`), text shaped like a directive this schema has no
 *  block for (`directive-like-text`), the mark spans a document carries (`marks`) and text
 *  outside ASCII (`unicode`). */
const unrelatedBodies = [
  "lists",
  "emphasis-beside-a-tilde",
  "writer-spells-a-flanked-letter-after-a-backslash",
  "email-autolinks-in-a-spec",
  "bare-addresses-each-with-its-own-host",
  "directive-like-text",
  "marks",
  "unicode",
].map(fixture);

// LEGION-540. A body the schema has no node for throws, and that body falls back to its literal
// text. The throw must stop there: the next bodies through the same engine, on the same page,
// are unrelated text someone else wrote, and they render formatted. Before the headless editor
// dropped its refused parser, the one or two parses after a throw threw too, so a numbered list
// under a raw-HTML or mark-span turn in the live transcript read as `1. **Stop** …`. A span of a
// mark kind this schema does not carry still throws (reference-style links now resolve, so they
// no longer do), so it drives the recovery this test guards; removing the drop-on-throw in
// `@legion/proof-editor`'s headless parser fails the assertions after the refused parse.
test("a parse the engine refuses leaves the next parses of unrelated text intact", async () => {
  const load = spyOn(schemaModule, "loadBlockSchema").mockResolvedValue(blockSchema);
  try {
    const warm = fixture("emphasis");
    expect(await parseThroughSharedEngine(warm.markdown)).toBe(wholeText(warm));
    // A span of a mark kind this schema has no parser for: the parser throws mid-paragraph.
    expect(
      await parseThroughSharedEngine('see <span data-dispatch="bogus">this</span> now')
    ).toBeUndefined();
    for (const entry of unrelatedBodies) {
      expect(await parseThroughSharedEngine(entry.markdown), entry.name).toBe(wholeText(entry));
    }
  } finally {
    load.mockRestore();
  }
});

// The boundary the browser rows measure (`e2e/heavy-bodies.e2e.ts`, whose two shortest rows sit
// one character under it): a body of exactly `markdownWorkerThreshold` characters is parsed
// where every body always was, on the page's own thread, and one character more is parsed in the
// worker (sjawhar/legion#2519).
describe("the parse worker's threshold", () => {
  afterEach(restoreParseWorker);

  test("a body of exactly the threshold parses on the page, one character more in the worker", async () => {
    await warmMarkdownRenderer();
    const worker = installFakeParseWorker();
    // One paragraph of one word, so each body is exactly as long as the count under test.
    const atThreshold = "x".repeat(markdownWorkerThreshold);
    const overThreshold = "x".repeat(markdownWorkerThreshold + 1);

    const onThePage: MarkdownRendering[] = [];
    const cancelOnThePage = renderWithEngine(atThreshold, "space", "refuse", (rendering) => {
      onThePage.push(rendering);
    });
    try {
      // The engine is loaded, so this body is answered in the caller's own commit, parsed, the
      // way every body was before the worker: nothing about it is pending.
      expect(onThePage).toHaveLength(1);
      expect(onThePage[0].pending).toBe(false);
      expect(onThePage[0].engine).toBeDefined();
      expect(onThePage[0].parsed?.textContent).toBe(atThreshold);
      await flushTasks();
      expect(worker.created).toBe(0);
      expect(worker.requests).toHaveLength(0);
    } finally {
      cancelOnThePage();
    }

    const inTheWorker: MarkdownRendering[] = [];
    const cancelInTheWorker = renderWithEngine(overThreshold, "space", "refuse", (rendering) => {
      inTheWorker.push(rendering);
    });
    try {
      // One character more, and the body is answered at once as pending, with no document: the
      // literal text is what a surface paints until the worker answers.
      expect(inTheWorker).toHaveLength(1);
      expect(inTheWorker[0].pending).toBe(true);
      expect(inTheWorker[0].parsed).toBeUndefined();
      expect(inTheWorker[0].engine).toBeDefined();
      await worker.answer(0);
      await flushTasks();
      expect(worker.created).toBe(1);
      expect(worker.requests).toHaveLength(1);
      expect(worker.requests[0].markdown).toBe(overThreshold);
      const answered = inTheWorker.at(-1) as MarkdownRendering;
      expect(inTheWorker.length).toBeGreaterThan(1);
      expect(answered.pending).toBe(false);
      expect(answered.parsed?.textContent).toBe(overThreshold);
    } finally {
      cancelInTheWorker();
    }
  });
});

// The cache names a body's answer by the schema version, both parse policies and the source. Were
// the raw-HTML policy left out of that name, the document's reading of a body holding the
// `<!-- legion: … -->` footer - the footer drawn as the characters its author wrote - would be
// served to the comment that asked for `text`, which draws it as nothing, and that comment would
// show the footer for as long as the answer is kept. The worker's test double reads the
// request's own fields, so only the cache can show this.
describe("the answer cache and the raw-HTML policy", () => {
  afterEach(restoreParseWorker);

  test("a body parsed under one raw-HTML policy is never served to a caller that asked for the other", async () => {
    await warmMarkdownRenderer();
    const worker = installFakeParseWorker();
    const words = "The deploy is held. ".repeat(110).trim();
    const body = `${words}\n\n<!-- legion: {"session":"01HQ8F3K2M4N5P6Q7R8S9T0V","phase":"retro"} -->`;
    expect(parsesInTheWorker(body)).toBe(true);

    const asDocument: MarkdownRendering[] = [];
    const cancelAsDocument = renderWithEngine(body, "space", "refuse", (rendering) => {
      asDocument.push(rendering);
    });
    try {
      await worker.answer(0);
      await flushTasks();
      expect(worker.requests[0].rawHtml).toBe("refuse");
      // A document's policy draws the footer as the text it is, which is what the cache holds.
      expect(asDocument.at(-1)?.pending).toBe(false);
      expect(asDocument.at(-1)?.parsed?.textContent).toBe(words);
      expect(asDocument.at(-1)?.parsed?.toString()).toContain("html");
    } finally {
      cancelAsDocument();
    }

    const asBody: MarkdownRendering[] = [];
    const cancelAsBody = renderWithEngine(body, "space", "text", (rendering) => {
      asBody.push(rendering);
    });
    try {
      // The second policy is a parse of its own: `answer(1)` names a request the engine sends
      // only where the cache told the two apart.
      await worker.answer(1);
      await flushTasks();
      expect(worker.requests).toHaveLength(2);
      expect(worker.requests[1].rawHtml).toBe("text");
      expect(asBody.at(-1)?.pending).toBe(false);
      expect(asBody.at(-1)?.parsed?.textContent).toBe(words);
    } finally {
      cancelAsBody();
    }
  });
});

// What the warm-up is for, beyond the chunk and the schema: a fresh engine's first parse builds
// the parser for the pair of policies it is asked for and takes micromark through V8's
// interpreter for the first time, and on a phone at 4x CPU throttling that first parse was
// 469 ms of the React commit that mounts a document's page, against 9 ms for a parse with one
// behind it (dispatch://INFRA-341, R11). The warm-up pays it, in a task of its own; this row is
// what says the parse is still in it.
describe("the engine's warm-up", () => {
  test("takes a parse through the engine it built, so no surface's first render pays for it", async () => {
    const built = headlessModule.createHeadlessProof;
    const parsed: string[] = [];
    const create = spyOn(headlessModule, "createHeadlessProof").mockImplementation(
      async (options) => {
        const proof = await built(options);
        return {
          ...proof,
          parseMarkdown(markdown, softBreaks, rawHtml) {
            parsed.push(markdown);
            return proof.parseMarkdown(markdown, softBreaks, rawHtml);
          },
        };
      }
    );
    // A schema version of its own, so the warm-up builds an engine rather than being answered
    // with the one an earlier row left behind (`engines`, keyed by the schema's version).
    const load = spyOn(schemaModule, "loadBlockSchema").mockResolvedValue({
      ...blockSchema,
      version: blockSchema.version + 1,
    });
    try {
      await warmMarkdownRenderer();

      expect(parsed).toHaveLength(1);
      // Wide enough to be worth parsing: a heading, a paragraph, a list, a fence and a table.
      expect(parsed[0]).toContain("# A heading");
      expect(parsed[0].split("\n\n").length).toBeGreaterThanOrEqual(5);
    } finally {
      create.mockRestore();
      load.mockRestore();
    }
  });
});

// The screenful a document opens with is under the worker's threshold, so the page parses it
// itself - in the layout phase of the commit that mounts the issue page, which is the one task
// that also renders the whole page. On a phone at 4x CPU throttling that parse was 294-545 ms
// of a task already over the 500 ms a reader feels as a freeze (dispatch://INFRA-341, R11).
// Parsed ahead, from the task the document's text arrives in, the commit reads the answer
// instead of making it.
describe("a body parsed ahead of the render", () => {
  test("is rendered from the answer the page already has, and is not parsed a second time", async () => {
    const built = headlessModule.createHeadlessProof;
    const parsed: string[] = [];
    const create = spyOn(headlessModule, "createHeadlessProof").mockImplementation(
      async (options) => {
        const proof = await built(options);
        return {
          ...proof,
          parseMarkdown(markdown, softBreaks, rawHtml) {
            parsed.push(markdown);
            return proof.parseMarkdown(markdown, softBreaks, rawHtml);
          },
        };
      }
    );
    // A schema version of its own, so this row builds an engine of its own rather than being
    // answered with one an earlier row left behind (`engines`, keyed by the schema's version).
    const load = spyOn(schemaModule, "loadBlockSchema").mockResolvedValue({
      ...blockSchema,
      version: blockSchema.version + 2,
    });
    const body = "# The lease ledger\n\nThe paragraph a reader meets first.\n";
    try {
      await parseAheadOfTheRender(body);
      // The warm-up body and this one: everything the engine has parsed so far.
      const before = parsed.length;
      expect(parsed.at(-1)).toBe(body);

      const renderings: MarkdownRendering[] = [];
      const cancel = renderWithEngine(body, "space", "refuse", (rendering) =>
        renderings.push(rendering)
      );
      try {
        // Rendered in this task, from the answer, with no parse of its own.
        expect(renderings).toHaveLength(1);
        expect(renderings[0]?.pending).toBe(false);
        expect(renderings[0]?.parsed?.textContent).toContain("The paragraph a reader meets first.");
        expect(parsed).toHaveLength(before);
      } finally {
        cancel();
      }
    } finally {
      create.mockRestore();
      load.mockRestore();
    }
  });
});
