// The Evals tab's inventory rows (dispatch://INFRA-2918/artifact/plan-md, "Inventory tests,
// written first and failing"): each names every member of a class the tab must hold, so a member
// the tab leaves out fails its row rather than passing unseen. T1 wrote them before the tab drew
// its sections, marked to fail; T28 turned them passing, each over `e2e/evals-seed.ts`'s project
// with every source live, and each reads the tab by the locators its components draw
// (`web/src/features/evals/`). Each is tagged `@evals`, which the browser suite's Job selects them
// by (`bun run e2e -- --grep @evals`).
//
// acme/widgets-shaped placeholders throughout (AGENTS.md: "This repository is public").
import {
  chartFigure,
  EVERY_SOURCE,
  expectEveryRelation,
  expectLegionWork,
  groupLinks,
  headersCut,
  openGroupLink,
  openTab,
  pinnedOverShare,
  RELATIONS_EVAL,
  seedEverySource,
  seedRelations,
  showChart,
  showGroupLinks,
  showP0Link,
  TAB_QUERY,
} from "./evals-seed";
import { openMergeTimeline, paintedSection } from "./evals-tab";
import { expect, type Locator, test } from "./test";

/** A brush window inside the tab's window, which the timeline's address carries as the Delivery
 *  page's did. */
const BRUSH_QUERY = "ws=2026-09-05T00%3A00%3A00Z&we=2026-09-20T00%3A00%3A00Z";

/** Which of `items` the page shows none of, read until it shows every one or the poll's time
 *  ends, so the failure names each item missing rather than the first. */
async function expectEveryItem(items: readonly (readonly [string, Locator])[]): Promise<void> {
  await expect
    .poll(async () => {
      const counts = await Promise.all(items.map(([, locator]) => locator.count()));
      return items.map(([name], index) => [name, (counts[index] ?? 0) > 0]);
    })
    .toEqual(items.map(([name]) => [name, true]));
}

// SLOW(>=2s): seeds every source, opens the tab, a chart's table and the timeline, and opens the
// drill-down of a merge from the merge list.
test("the tab draws every item of root R4's list @evals", async ({ page }) => {
  await seedEverySource();
  await openTab(page, EVERY_SOURCE, `${TAB_QUERY}&${BRUSH_QUERY}`);
  const cards = await paintedSection(page, "cards");
  const deploys = await paintedSection(page, "deploys");
  const ci = await paintedSection(page, "ci");
  // The per-day table is a chart's table, drawn once the reader turns the chart to it.
  await showChart(chartFigure(deploys, "deploy-run-success"), "Table");
  const timeline = await openMergeTimeline(page);
  const freshness = page.getByRole("region", { name: "Source freshness" });

  await expectEveryItem([
    ["target cards", cards.locator("[data-card]")],
    ["definitions", deploys.locator("[data-method]")],
    ["per-day table", deploys.getByRole("table", { name: /per day$/ })],
    ["the timeline", timeline.getByTestId("delivery-chart")],
    ["the timeline's brush", timeline.getByRole("button", { name: "clear brush window" })],
    ["the timeline's colour-by", timeline.getByLabel("Color merges by")],
    ["the facets", timeline.getByRole("button", { name: /^Repository:/ })],
    ["the session groups", timeline.getByRole("button", { name: "Edit groups" })],
    ["the freshness line", freshness],
    ["live updates", freshness.locator('[data-source="prs"]').filter({ hasText: /^PRs Live/ })],
    ["the staging end-to-end card", ci.locator('[data-eval="staging-e2e"]')],
    ["the staging deploy card", ci.locator('[data-eval="staging-deploy"]')],
    ["the production deploy card", ci.locator('[data-eval="production-deploy"]')],
    ["the slowest jobs", ci.getByRole("region", { name: "PR CI: slowest jobs" })],
    [
      "the jobs failed in the last 24 hours",
      ci.getByRole("region", { name: "PR CI: jobs that failed in the last 24 hours" }),
    ],
    ["P0 with no owner", cards.locator('[data-card="unowned-p0"]')],
  ]);

  // The merge list, and the drill-down a merge's deploy opens from it.
  await timeline.getByRole("button", { exact: true, name: "List" }).click();
  await timeline.getByRole("button", { name: "Open deploy run #501" }).first().click();
  await expect(page.getByRole("dialog", { name: "Details" })).toBeVisible();
});

// SLOW(>=2s): seeds every source and walks every chart of the tab twice, in Graph and in Table.
test("every chart of the tab has both controls @evals", async ({ page }) => {
  await seedEverySource();
  await openTab(page, EVERY_SOURCE, TAB_QUERY);
  for (const section of ["cards", "legion", "ci", "deploys", "logged"]) {
    await paintedSection(page, section);
  }
  await openMergeTimeline(page);
  const charts = page.locator("main figure[data-chart]");
  // Zero charts is a failure, not an empty walk: the tab draws a chart in every section it shows.
  await expect.poll(() => charts.count()).toBeGreaterThan(0);

  // Read after the poll above has seen the charts drawn, so the walk covers the tab's charts.
  for (const chart of await charts.all()) {
    // The chart is drawn, so the name it carries is on it already and never changes after.
    const name = (await chart.getAttribute("data-chart")) ?? "";
    // The chart is drawn, so the kind it carries is on it already and never changes after.
    const kind = await chart.getAttribute("data-chart-kind");
    await expect(chart.getByRole("button", { exact: true, name: "Graph" }), name).toBeVisible();
    await expect(chart.getByRole("button", { exact: true, name: "Table" }), name).toBeVisible();
    // R7 exempts the merge timeline from the compare control, and a pending chart draws no data.
    const compares = name !== "timeline" && kind !== "pending";
    await expect(chart.getByRole("checkbox", { name: "Compare" }), name).toHaveCount(
      compares ? 1 : 0
    );

    const drawn = await chart
      .locator("[data-value]")
      .evaluateAll((values) => values.map((value) => value.getAttribute("data-value")));
    await showChart(chart, "Table");
    const tabled = await chart
      .getByRole("table")
      .locator("[data-value]")
      .evaluateAll((values) => values.map((value) => value.getAttribute("data-value")));
    expect(
      drawn.filter((value) => !tabled.includes(value)),
      name
    ).toEqual([]);
  }
});

