// THROWAWAY: models two container runs - a fresh corpus each, a minute apart - and reports every
// line of the shot set the two disagree on. Deleted once its findings are settled.
import { appendFileSync } from "node:fs";
import type { Browser } from "@playwright/test";
import { test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { type ReviewSeed, shotRoutes, waitForRouteSettled } from "./review-routes";
import { seedReviewRoutes } from "./review-seed";
import { resetDatabase } from "./seed";
import { asUser } from "./users";
import { workspaceSessions } from "./workspace";

/** The lines the visual layers paint over (`e2e/visual.e2e.ts`'s `mask`), dropped so this probe
 *  measures what those layers compare rather than what the page says. None of the three is
 *  settleable, and each is of fixed width, which is what makes a mask safe for it:
 *
 *  - "Generated 11/8/2026, 4:15:00 AM …" and "…, as of 04:14 AM" are an absolute moment, not a
 *    relative label. The page clock below is pinned a minute after the corpus was written, so
 *    the relative labels read the same twice; an absolute one then reads as the corpus's own
 *    wall-clock time, which is a different minute in every seeded run by construction.
 *  - "Block 0c1f…" is the id of the spec block a decision shaped, a uuid the server makes fresh
 *    in every seeded corpus. */
const MASKED = [/^Generated /, /, as of /, /^Block [0-9a-f]{8}/];

async function take(
  browser: Browser,
  pin: boolean,
  dump: string
): Promise<Record<string, string[]>> {
  await resetDatabase();
  const base = await seedReviewRoutes();
  const seed: ReviewSeed = { ...base, seededAt: new Date().toISOString() };
  await setLiveSessions(workspaceSessions);
  const seen: Record<string, string[]> = {};
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  if (pin && seed.seededAt !== undefined) {
    await page.clock.install({ time: new Date(Date.parse(seed.seededAt) + 60_000) });
  }
  for (const route of shotRoutes) {
    await page.goto(route.path(seed));
    await waitForRouteSettled(page, route);
    const text = await page.evaluate(() => document.body.innerText);
    seen[route.name] = text
      .split("\n")
      .filter((line) => !MASKED.some((masked) => masked.test(line)));
    // Written as each page is read, so a row that times out later still leaves every page
    // before it on disk to diff by hand.
    appendFileSync(dump, `=== ${route.name}\n${seen[route.name].join("\n")}\n`);
  }
  await alice.close();
  return seen;
}

test("two seeded runs a minute apart render the same text", async ({ browser }) => {
  test.setTimeout(900_000);
  const dumps = `${process.env.TMPDIR ?? "/tmp"}/drift-probe`;
  const first = await take(browser, true, `${dumps}-first.txt`);
  const wait = Promise.withResolvers<void>();
  setTimeout(wait.resolve, 90_000);
  await wait.promise;
  const second = await take(browser, true, `${dumps}-second.txt`);
  let drifts = 0;
  for (const route of shotRoutes) {
    const [a, b] = [first[route.name] ?? [], second[route.name] ?? []];
    for (let i = 0; i < Math.max(a.length, b.length); i += 1) {
      if (a[i] !== b[i]) {
        drifts += 1;
        console.log(`DRIFT ${route.name} | ${a[i] ?? "<none>"} | ${b[i] ?? "<none>"}`);
      }
    }
  }
  console.log(`DRIFT-TOTAL ${drifts}`);
});
