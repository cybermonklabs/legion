// A word typed at the start of a block, in each state the editor's build passes through and in
// a block the reader has just made, read back from the document the server stores.
//
// The editor is built a batch of blocks at a time and the binding adopts what the batches made
// (`packages/proof-editor/src/progressive-build.ts`, dispatch://INFRA-341 R11), so a reader's
// keystroke lands in one of four states: before the first batch, while the batches are still
// being placed, across the hand-over from the batches' own write path to the binding, and in
// the finished editor after it. Three rows cover them, the last two states in one row, since
// a word that crosses the hand-over is what proves either. Each row types one character at a
// time at the start of a block - the position a lost character was meant for - from a caret
// placed once and never put back, which is what a reader relies on.
//
// The fourth row is a block of the reader's own rather than a state of the build: the Enter
// that makes a block and the word typed into it from there, which is where the browser itself
// can leave their caret in front of the first character
// (`packages/proof-editor/src/caret-after-typing.ts`).
//
// Each row reads the markdown the server holds and nothing else: the store is what the reader
// loses. The build's own failure showed the right word in the editor and the wrong one there -
// the build moved the editor's own selection, ProseMirror wrote that selection over the
// reader's caret, and the characters after it went in at an offset the reader never asked for,
// one of them behind their own word and the rest at the end of the block the build had just
// finished (`offline-edits.e2e.ts`'s read-only admission row, CI run 42485503469). That row
// failed again with the fix for it in, and this time the editor held the wrong word too, as
// the run's own trace shows: `urvives a read-only admissions`, the caret the browser left in
// the block the reader had just made, which is the fourth row's subject (CI run 42504071017).
import { expect, test } from "@playwright/test";

import { createIssue, createProject, getArtifactText } from "./api";
import { documentEditor, documentTransport } from "./editor";
import { throttleTheCpu } from "./large-documents-page";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** Every fact these rows poll for lands in milliseconds once the write is on its way. */
const PROBE = { intervals: [50, 50, 100, 200, 500] };

/** How long a throttled build of this document may take before a wait on it reads as a hang. */
const BUILD_TIMEOUT_MS = 120_000;

/** The paragraph at `index` of the seeded document, each with words of its own so a row can
 *  name one and the markdown it is in can be compared whole. */
function paragraph(index: number): string {
  return `Paragraph ${index} of the ledger.`;
}

/** Blocks enough that the build takes several batches under the throttle, so a character typed
 *  a frame has batches landing around it: the first batch takes eight blocks and each batch
 *  after it is sized by what the one before cost. */
const BLOCKS = 120;

const SPEC = `${Array.from({ length: BLOCKS }, (_, index) => paragraph(index)).join("\n\n")}\n`;

/** Where the build had got to when a character was typed, as the page can tell from the DOM.
 *  `waiting` is before the first batch: the editor's root is in the page from the moment it is
 *  constructed, hidden and holding nothing of the document, and takes no edit until its first
 *  batch lands in it, so no character is ever recorded there. `building` is from that batch
 *  until the whole document is in the editor and the host has swapped its own draw for it,
 *  which is when the editor is given its name (`web/src/features/doc/editor.ts`). `named` is
 *  after that. */
type BuildState = "building" | "named" | "waiting";

declare global {
  interface Window {
    /** The state the build was in as each character the page typed went in, in order. */
    __buildTyping?: BuildState[];
    /** Why the page stopped typing, where something refused it. */
    __buildTypingRefused?: string;
    /** Whether the page ever found the editor standing with no batch in it, which is what
     *  makes the pre-batch row's claim mean anything. */
    __buildTypingSawPreBatch?: boolean;
  }
}

/**
 * Has the page itself type the first `chars` characters of `word` at the start of the first
 * block holding `quote`, one character a frame, from the earliest frame the editor takes one.
 *
 * The typing is the page's own, not this side's: which state of the build a character lands in
 * is what these rows are about, and a `page.keyboard` press is a round trip's business. Each
 * character takes the editor's own path - the caret is a DOM Range plus the `selectionchange`
 * ProseMirror's DOMObserver reads, and the character is `execCommand("insertText")`, whose DOM
 * mutation that observer picks up. The caret is placed once, before the first character: every
 * character after it goes where the editor says the caret now is, which is the thing a reader
 * relies on and the thing that broke.
 */
