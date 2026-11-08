/** The accessibility layer: every page of `e2e/review-routes.ts`'s scanned set, in light and in
 *  dark, read by axe at WCAG 2.0 and 2.1 level A and AA. The rows are untagged, so only the
 *  `chromium` project runs them and the pass reaches every pull request
 *  (e2e/playwright.config.ts).
 *
 *  A row fails on any serious or critical violation `e2e/accessibility-exclusions.ts` does not
 *  cover, and each (route, scheme) prints one `A11Y …` line to stdout, so the shard legs' own job
 *  logs carry the counts and the run's report carries the whole axe result as an attachment. */
import AxeBuilder from "@axe-core/playwright";
import { expect, test } from "@playwright/test";

import { type AccessibilityExclusion, accessibilityExclusions } from "./accessibility-exclusions";
import { scannedRoutes, waitForRouteSettled } from "./review-routes";
import { seedReviewRoutes } from "./review-seed";
import { resetDatabase } from "./seed";
import { withAlice } from "./users";

/** WCAG 2.0 and 2.1, levels A and AA: the tags the plan names, and the ones a reviewer of this
 *  dashboard is held to. Axe's best-practice rules are not in it, since a best practice is not a
 *  conformance failure and this gate refuses a pull request. */
const AXE_TAGS = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"];

/** Both schemes are read inside one row rather than in a row each. Measured on three routes of
 *  differing weight, one row per route costs 15.0 s against 19.2 s for one row per route and
 *  scheme - a reset, a seed and a sign-in paid once instead of twice - and nothing is lost,
 *  since every report line and every failure names its scheme. */
const SCHEMES = ["light", "dark"] as const;

/** The four impacts axe reports, worst first. Axe's own types live in `axe-core`, which is
 *  `@axe-core/playwright`'s dependency and not this package's, so there is no name here to
 *  import for them. */
type Impact = "critical" | "serious" | "moderate" | "minor";

/** What this worker process scanned, and which entries its scans matched. `test.afterAll` reads
 *  both: an entry whose route this process scanned, and which nothing matched, is stale, and a
 *  stale entry would hide the regression it once accepted.
 *
 *  The check is a hook and not a final row because no row can see the run. `--shard=N/4` splits
 *  this file's rows over four machines (.github/workflows/dispatch-browser.yaml's shard job) and
 *  the lane's four workers split each shard again, so a final row would sit in one process and
 *  judge entries whose routes another process ran. `afterAll` runs once in each worker, after
 *  the rows that worker ran, which is exactly the set it can judge - and judging only the routes
 *  this process scanned is what keeps a shard that ran none of an entry's route from failing
 *  for it. */
const scannedHere = new Set<string>();
const matchedHere = new Set<string>();

/** The key a finding and an entry meet on: the axe rule, the route, and the node's selector.
 *  One spelling of it, because the stale check below reads the joined string as the entry's
 *  identity and a finding that matched has to have written that same string. */
function keyOf(rule: string, route: string, selector: string): string {
  return `${rule} ${route} ${selector}`;
}

/** An entry's own key, which is what `matchedHere` holds and what the stale check looks for. */
function exclusionKey(exclusion: AccessibilityExclusion): string {
  return keyOf(exclusion.rule, exclusion.route, exclusion.selector);
}

/** Every entry's key, built once: a serious or critical finding is excluded exactly when its
 *  own key is in here. */
const excludedKeys = new Set(accessibilityExclusions.map(exclusionKey));

for (const route of scannedRoutes) {
  // SLOW(>=2s): a database reset, the review corpus, a sign-in and two axe scans of a drawn page.
  test(`${route.name} has no serious or critical accessibility violation in light or dark`, async ({
    browser,
  }, testInfo) => {
    await resetDatabase();
    const seed = await seedReviewRoutes();
    await withAlice(browser, async (page) => {
      await page.goto(route.path(seed));
      const failures: string[] = [];
      for (const colorScheme of SCHEMES) {
        await page.emulateMedia({ colorScheme });
        // Before the scan, so axe reads the page a reader reads and never a loading skeleton,
        // a 404 or a page still resolving itself: the first two would pass with no findings at
        // all, and the third would report findings about a state no reader sees
        // (`waitForRouteSettled`).
        await waitForRouteSettled(page, route);
        const results = await new AxeBuilder({ page }).withTags(AXE_TAGS).analyze();
        await testInfo.attach(`axe ${route.name} ${colorScheme}.json`, {
          body: JSON.stringify(results, null, 2),
          contentType: "application/json",
        });

        // One count per impact, counted by node and not by rule: a rule that fires in twelve
        // places is twelve things to fix, and a regression that adds one place moves the number.
        const counts: Record<Impact, number> = { critical: 0, minor: 0, moderate: 0, serious: 0 };
        for (const violation of results.violations) {
          for (const node of violation.nodes) {
            // A node's own impact where axe gave it one, else its rule's. Axe leaves both unset
            // only where it cannot judge the node, and such a node is counted minor rather than
            // gated on: this layer refuses a pull request, so a finding whose impact axe does
            // not name is reported, never enforced.
            const impact = (node.impact ?? violation.impact ?? "minor") as Impact;
            counts[impact] += 1;
            if (impact !== "serious" && impact !== "critical") continue;
            // `target` is one selector per frame, outermost first; the dashboard's pages hold no
            // frame, so this is one selector in practice and a nested one reads as its path.
            const selector = node.target
              .map((part) => (Array.isArray(part) ? part.join(" ") : String(part)))
              .join(" ");
            const key = keyOf(violation.id, route.name, selector);
            if (excludedKeys.has(key)) {
              matchedHere.add(key);
            } else {
              failures.push(
                `${violation.id} (${impact}) on ${route.name} in ${colorScheme} at ${selector}`
              );
            }
          }
        }
        console.log(
          `A11Y route=${route.name} scheme=${colorScheme} critical=${counts.critical} ` +
            `serious=${counts.serious} moderate=${counts.moderate} minor=${counts.minor}`
        );
      }
      // After both scans, so an entry is judged stale only against a route this process really
      // read, and a row that failed before it scanned leaves nothing behind to judge.
      scannedHere.add(route.name);
      expect(
        failures,
        "serious or critical axe violations no entry of e2e/accessibility-exclusions.ts covers"
      ).toEqual([]);
    });
  });
}

test.afterAll(() => {
  const stale = accessibilityExclusions
    .filter(
      (exclusion) => scannedHere.has(exclusion.route) && !matchedHere.has(exclusionKey(exclusion))
    )
    .map(exclusionKey);
  expect(
    stale,
    "entries of e2e/accessibility-exclusions.ts whose route was scanned and which matched nothing"
  ).toEqual([]);
});
