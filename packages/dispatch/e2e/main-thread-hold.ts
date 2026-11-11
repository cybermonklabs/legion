// How long the page's own work holds the main thread while a card draws. Two specs assert a hold
// through this module - `heavy-bodies.e2e.ts`, whose rows draw the bodies of `heavy-bodies.ts` in
// the Conversation, the Inbox's ask card and a hover card, and `nested-directive.e2e.ts`, whose
// rows draw the two bodies that froze a reader's tab - so both read a hold the same way and
// against the same bound (sjawhar/legion#2612).
import { expect, type Page, type Route, test } from "@playwright/test";

declare global {
  interface Window {
    /** The largest gap, in milliseconds, between two ticks of the page's main-thread heartbeat. */
    __mainThreadLongestGap?: number;
    /** `performance.now()` when the record last started, so a row can time the draw from it. */
    __mainThreadResetAt?: number;
    /** Starts the record again: no gap so far and the last tick now. */
    __mainThreadResetGap?: () => void;
  }
}

/** The longest the page's own work may hold the main thread while the card draws. */
const longestHoldMs = 1_000;

/** The one read a cold page makes for its viewer, whose single answer carries the shell every
 *  page draws and the page the address names (`web/src/api/queries.ts`, `readLanding`). A body
 *  that answer carries reaches the page in it and in no request of that body's own, so this is
 *  the request the record starts at for such a body (`measureDraw`). */
const landingRead = /\/api\/v1\/landing(\?.*)?$/;

export interface Measured {
  /** Milliseconds from the record's start until this row saw the formatted body; the row's own
   *  polling rounds it up. */
  readonly drawn: number;
  /** The largest gap, in milliseconds, between two ticks of the main-thread heartbeat since the
   *  record started, which on a machine the page has to itself is the page's own work. This is
   *  what a row asserts. */
  readonly held: number;
}

/**
 * Installs a heartbeat before the page's own scripts - a `setTimeout(0)` loop whose gap between
 * ticks a parse holding the main thread widens to the parse's length - and starts the record
 * again when the page first asks for the data the body comes in, before that request goes out:
 * the bundle has been evaluated by then, since that ask is the bundle's own code asking, and
 * the body cannot draw before the answer.
 *
 * For a body the landing carries, that ask is the landing, which goes out before the shell has
 * drawn, so the window holds the shell's first render as well as the draw. That widens what a
 * gap can be read from and narrows nothing: a longest gap over a longer window is at least the
 * longest gap over the draw alone, so the bound below is asserted of strictly more of the
 * page's work than it was when every body came in a read of its own.
 *
 * A gap counts three things, and only the first is what these rows are about: the page's own
 * work on its own thread; wall time in which the operating system did not schedule this renderer
 * at all; and wall time in which it did schedule it, slower than this machine can run it,
 * because something else wanted the same cores. One browser at a time left the last two near
 * nothing. A Playwright worker per core did not: in CI run 41370867411 the row for `49 list
 * markers on one line ...`, whose whole draw is about 300 ms of work, reported a gap of 1,632 ms
 * (sjawhar/legion#2612).
 *
 * So the rows that read a gap are given the machine instead of a correction for sharing it. They
 * run in the `-alone` projects (`e2e/playwright.config.ts`), which Playwright starts only once
 * every other project has finished and which run one row at a time, so nothing of the suite is
 * beside them and a gap is the page's own work. Two instruments stood here before and told that
 * work from the machine's - a witness thread of the renderer's own process, which counted a gap
 * only in the time it was itself running, and a yardstick job timed on the main thread, which
 * said what that thread was running at - and both cost the bound its sensitivity: a 3.3 ms job
 * escapes the preemption a hold of hundreds of milliseconds cannot, so the pace they read was
 * the kinder one and the hold they left was too low. They are gone with the contention they
 * corrected for.
 *
 * Measured with the machine to itself on an issue pod of two cores, which is about a third of a
 * CI runner's speed on these rows: over the twelve measuring rows of `heavy-bodies.e2e.ts` the
 * longest hold was 585 ms in Chromium and 1,001 ms in Firefox, and the two rows of
 * `nested-directive.e2e.ts` read 61 and 79 ms. The Firefox figure is the 49-marker body, the
 * longest the page parses on its own thread (`markdown-worker-threshold.ts`), and it is the one
 * row this pod leaves no room under the bound: the same row read a 435 ms gap at one worker on a
 * CI runner, and 787 to 1,370 ms over six runs here. With `MarkdownBody`'s sliced painter taken
 * out, so that the 16,000 elements of an 8,000-item list are built and laid out in one task,
 * the three rows that draw one read 908 to 1,010 ms here against the 312 to 382 ms they read
 * with it, and one of those six runs crossed the bound.
 */
