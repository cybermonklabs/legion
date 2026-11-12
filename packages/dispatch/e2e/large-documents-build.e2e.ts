// The editor of a large document, built a batch of blocks at a time, read as the reader meets
// it: the draw they have before it exists, the swap that leaves them where they were, every
// block of the document in the editor and every block on the screen exactly once while it
// fills, the editor standing before its socket has synced, and the decision cards drawn a task
// each. What the numbers of `large-documents-measured.e2e.ts` are read over is this build, and
// these rows read what it does rather than how long it takes: the throttling several of them
// carry is there so the build lasts long enough for the page to draw frames while it runs,
// which is the only state they are about, and no row here is timed.
//
// Which document a row seeds is in its title. Every row here seeds the AHXV6-1-shaped document
// of `large-documents.ts`, the plain document beside it or the one short section
// `shortDocument` spells, never the larger document, whose build costs about five times as
// much: what these rows read is an order, a place or a count, and 406,000 characters buy none
// of them anything.
//
// The numbers R11's targets are read from, and every row that needs a build to last, are in
// `large-documents-measured.e2e.ts`; what a reader does with a document whose editor is still
// being built is in `large-documents-reader.e2e.ts`; the draw the server sends, and the reads
// and the chrome the page holds until the first batch, are in
// `large-documents-first-screen.e2e.ts`. What the four files share is `large-documents-rows.ts`.
import { expect, type Page, test } from "@playwright/test";

import { getArtifactBlocks } from "./api";
import {
  connectedDot,
  documentEditor,
  documentTransport,
  editorBeingBuilt,
  placeCaret,
} from "./editor";
import {
  decisionBlocks,
  documentDiagrams,
  documentNames,
  documentTitle,
  plainDocument,
  sectionHeading,
  smallDocument,
} from "./large-documents";
import { throttleTheCpu } from "./large-documents-page";
import {
  type BlockFrame,
  blocksBuilt,
  builtBlocks,
  type DrawnInATask,
  drawnBlocks,
  framesOfTheBuild,
  seedDocument,
  serverDrawnBlocks,
  shortDocument,
  topLevelBlocks,
} from "./large-documents-rows";
import { settledScrollTop } from "./mermaid-helpers";
import { asUser } from "./users";

// The two rows below are about the draw that makes the numbers of
// `large-documents-measured.e2e.ts` reachable at all: the document is drawn from its own
// markdown, through the read-only headless engine, before the editor exists
// (`web/src/features/doc/DocumentPreview.tsx`). Each holds the document's socket
// open-ended (`documentTransport`), so no sync ever reaches the page and no editor is ever
// built: what is left on the screen is what a reader has while one is being built for them.

