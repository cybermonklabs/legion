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

import type { ReviewSeed, VisualSeed } from "./review-routes";
import { seedReviewRoutes } from "./review-seed";
import { resetDatabase } from "./seed";
import { midnightToSeedAfter } from "./visual-day";

const path = process.argv[2];
if (path === undefined) {
  console.error("usage: bun e2e/visual-seed.ts <path to write the seed JSON to>");
  process.exit(2);
}

// No Playwright row runs this seed, so no expect timeout applies; 15 s is far above the 230 ms
// the Inbox was read settling in (the sampler e2e/workspace.ts cites), so only a seed that does
// not settle fails it.
const seedBoundMs = 15_000;

/** The corpus, written on one UTC day with room after it for every row's clock
 *  (e2e/visual-day.ts says why). A seed begun in the last two minutes of a day could not be kept
 *  however quickly it ran, so it waits for that midnight first. A seed that runs into those two
 *  minutes is written again just after its midnight; that second seed has the whole day ahead of
 *  it, so a second refusal is a clock this file does not understand, and it throws. */
async function seedOnOneDay(): Promise<{ readonly seed: ReviewSeed; readonly seededAt: number }> {
  const now = Date.now();
  const firstMidnight = midnightToSeedAfter(now, now);
  if (firstMidnight !== null) {
    console.error(
      `e2e/visual-seed.ts: it is ${new Date(now).toISOString()}, and a row's clock could reach ` +
        `past ${new Date(firstMidnight).toISOString()} however quickly the seed ran; ` +
        "seeding after that midnight."
    );
    await Bun.sleep(new Date(firstMidnight));
  }
  // Two seeds at most: the second throws on a refusal rather than sleeping again.
  for (let attempt = 1; ; attempt += 1) {
    const startedAt = Date.now();
    await resetDatabase();
    const seed = await seedReviewRoutes({ boundMs: seedBoundMs });
    const seededAt = Date.now();
    const midnight = midnightToSeedAfter(startedAt, seededAt);
    if (midnight === null) return { seed, seededAt };
    const refusal =
      `e2e/visual-seed.ts: the seed ran from ${new Date(startedAt).toISOString()} to ` +
      `${new Date(seededAt).toISOString()}, and a row's clock could reach past ` +
      `${new Date(midnight).toISOString()}, where its day headings would read "Yesterday"`;
    if (attempt === 2) {
      throw new Error(
        `${refusal}. This was the second seed, begun after a midnight with the whole day ahead ` +
          "of it, so this host's clock is not moving as Date.now() says."
      );
    }
    console.error(`${refusal}; seeding again after that midnight.`);
    // `Bun.sleep` takes the moment itself and resolves at once for one already past, so a seed
    // that straddled its midnight is written again at once.
    await Bun.sleep(new Date(midnight));
  }
}

const { seed, seededAt } = await seedOnOneDay();

// A write that fails is the whole run's failure and not the container's: without this file every
// row of the visual spec skips itself, and the run would pass having compared nothing. So the
// message names the path and the status is non-zero.
try {
  // `seededAt` is what lets every row pin its page clock to a fixed moment just after this
  // corpus was written, so a relative label reads the same whatever the run's own pace
  // (e2e/visual.e2e.ts says why that matters, and why a mask does not do it). Typed as
  // `VisualSeed`, which requires it, so the rows that read it never have to ask.
  const written: VisualSeed = { ...seed, seededAt: new Date(seededAt).toISOString() };
  writeFileSync(path, `${JSON.stringify(written, null, 2)}\n`);
} catch (error) {
  console.error(`e2e/visual-seed.ts could not write the seed to ${path}: ${String(error)}`);
  process.exit(1);
}
console.error(`e2e/visual-seed.ts wrote the review corpus's ids to ${path}`);