// SLOW(>=2s): seeds every source, turns every chart to its table and sorts every column of every
// table once, reading each header unsorted and sorted, then reads the issue table's pinned columns.
test("every column header draws its natural width sorted and unsorted @evals", async ({ page }) => {
  // Every column of every table of the tab is sorted once, each sort a render of its table.
  test.setTimeout(300_000);
  await seedEverySource();
  await openTab(page, EVERY_SOURCE, TAB_QUERY);
  for (const section of ["cards", "legion", "ci", "deploys", "logged"]) {
    await paintedSection(page, section);
  }
  const timeline = await openMergeTimeline(page);
  await timeline.getByRole("button", { exact: true, name: "List" }).click();
  const tableButtons = page
    .locator("main figure[data-chart]")
    .getByRole("button", { exact: true, name: "Table" });
  await expect.poll(() => tableButtons.count()).toBeGreaterThan(0);
  // Read after the poll above has seen the charts drawn, so every chart is turned to its table.
  for (const button of await tableButtons.all()) await button.click();

  const headers = page.locator("main table th");
  // Zero headers is a failure: every table of the tab has a header row.
  await expect.poll(() => headers.count()).toBeGreaterThan(0);
  // Read after the poll above has seen the headers drawn, so each table of the tab is measured.
  for (const table of await page.locator("main table").all()) {
    expect(await headersCut(table), "unsorted").toEqual([]);
    const sortButtons = table.locator("th button[title^='Sort']");
    // Read after the table's headers were measured above, so its sortable columns are drawn.
    for (const sort of await sortButtons.all()) {
      await sort.click();
      expect(await headersCut(table), "sorted").toEqual([]);
    }
  }

  // The tab pins no column (EvalTable names no pins); the issue table its group links open pins
  // its key and title columns, which stay within their share of the box once each header's natural
  // width is applied (ledger item 5).
  await page.goto(`/projects/${EVERY_SOURCE}/issues?nofilter=1`);
  await expect(page.locator("[data-issue-row]").first()).toBeVisible();
  expect(await pinnedOverShare(page)).toEqual([]);
});

// SLOW(>=2s): seeds every source and opens at least ten group numbers, each on a tab of its own.
test("every group number opens a view with its count @evals", async ({ page }) => {
  // Each link is opened from a tab of its own, since pressing one leaves the tab.
  test.setTimeout(120_000);
  await seedEverySource();
  await expectLegionWork();
  await showGroupLinks(page);
  // The tab itself lists no issue (spec R8): its tables' rows carry `data-row`.
  await expect(page.locator("[data-issue-row]")).toHaveCount(0);
  // Read after groupLinks' own poll has seen at least ten links drawn, so every link is walked.
  const links = await groupLinks(page, 10);
  // The Legion headlines, each phase bar that holds hours (ledger item 13) and the per-day rows,
  // over the default window.
  const phaseBars = links.filter((link) => link.selector.includes('"legion-phase-time"'));
  expect(phaseBars.length).toBeGreaterThan(0);
  for (const link of links) {
    await showGroupLinks(page);
    await openGroupLink(page, link);
  }
  // The P0 card's one link, over the days from its input's first day (`showP0Link`).
  await openGroupLink(page, await showP0Link(page));
});

// SLOW(>=2s): seeds every source and a logged eval, and opens its rows, its reading and its panel.
test("each relation draws as its kind @evals", async ({ page }) => {
  await seedEverySource();
  await seedRelations(EVERY_SOURCE);
  await openTab(page, EVERY_SOURCE, TAB_QUERY);
  const logged = await paintedSection(page, "logged");
  const box = logged.locator(`[data-eval="${RELATIONS_EVAL.slug}"]`);

  // The reading as logged, with the links it carried, under the box's Rows.
  await box.getByRole("button", { exact: true, name: "Rows" }).click();
  await expectEveryRelation(box.locator("[data-reading]"), "the reading as logged");

  // The number's own panel: the eval's links and its primary series' links, under the method.
  await box.getByRole("button", { exact: true, name: RELATIONS_EVAL.name }).click();
  const panel = page.getByRole("dialog", { name: RELATIONS_EVAL.name });
  await expectEveryRelation(panel.getByRole("list", { name: "Links" }), "the panel");

  // The rows behind the number: each row's links beside its own href.
  await expectEveryRelation(panel.locator("[data-eval-row]"), "the rows");
});
