/** the rows press the view row over an issue the server seeds and serves, each count its
 *  reads answer, and count the view reads Dashboard sends the server */

import type { Page } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import { PRESET_NAMES, type Preset } from "../web/src/features/issue-page/page-words";
import { seedWholeIssue } from "./issue-layout-seed";
import {
  gotoIssue,
  issueRead,
  phase,
  presetCount,
  presetToggle,
  presetToolbar,
} from "./issue-page";
import { expectIssuePageDrawn, expectOpeningLanded, focusThePage } from "./issue-page-g";
import { expectActivePreset, pickPreset } from "./issue-page-i";
import { addressOf, FILTERS, PAGE_VIEWS, presetControls, ROW_MS } from "./issue-page-t-rows";
import {
  convertedGate,
  dashboardPartGate,
  owedUnless,
  treeReadGate,
  treeReads,
} from "./issue-page-t-tree";
import { expect, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * Rows 7 and 11 of the rows that draw the new issue page from the tree read: the view row's
 * controls in root R2's order, one active at a time (spec R12), and Dashboard's reads of the tree
 * read's views, which wait on Dashboard's part (spec R16). The head of `e2e/issue-page-t-rows.ts`
 * says what the rows read and what their gates are.
 */

/** Root R2's one declared list of views, by the view row's own tokens: every control the row may
 *  draw, in the order it draws them. Dashboard, Threads and Documents are each a line its own
 *  part adds with its view (spec R12). */
const ROOT_R2_ORDER = [
  "dashboard",
  "conversation",
  "spec",
  "verification",
  "plan",
  "decisions",
  "threads",
  "documents",
  "graph",
] as const;

/** The token a control's drawn name is the word of (`PRESET_NAMES`), and the name itself where no
 *  token's word is it. */
function tokenOf(name: string): string {
  const named = Object.entries(PRESET_NAMES).find(([, word]) => word === name);
  return named === undefined ? name : named[0];
}

/** Each control of the row of toggles, by its token, in the order the row draws them. */
async function drawnControls(page: Page): Promise<string[]> {
  return (await presetControls(page)).map((control) => tokenOf(control.name));
}

/** Each control of the row whose pressed state is on, by its token. */
async function activeControls(page: Page): Promise<string[]> {
  return (await presetControls(page)).flatMap((control) =>
    control.pressed ? [tokenOf(control.name)] : []
  );
}

/** Fails unless the one control whose pressed state is on is `view`'s, after `what`. */
async function expectOneActive(page: Page, view: Preset, what: string): Promise<void> {
  await expect
    .poll(() => activeControls(page), {
      message: `after ${what}, one control is active, ${PRESET_NAMES[view]}`,
    })
    .toEqual([view]);
}

// c-row-controls.
// SLOW(>=2s): the whole seeded issue, loaded twice, and eleven presses of the view row, each
// waited for until its address and its one active control are drawn.
test("the view row holds its controls in order, one active at a time", async ({ browser }) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    await gotoIssue(page, presetPath(seeded.key, "conversation"));
    await expectIssuePageDrawn(page);
    await expect(presetToolbar(page), "the row of toggles is not drawn at 1440 px").toHaveCount(1);

    // Root R2's order, read from the drawn row and not from the page's own list: each control the
    // row draws stands where that order puts it, and the six views of this page are among them.
    const drawn = await drawnControls(page);
    expect(drawn, "the controls stand in root R2's order").toEqual(
      ROOT_R2_ORDER.filter((view) => drawn.includes(view))
    );
    expect(
      PAGE_VIEWS.filter((view) => !drawn.includes(view)),
      "the row draws a control for each of the page's six views"
    ).toEqual([]);

    // Each filter draws its count, and Graph none.
    for (const filter of FILTERS) {
      await expect(presetCount(page, filter), `${PRESET_NAMES[filter]} draws its count`).toHaveText(
        /^\d+\+?$/
      );
    }
    await expect(presetCount(page, "graph"), "Graph draws no count").toHaveCount(0);

    await expectOneActive(page, "conversation", "the load");
    // A filter's press turns it on and every other control off, and its second press returns to
    // Conversation.
    for (const filter of FILTERS) {
      const name = PRESET_NAMES[filter];
      await presetToggle(page, filter).click();
      await expect(page, `${name} pressed is not at its address`).toHaveURL(
        addressOf(seeded.key, filter)
      );
      await expectOneActive(page, filter, `a press of ${name}`);
      await presetToggle(page, filter).click();
      await expect(page, `${name} pressed again is not at Conversation's address`).toHaveURL(
        addressOf(seeded.key, "conversation")
      );
      await expectOneActive(page, "conversation", `a second press of ${name}`);
    }
    // Graph's press turns every filter off, and its second press returns to Conversation.
    await presetToggle(page, "plan").click();
    await expectOneActive(page, "plan", "a press of Plan");
    await presetToggle(page, "graph").click();
    await expect(page, "Graph pressed is not at its address").toHaveURL(
      addressOf(seeded.key, "graph")
    );
    await expectOneActive(page, "graph", "a press of Graph with Plan on");
    await presetToggle(page, "graph").click();
    await expect(page, "Graph pressed again is not at Conversation's address").toHaveURL(
      addressOf(seeded.key, "conversation")
    );
    await expectOneActive(page, "conversation", "a second press of Graph");
    // Conversation's press returns to the page with no filter on.
    await presetToggle(page, "decisions").click();
    await expectOneActive(page, "decisions", "a press of Decisions");
    await presetToggle(page, "conversation").click();
    await expect(page, "Conversation pressed is not at the issue's own address").toHaveURL(
      addressOf(seeded.key, "conversation")
    );
    await expectOneActive(page, "conversation", "a press of Conversation with Decisions on");

    // `t g` opens Graph.
    await gotoIssue(page, presetPath(seeded.key, "conversation"));
    await expectOpeningLanded(page);
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("g");
    await expect(page, "t g did not open Graph").toHaveURL(addressOf(seeded.key, "graph"));
    await expectOneActive(page, "graph", "t g");
  });
});

