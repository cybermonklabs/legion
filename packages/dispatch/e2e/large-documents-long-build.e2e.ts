// What a build of the 406,000-character document of `large-documents.ts` costs the page, read
// without holding any number to the wall clock: that the window the measurements read their
// longest task over covers the whole build and not the draw in front of it, that another
// person's edit crosses a build instead of waiting for it, and that a build over a page
// already holding the whole drawn document takes no longer than one over a screenful.
//
// No row here is read from the clock. The last of them does read two builds' own times, and
// holds their ratio: both are measured in the one run, one after the other, so what they are
// compared against is the same machine under the same load rather than a number a quiet machine
// earned (its own comment says what the ratio rules out).
//
// The desktop half runs in `chromium`, four workers wide, like every other row of this suite.
// The phone half is `iphone-long-build-alone`'s, a leg of its own, and `iphone` ignores this file
// (`phoneAloneSpecNames`, e2e/configurations.ts): those three builds under 4x CPU throttling
// cost 1.8 min, 51.8 s and 39.3 s in `shard (5)` of run 43421318396, and as the tail of the
// `iphone` listing Playwright cuts its shards from they all landed in that last shard, which
// stood at 499 s of its 540 s.
//
// Each row seeds the larger document, because each needs a build that lasts: a window read
// over a build of a few seconds says nothing about a build of minutes, the edit has to cross
// one, and the ratio is about the page the whole draw leaves behind. The one other row of the
// suite that seeds it reads the reader's place half-way down it, and is in
// `large-documents-first-screen.e2e.ts`.
//
// The rows read from the wall clock over these documents are a file apiece, one per project,
// and every parallel project ignores all three (`clockSpecNames`, e2e/configurations.ts):
// `large-documents-measured.e2e.ts` is `iphone-measure-alone`'s, the two measurements of a
// first editable block; `large-documents-build-serves.e2e.ts` is `iphone-alone`'s, the page's
// own clock and a decision answered inside a build; and
// `large-documents-desktop-build.e2e.ts` is `chromium-alone`'s, the longest main-thread task
// of a build at a desktop width.
//
// Every other row of what was one file reads an order, a place or a count rather than a clock,
// and 406,000 characters buy none of them anything: they are in `large-documents-build.e2e.ts`,
// `large-documents-reader.e2e.ts` and `large-documents-first-screen.e2e.ts`, and what the seven
// files share is `large-documents-rows.ts`.
import { type Browser, expect, type Page, test } from "@playwright/test";

import { getArtifactText } from "./api";
import { documentEditor, documentTransport } from "./editor";
import { documentDiagrams, documentNames, documentTitle, largeDocument } from "./large-documents";
import {
  measureDocument,
  theMomentThePageReached,
  throttleTheCpu,
  watchTheDocument,
} from "./large-documents-page";
import {
  blocksBuilt,
  drawnBlocks,
  everythingOwedFinished,
  liveEditorRoot,
  namedEditor,
  seedDocument,
  type TheOtherPersonsPen,
  theOtherPersonsPen,
  topLevelBlocks,
} from "./large-documents-rows";
import { asUser } from "./users";

// What a row of this file waits for of the page - its first block in the viewport and its
// editor open - is an announcement the page makes (`watchTheDocument`), and one that never
// comes is a promise that never settles. Every such wait here is bounded by the two figures
// below, which are the ones the first row gives `measureDocument`, so a page that never makes
// the announcement fails the row by name. Left to the row's own timeout it failed nothing: in
// run 43385477411 at b6c6a359 the build-time row below started 10 s into shard 5 of 5 and was
// still waiting when that leg's 9-minute budget ended it, its other 293 rows green, with no row
// named and no report kept. Where the page announces both it ends in 40 to 80 s
// (dispatch://INFRA-341).

/** How long a row here waits for the editor's first block to come into the viewport. */
const firstBlockTimeoutMs = 240_000;

/** How long a row here waits for the editor to open, which is the end of its build. */
const buildTimeoutMs = 480_000;

const neverInTheViewport =
  "the editor's first visible block never came into the viewport: either no editor root ever " +
  'carried `data-write-path="live"` with a block in it, or a taller page header has pushed ' +
  "that block under the fold";

const neverOpened = "the editor never opened, so its build never ended";

/** How long the row below holds the socket after the document's own draw is on the screen.
 *  Well past the moment the page's own end is reached - the draw is a screenful and a few
 *  steps of 100 ms exhaust it - so a window that closed there closed inside this wait. */
const heldAfterTheDrawMs = 20_000;

