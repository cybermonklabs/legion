// What a large document costs a reader on a phone, measured under the set-up
// dispatch://INFRA-341/spec names: the `iphone` project under 4x CPU throttling, the time from
// the start of navigation until the editor's first visible block is in the viewport, and the
// longest main-thread task until the page has scrolled to the end. The two documents are
// `large-documents.ts`: one shaped like dispatch://AHXV6-1/artifact/intermediate-specs-md, and
// one five times as large.
//
// The two measuring rows fail when a target of "Targets for large documents" is missed: the
// editor's first visible block within 3 s for the smaller document and 5 s for the larger, and
// no main-thread task longer than 500 ms for either. That block is the one the reader can type
// in - the editor's own, while its root carries `data-write-path="live"` - and never the
// server's draw or the page's own in front of it, which each row reads off the timed block
// itself and holds it to (`large-documents-page.ts`). Those two draws are printed as numbers of
// their own, `first painted` and the page's own draw, against no target. Every number is
// printed whether or not its target was met, since a target met says nothing about how much
// room is left.
//
// One row below reads a main-thread task at a desktop width instead ("no task of the larger
// document's build holds a desktop page for half a second"). It is a regression guard rather
// than a target of the spec, since the set-up the spec names is the `iphone` project alone,
// which left a build whose longest task was 3.3 s at a desktop width against 460 ms on the
// phone read by nothing.
//
// The rest of acceptance A7 is here too, and each of those rows reads a build rather than a
// clock: a link to the decision block at the end of a document lands on that block, a reader
// answers that decision, and a comment body of twenty diagrams draws only the diagrams the
// reader has reached. Two rows of the draw that makes those targets reachable at all are here
// as well: the document on the screen before its editor exists, and the editor opening where
// its reader had got to. None of them carries throttling: what they prove is what the page
// does, not how long it takes.
//
// Which document a row seeds is in its title, and the larger one is seeded only where the row
// needs it: the measurement of it, the row that holds that measurement's window open over its
// build, the two rows that read what a build serves the rest of the page (a build of a few
// seconds hands over too few timers and carries too few of another person's words to say
// anything), the row that answers a decision inside a build, which needs a build still running
// when the page presses the card, the desktop guard above, the row whose claim is what a build
// costs over a page of that size, and the row that reads the reader's place half-way down it.
// Every other row seeds the AHXV6-1-shaped document, whose build costs about a fifth as much:
// what those rows read is an order, a place or a count, and 406,000 characters buy none of
// them anything.
import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import { LONGEST_HOLD_MS } from "../web/src/features/doc/first-batch";
import { subscribeSession } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  getArtifactBlocks,
  getArtifactText,
  listIssueAsks,
  patchIssue,
} from "./api";
import {
  connectedDot,
  documentEditor,
  documentTransport,
  editorBeingBuilt,
  placeCaret,
  selectTextIn,
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
  plainDocument,
  proseOnlyDocument,
  proseOpening,
  sectionHeading,
  smallDocument,
} from "./large-documents";
import {
  longTaskTargetMs,
  measureDocument,
  type PageRead,
  printMeasurement,
  readTheChrome,
  readTheReads,
  throttleTheCpu,
  watchTheChrome,
  watchTheDocument,
  watchTheReads,
} from "./large-documents-page";
import { openCommentThread, settledScrollTop, withoutScrollAnchoring } from "./mermaid-helpers";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** What one animation frame of the build held: how many of the document's top-level blocks
 *  stood on the screen across the three surfaces that draw them, how many of those the editor
 *  had built, and any block id two surfaces held at once. `framesOfTheBlocks` records one of
 *  these per frame and the count row reads them. */
interface BlockFrame {
  readonly blocks: number;
  readonly built: number;
  readonly twice: readonly string[];
}

/** What one task of the page drew: how many top-level blocks the editor gained in it, and how
 *  many decision cards were mounted into the shells of their blocks. A `MutationObserver`
 *  delivers every record of one task in one callback, at that task's own microtask checkpoint,
 *  so one of these is one task's work and nothing else's. `drawsOfEachTask` records them and
 *  the row below reads them. */
interface DrawnInATask {
  /** When the task's changes were read, counted from the start of navigation. */
  readonly at: number;
  readonly blocks: number;
  readonly cards: number;
}

/** What one task the page handed the browser while the editor was being built cost it: when
 *  the browser was given it, and how long it then waited before it ran. `whatTheBuildServes`
 *  records one of these per timer and per socket message, and the row below reads them. */
interface ServedLate {
  /** `performance.now()` when the browser was given the task: when `setTimeout` was called
   *  for a timer, and the message event's own `timeStamp` - the moment the browser made the
   *  event, which is when it queued the task that delivers it - for a socket message. */
  readonly handedAt: number;
  /** How many top-level blocks the editor held when it was handed over, for a timer the probe
   *  below hands over; `undefined` for a socket message, which arrives when it arrives. */
  readonly builtThen?: number;
  /** How long it waited from then until it ran. */
  readonly waitedMs: number;
}

/** The build's own window on the page's clock, as `whatTheBuildServes` sees it: the frame its
 *  first batch was found in, and the frame the finished editor was named in. */
interface TheBuildRan {
  firstBatchMs?: number;
  openedMs?: number;
}

/** Where the editor was when another person's word first showed up in its first block: how
 *  many blocks it held, and whether its build was over by then. */
interface TheirWordLanded {
  readonly blocks: number;
  readonly open: boolean;
}

/** What the page recorded of the answer it pressed while the editor was being built: the press
 *  the page received, how much of the document the editor held then, and whether the build was
 *  still running. */
interface TheAnswerPressed {
  /** The click event's own `timeStamp`, which the browser sets when it makes the event. */
  readonly at: number;
  /** How many top-level blocks the editor held when the press arrived. */
  readonly built: number;
  /** Whether the build was still running then: the finished editor had not been named. */
  readonly building: boolean;
}

declare global {
  interface Window {
    /** How many of the document's blocks the editor's root held at each animation frame, which
     *  `framesOfTheBuild` records and the two progressive-build rows read. */
    __blocksBuilt?: number[];
    /** What each animation frame of the build held, which `framesOfTheBlocks` records and the
     *  count row reads. */
    __blockFrames?: BlockFrame[];
    /** What the row that answers a decision inside a build recorded: the press the page made
     *  in a frame of the build, and `performance.now()` when the request carrying the answer
     *  left the page. */
    __answerPressed?: TheAnswerPressed;
    __answerSentMs?: number;
    /** What each task of the build drew, which `drawsOfEachTask` records and the decision-card
     *  row reads. */
    __drawnInTasks?: DrawnInATask[];
    /** What the page made of the edits `readerEditsTheBuild` gave it, and the refusal where
     *  the editor would not take one. */
    __readerEdits?: EditMade[];
    __readerEditsRefused?: string;
    /** What the no-pixels row records about the words its reader is looking at: how far they
     *  ever moved on the screen, and whether the page above them ever changed height at all,
     *  which is what makes the row's answer mean anything. `started` is false where the row
     *  never found its heading. */
    __readersPlace?: { grew: boolean; moved: number; started: boolean };
    /** What the chrome's no-pixels row records about the document's own first block: where
     *  its top stood on the reader's screen when the row began, how far it ever moved from
     *  there, and whether the page above the reader ever changed height at all, which is what
     *  makes the row's answer mean anything. */
    __firstBlockPlace?: { grew: boolean; moved: number; started: number };
    /** What `whatTheBuildServes` recorded: how long each timer the page handed the browser
     *  and each message the document's socket received waited to be run, and the build's own
     *  window, which says which of them were handed over while it ran. */
    __servedTimers?: ServedLate[];
    __servedMessages?: ServedLate[];
    __theBuildRan?: TheBuildRan;
    /** Where the editor was when another person's word reached its first block
     *  (`whenTheirWordLands`). */
    __theirWord?: TheirWordLanded;
  }
}

const agent = {
  actor: { id: "e2e-large-documents-agent", kind: "session" as const },
  as: "agent" as const,
};

/**
 * Resolves once the server owes nothing this seed left it: no document settlement queued or
 * running, and no document owed a block index. A document of this size defers its block index
 * (`deferBlockIndexBlocks`, `docs/reference_excerpts.go`), and that work costs the same machine
 * the browser runs on, so a measurement taken while it runs measures the reconciliation too.
 * The same wait as `heavy-document-writes.e2e.ts`'s, against the one database this run holds.
 */
async function everythingOwedFinished(what: string): Promise<void> {
  await expect
    .poll(
      () =>
        sql(
          "select (select count(*) from doc_settlements_pending) || ' settlements, ' || " +
            "(select count(*) from document_block_versions where version is null) || ' block indexes'"
        ),
      {
        intervals: [500],
        message: `${what}: the write's own work never finished`,
        timeout: 180_000,
      }
    )
    .toBe("0 settlements, 0 block indexes");
}

/**
 * Writes `content` as an issue document and answers the issue it belongs to and the path a
 * reader opens it at.
 *
 * The issue's title and the document's file name are both short, and both rows use names of the
 * same length. At 390 px the document's first block begins 650 px down a 664 px viewport, and
 * every line either of them wraps to raises the header by about 30 px: a two-line title or file
 * name puts the first block under the fold, where it never comes into the viewport without a
 * scroll and there is no first number to read. Which document a row measures is in the row's
 * own title, not in what it seeds.
 */
async function seedDocument(
  name: string,
  content: string
): Promise<{ artifactId: string; issueKey: string; path: string }> {
  await resetDatabase();
  await createProject({ key: "BIG", name: "Big documents" });
  const issue = await createIssue({ project: "BIG", title: "A document" }, agent);
  const uploaded = await createIssueArtifact(issue.key, { content, name }, agent);
  await everythingOwedFinished(name);
  return {
    artifactId: uploaded.artifact.id,
    issueKey: issue.key,
    path: `/issues/${issue.key}/artifacts/${uploaded.artifact.slug}`,
  };
}

interface Row {
  /** How long the row waits for the editor to open, which is the end of its build and the end
   *  of the window the longest task is read over. */
  readonly buildTimeoutMs: number;
  /** The document an agent writes. */
  readonly content: string;
  /** How many diagrams it holds, which the row proves before it measures anything. */
  readonly diagrams: number;
  /** The target the first block has to be in the viewport within. */
  readonly firstBlockTargetMs: number;
  /** How long the row waits for the first block. A build that draws it at all is read against
   *  the target; this is the wait for one that never draws it, so a failure reads as the miss it
   *  is rather than as a hang. */
  readonly firstBlockTimeoutMs: number;
  /** The file it is written under. */
  readonly name: string;
  /** The row's own timeout, since the suite sets none and Playwright's 30 s default is under
   *  what the seed, the throttled draw, the build and 120 scroll steps cost. */
  readonly timeoutMs: number;
  /** What the row calls it in the numbers it prints. */
  readonly what: string;
}

const rows: readonly [Row, Row] = [
  {
    buildTimeoutMs: 240_000,
    content: smallDocument,
    diagrams: documentDiagrams.small,
    firstBlockTargetMs: 3_000,
    firstBlockTimeoutMs: 120_000,
    name: "specs-small.md",
    timeoutMs: 300_000,
    what: documentNames.small,
  },
  {
    buildTimeoutMs: 480_000,
    content: largeDocument,
    diagrams: documentDiagrams.large,
    firstBlockTargetMs: 5_000,
    firstBlockTimeoutMs: 240_000,
    name: "specs-large.md",
    timeoutMs: 900_000,
    what: documentNames.large,
  },
];

