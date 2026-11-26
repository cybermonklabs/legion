// The Evals tab's inventory rows (dispatch://INFRA-2918/artifact/plan-md, "Inventory tests,
// written first and failing"): each names every member of a class the tab must hold, so a member
// the tab leaves out fails its row rather than passing unseen. They are written before the tab
// draws its sections, and each is marked `test.fail` until the task that draws its members lands
// (the plan's T28 turns them passing): a row that starts passing while still marked fails the
// run, which is the sign to drop the marker.
//
// acme/widgets-shaped placeholders throughout (AGENTS.md: "This repository is public").
import { baseUrl, registerEval, userHeaders } from "./api";
import { seedMeasuresFixture, seedPipelineRows } from "./delivery-seed";
import { LEGION_PROJECT, seedLegionDaemon } from "./legion-seed";
import { expect, type Locator, type Page, test } from "./test";

/** The project every source is seeded for: the delivery fixtures' and the Legion seed's one. */
const PROJECT = LEGION_PROJECT;

/** A window spanning both delivery fixtures, the measures fixture's (2026-08-30 to 09-27) and the
 *  Pipeline fixture's (to 2026-10-02), as the tab's address names a custom window. */
const WINDOW_QUERY = "from=2026-08-30T00%3A00%3A00Z&to=2026-10-03T00%3A00%3A00Z";

/** A brush window inside it, which the timeline's address carries as the Delivery page's did. */
const BRUSH_QUERY = "ws=2026-09-05T00%3A00%3A00Z&we=2026-09-20T00%3A00%3A00Z";

/** The logged eval the relation row reads its links from. */
const RELATIONS_EVAL = { name: "Widget relations", slug: "e2e-widget-relations" } as const;

/** The four relations the tab draws apart from a plain link (spec R10), and two it draws plain. */
const SPECIAL_RELATIONS = ["via", "describedby", "enclosure", "source-rule"] as const;
const PLAIN_RELATIONS = ["related", "alternate"] as const;

/** Every source the tab reads, on one project: the delivery settings and the measures fixture's
 *  pull requests, deploys and failures with its unowned P0 issue, the Pipeline fixture's PR-checks
 *  and deploy jobs, and a Legion daemon. */
async function seedEverySource(): Promise<void> {
  await seedMeasuresFixture();
  await seedPipelineRows();
  await seedLegionDaemon("http://127.0.0.1:9/legion/state");
}

/** The tab, opened on the seeded window, once its header has drawn. */
async function openTab(page: Page, query = ""): Promise<void> {
  await page.goto(`/projects/${PROJECT}/evals?${WINDOW_QUERY}${query}`);
  await expect(page.getByRole("tab", { name: "Evals" })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByRole("group", { name: "Window and filters" })).toBeVisible();
}

/** Opens the merge timeline, which the tab keeps closed on its first paint (spec R13). */
async function openTimeline(page: Page): Promise<Locator> {
  const timeline = page.getByRole("region", { name: "Merge timeline" });
  await timeline.getByRole("button", { name: "Show the merge timeline" }).click();
  return timeline;
}

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

