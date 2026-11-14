// What a build of a large document serves the rest of the page while it runs, read from the
// wall clock on the phone the spec names: the page's own clock and another person's edit. The
// row seeds the 406,000-character document of `large-documents.ts` at 4x CPU throttling,
// because it needs a build that lasts - a build of a few seconds hands over too few timers and
// carries too few of another person's words to say anything. A decision the reader answers
// inside the same build is the row of `large-documents-build-serves.the-decision.e2e.ts`,
// which stood here until it was moved into a file of its own, named to list right after this
// one.
//
// This file and that one are `iphone-alone`'s and no other project's: that project names both
// in its `testMatch` and every parallel project ignores both (`clockSpecNames`,
// e2e/configurations.ts). The file is the selector because a title is not one. These two rows
// used to be picked out of a larger file by a pattern over their titles, so a row renamed - or
// one added beside them under a title the pattern did not match - ran in `iphone` four workers
// wide instead, which is a reading of the other three lanes, with every suite green
// (sjawhar/legion#2697).
//
// The other files of this family, and what each holds, are listed once, at the head of
// `large-documents-rows.ts`, which holds what they share - among it the probe the row below
// reads a build through (`whatTheBuildServes`).
import { expect, test } from "@playwright/test";

import { documentNames, largeDocument } from "./large-documents";
import { throttleTheCpu, watchTheDocument } from "./large-documents-page";
import {
  handedToTheBuild,
  type ServedLate,
  type ServedWord,
  seedDocument,
  type TheOtherPersonsPen,
  theOtherPersonsPen,
  theWindowHeldABuild,
  topLevelBlocks,
  whatTheBuildServes,
} from "./large-documents-rows";
import { asUser } from "./users";

// The row of this file, and the decision row of
// `large-documents-build-serves.the-decision.e2e.ts`, are about everything on the page that is
// not the build, for as long as the build lasts. A batch hands the thread back so the page stays
// usable, and a hand-back the page is served nothing in is no hand-back at all: the build used
// to resume through `scheduler.yield()`, whose continuation the browser serves ahead of any
// posted task, so from a build's first batch to its last no socket message, no fetch callback
// and no timer ran - and React, whose scheduler posts one, drew nothing. Measured on this pod,
// `iphone` at 4x CPU throttling, a `page.evaluate` poll was answered twice and then not again
// for 209 s, and `scheduler.postTask()` at its default priority served a socket message in 7 ms
// while a timer waited 19,945 ms. It hands the thread back as a timer now (`endOfTask` in
// `packages/proof-editor/src/progressive-build.ts`), which queues the continuation behind
// everything already waiting, and three things a reader loses while nothing else runs are read
// over it: the page's own clock and another person's edit, in the row below, a decision they
// answer, in that decision row, and the edit's own arrival, which is no clock and is in
// `large-documents-long-build.e2e.ts`.