test(`${documentNames.small} is on the screen before its editor is @phone`, async ({ browser }) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("specs-small.md", smallDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await documentTransport(page, { holding: true });
    await page.goto(path);

    // The first block the measurement records as `first painted`, drawn with no socket, no sync
    // and no editor (`large-documents-page.ts`): it carries `data-block-id` as the editor's
    // blocks do, and it is not the block the measurement's targets are read against.
    const firstBlock = page.locator('[aria-label="Document text"] [data-block-id]').first();
    await expect(firstBlock).toBeInViewport({ timeout: 120_000 });
    await expect(firstBlock).toContainText(documentTitle(documentDiagrams.small));
    // And the sections below it are drawn too, not only the block the measurement reads.
    await expect(page.getByRole("heading", { name: sectionHeading(0) })).toBeVisible();
    // And the editor really is not there: the page holds no document textbox at all.
    await expect(documentEditor(page)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test(`the editor opens ${documentNames.small} where its reader had got to @phone`, async ({
  browser,
}) => {
  test.setTimeout(600_000);
  // The AHXV6-1-shaped document, whose draw the build here runs over whole: what the row reads
  // is where the reader is left, which is the same question at either size. Measured on this
  // pod, the row costs 16 to 23 s over it against 105 s over the larger document.
  const { path } = await seedDocument("specs-small.md", smallDocument);
  // A section half-way down the document: far enough that a swap which kept the page's scroll
  // offset rather than the reader's place would leave them sections away from these words.
  const landmarkText = sectionHeading(Math.floor(documentDiagrams.small / 2));

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);

    const landmark = page.getByRole("heading", { name: landmarkText });
    await expect(landmark).toBeAttached({ timeout: 240_000 });
    // The reader stops with this section's heading at the top of their screen - the block the
    // swap reads their place from is the one the screen starts with, so this is the block the
    // row is about. The blocks the scroll passed paint as it goes and the page settles under
    // them, which moves the heading, so it is put back at the top once that is over. How far
    // down the reader is, the row proves before it swaps: over 10,000 px, tens of screenfuls,
    // so a swap that kept the page's offset rather than their place would land sections away.
    await landmark.scrollIntoViewIfNeeded();
    await settledScrollTop(page);
    await landmark.evaluate((heading) => heading.scrollIntoView({ block: "start" }));
    const scrolled = await settledScrollTop(page);
    expect(scrolled).toBeGreaterThan(10_000);
    const where = await landmark.boundingBox();
    expect(where?.y ?? Number.NaN).toBeLessThan(200);

    await transport.release();
    await expect(documentEditor(page)).toBeVisible({ timeout: 240_000 });
    // The editor has replaced the draw the reader was reading, and left their own words on the
    // same line of the screen. Read once the page has stopped moving: the editor's diagrams
    // draw for seconds after it opens and its skipped blocks paint at their own heights, and
    // the page is put back under the reader after each of those (`refocusAfterDiagrams`). Not
    // to the pixel: the two draws wear different stylesheets, so the block they were reading is
    // a few pixels taller or shorter in one than in the other.
    await settledScrollTop(page);
    const landed = await documentEditor(page)
      .getByRole("heading", { name: landmarkText })
      .boundingBox();
    expect(Math.abs((landed?.y ?? Number.NaN) - (where?.y ?? Number.NaN))).toBeLessThan(200);
  } finally {
    await alice.close();
  }
});

// The three rows below are about the build behind that swap. The editor is not drawn in one task
// any more: it is built a batch of blocks at a time, each batch its own task, the collab binding
// adopts the document and the mapping those batches made rather than drawing the whole document
// itself, and the last batch, that adoption and the swap are three tasks
// (`packages/proof-editor/src/progressive-build.ts`). Each holds the document's
// socket (`documentTransport`), so the build begins when the row releases it, and each throttles
// the renderer as the measurement does - not to time anything, but so the build lasts long
// enough for the page to draw frames while it runs, which is the only state these rows are about.

test(`the editor opens ${documentNames.small} with every block of it in the DOM @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { artifactId, path } = await seedDocument("specs-small.md", smallDocument);
  const blocks = await getArtifactBlocks(artifactId);
  expect(blocks.length).toBeGreaterThan(100);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await framesOfTheBuild(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    // The document's own draw is what the reader has while no socket has synced, and no editor
    // has been built from it yet.
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );
    expect(await blocksBuilt(page)).toBe(0);

    // Throttled here and not before the load: the build this row reads begins with the release
    // below, and the boot and the page's own draw above it are setup (`throttleTheCpu`).
    await throttleTheCpu(page);
    await transport.release();
    // The wait is this row's own reading as much as it is the build: the per-frame count of
    // `framesOfTheBuild` scans every block element the editor holds in each frame the build
    // lets the browser draw, and this wait's role selector runs over the same document again
    // on every mutation the batches make. Measured on this pod at 4x CPU throttling, with the
    // socket released at the draw's first block as above, the whole row costs 30 to 44 s over
    // the AHXV6-1-shaped document, against 402 s over the larger one, which it seeded until the
    // browser shards ran out of their nine minutes. The 900 s stands because a `chromium`
    // lane pays about twice a phone's for the same reading, and because before the build's
    // frontier was fixed (`packages/proof-editor/src/progressive-build.ts`) the same two
    // readings over the larger document were 173 s and 281 s.
    await expect(documentEditor(page)).toBeVisible({ timeout: 900_000 });

    // Every block the server indexed the document under is in the editor's DOM, under the id a
    // `#b-<id>` link lands on, rather than only counted: two different blocks satisfy a count.
    // A code block is the one exception, and not a missing block: its node view builds its own
    // element and stamps no id on it
    // (`packages/proof-editor/src/mermaid-code-block-view.ts`), and this document's code blocks
    // are the mermaid source of each of its diagrams and a TypeScript listing per section.
    const identified = blocks
      .filter((block) => block.type !== "code_block")
      .map((block) => block.id);
    expect(identified.length).toBeGreaterThan(blocks.length / 2);
    const missing = await documentEditor(page).evaluate(
      (editor, ids) =>
        ids.filter((id) => editor.querySelector(`[data-block-id="${CSS.escape(id)}"]`) === null),
      identified
    );
    expect(missing).toEqual([]);

    // And the build really was in batches: the page drew frames holding part of the document
    // and not all of it, which a build done in one task never shows.
    const whole = await blocksBuilt(page);
    const frames = await page.evaluate(() => window.__blocksBuilt ?? []);
    expect(
      frames.filter((held) => held > 0 && held < whole),
      "no frame was drawn holding part of the document: it was built in one task, which is " +
        "the freeze this build exists to end"
    ).not.toEqual([]);
  } finally {
    await alice.close();
  }
});