// What the two measuring rows of `large-documents-measured.e2e.ts` read their longest task
// over has to cover the build, and the page is
// not the document until the editor is in it: the draw in front of the reader is the blocks the
// document opens with (`large-documents-page.ts`'s module comment), so a window that ended
// where that page ends ended four scroll steps in, with most of the build still to come. This
// row holds the document's socket, so the editor is built only when it says so, and reads
// whether the measurement closed its window meanwhile.
//
// It is also the one row whose build the measurement could not read through, which is what
// `listenForThePage` in `large-documents-page.ts` is there for: holding the socket this long
// leaves the reader's own draw of the whole document on the page - every one of its 986 blocks
// and 140 diagrams - so the editor builds over all of it and takes minutes, where those two
// rows pay about 35 s for the same build over a page holding one screenful. From a build's
// first batch to its last the page answers nothing the run asks it, so a number the run had to
// read was a number this row could not have until the build was over, and the wait for the
// editor's first block ran out on a block the page had recorded three seconds into it. The
// numbers are announcements the page makes now, which those two rows never needed.
test(`the measurement's window stays open until ${documentNames.large}'s editor opens @phone`, async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone-long-build-alone",
    "the measurement set-up is the phone's viewport, touch and Chromium, which is " +
      "iphone-long-build-alone's"
  );
  test.setTimeout(900_000);
  const { path } = await seedDocument("specs-large.md", largeDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page, { holding: true });
    const measuring = measureDocument(page, path, { buildTimeoutMs, firstBlockTimeoutMs });
    let closed = false;
    const noteClosed = () => {
      closed = true;
    };
    void measuring.then(noteClosed, noteClosed);

    // The reader has the document's own draw, and no editor is being built behind it.
    const theDraw = page.locator('[aria-label="Document text"] [data-block-id]').first();
    await expect(theDraw).toBeAttached({ timeout: 240_000 });
    await expect(documentEditor(page)).toHaveCount(0);
    // A window that stays open cannot be awaited: this is the hold above, long enough that a
    // window closing at the page's own end would have closed inside it (`heldAfterTheDrawMs`).
    await page.waitForTimeout(heldAfterTheDrawMs);
    expect(
      closed,
      "the measurement closed its window with no editor on the page: it read the longest task " +
        "of the draw's own screenful and none of the build"
    ).toBe(false);

    // The reader scrolls the document onto the screen, as a reader of this page on a phone must:
    // the issue's header, its tabs and the document's own controls stand 706 px down a 664 px
    // viewport, so at the top of the page no block of the document is on the screen at all - the
    // blocks the server drew included. The measurement times the editor's first block where the
    // reader can see it (`firstBlockSelector`, `e2e/large-documents-page.ts`), so with nobody
    // scrolling it waits its whole wait out on a block 42 px under the fold and times none of
    // the build this row is about.
    //
    // To the foot of the screen rather than to the top of it, and here rather than when the draw
    // first landed. Both are about where the browser keeps the reader while the build fills the
    // page: scrolled this far the top of the viewport is still the page's own header, which is
    // what the reader is held against, and the editor's blocks land under it in the place the
    // draw's blocks are giving up (`DocumentPreview`'s `takesTheBuiltBlocks`) - the editor's
    // first block drawn where the draw's first block stands now. Scrolled so that block is at
    // the top instead, the reader is held against the document itself and the build's first
    // batch goes in above them: the editor's first block is then 1,076 px over the fold and
    // stays there for the whole build. Later than the draw's own painting, because until this
    // line it is still putting the rest of the document in and holding the reader through it
    // (`readers-place.ts`), which leaves a reader scrolled here deep in the document; from this
    // line it paints no more, since the release below begins the build and the draw stops in
    // that same task (`building`).
    await theDraw.evaluate((block) => block.scrollIntoView({ block: "end" }));

    // Released, the build runs, the editor opens, and only then does the window close.
    await transport.release();
    const measured = await measuring;
    expect(measured.editorOpenedMs ?? 0).toBeGreaterThan(heldAfterTheDrawMs);
    // And what it scrolled was the editor's own page, not the draw's screenful.
    expect(measured.scrollSteps).toBeGreaterThan(10);
  } finally {
    await alice.close();
  }
});

/**
 * Records the first animation frame in which the editor's own first block holds `word`, with
 * how many blocks the editor held then and whether its build was over.
 *
 * Only the first block is read, because that is the block the other person writes in and
 * reading the whole of a 406,000-character document every frame would be the thing slowing the
 * page down. The loop does not stop when the editor is named: a word that lands only once the
 * build is over is the failure this row is about, and the row says so rather than finding
 * nothing recorded.
 */
async function whenTheirWordLands(page: Page, word: string): Promise<void> {
  await page.addInitScript(
    ({ live, named, wanted }: { live: string; named: string; wanted: string }) => {
      const look = () => {
        const root = document.querySelector(live);
        const first = root === null ? null : root.querySelector("[data-block-id]");
        if (root !== null && first !== null && (first.textContent ?? "").includes(wanted)) {
          window.__theirWord = {
            blocks: root.querySelectorAll("[data-block-id]").length,
            open: document.querySelector(named) !== null,
          };
          return;
        }
        requestAnimationFrame(look);
      };
      requestAnimationFrame(look);
    },
    { live: liveEditorRoot, named: namedEditor, wanted: word }
  );
}

