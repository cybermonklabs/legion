// The probe the wall-clock rows read a build through (`whatTheBuildServes`), what it records
// and says on the page's `window`, and the readings those rows take of a window it closed. The
// timer row of `large-documents-build-serves.e2e.ts`, which `iphone-alone` runs, and the
// desktop guard of `large-documents-desktop-build.e2e.ts`, which `chromium-alone` runs, read a
// build through it, and no other file does. The two editor selectors it carries are
// `large-documents-rows.ts`'s, because two more files take them alone. What the probe writes
// is the three window readings declared below - `__servedTimers`, `__theirWordsServed` and
// `__freeTurnEndedMs` - and what it says is the two announcements declared beside them, so the
// writer stands beside the declarations of what it writes.
//
// The other files of this family, and what each holds, are listed once, at the head of
// `large-documents-rows.ts`.
import { expect, type Page } from "@playwright/test";

import { theMomentThePageReached } from "./large-documents-page";
import { liveEditorRoot, namedEditor } from "./large-documents-rows";

/** What one task the page was given while the editor was being built cost it: when it was
 *  given, on the page's own clock, and how long it then waited before it ran. The timer row of
 *  `large-documents-build-serves.e2e.ts` and the desktop guard of
 *  `large-documents-desktop-build.e2e.ts` read these - one per timer
 *  `whatTheBuildServes` hands over, and one per word another person wrote, which the timer row
 *  builds from the stamp the writer's page took as it sent the word and the stamp the
 *  reader's page took as it served the frame carrying it. */
export interface ServedLate {
  /** `performance.now()` when the task was given: when `setTimeout` was called for a timer,
   *  and the moment the other person's page sent the word, counted from the reader's page's
   *  time origin, for one of their words. */
  readonly handedAt: number;
  /** How many top-level blocks the editor held when it was handed over, for a timer the probe
   *  below hands over; `undefined` for one of their words, which arrives when it arrives. */
  readonly builtThen?: number;
  /** How long it waited from then until it ran. */
  readonly waitedMs: number;
}

/** When the reader's page served the frame carrying one of another person's words: the wall
 *  clock it served it on, which `large-documents-build-serves.e2e.ts`'s timer row prints, and
 *  the turn of the page's own loop it was served in, which is the whole of what that row
 *  reads. A turn is a number the page hands itself, one per task (`whatTheBuildServes` below):
 *  a word whose turn is the swap's own or an earlier one was served by the build, and one
 *  whose turn is later was served by a page that had already taken a fresh turn of work ahead
 *  of it. */
export interface ServedWord {
  readonly atMs: number;
  readonly turn: number;
}

/** The build's own window on the page's clock and the swap that ends it, as
 *  `whatTheBuildServes` says them in one announcement (`swapped`): the frame its first batch
 *  was found in, the frame the finished editor was named in, and the moment the page was free
 *  again after the swap that ends a build - the adoption, the reveal and the margin's first
 *  placement - which the page says by serving a task it was handed in that frame, with the turn
 *  of its own loop it served it in. A reader's build is over at the third of these, not the
 *  second.
 *
 *  `swapTurn` is the only one of these the liveness reading uses. The first two are the window
 *  a row reads the timers and words of the build over, and `swappedMs` is printed and held to
 *  nothing. The page says this only once it holds all four - and where it never will, because
 *  its editor was named with no first batch found before it, it says why instead - so a row
 *  has no part of this to read before it is whole and no missing field to stand a default in
 *  for. */
export interface TheBuildRan {
  readonly firstBatchMs: number;
  readonly openedMs: number;
  readonly swappedMs: number;
  readonly swapTurn: number;
}

