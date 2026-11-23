/** `bun e2e/visual-seed.ts <path>`: empties this stack's database, writes the review corpus into
 *  it, and saves the ids that corpus made as JSON at `<path>`, with the instant the rows read it
 *  at.
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

import { Interrupted } from "./local-instance-support";
import { sql } from "./psql";
import type { ReviewSeed, VisualSeed } from "./review-routes";
import { seedReviewRoutes } from "./review-seed";
import { resetDatabase } from "./seed";

const path = process.argv[2];
if (path === undefined) {
  console.error("usage: bun e2e/visual-seed.ts <path to write the seed JSON to>");
  process.exit(2);
}

/** The seed's waits have no bound of their own (`awaitFact`): what ends one that never settles
 *  is the end of the run around it - the visual step's own `timeout-minutes` in CI, a person's
 *  Ctrl-C at a desk. A bound of their own would be a clock: a seed that a loaded machine slows
 *  past it fails the whole run with nothing wrong in the corpus. The run's end arrives as a
 *  signal, which aborts the waits, so the one under way names its fact (below), and the process
 *  exits with the status the signal's own death would have given. The exit waits one turn of the
 *  event loop, which the abort's rejection reaches the catch below within, and no longer, so a
 *  signal that lands between two waits ends the seed as promptly. */
const stopping = new AbortController();
let stoppedWith: number | undefined;
for (const [signal, status] of [
  ["SIGINT", 130],
  ["SIGTERM", 143],
] as const) {
  process.once(signal, () => {
    stoppedWith = status;
    stopping.abort();
    setTimeout(() => process.exit(status), 0);
  });
}

/** The database's clock, which stamps every row the corpus writes, as ISO 8601 in UTC. */
async function databaseNow(): Promise<string> {
  return sql(
    `select to_char(clock_timestamp() at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`
  );
}

/** One corpus from an empty database, with the database's clock read before its first write and
 *  after its last. */
async function seedOnce(): Promise<{ began: string; seed: ReviewSeed; seededAt: string }> {
  await resetDatabase();
  const began = await databaseNow();
  const seed = await seedReviewRoutes({
    boundMs: Number.POSITIVE_INFINITY,
    signal: stopping.signal,
  });
  return { began, seed, seededAt: await databaseNow() };
}

/** The UTC day an instant `databaseNow` read falls on. */
const utcDay = (instant: string): string => instant.slice(0, 10);

let seeded: { began: string; seed: ReviewSeed; seededAt: string };
try {
  seeded = await seedOnce();
  // The whole corpus inside one UTC day, the zone the visual projects read pages in
  // (e2e/playwright.config.ts). A page heads a list by the day of each row against the day of
  // its pinned clock, so a corpus written across a midnight draws "Yesterday" over the rows
  // before it, where every baseline draws one day. Written again from empty, the corpus starts
  // after that midnight and is done long before the next.
  if (utcDay(seeded.began) !== utcDay(seeded.seededAt)) {
    console.error(
      `e2e/visual-seed.ts wrote the corpus from ${seeded.began} to ${seeded.seededAt}, across ` +
        "a UTC midnight, so it writes it again from an empty database"
    );
    seeded = await seedOnce();
    if (utcDay(seeded.began) !== utcDay(seeded.seededAt)) {
      throw new Error(
        `the corpus took from ${seeded.began} to ${seeded.seededAt} and crossed a UTC midnight ` +
          "twice; a seed that takes a day is broken, not unlucky"
      );
    }
  }
} catch (error) {
  if (error instanceof Interrupted) {
    console.error(`e2e/visual-seed.ts was stopped: ${error.message}`);
    process.exit(stoppedWith ?? 130);
  }
  throw error;
}

// A write that fails is the whole run's failure and not the container's: without this file every
// row of the visual spec skips itself, and the run would pass having compared nothing. So the
// message names the path and the status is non-zero.
try {
  // `seededAt` is the one instant every row pins its page clock to, so a relative label and a
  // day heading read the same whatever the run's own pace and whenever it runs
  // (e2e/visual.e2e.ts says why that matters, and why a mask does not do it). Typed as
  // `VisualSeed`, which requires it, so the rows that read it never have to ask.
  const written: VisualSeed = { ...seeded.seed, seededAt: seeded.seededAt };
  writeFileSync(path, `${JSON.stringify(written, null, 2)}\n`);
} catch (error) {
  console.error(`e2e/visual-seed.ts could not write the seed to ${path}: ${String(error)}`);
  process.exit(1);
}
console.error(`e2e/visual-seed.ts wrote the review corpus's ids to ${path}`);
