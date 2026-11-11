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
// The rest of this file is the build those numbers are read over, read as what it costs the
// reader rather than as a clock: the three things a reader loses while nothing else runs - the
// page's own clock, a decision they answer and another person's edit - and the build's own time
// over a page that already holds the whole drawn document.
//
// Which document a row seeds is in its title, and every row here but the smaller measurement
// seeds the larger one, because each of them needs a build that lasts: the measurement of it,
// the row that holds that measurement's window open over its build, the two rows that read what
// a build serves the rest of the page (a build of a few seconds hands over too few timers and
// carries too few of another person's words to say anything), the row that answers a decision
// inside a build, which needs a build still running when the page presses the card, the desktop
// guard above, and the row whose claim is what a build costs over a page of that size. The one
// other row of the suite that seeds it reads the reader's place half-way down it, and is in
// `large-documents-first-screen.e2e.ts`.
//
// Every other row of what was one file reads an order, a place or a count rather than a clock,
// and 406,000 characters buy none of them anything: they are in `large-documents-build.e2e.ts`,
// `large-documents-reader.e2e.ts` and `large-documents-first-screen.e2e.ts`, and what the four
// files share is `large-documents-rows.ts`.
import { type Browser, expect, type Page, test } from "@playwright/test";

import { getArtifactText, listIssueAsks } from "./api";
import { connectedDot, documentEditor, documentTransport, placeCaret } from "./editor";
import {
  decisionBlockId,
  diagramsIn,
  documentDiagrams,
  documentNames,
  documentTitle,
  largeDocument,
  smallDocument,
} from "./large-documents";
import {
  longTaskTargetMs,
  measureDocument,
  printMeasurement,
  throttleTheCpu,
  watchTheDocument,
} from "./large-documents-page";
import {
  blocksBuilt,
  drawnBlocks,
  everythingOwedFinished,
  type ServedLate,
  seedDocument,
  type TheBuildRan,
  topLevelBlocks,
} from "./large-documents-rows";
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
      "the measurement set-up is the iphone viewport, touch and Chromium, and the number is read from the wall clock, so it runs in `iphone-alone` with the machine to itself (e2e/playwright.config.ts's `phoneClockRows`)"
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
      // And the tasks the second target is read over are a set the page really collected: the
      // `longtask` observer was watching this page, and it was given entries. "No task over
      // half a second" is true of no tasks at all, so the longest of them says nothing until
      // both of these do (dispatch://INFRA-341, the pairing every universal clause of a check
      // takes).
      expect(
        measured.longTasksWatched,
        "the page never took the `longtask` observer, so every main-thread number of this run " +
          "is a reading of nothing"
      ).toBe(true);
      expect(
        measured.longTasks.length,
        "the observer recorded no main-thread task at all over the whole window, so the " +
          "longest task below is the target met by an empty set rather than by a quick build"
      ).toBeGreaterThan(0);

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

