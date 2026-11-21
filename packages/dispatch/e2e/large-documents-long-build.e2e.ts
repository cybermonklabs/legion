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
// one, and the ratio is about the page the whole draw leaves behind.
//
// The other files of this family, and what each holds, are listed once, at the head of
// `large-documents-rows.ts`, which holds what they share.
import { type Browser, expect, type Page, type TestInfo, test } from "@playwright/test";

import { getArtifactText } from "./api";
import { documentEditor, documentTransport } from "./editor";
import { documentDiagrams, documentNames, documentTitle, largeDocument } from "./large-documents";
import {
  measureDocument,
  theMomentThePageReached,
  throttleTheCpu,
  watchTheDocument,
} from "./large-documents-page";
import { watchThePageMove, whileThePageMoves } from "./large-documents-page-moves";
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
// The second and third rows' waits on the page are bounded on its progress (the paragraphs
// below this one's say how), and in the phone's leg each is also given the most it is waited
// for: `firstBlockTimeoutMs`, `buildTimeoutMs` and, for the third row, the two after them. The
// first block came at up to 11.1 s from the navigation (`iphone`, run 43421318396; 10.7 s in
// `chromium`, run 42686241170; 10.4 s on this pod), and the editor opened up to 14.3 s after it
// on CI (`iphone`, run 42682941869). This pod is slower: the third row's builds took up to
// 30.2 s (`chromium`), and the second row's editor opened 42.3 s from the navigation in one run,
// which is longer than its build alone. 30 s is near three times the slowest first block; 60 s
// is about twice the slowest build and one and a half times that whole span from the
// navigation.
// In the phone's leg the second row starts with 124.4 s of the 233 left and fails at about 10 s
// of seeding plus 11.1 s and 60 s, 81.1 s; the third starts with 85.1 s left and fails its first
// build at 6.7 s of seeding plus 11.1 s and 60 s, 77.8 s.
//
// In the phone's leg the third row's second build has a bound of its own, because 60 s does not
// fit after everything ahead of it: its first block is reached at 6.7 + 20.9 + 22.5 = 50.1 s at
// the most (seeding, the first build whole, and the draw whole and released), and 85.1 s less
// that leaves 35 s. `theDrawnBuildTimeoutMs` is 30 s, which ends that path at 80.1 s, 5 s
// inside. That build took 2.2 to 10.6 s on CI four workers wide (runs 42628596511 to
// 43421318396) and 12.2 to 23.4 s on this pod, so 30 s is near three times CI's slowest and 1.3
// times this pod's: the room the leg leaves it, and no more.
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
// Every wait of the second and third rows on the page goes on for as long as the page keeps
// moving, rather than for a time a quiet machine earned (`whileThePageMoves`, with the steps
// `watchThePageMove` hears, both `large-documents-page-moves.ts`'s). Beside a loaded
// machine the clock is the wrong shape: at 897ae8da, in a `chromium` row on two CPUs beside the
// full lane's shard 3 of 4 with three workers and no retry, the third row's build over one
// screenful was still not open 60 s after its first block, and beside the same load at adcb2968
// its first block once had not come 30 s after the navigation (dispatch://INFRA-341, R16). What
// tells a page that stopped from a slow one is whether it is still taking steps of what is
// waited for - the steps of its opening before the first block, the build's batches before the
// editor opens, its own draw before the whole document is drawn - so a row
// fails by the stall's name once `thePageStallsAfterMs` passes with none (`theDrawStallsAfterMs`
// for the draw), and waits for a page that keeps taking them. Measured on that same shape at
// adcb2968, 50 runs of the third row and 25 of the second, the longest this side went without
// a step was 7,591 ms before a first block (the second row's, between the server's first frame
// and the first batch: the document's state applied and the editor bound), 5,485 ms inside a
// build (6,123 ms in an earlier 25 runs, whose batches were counted the same way), and
// 13,721 ms inside the third row's draw of the whole document. 30 s is 3.9 times the longest
// before a first block and 4.9 times the longest inside a build, 45 s is 3.3 times the longest
// inside the draw, and a page that stopped fails that long after its last step.
//
// How long a page that keeps moving is waited for is the room its leg leaves the row. In the
// phone's leg that is the bounds above, because that leg's 240 s step was derived from them and
// it runs alone on a machine of its own. In a `chromium` shard leg it is one deadline over the
// row, `theShardsRowWithinMs`, 420 s from the row's start, which every one of its waits on the
// page shares (`theRoomTheLegLeaves`): one attempt of the third row ends by 420 s at the most,
// and of the second by 480 s, with the 60 s its open editor's two checks are given after
// (`theirWordStaysWithinMs`, `owedWorkFinishesWithinMs`) - 60 s inside the 540 s of the shard
// job's browser step. A stall fails an attempt far sooner: the second row at 10 + 11.1 + 14.3 +
// 30 = 65.4 s at the most (its build stalling at the end of the longest such build CI
// measured), and the third at 6.7 + 22.6 + 32.5 + 45 = 106.8 s (its draw stalling at the end of
// the longest such draw), so both attempts add at most 163 s to its worker, against the 314 s
// that leg's rows took in run 43421318396: 477 s of its 540. A row whose page keeps moving for
// the whole deadline has no retry left in that step; a page that slow is the one case the leg
// cannot run twice.