test(`an edit made while ${documentNames.small}'s editor is being built is in it when the build ends @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  // The AHXV6-1-shaped document: what this row needs of a build is that it outlast several of
  // the other person's words, which the row proves itself before it reads anything else. It
  // costs 94 s over this document against 428 s over the larger one on this pod.
  const { path } = await seedDocument("specs-small.md", smallDocument);
  const title = documentTitle(documentDiagrams.small);

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    // Bob has the document open and his caret in its first block before Alice's socket is let
    // through, so the only thing left for him to do afterwards is press the keys.
    const bobPage = await bob.newPage();
    await bobPage.goto(path);
    await expect(documentEditor(bobPage)).toContainText(title, { timeout: 240_000 });
    await expect(connectedDot(bobPage)).toHaveText("connected");
    await placeCaret(bobPage, "after", title);

    const page = await alice.newPage();
    await framesOfTheBuild(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );

    // Throttled here and not before the load: her build begins with the release below, and her
    // boot and her page's own draw above it are setup (`throttleTheCpu`).
    await throttleTheCpu(page);

    // Bob writes a word at a time, from the moment Alice's socket is let through until her
    // editor is open, so his writing covers the whole of her build rather than a moment the row
    // has to guess at. He writes at a person's pace (`delay`) rather than as fast as the
    // protocol allows, for the same reason: a hundred words typed in three seconds would all be
    // written before her socket had even finished syncing. Which of his words crossed the build
    // is not something either page can say; that his writing covered one is what the frames
    // below prove.
    await transport.release();
    const opened = documentEditor(page)
      .waitFor({ state: "visible", timeout: 240_000 })
      .then(() => true);
    const written: string[] = [];
    let open = false;
    void opened.then((it) => {
      open = it;
    });
    while (!open && written.length < 120) {
      const word = `Pangolin${written.length}`;
      await bobPage.keyboard.type(` ${word}`, { delay: 25 });
      written.push(word);
    }
    await opened;
    expect(written.length).toBeGreaterThan(3);

    // Alice's editor holds every word Bob wrote while it was being built, in the block he wrote
    // them in.
    await expect(documentEditor(page)).toContainText(written.join(" "), { timeout: 240_000 });
    const whole = await blocksBuilt(page);
    const frames = await page.evaluate(() => window.__blocksBuilt ?? []);
    expect(
      frames.filter((held) => held > 0 && held < whole),
      "no frame was drawn holding part of the document, so Bob's words crossed no build and " +
        "this run says nothing about an edit that does"
    ).not.toEqual([]);
  } finally {
    await bob.close();
    await alice.close();
  }
});

/**
 * Records, every animation frame until the editor is open, every top-level block of the
 * document on the screen: how many there are across the three surfaces, how many of them the
 * editor has built, and any `data-block-id` held twice. Install before the navigation.
 *
 * The reading is the page's own rather than a poll from here, for the reason `framesOfTheBuild`
 * gives: a poll cannot land between the tasks of the build, and a frame is what the reader is
 * shown anyway. The editor is named only once its last batch is in and its binding is live
 * (`web/src/features/doc/editor.ts`), which is the last frame worth reading.
 */
async function framesOfTheBlocks(page: Page): Promise<void> {
  await page.addInitScript(
    ({ built, editor, surfaces }: { built: string; editor: string; surfaces: string[] }) => {
      const frames: BlockFrame[] = [];
      window.__blockFrames = frames;
      const look = () => {
        const held = new Map<string, number>();
        let blocks = 0;
        for (const surface of surfaces) {
          for (const block of document.querySelectorAll(surface)) {
            const id = block.getAttribute("data-block-id") ?? "";
            held.set(id, (held.get(id) ?? 0) + 1);
            blocks += 1;
          }
        }
        frames.push({
          blocks,
          built: document.querySelectorAll(built).length,
          twice: [...held]
            .filter(([, count]) => count > 1)
            .map(([id]) => id)
            .slice(0, 5),
        });
        if (document.querySelector(editor) !== null) return;
        requestAnimationFrame(look);
      };
      requestAnimationFrame(look);
    },
    {
      built: builtBlocks,
      editor: '[role="textbox"][aria-label="Document editor"]',
      surfaces: [serverDrawnBlocks, drawnBlocks, builtBlocks],
    }
  );
}

// Acceptance A7's own count: each block id across the server's draw, the page's own draw and
// the editor, on every animation frame, with every block on the screen exactly once. The
// document is the one with neither a code block nor a diagram in it, because a code block's
// node view builds its own element and stamps no id on it
// (`packages/proof-editor/src/mermaid-code-block-view.ts`), so in that one document every
// top-level block carries an id in every draw of it and the three surfaces are comparable
// block for block.
test("every block of a document is on the screen exactly once as its editor is built @phone", async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { artifactId, path } = await seedDocument("specs-plain.md", plainDocument);
  const whole = await topLevelBlocks(artifactId);
  expect(whole).toBeGreaterThan(100);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await framesOfTheBlocks(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);

    // The whole document, drawn by the page itself with no editor behind it: the count every
    // frame from here on has to hold, whichever surface is holding the blocks.
    await expect
      .poll(() => page.locator(drawnBlocks).count(), {
        intervals: [500],
        message: "the page never drew the whole document, so no frame holds every block of it",
        timeout: 240_000,
      })
      .toBe(whole);
    const drewAt = await page.evaluate(() => window.__blockFrames?.length ?? 0);

    // Throttled here and not before the load: the build this row reads begins with the release
    // below, and the boot and the page's own draw above it - with the poll that waits for it -
    // are setup (`throttleTheCpu`).
    await throttleTheCpu(page);
    await transport.release();
    await expect(documentEditor(page)).toBeVisible({ timeout: 480_000 });
    const frames = (await page.evaluate(() => window.__blockFrames ?? [])).slice(drewAt);
    expect(frames.length).toBeGreaterThan(1);

    // Never twice: no frame held one block id on two surfaces at once, which is what a build
    // that revealed its blocks without taking the draw's copies away would show.
    expect(
      frames.filter((frame) => frame.twice.length > 0).map((frame) => frame.twice),
      "a frame held a block of the document twice: the editor built it and the page's own draw " +
        "still held it"
    ).toEqual([]);
    // And never missing: every frame held the whole document, whichever surface each block was
    // on, so no block of it was off the screen while the editor was being built.
    expect(
      frames.filter((frame) => frame.blocks !== whole).map((frame) => frame.blocks),
      `a frame held something other than the document's ${whole} blocks`
    ).toEqual([]);
    // And the build really was in batches behind that count: frames were drawn with part of the
    // document in the editor and the rest still in the page's own draw.
    expect(
      frames.filter((frame) => frame.built > 0 && frame.built < whole).length,
      "no frame was drawn with part of the document in the editor: it was built in one task, " +
        "which is the freeze this build exists to end"
    ).toBeGreaterThan(0);
    expect(frames.at(-1)?.built).toBe(whole);
  } finally {
    await alice.close();
  }
});