/*
 * What the row below holds another person's edit to while the reader's editor is built, and
 * what it only reports.
 *
 * **It is not held to half a second.** R11's 500 ms is a bound on a main-thread task, which
 * the two measuring rows at the top of this file read; the spec asks nowhere that another
 * person's word be *served* within 500 ms. A word may wait behind several lawful tasks, so a
 * row holding the wait to 500 ms would be restating a task bound as a delivery bound and
 * asserting a promise the product never made (the architect, dispatch://INFRA-341). A new
 * promise belongs in a spec version a person approves. **Do not restore that bound here.**
 *
 * **The one thing the row holds the words to is liveness.** Every word their page sent inside
 * the build is served, inside the build or inside the swap that ends it, which the page stamps
 * itself (`TheBuildRan`'s `swappedMs`). A page that stops serving while it builds fails there,
 * and so does one that holds a word past its own swap.
 *
 * **The gap between servings is printed and held to nothing.** The longest the page went
 * without serving a word already waiting carries the relay between two browsers - the writer's
 * page, the server, the reader's socket - as well as the reader's own serving, and no reading
 * a page can take separates them; a page that stops serving fails liveness instead. Measured
 * on this pod inside the window: 1,469 to 1,895 ms against that window's longest main-thread
 * task of 547 to 789 ms, with every word's wait in the row's own spread.
 *
 * The waits themselves are printed and go in the pull request as a measurement. Measured on
 * this pod over the 406,319-character document with the second reader writing throughout, the
 * build's longest task 614 ms: his words waited 93 to 1,710 ms, most of them 400 to 600 ms,
 * which is that pod's own batch.
 *
 * The wait is read from the writer's own stamp (`whenTheirWordsAreSent`) because no clock on
 * the reader's side holds the moment a frame arrived. Until INFRA-341 the row read
 * `performance.now() - event.timeStamp`, and a browser stamps a message event in the task that
 * dispatches it, so the expression read a few microseconds however long the frame had waited.
 * The browser's own `Network.webSocketFrameReceived` is no better: it is stamped in the
 * renderer as Blink takes the frame off the pipe, and measured here - with the page holding
 * its thread for two seconds inside the listener - it read 0 to 37 ms for every frame of a
 * build, exactly as the event's own stamp does; Playwright's `framereceived` is told of a
 * frame 4 to 319 ms after the page's own listener ran, so every wait read that way came out
 * negative. The writer's stamp is on the other side of the wire, so it carries the relay as
 * well as the wait, which overstates rather than flatters.
 */

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

/** The words the other person writes in the row below, as a pattern both pages read their own
 *  frames for: one of his writes is one frame on the socket, and the word in it is what pairs
 *  the moment his page sent that frame with the moment the reader's page served it. */
const theirWordsPattern = "Pan\\d+";

/** How large a frame the reader's page reads for those words. The document's own state frame
 *  is hundreds of kilobytes and carries none of them; a frame of one write is a few hundred
 *  bytes. */
const theirWordsInFramesUnderBytes = 16_384;

/**
 * Records what the page's own posted tasks cost it while the editor is being built: every
 * timer the page hands the browser, the build's own window, and - where `theirWords` names a
 * pattern - `Date.now()` the moment the page serves the socket frame carrying each word
 * another person wrote, so the row can read what the build made each of them wait.
 *
 * A timer is handed over from an animation frame, because a frame is the one thing that does
 * run through a starved build and so the one place a probe can be handed over from.
 *
 * A word is read out of the frame's own bytes, which is what pairs it with the moment the
 * other person's page sent it (`whenTheirWordsAreSent`): the two stamps are the one machine's
 * wall clock, so the difference is what the reader's thread made that edit wait. Nothing of
 * the frame's arrival is read here, because no clock inside the page holds it - a
 * `MessageEvent` is stamped in the task that dispatches it, and the browser's own
 * `Network.webSocketFrameReceived` is stamped in this renderer as Blink takes the frame off
 * the pipe, which a thread the build owns delays as surely as it delays the event. Both read
 * zero behind a page held for two seconds, measured (dispatch://INFRA-341).
 *
 * A frame larger than `theirWordsInFramesUnderBytes` is left unread: the document's own state
 * frame is hundreds of kilobytes and carries no word of theirs, and decoding it would be this
 * probe holding the thread it is measuring. Only the first document socket is recorded. The
 * socket is wrapped as a `construct` proxy rather than a subclass for the reason
 * `large-documents-page.ts` gives: the page's transport subclasses `globalThis.WebSocket` too.
 *
 * Install before the navigation; read once the editor has opened.
 */