// The larger document, as every row of this file: what this one needs of a build is that it outlast
// one word of another person's and the socket message that carries it. Over the AHXV6-1-shaped
// document the word reached the editor with 1,343 of its 1,436 blocks built on this pod, which
// leaves the row nothing to fail on, against 2,111 of 7,036 over the larger one when the word
// was typed into a second browser page and 53 of 7,036 now that it is an insert from this
// process (`theOtherPersonsPen`): the page the word used to be typed into spent its own second
// or more laying out the same document before it could send one.
test(`another person's edit reaches ${documentNames.large} while its editor is built @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { artifactId, path } = await seedDocument("specs-large.md", largeDocument);
  const title = documentTitle(documentDiagrams.large);
  const word = "Pangolin.";

  const alice = await asUser(browser, "alice");
  // The other person writes from this process rather than from a second browser page of the
  // same 406,000 characters: his word is one insert and one frame on her socket, where his page
  // spent a second or more laying the whole document out for it, which is what the row would
  // have been measuring instead of her build (`theOtherPersonsPen`). He is in the room before
  // her page loads, so the only thing left for him to do while she builds is write. The pattern
  // he stamps his words by is this one word taken literally: every character a pattern reads
  // specially is escaped, the backslash among them, as `agent-name.e2e.ts` escapes a name, so a
  // word that later holds one still matches only itself.
  //
  // Opened inside the row's own `try`, as the timer row of
  // `large-documents-build-serves.e2e.ts` opens him: a room that refuses him
  // leaves neither him nor her context behind.
  let writer: TheOtherPersonsPen | undefined;
  try {
    writer = await theOtherPersonsPen(artifactId, word.replace(/[\\^$.*+?()[\]{}|]/g, "\\$&"));
    const page = await alice.newPage();
    await throttleTheCpu(page);
    const reached = await watchTheDocument(page);
    await whenTheirWordLands(page, word);
    await page.goto(path, { waitUntil: "commit" });

    // He writes one word into the document's first block, which Alice's first batch has
    // already built, the moment she has that batch: it is in her Yjs document as the insert
    // returns, and the only thing between it and her editor is the socket message that carries
    // it.
    await theMomentThePageReached(reached.firstBlock, firstBlockTimeoutMs, neverInTheViewport);
    writer.write(word);

    const openedMs = await theMomentThePageReached(
      reached.editorOpened,
      buildTimeoutMs,
      neverOpened
    );
    const whole = await blocksBuilt(page);
    const landed = await page.evaluate(() => window.__theirWord);
    console.log(
      `R11 their edit during the build · the word reached the editor with ` +
        `${landed?.blocks ?? 0} of ${whole} blocks built, build ` +
        `${landed?.open === true ? "over" : "running"}, editor open at ${Math.round(openedMs)} ms.`
    );

    expect(landed, "the editor never held the word the other person wrote").toBeDefined();
    expect(landed?.blocks ?? 0).toBeGreaterThan(0);
    expect(
      landed?.open,
      "the word reached the editor only once its build was over: another person's edit waits " +
        "for the whole build before the reader sees it"
    ).toBe(false);
    expect(
      landed?.blocks ?? 0,
      "the editor held the whole document when the word reached it, so nothing about this run " +
        "crossed a build"
    ).toBeLessThan(whole);

    // And it is the document's own word afterwards, not a redraw the binding undid.
    await expect(documentEditor(page)).toContainText(`${title} ${word}`, { timeout: 240_000 });
    await everythingOwedFinished("the word another person wrote during the build");
    const { markdown } = await getArtifactText(artifactId);
    expect(markdown).toContain(`# ${title} ${word}`);
  } finally {
    writer?.close();
    await alice.close();
  }
});

// What the editor's build costs must be the document's business, not the page's. The same 986
// top-level blocks took about 35 s to build over a page holding one screenful and about 200 s
// over a page holding the whole drawn document - the same blocks, the same batches, and
// minutes that scaled with what was already in the DOM (dispatch://INFRA-341, R11). The cost
// was a forced layout of the whole page in each batch (`ProofDocument`'s `onBatch` read the
// draw's box) over a page the build never let the browser render, so the layout was never
// clean when it was read: 147 ms of each batch over the whole drawn document against 11 ms
// over one screenful. That is more than the 120 ms a batch is budgeted, so the build cut every
// batch to one or two blocks and paid it 414 times instead of 101. The build hands the thread
// back as a posted task now, so the browser renders between batches and the read is of a
// layout it has just done, and the draw is indexed once rather than scanned in each batch
// (`takesTheBuiltBlocks`). This row is what keeps that: the build's own time over the whole
// drawn document, against the build's own time over a screenful, measured one after the other
// in one run so the pod's own load is the same for both.

