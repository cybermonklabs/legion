/** `bun e2e/visual-seed.ts <path>`: empties this lane's database, writes the review corpus into
 *  it, and saves the ids that corpus made as JSON at `<path>`.
 *
 *  It runs on the HOST, under `bun`, inside the harness `e2e/run.ts` started: `e2e/seed.ts` and
 *  `e2e/delivery-seed.ts` reach the `psql` binary (`e2e/psql.ts`), and the pinned Playwright
 *  image carries none. The container that takes the shots reads this file instead of seeding -
 *  `DISPATCH_E2E_VISUAL_SEED` names it there - so the visual specs import nothing that reaches
 *  `./psql`. `e2e/visual-run.sh` is what runs the two halves in order.
 *
 *  One seed serves the whole run: the rows of `e2e/visual.e2e.ts` and `e2e/visual-phone.e2e.ts`
 *  reset nothing and seed nothing, so the corpus every baseline was taken against is the corpus
 *  this entry wrote once. */
import { writeFileSync } from "node:fs";

import { seedReviewRoutes } from "./review-seed";
import { resetDatabase } from "./seed";

const path = process.argv[2];
if (path === undefined) {
  console.error("usage: bun e2e/visual-seed.ts <path to write the seed JSON to>");
  process.exit(2);
}

await resetDatabase();
const seed = await seedReviewRoutes();

// A write that fails is the whole run's failure and not the container's: without this file every
// row of the two specs skips itself, and the run would pass having compared nothing. So the
// message names the path and the status is non-zero.
try {
  writeFileSync(path, `${JSON.stringify(seed, null, 2)}\n`);
} catch (error) {
  console.error(`e2e/visual-seed.ts could not write the seed to ${path}: ${String(error)}`);
  process.exit(1);
}
console.error(`e2e/visual-seed.ts wrote the review corpus's ids to ${path}`);