async function theReaderTypes(
  page: Parameters<typeof documentEditor>[0],
  plan: { chars: number; quote: string; word: string }
): Promise<void> {
  await page.addInitScript((typing: { chars: number; quote: string; word: string }) => {
    const typed: BuildState[] = [];
    window.__buildTyping = typed;
    /** Collapses the caret before the first `quote` among the text a caret can enter, the way
     *  a click there does. */
    const caretBefore = (editor: HTMLElement, quote: string): void => {
      const runs: { node: Node; start: number; end: number }[] = [];
      const walker = document.createTreeWalker(editor, NodeFilter.SHOW_TEXT);
      let text = "";
      for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
        if (node.parentElement?.isContentEditable !== true) continue;
        const start = text.length;
        text += node.textContent ?? "";
        runs.push({ end: text.length, node, start });
      }
      const at = text.indexOf(quote);
      const run = runs.find((each) => each.start <= at && at < each.end);
      if (at < 0 || run === undefined) {
        throw new Error(`the editor holds no ${JSON.stringify(quote)}`);
      }
      const range = document.createRange();
      range.setStart(run.node, at - run.start);
      range.collapse(true);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      document.dispatchEvent(new Event("selectionchange"));
    };
    let placed = false;
    const step = (): void => {
      if (typed.length >= typing.chars) return;
      // The editor the batches are filling wears no name of its own; once it has one, it is
      // the same element under both selectors.
      const live = document.querySelector('.proof-editor[data-write-path="live"]');
      const at: BuildState =
        document.querySelector('[role="textbox"][aria-label="Document editor"]') !== null
          ? "named"
          : live === null
            ? "waiting"
            : "building";
      if (at === "waiting") {
        if (document.querySelector(".proof-editor") !== null) {
          window.__buildTypingSawPreBatch = true;
        }
        requestAnimationFrame(step);
        return;
      }
      const editor = document.querySelector<HTMLElement>('.proof-editor [contenteditable="true"]');
      if (editor === null || !(editor.textContent ?? "").includes(typing.quote)) {
        requestAnimationFrame(step);
        return;
      }
      try {
        editor.focus();
        if (!placed) {
          caretBefore(editor, typing.quote);
          placed = true;
        }
        document.execCommand("insertText", false, typing.word[typed.length] ?? "");
      } catch (error) {
        window.__buildTypingRefused = String(error);
        return;
      }
      typed.push(at);
      requestAnimationFrame(step);
    };
    requestAnimationFrame(step);
  }, plan);
}

/** The state of the build as each of the `chars` characters the page was given went in, once
 *  it has typed them all. */
async function whatTheReaderTyped(
  page: Parameters<typeof documentEditor>[0],
  chars: number
): Promise<BuildState[]> {
  await expect
    .poll(() => page.evaluate(() => window.__buildTypingRefused ?? window.__buildTyping?.length), {
      ...PROBE,
      message: "the page never finished typing the word into the editor",
      timeout: BUILD_TIMEOUT_MS,
    })
    .toBe(chars);
  return (await page.evaluate(() => window.__buildTyping)) ?? [];
}

/** Opens the issue's spec with the document's socket held, so the editor is constructed and
 *  standing with no batch in it, and lets the socket through once the reader's own draw of the
 *  document is on the screen: from there the build runs under the throttle. */
async function openWithTheBuildHeld(
  page: Parameters<typeof documentEditor>[0],
  issueKey: string
): Promise<void> {
  const transport = await documentTransport(page, { holding: true });
  await page.goto(`/issues/${issueKey}/spec`);
  await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached({
    timeout: BUILD_TIMEOUT_MS,
  });
  await transport.release();
}

/** The document the server holds, once the word is in it. A read of the store, never of the
 *  editor: the editor showed the word in its right place while the store held it in the wrong
 *  one, which is the failure these rows are for. */
async function theServerHolds(artifactId: string, markdown: string): Promise<void> {
  await expect
    .poll(async () => (await getArtifactText(artifactId)).markdown, {
      ...PROBE,
      timeout: 30_000,
    })
    .toBe(markdown);
}