// What Dashboard's row can only read once Dashboard's part draws it: each is a clause of
// c-dashboard-reads that reads a mark or a page only that part brings, so it can be built only
// once that part's marks are on this branch's base. Each is false here, and each fails the row
// past its gate until it is built.
/** Today's Dashboard tab's reads: one read each of the three views, and no results, coverage or
 *  decisions read, read on that tab's own load. */
const TODAY_DASHBOARD_READS_BUILT = false;
/** The reviews read, by the ids of the query nodes of the spec and Verification views. */
const REVIEWS_BY_QUERY_NODES_BUILT = false;
/** The same items, tiles and words that root `c-dashboard` expects on the issue the check names:
 *  an approval request that waits on the person, a stale approval and a decision to review. */
const DASHBOARD_DRAWS_BUILT = false;

// c-dashboard-reads, owed on Dashboard's part (`dashboardPartGate`), which brings Dashboard: a
// load of the new page's Dashboard reads the three views once each and no results, coverage or
// decisions read. Past the gate the row reads what the page reads without that part's marks -
// the new page's own Dashboard reads - and fails on each clause that needs those marks until it
// is built.
// SLOW(>=2s): the whole seeded issue, loaded, Dashboard pressed, and the page loaded again on
// Dashboard's address with that load waited out.
test("Dashboard reads the tree read views", async ({ browser }) => {
  owedUnless(dashboardPartGate);
  test.setTimeout(ROW_MS);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    owedUnless(await convertedGate(page, seeded.key));
    // The gate above holds only where the row's list names Dashboard, whose part then gives it
    // its word in the page's list of names, so the name is read from that list.
    const dashboard = (PRESET_NAMES as Readonly<Record<string, string>>).dashboard ?? "";
    await gotoIssue(page, presetPath(seeded.key, "conversation"));
    await expectIssuePageDrawn(page);
    await pickPreset(page, dashboard);
    await expectActivePreset(page, dashboard);
    const traffic = watchApiRequests(page);
    await page.reload();
    await issueRead(page);
    await expectLoadSettled(traffic);
    const load = traffic.requests;
    for (const view of ["spec", "verification", "decisions"] as const) {
      expect(treeReads(load, seeded.key, view), `Dashboard's load reads view=${view} once`).toBe(1);
    }
    const routes = (route: string) =>
      load.filter((request) => request.method === "GET" && request.route === route).length;
    expect(routes("/api/v1/issues/:id/results"), "Dashboard reads no results").toBe(0);
    expect(
      routes("/api/v1/issues/:id/coverage"),
      "Dashboard reads no coverage on a converted issue"
    ).toBe(0);
    expect(routes("/api/v1/issues/:id/decisions"), "Dashboard reads no decisions").toBe(0);

    expect
      .soft(
        TODAY_DASHBOARD_READS_BUILT,
        "on today's Dashboard tab, Dashboard reads no coverage on a converted issue: not built: " +
          "needs the Dashboard part's marks"
      )
      .toBe(true);
    expect
      .soft(
        REVIEWS_BY_QUERY_NODES_BUILT,
        "Dashboard reads the reviews by the query nodes' ids: not built: needs the Dashboard " +
          "part's marks"
      )
      .toBe(true);
    expect
      .soft(
        DASHBOARD_DRAWS_BUILT,
        "Dashboard draws the items, tiles and words root c-dashboard expects: not built: needs " +
          "the Dashboard part's marks"
      )
      .toBe(true);
  });
});
