// What a large document costs a reader on a phone, measured under the set-up
// dispatch://INFRA-341/spec names: the `iphone` project under 4x CPU throttling, the time from
// the start of navigation until the editor's first visible block is in the viewport, and the
// longest main-thread task until the page has scrolled to the end. The two documents are
// `large-documents.ts`: one shaped like dispatch://AHXV6-1/artifact/intermediate-specs-md, and
// one five times as large.
//
// The two rows of this file fail when a target of "Targets for large documents" is missed: the
// editor's first visible block within 3 s for the smaller document and 5 s for the larger, and
// no main-thread task longer than 500 ms for either. That block is the one the reader can type
// in - the editor's own, while its root carries `data-write-path="live"` - and never the
// server's draw or the page's own in front of it, which each row reads off the timed block
// itself and holds it to (`large-documents-page.ts`). Those two draws are printed as numbers of
// their own, `first painted` and the page's own draw, against no target. Every number is
// printed whether or not its target was met, since a target met says nothing about how much
// room is left.
//
// Each of those two numbers is the median of three readings, which is the protocol the spec's
// own Design paragraph names: a row takes three, each a navigation of a page of its own, and
// holds the median of them to the target (`measuredReadings` in `large-documents-page.ts` says
// why three and why a page apiece). All three are printed beside the median and held to
// nothing, so a reader of the line sees the spread the median came out of.
//
// This file is `iphone-measure-alone`'s and no other project's: that project names it in its
// `testMatch` and every parallel project ignores it (`clockSpecNames`, e2e/configurations.ts).
// The file is the selector because a title is not one. These rows used to be picked out of a
// larger file by a pattern over their titles, so a row renamed - or one added beside them
// under a title the pattern did not match - ran in `iphone` four workers wide instead, which
// is a reading of the other three lanes, with every suite green (sjawhar/legion#2697).
//
// The other rows read from the wall clock over these documents are a file apiece, one per
// project: `large-documents-build-serves.e2e.ts` is `iphone-alone`'s, the page's own clock and
// a decision answered inside a build, and `large-documents-desktop-build.e2e.ts` is
// `chromium-alone`'s, the longest main-thread task of a build at a desktop width. What a build
// of the larger document costs the page with no number read from the clock is
// `large-documents-long-build.e2e.ts`, which the parallel projects run, and it holds the row
// that proves the window the measurements below read their longest task over covers the whole
// build.
//
// Every other row of what was one file reads an order, a place or a count rather than a clock,
// and 406,000 characters buy none of them anything: they are in `large-documents-build.e2e.ts`,
// `large-documents-reader.e2e.ts` and `large-documents-first-screen.e2e.ts`, and what the seven
// files share is `large-documents-rows.ts`.
import { expect, test } from "@playwright/test";

import { documentEditor } from "./editor";
import {
  diagramsIn,
  documentDiagrams,
  documentNames,
  largeDocument,
  smallDocument,
} from "./large-documents";
import {
  type DocumentMeasurement,
  longTaskTargetMs,
  measureDocument,
  measuredReadings,
  printMeasurement,
  printTheMedian,
} from "./large-documents-page";
import { seedDocument } from "./large-documents-rows";
import { expectNoneOf } from "./none-of";
import { asUser } from "./users";

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
  /** The row's own timeout for one reading, since the suite sets none and Playwright's 30 s
   *  default is under what the seed, the throttled draw, the build and 120 scroll steps cost.
   *  The row takes `measuredReadings` of them and sets that many times this. */
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
  // No guard against the project here: this file is `iphone-measure-alone`'s whole selection
  // and no other project selects it (`clockSpecNames`, e2e/configurations.ts), so the
  // measurement set-up - the iPhone 13 viewport, touch, Chromium and the machine to itself -
  // is the file's and not a row's to check. The `test.skip` that stood here read
  // `testInfo.project.name`, which is a row deciding at run time what a selector decides
  // before the run (sjawhar/legion#2697).
  test(`measures ${row.what} on a phone under 4x CPU throttling @phone`, async ({ browser }) => {
    test.setTimeout(row.timeoutMs * measuredReadings);
    // The document the run measures is the one this module spells, so a change to the generator
    // that moved either size would be read here rather than in a number that quietly moved.
    expect(diagramsIn(row.content)).toBe(row.diagrams);
    const { path } = await seedDocument(row.name, row.content);

    const alice = await asUser(browser, "alice");
    try {
      const what = `${row.what}, ${row.content.length} characters, ${row.diagrams} diagrams`;
      // The protocol the spec names: three readings of the one document and the targets read
      // against the median of them (`measuredReadings` in `large-documents-page.ts`, which says
      // why it is three and why each is a page of its own). Every reading is printed as it is
      // taken and held to nothing, so what a reader is shown is the spread the median came out
      // of rather than one number that hides it.
      const readings: DocumentMeasurement[] = [];
      for (let reading = 1; reading <= measuredReadings; reading += 1) {
        const page = await alice.newPage();
        try {
          const measured = await measureDocument(page, path, {
            buildTimeoutMs: row.buildTimeoutMs,
            firstBlockTimeoutMs: row.firstBlockTimeoutMs,
          });
          printMeasurement(what, measured, {
            firstBlockTargetMs: row.firstBlockTargetMs,
            reading,
          });

          // The number the median is taken over is the editor's block, not the draw in front of
          // it: the block that was timed stood in an editor root carrying
          // `data-write-path="live"`, it was inside an editable host, and it was in neither the
          // blocks the server drew nor the page's own draw of the document's text. A rig
          // reading the preview's block fails here rather than printing that draw as the
          // editor's.
          expect(measured.firstBlock).toEqual({
            editable: true,
            inLiveEditor: true,
            inPreview: false,
          });

          // What a measured number needs to mean anything: the editor really opened and the
          // page really scrolled afterwards, so the longest task covers the whole build and
          // then a scroll through the whole document.
          await expect(documentEditor(page)).toBeVisible();
          expect(measured.scrollSteps).toBeGreaterThan(0);
          readings.push(measured);
        } finally {
          // Each reading is a first visit, so the page that took this one is shut before the
          // next opens: a page left standing holds this document's room, its socket and its
          // memory beside the reading after it.
          await page.close();
        }
      }

      const median = printTheMedian(what, readings, row.firstBlockTargetMs);
      expect(median.firstBlockMs).toBeLessThanOrEqual(row.firstBlockTargetMs);
      // And the readings the second target is taken over are readings the page really
      // collected: the `longtask` observer was watching, and it was given entries. "No task
      // over half a second" is true of no tasks at all, so the median of their longest says
      // nothing until every reading has some (dispatch://INFRA-341, the pairing every
      // universal clause of a check takes). It stands here rather than inside the loop
      // because this is the clause it guards; each reading is printed in full as it is taken,
      // so a reading that collected nothing is on the report as it happens either way.
      expectNoneOf(readings, {
        amongAtLeast: measuredReadings,
        are: (reading) => !reading.longTasksWatched || reading.longTasks.length === 0,
        matched:
          "readings whose page never took the `longtask` observer, or whose observer " +
          "recorded no main-thread task at all over the whole window",
        measured: "readings the median's longest task is taken over",
      });
      expect(median.longestTaskMs).toBeLessThanOrEqual(longTaskTargetMs);
    } finally {
      await alice.close();
    }
  });
}