/*
 * What the row below holds another person's edit to while the reader's editor is built, and
 * what it only reports.
 *
 * **It is not held to half a second.** R11's 500 ms is a bound on a main-thread task, which
 * the two measuring rows of `large-documents-measured.e2e.ts` read; the spec asks nowhere that another
 * person's word be *served* within 500 ms. A word may wait behind several lawful tasks, so a
 * row holding the wait to 500 ms would be restating a task bound as a delivery bound and
 * asserting a promise the product never made (the architect, dispatch://INFRA-341). A new
 * promise belongs in a spec version a person approves. **Do not restore that bound here.**
 *
 * **The one thing the row holds the words to is liveness.** Every word their page sent inside
 * the build is served, inside the build or inside the swap that ends it, and the page says
 * which by itself: it numbers the turns of its own loop, stamps each word it serves with the
 * turn it served it in and its swap's own end with the turn that fell in (`TheBuildRan`'s
 * `swapTurn`), and a word whose turn is the swap's own or an earlier one was served by that
 * build. A page that stops serving while it builds fails there, and so does one that holds a
 * word into a later turn. No clock is in that reading - no tolerance, no allowance and
 * nothing of this machine's speed - and the clock stamps beside it are printed and held to
 * nothing.
 *
 * **The gap between servings is printed and held to nothing.** The longest the page went
 * without serving a word already waiting carries the relay from the writer - his socket, the
 * server, the reader's socket - as well as the reader's own serving, and no reading a page
 * can take separates them; a page that stops serving fails liveness instead. Measured on this
 * pod inside the window: 1,469 to 1,895 ms against that window's longest main-thread task of
 * 547 to 789 ms, with every word's wait in the row's own spread.
 *
 * The waits themselves are printed and go in the pull request as a measurement. Measured on
 * this pod over the 406,319-character document with the second reader writing throughout, the
 * build's longest task 614 ms: his words waited 93 to 1,710 ms, most of them 400 to 600 ms,
 * which is that pod's own batch.
 *
 * The wait is read from the writer's own stamp (`theOtherPersonsPen`) because no clock on
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
 *  which is what the desktop guard of `large-documents-desktop-build.e2e.ts` and the two
 *  measuring rows of `large-documents-measured.e2e.ts` hold; their sum is not, and their sum
 *  is what this number has to cover.
 *
 *  Measured on this pod at 4x CPU throttling, one timer outstanding at a time, over the
 *  406,319-character document with a second reader writing into it throughout, across six
 *  runs: through the batches the worst turn is 1,001 ms on `chromium` and 1,114 ms on
 *  `iphone`; the turn at the swap is 1,480 to 2,954 ms with the pod otherwise quiet, and
 *  3,923 and 5,108 ms in the two runs taken while the whole browser suite ran beside this
 *  one - the same load that took the phone's own 500 ms task target to 723 and 1,114 ms in
 *  the two measuring rows of `large-documents-measured.e2e.ts`. So 6,000 ms.
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

/** How much of the document the other person writes into: he writes from her first batch
 *  until her page says its editor has built this share of the document's own top-level
 *  blocks, and he writes nothing after that. The rest of that build is the margin his last
 *  word is served inside, which is what makes every word he sent a word the build owed.
 *
 *  A share of her build and never a number of milliseconds before its end. What a margin has
 *  to cover is the relay from the writer to her page and whatever task her page is in the
 *  middle of, and both of those are the machine's: a margin named in milliseconds is a margin
 *  measured on the machine it was written on, and the slower the machine the less of one it
 *  is. The share is the same reading on any machine, because the same batches carry the same
 *  blocks into the editor whatever the machine's speed - which is what `theWindowHeldABuild`
 *  already reads a window by - and the half still to come is hundreds of those batches, every
 *  one of them a task that could starve the socket.
 *
 *  Half, because the row needs both halves. The words he sends are the population the reading
 *  is about, and the floor below holds that count; the build still to come is what says the
 *  page served them while it was building rather than once it had finished. The word that
 *  failed this row at the head before the window was arranged was sent 21,683 ms into a
 *  22,026 ms build - 343 ms before it ended - and served one turn past the swap, which is a
 *  word handed over as a build ended rather than a build starving a socket. */
const theShareHeWritesInto = 1 / 2;

