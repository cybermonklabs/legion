// The longest main-thread task of a large document's build at a desktop width, read from the
// wall clock over the 406,000-character document of `large-documents.ts`.
//
// A regression guard rather than a target of the spec: R11's targets are the phone's, and the
// two measuring rows read them under the set-up the spec names, which is the `iphone` project
// alone (`large-documents-measured.e2e.ts`). That left this number read nowhere at a desktop
// width, and a build whose longest task was 3.3 s there against 460 ms on the phone went
// unseen.
//
// This file is `chromium-alone`'s and no other project's: that project names it in its
// `testMatch`, beside the three specs it runs whole, and every parallel project ignores it
// (`clockSpecNames`, e2e/configurations.ts). The file is the selector because a title is not
// one. This row used to be picked out of a larger file by a pattern over its title, so a row
// renamed - or one added beside it under a title the pattern did not match - ran in `chromium`
// four workers wide instead, which is a reading of the other three lanes, with every suite
// green (sjawhar/legion#2697).
//
// The phone's own wall-clock rows over this document are two files beside it, one per project:
// `large-documents-measured.e2e.ts` is `iphone-measure-alone`'s, the two measurements of a
// first editable block, and `large-documents-build-serves.e2e.ts` is `iphone-alone`'s, the
// page's own clock and a decision answered inside a build. What a build of this document costs
// the page with no number read from the clock is `large-documents-long-build.e2e.ts`, which the
// parallel projects run.
//
// Every other row of what was one file reads an order, a place or a count rather than a clock:
// they are in `large-documents-build.e2e.ts`, `large-documents-reader.e2e.ts` and
// `large-documents-first-screen.e2e.ts`, and what the seven files share is
// `large-documents-rows.ts`, which holds the probe this row reads a build through
// (`whatTheBuildServes`).
import { expect, test } from "@playwright/test";

import { documentNames, largeDocument } from "./large-documents";
import { throttleTheCpu, watchTheDocument } from "./large-documents-page";
import {
  builtAcross,
  handedToTheBuild,
  seedDocument,
  theWindowHeldABuild,
  whatTheBuildServes,
} from "./large-documents-rows";
import { asUser } from "./users";

/** The longest a main-thread task of a build may hold a page at a desktop width.
 *
 *  A regression guard, not a target of the spec: R11's targets are the phone's, and the two
 *  measuring rows of `large-documents-measured.e2e.ts` read them under the set-up the spec
 *  names, which is
 *  the `iphone` project alone. That left the number below read nowhere at a desktop width, and
 *  a build whose longest task was 3.3 s there against 460 ms on the phone went unseen. Half a
 *  second is the number R11 calls a freeze, so it is the number a wider page is guarded by
 *  here too (dispatch://INFRA-341, R11).
 *
 *  Measured on this pod after the fix: 350, 406 and 416 ms over builds of 19 to 27 s with the
 *  pod otherwise quiet, against 2,680 ms before it. It is as load-sensitive as the two
 *  measuring rows and their own 500 ms, and for the same reason: run beside the whole browser
 *  suite, the worst task of a build reached 902 ms here while the phone's rows read 723 and
 *  1,114 ms for theirs. That is why the row is given a machine of its own, as they are, and why
 *  it still prints every task of the build before it reads any of them: a miss on a quiet
 *  machine is the regression this guards, and the print is what says the machine was quiet. */
const aDesktopBuildTaskAtMostMs = 500;

/** How long the row below waits for the editor to open, counted from the navigation.
 *
 *  The opening is an announcement the page makes (`watchTheDocument`), and one that never comes
 *  is a promise that never settles: left to the row's own 900 s timeout, it would outlive the 4
 *  minutes `chromium-alone`'s browser step has (.github/workflows/dispatch-browser.yaml) and end
 *  the leg with no row named and no report kept.
 *
 *  What the wait costs: on this leg, one worker on a machine of its own, the editor opened at
 *  4,091 ms of the page's own clock (run 42697341170); in a `chromium` shard leg, four workers
 *  wide, at 29,331, 52,991 and 58,754 ms (runs 42628596511, 42682941869 and 42686241170). What
 *  the leg leaves: its other rows and everything of the step that is not a row took 116 s of
 *  the 240 (run 42316487530, the job's head in that file), so 124 s are this row's, the seeding
 *  among them. 90 s is half as long again as the slowest opening ever read, on a machine three
 *  others were sharing, and leaves the seeding 34 s. */
const editorOpensWithinMs = 90_000;

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
// This row is the guard on that. It carries no `@phone` tag, and its number is read from the
// wall clock, so it runs in `chromium-alone` - Desktop Chrome with the machine to itself, one
// row at a time after every lane, which is what this file being that project's own selection
// buys it - and in no parallel project. Four workers on one runner measure the runner: in the
// `chromium` shard leg
// this row printed `longest 445 ms`, `longest 467 ms` and `longest 557 ms` over three CI runs
// of one branch, the last of them red on both attempts, while the build it reads grew from
// 23,061 ms to 41,888 ms with nothing of the page changing. The two measuring rows cover the
// phone, over a window that is wider still, and this row reads the same `longtask` entries they
// do, over the build alone.
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
    const openedMs = await reached.editorOpened(editorOpensWithinMs);

    const ran = (await page.evaluate(() => window.__theBuildRan)) ?? {};
    const began = ran.firstBatchMs ?? 0;
    const timers = handedToTheBuild((await page.evaluate(() => window.__servedTimers)) ?? [], ran);
    const [leastBuilt, mostBuilt] = builtAcross(timers);
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
        `opened at ${Math.round(openedMs)} ms, holding ${leastBuilt} top-level block(s) at ` +
        `the first of ${timers.length} timer(s) the page was handed inside the build and ` +
        `${mostBuilt} at the last. Every task of the build, as ` +
        `began@from-the-first-batch:held: ${spread}`
    );

    // The window is a real build, read from what the build had done rather than from how long
    // it took: its first batch landed, and the editor's own top-level child count rose between
    // the first timer the page was handed inside the window and the last, so the batches were
    // still arriving across it and the number below is not a target met by nothing having
    // happened. That count is the document's - the same blocks arrive in the same batches
    // whatever the machine - while the window's length is the runner's, which is why the
    // 5,000 ms floor that used to stand here was a second reading of the runner beside the one
    // this row guards: the same build measured 23,061 ms, 41,888 ms and 46,187 ms over three
    // CI runs four workers wide (42628596511, 42682941869, 42686241170) and 1,640 ms on the
    // machine of its own this row now has (42697341170), where the floor failed and the guard
    // it exists for passed at 139 ms of 500.
    expect(began).toBeGreaterThan(0);
    theWindowHeldABuild(timers);
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