/** How long the first row waits for the editor's first block, from the navigation. */
const theWindowsFirstBlockTimeoutMs = 90_000;

/** How long the first row waits for the editor to open once its first block is in. */
const theWindowsBuildTimeoutMs = 75_000;

/** How long the first row waits for the document's own draw to be on the page. */
const theDrawWithinMs = 30_000;

/** The most the second and third rows wait, in the phone's leg, for the editor's first block
 *  while the page keeps opening its document, counted from the navigation, or from the release
 *  where the socket was held. */
const firstBlockTimeoutMs = 30_000;

/** The most the second and third rows wait, in the phone's leg, for the editor to open once its
 *  first block is in while its build keeps landing batches, which is the end of its build: in
 *  the second row its wait for its word to land in the editor shares this one's deadline rather
 *  than adding one of its own, and in the third row it is its build over one screenful's. */
const buildTimeoutMs = 60_000;

/** The most the third row waits, in the phone's leg, for the editor to open over the whole
 *  drawn document while its build keeps landing batches. */
const theDrawnBuildTimeoutMs = 30_000;

/** The most the third row waits, in the phone's leg, for the page's own draw to hold the whole
 *  document while it keeps drawing. */
const theWholeDrawWithinMs = 45_000;

/** How long the second and third rows let the page go without a step of what they are waiting
 *  on before they fail the row on the stall, by that name: a step of its opening while they
 *  wait for the first block, and a batch of the build while they wait for the editor to open
 *  (`whileThePageMoves`). */
const thePageStallsAfterMs = 30_000;

/** The same for the third row's wait for the page's own draw to hold the whole document, which
 *  goes longer between its steps than an opening or a build does (the file's head gives both
 *  readings). */
const theDrawStallsAfterMs = 45_000;

/** The most the second and third rows wait, in a `chromium` shard leg, for everything they wait
 *  on the page for while it keeps moving: one deadline over the row, counted from its start,
 *  which every one of those waits shares. */
const theShardsRowWithinMs = 420_000;

/** How long the second row waits for the open editor to hold the other person's word. */
const theirWordStaysWithinMs = 30_000;

/** How long the row below holds the socket after the document's own draw is on the screen.
 *  Well past the moment the page's own end is reached - the draw is a screenful and a few
 *  steps of 100 ms exhaust it - so a window that closed there closed inside this wait. */
const heldAfterTheDrawMs = 20_000;

/** The leg that runs this file's three rows alone, one worker on a machine of its own: the one
 *  leg the first row runs in, the one whose step the second and third rows' waits were derived
 *  for (`theRoomTheLegLeaves`), and the one the last row holds its ratio in (the comment above
 *  that row says why). Every other leg these rows run in is a `chromium` shard. */
const thePhonesLeg = "iphone-long-build-alone";