/** The fewest of the other person's words a run of the row below reads a build by: the words
 *  he sent inside the window that row arranges, which is the population its liveness
 *  reading is about. A run that carries fewer says so and fails, because a build too quick to
 *  carry two of his words is a build this row cannot read.
 *
 *  Two, because what a window carries is a count of her own build's steps and not something
 *  this row can set. Her page says how far its build has got once per animation frame in
 *  which its editor's top-level count crossed the next step (`whatTheBuildServes`), and the
 *  writer answers every step she has crossed the moment she says it, owed ones included, so
 *  a window carries the fewer of the `theWordsHerStepsAskFor` its steps ask for and the
 *  steps her build announced crossing before it reached the stop. A build of a hundred
 *  batches announces each of them and carries all eight; a build quick enough to take its
 *  986 blocks in five batches reaches the stop inside the fourth, and the third's count is
 *  all it ever asked for - two of them. Measured on this pod at this head, over four runs:
 *  her build runs 17.3 to 35.0 s over this document, and the window carries seven of the
 *  eight its steps ask for, eight in the quickest of them.
 *
 *  Two was the floor when the writer was a second browser page holding the same 406,000
 *  characters she does, where one of his words cost a layout of all of them - 1.2 s on this
 *  pod and 232 ms on CI's own runner - so what a window carried was her build's length
 *  divided by his page's speed, and CI's leg read one word against this floor and failed on
 *  its premise (run 43265368727). That cap is gone: the writer is a Node client of the
 *  document's socket now (`theOtherPersonsPen`), and a word costs him an insert and a frame
 *  rather than a layout of 406,000 characters, so what bounds the count is her build's own
 *  shape. The floor stays where it was, because what it is for never was the cap.
 *
 *  Two is a reading rather than a formality, because the population is the words he sent and
 *  never the words her page served (`theirWordsOfTheBuild`): a page that serves its socket
 *  once and then stops fails the liveness reading below on every word it was sent afterwards,
 *  whatever this floor says. What the floor is for is the other thing - saying that the
 *  window held a build at all to read - and for that, two of his words sent at two steps of
 *  her build, each paired with the turn her page served it in, is the least that can be one. */
const theFewestWordsRead = 2;

/** How many of his words that window asks for: one at each step of her build, where a step is
 *  the window's own blocks divided by this (`stepBlocks` in the row below).
 *
 *  His writing is driven by her build and by nothing else. Her page says how far it has got
 *  each time its editor's own top-level count crosses the next step (`whatTheBuildServes`),
 *  and he writes one word for each of those. So the number of words a window carries is a
 *  number of her blocks rather than of her milliseconds: a build that takes 44 s and one that
 *  takes 14 s cross the same steps and ask for the same words, and his words fall across the
 *  whole window instead of clustering wherever the writer happened to be quick. He used to
 *  write flat out from the first batch, which made the count the ratio of two machine speeds -
 *  21 words on this pod, 12 on CI's runner at one head and 2 at the next.
 *
 *  Four times `theFewestWordsRead`, so a window whose build crosses several steps inside one
 *  batch - which says them in one announcement - still clears the floor rather than sitting
 *  on it. The writer answers every step she has crossed the moment she says it, owed words
 *  included (he writes from Node, one insert and one frame), so what a window carries is her
 *  announcements and nothing of his speed. */
const theWordsHerStepsAskFor = 4 * theFewestWordsRead;

// How long the timer row below waits for each of the two announcements it needs of the page
// (`watchTheDocument`): one that never comes is a promise that never settles, and a row left to
// its own 900 s timeout outlives the 4 minutes `iphone-alone`'s browser step has
// (.github/workflows/dispatch-browser.yaml), which ends the leg with no row named and no report
// kept. Each bound is sized from what the wait costs and from what that leg leaves the row.
//
// What the waits cost. On that leg, one worker on a machine of its own, run 43098661012 timed
// the larger document's first editable block at 1,569 ms and its editor open at 2,767 ms of the
// page's own clock, and this row's build at 1,123 ms; the row took 10.3 s and the decision row
// after it 19.8 s. The slowest readings of the same page are four workers wide and on this pod
// (`large-documents-long-build.e2e.ts`, `firstBlockTimeoutMs` there): a first block up to
// 11.1 s from the navigation, and an editor open up to 14.3 s after it on CI and, on this pod,
// 42.3 s from the navigation. 30 s and 60 s, the bounds that file's second and third rows wait
// by, are near three times the first and about one and a half times the second.
//
// What the leg leaves. The row is the first of the leg's two. Everything of it but its page's
// own 2.8 s came to 7.5 s - the seeding, his admission, her page, and what it reads after - and
// that much ahead of its first wait over-states it. A page that never shows its first block
// fails the row at 37.5 s; one whose build never reaches the stop he writes into, at 7.5 s plus
// a first block of 11.1 s and `theStopWithinMs`, 63.6 s; one whose editor never opens after
// that, 60 s later still, 123.6 s at the most. The decision row's 19.8 s and the 7 s of the
// step that is no row end the step at about 150 s of its 240.