for (const row of rows) {
  test(`measures ${row.what} on a phone under 4x CPU throttling @phone`, async ({
    browser,
  }, testInfo) => {
    test.skip(
      testInfo.project.name !== "iphone-alone",
      "the measurement set-up is the iphone viewport, touch and Chromium, and the number is read from the wall clock, so it runs in `iphone-alone` with the machine to itself (e2e/playwright.config.ts's `clockRows`)"
    );
    test.setTimeout(row.timeoutMs);
    // The document the run measures is the one this module spells, so a change to the generator
    // that moved either size would be read here rather than in a number that quietly moved.
    expect(diagramsIn(row.content)).toBe(row.diagrams);
    const { path } = await seedDocument(row.name, row.content);

    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      const measured = await measureDocument(page, path, {
        buildTimeoutMs: row.buildTimeoutMs,
        firstBlockTimeoutMs: row.firstBlockTimeoutMs,
      });
      printMeasurement(
        `${row.what}, ${row.content.length} characters, ${row.diagrams} diagrams`,
        measured,
        row.firstBlockTargetMs
      );

      // The number the targets are read against is the editor's block, not the draw in front of
      // it: the block that was timed stood in an editor root carrying `data-write-path="live"`,
      // it was inside an editable host, and it was in neither the blocks the server drew nor
      // the page's own draw of the document's text. A rig reading the preview's block fails
      // here rather than printing that draw as the editor's.
      expect(measured.firstBlock).toEqual({
        editable: true,
        inLiveEditor: true,
        inPreview: false,
      });

      // What a measured number needs to mean anything: the document really drew, the editor
      // really opened, and the page really scrolled afterwards, so the longest task covers the
      // whole build and then a scroll through the whole document.
      await expect(documentEditor(page)).toBeVisible();
      expect(measured.firstBlockMs).toBeGreaterThan(0);
      expect(measured.editorOpenedMs ?? 0).toBeGreaterThan(0);
      expect(measured.windowClosedMs).toBeGreaterThan(measured.editorOpenedMs ?? 0);
      expect(measured.scrollSteps).toBeGreaterThan(0);
      expect(measured.longestTaskMs).toBeGreaterThan(0);
      // And the editor's block came no earlier than either draw in front of it: the first block
      // on the screen, which is the server's own where it drew one, and the first block the
      // page's own code drew, which is the preview's and which this rig read as the editor's
      // until INFRA-341. A number at or under that one is the preview's block timed again.
      expect(measured.firstBlockMs).toBeGreaterThanOrEqual(measured.firstPaintedMs ?? 0);
      expect(measured.firstBlockMs).toBeGreaterThan(measured.pageDrawnMs ?? 0);

      expect(measured.firstBlockMs).toBeLessThanOrEqual(row.firstBlockTargetMs);
      expect(measured.longestTaskMs).toBeLessThanOrEqual(longTaskTargetMs);
    } finally {
      await alice.close();
    }
  });
}

/** How long the row below holds the socket after the document's own draw is on the screen.
 *  Well past the moment the page's own end is reached - the draw is a screenful and a few
 *  steps of 100 ms exhaust it - so a window that closed there closed inside this wait. */
const heldAfterTheDrawMs = 20_000;

// What the two rows above read their longest task over has to cover the build, and the page is
// not the document until the editor is in it: the draw in front of the reader is the blocks the
// document opens with (`large-documents-page.ts`'s module comment), so a window that ended
// where that page ends ended four scroll steps in, with most of the build still to come. This
// row holds the document's socket, so the editor is built only when it says so, and reads
// whether the measurement closed its window meanwhile.
//
// It is also the one row whose build the measurement could not read through, which is what
// `listenForThePage` in `large-documents-page.ts` is there for: holding the socket this long
// leaves the reader's own draw of the whole document on the page - every one of its 986 blocks
// and 140 diagrams - so the editor builds over all of it and takes minutes, where the two rows
// above pay about 35 s for the same build over a page holding one screenful. From a build's
// first batch to its last the page answers nothing the run asks it, so a number the run had to
// read was a number this row could not have until the build was over, and the wait for the
// editor's first block ran out on a block the page had recorded three seconds into it. The
// numbers are announcements the page makes now, which the rows above never needed.
test(`the measurement's window stays open until ${documentNames.large}'s editor opens @phone`, async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the measurement set-up is the iphone project's viewport, touch and Chromium"
  );
  test.setTimeout(900_000);
  const { path } = await seedDocument("specs-large.md", largeDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page, { holding: true });
    const measuring = measureDocument(page, path, {
      buildTimeoutMs: 480_000,
      firstBlockTimeoutMs: 240_000,
    });
    let closed = false;
    const noteClosed = () => {
      closed = true;
    };
    void measuring.then(noteClosed, noteClosed);

    // The reader has the document's own draw, and no editor is being built behind it.
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );
    await expect(documentEditor(page)).toHaveCount(0);
    await page.waitForTimeout(heldAfterTheDrawMs);
    expect(
      closed,
      "the measurement closed its window with no editor on the page: it read the longest task " +
        "of the draw's own screenful and none of the build"
    ).toBe(false);

    // Released, the build runs, the editor opens, and only then does the window close.
    await transport.release();
    const measured = await measuring;
    expect(measured.editorOpenedMs ?? 0).toBeGreaterThan(heldAfterTheDrawMs);
    expect(measured.windowClosedMs).toBeGreaterThan(measured.editorOpenedMs ?? 0);
    // And what it scrolled was the editor's own page, not the draw's screenful.
    expect(measured.scrollSteps).toBeGreaterThan(10);
  } finally {
    await alice.close();
  }
});

test(`a link to the decision block at the end of ${documentNames.small} lands on that block @phone`, async ({
  browser,
}) => {
  test.setTimeout(600_000);
  const block = decisionBlockId(decisionBlocks);
  // The AHXV6-1-shaped document: the last of the five decision blocks sits in the last section
  // of either document, and where the page ends up is the same question over 81,000 characters
  // as over 406,000.
  const { path } = await seedDocument("specs-small.md", smallDocument);

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
    await expect(linked).toBeInViewport({ timeout: 240_000 });
    await settledScrollTop(page);
    await expect(linked).toBeInViewport();
  } finally {
    await alice.close();
  }
});

// The two rows below are about the draw that makes the numbers above reachable at all: the
// document is drawn from its own markdown, through the read-only headless engine, before the
// editor exists (`web/src/features/doc/DocumentPreview.tsx`). Each holds the document's socket
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

/** How many of the document's blocks the editor's root holds, at whatever depth. The view wears
 *  its `Document editor` name only once the build is over, and its root is hidden until then,
 *  so neither `documentEditor` nor a visibility check reads a build in progress. */
function blocksBuilt(page: Page): Promise<number> {
  return page.locator(".proof-editor [data-block-id]").count();
}

/**
 * Records, every animation frame until the editor is named, how many of the document's blocks
 * the editor's root holds, and answers what was recorded.
 *
 * The reading is the page's own rather than a poll from here, because a poll reads the build
 * from outside it: a `page.evaluate` is one task among the page's own, and the count it comes
 * back with is whatever the batches had reached by the time that task ran, never the frame a
 * reader was shown. A frame is the reading a reader gets, and a frame that holds part of a
 * document is the proof this row wants, since a build that froze the page would have drawn no
 * frame at all. Install before the navigation; read once the editor is open.
 */
async function framesOfTheBuild(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const built: number[] = [];
    window.__blocksBuilt = built;
    const look = () => {
      const root = document.querySelector(".proof-editor");
      if (root !== null) {
        built.push(root.querySelectorAll("[data-block-id]").length);
        // The last frame worth reading is the first one the finished editor is named in: after
        // it every frame would pay for a scan of the whole document and read the same number.
        if (root.querySelector('[aria-label="Document editor"]') !== null) return;
      }
      requestAnimationFrame(look);
    };
    requestAnimationFrame(look);
  });
}

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
    await throttleTheCpu(page);
    await framesOfTheBuild(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    // The document's own draw is what the reader has while no socket has synced, and no editor
    // has been built from it yet.
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );
    expect(await blocksBuilt(page)).toBe(0);

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
    await throttleTheCpu(page);
    await framesOfTheBuild(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );

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

/** Each surface a top-level block of the document can stand on the screen in, in the order the
 *  page holds them: the blocks the server drew into the page it sent
 *  (`web/src/features/doc/first-screen.ts`), the page's own draw of the document's text, and
 *  the editor, whose root stands before that draw while the build fills it from the top. */
const serverDrawnBlocks = "#first-screen > * > [data-block-id]";
const drawnBlocks = '[aria-label="Document text"] > [data-block-id]';
const builtBlocks = '.proof-editor [contenteditable="true"] > [data-block-id]';

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

/**
 * How many top-level blocks the server's index of `artifactId` holds: every block whose range
 * no other block's range contains. That index is the server's own parse of the document, so the
 * count is the document's, not this module's reading of the generator that spelled it, and it is
 * the number of blocks each draw of the document draws.
 */
async function topLevelBlocks(artifactId: string): Promise<number> {
  const blocks = await getArtifactBlocks(artifactId);
  return blocks.filter(
    (block) =>
      !blocks.some(
        (other) =>
          other.id !== block.id &&
          other.from <= block.from &&
          other.to >= block.to &&
          (other.from < block.from || other.to > block.to)
      )
  ).length;
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
    await throttleTheCpu(page);
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

/** What the page recorded for one of those edits: what it did, how many blocks the editor held
 *  when it did it - which is what says the build was still running - and how many it held
 *  afterwards, which says what the edit did to the document's shape. */
interface EditMade {
  readonly after: number;
  readonly built: number;
  readonly what: string;
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
    await throttleTheCpu(page);
    await readerEditsTheBuild(page, typing);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );

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
    const bobPage = await bob.newPage();
    await bobPage.goto(path);
    await expect(documentEditor(bobPage)).toContainText(title, { timeout: 240_000 });
    await expect(connectedDot(bobPage)).toHaveText("connected");
    await placeCaret(bobPage, "after", title);

    const page = await alice.newPage();
    await throttleTheCpu(page);
    await framesOfTheBuild(page);
    await readerEditsTheBuild(page, reading);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );

    // Bob writes from the moment Alice's socket is let through: a word in a block her batches
    // have built, a block inserted inside that built run, and that block taken out of it again
    // over the prose it was inserted above.
    await transport.release();
    await placeCaret(bobPage, "after", title);
    await bobPage.keyboard.type(" Pangolin.");
    await placeCaret(bobPage, "after", sectionHeading(0));
    await bobPage.keyboard.press("Enter");
    await bobPage.keyboard.type("Otter paragraph.");
    await selectTextIn(bobPage, documentEditor(bobPage), `Otter paragraph.${proseOpening(0)}`);
    await bobPage.keyboard.press("Backspace");

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
    // And Bob's writing crossed that build too: the page drew frames holding part of the
    // document while he was writing.
    const frames = await page.evaluate(() => window.__blocksBuilt ?? []);
    expect(
      frames.filter((held) => held > 0 && held < whole),
      "no frame was drawn holding part of the document, so Bob's writing crossed no build"
    ).not.toEqual([]);

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

// The next three rows are about everything on the page that is not the build, for as long as
// the build lasts. A batch hands the thread back so the page stays usable, and a hand-back the
// page is served nothing in is no hand-back at all: the build used to resume through
// `scheduler.yield()`, whose continuation the browser serves ahead of any posted task, so from
// a build's first batch to its last no socket message, no fetch callback and no timer ran - and
// React, whose scheduler posts one, drew nothing. Measured on this pod, `iphone` at 4x CPU
// throttling, a `page.evaluate` poll was answered twice and then not again for 209 s, and
// `scheduler.postTask()` at its default priority served a socket message in 7 ms while a
// timer waited 19,945 ms. It hands the thread back as a timer now (`endOfTask` in
// `packages/proof-editor/src/progressive-build.ts`), which queues the continuation behind
// everything already waiting, and the three rows are the three things a reader loses while
// nothing else runs: the page's own clock, a decision they answer, and another person's edit.

