// What a reader does with a large document whose editor is still being built, and the rest of
// acceptance A7.
//
// Three of these rows are the editor a reader has before its last batch: its blocks are on the
// screen and they take edits, which reach the Yjs document through a window onto the blocks
// already built (`packages/proof-editor/src/progressive-build.ts`). A reader types in the first
// block the batches reached, two people write into one document while it fills, and a decision
// the server drew is answered through the shared ask card in the page's own draw, with no
// editor on the page at all.
//
// The other three are the rest of acceptance A7, and each of them reads a build rather than a
// clock: a link to the decision block at the end of a document lands on that block, a reader
// answers that decision, and a comment body of twenty diagrams draws only the diagrams the
// reader has reached. None of those three carries throttling: what they prove is what the page
// does, not how long it takes.
//
// The last row is acceptance A11's reconnect: an edit the reader types into the larger document
// while its socket is down reaches the server through a reconnect whose sync is the whole
// document, which the server sends in frames of at most 64 KiB (R11), and the row reads those
// frames off the socket.
//
// Which document a row seeds is in its title. The three rows about the editor before its last
// batch seed the AHXV6-1-shaped document of `large-documents.ts` or the document whose opening
// block is a decision: what they read is an order, a place or a count, and 406,000 characters
// buy none of them anything. The two rows of A7 that are about the larger document seed the
// larger document, because that is what the acceptance and its check c-a7 name - a block link
// and an answered decision near the end of it - and a build of five times the work is what
// they cost the lane for it (the link row says the measurement).
//
// The numbers R11's targets are read from are in `large-documents-measured.e2e.ts`, and the
// rows beside them that need a build of the larger document to last are in
// `large-documents-build-serves.e2e.ts`, `large-documents-desktop-build.e2e.ts` and
// `large-documents-long-build.e2e.ts`; what stands on the reader's screen while the editor is
// built is in `large-documents-build.e2e.ts`; the draw the server sends, and the reads and the
// chrome the page holds until the first batch, are in `large-documents-first-screen.e2e.ts`.
// What the seven files share is `large-documents-rows.ts`.
import { MessageType } from "@hocuspocus/provider";
import { expect, type Locator, type Page, test } from "@playwright/test";
import { createDecoder, readVarUint } from "lib0/decoding";
import { messageYjsSyncStep1, messageYjsSyncStep2 } from "y-protocols/sync";

import { readField } from "../web/src/features/doc/sync-frame";
import { createComment, createIssue, createProject, getArtifactText, listIssueAsks } from "./api";
import {
  connectedDot,
  documentEditor,
  documentTransport,
  editorBeingBuilt,
  placeCaret,
  selectTextIn,
  waitForPersistedTyping,
} from "./editor";
import {
  commentDiagrams,
  decisionBlockId,
  decisionBlocks,
  diagramComment,
  diagramsIn,
  documentDiagrams,
  documentNames,
  documentTitle,
  largeDocument,
  openingDecisionBlockId,
  openingDecisionDocument,
  proseOpening,
  sectionHeading,
  smallDocument,
} from "./large-documents";
import { throttleTheCpu, watchTheBuildsWindow } from "./large-documents-page";
import {
  agent,
  blocksBuilt,
  type EditMade,
  everythingOwedFinished,
  seedDocument,
} from "./large-documents-rows";
import { openCommentThread, settledScrollTop } from "./mermaid-helpers";
import { expectNoneOf } from "./none-of";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test(`a link to the decision block at the end of ${documentNames.large} lands on that block @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const block = decisionBlockId(decisionBlocks);
  // The larger document, because that is the document the claim is about: A7 and its check
  // c-a7 say the block link lands on a decision block near the end of the LARGER document, and
  // a row over the AHXV6-1-shaped one proves the clause over 81,000 characters rather than the
  // 406,000 the check names (dispatch://INFRA-341, the architect's decision on review round 3:
  // seed the larger document in this row and in the answering row below, with the check left
  // as it is and no deviation recorded). The landing is the same question over either, and what
  // the larger one costs the lane is the seed and the build it then waits for, measured on this
  // pod in the `iphone` project a row at a time: this row 8.1 s over the smaller document and
  // 17.9 s over this one, the answering row below 9.0 s against 22.9 s. The pair costs that
  // leg 23.7 s more than it did, which is 0.4 of a minute, and neither row throttles anything,
  // so nothing of that is a measurement another row would have to be read against.
  const { path } = await seedDocument("specs-large.md", largeDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`${path}#b-${block}`);
    const linked = documentEditor(page).locator(`[data-block-id="${block}"]`);
    // The landing is in two parts: the focus the editor runs as it binds, and the one it runs
    // again once the diagrams around the target have drawn and moved it
    // (`features/doc/refocus-when-settled.ts`). A block behind `contain-intrinsic-size` has a
    // box before it has painted, so the first focus lands on an estimate; the block has to be in
    // the viewport once the page has stopped moving, not only before it.
    //
    // The wait is the larger document's build, which is minutes rather than seconds on a busy
    // machine: 600 s is the wait a build that never ends fails by, not a budget this row spends.
    await expect(linked).toBeInViewport({ timeout: 600_000 });
    await settledScrollTop(page);
    await expect(linked).toBeInViewport();
  } finally {
    await alice.close();
  }
});