/** The markdown of the seeded document with `word` at the start of paragraph `index`. */
function withWordAt(index: number, word: string): string {
  return SPEC.replace(paragraph(index), `${word}${paragraph(index)}`);
}

async function makeIssue() {
  await createProject({ key: "BUILD", name: "Build typing" });
  return createIssue({ project: "BUILD", spec: SPEC, title: "Typing into a build" });
}

const WORD = "Pangolin ";

test.beforeEach(async () => {
  await resetDatabase();
});

// The throttle is Chromium's own (`Emulation.setCPUThrottlingRate`), which no other engine the
// suite runs can be given. It times nothing here: it stretches the build so that a frame of
// the page's own falls inside it, which is the state these rows are about.
test.describe(() => {
  test.skip(
    ({ browserName }) => browserName !== "chromium",
    "the build is stretched with Chromium's CPU throttle, which no other engine takes"
  );

  // The reader is at the document before the editor has any of it: the root is constructed and
  // hidden while the socket is still held, and takes no keystroke until its first batch lands.
  // What this row holds the editor to is that a word begun from there is stored whole and in
  // one piece, at the start of the block it was aimed at.
  // SLOW(>=2s): a 120-block document built under 4x CPU throttling, typed into a character a
  // frame.
  test("a word typed at a block's start from before the first batch is stored as typed", async ({
    browser,
  }) => {
    const issue = await makeIssue();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await throttleTheCpu(page);
      await theReaderTypes(page, { chars: WORD.length, quote: paragraph(0), word: WORD });
      await openWithTheBuildHeld(page, issue.key);

      const typed = await whatTheReaderTyped(page, WORD.length);
      expect(
        await page.evaluate(() => window.__buildTypingSawPreBatch),
        "the editor was never found standing with no batch in it, so this run says nothing " +
          "about a word begun before the first one"
      ).toBe(true);
      expect(typed.length).toBe(WORD.length);

      await expect(documentEditor(page)).toBeVisible({ timeout: BUILD_TIMEOUT_MS });
      await theServerHolds(issue.primary_artifact_id, withWordAt(0, WORD));
    } finally {
      await alice.close();
    }
  });

  // SLOW(>=2s): the same throttled build, typed into while its batches land.
  test("a word typed at a block's start while the batches are still being placed is stored as typed", async ({
    browser,
  }) => {
    const issue = await makeIssue();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await throttleTheCpu(page);
      await theReaderTypes(page, { chars: WORD.length, quote: paragraph(0), word: WORD });
      await openWithTheBuildHeld(page, issue.key);

      const typed = await whatTheReaderTyped(page, WORD.length);
      expect(
        typed,
        "the build was over before the word was typed, so this run says nothing about a word " +
          "typed into one"
      ).toContain("building");

      await expect(documentEditor(page)).toBeVisible({ timeout: BUILD_TIMEOUT_MS });
      await theServerHolds(issue.primary_artifact_id, withWordAt(0, WORD));
    } finally {
      await alice.close();
    }
  });

  // The last two states in one row, because a word crossing them is what proves either: the
  // page types the word's first half while the batches are still placing blocks, and the rest
  // goes in after the editor has been handed over to the binding and named - the finished
  // editor - from the caret the reader was left holding. A row that typed into the finished
  // editor alone would prove nothing here: that state was never the broken one, and a word
  // typed there went in where it was asked for before this fix as well.
  // SLOW(>=2s): the same throttled build, with the word's two halves on either side of the
  // hand-over from the batches' own write path to the binding.
  test("a word typed at a block's start across the build's hand-over to the binding, and on into the finished editor, is stored as typed", async ({
    browser,
  }) => {
    const issue = await makeIssue();
    const typedByThePage = 4;
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await throttleTheCpu(page);
      await theReaderTypes(page, { chars: typedByThePage, quote: paragraph(0), word: WORD });
      await openWithTheBuildHeld(page, issue.key);

      const typed = await whatTheReaderTyped(page, typedByThePage);
      expect(
        typed,
        "the build was over before the page typed anything, so this run crosses no hand-over"
      ).toContain("building");

      // The rest of the word goes in from the caret the editor was left holding: the reader
      // never asked for it to move, and the binding took the document over under them in
      // between.
      await expect(documentEditor(page)).toBeVisible({ timeout: BUILD_TIMEOUT_MS });
      await page.keyboard.type(WORD.slice(typedByThePage));
      await theServerHolds(issue.primary_artifact_id, withWordAt(0, WORD));
    } finally {
      await alice.close();
    }
  });
});