/** The longest a message on the document's socket may wait to be served while the editor is
 *  built. The same half-second R11 holds a main-thread task to: Chromium runs a networking
 *  task ahead of a timer, so a socket message waits only for whatever the thread is in the
 *  middle of, and the worst one measured across a build was 45 ms. */
const aMessageWaitsAtMostMs = 500;

/** How long a timer the page sets may wait while the editor is built.
 *
 *  A flat number, not a multiple of the build's own longest tasks. This row used to hold the
 *  timer to two of them and 5,000 ms more, which recorded a symptom as a limit: a build whose
 *  tasks were long bought itself a looser bound. What a timer waits for is one turn of the
 *  page's loop - the task in flight when it was handed over, then the browser laying out and
 *  painting the document, which is not a task and so is in no main-thread task number, and
 *  then, where the next batch's own timer was queued first, that batch and its paint as well -
 *  so what this holds is the turn.
 *
 *  Half a second is not what a turn costs, and the measurement is why. Traced per animation
 *  frame through a whole build, the browser's own `Layout` and `Paint` are flat in the blocks
 *  built - 10 to 40 ms and 4 to 8 ms a frame at 8 blocks built and at 986 - so a turn is made
 *  of the tasks around it rather than of the document's size, and this row's worst wait is
 *  always the same turn: the timer handed over in the last frame of the batches, served after
 *  the binding has adopted the document, the editor has been revealed and the margin has
 *  placed its cards over it for the first time, which is four or five tasks of 150 to 660 ms
 *  one after another. Each of those is inside the half-second a reader feels as a freeze,
 *  which is what `aDesktopBuildTaskAtMostMs` below and the two measuring rows at the top of
 *  this file hold; their sum is not, and their sum is what this number has to cover.
 *
 *  Measured on this pod at 4x CPU throttling, one timer outstanding at a time, over the
 *  406,319-character document with a second reader writing into it throughout, across six
 *  runs: through the batches the worst turn is 1,001 ms on `chromium` and 1,114 ms on
 *  `iphone`; the turn at the swap is 1,480 to 2,954 ms with the pod otherwise quiet, and
 *  3,923 and 5,108 ms in the two runs taken while the whole browser suite ran beside this
 *  one - the same load that took the phone's own 500 ms task target to 723 and 1,114 ms in
 *  the two measuring rows at the top of this file. So 6,000 ms.
 *
 *  The number is flat, and that is the change. It used to be two of the build's own longest
 *  main-thread tasks and 5,000 ms more, which came to 6,016, 6,086, 6,138 and 6,318 ms in
 *  those same runs: a build whose tasks were long bought itself a looser bound, so the row
 *  recorded a symptom as a limit and could not have failed on the thing it is named for.
 *
 *  The two whole-document layouts that used to stand in those turns are gone, and they are
 *  what the old bound was built around: a read of every `data-block-id` of the editor, most of
 *  them nested inside a top-level block the browser was skipping, forced each of those blocks
 *  to render one at a time (`web/src/features/doc/marks.ts`), and the document stopped being
 *  skipped at all the moment anything was placed beside it (`web/src/styles.css`). Those cost
 *  4.4 s and 1.8 s of layout in one task at a desktop width, and the worst turn measured here
 *  before they were fixed was 12,140 ms. */
const aTimerWaitsAtMostMs = 6_000;

/** How much build each timer served stands for, at worst: a page whose clock keeps running
 *  serves far more than this, and a page that served one timer and then nothing until the
 *  build ended serves far fewer. */
const aTimerEveryMs = 2_000;

/** The editor a reader already has while the rest of its blocks are built, as a selector an
 *  init script can carry (`editorBeingBuilt` is the same root as a locator). */
const liveEditorRoot = '.proof-editor[data-write-path="live"]';

/** The finished editor's own name, which the host writes once the binding has answered
 *  its handle - a task after the binding connected - so an editor wearing it is one whose
 *  build is over (`web/src/features/doc/editor.ts`). */
const namedEditor = '[role="textbox"][aria-label="Document editor"]';

/** How often the probe below hands the browser a timer. Often enough that a build of a few
 *  seconds is covered by a dozen of them, rarely enough that the probe is not itself the thing
 *  keeping the page busy. */
const timerEveryMs = 250;

/**
 * Records what the page's own posted tasks cost it while the editor is being built: every
 * timer the page hands the browser, every message the document's socket receives, and the
 * build's own window, so the row can read the tasks handed over inside it.
 *
 * A timer is handed over from an animation frame, because a frame is the one thing that does
 * run through a starved build and so the one place a probe can be handed over from. A socket
 * message is timed from the message event's own `timeStamp`, which the browser sets when it
 * makes the event - that is, when it queues the task that delivers it - so the difference is
 * what the page's own thread made that message wait and nothing of the network's. The socket
 * is wrapped as a `construct` proxy rather than a subclass for the reason
 * `large-documents-page.ts` gives: the page's transport subclasses `globalThis.WebSocket` too.
 *
 * Install before the navigation; read once the editor has opened.
 */
async function whatTheBuildServes(page: Page): Promise<void> {
  await page.addInitScript(
    ({ every, live, named }: { every: number; live: string; named: string }) => {
      const timers: ServedLate[] = [];
      const messages: ServedLate[] = [];
      const ran: TheBuildRan = {};
      window.__servedTimers = timers;
      window.__servedMessages = messages;
      window.__theBuildRan = ran;
      window.WebSocket = new Proxy(window.WebSocket, {
        construct(made, args: [string | URL, (string | string[])?], asked) {
          const socket = Reflect.construct(made, args, asked) as WebSocket;
          if (!String(args[0]).includes("/ws/doc/")) {
            return socket;
          }
          socket.addEventListener("message", (event) => {
            messages.push({
              handedAt: event.timeStamp,
              waitedMs: performance.now() - event.timeStamp,
            });
          });
          return socket;
        },
      });
      let handedLast = Number.NEGATIVE_INFINITY;
      /** Whether a timer the probe handed over has still to run. One at a time, because what
       *  this row is about is how long the page waits for a timer, not how deep a queue of
       *  them grows: a frame of this page costs hundreds of milliseconds, so a probe handing
       *  one over every frame outruns the browser and measures its own backlog. A reader's
       *  page posts one and waits for it - React's scheduler, and the one-card-per-task timer
       *  of `useAskCardsDrawnInTasks` - which is what this does too. */
      let owed = false;
      const look = () => {
        const now = performance.now();
        const root = document.querySelector(live);
        if (ran.firstBatchMs === undefined && root !== null) {
          ran.firstBatchMs = now;
        }
        if (!owed && now - handedLast >= every) {
          const handedAt = now;
          handedLast = now;
          owed = true;
          // How far the build had got, read as the top-level child count of the editable host
          // rather than a query over the whole document, so the probe costs the page nothing.
          const built = root?.querySelector('[contenteditable="true"]')?.childElementCount ?? 0;
          setTimeout(() => {
            owed = false;
            timers.push({ builtThen: built, handedAt, waitedMs: performance.now() - handedAt });
          }, 0);
        }
        if (document.querySelector(named) !== null) {
          ran.openedMs = now;
          return;
        }
        requestAnimationFrame(look);
      };
      requestAnimationFrame(look);
    },
    { every: timerEveryMs, live: liveEditorRoot, named: namedEditor }
  );
}

/** The tasks of `served` the browser was given while the build ran, which are the ones the
 *  build could have starved. A task handed over inside the window and run after it is one of
 *  them, which is why they are read by when each was handed over. */
function handedToTheBuild(served: readonly ServedLate[], ran: TheBuildRan): ServedLate[] {
  const from = ran.firstBatchMs ?? Number.POSITIVE_INFINITY;
  const to = ran.openedMs ?? Number.POSITIVE_INFINITY;
  return served.filter((task) => task.handedAt >= from && task.handedAt <= to);
}

/** The longest any of them waited, and 0 where there were none. */
function longestWait(served: readonly ServedLate[]): number {
  return served.reduce((worst, task) => Math.max(worst, task.waitedMs), 0);
}