// The three rows below are about the editor a reader has before its last batch: its blocks are
// on the screen and they take edits, which reach the Yjs document through a window onto the
// blocks already built (`packages/proof-editor/src/progressive-build.ts`, acceptance A7). The
// first of them needs no editor at all - a decision the server drew is answered through the
// shared ask card in the page's own draw - and the two after it throttle the renderer as the
// measurement does, not to time anything but so the build lasts long enough for the page to
// draw frames while it runs, which is where a reader's edits land.

test("a reader answers a decision block the server drew, with the editor's socket held @phone", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  const { issueKey, path } = await seedDocument("opening-decision.md", openingDecisionDocument);
  // The ask the server indexed the block into, which the card in the draw is named for: the
  // text engine's own decision shell carries no `data-dispatch-ask-block` - that attribute is
  // the editor's node view's alone (`typed-block-drawing.ts`) - so the card is found by the
  // ask's id, which is what every surface names it by.
  const indexed = (await listIssueAsks(issueKey)).find(
    (ask) => ask.block_id === openingDecisionBlockId
  );
  expect(indexed, "the server indexed no ask for the document's decision block").toBeDefined();

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Held open-ended, so no sync ever reaches the page and no editor is ever built: what the
    // reader answers is the page's own draw of the document's text.
    await documentTransport(page, { holding: true });
    await page.goto(path);

    const drawn = page.locator('[aria-label="Document text"]');
    const card = drawn.getByTestId(`ask-${indexed?.id}`);
    await expect(card).toBeVisible({ timeout: 120_000 });
    await expect(documentEditor(page)).toHaveCount(0);
    await expect(editorBeingBuilt(page)).toHaveCount(0);

    await card.getByRole("radio", { name: "The next window" }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect(drawn).toContainText("Answered by alice");
    // Answered with no editor on the page at all, which is the whole of this row.
    await expect(documentEditor(page)).toHaveCount(0);

    // And the server holds it: the issue's ask for that block reads answered.
    await expect
      .poll(
        async () =>
          (await listIssueAsks(issueKey)).find((ask) => ask.block_id === openingDecisionBlockId)
            ?.state,
        {
          intervals: [500],
          message: "the answer never reached the server",
          timeout: 60_000,
        }
      )
      .toBe("answered");
  } finally {
    await alice.close();
  }
});

/** One edit the reader makes in an editor whose build is still running: where to put the
 *  selection, and what to do there. */
interface ReaderEdit {
  /** Where against `quote` the selection goes: over it, or collapsed at either end. */
  readonly edge: "after" | "before" | "whole";
  readonly kind: "backspace" | "enter" | "paste" | "type";
  /** The text the selection is found by, among the text a caret can enter. */
  readonly quote: string;
  /** What a type inserts, or what a paste carries. */
  readonly text?: string;
}

/**
 * Has the page itself make `edits` in the editor, in one frame, as soon as the editor is live
 * and holds every block they name. Install before the navigation; read what it made with
 * `editsMade` once the build is over.
 *
 * The edits are made from a frame of the page rather than driven from here, because where in
 * a build each one lands is what these rows are about and this side cannot say. A locator's
 * click is a task among the page's own: it is served between batches now
 * (`endOfTask` in `packages/proof-editor/src/progressive-build.ts`), but which batch it falls
 * between is a round trip's business, and an edit that landed after the last batch would
 * prove nothing. A frame is where a reader's keystroke lands anyway, and from a frame the row
 * reads how much of the document the editor held as it made each one.
 *
 * Each edit takes the editor's own path, never a shortcut into its state: the selection is a
 * DOM Range plus the `selectionchange` ProseMirror's DOMObserver reads, a type is
 * `execCommand("insertText")`, whose DOM mutation that observer picks up, Enter and Backspace
 * are `keydown` events its keymaps handle, and a paste is the `ClipboardEvent` its clipboard
 * plugin handles, the same event `e2e/editor.ts`'s `paste` dispatches.
 */