/** How long the timer row waits for the editor's first block to come into the viewport. */
const firstBlockTimeoutMs = 30_000;

/** How long the timer row's writing window may last, from her first block until her build
 *  says it has reached the stop he writes into: one deadline, which every step he waits for
 *  inside the window shares. The stop is half her build (`theShareHeWritesInto`), and the
 *  whole build took 1,123 ms on the leg (run 43098661012) and 30,150 ms on this pod; 45 s is
 *  three times the half this pod took. */
const theStopWithinMs = 45_000;

/** How long the timer row waits, once her build has told him to stop, for the editor to open. */
const buildTimeoutMs = 60_000;

/** The words the other person writes in the row below, as a pattern both sides read their own
 *  frames for: one of his writes is one frame on the socket, and the word in it is what pairs
 *  the moment he sent that frame with the moment the reader's page served it. */
const theirWordsPattern = "Pan\\d+";

/** One word the other person wrote: when he sent it and when the reader's page served the
 *  frame carrying it, both on the one machine's wall clock and both printed, and the turn of
 *  the reader's page's own loop it was served in, which is what the row reads it by.
 *  `undefined` where that page never served it at all. */
interface TheirWord {
  readonly sentMs: number;
  readonly servedMs: number | undefined;
  readonly servedTurn: number | undefined;
  readonly word: string;
}

/** Every word he sent, in the order he sent them, each read onto the reader's clock so
 *  the build's own window and its tasks can be read against them, and each carrying the turn
 *  the reader's page served it in. */
