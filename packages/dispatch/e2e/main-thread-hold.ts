// How long the page's own work holds the main thread while a card draws. Two specs assert a hold
// through this module - `heavy-bodies.e2e.ts`, whose rows draw the bodies of `heavy-bodies.ts` in
// the Conversation, the Inbox's ask card and a hover card, and `nested-directive.e2e.ts`, whose
// rows draw the two bodies that froze a reader's tab - so both read a hold the same way and
// against the same bound (sjawhar/legion#2612).
import { relative } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, type Page, test } from "@playwright/test";

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

export interface Measured {
  /** Milliseconds from the record's start until this row saw the formatted body; the row's own
   *  polling rounds it up. */
  readonly drawn: number;
  /** The largest gap, in milliseconds, between two ticks of the main-thread heartbeat since the
   *  record started, which on a machine the page has to itself is the page's own work. This is
   *  what a row asserts. */
  readonly held: number;
}

// The row's file as the list reporter prints it, relative to the package (`packageRoot` in
// e2e/measure-reporter.ts holds that reading, in the comment above it) rather than to the run's
// cwd or to Playwright's rootDir, which is this directory: the command the refusal below prints
// is run from the package.
const packageRoot = fileURLToPath(new URL("..", import.meta.url));

/**
 * Fails the row when the run has more than one worker, because what the row would report is not
 * what it claims to measure: the gap counts whatever the other lanes ran. A failure and not a
 * skip - a wall-clock row that passes out of sight reads as coverage this suite then does not
 * have.
 *
 * This reaches only the rows that call `measureDraw`. The rows of
 * `agent-workspace-timing.e2e.ts` read a wall clock of their own and never call it, so what
 * holds them to one worker is the command that runs them, which names `--workers=1`
 * (`e2e/playwright.config.ts`, above `aloneProjects`). The other rows of the `-alone` projects
 * read no clock, never call it, and run at any worker count with every assertion they have.
 *
 * The count is the one the run asked for, which is what the reader controls, and not the number
 * of workers Playwright went on to start: a run holding fewer jobs than workers starts fewer and
 * reports that smaller number (`metadata.actualWorkers = Math.min(config.workers, ...)`,
 * `playwright/lib/runner/index.js:6735`), while `config.workers` keeps what was asked. So a run
 * of one row that said `--workers=4` fails here and prints `using 1 worker` above the failure.
 * The per-project cap of the `-alone` projects is no answer either: it holds a project's own
 * rows to one at a time and lets every other project the command selected run beside them.
 *
 * One worker is what this can ask for, and it is less than quiet: a one-worker run started
 * beside another run, a build or a browser on the same machine reads that work in the gap just
 * the same. The `dependencies` that ordered these projects before dispatch://INFRA-1280 left
 * exactly the same hole, since they ordered this suite's own projects and nothing else on the
 * machine.
 */
function refuseASharedRun(): void {
  const info = test.info();
  const workers = info.config.workers;
  if (workers <= 1) {
    return;
  }
  // The runner process's own `process.argv`, snapshotted by the `playwright test` command
  // (`cliOverrides.argv = process.argv`, `lib/cli/testActions.js:43`) and carried into every
  // worker inside the serialized config (`argv: configCLIOverrides.argv ?? []`,
  // `lib/common/index.js:506`), so it reads here as `config.argv`, "Snapshot of `process.argv`
  // captured in the runner process." (playwright/types/test.d.ts:2105). Playwright parses none of
  // it and sets it on no other path, so a run started from the test server, the UI or an editor
  // leaves it empty; then the printed commands are all the reader gets.
  const commandLine = info.config.argv.join(" ");
  // The command comes first and alone, as the wall-clock refusal in `e2e/playwright.config.ts`
  // prints it, so the line a reader is shown beside the failed row is the one that fixes it. It
  // names its own `--workers=1`, because dispatch://INFRA-1280/spec#b-r2 has every command
  // that measures a wall-clock row state the one worker its reading rests on, rather than
  // take it from the configuration's default (`workers: 1`, e2e/playwright.config.ts).
  const file = relative(packageRoot, info.file);
  throw new Error(
    `bun run e2e -- --project=${info.project.name} ${file} --workers=1` +
      `\n\nThat is how this row is measured, and this run has ${workers} workers. The hold it ` +
      "reads is the longest gap between two ticks of a heartbeat on the page's main thread, " +
      "and that gap is the page's own work only on a machine the run is not sharing: with " +
      `${workers} workers, what the other workers run goes into it as well, which is how a ` +
      "300 ms draw read 1,632 ms in CI run 41370867411 (sjawhar/legion#2612). To measure every " +
      "wall-clock row in all three engines, run `bun run e2e:clock`." +
      (commandLine === "" ? "" : `\n\nThis run's command line: ${commandLine}`)
  );
}

/**
 * Installs a heartbeat before the page's own scripts - a `setTimeout(0)` loop whose gap between
 * ticks a parse holding the main thread widens to the parse's length - and starts the record
 * again when the page first asks for the data the body comes in, before that request goes out:
 * the bundle has been evaluated and the tab drawn by then, and the body cannot draw before the
 * answer.
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
 * run in the `-alone` projects (`e2e/playwright.config.ts`), which name no dependency and are
 * out of the default selection, so a command names them, and each caps its own workers at one,
 * which keeps its own rows from running four at a time. What leaves nothing of the suite beside
 * them is the run being one worker wide, and `refuseASharedRun` above is what holds a reading to
 * that: a measuring row in a run of more than one worker fails rather than reporting the other
 * lanes' work as this page's. Two instruments stood here before and told that
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
  // Before the page is touched at all, so no work is done for a reading that would not be kept.
  refuseASharedRun();

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
  // Only the first request resets: a later one could wait out the draw, then erase its gap.
  let started = false;
  await page.route(reset, async (route) => {
    if (!started && route.request().method() === "GET") {
      started = true;
      await page.evaluate(() => window.__mainThreadResetGap?.());
    }
    await route.continue();
  });

  await open();
  await drawn();
  if (!started) {
    throw new Error(`the page never asked for ${reset.source}`);
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