/** The editor's root, whether or not anything of the document is in it: `constructProofEditor`
 *  puts this class on the element it builds into, before the binding draws a block
 *  (`packages/proof-editor/src/lib.ts`). `liveEditorRoot` below is the later moment its first
 *  batch lands. */
const editorRoot = ".proof-editor";

// The editor is built while its socket is still syncing, and only bound when the state frame
// has been applied: nothing the construction is made of - Milkdown, the schema's typed blocks,
// every plugin's view, Dispatch's own node views - depends on a block of the document, and the
// whole of it used to be paid for after the sync (dispatch://INFRA-341, R11,
// `@legion/proof-editor`'s `constructProofEditor`). This row holds the socket open-ended, so
// nothing ever syncs: an editor root in the page is then an editor built with no document
// anywhere near it, which is the order rather than the clock. Before the split there was
// nothing to find - the page called the editor's one factory inside its `onSynced` - and the
// measured opening timeline says the same thing in milliseconds.
//
// The reader keeps their own draw throughout: the root is hidden until the first batch, which
// is the binding's, so the editor standing behind it puts no empty document in front of them.
test("the editor is built while the document's socket is still syncing @phone", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("while-syncing.md", shortDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);

    // The reader's own draw of the document, and behind it an editor with nothing in it.
    const drawn = page.locator('[aria-label="Document text"]');
    await expect(drawn).toContainText("The paragraph a reader meets first.");
    const root = page.locator(editorRoot);
    await expect(root).toHaveCount(1, { timeout: 60_000 });
    // Hidden, so the draw is the only document on the screen; no first batch has landed, so
    // the root takes no edits; and the editor wears no name, so its build is not over.
    await expect(root).toBeHidden();
    await expect(editorBeingBuilt(page)).toHaveCount(0);
    await expect(documentEditor(page)).toHaveCount(0);
    // And nothing of the document is in it: every block on the screen is the draw's.
    expect(await page.locator(`${editorRoot} [data-block-id]`).count()).toBe(0);

    // The state frame, at last: the editor that was already standing takes the document.
    await transport.release();
    await expect(documentEditor(page)).toBeVisible({ timeout: 120_000 });
    await expect(documentEditor(page)).toContainText("The paragraph a reader meets first.");
    await expect(connectedDot(page)).toBeVisible();
  } finally {
    await alice.close();
  }
});