async function readerEditsTheBuild(page: Page, edits: readonly ReaderEdit[]): Promise<void> {
  await page.addInitScript((planned: readonly ReaderEdit[]) => {
    const select = (editor: Element, edit: ReaderEdit): void => {
      const runs: { node: Node; start: number; end: number }[] = [];
      const walker = document.createTreeWalker(editor, NodeFilter.SHOW_TEXT);
      let text = "";
      for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
        if (node.parentElement?.isContentEditable !== true) continue;
        const start = text.length;
        text += node.textContent ?? "";
        runs.push({ end: text.length, node, start });
      }
      const at = text.indexOf(edit.quote);
      const first = runs.find((run) => run.start <= at && at < run.end);
      const last = runs.find(
        (run) => run.start < at + edit.quote.length && at + edit.quote.length <= run.end
      );
      if (at < 0 || first === undefined || last === undefined) {
        throw new Error(`the editor holds no ${JSON.stringify(edit.quote)}`);
      }
      const startAt = { node: first.node, offset: at - first.start };
      const endAt = { node: last.node, offset: at + edit.quote.length - last.start };
      const from = edit.edge === "after" ? endAt : startAt;
      const to = edit.edge === "before" ? startAt : endAt;
      const range = document.createRange();
      range.setStart(from.node, from.offset);
      range.setEnd(to.node, to.offset);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      document.dispatchEvent(new Event("selectionchange"));
    };
    const made: EditMade[] = [];
    window.__readerEdits = made;
    const look = () => {
      // One edit a frame, never four in one: an edit of this kind lands in the DOM, and
      // ProseMirror reads it back off the DOM in its own time, so a second edit made before
      // that read is placed against a document the editor has not caught up with yet.
      const edit = planned[made.length];
      if (edit === undefined) return;
      const root = document.querySelector(".proof-editor[data-write-path='live']");
      const editor = root?.querySelector<HTMLElement>('[contenteditable="true"]') ?? null;
      if (document.querySelector('[role="textbox"][aria-label="Document editor"]') !== null) return;
      // Each edit waits for the block it names, which the batches reach in their own time; an
      // edit before it in the list may have changed what the blocks after it read.
      if (root === null || editor === null || !(editor.textContent ?? "").includes(edit.quote)) {
        requestAnimationFrame(look);
        return;
      }
      try {
        editor.focus();
        select(editor, edit);
        const built = root.querySelectorAll("[data-block-id]").length;
        if (edit.kind === "type") {
          document.execCommand("insertText", false, edit.text);
        } else if (edit.kind === "paste") {
          const data = new DataTransfer();
          data.setData("text/plain", edit.text ?? "");
          editor.dispatchEvent(
            new ClipboardEvent("paste", { bubbles: true, cancelable: true, clipboardData: data })
          );
        } else {
          editor.dispatchEvent(
            new KeyboardEvent("keydown", {
              bubbles: true,
              cancelable: true,
              key: edit.kind === "enter" ? "Enter" : "Backspace",
            })
          );
        }
        made.push({
          after: root.querySelectorAll("[data-block-id]").length,
          built,
          what: edit.kind,
        });
      } catch (error) {
        window.__readerEditsRefused = String(error);
        return;
      }
      requestAnimationFrame(look);
    };
    requestAnimationFrame(look);
  }, edits);
}

/** What the page made of the edits it was given, once the build is over. */
async function editsMade(page: Page, planned: readonly ReaderEdit[]): Promise<EditMade[]> {
  const refused = await page.evaluate(() => window.__readerEditsRefused);
  expect(refused, "the reader's edit was refused by the editor it was made in").toBeUndefined();
  const made = (await page.evaluate(() => window.__readerEdits)) ?? [];
  expect(
    made.map((edit) => edit.what),
    "the reader's edits never ran: the editor was open before the page could make them"
  ).toEqual(planned.map((edit) => edit.kind));
  return made;
}

test(`typing in ${documentNames.small}'s first block while it is built reaches the document @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { artifactId, path } = await seedDocument("specs-small.md", smallDocument);
  const before = await getArtifactText(artifactId);
  // What the editor ends up holding is read after the build, not from the server's index: the
  // page counts every block of the editor at whatever depth, which is the same number the
  // reader's edits were recorded against.
  const title = documentTitle(documentDiagrams.small);
  const typing: readonly ReaderEdit[] = [
    { edge: "after", kind: "type", quote: title, text: " Pangolin." },
  ];

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await readerEditsTheBuild(page, typing);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );

    // Throttled here and not before the load: the build this row reads begins with the release
    // below, and the boot and draw above it are setup (`throttleTheCpu`).
    await throttleTheCpu(page);

    await transport.release();
    await expect(documentEditor(page)).toBeVisible({ timeout: 900_000 });
    const whole = await blocksBuilt(page);
    const made = await editsMade(page, typing);
    expect(
      made.map((edit) => edit.built < whole),
      "the build was over before the word was typed, so this run says nothing about an edit " +
        "that crosses one"
    ).toEqual([true]);

    // The server holds the word in the block it was typed in, with every other block of the
    // document exactly as it was.
    await everythingOwedFinished("the word typed into the build");
    const after = await getArtifactText(artifactId);
    expect(after.markdown).toBe(before.markdown.replace(`# ${title}`, `# ${title} Pangolin.`));
  } finally {
    await alice.close();
  }
});