export async function measureDraw(
  page: Page,
  {
    drawn,
    open,
    reset,
  }: {
    /** Resolves once the formatted body is in the page. */
    readonly drawn: () => Promise<void>;
    /** What the reader does: open the page, and whatever else reaches the body. */
    readonly open: () => Promise<void>;
    /** The first GET of this request starts the record. */
    readonly reset: RegExp;
  }
): Promise<Measured> {
  await page.addInitScript(() => {
    let last = performance.now();
    window.__mainThreadLongestGap = 0;
    window.__mainThreadResetAt = last;
    window.__mainThreadResetGap = () => {
      last = performance.now();
      window.__mainThreadLongestGap = 0;
      window.__mainThreadResetAt = last;
    };
    const tick = () => {
      const now = performance.now();
      window.__mainThreadLongestGap = Math.max(window.__mainThreadLongestGap ?? 0, now - last);
      last = now;
      setTimeout(tick, 0);
    };
    setTimeout(tick, 0);
  });
  // The record starts at the page's first ask for this body, and at its first GET alone: a
  // later request of the same shape could wait out the draw and erase the gap it was there to
  // hold.
  //
  // Two requests can be that ask. The landing is every page's first read, and a body it carries
  // - a comment of the issue the address names, which rides the issue page it answers, or an
  // ask of the Inbox, which rides the shell - reaches the page in that one answer: the key is
  // seeded from it, so the component that draws the body sends no request of its own and a
  // record waiting for that body's route would wait for a request that never comes
  // (`web/src/api/queries.ts`, `seedIssuePage` and `seedShell`). `reset` is the read for a body
  // the page still fetches itself - the issue's messages, or the one comment behind a hover
  // card - and the landing precedes it. Whichever goes out first is the first ask, so whichever
  // goes out first starts the record, and the row's bound is read from there.
  let started = false;
  const startRecord = async (route: Route) => {
    if (!started && route.request().method() === "GET") {
      started = true;
      await page.evaluate(() => window.__mainThreadResetGap?.());
    }
    await route.continue();
  };
  await page.route(landingRead, startRecord);
  await page.route(reset, startRecord);

  await open();
  await drawn();
  if (!started) {
    throw new Error(`the page asked for neither ${landingRead.source} nor ${reset.source}`);
  }
  const measured = await page.evaluate(() => ({
    drawn: performance.now() - (window.__mainThreadResetAt ?? Number.NaN),
    gap: window.__mainThreadLongestGap,
  }));
  if (measured.gap === undefined) {
    throw new Error("the main-thread heartbeat never started");
  }
  return { drawn: measured.drawn, held: measured.gap };
}

/** Every measurement is the run's evidence, so each is kept on the result even when it passes:
 *  the hold the row asserts, and the time to the formatted body, which is recorded and never a
 *  limit. */
export function record({ drawn, held }: Measured): void {
  test.info().annotations.push(
    { description: `${Math.round(held)} ms`, type: "longest main-thread hold" },
    {
      description: `${Math.round(drawn)} ms, as this row polled for it`,
      type: "time to the formatted body",
    }
  );
}

export function expectUnderTheBound({ held }: Measured): void {
  expect(
    held,
    `the page's own work held the main thread for ${Math.round(held)} ms while the card drew`
  ).toBeLessThan(longestHoldMs);
}