/** How many times longer the build over a page holding the whole drawn document may take than
 *  the build over a page holding one screenful. A page carrying the whole draw beside the
 *  editor carries about twice the boxes for the browser to lay out on its way through, which
 *  no build can give back; what this rules out is the quadratic above, which was seven times
 *  on this pod. Measured at 1.6 to 2.2 times over the runs this row was written against. */
const theSameRangeTimes = 3;

/** How long one editor's build took, on the page's own clock. */
interface TheBuildsTime {
  /** When the reader first had a block of the editor's own in the viewport, which is its
   *  first batch. */
  readonly firstBlockMs: number;
  /** When the editor opened: its last batch in and its binding live. */
  readonly openedMs: number;
  /** The build itself: the time between the two. */
  readonly buildMs: number;
}

/**
 * Opens `path` on a page of its own and answers how long the editor's build took there.
 *
 * With `overTheWholeDraw`, the document's socket is held until the page's own draw holds that
 * many of the document's top-level blocks, so the build runs over a page already holding the
 * whole document; without it the build runs over the screenful the draw has drawn by then,
 * which is what a reader whose socket answers at once gets. Both numbers are announcements
 * the page makes, so neither is a reading this side takes.
 */
async function theBuildsTime(
  browser: Browser,
  path: string,
  { overTheWholeDraw }: { overTheWholeDraw?: number } = {}
): Promise<TheBuildsTime> {
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const reached = await watchTheDocument(page);
    const transport =
      overTheWholeDraw === undefined ? undefined : await documentTransport(page, { holding: true });
    // The rate is the build's, and both builds are read under it. The page that holds its
    // socket is throttled once its draw is whole and before that socket is let through, so the
    // draw this arranges - and the poll that waits for it - is not paid at the build's rate;
    // neither is read, and the build that is begins at the release below. The page that holds
    // no socket has no such moment - its build begins with the sync its own load asks for - so
    // it is throttled before the navigation (`throttleTheCpu`).
    if (transport === undefined) {
      await throttleTheCpu(page);
    }
    await page.goto(path, { waitUntil: "commit" });
    if (transport !== undefined) {
      await expect
        .poll(() => page.locator(drawnBlocks).count(), {
          intervals: [1_000],
          message: "the page never drew the whole document, so no build ran over one",
          timeout: 300_000,
        })
        .toBeGreaterThanOrEqual(overTheWholeDraw ?? 0);
      await throttleTheCpu(page);
      await transport.release();
    }
    const firstBlock = await theMomentThePageReached(
      reached.firstBlock,
      firstBlockTimeoutMs,
      neverInTheViewport
    );
    const openedMs = await theMomentThePageReached(
      reached.editorOpened,
      buildTimeoutMs,
      neverOpened
    );
    return { buildMs: openedMs - firstBlock.at, firstBlockMs: firstBlock.at, openedMs };
  } finally {
    await alice.close();
  }
}

test(`${documentNames.large}'s editor builds over the whole drawn document in the time it takes over one screenful @phone`, async ({
  browser,
}) => {
  test.setTimeout(1_800_000);
  const { artifactId, path } = await seedDocument("specs-large.md", largeDocument);
  const whole = await topLevelBlocks(artifactId);
  expect(whole).toBeGreaterThan(900);

  // One screenful first, because that is the build the two measuring rows are held to: a pod
  // that has grown slower since would show it there before it showed it in the ratio.
  const screenful = await theBuildsTime(browser, path);
  const drawn = await theBuildsTime(browser, path, { overTheWholeDraw: whole });
  console.log(
    `R11 build time · over one screenful ${Math.round(screenful.buildMs)} ms ` +
      `(first block ${Math.round(screenful.firstBlockMs)} ms, editor open ` +
      `${Math.round(screenful.openedMs)} ms); over the whole drawn document ` +
      `(${whole} blocks) ${Math.round(drawn.buildMs)} ms (first block ` +
      `${Math.round(drawn.firstBlockMs)} ms, editor open ${Math.round(drawn.openedMs)} ms); ` +
      `${(drawn.buildMs / screenful.buildMs).toFixed(2)} times, against ${theSameRangeTimes}.`
  );

  expect(screenful.buildMs).toBeGreaterThan(0);
  expect(drawn.buildMs).toBeGreaterThan(0);
  expect(
    drawn.buildMs,
    "the editor took far longer to build over a page holding the whole drawn document than " +
      "over one holding a screenful, for the same blocks in the same batches: the build is " +
      "paying for what the page already holds once per batch again"
  ).toBeLessThanOrEqual(screenful.buildMs * theSameRangeTimes);
});