/**
 * Records, in every animation frame of `page` until its editor is named, how much of the
 * document that editor holds and whether it is open, each frame stamped with the wall clock
 * the frame ran at (`performance.timeOrigin` plus `performance.now()`, which is the clock this
 * side's `Date.now()` reads too, so another page's writes and these frames can be put in one
 * order).
 *
 * The reading is the page's own rather than a poll from here, as `framesOfTheBuild` in
 * `large-documents-rows.ts` gives the reason for, and for one more: a `page.evaluate` into a
 * throttled renderer is served between the batches of the build it is asking about, so a row
 * that reads this page four times spends four of those gaps - a reading that costs the window
 * it is reading. A frame costs it nothing, and a frame is the reading a reader gets.
 *
 * Install before the navigation; read what it recorded once the editor is open.
 */
async function framesOfHerBuild(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const frames: { at: number; built: number; open: boolean }[] = [];
    window.__herBuildFrames = frames;
    const look = () => {
      const open =
        document.querySelector('[role="textbox"][aria-label="Document editor"]') !== null;
      frames.push({
        at: performance.timeOrigin + performance.now(),
        built: document.querySelectorAll(".proof-editor [data-block-id]").length,
        open,
      });
      // The last frame worth recording is the first one the finished editor is named in: the
      // row reads where a build was, and after this there is no build.
      if (open) {
        return;
      }
      requestAnimationFrame(look);
    };
    requestAnimationFrame(look);
  });
}

// What the row below needs of a build, and what it throttles the reader's renderer by to get
// it. The window his four writes have to fall inside is her build: from its first batch to the
// naming of her editor. At the measurement's 4x that window is about as long as the writes
// themselves - 471 ms on a GitHub runner, a couple of seconds on this pod - and a row whose
// writes are as long as the window it writes into fails at whichever end the machine moves:
// the run of 2026-11-11 on this pod had her whole build pass by inside his writes (her editor
// held all 1,437 blocks at the last of them) and the run on that runner had none of it begin
// until after them (it held none, run 42988104355).
//
// So the build is made longer than the writes rather than hoped to be. A batch takes the
// blocks it can convert inside its 120 ms budget (`BATCH_BUDGET_MS`), so a renderer four times
// slower again is a build about four times longer, while his writes stay what they are: seven
// round trips into a page that is not throttled, with nothing of hers read between them
// (`framesOfHerBuild`). What the rate costs is the processor the throttled renderer spins
// while it waits, which this pod has two of, so what it bought is read back from the line the
// row prints - her build's two ends and where in them each of his writes fell - rather than
// assumed. Measured here at this rate, his four writes were 3.4 s of a 43 s build, and his
// last of them fell at 262 of her 1,437 blocks.
//
// The rate times nothing: this row reads where in a build each write fell, never how long
// anything took (`throttleTheCpu`).
const aBuildFourWritesFitInside = 16;