test(`a page timer and a socket message are served while ${documentNames.large}'s editor is built @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { path } = await seedDocument("specs-large.md", largeDocument);
  const title = documentTitle(documentDiagrams.large);

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    // Bob has the document open with his caret in its first block before Alice loads hers, so
    // the only thing left for him to do while she builds is press the keys: every word he
    // types is a message on her socket.
    const bobPage = await bob.newPage();
    await bobPage.goto(path);
    await expect(documentEditor(bobPage)).toContainText(title, { timeout: 240_000 });
    await expect(connectedDot(bobPage)).toHaveText("connected");
    await placeCaret(bobPage, "after", title);

    const page = await alice.newPage();
    await throttleTheCpu(page);
    const reached = await watchTheDocument(page);
    await whatTheBuildServes(page);
    await page.goto(path, { waitUntil: "commit" });

    // From Alice's first batch until her editor opens, so his words cover her whole build
    // rather than a moment this row has to guess at. Both moments are announcements the page
    // makes rather than readings this side takes (`large-documents-page.ts`).
    //
    // He types without resting between words. He used to rest half a second, so that four
    // words a second would not go into a document of 986 blocks - a load no reader puts on
    // another - and that rest was written when a build of this document lasted thirteen
    // seconds. It does not any more: on a quiet machine the build is now about 1.3 s
    // (dispatch://INFRA-341, R11), and a writer who rests half a second gets two words into it,
    // which is fewer than this row needs to have proved anything. What bounded his rate was
    // never that rest anyway, but his own page: he holds the same 986-block document, and at a
    // desktop width each of his keystrokes costs a layout of it and of the margin beside it, so
    // on a loaded machine `Pangolin0` was three to four seconds of his. So the rest is gone and
    // the key delay stays: where the machine is slow his page still paces him, and where it is
    // fast his words cover a fast build.
    await reached.firstBlock;
    let open = false;
    const opened = reached.editorOpened.then((at) => {
      open = true;
      return at;
    });
    let words = 0;
    while (!open && words < 60) {
      await bobPage.keyboard.type(` Pan${words}`, { delay: 25 });
      words += 1;
    }
    const openedMs = await opened;

    const ran = (await page.evaluate(() => window.__theBuildRan)) ?? {};
    const everyTimer = (await page.evaluate(() => window.__servedTimers)) ?? [];
    const timers = handedToTheBuild(everyTimer, ran);
    const messages = handedToTheBuild(
      (await page.evaluate(() => window.__servedMessages)) ?? [],
      ran
    );
    const built = (ran.openedMs ?? 0) - (ran.firstBatchMs ?? 0);
    // Every timer of the load, not only the ones inside the build: what says whether a long
    // wait is the build's doing is where the long ones fall.
    const began = ran.firstBatchMs ?? 0;
    const spread = everyTimer
      .map(
        (timer) =>
          `${Math.round(timer.handedAt - began)}@${timer.builtThen ?? 0}:` +
          `${Math.round(timer.waitedMs)}`
      )
      .join(" ");
    // What one turn of the page's own loop cost while all that ran: a task the page was given
    // waits behind whatever the thread is in the middle of, so the longest of those says what
    // the waits above are made of.
    const tasks = ((await page.evaluate(() => window.__largeDocumentLongTasks)) ?? []).filter(
      (task) => task.at >= began && task.at <= (ran.openedMs ?? 0)
    );
    const longestTask = tasks.reduce((worst, task) => Math.max(worst, task.ms), 0);
    // Printed before any of them is read against its target, as every other number in this
    // file is: a run that missed one says what it measured rather than only what it wanted.
    console.log(
      `R11 posted tasks · a build of ${Math.round(built)} ms from its first batch, ` +
        `${timers.length} timer(s) handed over inside it one at a time, worst wait ` +
        `${Math.round(longestWait(timers))} ms (target ${aTimerWaitsAtMostMs} ms, and one ` +
        `served every ${aTimerEveryMs} ms of the build); ${messages.length} socket message(s), ` +
        `worst wait ${Math.round(longestWait(messages))} ms (target ${aMessageWaitsAtMostMs} ms). ` +
        `${tasks.length} main-thread task(s) over 50 ms inside the build, longest ` +
        `${Math.round(longestTask)} ms. The editor opened at ${Math.round(openedMs)} ms, with ` +
        `${words} word(s) written into it while it was built (target more than 3). ` +
        `Every timer of the load, as handed-over@blocks-built:waited, counted from the ` +
        `first batch: ${spread}`
    );

    expect(words, "Bob never wrote while Alice's editor was being built").toBeGreaterThan(3);

    // The window is a real build, read from what the build had done rather than from how long
    // it took: the first batch landed before the editor opened, and the editor's own top-level
    // child count rose while the page was serving the timers counted below, so those timers
    // were handed over while the batches were still arriving. A duration cannot say this, and
    // a floor under one would say the opposite of what this row is for - the build on a quiet
    // machine is now about 1.3 s, and a row that failed because the build was quick would be
    // asserting that the product stays slow.
    expect(ran.firstBatchMs ?? 0).toBeGreaterThan(0);
    const blocksBuilt = timers.map((timer) => timer.builtThen ?? 0);
    expect(
      timers.length,
      "the page served fewer than two of its own timers inside the build, so nothing here " +
        "shows the window held a build at all"
    ).toBeGreaterThanOrEqual(2);
    expect(
      Math.max(...blocksBuilt) - Math.min(...blocksBuilt),
      `the editor held ${Math.min(...blocksBuilt)} top-level blocks when the page served its ` +
        `first timer of the window and ${Math.max(...blocksBuilt)} when it served its last: ` +
        "the window did not cover the document being built"
    ).toBeGreaterThan(0);
    // And the page was given both kinds of task inside it, so neither number below is the
    // target met by nothing having happened.
    expect(messages.length, "no socket message arrived while the build ran").toBeGreaterThan(3);

    // The page's own clock ran right through the build: it was handed a timer, served it, was
    // handed the next, over and over. A build that served one and then nothing until it ended
    // is what this fails on.
    expect(
      timers.length,
      `the page served ${timers.length} timer(s) across a build of ${Math.round(built)} ms: ` +
        "its own clock stopped for the build rather than ticking through it"
    ).toBeGreaterThanOrEqual(Math.floor(built / aTimerEveryMs));
    expect(
      Math.round(longestWait(timers)),
      `a timer the page set during the build waited ${Math.round(longestWait(timers))} ms, ` +
        `with the longest main-thread task inside that build ${Math.round(longestTask)} ms: ` +
        "it is waiting for the build rather than for a turn of the page's own loop"
    ).toBeLessThanOrEqual(aTimerWaitsAtMostMs);
    expect(
      Math.round(longestWait(messages)),
      "a message on the document's socket waited longer than a reader feels as a freeze " +
        "while the editor was being built: another person's edit, or a reconnect, is held " +
        "for as long as the build lasts"
    ).toBeLessThanOrEqual(aMessageWaitsAtMostMs);
  } finally {
    await bob.close();
    await alice.close();
  }
});

/** The longest a main-thread task of a build may hold a page at a desktop width.
 *
 *  A regression guard, not a target of the spec: R11's targets are the phone's, and the two
 *  measuring rows at the top of this file read them under the set-up the spec names, which is
 *  the `iphone` project alone. That left the number below read nowhere at a desktop width, and
 *  a build whose longest task was 3.3 s there against 460 ms on the phone went unseen. Half a
 *  second is the number R11 calls a freeze, so it is the number a wider page is guarded by
 *  here too (dispatch://INFRA-341, R11).
 *
 *  Measured on this pod after the fix: 350, 406 and 416 ms over builds of 19 to 27 s with the
 *  pod otherwise quiet, against 2,680 ms before it. It is as load-sensitive as the two
 *  measuring rows and their own 500 ms, and for the same reason: run beside the whole browser
 *  suite, the worst task of a build reached 902 ms here while the phone's rows read 723 and
 *  1,114 ms for theirs. A miss is read the way theirs is - against what else the machine was
 *  doing - which is why the row prints every task of the build before it reads any of them. */
const aDesktopBuildTaskAtMostMs = 500;

// What cost a desktop width ten times a phone's longest task was not a batch. Traced per
// animation frame through a whole build (`devtools.timeline`, 4x CPU throttling): the browser's
// own `Layout` and `Paint` are flat in the blocks built - 10 to 40 ms and 4 to 8 ms a frame at
// 8 blocks built and at 986 - so the editor's top-level blocks were being skipped all along,
// and a batch cost 150 to 420 ms at a desktop width against 160 to 240 ms on the phone. What
// held the thread for seconds was the one task the margin's placements are first published in,
// which is the task the editor opens in and which only a desktop width reaches inside a build,
// since there the margin is beside the document from the page's first commit: 915 ms of style
// and 1,837 ms of layout to stop skipping the document, and 1.8 s of style and 4.4 s of layout
// to read the box of every `data-block-id` of a document that holds about 7,000 of them inside
// its 986 top-level blocks. Both are fixed where they were: the document's blocks are skipped
// whatever stands beside them (`web/src/styles.css`), and one box is read per top-level block
// (`web/src/features/doc/marks.ts`).
//
// This row is the guard on that. It carries no `@phone` tag, so it runs in the `chromium`
// project alone - the two measuring rows cover the phone, over a window that is wider still -
// and it reads the same `longtask` entries they do, over the build alone.
test(`no task of ${documentNames.large}'s build holds a desktop page for half a second`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { path } = await seedDocument("specs-large.md", largeDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await throttleTheCpu(page);
    const reached = await watchTheDocument(page);
    await whatTheBuildServes(page);
    await page.goto(path, { waitUntil: "commit" });
    // Both moments are announcements the page makes rather than readings this side takes, so
    // nothing of this run is on the thread the row is measuring (`large-documents-page.ts`).
    const openedMs = await reached.editorOpened;

    const ran = (await page.evaluate(() => window.__theBuildRan)) ?? {};
    const began = ran.firstBatchMs ?? 0;
    const tasks = ((await page.evaluate(() => window.__largeDocumentLongTasks)) ?? []).filter(
      (task) => task.at >= began && task.at <= (ran.openedMs ?? 0)
    );
    const longestTask = tasks.reduce((worst, task) => Math.max(worst, task.ms), 0);
    const spread = tasks
      .map((task) => `${Math.round(task.at - began)}:${Math.round(task.ms)}`)
      .join(" ");
    // Printed before it is read against its target, as every other number in this file is.
    console.log(
      `R11 desktop build · a build of ${Math.round((ran.openedMs ?? 0) - began)} ms from its ` +
        `first batch, ${tasks.length} main-thread task(s) over 50 ms inside it, longest ` +
        `${Math.round(longestTask)} ms (guard ${aDesktopBuildTaskAtMostMs} ms). The editor ` +
        `opened at ${Math.round(openedMs)} ms. Every task of the build, as ` +
        `began@from-the-first-batch:held: ${spread}`
    );

    // The window is a real build: its first batch landed, the editor opened after it, and the
    // page was held for something, so the number below is not a target met by nothing having
    // happened.
    expect(began).toBeGreaterThan(0);
    expect((ran.openedMs ?? 0) - began).toBeGreaterThan(5_000);
    expect(tasks.length, "no task of the build held the thread for 50 ms").toBeGreaterThan(3);

    expect(
      Math.round(longestTask),
      `a task of the build held this desktop page for ${Math.round(longestTask)} ms: a reader ` +
        "at this width meets a freeze the phone's rows would have failed on"
    ).toBeLessThanOrEqual(aDesktopBuildTaskAtMostMs);
  } finally {
    await alice.close();
  }
});

/** How long after the press the answer's request may take to leave the page. What stands
 *  between the two is the page's own work and none of the build's length: React's state for the
 *  pick, the card's render, and the write the press makes, each a posted task the build has to
 *  let through. Half a second is the span R11 calls a freeze, and it is what a build may not
 *  cost a reader who answers a decision while it runs. */
const theAnswerLeavesWithinMs = 500;

/**
 * Has the page itself answer the decision block `block` holds - picking `option` and pressing
 * the card's Answer button as soon as the build has drawn them - and records what it did: the
 * press the page received, how much of the document the editor held then, whether the build
 * was still running, and when the request carrying the answer left the page.
 *
 * The press is the page's own rather than this side's, for the reason `readerEditsTheBuild`
 * gives: an action driven from here is a round trip, and on a document this size Playwright's
 * own check of the radio and press of the button cost 10.9 s on this pod and 11 to 17 s on
 * GitHub's runners, where the whole build is 8.2 s. A row that presses from here therefore
 * reads whether two round trips beat the build, which says nothing about the reader. From a
 * frame the press lands inside the build, and the row says so rather than assuming it.
 *
 * Install before the navigation; read what it recorded off the page afterwards.
 */
async function readerAnswersTheBuild(
  page: Page,
  { answer, block, option }: { answer: string; block: string; option: string }
): Promise<void> {
  await page.addInitScript(
    ({
      answered,
      blockId,
      live,
      named,
      wanted,
    }: {
      answered: string;
      blockId: string;
      live: string;
      named: string;
      wanted: string;
    }) => {
      // When the answer left the page, read where the page makes the request rather than off
      // the browser's resource timing: the number is then the moment the reader's answer went
      // out, not the moment a task the build served recorded that it had.
      window.fetch = new Proxy(window.fetch, {
        apply(made, that, args: [RequestInfo | URL, RequestInit?]) {
          const asked = args[0];
          const where =
            typeof asked === "string" ? asked : asked instanceof URL ? asked.href : asked.url;
          if (new URL(where, window.location.origin).pathname === answered) {
            window.__answerSentMs ??= performance.now();
          }
          return Reflect.apply(made, that, args);
        },
      });
      const look = () => {
        const root = document.querySelector(live);
        const card =
          root?.querySelector(
            `[data-dispatch-ask-block="${blockId}"] article[data-testid^="ask-"]`
          ) ?? null;
        if (root === null || card === null) {
          requestAnimationFrame(look);
          return;
        }
        const radio = [...card.querySelectorAll<HTMLInputElement>('input[type="radio"]')].find(
          (input) => (input.closest("label")?.textContent ?? "").includes(wanted)
        );
        if (radio === undefined) {
          requestAnimationFrame(look);
          return;
        }
        if (!radio.checked) {
          // The pick in a frame of its own, because what it leaves behind is React's: the lone
          // Answer button a compact card shows once an option is picked, and the refusal that
          // button answers a press with while none is.
          radio.click();
          requestAnimationFrame(look);
          return;
        }
        const press = [...card.querySelectorAll("button")].find(
          (button) => (button.textContent ?? "").trim() === "Answer"
        );
        if (press === undefined || press.disabled || press.ariaDisabled === "true") {
          requestAnimationFrame(look);
          return;
        }
        press.addEventListener(
          "click",
          (event) => {
            window.__answerPressed = {
              at: event.timeStamp,
              building: document.querySelector(named) === null,
              built: root.querySelector('[contenteditable="true"]')?.childElementCount ?? 0,
            };
          },
          { capture: true, once: true }
        );
        press.click();
      };
      requestAnimationFrame(look);
    },
    { answered: answer, blockId: block, live: liveEditorRoot, named: namedEditor, wanted: option }
  );
}

