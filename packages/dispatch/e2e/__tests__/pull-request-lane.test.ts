/** The budget the pull-request lane is held to, read from the config the lane runs.
 *
 *  `.github/workflows/dispatch-browser.yaml`'s shard job runs the pull-request lane as
 *  `--project=chromium --project=iphone --project=stub --project=stub-phone --grep=@pr`, and the
 *  full lane as every project with no grep. This lists the config exactly that way and holds the
 *  lane to two numbers, so a row added without a thought about which lane answers it fails here
 *  rather than on the day the lane is ten minutes slower than it was.
 *
 *  Why 330 and not the 350 dispatch://INFRA-1407's R1 names: dispatch://INFRA-1410 and
 *  dispatch://INFRA-1511 land after this head and will each want rows on this lane, and the
 *  parent's own check reads the 350 at the root's last round rather than here. Twenty rows is
 *  the margin left for them.
 *
 *  Why at most 150 outside the stubbed projects: a real-stack row pays a Go server, a Postgres,
 *  three fakes, a sign-in and a database reset, and R1 holds the lane to 150 of them. The rest
 *  of the lane is the stubbed layer, which pays none of that.
 *
 *  A row with no `@pr` tag is not lost: it runs on the full lane, which `ci.yaml` runs for every
 *  merge group and `.github/workflows/dispatch-browser-nightly.yaml` runs daily, so `main` is
 *  gated exactly as it was. What such a row loses is feedback on the pull request itself.
 */
import { expect, test } from "bun:test";

import { rowsOfListing } from "../configurations";
import { listing, retained } from "./playwright-listing";

const laneProjects = ["chromium", "iphone", "stub", "stub-phone"] as const;
const stubProjects: readonly string[] = ["stub", "stub-phone"];

/** The rows the pull-request lane schedules, and the rows the whole suite holds, read from two
 *  listings of one config. Both start here rather than inside a test, so the second collects
 *  while the first does. */
const lane = retained(
  listing("e2e/playwright.config.ts", {}, [
    ...laneProjects.flatMap((project) => ["--project", project]),
    "--grep",
    "@pr",
  ])
);
const everything = retained(listing("e2e/playwright.config.ts", {}));

/** Two listings at about five seconds each, on a two-core runner. */
const listingTimeout = 90_000;

/** The number of rows the pull-request lane may schedule. */
const laneBudget = 330;
/** Of those, the number that may drive the real stack. */
const realStackBudget = 150;

test(
  "the pull-request lane stays inside its budget, and inside its real-stack share",
  async () => {
    const rows = rowsOfListing(await lane);
    const realStack = rows.filter((row) => !stubProjects.includes(row.project));
    expect(
      rows.length,
      `The pull-request lane schedules ${rows.length} rows, and its budget is ${laneBudget}. ` +
        "It is 330 rather than R1's 350 because INFRA-1410 and INFRA-1511 land after this head " +
        "and need rows of their own on this lane. Take the `@pr` tag off the rows this lane " +
        "answers least, which leaves them running on the full lane at the merge queue and the " +
        "nightly."
    ).toBeLessThanOrEqual(laneBudget);
    expect(
      realStack.length,
      `${realStack.length} rows of the pull-request lane drive the real stack, and the budget ` +
        `is ${realStackBudget}: a real-stack row pays a Go server, a Postgres, three fakes, a ` +
        "sign-in and a database reset. Move the row to the stubbed layer " +
        '(packages/dispatch/AGENTS.md, "The stubbed layer") or take its `@pr` tag off.'
    ).toBeLessThanOrEqual(realStackBudget);
  },
  listingTimeout
);

test(
  "every stubbed row carries the tag, so none of them is left off the pull-request lane",
  async () => {
    // The stubbed layer exists to answer a pull request, so a stubbed row that the lane does not
    // schedule is a row whose cost was paid and whose feedback was given away.
    const stubbed = rowsOfListing(await everything).filter((row) =>
      stubProjects.includes(row.project)
    );
    const untagged = stubbed.filter((row) => !/(^|\s)@pr(\s|$)/.test(row.title));
    expect(
      untagged.map((row) => `${row.file} › ${row.title}`),
      "A stubbed row with no `@pr` tag runs on the full lane alone, which is the one lane it was " +
        "never written for. Append ` @pr` to its title."
    ).toEqual([]);
    expect(stubbed.length).toBeGreaterThan(0);
  },
  listingTimeout
);