/** The seeded document with a block of `word` after paragraph `index`, which is what pressing
 *  Enter at the end of that paragraph and typing the word leaves behind. */
function withABlockAfter(index: number, word: string): string {
  return SPEC.replace(paragraph(index), `${paragraph(index)}\n\n${word}`);
}

/**
 * Has the page press Enter at the end of the block holding `quote` and type `word` into the
 * block that makes, the first character in the same task as the press and the rest a character
 * a frame, from the caret the editor is left holding.
 *
 * The press and the first character are one task on purpose. An empty block is drawn with a
 * placeholder break and the caret in it stands on the block rather than in any text, and
 * Chromium decides what to do with both as it inserts the first character: given a frame in
 * between it replaces the placeholder and leaves the caret after the character, and given none
 * it inserts the text in front of the placeholder it keeps and leaves the caret where it was,
 * in front of the character (`caret-after-typing.ts` in `@legion/proof-editor`). A reader hits
 * that second case by typing straight on from the Enter on a machine with no frame to spare,
 * which this row has the page do every time rather than wait for a run that happens to.
 */
async function theReaderMakesABlockAndTypes(
  page: Parameters<typeof documentEditor>[0],
  plan: { quote: string; word: string }
): Promise<void> {
  await page.evaluate((typing: { quote: string; word: string }) => {
    const editor = document.querySelector<HTMLElement>(
      '[role="textbox"][aria-label="Document editor"]'
    );
    if (editor === null) throw new Error("the page holds no editor");
    const block = [...editor.querySelectorAll<HTMLElement>("p")].find(
      (each) => each.textContent === typing.quote
    );
    const text = block?.firstChild ?? null;
    if (text === null) throw new Error(`the editor holds no block ${JSON.stringify(typing.quote)}`);
    editor.focus();
    const range = document.createRange();
    range.setStart(text, text.textContent?.length ?? 0);
    range.collapse(true);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
    document.dispatchEvent(new Event("selectionchange"));
    editor.dispatchEvent(
      new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key: "Enter", keyCode: 13 })
    );
    document.execCommand("insertText", false, typing.word.slice(0, 1));
    let typed = 1;
    const step = () => {
      if (typed >= typing.word.length) return;
      document.execCommand("insertText", false, typing.word[typed] ?? "");
      typed += 1;
      requestAnimationFrame(step);
    };
    requestAnimationFrame(step);
  }, plan);
}

// A block of the reader's own. Every row above types into a block the document already held;
// this one has the reader make the block - the Enter that ends a paragraph and opens the next -
// and type the word into it from there, which is the one place the editor takes a character the
// browser can leave their caret in front of. The word is the whole of the block, so it is at its
// start as surely as the words above are.
//
// The caret this row is about is Chromium's own, and no other engine the suite runs leaves one
// like it.
// SLOW(>=2s): a 120-block document, loaded and then typed into a character a frame.
test("a word typed into a block the reader has just made is stored as typed", async ({
  browser,
  browserName,
}) => {
  test.skip(
    browserName !== "chromium",
    "the caret a browser leaves in a block it has just drawn is Chromium's own"
  );
  const issue = await makeIssue();
  const alice = await asUser(browser, "alice");
  const word = WORD.trim();
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(page)).toBeVisible({ timeout: BUILD_TIMEOUT_MS });

    await theReaderMakesABlockAndTypes(page, { quote: paragraph(0), word });
    await expect
      .poll(() => documentEditor(page).textContent(), { ...PROBE, timeout: BUILD_TIMEOUT_MS })
      .toContain(word.slice(1));
    await theServerHolds(issue.primary_artifact_id, withABlockAfter(0, word));
  } finally {
    await alice.close();
  }
});