function theirWordsOfTheBuild(
  sent: Readonly<Record<string, number>>,
  served: Readonly<Record<string, ServedWord>>,
  timeOriginMs: number
): TheirWord[] {
  return Object.entries(sent)
    .map(([word, sentAt]) => {
      const servedWord = served[word];
      return {
        sentMs: sentAt - timeOriginMs,
        servedMs: servedWord === undefined ? undefined : servedWord.atMs - timeOriginMs,
        servedTurn: servedWord?.turn,
        word,
      };
    })
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

/** The longest any of them waited, and 0 where there were none. */
function longestWait(served: readonly ServedLate[]): number {
  return served.reduce((worst, task) => Math.max(worst, task.waitedMs), 0);
}

test(`a page timer and a socket message are served while ${documentNames.large}'s editor is built @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { artifactId, path } = await seedDocument("specs-large.md", largeDocument);
  // The document's own top-level blocks, from the server's index of it (`topLevelBlocks`),
  // which is what her page's count of the blocks its editor has built is read against. The
  // document's, so the point he stops writing at is a share of this build and not a number
  // this file would have to keep in step with the generator.
  const whole = await topLevelBlocks(artifactId);
  const stopAfterBlocks = Math.ceil(whole * theShareHeWritesInto);
  // How far her build goes between two of his words: the window's own blocks shared out among
  // the words it asks for (`theWordsHerStepsAskFor`), so what paces him is how far this build
  // has got and never how long it has taken.
  const stepBlocks = Math.floor(stopAfterBlocks / theWordsHerStepsAskFor);

  const alice = await asUser(browser, "alice");
  // He is in the document's room, holding the copy she is about to build, before her page
  // loads: the only thing left for him to do while she builds is write, and every word he
  // writes is a frame on her socket (`theOtherPersonsPen`).
  //
  // Opened inside the row's own `try`, so that a room which refuses him - a read-only scope, a
  // refused authentication - leaves neither him nor her context behind: he drops what he is
  // made of before he throws, and the `finally` below closes her.
  let writer: TheOtherPersonsPen | undefined;
  try {
    writer = await theOtherPersonsPen(artifactId, { theirWords: theirWordsPattern });
    const page = await alice.newPage();
    await throttleTheCpu(page);
    const reached = await watchTheDocument(page);
    const herBuild = await whatTheBuildServes(page, {
      stepBlocks,
      stopAfterBlocks,
      theirWords: theirWordsPattern,
    });
    await page.goto(path, { waitUntil: "commit" });

    // From Alice's first batch until her page says its editor has built the blocks this row
    // stops him at - half of the document's own (`theShareHeWritesInto`) - and not a word
    // after that. Both ends are announcements her page makes rather than readings this side
    // takes (`large-documents-page.ts`, `whatTheBuildServes`), because a reading taken from
    // here is answered when the build it is asking about is over.
    //
    // And inside that window her build is what asks for each word: she says how far it has
    // got each time its editor's own top-level count crosses the next step of `stepBlocks`,
    // and he answers one word to each of those. So his words are spread across the window by
    // her blocks, and how many of them it carries is her build's own number rather than the
    // ratio of her machine's speed to his.
    //
    // He used to write flat out until her editor opened, and the words he sent in the last
    // moments of a build were what this row kept failing on: at that head on this pod his
    // `Pan20` left him 21,683 ms into a 22,026 ms build and hers served it one turn past
    // the swap, and on CI's own `iphone-alone` runner a word sent 4,911 ms into a 5,147 ms
    // build was served 8 ms after that turn. Neither is a build starving a socket; both are a
    // word handed over as a build ended. So the window is arranged rather than the reading
    // loosened: every word he sends is one her build still had time to serve, and what gives
    // it that time is the half of her own build still to come - hundreds of batches, each a
    // task that could starve the socket, and the same share of the work on a fast machine and
    // a slow one. Not a number of milliseconds before the swap, which would be a margin
    // measured on the machine it was written on.
    //
    // He writes each word in one insert, so that one of his writes is one frame on her socket
    // and the word in it pairs the two stamps the wait is read from. He rests between words
    // only as long as her build makes him, and nothing of his own speed is in the pacing: a
    // word is an insert and a frame from Node, where it cost his page one to four seconds of
    // laying out the same 406,000 characters she holds, which is what used to decide how many
    // words a window carried (`theOtherPersonsPen`).
    await reached.firstBlock(firstBlockTimeoutMs);
    // One deadline for the whole writing window, shared by every wait inside it.
    const stopBy = Date.now() + theStopWithinMs;
    let words = 0;
    while (herBuild.stopped() === undefined && words < theWordsHerStepsAskFor) {
      // One word per step her build has crossed, and otherwise a wait for her next word about
      // it. A step she crossed while he was still writing the word before it is not lost: it
      // is owed, and he writes it next, so a build that runs through several steps in one of
      // his writes still gets its words.
      const owed = Math.min(
        theWordsHerStepsAskFor,
        Math.floor((herBuild.built() ?? 0) / stepBlocks)
      );
      if (words >= owed) {
        await herBuild.nextStep(stopBy - Date.now());
        continue;
      }
      writer.write(`Pan${words}`);
      words += 1;
    }
    // How far her build had got when it told him to stop, which is her page's own count: the
    // blocks this row stops him at or more, and the count her build ended on where it was
    // never told to stop at all, which is what the row fails on below.
    const builtAtTheStop = await herBuild.reachedTheStop(stopBy - Date.now());
    const openedMs = await reached.editorOpened(buildTimeoutMs);

    const ran = (await page.evaluate(() => window.__theBuildRan)) ?? {};
    const everyTimer = (await page.evaluate(() => window.__servedTimers)) ?? [];
    const timers = handedToTheBuild(everyTimer, ran);
    const sentWords = writer.sent;
    const servedWords = (await page.evaluate(() => window.__theirWordsServed)) ?? {};
    const timeOriginMs = await page.evaluate(() => performance.timeOrigin);
    // Each word he wrote is one frame of his and one of hers, so the pairing is a reading of
    // this run rather than an assumption about it: a word he never sent, or one her page
    // never served, is named below rather than guessed at.
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
    // Every word of his, in the order he wrote them, named and given as sent:waited from her
    // first batch - reported, never held to a number of its own. The names are what the
    // window carried, and where a long wait falls is what says whether the build held it.
    const theirSpread = theirs
      .map(
        (word) =>
          `${word.word}@${Math.round(word.sentMs - began)}:` +
          `${word.servedMs === undefined ? "never" : Math.round(word.servedMs - word.sentMs)}`
      )
      .join(" ");
    // What one turn of the page's own loop cost while all that ran: a task the page was given
    // waits behind whatever the thread is in the middle of, so the longest of those says what
    // the waits above are made of.
    const everyTask = (await page.evaluate(() => window.__largeDocumentLongTasks)) ?? [];
    const tasks = everyTask.filter((task) => task.at >= began && task.at <= (ran.openedMs ?? 0));
    const longestTask = tasks.reduce((worst, task) => Math.max(worst, task.ms), 0);
    // When the page said it was free again after the swap that ends the build, which is the
    // page's own stamp and no reading of this side's (`whatTheBuildServes`). Printed, to say
    // how far past the window the swap carried; nothing below is read from it.
    const swappedMs = ran.swappedMs ?? ran.openedMs ?? 0;
    // The end of the turn the page said that in, the page's own stamp too: the first task it
    // was handed from inside that turn, served once it had got through everything it was
    // already holding. Printed, and read by nothing - that backlog was 1.5 to 21 s on this
    // pod, so a row reading its words against it would have an allowance made of the thing it
    // is measuring, and a page that served nothing while it built and drained every word
    // afterwards would pass.
    const freeTurnEndedMs = ran.freeTurnEndedMs ?? swappedMs;
    // The turn the page wrote the first of those stamps in, and the whole of the liveness
    // reading below: a word served in that turn or an earlier one was served by the build and
    // the swap that ends it, and a word served in a later one is a word the page took a fresh
    // turn of work ahead of. The page hands these numbers out itself, one per task
    // (`whatTheBuildServes`), so the reading holds no clock, no tolerance and nothing of this
    // machine. No turn is 0, so a page that never finished its swap fails the reading rather
    // than passing on a missing number.
    const swapTurn = ran.swapTurn ?? 0;
    // The longest the page went without serving a word that was already waiting, over the
    // words the build itself served. Printed, asserted against nothing: it carries the relay
    // from the writer as well as this page's own serving (the block above
    // `aTimerWaitsAtMostMs` says why), and a page that stops serving fails liveness instead.
    const withoutServing = theLongestWithoutServing(
      inTheBuild.filter(
        (word) => (word.servedMs ?? Number.POSITIVE_INFINITY) <= (ran.openedMs ?? 0)
      )
    );
    const heldPastTheSwap = inTheBuild.filter(
      (word) => word.servedTurn === undefined || word.servedTurn > swapTurn
    );
    // The words the page served after the window closed: when he sent each, the turn it was
    // served in against the swap's own, and the clock beside that - how far past the window
    // and how far before the end of the swap's turn - so the output shows the shape of the
    // end of a build rather than a verdict on it. A turn of the swap's own or an earlier one
    // is the swap serving the word; a later one is what the reading below fails on.
    const servedInTheSwap = inTheBuild
      .filter((word) => word.servedMs !== undefined && word.servedMs > (ran.openedMs ?? 0))
      .map(
        (word) =>
          `${word.word} sent ${Math.round(word.sentMs - began)} ms into the build, served in ` +
          `turn ${word.servedTurn ?? 0} against the swap's own ${swapTurn}, ` +
          `${Math.round((word.servedMs ?? 0) - (ran.openedMs ?? 0))} ms past the window and ` +
          `${Math.round(freeTurnEndedMs - (word.servedMs ?? 0))} ms before the end of that ` +
          "turn"
      )
      .join("; ");
    const worstWait = inTheBuild.reduce(
      (worst, word) =>
        word.servedMs === undefined ? worst : Math.max(worst, word.servedMs - word.sentMs),
      0
    );
    // The margin, on the clock the reading does not use: the last of his words the page
    // served, and how long before the end of the swap's turn it served it. Printed so a run
    // says how much room that end of a build had rather than only that it passed.
    const servedLastMs = inTheBuild.reduce(
      (last, word) => (word.servedMs === undefined ? last : Math.max(last, word.servedMs)),
      Number.NEGATIVE_INFINITY
    );
    const margin =
      servedLastMs === Number.NEGATIVE_INFINITY
        ? "no word of his was served at all"
        : `the last of them ${Math.round(freeTurnEndedMs - servedLastMs)} ms before that ` +
          "turn ended";
    // The margin the arranged window left, on the clock no reading uses: when the last of his
    // words left him, and so how much of her build still stood between it and the swap.
    // Printed, held to nothing - what the margin is made of is the half of her build still to
    // come, and how many milliseconds that is belongs to the machine.
    const lastSentMs = inTheBuild.reduce((last, word) => Math.max(last, word.sentMs), began);
    // Printed before any of them is read against its target, as every other number in this
    // file is: a run that missed one says what it measured rather than only what it wanted.
    console.log(
      `R11 posted tasks · a build of ${Math.round(built)} ms from its first batch, ` +
        `${timers.length} timer(s) handed over inside it one at a time, worst wait ` +
        `${Math.round(longestWait(timers))} ms (target ${aTimerWaitsAtMostMs} ms, and one ` +
        `served every ${aTimerEveryMs} ms of the build); he wrote one word at each step of ` +
        `${stepBlocks} block(s) of her build, ${theWordsHerStepsAskFor} of them asked for, ` +
        `until her page said its editor had built ${builtAtTheStop} of the document's ` +
        `${whole} top-level block(s), the ${stopAfterBlocks} this row stops him at, which ` +
        `left ${Math.round((ran.openedMs ?? 0) - lastSentMs)} ms of build after the last ` +
        `word he sent; ${inTheBuild.length} of the ${theirs.length} word(s) he sent ` +
        `were sent inside that window (floor ${theFewestWordsRead}), ` +
        `${inTheBuild.length - heldPastTheSwap.length} of those served in the swap's own ` +
        `turn (${swapTurn}) or an earlier one, which is the whole of the reading; on the ` +
        `clock beside it, which no reading uses, the page put the end of that turn ` +
        `${Math.round(freeTurnEndedMs - (ran.openedMs ?? 0))} ms past the window, saying it ` +
        `was free again ${Math.round(swappedMs - (ran.openedMs ?? 0))} ms past it and ` +
        `${Math.round(freeTurnEndedMs - swappedMs)} ms before that turn's end; ` +
        `${margin}; the longest it went without serving one that was already waiting ` +
        `${Math.round(withoutServing.gapMs)} ms (${withoutServing.word}; reported, held to ` +
        `nothing - it carries the relay from the writer as well as this page's own ` +
        `serving), worst wait ${Math.round(worstWait)} ms (reported too: R11's 500 ms is a ` +
        `task bound). ${tasks.length} main-thread task(s) over 50 ms inside the build, ` +
        `longest ${Math.round(longestTask)} ms. The editor opened at ${Math.round(openedMs)} ` +
        `ms, with ${words} word(s) written into it while it was built (floor ` +
        `${theFewestWordsRead}). ` +
        `Words the swap served: ${servedInTheSwap === "" ? "none" : servedInTheSwap}. ` +
        `Every timer of the load, as handed-over@blocks-built:waited, counted from the ` +
        `first batch: ${spread}. Every word of his, as word@sent:waited, counted from the ` +
        `same batch: ${theirSpread}`
    );

    // What he wrote, against the same floor the population below is held to: these are the
    // words that went into her build at her own steps, and one short of the floor is a build
    // that crossed too few of those steps for this row to read it by.
    expect(
      words,
      `the other person wrote ${words} word(s) while her editor was being built, fewer than ` +
        `the ${theFewestWordsRead} this row reads a build by`
    ).toBeGreaterThanOrEqual(theFewestWordsRead);

    // The window is a real build, read from what the build had done rather than from how long
    // it took: the first batch landed before the editor opened, and the editor's own top-level
    // child count rose while the page was serving the timers counted below, so those timers
    // were handed over while the batches were still arriving. A duration cannot say this, and
    // a floor under one would say the opposite of what this row is for - the build on a quiet
    // machine is now about 1.3 s, and a row that failed because the build was quick would be
    // asserting that the product stays slow.
    expect(ran.firstBatchMs ?? 0).toBeGreaterThan(0);
    theWindowHeldABuild(timers);
    // And the window the reading below is taken over was arranged rather than taken: her page
    // said its build had reached the blocks he stops at while it was still building, so every
    // word he sent is one that build had the rest of itself left to serve.
    expect(
      builtAtTheStop,
      `her editor was named holding ${builtAtTheStop} of the document's ${whole} top-level ` +
        `block(s) without its build ever reaching the ${stopAfterBlocks} this row stops the ` +
        "other person's writing at, so his words ran to the end of that build and the " +
        "reading below is of a window nothing arranged"
    ).toBeGreaterThanOrEqual(stopAfterBlocks);
    // And he wrote enough words inside it for that reading to be of a build rather than of one
    // moment of one. The floor is over the words he sent, so arranging the window cannot
    // shrink the population to a handful without this saying so, and a page that holds his
    // words instead of serving them counts every one of them here and fails the reading itself
    // (`theFewestWordsRead`). Her steps ask for four times this many, so a run reaches it
    // unless her build crossed too few of those steps to ask - which is what a build too
    // quick for this row to read looks like from here, and this is where the row says so
    // rather than passing on a word or two.
    expect(
      inTheBuild.length,
      `he sent ${theirs.length} word(s), and ${inTheBuild.length} of them fell inside ` +
        `the window this row arranged - her first batch to the ${stopAfterBlocks} block(s) ` +
        `it stops him at, one word asked for at each step of ${stepBlocks} block(s) and ` +
        `${theWordsHerStepsAskFor} asked for in all - fewer than the ${theFewestWordsRead} ` +
        `words this row reads a build by: a build of ${Math.round(built)} ms carried too few ` +
        "of his words for this to be a reading of what it did to them"
    ).toBeGreaterThanOrEqual(theFewestWordsRead);

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
    // reader's build is really over.
    //
    // It is read in turns of the page's own loop and in nothing else: a word whose turn is
    // the swap's own or an earlier one was served by that build, and one whose turn is later
    // was served by a page that had already taken fresh work. Two records share a turn only
    // when one task made them both, so a word served in the swap's own task passes - no
    // reader can tell the order of two writes inside one task apart, and reading that order
    // failed this row on CI by 0 ms - while a word one task later fails. There is no clock
    // here and so no tolerance and no allowance: not a millisecond of this pod's speed, of
    // the relay from the writer, or of the backlog a build leaves behind it is in the
    // reading. A word never served at all fails the same way.
    //
    // What this proves: her page served every word the build owed it - every word he
    // sent inside a window this row arranged, which ends while half of her build is still to
    // come - in the turn of her own loop the swap ran in or an earlier one. A build that runs
    // the page's own loop and serves nothing is what fails here, whether it holds his words
    // to the end of the build or lets a few through and then stops: every word he sent is
    // counted, and the count he sent is held to a floor of its own above.
    //
    // What it does not prove: what a page does with a word handed to it in the last moments
    // before its swap. Those are the words this row stops sending before, because what
    // decides them is the relay from the writer and whichever task the thread is in the
    // middle of, and no page can be held to either (`theShareHeWritesInto`). A row that read
    // them would be reading the machine, which is how it came to fail twice on a word sent
    // inside the last quarter-second of a build.
    expect(
      heldPastTheSwap.map(
        (word) =>
          `${word.word} sent ${Math.round(word.sentMs - began)} ms into the build, ` +
          `${word.servedTurn === undefined ? "never served" : `served in turn ${word.servedTurn}, ${word.servedTurn - swapTurn} turn(s) past the one the page's swap ended in`}`
      ),
      "the page held the other person's words past the turn its swap ended in " +
        `(turn ${swapTurn}), which it put ${Math.round(swappedMs - (ran.openedMs ?? 0))} ms ` +
        "past the window"
    ).toEqual([]);
  } finally {
    writer?.close();
    await alice.close();
  }
});