// What a reader's answer costs while their editor is being built. The card is React's - the
// radio is its state, the button's refusal is its state, and the answer is a write the press
// makes - so every step between the press and the request is a posted task the build has to
// let through. The row holds that span to half a second, with the build still running when the
// page received the press: the build may not starve the answer a reader gives it.
//
// Both numbers are the page's own. Until INFRA-341 the row compared the moment the server had
// stored the answer with the moment the editor opened, and that comparison turned on neither:
// the answer is stored 30 to 180 ms after the press in every run, so what decided it was
// whether Playwright's own check and click - 10.9 s on this pod, 11 to 17 s on GitHub's
// runners - beat the build, which is 41 to 108 s here and 8.2 s there. It passed here and
// failed there, and neither answer was about the reader. The press is the page's own now
// (`readerAnswersTheBuild`), so it lands inside the build whatever the machine costs.
test(`a decision answered while ${documentNames.large}'s editor is built goes out within half a second of the press @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const block = decisionBlockId(1);
  const { issueKey, path } = await seedDocument("specs-large.md", largeDocument);
  const indexed = (await listIssueAsks(issueKey)).find((ask) => ask.block_id === block);
  expect(
    indexed,
    "the server indexed no ask for the document's first decision block"
  ).toBeDefined();

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await throttleTheCpu(page);
    await readerAnswersTheBuild(page, {
      answer: `/api/v1/asks/${indexed?.id}/answer`,
      block,
      option: "The next window",
    });
    await page.goto(path, { waitUntil: "commit" });

    // The page has pressed the card and its answer has left. The wait is for the request, not
    // for the build: what the editor does with the rest of the document after this is the
    // business of the rows above.
    await expect
      .poll(async () => (await page.evaluate(() => window.__answerSentMs)) !== undefined, {
        intervals: [250],
        message: "the page never answered the decision block its build had drawn",
        timeout: 300_000,
      })
      .toBe(true);
    // And the server holds it, which is what the press was for.
    await expect
      .poll(
        async () => (await listIssueAsks(issueKey)).find((ask) => ask.block_id === block)?.state,
        {
          intervals: [250],
          message: "the answer never reached the server",
          timeout: 120_000,
        }
      )
      .toBe("answered");

    const pressed = await page.evaluate(() => window.__answerPressed);
    const sentMs = (await page.evaluate(() => window.__answerSentMs)) ?? Number.NaN;
    const leftAfterMs = sentMs - (pressed?.at ?? Number.NaN);
    // Printed before either number is read against anything, as every other number in this
    // file is.
    console.log(
      `R11 decision during the build · the page received the press at ` +
        `${Math.round(pressed?.at ?? Number.NaN)} ms with ${pressed?.built ?? 0} block(s) built ` +
        `and the build ${pressed?.building === true ? "running" : "over"}, and the answer's ` +
        `request left at ${Math.round(sentMs)} ms, ${Math.round(leftAfterMs)} ms later ` +
        `(target ${theAnswerLeavesWithinMs} ms), on the page's own clock.`
    );

    expect(pressed, "the page never pressed the card's Answer button").toBeDefined();
    // The press was made in a build: the editor held part of the document and had not been
    // named, so the number below is not a target met by a page with nothing left to do.
    expect(pressed?.built ?? 0).toBeGreaterThan(0);
    expect(
      pressed?.building,
      "the build was over by the time the page pressed the card, so this run says nothing " +
        "about an answer given during one"
    ).toBe(true);
    expect(
      Math.round(leftAfterMs),
      `the reader's answer took ${Math.round(leftAfterMs)} ms to leave the page after the ` +
        "press: the build is holding the one thing the reader did with it"
    ).toBeLessThanOrEqual(theAnswerLeavesWithinMs);
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

// The larger document, as the row above it: what this one needs of a build is that it outlast
// a keystroke driven from here and the socket message that carries it. Over the AHXV6-1-shaped
// document the word reached the editor with 1,343 of its 1,436 blocks built on this pod, which
// leaves the row nothing to fail on, against 2,111 of 7,036 over the larger one.
test(`another person's edit reaches ${documentNames.large} while its editor is built @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { artifactId, path } = await seedDocument("specs-large.md", largeDocument);
  const title = documentTitle(documentDiagrams.large);
  const word = "Pangolin.";

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const bobPage = await bob.newPage();
    await bobPage.goto(path);
    await expect(documentEditor(bobPage)).toContainText(title, { timeout: 240_000 });
    await expect(connectedDot(bobPage)).toHaveText("connected");
    await placeCaret(bobPage, "after", title);

    const page = await alice.newPage();
    await throttleTheCpu(page);
    const reached = await watchTheDocument(page);
    await whenTheirWordLands(page, word);
    await page.goto(path, { waitUntil: "commit" });

    // Bob writes one word into the document's first block, which Alice's first batch has
    // already built, the moment she has that batch: it is in her Yjs document before his
    // keystroke is over, and the only thing between it and her editor is the socket message
    // that carries it.
    await reached.firstBlock;
    await bobPage.keyboard.type(` ${word}`, { delay: 25 });

    const openedMs = await reached.editorOpened;
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
    await bob.close();
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
    await throttleTheCpu(page);
    const reached = await watchTheDocument(page);
    const transport =
      overTheWholeDraw === undefined ? undefined : await documentTransport(page, { holding: true });
    await page.goto(path, { waitUntil: "commit" });
    if (transport !== undefined) {
      await expect
        .poll(() => page.locator(drawnBlocks).count(), {
          intervals: [1_000],
          message: "the page never drew the whole document, so no build ran over one",
          timeout: 300_000,
        })
        .toBeGreaterThanOrEqual(overTheWholeDraw ?? 0);
      await transport.release();
    }
    const firstBlock = await reached.firstBlock;
    const openedMs = await reached.editorOpened;
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

// The three rows below are about what the page does in front of the document's own draw,
// which is what the measured numbers above are mostly made of. The document's own reads are
// started from the address, the first of them before any of the page's own code has
// evaluated, so they wait for nothing the entry chunk or the issue page does; and the margin
// beside the document draws no card a reader cannot see. None of the three is timed: each
// reads what the page asked for and what it drew, never how long either took.

/** A document of one short section, so these rows seed in a moment: what they read is the
 *  order the page does things in, which no length of document changes. */
const shortDocument = "# The lease ledger\n\nThe paragraph a reader meets first.\n";

/** The pathname of each request the page made, newest last, for a row that reads what the page
 *  asked for. */
function requestedPaths(page: Page): string[] {
  const paths: string[] = [];
  page.on("request", (request) => paths.push(new URL(request.url()).pathname));
  return paths;
}

/** Either route a document's markdown is read by: the one the address names it at, and the one
 *  the issue's own read names it at (`web/src/api/client.ts`). */
const documentTextRoute = /^\/api\/v1\/(issues\/[^/]+\/artifacts\/[^/]+|artifacts\/[^/]+)\/text$/;

test("a document's own text is read before the sign-in check has answered @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const { issueKey, path } = await seedDocument("notes.md", shortDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const asked = requestedPaths(page);
    // The sign-in check, held: every page waits on it (`app.tsx`'s `AuthGate` shows its
    // skeleton until it answers), so while it is out nothing of the shell, the issue page or
    // the margin has rendered at all.
    const signInHeld = Promise.withResolvers<void>();
    await page.route("**/auth/whoami", async (route) => {
      await signInHeld.promise;
      await route.continue();
    });
    await page.goto(path, { waitUntil: "commit" });

    // The document's own text, asked for from the address alone
    // (`web/src/features/doc/open-document-early.ts`), while the check is still out. The
    // address names the document, so the read is the slug route's; a Spec tab's address names
    // no slug and its text is read by the id the issue answers with.
    await expect
      .poll(() => asked.filter((what) => documentTextRoute.test(what)), {
        intervals: [100],
        message:
          "the document's text was never asked for while the sign-in check was out: the read " +
          "is waiting for the shell again",
        timeout: 60_000,
      })
      .toEqual([`/api/v1/issues/${issueKey}/artifacts/notes-md/text`]);
    // And the shell really has not rendered: the Inbox read it makes is still unmade.
    expect(asked).not.toContain("/api/v1/inbox");

    // Released, the page shows the document it already holds the text of.
    signInHeld.resolve();
    const firstBlock = page
      .locator(
        '[aria-label="Document text"] [data-block-id], [aria-label="Document editor"] [data-block-id]'
      )
      .first();
    await expect(firstBlock).toBeVisible({ timeout: 60_000 });
    await expect(firstBlock).toContainText("The lease ledger");
  } finally {
    await alice.close();
  }
});

test("a document's text is asked for before the entry's imports have evaluated @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const { issueKey, path } = await seedDocument("notes.md", shortDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(path, { waitUntil: "commit" });
    const firstBlock = page
      .locator(
        '[aria-label="Document text"] [data-block-id], [aria-label="Document editor"] [data-block-id]'
      )
      .first();
    await expect(firstBlock).toBeVisible({ timeout: 120_000 });

    // What the browser itself timed: when the page asked for the document's text, and when the
    // entry's own module body began. Its first statement writes the
    // `dispatch:entry-imports-evaluated` mark (`web/src/main.tsx`), and a module evaluates every
    // static import of it before its own first statement runs, so a read that started before
    // that mark went out without waiting for any of them - which is what the page used to do
    // nothing but wait for, asking from the entry's body about 1.4 s into the load on a
    // throttled phone (`web/src/features/doc/open-document-early.ts`). Asking for it from
    // `web/index.html`'s head is what moves it in front of them. Both times are the browser's
    // own performance timeline, so nothing this row does sits between them.
    const timed = await page.evaluate((textPath: string) => {
      const resources = performance.getEntriesByType("resource") as PerformanceResourceTiming[];
      return {
        importsEvaluatedMs: performance.getEntriesByName("dispatch:entry-imports-evaluated")[0]
          ?.startTime,
        textAskedMs: resources.find((resource) => new URL(resource.name).pathname === textPath)
          ?.startTime,
      };
    }, `/api/v1/issues/${issueKey}/artifacts/notes-md/text`);

    expect(
      timed.importsEvaluatedMs,
      "the entry never marked itself: its first statement writes the " +
        "`dispatch:entry-imports-evaluated` mark (`web/src/main.tsx`)"
    ).toBeDefined();
    expect(timed.textAskedMs, "the document's text was never asked for at all").toBeDefined();
    const evaluatedMs = Math.round(timed.importsEvaluatedMs ?? Number.NaN);
    const askedMs = Math.round(timed.textAskedMs ?? Number.NaN);
    console.log(
      `R11 early read · the document's text was asked for at ${askedMs} ms, the entry's ` +
        `imports had evaluated at ${evaluatedMs} ms, ${evaluatedMs - askedMs} ms later.`
    );
    expect(
      timed.textAskedMs ?? Number.POSITIVE_INFINITY,
      "the document's text was asked for only after the entry's own first statement ran, so it " +
        "waited for every static import of the entry to evaluate before it went out " +
        "(`web/index.html`)"
    ).toBeLessThan(timed.importsEvaluatedMs ?? 0);
  } finally {
    await alice.close();
  }
});

// The Markdown engine the page draws a document with - the headless Proof editor's chunk, 461 KB
// to download and compile, and the engine built over it - used to be loaded before the page had
// rendered anything: `openDocumentEarly` warmed it, and the shell warmed it again from its first
// commit. That buys a reader their first block where the page has to draw it. It buys nothing at
// all where the server drew the document's opening blocks into the page it sent
// (`web/src/features/doc/first-screen.ts`), because `DocumentPreview` adopts those blocks instead
// of parsing them - and on a phone at 4x CPU throttling that load is about a second and a half of
// the page's own thread, taken in front of the issue page's first render, which is the render
// that carries the document's surface, its socket and its editor (dispatch://INFRA-341, R11).
//
// So the engine left the boot task, and then the surface stopped asking for it at all while an
// editor is on its way (`DocumentPreview`'s `waitsForTheBuildMs`): on such a page the pass it
// would build the engine for is dropped at the build every time - the sync landed 0.5 to 1.1 s
// after the surface mounted in every run measured on this pod, and the parse worker answers the
// whole document about 16 s later - so the engine was a second headless Proof editor, 294 to
// 546 ms of the main thread, built for a draw that never reached the reader.
//
// This row reads the browser's own resource timing after the reader has an editor they can type
// in: neither the engine's chunk nor the parse worker's was ever asked for.
test("a page the server drew a document into builds no Markdown engine while its editor comes @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the engine's one reader here is the document's own preview, which is what a phone draws: " +
      "at a width that opens the margin, its cards draw Markdown of their own"
  );
  test.setTimeout(300_000);
  // Plain prose, well over the parse worker's threshold and well over the first screen the
  // server draws: nothing on this page but the document's own draw has any use for the engine,
  // so a chunk asked for is that draw asking for it. A decision block would bring its own card
  // and a diagram its own renderer, and neither is what this row is about.
  const prose = `# The lease ledger\n\n${"The paragraph a reader meets. ".repeat(8)}\n\n`.concat(
    Array.from({ length: 120 }, (_, index) => `## Section ${index}\n\nWhat it says.`).join("\n\n"),
    "\n"
  );
  const { path } = await seedDocument("ledger.md", prose);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(path, { waitUntil: "commit" });

    // The reader's own block: the editor's first, under a root that takes edits, which is what
    // the wait is for. Until it is there, nothing says the engine was skipped rather than
    // merely late.
    await expect(
      page.locator('.proof-editor[data-write-path="live"] [data-block-id]').first()
    ).toBeVisible({ timeout: 180_000 });

    // What the browser itself timed. The engine's chunk is the headless editor's entry
    // (`@legion/proof-editor/headless`, `packages/proof-editor/src/lib-headless.ts`), which the
    // build names after that module, and the parse worker is the chunk the queue creates for a
    // body over the threshold; nothing else on this page asks for either.
    const asked = await page.evaluate(() => {
      const resources = performance.getEntriesByType("resource") as PerformanceResourceTiming[];
      const paths = resources.map((resource) => new URL(resource.name).pathname);
      return {
        engine: paths.filter((name) => /^\/assets\/lib-headless-[^/]*\.js$/.test(name)),
        worker: paths.filter((name) => /^\/assets\/markdown-parse\.worker-[^/]*\.js$/.test(name)),
        text: paths.filter((name) =>
          /^\/api\/v1\/(issues\/[^/]+\/artifacts\/[^/]+|artifacts\/[^/]+)\/text$/.test(name)
        ),
      };
    });

    expect(
      asked.text.length,
      "the document's text was never read, so this page never drew the document at all"
    ).toBeGreaterThan(0);
    expect(
      asked.engine,
      "the page built its own Markdown engine while the editor was on its way: a second " +
        "headless Proof editor for a draw the build replaces before it can paint " +
        "(`DocumentPreview`'s `waitsForTheBuildMs`)"
    ).toEqual([]);
    expect(
      asked.worker,
      "the page posted the whole document to the parse worker while the editor was on its way"
    ).toEqual([]);
  } finally {
    await alice.close();
  }
});

