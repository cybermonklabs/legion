// Captures a set of declared screenshots against a freshly seeded harness. A set is a section's
// `shots.config.ts` (Dispatch's is `docs/site/media/shots.config.ts`; another section's lives in
// `docs/site/media/<section>/shots.config.ts`), and `shots.ts` runs them all.
import { mkdirSync } from "node:fs";
import { dirname, join, relative } from "node:path";

import type { Browser, Locator, Page } from "@playwright/test";

import {
  assertScreenClean,
  type Harness,
  newViewerContext,
  pageErrors,
  REPO,
  type Theme,
  type Viewport,
  WORK,
  waitForSettled,
  withinStepTimeout,
} from "./harness";

/** Every set writes `<SHOTS_ROOT>/<set>/<id>.png`, which the site serves at
 *  `/legion/media/<set>/<id>.png`. Gitignored: CI takes them before each build. */
export const SHOTS_ROOT = join(REPO, "docs/site/public/media");

export interface Shot<Seeded> {
  readonly id: string;
  /** The alt text a page embedding this shot gives it. */
  readonly alt: string;
  readonly route: string | ((seeded: Seeded) => string);
  readonly viewport: Viewport;
  /** Overrides the desktop viewport's 1440x900 (CSS pixels), for a shot of what a window of
   *  another width shows. A phone is always the iPhone 13 profile. */
  readonly size?: { readonly width: number; readonly height: number };
  /** Server-side writes that bring the seeded data to the state this shot shows, before the page
   *  opens. Shots run in the order declared, so a set can follow one record through a journey. */
  readonly prepare?: (seeded: Seeded) => Promise<void>;
  readonly theme: Theme;
  /** Waits for the page's own readiness: the content the shot is of, on screen. */
  readonly ready: (page: Page, seeded: Seeded) => Promise<void>;
  /** Brings the ready page to the state the shot shows, and waits for that state. */
  readonly steps?: (page: Page, seeded: Seeded) => Promise<void>;
  /** Captures this one element rather than the viewport. */
  readonly element?: (page: Page, seeded: Seeded) => Locator;
  /** Empty states, by `aria-label`, this shot may show; any other one fails it. */
  readonly allowEmpty?: readonly string[];
}

export interface ShotSet<Seeded> {
  /** The directory under `SHOTS_ROOT`, e.g. `dispatch`. */
  readonly set: string;
  /** Seeds the freshly reset database; what it returns reaches every shot. */
  readonly seed: () => Promise<Seeded>;
  readonly shots: readonly Shot<Seeded>[];
  /** Empty states, by `aria-label`, every shot in the set may show. */
  readonly allowEmpty?: readonly string[];
}

/** Every `<time>` the page shows when a shot is taken, written beside nothing the site serves:
 *  `.work/time-elements/<set>-<id>.json`, which `docs-site.yaml` uploads with the shots.
 *
 *  The pages' images are made rather than committed, so the only way to ask whether two builds
 *  render them the same is to compare two runs' images; and the dashboard draws its times as
 *  relative labels from the clock over rows seeded at run time, so two runs of one commit differ
 *  wherever a label crossed a rounding boundary between them. That is what this file tells apart
 *  from a difference in shape at identical text, which is what a changed font looks like: a
 *  region that moved is the clock only where it lies inside one of these boxes AND that
 *  element's text differs between the two runs. Reading the pixels cannot show the second half.
 *
 *  Each box is given twice: `viewport`, as Playwright reports it in CSS pixels, and `image`, the
 *  same box in the pixels of the PNG this shot wrote — scaled by the device pixel ratio and, for
 *  a shot of one element, with that element's own origin taken off, so a reader compares it with
 *  a difference box without doing arithmetic. */
async function recordTimeElements(
  page: Page,
  element: Locator | undefined,
  set: string,
  id: string
): Promise<void> {
  const ratio = await page.evaluate(() => window.devicePixelRatio);
  const origin = (await element?.boundingBox()) ?? { x: 0, y: 0 };
  const times = page.locator("time");
  const elements = [];
  for (let i = 0; i < (await times.count()); i++) {
    const one = times.nth(i);
    const box = await one.boundingBox();
    elements.push({
      dateTime: await one.getAttribute("datetime"),
      title: await one.getAttribute("title"),
      text: (await one.innerText()).trim(),
      viewport: box,
      image:
        box === null
          ? null
          : {
              x: Math.round((box.x - origin.x) * ratio),
              y: Math.round((box.y - origin.y) * ratio),
              width: Math.round(box.width * ratio),
              height: Math.round(box.height * ratio),
            },
    });
  }
  const out = join(WORK, "time-elements", `${set}-${id}.json`);
  mkdirSync(dirname(out), { recursive: true });
  await Bun.write(
    out,
    `${JSON.stringify({ set, id, devicePixelRatio: ratio, origin, elements }, null, 2)}\n`
  );
}

/** Resets and seeds the harness, takes every shot in `set` (or those `only` names) in `browser`,
 *  and returns one line per shot that failed. A failed shot leaves what the page showed in
 *  `.work/`. Every shot's `prepare` runs, in order, whether or not the shot is taken. The reset
 *  and seed, and each shot, get STEP_TIMEOUT_MS; one that runs past it ends the run with a
 *  HarnessHang naming it and what it was doing. */
export async function runShotSet<Seeded>(
  harness: Harness,
  browser: Browser,
  set: ShotSet<Seeded>,
  only?: ReadonlySet<string>
): Promise<string[]> {
  if (!set.shots.some((shot) => only === undefined || only.has(shot.id))) return [];
  const seeded = await withinStepTimeout(
    () => `${set.set}: the reset and seed`,
    harness.reset().then(() => set.seed())
  );
  const failures: string[] = [];
  for (const shot of set.shots) {
    let doing = "its prepare";
    const capture = async () => {
      await shot.prepare?.(seeded);
      if (only !== undefined && !only.has(shot.id)) return;
      const out = join(SHOTS_ROOT, set.set, `${shot.id}.png`);
      mkdirSync(dirname(out), { recursive: true });
      doing = "opening its page";
      const context = await newViewerContext(browser, harness.baseURL, shot);
      const page = await context.newPage();
      const errors = pageErrors(page);
      try {
        await page.goto(typeof shot.route === "string" ? shot.route : shot.route(seeded));
        doing = "its ready";
        await shot.ready(page, seeded);
        doing = "its steps";
        await shot.steps?.(page, seeded);
        doing = "waiting for the page to settle";
        await waitForSettled(page);
        doing = "the screen check and the capture";
        await assertScreenClean(page, errors, [
          ...(set.allowEmpty ?? []),
          ...(shot.allowEmpty ?? []),
        ]);
        const options = { animations: "disabled", caret: "hide", path: out } as const;
        const subject = shot.element?.(page, seeded) ?? page;
        await subject.screenshot(options);
        await recordTimeElements(page, shot.element?.(page, seeded), set.set, shot.id);
        console.log(`${set.set}/${shot.id}: ${relative(REPO, out)}`);
      } catch (error) {
        doing = "keeping what its page showed";
        const failed = join(WORK, "failed-shots", `${set.set}-${shot.id}.png`);
        await page.screenshot({ path: failed }).catch(() => undefined);
        failures.push(
          `${set.set}/${shot.id}: ${error instanceof Error ? error.message : String(error)} ` +
            `(the page as it was: ${relative(REPO, failed)})`
        );
      } finally {
        doing = "closing its page";
        await context.close();
      }
    };
    await withinStepTimeout(() => `${set.set}/${shot.id}, in ${doing}`, capture());
  }
  return failures;
}