test(`two people write in ${documentNames.small} while its editor is built @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { artifactId, path } = await seedDocument("specs-small.md", smallDocument);
  // The editor's own finished count is read after the build (the typing row says why).
  const title = documentTitle(documentDiagrams.small);
  // A word inside a block, Enter to split one, Backspace to join two, and a paste that runs
  // from one block into the next and lands three.
  const reading: readonly ReaderEdit[] = [
    { edge: "after", kind: "type", quote: proseOpening(4), text: " Beaver." },
    { edge: "after", kind: "enter", quote: proseOpening(2) },
    { edge: "before", kind: "backspace", quote: proseOpening(3) },
    {
      edge: "whole",
      kind: "paste",
      quote: `${sectionHeading(6)}${proseOpening(6)}`,
      text: "Otter one\n\nOtter two\n\nOtter three",
    },
  ];

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    // Bob has the document open and his caret in its first block before Alice's socket is let
    // through, so the only thing left for him to do afterwards is press the keys.
    //
    // The two of them get ready side by side, because neither waits on the other: her socket is
    // held from its construction, so no batch of her build runs until the release below, and
    // his page is a reader of this document like any other. One after the other, his whole
    // build stood in front of her load for nothing.
    const bobPage = await bob.newPage();
    const page = await alice.newPage();
    await readerEditsTheBuild(page, reading);
    await framesOfHerBuild(page);
    const build = await watchTheBuildsWindow(page);
    const transport = await documentTransport(page, { holding: true });
    await Promise.all([
      (async () => {
        await bobPage.goto(path);
        await expect(documentEditor(bobPage)).toContainText(title, { timeout: 240_000 });
        await expect(connectedDot(bobPage)).toHaveText("connected");
        await placeCaret(bobPage, "after", title);
      })(),
      (async () => {
        await page.goto(path);
        await expect(
          page.locator('[aria-label="Document text"] [data-block-id]').first()
        ).toBeAttached({ timeout: 240_000 });
      })(),
    ]);

    // Bob writes from inside her build: a word in a block her batches have built, a block
    // inserted inside that built run, and that block taken out of it again over the prose it
    // was inserted above.
    //
    // From her build's own start, never from the moment her socket was let through. What
    // stands between the two is the sync, the apply and the editor's first batch, and that is
    // a wait of the machine's own length: on a GitHub runner it outlasted all four of his
    // writes, so her editor held none of the document at the last of them and the run said
    // nothing about two people writing into one build (run 42988104355). Her page says when
    // its first batch landed, in the task it landed in (`watchTheBuildsWindow`), and his first
    // keystroke follows that.
    //
    // Where in her build each of his writes falls is her page's own frames, paired with the
    // moment each write was made, because nothing else in this row can say that he wrote
    // during one at all: that some frame of hers held part of the document is true of any
    // batched build wherever his keystrokes landed, and he could have written every word of
    // this after her editor opened. The frame taken for a write is the first one at or after
    // it, which is the order that cannot flatter him - a build still running in that frame was
    // running when he wrote, and the blocks it holds are the ones it had reached by then or
    // more.
    //
    // Her frames rather than a reading taken from here after each write. A `page.evaluate` into
    // a renderer this throttled waits for a gap between the batches of the very build it is
    // asking about, and four of them were most of the 19.6 s his writes took in the run of
    // 2026-11-11 on this pod, against a build of 59 s: the reading cost the window it was
    // reading. A frame costs that window nothing, and it is what a reader is shown
    // (`framesOfHerBuild`, and `framesOfTheBuild` in `large-documents-rows.ts` for the same
    // reason).
    const hisWrites: number[] = [];
    const heWrote = (): void => {
      hisWrites.push(Date.now());
    };
    // Throttled here and not before the load: her build begins with the release below, and her
    // boot and her page's own draw above it are setup no reading of this row is taken of
    // (`throttleTheCpu`). The rate covers the whole of the window this row reads.
    await throttleTheCpu(page, aBuildFourWritesFitInside);
    await transport.release();
    const firstBatchMs = await build.firstBatch(300_000);
    // His two words go in as one `Input.insertText` each rather than as one round trip per
    // character: a word typed key by key is ten or sixteen of them, and a round trip on a pod
    // whose other processor is a throttled renderer cost a second apiece in the run of
    // 2026-11-11 - thirty of them were 34 s of a 43 s build, which is her whole build spent
    // inside his four writes. What ProseMirror reads a word by is the DOM mutation its
    // observer picks up, which is the same here as under a keystroke; Enter and Backspace stay
    // key presses, since those two are what its keymaps answer.
    await placeCaret(bobPage, "after", title);
    await bobPage.keyboard.insertText(" Pangolin.");
    heWrote();
    await placeCaret(bobPage, "after", sectionHeading(0));
    await bobPage.keyboard.press("Enter");
    heWrote();
    await bobPage.keyboard.insertText("Otter paragraph.");
    heWrote();
    await selectTextIn(bobPage, documentEditor(bobPage), `Otter paragraph.${proseOpening(0)}`);
    await bobPage.keyboard.press("Backspace");
    heWrote();

    await expect(documentEditor(page)).toBeVisible({ timeout: 900_000 });
    const whole = await blocksBuilt(page);
    // Alice's four edits, each made in a frame of her own build, with part of the document in
    // the editor and the rest still to come.
    const made = await editsMade(page, reading);
    expect(
      made.map((edit) => edit.built < whole),
      "the build was over before every edit was made, so this run says nothing about the edits " +
        "that cross one"
    ).toEqual([true, true, true, true]);
    // And each did what it is there to do, in the blocks the editor was holding at the time: a
    // word changes no count, Enter makes a block, Backspace unmakes one, and three blocks
    // pasted over a selection that runs from one into the next leave one more than there were.
    expect(made.map((edit) => edit.after - edit.built)).toEqual([0, 1, -1, 1]);
    // And Bob's writing crossed that build: her editor had not been named at any of his four
    // writes, and it held part of the document - some of its blocks, and not all of them - at
    // the last of them, so what he wrote he wrote into a document still being built. Each of
    // his writes is read in the first frame of hers at or after it; a write her recorder has
    // no frame after is read in its last, which is the frame her editor was named in, so a
    // write made past the end of her build is refused by the first guard below rather than
    // quietly matched to a frame before it.
    const herFrames = await page.evaluate(() => window.__herBuildFrames ?? []);
    const herClockBegan = await page.evaluate(() => performance.timeOrigin);
    const herBuild = hisWrites.map(
      (at) =>
        herFrames.find((frame) => frame.at >= at) ??
        herFrames.at(-1) ?? { at: Number.NaN, built: 0, open: true }
    );
    const window_ = await build.window();
    console.log(
      `R11 two people write · Alice's build ran from its first batch at ` +
        `${Math.round(firstBatchMs)} ms to her editor at ` +
        `${Math.round(window_.namedMs ?? Number.NaN)} ms, and Bob's four writes were made at ` +
        `${hisWrites.map((at) => Math.round(at - herClockBegan)).join(", ")} ms of it, read in ` +
        `her frames at ${herBuild.map((at) => Math.round(at.at - herClockBegan)).join(", ")} ms ` +
        `with her editor holding ${herBuild
          .map((at) => `${at.built}${at.open ? " open" : ""}`)
          .join(", ")} of ${whole} block(s). Every number is her page's own clock.`
    );
    expect(
      herBuild.map((at) => at.open),
      "Alice's editor was open before Bob had written, so this run says nothing about two " +
        "people writing into one build"
    ).toEqual([false, false, false, false]);
    expect(
      herBuild.at(-1)?.built ?? 0,
      "Alice's editor held none of the document when Bob made his last write: nothing of her " +
        "build had reached the page he was writing into"
    ).toBeGreaterThan(0);
    expect(
      herBuild.at(-1)?.built ?? 0,
      "Alice's editor held the whole document when Bob made his last write, so his writing " +
        "crossed no build"
    ).toBeLessThan(whole);

    // The binding has taken over, and the document the server holds carries every edit of both
    // of them.
    await everythingOwedFinished("the edits made during the build");
    const { markdown } = await getArtifactText(artifactId);
    // What the markdown is read as: the document's own blocks, which is what `\n\n` separates
    // and what each of these edits made or unmade. A heading's own text is read inside it,
    // since remark escapes the stop after a section's number (`## 4\. The block index`).
    const blocks = markdown.split("\n\n");
    // Bob's: the word in the title, and the block he added taken out again over the prose it
    // stood above, which the one block left of the two no longer opens with.
    expect(markdown).toContain(`# ${title} Pangolin.`);
    expect(markdown).not.toContain("Otter paragraph.");
    expect(markdown).not.toContain(proseOpening(0));
    // Alice's: the word inside a block, Enter splitting one into two, Backspace joining a
    // section's prose onto its heading, and a paste that ran from one block into the next and
    // landed three.
    expect(markdown).toContain(`${proseOpening(4)} Beaver.`);
    expect(blocks.filter((block) => block.startsWith(proseOpening(2)))).toEqual([proseOpening(2)]);
    expect(markdown).toContain(`The block index${proseOpening(3)}`);
    expect(blocks).toContain("## Otter one");
    expect(markdown).toContain("Otter two");
    expect(markdown).toContain("Otter three");
  } finally {
    await bob.close();
    await alice.close();
  }
});