test("a phone draws the margin's ask cards only once its reader opens the review panel @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "above the phone layout the margin is a column of its own, which is always on screen"
  );
  test.setTimeout(180_000);
  const { issueKey, path } = await seedDocument("notes.md", shortDocument);
  const question = "Which window should the lease ledger settle in?";
  await createAsk(
    issueKey,
    { options: [{ label: "The next window" }, { label: "The window after it" }], question },
    agent
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const asked = requestedPaths(page);
    await page.goto(path);
    await expect(documentEditor(page)).toContainText("The lease ledger", { timeout: 120_000 });

    // The whole page is up, and the shut sheet holds no card: a card behind it is 64 px of
    // `Review (1)`, and the thread read each card makes is a read the reader never sees the
    // answer to (dispatch://INFRA-341, R11).
    const sheet = page.getByTestId("margin-sheet");
    await expect(sheet.locator("[data-margin-item]")).toHaveCount(0);
    expect(asked.filter((what) => /^\/api\/v1\/asks\/[^/]+$/.test(what))).toHaveLength(0);
    // The count beside `Review` is the margin's own asks read, which it makes either way.
    const review = page.getByRole("button", { name: /^Open review panel/ });
    await expect(review).toHaveAccessibleName("Open review panel (1 open ask)");

    await review.click();
    await expect(sheet.getByText(question)).toBeVisible();
    await expect(sheet.locator("[data-margin-item]")).toHaveCount(1);
    await expect
      .poll(() => asked.filter((what) => /^\/api\/v1\/asks\/[^/]+$/.test(what)), {
        intervals: [100],
        message: "the opened card never read its own thread",
        timeout: 30_000,
      })
      .toHaveLength(1);
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

test(`a reader answers a decision block in ${documentNames.small} @phone`, async ({ browser }) => {
  test.setTimeout(600_000);
  const block = decisionBlockId(decisionBlocks);
  const { path } = await seedDocument("specs-small.md", smallDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`${path}#b-${block}`);
    const decision = documentEditor(page).locator(`[data-dispatch-ask-block="${block}"]`);
    // The block hosts the Inbox's own ask card once the server has indexed it.
    const card = decision.locator("article[data-testid^=ask-]");
    await expect(card).toBeVisible({ timeout: 240_000 });

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
    expect(blocks).toHaveLength(commentDiagrams);
    // Every block the page asked for drew: a refusal would mean the renderer ran for it after
    // all, which is the cost this rule exists to save.
    expect(blocks.filter((block) => block.asked !== block.drawn)).toEqual([]);
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
    // And nothing within one viewport height of the reader is left undrawn.
    expect(blocks.filter((block) => block.near && !block.drawn)).toEqual([]);
  } finally {
    await alice.close();
  }
});

/** The build's own budget of recovery reloads, spent before the page loads, so the row below
 *  reads one page rather than the three the shell's inline script would reload through when it
 *  sees every script of the build fail (`web/index.html`, `CHUNK_RELOADS_STORAGE_KEY` in
 *  `web/src/features/shell/DeploymentResilience.tsx`). */
const CHUNK_RELOADS_SPENT = "dispatch.chunk-reloads";

// The reader meets their document in the page's own bytes. The Go server draws the document's
// opening blocks into the page it sends a reader whose session may read it
// (`internal/dispatch/pmdoc/first_screen.go`, dispatch://INFRA-341 R11, acceptance A11), so the
// first block is on the screen with none of the app downloaded, parsed or run: this row blocks
// every script the build serves and finds it there. What it proves is the one thing no
// measurement can - that the first block owes nothing at all to the page's own code.
test("draws the document's first block with the app's code blocked @phone", async ({ browser }) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("specs-blocked.md", smallDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.addInitScript(
      ([key, now]) => {
        window.sessionStorage.setItem(key as string, JSON.stringify([now, now, now]));
      },
      [CHUNK_RELOADS_SPENT, Date.now()]
    );
    await page.route("**/*.js", (route) => route.abort());
    await page.goto(path);

    const first = page.locator('[aria-label="Document text"] [data-block-id]').first();
    await expect(first).toBeVisible();
    await expect(first).toHaveText(documentTitle(documentDiagrams.small));
    // The app never ran: its own root is empty, and the editor it would build is nowhere.
    await expect(page.locator("#root")).toBeEmpty();
    await expect(page.getByRole("textbox", { name: "Document editor" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// --- The reads the first editable block needs none of ------------------------------------
//
// A document address spends its first seconds on one thing, and the three rows below are about
// everything else the page asks Dispatch for while it does. Measured on this pod, `iphone` at
// 4x CPU throttling, seven reads went out in front of the first block a reader can type in -
// 4,328 to 5,493 ms on a load whose preview mounted at 5,611 - and the block needs none of
// them: they fill the issue page's non-document panels (dispatch://INFRA-341, R11). They are
// made after the editor's first batch now (`web/src/features/doc/first-batch.ts`).
//
// Each row holds the document's socket (`documentTransport`), so the first batch happens when
// the row says so rather than when a loaded machine gets round to it, and reads the order off
// the browser's own resource timing (`watchTheReads`). None of them is a measurement: what
// they prove is which side of the batch a read falls on.
//
// Their document holds no decision block (`proseOnlyDocument`), because a decision block in
// the reader's own draw is a card they can answer before any editor exists and it reads the
// document's asks and its recipients to do so: a reader who has one on the screen is waiting
// for those two reads, so they go out there rather than at the batch, which is the rule
// working rather than failing.

/** The seven, by the path each goes out on. `/references` is the spec's `part_of` read, which
 *  the page makes only for a reader who has read the document before, so the row that reads it
 *  opens the spec twice. */
function readsTheFirstBlockNeedsNone(issueKey: string, project: string): readonly string[] {
  return [
    `/issues/${issueKey}/comments`,
    `/issues/${issueKey}/asks`,
    "/agents",
    "/settings/repo-projects",
    `/projects/${project}/architecture-sources`,
    `/issues/${issueKey}/subscribers`,
    "/references",
  ];
}

/** Every read the page made on one of `paths`, with its query, in the order the browser timed
 *  them: what a row names when it says a read went out it should not have. */
function readsAmong(reads: readonly PageRead[], paths: readonly string[]): PageRead[] {
  return reads.filter((read) => paths.includes(read.path.split("?")[0] ?? read.path));
}

/** When each of `paths` went out, in order, by path: every read the page made on that path
 *  with its query dropped, so one entry means one request. */
function whenEachWentOut(
  reads: readonly PageRead[],
  paths: readonly string[]
): Map<string, number[]> {
  const when = new Map<string, number[]>(paths.map((path) => [path, []]));
  for (const read of readsAmong(reads, paths)) {
    when.get(read.path.split("?")[0] ?? read.path)?.push(read.at);
  }
  return when;
}

/** Puts the order a row read on its annotations, which the report keeps with the result, and on
 *  stdout, which `--reporter line` prints as the row runs, exactly as `printMeasurement` does:
 *  a report says which read went out when rather than only that the row was green. */
function printTheOrder(
  what: string,
  firstBatchMs: number | undefined,
  when: Map<string, number[]>
): void {
  const line = `first batch ${
    firstBatchMs === undefined ? "never" : `${Math.round(firstBatchMs)} ms`
  }; ${[...when]
    .map(
      ([path, at]) => `${path} ${at.length === 0 ? "never" : `${at.map(Math.round).join(", ")} ms`}`
    )
    .join("; ")}`;
  test.info().annotations.push({ description: line, type: what });
  console.log(`${what} · ${line}`);
}

/** How long a row waits, with the socket held and the page settled, before it decides a read
 *  the page was going to make has not been made. The page's own draw of the document is on the
 *  screen well inside it, and every panel of the header has rendered.
 *
 *  Both this window and the editor's build after it have to finish inside the hold's own
 *  deadline, since a page held past that deadline releases the hold by design and the reads
 *  then go out with no batch behind them (`web/src/features/doc/first-batch.ts`,
 *  `LONGEST_HOLD_MS`, which the last of this file's chrome rows covers). The assertion below
 *  is what keeps the two in step when either number moves. */
const settledWithTheSocketHeldMs = 6_000;
if (settledWithTheSocketHeldMs * 2 >= LONGEST_HOLD_MS) {
  throw new Error(
    `the settled window (${settledWithTheSocketHeldMs} ms) leaves no room for the build inside the hold's deadline (${LONGEST_HOLD_MS} ms)`
  );
}

test("the reads the first editable block needs none of go out after the editor's first batch @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the seven were measured on a phone, where the margin's cards are a sheet the reader opens"
  );
  test.setTimeout(300_000);
  const { issueKey, path } = await seedDocument("specs-order.md", proseOnlyDocument);
  const paths = readsTheFirstBlockNeedsNone(issueKey, "BIG");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);

    // The page is up and the reader has the document's own draw. The header waits for the
    // batch too now (`web/src/features/doc/AfterTheFirstBatch.tsx`), so the reader reaches for
    // its box, which draws it: every panel the held reads belong to is then on the page, and
    // each of those reads still waits, because each has a gate of its own.
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeVisible({
      timeout: 120_000,
    });
    await page.getByTestId("issue-header-box").click({ position: { x: 5, y: 5 } });
    await expect(page.getByRole("button", { name: /^Subscribers/ })).toBeVisible();
    await expect(page.locator('[data-testid="issue-components"]')).toBeVisible();
    await page.waitForTimeout(settledWithTheSocketHeldMs);

    const held = await readTheReads(page);
    expect(held.firstBatchMs).toBeUndefined();
    expect(
      readsAmong(held.reads, paths),
      "a read the first editable block needs none of went out before the editor's first batch"
    ).toEqual([]);

    // Released, the editor builds, and every one of them goes out behind its first batch.
    await transport.release();
    await expect(editorBeingBuilt(page)).toBeAttached({ timeout: 120_000 });
    await expect
      .poll(
        async () =>
          [...whenEachWentOut((await readTheReads(page)).reads, paths)].filter(
            ([, at]) => at.length === 0
          ).length,
        { message: "a read the page held behind the first batch never went out", timeout: 120_000 }
      )
      .toBe(0);

    const after = await readTheReads(page);
    const when = whenEachWentOut(after.reads, paths);
    printTheOrder("reads after the first batch", after.firstBatchMs, when);
    const firstBatchMs = after.firstBatchMs ?? 0;
    expect(firstBatchMs).toBeGreaterThan(0);
    for (const [read, at] of when) {
      expect(at, `${read} never went out`).not.toHaveLength(0);
      for (const went of at) {
        expect(went, `${read} went out before the first batch`).toBeGreaterThan(firstBatchMs);
      }
    }
    // And none of them was made twice: one request per address, whichever panel wanted it.
    const addresses = readsAmong(after.reads, paths).map((read) => read.path);
    expect(addresses, "a read went out twice").toHaveLength(new Set(addresses).size);
  } finally {
    await alice.close();
  }
});

// The other half of the rule: a read is held from a reader who is not waiting for it, never
// from one who is. The Subscribers panel is the issue header's own, shut until the reader
// opens it, and opening it reads who is subscribed there and then - with the document's socket
// still held, so no first batch has happened and none is on its way.
test("a reader who opens a panel before the first batch has its read started then @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the seven were measured on a phone, where the margin's cards are a sheet the reader opens"
  );
  test.setTimeout(300_000);
  const { issueKey, path } = await seedDocument("specs-reached.md", proseOnlyDocument);
  const subscribers = `/issues/${issueKey}/subscribers`;
  const others = readsTheFirstBlockNeedsNone(issueKey, "BIG").filter(
    (read) => read !== subscribers
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await documentTransport(page, { holding: true });
    await page.goto(path);

    // The header waits for the batch too, so the reader reaches for its box first; the panel
    // inside it is still shut, and its read still held.
    await page.getByTestId("issue-header-box").click({ position: { x: 5, y: 5 } });
    const toggle = page.getByRole("button", { name: /^Subscribers/ });
    await expect(toggle).toBeVisible({ timeout: 120_000 });
    await page.waitForTimeout(settledWithTheSocketHeldMs);
    expect((await readTheReads(page)).reads.filter((read) => read.path === subscribers)).toEqual(
      []
    );

    await toggle.click();
    await expect
      .poll(
        async () =>
          (await readTheReads(page)).reads.filter((read) => read.path === subscribers).length,
        { message: "the panel the reader opened never read its subscribers", timeout: 30_000 }
      )
      .toBe(1);

    // And it was the reader's own panel that did it, with no first batch and none of the other
    // held reads behind it.
    const reached = await readTheReads(page);
    printTheOrder(
      "the reader's own panel",
      reached.firstBatchMs,
      whenEachWentOut(reached.reads, [subscribers, ...others])
    );
    expect(reached.firstBatchMs).toBeUndefined();
    expect(
      [...whenEachWentOut(reached.reads, others)].filter(([, at]) => at.length > 0),
      "opening one panel started a read belonging to another"
    ).toEqual([]);
  } finally {
    await alice.close();
  }
});