// SLOW(>=2s): seeds every delivery fixture and a daemon, opens the tab and its timeline, and opens
// the drill-down of a merge from the merge list.
test.fail("the tab draws every item of root R4's list", async ({ page }) => {
  await seedEverySource();
  await openTab(page, `&${BRUSH_QUERY}`);
  const cards = page.getByRole("region", { name: "Target cards" });
  const deploys = page.getByRole("region", { name: "Deploys and lead time" });
  const ci = page.getByRole("region", { name: "CI health" });
  const timeline = await openTimeline(page);
  const freshness = page.getByRole("region", { name: "Source freshness" });

  await expectEveryItem([
    ["target cards", cards.locator("[data-card]")],
    ["definitions", deploys.locator("[data-definition]")],
    ["per-day table", deploys.getByRole("table", { name: /per day/ })],
    ["the timeline", timeline.getByTestId("delivery-chart")],
    ["the timeline's brush", page.getByRole("button", { name: "clear brush window" })],
    ["the timeline's colour-by", timeline.getByLabel("Color merges by")],
    ["the facets", timeline.getByRole("button", { name: /^Repository/ })],
    ["the session groups", timeline.getByRole("button", { name: "Edit groups" })],
    ["the freshness line", freshness],
    ["live updates", freshness.getByText(/^Live/)],
    ["the staging end-to-end card", ci.locator('[data-eval="staging-e2e"]')],
    ["the staging deploy card", ci.locator('[data-eval="staging-deploy"]')],
    ["the production deploy card", ci.locator('[data-eval="production-deploy"]')],
    ["the slowest jobs", ci.getByRole("region", { name: "PR CI slowest jobs" })],
    [
      "the jobs failed in the last 24 hours",
      ci.getByRole("region", { name: "PR CI failing jobs" }),
    ],
    ["P0 with no owner", cards.locator('[data-card="unowned-p0"]')],
  ]);

  // The merge list, and the drill-down a merge's deploy opens from it.
  await timeline.getByRole("button", { name: "List", exact: true }).click();
  await timeline.getByRole("button", { name: "Open deploy run #501" }).click();
  await expect(page.getByRole("dialog", { name: "Details" })).toBeVisible();
});