test(`a reader answers a decision block in ${documentNames.large} @phone`, async ({ browser }) => {
  test.setTimeout(900_000);
  const block = decisionBlockId(decisionBlocks);
  // The larger document, for the reason the link row at the top of this file gives: A7's check
  // c-a7 names the answered decision near the end of the LARGER document, and this row is the
  // half of it that answers one.
  const { path } = await seedDocument("specs-large.md", largeDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`${path}#b-${block}`);
    const decision = documentEditor(page).locator(`[data-dispatch-ask-block="${block}"]`);
    // The block hosts the Inbox's own ask card once the server has indexed it, which is once
    // the editor this document's build fills has reached that block: minutes over 406,000
    // characters, so the wait below is what a build that never ends fails by.
    const card = decision.locator("article[data-testid^=ask-]");
    await expect(card).toBeVisible({ timeout: 600_000 });

    await card.getByRole("radio", { name: "The next window" }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect(decision).toContainText("Answered by alice");
    await expect(
      decision.locator('ul[aria-label="Options"] li[data-selected="true"]')
    ).toContainText("The next window");
  } finally {
    await alice.close();
  }
});

/** Each mermaid block of `body`, in document order: whether it has drawn (a non-empty shadow
 *  root, `packages/proof-editor/src/mermaid-svg.ts`), whether the page asked for it at all (the
 *  figure a drawing or a refusal leaves in place of the `pre`), and whether it is within one
 *  viewport height of the viewport, which is the margin a diagram draws inside
 *  (`MERMAID_DRAW_MARGIN`). */
function diagramBlocks(
  body: Locator
): Promise<{ asked: boolean; drawn: boolean; near: boolean }[]> {
  return body.evaluate((element) => {
    const margin = window.innerHeight;
    // One entry per source block: a drawn block keeps its `pre` inside the figure's disclosure,
    // so the `pre`s are the blocks however many of them have drawn.
    return Array.from(
      element.querySelectorAll<HTMLPreElement>('pre[data-language="mermaid"]'),
      (pre) => {
        const figure = pre.closest("figure.dispatch-mermaid");
        const box = (figure ?? pre).getBoundingClientRect();
        const preview = figure?.querySelector(".dispatch-mermaid-preview");
        return {
          asked: figure !== null,
          drawn: (preview?.shadowRoot?.childElementCount ?? 0) > 0,
          near: box.top < window.innerHeight + margin && box.bottom > -margin,
        };
      }
    );
  });
}