// And a page with no document on it holds nothing: there is no first batch to wait for, so
// every one of those reads goes out as it did before INFRA-341. The issue's Conversation is
// that page - the same header, the same margin, no document surface - and the row reads it
// with no socket held at all, because there is no socket to hold.
test("a page with no document on it makes those reads as it always did @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the seven were measured on a phone, where the margin's cards are a sheet the reader opens"
  );
  test.setTimeout(300_000);
  const { issueKey } = await seedDocument("specs-no-document.md", proseOnlyDocument);
  // Every one of the seven but the spec's `part_of` read, which belongs to the document this
  // page does not show.
  const paths = readsTheFirstBlockNeedsNone(issueKey, "BIG").filter(
    (read) => read !== "/references"
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await page.goto(`/issues/${issueKey}/conversation`);
    await expect(page.getByRole("button", { name: /^Subscribers/ })).toBeVisible({
      timeout: 120_000,
    });

    await expect
      .poll(
        async () =>
          [...whenEachWentOut((await readTheReads(page)).reads, paths)].filter(
            ([, at]) => at.length === 0
          ).length,
        { message: "a read of a page with no document on it never went out", timeout: 60_000 }
      )
      .toBe(0);

    const made = await readTheReads(page);
    printTheOrder("no document on the page", made.firstBatchMs, whenEachWentOut(made.reads, paths));
    // Nothing was ever built, so nothing was ever waited for.
    expect(made.firstBatchMs).toBeUndefined();
    await expect(page.locator('.proof-editor[data-write-path="live"]')).toHaveCount(0);
    // And its chrome is the chrome it always had, drawn with nothing waited for: the header
    // the Subscribers control stands in, the tab strip itself rather than the box the page
    // keeps for it, and the shell's own inbox peek.
    await expect(page.getByRole("tab", { name: /^Spec$/ })).toBeVisible();
    await expect(page.getByTestId("issue-tabs-box")).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^Peek at the inbox/ })).toBeVisible();
  } finally {
    await alice.close();
  }
});

// And the rule the three rows above are held to: a panel the page draws after the reader is in
// place moves what they are looking at by no pixels (`web/src/features/doc/readers-place.ts`).
// The panel here is the issue header's Subscribers list, whose read is one of the seven; the
// row holds that read at the server until the reader is deep in the document, so the panel
// lands under a page that has stopped moving rather than during the load, which is the state
// the rule is about. The browser's own scroll anchoring is off (`withoutScrollAnchoring`), as
// it is in Safari before 27, so what keeps the reader in place is the page's own doing.
test("a panel arriving while the reader is in a long document moves what they are looking at by no pixels @phone", async ({
  browser,
}) => {
  test.setTimeout(600_000);
  const { issueKey, path } = await seedDocument("specs-large.md", largeDocument);
  await subscribeSession(issueKey);
  // A section well down the document: the heading the reader stops at, and the words the row
  // measures. Far enough in that the whole header, panel and all, is off their screen.
  const landmarkText = sectionHeading(Math.floor(documentDiagrams.large / 2));

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const subscribers = Promise.withResolvers<void>();
    await page.route(`**/api/v1/issues/${issueKey}/subscribers`, async (route) => {
      await subscribers.promise;
      await route.continue();
    });
    await page.goto(path);
    await expect(documentEditor(page)).toBeVisible({ timeout: 240_000 });
    await withoutScrollAnchoring(page);

    // The reader opens the panel - its read is still held, so it is the empty frame of itself -
    // and reads their way down to the landmark, which is where they are when it lands.
    await page.getByRole("button", { name: "Subscribers", exact: true }).click();
    const landmark = documentEditor(page).getByRole("heading", { name: landmarkText });
    await landmark.scrollIntoViewIfNeeded();
    await settledScrollTop(page);
    await landmark.evaluate((heading) => heading.scrollIntoView({ block: "start" }));
    expect(await settledScrollTop(page)).toBeGreaterThan(10_000);

    // From here the row reads the landmark after every frame the browser paints - a task
    // scheduled from an animation frame runs once that frame is on the screen - so what it
    // records is what the reader saw, never a page the correction had not reached yet.
    await page.evaluate((text: string) => {
      const heading = [...document.querySelectorAll(".proof-editor h2")].find(
        (candidate) => candidate.textContent === text
      );
      window.__readersPlace = { grew: false, moved: 0, started: heading !== undefined };
      if (heading === undefined) return;
      const scrolled = document.scrollingElement?.scrollTop ?? 0;
      const started = heading.getBoundingClientRect().top;
      const sample = () => {
        const place = window.__readersPlace;
        if (place === undefined) return;
        const top = heading.getBoundingClientRect().top;
        const above = top + (document.scrollingElement?.scrollTop ?? 0) - scrolled;
        place.moved = Math.max(place.moved, Math.abs(top - started));
        if (Math.abs(above - started) > 0.5) place.grew = true;
        requestAnimationFrame(() => window.setTimeout(sample, 0));
      };
      sample();
    }, landmarkText);

    subscribers.resolve();
    await expect(page.getByRole("region", { name: "Subscribed agents" })).toContainText(
      "e2e-session-title",
      { timeout: 60_000 }
    );
    await expect
      .poll(async () => (await page.evaluate(() => window.__readersPlace))?.grew, {
        message: "the panel landing never changed how much page stands above the reader",
        timeout: 60_000,
      })
      .toBe(true);

    // The reader's words stay on the line they were on. Not to the exact float: a scroll offset
    // is a fraction of a pixel on a page this tall, so the correction lands within one, and a
    // pixel is the unit a reader has.
    const place = await page.evaluate(() => window.__readersPlace);
    expect(place?.started, "the row never found the heading it measures").toBe(true);
    expect(
      place?.moved ?? Number.NaN,
      "the panel moved the words the reader was looking at"
    ).toBeLessThan(1);
  } finally {
    await alice.close();
  }
});

// --- The chrome the page draws after that batch -------------------------------------------
//
// The reads above were half the rule; the React that draws their panels is the other half.
// Measured on this pod, the shell's own render and its commits cost the main thread 1,062 ms
// on the way to the first editable block of the 28-section document, and the issue page's
// header, tab strip and artifact chrome 522 ms of React inside a 1,065 ms window. The route
// has to render; the sidebar, the inbox peek, the live pill, the needs-you and replies counts
// and the page's own chrome do not, so each of them is drawn after the editor's first batch
// (dispatch://INFRA-341 R11, `web/src/features/doc/AfterTheFirstBatch.tsx`).
//
// Each row below holds the document's socket (`documentTransport`), so the batch happens when
// the row says so rather than when a loaded machine gets round to it, and names every part it
// watches by what that part leaves in the page.