declare global {
  interface Window {
    /** What `whatTheBuildServes` recorded: how long each timer the page handed the browser
     *  waited to be run, and the turn of the page's own loop each word another person wrote
     *  was served in - with `Date.now()` as it served the frame carrying it, which is printed
     *  and nothing more. Which of them the build was given is the build's own window, which the
     *  page says rather than records (`__theBuildSwapped`). The same wall clock is stamped on
     *  the other side by the writer himself, as the frame carrying each word leaves him
     *  (`theOtherPersonsPen` in `large-documents-rows.ts`), which is the half of a word's wait
     *  no reading inside this page can take. */
    __servedTimers?: ServedLate[];
    __theirWordsServed?: Record<string, ServedWord>;
    /** The end of the turn the page said its swap in, as `whatTheBuildServes` stamps it from a
     *  task handed over inside that turn: printed and held to nothing, and a task later than
     *  the swap, so a reading taken once the swap is said may not hold it yet. */
    __freeTurnEndedMs?: number;
    /** How far the editor's build has got, as `whatTheBuildServes` says it: its own top-level
     *  block count each time that count crosses the next step the liveness row of
     *  `large-documents-build-serves.e2e.ts` writes the other person's words by, and again -
     *  with `itsStop` - when it reaches the blocks that row stops his writing at or when the
     *  editor was named without ever reaching them. Said in the task the page read the count
     *  in. An announcement and not a reading the run takes, for the reason `watchTheDocument`
     *  in `large-documents-page.ts` gives - a page building the rest of a 406,000-character
     *  document answers nothing the run asks it until that build is over, which is the whole
     *  of what the row is arranging its window against. */
    __theBuildReached?: (built: number, itsStop: boolean) => void;
    /** The swap that ends the build, with the build's whole window (`TheBuildRan`), as
     *  `whatTheBuildServes` says it in the task it stamps the swap in; or, where the editor was
     *  named with no first batch found before it, why there is no build to say. Said rather
     *  than left to a reading because the stamp comes tasks after the editor opens, and a
     *  reading taken between the two holds no swap. */
    __theBuildSwapped?: (ran: TheBuildRan | string) => void;
  }
}

/** How often the probe below hands the browser a timer. Often enough that a build of a few
 *  seconds is covered by a dozen of them, rarely enough that the probe is not itself the thing
 *  keeping the page busy. */
export const timerEveryMs = 250;

/** How large a frame the reader's page reads for the words another person writes into the
 *  document (`theirWords` below). The document's own state frame is hundreds of kilobytes and
 *  carries none of them; a frame of one write is a few hundred bytes. */
export const theirWordsInFramesUnderBytes = 16_384;

/** What the probe below answers a row: her page's own word about how far its build has got,
 *  said at each step of it, which is what the timer row writes and stops by, and its word
 *  that the swap ending that build is over, which is when a row reads what the probe
 *  recorded. */
export interface TheBuildServed {
  /** The editor's own top-level block count as her page last said it, and `undefined` before
   *  it has said anything: what the row reads to know how many of his words her build has
   *  asked for so far. */
  readonly built: () => number | undefined;
  /** Her next word about it, which is how the writer waits: for a step of her build rather
   *  than for a span of anyone's clock. Settles with the count she says; fails, naming what
   *  never came, where she says nothing more inside `timeoutMs`. */
  readonly nextStep: (timeoutMs: number) => Promise<number>;
  /** The count she said when her build reached the blocks the caller named - that many or
   *  more - or the count her editor was named holding, where the build never reached them,
   *  which the row fails on rather than waiting for a word that will never come. Settles
   *  once, and only where the caller named a stop; fails, naming what never came, where
   *  neither has happened inside `timeoutMs`. */
  readonly reachedTheStop: (timeoutMs: number) => Promise<number>;
  /** That same count as soon as she has said it, and `undefined` until then: the writing loop
   *  stops between two of its own keystrokes rather than waiting on a promise, so the word it
   *  is in the middle of is the last one it sends. */
  readonly stopped: () => number | undefined;
  /** The swap that ends her build, with the build's whole window, as her page said it in the
   *  task it stamped the swap in: what a row waits for before it reads anything the probe
   *  recorded. The build's window is closed in the frame after her editor is named rather
   *  than when a row hears the editor is open, every timer the probe handed over inside that
   *  window was handed over ahead of the swap's own task, and the swap's stamp comes tasks
   *  after her editor opens, so a reading taken any sooner can hold none of them. Settles
   *  once; fails, naming what never came, where she has not said it inside `timeoutMs`, and
   *  naming why where her editor was named with no first batch found before it, which leaves
   *  no build to read. */
  readonly swapped: (timeoutMs: number) => Promise<TheBuildRan>;
}