/**
 * The most a wait of the second or third row may take while the page keeps moving: in the
 * phone's leg the bound the caller names, which that leg's step was derived from, and in a
 * shard leg whatever is left of the row's one deadline, `theShardsRowWithinMs` from the moment
 * this is called. Call it at the row's start.
 *
 * Never less than a millisecond. The third row hands what this answers to an `expect.poll` as
 * its `timeout`, and Playwright reads a `timeout` of 0 there as no limit, which would leave a
 * wait begun after the deadline to run on to the test's own 1,800,000 ms. Every other wait it
 * bounds is `theMomentThePageReached`'s timer, for which 0 and 1 both mean at once.
 */
function theRoomTheLegLeaves(testInfo: TestInfo): (phonesLegMs: number) => number {
  if (testInfo.project.name === thePhonesLeg) {
    return (phonesLegMs) => phonesLegMs;
  }
  const endsBy = Date.now() + theShardsRowWithinMs;
  return () => Math.max(1, endsBy - Date.now());
}

/** What each of the three waits on the page counts, how long it lets the page go without one,
 *  and what it calls the stall it fails by. */
const theOpeningStopped = {
  stalledAfterMs: thePageStallsAfterMs,
  stopped: "the page stopped opening its document before the editor's first block",
  what: "step of its opening",
};
const theBuildStalled = {
  stalledAfterMs: thePageStallsAfterMs,
  stopped: "the editor's build stalled",
  what: "batch of its blocks",
};
const theDrawStopped = {
  stalledAfterMs: theDrawStallsAfterMs,
  stopped: "the page stopped drawing the document before it was whole",
  what: "step of its own draw",
};

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
    testInfo.project.name !== thePhonesLeg,
    "the measurement set-up is the phone's viewport, touch and Chromium, which is " +
      `${thePhonesLeg}'s`
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

/** Where the editor was when another person's word first showed up in its first block: how
 *  many blocks it held, and whether its build was over by then. */
interface TheirWordLanded {
  readonly blocks: number;
  readonly open: boolean;
}

declare global {
  interface Window {
    /** What the page says in the frame its editor's first block first holds the other
     *  person's word, so the row waits for that frame rather than reading before it
     *  (`whenTheirWordLands`). */
    __theirWordLanded?: (landed: TheirWordLanded) => void;
  }
}

/**
 * Finds the first animation frame in which the editor's own first block holds `word`, with
 * how many blocks the editor held then and whether its build was over, and says so in that
 * frame: what it answers is a wait for that word, which fails by name where the editor never
 * held it inside `timeoutMs`.
 *
 * Only the first block is read, because that is the block the other person writes in and
 * reading the whole of a 406,000-character document every frame would be the thing slowing the
 * page down. The loop does not stop when the editor is named: a word that lands only once the
 * build is over is the failure this row is about, and the row says so rather than hearing
 * nothing. It is said rather than read for the same reason: that frame comes after the editor
 * is open, so a reading taken when the row heard the editor was open would find no word and
 * fail as though it had never landed.
 */
