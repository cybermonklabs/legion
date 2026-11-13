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
// The rows read from the wall clock over these documents are in four files, and every parallel
// project ignores all four (`clockSpecNames`, e2e/configurations.ts):
// `large-documents-measured.e2e.ts` is `iphone-measure-alone`'s, the two measurements of a
// first editable block; `large-documents-build-serves.e2e.ts` and
// `large-documents-build-serves.the-decision.e2e.ts` are `iphone-alone`'s, the page's own clock
// and a decision answered inside a build; and `large-documents-desktop-build.e2e.ts` is
// `chromium-alone`'s, the longest main-thread task of a build at a desktop width.
//
// Every other row of what was one file reads an order, a place or a count rather than a clock,
// and 406,000 characters buy none of them anything: they are in `large-documents-build.e2e.ts`,
// `large-documents-reader.e2e.ts`, the three `large-documents-first-screen.*.e2e.ts` files and
// `large-documents-fold.e2e.ts`, and what the eleven files share is `large-documents-rows.ts`.
import { type Browser, expect, type Page, test } from "@playwright/test";

import { getArtifactText } from "./api";
import { documentEditor, documentTransport } from "./editor";
import { documentDiagrams, documentNames, documentTitle, largeDocument } from "./large-documents";
import { measureDocument, throttleTheCpu, watchTheDocument } from "./large-documents-page";
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
// comes is a promise that never settles. Every such wait here is bounded, so a page that never
// makes the announcement fails the row by name. Left to the row's own timeout it failed
// nothing: in run 43385477411 at b6c6a359 the build-time row below started 10 s into shard 5 of
// 5 and was still waiting when that leg's 9-minute budget ended it, its other 293 rows green,
// with no row named and no report kept (dispatch://INFRA-341).
//
// So each bound is sized from what its wait costs and from what the leg the row runs in leaves
// it. The three rows run in `iphone-long-build-alone`, one worker, in this order, under a
// browser step of 240 s with no retry, 7 s of which is not a row
// (.github/workflows/dispatch-browser.yaml derives both); the leg was sized from what they cost
// four workers wide in `shard (5)` of run 43421318396, 108.6 s, 39.3 s and 51.8 s. The second
// and third also run in a `chromium` shard leg, four workers wide, under 540 s with one retry.
// The readings below are the page's own clock, from CI's shard legs and from this pod, one
// worker on two CPUs, at 4x CPU throttling (dispatch://INFRA-341); the leg's own quiet machine
// read the same page's first editable block at 1.6 s and its editor open at 2.8 s (run
// 43098661012), so every figure here over-states the phone's leg.
//
// The first row's waits are its own (`theWindowsFirstBlockTimeoutMs`, `theWindowsBuildTimeoutMs`
// below). Its first block cannot come before the row lets the socket through, which is after
// the draw and the 20 s hold: this pod released at 36.5 s, had the first block at 40.0 s and
// the editor open 32.4 s after it, over the whole drawn document. 90 s and 75 s are more than
// twice each. The row is the leg's first, and a page that never announces fails it 9 s of
// seeding plus 90 s in, or plus 40 s and 75 s: 124 s, and the other two rows' 91.1 s and the
// step's 7 s bring the leg to 222 s of its 240, so it ends with its report.
//
// The second and third rows share `firstBlockTimeoutMs` and `buildTimeoutMs`. The first block
// came at up to 11.1 s from the navigation (`iphone`, run 43421318396; 10.7 s in `chromium`,
// run 42686241170; 10.4 s on this pod), and the editor opened up to 14.3 s after it on CI
// (`iphone`, run 42682941869). This pod is slower: the third row's builds took up to 30.2 s
// (`chromium`), and the second row's editor opened 42.3 s from the navigation in one run, which
// is longer than its build alone. 30 s is near three times the slowest first block; 60 s is
// about twice the slowest build and one and a half times that whole span from the navigation.
// In the phone's leg the second row starts with 124.4 s of the 233 left and fails at about 10 s
// of seeding plus 11.1 s and 60 s, 81.1 s; the third starts with 85.1 s left and fails its first
// build at 6.7 s of seeding plus 11.1 s and 60 s, 77.8 s.
//
// The third row's second build has a bound of its own, because 60 s does not fit after
// everything ahead of it: its first block is reached at 6.7 + 20.9 + 22.5 = 50.1 s at the most
// (seeding, the first build whole, and the draw whole and released), and 85.1 s less that
// leaves 35 s. `theDrawnBuildTimeoutMs` is 30 s, which ends that path at 80.1 s, 5 s inside. That
// build took 2.2 to 10.6 s on CI four workers wide (runs 42628596511 to 43421318396) and 12.2
// to 23.4 s on this pod, so 30 s is near three times CI's slowest and 1.3 times this pod's: the
// room the leg leaves it, and no more.
//
// The rows' other waits are sized the same way: the first row's draw, at 3.9 s on this pod,
// `theDrawWithinMs`; the second row's word in the open editor, which the page had already shown
// before the editor opened, `theirWordStaysWithinMs`; and the third row's draw of the whole
// document, inside the 22.5 s (`iphone`) and 32.5 s (`chromium`) its first block took from the
// navigation, `theWholeDrawWithinMs`. Each path fits: 9 + 30 = 39 s of the first row's 233,
// 10 + 42.3 + 30 = 82.3 s of the second's 124.4 - 112.3 s with the 30 s its write's own work
// is then given (`owedWorkFinishesWithinMs`, `large-documents-rows.ts`) - and
// 6.7 + 20.9 + 45 = 72.6 s of the third's 85.1.
//
// In the `chromium` shard leg a row fails an attempt at 92 s at the most (the third row's second
// build, 6.7 + 22.6 + 32.5 + 30), so both attempts add at most 133 s to its worker, against the
// 314 s that leg's rows took in run 43421318396: 447 s of its 540.