/**
 * Records what the page's own posted tasks cost it while the editor is being built - every
 * timer the page hands the browser and, where `theirWords` names a pattern, the turn of its own
 * loop the page served the socket frame carrying each word another person wrote in - and says
 * the build's own window, so the row can read whether the build was still what served them.
 *
 * **A turn is the page's own number for the task it is in.** The page keeps a counter and
 * hands it out lazily: the first thing recorded in a task takes `++turns` and queues a
 * microtask that drops it again, so everything recorded in that task is given the same
 * number and the next task takes a fresh one. That is exact rather than close, because the
 * checkpoint that drops the number is the one the browser runs when the stack empties at the
 * end of a callback, which no task spans: two records share a number only if one task served
 * them both, and a larger number means a later task, whatever the machine's speed. Nothing
 * the page already publishes can say that - `performance.now()` and the `longtask` entries
 * are a clock, an animation frame is several tasks wide, and a `MessageChannel` message or a
 * posted task has no identity to carry - so the counter is what the swap's own write and each
 * served word are stamped with, and the row's liveness reading is those two numbers and
 * nothing else.
 *
 * A timer is handed over from an animation frame, because a frame is the one thing that does
 * run through a starved build and so the one place a probe can be handed over from.
 *
 * A word is read out of the frame's own bytes, which is what pairs it with the moment the
 * other person sent it (`theOtherPersonsPen`): the two wall-clock stamps are the one
 * machine's, so the difference is what the reader's thread made that edit wait, which the row
 * prints and holds to nothing. Nothing of the frame's arrival is read here, because no clock
 * inside the page holds it - a `MessageEvent` is stamped in the task that dispatches it, and
 * the browser's own `Network.webSocketFrameReceived` is stamped in this renderer as Blink
 * takes the frame off the pipe, which a thread the build owns delays as surely as it delays
 * the event. Both read zero behind a page held for two seconds, measured
 * (dispatch://INFRA-341).
 *
 * A frame larger than `theirWordsInFramesUnderBytes` is left unread: the document's own state
 * frame is hundreds of kilobytes and carries no word of theirs, and decoding it would be this
 * probe holding the thread it is measuring. Only the first document socket is recorded, and it
 * is read through `watchTheDocument`'s wrap of the page's WebSocket, which hands this each
 * document socket it makes (`__documentSocketMade` in `large-documents-page.ts`): that is the
 * one wrap a page carries, so a row that names `theirWords` calls `watchTheDocument` on the
 * same page, as both rows that read a build through this do.
 *
 * Where `stepBlocks` names a count, the page says how far its editor's build has got each
 * time its own top-level block count crosses the next multiple of it, in the task it read
 * that count in; and where `stopAfterBlocks` names one, it says so again the moment the count
 * reaches that, and with the count it ended on where the editor was named without ever
 * reaching it. Those are the announcements the row writes the other person's words by and
 * stops his writing by, and they are announcements rather than readings for the reason
 * `watchTheDocument` gives: a run that asked this page how far its build had got would be
 * answered when the build was over.
 *
 * The swap that ends the build is said too, in the task the page stamps it in, and with it the
 * build's whole window - the first batch and the naming as well as the swap's turn and clock -
 * in one announcement that holds every one of them (`swapped`, `TheBuildRan`). That stamp is a
 * task handed over in the frame after the one the editor was named in, so it comes tasks after
 * the editor opens, and a run that read a record of the build the moment it heard the editor
 * was open could read it with no swap in it yet. CI's `iphone-alone` leg did, at `beef6c36`
 * (run 43718779158): its trace's reading of the record this probe kept then held the first
 * batch and the opening and nothing else, the row took the missing turn for turn 0, and it
 * failed six words the build had served before its editor even opened. Where the editor is
 * named with no first batch found before it, the page says that instead, and the row fails on
 * it by name.
 *
 * Install before the navigation; read once the page has said its swap (`swapped`), because
 * its records of the build are not whole before that (`TheBuildServed`).
 */