test(`a comment body of ${commentDiagrams} diagrams draws only the diagrams near the viewport @phone`, async ({
  browser,
}, testInfo) => {
  test.setTimeout(300_000);
  await resetDatabase();
  await createProject({ key: "BIG", name: "Big documents" });
  const issue = await createIssue({ project: "BIG", title: "A body" }, agent);
  const body = diagramComment(commentDiagrams);
  expect(diagramsIn(body)).toBe(commentDiagrams);
  const comment = await createComment(issue.key, { body }, agent);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, testInfo, comment.id, "Section 1 is about");

    // A diagram that is never asked for can only be shown by waiting: the drawn count is read
    // until it has stopped moving, so every block the margin reached has had its answer.
    let drawnBefore = -1;
    await expect
      .poll(
        async () => {
          const drawnNow = (await diagramBlocks(card)).filter((block) => block.drawn).length;
          const settled = drawnNow > 0 && drawnNow === drawnBefore;
          drawnBefore = drawnNow;
          return settled;
        },
        { intervals: [1_000], timeout: 120_000 }
      )
      .toBe(true);

    const blocks = await diagramBlocks(card);
    // Every block the page asked for drew: a refusal would mean the renderer ran for it after
    // all, which is the cost this rule exists to save. Over the body's own diagrams, of which
    // there are as many as it was written with: a reading that found fewer read something other
    // than this body, and "every block that was asked for drew" is then true of what it never
    // measured (dispatch://INFRA-341, the pairing every universal clause of a check takes).
    expectNoneOf(blocks, {
      amongAtLeast: commentDiagrams,
      are: (block) => block.asked !== block.drawn,
      matched: "diagrams the page asked for and left undrawn, or drew without asking for",
      measured: "diagram blocks of the body",
    });
    // The reader has reached some of the diagrams and not the rest.
    const drawn = blocks.filter((block) => block.drawn).length;
    expect(drawn).toBeGreaterThan(0);
    expect(drawn).toBeLessThan(commentDiagrams);
    // Which ones: the margin is one stretch of the body around the reader, so the diagrams that
    // drew are one run of neighbours - the top of the body on a phone, its end on a desktop,
    // which opens the Conversation at its newest turn - and never a scattering.
    const first = blocks.findIndex((block) => block.drawn);
    expect(blocks.map((block) => block.drawn)).toEqual(
      blocks.map((_, at) => at >= first && at < first + drawn)
    );
    // And nothing within one viewport height of the reader is left undrawn, over a body that
    // really had something near: "every near block drew" is met by a body with no near block
    // at all (dispatch://INFRA-341, the pairing every universal clause of a check takes).
    expectNoneOf(
      blocks.filter((block) => block.near),
      {
        amongAtLeast: 1,
        are: (block) => !block.drawn,
        matched: "diagrams the margin reached and left undrawn",
        measured: "diagrams of the body within a viewport height of the reader",
      }
    );
  } finally {
    await alice.close();
  }
});

/** The cap R11 holds every frame of a reply to a SyncStep1 to, the document's name and the outer
 *  tag included: `stateFrameBytes` of the sjawhar/ygo fork's `provider/websocket/peer.go`, which
 *  `packages/envoy/internal/dispatch/docs/first_sync_frames_test.go` holds the server to. */
const syncFrameBytes = 64 << 10;

/** One frame of the server's reply to the page's SyncStep1, as it crossed the socket. */
interface ReplyFrame {
  /** The whole frame, the document's name and the outer tag included. */
  readonly bytes: number;
  /** The sync message it carries: `messageYjsUpdate` for each frame of a split reply but the
   *  last, `messageYjsSyncStep2` for the last. */
  readonly sync: number;
}

/**
 * The reply to the page's SyncStep1 among `frames`, every frame the server sent the page in
 * order: each sync frame carrying an update or a SyncStep2, up to and including the first
 * SyncStep2, which is what the page reads as the end of its sync. The server's own SyncStep1 and
 * every frame of another tag are not the reply and are passed over.
 */
function replyToTheSyncStep1(frames: readonly Buffer[]): ReplyFrame[] {
  const reply: ReplyFrame[] = [];
  for (const frame of frames) {
    const decoder = createDecoder(new Uint8Array(frame));
    readField(decoder); // the document name
    if (readVarUint(decoder) !== MessageType.Sync) {
      continue;
    }
    const sync = readVarUint(decoder);
    if (sync === messageYjsSyncStep1) {
      continue;
    }
    reply.push({ bytes: frame.length, sync });
    if (sync === messageYjsSyncStep2) {
      break;
    }
  }
  return reply;
}