async function whatTheBuildServes(page: Page, theirWords?: string): Promise<void> {
  await page.addInitScript(
    ({
      every,
      live,
      named,
      under,
      words,
    }: {
      every: number;
      live: string;
      named: string;
      under: number;
      words: string | undefined;
    }) => {
      const timers: ServedLate[] = [];
      const servedWords: Record<string, number> = {};
      const ran: TheBuildRan = {};
      window.__servedTimers = timers;
      window.__theirWordsServed = servedWords;
      window.__theBuildRan = ran;
      let recording = false;
      const theirs = words === undefined ? undefined : new RegExp(words, "g");
      const read = new TextDecoder();
      window.WebSocket = new Proxy(window.WebSocket, {
        construct(made, args: [string | URL, (string | string[])?], asked) {
          const socket = Reflect.construct(made, args, asked) as WebSocket;
          if (recording || theirs === undefined || !String(args[0]).includes("/ws/doc/")) {
            return socket;
          }
          recording = true;
          socket.addEventListener("message", (event) => {
            const servedAt = Date.now();
            const data: unknown = event.data;
            if (!(data instanceof ArrayBuffer) || data.byteLength > under) return;
            for (const [word] of read.decode(data).matchAll(theirs)) {
              servedWords[word] ??= servedAt;
            }
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
          // And the swap that ends the build runs in the tasks after this frame: the binding
          // adopting the document, the editor being revealed, and the margin placing its cards
          // over it for the first time. The page says when that is over rather than leaving it
          // to be inferred from a run of `longtask` entries: a task handed over now runs after
          // the work already queued, so the frame it is served in is the page's own word that
          // it is free again, and it is the stamp the row reads for every word the swap owed.
          requestAnimationFrame(() => {
            setTimeout(() => {
              ran.swappedMs = performance.now();
            }, 0);
          });
          return;
        }
        requestAnimationFrame(look);
      };
      requestAnimationFrame(look);
    },
    {
      every: timerEveryMs,
      live: liveEditorRoot,
      named: namedEditor,
      under: theirWordsInFramesUnderBytes,
      words: theirWords,
    }
  );
}

/**
 * Stamps, in the page that writes them, when each word matching `theirWords` left on the
 * document's socket: the moment the other person's edit went out, read where it went out.
 *
 * This is the half of a socket message's wait the reader's own page cannot hold. The frame
 * reaches the server and comes back down the reader's socket in a millisecond or two on one
 * machine, so the stamp is the reader's frame arriving to within that, and the wait read
 * against it carries the relay with it - which is the direction that cannot flatter the
 * reader's page.
 *
 * The socket is wrapped the way `large-documents-page.ts` wraps it, and `send` sits on the
 * instance in front of the transport's own override on the prototype. Install before the
 * navigation.
 */
async function whenTheirWordsAreSent(page: Page, theirWords: string): Promise<void> {
  await page.addInitScript((words: string) => {
    const sent: Record<string, number> = {};
    window.__theirWordsSent = sent;
    const theirs = new RegExp(words, "g");
    const read = new TextDecoder();
    window.WebSocket = new Proxy(window.WebSocket, {
      construct(made, args: [string | URL, (string | string[])?], asked) {
        const socket = Reflect.construct(made, args, asked) as WebSocket;
        if (!String(args[0]).includes("/ws/doc/")) {
          return socket;
        }
        const sends = Object.getPrototypeOf(socket).send as WebSocket["send"];
        socket.send = (data) => {
          if (data instanceof ArrayBuffer || ArrayBuffer.isView(data)) {
            const sentAt = Date.now();
            for (const [word] of read.decode(data).matchAll(theirs)) {
              sent[word] ??= sentAt;
            }
          }
          sends.call(socket, data);
        };
        return socket;
      },
    });
  }, theirWords);
}

/** One word the other person wrote, on the reader's page's own clock: when their page sent it,
 *  and when the reader's page served the frame carrying it, `undefined` where it never did. */
interface TheirWord {
  readonly sentMs: number;
  readonly servedMs: number | undefined;
  readonly word: string;
}

/** Every word their page sent, in the order it sent them, each read onto the reader's clock so
 *  the build's own window and its tasks can be read against them. */
function theirWordsOfTheBuild(
  sent: Readonly<Record<string, number>>,
  served: Readonly<Record<string, number>>,
  timeOriginMs: number
): TheirWord[] {
  return Object.entries(sent)
    .map(([word, sentAt]) => ({
      sentMs: sentAt - timeOriginMs,
      servedMs: served[word] === undefined ? undefined : served[word] - timeOriginMs,
      word,
    }))
    .sort((one, next) => one.sentMs - next.sentMs);
}

/**
 * The longest the page went without serving a word that was waiting for it: for each word the
 * page served, the time from the moment it could have been served - the later of their page
 * sending it and the page finishing the word before it - until the page served it, and the
 * worst of those, with the word it was.
 *
 * This is the no-starvation reading, and it is not the wait: a word sent while the page is
 * still on the word before it is owed only the time after that one, and a page serving its
 * socket between the build's tasks owes each word the task it is in the middle of and no more.
 */
function theLongestWithoutServing(words: readonly TheirWord[]): {
  gapMs: number;
  word: string;
} {
  const served = words
    .filter((word): word is TheirWord & { servedMs: number } => word.servedMs !== undefined)
    .sort((one, next) => one.servedMs - next.servedMs);
  let worst = { gapMs: 0, word: "none" };
  let servedLast: number | undefined;
  for (const word of served) {
    const couldHave = servedLast === undefined ? word.sentMs : Math.max(word.sentMs, servedLast);
    const gapMs = word.servedMs - couldHave;
    if (gapMs > worst.gapMs) worst = { gapMs, word: word.word };
    servedLast = word.servedMs;
  }
  return worst;
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

/** How far the build had got at the first and at the last of the tasks the page was handed
 *  inside a window: the fewest and the most top-level blocks the editor's own host held at any
 *  of them. `[0, 0]` where it was handed none. */
function builtAcross(served: readonly ServedLate[]): [number, number] {
  const built = served.map((task) => task.builtThen ?? 0);
  return built.length === 0 ? [0, 0] : [Math.min(...built), Math.max(...built)];
}

/**
 * Fails unless the timers the page was handed inside a window show the document being built
 * across it: two of them at least, and the editor holding more top-level blocks at the last of
 * them than at the first, which is the build's own batches arriving while they were served.
 *
 * This is the one proof in this file that a window a row measures held a build, and both rows
 * that read such a window use it. It is a reading of the document rather than of the runner:
 * the same batches carry the same blocks into the editor whatever the machine's speed, so the
 * rise is there on a loaded runner and on an idle one. A window the document was not built in -
 * one that ended before the first batch, or opened after the editor did - holds either no timer
 * at all or timers that all read the same count, and fails here.
 */
function theWindowHeldABuild(timers: readonly ServedLate[]): void {
  const [least, most] = builtAcross(timers);
  expect(
    timers.length,
    "the page served fewer than two of its own timers inside the window, so nothing here " +
      "shows it held a build at all"
  ).toBeGreaterThanOrEqual(2);
  expect(
    most - least,
    `the editor held ${least} top-level blocks when the page served its first timer of the ` +
      `window and ${most} when it served its last: the window did not cover the document ` +
      "being built"
  ).toBeGreaterThan(0);
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
    // Before his page has a socket, so the moment each of his writes leaves it is his page's
    // own stamp: the reader's page cannot hold that moment, and this row's claim is the span
    // between the two.
    await whenTheirWordsAreSent(bobPage, theirWordsPattern);
    await bobPage.goto(path);
    await expect(documentEditor(bobPage)).toContainText(title, { timeout: 240_000 });
    await expect(connectedDot(bobPage)).toHaveText("connected");
    await placeCaret(bobPage, "after", title);

    const page = await alice.newPage();
    await throttleTheCpu(page);
    const reached = await watchTheDocument(page);
    await whatTheBuildServes(page, theirWordsPattern);
    await page.goto(path, { waitUntil: "commit" });

    // From Alice's first batch until her editor opens, so his words cover her whole build
    // rather than a moment this row has to guess at. Both moments are announcements the page
    // makes rather than readings this side takes (`large-documents-page.ts`).
    //
    // He writes each word in one insert, so that one of his writes is one frame on her socket
    // and the word in it pairs the two stamps the wait is read from. He rests between words
    // only as long as his own page makes him: he used to rest half a second so that four words
    // a second would not go into a document of 986 blocks - a load no reader puts on another -
    // and that rest was written when a build of this document lasted thirteen seconds. It does
    // not any more: on a quiet machine the build is now about 1.3 s (dispatch://INFRA-341,
    // R11), and a writer who rests half a second gets two words into it, which is fewer than
    // this row needs to have proved anything. What bounded his rate was never that rest
    // anyway, but his own page: he holds the same 986-block document, and each of his writes
    // costs a layout of it and of the margin beside it, so on a loaded machine `Pangolin0` was
    // three to four seconds of his. Where the machine is slow his page still paces him, and
    // where it is fast his words cover a fast build.
    await reached.firstBlock;
    let open = false;
    const opened = reached.editorOpened.then((at) => {
      open = true;
      return at;
    });
    let words = 0;
    while (!open && words < 60) {
      await bobPage.keyboard.insertText(` Pan${words}`);
      words += 1;
    }
    const openedMs = await opened;

    const ran = (await page.evaluate(() => window.__theBuildRan)) ?? {};
    const everyTimer = (await page.evaluate(() => window.__servedTimers)) ?? [];
    const timers = handedToTheBuild(everyTimer, ran);
    const sentWords = (await bobPage.evaluate(() => window.__theirWordsSent)) ?? {};
    const servedWords = (await page.evaluate(() => window.__theirWordsServed)) ?? {};
    const timeOriginMs = await page.evaluate(() => performance.timeOrigin);
    // Each word he wrote is one of his page's frames and one of hers, so the pairing is a
    // reading of this run rather than an assumption about it: a word his page never sent, or
    // one hers never served, is named below rather than guessed at.
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
    const theirs = theirWordsOfTheBuild(sentWords, servedWords, timeOriginMs);
    // The words he sent inside her build's own window: the ones that build could have held.
    // One he sent after it says nothing about this build and is left out.
    const inTheBuild = theirs.filter(
      (word) => word.sentMs >= began && word.sentMs <= (ran.openedMs ?? 0)
    );
    // Every word of his, in the order he wrote them, as sent:waited from her first batch -
    // reported, never held to a number of its own. Where a long wait falls is what says
    // whether the build is what held it.
    const theirSpread = theirs
      .map(
        (word) =>
          `${Math.round(word.sentMs - began)}:` +
          `${word.servedMs === undefined ? "never" : Math.round(word.servedMs - word.sentMs)}`
      )
      .join(" ");
    // What one turn of the page's own loop cost while all that ran: a task the page was given
    // waits behind whatever the thread is in the middle of, so the longest of those says what
    // the waits above are made of.
    const everyTask = (await page.evaluate(() => window.__largeDocumentLongTasks)) ?? [];
    const tasks = everyTask.filter((task) => task.at >= began && task.at <= (ran.openedMs ?? 0));
    const longestTask = tasks.reduce((worst, task) => Math.max(worst, task.ms), 0);
    // When the page was free again after the swap that ends the build, which is the page's own
    // stamp and no reading of this side's (`whatTheBuildServes`). It is the moment a reader's
    // build is really over, and the moment every word the build owed has to have been served
    // by. A page that never wrote it never finished its swap, and `openedMs` stands in so the
    // liveness reading below fails rather than passing on a missing number.
    const swappedMs = ran.swappedMs ?? ran.openedMs ?? 0;
    // The longest the page went without serving a word that was already waiting, over the
    // words the build itself served. Printed, asserted against nothing: it carries the relay
    // between two browsers as well as this page's own serving (the block above
    // `aTimerWaitsAtMostMs` says why), and a page that stops serving fails liveness instead.
    const withoutServing = theLongestWithoutServing(
      inTheBuild.filter(
        (word) => (word.servedMs ?? Number.POSITIVE_INFINITY) <= (ran.openedMs ?? 0)
      )
    );
    const heldPastTheSwap = inTheBuild.filter(
      (word) => word.servedMs === undefined || word.servedMs > swappedMs
    );
    // The words the build itself did not serve: when he sent each and how far past the window
    // and the swap's own end the page served it, so the output shows the shape of the end of a
    // build rather than a verdict on it.
    const servedInTheSwap = inTheBuild
      .filter((word) => word.servedMs !== undefined && word.servedMs > (ran.openedMs ?? 0))
      .map(
        (word) =>
          `${word.word} sent ${Math.round(word.sentMs - began)} ms into the build, served ` +
          `${Math.round((word.servedMs ?? 0) - (ran.openedMs ?? 0))} ms past the window and ` +
          `${Math.round((word.servedMs ?? 0) - swappedMs)} ms past the swap's own end`
      )
      .join("; ");
    const worstWait = inTheBuild.reduce(
      (worst, word) =>
        word.servedMs === undefined ? worst : Math.max(worst, word.servedMs - word.sentMs),
      0
    );
    // Printed before any of them is read against its target, as every other number in this
    // file is: a run that missed one says what it measured rather than only what it wanted.
    console.log(
      `R11 posted tasks · a build of ${Math.round(built)} ms from its first batch, ` +
        `${timers.length} timer(s) handed over inside it one at a time, worst wait ` +
        `${Math.round(longestWait(timers))} ms (target ${aTimerWaitsAtMostMs} ms, and one ` +
        `served every ${aTimerEveryMs} ms of the build); ${inTheBuild.length} of the ` +
        `${theirs.length} word(s) his page sent were sent inside it, ` +
        `${inTheBuild.length - heldPastTheSwap.length} of those served by the time the page ` +
        `was free again, which it said at ${Math.round(swappedMs - (ran.openedMs ?? 0))} ms ` +
        `past the window; the longest it went without serving one that was already waiting ` +
        `${Math.round(withoutServing.gapMs)} ms (${withoutServing.word}; reported, held to ` +
        `nothing - it carries the relay between two browsers as well as this page's own ` +
        `serving), worst wait ${Math.round(worstWait)} ms (reported too: R11's 500 ms is a ` +
        `task bound). ${tasks.length} main-thread task(s) over 50 ms inside the build, ` +
        `longest ${Math.round(longestTask)} ms. The editor opened at ${Math.round(openedMs)} ` +
        `ms, with ${words} word(s) written into it while it was built (target more than 3). ` +
        `Words the swap served: ${servedInTheSwap === "" ? "none" : servedInTheSwap}. ` +
        `Every timer of the load, as handed-over@blocks-built:waited, counted from the ` +
        `first batch: ${spread}. Every word of his, as sent:waited, counted from the same ` +
        `batch: ${theirSpread}`
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
    theWindowHeldABuild(timers);
    // And the page was given both kinds of task inside it, so neither reading below is the
    // bound met by nothing having happened.
    expect(
      inTheBuild.length,
      "the other person wrote none of his words inside the build, so nothing below is a " +
        "reading of what that build did to them"
    ).toBeGreaterThan(3);

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
    // Liveness, and the only thing the words are held to: every word he sent inside the build
    // was served, inside the build or inside the swap that ended it - the binding adopting the
    // document, the editor being revealed and the margin placing its cards, which is when a
    // reader's build is really over. The end of that swap is the page's own stamp
    // (`ran.swappedMs`, written in the frame the page was free again), never a run of tasks
    // this side groups, so a word still unserved then is a failure here, and so is one never
    // served at all, whatever the machine's speed.
    expect(
      heldPastTheSwap.map(
        (word) =>
          `${word.word} sent ${Math.round(word.sentMs - began)} ms into the build, ` +
          `${word.servedMs === undefined ? "never served" : `served ${Math.round(word.servedMs - swappedMs)} ms past the swap's own end`}`
      ),
      `the page held the other person's words past the swap that ended its build, which it ` +
        `said was over ${Math.round(swappedMs - (ran.openedMs ?? 0))} ms past the window`
    ).toEqual([]);
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
 *  1,114 ms for theirs. That is why the row is given a machine of its own, as they are, and why
 *  it still prints every task of the build before it reads any of them: a miss on a quiet
 *  machine is the regression this guards, and the print is what says the machine was quiet. */
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
// This row is the guard on that. It carries no `@phone` tag, and its number is read from the
// wall clock, so it runs in `chromium-alone` - Desktop Chrome with the machine to itself, one
// row at a time after every lane (`desktopClockRow`, e2e/playwright.config.ts) - and in no
// parallel project. Four workers on one runner measure the runner: in the `chromium` shard leg
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
    const openedMs = await reached.editorOpened;

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