export async function whatTheBuildServes(
  page: Page,
  {
    stepBlocks = 0,
    stopAfterBlocks = 0,
    theirWords,
  }: { stepBlocks?: number; stopAfterBlocks?: number; theirWords?: string } = {}
): Promise<TheBuildServed> {
  const reached = Promise.withResolvers<number>();
  let built: number | undefined;
  let stopped: number | undefined;
  let step = Promise.withResolvers<number>();
  const swapped = Promise.withResolvers<TheBuildRan | string>();
  await page.exposeFunction("__theBuildSwapped", (said: TheBuildRan | string) => {
    swapped.resolve(said);
  });
  await page.exposeFunction("__theBuildReached", (said: number, itsStop: boolean) => {
    built = said;
    if (itsStop) {
      stopped ??= said;
      reached.resolve(said);
    }
    step.resolve(said);
    step = Promise.withResolvers<number>();
  });
  await page.addInitScript(
    ({
      every,
      live,
      named,
      perStep,
      stop,
      under,
      words,
    }: {
      every: number;
      live: string;
      named: string;
      perStep: number;
      stop: number;
      under: number;
      words: string | undefined;
    }) => {
      const timers: ServedLate[] = [];
      const servedWords: Record<string, ServedWord> = {};
      window.__servedTimers = timers;
      window.__theirWordsServed = servedWords;
      /** The turn of the page's own loop this probe is recording in, handed out lazily: the
       *  first record of a task takes the next number and queues the microtask that drops it,
       *  which the browser runs when the stack empties at the end of that task, so every
       *  record of one task carries one number and the next task's records carry a larger
       *  one. This is the whole of the row's liveness reading and it holds no clock. */
      let turns = 0;
      let held: number | undefined;
      const thisTurn = (): number => {
        if (held === undefined) {
          held = ++turns;
          queueMicrotask(() => {
            held = undefined;
          });
        }
        return held;
      };
      // The first document socket's frames, read through `watchTheDocument`'s wrap of the
      // page's WebSocket, which hands this each document socket it makes once its own
      // listeners are on it (`__documentSocketMade`), so the page carries one wrap.
      if (words !== undefined) {
        const theirs = new RegExp(words, "g");
        const read = new TextDecoder();
        let recording = false;
        window.__documentSocketMade = (socket) => {
          if (recording) return;
          recording = true;
          socket.addEventListener("message", (event) => {
            const servedAt = Date.now();
            const data: unknown = event.data;
            if (!(data instanceof ArrayBuffer) || data.byteLength > under) return;
            const servedIn = thisTurn();
            for (const [word] of read.decode(data).matchAll(theirs)) {
              servedWords[word] ??= { atMs: servedAt, turn: servedIn };
            }
          });
        };
      }
      let handedLast = Number.NEGATIVE_INFINITY;
      /** Whether a timer the probe handed over has still to run. One at a time, because what
       *  this row is about is how long the page waits for a timer, not how deep a queue of
       *  them grows: a frame of this page costs hundreds of milliseconds, so a probe handing
       *  one over every frame outruns the browser and measures its own backlog. A reader's
       *  page posts one and waits for it - React's scheduler, and the one-card-per-task timer
       *  of `useAskCardsDrawnInTasks` - which is what this does too. */
      let owed = false;
      /** The next count of its own blocks the page has still to say it has reached, and
       *  whether it has said the stop. The page says one thing per step rather than one per
       *  frame: what the row writes by is how far this build has got, and a word per step is
       *  what makes the number of them the build's own rather than the machine's. */
      let nextStep = perStep;
      let saidTheStop = false;
      /** `performance.now()` in the frame the editor's live root was first found in, which is
       *  the frame its first batch landed in; undefined until then. */
      let firstBatchMs: number | undefined;
      const look = () => {
        const now = performance.now();
        const root = document.querySelector(live);
        if (firstBatchMs === undefined && root !== null) {
          firstBatchMs = now;
        }
        // How far the build has got, read as the top-level child count of the editable host
        // rather than a query over the whole document, so the probe costs the page nothing:
        // the count is a property of a node the browser already holds, and reading it forces
        // no layout. Read once a frame, because the stop below is read from it as well as the
        // timer the probe hands over.
        const built = root?.querySelector('[contenteditable="true"]')?.childElementCount ?? 0;
        if (!owed && now - handedLast >= every) {
          const handedAt = now;
          handedLast = now;
          owed = true;
          setTimeout(() => {
            owed = false;
            timers.push({ builtThen: built, handedAt, waitedMs: performance.now() - handedAt });
          }, 0);
        }
        // How far this build has got, said in the task this read the count in so the run is
        // told rather than asking: once per step of its own blocks, and once more when it
        // reaches the blocks the row stops the other person's writing at.
        if (perStep > 0 && built >= nextStep) {
          while (built >= nextStep) nextStep += perStep;
          const itsStop = stop > 0 && built >= stop;
          saidTheStop ||= itsStop;
          window.__theBuildReached?.(built, itsStop);
        } else if (!saidTheStop && stop > 0 && built >= stop) {
          saidTheStop = true;
          window.__theBuildReached?.(built, true);
        }
        if (document.querySelector(named) !== null) {
          const openedMs = now;
          const batchedMs = firstBatchMs;
          // And the swap that ends the build runs in the tasks after this frame: the binding
          // adopting the document, the editor being revealed, and the margin placing its cards
          // over it for the first time. The page says when that is over rather than leaving it
          // to be inferred from a run of `longtask` entries: a task handed over now runs after
          // the work already queued, so the task it is served in is the page's own word that
          // it is free again - and that task's own turn is the last one the build can still be
          // what served something in. The page stamps the turn there, which is the number the
          // row reads, and the clock beside it, which the row only prints.
          //
          // The end of that turn is stamped too, from a task handed over inside it, and it is
          // printed and nothing more: it is served once the page has got through everything it
          // was already holding, which on this pod is 1.5 to 21 s of the backlog a build
          // leaves - a measurement of what the page carries out of a build, and no part of any
          // reading. The reading is `swapTurn` against the turn each word was served in.
          if (batchedMs === undefined) {
            // A page that named its editor before any frame found the first batch has no
            // build window to say, and says so now, so the row fails on that by name rather
            // than on a wait that never ends or on a window made of a default.
            window.__theBuildSwapped?.(
              "her editor was named before any frame found its first batch, so her page has " +
                "no build window to say"
            );
          } else {
            requestAnimationFrame(() => {
              setTimeout(() => {
                const swapTurn = thisTurn();
                // Said in the task it was stamped in, with the window it ends, so the run is
                // told the whole of the build at once: the stamp comes tasks after the frame
                // that named the editor, and a reading taken between the two would hold no
                // swap at all.
                window.__theBuildSwapped?.({
                  firstBatchMs: batchedMs,
                  openedMs,
                  swappedMs: performance.now(),
                  swapTurn,
                });
                setTimeout(() => {
                  window.__freeTurnEndedMs = performance.now();
                }, 0);
              }, 0);
            });
          }
          // And where the editor was named without the build ever reaching that count, the
          // page says so with the count it ended on: the run's wait ends in the row's own
          // failure, which names what the build did, rather than in a timeout.
          if (!saidTheStop && stop > 0) {
            saidTheStop = true;
            window.__theBuildReached?.(built, true);
          }
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
      perStep: stepBlocks,
      stop: stopAfterBlocks,
      under: theirWordsInFramesUnderBytes,
      words: theirWords,
    }
  );
  return {
    built: () => built,
    nextStep: (timeoutMs) =>
      theMomentThePageReached(
        step.promise,
        timeoutMs,
        "her build said nothing more about how far it had got: no further step of it came"
      ),
    reachedTheStop: (timeoutMs) =>
      theMomentThePageReached(
        reached.promise,
        timeoutMs,
        "her build never reached the blocks the row stops him at, and her editor was never " +
          "named holding fewer"
      ),
    stopped: () => stopped,
    swapped: async (timeoutMs) => {
      const said = await theMomentThePageReached(
        swapped.promise,
        timeoutMs,
        "her page never said the swap that ends its build was over: the task it handed over " +
          "in the frame after her editor was named never ran, so nothing it recorded of that " +
          "build is whole enough to read"
      );
      if (typeof said === "string") throw new Error(said);
      return said;
    },
  };
}

/** The tasks of `served` the browser was given while the build ran, which are the ones the
 *  build could have starved. A task handed over inside the window and run after it is one of
 *  them, which is why they are read by when each was handed over. */
export function handedToTheBuild(served: readonly ServedLate[], ran: TheBuildRan): ServedLate[] {
  return served.filter(
    (task) => task.handedAt >= ran.firstBatchMs && task.handedAt <= ran.openedMs
  );
}

/** How far the build had got at the first and at the last of the tasks the page was handed
 *  inside a window: the fewest and the most top-level blocks the editor's own host held at any
 *  of them. `[0, 0]` where it was handed none. */
export function builtAcross(served: readonly ServedLate[]): [number, number] {
  const built = served.map((task) => task.builtThen ?? 0);
  return built.length === 0 ? [0, 0] : [Math.min(...built), Math.max(...built)];
}

/**
 * Fails unless the timers the page was handed inside a window show the document being built
 * across it: two of them at least, and the editor holding more top-level blocks at the last of
 * them than at the first, which is the build's own batches arriving while they were served.
 *
 * The two rows that read a window through `whatTheBuildServes` use it as their proof that the
 * window held a build: the timer row of `large-documents-build-serves.e2e.ts` and the desktop
 * guard of `large-documents-desktop-build.e2e.ts`. (The decision row of
 * `large-documents-build-serves.the-decision.e2e.ts` reads its window through
 * `watchTheBuildsWindow` instead, and holds its press to a build by the blocks the editor still
 * owed at it.) It is a reading of the document rather than of the runner: the same batches
 * carry the same blocks into the editor whatever the machine's speed, so the
 * rise is there on a loaded runner and on an idle one. A window the document was not built in -
 * one that ended before the first batch, or opened after the editor did - holds either no timer
 * at all or timers that all read the same count, and fails here.
 */
export function theWindowHeldABuild(timers: readonly ServedLate[]): void {
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
