/** `bun e2e/visual-seed.ts <path>`: empties this stack's database, writes the review corpus into
 *  it, and saves the ids that corpus made as JSON at `<path>`.
 *
 *  It runs on the HOST, under `bun`, inside the stack `e2e/harness-up.ts` started: `e2e/seed.ts`
 *  and `e2e/delivery-seed.ts` reach the `psql` binary (`e2e/psql.ts`), and the pinned Playwright
 *  image carries none. The container that takes the shots reads this file instead of seeding -
 *  `DISPATCH_E2E_VISUAL_SEED` names it there - so the visual specs import nothing that reaches
 *  `./psql`. `e2e/visual-run.sh` is what runs the two halves in order.
 *
 *  One seed serves the whole run: the rows of `e2e/visual.e2e.ts` reset nothing and seed
 *  nothing, so the corpus every baseline was taken against is the corpus this entry wrote
 *  once. */
import { writeFileSync } from "node:fs";

import type { VisualSeed } from "./review-routes";
import { seedReviewRoutes } from "./review-seed";
import { resetDatabase } from "./seed";

const path = process.argv[2];
if (path === undefined) {
  console.error("usage: bun e2e/visual-seed.ts <path to write the seed JSON to>");
  process.exit(2);
}

// The corpus and the moment the rows pin their page clock to (`seededAt` and a minute after it,
// e2e/visual.e2e.ts) must fall on one day: the margin and the activity feed head their entries
// "Today" or "Yesterday" by the reader's day, which is UTC in the container that takes the shots,
// so a seed written in the last minutes before midnight UTC drew "Yesterday" over a baseline
// taken as "Today" (run 44840127934, seeded 23:59:46Z: issue-activity and priority, light and
// dark). So a seed that would start within `dayEndMarginMs` of midnight waits for the new day.
// 90 s is the seed's 15 s bound below, the minute the pin adds and 15 s for the reset before it;
// the wait is the wall clock's, as the day the page reads is, and at most 91 s, which the visual
// job's two browser steps' budgets hold (.github/ci-notes/dispatch-browser.md).
const dayEndMarginMs = 90_000;
const now = Date.now();
const nextUtcMidnight = Math.ceil((now + 1) / 86_400_000) * 86_400_000;
if (nextUtcMidnight - now < dayEndMarginMs) {
  const wait = nextUtcMidnight - now + 1_000;
  console.error(`e2e/visual-seed.ts waits ${wait} ms for the next UTC day before it seeds`);
  // The wall clock's own day is what the page reads, so this waits on the wall clock for it.
  await Bun.sleep(wait);
}

await resetDatabase();
// No Playwright row runs this seed, so no expect timeout applies; 15 s is far above the 230 ms
// the Inbox was read settling in (the sampler e2e/workspace.ts cites), so only a seed that does
// not settle fails it.
const seedBoundMs = 15_000;
const seed = await seedReviewRoutes({ boundMs: seedBoundMs });

// A write that fails is the whole run's failure and not the container's: without this file every
// row of the visual spec skips itself, and the run would pass having compared nothing. So the
// message names the path and the status is non-zero.
try {
  // `seededAt` is what lets every row pin its page clock to a fixed moment just after this
  // corpus was written, so a relative label reads the same whatever the run's own pace
  // (e2e/visual.e2e.ts says why that matters, and why a mask does not do it). Typed as
  // `VisualSeed`, which requires it, so the rows that read it never have to ask.
  const written: VisualSeed = { ...seed, seededAt: new Date().toISOString() };
  writeFileSync(path, `${JSON.stringify(written, null, 2)}\n`);
} catch (error) {
  console.error(`e2e/visual-seed.ts could not write the seed to ${path}: ${String(error)}`);
  process.exit(1);
}
console.error(`e2e/visual-seed.ts wrote the review corpus's ids to ${path}`);