/**
 * Records what each task of the page drew: the editor's top-level blocks and the decision
 * cards mounted into their shells. Install before the navigation it watches.
 *
 * A `MutationObserver` is what makes a task the unit: every record one task leaves is
 * delivered in a single callback, at that task's own microtask checkpoint, so a callback that
 * sees two cards appear is a task that drew two of them. The counts are read off the page
 * rather than taken from the records, because a card's own markup is a subtree the portal
 * brings in whole and the editor's blocks arrive as a batch's nodes: the count after each task
 * is the only thing either of them says plainly.
 *
 * A decision card is drawn when the shell's header slot - which `AskBlockView` leaves empty
 * (`web/src/features/doc/typed-block-drawing.ts`) - holds anything: that is `AskBlockCard`'s
 * portal, the first draw of the card and the one this row is about.
 */
async function drawsOfEachTask(page: Page): Promise<void> {
  await page.addInitScript(
    ({ blocks: blockSelector, headers }: { blocks: string; headers: string }) => {
      const drawn: DrawnInATask[] = [];
      window.__drawnInTasks = drawn;
      let blocks = 0;
      let cards = 0;
      const look = () => {
        const nowBlocks = document.querySelectorAll(blockSelector).length;
        const nowCards = [...document.querySelectorAll(headers)].filter(
          (header) => header.childElementCount > 0
        ).length;
        if (nowBlocks === blocks && nowCards === cards) return;
        drawn.push({
          at: Math.round(performance.now()),
          blocks: nowBlocks - blocks,
          cards: nowCards - cards,
        });
        blocks = nowBlocks;
        cards = nowCards;
      };
      // The document itself, because this script runs before the page has an element to watch.
      new MutationObserver(look).observe(document, { childList: true, subtree: true });
    },
    {
      blocks: builtBlocks,
      headers: "[data-dispatch-ask-block] > [data-dispatch-ask-header]",
    }
  );
}

// The five decision cards of a document are five React cards portalled into the editor's own
// shells, and their first draw is the dearest thing on the way to an open editor after the
// build itself: drawn together they were 470 ms of one 678 ms task on this pod, `iphone` at 4x
// CPU throttling, and that task was the one the host swaps the editor in with
// (dispatch://INFRA-341, R11). So each card is drawn in a task of its own, and no task both
// adopts a batch of the editor's blocks and draws a card. This row reads the seam, not the
// clock: it throttles nothing, because which task a draw lands in is the same however fast the
// machine is.
test(`each decision card of ${documentNames.small} is drawn in a task of its own @phone`, async ({
  browser,
}) => {
  test.setTimeout(600_000);
  const { path } = await seedDocument("specs-small.md", smallDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await drawsOfEachTask(page);
    await page.goto(path);
    await expect(documentEditor(page)).toBeVisible({ timeout: 240_000 });
    // Every decision of the document is hosting its ask card, so every card this row is about
    // has been drawn and the record below is complete.
    await expect(
      documentEditor(page).locator('[data-dispatch-ask-block] article[data-testid^="ask-"]')
    ).toHaveCount(decisionBlocks, { timeout: 240_000 });

    const drawn = await page.evaluate(() => window.__drawnInTasks ?? []);
    const drewACard = drawn.filter((task) => task.cards > 0);
    expect(
      drewACard.length,
      `the page drew its ${decisionBlocks} decision cards over ${drewACard.length} task(s): ` +
        "a card drawn in the same task as another is a freeze the reader feels as one"
    ).toBeGreaterThanOrEqual(decisionBlocks);
    expect(
      drewACard.filter((task) => task.cards > 1),
      "one task drew more than one decision card"
    ).toEqual([]);
    expect(
      drawn.filter((task) => task.cards > 0 && task.blocks !== 0),
      "one task both changed the editor's own blocks and drew a decision card: the build's " +
        "batch and the card's first draw are two tasks, never one"
    ).toEqual([]);
  } finally {
    await alice.close();
  }
});