// A11's "an unsent edit survives a reconnect", over the document whose reply R11 cuts into
// frames. The page's socket is dropped once its editor is built and editable, the reader types
// while it is down, and the reconnect is held until the page has reloaded: what the reader typed
// is then in this browser's pending-edit store and nowhere else, and it reaches the server only
// as the store's restore, which waits for the reconnect's sync to end in its SyncStep2
// (`web/src/features/doc/connection.ts`, `notifyAdmitted`). The text is read from the server's
// `/text`, never from the page.
//
// The reload is what makes the reconnect's sync the larger document. A socket that reconnects
// under the same page asks the room only for what its Yjs document lacks - the provider's
// SyncStep1 carries that document's state vector - and with nobody writing meanwhile the room
// answers with one SyncStep2 frame: 1,159 bytes on this document, measured on an issue pod with
// this row's steps less the reload, which a split never touches. A reloaded page's document is
// empty, so its SyncStep1 asks for everything, and the reply is the whole 406,000 characters:
// 1,229,593 to 1,261,067 bytes in 20 or 21 frames over four runs there, the reply R11 splits.
// The row reads that reply's frames off the socket it holds (`documentTransport`'s
// `onServerFrame`) and holds them to R11's cap, so a server that sent it as one frame fails here
// by name: on the fork tag `main` pins (v1.55.1-sami.2) it came as one frame of 1,259,887 bytes.
//
// In `chromium` and not on a phone: the claim is about the server's frames and the browser's
// store, neither of which a phone's viewport changes, and an untagged row runs in that project
// alone, whose rows the lane lists first and so cuts into its early shards rather than the last.
//
// SLOW(>=2s): the larger document's seed and its editor's build, which the typing waits for, a
// reload, and the reconnect's sync of the whole document and the restore after it.
test(`an unsent edit survives a reconnect to ${documentNames.large}, whose sync comes in frames of at most 64 KiB`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { artifactId, path } = await seedDocument("specs-large.md", largeDocument);
  // The last section's prose, so the edit stands on the end of the document, which a reply cut
  // in document order carries in its last frames.
  const quote = proseOpening(documentDiagrams.large - 1);
  const typed = " Kept through the split reconnect.";

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    let reconnecting = false;
    const reconnectFrames: Buffer[] = [];
    const transport = await documentTransport(page, {
      onServerFrame: (message) => {
        if (reconnecting && typeof message !== "string") {
          reconnectFrames.push(message);
        }
      },
    });
    await page.goto(path);
    // The build of 406,000 characters is minutes on a busy machine: this is the wait a build
    // that never ends fails by, not a budget the row spends.
    await expect(documentEditor(page)).toHaveAttribute("contenteditable", "true", {
      timeout: 600_000,
    });
    await expect(connectedDot(page)).toHaveText("connected");

    // The socket is dropped, not merely held, and its reconnect is held.
    transport.hold();
    await transport.sever();
    await expect(connectedDot(page)).toHaveCount(0);
    await placeCaret(page, "after", quote);
    await page.keyboard.type(typed);
    await waitForPersistedTyping(page, artifactId, typed);
    expect(
      (await getArtifactText(artifactId)).markdown,
      "the server holds the edit before the reconnect: the socket was never down"
    ).not.toContain(typed);

    await page.reload();
    reconnecting = true;
    await transport.release();
    await expect(connectedDot(page)).toHaveText("connected", { timeout: 240_000 });

    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown, {
        intervals: [500],
        message: "the edit typed while the socket was down never reached the server's /text",
        timeout: 240_000,
      })
      .toContain(`${quote}${typed}`);
    const stored = (await getArtifactText(artifactId)).markdown;
    expect(stored.split(typed).length - 1, "the edit reached the server more than once").toBe(1);

    const reply = replyToTheSyncStep1(reconnectFrames);
    const largest = Math.max(0, ...reply.map((frame) => frame.bytes));
    const total = reply.reduce((sum, frame) => sum + frame.bytes, 0);
    console.log(
      `R11 the reconnect's sync · ${reply.length} frames, the largest ${largest} bytes, ` +
        `${total} bytes in all, against a cap of ${syncFrameBytes} a frame.`
    );
    expect(
      reply.at(-1)?.sync,
      "the reconnect's sync never ended in a SyncStep2, so no reply to the page's SyncStep1 " +
        "was read"
    ).toBe(messageYjsSyncStep2);
    expect(
      total,
      "the reconnect's sync carried no more than one frame's worth, so there was nothing to split " +
        "and this run says nothing about a split reply"
    ).toBeGreaterThan(syncFrameBytes);
    expect(
      largest,
      "a frame of the reconnect's sync is over 64 KiB: the reply to the page's SyncStep1 was " +
        "not cut into R11's frames"
    ).toBeLessThanOrEqual(syncFrameBytes);
  } finally {
    await alice.close();
  }
});