/** The parts a document address draws after the batch, by what each leaves in the page. The
 *  tab is named by its own id rather than by its role, because the margin draws tabs of its
 *  own. The shell's part differs with the width: above the phone layout it is the sidebar's
 *  navigation column, and on a phone the count the compact header's inbox entry carries,
 *  which is the same needs-you read. The entry itself stands there before the batch, at a
 *  count of nothing, so the count is what arrives (`web/src/features/shell/ShellCounts.tsx`).
 */
function theChromeAfterTheBatch(phone: boolean): Record<string, string> {
  return {
    "the artifact's header": '[data-testid="artifact-header"]',
    "the issue's header": "[data-issue-header]",
    "the issue's tab strip": "#issue-spec-tab",
    ...(phone
      ? { "the shell's inbox count": "[data-inbox-count]" }
      : { "the sidebar's navigation": 'nav[aria-label="Navigation"]' }),
  };
}

/** Puts what a chrome row read on its annotations, which the report keeps with the result, and
 *  on stdout, which `--reporter line` prints as the row runs, exactly as `printTheOrder` does:
 *  a report says which part was drawn when rather than only that the row was green. */
function printTheChrome(
  what: string,
  firstBatchMs: number | undefined,
  drawn: Readonly<Record<string, number>>
): void {
  const parts = Object.entries(drawn).sort(([left], [right]) => left.localeCompare(right));
  const line = `first batch ${
    firstBatchMs === undefined ? "never" : `${Math.round(firstBatchMs)} ms`
  }; ${
    parts.length === 0
      ? "nothing of the chrome drawn"
      : parts.map(([name, at]) => `${name} ${Math.round(at)} ms`).join("; ")
  }`;
  test.info().annotations.push({ description: line, type: what });
  console.log(`${what} · ${line}`);
}

test("the shell's parts and the page's chrome are drawn after the editor's first batch @phone", async ({
  browser,
}, testInfo) => {
  test.setTimeout(300_000);
  const { issueKey, path } = await seedDocument("specs-chrome.md", proseOnlyDocument);
  // One ask of an agent's, so the shell has a count to draw: with nothing waiting on the
  // reader the compact header's peek reads `Inbox` before the batch and after it, and the row
  // would be watching for a part that never changes.
  // The count is the reader's own, which is an ask waiting on a human on an issue assigned to
  // them (`isMine` in `web/src/features/inbox/sections.ts`), so the issue is alice's here.
  await patchIssue(issueKey, { assignee: "alice" });
  await createAsk(issueKey, { question: "Which ledger settles first?" }, agent);
  const phone = testInfo.project.name === "iphone";
  const parts = theChromeAfterTheBatch(phone);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await watchTheChrome(page, parts);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);

    // The reader has the document's own draw and the boxes the page keeps where its chrome
    // will stand; no editor is being built, so none of the chrome itself is drawn.
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeVisible({
      timeout: 120_000,
    });
    await expect(page.getByTestId("issue-tabs-box")).toBeAttached();
    await page.waitForTimeout(settledWithTheSocketHeldMs);

    const heldReads = await readTheReads(page);
    const heldChrome = await readTheChrome(page);
    printTheChrome("the chrome with the socket held", heldReads.firstBatchMs, heldChrome);
    expect(heldReads.firstBatchMs).toBeUndefined();
    expect(
      Object.keys(heldChrome).sort(),
      "a part of the chrome was drawn before the editor's first batch"
    ).toEqual([]);

    // Released, the editor builds, and every part is drawn behind its first batch.
    await transport.release();
    await expect(editorBeingBuilt(page)).toBeAttached({ timeout: 120_000 });
    await expect
      .poll(async () => Object.keys(await readTheChrome(page)).sort(), {
        message: "a part of the chrome the page held behind the first batch was never drawn",
        timeout: 120_000,
      })
      .toEqual(Object.keys(parts).sort());

    const afterReads = await readTheReads(page);
    const afterChrome = await readTheChrome(page);
    printTheChrome("the chrome after the first batch", afterReads.firstBatchMs, afterChrome);
    const firstBatchMs = afterReads.firstBatchMs ?? 0;
    expect(firstBatchMs).toBeGreaterThan(0);
    for (const [name, at] of Object.entries(afterChrome)) {
      expect(at, `${name} was drawn before the editor's first batch`).toBeGreaterThan(firstBatchMs);
    }
    // And the box the tab strip stood in has gone, which is the strip itself standing there.
    await expect(page.getByTestId("issue-tabs-box")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// And the other end of that hold. A socket that opens and then neither connects nor fails
// tells the page nothing, so none of the releases above ever comes; the reader would be left
// with the document's own text and a header and tab strip that stay a skeleton for as long as
// they are on the page, which is the way out of a document that is not opening. The hold is
// bounded for that reason (`web/src/features/doc/first-batch.ts`, `LONGEST_HOLD_MS`), and this
// is the row that says so: the socket is held throughout, no batch ever comes, and the chrome
// is drawn all the same.
test("the chrome is drawn even where the socket never answers @phone", async ({
  browser,
}, testInfo) => {
  test.setTimeout(300_000);
  const { issueKey, path } = await seedDocument("specs-deadline.md", proseOnlyDocument);
  await patchIssue(issueKey, { assignee: "alice" });
  await createAsk(issueKey, { question: "Which ledger settles first?" }, agent);
  const parts = theChromeAfterTheBatch(testInfo.project.name === "iphone");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await watchTheChrome(page, parts);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);

    // Every part of the chrome, with the socket still held and no batch behind any of them.
    await expect
      .poll(async () => Object.keys(await readTheChrome(page)).sort(), {
        message: "the chrome was never drawn on a page whose socket never answered",
        timeout: 120_000,
      })
      .toEqual(Object.keys(parts).sort());
    const held = await readTheReads(page);
    printTheChrome("the chrome with the socket never answering", held.firstBatchMs, {
      ...(await readTheChrome(page)),
    });
    expect(held.firstBatchMs, "a batch came, so this row proved nothing").toBeUndefined();
    await transport.release();
  } finally {
    await alice.close();
  }
});

// The rule that chrome is held to, at the place the chrome stands over: the document's own
// first block. Every part above the document is drawn into a box the page already kept - the
// header's, the tab strip's, the artifact card's frame - and what the part turns out to be
// taller than its box by is a growth above a reader who has placed themselves, which the page
// answers by moving with it (`ChromeAfterTheFirstBatch`). So the first block stands on the
// same line of their screen before the chrome and after it.
//
// The reader here has read the chrome's boxes off the top of their screen, which is where a
// reader of a document on a phone is seconds into the load: the chrome stands 250 px down a
// 664 px screen before it is drawn and 590 px down once it is. The browser's own scroll
// anchoring is off (`withoutScrollAnchoring`), as it is in Safari before 27, so what keeps
// them in place is the page's own doing.
test("the chrome arriving moves the document's own first block by no pixels @phone", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("specs-chrome-place.md", proseOnlyDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeVisible({
      timeout: 120_000,
    });
    await withoutScrollAnchoring(page);
    // The reader's own draw of the whole document, before they are placed in it: the page
    // stops growing under them, and the draw stops painting when the build starts
    // (`DocumentPreview`'s `building`), so a draw still painting would shorten the page under
    // the reader for a reason that is not the chrome's.
    await page.waitForTimeout(settledWithTheSocketHeldMs);

    // The reader reads the chrome's boxes off the top of their screen and stops with the
    // document's first block just under it. Every box of the chrome now stands above them,
    // which the row proves before it measures anything: a box a reader can see is a box they
    // watch land, and nothing moves for them (`readersBlock`).
    const landed = await page.evaluate(() => {
      const block = document.querySelector('[aria-label="Document text"] [data-block-id]');
      if (block === null) return undefined;
      window.scrollBy(0, block.getBoundingClientRect().top - 16);
      return Math.round(block.getBoundingClientRect().top);
    });
    expect(landed, "the row never found the document's first block").toBe(16);
    const chromeAbove = await page.evaluate(() =>
      [...document.querySelectorAll('[data-testid="issue-tabs-box"], header')]
        .map((box) => Math.round(box.getBoundingClientRect().top))
        .filter((top) => top >= 0)
    );
    expect(chromeAbove, "a box the chrome stands in was still on the reader's screen").toEqual([]);

    // From here the row reads the first block after every frame the browser paints - a task
    // scheduled from an animation frame runs once that frame is on the screen - so what it
    // records is what the reader saw. The block is looked up afresh each time: the editor's
    // first batch puts its own copy of it in front of the draw's, and both stand at the top of
    // the document surface, which is what the chrome above moves.
    await page.evaluate(() => {
      const first = () =>
        document.querySelector(
          '.proof-editor [data-block-id], [aria-label="Document text"] [data-block-id]'
        );
      const block = first();
      window.__firstBlockPlace =
        block === null
          ? undefined
          : { grew: false, moved: 0, started: block.getBoundingClientRect().top };
      const scrolled = document.scrollingElement?.scrollTop ?? 0;
      const sample = () => {
        const place = window.__firstBlockPlace;
        const now = first();
        if (place === undefined || now === null) return;
        const top = now.getBoundingClientRect().top;
        const above = top + (document.scrollingElement?.scrollTop ?? 0) - scrolled;
        place.moved = Math.max(place.moved, Math.abs(top - place.started));
        if (Math.abs(above - place.started) > 0.5) place.grew = true;
        requestAnimationFrame(() => window.setTimeout(sample, 0));
      };
      sample();
    });

    await transport.release();
    await expect(page.locator("[data-issue-header]")).toBeAttached({ timeout: 120_000 });
    await expect(page.locator('[data-testid="artifact-header"]')).toBeAttached({
      timeout: 120_000,
    });
    await expect
      .poll(async () => (await page.evaluate(() => window.__firstBlockPlace))?.grew, {
        message: "the chrome landing never changed how much page stands above the reader",
        timeout: 120_000,
      })
      .toBe(true);

    const place = await page.evaluate(() => window.__firstBlockPlace);
    test.info().annotations.push({
      description: `the first block started at ${Math.round(place?.started ?? Number.NaN)} px and moved ${place?.moved ?? Number.NaN} px`,
      type: "the chrome moves the document by no pixels",
    });
    console.log(
      `the chrome moves the document by no pixels · started ${Math.round(place?.started ?? Number.NaN)} px, moved ${place?.moved ?? Number.NaN} px`
    );
    // Not to the exact float: a scroll offset is a fraction of a pixel, so the correction lands
    // within one, and a pixel is the unit a reader has.
    expect(
      place?.moved ?? Number.NaN,
      "the chrome moved the document's own first block"
    ).toBeLessThan(1);
  } finally {
    await alice.close();
  }
});

// And the other half of the rule, for the chrome as for the reads: a part is held from a
// reader who is not waiting for it, never from one who is. The reader here presses the box the
// tab strip will stand in - which is how a reader reaches for a tab - with the document's
// socket still held, so no first batch has happened and none is on its way.
test("a reader who reaches for the chrome before the batch has it then @phone", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("specs-chrome-reached.md", proseOnlyDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await documentTransport(page, { holding: true });
    await page.goto(path);

    const box = page.getByTestId("issue-tabs-box");
    await expect(box).toBeAttached({ timeout: 120_000 });
    await page.waitForTimeout(settledWithTheSocketHeldMs);
    expect((await readTheReads(page)).firstBatchMs).toBeUndefined();
    await expect(page.getByRole("tab", { name: /^Spec$/ })).toHaveCount(0);

    await box.click({ position: { x: 5, y: 5 } });

    await expect(page.getByRole("tab", { name: /^Spec$/ })).toBeVisible({ timeout: 30_000 });
    // And it was the reader's own press that did it, with no first batch behind it.
    expect((await readTheReads(page)).firstBatchMs).toBeUndefined();
  } finally {
    await alice.close();
  }
});
