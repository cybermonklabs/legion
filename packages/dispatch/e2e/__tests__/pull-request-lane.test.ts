/** The budget the pull-request lane is held to, read from the config the lane runs.
 *
 *  `.github/workflows/dispatch-browser.yaml`'s shard job runs the pull-request lane as
 *  `--project=chromium --project=iphone --project=stub --project=stub-phone` with
 *  `--grep=(^|\s)@pr(\s|$)`, and the full lane as every project with no grep. This lists the
 *  config exactly that way - the grep is `tagged("pr")`'s own source, so the two readings of
 *  the lane cannot select different rows - and holds the lane to two numbers, so a row added
 *  without a thought about which lane answers it fails here rather than on the day the lane is
 *  ten minutes slower than it was.
 *
 *  The shard job is not all of it. The same workflow's `embedder` job runs on both lanes, so on
 *  every pull request: `bun run e2e:embedder`, the rows of `e2e/search-tiers.e2e.ts` against a
 *  server that embeds at a counting fake, in `chromium` and with no grep. Those rows drive the
 *  real stack whatever their tags say, so they count toward both numbers, read from the command
 *  that script runs (`embedderCommand` of `./playwright-listing`).
 *
 *  The budget is dispatch://INFRA-1407's R1: 350. It was 330 for a while, twenty rows held for
 *  dispatch://INFRA-1410 and dispatch://INFRA-1511, and both of those are done.
 *
 *  Why at most 150 outside the stubbed projects: a real-stack row pays a Go server, a Postgres,
 *  three fakes, a reset of the fakes and a sign-in, and R1 holds the lane to 150 of them. The
 *  rest of the lane is the stubbed layer, which pays none of that.
 *
 *  A row with no `@pr` tag is not lost: it runs on the full lane, which `ci.yaml` runs for every
 *  merge group and `.github/workflows/dispatch-browser-nightly.yaml` runs daily, so `main` is
 *  gated exactly as it was. What such a row loses is feedback on the pull request itself.
 */
import { expect, test } from "bun:test";

import {
  pullRequestLaneProjects,
  rowsOfListing,
  stubProjectNames,
  tagged,
} from "../configurations";
import { rowLimit } from "./bounded-subprocess";
import { embedderCommand, listing, projectArguments, retained } from "./playwright-listing";

/** The rows the pull-request lane schedules, the rows the embedder job runs beside it, and the
 *  rows of the default selection, read from three listings of one config. The last is read for
 *  the stubbed rows alone, and both stubbed projects are in the default selection, so this file
 *  needs no fourth listing: the three wall-clock projects carry `default: false` since
 *  dispatch://INFRA-1280, so a listing that names no project holds no row of theirs, and no row
 *  of theirs is stubbed. All three start here rather than inside a test, so they collect at
 *  once. */
const lane = retained(
  listing(import.meta.url, "e2e/playwright.config.ts", {}, [
    ...projectArguments(pullRequestLaneProjects),
    "--grep",
    tagged("pr").source,
  ])
);
const embedder = embedderCommand();
const embedderRows = retained(
  listing(import.meta.url, "e2e/playwright.config.ts", embedder.environment, embedder.extra)
);
const defaultSelection = retained(listing(import.meta.url, "e2e/playwright.config.ts", {}));

/** The number of rows the pull-request lane may schedule (dispatch://INFRA-1407's R1). */
const laneBudget = 350;
/** Of those, the number that may drive the real stack. */
const realStackBudget = 150;

test(
  "the pull-request lane stays inside its budget, and inside its real-stack share",
  async () => {
    const rows = [...rowsOfListing(await lane), ...rowsOfListing(await embedderRows)];
    const realStack = rows.filter((row) => !stubProjectNames.includes(row.project));
    expect(
      rows.length,
      `The pull-request lane schedules ${rows.length} rows, and its budget is ${laneBudget}, ` +
        "dispatch://INFRA-1407's R1. Take the `@pr` tag off the rows this lane " +
        "answers least, which leaves them running on the full lane at the merge queue and the " +
        "nightly."
    ).toBeLessThanOrEqual(laneBudget);
    expect(
      realStack.length,
      `${realStack.length} rows of the pull-request lane drive the real stack, and the budget ` +
        `is ${realStackBudget}: a real-stack row pays a Go server, a Postgres, three fakes, a ` +
        "reset of the fakes and a sign-in. Move the row to the stubbed layer " +
        '(packages/dispatch/README.md, "The stubbed layer") or take its `@pr` tag off.'
    ).toBeLessThanOrEqual(realStackBudget);
  },
  rowLimit("listing")
);

test(
  "every stubbed row carries the tag, so none of them is left off the pull-request lane",
  async () => {
    // The stubbed layer exists to answer a pull request, so a stubbed row that the lane does not
    // schedule is a row whose cost was paid and whose feedback was given away.
    const stubbed = rowsOfListing(await defaultSelection).filter((row) =>
      stubProjectNames.includes(row.project)
    );
    const untagged = stubbed.filter((row) => !tagged("pr").test(row.title));
    expect(
      untagged.map((row) => `${row.file} › ${row.title}`),
      "A stubbed row with no `@pr` tag runs on the full lane alone, which is the one lane it was " +
        "never written for. Append ` @pr` to its title."
    ).toEqual([]);
    expect(stubbed.length).toBeGreaterThan(0);
  },
  rowLimit("listing")
);