// SLOW(>=2s): seeds every source and walks every chart of the tab twice, in Graph and in Table.
test.fail("every chart of the tab has both controls", async ({ page }) => {
  await seedEverySource();
  await openTab(page);
  await openTimeline(page);
  const charts = page.locator("[data-chart]");
  // Zero charts is a failure, not an empty walk: the tab draws a chart in every section it shows.
  await expect.poll(() => charts.count()).toBeGreaterThan(0);

  // Read after the poll above has seen the charts drawn, so the walk covers the tab's charts.
  for (const chart of await charts.all()) {
    // The chart is drawn, so the name it carries is on it already and never changes after.
    const name = (await chart.getAttribute("data-chart")) ?? "";
    // The chart is drawn, so the kind it carries is on it already and never changes after.
    const kind = await chart.getAttribute("data-chart-kind");
    await expect(chart.getByRole("button", { name: "Graph" }), name).toBeVisible();
    await expect(chart.getByRole("button", { name: "Table" }), name).toBeVisible();
    // R7 exempts the merge timeline from the compare control, and a pending chart draws no data.
    const compares = name !== "timeline" && kind !== "pending";
    await expect(chart.getByRole("checkbox", { name: "Compare" }), name).toHaveCount(
      compares ? 1 : 0
    );

    const drawn = await chart
      .locator("[data-value]")
      .evaluateAll((values) => values.map((value) => value.getAttribute("data-value")));
    await chart.getByRole("button", { name: "Table" }).click();
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

/** The headers of `table` that draw their label, their filter button or their sort mark cut: the
 *  header cell's content wider than the cell, or the label's button wider than the room it is
 *  given. One reading, each named by its text. */
function headersCut(table: Locator): Promise<string[]> {
  return table.locator("th").evaluateAll((headers) =>
    headers
      .filter((header) => {
        const content = header.firstElementChild;
        const label = header.querySelector("button");
        return (
          (content !== null && content.scrollWidth > header.clientWidth) ||
          (label !== null && label.scrollWidth > label.clientWidth)
        );
      })
      .map((header) => header.textContent ?? "")
  );
}

// SLOW(>=2s): seeds every source, turns every chart to its table and sorts every column of every
// table once, reading each header unsorted and sorted.
test.fail("every column header draws its natural width sorted and unsorted", async ({ page }) => {
  await seedEverySource();
  await openTab(page);
  const timeline = await openTimeline(page);
  await timeline.getByRole("button", { name: "List", exact: true }).click();
  const tableButtons = page.locator("[data-chart]").getByRole("button", { name: "Table" });
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
});

// SLOW(>=2s): seeds every source and opens at least ten group numbers, each in a page of its own.
test.fail("every group number opens a view with its count", async ({ page }) => {
  await seedEverySource();
  await openTab(page);
  const links = page.locator("a[data-group-count]");
  const p0 = page
    .getByRole("region", { name: "Target cards" })
    .locator('[data-card="unowned-p0"] [data-group-count]');
  // The Legion headlines, their phase parts and the per-day rows link; the P0 card resolves its
  // keys when pressed (plan D13) and is the tenth, opened apart below.
  await expect.poll(() => links.count()).toBeGreaterThanOrEqual(9);

  // Read after the poll above has seen at least nine group links drawn.
  for (const link of await links.all()) {
    const count = Number(await link.getAttribute("data-group-count"));
    const view = await page.context().newPage();
    await view.goto((await link.getAttribute("href")) ?? "");
    await expect(view.locator("[data-issue-row]")).toHaveCount(count);
    await view.close();
  }

  const p0Count = Number(await p0.getAttribute("data-group-count"));
  await p0.click();
  await expect(page).toHaveURL(/[?&]only=/);
  await expect(page.locator("[data-issue-row]")).toHaveCount(p0Count);
});

/** A link of every relation, each to a result page of its own under `where`. */
function linksOfEveryRelation(where: string) {
  return [...SPECIAL_RELATIONS, ...PLAIN_RELATIONS].map((rel) => ({
    href: `https://results.example.com/${where}/${rel}`,
    label: `${where} ${rel}`,
    rel,
  }));
}

/** The relation eval: a logged eval whose own links, its primary series' links and one reading's
 *  links each hold every relation. The readings write registers it (spec R19), and the register
 *  write then names its links. */
async function seedRelations(): Promise<void> {
  const reading = {
    links: linksOfEveryRelation("reading"),
    measured_at: "2026-09-10T12:00:00Z",
    row_key: "widget-1",
    row_kind: "row",
    series: "widgets",
    value: 3,
  };
  const written = await fetch(
    new URL(`/api/v1/projects/${PROJECT}/evals/${RELATIONS_EVAL.slug}/readings`, baseUrl),
    {
      body: JSON.stringify({ readings: [reading] }),
      headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
      method: "POST",
    }
  );
  expect(written.status, await written.text()).toBe(201);
  await registerEval(PROJECT, {
    links: linksOfEveryRelation("eval"),
    name: RELATIONS_EVAL.name,
    series: [{ is_primary: true, links: linksOfEveryRelation("series"), series: "widgets" }],
    slug: RELATIONS_EVAL.slug,
  });
}

/** Every relation drawn in `place`: the four of R10 each as its own kind, and the two others as a
 *  plain link. */
async function expectEveryRelation(place: Locator, where: string): Promise<void> {
  for (const rel of SPECIAL_RELATIONS) {
    const link = place.locator(`a[data-rel="${rel}"][data-link-kind="${rel}"]`).first();
    await expect(link, `${where}: ${rel}`).toBeVisible();
  }
  for (const rel of PLAIN_RELATIONS) {
    const link = place.locator(`a[data-rel="${rel}"][data-link-kind="plain"]`).first();
    await expect(link, `${where}: ${rel}`).toBeVisible();
  }
}

// SLOW(>=2s): seeds every source and a logged eval, and opens its panel, its rows and its reading.
test.fail("each relation draws as its kind", async ({ page }) => {
  await seedEverySource();
  await seedRelations();
  await openTab(page);

  // The number's own panel: the eval's links and its primary series' links, under the method.
  const logged = page.getByRole("region", { name: "Logged" });
  await logged.getByRole("button", { name: RELATIONS_EVAL.name }).click();
  const panel = page.getByRole("dialog", { name: RELATIONS_EVAL.name });
  await expectEveryRelation(panel.getByRole("list", { name: "Links" }), "the panel");

  // The rows behind the number: each row's links beside its own href.
  await expectEveryRelation(panel.locator("[data-eval-row]"), "the rows");

  // The reading as logged, with the links it carried.
  await expectEveryRelation(logged.locator("[data-reading]"), "the reading as logged");
});