/** How long the first row waits for the editor's first block, from the navigation. */
const theWindowsFirstBlockTimeoutMs = 90_000;

/** How long the first row waits for the editor to open once its first block is in. */
const theWindowsBuildTimeoutMs = 75_000;

/** How long the first row waits for the document's own draw to be on the page. */
const theDrawWithinMs = 30_000;

/** How long the second and third rows wait for the editor's first block, counted from the
 *  navigation, or from the release where the socket was held. */
const firstBlockTimeoutMs = 30_000;

/** How long the second and third rows wait for the editor to open once its first block is in,
 *  which is the end of its build; the third row's build over the whole drawn document waits
 *  `theDrawnBuildTimeoutMs` instead. */
const buildTimeoutMs = 60_000;

/** How long the third row waits for the editor to open over the whole drawn document. */
const theDrawnBuildTimeoutMs = 30_000;

/** How long the second row waits for the open editor to hold the other person's word. */
const theirWordStaysWithinMs = 30_000;

/** How long the third row waits for the page's own draw to hold the whole document. */
const theWholeDrawWithinMs = 45_000;

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
    const measuring = measureDocument(page, path, {
      buildTimeoutMs: theWindowsBuildTimeoutMs,
      firstBlockTimeoutMs: theWindowsFirstBlockTimeoutMs,
    });
    let closed = false;
    const noteClosed = () => {
      closed = true;
    };
    void measuring.then(noteClosed, noteClosed);

    // The reader has the document's own draw, and no editor is being built behind it.
    const theDraw = page.locator('[aria-label="Document text"] [data-block-id]').first();
    await expect(theDraw).toBeAttached({ timeout: theDrawWithinMs });
    await expect(documentEditor(page)).toHaveCount(0);
    // A window that stays open cannot be awaited: this is the hold above, long enough that a
    // window closing at the page's own end would have closed inside it (`heldAfterTheDrawMs`).
    await page.waitForTimeout(heldAfterTheDrawMs);
    expect(
      closed,
      "the measurement closed its window with no editor on the page: it read the longest task " +
        "of the draw's own screenful and none of the build"
    ).toBe(false);

    // The reader scrolls the document's first drawn block onto the screen. At this head the
    // scroll moves nothing: the document's own header is 168 px and the block the server drew
    // first stands at 590..616 of a 664 px viewport, so the page opens with it on the screen,
    // which is `large-documents-fold.e2e.ts`'s claim and fails there by name. The scroll stays
    // because it keeps this row independent of the header's height. The measurement times the
    // editor's first block where the reader can see it (`firstBlockSelector`,
    // `e2e/large-documents-page.ts`), so a header grown past the fold again - as it stood before
    // cb0c608e - would leave this row waiting its whole wait out on a block under the fold and
    // timing none of the build it is about, red for a cause that is not its own.
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
  // her page loads, so the only thing left for him to do while she builds is write. Nothing here
  // reads when his word left, so he is given no pattern to stamp it by.
  //
  // Opened inside the row's own `try`, as the timer row of
  // `large-documents-build-serves.e2e.ts` opens him: a room that refuses him
  // leaves neither him nor her context behind.
  let writer: TheOtherPersonsPen | undefined;
  try {
    writer = await theOtherPersonsPen(artifactId);
    const page = await alice.newPage();
    await throttleTheCpu(page);
    const reached = await watchTheDocument(page);
    await whenTheirWordLands(page, word);
    await page.goto(path, { waitUntil: "commit" });

    // He writes one word into the document's first block, which Alice's first batch has
    // already built, the moment she has that batch: it is in her Yjs document as the insert
    // returns, and the only thing between it and her editor is the socket message that carries
    // it.
    await reached.firstBlock(firstBlockTimeoutMs);
    writer.write(word);

    const openedMs = await reached.editorOpened(buildTimeoutMs);
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
    await expect(documentEditor(page)).toContainText(`${title} ${word}`, {
      timeout: theirWordStaysWithinMs,
    });
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
          timeout: theWholeDrawWithinMs,
        })
        .toBeGreaterThanOrEqual(overTheWholeDraw ?? 0);
      await throttleTheCpu(page);
      await transport.release();
    }
    const firstBlock = await reached.firstBlock(firstBlockTimeoutMs);
    const openedMs = await reached.editorOpened(
      overTheWholeDraw === undefined ? buildTimeoutMs : theDrawnBuildTimeoutMs
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