async function whenTheirWordLands(
  page: Page,
  word: string
): Promise<(timeoutMs: number) => Promise<TheirWordLanded>> {
  const landed = Promise.withResolvers<TheirWordLanded>();
  await page.exposeFunction("__theirWordLanded", (where: TheirWordLanded) => {
    landed.resolve(where);
  });
  await page.addInitScript(
    ({ live, named, wanted }: { live: string; named: string; wanted: string }) => {
      const look = () => {
        const root = document.querySelector(live);
        const first = root === null ? null : root.querySelector("[data-block-id]");
        if (root !== null && first !== null && (first.textContent ?? "").includes(wanted)) {
          window.__theirWordLanded?.({
            blocks: root.querySelectorAll("[data-block-id]").length,
            open: document.querySelector(named) !== null,
          });
          return;
        }
        requestAnimationFrame(look);
      };
      requestAnimationFrame(look);
    },
    { live: liveEditorRoot, named: namedEditor, wanted: word }
  );
  return (timeoutMs) =>
    theMomentThePageReached(
      landed.promise,
      timeoutMs,
      "the editor never held the word the other person wrote"
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
}, testInfo) => {
  test.setTimeout(900_000);
  const within = theRoomTheLegLeaves(testInfo);
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
    const moving = await watchThePageMove(page);
    const theirWord = await whenTheirWordLands(page, word);
    await page.goto(path, { waitUntil: "commit" });

    // He writes one word into the document's first block, which Alice's first batch has
    // already built, the moment she has that batch: it is in her Yjs document as the insert
    // returns, and the only thing between it and her editor is the socket message that carries
    // it.
    const first = await whileThePageMoves(
      reached.firstBlock(within(firstBlockTimeoutMs)),
      moving,
      () => true,
      theOpeningStopped
    );
    writer.write(word);

    // The end of her build, and her page's word on where his landed, inside one deadline: the
    // frame that records the word is an animation frame of her page, and it can come after the
    // moment this row hears her editor is open - always, for a word that lands only once the
    // build is over, which is the failure this row is about.
    const buildWithinMs = within(buildTimeoutMs);
    const endBy = Date.now() + buildWithinMs;
    const opened = await whileThePageMoves(
      reached.editorOpened(buildWithinMs),
      moving,
      (step) => step === "batch",
      theBuildStalled
    );
    const landed = await theirWord(endBy - Date.now());
    const whole = await blocksBuilt(page);
    console.log(
      `R11 their edit during the build · the word reached the editor with ` +
        `${landed.blocks} of ${whole} blocks built, build ` +
        `${landed.open ? "over" : "running"}, editor open at ${Math.round(opened.reached)} ms; ` +
        `longest quiet before the first block ${first.longestQuiet}, in the build ` +
        `${opened.longestQuiet}.`
    );

    expect(landed.blocks).toBeGreaterThan(0);
    expect(
      landed.open,
      "the word reached the editor only once its build was over: another person's edit waits " +
        "for the whole build before the reader sees it"
    ).toBe(false);
    expect(
      landed.blocks,
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
// in one run.
//
// The row holds that ratio in `iphone-long-build-alone` alone (`thePhonesLeg`), and in a
// `chromium` shard leg it takes the same two builds through the same waits and prints the
// ratio without holding it. The claim is about the page, and only a reading the load is no part
// of says anything about the page: this tree moved R11's measuring rows to
// `iphone-measure-alone` for the same reason. Beside the full lane's shard 3 of 4 with three
// workers on two CPUs, the ratio of two builds read the shard's other workers as well as the
// page, whatever shape the readings took (dispatch://INFRA-341, R16): one pair failed it in 1 run
// of 50 (3.49 times); the medians of three pairs taken in turn in none of 50, with 5 of the 50
// past the row's deadline; the fastest of two pairs in 3 of 50 (up to 3.22); and one pair after
// a warm-up build in 6 of 50 (up to 3.26), the warm-up slower than the screenful build after it
// in all 50. The phone's leg runs one worker alone on a machine of its own, so what its two
// builds read is the page.

/** How many times longer the build over a page holding the whole drawn document may take than
 *  the build over a page holding one screenful. A page carrying the whole draw beside the
 *  editor carries about twice the boxes for the browser to lay out on its way through, which
 *  no build can give back; what this rules out is the quadratic above, which was seven times
 *  on this pod. Measured at 1.6 to 2.2 times over the runs this row was written against. */
const theSameRangeTimes = 3;

/** What a whole build the row stopped waiting for ends its wait with, through
 *  `whileThePageMoves`: it had run past the bar the row holds it to, as long as `atLeastMs`. */
class TheBuildIsPastTheBar extends Error {
  readonly atLeastMs: number;

  constructor(atLeastMs: number) {
    super(`the build had run past the bar, ${atLeastMs} ms into it`);
    this.atLeastMs = atLeastMs;
  }
}

/** How long one editor's build took, on the page's own clock, and how long this side went
 *  without a step of what each wait counted: the readings `thePageStallsAfterMs` is sized from,
 *  printed by the row so each run adds to them. */
interface TheBuildsTime {
  /** When the reader first had a block of the editor's own in the viewport, which is its
   *  first batch. */
  readonly firstBlockMs: number;
  /** When the editor opened: its last batch in and its binding live; `undefined` for a build the
   *  row gave up. */
  readonly openedMs: number | undefined;
  /** The build itself: the time between the two. For a build the row gave up, the least it
   *  took: from its first block to the last batch this side heard land, since the editor cannot
   *  have opened before its last batch. */
  readonly buildMs: number;
  /** Whether the row stopped waiting for this build once it had run past its bar. */
  readonly givenUp: boolean;
  /** On this side's clock, from the row asking for the build until the build was over, which
   *  the row prints with it: the room a build takes of the leg, beside the build's own time. */
  readonly tookMs: number;
  /** The longest gap between steps of the page's own draw, over the wait for it to be whole;
   *  `undefined` for a build whose socket is not held, which waits for no draw. */
  readonly drawQuiet: string | undefined;
  /** The longest gap between steps of the page's opening, over the wait for the first block. */
  readonly openingQuiet: string;
  /** The longest gap between batches, over the wait for the editor to open, the last batch's
   *  way to that opening included; `undefined` for a build the row gave up. */
  readonly buildQuiet: string | undefined;
}

/**
 * Opens `path` on a page of its own and answers how long the editor's build took there.
 *
 * With `overTheWholeDraw`, the document's socket is held until the page's own draw holds that
 * many of the document's top-level blocks, so the build runs over a page already holding the
 * whole document; without it the build runs over the screenful the draw has drawn by then,
 * which is what a reader whose socket answers at once gets. Both numbers are announcements
 * the page makes, so neither is a reading this side takes. Each wait goes on while the page
 * keeps moving (`whileThePageMoves`), for what `within` leaves it at the most
 * (`theRoomTheLegLeaves`). With `givesUpPastMs`, the build is given up once a batch lands later
 * than that after its first block, and answered with how long it had at least run.
 */
async function theBuildsTime(
  browser: Browser,
  path: string,
  {
    givesUpPastMs,
    overTheWholeDraw,
    within,
  }: {
    givesUpPastMs?: number;
    overTheWholeDraw?: number;
    within: (phonesLegMs: number) => number;
  }
): Promise<TheBuildsTime> {
  const asked = Date.now();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The socket is held before the page is watched: `watchTheDocument`'s wrap of the page's
    // WebSocket says the socket's steps `watchThePageMove` hears, and it has to stand in front of
    // the WebSocket `documentTransport` puts where the page's stood, not behind it.
    const transport =
      overTheWholeDraw === undefined ? undefined : await documentTransport(page, { holding: true });
    const reached = await watchTheDocument(page);
    const moving = await watchThePageMove(page);
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
    let drawQuiet: string | undefined;
    if (transport !== undefined) {
      const drawn = await whileThePageMoves(
        expect
          .poll(() => page.locator(drawnBlocks).count(), {
            intervals: [1_000],
            message: "the page never drew the whole document, so no build ran over one",
            timeout: within(theWholeDrawWithinMs),
          })
          .toBeGreaterThanOrEqual(overTheWholeDraw ?? 0),
        moving,
        (step) => step === "draw",
        theDrawStopped
      );
      drawQuiet = drawn.longestQuiet;
      await throttleTheCpu(page);
      await transport.release();
    }
    const first = await whileThePageMoves(
      reached.firstBlock(within(firstBlockTimeoutMs)),
      moving,
      () => true,
      theOpeningStopped
    );
    const firstHeardAt = Date.now();
    const givingUp = Promise.withResolvers<never>();
    // Looked at once a second because a batch lands every few hundred milliseconds to a few
    // seconds under load, so a second is the most this side lets a build run past its bar.
    const looking =
      givesUpPastMs === undefined
        ? undefined
        : setInterval(() => {
            const lastBatch = moving.steps.filter(({ step }) => step === "batch").at(-1);
            const atLeastMs = Math.max(0, (lastBatch?.at ?? firstHeardAt) - firstHeardAt);
            if (atLeastMs > givesUpPastMs) {
              givingUp.reject(new TheBuildIsPastTheBar(atLeastMs));
            }
          }, 1_000);
    const read = { drawQuiet, firstBlockMs: first.reached.at, openingQuiet: first.longestQuiet };
    try {
      const opened = await whileThePageMoves(
        reached.editorOpened(
          within(overTheWholeDraw === undefined ? buildTimeoutMs : theDrawnBuildTimeoutMs)
        ),
        moving,
        (step) => step === "batch",
        theBuildStalled,
        looking === undefined ? undefined : givingUp.promise
      );
      return {
        ...read,
        buildMs: opened.reached - first.reached.at,
        buildQuiet: opened.longestQuiet,
        givenUp: false,
        openedMs: opened.reached,
        tookMs: Date.now() - asked,
      };
    } catch (error) {
      if (!(error instanceof TheBuildIsPastTheBar)) {
        throw error;
      }
      return {
        ...read,
        buildMs: error.atLeastMs,
        buildQuiet: undefined,
        givenUp: true,
        openedMs: undefined,
        tookMs: Date.now() - asked,
      };
    } finally {
      clearInterval(looking);
    }
  } finally {
    await alice.close();
  }
}

/** One build as the row prints it: its time, how it opened and what the page went longest
 *  without on the way. */
function theReading(build: TheBuildsTime): string {
  const built = build.givenUp
    ? `over ${Math.round(build.buildMs)} ms (given up past the bar)`
    : `${Math.round(build.buildMs)} ms`;
  const opened = build.openedMs === undefined ? "never" : `${Math.round(build.openedMs)} ms`;
  const draw = build.drawQuiet === undefined ? "" : `draw ${build.drawQuiet}, `;
  return (
    `${built} (first block ${Math.round(build.firstBlockMs)} ms, editor open ${opened}, ` +
    `${Math.round(build.tookMs)} ms from the row asking for it; longest quiet: ${draw}opening ` +
    `${build.openingQuiet}, build ${build.buildQuiet ?? "given up"})`
  );
}

test(`${documentNames.large}'s editor builds over the whole drawn document in the time it takes over one screenful @phone`, async ({
  browser,
}, testInfo) => {
  test.setTimeout(1_800_000);
  const within = theRoomTheLegLeaves(testInfo);
  const { artifactId, path } = await seedDocument("specs-large.md", largeDocument);
  const whole = await topLevelBlocks(artifactId);
  expect(whole).toBeGreaterThan(900);

  // One screenful first, because that is the build the two measuring rows are held to: a pod
  // that has grown slower since would show it there before it showed it in the ratio. Where the
  // ratio is held, the whole build is given up once it has run past the bar, since it fails the
  // ratio whatever it would have gone on to take: a build that scales with the page ends on the
  // ratio rather than on its wait. Elsewhere it is waited for whole, and only printed.
  const holdsTheRatio = testInfo.project.name === thePhonesLeg;
  const screenful = await theBuildsTime(browser, path, { within });
  const drawn = await theBuildsTime(browser, path, {
    givesUpPastMs: holdsTheRatio ? screenful.buildMs * theSameRangeTimes : undefined,
    overTheWholeDraw: whole,
    within,
  });
  console.log(
    `R11 build time · over one screenful ${theReading(screenful)}; over the whole drawn ` +
      `document (${whole} blocks) ${theReading(drawn)}; ` +
      `${(drawn.buildMs / screenful.buildMs).toFixed(2)} times, against ${theSameRangeTimes}, ` +
      `${holdsTheRatio ? "held here" : `held in ${thePhonesLeg} alone, not here`}.`
  );

  expect(screenful.buildMs).toBeGreaterThan(0);
  expect(drawn.buildMs).toBeGreaterThan(0);
  if (holdsTheRatio) {
    expect(
      drawn.buildMs,
      "the editor took far longer to build over a page holding the whole drawn document than " +
        "over one holding a screenful, for the same blocks in the same batches: the build is " +
        "paying for what the page already holds once per batch again"
    ).toBeLessThanOrEqual(screenful.buildMs * theSameRangeTimes);
  }
});
